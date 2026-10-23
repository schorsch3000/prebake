package main

import (
	"bytes"
	"encoding/json"
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
	if c.Branch != "main" || c.PollInterval != 5*time.Minute || c.DataDir != "/data" || c.ReadAddr != ":5000" ||
		c.WriteAddr != ":5001" || c.RegistryHost != "localhost:5001" {
		t.Errorf("defaults: %+v", c)
	}
	// env overrides, flags override env
	c, err = parseDaemonConfig([]string{"--repo-branch", "dev", "--poll", "30s"}, env(map[string]string{
		"REPO_URL": "u", "REPO_BRANCH": "stable", "POLL_INTERVAL": "1h", "DATA_DIR": "/x", "WRITE_ADDR": "127.0.0.1:6001",
	}), &sink)
	if err != nil || c.Branch != "dev" || c.PollInterval != 30*time.Second || c.DataDir != "/x" || c.RegistryHost != "localhost:6001" {
		t.Errorf("%+v %v", c, err)
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
	} {
		if _, err := parseDaemonConfig(nil, env(e), &sink); err == nil {
			t.Errorf("%s must fail", name)
		}
	}
}
