package daemon

import (
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
	if f.fail[j.Container] > 0 {
		f.fail[j.Container]--
		if f.testF[j.Container] {
			return nil, &build.TestError{Cmd: "false", Output: "nope", Err: errors.New("exit 1")}
		}
		return nil, errors.New("apt exploded")
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
	h.b = &fakeBuilder{fail: map[string]int{}, testF: map[string]bool{}}
	h.reg = &fakeRegistry{removed: map[string][]string{}}
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

func TestOutsideSlotNothingRunsExceptTrigger(t *testing.T) {
	h := newHarness(t)
	h.push("narrow slot", map[string]string{"prebake.yaml": "slots: [\"02:00-04:00\"]\nkeep: 4\n"})
	h.sync()
	h.tick() // 12:00 is outside
	if got := h.b.take(); len(got) != 0 {
		t.Fatalf("builds outside the slot: %v", got)
	}
	if err := h.d.Trigger("base", "", schedule.Rebuild); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if got := h.b.take(); len(got) != 1 || got[0] != "base:default rebuild" {
		t.Errorf("trigger: %v", got)
	}
	// slot opens: the rest runs (child; base was just built)
	h.clk.advance(14 * time.Hour) // 02:00 next day
	h.tick()
	if got := h.b.take(); len(got) == 0 || got[len(got)-1] != "child:default rebuild" {
		t.Errorf("in slot: %v", got)
	}
}

func TestTriggerValidation(t *testing.T) {
	h := newHarness(t)
	if err := h.d.Trigger("base", "", schedule.Rebuild); err == nil {
		t.Error("no repo loaded yet must fail")
	}
	h.sync()
	for _, c := range []struct {
		container, flavor string
		kind              schedule.Kind
	}{{"nope", "", schedule.Rebuild}, {"base", "cuda", schedule.Rebuild}, {"base", "", schedule.None}} {
		if err := h.d.Trigger(c.container, c.flavor, c.kind); err == nil {
			t.Errorf("%+v should fail", c)
		}
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
	if err := h.d.Trigger("base", "", schedule.Rebuild); err != nil {
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
