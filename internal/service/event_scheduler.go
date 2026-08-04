package service

import (
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// refreshScheduler coalesces and debounces refresh requests.
//
// One lifecycle transition produces a burst: `docker restart` emits kill, die,
// stop, start, and usually a health_status within a second or two. Acting on
// each would inspect the same container five times and write it five times.
// Instead a request per resource is held for the debounce window, and repeated
// requests for the same resource collapse into the pending one.
//
// The design constraints are deliberate and all point the same way -- an event
// storm must cost bounded resources:
//
//   - No goroutine per event. There is exactly one worker, owned by
//     EventService.Run.
//   - No timer per event. There is one timer, re-armed as the queue changes.
//   - The pending set is a map keyed by resource, so a thousand events for one
//     container are one entry.
//   - The map is hard-capped. Past the cap, the scheduler stops tracking
//     individual resources and escalates to a full reconciliation, which is
//     both cheaper and more correct than tracking an unbounded set.
//   - A full reconciliation request outranks every targeted one and clears
//     them: a full sweep re-reads everything they would have.
type refreshScheduler struct {
	mu sync.Mutex

	debounce time.Duration
	maxSize  int

	// pending maps a resource to when its debounce window expires.
	pending map[refreshKey]time.Time
	// fullPending is set when a full reconciliation is owed. It is separate
	// from pending because it is not a resource and must not be evicted.
	fullPending bool
	// fullTrigger records why the reconciliation was asked for, which is what
	// distinguishes a routine periodic sweep from an escalation in the logs.
	fullTrigger domain.RefreshTrigger

	// overflowed records that the pending set hit its cap, so status can report
	// the degradation until the resulting reconciliation completes.
	overflowed bool

	// wake carries a one-slot signal to the worker. Capacity 1 with a
	// non-blocking send: a second signal while one is unread is redundant, and
	// dropping it is what stops a producer ever blocking on the scheduler.
	wake chan struct{}

	now func() time.Time
}

// refreshKey identifies a debounced resource.
//
// Kind is part of the key so a container ID and a volume name that happen to
// collide are still two entries.
type refreshKey struct {
	kind   domain.RefreshRequest
	target string
}

func newRefreshScheduler(debounce time.Duration, maxSize int, now func() time.Time) *refreshScheduler {
	if maxSize < 1 {
		maxSize = 1
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &refreshScheduler{
		debounce: debounce,
		maxSize:  maxSize,
		pending:  make(map[refreshKey]time.Time),
		wake:     make(chan struct{}, 1),
		now:      now,
	}
}

// wakeup is the channel the worker selects on.
func (s *refreshScheduler) wakeup() <-chan struct{} { return s.wake }

// request enqueues a refresh, coalescing with anything already pending.
//
// Never blocks. Reports whether the request caused an overflow escalation, so
// the caller can log and count it.
func (s *refreshScheduler) request(kind domain.RefreshRequest, target string) (overflowed bool) {
	if kind == domain.RefreshNone {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if kind == domain.RefreshFull {
		s.requestFullLocked(domain.TriggerReconcile)
		s.signalLocked()
		return false
	}

	// A full reconciliation already covers every targeted request, so there is
	// nothing to add.
	if s.fullPending {
		return false
	}

	key := refreshKey{kind: kind, target: target}
	deadline := s.now().Add(s.debounce)

	if _, exists := s.pending[key]; exists {
		// Already queued. The deadline is deliberately NOT extended: a
		// container emitting health_status every few seconds would otherwise
		// have its refresh postponed forever. The first request's deadline
		// stands, so a busy resource is refreshed at a steady cadence.
		return false
	}

	if len(s.pending) >= s.maxSize {
		// Tracking more resources individually would grow without bound. A full
		// reconciliation covers all of them and costs less than the tracking.
		s.pending = make(map[refreshKey]time.Time)
		s.overflowed = true
		s.requestFullLocked(domain.TriggerReconcile)
		s.signalLocked()
		return true
	}

	s.pending[key] = deadline
	s.signalLocked()
	return false
}

// requestFull enqueues a full reconciliation, discarding pending targeted work.
func (s *refreshScheduler) requestFull(trigger domain.RefreshTrigger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestFullLocked(trigger)
	s.signalLocked()
}

// markOverflow records a queue overflow that happened elsewhere -- the event
// reader dropping an event because the processing queue was full -- and
// escalates to reconciliation. Dropped events may have been the only notice of
// a change, so nothing narrower is honest.
func (s *refreshScheduler) markOverflow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overflowed = true
	s.requestFullLocked(domain.TriggerReconcile)
	s.signalLocked()
}

func (s *refreshScheduler) requestFullLocked(trigger domain.RefreshTrigger) {
	if !s.fullPending {
		s.fullPending = true
		s.fullTrigger = trigger
	}
	// Targeted requests are dropped: the sweep re-reads everything they name,
	// so keeping them would mean inspecting those resources twice.
	clear(s.pending)
}

func (s *refreshScheduler) signalLocked() {
	select {
	case s.wake <- struct{}{}:
	default:
		// A signal is already waiting; the worker will re-examine the whole
		// queue when it wakes, so a second one buys nothing.
	}
}

// due returns the work whose debounce window has expired, together with how
// long to wait before anything else becomes due.
//
// A zero wait means nothing is pending and the worker should block on its
// signal channel rather than on a timer.
//
// Claimed work is removed from the queue, so the caller owns it: a failure is
// the caller's to escalate, not something the scheduler retries invisibly.
func (s *refreshScheduler) due(now time.Time) (full bool, trigger domain.RefreshTrigger, targeted []refreshKey, wait time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.fullPending {
		s.fullPending = false
		return true, s.fullTrigger, nil, 0
	}

	var soonest time.Duration
	for key, deadline := range s.pending {
		if !deadline.After(now) {
			targeted = append(targeted, key)
			delete(s.pending, key)
			continue
		}
		remaining := deadline.Sub(now)
		if soonest == 0 || remaining < soonest {
			soonest = remaining
		}
	}

	return false, "", targeted, soonest
}

// clearOverflow marks the overflow resolved, which a completed reconciliation
// does. Health stops reporting degraded once this is called.
func (s *refreshScheduler) clearOverflow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overflowed = false
}

// snapshot reports the scheduler's state for the status endpoint.
func (s *refreshScheduler) snapshot() (pending int, fullPending, overflowed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending), s.fullPending, s.overflowed
}
