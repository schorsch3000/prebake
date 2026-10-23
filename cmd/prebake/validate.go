package main

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/schorsch3000/prebake/internal/validate"
)

const validateUsage = `usage: prebake validate [--repo PATH] [--staged | --rev REV | --worktree] [--format text|json] [--strict]

  --worktree  validate files on disk (default)
  --staged    validate the git index (use in a pre-commit hook)
  --rev REV   validate a commit
  --strict    treat warnings as errors

exit codes: 0 ok, 1 validation errors, 2 usage or internal error
`

func cmdValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, validateUsage) }
	repo := fs.String("repo", ".", "path of the config repo")
	staged := fs.Bool("staged", false, "validate the git index")
	worktree := fs.Bool("worktree", false, "validate files on disk (default)")
	rev := fs.String("rev", "", "validate this commit")
	format := fs.String("format", "text", "output format: text or json")
	strict := fs.Bool("strict", false, "treat warnings as errors")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "prebake validate: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	modes := 0
	for _, set := range []bool{*staged, *worktree, *rev != ""} {
		if set {
			modes++
		}
	}
	if modes > 1 {
		fmt.Fprintln(stderr, "prebake validate: --staged, --rev and --worktree are mutually exclusive")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "prebake validate: unknown format %q\n", *format)
		return 2
	}

	dir := *repo
	if *staged || *rev != "" {
		tmp, err := os.MkdirTemp("", "prebake-validate-")
		if err != nil {
			fmt.Fprintln(stderr, "prebake validate:", err)
			return 2
		}
		defer os.RemoveAll(tmp)
		if *staged {
			err = exportIndex(*repo, tmp)
		} else {
			err = exportRev(*repo, *rev, tmp)
		}
		if err != nil {
			fmt.Fprintln(stderr, "prebake validate:", err)
			return 2
		}
		dir = tmp
	} else if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		fmt.Fprintf(stderr, "prebake validate: %s is not a directory\n", dir)
		return 2
	}

	res := validate.Dir(dir)
	if *format == "json" {
		if err := writeJSON(stdout, res); err != nil {
			fmt.Fprintln(stderr, "prebake validate:", err)
			return 2
		}
	} else {
		for _, i := range res.Issues {
			fmt.Fprintln(stdout, i)
		}
	}
	if res.Errors() > 0 || (*strict && res.Warnings() > 0) {
		return 1
	}
	return 0
}

func writeJSON(w io.Writer, res *validate.Result) error {
	type issue struct {
		Severity string `json:"severity"`
		File     string `json:"file"`
		Line     int    `json:"line,omitempty"`
		Message  string `json:"message"`
	}
	out := struct {
		Errors   int     `json:"errors"`
		Warnings int     `json:"warnings"`
		Issues   []issue `json:"issues"`
	}{Errors: res.Errors(), Warnings: res.Warnings(), Issues: []issue{}}
	for _, i := range res.Issues {
		out.Issues = append(out.Issues, issue{i.Severity.String(), i.File, i.Line, i.Msg})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func git(repo string, args ...string) *exec.Cmd {
	return exec.Command("git", append([]string{"-C", repo}, args...)...)
}

// exportIndex writes the staged tree into dst, so a partial commit is judged
// as it will be committed.
func exportIndex(repo, dst string) error {
	cmd := git(repo, "checkout-index", "-a", "--prefix="+dst+string(filepath.Separator))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout-index: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func exportRev(repo, rev, dst string) error {
	cmd := git(repo, "archive", "--format=tar", rev)
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
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
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
		// symlinks and other types are skipped: validation only needs regular files
	}
}
