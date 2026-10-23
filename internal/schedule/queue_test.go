package schedule

import (
	"testing"
	"time"
)

var (
	t0    = ts("2026-10-22 02:00")
	retry = 15 * time.Minute
)

func k(c string) Key { return Key{c, "default"} }

// gate allows every job when inSlot, none otherwise (what the daemon does inside and outside slots
// for jobs that are not needed right now).
func gate(inSlot bool) func(Key) bool { return func(Key) bool { return inSlot } }

func next(t *testing.T, q *Queue, now time.Time, inSlot bool) Key {
	t.Helper()
	j, ok := q.Next(now, gate(inSlot))
	if !ok {
		t.Fatalf("expected a job at %v", now)
	}
	return j.Key
}

func none(t *testing.T, q *Queue, now time.Time, inSlot bool) {
	t.Helper()
	if j, ok := q.Next(now, gate(inSlot)); ok {
		t.Fatalf("unexpected job %v at %v", j.Key, now)
	}
}

func TestSingleBuildAtATime(t *testing.T) {
	q := NewQueue(retry, nil)
	q.Enqueue(k("a"), Refresh)
	q.Enqueue(k("b"), Refresh)
	if got := next(t, q, t0, true); got != k("a") {
		t.Fatal(got)
	}
	none(t, q, t0, true) // a is still building
	q.Done(k("a"))
	if got := next(t, q, t0, true); got != k("b") {
		t.Fatal(got)
	}
}

func TestParentBeforeChild(t *testing.T) {
	q := NewQueue(retry, map[string][]string{"child": {"base"}})
	q.Enqueue(k("child"), Refresh) // enqueued first, must still wait
	q.Enqueue(k("base"), Rebuild)
	if got := next(t, q, t0, true); got != k("base") {
		t.Fatalf("parent must go first, got %v", got)
	}
	none(t, q, t0, true)
	q.Done(k("base"))
	if got := next(t, q, t0, true); got != k("child") {
		t.Fatal(got)
	}
}

func TestChildNotQueuedByParent(t *testing.T) {
	q := NewQueue(retry, map[string][]string{"child": {"base"}})
	q.Enqueue(k("base"), Rebuild)
	next(t, q, t0, true)
	q.Done(k("base"))
	if len(q.Pending()) != 0 {
		t.Errorf("a parent build must not queue its children: %v", q.Pending())
	}
}

func TestSlotGating(t *testing.T) {
	q := NewQueue(retry, nil)
	q.Enqueue(k("a"), Refresh)
	none(t, q, t0, false)
	q.Trigger(k("a"), Refresh)
	if got := next(t, q, t0, false); got != k("a") {
		t.Fatalf("manual job ignores slots: %v", got)
	}
}

func TestRetryThenPark(t *testing.T) {
	q := NewQueue(retry, nil)
	q.Enqueue(k("a"), Refresh)
	q.Enqueue(k("b"), Refresh)
	next(t, q, t0, true) // a
	if notify := q.Fail(k("a"), t0, "boom"); notify {
		t.Fatal("first failure must be silent")
	}
	if s, _ := q.StatusOf(k("a")); s != Retrying {
		t.Fatalf("status = %v", s)
	}
	// a went to the end of the queue and waits retry delay: b first
	if got := next(t, q, t0, true); got != k("b") {
		t.Fatalf("b first, got %v", got)
	}
	q.Done(k("b"))
	none(t, q, t0.Add(retry-time.Second), true)
	if got := next(t, q, t0.Add(retry), true); got != k("a") {
		t.Fatal(got)
	}
	if notify := q.Fail(k("a"), t0.Add(retry), "boom again"); !notify {
		t.Fatal("second failure must notify")
	}
	if s, msg := q.StatusOf(k("a")); s != Parked || msg != "boom again" {
		t.Fatalf("status = %v %q", s, msg)
	}
	none(t, q, t0.Add(48*time.Hour), true) // parked stays parked
	q.Enqueue(k("a"), Refresh)             // scheduler re-enqueue is ignored while parked
	none(t, q, t0.Add(48*time.Hour), true)
}

func TestRetryWaitsForSlot(t *testing.T) {
	q := NewQueue(retry, nil)
	q.Enqueue(k("a"), Refresh)
	next(t, q, t0, true)
	q.Fail(k("a"), t0, "x")
	none(t, q, t0.Add(time.Hour), false) // delay passed but outside slot
	next(t, q, t0.Add(time.Hour), true)
}

func TestTriggerUnparks(t *testing.T) {
	q := NewQueue(retry, nil)
	q.Enqueue(k("a"), Refresh)
	next(t, q, t0, true)
	q.Fail(k("a"), t0, "x")
	next(t, q, t0.Add(retry), true)
	q.Fail(k("a"), t0.Add(retry), "x")
	q.Trigger(k("a"), Rebuild)
	j, ok := q.Next(t0.Add(retry), gate(false))
	if !ok || j.Kind != Rebuild || !j.Manual {
		t.Fatalf("%+v %v", j, ok)
	}
	// after unpark a failure starts counting from zero again
	if notify := q.Fail(k("a"), t0, "x"); notify {
		t.Fatal("failure counter must reset on unpark")
	}
}

func TestParentFailureSkipsChildren(t *testing.T) {
	q := NewQueue(retry, map[string][]string{"child": {"base"}, "grand": {"child"}})
	q.Enqueue(k("base"), Rebuild)
	q.Enqueue(k("child"), Refresh)
	q.Enqueue(k("grand"), Refresh)
	next(t, q, t0, true)
	q.Fail(k("base"), t0, "x")
	none(t, q, t0, true) // child waits while parent is retrying
	next(t, q, t0.Add(retry), true)
	q.Fail(k("base"), t0.Add(retry), "x") // parked
	none(t, q, t0.Add(retry), true)
	for _, c := range []string{"child", "grand"} {
		if s, msg := q.StatusOf(k(c)); s != Skipped || msg != "parent failed" {
			t.Errorf("%s: %v %q", c, s, msg)
		}
	}
	// scheduler enqueues again: still skipped, not queued
	q.Enqueue(k("child"), Refresh)
	if len(q.Pending()) != 0 {
		t.Errorf("pending = %v", q.Pending())
	}
	// a push unparks everything; parent builds first again
	q.UnparkAll()
	if got := next(t, q, t0.Add(2*retry), true); got != k("base") {
		t.Fatalf("got %v", got)
	}
	q.Done(k("base"))
	if s, _ := q.StatusOf(k("child")); s != Queued {
		t.Errorf("child status after unpark = %v", s)
	}
	if got := next(t, q, t0.Add(2*retry), true); got != k("child") {
		t.Fatalf("got %v", got)
	}
}

func TestRebuildUpgradesQueuedRefresh(t *testing.T) {
	q := NewQueue(retry, nil)
	q.Enqueue(k("a"), Refresh)
	q.Enqueue(k("a"), Rebuild)
	q.Enqueue(k("a"), Refresh)
	j, _ := q.Next(t0, nil)
	if j.Kind != Rebuild || len(q.Pending()) != 0 {
		t.Errorf("%+v %v", j, q.Pending())
	}
}

func TestFlavorsAreIndependentKeys(t *testing.T) {
	q := NewQueue(retry, map[string][]string{"child": {"base"}})
	q.Enqueue(Key{"base", "cuda"}, Refresh)
	q.Enqueue(Key{"child", "default"}, Refresh)
	if got := next(t, q, t0, true); got != (Key{"base", "cuda"}) {
		t.Fatal(got)
	}
	none(t, q, t0, true) // child waits for any pending flavor of the parent
}

func TestAllowedPredicateIsPerJob(t *testing.T) {
	q := NewQueue(retry, nil)
	q.Enqueue(k("early"), Refresh)  // look-ahead job: only inside a slot
	q.Enqueue(k("needed"), Rebuild) // needed now: may run anywhere
	onlyNeeded := func(key Key) bool { return key == k("needed") }
	j, ok := q.Next(t0, onlyNeeded)
	if !ok || j.Key != k("needed") {
		t.Fatalf("the needed job is skipped over the gated one: %v %v", j, ok)
	}
	q.Done(k("needed"))
	if _, ok := q.Next(t0, onlyNeeded); ok {
		t.Fatal("the gated job must wait for its slot")
	}
	if j, ok := q.Next(t0, nil); !ok || j.Key != k("early") { // nil: no gate
		t.Fatalf("%v %v", j, ok)
	}
}
