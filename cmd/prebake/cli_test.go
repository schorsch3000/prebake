package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/schorsch3000/prebake/internal/api"
)

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

func stripANSI(s string) string { return ansi.ReplaceAllString(s, "") }

// --- help ---------------------------------------------------------------------------------

func TestUsageIsGeneratedFromTheCommandTable(t *testing.T) {
	u := usage()
	for _, c := range commands {
		if !strings.Contains(u, "  "+c.Name+" ") {
			t.Errorf("usage lacks %s", c.Name)
		}
	}
	for _, g := range []string{groupClient, groupRepo, groupOther, groupService} {
		if !strings.Contains(u, g+":") {
			t.Errorf("usage lacks the group %q", g)
		}
	}
	if !strings.Contains(u, "the service, run by the container image") {
		t.Errorf("daemon must be marked as the container's service:\n%s", u)
	}
	// the service comes after the client commands: it is not what a workstation user wants
	if strings.Index(u, groupService) < strings.Index(u, groupClient) {
		t.Error("service group should be listed last")
	}
	if code, out, _ := runCLI(t, "--help"); code != 0 || out != u {
		t.Errorf("--help: %d", code)
	}
}

// Every flag the table promises (help and completion) must really exist.
func TestCommandTableFlagsExist(t *testing.T) {
	tmp := t.TempDir() // never a git repo: commands that would act on a repo fail instead
	safe := map[string][]string{
		"status": {"--url", "http://127.0.0.1:1"}, "dry-run": {"--url", "http://127.0.0.1:1"},
		"trigger": {"--url", "http://127.0.0.1:1", "x"}, "logs": {"--url", "http://127.0.0.1:1", "x"}, "gc": {"--url", "http://127.0.0.1:1"},
		"validate": {"--repo", tmp}, "install-hook": {"--repo", tmp}, "sync": {"--url", "http://127.0.0.1:1"}, "show-key": {"--data-dir", tmp},
	}
	for _, c := range commands {
		base, ok := safe[c.Name]
		if !ok {
			continue // daemon would start; completion and version have no flags
		}
		for _, f := range c.Flags {
			for _, d := range f.dashed() {
				args := append([]string{c.Name, d}, base...)
				switch {
				case f.Value == "":
				case f.enum() != nil:
					args = append([]string{c.Name, d, f.enum()[0]}, base...)
				case f.Name == "interval":
					args = append([]string{c.Name, d, "1s"}, base...)
				case f.Name == "tail":
					args = append([]string{c.Name, d, "5"}, base...)
				default:
					args = append([]string{c.Name, d, "x"}, base...)
				}
				if c.Name == "status" && (f.Name == "watch" || f.Short == "w") || c.Name == "logs" && f.Name == "tail" || c.Name == "sync" && f.Name == "timeout" {
					continue // would wait for a terminal; covered by its own test
				}
				_, _, errOut := runCLI(t, args...)
				if strings.Contains(errOut, "provided but not defined") {
					t.Errorf("%s: flag %s is in the command table but not accepted: %s", c.Name, d, errOut)
				}
			}
		}
	}
	// ... and the daemon's flags, through its parser (it only parses here, nothing starts)
	d, _ := findCommand("daemon")
	var sink bytes.Buffer
	for _, f := range d.Flags {
		args := []string{"--" + f.Name}
		if f.Value != "" {
			args = append(args, "x")
		}
		if f.Name == "poll" || f.Name == "jitter" {
			args = []string{"--" + f.Name, "1m"}
		}
		_, err := parseDaemonConfig(append(args, "--repo-url", "u"), func(string) string { return "" }, &sink)
		if err != nil && strings.Contains(err.Error()+sink.String(), "provided but not defined") {
			t.Errorf("daemon: flag --%s is in the command table but not accepted", f.Name)
		}
	}
}

// --- color ----------------------------------------------------------------------------------

func TestPaletteModes(t *testing.T) {
	var buf bytes.Buffer
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	for mode, want := range map[string]bool{"always": true, "never": false, "auto": false /* a buffer is no terminal */, "": false} {
		p, err := newPalette(mode, &buf)
		if err != nil || p.on != want {
			t.Errorf("%q: %v %v", mode, p.on, err)
		}
	}
	if _, err := newPalette("rainbow", &buf); err == nil {
		t.Error("an unknown mode must be an error")
	}
	// auto on a terminal: on, unless NO_COLOR or TERM=dumb
	old := isTerminal
	isTerminal = func(io.Writer) bool { return true }
	defer func() { isTerminal = old }()
	if p, _ := newPalette("auto", &buf); !p.on {
		t.Error("auto on a terminal should color")
	}
	t.Setenv("NO_COLOR", "1")
	if p, _ := newPalette("auto", &buf); p.on {
		t.Error("NO_COLOR must switch color off")
	}
	if p, _ := newPalette("always", &buf); !p.on {
		t.Error("--color always beats NO_COLOR")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if p, _ := newPalette("auto", &buf); p.on {
		t.Error("TERM=dumb must switch color off")
	}
}

func TestStateColors(t *testing.T) {
	for state, want := range map[string]string{
		"ok": ansiGreen, "idle": ansiGreen, "queued": ansiYellow, "building": ansiYellow, "retrying": ansiYellow,
		"waiting-for-deploy-key": ansiYellow, "parked": ansiRed, "skipped": ansiRed, "degraded": ansiRed, "fetch-error": ansiRed,
	} {
		if got := stateCode(state); got != want {
			t.Errorf("%s: %q", state, got)
		}
	}
}

func TestColoredOutputIsThePlainOutputWithEscapes(t *testing.T) {
	srv, _ := statusServer(t, statusJSON)
	_, plain, _ := runCLI(t, "status", "--color", "never", "--url", srv.URL)
	code, colored, _ := runCLI(t, "status", "--color", "always", "--url", srv.URL)
	if code != 0 || colored == plain {
		t.Fatalf("%d: color must change the output", code)
	}
	if got := stripANSI(colored); got != plain {
		t.Errorf("columns must line up exactly as without color:\n--- colored, escapes stripped\n%s--- plain\n%s", got, plain)
	}
	for _, want := range []string{ansiRed + "parked", ansiGreen + "idle", ansiYellow + "queued", ansiBold + "CONTAINER"} {
		if !strings.Contains(colored, want) {
			t.Errorf("colored output lacks %q", strings.ReplaceAll(want, "\x1b", `\e`))
		}
	}
	if strings.Contains(plain, "\x1b") {
		t.Error("--color never must not print escapes")
	}
	if strings.Contains(colored, "late"+ansiReset) {
		t.Error("uncolored cells must not carry stray reset sequences")
	}
	// a buffer is not a terminal: auto means plain, which keeps pipes and scripts clean
	_, auto, _ := runCLI(t, "status", "--url", srv.URL)
	if auto != plain {
		t.Error("auto must not color a non-terminal")
	}
	// dry-run too
	_, dp, _ := runCLI(t, "dry-run", "--color", "never", "--url", srv.URL)
	_, dc, _ := runCLI(t, "dry-run", "--color", "always", "--url", srv.URL)
	if stripANSI(dc) != dp || dc == dp {
		t.Errorf("dry-run color:\n%s\n%s", dc, dp)
	}
	if code, _, e := runCLI(t, "status", "--color", "rainbow", "--url", srv.URL); code != 2 || !strings.Contains(e, "auto, always or never") {
		t.Errorf("bad --color: %d %q", code, e)
	}
}

func TestStatusShowsHowLongABuildRuns(t *testing.T) {
	dto := `{"generated_at":"2026-10-22T02:10:00Z","repo":{"state":"ok"},"jobs":[
	 {"container":"base","flavor":"default","state":"building","running_since":"2026-10-22T02:06:48Z"}]}`
	srv, _ := statusServer(t, dto)
	_, out, _ := runCLI(t, "status", "--url", srv.URL)
	if !strings.Contains(out, "building 3m12s") {
		t.Errorf("running time missing:\n%s", out)
	}
}

// --- watch ----------------------------------------------------------------------------------

func TestWatchStatusRedrawsInPlaceAndSurvivesAnOutage(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	fetch := func() (*api.StatusDTO, error) {
		calls++
		if calls == 2 {
			return nil, io.ErrUnexpectedEOF // the daemon restarts
		}
		if calls == 3 {
			cancel()
		}
		return &api.StatusDTO{Repo: api.RepoDTO{State: "ok"}, Jobs: []api.JobDTO{{Container: "base", Flavor: "default", State: "idle"}}}, nil
	}
	watchNow = func() time.Time { return time.Date(2026, 10, 22, 12, 34, 56, 0, time.UTC) }
	watchStatus(ctx, &out, 10*time.Millisecond, palette{}, fetch)
	got := out.String()
	if calls != 3 {
		t.Fatalf("fetches = %d", calls)
	}
	if strings.Count(got, cursorHome) != 3 || !strings.HasPrefix(got, "\x1b[2J") || strings.Count(got, "\x1b[2J") != 1 {
		t.Errorf("each frame starts at the cursor home, only the first clears the screen: %q", got)
	}
	if strings.Count(got, "base") != 2 || !strings.Contains(got, "cannot reach the daemon: unexpected EOF (retrying)") {
		t.Errorf("frames:\n%s", stripANSI(got))
	}
	if !strings.Contains(got, "updated 12:34:56, every 10ms, Ctrl-C to quit") || !strings.Contains(got, eraseLine) || !strings.HasSuffix(got, eraseBelow) {
		t.Errorf("footer/erase sequences missing: %q", got)
	}
}

func TestWatchFlagChecks(t *testing.T) {
	srv, _ := statusServer(t, statusJSON)
	// not a terminal (a bytes.Buffer): refused instead of spewing escape codes into a pipe
	if code, _, e := runCLI(t, "status", "--watch", "--url", srv.URL); code != 2 || !strings.Contains(e, "needs a terminal") {
		t.Errorf("non-terminal: %d %q", code, e)
	}
	old := isTerminal
	isTerminal = func(io.Writer) bool { return true }
	defer func() { isTerminal = old }()
	if code, _, e := runCLI(t, "status", "--watch", "--json", "--url", srv.URL); code != 2 || !strings.Contains(e, "cannot be combined") {
		t.Errorf("--json: %d %q", code, e)
	}
	if code, _, e := runCLI(t, "status", "-w", "-n", "10ms", "--url", srv.URL); code != 2 || !strings.Contains(e, "at least 200ms") {
		t.Errorf("interval: %d %q", code, e)
	}
	// on a terminal it runs until the context ends; -w and -n are the short forms
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	oldCtx := watchContext
	watchContext = func() (context.Context, context.CancelFunc) { return ctx, func() {} }
	defer func() { watchContext = oldCtx }()
	code, out, e := runCLI(t, "status", "-w", "-n", "200ms", "--url", srv.URL)
	if code != 0 || !strings.Contains(out, "parked") || !strings.Contains(out, "Ctrl-C to quit") {
		t.Errorf("watch run: %d %q %q", code, out, e)
	}
}

// --- completion -------------------------------------------------------------------------------

func TestCompleteContainersFromTheDaemon(t *testing.T) {
	srv, _ := statusServer(t, statusJSON)
	_, out, _ := runCLI(t, "__complete", "containers", "--url", srv.URL)
	if got := strings.Fields(out); strings.Join(got, ",") != "late,late:default,parked,parked:cuda,soon,soon:default" {
		t.Errorf("candidates = %v", got)
	}
	// an unreachable daemon yields nothing, silently
	if code, out, e := runCLI(t, "__complete", "containers", "--url", "http://127.0.0.1:1"); code != 0 || out != "" || e != "" {
		t.Errorf("unreachable: %d %q %q", code, out, e)
	}
	if code, out, _ := runCLI(t, "__complete", "nonsense"); code != 0 || out != "" {
		t.Errorf("unknown request: %d %q", code, out)
	}
	if strings.Contains(usage(), "__complete") {
		t.Error("the helper command is hidden")
	}
}

func TestCompletionCommand(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		code, out, _ := runCLI(t, "completion", shell)
		if code != 0 || out == "" {
			t.Fatalf("%s: %d", shell, code)
		}
		for _, c := range commands {
			if !strings.Contains(out, c.Name) {
				t.Errorf("%s script lacks the command %s", shell, c.Name)
			}
		}
		for _, f := range []string{"watch", "interval", "color", "kind", "staged", "strict"} {
			if !strings.Contains(out, f) {
				t.Errorf("%s script lacks the flag %s", shell, f)
			}
		}
		if !strings.Contains(out, "__complete containers") {
			t.Errorf("%s script must ask the daemon for container names", shell)
		}
	}
	if code, _, e := runCLI(t, "completion", "powershell"); code != 2 || !strings.Contains(e, "bash|zsh|fish") {
		t.Errorf("unknown shell: %d %q", code, e)
	}
	if code, _, _ := runCLI(t, "completion"); code != 2 {
		t.Errorf("no shell: %d", code)
	}
}

// complete runs the generated bash script for a command line and returns the candidates.
// `prebake` inside the script is replaced by a function that plays the daemon's helper.
func bashComplete(t *testing.T, line string) []string {
	t.Helper()
	_, script, _ := runCLI(t, "completion", "bash")
	words := strings.Fields(line)
	if strings.HasSuffix(line, " ") {
		words = append(words, "")
	}
	// the cursor word for bash, split the way bash does (':' is a word break)
	var comp []string
	for _, w := range words {
		comp = append(comp, w)
	}
	prog := `
prebake() { if [ "$1" = __complete ]; then printf 'dev\ndev:gpu\nbase\n'; fi; }
` + script + `
COMP_LINE=` + shQuote(line) + `
COMP_POINT=${#COMP_LINE}
COMP_WORDS=(` + shWords(comp) + `)
COMP_CWORD=` + itoa(len(comp)-1) + `
_prebake
printf '%s\n' "${COMPREPLY[@]}"
`
	out, err := exec.Command("bash", "-c", prog).CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	return strings.Fields(string(out))
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
func shWords(w []string) string {
	var q []string
	for _, x := range w {
		q = append(q, shQuote(x))
	}
	return strings.Join(q, " ")
}
func itoa(n int) string { return strconv.Itoa(n) }

func TestBashCompletion(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	eq := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v", got, want)
		}
	}
	eq(bashComplete(t, "prebake tri"), "trigger")
	eq(bashComplete(t, "prebake st"), "status")
	eq(bashComplete(t, "prebake status --co"), "--color")
	eq(bashComplete(t, "prebake status --color "), "auto", "always", "never")
	eq(bashComplete(t, "prebake trigger --kind "), "rebuild", "refresh")
	eq(bashComplete(t, "prebake completion "), "bash", "zsh", "fish")
	eq(bashComplete(t, "prebake validate --format "), "text", "json")
	// container names come from the daemon; flags only when a dash was typed
	eq(bashComplete(t, "prebake trigger d"), "dev", "dev:gpu")
	eq(bashComplete(t, "prebake trigger b"), "base")
	// the word 'dev:g' is one word for the user; only the part after the colon is replaced
	eq(bashComplete(t, "prebake trigger dev:g"), "gpu")
}

func TestZshAndFishScriptsAreWellFormed(t *testing.T) {
	_, zsh, _ := runCLI(t, "completion", "zsh")
	for _, want := range []string{"#compdef prebake", "_arguments", "_describe 'command' cmds", "'(-w --watch)'{-w,--watch}", "--kind[", ":value:(rebuild refresh)", "_prebake_containers", `_prebake "$@"`} {
		if !strings.Contains(zsh, want) {
			t.Errorf("zsh script lacks %q", want)
		}
	}
	for i, line := range strings.Split(zsh, "\n") {
		if strings.Count(strings.ReplaceAll(line, `'\''`, ""), "'")%2 != 0 {
			t.Errorf("zsh script line %d has unbalanced quotes: %s", i+1, line)
		}
	}
	_, fish, _ := runCLI(t, "completion", "fish")
	for _, want := range []string{"complete -c prebake -f", "__fish_use_subcommand", "-l watch -s w", "-l kind", "-x -a 'rebuild refresh'", "(prebake __complete containers 2>/dev/null)"} {
		if !strings.Contains(fish, want) {
			t.Errorf("fish script lacks %q", want)
		}
	}
	// syntax-check with the real shells where they are installed
	if _, err := exec.LookPath("zsh"); err == nil {
		f := t.TempDir() + "/_prebake"
		os.WriteFile(f, []byte(zsh), 0o644)
		if out, err := exec.Command("zsh", "-n", f).CombinedOutput(); err != nil {
			t.Errorf("zsh -n: %v\n%s", err, out)
		}
	}
	if _, err := exec.LookPath("fish"); err == nil {
		f := t.TempDir() + "/prebake.fish"
		os.WriteFile(f, []byte(fish), 0o644)
		if out, err := exec.Command("fish", "--no-execute", f).CombinedOutput(); err != nil {
			t.Errorf("fish --no-execute: %v\n%s", err, out)
		}
	}
}

// --- logs -------------------------------------------------------------------------------------

func logsServer(t *testing.T) (*httptest.Server, *[]string, chan struct{}) {
	var reqs []string
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.URL.RequestURI())
		switch {
		case strings.HasPrefix(r.URL.Path, "/logs/nope"):
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"not found: unknown container"}`))
		case r.URL.Query().Get("follow") == "1":
			w.WriteHeader(200)
			w.Write([]byte("#1 building\n"))
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
			w.Write([]byte("#2 done\n"))
		default:
			w.WriteHeader(200)
			w.Write([]byte("#1 old output\n"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs, release
}

func TestLogsCommand(t *testing.T) {
	srv, reqs, release := logsServer(t)
	close(release)
	code, out, e := runCLI(t, "logs", "dev", "--url", srv.URL)
	if code != 0 || out != "#1 old output\n" {
		t.Fatalf("%d %q %q", code, out, e)
	}
	// flags after the argument, container:flavor, --tail, -f
	code, out, e = runCLI(t, "logs", "dev:gpu", "-f", "--tail", "50", "--url", srv.URL)
	if code != 0 || out != "#1 building\n#2 done\n" {
		t.Fatalf("follow: %d %q %q", code, out, e)
	}
	want := []string{"/logs/dev?tail=0", "/logs/dev/gpu?tail=50&follow=1"}
	if strings.Join(*reqs, " ") != strings.Join(want, " ") {
		t.Errorf("requests = %v", *reqs)
	}
	if code, _, e := runCLI(t, "logs", "nope", "--url", srv.URL); code != 1 || !strings.Contains(e, "404") || !strings.Contains(e, "unknown container") {
		t.Errorf("unknown container: %d %q", code, e)
	}
	for _, args := range [][]string{{"logs"}, {"logs", "a", "b"}, {"logs", "a", "--tail", "-1"}} {
		if code, _, _ := runCLI(t, append(args, "--url", srv.URL)...); code != 2 {
			t.Errorf("%v: want exit 2, got %d", args, code)
		}
	}
	if code, _, e := runCLI(t, "logs", "dev", "--url", "http://127.0.0.1:1"); code != 2 || e == "" {
		t.Errorf("unreachable: %d %q", code, e)
	}
}

func TestLogsFollowEndsOnCtrlC(t *testing.T) {
	srv, _, _ := logsServer(t) // the stream never ends by itself
	ctx, cancel := context.WithCancel(context.Background())
	oldCtx := watchContext
	watchContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	defer func() { watchContext = oldCtx }()
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	code, out, e := runCLI(t, "logs", "dev", "-f", "--url", srv.URL)
	if code != 0 || out != "#1 building\n" {
		t.Errorf("Ctrl-C ends a followed stream quietly: %d %q %q", code, out, e)
	}
}

// examples/status-demo/status is documented as a way to try the client without a daemon: it must
// stay valid and show every state.
func TestStatusDemoFileRendersEveryState(t *testing.T) {
	b, err := os.ReadFile("../../examples/status-demo/status")
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := statusServer(t, string(b))
	code, out, e := runCLI(t, "status", "--color", "never", "--url", srv.URL)
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	for _, want := range []string{"repo: degraded", "idle", "building 3m12s", "queued", "retrying", "parked", "skipped", "never", "CET +01:00"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = runCLI(t, "dry-run", "--color", "never", "--url", srv.URL)
	if code != 0 || !strings.Contains(out, "now") || !strings.Contains(out, "15m0s") || !strings.Contains(out, "(parked)") {
		t.Errorf("dry-run: %d\n%s", code, out)
	}
	if _, colored, _ := runCLI(t, "status", "--color", "always", "--url", srv.URL); !strings.Contains(colored, ansiRed+"parked") || !strings.Contains(colored, ansiYellow+"building") {
		t.Error("colors")
	}
}

func TestVersionSkew(t *testing.T) {
	for _, c := range []struct {
		client, daemon string
		warn           string // substring of the warning, "" for none
	}{
		{"0.2.1", "0.2.1", ""},
		{"0.2.0", "0.2.3", ""}, // patch releases do not change the API
		{"v0.2.0", "0.2.0", ""},
		{"0.2.1", "0.3.0", "older than the daemon"},
		{"0.3.0", "0.2.9", "newer than the daemon"},
		{"0.9.0", "1.0.0", "older than the daemon"},
		{"2.0.0", "1.9.9", "newer than the daemon"},
		{"0.2.1", "edge", ""},  // a rolling tag cannot be compared
		{"dev", "0.2.1", ""},   // a development build neither
		{"0.2.1", "local", ""}, // a locally built image
		{"0.2.1", "", ""},      // an old daemon that does not report its version
		{"0.2.1-rc1", "0.3.0", "older than the daemon"},
		{"0.2", "0.3.0", ""}, // not a full version
	} {
		got := versionSkew(c.client, c.daemon)
		if (c.warn == "") != (got == "") || !strings.Contains(got, c.warn) {
			t.Errorf("%q vs %q: %q, want %q", c.client, c.daemon, got, c.warn)
		}
	}
}

func TestStatusShowsTheDaemonVersionAndWarnsOnSkew(t *testing.T) {
	dto := `{"generated_at":"2026-10-22T02:10:00Z","version":"0.3.0","revision":"abcdef1234567","repo":{"state":"ok"},"jobs":[]}`
	srv, _ := statusServer(t, dto)
	old := clientVersion
	defer func() { clientVersion = old }()

	clientVersion = func() string { return "0.2.1" }
	_, out, _ := runCLI(t, "status", "--color", "never", "--url", srv.URL)
	if !strings.Contains(out, "daemon: prebake 0.3.0 (abcdef1)") || !strings.Contains(out, "warning: this CLI (0.2.1) is older than the daemon (0.3.0)") {
		t.Errorf("status:\n%s", out)
	}
	_, _, e := runCLI(t, "dry-run", "--url", srv.URL)
	if !strings.Contains(e, "warning: this CLI (0.2.1) is older") {
		t.Errorf("dry-run warns on stderr: %q", e)
	}
	// matching versions: the line, no warning
	clientVersion = func() string { return "0.3.2" }
	_, out, e = runCLI(t, "status", "--color", "never", "--url", srv.URL)
	if !strings.Contains(out, "daemon: prebake 0.3.0") || strings.Contains(out, "warning") || strings.Contains(e, "warning") {
		t.Errorf("no skew: %q %q", out, e)
	}
	// an old daemon without version: nothing shown, nothing warned
	old2, _ := statusServer(t, `{"generated_at":"2026-10-22T02:10:00Z","repo":{"state":"ok"},"jobs":[]}`)
	_, out, _ = runCLI(t, "status", "--color", "never", "--url", old2.URL)
	if strings.Contains(out, "daemon:") || strings.Contains(out, "warning") {
		t.Errorf("old daemon: %q", out)
	}
	// the warning is yellow
	clientVersion = func() string { return "0.1.0" }
	if _, colored, _ := runCLI(t, "status", "--color", "always", "--url", srv.URL); !strings.Contains(colored, ansiYellow+"warning:") {
		t.Error("colored warning")
	}
}
