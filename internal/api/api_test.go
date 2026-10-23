package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/schorsch3000/prebake/internal/daemon"
	"github.com/schorsch3000/prebake/internal/schedule"
	"github.com/schorsch3000/prebake/internal/state"
)

type fakeBackend struct {
	status    daemon.Status
	triggered []string
	err       error
	also      []string
	gcs       int
}

func (f *fakeBackend) Status() daemon.Status { return f.status }
func (f *fakeBackend) Trigger(c, fl string, k schedule.Kind) ([]string, error) {
	f.triggered = append(f.triggered, fmt.Sprintf("%s/%s/%s", c, fl, k))
	return f.also, f.err
}
func (f *fakeBackend) RequestGC() { f.gcs++ }

var t0 = time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)

func fixture() *fakeBackend {
	return &fakeBackend{status: daemon.Status{
		Repo: daemon.RepoStatus{State: daemon.RepoDegraded, Head: "bbbbbbbbbbbbbbbb", Active: "aaaaaaaaaaaaaaaa",
			Error: "containers/dev/junk.txt: stray file <script>", LastFetch: t0},
		Jobs: []daemon.JobStatus{
			{Key: schedule.Key{Container: "base", Flavor: "default"}, State: "idle",
				Image:    state.Image{State: schedule.State{LastRebuild: t0, LastRefresh: t0.Add(time.Hour)}, DateTag: "default-20261022-020000", Digest: "sha256:abcdef0123456789abcdef"},
				NextKind: schedule.Refresh, NextRun: t0.Add(24 * time.Hour)},
			{Key: schedule.Key{Container: "child", Flavor: "cuda"}, State: "parked", Error: "apt <exploded>"},
		},
	}}
}

func serve(t *testing.T, b Backend) *httptest.Server {
	srv := httptest.NewServer(Handler(b, KeyInfo{PublicLine: "ssh-ed25519 AAAA prebake"}, func() time.Time { return t0.Add(time.Minute) }))
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
	for _, want := range []string{"slots in UTC", "degraded", "aaaaaaaaaaaa", "base", "child", "cuda", "parked", "default-20261022-020000", "sha256:abcde"} {
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
