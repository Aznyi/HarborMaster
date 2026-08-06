package service

import (
	"context"
	"log/slog"
	"sync"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The recreation worker.
//
// # What it does not do
//
// It does not decide to recreate anything. Every recreation it runs was
// requested by an operator and already recorded; the worker's job is to give
// queued work a slot, within bounds.
//
// **There is no scheduled, automatic, or retried recreation.** No timer here
// creates work. The periodic ticks only EXPIRE requests that waited too long --
// which changes nothing on the host, because an expired request never started
// -- and PRUNE old audit records. A HarborMaster left running with nobody
// asking it for anything replaces no container, forever.
//
// # Recovery reads checkpoints, not states
//
// See recover below. This is the part of the feature that most repays reading
// twice: after a crash, the question is not "what was HarborMaster doing" but
// "what is true of this host", and only the checkpoint answers that.

// Run drives recreation until ctx is cancelled.
func (s *ExecutionService) Run(ctx context.Context) {
	if !s.Enabled() {
		// Stated once at startup, and stated distinctly from a
		// misconfiguration: a deployment that has not opted in is a supported
		// configuration, not a fault.
		s.logger.Info("container recreation disabled by configuration",
			slog.Bool("configured", s.cfg.Enabled),
			slog.Bool("capabilityWired", s.mutator != nil && s.capturer != nil))
		return
	}

	// Logged at WARN. This process can now stop and replace running containers,
	// and that belongs at a level a default log configuration shows.
	s.logger.Warn("container recreation ENABLED: this HarborMaster can stop and replace running containers",
		slog.Int("maxConcurrent", s.cfg.MaxConcurrent),
		slog.Duration("startupTimeout", s.cfg.StartupTimeout),
		slog.Duration("stabilityPeriod", s.cfg.StabilityPeriod),
		slog.Duration("stopTimeout", s.cfg.StopTimeout),
		slog.Bool("requireSnapshot", s.cfg.RequireSnapshot))

	// Anything left mid-flight by a crash is settled BEFORE the queue is
	// touched. An execution in `creating` is a claim about a process that no
	// longer exists, and it may have left two containers on the host.
	s.recover(ctx)

	sweep := newOptionalTicker(s.cfg.SweepInterval)
	defer sweep.Stop()

	prune := newOptionalTicker(s.cfg.PruneInterval)
	defer prune.Stop()

	// workers tracks dispatched recreations so shutdown can wait for them.
	var workers sync.WaitGroup
	defer workers.Wait()

	// Whatever was already queued is dispatched immediately. A request accepted
	// just before a restart would otherwise sit until the first sweep.
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

// recover settles recreations interrupted by a restart.
//
// # Why this is not one UPDATE
//
// The acquisition equivalent is a single blanket statement, and that is right
// there: an interrupted pull leaves an image in the store and nothing else, so
// every interrupted row deserves the same treatment.
//
// A recreation is not like that. An interrupted one may have changed nothing,
// or may have stopped a container, parked it, created a replacement, and
// started it -- and the record, the recovery plan, and the urgency all differ.
// So each row is settled from its own CHECKPOINT.
//
// # Nothing is resumed, and nothing is undone
//
// Not one Docker call is made here. Resuming would mean continuing a mutation
// sequence whose last step nobody watched; undoing would mean mutating on the
// strength of the same uncertainty. Both are refused in favour of recording
// precisely what is known and handing an operator a plan.
//
// # The uncertain case is stated as uncertain
//
// An execution in a mutating state whose checkpoint is still empty issued a
// stop that was never confirmed. HarborMaster does not know whether the
// container is stopped, and the recovery plan says exactly that rather than
// guessing in either direction.
func (s *ExecutionService) recover(ctx context.Context) {
	interrupted, err := s.store.Interrupted(ctx, executionRecoveryBatch)
	if err != nil {
		s.logger.Warn("could not read interrupted recreations",
			slog.String("error", err.Error()))
		return
	}
	if len(interrupted) == 0 {
		return
	}

	var changedHost int
	for _, execution := range interrupted {
		if s.recoverOne(ctx, execution) {
			changedHost++
		}
	}

	// Two counts, because they mean different things to an operator. One says
	// how much was interrupted; the other says how much of it left containers
	// behind.
	s.logger.Warn("recreations were interrupted by a restart and did not complete",
		slog.Int("count", len(interrupted)),
		slog.Int("hostChanged", changedHost))
}

// recoverOne settles a single interrupted recreation, reporting whether it had
// changed the host.
func (s *ExecutionService) recoverOne(ctx context.Context, execution domain.Execution) bool {
	// The mutation was ATTEMPTED whenever the pipeline reached a mutating
	// state, whether or not any checkpoint landed. That distinction is the
	// whole point: state says what was being attempted, checkpoint says what is
	// confirmed, and an attempt with no confirmation is the uncertain case.
	attempted := execution.State.Mutating()
	changedHost := execution.Checkpoint.HostChanged() || attempted

	plan := domain.BuildRecoveryPlan(domain.RecoveryContext{
		ExecutionID:       execution.ExecutionID,
		ContainerName:     execution.ContainerName,
		OriginalID:        execution.ContainerID,
		ParkedName:        execution.ParkedName,
		ReplacementID:     execution.ReplacementID,
		QuarantineName:    execution.QuarantineName,
		Checkpoint:        execution.Checkpoint,
		Failure:           domain.ExecutionFailureInterrupted,
		MutationAttempted: attempted,
	})

	message := domain.ExecutionFailureInterrupted.Explain()
	if !changedHost {
		message = "HarborMaster restarted while this recreation was being checked; " +
			"nothing on this host was changed"
	}

	if _, err := s.store.Advance(ctx, store.ExecutionChange{
		ExecutionID: execution.ExecutionID,
		To:          domain.ExecutionFailed,
		Failure:     domain.ExecutionFailureInterrupted,
		Message:     message,
		Detail:      "settled by the restart recovery pass",
		Recovery:    plan,
	}, s.now().UTC()); err != nil {
		s.logger.Warn("could not settle an interrupted recreation",
			slog.String("executionId", execution.ExecutionID),
			slog.String("error", err.Error()))
		return changedHost
	}

	if changedHost {
		s.logger.Error("a recreation was interrupted after changing this host and needs attention",
			slog.String("executionId", execution.ExecutionID),
			slog.String("containerName", execution.ContainerName),
			slog.String("checkpoint", string(execution.Checkpoint)),
			slog.String("parkedName", execution.ParkedName),
			slog.String("replacementId", domain.ShortenID(execution.ReplacementID)))
	}
	return changedHost
}

// dispatch starts as much queued work as the limits allow.
//
// Never blocks: a recreation that does not fit stays queued and is picked up by
// the next signal or sweep.
func (s *ExecutionService) dispatch(ctx context.Context, workers *sync.WaitGroup) {
	for {
		if ctx.Err() != nil {
			return
		}

		// A whole batch is read but at most one is started per pass, so the
		// limit is re-evaluated between each.
		queued, err := s.store.Claimable(ctx, s.cfg.MaxConcurrent)
		if err != nil {
			s.logger.WarnContext(ctx, "could not read the recreation queue",
				slog.String("error", err.Error()))
			return
		}
		if len(queued) == 0 {
			return
		}

		started := false
		for _, execution := range queued {
			if !s.reserve(execution.ContainerID) {
				continue
			}

			workers.Add(1)
			go func(work domain.Execution) {
				defer workers.Done()
				// The slot reserved above is released by execute's deferred
				// bookkeeping, whatever happens inside it.
				s.execute(ctx, work)
				// Another slot is free, so look again rather than waiting.
				s.signal()
			}(execution)

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
// The database index is authoritative across restarts, but it cannot stop two
// dispatch passes in one process both seeing a free slot: the row is not marked
// until execute claims it, and between the read and that claim the count is
// stale. These counters close that window.
//
// The per-container check is the one that matters most. Two simultaneous
// recreations of one container would both stop it and both try to take its
// name, leaving the host in a state neither of them recorded.
func (s *ExecutionService) reserve(containerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.inFlight >= s.cfg.MaxConcurrent {
		return false
	}
	if _, busy := s.containers[containerID]; busy {
		return false
	}

	s.inFlight++
	s.containers[containerID] = struct{}{}
	return true
}

// expire abandons requests that waited past their deadline.
//
// Queued only. An expired request never started, so abandoning it changes
// nothing on the host -- which is exactly why the repository restricts the
// statement to that one state.
func (s *ExecutionService) expire(ctx context.Context) {
	expired, err := s.store.ExpireStale(ctx, s.now().UTC(), executionExpiryBatch)
	if err != nil {
		s.logger.WarnContext(ctx, "could not expire stale recreation requests",
			slog.String("error", err.Error()))
		return
	}
	if expired > 0 {
		s.logger.InfoContext(ctx, "recreation requests expired before they could start",
			slog.Int64("count", expired))
	}
}

// runRetention prunes old audit records.
func (s *ExecutionService) runRetention(ctx context.Context) {
	if s.cfg.RetentionAge <= 0 {
		return
	}

	cutoff := s.now().UTC().Add(-s.cfg.RetentionAge)
	pruned, err := s.store.Prune(ctx, cutoff, executionPruneBatch)
	if err != nil {
		s.logger.WarnContext(ctx, "recreation retention pass failed",
			slog.String("error", err.Error()))
		return
	}
	if pruned > 0 {
		s.logger.InfoContext(ctx, "pruned old recreation records",
			slog.Int64("removed", pruned),
			slog.Duration("maxAge", s.cfg.RetentionAge))
	}
}

const (
	// executionRecoveryBatch bounds one startup recovery pass, so a database
	// left by a pathological crash cannot hold startup open.
	executionRecoveryBatch = 100
	// executionExpiryBatch bounds one expiry transaction.
	executionExpiryBatch = 100
	// executionPruneBatch bounds one retention transaction, so a large backlog
	// cannot hold the single SQLite writer for an unbounded time.
	executionPruneBatch = 200
)
