package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/schorsch3000/prebake/internal/gitsync"
)

const hookMarker = "# prebake-managed"

const preCommitHook = "#!/bin/sh\n" + hookMarker + " pre-commit hook; remove this file to uninstall\nexec prebake validate --staged\n"

// prePushHook tells the daemon to fetch right after a push. Git has no client-side hook that
// runs after the push, so the sync runs in the background and waits for the pushed commit to
// become visible at the remote (prebake sync --commit retries until it is, or gives up). The push
// itself is never delayed and never fails because of this hook.
const prePushHook = `#!/bin/sh
` + hookMarker + ` pre-push hook: after a push, ask the prebake daemon to fetch now; remove this file to uninstall
# stdin: one line per pushed ref: <local ref> <local oid> <remote ref> <remote oid>
if ! command -v prebake >/dev/null 2>&1; then cat >/dev/null; exit 0; fi
while read -r local_ref local_oid remote_ref remote_oid; do
  case "$remote_ref" in refs/heads/*) ;; *) continue ;; esac   # branches only, no tags
  case "$local_oid" in *[!0]*) ;; *) continue ;; esac           # an all-zero oid is a deleted branch
  ( prebake sync --commit "$local_oid" --branch "${remote_ref#refs/heads/}" --quiet >/dev/null 2>&1 & )
done
exit 0
`

func cmdInstallHook(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install-hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "path of the config repo")
	force := fs.Bool("force", false, "overwrite an existing hook not written by prebake")
	withSync := fs.Bool("sync", false, "also install a pre-push hook that asks the daemon to fetch right after a push")
	url := fs.String("url", "", "with --sync: remember the daemon's status URL in the repo (git config prebake.url)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *url != "" && !*withSync {
		fmt.Fprintln(stderr, "prebake install-hook: --url only makes sense with --sync")
		return 2
	}
	if code := installHook(*repo, "pre-commit", preCommitHook, *force, stdout, stderr); code != 0 {
		return code
	}
	if !*withSync {
		return 0
	}
	if code := installHook(*repo, "pre-push", prePushHook, *force, stdout, stderr); code != 0 {
		return code
	}
	if *url != "" {
		if out, err := gitsync.Git(*repo, "config", "prebake.url", *url).CombinedOutput(); err != nil {
			fmt.Fprintf(stderr, "prebake install-hook: git config prebake.url: %v: %s\n", err, strings.TrimSpace(string(out)))
			return 2
		}
		fmt.Fprintln(stdout, "set git config prebake.url", *url)
	} else if os.Getenv("PREBAKE_URL") == "" {
		fmt.Fprintln(stdout, "note: the hook finds the daemon through $PREBAKE_URL or `git config prebake.url <url>` (or run install-hook again with --url)")
	}
	return 0
}

// installHook writes one hook script, protecting a hook that prebake did not write.
func installHook(repo, name, script string, force bool, stdout, stderr io.Writer) int {
	// --git-path resolves core.hooksPath and linked worktrees correctly
	out, err := gitsync.Git(repo, "rev-parse", "--git-path", "hooks/"+name).Output()
	if err != nil {
		fmt.Fprintf(stderr, "prebake install-hook: %s is not a git repository\n", repo)
		return 2
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(repo, path)
	}
	if existing, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(existing), hookMarker) && !force {
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
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
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
