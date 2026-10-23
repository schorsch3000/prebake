package main

import (
	"fmt"
	"strings"
)

// flagSpec describes one flag for help and shell completion. Value is "" for a boolean
// flag, "dir" or "file" for paths, "text" for free text, or "a|b|c" for a fixed set.
type flagSpec struct {
	Name, Short, Value, Help string
}

// cmdSpec describes one subcommand. Args is "" (no positional arguments), "containers"
// (container or container:flavor, completed from the running daemon) or "shell".
type cmdSpec struct {
	Name, Group, Help, Args string
	Flags                   []flagSpec
}

const (
	groupClient  = "client commands (talk to a running daemon: --url or PREBAKE_URL)"
	groupRepo    = "config repo commands (no daemon needed)"
	groupOther   = "other"
	groupService = "service"
)

var (
	fURL   = flagSpec{Name: "url", Value: "text", Help: "base URL of the daemon's status API (default $PREBAKE_URL or http://127.0.0.1:8080)"}
	fColor = flagSpec{Name: "color", Value: "auto|always|never", Help: "colored output (default auto: on for a terminal, off for NO_COLOR)"}
)

// commands is the single source for the usage text and the shell completion scripts.
// A test checks that every listed flag is really accepted by its command.
var commands = []cmdSpec{
	{Name: "status", Group: groupClient, Help: "show build status (--watch for a live view)", Flags: []flagSpec{
		fURL, fColor,
		{Name: "json", Help: "print the raw JSON"},
		{Name: "watch", Short: "w", Help: "live view: redraw until Ctrl-C (needs a terminal)"},
		{Name: "interval", Short: "n", Value: "text", Help: "refresh interval for --watch, e.g. 1s (default 2s)"},
	}},
	{Name: "logs", Group: groupClient, Help: "show the build output of a container (-f follows a running build)", Args: "containers", Flags: []flagSpec{
		fURL,
		{Name: "follow", Short: "f", Help: "follow a running build until it ends; waits for a queued one to start"},
		{Name: "tail", Value: "text", Help: "only the last N lines"},
	}},
	{Name: "dry-run", Group: groupClient, Help: "show what is due when", Flags: []flagSpec{fURL, fColor}},
	{Name: "trigger", Group: groupClient, Help: "queue a rebuild or refresh now", Args: "containers", Flags: []flagSpec{
		fURL,
		{Name: "kind", Value: "rebuild|refresh", Help: "rebuild (default) or refresh"},
	}},
	{Name: "sync", Group: groupClient, Help: "fetch the config repo now instead of at the next poll", Flags: []flagSpec{
		fURL,
		{Name: "commit", Value: "text", Help: "wait until the daemon has seen this commit (HEAD: the one checked out here)"},
		{Name: "branch", Value: "text", Help: "the branch that was pushed; other branches than the daemon's are ignored"},
		{Name: "timeout", Value: "text", Help: "how long --commit waits for the commit to appear at the remote (default 2m)"},
		{Name: "quiet", Help: "print nothing when it worked"},
	}},
	{Name: "purge", Group: groupClient, Help: "remove the images of a container that is no longer in the config, now", Args: "orphans", Flags: []flagSpec{
		fURL,
		{Name: "yes", Help: "really remove (without it only the plan is shown)"},
	}},
	{Name: "gc", Group: groupClient, Help: "run registry garbage collection", Flags: []flagSpec{fURL}},

	{Name: "validate", Group: groupRepo, Help: "validate a config repo", Flags: []flagSpec{
		{Name: "repo", Value: "dir", Help: "path of the config repo (default .)"},
		{Name: "worktree", Help: "validate files on disk (default)"},
		{Name: "staged", Help: "validate the git index (pre-commit)"},
		{Name: "rev", Value: "text", Help: "validate this commit"},
		{Name: "format", Value: "text|json", Help: "output format"},
		{Name: "strict", Help: "treat warnings as errors"},
	}},
	{Name: "install-hook", Group: groupRepo, Help: "install the pre-commit hook", Flags: []flagSpec{
		{Name: "repo", Value: "dir", Help: "path of the config repo (default .)"},
		{Name: "force", Help: "overwrite a hook that prebake did not write"},
		{Name: "sync", Help: "also install a pre-push hook that asks the daemon to fetch right after a push"},
		{Name: "url", Value: "text", Help: "with --sync: remember the daemon's status URL in the repo (git config prebake.url)"},
	}},

	{Name: "show-key", Group: groupOther, Help: "print the SSH deploy public key (reads the data directory: run it in the container)", Flags: []flagSpec{
		{Name: "data-dir", Value: "dir", Help: "data directory (default $DATA_DIR or /data)"},
	}},
	{Name: "completion", Group: groupOther, Help: "print a shell completion script: bash, zsh or fish", Args: "shell"},
	{Name: "version", Group: groupOther, Help: "print the version"},

	{Name: "daemon", Group: groupService, Help: "the service, run by the container image (needs a config repo and Docker; not for your workstation)", Flags: []flagSpec{
		{Name: "repo-url", Value: "text", Help: "git URL of the config repo (REPO_URL)"},
		{Name: "repo-branch", Value: "text", Help: "branch to follow (REPO_BRANCH)"},
		{Name: "poll", Value: "text", Help: "how often to fetch the repo (POLL_INTERVAL)"},
		{Name: "jitter", Value: "text", Help: "max random start offset after a slot opens (JITTER)"},
		{Name: "data-dir", Value: "dir", Help: "state, keys, checkouts, registry storage (DATA_DIR)"},
		{Name: "read-addr", Value: "text", Help: "read-only registry listener (READ_ADDR)"},
		{Name: "write-addr", Value: "text", Help: "registry write listener (WRITE_ADDR)"},
		{Name: "status-addr", Value: "text", Help: "status and control API listener (STATUS_ADDR)"},
		{Name: "registry-host", Value: "text", Help: "host:port the Docker host pushes to (REGISTRY_HOST)"},
		{Name: "tls-cert", Value: "file", Help: "TLS certificate for the read-only listener (TLS_CERT)"},
		{Name: "tls-key", Value: "file", Help: "TLS key (TLS_KEY)"},
		{Name: "known-hosts", Value: "file", Help: "pinned known_hosts file (KNOWN_HOSTS)"},
		{Name: "verbose", Help: "log every registry request (VERBOSE)"},
	}},
}

func findCommand(name string) (cmdSpec, bool) {
	for _, c := range commands {
		if c.Name == name {
			return c, true
		}
	}
	return cmdSpec{}, false
}

// usage renders the help text from the command table.
func usage() string {
	var b strings.Builder
	b.WriteString("usage: prebake <command> [flags]\n")
	var last string
	for _, c := range commands {
		if c.Group != last {
			fmt.Fprintf(&b, "\n%s:\n", c.Group)
			last = c.Group
		}
		fmt.Fprintf(&b, "  %-13s %s\n", c.Name, c.Help)
	}
	b.WriteString("\nRun 'prebake <command> -h' for the flags of a command.\n")
	return b.String()
}
