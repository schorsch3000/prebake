package schedule

import (
	"sort"
	"sync"
	"time"
)

// Key identifies one buildable unit.
type Key struct{ Container, Flavor string }

func (k Key) String() string { return k.Container + ":" + k.Flavor }

type Status int

const (
	Idle     Status = iota // nothing pending
	Queued                 // waiting for its turn
	Building               // running now
	Retrying               // failed once, queued again after the retry delay
	Parked                 // failed twice, waits for a push or a manual trigger
	Skipped                // not built because a parent is parked
)

func (s Status) String() string {
	return [...]string{"idle", "queued", "building", "retrying", "parked", "skipped"}[s]
}

// Job is a queue entry plus the failure state kept per key.
type Job struct {
	Key     Key
	Kind    Kind
	Manual  bool      // triggered by hand: ignores slots
	RetryAt time.Time // not before this time (zero = immediately)
	Err     string    // last error
}

type entry struct {
	Job
	status   Status
	failures int
}

// Queue is the single-build-at-a-time job queue with dependency ordering and
// the retry / park state machine (docs/design.md §6.3). It is safe for
// concurrent use; time is always passed in.
type Queue struct {
	mu         sync.Mutex
	retryDelay time.Duration
	parents    map[string][]string // container -> parent containers
	order      []Key               // queue order; failed jobs go to the end
	jobs       map[Key]*entry
	building   *Key
}

func NewQueue(retryDelay time.Duration, parents map[string][]string) *Queue {
	return &Queue{retryDelay: retryDelay, parents: parents, jobs: map[Key]*entry{}}
}

// SetRetryDelay changes the delay for future failures (after a config reload).
func (q *Queue) SetRetryDelay(d time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.retryDelay = d
}

// SetParents replaces the dependency graph (after a config reload).
func (q *Queue) SetParents(parents map[string][]string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.parents = parents
}

func (q *Queue) get(k Key) *entry {
	e := q.jobs[k]
	if e == nil {
		e = &entry{Job: Job{Key: k}}
		q.jobs[k] = e
	}
	return e
}

func (q *Queue) removeFromOrder(k Key) {
	for i, o := range q.order {
		if o == k {
			q.order = append(q.order[:i], q.order[i+1:]...)
			return
		}
	}
}

// RestoreParked puts a key back into the parked state after a restart. It is
// replaced by a normal queue entry as soon as the job is triggered or unparked.
func (q *Queue) RestoreParked(k Key, kind Kind, failures int, errMsg string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e := q.get(k)
	e.Kind, e.failures, e.Err, e.status = kind, failures, errMsg, Parked
	q.removeFromOrder(k)
}

// Enqueue adds a scheduled job. It is a no-op for a job that is already
// pending, building or parked; a queued refresh is upgraded by a rebuild.
// If a parent is parked the job is marked Skipped instead of queued.
func (q *Queue) Enqueue(k Key, kind Kind) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueue(k, kind, false)
}

func (q *Queue) enqueue(k Key, kind Kind, manual bool) {
	e := q.get(k)
	switch e.status {
	case Queued, Retrying:
		if kind > e.Kind {
			e.Kind = kind
		}
		if manual {
			e.Manual = true
			e.RetryAt = time.Time{}
		}
		return
	case Building:
		return
	case Parked:
		if !manual {
			return
		}
		e.failures = 0
	}
	if !manual && q.parentParked(k.Container) {
		e.status = Skipped
		e.Err = "parent failed"
		q.removeFromOrder(k)
		return
	}
	e.Kind, e.Manual, e.RetryAt, e.status = kind, manual, time.Time{}, Queued
	e.Err = ""
	q.removeFromOrder(k)
	q.order = append(q.order, k)
}

// Trigger queues a job right now, bypassing slots, and unparks it.
func (q *Queue) Trigger(k Key, kind Kind) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueue(k, kind, true)
}

// UnparkAll re-queues every parked and skipped job; call it after a valid push.
func (q *Queue) UnparkAll() {
	q.mu.Lock()
	defer q.mu.Unlock()
	var keys []Key
	for k, e := range q.jobs {
		if e.status == Parked || e.status == Skipped {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, k := range keys {
		e := q.jobs[k]
		kind := e.Kind
		if kind == None {
			kind = Rebuild
		}
		e.failures = 0
		e.status = Idle
		q.enqueue(k, kind, false)
	}
}

// pending reports whether container has a job that must finish before its children build.
func (q *Queue) pending(container string) bool {
	for k, e := range q.jobs {
		if k.Container == container && (e.status == Queued || e.status == Retrying || e.status == Building) {
			return true
		}
	}
	return false
}

func (q *Queue) parentParked(container string) bool {
	for _, p := range q.parents[container] {
		for k, e := range q.jobs {
			if k.Container == p && e.status == Parked {
				return true
			}
		}
		if q.parentParked(p) {
			return true
		}
	}
	return false
}

func (q *Queue) parentPending(container string) bool {
	for _, p := range q.parents[container] {
		if q.pending(p) || q.parentPending(p) {
			return true
		}
	}
	return false
}

// Next returns the next job to run and marks it Building, or false if nothing
// is runnable. Only one job builds at a time. Scheduled (non-manual) jobs are
// only runnable when inSlot; jobs waiting for their retry delay are skipped
// until it has passed; a child never starts while a parent has a pending job.
// Children of a parked parent are marked Skipped.
func (q *Queue) Next(now time.Time, inSlot bool) (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.building != nil {
		return Job{}, false
	}
	for _, k := range append([]Key(nil), q.order...) {
		e := q.jobs[k]
		if q.parentParked(k.Container) {
			e.status, e.Err = Skipped, "parent failed"
			q.removeFromOrder(k)
			continue
		}
		if !e.Manual && !inSlot {
			continue
		}
		if now.Before(e.RetryAt) {
			continue
		}
		if q.parentPending(k.Container) {
			continue
		}
		e.status = Building
		q.building = &k
		q.removeFromOrder(k)
		return e.Job, true
	}
	return Job{}, false
}

// Done records a successful build.
func (q *Queue) Done(k Key) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e := q.get(k)
	e.status, e.failures, e.Err, e.Manual = Idle, 0, "", false
	if q.building != nil && *q.building == k {
		q.building = nil
	}
	// children skipped because of this parent may build again on their own schedule
	for ck, ce := range q.jobs {
		if ce.status == Skipped && !q.parentParked(ck.Container) {
			ce.status, ce.Err = Idle, ""
		}
	}
}

// Fail records a failed build. The first failure re-queues the job at the end
// after the retry delay, silently. The second consecutive failure parks it and
// returns notify=true.
func (q *Queue) Fail(k Key, now time.Time, err string) (notify bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e := q.get(k)
	if q.building != nil && *q.building == k {
		q.building = nil
	}
	e.failures++
	e.Err = err
	e.Manual = false
	if e.failures >= 2 {
		e.status = Parked
		return true
	}
	e.status = Retrying
	e.RetryAt = now.Add(q.retryDelay)
	q.removeFromOrder(k)
	q.order = append(q.order, k)
	return false
}

// StatusOf returns the state of a key and its last error.
func (q *Queue) StatusOf(k Key) (Status, string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e := q.jobs[k]; e != nil {
		return e.status, e.Err
	}
	return Idle, ""
}

// Pending returns the queued keys in order.
func (q *Queue) Pending() []Key {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Key(nil), q.order...)
}
