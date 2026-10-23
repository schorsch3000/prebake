package main

import (
	"fmt"
	"io"
	"os"
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
	case "validate":
		return cmdValidate(args[1:], stdout, stderr)
	case "install-hook":
		return cmdInstallHook(args[1:], stdout, stderr)
	case "show-key":
		return cmdShowKey(args[1:], stdout, stderr)
	case "daemon", "status", "trigger", "dry-run", "gc":
		fmt.Fprintf(stderr, "prebake %s: not implemented\n", args[0])
		return 2
	default:
		fmt.Fprintf(stderr, "prebake: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
