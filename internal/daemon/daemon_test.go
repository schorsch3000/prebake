package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/schorsch3000/prebake/internal/build"
	"github.com/schorsch3000/prebake/internal/gitsync"
	"github.com/schorsch3000/prebake/internal/notify"
	"github.com/schorsch3000/prebake/internal/schedule"
	"github.com/schorsch3000/prebake/internal/state"
)

// --- fakes ---------------------------------------------------------------------------

type fakeBuilder struct {
	onRun func(build.Job) // called while the build "runs"
	reg   *fakeRegistry   // gets the alias tag of every successful build, like the real registry
	mu    sync.Mutex
	runs  []string        // "<container>:<flavor> <kind>"
	fail  map[string]int  // container -> remaining failures
	testF map[string]bool // container -> failure is a smoke test failure
}

func (f *fakeBuilder) Run(_ context.Context, j build.Job, log io.Writer) (*build.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, j.Container+":"+j.Flavor+" "+j.Kind.String())
	io.WriteString(log, "building "+j.Container+"\n")
	if f.onRun != nil {
		f.mu.Unlock() // the callback looks at the daemon, which must not wait for this lock
		f.onRun(j)
		f.mu.Lock()
	}
	if f.fail[j.Container] > 0 {
		f.fail[j.Container]--
		if f.testF[j.Container] {
			return nil, &build.TestError{Cmd: "false", Output: "nope", Err: errors.New("exit 1")}
		}
		return nil, errors.New("apt exploded")
	}
	if f.reg != nil {
		f.reg.addTag(j.Container, build.Alias(j.Flavor))
	}
	return &build.Result{Kind: j.Kind, DateTag: j.Flavor + "-20261022-120000", Alias: build.Alias(j.Flavor), Digest: "sha256:" + j.Container}, nil
}

func (f *fakeBuilder) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.runs
	f.runs = nil
	return r
}

type fakeRegistry struct {
	mu      sync.Mutex
	pruned  []string // "<repo> keep=N"
	gcs     int
	removed map[string][]string
	tags    map[string][]string
}

func (r *fakeRegistry) addTag(repo, tag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tags[repo] = append(r.tags[repo], tag)
}

func (r *fakeRegistry) Tags(_ context.Context, repo string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tags[repo]...), nil
}

func (r *fakeRegistry) Prune(_ context.Context, repo string, keep int) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruned = append(r.pruned, repo+" keep="+itoa(keep))
	removed := r.removed[repo]
	delete(r.removed, repo) // reported once, like a real registry
	return removed, nil
}
func (r *fakeRegistry) GC(context.Context) error { r.mu.Lock(); r.gcs++; r.mu.Unlock(); return nil }

func itoa(n int) string { return string(rune('0' + n)) }

type recNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (n *recNotifier) Notify(_ context.Context, e notify.Event) error {
	n.mu.Lock()
	n.events = append(n.events, e)
	n.mu.Unlock()
	return nil
}
func (n *recNotifier) types() []notify.Type {
	n.mu.Lock()
	defer n.mu.Unlock()
	var t []notify.Type
	for _, e := range n.events {
		t = append(t, e.Type)
	}
	return t
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// --- harness ------------------------------------------------------------------------

type harness struct {
	t     *testing.T
	work  string // working clone of the origin
	data  string
	d     *Daemon
	b     *fakeBuilder
	reg   *fakeRegistry
	n     *recNotifier
	clk   *clock
	store *state.Store
	src   *gitsync.Syncer
}

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) push(msg string, files map[string]string) {
	h.t.Helper()
	for rel, c := range files {
		write(h.t, h.work, rel, c)
	}
	sh(h.t, h.work, "add", "-A")
	sh(h.t, h.work, "commit", "-q", "-m", msg)
	sh(h.t, h.work, "push", "-q", "origin", "HEAD:main")
}

const globalYAML = `slots: ["00:00-23:59"]
retry_delay: 15m
keep: 4
registry:
  gc_slot_only: true
`

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	bare, work := filepath.Join(root, "origin.git"), filepath.Join(root, "work")
	sh(t, root, "init", "-q", "--bare", "-b", "main", bare)
	sh(t, root, "clone", "-q", bare, work)
	h := &harness{t: t, work: work, data: filepath.Join(root, "data")}
	h.push("initial", map[string]string{
		"prebake.yaml":                    globalYAML,
		"containers/base/Dockerfile":      "FROM debian\n",
		"containers/base/container.yaml":  "tests: [\"true\"]\nkeep: 2\n",
		"containers/child/Dockerfile":     "ARG PARENT\nFROM ${PARENT}\n",
		"containers/child/container.yaml": "depends_on: [base]\ntests: [\"true\"]\n",
	})
	h.start()
	return h
}

// start creates (or re-creates, simulating a restart) the daemon on the same data dir.
func (h *harness) start() {
	t := h.t
	if h.store != nil {
		h.store.Close()
	}
	store, err := state.Open(filepath.Join(h.data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if h.clk == nil {
		h.clk = &clock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	}
	if h.reg == nil { // the registry lives in the data volume and survives a daemon restart
		h.reg = &fakeRegistry{removed: map[string][]string{}, tags: map[string][]string{}}
	}
	h.b = &fakeBuilder{fail: map[string]int{}, testF: map[string]bool{}, reg: h.reg}
	h.n = &recNotifier{}
	h.store = store
	h.src = &gitsync.Syncer{URL: filepath.Join(filepath.Dir(h.work), "origin.git"), Branch: "main", Dir: filepath.Join(h.data, "repo")}
	h.d = New(Options{Jitter: -1, Now: h.clk.now, Logger: quietLogger()}, h.src, h.b, h.reg, store, h.n)
	h.d.loadCurrent()
}

func (h *harness) sync() {
	h.t.Helper()
	if err := h.d.SyncOnce(context.Background()); err != nil {
		h.t.Fatalf("SyncOnce: %v", err)
	}
}

func (h *harness) tick() { h.d.Tick(context.Background()) }

func jobState(s Status, key string) JobStatus {
	for _, j := range s.Jobs {
		if j.Key.String() == key {
			return j
		}
	}
	return JobStatus{}
}

// --- tests --------------------------------------------------------------------------

func TestFirstSyncBuildsEverythingParentFirst(t *testing.T) {
	h := newHarness(t)
	h.tick()
	if got := h.b.take(); len(got) != 0 {
		t.Fatalf("nothing is loaded before the first sync: %v", got)
	}
	h.sync()
	if s := h.d.Status(); s.Repo.State != RepoOK || s.Repo.Active == "" {
		t.Fatalf("repo status: %+v", s.Repo)
	}
	h.tick()
	got := h.b.take()
	if len(got) != 2 || got[0] != "base:default rebuild" || got[1] != "child:default rebuild" {
		t.Fatalf("builds = %v (never-built images are rebuilt, parent first)", got)
	}
	// results are recorded and drive the schedule: an immediate second tick is idle
	h.tick()
	if got := h.b.take(); len(got) != 0 {
		t.Errorf("second tick built %v", got)
	}
	img, _ := h.store.ImageState("base", "default")
	if img.LastRebuild.IsZero() || img.Digest != "sha256:base" {
		t.Errorf("state: %+v", img)
	}
	hist, _ := h.store.History("base", "default", 5)
	if len(hist) != 1 || !hist[0].OK || !strings.Contains(hist[0].Log, "building base") {
		t.Errorf("history: %+v", hist)
	}
	// retention uses the effective keep: base has 2, child inherits the global 4
	if strings.Join(h.reg.pruned, ",") != "base keep=2,child keep=4" {
		t.Errorf("pruned = %v", h.reg.pruned)
	}
}

func TestRefreshWhenDue(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.tick()
	h.b.take()
	h.clk.advance(25 * time.Hour) // refresh interval (1d) has passed, rebuild (7d) has not
	h.tick()
	got := h.b.take()
	if len(got) != 2 || got[0] != "base:default refresh" || got[1] != "child:default refresh" {
		t.Errorf("builds = %v", got)
	}
	h.clk.advance(7 * 24 * time.Hour)
	h.tick()
	if got := h.b.take(); len(got) != 2 || got[0] != "base:default rebuild" {
		t.Errorf("after a week: %v", got)
	}
}

func TestNotDueWaitsForTheSlotTriggerDoesNot(t *testing.T) {
	h := narrowSlotHarness(t) // 12:00 is outside the 02:00-04:00 slot
	h.seedBuilt("base", schedule.Rebuild, 10*time.Hour)
	h.seedBuilt("child", schedule.Rebuild, 10*time.Hour)
	h.seedBuilt("solo", schedule.Rebuild, 10*time.Hour)
	h.sync()
	h.tick() // nothing is due (refresh in 14h), nothing is absent
	if got := h.b.take(); len(got) != 0 {
		t.Fatalf("builds outside the slot with nothing due: %v", got)
	}
	if _, err := h.d.Trigger("base", "", schedule.Rebuild); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if got := h.b.take(); len(got) != 1 || got[0] != "base:default rebuild" {
		t.Errorf("trigger: %v", got)
	}
	// 02:00 next day, in the slot: child and solo fall due exactly then; base (rebuilt at 12:00
	// by the trigger) is due at 12:00, which is before the next slot, so the slot rule runs it now
	// as well, instead of letting it run late
	h.clk.advance(14 * time.Hour)
	h.tick()
	if got := h.b.take(); len(got) != 3 {
		t.Errorf("in slot: %v", got)
	}
}

func TestSlotsArePreferredNotRequired(t *testing.T) {
	h := narrowSlotHarness(t)                           // 12:00 is outside the slot
	h.seedBuilt("base", schedule.Rebuild, 30*time.Hour) // refresh was due 6h ago: the slot was missed
	h.seedBuilt("child", schedule.Rebuild, 30*time.Hour)
	h.seedBuilt("solo", schedule.Rebuild, time.Hour)
	h.sync()
	h.tick()
	got := h.b.take()
	if len(got) != 2 || got[0] != "base:default refresh" || got[1] != "child:default refresh" {
		t.Errorf("what is overdue runs now, parent first, even outside the slot: %v", got)
	}
}

func TestTriggerValidation(t *testing.T) {
	h := newHarness(t)
	if _, err := h.d.Trigger("base", "", schedule.Rebuild); !errors.Is(err, ErrNotReady) {
		t.Errorf("no repo loaded yet: %v", err)
	}
	h.sync()
	for _, c := range []struct {
		container, flavor string
		kind              schedule.Kind
		want              error
	}{
		{"nope", "", schedule.Rebuild, ErrNotFound},
		{"base", "cuda", schedule.Rebuild, ErrNotFound},
		{"base", "", schedule.None, ErrInvalid},
	} {
		if _, err := h.d.Trigger(c.container, c.flavor, c.kind); !errors.Is(err, c.want) {
			t.Errorf("%+v: got %v, want %v", c, err, c.want)
		}
	}
}

func TestRequestGCOverridesSlotOnly(t *testing.T) {
	h := newHarness(t)
	h.push("narrow slot", map[string]string{"prebake.yaml": "slots: [\"02:00-04:00\"]\nkeep: 4\nregistry:\n  gc_slot_only: true\n"})
	h.sync()
	h.tick() // 12:00, outside the slot
	h.d.RequestGC()
	h.tick()
	if h.reg.gcs != 1 {
		t.Errorf("manual GC outside the slot: %d", h.reg.gcs)
	}
	h.tick()
	if h.reg.gcs != 1 {
		t.Errorf("a manual request is consumed: %d", h.reg.gcs)
	}
}

func TestInvalidPushKeepsLastGoodAndNotifiesOnce(t *testing.T) {
	h := newHarness(t)
	h.sync()
	good := h.d.Status().Repo.Active

	h.push("stray file", map[string]string{"containers/base/junk.txt": "x"})
	h.sync()
	h.sync() // same bad commit again: no second notification
	s := h.d.Status()
	if s.Repo.State != RepoDegraded || s.Repo.Active != good || s.Repo.Head == good || !strings.Contains(s.Repo.Error, "junk.txt") {
		t.Fatalf("repo status: %+v", s.Repo)
	}
	if got := h.n.types(); len(got) != 1 || got[0] != notify.RepoInvalid {
		t.Fatalf("events = %v", got)
	}
	h.tick() // still builds from the last good commit
	if got := h.b.take(); len(got) != 2 {
		t.Errorf("builds on last good: %v", got)
	}

	h.push("fix", map[string]string{"containers/base/junk.txt": ""})
	sh(t, h.work, "rm", "-q", "-f", "containers/base/junk.txt")
	sh(t, h.work, "commit", "-q", "-m", "remove junk")
	sh(t, h.work, "push", "-q", "origin", "HEAD:main")
	h.sync()
	if s := h.d.Status(); s.Repo.State != RepoOK || s.Repo.Active == good {
		t.Errorf("after the fix: %+v", s.Repo)
	}
	if got := h.n.types(); len(got) != 2 || got[1] != notify.Recovered {
		t.Errorf("events = %v", got)
	}
}

func TestFailureRetryParkSkipAndUnpark(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.b.fail["base"] = 2

	h.tick() // base fails (1st): silent, child waits behind its parent
	if got := h.b.take(); len(got) != 1 || got[0] != "base:default rebuild" {
		t.Fatalf("builds = %v", got)
	}
	if len(h.n.types()) != 0 {
		t.Fatalf("first failure must be silent: %v", h.n.types())
	}
	if j := jobState(h.d.Status(), "base:default"); j.State != "retrying" {
		t.Errorf("state = %q", j.State)
	}
	h.clk.advance(5 * time.Minute)
	h.tick()
	if got := h.b.take(); len(got) != 0 {
		t.Fatalf("retry delay not over yet, got %v", got)
	}
	h.clk.advance(11 * time.Minute) // 16m since the failure
	h.tick()                        // base fails again: parked, notified once, child skipped
	if got := h.b.take(); len(got) != 1 {
		t.Fatalf("builds = %v", got)
	}
	if got := h.n.types(); len(got) != 1 || got[0] != notify.JobParked {
		t.Fatalf("events = %v", got)
	}
	st := h.d.Status()
	if j := jobState(st, "base:default"); j.State != "parked" || !strings.Contains(j.Error, "apt exploded") {
		t.Errorf("base: %+v", j)
	}
	if j := jobState(st, "child:default"); j.State != "skipped" {
		t.Errorf("child: %+v", j)
	}
	h.clk.advance(48 * time.Hour)
	h.tick()
	if got := h.b.take(); len(got) != 0 {
		t.Errorf("parked jobs stay parked, got %v", got)
	}

	// a restart keeps it parked
	h.start()
	h.tick()
	if got := h.b.take(); len(got) != 0 {
		t.Errorf("after restart: %v", got)
	}
	if j := jobState(h.d.Status(), "base:default"); j.State != "parked" {
		t.Errorf("after restart: %+v", j)
	}

	// a valid push unparks; the (now healthy) builder succeeds, parent before child
	h.push("tweak", map[string]string{"containers/base/container.yaml": "tests: [\"true\"]\nkeep: 3\n"})
	h.sync()
	h.tick()
	got := h.b.take()
	if len(got) != 2 || got[0] != "base:default rebuild" || got[1] != "child:default rebuild" {
		t.Fatalf("after push: %v", got)
	}
	if got := h.n.types(); len(got) != 1 || got[0] != notify.Recovered {
		t.Errorf("events after recovery = %v", got)
	}
}

func TestSilentRetrySuccessIsNotAnnounced(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.b.fail["base"] = 1
	h.tick()
	h.clk.advance(16 * time.Minute)
	h.tick()
	if got := h.b.take(); len(got) != 3 { // fail, then base ok + child
		t.Fatalf("builds = %v", got)
	}
	if got := h.n.types(); len(got) != 0 {
		t.Errorf("a failure that was never reported must not be 'recovered': %v", got)
	}
}

func TestSmokeTestFailureEvent(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.b.fail["base"], h.b.testF["base"] = 2, true
	h.tick()
	h.clk.advance(16 * time.Minute)
	h.tick()
	if got := h.n.types(); len(got) != 1 || got[0] != notify.TestFailed {
		t.Errorf("events = %v", got)
	}
}

func TestManualTriggerUnparks(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.b.fail["base"] = 2
	h.tick()
	h.clk.advance(16 * time.Minute)
	h.tick()
	h.b.take()
	if _, err := h.d.Trigger("base", "", schedule.Rebuild); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if got := h.b.take(); len(got) == 0 || got[0] != "base:default rebuild" {
		t.Errorf("trigger after park: %v", got)
	}
	if j := jobState(h.d.Status(), "base:default"); j.State != "idle" {
		t.Errorf("state = %q", j.State)
	}
	// the persisted parked state is gone too
	if states, _ := h.store.QueueStates(); len(states) != 0 {
		t.Errorf("queue states: %v", states)
	}
}

func TestGCRunsAfterPruneInSlotOnly(t *testing.T) {
	h := newHarness(t)
	h.push("narrow slot", map[string]string{"prebake.yaml": "slots: [\"02:00-04:00\"]\nkeep: 4\nregistry:\n  gc_slot_only: true\n"})
	h.sync()
	h.reg.removed["base"] = []string{"default-20260101-000000"}
	h.d.Trigger("base", "", schedule.Rebuild) // outside the slot
	h.tick()
	if h.reg.gcs != 0 {
		t.Errorf("GC ran outside the slot")
	}
	h.clk.advance(14 * time.Hour) // 02:00
	h.tick()
	if h.reg.gcs != 1 {
		t.Errorf("GC count = %d, want 1 once the slot opens", h.reg.gcs)
	}
	h.tick()
	if h.reg.gcs != 1 {
		t.Errorf("GC must only run when something was pruned: %d", h.reg.gcs)
	}
}

func TestRestartLoadsLastCheckoutOffline(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.tick()
	h.b.take()
	h.start() // restart without ever calling sync again (network down)
	if s := h.d.Status(); len(s.Jobs) != 2 || s.Repo.Active == "" {
		t.Fatalf("status after restart: %+v", s)
	}
	h.clk.advance(25 * time.Hour)
	h.tick()
	if got := h.b.take(); len(got) != 2 || got[0] != "base:default refresh" {
		t.Errorf("builds from disk checkout: %v", got)
	}
}

func TestNextBackoff(t *testing.T) {
	var d time.Duration
	var seq []time.Duration
	for i := 0; i < 8; i++ {
		d = nextBackoff(d)
		seq = append(seq, d)
	}
	if seq[0] != 10*time.Second || seq[1] != 20*time.Second || seq[len(seq)-1] != 5*time.Minute {
		t.Errorf("backoff = %v", seq)
	}
}

func TestNotificationPriorityFromConfig(t *testing.T) {
	h := newHarness(t)
	h.push("priorities", map[string]string{"prebake.yaml": globalYAML + "notify:\n  pushover:\n    priority_failure: 0\n    priority_recovery: -2\n"})
	h.sync()
	h.b.fail["base"] = 2
	h.tick()
	h.clk.advance(16 * time.Minute)
	h.tick() // parked
	h.d.Trigger("base", "", schedule.Rebuild)
	h.tick() // recovers
	h.n.mu.Lock()
	defer h.n.mu.Unlock()
	if len(h.n.events) != 2 || h.n.events[0].Priority != 0 || h.n.events[1].Priority != -2 {
		t.Errorf("events = %+v", h.n.events)
	}
}

func TestNotificationPriorityDefaults(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.b.fail["base"] = 2
	h.tick()
	h.clk.advance(16 * time.Minute)
	h.tick()
	h.n.mu.Lock()
	defer h.n.mu.Unlock()
	if len(h.n.events) != 1 || h.n.events[0].Priority != 1 {
		t.Errorf("events = %+v", h.n.events)
	}
}

func TestTriggerBuildsNeverBuiltParentsFirst(t *testing.T) {
	h := newHarness(t)
	h.push("narrow slot", map[string]string{"prebake.yaml": "slots: [\"02:00-04:00\"]\nkeep: 4\n"})
	h.sync()

	// triggering the child while nothing was ever built: its parent has no <parent>:latest yet,
	// so it is queued too (ignoring the slot, like the trigger itself) and built first
	also, err := h.d.Trigger("child", "", schedule.Rebuild)
	if err != nil || len(also) != 1 || also[0] != "base" {
		t.Fatalf("also queued = %v, err %v", also, err)
	}
	h.tick() // 12:00, outside the slot
	if got := h.b.take(); len(got) != 2 || got[0] != "base:default rebuild" || got[1] != "child:default rebuild" {
		t.Fatalf("builds = %v", got)
	}

	// base is built now: triggering the child alone does not rebuild the parent
	also, err = h.d.Trigger("child", "", schedule.Refresh)
	if err != nil || len(also) != 0 {
		t.Fatalf("also queued = %v, err %v", also, err)
	}
	h.tick()
	if got := h.b.take(); len(got) != 1 || got[0] != "child:default refresh" {
		t.Errorf("builds = %v", got)
	}
}

func TestTriggerParentsAreTransitive(t *testing.T) {
	h := newHarness(t)
	h.push("grandchild", map[string]string{
		"containers/grand/Dockerfile":     "ARG PARENT\nFROM ${PARENT}\n",
		"containers/grand/container.yaml": "depends_on: [child]\ntests: [\"true\"]\n",
	})
	h.sync()
	also, err := h.d.Trigger("grand", "", schedule.Rebuild)
	if err != nil || strings.Join(also, ",") != "base,child" {
		t.Fatalf("also queued = %v, err %v", also, err)
	}
	h.tick()
	if got := strings.Join(h.b.take(), ","); got != "base:default rebuild,child:default rebuild,grand:default rebuild" {
		t.Errorf("builds = %s", got)
	}
}

// --- what must be built now -------------------------------------------------------------------

// seedBuilt records an earlier successful build in the state store, finished `age` ago, and
// registers its alias tag as present in the registry.
func (h *harness) seedBuilt(container string, kind schedule.Kind, age time.Duration) {
	h.t.Helper()
	fin := h.clk.now().Add(-age)
	_, err := h.store.RecordJob(state.JobRecord{Container: container, Flavor: "default", Kind: kind,
		Started: fin.Add(-time.Minute), Finished: fin, OK: true, DateTag: "default-20260101-000000", Digest: "sha256:old"})
	if err != nil {
		h.t.Fatal(err)
	}
	h.reg.addTag(container, "latest")
}

func narrowSlotHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.push("narrow slot, one more container", map[string]string{
		"prebake.yaml":                   "slots: [\"02:00-04:00\"]\nkeep: 4\n",
		"containers/solo/Dockerfile":     "FROM debian\n",
		"containers/solo/container.yaml": "tests: [\"true\"]\n",
	})
	return h
}

func TestAbsentBuiltFirstThenDue(t *testing.T) {
	h := narrowSlotHarness(t)                             // 12:00 is outside the 02:00-04:00 slot
	h.seedBuilt("base", schedule.Rebuild, 3*24*time.Hour) // refresh (1d) is overdue, rebuild (7d) is not
	h.seedBuilt("child", schedule.Rebuild, time.Hour)     // fresh: not due
	// solo was never built
	h.sync()
	h.tick()
	got := h.b.take()
	if len(got) != 2 || got[0] != "solo:default rebuild" || got[1] != "base:default refresh" {
		t.Fatalf("builds = %v, want the absent solo first, then base's refresh", got)
	}
}

func TestAbsentParentsBeforeChildren(t *testing.T) {
	h := narrowSlotHarness(t)
	h.sync()
	h.tick()
	got := h.b.take()
	idx := func(s string) int {
		for i, g := range got {
			if g == s {
				return i
			}
		}
		return -1
	}
	if len(got) != 3 || idx("base:default rebuild") < 0 || idx("child:default rebuild") < idx("base:default rebuild") {
		t.Errorf("everything absent is built, the parent before its child: %v", got)
	}
}

func TestMissingAliasCountsAsAbsent(t *testing.T) {
	h := narrowSlotHarness(t)
	h.seedBuilt("base", schedule.Rebuild, time.Hour)
	h.seedBuilt("child", schedule.Rebuild, time.Hour)
	h.seedBuilt("solo", schedule.Rebuild, time.Hour)
	h.reg.mu.Lock()
	h.reg.tags["base"] = nil // the registry lost base (wiped volume), the state still says it was built
	h.reg.mu.Unlock()
	h.sync()
	h.tick()
	if got := h.b.take(); len(got) != 1 || got[0] != "base:default rebuild" {
		t.Errorf("builds = %v", got)
	}
}

func TestRestartLeavesParkedJobsParked(t *testing.T) {
	h := narrowSlotHarness(t)
	h.sync()
	h.b.fail["base"] = 2
	h.d.Trigger("base", "", schedule.Rebuild) // fails, retries once, parks
	h.tick()
	h.clk.advance(16 * time.Minute)
	h.d.Trigger("base", "", schedule.Rebuild)
	h.b.fail["base"] = 1
	h.tick()
	h.b.take()
	h.clk.advance(16 * time.Minute)
	h.tick()
	h.b.take()
	if j := jobState(h.d.Status(), "base:default"); j.State != "parked" {
		t.Fatalf("setup: base should be parked, is %q", j.State)
	}
	h.start() // restart: catch-up runs, but must not release the parked job
	h.sync()
	h.tick()
	for _, b := range h.b.take() {
		if strings.HasPrefix(b, "base:") || strings.HasPrefix(b, "child:") {
			t.Errorf("a parked job (or its child) was built at startup: %s", b)
		}
	}
	if j := jobState(h.d.Status(), "base:default"); j.State != "parked" {
		t.Errorf("state = %q", j.State)
	}
}

func TestRestartBuildsFromTheCheckoutOnDisk(t *testing.T) {
	h := narrowSlotHarness(t)
	h.sync()
	h.start() // restart with a checkout on disk, nothing built yet, and no sync before the tick
	h.tick()
	if got := h.b.take(); len(got) != 3 {
		t.Errorf("catch-up from the checkout on disk: %v", got)
	}
}

// --- images must never lag behind git ----------------------------------------------------

func TestChangedDockerfileRebuildsAtOnceOutsideTheSlot(t *testing.T) {
	h := narrowSlotHarness(t) // 12:00 is outside the slot
	h.sync()
	h.tick() // everything absent is built, the inputs of each build are recorded
	if got := h.b.take(); len(got) != 3 {
		t.Fatalf("initial builds: %v", got)
	}
	h.tick()
	if got := h.b.take(); len(got) != 0 {
		t.Fatalf("nothing changed: %v", got)
	}
	h.push("change base", map[string]string{"containers/base/Dockerfile": "FROM debian\nRUN echo new\n"})
	h.sync()
	h.tick()
	if got := h.b.take(); len(got) != 1 || got[0] != "base:default rebuild" {
		t.Errorf("the image is stale against git and is rebuilt now, only that one: %v", got)
	}
	h.tick()
	if got := h.b.take(); len(got) != 0 {
		t.Errorf("rebuilt once: %v", got)
	}
}

func TestSchedulerOnlyChangesDoNotRebuild(t *testing.T) {
	h := narrowSlotHarness(t)
	h.sync()
	h.tick()
	h.b.take()
	h.push("tune base", map[string]string{"containers/base/container.yaml": "tests: [\"false\"]\nkeep: 3\nrefresh_cmd: other\nrefresh_interval: 2d\n"})
	h.sync()
	h.tick()
	if got := h.b.take(); len(got) != 0 {
		t.Errorf("container.yaml settings do not change the image: %v", got)
	}
}

func TestNewContainerIsBuiltImmediately(t *testing.T) {
	h := narrowSlotHarness(t)
	h.sync()
	h.tick()
	h.b.take()
	h.push("add tools", map[string]string{
		"containers/tools/Dockerfile":     "FROM debian\n",
		"containers/tools/container.yaml": "tests: [\"true\"]\n",
	})
	h.sync()
	h.tick() // 12:00, outside the slot
	if got := h.b.take(); len(got) != 1 || got[0] != "tools:default rebuild" {
		t.Errorf("an absent image must not wait for a slot: %v", got)
	}
}

func TestChangedFlavorArgsRebuildOnlyThatFlavor(t *testing.T) {
	h := newHarness(t)
	h.push("flavors", map[string]string{
		"containers/gpu/Dockerfile":     "ARG G\nFROM debian\n",
		"containers/gpu/container.yaml": "flavors:\n  default: {}\n  cuda:\n    args: {G: \"1\"}\n",
	})
	h.sync()
	h.tick()
	h.b.take()
	h.push("cuda args", map[string]string{
		"containers/gpu/container.yaml": "flavors:\n  default: {}\n  cuda:\n    args: {G: \"2\"}\n",
	})
	h.sync()
	h.tick()
	if got := h.b.take(); len(got) != 1 || got[0] != "gpu:cuda rebuild" {
		t.Errorf("builds = %v", got)
	}
}

func TestImageBuiltBeforeHashesExistIsAdoptedNotRebuilt(t *testing.T) {
	h := narrowSlotHarness(t)
	h.seedBuilt("base", schedule.Rebuild, time.Hour) // no inputs hash recorded (older version)
	h.seedBuilt("child", schedule.Rebuild, time.Hour)
	h.seedBuilt("solo", schedule.Rebuild, time.Hour)
	h.sync()
	h.tick()
	if got := h.b.take(); len(got) != 0 {
		t.Fatalf("an unknown hash is adopted, not treated as stale: %v", got)
	}
	h.push("change solo", map[string]string{"containers/solo/Dockerfile": "FROM debian\nRUN true\n"})
	h.sync()
	h.tick()
	if got := h.b.take(); len(got) != 1 || got[0] != "solo:default rebuild" {
		t.Errorf("after adoption, changes are detected: %v", got)
	}
}

func TestRefreshOfAStaleImageDoesNotHideTheStaleness(t *testing.T) {
	h := narrowSlotHarness(t)
	h.sync()
	h.tick()
	h.b.take()
	before, _ := h.store.ImageState("base", "default")
	h.push("change base", map[string]string{"containers/base/Dockerfile": "FROM debian\nRUN echo new\n"})
	h.sync()
	// a refresh that succeeds while the image is stale (queued by hand, before the scheduler
	// got to it) stacks on the OLD image: it must not record the new inputs
	h.d.mu.Lock()
	repo := h.d.repo
	h.d.mu.Unlock()
	h.d.runJob(context.Background(), repo, schedule.Job{Key: schedule.Key{Container: "base", Flavor: "default"}, Kind: schedule.Refresh})
	after, _ := h.store.ImageState("base", "default")
	if after.InputsHash != before.InputsHash || after.InputsHash == "" {
		t.Fatalf("a refresh changed the recorded inputs: %q -> %q", before.InputsHash, after.InputsHash)
	}
	h.b.take()
	h.tick()
	if got := h.b.take(); len(got) != 1 || got[0] != "base:default rebuild" {
		t.Errorf("the image is still stale and must be rebuilt: %v", got)
	}
}

func TestStatusShowsTheBuildInProgress(t *testing.T) {
	h := newHarness(t)
	h.sync()
	var during Status
	h.b.onRun = func(j build.Job) {
		if j.Container == "base" {
			during = h.d.Status()
		}
	}
	h.tick()
	var base JobStatus
	for _, j := range during.Jobs {
		if j.Key.Container == "base" {
			base = j
		}
	}
	if base.State != "building" || !base.RunningSince.Equal(h.clk.now()) {
		t.Errorf("during the build: %+v", base)
	}
	after := h.d.Status()
	for _, j := range after.Jobs {
		if !j.RunningSince.IsZero() {
			t.Errorf("nothing runs after the build: %+v", j)
		}
	}
}

// --- logs ---------------------------------------------------------------------------------

func TestLogsFollowStreamsALiveBuild(t *testing.T) {
	h := newHarness(t)
	h.sync()
	started, release := make(chan struct{}), make(chan struct{})
	h.b.onRun = func(j build.Job) {
		if j.Container == "base" {
			close(started)
			<-release
		}
	}
	tickDone := make(chan struct{})
	go func() { h.d.Tick(context.Background()); close(tickDone) }()
	<-started

	ls, err := h.d.Logs("base", "", true)
	if err != nil {
		t.Fatal(err)
	}
	var got syncBuf
	streamDone := make(chan error, 1)
	go func() { streamDone <- ls.Stream(context.Background(), &got, true, 0, nil) }()
	waitFor(t, "output of the running build", func() bool { return strings.Contains(got.String(), "building base") })
	select {
	case <-streamDone:
		t.Fatal("the stream ended while the build is still running")
	default:
	}
	close(release)
	<-tickDone
	select {
	case err := <-streamDone:
		if err != nil {
			t.Errorf("stream: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream must end with the build")
	}
	if strings.Contains(got.String(), "building child") {
		t.Errorf("only the followed job's output: %q", got.String())
	}
}

func TestLogsFollowWaitsForAQueuedJob(t *testing.T) {
	h := newHarness(t)
	h.sync()
	started, release := make(chan struct{}), make(chan struct{})
	h.b.onRun = func(j build.Job) {
		if j.Container == "base" {
			close(started)
			<-release
		}
	}
	tickDone := make(chan struct{})
	go func() { h.d.Tick(context.Background()); close(tickDone) }()
	<-started
	// the child is queued behind its running parent
	ls, err := h.d.Logs("child", "", true)
	if err != nil {
		t.Fatal(err)
	}
	var got syncBuf
	streamDone := make(chan error, 1)
	go func() { streamDone <- ls.Stream(context.Background(), &got, true, 0, nil) }()
	waitFor(t, "the waiting notice", func() bool { return strings.Contains(got.String(), "waiting for child:default to start (queued)") })
	close(release)
	<-tickDone
	select {
	case <-streamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream must end when the child's build ends")
	}
	if !strings.Contains(got.String(), "building child") || strings.Contains(got.String(), "did not start") {
		t.Errorf("the child's build output follows the notice, even if the build was quick: %q", got.String())
	}
}

func TestLogsOfAFinishedBuildAndOfAFreshProcess(t *testing.T) {
	h := newHarness(t)
	if _, err := h.d.Logs("base", "", false); !errors.Is(err, ErrNotReady) {
		t.Errorf("no repo yet: %v", err)
	}
	h.sync()
	if _, err := h.d.Logs("base", "", false); !errors.Is(err, ErrNoLogs) {
		t.Errorf("nothing built yet: %v", err)
	}
	h.tick()
	read := func(follow bool) string {
		t.Helper()
		ls, err := h.d.Logs("base", "", follow)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := ls.Stream(context.Background(), &b, follow, 0, nil); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if got := read(false); got != "building base\n" {
		t.Errorf("last build: %q", got)
	}
	if got := read(true); got != "building base\n" {
		t.Errorf("following an idle job shows the last output and returns: %q", got)
	}
	// a restarted daemon has no live log; the last recorded one comes from the state database
	h.start()
	h.sync()
	if got := read(false); got != "building base\n" {
		t.Errorf("after a restart: %q", got)
	}
	// errors
	if _, err := h.d.Logs("nope", "", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown container: %v", err)
	}
	if _, err := h.d.Logs("base", "cuda", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown flavor: %v", err)
	}
}

func TestLogsFollowOfAParkedJobExplains(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.b.fail["base"] = 2
	h.tick()
	h.clk.advance(16 * time.Minute)
	h.tick() // parked
	ls, err := h.d.Logs("base", "", true)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := ls.Stream(context.Background(), &b, true, 0, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "building base") {
		t.Errorf("the failed build's output is still available: %q", b.String())
	}
}

// --- sync now -----------------------------------------------------------------------------

func (h *harness) head() string { return sh(h.t, h.work, "rev-parse", "HEAD") }

func TestSyncNowActivatesWithoutWaitingForThePoll(t *testing.T) {
	h := newHarness(t)
	h.sync()
	before := h.d.Status().Repo.Active
	h.push("change base", map[string]string{"containers/base/Dockerfile": "FROM debian\nRUN echo new\n"})
	want := h.head()
	res, err := h.d.SyncNow(context.Background(), want, "main")
	if err != nil {
		t.Fatal(err)
	}
	if res.State != RepoOK || res.Active != want || res.Head != want || !res.Changed || res.Branch != "main" ||
		res.HasCommit == nil || !*res.HasCommit || res.Ignored {
		t.Errorf("%+v", res)
	}
	if before == want {
		t.Fatal("test setup")
	}
	// nothing new: unchanged, still fine
	res, _ = h.d.SyncNow(context.Background(), want, "")
	if res.Changed || res.State != RepoOK {
		t.Errorf("second call: %+v", res)
	}
	// the scheduler is woken: the stale image is rebuilt on the next tick
	h.b.take()
	h.tick()
	if got := h.b.take(); len(got) == 0 {
		t.Errorf("a changed commit must lead to builds: %v", got)
	}
}

func TestSyncNowAnswersForACommitThatIsNotThereYet(t *testing.T) {
	h := newHarness(t)
	h.sync()
	res, err := h.d.SyncNow(context.Background(), strings.Repeat("f", 40), "")
	if err != nil || res.HasCommit == nil || *res.HasCommit || res.State != RepoOK {
		t.Errorf("the push has not arrived: %+v %v", res, err)
	}
}

func TestSyncNowIgnoresOtherBranchesAndRejectsGarbage(t *testing.T) {
	h := newHarness(t)
	h.sync()
	res, err := h.d.SyncNow(context.Background(), "", "feature/x")
	if err != nil || !res.Ignored || res.Branch != "main" {
		t.Errorf("%+v %v", res, err)
	}
	if _, err := h.d.SyncNow(context.Background(), "not-a-sha", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad commit: %v", err)
	}
	if _, err := h.d.SyncNow(context.Background(), "abc; rm -rf /", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("injection attempt: %v", err)
	}
}

func TestSyncNowReportsAnInvalidCommit(t *testing.T) {
	h := newHarness(t)
	h.sync()
	good := h.d.Status().Repo.Active
	h.push("stray file", map[string]string{"containers/base/junk.txt": "x"})
	res, err := h.d.SyncNow(context.Background(), h.head(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if res.State != RepoDegraded || !strings.Contains(res.Error, "junk.txt") || res.Active != good || res.Changed ||
		res.HasCommit == nil || !*res.HasCommit {
		t.Errorf("%+v", res)
	}
}

func TestSyncNowRacesWithThePollLoopSafely(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.push("change", map[string]string{"containers/base/Dockerfile": "FROM debian\nRUN echo x\n"})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); h.d.SyncNow(context.Background(), h.head(), "") }()
		go func() { defer wg.Done(); h.d.SyncOnce(context.Background()) }()
	}
	wg.Wait()
	if got := h.d.Status().Repo; got.State != RepoOK || got.Active != h.head() {
		t.Errorf("%+v", got)
	}
}
