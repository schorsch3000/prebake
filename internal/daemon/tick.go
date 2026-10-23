package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/schorsch3000/prebake/internal/build"
	"github.com/schorsch3000/prebake/internal/config"
	"github.com/schorsch3000/prebake/internal/notify"
	"github.com/schorsch3000/prebake/internal/schedule"
	"github.com/schorsch3000/prebake/internal/state"
)

// tailBuffer keeps the last max bytes written to it, so a chatty build cannot
// use unbounded memory.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	if len(t.buf) > t.max {
		return string(t.buf[len(t.buf)-t.max:])
	}
	return string(t.buf)
}

func policyOf(e config.Effective) schedule.Policy {
	return schedule.Policy{RebuildInterval: e.RebuildInterval.Std(), RefreshInterval: e.RefreshInterval.Std()}
}

// keys lists every (container, flavor) of the active repo.
func (d *Daemon) keys() []schedule.Key {
	d.mu.Lock()
	repo := d.repo
	d.mu.Unlock()
	if repo == nil {
		return nil
	}
	var keys []schedule.Key
	for _, name := range repo.Names() {
		for _, f := range repo.Containers[name].FlavorNames() {
			keys = append(keys, schedule.Key{Container: name, Flavor: f})
		}
	}
	return keys
}

// Tick evaluates the schedule once and runs everything that is runnable now,
// one build at a time. It returns when the queue has nothing more to do.
func (d *Daemon) Tick(ctx context.Context) {
	d.mu.Lock()
	repo := d.repo
	d.mu.Unlock()
	if repo == nil {
		return
	}
	d.queue.SetRetryDelay(repo.Global.RetryDelay.Std())
	if !d.catchUpDone {
		d.catchUpDone = true
		d.catchUp(ctx, repo)
	}
	now := d.now()
	for _, k := range d.keys() {
		img, err := d.store.ImageState(k.Container, k.Flavor)
		if err != nil {
			d.log.Error("cannot read state", "key", k, "err", err)
			continue
		}
		eff := repo.Containers[k.Container].Effective(repo.Global)
		dec := schedule.Decide(repo.Global.Slots, policyOf(eff), img.State, now, schedule.Jitter(k.String(), d.opts.Jitter))
		if dec.Run {
			d.queue.Enqueue(k, dec.Kind)
		}
	}
	for ctx.Err() == nil {
		now = d.now()
		_, inSlot := schedule.CurrentWindow(repo.Global.Slots, now)
		job, ok := d.queue.Next(now, inSlot)
		if !ok {
			break
		}
		d.runJob(ctx, repo, job)
	}
	d.maybeGC(ctx, repo)
}

// Errors returned by Trigger, for mapping to API status codes.
var (
	ErrNotReady = errors.New("no config repo loaded yet")
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid request")
)

// RequestGC asks for a garbage collection at the next scheduler pass, even
// outside a slot (a manual request overrides gc_slot_only).
func (d *Daemon) RequestGC() {
	d.mu.Lock()
	d.gcPending, d.gcForce = true, true
	d.mu.Unlock()
	d.Wake()
}

// catchUp runs once, when the daemon first has a config (at start from the checkout on disk,
// or after the first fetch). Slots exist to keep builds out of working hours, but after a
// start there is a backlog and someone is waiting for it, so it is built right away:
// first everything that is absent (never built, or its alias tag is missing from the
// registry), then what is due for a refresh or rebuild. Parents still go before their
// children, and parked jobs stay parked.
func (d *Daemon) catchUp(ctx context.Context, repo *config.Repo) {
	type job struct {
		key  schedule.Key
		kind schedule.Kind
	}
	var absent, due []job
	now := d.now()
	for _, k := range d.keys() {
		img, err := d.store.ImageState(k.Container, k.Flavor)
		if err != nil {
			d.log.Error("cannot read state", "key", k, "err", err)
			continue
		}
		if img.LastRebuild.IsZero() || d.aliasMissing(ctx, k) {
			absent = append(absent, job{k, schedule.Rebuild})
			continue
		}
		eff := repo.Containers[k.Container].Effective(repo.Global)
		if kind, _ := schedule.Pick(policyOf(eff), img.State, now); kind != schedule.None {
			due = append(due, job{k, kind})
		}
	}
	if len(absent)+len(due) == 0 {
		return
	}
	d.log.Info("startup catch-up: building now, outside the slots", "absent", len(absent), "due", len(due))
	for _, j := range append(absent, due...) {
		d.queue.EnqueueNow(j.key, j.kind)
	}
}

// aliasMissing reports whether the registry certainly lacks the moving tag of a key that the
// state says was built (a wiped or replaced registry). Errors count as "present": a registry
// that cannot be asked must not cause a rebuild of everything.
func (d *Daemon) aliasMissing(ctx context.Context, k schedule.Key) bool {
	tags, err := d.registry.Tags(ctx, k.Container)
	if err != nil {
		d.log.Warn("cannot list registry tags", "container", k.Container, "err", err)
		return false
	}
	alias := build.Alias(k.Flavor)
	for _, t := range tags {
		if t == alias {
			return false
		}
	}
	return true
}

// Trigger queues a job right now, bypassing slots, and unparks it.
//
// Children build FROM <registry>/<parent>:latest, so the transitive parents that were never
// built are queued as well (also manual); the queue builds them before the child. Their names
// are returned.
func (d *Daemon) Trigger(container, flavor string, kind schedule.Kind) (alsoQueued []string, err error) {
	d.mu.Lock()
	repo := d.repo
	d.mu.Unlock()
	if repo == nil {
		return nil, ErrNotReady
	}
	c, ok := repo.Containers[container]
	if !ok {
		return nil, fmt.Errorf("%w: unknown container %q", ErrNotFound, container)
	}
	if flavor == "" {
		flavor = config.DefaultFlavor
	}
	known := false
	for _, f := range c.FlavorNames() {
		known = known || f == flavor
	}
	if !known {
		return nil, fmt.Errorf("%w: container %q has no flavor %q", ErrNotFound, container, flavor)
	}
	if kind != schedule.Rebuild && kind != schedule.Refresh {
		return nil, fmt.Errorf("%w: kind must be rebuild or refresh", ErrInvalid)
	}
	for _, parent := range ancestors(repo, container) {
		pk := schedule.Key{Container: parent, Flavor: config.DefaultFlavor}
		img, err := d.store.ImageState(pk.Container, pk.Flavor)
		if err != nil {
			return nil, err
		}
		if img.LastRebuild.IsZero() { // never built: <parent>:latest does not exist yet
			d.queue.Trigger(pk, schedule.Rebuild)
			if d.store != nil {
				d.store.DeleteQueueState(pk)
			}
			alsoQueued = append(alsoQueued, parent)
		}
	}
	k := schedule.Key{Container: container, Flavor: flavor}
	d.queue.Trigger(k, kind)
	if d.store != nil {
		d.store.DeleteQueueState(k)
	}
	d.Wake()
	return alsoQueued, nil
}

// ancestors returns the transitive depends_on targets of name, parents before children.
func ancestors(repo *config.Repo, name string) []string {
	var out []string
	seen := map[string]bool{name: true}
	var visit func(n string)
	visit = func(n string) {
		c := repo.Containers[n]
		if c == nil {
			return
		}
		for _, p := range c.DependsOn {
			if seen[p] {
				continue
			}
			seen[p] = true
			visit(p)
			out = append(out, p)
		}
	}
	visit(name)
	return out
}

func (d *Daemon) runJob(ctx context.Context, repo *config.Repo, job schedule.Job) {
	k := job.Key
	c, ok := repo.Containers[k.Container]
	if !ok { // removed from the config while queued
		d.queue.Done(k)
		return
	}
	log := d.log.With("job", k.String(), "kind", job.Kind.String())
	log.Info("build starting")
	var out tailBuffer
	out.max = state.MaxLogBytes
	started := d.now()
	res, err := d.builder.Run(ctx, build.Job{
		Container: k.Container, Flavor: k.Flavor, Kind: job.Kind,
		ContextDir: repo.ContainerDir(k.Container), Config: c,
	}, &out)
	finished := d.now()

	rec := state.JobRecord{Container: k.Container, Flavor: k.Flavor, Kind: job.Kind,
		Started: started, Finished: finished, OK: err == nil, Log: out.String()}
	if res != nil {
		rec.Kind, rec.DateTag, rec.Digest, rec.Note = res.Kind, res.DateTag, res.Digest, res.Note
	}
	if err != nil {
		rec.Error = err.Error()
	}
	if _, rerr := d.store.RecordJob(rec); rerr != nil {
		log.Error("cannot record job", "err", rerr)
	}

	if err != nil {
		d.jobFailed(ctx, k, job, err)
		return
	}
	d.queue.Done(k)
	if d.store != nil {
		d.store.DeleteQueueState(k)
	}
	d.mu.Lock()
	d.lastRun[k] = fmt.Sprintf("%s ok %s", rec.Kind, finished.Format(time.RFC3339))
	wasFailing := d.failing[k]
	delete(d.failing, k)
	d.mu.Unlock()
	log.Info("build finished", "tag", rec.DateTag, "digest", rec.Digest, "took", finished.Sub(started).Round(time.Second))
	if wasFailing {
		d.notify(ctx, notify.Event{Type: notify.Recovered, Title: k.String() + " builds again",
			Message: fmt.Sprintf("%s succeeded (%s)", rec.Kind, rec.DateTag)})
	}
	d.prune(ctx, repo, k.Container)
}

func (d *Daemon) jobFailed(ctx context.Context, k schedule.Key, job schedule.Job, err error) {
	d.log.Error("build failed", "job", k.String(), "err", err)
	d.mu.Lock()
	d.lastRun[k] = fmt.Sprintf("%s failed %s", job.Kind, d.now().Format(time.RFC3339))
	d.mu.Unlock()
	if !d.queue.Fail(k, d.now(), err.Error()) {
		return // first failure: silent retry later
	}
	d.mu.Lock()
	d.failing[k] = true // reported, so a later success is worth a "recovered" notice
	d.mu.Unlock()
	if d.store != nil {
		d.store.PutQueueState(state.QueueState{Key: k, Status: schedule.Parked.String(), Failures: 2,
			Error: err.Error(), Kind: job.Kind})
	}
	typ := notify.JobParked
	var te *build.TestError
	if errors.As(err, &te) {
		typ = notify.TestFailed
	}
	d.notify(ctx, notify.Event{Type: typ, Title: k.String() + " failed twice",
		Message: fmt.Sprintf("%s failed and is parked until the next push or a manual trigger.\n%s", job.Kind, firstLines(err.Error(), 8))})
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "\n")
}

// prune applies retention to a container's repository; GC follows in maybeGC.
func (d *Daemon) prune(ctx context.Context, repo *config.Repo, container string) {
	keep := repo.Containers[container].Effective(repo.Global).Keep
	removed, err := d.registry.Prune(ctx, container, keep)
	if err != nil {
		d.log.Error("prune failed", "container", container, "err", err)
		d.notify(ctx, notify.Event{Type: notify.PruneFailed, Title: "registry pruning failed",
			Message: fmt.Sprintf("%s: %v", container, err)})
	}
	if len(removed) > 0 {
		d.log.Info("pruned old images", "container", container, "tags", removed)
		d.mu.Lock()
		d.gcPending = true
		d.mu.Unlock()
	}
}

// maybeGC runs garbage collection once the queue is idle, inside a slot when
// registry.gc_slot_only is set. It holds the build lock implicitly: Tick is the
// only place that builds and pushes.
func (d *Daemon) maybeGC(ctx context.Context, repo *config.Repo) {
	d.mu.Lock()
	pending, force := d.gcPending, d.gcForce
	d.mu.Unlock()
	if !pending || len(d.queue.Pending()) > 0 {
		return
	}
	if _, inSlot := schedule.CurrentWindow(repo.Global.Slots, d.now()); !inSlot && !force &&
		repo.Global.Registry.GCSlotOnly != nil && *repo.Global.Registry.GCSlotOnly {
		return
	}
	if err := d.registry.GC(ctx); err != nil {
		d.log.Error("registry GC failed", "err", err)
		d.notify(ctx, notify.Event{Type: notify.PruneFailed, Title: "registry GC failed", Message: err.Error()})
		return
	}
	d.mu.Lock()
	d.gcPending, d.gcForce = false, false
	d.mu.Unlock()
	d.log.Info("registry GC done")
}

// JobStatus describes one (container, flavor) for the status page.
type JobStatus struct {
	Key      schedule.Key
	State    string // schedule.Status as text, or "idle"
	Error    string
	LastRun  string
	Image    state.Image
	NextKind schedule.Kind
	NextRun  time.Time
}

// Status is a snapshot for the status page and `prebake status`.
type Status struct {
	Repo RepoStatus
	Jobs []JobStatus
}

func (d *Daemon) Status() Status {
	d.mu.Lock()
	repo := d.repo
	st := Status{Repo: d.repoStatus}
	last := map[schedule.Key]string{}
	for k, v := range d.lastRun {
		last[k] = v
	}
	d.mu.Unlock()
	if repo == nil {
		return st
	}
	now := d.now()
	for _, k := range d.keys() {
		img, _ := d.store.ImageState(k.Container, k.Flavor)
		s, msg := d.queue.StatusOf(k)
		eff := repo.Containers[k.Container].Effective(repo.Global)
		kind, at := schedule.NextRun(repo.Global.Slots, policyOf(eff), img.State, now)
		st.Jobs = append(st.Jobs, JobStatus{Key: k, State: s.String(), Error: msg, LastRun: last[k], Image: img, NextKind: kind, NextRun: at})
	}
	sort.Slice(st.Jobs, func(i, j int) bool { return st.Jobs[i].Key.String() < st.Jobs[j].Key.String() })
	return st
}
