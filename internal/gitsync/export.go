package gitsync

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Git returns a git command running in repo (-C).
func Git(repo string, args ...string) *exec.Cmd {
	return exec.Command("git", append([]string{"-C", repo}, args...)...)
}

// ExportIndex writes the staged tree of repo into dst, so a partial commit is
// judged as it will be committed.
func ExportIndex(repo, dst string) error {
	cmd := Git(repo, "checkout-index", "-a", "--prefix="+dst+string(filepath.Separator))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout-index: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ExportRev writes the tree of rev into dst (git archive, extracted safely).
// repo may be a bare repository.
func ExportRev(repo, rev, dst string) error {
	cmd := Git(repo, "archive", "--format=tar", rev)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	extractErr := untar(pipe, dst)
	io.Copy(io.Discard, pipe)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive %s: %v: %s", rev, err, strings.TrimSpace(stderr.String()))
	}
	return extractErr
}

func untar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(h.Name))
		if rel, err := filepath.Rel(dst, target); err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("unsafe path in archive: %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
		// symlinks and other types are skipped: the config repo only needs regular files
	}
}
