package validate

import (
	"strings"
	"testing"
	"testing/fstest"
)

const goodGlobal = "keep: 5\nslots: [\"02:00-06:00\"]\n"

func repo(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for k, v := range files {
		m[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return m
}

// has reports whether an issue of the given severity contains substr.
func has(r *Result, sev Severity, substr string) bool {
	for _, i := range r.Issues {
		if i.Severity == sev && strings.Contains(i.String(), substr) {
			return true
		}
	}
	return false
}

func base() map[string]string {
	return map[string]string{
		"prebake.yaml":                  goodGlobal,
		"containers/dev/Dockerfile":     "FROM debian\nCOPY setup.sh /setup.sh\nRUN /setup.sh\n",
		"containers/dev/container.yaml": "refresh_cmd: apt-get update\ntests: [\"git --version\"]\n",
		"containers/dev/setup.sh":       "#!/bin/sh\n",
	}
}

func TestValidRepo(t *testing.T) {
	r := FS(repo(base()))
	if len(r.Issues) != 0 {
		t.Fatalf("want no issues, got %v", r.Issues)
	}
}

func TestMissingPieces(t *testing.T) {
	r := FS(repo(map[string]string{}))
	if !has(r, Error, "prebake.yaml: missing global config") || !has(r, Error, "missing containers/") {
		t.Fatalf("got %v", r.Issues)
	}

	m := base()
	delete(m, "containers/dev/Dockerfile")
	delete(m, "containers/dev/container.yaml")
	r = FS(repo(m))
	if !has(r, Error, "missing Dockerfile") || !has(r, Error, "missing container.yaml") {
		t.Fatalf("got %v", r.Issues)
	}
}

func TestStrayFiles(t *testing.T) {
	m := base()
	m["containers/dev/notes.txt"] = "x"
	m["containers/README.md"] = "x"
	m["containers/dev/.dockerignore"] = "x"
	r := FS(repo(m))
	if !has(r, Error, "containers/dev/notes.txt: stray file") {
		t.Errorf("stray file in container not reported: %v", r.Issues)
	}
	if !has(r, Error, "containers/README.md: stray file directly under") {
		t.Errorf("stray file under containers/ not reported: %v", r.Issues)
	}
	if has(r, Error, ".dockerignore") {
		t.Errorf(".dockerignore must be allowed")
	}

	m = base()
	m["containers/dev/notes.txt"] = "x"
	m["containers/dev/container.yaml"] += "extra_files: [\"*.txt\"]\n"
	if r := FS(repo(m)); len(r.Issues) != 0 {
		t.Errorf("extra_files should allow notes.txt: %v", r.Issues)
	}
}

func TestCopyChecks(t *testing.T) {
	m := base()
	m["containers/dev/Dockerfile"] = strings.Join([]string{
		"FROM debian AS b",
		"COPY missing.txt /x",
		"COPY ../outside /y",
		"COPY --from=b /a /b",
		"ADD https://example.com/f /f",
		"COPY --chown=1:1 conf/ /etc/conf/",
		"COPY [\"setup.sh\", \"/s\"]",
		"COPY $FILE /z",
	}, "\n")
	m["containers/dev/conf/a/b.conf"] = "x"
	r := FS(repo(m))
	if !has(r, Error, `Dockerfile:2: COPY source "missing.txt" does not exist`) {
		t.Errorf("missing source: %v", r.Issues)
	}
	if !has(r, Error, `Dockerfile:3: COPY source "../outside" escapes`) {
		t.Errorf("escape: %v", r.Issues)
	}
	for _, i := range r.Issues {
		if i.Line > 3 && i.Severity == Error {
			t.Errorf("unexpected: %v", i)
		}
	}
	if has(r, Error, "stray") {
		t.Errorf("files under a copied directory must not be stray: %v", r.Issues)
	}
}

func TestContinuationAndHeredoc(t *testing.T) {
	df := "FROM debian\nRUN apt-get update && \\\n    apt-get install -y \\\n    # comment\n    git\nCOPY <<EOF /etc/x\nCOPY nope /bad\nEOF\nCOPY setup.sh /s\n"
	ins := parseDockerfile(df)
	var cmds []string
	for _, i := range ins {
		cmds = append(cmds, i.Cmd)
	}
	if got := strings.Join(cmds, ","); got != "FROM,RUN,COPY,COPY" {
		t.Fatalf("instructions = %s", got)
	}
	if ins[1].Line != 2 || ins[3].Line != 9 {
		t.Errorf("lines = %d, %d", ins[1].Line, ins[3].Line)
	}
}

func TestKeepAndSlots(t *testing.T) {
	m := base()
	m["containers/dev/container.yaml"] = "keep: 9\ntests: [\"true\"]\n"
	r := FS(repo(m))
	if !has(r, Error, "container.yaml:1: keep 9 exceeds global keep 5") {
		t.Errorf("keep: %v", r.Issues)
	}

	m = base()
	m["prebake.yaml"] = "slots: [\"02:00-06:00\", \"05:00-07:00\"]\n"
	if r := FS(repo(m)); !has(r, Error, "overlap") {
		t.Errorf("overlap: %v", r.Issues)
	}
	m["prebake.yaml"] = "slots: [\"22:00-03:00\", \"02:00-06:00\"]\n"
	if r := FS(repo(m)); !has(r, Error, "overlap") {
		t.Errorf("midnight overlap: %v", r.Issues)
	}
	m["prebake.yaml"] = "slots: [\"22:00-02:00\", \"02:00-06:00\"]\n"
	if r := FS(repo(m)); has(r, Error, "overlap") {
		t.Errorf("adjacent slots must not overlap: %v", r.Issues)
	}
}

func TestUnknownKeyHasLine(t *testing.T) {
	m := base()
	m["containers/dev/container.yaml"] = "refresh_cmd: x\nrefrsh_interval: 1d\n"
	r := FS(repo(m))
	if !has(r, Error, "container.yaml:2:") || !has(r, Error, "refrsh_interval") {
		t.Errorf("got %v", r.Issues)
	}
}

func TestDependencies(t *testing.T) {
	m := base()
	m["containers/a/Dockerfile"] = "ARG PARENT\nFROM $PARENT\n"
	m["containers/a/container.yaml"] = "depends_on: [b]\ntests: [\"true\"]\n"
	m["containers/b/Dockerfile"] = "ARG PARENT\nFROM $PARENT\n"
	m["containers/b/container.yaml"] = "depends_on: [a]\ntests: [\"true\"]\n"
	m["containers/dev/container.yaml"] = "depends_on: [ghost, dev]\ntests: [\"true\"]\n"
	r := FS(repo(m))
	if !has(r, Error, "dependency cycle: ") {
		t.Errorf("cycle: %v", r.Issues)
	}
	if !has(r, Error, `"ghost": no such container`) || !has(r, Error, "refers to itself") {
		t.Errorf("targets: %v", r.Issues)
	}
	if !has(r, Warning, "no 'ARG PARENT'") {
		t.Errorf("dev has depends_on without ARG PARENT: %v", r.Issues)
	}
}

func TestWarnings(t *testing.T) {
	m := base()
	m["containers/dev/container.yaml"] = "refresh_cmd: x\n"
	r := FS(repo(m))
	if r.Errors() != 0 || !has(r, Warning, "no tests configured") {
		t.Errorf("got %v", r.Issues)
	}
}

func TestInvalidContainerName(t *testing.T) {
	m := base()
	m["containers/Bad_Name/Dockerfile"] = "FROM debian\n"
	m["containers/Bad_Name/container.yaml"] = "tests: [\"true\"]\n"
	if r := FS(repo(m)); !has(r, Error, "invalid container name") {
		t.Errorf("got %v", r.Issues)
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		p, n string
		want bool
	}{
		{"*.txt", "a.txt", true},
		{"*.txt", "d/a.txt", false},
		{"**/*.txt", "d/e/a.txt", true},
		{"scripts/**", "scripts/a/b.sh", true},
		{"scripts", "scripts", true},
		{"scripts", "scripts/a", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.p, c.n); got != c.want {
			t.Errorf("matchGlob(%q,%q) = %v", c.p, c.n, got)
		}
	}
}

func TestPushoverPriorityRange(t *testing.T) {
	m := base()
	m["prebake.yaml"] = goodGlobal + "notify:\n  pushover:\n    priority_failure: 2\n    priority_recovery: -3\n"
	r := FS(repo(m))
	if !has(r, Error, "priority_failure 2 out of range") || !has(r, Error, "priority_recovery -3 out of range") {
		t.Errorf("got %v", r.Issues)
	}
	m["prebake.yaml"] = goodGlobal + "notify:\n  pushover:\n    priority_failure: 0\n    priority_recovery: -2\n"
	if r := FS(repo(m)); len(r.Issues) != 0 {
		t.Errorf("valid priorities rejected: %v", r.Issues)
	}
}
