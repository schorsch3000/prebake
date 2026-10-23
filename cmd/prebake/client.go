package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/schorsch3000/prebake/internal/api"
	"github.com/schorsch3000/prebake/internal/daemon"
)

// clientFlags registers --url (default PREBAKE_URL or the local daemon).
func clientFlags(fs *flag.FlagSet) *string {
	return fs.String("url", defaultURL(), "base URL of the daemon's status listener (PREBAKE_URL, else `git config prebake.url`)")
}

// defaultURL is $PREBAKE_URL, else the prebake.url of the git repository in the current
// directory (what `install-hook --sync --url` sets, for the pre-push hook), else the local daemon.
func defaultURL() string {
	if u := os.Getenv("PREBAKE_URL"); u != "" {
		return u
	}
	if out, err := exec.Command("git", "config", "--get", "prebake.url").Output(); err == nil {
		if u := strings.TrimSpace(string(out)); u != "" {
			return u
		}
	}
	return "http://127.0.0.1:8080"
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

// colorFlag registers --color.
func colorFlag(fs *flag.FlagSet) *string {
	return fs.String("color", "auto", "colored output: auto, always or never (auto: terminal only, off with NO_COLOR)")
}

// clientVersion is the version of this CLI; tests replace it.
var clientVersion = func() string { v, _ := versionInfo(); return v }

// parseSemver reads "1.2.3", "v1.2.3" and "1.2.3-rc1"; anything else (dev, local, edge, a branch
// name) is not comparable.
func parseSemver(v string) (major, minor, patch int, ok bool) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	n := make([]int, 3)
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return 0, 0, 0, false
		}
		n[i] = x
	}
	return n[0], n[1], n[2], true
}

// versionSkew returns a warning when the CLI and the daemon differ in major or minor version
// (a patch release does not change the API), and "" when they match or cannot be compared (a
// daemon running `edge` or a development build).
func versionSkew(client, daemon string) string {
	cMaj, cMin, _, ok1 := parseSemver(client)
	dMaj, dMin, _, ok2 := parseSemver(daemon)
	if !ok1 || !ok2 || (cMaj == dMaj && cMin == dMin) {
		return ""
	}
	if cMaj < dMaj || (cMaj == dMaj && cMin < dMin) {
		return fmt.Sprintf("this CLI (%s) is older than the daemon (%s): update the CLI, it may lack commands or flags the daemon has", client, daemon)
	}
	return fmt.Sprintf("this CLI (%s) is newer than the daemon (%s): update the daemon, it may not know the requests the CLI sends", client, daemon)
}

// renderStatus renders the status table as text.
func renderStatus(dto *api.StatusDTO, p palette) string {
	var b strings.Builder
	if dto.Version != "" {
		fmt.Fprintf(&b, "daemon: prebake %s (%s)\n", dto.Version, short(dto.Revision, 7))
		if w := versionSkew(clientVersion(), dto.Version); w != "" {
			fmt.Fprintf(&b, "%s\n", p.paint(ansiYellow, "warning: "+w))
		}
	}
	if dto.TimeZone != "" {
		fmt.Fprintf(&b, "time zone of the build slots: %s\n", dto.TimeZone)
	}
	fmt.Fprintf(&b, "repo: %s  active=%s head=%s\n", p.paint(stateCode(dto.Repo.State), dto.Repo.State),
		short(dto.Repo.Active, 7), short(dto.Repo.Head, 7))
	if dto.Repo.Error != "" {
		fmt.Fprintf(&b, "      %s\n", p.paint(ansiRed, strings.ReplaceAll(strings.TrimSpace(dto.Repo.Error), "\n", "\n      ")))
	}
	rows := [][]cell{{{"CONTAINER", ansiBold}, {"FLAVOR", ansiBold}, {"STATE", ansiBold}, {"LAST REBUILD", ansiBold}, {"LAST REFRESH", ansiBold}, {"NEEDS", ansiBold}, {"ERROR", ansiBold}}}
	for _, j := range dto.Jobs {
		state := j.State
		if j.RunningSince != nil && !dto.GeneratedAt.IsZero() {
			state += " " + dto.GeneratedAt.Sub(*j.RunningSince).Round(time.Second).String()
		}
		when := func(t *time.Time) cell {
			if t == nil {
				return cell{"never", ansiDim}
			}
			return cell{fmtTime(t), ""}
		}
		needs := cell{"-", ansiDim}
		if j.Needs != "" {
			needs = cell{j.Needs, ansiYellow}
		}
		rows = append(rows, []cell{{j.Container, ""}, {j.Flavor, ""}, {state, stateCode(j.State)},
			when(j.LastRebuild), when(j.LastRefresh), needs, {errorSummary(j.Error), ansiDim}})
	}
	b.WriteString(p.table(rows))
	return b.String()
}

// watchContext and the clock are replaced in tests.
var (
	watchContext = func() (context.Context, context.CancelFunc) {
		return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	}
	watchNow = time.Now
)

// Terminal control sequences of the live view: cursor home, erase to end of line, erase below.
const (
	cursorHome = "\x1b[H"
	eraseLine  = "\x1b[K"
	eraseBelow = "\x1b[J"
)

// watchStatus redraws the status every interval until ctx ends. It paints each frame in one
// write over the previous one (no flicker from clearing the screen), and keeps going when the
// daemon cannot be reached, so a restart of the daemon is visible instead of fatal.
func watchStatus(ctx context.Context, out io.Writer, interval time.Duration, p palette, fetch func() (*api.StatusDTO, error)) {
	frame := func() {
		var body string
		if dto, err := fetch(); err != nil {
			body = p.paint(ansiRed, "cannot reach the daemon: "+err.Error()+" (retrying)") + "\n"
		} else {
			body = renderStatus(dto, p)
		}
		footer := p.paint(ansiDim, fmt.Sprintf("updated %s, every %s, Ctrl-C to quit", watchNow().Format("15:04:05"), interval))
		var b strings.Builder
		b.WriteString(cursorHome)
		for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
			b.WriteString(line + eraseLine + "\n")
		}
		b.WriteString(eraseLine + "\n" + footer + eraseLine + "\n" + eraseBelow)
		io.WriteString(out, b.String())
	}
	io.WriteString(out, "\x1b[2J") // the first frame starts on a clean screen
	frame()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			frame()
		}
	}
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := clientFlags(fs)
	color := colorFlag(fs)
	asJSON := fs.Bool("json", false, "print the raw JSON")
	var watch bool
	fs.BoolVar(&watch, "watch", false, "live view: redraw until Ctrl-C (needs a terminal)")
	fs.BoolVar(&watch, "w", false, "same as --watch")
	interval := 2 * time.Second
	fs.DurationVar(&interval, "interval", interval, "refresh interval for --watch")
	fs.DurationVar(&interval, "n", interval, "same as --interval")
	if _, err := parseInterspersed(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	p, err := newPalette(*color, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "prebake status:", err)
		return 2
	}
	if watch {
		switch {
		case *asJSON:
			fmt.Fprintln(stderr, "prebake status: --watch and --json cannot be combined")
			return 2
		case interval < 200*time.Millisecond:
			fmt.Fprintln(stderr, "prebake status: --interval must be at least 200ms")
			return 2
		case !isTerminal(stdout):
			fmt.Fprintln(stderr, "prebake status: --watch needs a terminal (use a loop around plain status in scripts)")
			return 2
		}
		ctx, cancel := watchContext()
		defer cancel()
		watchStatus(ctx, stdout, interval, p, func() (*api.StatusDTO, error) { return fetchStatus(*url) })
		return 0
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
	fmt.Fprint(stdout, renderStatus(dto, p))
	return 0
}

func cmdDryRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dry-run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := clientFlags(fs)
	color := colorFlag(fs)
	if _, err := parseInterspersed(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	p, err := newPalette(*color, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "prebake dry-run:", err)
		return 2
	}
	dto, err := fetchStatus(*url)
	if err != nil {
		fmt.Fprintln(stderr, "prebake dry-run:", err)
		return 2
	}
	if w := versionSkew(clientVersion(), dto.Version); w != "" {
		fmt.Fprintln(stderr, "warning:", w)
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
	rows := [][]cell{{{"CONTAINER", ansiBold}, {"FLAVOR", ansiBold}, {"NEXT", ansiBold}, {"AT", ansiBold}, {"IN", ansiBold}, {"WHY", ansiBold}}}
	for _, j := range jobs {
		if j.NextRun == nil || j.NextKind == "" {
			rows = append(rows, []cell{{j.Container, ""}, {j.Flavor, ""}, {"-", ""}, {"-", ""}, {"(" + j.State + ")", stateCode(j.State)}, {j.Needs, ansiYellow}})
			continue
		}
		in := cell{"now", ansiYellow}
		if d := j.NextRun.Sub(now).Round(time.Minute); d > 0 {
			in = cell{d.String(), ""}
		}
		why := cell{j.Needs, ansiYellow}
		if j.Needs == "" {
			why = cell{"on schedule", ansiDim}
		}
		rows = append(rows, []cell{{j.Container, ""}, {j.Flavor, ""}, {j.NextKind, ""}, {fmtTime(j.NextRun), ""}, in, why})
	}
	fmt.Fprint(stdout, p.table(rows))
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

func cmdLogs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: prebake logs <container>[:<flavor>] [-f] [--tail N] [--url URL]")
	}
	url := clientFlags(fs)
	var follow bool
	fs.BoolVar(&follow, "follow", false, "follow a running build until it ends; waits for a queued one to start")
	fs.BoolVar(&follow, "f", false, "same as --follow")
	tail := fs.Int("tail", 0, "only the last N lines")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if len(pos) != 1 || *tail < 0 {
		fs.Usage()
		return 2
	}
	container, flavor, _ := strings.Cut(pos[0], ":")
	path := "/logs/" + container
	if flavor != "" {
		path += "/" + flavor
	}
	query := "?tail=" + strconv.Itoa(*tail)
	if follow {
		query += "&follow=1"
	}
	ctx, cancel := watchContext() // Ctrl-C ends a followed stream
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(*url, "/")+path+query, nil)
	if err != nil {
		fmt.Fprintln(stderr, "prebake logs:", err)
		return 2
	}
	// no client timeout: a followed build can run for a long time
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "prebake logs:", err)
		return 2
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fmt.Fprintf(stderr, "prebake logs: %d %s\n", resp.StatusCode, strings.TrimSpace(string(b)))
		return 1
	}
	if _, err := io.Copy(stdout, resp.Body); err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, "prebake logs: connection lost:", err)
		return 1
	}
	return 0
}

// syncPoll is how often `sync --commit` asks again while the commit is not visible yet.
var syncPoll = 2 * time.Second

func cmdSync(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: prebake sync [--commit SHA|HEAD] [--branch NAME] [--timeout 2m] [--quiet] [--url URL]")
	}
	url := clientFlags(fs)
	commit := fs.String("commit", "", "wait until the daemon has seen this commit (HEAD: the commit checked out here)")
	branch := fs.String("branch", "", "the branch that was pushed; the daemon ignores a request about another branch")
	timeout := fs.Duration("timeout", 2*time.Minute, "how long --commit waits for the commit to show up at the remote")
	quiet := fs.Bool("quiet", false, "print nothing when it worked")
	if _, err := parseInterspersed(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	sha := *commit
	if sha == "HEAD" {
		out, err := exec.Command("git", "rev-parse", "HEAD").Output()
		if err != nil {
			fmt.Fprintln(stderr, "prebake sync: --commit HEAD needs a git repository in the current directory")
			return 2
		}
		sha = strings.TrimSpace(string(out))
	}
	deadline := time.Now().Add(*timeout)
	client := &http.Client{Timeout: 3 * time.Minute} // a fetch can take a while
	for {
		q := "?"
		if sha != "" {
			q += "commit=" + sha + "&"
		}
		if *branch != "" {
			q += "branch=" + *branch
		}
		resp, err := client.Post(strings.TrimSuffix(*url, "/")+"/sync"+strings.TrimRight(q, "&?"), "", nil)
		if err != nil {
			fmt.Fprintln(stderr, "prebake sync:", err)
			return 2
		}
		var res daemon.SyncResult
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(stderr, "prebake sync: %d %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
			return 1
		}
		if err := json.Unmarshal(body, &res); err != nil {
			fmt.Fprintln(stderr, "prebake sync: cannot read the answer:", err)
			return 2
		}
		if res.Ignored {
			if !*quiet {
				fmt.Fprintf(stdout, "ignored: the daemon follows %s, not %s\n", res.Branch, *branch)
			}
			return 0
		}
		if sha != "" && (res.HasCommit == nil || !*res.HasCommit) {
			if time.Now().Add(syncPoll).After(deadline) {
				fmt.Fprintf(stderr, "prebake sync: commit %s did not show up on %s within %s\n", short(sha, 7), res.Branch, *timeout)
				return 1
			}
			time.Sleep(syncPoll)
			continue
		}
		if res.State != daemon.RepoOK {
			fmt.Fprintf(stderr, "prebake sync: repo is %s", res.State)
			if res.Error != "" {
				fmt.Fprintf(stderr, ":\n  %s", strings.ReplaceAll(strings.TrimSpace(res.Error), "\n", "\n  "))
			}
			fmt.Fprintln(stderr)
			return 1
		}
		if !*quiet {
			what := "up to date"
			if res.Changed {
				what = "new commit activated"
			}
			fmt.Fprintf(stdout, "synced: running %s on %s (%s)\n", short(res.Active, 7), res.Branch, what)
		}
		return 0
	}
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

// errorSummary is the one-line form of a job error for the table. Build errors are the
// operation first and docker's output after it, and the reason is at the end, so the last
// line is shown; `status --json` and the web page carry the whole text.
func errorSummary(e string) string {
	lines := strings.Split(strings.TrimSpace(e), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(lines) > 1 {
		last += "  (+" + fmt.Sprint(len(lines)-1) + " lines, see status --json)"
	}
	if len(last) > 110 {
		last = last[:107] + "..."
	}
	return last
}
