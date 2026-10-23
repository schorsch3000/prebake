package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPushoverRequest(t *testing.T) {
	var got map[string]string
	var method, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ctype = r.Method, r.Header.Get("Content-Type")
		r.ParseForm()
		got = map[string]string{}
		for k := range r.PostForm {
			got[k] = r.PostForm.Get(k)
		}
		w.Write([]byte(`{"status":1}`))
	}))
	defer srv.Close()
	p := &Pushover{Token: "tok", User: "usr", URL: srv.URL}
	err := p.Notify(context.Background(), Event{Type: JobParked, Title: "dev:default failed twice", Message: "line1\nline2", Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	if method != "POST" || !strings.HasPrefix(ctype, "application/x-www-form-urlencoded") {
		t.Errorf("%s %s", method, ctype)
	}
	want := map[string]string{"token": "tok", "user": "usr", "title": "prebake: dev:default failed twice", "message": "line1\nline2", "priority": "1"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestPushoverLimitsAndEmptyMessage(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = map[string]string{"title": r.PostForm.Get("title"), "message": r.PostForm.Get("message"), "priority": r.PostForm.Get("priority")}
	}))
	defer srv.Close()
	p := &Pushover{Token: "t", User: "u", URL: srv.URL}
	long := strings.Repeat("ä", 3000) // multi-byte: must be cut on rune boundaries
	if err := p.Notify(context.Background(), Event{Title: long, Message: long, Priority: -1}); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got["message"])); n != maxMessage || !strings.HasSuffix(got["message"], "…") {
		t.Errorf("message runes = %d", n)
	}
	if n := len([]rune(got["title"])); n != maxTitle {
		t.Errorf("title runes = %d", n)
	}
	if got["priority"] != "-1" {
		t.Errorf("priority = %q", got["priority"])
	}
	p.Notify(context.Background(), Event{Title: "only a title"})
	if got["message"] != "only a title" {
		t.Errorf("empty message must fall back to the title: %q", got["message"])
	}
}

func TestPushoverErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"user":"invalid","status":0}`))
	}))
	p := &Pushover{Token: "secret-token", User: "u", URL: srv.URL}
	err := p.Notify(context.Background(), Event{Title: "x"})
	srv.Close()
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Error("the token must never appear in errors")
	}
	// unreachable server
	if err := p.Notify(context.Background(), Event{Title: "x"}); err == nil {
		t.Error("closed server must fail")
	}
}

func TestFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if n, on, err := FromEnv(env(nil)); err != nil || on || n == nil {
		t.Errorf("unset: %v %v %v", n, on, err)
	}
	n, on, err := FromEnv(env(map[string]string{"PUSHOVER_TOKEN": " tok ", "PUSHOVER_USER": "usr"}))
	if err != nil || !on || n.(*Pushover).Token != "tok" || n.(*Pushover).User != "usr" {
		t.Errorf("env: %v %v %v", n, on, err)
	}
	if _, _, err := FromEnv(env(map[string]string{"PUSHOVER_TOKEN": "tok"})); err == nil {
		t.Error("token without user must fail")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "tok")
	os.WriteFile(f, []byte("filetoken\n"), 0o600)
	n, on, err = FromEnv(env(map[string]string{"PUSHOVER_TOKEN_FILE": f, "PUSHOVER_USER": "usr"}))
	if err != nil || !on || n.(*Pushover).Token != "filetoken" {
		t.Errorf("file secret: %v %v %v", n, on, err)
	}
	if _, _, err := FromEnv(env(map[string]string{"PUSHOVER_TOKEN_FILE": filepath.Join(dir, "missing"), "PUSHOVER_USER": "u"})); err == nil {
		t.Error("unreadable secret file must fail")
	}
}

func TestTypeFailure(t *testing.T) {
	for _, ty := range []Type{JobParked, TestFailed, RepoInvalid, PruneFailed} {
		if !ty.Failure() {
			t.Errorf("%s should be a failure", ty)
		}
	}
	if Recovered.Failure() {
		t.Error("recovered is not a failure")
	}
}
