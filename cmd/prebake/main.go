package main

import (
	"fmt"
	"io"
	"os"
)

// Set at build time: -ldflags "-X main.version=... -X main.revision=...".
var (
	version  = "dev"
	revision = "unknown"
)

const usage = `usage: prebake <command>

commands:
  daemon        run the scheduler, registry and status listeners
  validate      validate a config repo
  install-hook  install the pre-commit hook
  show-key      print the SSH deploy public key
  status        show build status
  trigger       queue a rebuild or refresh now
  dry-run       show what is due when
  gc            run registry garbage collection
  version       print the version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "-v", "--version":
		fmt.Fprintf(stdout, "prebake %s (%s)\n", version, revision)
		return 0
	case "validate":
		return cmdValidate(args[1:], stdout, stderr)
	case "install-hook":
		return cmdInstallHook(args[1:], stdout, stderr)
	case "show-key":
		return cmdShowKey(args[1:], stdout, stderr)
	case "daemon":
		return cmdDaemon(args[1:], stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "dry-run":
		return cmdDryRun(args[1:], stdout, stderr)
	case "trigger":
		return cmdTrigger(args[1:], stdout, stderr)
	case "gc":
		return cmdGC(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "prebake: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
