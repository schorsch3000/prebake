package main

import (
	"fmt"
	"io"
	"os"
)

// Set at build time: -ldflags "-X main.version=... -X main.revision=..." (the release archives).
// The container image is built without them, so that its compile layer is the same for every
// version; it sets PREBAKE_VERSION and PREBAKE_REVISION instead (see versionInfo).
var (
	version  = "dev"
	revision = "unknown"
)

// versionInfo returns the version and commit: the linker values win, the PREBAKE_* variables
// fill in what the linker did not set.
func versionInfo() (v, r string) {
	v, r = version, revision
	if v == "dev" {
		if e := os.Getenv("PREBAKE_VERSION"); e != "" {
			v = e
		}
	}
	if r == "unknown" {
		if e := os.Getenv("PREBAKE_REVISION"); e != "" {
			r = e
		}
	}
	return v, r
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, usage())
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage())
		return 0
	case "version", "-v", "--version":
		v, r := versionInfo()
		fmt.Fprintf(stdout, "prebake %s (%s)\n", v, r)
		return 0
	case "completion":
		return cmdCompletion(args[1:], stdout, stderr)
	case "__complete": // hidden: dynamic candidates for the completion scripts
		return cmdComplete(args[1:], stdout)
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
	case "logs":
		return cmdLogs(args[1:], stdout, stderr)
	case "dry-run":
		return cmdDryRun(args[1:], stdout, stderr)
	case "trigger":
		return cmdTrigger(args[1:], stdout, stderr)
	case "gc":
		return cmdGC(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "prebake: unknown command %q\n\n%s", args[0], usage())
		return 2
	}
}
