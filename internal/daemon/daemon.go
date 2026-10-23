// Package daemon wires the pieces together: it follows the config repo, keeps
// the job queue fed from the schedule, runs builds one at a time and prunes the
// registry afterwards (docs/design.md §6, §7, §10, §11).
package daemon

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/schorsch3000/prebake/internal/build"
	"github.com/schorsch3000/prebake/internal/config"
	"github.com/schorsch3000/prebake/internal/notify"
	"github.com/schorsch3000/prebake/internal/schedule"
	"github.com/schorsch3000/prebake/internal/state"
)

// Source provides the config repo; *gitsync.Syncer implements it.
type Source interface {
	Fetch(ctx context.Context) (head string, err error)
	Export(ctx context.Context, sha string) (path string, err error)
	Promote(sha string) error
	Current() (path, sha string, ok bool)
	Cleanup(keep int) error
	// Contains reports whether commit sha is part of the followed branch as fetched so far.
	Contains(ctx context.Context, sha string) (bool, error)
	// FollowedBranch is the branch the daemon follows.
	FollowedBranch() string
}

// Builder runs one job; *build.Runner implements it.
type Builder interface {
	Run(ctx context.Context, job build.Job, log io.Writer) (*build.Result, error)
}

// Registry is what the daemon needs from the embedded registry.
type Registry interface {
	// Prune untags the date tags of repo beyond keep and returns them.
	Prune(ctx context.Context, repo string, keep int) ([]string, error)
	// Tags lists the tags of repo (none for an unknown repository).
	Tags(ctx context.Context, repo string) ([]string, error)
	// GC collects unreferenced manifests and blobs; the build lock is held by the caller.
	GC(ctx context.Context) error
}

// Options configures a Daemon. Zero values get sensible defaults.
type Options struct {
	PollInterval  time.Duration // repo fetch interval (default 5m)
	TickInterval  time.Duration // scheduler evaluation interval (default 30s)
	Jitter        time.Duration // max start offset after a slot opens (default 15m)
	KeepCheckouts int           // exported commits to keep on disk (default 3)
	Logger        *slog.Logger
	Now           func() time.Time
}

func (o *Options) defaults() {
	if o.PollInterval <= 0 {
		o.PollInterval = 5 * time.Minute
	}
	if o.TickInterval <= 0 {
		o.TickInterval = 30 * time.Second
	}
	if o.Jitter < 0 {
		o.Jitter = 0
	} else if o.Jitter == 0 {
		o.Jitter = 15 * time.Minute
	}
	if o.KeepCheckouts <= 0 {
		o.KeepCheckouts = 3
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Daemon is the long-running service.
type Daemon struct {
	opts     Options
	log      *slog.Logger
	src      Source
	builder  Builder
	registry Registry
	store    *state.Store
	notifier notify.Notifier
	queue    *schedule.Queue

	mu         sync.Mutex
	repo       *config.Repo
	repoSHA    string
	repoStatus RepoStatus
	badSHA     string                // last commit we reported as invalid
	failing    map[schedule.Key]bool // keys with an unreported-recovery failure
	gcPending  bool
	gcForce    bool                       // manual request: ignore gc_slot_only
	lastRun    map[schedule.Key]string    // last result summary for status
	running    map[schedule.Key]time.Time // builds in progress and when they started
	needs      map[schedule.Key]candidate // why work is pending, from the last scheduler pass
	logs       map[schedule.Key]*LiveLog  // output of the current or last build in this process

	backoff time.Duration // current sync retry backoff
	syncMu  sync.Mutex    // one fetch at a time: the poll loop and `prebake sync` must not interleave
	wake    chan struct{}
}

// New creates a daemon. notifier may be nil.
func New(opts Options, src Source, b Builder, reg Registry, store *state.Store, n notify.Notifier) *Daemon {
	opts.defaults()
	if n == nil {
		n = notify.Nop{}
	}
	return &Daemon{
		opts: opts, log: opts.Logger, src: src, builder: b, registry: reg, store: store, notifier: n,
		queue:   schedule.NewQueue(15*time.Minute, nil),
		failing: map[schedule.Key]bool{},
		lastRun: map[schedule.Key]string{},
		running: map[schedule.Key]time.Time{},
		logs:    map[schedule.Key]*LiveLog{},
		wake:    make(chan struct{}, 1),
	}
}

func (d *Daemon) now() time.Time { return d.opts.Now() }

// Wake asks the scheduler loop to run now instead of at the next tick.
func (d *Daemon) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run loads the last good checkout (so the daemon works offline), then runs the
// sync loop and the scheduler loop until ctx ends.
func (d *Daemon) Run(ctx context.Context) error {
	d.loadCurrent()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.syncLoop(ctx)
	}()
	d.schedulerLoop(ctx)
	wg.Wait()
	return ctx.Err()
}

func (d *Daemon) schedulerLoop(ctx context.Context) {
	t := time.NewTicker(d.opts.TickInterval)
	defer t.Stop()
	for {
		d.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.wake:
		}
	}
}

func (d *Daemon) notify(ctx context.Context, e notify.Event) {
	d.mu.Lock()
	repo := d.repo
	d.mu.Unlock()
	if repo != nil {
		p := repo.Global.Notify.Pushover
		if e.Type.Failure() {
			e.Priority = *p.PriorityFailure
		} else {
			e.Priority = *p.PriorityRecovery
		}
	} else if e.Type.Failure() {
		e.Priority = 1
	} else {
		e.Priority = -1
	}
	if err := d.notifier.Notify(ctx, e); err != nil {
		d.log.Error("notification failed", "type", e.Type, "err", err)
	}
}
