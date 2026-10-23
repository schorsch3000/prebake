package schedule

import (
	"time"

	"github.com/schorsch3000/prebake/internal/config"
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

// Decide applies the slot rule. Slots are a preference, not a restriction:
//
//   - A job that is due now runs now, inside a slot or not (a missed slot, a long
//     downtime, a retry, a config change must not leave an image stale for a day).
//   - A job that is not yet due runs early, inside a slot only, when it would become due
//     before the next slot starts: it then runs in the last slot before its due time and
//     is never late. jitter delays that early start after the slot opens (capped to half
//     the window); it never delays a job that is due.
//
// A manual trigger bypasses Decide.
func Decide(slots []config.Slot, p Policy, s State, now time.Time, jitter time.Duration) Decision {
	if k, due := Pick(p, s, now); k != None {
		return Decision{Run: true, Kind: k, Due: due, Reason: "due"}
	}
	w, ok := CurrentWindow(slots, now)
	if !ok {
		return Decision{Reason: "not due, outside slot"}
	}
	if max := w.End.Sub(w.Start) / 2; jitter > max {
		jitter = max
	}
	if now.Before(w.Start.Add(jitter)) {
		return Decision{Reason: "waiting for jitter offset"}
	}
	horizon := NextStart(slots, now)
	if horizon.IsZero() || horizon.Before(now) {
		return Decision{Reason: "not due"}
	}
	if k, due := Pick(p, s, horizon); k != None {
		return Decision{Run: true, Kind: k, Due: due, Reason: "due before the next slot, running in this one"}
	}
	return Decision{Reason: "not due before the next slot"}
}

// NextRun estimates when a job will next run, for dry-run output: now if it is due,
// otherwise the slot start in which the slot rule will pick it up.
func NextRun(slots []config.Slot, p Policy, s State, now time.Time) (Kind, time.Time) {
	kind, due := Rebuild, RebuildDue(p, s)
	if d := RefreshDue(p, s); d.Before(due) {
		kind, due = Refresh, d
	}
	if !due.After(now) {
		return kind, now // due: runs as soon as the queue gets to it, slot or not
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
