package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/schorsch3000/prebake/internal/gitsync"
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
			err = gitsync.ExportIndex(*repo, tmp)
		} else {
			err = gitsync.ExportRev(*repo, *rev, tmp)
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
