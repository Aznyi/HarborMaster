package service

import (
	"sync"
	"time"
)

// The coalescing evaluation queue.
//
// Shared by the drift engine and the policy engine, which have the same
// scheduling problem: a burst of container changes must become a bounded amount
// of work, and a queue that cannot be tracked must degrade into something that
// covers everything rather than dropping requests.
//
// # Bounded under a refresh storm
//
// The same discipline the event engine's refreshScheduler uses, for the same
// reason:
//
//   - One worker goroutine, owned by the caller's Run. No goroutine per event.
//   - One timer, re-armed. No timer per container.
//   - The pending set is keyed by container, so a thousand events for one
//     container are one entry.
//   - The set is hard-capped. Past the cap the queue stops tracking containers
//     individually and escalates to a full sweep, which covers all of them and
//     costs less than the tracking.
//
// A request NEVER blocks and never fails. A request that cannot be tracked
// escalates rather than being dropped, because a dropped request means a
// difference -- or a policy failure -- that is never noticed.
type evaluationQueue struct {
	mu sync.Mutex

	debounce time.Duration
	maxSize  int

	// pending maps a container to when its debounce window expires.
	pending map[string]time.Time
	// sweepPending is set when a full sweep is owed. Separate from pending
	// because it is not a container and must not be evicted.
	sweepPending bool
	// overflowed records that the set hit its cap, so status can report the
	// degradation until the resulting sweep completes.
	overflowed bool

	// wake carries a one-slot signal. Capacity 1 with a non-blocking send: a
	// second signal while one is unread is redundant, and dropping it is what
	// stops a producer ever blocking on the queue.
	wake chan struct{}

	now func() time.Time
}

func newEvaluationQueue(debounce time.Duration, maxSize int, now func() time.Time) *evaluationQueue {
	if maxSize < 1 {
		maxSize = 1
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &evaluationQueue{
		debounce: debounce,
		maxSize:  maxSize,
		pending:  make(map[string]time.Time),
		wake:     make(chan struct{}, 1),
		now:      now,
	}
}

func (q *evaluationQueue) wakeup() <-chan struct{} { return q.wake }

// request enqueues one container, coalescing with anything already pending.
//
// Never blocks. Reports whether the request caused an overflow escalation.
func (q *evaluationQueue) request(containerID string) (overflowed bool) {
	if containerID == "" {
		return false
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	// A sweep already covers every container, so there is nothing to add.
	if q.sweepPending {
		return false
	}

	if _, exists := q.pending[containerID]; exists {
		// Already queued. The deadline is deliberately NOT extended: a
		// container emitting health_status every few seconds would otherwise
		// have its evaluation postponed forever. The first request's deadline
		// stands, so a busy container is evaluated at a steady cadence.
		return false
	}

	if len(q.pending) >= q.maxSize {
		clear(q.pending)
		q.overflowed = true
		q.sweepPending = true
		q.signalLocked()
		return true
	}

	q.pending[containerID] = q.now().Add(q.debounce)
	q.signalLocked()
	return false
}

// requestSweep enqueues a full sweep, discarding pending per-container work.
func (q *evaluationQueue) requestSweep() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sweepPending = true
	// Per-container requests are dropped: the sweep evaluates everything they
	// name, so keeping them would mean evaluating those containers twice.
	clear(q.pending)
	q.signalLocked()
}

func (q *evaluationQueue) signalLocked() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// due returns the work whose debounce window has expired, and how long until
// anything else becomes due.
//
// Claimed work is removed, so the caller owns it: a failure is the caller's to
// log, not something the queue retries invisibly.
func (q *evaluationQueue) due(now time.Time) (sweep bool, containers []string, wait time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.sweepPending {
		q.sweepPending = false
		return true, nil, 0
	}

	var soonest time.Duration
	for id, deadline := range q.pending {
		if !deadline.After(now) {
			containers = append(containers, id)
			delete(q.pending, id)
			continue
		}
		if remaining := deadline.Sub(now); soonest == 0 || remaining < soonest {
			soonest = remaining
		}
	}
	return false, containers, soonest
}

func (q *evaluationQueue) clearOverflow() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.overflowed = false
}

func (q *evaluationQueue) snapshot() (pending int, sweepPending, overflowed bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending), q.sweepPending, q.overflowed
}
