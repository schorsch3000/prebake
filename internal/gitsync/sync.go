package gitsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// AuthError means the remote refused our key: the deploy key has not been added
// to the repository yet (or was removed). The daemon waits and retries.
type AuthError struct{ Output string }

func (e *AuthError) Error() string {
	return "git: authentication failed (deploy key not accepted): " + e.Output
}

// HostKeyError means the SSH host key changed or could not be verified.
type HostKeyError struct{ Output string }

func (e *HostKeyError) Error() string { return "git: host key verification failed: " + e.Output }

// classify maps git/ssh stderr to a typed error, or a plain error.
func classify(op string, err error, output string) error {
	out := strings.TrimSpace(output)
	switch {
	case strings.Contains(out, "REMOTE HOST IDENTIFICATION HAS CHANGED"), strings.Contains(out, "Host key verification failed"):
		return &HostKeyError{Output: out}
	case strings.Contains(out, "Permission denied (publickey"), strings.Contains(out, "Permission denied, please try again"),
		strings.Contains(out, "Authentication failed"), strings.Contains(out, "Repository not found"),
		strings.Contains(out, "Could not read from remote repository") && strings.Contains(out, "access rights"):
		return &AuthError{Output: out}
	}
	return fmt.Errorf("%s: %v: %s", op, err, out)
}

// Syncer maintains a bare mirror of one branch and exports commits as
// checkouts. Layout under Dir:
//
//	repo.git            bare repository
//	checkouts/<sha>/    exported trees
//	current             symlink to the promoted checkout
type Syncer struct {
	URL      string
	Branch   string
	Dir      string
	Identity *Identity // nil: no SSH key (https or local URLs)
	// KnownHosts is a pinned known_hosts file. Empty means trust on first use:
	// the first key seen is recorded and a later change fails the fetch.
	KnownHosts string
}

func (s *Syncer) repoDir() string      { return filepath.Join(s.Dir, "repo.git") }
func (s *Syncer) checkoutsDir() string { return filepath.Join(s.Dir, "checkouts") }
func (s *Syncer) currentLink() string  { return filepath.Join(s.Dir, "current") }

// sshCommand is the value of GIT_SSH_COMMAND.
func (s *Syncer) sshCommand() string {
	if s.Identity == nil {
		return "ssh -o BatchMode=yes"
	}
	known, strict := s.KnownHosts, "yes"
	if known == "" {
		known, strict = filepath.Join(filepath.Dir(s.Identity.KeyPath), "known_hosts"), "accept-new"
	}
	return strings.Join([]string{
		"ssh", "-i", s.Identity.KeyPath, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
		"-o", "UserKnownHostsFile=" + known, "-o", "StrictHostKeyChecking=" + strict,
	}, " ")
}

func (s *Syncer) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+s.sshCommand(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// Fetch clones on first use, otherwise fetches the branch, and returns the
// commit the branch points to.
func (s *Syncer) Fetch(ctx context.Context) (string, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return "", err
	}
	ref := "refs/heads/" + s.Branch
	if _, err := os.Stat(s.repoDir()); errors.Is(err, os.ErrNotExist) {
		tmp := s.repoDir() + ".tmp"
		os.RemoveAll(tmp)
		if out, err := s.git(ctx, s.Dir, "clone", "--bare", "--single-branch", "--branch", s.Branch, s.URL, tmp); err != nil {
			os.RemoveAll(tmp)
			return "", classify("git clone", err, out)
		}
		if err := os.Rename(tmp, s.repoDir()); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else if out, err := s.git(ctx, s.repoDir(), "fetch", "--prune", s.URL, "+"+ref+":"+ref); err != nil {
		return "", classify("git fetch", err, out)
	}
	head, err := s.git(ctx, s.repoDir(), "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("branch %q not found in %s: %s", s.Branch, s.URL, head)
	}
	return head, nil
}

// Export writes the tree of sha to checkouts/<sha> (reusing an existing one)
// and returns its path.
func (s *Syncer) Export(ctx context.Context, sha string) (string, error) {
	if len(sha) < 7 || strings.ContainsAny(sha, "/.") {
		return "", fmt.Errorf("invalid commit %q", sha)
	}
	dest := filepath.Join(s.checkoutsDir(), sha)
	if st, err := os.Stat(dest); err == nil && st.IsDir() {
		return dest, nil
	}
	if err := os.MkdirAll(s.checkoutsDir(), 0o755); err != nil {
		return "", err
	}
	tmp := dest + ".tmp"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	if err := ExportRev(s.repoDir(), sha, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	return dest, nil
}

// Promote atomically makes checkouts/<sha> the current checkout.
func (s *Syncer) Promote(sha string) error {
	if _, err := os.Stat(filepath.Join(s.checkoutsDir(), sha)); err != nil {
		return err
	}
	tmp := s.currentLink() + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(filepath.Join("checkouts", sha), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, s.currentLink())
}

// Current returns the promoted checkout, if any.
func (s *Syncer) Current() (path, sha string, ok bool) {
	target, err := os.Readlink(s.currentLink())
	if err != nil {
		return "", "", false
	}
	return filepath.Join(s.Dir, target), filepath.Base(target), true
}

// Cleanup removes all but the keep newest checkouts; the current one is always kept.
func (s *Syncer) Cleanup(keep int) error {
	entries, err := os.ReadDir(s.checkoutsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	_, cur, _ := s.Current()
	type cp struct {
		name string
		mod  int64
	}
	var all []cp
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		all = append(all, cp{e.Name(), info.ModTime().UnixNano()})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod > all[j].mod })
	var errs []error
	for i, c := range all {
		if i < keep || c.name == cur {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.checkoutsDir(), c.name)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
