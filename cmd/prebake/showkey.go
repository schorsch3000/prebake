package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/schorsch3000/prebake/internal/gitsync"
)

// dataDirDefault is the DATA_DIR env var, else /data.
func dataDirDefault() string {
	if d := os.Getenv("DATA_DIR"); d != "" {
		return d
	}
	return "/data"
}

func cmdShowKey(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("show-key", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", dataDirDefault(), "data directory (DATA_DIR)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	// creates the key on first use, exactly like the daemon does
	id, err := gitsync.LoadOrCreate(filepath.Join(*dataDir, "ssh"))
	if err != nil {
		fmt.Fprintln(stderr, "prebake show-key:", err)
		return 2
	}
	fmt.Fprintln(stdout, id.PublicLine)
	fmt.Fprintln(stderr, "fingerprint:", id.Fingerprint)
	fmt.Fprintln(stderr, "add the line above as a read-only deploy key to the config repo")
	return 0
}
