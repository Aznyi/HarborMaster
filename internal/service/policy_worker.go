package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The policy worker: refresh integration, coalescing, and the periodic sweep.
//
// # When evaluation happens
//
// Three triggers, in order of authority:
//
//  1. After every SUCCESSFUL INVENTORY REFRESH. The inventory service notifies
//     its observers once a refresh has COMMITTED, so a pass always reads
//     committed data rather than the generation the refresh is about to
//     replace. This is the authoritative trigger and it covers the startup
//     refresh, the periodic one, the manual one, and the event engine's
//     reconciliation, because all four funnel through the same commit.
//  2. After a TARGETED refresh of one container commits, which keeps the
//     dashboard current between full refreshes without re-reading the estate.
//  3. On the PERIODIC SWEEP, which is the safety net for anything the other
//     two missed, and on the manual evaluate endpoint.
//
// # Bounded under a refresh storm
//
// The coalescing, the cap, and the overflow escalation live in
// evaluationQueue, which the drift engine shares. A request never blocks, and
// one that cannot be tracked escalates to a full sweep rather than being
// dropped: a dropped request means a policy failure that is never noticed.

// RequestEvaluation schedules a policy evaluation for one container.
//
// Called after a targeted refresh commits. Never blocks and never fails.
func (s *PolicyService) RequestEvaluation(containerID string) {
	if !s.cfg.Enabled || !s.cfg.EvaluateOnEvents || s.queue == nil {
		return
	}
	if overflowed := s.queue.request(containerID); overflowed {
		s.logger.Warn("policy evaluation queue full; escalating to a full sweep",
			slog.Int("capacity", s.cfg.MaxPendingEvaluations))
	}
}

// RequestSweep schedules a full compliance pass.
//
// Called after a full inventory refresh commits -- every container has just
// been re-read, so every container's compliance may have moved -- and by the
// manual evaluate endpoint.
func (s *PolicyService) RequestSweep() {
	if !s.cfg.Enabled || s.queue == nil {
		return
	}
	s.queue.requestSweep()
}

// InventoryRefreshed implements the inventory service's RefreshObserver.
//
// Non-blocking by contract: the inventory service must never wait on a
// downstream consumer, because that is how a slow consumer turns into a stalled
// refresh loop.
func (s *PolicyService) InventoryRefreshed(int64) { s.RequestSweep() }

// Run drives evaluation until ctx is cancelled.
//
// One goroutine for the debounced evaluations, the periodic sweep, and
// retention. They are combined because they must not overlap: a sweep while a
// targeted evaluation is mid-write buys nothing, and pruning while either runs
// would contend for the single SQLite writer.
func (s *PolicyService) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("policy engine disabled by configuration")
		return
	}

	s.logger.Info("policy engine starting",
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

// runEvaluations evaluates a batch of containers against ONE loaded policy set.
//
// This is the batching that matters for a burst: a storm that queues fifty
// containers costs one policy query, not fifty. Evaluation itself is sequential
// because each container writes through the single SQLite writer, so
// parallelism would queue at the database anyway while multiplying peak memory.
func (s *PolicyService) runEvaluations(ctx context.Context, containers []string) {
	if !s.ready() {
		return
	}

	policies, err := s.loadActivePolicies(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Warn("could not load policies for evaluation", slog.String("error", err.Error()))
		return
	}
	if len(policies) == 0 {
		// Nothing to check. Deliberately writes no evaluation rows: an estate
		// with no policies has not been found compliant, it has not been asked.
		return
	}

	for _, containerID := range containers {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.evaluateAgainst(ctx, containerID, policies); err != nil {
			// A container that left the inventory between the refresh and the
			// evaluation is ordinary churn, not a fault.
			if isExpectedPolicyMiss(err) {
				continue
			}
			s.logger.Warn("policy evaluation failed",
				slog.String("container", domain.ShortenID(containerID)),
				slog.String("error", err.Error()))
		}
	}
}

// runSweep performs a full pass, bounded and detached from cancellation for
// long enough to finish what it started.
func (s *PolicyService) runSweep(ctx context.Context) {
	sweepCtx, cancel := GraceContext(ctx, policyWriteGrace, s.sweepBudget())
	defer cancel()

	started := s.now()
	result, err := s.Sweep(sweepCtx)
	if err != nil {
		if sweepCtx.Err() != nil {
			return
		}
		if errors.Is(err, ErrNoActivePolicies) {
			// Not a failure. The overflow flag is still cleared: whatever the
			// queue was escalating for has now been considered.
			s.queue.clearOverflow()
			s.logger.Debug("policy sweep skipped; no active policies are defined")
			return
		}
		s.logger.Warn("policy sweep failed", slog.String("error", err.Error()))
		return
	}

	s.queue.clearOverflow()
	s.logger.Info("policy sweep complete",
		slog.Int("evaluated", result.Evaluated),
		slog.Int("skipped", result.Skipped),
		slog.Int("failed", result.Failed),
		slog.Duration("took", s.now().Sub(started)))
}

// sweepBudget bounds a full pass.
//
// Derived from the per-container timeout rather than set as a constant, so an
// operator who widens one does not have to discover the other. Capped, because
// a sweep that outlives its own interval would overlap the next one.
func (s *PolicyService) sweepBudget() time.Duration {
	budget := s.cfg.EvaluationTimeout * 500
	if s.cfg.SweepInterval > 0 && budget > s.cfg.SweepInterval {
		budget = s.cfg.SweepInterval
	}
	if budget < time.Minute {
		budget = time.Minute
	}
	return budget
}

// runRetention prunes resolved violations.
func (s *PolicyService) runRetention(ctx context.Context) {
	if s.cfg.RetentionAge <= 0 || s.pruner == nil {
		return
	}

	cutoff := s.now().Add(-s.cfg.RetentionAge)
	removed, err := s.pruner.PruneResolvedViolations(ctx, cutoff, policyPruneBatch)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Warn("policy retention pass failed", slog.String("error", err.Error()))
		return
	}
	if removed > 0 {
		s.logger.Info("pruned resolved policy violations",
			slog.Int64("removed", removed),
			slog.Duration("maxAge", s.cfg.RetentionAge))
	}
}

// policyPruneBatch bounds one retention transaction, so a large backlog cannot
// hold the single SQLite writer for an unbounded time.
const policyPruneBatch = 500

// Status reports the engine state for the health endpoint.
func (s *PolicyService) Status() domain.PolicyEngineStatus {
	status := domain.PolicyEngineStatus{Enabled: s.cfg.Enabled}

	s.counts.RLock()
	status.PolicyCount = s.active
	s.counts.RUnlock()

	if s.queue == nil {
		return status
	}
	status.PendingEvaluations, status.SweepPending, status.Overflowed = s.queue.snapshot()
	return status
}

// isExpectedPolicyMiss reports whether an evaluation failure is ordinary churn
// rather than a fault worth logging.
//
// A container that left the inventory between the refresh and the evaluation is
// the common case on a busy host, and logging it at warning level would fill
// the log with the normal operation of `docker compose up`.
func isExpectedPolicyMiss(err error) bool {
	return errors.Is(err, store.ErrNotFound) ||
		errors.Is(err, ErrPolicyDisabled) ||
		errors.Is(err, ErrNoActivePolicies)
}
