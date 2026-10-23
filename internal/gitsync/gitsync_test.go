package gitsync

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// origin creates a bare repository with one commit on main and returns it plus a
// working clone to add commits with.
func origin(t *testing.T) (bare, work string) {
	t.Helper()
	root := t.TempDir()
	bare, work = filepath.Join(root, "origin.git"), filepath.Join(root, "work")
	mustGit(t, root, "init", "-q", "--bare", "-b", "main", bare)
	mustGit(t, root, "clone", "-q", bare, work)
	commit(t, work, "prebake.yaml", "keep: 5\n", "first")
	return bare, work
}

func commit(t *testing.T, work, file, content, msg string) string {
	t.Helper()
	p := filepath.Join(work, file)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "-A")
	mustGit(t, work, "commit", "-q", "-m", msg)
	mustGit(t, work, "push", "-q", "origin", "HEAD:main")
	return mustGit(t, work, "rev-parse", "HEAD")
}

func TestIdentityCreatedOnceAndStable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ssh")
	a, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.PublicLine, "ssh-ed25519 AAAA") || !strings.HasPrefix(a.Fingerprint, "SHA256:") {
		t.Fatalf("%+v", a)
	}
	st, _ := os.Stat(a.KeyPath)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("private key mode = %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("ssh dir mode = %v", st.Mode().Perm())
	}
	priv, _ := os.ReadFile(a.KeyPath)
	b, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	priv2, _ := os.ReadFile(a.KeyPath)
	if b.PublicLine != a.PublicLine || b.Fingerprint != a.Fingerprint || string(priv) != string(priv2) {
		t.Error("existing key must never be regenerated")
	}
	// a lost .pub file is repaired from the private key
	os.Remove(a.PubPath)
	c, err := LoadOrCreate(dir)
	if err != nil || c.PublicLine != a.PublicLine {
		t.Fatalf("%v %+v", err, c)
	}
	if b, _ := os.ReadFile(a.PubPath); !strings.HasPrefix(string(b), "ssh-ed25519 ") {
		t.Errorf(".pub not restored: %q", b)
	}
	// the key is accepted by the real ssh tooling
	if _, err := exec.LookPath("ssh-keygen"); err == nil {
		out, err := exec.Command("ssh-keygen", "-y", "-f", a.KeyPath).CombinedOutput()
		if err != nil || !strings.HasPrefix(string(out), "ssh-ed25519 ") {
			t.Errorf("ssh-keygen -y: %v %s", err, out)
		}
	}
}

func TestSSHCommand(t *testing.T) {
	id := &Identity{KeyPath: "/data/ssh/id_ed25519"}
	got := (&Syncer{Identity: id}).sshCommand()
	for _, want := range []string{"-i /data/ssh/id_ed25519", "IdentitiesOnly=yes", "BatchMode=yes",
		"UserKnownHostsFile=/data/ssh/known_hosts", "StrictHostKeyChecking=accept-new"} {
		if !strings.Contains(got, want) {
			t.Errorf("TOFU: missing %q in %q", want, got)
		}
	}
	got = (&Syncer{Identity: id, KnownHosts: "/etc/kh"}).sshCommand()
	if !strings.Contains(got, "UserKnownHostsFile=/etc/kh") || !strings.Contains(got, "StrictHostKeyChecking=yes") || strings.Contains(got, "accept-new") {
		t.Errorf("pinned: %q", got)
	}
}

func TestClassify(t *testing.T) {
	var ae *AuthError
	var he *HostKeyError
	e := errors.New("exit 128")
	if err := classify("git fetch", e, "git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository."); !errors.As(err, &ae) {
		t.Errorf("publickey: %v", err)
	}
	if err := classify("git fetch", e, "ERROR: Repository not found."); !errors.As(err, &ae) {
		t.Errorf("not found: %v", err)
	}
	if err := classify("git fetch", e, "@@@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @@@"); !errors.As(err, &he) {
		t.Errorf("host key changed: %v", err)
	}
	if err := classify("git fetch", e, "Host key verification failed."); !errors.As(err, &he) {
		t.Errorf("host key: %v", err)
	}
	err := classify("git fetch", e, "fatal: unable to access: Could not resolve host")
	if errors.As(err, &ae) || errors.As(err, &he) {
		t.Errorf("network errors are neither: %v", err)
	}
}

func TestFetchExportPromote(t *testing.T) {
	bare, work := origin(t)
	s := &Syncer{URL: bare, Branch: "main", Dir: filepath.Join(t.TempDir(), "repo")}
	ctx := context.Background()

	h1, err := s.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := mustGit(t, work, "rev-parse", "HEAD"); h1 != want {
		t.Fatalf("head %s, want %s", h1, want)
	}
	if _, _, ok := s.Current(); ok {
		t.Error("nothing promoted yet")
	}
	p1, err := s.Export(ctx, h1)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(p1, "prebake.yaml")); string(b) != "keep: 5\n" {
		t.Errorf("exported file: %q", b)
	}
	if err := s.Promote(h1); err != nil {
		t.Fatal(err)
	}
	if path, sha, ok := s.Current(); !ok || sha != h1 || path != p1 {
		t.Errorf("current = %q %q %v", path, sha, ok)
	}

	// no change: same head
	if h, err := s.Fetch(ctx); err != nil || h != h1 {
		t.Errorf("refetch: %s %v", h, err)
	}
	// new commit: new head, old checkout untouched until promote switches
	h2 := commit(t, work, "containers/dev/Dockerfile", "FROM debian\n", "second")
	h, err := s.Fetch(ctx)
	if err != nil || h != h2 {
		t.Fatalf("fetch: %s %v (want %s)", h, err, h2)
	}
	p2, err := s.Export(ctx, h2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p2, "containers/dev/Dockerfile")); err != nil {
		t.Error(err)
	}
	if _, sha, _ := s.Current(); sha != h1 {
		t.Error("current must stay on the last promoted commit until Promote")
	}
	if again, err := s.Export(ctx, h2); err != nil || again != p2 {
		t.Errorf("export is idempotent: %v %v", again, err)
	}
	s.Promote(h2)
	if _, sha, _ := s.Current(); sha != h2 {
		t.Error("promote did not switch")
	}

	// force-push: history rewritten upstream is still followed
	mustGit(t, work, "reset", "-q", "--hard", h1)
	mustGit(t, work, "push", "-q", "--force", "origin", "HEAD:main")
	if h, err := s.Fetch(ctx); err != nil || h != h1 {
		t.Errorf("after force-push: %s %v", h, err)
	}
}

func TestFetchErrors(t *testing.T) {
	bare, _ := origin(t)
	ctx := context.Background()
	s := &Syncer{URL: bare, Branch: "nope", Dir: filepath.Join(t.TempDir(), "repo")}
	if _, err := s.Fetch(ctx); err == nil {
		t.Error("missing branch must fail")
	}
	if _, err := os.Stat(s.repoDir()); err == nil {
		t.Error("a failed clone must not leave a half-initialised repo behind")
	}
	s = &Syncer{URL: filepath.Join(t.TempDir(), "missing.git"), Branch: "main", Dir: filepath.Join(t.TempDir(), "repo")}
	if _, err := s.Fetch(ctx); err == nil {
		t.Error("missing remote must fail")
	}
	if _, err := s.Export(ctx, "../etc"); err == nil {
		t.Error("path-like sha must be rejected")
	}
}

func TestCleanupKeepsCurrentAndNewest(t *testing.T) {
	bare, work := origin(t)
	s := &Syncer{URL: bare, Branch: "main", Dir: filepath.Join(t.TempDir(), "repo")}
	ctx := context.Background()
	var shas []string
	for i := 0; i < 4; i++ {
		if i > 0 {
			commit(t, work, "f.txt", strings.Repeat("x", i), "c")
		}
		h, err := s.Fetch(ctx)
		if err != nil {
			t.Fatal(err)
		}
		p, err := s.Export(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(time.Duration(i-10) * time.Hour)
		os.Chtimes(p, old, old)
		shas = append(shas, h)
	}
	s.Promote(shas[0]) // oldest is current: must survive although it is the oldest
	if err := s.Cleanup(1); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(s.checkoutsDir())
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if len(left) != 2 {
		t.Fatalf("left = %v", left)
	}
	for _, want := range []string{shas[0], shas[3]} {
		found := false
		for _, l := range left {
			found = found || l == want
		}
		if !found {
			t.Errorf("%s should remain: %v", want, left)
		}
	}
}
