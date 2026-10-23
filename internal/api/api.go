// Package api serves the status page and the small control API of the daemon
// (docs/design.md §13). It is read-only apart from queueing builds and GC, and
// has no authentication: it is meant for an internal network.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"github.com/schorsch3000/prebake/internal/daemon"
	"github.com/schorsch3000/prebake/internal/schedule"
)

// Backend is what the API needs from the daemon.
type Backend interface {
	Status() daemon.Status
	Trigger(container, flavor string, kind schedule.Kind) (alsoQueued []string, err error)
	RequestGC()
}

// KeyInfo is the SSH deploy key shown at /pubkey.
type KeyInfo struct {
	PublicLine  string
	Fingerprint string
}

// StatusDTO is the JSON document of GET /status.
type StatusDTO struct {
	GeneratedAt time.Time `json:"generated_at"`
	// TimeZone is the zone the build slots are interpreted in, e.g. "CET +01:00".
	TimeZone string   `json:"time_zone"`
	Repo     RepoDTO  `json:"repo"`
	Jobs     []JobDTO `json:"jobs"`
}

type RepoDTO struct {
	State       string     `json:"state"`
	Head        string     `json:"head,omitempty"`
	Active      string     `json:"active,omitempty"`
	Error       string     `json:"error,omitempty"`
	LastFetch   *time.Time `json:"last_fetch,omitempty"`
	LastChanged *time.Time `json:"last_changed,omitempty"`
}

type JobDTO struct {
	Container    string     `json:"container"`
	Flavor       string     `json:"flavor"`
	State        string     `json:"state"`
	Error        string     `json:"error,omitempty"`
	LastRun      string     `json:"last_run,omitempty"`
	RunningSince *time.Time `json:"running_since,omitempty"`
	LastRebuild  *time.Time `json:"last_rebuild,omitempty"`
	LastRefresh  *time.Time `json:"last_refresh,omitempty"`
	DateTag      string     `json:"date_tag,omitempty"`
	Digest       string     `json:"digest,omitempty"`
	NextKind     string     `json:"next_kind,omitempty"`
	NextRun      *time.Time `json:"next_run,omitempty"`
}

func tp(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// ToDTO converts the daemon snapshot.
func ToDTO(s daemon.Status, now time.Time) StatusDTO {
	out := StatusDTO{GeneratedAt: now.UTC(), TimeZone: now.Format("MST -07:00"), Jobs: []JobDTO{}, Repo: RepoDTO{
		State: string(s.Repo.State), Head: s.Repo.Head, Active: s.Repo.Active, Error: s.Repo.Error,
		LastFetch: tp(s.Repo.LastFetch), LastChanged: tp(s.Repo.LastChanged),
	}}
	for _, j := range s.Jobs {
		d := JobDTO{
			Container: j.Key.Container, Flavor: j.Key.Flavor, State: j.State, Error: j.Error, LastRun: j.LastRun,
			RunningSince: tp(j.RunningSince),
			LastRebuild:  tp(j.Image.LastRebuild), LastRefresh: tp(j.Image.LastRefresh),
			DateTag: j.Image.DateTag, Digest: j.Image.Digest, NextRun: tp(j.NextRun),
		}
		if j.NextKind != schedule.None && !j.NextRun.IsZero() {
			d.NextKind = j.NextKind.String()
		}
		out.Jobs = append(out.Jobs, d)
	}
	return out
}

// Handler returns the HTTP handler.
func Handler(b Backend, key KeyInfo, now func() time.Time) http.Handler {
	if now == nil {
		now = time.Now
	}
	s := &server{b: b, key: key, now: now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /pubkey", s.pubkey)
	mux.HandleFunc("POST /trigger/{container}", s.trigger)
	mux.HandleFunc("POST /trigger/{container}/{flavor}", s.trigger)
	mux.HandleFunc("POST /gc", s.gc)
	return sameOrigin(mux)
}

// sameOrigin rejects state-changing requests that a browser sends from a page
// on another origin (cross-site form posts). Requests without an Origin header,
// such as curl and the CLI, are unaffected.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

type server struct {
	b   Backend
	key KeyInfo
	now func() time.Time
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ToDTO(s.b.Status(), s.now()))
}

func (s *server) pubkey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, s.key.PublicLine)
}

func (s *server) trigger(w http.ResponseWriter, r *http.Request) {
	container, flavor := r.PathValue("container"), r.PathValue("flavor")
	kind := schedule.Rebuild
	switch k := r.URL.Query().Get("kind"); k {
	case "", "rebuild":
	case "refresh":
		kind = schedule.Refresh
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("kind %q: want rebuild or refresh", k)})
		return
	}
	also, err := s.b.Trigger(container, flavor, kind)
	if err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, daemon.ErrNotFound):
			code = http.StatusNotFound
		case errors.Is(err, daemon.ErrInvalid):
			code = http.StatusBadRequest
		case errors.Is(err, daemon.ErrNotReady):
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	resp := map[string]any{"queued": kind.String(), "container": container, "flavor": flavorOrDefault(flavor)}
	if len(also) > 0 {
		// parents that were never built: the child needs <parent>:latest, so they go first
		resp["also_queued"] = also
	}
	writeJSON(w, http.StatusAccepted, resp)
}

func flavorOrDefault(f string) string {
	if f == "" {
		return "default"
	}
	return f
}

func (s *server) gc(w http.ResponseWriter, r *http.Request) {
	s.b.RequestGC()
	writeJSON(w, http.StatusAccepted, map[string]string{"queued": "gc"})
}

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"ts": func(t *time.Time) string {
		if t == nil {
			return "never"
		}
		return t.Format("2006-01-02 15:04 MST")
	},
	"short": func(s string) string {
		if len(s) > 12 {
			return s[:12]
		}
		return s
	},
}).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta http-equiv="refresh" content="30"><title>prebake</title>
<style>
body{font:14px/1.4 system-ui,sans-serif;margin:2em;color:#222}
table{border-collapse:collapse}th,td{padding:.3em .8em;border-bottom:1px solid #ddd;text-align:left;vertical-align:top}
.ok,.idle{color:#1a7f37}.queued,.building,.retrying{color:#9a6700}.parked,.skipped,.degraded{color:#cf222e}
pre{margin:0;white-space:pre-wrap}
</style></head><body>
<h1>prebake</h1>
<p>repo: <b class="{{.Repo.State}}">{{.Repo.State}}</b>
{{if .Repo.Active}} · running <code>{{short .Repo.Active}}</code>{{end}}
{{if and .Repo.Head (ne .Repo.Head .Repo.Active)}} · newest <code>{{short .Repo.Head}}</code>{{end}}
· last fetch {{ts .Repo.LastFetch}}</p>
{{if .Repo.Error}}<pre>{{.Repo.Error}}</pre>{{end}}
<table>
<tr><th>container</th><th>flavor</th><th>state</th><th>last rebuild</th><th>last refresh</th><th>next</th><th>image</th></tr>
{{range .Jobs}}<tr>
<td>{{.Container}}</td><td>{{.Flavor}}</td>
<td><b class="{{.State}}">{{.State}}</b>{{if .Error}}<pre>{{.Error}}</pre>{{end}}</td>
<td>{{ts .LastRebuild}}</td><td>{{ts .LastRefresh}}</td>
<td>{{if .NextKind}}{{.NextKind}} {{ts .NextRun}}{{else}}-{{end}}</td>
<td>{{.DateTag}} <code>{{short .Digest}}</code></td>
</tr>{{else}}<tr><td colspan="7">no containers loaded yet</td></tr>{{end}}
</table>
<p><small>generated {{ts .GeneratedAt}} · slots in {{.TimeZone}} · <a href="/status">json</a> · <a href="/pubkey">deploy key</a></small></p>
</body></html>`))

func (s *server) page(w http.ResponseWriter, r *http.Request) {
	dto := ToDTO(s.b.Status(), s.now())
	g := dto.GeneratedAt
	data := struct {
		StatusDTO
		GeneratedAt *time.Time
	}{dto, &g}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
