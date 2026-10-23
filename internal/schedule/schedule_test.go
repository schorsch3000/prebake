package schedule

import (
	"testing"
	"time"

	"prebake/internal/config"
)

func slot(t *testing.T, s string) config.Slot {
	t.Helper()
	v, err := config.ParseSlot(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func slots(t *testing.T, ss ...string) []config.Slot {
	var out []config.Slot
	for _, s := range ss {
		out = append(out, slot(t, s))
	}
	return out
}

func ts(s string) time.Time {
	v, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return v
}

var day = 24 * time.Hour

func TestCurrentWindowAndNextStart(t *testing.T) {
	ss := slots(t, "02:00-06:00")
	if _, ok := CurrentWindow(ss, ts("2026-10-22 01:59")); ok {
		t.Error("01:59 is outside")
	}
	w, ok := CurrentWindow(ss, ts("2026-10-22 02:00"))
	if !ok || !w.End.Equal(ts("2026-10-22 06:00")) {
		t.Errorf("02:00 inside: %v %v", w, ok)
	}
	if _, ok := CurrentWindow(ss, ts("2026-10-22 06:00")); ok {
		t.Error("end is exclusive")
	}
	if got := NextStart(ss, ts("2026-10-22 02:00")); !got.Equal(ts("2026-10-23 02:00")) {
		t.Errorf("strictly after the current start: %v", got)
	}
	if got := NextStart(ss, ts("2026-10-22 01:00")); !got.Equal(ts("2026-10-22 02:00")) {
		t.Errorf("same day: %v", got)
	}
	if !NextStart(nil, ts("2026-10-22 01:00")).IsZero() {
		t.Error("no slots => zero")
	}
}

func TestMidnightCrossing(t *testing.T) {
	ss := slots(t, "22:00-02:00")
	for _, in := range []string{"2026-10-22 23:30", "2026-10-23 01:59"} {
		if _, ok := CurrentWindow(ss, ts(in)); !ok {
			t.Errorf("%s should be inside", in)
		}
	}
	if _, ok := CurrentWindow(ss, ts("2026-10-23 02:00")); ok {
		t.Error("02:00 is the exclusive end")
	}
	w, _ := CurrentWindow(ss, ts("2026-10-23 00:30"))
	if !w.Start.Equal(ts("2026-10-22 22:00")) || !w.End.Equal(ts("2026-10-23 02:00")) {
		t.Errorf("window = %v", w)
	}
}

func TestMultipleSlotsNextStart(t *testing.T) {
	ss := slots(t, "02:00-04:00", "14:00-16:00")
	if got := NextStart(ss, ts("2026-10-22 03:00")); !got.Equal(ts("2026-10-22 14:00")) {
		t.Errorf("got %v", got)
	}
	if got := NextStart(ss, ts("2026-10-22 15:00")); !got.Equal(ts("2026-10-23 02:00")) {
		t.Errorf("got %v", got)
	}
}

func TestDueMath(t *testing.T) {
	p := Policy{RebuildInterval: 7 * day, RefreshInterval: day}
	built := ts("2026-10-01 03:00")

	// never built: rebuild due immediately
	if k, _ := Pick(p, State{}, ts("2026-10-01 00:00")); k != Rebuild {
		t.Errorf("never built => %v", k)
	}
	// rebuild resets the refresh clock
	s := State{LastRebuild: built, LastRefresh: built.Add(-3 * day)}
	if got := RefreshDue(p, s); !got.Equal(built.Add(day)) {
		t.Errorf("refresh due = %v", got)
	}
	// refresh pushes only the refresh clock
	s = State{LastRebuild: built, LastRefresh: built.Add(day)}
	if got := RefreshDue(p, s); !got.Equal(built.Add(2 * day)) {
		t.Errorf("refresh due = %v", got)
	}
	if got := RebuildDue(p, s); !got.Equal(built.Add(7 * day)) {
		t.Errorf("rebuild due = %v", got)
	}
	// both due: rebuild wins
	if k, _ := Pick(p, s, built.Add(8*day)); k != Rebuild {
		t.Errorf("both due => %v", k)
	}
	if k, _ := Pick(p, s, built.Add(3*day)); k != Refresh {
		t.Errorf("only refresh due => %v", k)
	}
	if k, _ := Pick(p, s, built.Add(day)); k != None {
		t.Errorf("nothing due => %v", k)
	}
}

func TestDecideSlotRule(t *testing.T) {
	ss := slots(t, "02:00-06:00")
	p := Policy{RebuildInterval: 30 * day, RefreshInterval: day}
	// refresh due tomorrow 05:00: would be late for tomorrow's 02:00..05:00? no,
	// tomorrow's slot start (02:00) is before 05:00, so it must run today.
	s := State{LastRebuild: ts("2026-09-30 05:00"), LastRefresh: ts("2026-10-21 05:00")}
	d := Decide(ss, p, s, ts("2026-10-22 02:00"), 0)
	if !d.Run || d.Kind != Refresh {
		t.Errorf("due 05:00 today, in slot: %+v", d)
	}
	// refresh due 2026-10-24 03:00; today's slot (10-22) is not the last one before
	// due (10-23 02:00 is, and 10-24 02:00 is), so not today
	s = State{LastRebuild: ts("2026-09-30 05:00"), LastRefresh: ts("2026-10-23 03:00")}
	if d := Decide(ss, p, s, ts("2026-10-22 02:00"), 0); d.Run {
		t.Errorf("due in 10-24 03:00 must wait, got %+v", d)
	}
	// due 2026-10-23 01:00 (before tomorrow's slot starts): last chance is today
	s = State{LastRebuild: ts("2026-09-30 05:00"), LastRefresh: ts("2026-10-22 01:00")}
	if d := Decide(ss, p, s, ts("2026-10-22 03:00"), 0); !d.Run || d.Kind != Refresh {
		t.Errorf("due before next slot => run now: %+v", d)
	}
	// outside the slot nothing runs, even when overdue
	s = State{LastRebuild: ts("2026-01-01 00:00")}
	if d := Decide(ss, p, s, ts("2026-10-22 12:00"), 0); d.Run || d.Reason != "outside slot" {
		t.Errorf("outside slot: %+v", d)
	}
	// catch-up: host was off, overdue job runs at the next slot
	if d := Decide(ss, p, s, ts("2026-10-23 02:00"), 0); !d.Run || d.Kind != Rebuild {
		t.Errorf("catch-up: %+v", d)
	}
}

func TestDecideJitter(t *testing.T) {
	ss := slots(t, "02:00-06:00")
	p := Policy{RebuildInterval: day, RefreshInterval: day}
	s := State{}
	if d := Decide(ss, p, s, ts("2026-10-22 02:10"), 30*time.Minute); d.Run {
		t.Errorf("must wait for jitter: %+v", d)
	}
	if d := Decide(ss, p, s, ts("2026-10-22 02:30"), 30*time.Minute); !d.Run {
		t.Errorf("jitter elapsed: %+v", d)
	}
	// huge jitter is capped to half the window (2h)
	if d := Decide(ss, p, s, ts("2026-10-22 04:00"), 10*time.Hour); !d.Run {
		t.Errorf("jitter must be capped: %+v", d)
	}
}

func TestJitterStable(t *testing.T) {
	a, b := Jitter("dev:default", 30*time.Minute), Jitter("dev:default", 30*time.Minute)
	if a != b || a < 0 || a >= 30*time.Minute {
		t.Errorf("jitter = %v, %v", a, b)
	}
	if Jitter("x", 0) != 0 {
		t.Error("zero max => zero")
	}
	if Jitter("a", time.Hour) == Jitter("b", time.Hour) && Jitter("a", time.Hour) == Jitter("c", time.Hour) {
		t.Error("jitter should vary with key")
	}
}

func TestDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tzdata")
	}
	ss := slots(t, "02:00-06:00")
	// 2026-10-25: clocks go back at 03:00. 02:00 occurs twice and Go does not guarantee
	// which one time.Date picks, so only the wall-clock time of the slot start is asserted.
	now := time.Date(2026, 10, 25, 1, 0, 0, 0, loc)
	next := NextStart(ss, now)
	if next.Hour() != 2 || next.Day() != 25 {
		t.Errorf("next = %v", next)
	}
	after := NextStart(ss, next)
	if after.Hour() != 2 || after.Day() != 26 {
		t.Errorf("after = %v", after)
	}
	// spring forward 2026-03-29: 02:00 does not exist; Go normalises, slot still found
	now = time.Date(2026, 3, 29, 1, 0, 0, 0, loc)
	if next := NextStart(ss, now); next.IsZero() || next.Day() != 29 {
		t.Errorf("spring next = %v", next)
	}
}

func TestNextRun(t *testing.T) {
	ss := slots(t, "02:00-06:00")
	p := Policy{RebuildInterval: 30 * day, RefreshInterval: day}
	s := State{LastRebuild: ts("2026-10-01 05:00"), LastRefresh: ts("2026-10-21 05:00")}
	// refresh due 10-22 05:00, now 10-22 00:00: the slot starting 10-22 02:00 is the one
	k, at := NextRun(ss, p, s, ts("2026-10-22 00:00"))
	if k != Refresh || !at.Equal(ts("2026-10-22 02:00")) {
		t.Errorf("got %v %v", k, at)
	}
	// overdue and currently inside a slot: runs now
	k, at = NextRun(ss, p, s, ts("2026-10-22 05:30"))
	if k != Refresh || !at.Equal(ts("2026-10-22 05:30")) {
		t.Errorf("got %v %v", k, at)
	}
	// overdue, outside slot: next slot start
	k, at = NextRun(ss, p, s, ts("2026-10-22 12:00"))
	if k != Refresh || !at.Equal(ts("2026-10-23 02:00")) {
		t.Errorf("got %v %v", k, at)
	}
}
