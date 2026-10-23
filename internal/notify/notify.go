// Package notify defines what the daemon reports to a human. Delivery targets
// (Pushover) implement Notifier.
package notify

import "context"

// Type is the kind of event; delivery targets map it to a priority.
type Type string

const (
	JobParked   Type = "job-parked"   // a build failed twice and waits for a push or trigger
	TestFailed  Type = "test-failed"  // like JobParked, but the smoke test was the cause
	RepoInvalid Type = "repo-invalid" // a pushed commit failed validation; the last good one stays active
	PruneFailed Type = "prune-failed" // retention or GC failed
	Recovered   Type = "recovered"    // something that was failing works again
)

// Event is one notification. Title is short, Message may be multi-line.
type Event struct {
	Type     Type
	Title    string
	Message  string
	Priority int // Pushover scale -2..1, set by the daemon from the repo config
}

// Failure reports whether the event type is a problem (as opposed to a recovery).
func (t Type) Failure() bool { return t != Recovered }

// Notifier delivers events. Implementations must not block for long; the daemon
// calls Notify from its main loop.
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

// Nop discards events.
type Nop struct{}

func (Nop) Notify(context.Context, Event) error { return nil }
