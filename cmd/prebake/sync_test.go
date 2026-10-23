package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schorsch3000/prebake/internal/daemon"
)

// syncServer answers POST /sync with the scripted results, one per call (the last repeats), and
// records the query strings.
func syncServer(t *testing.T, answers ...daemon.SyncResult) (*httptest.Server, *[]string) {
	var queries []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/sync" {
			w.WriteHeader(404)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		a := answers[min(n, len(answers)-1)]
		n++
		json.NewEncoder(w).Encode(a)
	}))
	t.Cleanup(srv.Close)
	return srv, &queries
}

func boolp(b bool) *bool { return &b }

func fastPoll(t *testing.T) {
	old := syncPoll
	syncPoll = 5 * time.Millisecond
	t.Cleanup(func() { syncPoll = old })
}

func TestSyncPlain(t *testing.T) {
	srv, queries := syncServer(t, daemon.SyncResult{Branch: "main", State: daemon.RepoOK, Active: "abcdef1234", Changed: true})
	code, out, e := runCLI(t, "sync", "--url", srv.URL)
	if code != 0 || out != "synced: running abcdef1 on main (new commit activated)\n" || e != "" {
		t.Fatalf("%d %q %q", code, out, e)
	}
	if len(*queries) != 1 || (*queries)[0] != "" {
		t.Errorf("queries = %v", *queries)
	}
	srv2, _ := syncServer(t, daemon.SyncResult{Branch: "main", State: daemon.RepoOK, Active: "abcdef1234"})
	if _, out, _ := runCLI(t, "sync", "--url", srv2.URL); !strings.Contains(out, "up to date") {
		t.Errorf("unchanged: %q", out)
	}
	if code, out, _ := runCLI(t, "sync", "--quiet", "--url", srv2.URL); code != 0 || out != "" {
		t.Errorf("--quiet: %d %q", code, out)
	}
}

func TestSyncWaitsForTheCommitToAppear(t *testing.T) {
	fastPoll(t)
	srv, queries := syncServer(t,
		daemon.SyncResult{Branch: "main", State: daemon.RepoOK, Active: "old", HasCommit: boolp(false)},
		daemon.SyncResult{Branch: "main", State: daemon.RepoOK, Active: "old", HasCommit: boolp(false)},
		daemon.SyncResult{Branch: "main", State: daemon.RepoOK, Active: "abcdef1234", Changed: true, HasCommit: boolp(true)},
	)
	code, out, e := runCLI(t, "sync", "--commit", "abcdef1234", "--branch", "main", "--timeout", "5s", "--url", srv.URL)
	if code != 0 || !strings.Contains(out, "running abcdef1") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	if len(*queries) != 3 || (*queries)[0] != "commit=abcdef1234&branch=main" {
		t.Errorf("it asks again until the commit is there: %v", *queries)
	}
}

func TestSyncGivesUpWhenTheCommitNeverShowsUp(t *testing.T) {
	fastPoll(t)
	srv, queries := syncServer(t, daemon.SyncResult{Branch: "main", State: daemon.RepoOK, HasCommit: boolp(false)})
	start := time.Now()
	code, _, e := runCLI(t, "sync", "--commit", "abcdef1", "--timeout", "100ms", "--url", srv.URL)
	if code != 1 || !strings.Contains(e, "did not show up on main within 100ms") {
		t.Errorf("%d %q", code, e)
	}
	if time.Since(start) > 2*time.Second || len(*queries) < 2 {
		t.Errorf("took %v, %d queries", time.Since(start), len(*queries))
	}
}

func TestSyncIgnoredBranchAndProblems(t *testing.T) {
	srv, _ := syncServer(t, daemon.SyncResult{Branch: "main", Ignored: true, State: daemon.RepoOK})
	if code, out, _ := runCLI(t, "sync", "--branch", "feature", "--url", srv.URL); code != 0 || !strings.Contains(out, "the daemon follows main, not feature") {
		t.Errorf("ignored: %d %q", code, out)
	}
	if code, out, _ := runCLI(t, "sync", "--branch", "feature", "--quiet", "--url", srv.URL); code != 0 || out != "" {
		t.Errorf("ignored, quiet: %d %q", code, out)
	}
	bad, _ := syncServer(t, daemon.SyncResult{Branch: "main", State: daemon.RepoDegraded, Error: "containers/base/junk.txt: stray file", HasCommit: boolp(true)})
	code, _, e := runCLI(t, "sync", "--commit", "abcdef1", "--url", bad.URL)
	if code != 1 || !strings.Contains(e, "repo is degraded") || !strings.Contains(e, "junk.txt") {
		t.Errorf("an invalid commit is reported and fails: %d %q", code, e)
	}
	waiting, _ := syncServer(t, daemon.SyncResult{Branch: "main", State: daemon.RepoWaitingForKey, Error: "git: authentication failed"})
	if code, _, e := runCLI(t, "sync", "--url", waiting.URL); code != 1 || !strings.Contains(e, "waiting-for-deploy-key") {
		t.Errorf("%d %q", code, e)
	}
	if code, _, e := runCLI(t, "sync", "--url", "http://127.0.0.1:1"); code != 2 || e == "" {
		t.Errorf("unreachable: %d %q", code, e)
	}
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid request: invalid commit"}`))
	}))
	defer reject.Close()
	if code, _, e := runCLI(t, "sync", "--commit", "nope", "--url", reject.URL); code != 1 || !strings.Contains(e, "400") {
		t.Errorf("rejected: %d %q", code, e)
	}
}

func TestSyncCommitHEADAndTheURLFromGitConfig(t *testing.T) {
	root := goodRepo(t)
	write(t, root, "f.txt", "x")
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-q", "-m", "c")
	head := strings.TrimSpace(func() string {
		out, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
		return string(out)
	}())
	srv, queries := syncServer(t, daemon.SyncResult{Branch: "main", State: daemon.RepoOK, Active: head, HasCommit: boolp(true)})
	t.Chdir(root)
	t.Setenv("PREBAKE_URL", "")
	gitT(t, root, "config", "prebake.url", srv.URL)
	if got := defaultURL(); got != srv.URL {
		t.Fatalf("git config prebake.url should be the default: %q", got)
	}
	if code, _, e := runCLI(t, "sync", "--commit", "HEAD", "--quiet"); code != 0 { // no --url: from git config
		t.Fatalf("%d %q", code, e)
	}
	if len(*queries) != 1 || (*queries)[0] != "commit="+head {
		t.Errorf("--commit HEAD resolves to the checked out commit: %v", *queries)
	}
	t.Setenv("PREBAKE_URL", "http://env.example")
	if got := defaultURL(); got != "http://env.example" {
		t.Errorf("the environment wins over git config: %q", got)
	}
	t.Setenv("PREBAKE_URL", "")
	gitT(t, root, "config", "--unset", "prebake.url")
	if got := defaultURL(); got != "http://127.0.0.1:8080" {
		t.Errorf("fallback: %q", got)
	}
	// outside a repository --commit HEAD is a usage error
	t.Chdir(t.TempDir())
	if code, _, e := runCLI(t, "sync", "--commit", "HEAD", "--url", srv.URL); code != 2 || !strings.Contains(e, "git repository") {
		t.Errorf("%d %q", code, e)
	}
}

// --- the pre-push hook --------------------------------------------------------------------------

// runHook runs the generated pre-push script with a fake `prebake` that records its arguments.
func runHook(t *testing.T, stdin string, withPrebake bool, fakeBody string) (calls []string, took time.Duration, code int) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "pre-push")
	os.WriteFile(script, []byte(prePushHook), 0o755)
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	log := filepath.Join(dir, "calls.log")
	if withPrebake {
		os.WriteFile(filepath.Join(bin, "prebake"), []byte("#!/bin/sh\n"+fakeBody+"\necho \"$*\" >> "+log+"\n"), 0o755)
	}
	for _, tool := range []string{"cat", "sleep"} { // a minimal PATH that has no prebake unless we put one there
		if p, err := exec.LookPath(tool); err == nil {
			os.Symlink(p, filepath.Join(bin, tool))
		}
	}
	cmd := exec.Command("sh", script, "origin", "git@example.com:me/r.git")
	cmd.Env = []string{"PATH=" + bin}
	cmd.Stdin = strings.NewReader(stdin)
	start := time.Now()
	err := cmd.Run()
	took = time.Since(start)
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	// the sync runs in the background: wait for what it records
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(log); err == nil && len(strings.Fields(string(b))) > 0 {
			time.Sleep(50 * time.Millisecond) // let a second call land too
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if b, err := os.ReadFile(log); err == nil {
		calls = strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	return calls, took, code
}

func TestPrePushHookSyncsEveryPushedBranch(t *testing.T) {
	const zero = "0000000000000000000000000000000000000000"
	stdin := strings.Join([]string{
		"refs/heads/main aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/main " + zero,
		"refs/heads/feat bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/team/feat " + zero,
		"refs/tags/v1 cccccccccccccccccccccccccccccccccccccccc refs/tags/v1 " + zero,    // a tag: not a branch
		"(delete) " + zero + " refs/heads/old dddddddddddddddddddddddddddddddddddddddd", // a deleted branch
	}, "\n") + "\n"
	calls, _, code := runHook(t, stdin, true, "")
	if code != 0 {
		t.Fatalf("a hook must never fail the push: %d", code)
	}
	want := map[string]bool{
		"sync --commit aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --branch main --quiet":      true,
		"sync --commit bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb --branch team/feat --quiet": true,
	}
	if len(calls) != 2 || !want[calls[0]] || !want[calls[1]] || calls[0] == calls[1] {
		t.Errorf("calls = %q (tags and deletions must not trigger a sync)", calls)
	}
}

func TestPrePushHookDoesNotDelayOrBreakThePush(t *testing.T) {
	stdin := "refs/heads/main aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/main 0000000000000000000000000000000000000000\n"
	// a slow sync (the daemon waiting for the commit) runs in the background
	_, took, code := runHook(t, stdin, true, "sleep 2")
	if code != 0 || took > time.Second {
		t.Errorf("the push must not wait for the sync: exit %d after %v", code, took)
	}
	// a failing prebake does not matter either
	if _, _, code := runHook(t, stdin, true, "exit 1"); code != 0 {
		t.Errorf("failing sync: exit %d", code)
	}
	// prebake not installed: silently nothing
	calls, _, code := runHook(t, stdin, false, "")
	if code != 0 || len(calls) != 0 {
		t.Errorf("no prebake: exit %d, calls %q", code, calls)
	}
	// nothing pushed
	if calls, _, code := runHook(t, "", true, ""); code != 0 || len(calls) != 0 {
		t.Errorf("empty stdin: exit %d, calls %q", code, calls)
	}
}

func TestInstallHookWithSync(t *testing.T) {
	root := goodRepo(t)
	code, out, e := runCLI(t, "install-hook", "--sync", "--url", "http://prebake.home:6002", "--repo", root)
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, e)
	}
	for _, name := range []string{"pre-commit", "pre-push"} {
		hook := filepath.Join(root, ".git", "hooks", name)
		st, err := os.Stat(hook)
		if err != nil || st.Mode()&0o111 == 0 {
			t.Errorf("%s hook missing or not executable: %v", name, err)
		}
		if b, _ := os.ReadFile(hook); !strings.Contains(string(b), hookMarker) {
			t.Errorf("%s hook lacks the marker", name)
		}
	}
	if b, _ := exec.Command("git", "-C", root, "config", "--get", "prebake.url").Output(); strings.TrimSpace(string(b)) != "http://prebake.home:6002" {
		t.Errorf("prebake.url = %q", b)
	}
	if !strings.Contains(out, "set git config prebake.url") {
		t.Errorf("output: %q", out)
	}
	// without --sync there is no pre-push hook
	other := goodRepo(t)
	runCLI(t, "install-hook", "--repo", other)
	if _, err := os.Stat(filepath.Join(other, ".git", "hooks", "pre-push")); err == nil {
		t.Error("pre-push must only be installed with --sync")
	}
	// a foreign pre-push hook is protected, --force replaces it
	os.WriteFile(filepath.Join(other, ".git", "hooks", "pre-push"), []byte("#!/bin/sh\necho mine\n"), 0o755)
	if code, _, e := runCLI(t, "install-hook", "--sync", "--repo", other); code != 1 || !strings.Contains(e, "pre-push") {
		t.Errorf("foreign hook: %d %q", code, e)
	}
	if b, _ := os.ReadFile(filepath.Join(other, ".git", "hooks", "pre-push")); !strings.Contains(string(b), "mine") {
		t.Error("the foreign hook was overwritten")
	}
	if code, _, _ := runCLI(t, "install-hook", "--sync", "--force", "--repo", other); code != 0 {
		t.Error("--force")
	}
	// --url alone makes no sense
	if code, _, e := runCLI(t, "install-hook", "--url", "http://x", "--repo", other); code != 2 || !strings.Contains(e, "--sync") {
		t.Errorf("%d %q", code, e)
	}
	// a hint when nothing tells the hook where the daemon is
	t.Setenv("PREBAKE_URL", "")
	third := goodRepo(t)
	if _, out, _ := runCLI(t, "install-hook", "--sync", "--repo", third); !strings.Contains(out, "prebake.url") {
		t.Errorf("hint: %q", out)
	}
}
