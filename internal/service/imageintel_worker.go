package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The image intelligence worker.
//
// # When collection happens
//
//  1. After every SUCCESSFUL INVENTORY REFRESH, the reference set is
//     re-projected. That is cheap -- one query and one transaction, no network
//     -- and it is what makes a newly deployed container visible immediately
//     rather than at the next interval.
//  2. On the COLLECTION TICK, references whose next-check time has passed are
//     looked up. This is the only step that touches the network.
//  3. On REQUEST, through POST /images/refresh, which schedules the same pass
//     rather than running one inline.
//
// The separation matters: an inventory refresh must never trigger a burst of
// registry traffic, because inventory refreshes are frequent and registries are
// third parties. Projecting is free; asking is rationed.
//
// # Why there is one worker and no queue
//
// Unlike drift and policy, this engine's work is not event-shaped. Every
// reference carries its own next_check_at, so "what is due" is a single indexed
// question against the database rather than a set held in memory. That scales
// to ten thousand references without tracking any of them, and it survives a
// restart -- a queue would not.

// RequestCollection asks for a collection pass.
//
// Never blocks and never fails. Called by the refresh endpoint; the pass itself
// is bounded and refuses to overlap with one already running.
func (s *ImageIntelService) RequestCollection() {
	if !s.cfg.Enabled {
		return
	}

	s.state.Lock()
	s.status.sweepPending = true
	s.state.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// InventoryRefreshed implements the inventory service's RefreshObserver.
//
// Non-blocking by contract: the inventory service must never wait on a
// downstream consumer. This only sets a flag; the projection itself happens on
// the worker goroutine.
func (s *ImageIntelService) InventoryRefreshed(int64) { s.RequestCollection() }

// Run drives collection until ctx is cancelled.
//
// One goroutine for the projection, the collection pass, and retention. They
// are combined because they must not overlap: pruning while a pass is
// mid-write would contend for the single SQLite writer, and two passes would
// double the load on a registry.
func (s *ImageIntelService) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("image intelligence disabled by configuration")
		return
	}

	s.logger.Info("image intelligence starting",
		slog.Duration("refreshInterval", s.cfg.RefreshInterval),
		slog.Duration("collectInterval", s.cfg.CollectInterval),
		slog.Int("concurrency", s.cfg.MaxConcurrentRequests))

	if s.cfg.CollectOnStartup {
		s.RequestCollection()
	}

	collect := newOptionalTicker(s.cfg.CollectInterval)
	defer collect.Stop()

	prune := newOptionalTicker(s.cfg.PruneInterval)
	defer prune.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-s.wake:
			s.runPass(ctx)

		case <-collect.C():
			s.runPass(ctx)

		case <-prune.C():
			s.runRetention(ctx)
		}
	}
}

// runPass projects the inventory and then collects what is due.
//
// Projection first: a reference that entered the inventory a moment ago should
// be looked up in this pass rather than the next one.
func (s *ImageIntelService) runPass(ctx context.Context) {
	s.state.Lock()
	s.status.sweepPending = false
	s.state.Unlock()

	passCtx, cancel := GraceContext(ctx, imageWriteGrace, s.passBudget())
	defer cancel()

	if _, err := s.SyncInventory(passCtx); err != nil {
		if passCtx.Err() != nil {
			return
		}
		if !errors.Is(err, ErrImageIntelDisabled) {
			s.logger.Warn("could not project image references",
				slog.String("error", err.Error()))
		}
		return
	}

	started := s.now()
	result, err := s.Collect(passCtx)
	if err != nil {
		if passCtx.Err() != nil {
			return
		}
		s.logger.Warn("image intelligence pass failed", slog.String("error", err.Error()))
		return
	}

	at := s.now()
	s.state.Lock()
	s.status.lastSweepAt = &at
	s.status.checked = result.Checked
	s.status.skipped = result.Skipped
	s.status.failed = result.Failed
	s.state.Unlock()

	if result.Checked > 0 || result.Failed > 0 {
		s.logger.Info("image intelligence pass complete",
			slog.Int("checked", result.Checked),
			slog.Int("skipped", result.Skipped),
			slog.Int("failed", result.Failed),
			slog.Duration("took", at.Sub(started)))
	}
}

// passBudget bounds one collection pass.
//
// Derived from the batch size and the per-request timeout rather than set as a
// constant, so an operator who widens one does not have to discover the other.
// Capped by the collection interval, because a pass that outlived its own
// interval would overlap the next.
func (s *ImageIntelService) passBudget() time.Duration {
	perRequest := s.cfg.RequestTimeout
	if perRequest <= 0 {
		perRequest = 15 * time.Second
	}

	// The batch divided by the concurrency is how many sequential rounds a pass
	// needs; each round costs at most one request timeout.
	rounds := s.cfg.MaxReferencesPerPass / s.cfg.MaxConcurrentRequests
	if rounds < 1 {
		rounds = 1
	}
	budget := perRequest * time.Duration(rounds+1)

	if s.cfg.CollectInterval > 0 && budget > s.cfg.CollectInterval {
		budget = s.cfg.CollectInterval
	}
	if budget < time.Minute {
		budget = time.Minute
	}
	return budget
}

// runRetention prunes history and orphaned references.
func (s *ImageIntelService) runRetention(ctx context.Context) {
	if s.store == nil {
		return
	}

	if s.cfg.HistoryRetention > 0 {
		cutoff := s.now().Add(-s.cfg.HistoryRetention)
		removed, err := s.store.PruneHistory(ctx, cutoff, imagePruneBatch)
		switch {
		case err != nil && ctx.Err() == nil:
			s.logger.Warn("image history retention failed", slog.String("error", err.Error()))
		case removed > 0:
			s.logger.Info("pruned image update history",
				slog.Int64("removed", removed),
				slog.Duration("maxAge", s.cfg.HistoryRetention))
		}
	}

	// References no present container declares any more. Without this, every
	// tag the estate has ever run would be checked forever.
	removed, err := s.store.PruneOrphans(ctx, imagePruneBatch)
	switch {
	case err != nil && ctx.Err() == nil:
		s.logger.Warn("image reference retention failed", slog.String("error", err.Error()))
	case removed > 0:
		s.logger.Info("stopped tracking unused image references",
			slog.Int64("removed", removed))
	}
}

// imagePruneBatch bounds one retention transaction, so a large backlog cannot
// hold the single SQLite writer for an unbounded time.
const imagePruneBatch = 500

// Status reports the engine state for the health and refresh endpoints.
func (s *ImageIntelService) Status(ctx context.Context) domain.ImageIntelEngineStatus {
	status := domain.ImageIntelEngineStatus{Enabled: s.cfg.Enabled}

	s.state.RLock()
	status.Running = s.status.running
	status.SweepPending = s.status.sweepPending
	status.LastSweepAt = s.status.lastSweepAt
	status.Checked = s.status.checked
	status.Skipped = s.status.skipped
	status.Failed = s.status.failed
	s.state.RUnlock()

	if s.store != nil {
		// Best effort: a status endpoint must not fail because a count query
		// did. The zero is honest -- it says "unknown", not "none due".
		if due, err := s.store.CountDue(ctx, s.now().UTC()); err == nil {
			status.DueNow = due
		}
	}
	return status
}

// setRunning records whether a pass is in flight.
func (s *ImageIntelService) setRunning(running bool) {
	s.state.Lock()
	defer s.state.Unlock()
	s.status.running = running
}
