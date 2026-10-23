package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	ok := map[string]time.Duration{
		"15m":    15 * time.Minute,
		"7d":     7 * 24 * time.Hour,
		"1w":     7 * 24 * time.Hour,
		"1d12h":  36 * time.Hour,
		"2w1d":   15 * 24 * time.Hour,
		"90s":    90 * time.Second,
		" 30m  ": 30 * time.Minute,
	}
	for in, want := range ok {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "7", "-1h", "0s", "0d", "abc", "1x"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) should fail", in)
		}
	}
}

func TestParseSlot(t *testing.T) {
	s, err := ParseSlot("02:00-06:00")
	if err != nil || s.Start != 2*time.Hour || s.End != 6*time.Hour || s.Length() != 4*time.Hour {
		t.Fatalf("got %+v, %v", s, err)
	}
	s, err = ParseSlot("22:00-02:00")
	if err != nil || s.Length() != 4*time.Hour {
		t.Fatalf("midnight crossing: got %+v len %v, %v", s, s.Length(), err)
	}
	if s.String() != "22:00-02:00" {
		t.Errorf("String = %q", s.String())
	}
	for _, in := range []string{"", "02:00", "2:00-25:00", "02:00-02:00", "a-b"} {
		if _, err := ParseSlot(in); err == nil {
			t.Errorf("ParseSlot(%q) should fail", in)
		}
	}
}

func TestGlobalDefaultsAndOverrides(t *testing.T) {
	g, err := ParseGlobal("prebake.yaml", []byte(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Slots) != 1 || g.Keep != 5 || g.RetryDelay.Std() != 15*time.Minute || !*g.Registry.GCSlotOnly {
		t.Errorf("unexpected defaults: %+v", g)
	}
	g, err = ParseGlobal("prebake.yaml", []byte(`
slots: ["01:00-03:00", "22:00-23:30"]
defaults: {rebuild_interval: 14d, refresh_interval: 12h}
retry_delay: 30m
keep: 3
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Slots) != 2 || g.Defaults.RebuildInterval.Std() != 14*24*time.Hour || g.Keep != 3 {
		t.Errorf("unexpected: %+v", g)
	}
}

func TestStrictErrorsHaveLines(t *testing.T) {
	_, err := ParseContainer("containers/dev/container.yaml", []byte("refresh_cmd: x\nrefrsh_interval: 1d\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "refrsh_interval") {
		t.Fatalf("want unknown-key error with line 2, got %v", err)
	}
	_, err = ParseContainer("c.yaml", []byte("refresh_cmd: x\nrebuild_interval: nope\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want duration error with line 2, got %v", err)
	}
	_, err = ParseGlobal("g.yaml", []byte("slots:\n  - \"02:00-06:00\"\n  - \"bad\"\n"))
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("want slot error with line 3, got %v", err)
	}
	if _, err = ParseGlobal("g.yaml", []byte("keep: -1\n")); err == nil {
		t.Error("negative keep must fail")
	}
}

func TestContainerFlavorsAndEffective(t *testing.T) {
	c, err := ParseContainer("c.yaml", []byte(`
refresh_cmd: "apt-get update && apt-get upgrade -y"
keep: 2
depends_on: [base]
flavors:
  cuda: {args: {CUDA: "1"}}
  default: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.FlavorNames(), ","); got != "cuda,default" {
		t.Errorf("flavors = %s", got)
	}
	g, _ := ParseGlobal("g.yaml", nil)
	e := c.Effective(g)
	if e.Keep != 2 || e.RebuildInterval != g.Defaults.RebuildInterval {
		t.Errorf("effective = %+v", e)
	}
	none, _ := ParseContainer("c.yaml", nil)
	if got := none.FlavorNames(); len(got) != 1 || got[0] != DefaultFlavor {
		t.Errorf("no flavors => %v", got)
	}
	if _, err := ParseContainer("c.yaml", []byte("flavors:\n  Bad_Name: {}\n")); err == nil {
		t.Error("invalid flavor name must fail")
	}
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"dev", "dev-go", "a.b_c", "go1"} {
		if !ValidName(n) {
			t.Errorf("%q should be valid", n)
		}
	}
	for _, n := range []string{"", "Dev", "-x", "x-", "a b", "a//b", "a..b"} {
		if ValidName(n) {
			t.Errorf("%q should be invalid", n)
		}
	}
}

func TestPushoverPriorityDefaultsAndExplicitZero(t *testing.T) {
	g, err := ParseGlobal("g.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	if *g.Notify.Pushover.PriorityFailure != 1 || *g.Notify.Pushover.PriorityRecovery != -1 {
		t.Errorf("defaults: %d %d", *g.Notify.Pushover.PriorityFailure, *g.Notify.Pushover.PriorityRecovery)
	}
	// an explicit 0 must not be mistaken for "unset"
	g, err = ParseGlobal("g.yaml", []byte("notify:\n  pushover:\n    priority_failure: 0\n    priority_recovery: -2\n"))
	if err != nil || *g.Notify.Pushover.PriorityFailure != 0 || *g.Notify.Pushover.PriorityRecovery != -2 {
		t.Errorf("explicit: %+v %v", g.Notify, err)
	}
}

func TestInputsHash(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
	}
	write("Dockerfile", "FROM debian\n", 0o644)
	write("scripts/setup.sh", "#!/bin/sh\n", 0o644)
	write(ContainerFile, "tests: [\"true\"]\n", 0o644)
	c := &Container{Flavors: map[string]Flavor{"default": {}, "cuda": {Args: map[string]string{"CUDA": "1"}}}}
	hash := func(c *Container, flavor string) string {
		t.Helper()
		h, err := InputsHash(dir, c, flavor)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := hash(c, "default")
	if base != hash(c, "default") {
		t.Fatal("hash must be stable")
	}
	if hash(c, "cuda") == base {
		t.Error("flavors differ in name and args")
	}
	// scheduler/test settings and container.yaml do not make an image stale
	write(ContainerFile, "tests: [\"false\"]\nkeep: 2\nrefresh_cmd: x\n", 0o644)
	c2 := *c
	c2.Keep, c2.RefreshCmd, c2.Tests = 3, "other", []string{"x"}
	c2.RebuildInterval, c2.RefreshInterval = Duration(time.Hour), Duration(time.Hour)
	if got := hash(&c2, "default"); got != base {
		t.Error("container.yaml and scheduler settings must not change the hash")
	}
	// what ends up in the image does
	changes := map[string]func(){
		"Dockerfile":     func() { write("Dockerfile", "FROM debian\nRUN true\n", 0o644) },
		"copied file":    func() { write("scripts/setup.sh", "#!/bin/sh\necho hi\n", 0o644) },
		"executable bit": func() { write("scripts/setup.sh", "#!/bin/sh\n", 0o755) },
		"new file":       func() { write("extra.txt", "x", 0o644) },
		"dockerignore":   func() { write(".dockerignore", "x", 0o644) },
	}
	for name, change := range changes {
		before := hash(c, "default")
		change()
		if hash(c, "default") == before {
			t.Errorf("%s must change the hash", name)
		}
	}
	// depends_on and build args
	c3 := *c
	c3.DependsOn = []string{"base"}
	if hash(&c3, "default") == hash(c, "default") {
		t.Error("depends_on must change the hash")
	}
	c4 := &Container{Flavors: map[string]Flavor{"default": {Args: map[string]string{"A": "1"}}}}
	c5 := &Container{Flavors: map[string]Flavor{"default": {Args: map[string]string{"A": "2"}}}}
	if hash(c4, "default") == hash(c5, "default") {
		t.Error("build args must change the hash")
	}
}

func TestLoadRepoComputesInputsHashes(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel, content string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	mk("prebake.yaml", "keep: 3\n")
	mk("containers/a/Dockerfile", "FROM debian\n")
	mk("containers/a/container.yaml", "flavors:\n  default: {}\n  gpu: {}\n")
	r, err := LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, b := r.InputsHash("a", "default"), r.InputsHash("a", "gpu")
	if a == "" || b == "" || a == b || r.InputsHash("a", "nope") != "" {
		t.Errorf("hashes: %q %q", a, b)
	}
}

func TestPruneRemovedDefaultsAndOverrides(t *testing.T) {
	g, err := ParseGlobal("g.yaml", nil)
	if err != nil || !*g.Registry.PruneRemoved || g.Registry.PruneRemovedAfter.Std() != 7*24*time.Hour {
		t.Fatalf("defaults: %+v %v", g.Registry, err)
	}
	g, err = ParseGlobal("g.yaml", []byte("registry:\n  prune_removed: false\n  prune_removed_after: 12h\n"))
	if err != nil || *g.Registry.PruneRemoved || g.Registry.PruneRemovedAfter.Std() != 12*time.Hour {
		t.Errorf("overrides: %+v %v", g.Registry, err)
	}
}

func TestParentArgName(t *testing.T) {
	for in, want := range map[string]string{
		"base": "PARENT_BASE", "dev-go": "PARENT_DEV_GO", "tools.v2": "PARENT_TOOLS_V2", "a--b": "PARENT_A_B", "go1": "PARENT_GO1",
	} {
		if got := ParentArgName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
