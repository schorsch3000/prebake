// Package schedule holds the pure scheduling math: when jobs are due, which
// build slot they run in, and the retry/queue state machine. It has no I/O and
// takes the current time as a parameter.
package schedule

import (
	"hash/fnv"
	"time"

	"prebake/internal/config"
)

// Window is one concrete occurrence of a slot on the calendar.
type Window struct {
	Start, End time.Time // End is exclusive
}

func (w Window) Contains(t time.Time) bool { return !t.Before(w.Start) && t.Before(w.End) }

func at(day time.Time, offset time.Duration) time.Time {
	// time.Date (not midnight+offset) keeps wall-clock times correct across DST changes
	m := int(offset / time.Minute)
	return time.Date(day.Year(), day.Month(), day.Day(), m/60, m%60, 0, 0, day.Location())
}

// windows returns the occurrences of the slots that start on the days
// [from, to] relative to t's date, in t's location.
func windows(slots []config.Slot, t time.Time, from, to int) []Window {
	var out []Window
	for d := from; d <= to; d++ {
		day := time.Date(t.Year(), t.Month(), t.Day()+d, 0, 0, 0, 0, t.Location())
		for _, s := range slots {
			w := Window{Start: at(day, s.Start)}
			endDay := day
			if s.End <= s.Start { // crosses midnight
				endDay = time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, t.Location())
			}
			w.End = at(endDay, s.End)
			out = append(out, w)
		}
	}
	return out
}

// CurrentWindow returns the slot window containing t, if any.
func CurrentWindow(slots []config.Slot, t time.Time) (Window, bool) {
	for _, w := range windows(slots, t, -1, 0) {
		if w.Contains(t) {
			return w, true
		}
	}
	return Window{}, false
}

// NextStart returns the earliest slot start strictly after t. The zero time is
// returned when there are no slots.
func NextStart(slots []config.Slot, t time.Time) time.Time {
	var best time.Time
	for _, w := range windows(slots, t, 0, 2) {
		if w.Start.After(t) && (best.IsZero() || w.Start.Before(best)) {
			best = w.Start
		}
	}
	return best
}

// Jitter returns a stable offset in [0, max) derived from key, so jobs do not
// all start at the beginning of a slot.
func Jitter(key string, max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	h := fnv.New64a()
	h.Write([]byte(key))
	return time.Duration(h.Sum64() % uint64(max))
}
