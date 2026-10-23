package schedule

import (
	"time"

	"prebake/internal/config"
)

type Kind int

const (
	None Kind = iota
	Refresh
	Rebuild
)

func (k Kind) String() string {
	switch k {
	case Refresh:
		return "refresh"
	case Rebuild:
		return "rebuild"
	}
	return "none"
}

// Policy holds the effective intervals of one container.
type Policy struct {
	RebuildInterval, RefreshInterval time.Duration
}

// State is when a (container, flavor) was last rebuilt and refreshed. Zero
// times mean "never".
type State struct {
	LastRebuild, LastRefresh time.Time
}

// RebuildDue is when the next rebuild is due. A never-built image is due at once.
func RebuildDue(p Policy, s State) time.Time {
	if s.LastRebuild.IsZero() {
		return time.Time{}
	}
	return s.LastRebuild.Add(p.RebuildInterval)
}

// RefreshDue is when the next refresh is due. A rebuild resets the refresh
// clock, so the later of the two timestamps counts.
func RefreshDue(p Policy, s State) time.Time {
	if s.LastRebuild.IsZero() {
		return time.Time{}
	}
	last := s.LastRefresh
	if s.LastRebuild.After(last) {
		last = s.LastRebuild
	}
	return last.Add(p.RefreshInterval)
}

// Pick returns the job to run given a horizon: everything due at or before the
// horizon counts as due. Rebuild wins over refresh.
func Pick(p Policy, s State, horizon time.Time) (Kind, time.Time) {
	if d := RebuildDue(p, s); !d.After(horizon) {
		return Rebuild, d
	}
	if d := RefreshDue(p, s); !d.After(horizon) {
		return Refresh, d
	}
	return None, time.Time{}
}

// Decision is the outcome of Decide.
type Decision struct {
	Run    bool
	Kind   Kind
	Due    time.Time
	Reason string
}

// Decide applies the slot rule. Jobs only start inside a slot. Within a slot a
// job runs if it is due now OR would become due before the next slot starts
// (otherwise it would run late), i.e. it runs in the last slot before its due
// time. jitter delays the start after the slot opens (capped to half the
// window). A manual trigger bypasses Decide.
func Decide(slots []config.Slot, p Policy, s State, now time.Time, jitter time.Duration) Decision {
	w, ok := CurrentWindow(slots, now)
	if !ok {
		return Decision{Reason: "outside slot"}
	}
	if max := w.End.Sub(w.Start) / 2; jitter > max {
		jitter = max
	}
	if now.Before(w.Start.Add(jitter)) {
		return Decision{Reason: "waiting for jitter offset"}
	}
	horizon := NextStart(slots, now)
	if horizon.IsZero() || horizon.Before(now) {
		horizon = now
	}
	if k, due := Pick(p, s, horizon); k != None {
		return Decision{Run: true, Kind: k, Due: due, Reason: "due"}
	}
	return Decision{Reason: "not due before the next slot"}
}

// NextRun estimates when a job will next run: the first slot start at or after
// the moment it must run (due time minus the slot-rule lead), for dry-run output.
func NextRun(slots []config.Slot, p Policy, s State, now time.Time) (Kind, time.Time) {
	kind, due := Rebuild, RebuildDue(p, s)
	if d := RefreshDue(p, s); d.Before(due) {
		kind, due = Refresh, d
	}
	if !due.After(now) {
		due = now
	}
	// earliest window start from which the job counts as due: the last slot
	// start at or before `due`, but never in the past
	best := time.Time{}
	for _, w := range windows(slots, now, -1, 60) {
		if !w.End.After(now) {
			continue // already over
		}
		start := w.Start
		if start.Before(now) {
			start = now // currently inside this window
		}
		next := NextStart(slots, start)
		if !due.After(start) || (!next.IsZero() && !due.After(next)) {
			if best.IsZero() || start.Before(best) {
				best = start
			}
		}
	}
	return kind, best
}
