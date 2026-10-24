package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schorsch3000/prebake/internal/daemon"
	"github.com/schorsch3000/prebake/internal/schedule"
	"github.com/schorsch3000/prebake/internal/state"
)

type fakeBackend struct {
	status     daemon.Status
	triggered  []string
	err        error
	also       []string
	gcs        int
	syncCalls  []string
	syncRes    daemon.SyncResult
	syncErr    error
	purgeCalls []string
	purgeKeys  []schedule.Key
	purgeErr   error
	logCalls   []string
	logErr     error
	logStream  daemon.LogStream
}

func (f *fakeBackend) Status() daemon.Status { return f.status }
func (f *fakeBackend) Trigger(c, fl string, k schedule.Kind) ([]string, error) {
	f.triggered = append(f.triggered, fmt.Sprintf("%s/%s/%s", c, fl, k))
	return f.also, f.err
}
func (f *fakeBackend) RequestGC() { f.gcs++ }

func (f *fakeBackend) Purge(_ context.Context, c, fl string, dry bool) ([]schedule.Key, error) {
	f.purgeCalls = append(f.purgeCalls, fmt.Sprintf("%s/%s/dry=%v", c, fl, dry))
	return f.purgeKeys, f.purgeErr
}

func (f *fakeBackend) SyncNow(_ context.Context, commit, branch string) (daemon.SyncResult, error) {
	f.syncCalls = append(f.syncCalls, commit+"@"+branch)
	return f.syncRes, f.syncErr
}

// fakeStream plays a LogStream: it writes its lines, flushing each, and (with follow) waits for
// release before ending, like a build that is still running.
type fakeStream struct {
	lines   []string
	release chan struct{}
	gotArgs *string
}

func (f fakeStream) Stream(ctx context.Context, w io.Writer, follow bool, tail int, flush func()) error {
	if f.gotArgs != nil {
		*f.gotArgs = fmt.Sprintf("follow=%v tail=%d", follow, tail)
	}
	for _, l := range f.lines {
		io.WriteString(w, l+"\n")
		flush()
	}
	if follow && f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
		}
	}
	return nil
}

func (f *fakeBackend) Logs(c, fl string, follow bool) (daemon.LogStream, error) {
	f.logCalls = append(f.logCalls, fmt.Sprintf("%s/%s/follow=%v", c, fl, follow))
	if f.logErr != nil {
		return nil, f.logErr
	}
	return f.logStream, nil
}

var t0 = time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)

func fixture() *fakeBackend {
	return &fakeBackend{status: daemon.Status{
		Repo: daemon.RepoStatus{State: daemon.RepoDegraded, Head: "bbbbbbbbbbbbbbbb", Active: "aaaaaaaaaaaaaaaa",
			Error: "containers/dev/junk.txt: stray file <script>", LastFetch: t0},
		Jobs: []daemon.JobStatus{
			{Key: schedule.Key{Container: "base", Flavor: "default"}, State: "idle",
				Image:    state.Image{State: schedule.State{LastRebuild: t0, LastRefresh: t0.Add(time.Hour)}, DateTag: "default-20261022-020000", Digest: "sha256:abcdef0123456789abcdef"},
				NextKind: schedule.Refresh, NextRun: t0.Add(24 * time.Hour)},
			{Key: schedule.Key{Container: "child", Flavor: "cuda"}, State: "parked", Error: "apt <exploded>", Needs: "never built"},
		},
		Orphans: []daemon.Orphan{{Key: schedule.Key{Container: "old", Flavor: "default"}, Since: t0.Add(-48 * time.Hour), RemoveAt: t0.Add(120 * time.Hour), Tags: []string{"latest", "default-20261001-020000"}}},
	}}
}

func serve(t *testing.T, b Backend) *httptest.Server {
	srv := httptest.NewServer(Handler(b, Info{PublicLine: "ssh-ed25519 AAAA prebake", Version: "0.2.1", Revision: "abcdef1234567890"}, func() time.Time { return t0.Add(time.Minute) }))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func post(t *testing.T, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestStatusJSON(t *testing.T) {
	srv := serve(t, fixture())
	code, body, h := get(t, srv.URL+"/status")
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "application/json") {
		t.Fatalf("%d %v", code, h)
	}
	var dto StatusDTO
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.Version != "0.2.1" || dto.Revision != "abcdef1234567890" {
		t.Errorf("the daemon's version must be in /status: %q %q", dto.Version, dto.Revision)
	}
	if want := t0.Add(time.Minute).Format("MST -07:00"); dto.TimeZone != want {
		t.Errorf("time_zone = %q, want %q", dto.TimeZone, want)
	}
	if dto.Repo.State != "degraded" || dto.Repo.Active != "aaaaaaaaaaaaaaaa" || len(dto.Jobs) != 2 {
		t.Fatalf("%+v", dto)
	}
	j := dto.Jobs[0]
	if j.Container != "base" || j.NextKind != "refresh" || j.NextRun == nil || !j.NextRun.Equal(t0.Add(24*time.Hour)) ||
		j.LastRebuild == nil || j.DateTag != "default-20261022-020000" {
		t.Errorf("%+v", j)
	}
	if dto.Jobs[0].Needs != "" || dto.Jobs[1].Needs != "never built" {
		t.Errorf("needs: %q %q", dto.Jobs[0].Needs, dto.Jobs[1].Needs)
	}
	p := dto.Jobs[1]
	if p.State != "parked" || p.Error != "apt <exploded>" || p.LastRebuild != nil || p.NextKind != "" {
		t.Errorf("never-built job must have null times and no next: %+v", p)
	}
	if !strings.Contains(body, `"last_rebuild"`) || strings.Count(body, `"last_rebuild"`) != 1 {
		t.Errorf("zero times must be omitted:\n%s", body)
	}
}

func TestPageEscapesAndShowsState(t *testing.T) {
	srv := serve(t, fixture())
	code, body, h := get(t, srv.URL+"/")
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/html") {
		t.Fatalf("%d %v", code, h)
	}
	for _, want := range []string{"never built", "prebake 0.2.1 (abcdef123456)", "slots in UTC", "degraded", "aaaaaaaaaaaa", "base", "child", "cuda", "parked", "default-20261022-020000", "sha256:abcde"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// error text from builds and git is untrusted: it must be escaped
	if strings.Contains(body, "<script>") || strings.Contains(body, "<exploded>") {
		t.Errorf("unescaped content in page:\n%s", body)
	}
	if !strings.Contains(body, "&lt;exploded&gt;") {
		t.Errorf("expected escaped error text")
	}
	// only the root path serves the page
	if code, _, _ := get(t, srv.URL+"/nope"); code != 404 {
		t.Errorf("/nope: %d", code)
	}
}

func TestEmptyStatusPage(t *testing.T) {
	srv := serve(t, &fakeBackend{})
	code, body, _ := get(t, srv.URL+"/")
	if code != 200 || !strings.Contains(body, "no containers loaded yet") {
		t.Errorf("%d %s", code, body)
	}
	_, js, _ := get(t, srv.URL+"/status")
	if !strings.Contains(js, `"jobs": []`) {
		t.Errorf("jobs must be an empty array, not null:\n%s", js)
	}
}

func TestPubkey(t *testing.T) {
	srv := serve(t, fixture())
	code, body, h := get(t, srv.URL+"/pubkey")
	if code != 200 || strings.TrimSpace(body) != "ssh-ed25519 AAAA prebake" || !strings.HasPrefix(h.Get("Content-Type"), "text/plain") {
		t.Errorf("%d %q %v", code, body, h)
	}
}

func TestTrigger(t *testing.T) {
	b := fixture()
	srv := serve(t, b)
	if code, body := post(t, srv.URL+"/trigger/base", nil); code != 202 || !strings.Contains(body, `"queued": "rebuild"`) {
		t.Errorf("default: %d %s", code, body)
	}
	if code, _ := post(t, srv.URL+"/trigger/child/cuda?kind=refresh", nil); code != 202 {
		t.Errorf("flavor refresh: %d", code)
	}
	b.also = []string{"base"}
	if code, body := post(t, srv.URL+"/trigger/child", nil); code != 202 || !strings.Contains(body, `"also_queued"`) || !strings.Contains(body, `"base"`) {
		t.Errorf("parents queued along must be reported: %d %s", code, body)
	}
	b.also = nil
	b.triggered = b.triggered[:2]
	want := []string{"base//rebuild", "child/cuda/refresh"}
	if strings.Join(b.triggered, ",") != strings.Join(want, ",") {
		t.Errorf("triggered = %v", b.triggered)
	}
	if code, _ := post(t, srv.URL+"/trigger/base?kind=nuke", nil); code != 400 {
		t.Errorf("bad kind: %d", code)
	}
	if len(b.triggered) != 2 {
		t.Errorf("bad kind must not reach the daemon")
	}
	for err, want := range map[error]int{
		fmt.Errorf("%w: x", daemon.ErrNotFound): 404, fmt.Errorf("%w: x", daemon.ErrInvalid): 400,
		daemon.ErrNotReady: 503, errors.New("boom"): 500,
	} {
		b.err = err
		if code, _ := post(t, srv.URL+"/trigger/base", nil); code != want {
			t.Errorf("%v: got %d want %d", err, code, want)
		}
	}
	// trigger is POST only
	if code, _, _ := get(t, srv.URL+"/trigger/base"); code != 405 {
		t.Errorf("GET /trigger: %d", code)
	}
}

func TestGC(t *testing.T) {
	b := fixture()
	srv := serve(t, b)
	if code, _ := post(t, srv.URL+"/gc", nil); code != 202 || b.gcs != 1 {
		t.Errorf("%d %d", code, b.gcs)
	}
	if code, _, _ := get(t, srv.URL+"/gc"); code != 405 {
		t.Errorf("GET /gc: %d", code)
	}
}

func TestCrossOriginPostRefused(t *testing.T) {
	b := fixture()
	srv := serve(t, b)
	code, _ := post(t, srv.URL+"/trigger/base", map[string]string{"Origin": "http://evil.example"})
	if code != 403 || len(b.triggered) != 0 {
		t.Errorf("cross-origin: %d %v", code, b.triggered)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	if code, _ := post(t, srv.URL+"/trigger/base", map[string]string{"Origin": "http://" + host}); code != 202 {
		t.Errorf("same origin: %d", code)
	}
	if code, _ := post(t, srv.URL+"/gc", map[string]string{"Origin": "null"}); code != 403 {
		t.Errorf("opaque origin on /gc: %d", code)
	}
}

func TestStatusJSONCarriesRunningSince(t *testing.T) {
	b := fixture()
	b.status.Jobs[0].State = "building"
	b.status.Jobs[0].RunningSince = t0.Add(-3 * time.Minute)
	srv := serve(t, b)
	_, body, _ := get(t, srv.URL+"/status")
	var dto StatusDTO
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.Jobs[0].RunningSince == nil || !dto.Jobs[0].RunningSince.Equal(t0.Add(-3*time.Minute)) || dto.Jobs[1].RunningSince != nil {
		t.Errorf("running_since: %+v / %+v", dto.Jobs[0].RunningSince, dto.Jobs[1].RunningSince)
	}
	if strings.Count(body, "running_since") != 1 {
		t.Errorf("only a running job carries it:\n%s", body)
	}
}

func TestLogsEndpointStreamsWhileTheBuildRuns(t *testing.T) {
	var args string
	release := make(chan struct{})
	b := fixture()
	b.logStream = fakeStream{lines: []string{"#1 first", "#2 second"}, release: release, gotArgs: &args}
	srv := serve(t, b)

	resp, err := http.Get(srv.URL + "/logs/base/default?follow=1&tail=20")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	// the first lines arrive while the stream is still open: it is flushed, not buffered until the end
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil || !strings.HasPrefix(string(buf[:n]), "#1 first") {
		t.Fatalf("first chunk: %q %v", buf[:n], err)
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(buf[:n])+string(rest), "#2 second") {
		t.Errorf("rest: %q", rest)
	}
	if args != "follow=true tail=20" {
		t.Errorf("query not passed on: %q", args)
	}
	if got := strings.Join(b.logCalls, ","); got != "base/default/follow=true" {
		t.Errorf("calls = %s", got)
	}
}

func TestLogsEndpointVariantsAndErrors(t *testing.T) {
	b := fixture()
	b.logStream = fakeStream{lines: []string{"hello"}}
	srv := serve(t, b)
	if code, body, _ := get(t, srv.URL+"/logs/base"); code != 200 || body != "hello\n" {
		t.Errorf("plain: %d %q", code, body)
	}
	if got := strings.Join(b.logCalls, ","); got != "base//follow=false" {
		t.Errorf("a missing flavor is passed on empty: %s", got)
	}
	if code, _, _ := get(t, srv.URL+"/logs/base?tail=x"); code != 400 {
		t.Errorf("bad tail: %d", code)
	}
	if code, _, _ := get(t, srv.URL+"/logs/base?tail=-1"); code != 400 {
		t.Errorf("negative tail: %d", code)
	}
	for err, want := range map[error]int{
		fmt.Errorf("%w: x", daemon.ErrNotFound): 404, daemon.ErrNoLogs: 404, daemon.ErrNotReady: 503, errors.New("boom"): 500,
	} {
		b.logErr = err
		if code, body, _ := get(t, srv.URL+"/logs/base"); code != want || !strings.Contains(body, "error") {
			t.Errorf("%v: got %d %q want %d", err, code, body, want)
		}
	}
	// read-only endpoint
	if code, _ := post(t, srv.URL+"/logs/base", nil); code != 405 {
		t.Errorf("POST /logs: %d", code)
	}
}

func TestClientDisconnectEndsTheLogStream(t *testing.T) {
	ended := make(chan struct{})
	b := fixture()
	b.logStream = endNotifier{fakeStream{lines: []string{"x"}, release: make(chan struct{})}, ended}
	srv := serve(t, b)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/logs/base?follow=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	resp.Body.Read(buf)
	cancel() // Ctrl-C in the client
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the server must stop streaming when the client goes away")
	}
}

type endNotifier struct {
	fakeStream
	ended chan struct{}
}

func (e endNotifier) Stream(ctx context.Context, w io.Writer, follow bool, tail int, flush func()) error {
	err := e.fakeStream.Stream(ctx, w, follow, tail, flush)
	close(e.ended)
	return err
}

func TestSyncEndpoint(t *testing.T) {
	yes := true
	b := fixture()
	b.syncRes = daemon.SyncResult{Branch: "main", State: daemon.RepoOK, Head: "abc", Active: "abc", Changed: true, HasCommit: &yes}
	srv := serve(t, b)
	code, body := post(t, srv.URL+"/sync?commit=abc1234&branch=main", nil)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var got daemon.SyncResult
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.State != daemon.RepoOK || !got.Changed || got.HasCommit == nil || !*got.HasCommit || got.Branch != "main" {
		t.Errorf("%+v %v\n%s", got, err, body)
	}
	if code, _ := post(t, srv.URL+"/sync", nil); code != 200 {
		t.Errorf("plain sync: %d", code)
	}
	if strings.Join(b.syncCalls, ",") != "abc1234@main,@" {
		t.Errorf("query not passed on: %v", b.syncCalls)
	}
	b.syncErr = fmt.Errorf("%w: invalid commit", daemon.ErrInvalid)
	if code, _ := post(t, srv.URL+"/sync?commit=zzz", nil); code != 400 {
		t.Errorf("invalid: %d", code)
	}
	b.syncErr = errors.New("boom")
	if code, _ := post(t, srv.URL+"/sync", nil); code != 500 {
		t.Errorf("boom: %d", code)
	}
	if code, _, _ := get(t, srv.URL+"/sync"); code != 405 {
		t.Errorf("GET /sync: %d", code)
	}
	// a browser on another site must not be able to make the daemon fetch
	b.syncErr = nil
	if code, _ := post(t, srv.URL+"/sync", map[string]string{"Origin": "http://evil.example"}); code != 403 {
		t.Errorf("cross-origin: %d", code)
	}
}

func TestStatusListsOrphans(t *testing.T) {
	srv := serve(t, fixture())
	_, body, _ := get(t, srv.URL+"/status")
	var dto StatusDTO
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatal(err)
	}
	if len(dto.Orphans) != 1 {
		t.Fatalf("%+v", dto.Orphans)
	}
	o := dto.Orphans[0]
	if o.Container != "old" || o.Flavor != "default" || len(o.Tags) != 2 || o.Since == nil || o.RemoveAt == nil || !o.RemoveAt.Equal(t0.Add(120*time.Hour)) {
		t.Errorf("%+v", o)
	}
	// none: the field is absent, not null
	_, body, _ = get(t, serve(t, &fakeBackend{}).URL+"/status")
	if strings.Contains(body, "orphans") {
		t.Errorf("no orphans, no field:\n%s", body)
	}
}

func TestPurgeEndpoint(t *testing.T) {
	b := fixture()
	b.purgeKeys = []schedule.Key{{Container: "old", Flavor: "default"}}
	srv := serve(t, b)
	code, body := post(t, srv.URL+"/purge/old?dry_run=1", nil)
	if code != 200 || !strings.Contains(body, `"dry_run"`) || !strings.Contains(body, "old:default") {
		t.Errorf("dry run: %d %s", code, body)
	}
	code, body = post(t, srv.URL+"/purge/old/default", nil)
	if code != 202 || !strings.Contains(body, `"queued"`) || !strings.Contains(body, "old:default") {
		t.Errorf("purge: %d %s", code, body)
	}
	if strings.Join(b.purgeCalls, ",") != "old//dry=true,old/default/dry=false" {
		t.Errorf("calls = %v", b.purgeCalls)
	}
	for err, want := range map[error]int{
		fmt.Errorf("%w: x", daemon.ErrNotFound): 404, fmt.Errorf("%w: still in the config", daemon.ErrConflict): 409,
		daemon.ErrNotReady: 503, errors.New("boom"): 500,
	} {
		b.purgeErr = err
		if code, _ := post(t, srv.URL+"/purge/old", nil); code != want {
			t.Errorf("%v: got %d want %d", err, code, want)
		}
	}
	if code, _, _ := get(t, srv.URL+"/purge/old"); code != 405 {
		t.Errorf("GET /purge: %d", code)
	}
	if code, _ := post(t, srv.URL+"/purge/old", map[string]string{"Origin": "http://evil.example"}); code != 403 {
		t.Errorf("cross-origin purge must be refused: %d", code)
	}
}

func TestStatusReportsDiskUsage(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blobs", "a"), make([]byte, 1500), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(fixture(), Info{StorageDir: dir}, func() time.Time { return t0 }))
	t.Cleanup(srv.Close)
	_, body, _ := get(t, srv.URL+"/status")
	var dto StatusDTO
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.Disk == nil || dto.Disk.RegistryBytes != 1500 || dto.Disk.TotalBytes == 0 {
		t.Fatalf("disk = %+v", dto.Disk)
	}
	if _, page, _ := get(t, srv.URL+"/"); !strings.Contains(page, "registry 1.5 KiB") {
		t.Fatalf("page lacks disk usage:\n%s", page)
	}
	// no storage dir: nothing reported
	_, body, _ = get(t, serve(t, fixture()).URL+"/status")
	if strings.Contains(body, `"disk"`) {
		t.Fatal("disk reported without a storage dir")
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 3 << 30: "3.0 GiB", 5 << 40: "5.0 TiB"} {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
