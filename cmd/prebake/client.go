package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/schorsch3000/prebake/internal/api"
)

// clientFlags registers --url (default PREBAKE_URL or the local daemon).
func clientFlags(fs *flag.FlagSet) *string {
	def := os.Getenv("PREBAKE_URL")
	if def == "" {
		def = "http://127.0.0.1:8080"
	}
	return fs.String("url", def, "base URL of the daemon's status listener (PREBAKE_URL)")
}

// parseInterspersed parses flags that may come before or after positional
// arguments (`trigger base --kind refresh`), which package flag does not allow.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func fetchStatus(base string) (*api.StatusDTO, error) {
	resp, err := httpClient.Get(strings.TrimSuffix(base, "/") + "/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status: %s", resp.Status)
	}
	var dto api.StatusDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		return nil, fmt.Errorf("decode status: %w", err)
	}
	return &dto, nil
}

func short(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := clientFlags(fs)
	asJSON := fs.Bool("json", false, "print the raw JSON")
	if _, err := parseInterspersed(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	dto, err := fetchStatus(*url)
	if err != nil {
		fmt.Fprintln(stderr, "prebake status:", err)
		return 2
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(dto)
		return 0
	}
	fmt.Fprintf(stdout, "repo: %s  active=%s head=%s\n", dto.Repo.State, short(dto.Repo.Active, 7), short(dto.Repo.Head, 7))
	if dto.Repo.Error != "" {
		fmt.Fprintf(stdout, "      %s\n", strings.ReplaceAll(strings.TrimSpace(dto.Repo.Error), "\n", "\n      "))
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "CONTAINER\tFLAVOR\tSTATE\tLAST REBUILD\tLAST REFRESH\tERROR")
	for _, j := range dto.Jobs {
		e := strings.SplitN(j.Error, "\n", 2)[0]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", j.Container, j.Flavor, j.State, fmtTime(j.LastRebuild), fmtTime(j.LastRefresh), short(e, 60))
	}
	w.Flush()
	return 0
}

func cmdDryRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dry-run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := clientFlags(fs)
	if _, err := parseInterspersed(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	dto, err := fetchStatus(*url)
	if err != nil {
		fmt.Fprintln(stderr, "prebake dry-run:", err)
		return 2
	}
	jobs := append([]api.JobDTO(nil), dto.Jobs...)
	sort.SliceStable(jobs, func(i, k int) bool {
		a, b := jobs[i].NextRun, jobs[k].NextRun
		switch {
		case a == nil:
			return false
		case b == nil:
			return true
		}
		return a.Before(*b)
	})
	now := dto.GeneratedAt
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "CONTAINER\tFLAVOR\tNEXT\tAT\tIN")
	for _, j := range jobs {
		if j.NextRun == nil || j.NextKind == "" {
			fmt.Fprintf(w, "%s\t%s\t-\t-\t(%s)\n", j.Container, j.Flavor, j.State)
			continue
		}
		in := j.NextRun.Sub(now).Round(time.Minute)
		rel := "now"
		if in > 0 {
			rel = in.String()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", j.Container, j.Flavor, j.NextKind, fmtTime(j.NextRun), rel)
	}
	w.Flush()
	return 0
}

func post(base, path string) (string, int, error) {
	resp, err := httpClient.Post(strings.TrimSuffix(base, "/")+path, "", nil)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return strings.TrimSpace(string(b)), resp.StatusCode, nil
}

func cmdTrigger(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("trigger", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: prebake trigger <container>[:<flavor>] [--kind rebuild|refresh] [--url URL]")
	}
	url := clientFlags(fs)
	kind := fs.String("kind", "rebuild", "rebuild or refresh")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if len(pos) != 1 {
		fs.Usage()
		return 2
	}
	container, flavor, _ := strings.Cut(pos[0], ":")
	path := "/trigger/" + container
	if flavor != "" {
		path += "/" + flavor
	}
	body, code, err := post(*url, path+"?kind="+*kind)
	if err != nil {
		fmt.Fprintln(stderr, "prebake trigger:", err)
		return 2
	}
	if code != http.StatusAccepted {
		fmt.Fprintf(stderr, "prebake trigger: %d %s\n", code, body)
		return 1
	}
	fmt.Fprintln(stdout, body)
	return 0
}

func cmdGC(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := clientFlags(fs)
	if _, err := parseInterspersed(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	body, code, err := post(*url, "/gc")
	if err != nil {
		fmt.Fprintln(stderr, "prebake gc:", err)
		return 2
	}
	if code != http.StatusAccepted {
		fmt.Fprintf(stderr, "prebake gc: %d %s\n", code, body)
		return 1
	}
	fmt.Fprintln(stdout, body)
	return 0
}
