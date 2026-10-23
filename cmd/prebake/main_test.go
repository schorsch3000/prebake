package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runCLI(t *testing.T, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	code = run(args, &o, &e)
	return code, o.String(), e.String()
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitT(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root,
		"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func goodRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gitT(t, root, "init", "-q")
	write(t, root, "prebake.yaml", "keep: 5\n")
	write(t, root, "containers/dev/Dockerfile", "FROM debian\n")
	write(t, root, "containers/dev/container.yaml", "tests: [\"true\"]\n")
	return root
}

func TestUsageAndUnknown(t *testing.T) {
	if code, out, _ := runCLI(t, "--help"); code != 0 || !strings.Contains(out, "validate") {
		t.Errorf("help: %d %q", code, out)
	}
	if code, _, _ := runCLI(t); code != 2 {
		t.Errorf("no args: %d", code)
	}
	if code, _, _ := runCLI(t, "nope"); code != 2 {
		t.Errorf("unknown: %d", code)
	}
}

func TestValidateWorktree(t *testing.T) {
	root := goodRepo(t)
	if code, out, e := runCLI(t, "validate", "--repo", root); code != 0 || out != "" {
		t.Fatalf("clean repo: %d %q %q", code, out, e)
	}
	write(t, root, "containers/dev/junk.txt", "x")
	code, out, _ := runCLI(t, "validate", "--repo", root)
	if code != 1 || !strings.Contains(out, "containers/dev/junk.txt: stray file") {
		t.Fatalf("stray: %d %q", code, out)
	}
}

func TestValidateStagedJudgesIndexNotDisk(t *testing.T) {
	root := goodRepo(t)
	gitT(t, root, "add", "-A")
	// unstaged junk on disk must not matter
	write(t, root, "containers/dev/junk.txt", "x")
	if code, out, _ := runCLI(t, "validate", "--repo", root, "--staged"); code != 0 {
		t.Fatalf("staged ignores unstaged junk: %d %q", code, out)
	}
	// staged junk must fail, even if removed from disk afterwards
	gitT(t, root, "add", "containers/dev/junk.txt")
	os.Remove(filepath.Join(root, "containers/dev/junk.txt"))
	if code, out, _ := runCLI(t, "validate", "--repo", root, "--staged"); code != 1 || !strings.Contains(out, "junk.txt") {
		t.Fatalf("staged junk: %d %q", code, out)
	}
}

func TestValidateRev(t *testing.T) {
	root := goodRepo(t)
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-q", "-m", "good")
	write(t, root, "containers/dev/container.yaml", "refrsh_cmd: x\n")
	gitT(t, root, "commit", "-q", "-am", "bad")
	if code, out, _ := runCLI(t, "validate", "--repo", root, "--rev", "HEAD~1"); code != 0 {
		t.Fatalf("good rev: %d %q", code, out)
	}
	code, out, _ := runCLI(t, "validate", "--repo", root, "--rev", "HEAD")
	if code != 1 || !strings.Contains(out, "container.yaml:1:") {
		t.Fatalf("bad rev: %d %q", code, out)
	}
	if code, _, e := runCLI(t, "validate", "--repo", root, "--rev", "nonexistent"); code != 2 {
		t.Fatalf("bad ref should be internal error: %d %q", code, e)
	}
}

func TestValidateStrictAndJSON(t *testing.T) {
	root := goodRepo(t)
	write(t, root, "containers/dev/container.yaml", "refresh_cmd: x\n") // warning: no tests
	if code, _, _ := runCLI(t, "validate", "--repo", root); code != 0 {
		t.Errorf("warning must not fail: %d", code)
	}
	if code, _, _ := runCLI(t, "validate", "--repo", root, "--strict"); code != 1 {
		t.Errorf("--strict must fail on warning: %d", code)
	}
	code, out, _ := runCLI(t, "validate", "--repo", root, "--format", "json")
	var got struct {
		Errors, Warnings int
		Issues           []struct{ Severity, File, Message string }
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 || got.Warnings != 1 || got.Issues[0].Severity != "warning" {
		t.Fatalf("json: %d %v %q", code, err, out)
	}
}

func TestValidateUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"validate", "--staged", "--rev", "HEAD"},
		{"validate", "--format", "xml"},
		{"validate", "extra"},
		{"validate", "--repo", "/nonexistent-dir-xyz"},
	} {
		if code, _, _ := runCLI(t, args...); code != 2 {
			t.Errorf("%v: want exit 2, got %d", args, code)
		}
	}
}

func TestInstallHook(t *testing.T) {
	root := goodRepo(t)
	if code, out, e := runCLI(t, "install-hook", "--repo", root); code != 0 {
		t.Fatalf("install: %d %q %q", code, out, e)
	}
	hook := filepath.Join(root, ".git", "hooks", "pre-commit")
	st, err := os.Stat(hook)
	if err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("hook missing or not executable: %v %v", err, st)
	}
	// reinstall over our own hook is fine
	if code, _, _ := runCLI(t, "install-hook", "--repo", root); code != 0 {
		t.Errorf("reinstall: %d", code)
	}
	// foreign hook is protected
	os.WriteFile(hook, []byte("#!/bin/sh\necho mine\n"), 0o755)
	if code, _, _ := runCLI(t, "install-hook", "--repo", root); code != 1 {
		t.Errorf("foreign hook must be protected: %d", code)
	}
	if b, _ := os.ReadFile(hook); !strings.Contains(string(b), "mine") {
		t.Error("foreign hook was overwritten")
	}
	if code, _, _ := runCLI(t, "install-hook", "--repo", root, "--force"); code != 0 {
		t.Errorf("--force: %d", code)
	}
	if b, _ := os.ReadFile(hook); !strings.Contains(string(b), hookMarker) {
		t.Error("--force did not overwrite")
	}
	if code, _, _ := runCLI(t, "install-hook", "--repo", t.TempDir()); code != 2 {
		t.Errorf("non-repo: %d", code)
	}
}

func TestShowKey(t *testing.T) {
	dir := t.TempDir()
	code, out, errOut := runCLI(t, "show-key", "--data-dir", dir)
	if code != 0 || !strings.HasPrefix(out, "ssh-ed25519 AAAA") || !strings.Contains(errOut, "fingerprint: SHA256:") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if st, err := os.Stat(filepath.Join(dir, "ssh", "id_ed25519")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("key file: %v %v", err, st)
	}
	// stable across runs
	if _, out2, _ := runCLI(t, "show-key", "--data-dir", dir); out2 != out {
		t.Errorf("key changed: %q vs %q", out, out2)
	}
	t.Setenv("DATA_DIR", dir)
	if _, out3, _ := runCLI(t, "show-key"); out3 != out {
		t.Errorf("DATA_DIR not honoured: %q", out3)
	}
}

func TestParseDaemonConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	var sink bytes.Buffer

	if _, err := parseDaemonConfig(nil, env(nil), &sink); err == nil || !strings.Contains(err.Error(), "REPO_URL") {
		t.Errorf("REPO_URL is required: %v", err)
	}
	c, err := parseDaemonConfig(nil, env(map[string]string{"REPO_URL": "git@h:me/r.git"}), &sink)
	if err != nil {
		t.Fatal(err)
	}
	if c.Jitter != 15*time.Minute {
		t.Errorf("jitter default: %v", c.Jitter)
	}
	if c.Branch != "main" || c.PollInterval != 5*time.Minute || c.DataDir != "/data" || c.ReadAddr != ":5000" ||
		c.WriteAddr != ":5001" || c.StatusAddr != ":8080" || c.RegistryHost != "localhost:5001" {
		t.Errorf("defaults: %+v", c)
	}
	// env overrides, flags override env
	c, err = parseDaemonConfig([]string{"--repo-branch", "dev", "--poll", "30s"}, env(map[string]string{
		"REPO_URL": "u", "REPO_BRANCH": "stable", "POLL_INTERVAL": "1h", "DATA_DIR": "/x", "WRITE_ADDR": "127.0.0.1:6001",
	}), &sink)
	if err != nil || c.Branch != "dev" || c.PollInterval != 30*time.Second || c.DataDir != "/x" || c.RegistryHost != "localhost:6001" {
		t.Errorf("%+v %v", c, err)
	}
	if c, err := parseDaemonConfig(nil, env(map[string]string{"REPO_URL": "u", "JITTER": "0s"}), &sink); err != nil || c.Jitter != 0 {
		t.Errorf("jitter 0: %+v %v", c, err)
	}
	c, _ = parseDaemonConfig(nil, env(map[string]string{"REPO_URL": "u", "REGISTRY_HOST": "host.docker.internal:5001"}), &sink)
	if c.RegistryHost != "host.docker.internal:5001" {
		t.Errorf("REGISTRY_HOST: %q", c.RegistryHost)
	}
	for name, e := range map[string]map[string]string{
		"bad poll":      {"REPO_URL": "u", "POLL_INTERVAL": "soon"},
		"zero poll":     {"REPO_URL": "u", "POLL_INTERVAL": "0s"},
		"cert only":     {"REPO_URL": "u", "TLS_CERT": "c.pem"},
		"bad writeaddr": {"REPO_URL": "u", "WRITE_ADDR": "nonsense"},
		"bad jitter":    {"REPO_URL": "u", "JITTER": "-5m"},
	} {
		if _, err := parseDaemonConfig(nil, env(e), &sink); err == nil {
			t.Errorf("%s must fail", name)
		}
	}
}

// statusServer serves canned /status JSON and records POSTs.
func statusServer(t *testing.T, dto string) (*httptest.Server, *[]string) {
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/status":
			w.Write([]byte(dto))
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/trigger/base/nope"):
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"not found: no flavor"}`))
		case r.Method == "POST":
			posts = append(posts, r.URL.RequestURI())
			w.WriteHeader(202)
			w.Write([]byte(`{"queued":"ok"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &posts
}

const statusJSON = `{"generated_at":"2026-10-22T02:00:00Z","time_zone":"JST +09:00","repo":{"state":"degraded","head":"bbbbbbbbbb","active":"aaaaaaaaaa","error":"containers/x: stray file"},
"jobs":[
 {"container":"late","flavor":"default","state":"idle","last_rebuild":"2026-10-20T02:00:00Z","next_kind":"refresh","next_run":"2026-10-23T02:00:00Z"},
 {"container":"parked","flavor":"cuda","state":"parked","error":"apt exploded\nsecond line"},
 {"container":"soon","flavor":"default","state":"queued","next_kind":"rebuild","next_run":"2026-10-22T02:00:00Z"}
]}`

func TestStatusCommand(t *testing.T) {
	srv, _ := statusServer(t, statusJSON)
	code, out, errOut := runCLI(t, "status", "--url", srv.URL)
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	for _, want := range []string{"time zone of the build slots: JST +09:00", "repo: degraded  active=aaaaaaa head=bbbbbbb", "containers/x: stray file", "CONTAINER", "parked", "second line", "never"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "second line") || strings.Contains(out, "apt exploded") {
		t.Errorf("the table shows the LAST error line (docker's reason), not the first:\n%s", out)
	}
	if !strings.Contains(out, "+1 lines") {
		t.Errorf("a multi-line error should say that there is more:\n%s", out)
	}
	code, out, _ = runCLI(t, "status", "--json", "--url", srv.URL)
	var dto struct{ Jobs []map[string]any }
	if code != 0 || json.Unmarshal([]byte(out), &dto) != nil || len(dto.Jobs) != 3 {
		t.Errorf("json: %d %s", code, out)
	}
	// PREBAKE_URL is the default
	t.Setenv("PREBAKE_URL", srv.URL)
	if code, _, _ := runCLI(t, "status"); code != 0 {
		t.Errorf("PREBAKE_URL not honoured: %d", code)
	}
	if code, _, e := runCLI(t, "status", "--url", "http://127.0.0.1:1"); code != 2 || e == "" {
		t.Errorf("unreachable daemon must be exit 2: %d %q", code, e)
	}
}

func TestDryRunOrdersByNextRun(t *testing.T) {
	srv, _ := statusServer(t, statusJSON)
	code, out, _ := runCLI(t, "dry-run", "--url", srv.URL)
	if code != 0 {
		t.Fatal(code)
	}
	soon, late, parked := strings.Index(out, "soon"), strings.Index(out, "late"), strings.Index(out, "parked")
	if !(soon >= 0 && soon < late && late < parked) {
		t.Errorf("order wrong (soon %d, late %d, parked %d):\n%s", soon, late, parked, out)
	}
	if !strings.Contains(out, "now") || !strings.Contains(out, "24h0m0s") || !strings.Contains(out, "(parked)") {
		t.Errorf("relative times:\n%s", out)
	}
}

func TestTriggerAndGCCommands(t *testing.T) {
	srv, posts := statusServer(t, statusJSON)
	if code, out, e := runCLI(t, "trigger", "base", "--url", srv.URL); code != 0 || !strings.Contains(out, "queued") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	// flags after the positional argument work, flavor syntax container:flavor
	if code, _, e := runCLI(t, "trigger", "child:cuda", "--kind", "refresh", "--url", srv.URL); code != 0 {
		t.Fatalf("%d %q", code, e)
	}
	if code, _, e := runCLI(t, "gc", "--url", srv.URL); code != 0 {
		t.Fatalf("%d %q", code, e)
	}
	want := []string{"/trigger/base?kind=rebuild", "/trigger/child/cuda?kind=refresh", "/gc"}
	if strings.Join(*posts, " ") != strings.Join(want, " ") {
		t.Errorf("posts = %v", *posts)
	}
	if code, _, e := runCLI(t, "trigger", "base:nope", "--url", srv.URL); code != 1 || !strings.Contains(e, "404") {
		t.Errorf("rejected trigger must be exit 1: %d %q", code, e)
	}
	if code, _, _ := runCLI(t, "trigger", "--url", srv.URL); code != 2 {
		t.Errorf("missing argument: %d", code)
	}
	if code, _, _ := runCLI(t, "trigger", "a", "b", "--url", srv.URL); code != 2 {
		t.Errorf("two arguments: %d", code)
	}
}

func TestVersionFromTheEnvironmentOfTheImage(t *testing.T) {
	// the image sets PREBAKE_VERSION/PREBAKE_REVISION; the linker values (release archives) win
	t.Setenv("PREBAKE_VERSION", "1.2.3")
	t.Setenv("PREBAKE_REVISION", "abc123")
	if _, out, _ := runCLI(t, "version"); out != "prebake 1.2.3 (abc123)\n" {
		t.Errorf("env fallback: %q", out)
	}
	oldV, oldR := version, revision
	version, revision = "9.9.9", "def456"
	defer func() { version, revision = oldV, oldR }()
	if _, out, _ := runCLI(t, "version"); out != "prebake 9.9.9 (def456)\n" {
		t.Errorf("linker values must win: %q", out)
	}
}

func TestVersionCommand(t *testing.T) {
	t.Setenv("PREBAKE_VERSION", "")
	t.Setenv("PREBAKE_REVISION", "")
	for _, arg := range []string{"version", "--version", "-v"} {
		code, out, _ := runCLI(t, arg)
		if code != 0 || !strings.HasPrefix(out, "prebake dev (") {
			t.Errorf("%s: %d %q", arg, code, out)
		}
	}
}

func TestValidateStagedHintsAtUntrackedFiles(t *testing.T) {
	root := goodRepo(t)
	write(t, root, "containers/dev/Dockerfile", "FROM debian\nCOPY init /init\nCOPY packages.apt /p\n")
	write(t, root, "containers/dev/init", "#!/bin/sh\n")
	write(t, root, "containers/dev/packages.apt", "git\n")
	gitT(t, root, "add", "prebake.yaml", "containers/dev/Dockerfile", "containers/dev/container.yaml") // not the two new files

	var o, e bytes.Buffer
	code := run([]string{"validate", "--repo", root, "--staged"}, &o, &e)
	if code != 1 || !strings.Contains(o.String(), `COPY source "init" does not exist`) {
		t.Fatalf("the index lacks the new files: %d %q", code, o.String())
	}
	if !strings.Contains(e.String(), "untracked") || !strings.Contains(e.String(), "containers/dev/init") ||
		!strings.Contains(e.String(), "containers/dev/packages.apt") || !strings.Contains(e.String(), "git commit -a") {
		t.Errorf("the hint must name the untracked files and the git commit -a pitfall:\n%s", e.String())
	}

	// staged: passes, and no hint is printed
	gitT(t, root, "add", "containers/dev")
	o.Reset()
	e.Reset()
	if code := run([]string{"validate", "--repo", root, "--staged"}, &o, &e); code != 0 || e.String() != "" {
		t.Errorf("after git add: %d %q %q", code, o.String(), e.String())
	}

	// the hint is only for --staged; a real error without untracked files gets none
	write(t, root, "containers/dev/container.yaml", "refrsh: x\n")
	gitT(t, root, "add", "-A")
	o.Reset()
	e.Reset()
	if code := run([]string{"validate", "--repo", root, "--staged"}, &o, &e); code != 1 || strings.Contains(e.String(), "hint:") {
		t.Errorf("no untracked files, no hint: %d %q", code, e.String())
	}
	// worktree mode never prints it
	e.Reset()
	write(t, root, "untracked.txt", "x")
	run([]string{"validate", "--repo", root}, &o, &e)
	if strings.Contains(e.String(), "hint:") {
		t.Errorf("worktree mode: %q", e.String())
	}
}
