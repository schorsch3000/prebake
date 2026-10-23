package config

import (
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
