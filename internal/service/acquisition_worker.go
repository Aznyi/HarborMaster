package service

import (
	"context"
	"log/slog"
	"sync"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The acquisition worker.
//
// # What it does not do
//
// It does not decide to acquire anything. Every acquisition it runs was
// requested by an operator and already recorded; the worker's job is to give
// queued work a slot, within bounds.
//
// **There is no scheduled or automatic pull.** No timer here creates work, and
// the periodic ticks below only EXPIRE requests that waited too long and PRUNE
// old audit records. A HarborMaster left running with nobody asking it for
// anything downloads nothing, forever.
//
// # Bounds
//
// Global concurrency and per-registry concurrency are enforced twice: once from
// the database, which is authoritative across restarts, and once from
// in-process counters, which is what makes the limit hold between checking and
// claiming. Two workers on one process cannot both see a free slot and take it.

// Run drives acquisition until ctx is cancelled.
//
// One goroutine dispatching, with each acquisition running in its own. The
// dispatcher never blocks on a transfer, so a slow pull cannot stop a
// cancellation being noticed.
func (s *AcquisitionService) Run(ctx context.Context) {
	if !s.Enabled() {
		// Stated once at startup rather than on every request, and stated
		// distinctly from a misconfiguration: a deployment that has not opted
		// in is a supported configuration, not a fault.
		s.logger.Info("image acquisition disabled by configuration",
			slog.Bool("configured", s.cfg.Enabled),
			slog.Bool("capabilityWired", s.acquirer != nil))
		return
	}

	s.logger.Info("image acquisition starting",
		slog.Int("maxConcurrent", s.cfg.MaxConcurrent),
		slog.Int("maxPerRegistry", s.cfg.MaxPerRegistry),
		slog.Duration("pullTimeout", s.cfg.PullTimeout))

	// Anything left mid-flight by a crash is resolved BEFORE the queue is
	// touched. An acquisition in `pulling` is a claim about a process that no
	// longer exists, and an unverified image must never be recorded as
	// acquired -- see RecoverInterrupted.
	s.recover(ctx)

	sweep := newOptionalTicker(s.cfg.SweepInterval)
	defer sweep.Stop()

	prune := newOptionalTicker(s.cfg.PruneInterval)
	defer prune.Stop()

	// workers tracks dispatched acquisitions so shutdown can wait for them.
	var workers sync.WaitGroup
	defer workers.Wait()

	// Whatever was already queued is dispatched immediately. A request accepted
	// just before a restart would otherwise sit until the first sweep, which is
	// a long time to leave an operator watching a spinner -- and if the sweep
	// is disabled, forever.
	s.dispatch(ctx, &workers)

	for {
		select {
		case <-ctx.Done():
			return

		case <-s.wake:
			s.dispatch(ctx, &workers)

		case <-sweep.C():
			s.expire(ctx)
			s.dispatch(ctx, &workers)

		case <-prune.C():
			s.runRetention(ctx)
		}
	}
}

// recover resolves acquisitions interrupted by a restart.
func (s *AcquisitionService) recover(ctx context.Context) {
	recovered, err := s.store.RecoverInterrupted(ctx, s.now().UTC())
	if err != nil {
		s.logger.Warn("could not recover interrupted acquisitions",
			slog.String("error", err.Error()))
		return
	}
	if recovered > 0 {
		// Worth a warning rather than an info: an operator asked for these and
		// they did not finish, and the images may or may not be on the host.
		s.logger.Warn("acquisitions were interrupted by a restart and did not complete",
			slog.Int64("count", recovered))
	}
}

// dispatch starts as much queued work as the limits allow.
//
// Never blocks: an acquisition that does not fit stays queued and is picked up
// by the next signal or sweep.
func (s *AcquisitionService) dispatch(ctx context.Context, workers *sync.WaitGroup) {
	for {
		if ctx.Err() != nil {
			return
		}

		// A whole batch is read but at most one is started per pass, so the
		// limit is re-evaluated between each. Reading a batch keeps the query
		// count down; starting one at a time keeps the accounting exact.
		queued, err := s.store.Claimable(ctx, s.cfg.MaxConcurrent)
		if err != nil {
			s.logger.WarnContext(ctx, "could not read the acquisition queue",
				slog.String("error", err.Error()))
			return
		}
		if len(queued) == 0 {
			return
		}

		started := false
		for _, acquisition := range queued {
			if !s.reserve(acquisition.Target.Registry) {
				continue
			}

			workers.Add(1)
			go func(work domain.Acquisition) {
				defer workers.Done()
				// The slot reserved above is released by execute's deferred
				// bookkeeping, whatever happens inside it.
				s.execute(ctx, work)
				// Another slot is free, so look again rather than waiting for
				// the next tick.
				s.signal()
			}(acquisition)

			started = true
			break
		}

		if !started {
			// Everything queued is blocked by a limit. Nothing to do until
			// something finishes, and finishing signals.
			return
		}
	}
}

// reserve takes an in-process slot if one is free.
//
// The database count is authoritative across restarts, but it cannot stop two
// dispatch passes in one process both seeing a free slot: the row is not marked
// until execute claims it, and between the read and that claim the count is
// stale. These counters close that window.
//
// The slot is taken HERE and released by execute's deferred bookkeeping. The
// pairing is deliberate and asymmetric: incrementing in both places would
// double-count, and incrementing only in execute would leave the window open
// for exactly as long as it takes a goroutine to start.
func (s *AcquisitionService) reserve(registry string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.inFlight >= s.cfg.MaxConcurrent {
		return false
	}
	if s.perRegistry[registry] >= s.cfg.MaxPerRegistry {
		return false
	}

	s.inFlight++
	s.perRegistry[registry]++
	return true
}

// expire abandons requests that waited past their deadline.
func (s *AcquisitionService) expire(ctx context.Context) {
	expired, err := s.store.ExpireStale(ctx, s.now().UTC(), acquisitionExpiryBatch)
	if err != nil {
		s.logger.WarnContext(ctx, "could not expire stale acquisition requests",
			slog.String("error", err.Error()))
		return
	}
	if expired > 0 {
		s.logger.InfoContext(ctx, "acquisition requests expired before they could start",
			slog.Int64("count", expired))
	}
}

// runRetention prunes old audit records.
func (s *AcquisitionService) runRetention(ctx context.Context) {
	if s.cfg.RetentionAge <= 0 {
		return
	}

	cutoff := s.now().UTC().Add(-s.cfg.RetentionAge)
	pruned, err := s.store.Prune(ctx, cutoff, acquisitionPruneBatch)
	if err != nil {
		s.logger.WarnContext(ctx, "acquisition retention pass failed",
			slog.String("error", err.Error()))
		return
	}
	if pruned > 0 {
		s.logger.InfoContext(ctx, "pruned old acquisition records",
			slog.Int64("removed", pruned),
			slog.Duration("maxAge", s.cfg.RetentionAge))
	}
}

const (
	// acquisitionExpiryBatch bounds one expiry transaction.
	acquisitionExpiryBatch = 100
	// acquisitionPruneBatch bounds one retention transaction, so a large
	// backlog cannot hold the single SQLite writer for an unbounded time.
	acquisitionPruneBatch = 200
)
