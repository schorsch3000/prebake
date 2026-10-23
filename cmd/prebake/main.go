package main

import (
	"fmt"
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
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Print(usage)
	case "daemon", "validate", "install-hook", "show-key", "status", "trigger", "dry-run", "gc":
		fmt.Fprintf(os.Stderr, "prebake %s: not implemented\n", os.Args[1])
		os.Exit(2)
	default:
		fmt.Fprintf(os.Stderr, "prebake: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
