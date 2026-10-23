package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const hookMarker = "# prebake-managed"

const hookScript = "#!/bin/sh\n" + hookMarker + " pre-commit hook; remove this file to uninstall\nexec prebake validate --staged\n"

func cmdInstallHook(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install-hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "path of the config repo")
	force := fs.Bool("force", false, "overwrite an existing hook not written by prebake")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	// --git-path resolves core.hooksPath and linked worktrees correctly
	out, err := git(*repo, "rev-parse", "--git-path", "hooks/pre-commit").Output()
	if err != nil {
		fmt.Fprintf(stderr, "prebake install-hook: %s is not a git repository\n", *repo)
		return 2
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(*repo, path)
	}
	if existing, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(existing), hookMarker) && !*force {
			fmt.Fprintf(stderr, "prebake install-hook: %s exists and was not written by prebake (use --force to overwrite)\n", path)
			return 1
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stderr, "prebake install-hook:", err)
		return 2
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintln(stderr, "prebake install-hook:", err)
		return 2
	}
	if err := os.WriteFile(path, []byte(hookScript), 0o755); err != nil {
		fmt.Fprintln(stderr, "prebake install-hook:", err)
		return 2
	}
	if err := os.Chmod(path, 0o755); err != nil { // WriteFile keeps the mode of an existing file
		fmt.Fprintln(stderr, "prebake install-hook:", err)
		return 2
	}
	fmt.Fprintln(stdout, "installed", path)
	return 0
}
