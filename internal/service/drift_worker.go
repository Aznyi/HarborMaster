package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The drift worker: event integration, coalescing, and the periodic sweep.
//
// # Why drift gets its own queue rather than joining the refresh scheduler
//
// The event engine's refreshScheduler exists to decide when to RE-READ a
// resource from Docker. Drift evaluation is a different job with a different
// input: it reads the inventory HarborMaster has already persisted, and it must
// run AFTER that inventory is committed, not instead of it. Bolting drift onto
// the refresh scheduler would mean evaluating against the inventory the refresh
// is about to replace -- comparing the baseline against data that is one
// generation stale, every time.
//
// So the ordering is: an event schedules an inventory refresh; the refresh
// commits; the refresh's completion schedules a drift evaluation. Inventory
// reconciliation stays authoritative, and drift is always computed from
// committed data.
//
// # Bounded under an event storm
//
// The same discipline as refreshScheduler, for the same reason:
//
//   - One worker goroutine, owned by Run. No goroutine per event.
//   - One timer, re-armed. No timer per container.
//   - The pending set is keyed by container, so a thousand events for one
//     container are one entry.
//   - The set is hard-capped. Past the cap the worker stops tracking
//     containers individually and escalates to a full sweep, which covers all
//     of them and costs less than the tracking.

// driftQueue coalesces per-container evaluation requests.
type driftQueue struct {
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

func newDriftQueue(debounce time.Duration, maxSize int, now func() time.Time) *driftQueue {
	if maxSize < 1 {
		maxSize = 1
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &driftQueue{
		debounce: debounce,
		maxSize:  maxSize,
		pending:  make(map[string]time.Time),
		wake:     make(chan struct{}, 1),
		now:      now,
	}
}

func (q *driftQueue) wakeup() <-chan struct{} { return q.wake }

// request enqueues one container, coalescing with anything already pending.
//
// Never blocks. Reports whether the request caused an overflow escalation.
func (q *driftQueue) request(containerID string) (overflowed bool) {
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
func (q *driftQueue) requestSweep() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sweepPending = true
	// Per-container requests are dropped: the sweep evaluates everything they
	// name, so keeping them would mean evaluating those containers twice.
	clear(q.pending)
	q.signalLocked()
}

func (q *driftQueue) signalLocked() {
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
func (q *driftQueue) due(now time.Time) (sweep bool, containers []string, wait time.Duration) {
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

func (q *driftQueue) clearOverflow() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.overflowed = false
}

func (q *driftQueue) snapshot() (pending int, sweepPending, overflowed bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending), q.sweepPending, q.overflowed
}

// ---------------------------------------------------------------- worker --

// RequestEvaluation schedules a drift evaluation for one container.
//
// Called by the event engine after a targeted refresh commits. Never blocks
// and never fails: a request that cannot be tracked escalates to a sweep
// rather than being dropped, because a dropped request means drift that is
// never noticed.
func (s *DriftService) RequestEvaluation(containerID string) {
	if !s.cfg.Enabled || !s.cfg.EvaluateOnEvents || s.queue == nil {
		return
	}
	if overflowed := s.queue.request(containerID); overflowed {
		s.logger.Warn("drift evaluation queue full; escalating to a full sweep",
			slog.Int("capacity", s.cfg.MaxPendingEvaluations))
	}
}

// RequestSweep schedules a full sweep.
//
// Called after a full inventory reconciliation commits: every container has
// just been re-read, so every container's drift may have moved.
func (s *DriftService) RequestSweep() {
	if !s.cfg.Enabled || s.queue == nil {
		return
	}
	s.queue.requestSweep()
}

// Run drives evaluation until ctx is cancelled.
//
// One goroutine for the debounced evaluations, the periodic sweep, and
// retention. They are combined because they must not overlap: a sweep while a
// targeted evaluation is mid-write buys nothing, and pruning while either runs
// would contend for the single SQLite writer.
func (s *DriftService) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("drift detection disabled by configuration")
		return
	}

	s.logger.Info("drift detection starting",
		slog.Duration("debounce", s.cfg.EvaluationDebounce),
		slog.Duration("sweepInterval", s.cfg.SweepInterval),
		slog.Bool("onEvents", s.cfg.EvaluateOnEvents))

	if s.cfg.SweepOnStartup {
		// Queued rather than run inline, so Run reaches its select promptly and
		// a slow first sweep cannot delay the first event-driven evaluation.
		s.queue.requestSweep()
	}

	sweep := newOptionalTicker(s.cfg.SweepInterval)
	defer sweep.Stop()

	prune := newOptionalTicker(s.cfg.PruneInterval)
	defer prune.Stop()

	// One timer, re-armed as the queue changes. Not one per container.
	debounce := time.NewTimer(time.Hour)
	defer debounce.Stop()
	if !debounce.Stop() {
		<-debounce.C
	}
	debounceArmed := false

	for {
		dueSweep, containers, wait := s.queue.due(s.now())
		switch {
		case dueSweep:
			s.runSweep(ctx)
			continue
		case len(containers) > 0:
			s.runEvaluations(ctx, containers)
			continue
		}

		if wait > 0 && !debounceArmed {
			debounce.Reset(wait)
			debounceArmed = true
		}

		select {
		case <-ctx.Done():
			return

		case <-s.queue.wakeup():
			// New work arrived. Re-arm from scratch on the next pass: the new
			// item may be due sooner than whatever the timer was set for.
			if debounceArmed {
				if !debounce.Stop() {
					select {
					case <-debounce.C:
					default:
					}
				}
				debounceArmed = false
			}

		case <-debounce.C:
			debounceArmed = false

		case <-sweep.C():
			s.queue.requestSweep()

		case <-prune.C():
			s.runRetention(ctx)
		}
	}
}

// runEvaluations evaluates a batch of containers sequentially.
//
// Sequential rather than concurrent: each evaluation decodes a snapshot
// document and writes through the single SQLite writer, so parallelism would
// queue at the database anyway while multiplying peak memory.
func (s *DriftService) runEvaluations(ctx context.Context, containers []string) {
	for _, containerID := range containers {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.EvaluateContainer(ctx, containerID); err != nil {
			// A container that left the inventory between the event and the
			// evaluation is ordinary churn, not a fault.
			if isExpectedDriftMiss(err) {
				continue
			}
			s.logger.Warn("drift evaluation failed",
				slog.String("container", domain.ShortenID(containerID)),
				slog.String("error", err.Error()))
		}
	}
}

// runSweep performs a full sweep, bounded and detached from cancellation for
// long enough to finish what it started.
func (s *DriftService) runSweep(ctx context.Context) {
	sweepCtx, cancel := GraceContext(ctx, driftWriteGrace, s.sweepBudget())
	defer cancel()

	started := s.now()
	result, err := s.Sweep(sweepCtx)
	if err != nil {
		if sweepCtx.Err() != nil {
			return
		}
		s.logger.Warn("drift sweep failed", slog.String("error", err.Error()))
		return
	}

	s.queue.clearOverflow()
	s.logger.Info("drift sweep complete",
		slog.Int("evaluated", result.Evaluated),
		slog.Int("skipped", result.Skipped),
		slog.Int("failed", result.Failed),
		slog.Duration("took", s.now().Sub(started)))
}

// sweepBudget bounds a full sweep.
//
// Derived from the per-container timeout rather than set as a constant, so an
// operator who widens one does not have to discover the other. Capped, because
// a sweep that outlives its own interval would overlap the next one.
func (s *DriftService) sweepBudget() time.Duration {
	budget := s.cfg.EvaluationTimeout * 500
	if s.cfg.SweepInterval > 0 && budget > s.cfg.SweepInterval {
		budget = s.cfg.SweepInterval
	}
	if budget < time.Minute {
		budget = time.Minute
	}
	return budget
}

// runRetention prunes resolved records.
func (s *DriftService) runRetention(ctx context.Context) {
	if s.cfg.RetentionAge <= 0 || s.pruner == nil {
		return
	}

	cutoff := s.now().Add(-s.cfg.RetentionAge)
	removed, err := s.pruner.PruneResolved(ctx, cutoff, driftPruneBatch)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Warn("drift retention pass failed", slog.String("error", err.Error()))
		return
	}
	if removed > 0 {
		s.logger.Info("pruned resolved drift records",
			slog.Int64("removed", removed),
			slog.Duration("maxAge", s.cfg.RetentionAge))
	}
}

// driftPruneBatch bounds one retention transaction, so a large backlog cannot
// hold the single SQLite writer for an unbounded time.
const driftPruneBatch = 500

// Status reports the queue state for the health endpoint.
func (s *DriftService) Status() domain.DriftEngineStatus {
	status := domain.DriftEngineStatus{Enabled: s.cfg.Enabled}
	if s.queue == nil {
		return status
	}
	status.PendingEvaluations, status.SweepPending, status.Overflowed = s.queue.snapshot()
	return status
}

// isExpectedDriftMiss reports whether an evaluation failure is ordinary churn
// rather than a fault worth logging.
//
// A container that left the inventory between the event and the evaluation is
// the common case on a busy host, and logging it at warning level would fill
// the log with the normal operation of `docker compose up`.
func isExpectedDriftMiss(err error) bool {
	return errors.Is(err, store.ErrNotFound) ||
		errors.Is(err, ErrDriftDisabled) ||
		errors.Is(err, ErrNoBaseline)
}

// optionalTicker is a ticker that may be disabled.
//
// A disabled ticker returns a nil channel, which blocks forever in a select --
// exactly the behaviour "this timer is off" should have, and without the
// caller needing a branch per select case.
type optionalTicker struct {
	ticker *time.Ticker
}

func newOptionalTicker(interval time.Duration) optionalTicker {
	if interval <= 0 {
		return optionalTicker{}
	}
	return optionalTicker{ticker: time.NewTicker(interval)}
}

func (t optionalTicker) C() <-chan time.Time {
	if t.ticker == nil {
		return nil
	}
	return t.ticker.C
}

func (t optionalTicker) Stop() {
	if t.ticker != nil {
		t.ticker.Stop()
	}
}
