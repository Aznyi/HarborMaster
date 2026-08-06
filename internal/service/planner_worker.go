package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The planner worker.
//
// # When generation happens
//
//  1. After every SUCCESSFUL INVENTORY REFRESH. A plan rests on the inventory,
//     so a committed refresh is the moment its inputs may have moved.
//  2. On the PERIODIC PASS, which catches the inputs a refresh does not touch:
//     a registry lookup that found an update, a policy an administrator added,
//     drift the engine noticed.
//  3. On REQUEST, through POST /plans/generate.
//
// All three converge on the same bounded pass, and the fingerprint means a run
// with nothing to say writes nothing at all. That is what makes it safe to
// trigger this often: the common case costs five queries per batch and no
// writes.
//
// # There is no scheduling here, in the other sense
//
// Nothing queues a change, defers one, or acts at a chosen time. The only thing
// scheduled is HarborMaster's own analysis of its own database.

// RequestGeneration asks for a generation pass.
//
// Never blocks and never fails. Called by the generate endpoint; the pass
// itself is bounded and refuses to overlap with one already running.
func (s *PlannerService) RequestGeneration() {
	if !s.cfg.Enabled {
		return
	}

	s.state.Lock()
	s.status.pending = true
	s.state.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// InventoryRefreshed implements the inventory service's RefreshObserver.
//
// Non-blocking by contract: the inventory service must never wait on a
// downstream consumer. This only sets a flag; the pass happens on the worker
// goroutine.
func (s *PlannerService) InventoryRefreshed(int64) { s.RequestGeneration() }

// Run drives generation until ctx is cancelled.
//
// One goroutine for generation and retention. They are combined because they
// must not overlap: pruning while a pass is mid-write would contend for the
// single SQLite writer.
func (s *PlannerService) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("change planning disabled by configuration")
		return
	}

	s.logger.Info("change planner starting",
		slog.String("plannerVersion", domain.PlannerVersion),
		slog.Duration("interval", s.cfg.Interval),
		slog.Int("batchSize", s.cfg.BatchSize))

	if s.cfg.GenerateOnStartup {
		s.RequestGeneration()
	}

	generate := newOptionalTicker(s.cfg.Interval)
	defer generate.Stop()

	prune := newOptionalTicker(s.cfg.PruneInterval)
	defer prune.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-s.wake:
			s.runPass(ctx)

		case <-generate.C():
			s.runPass(ctx)

		case <-prune.C():
			s.runRetention(ctx)
		}
	}
}

// runPass performs one generation pass, bounded and detached from cancellation
// for long enough to finish what it started.
func (s *PlannerService) runPass(ctx context.Context) {
	s.state.Lock()
	s.status.pending = false
	s.state.Unlock()

	passCtx, cancel := GraceContext(ctx, plannerWriteGrace, s.passBudget())
	defer cancel()

	started := s.now()
	result, err := s.Generate(passCtx)
	if err != nil {
		if passCtx.Err() != nil {
			return
		}
		s.logger.Warn("change plan generation failed", slog.String("error", err.Error()))
		return
	}

	at := s.now()
	s.state.Lock()
	s.status.lastRunAt = &at
	s.status.generated = result.Generated
	s.status.unchanged = result.Unchanged
	s.status.skipped = result.Skipped
	s.state.Unlock()

	// Logged only when something was actually written. A settled estate
	// generates a pass every refresh and writes nothing; saying so each time
	// would be noise that buries the passes that mattered.
	if result.Generated > 0 {
		s.logger.Info("change plans generated",
			slog.Int("generated", result.Generated),
			slog.Int("unchanged", result.Unchanged),
			slog.Int("skipped", result.Skipped),
			slog.Duration("took", at.Sub(started)))
	}
}

// passBudget bounds one generation pass.
//
// Derived from the configured timeout and capped by the interval, so a pass
// cannot outlive its own schedule and overlap the next.
func (s *PlannerService) passBudget() time.Duration {
	budget := s.cfg.GenerationTimeout
	if s.cfg.Interval > 0 && budget > s.cfg.Interval {
		budget = s.cfg.Interval
	}
	if budget < time.Minute {
		budget = time.Minute
	}
	return budget
}

// plannerWriteGrace bounds how long a pass may run past cancellation to finish
// its current transaction.
const plannerWriteGrace = 5 * time.Second

// runRetention prunes superseded and orphaned plans.
func (s *PlannerService) runRetention(ctx context.Context) {
	if s.store == nil {
		return
	}

	if s.cfg.RetentionAge > 0 {
		cutoff := s.now().Add(-s.cfg.RetentionAge)
		removed, err := s.store.PruneSuperseded(ctx, cutoff, plannerPruneBatch)
		switch {
		case err != nil && ctx.Err() == nil:
			s.logger.Warn("plan retention pass failed", slog.String("error", err.Error()))
		case removed > 0:
			s.logger.Info("pruned superseded change plans",
				slog.Int64("removed", removed),
				slog.Duration("maxAge", s.cfg.RetentionAge))
		}
	}

	removed, err := s.store.PruneOrphans(ctx, plannerPruneBatch)
	switch {
	case err != nil && ctx.Err() == nil:
		s.logger.Warn("plan orphan retention failed", slog.String("error", err.Error()))
	case removed > 0:
		s.logger.Info("removed plans for containers no longer present",
			slog.Int64("removed", removed))
	}
}

// plannerPruneBatch bounds one retention transaction, so a large backlog cannot
// hold the single SQLite writer for an unbounded time.
const plannerPruneBatch = 500

// Status reports the planner state for the generate endpoint.
func (s *PlannerService) Status() domain.PlannerStatus {
	status := domain.PlannerStatus{
		Enabled:        s.cfg.Enabled,
		PlannerVersion: domain.PlannerVersion,
	}

	s.state.RLock()
	defer s.state.RUnlock()

	status.Running = s.status.running
	status.Pending = s.status.pending
	status.LastRunAt = s.status.lastRunAt
	status.Generated = s.status.generated
	status.Unchanged = s.status.unchanged
	status.Skipped = s.status.skipped
	return status
}
