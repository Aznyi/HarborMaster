package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The rollback worker.
//
// # Restart recovery issues no Docker call at all
//
// A rollback left ACTIVE at startup was interrupted: this process has just
// begun and nothing else writes those rows. The recovery pass settles each one
// from its CHECKPOINT, records a manual recovery plan, and stops.
//
// It does not resume, and it does not undo. Both would be unattended mutations
// performed at exactly the moment HarborMaster does not know what is on the
// host, which is the situation this whole design exists to avoid entering.
//
// # The uncertain case is stated as uncertain
//
// A rollback in a mutating state whose checkpoint is still empty issued a stop
// that was never confirmed. HarborMaster does not know whether the replacement
// is stopped, and the recovery plan says exactly that rather than guessing in
// either direction.

// Worker bounds.
const (
	// rollbackRecoveryBatch bounds one restart recovery pass.
	rollbackRecoveryBatch = 100
	// rollbackExpiryBatch bounds one expiry pass.
	rollbackExpiryBatch = 200
	// rollbackPruneBatch bounds one retention pass, so a large backlog cannot
	// hold the single SQLite writer.
	rollbackPruneBatch = 500
)

// Run owns the rollback queue until ctx is cancelled.
//
// One goroutine, driven by a signal channel and a sweep ticker. The signal
// makes a freshly requested rollback start immediately; the ticker is the
// backstop for anything the signal missed, and it is also what runs expiry and
// retention.
func (s *RollbackService) Run(ctx context.Context) {
	if !s.Enabled() {
		// Stated once at startup, and stated distinctly from a
		// misconfiguration: a deployment that has not opted in is a supported
		// configuration, not a fault.
		//
		// Said at all because it used not to be. Acquisition and recreation
		// each announced their state and rollback announced nothing, so the
		// only way to tell whether the second capability that can stop a
		// running container was live was to call the API and read a 503.
		s.logger.Info("manual rollback disabled by configuration",
			slog.Bool("configured", s.cfg.Enabled),
			slog.Bool("capabilityWired", s.rollbacker != nil))

		// Waiting on the context rather than returning keeps the caller's
		// WaitGroup accounting uniform across every background service.
		<-ctx.Done()
		return
	}

	// Logged at WARN, matching recreation. This process can now stop the
	// container that is currently serving, which belongs at a level a default
	// log configuration shows.
	s.logger.Warn("manual rollback ENABLED: this HarborMaster can stop a serving container and start the one it replaced",
		slog.Int("maxConcurrent", s.cfg.MaxConcurrent),
		slog.Duration("startupTimeout", s.cfg.StartupTimeout),
		slog.Duration("stabilityPeriod", s.cfg.StabilityPeriod),
		slog.Duration("stopTimeout", s.cfg.StopTimeout))

	// Interrupted work is settled BEFORE anything new is claimed, so a queue
	// that was mid-flight at shutdown is reconciled before the process starts
	// changing the host again.
	s.recover(ctx)

	sweep := s.cfg.SweepInterval
	if sweep <= 0 {
		sweep = time.Minute
	}
	ticker := time.NewTicker(sweep)
	defer ticker.Stop()

	pruneEvery := s.cfg.PruneInterval
	if pruneEvery <= 0 {
		pruneEvery = time.Hour
	}
	pruneTicker := time.NewTicker(pruneEvery)
	defer pruneTicker.Stop()

	// Workers are tracked so Run does not return while one is still changing a
	// host. A shutdown that abandoned a rollback mid-rename would leave exactly
	// the arrangement this feature exists to avoid.
	var workers sync.WaitGroup
	defer workers.Wait()

	s.dispatch(ctx, &workers)

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			s.dispatch(ctx, &workers)
		case <-ticker.C:
			s.expire(ctx)
			s.dispatch(ctx, &workers)
		case <-pruneTicker.C:
			s.runRetention(ctx)
		}
	}
}

// recover settles rollbacks a restart interrupted.
func (s *RollbackService) recover(ctx context.Context) {
	interrupted, err := s.store.Interrupted(ctx, rollbackRecoveryBatch)
	if err != nil {
		s.logger.Warn("could not read interrupted rollbacks",
			slog.String("error", err.Error()))
		return
	}
	if len(interrupted) == 0 {
		return
	}

	var changedHost int
	for _, rollback := range interrupted {
		if s.recoverOne(ctx, rollback) {
			changedHost++
		}
	}

	// Two counts, because they mean different things to an operator. One says
	// how much was interrupted; the other says how much of it left containers
	// in an arrangement somebody has to settle.
	s.logger.Warn("rollbacks were interrupted by a restart and did not complete",
		slog.Int("count", len(interrupted)),
		slog.Int("hostChanged", changedHost))
}

// recoverOne settles a single interrupted rollback, reporting whether it had
// changed the host.
func (s *RollbackService) recoverOne(ctx context.Context, rollback domain.Rollback) bool {
	// The mutation was ATTEMPTED whenever the pipeline reached a mutating
	// state, whether or not any checkpoint confirmed it. That distinction is
	// the whole point: state says what was being attempted, checkpoint says
	// what is confirmed, and an attempt with no confirmation is the uncertain
	// case.
	attempted := rollback.State.Mutating()
	changedHost := rollback.Checkpoint.HostChanged() || attempted

	plan := domain.BuildRollbackRecoveryPlan(domain.RollbackRecoveryContext{
		RollbackID:            rollback.RollbackID,
		ExecutionID:           rollback.ExecutionID,
		ContainerName:         rollback.ContainerName,
		OriginalID:            rollback.OriginalID,
		ParkedName:            rollback.ParkedName,
		ReplacementID:         rollback.ReplacementID,
		ReplacementParkedName: rollback.ReplacementParkedName,
		Checkpoint:            rollback.Checkpoint,
		Failure:               domain.RollbackFailureInterrupted,
		MutationAttempted:     attempted,
	})

	message := domain.RollbackFailureInterrupted.Explain()
	if !changedHost {
		message = "HarborMaster restarted while this rollback was being checked; " +
			"nothing on this host was changed"
	}

	if _, err := s.store.Advance(ctx, store.RollbackChange{
		RollbackID: rollback.RollbackID,
		To:         domain.RollbackFailed,
		Failure:    domain.RollbackFailureInterrupted,
		Message:    message,
		Detail:     "settled by the restart recovery pass",
		Recovery:   plan,
	}, s.now().UTC()); err != nil {
		s.logger.Warn("could not settle an interrupted rollback",
			slog.String("rollbackId", rollback.RollbackID),
			slog.String("error", err.Error()))
		return changedHost
	}

	if changedHost {
		s.logger.Error("a rollback was interrupted after changing this host and needs attention",
			slog.String("rollbackId", rollback.RollbackID),
			slog.String("executionId", rollback.ExecutionID),
			slog.String("containerName", rollback.ContainerName),
			slog.String("checkpoint", string(rollback.Checkpoint)),
			slog.String("originalId", domain.ShortenID(rollback.OriginalID)),
			slog.String("replacementId", domain.ShortenID(rollback.ReplacementID)))
	}
	return changedHost
}

// dispatch starts as much queued work as the limits allow.
//
// Never blocks: a rollback that does not fit stays queued and is picked up by
// the next signal or sweep.
func (s *RollbackService) dispatch(ctx context.Context, workers *sync.WaitGroup) {
	for {
		if ctx.Err() != nil {
			return
		}

		// A whole batch is read but at most one is started per pass, so the
		// limit is re-evaluated between each.
		queued, err := s.store.Claimable(ctx, s.cfg.MaxConcurrent)
		if err != nil {
			s.logger.WarnContext(ctx, "could not read the rollback queue",
				slog.String("error", err.Error()))
			return
		}
		if len(queued) == 0 {
			return
		}

		started := false
		for _, rollback := range queued {
			if !s.reserve(rollback.ContainerName) {
				continue
			}

			workers.Add(1)
			go func(work domain.Rollback) {
				defer workers.Done()
				// The slot reserved above is released by execute's deferred
				// bookkeeping, whatever happens inside it.
				s.execute(ctx, work)
				// Another slot is free, so look again rather than waiting.
				s.signal()
			}(rollback)

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
// rollbacks of one container would each rename what the other just renamed, and
// neither would record what actually happened.
//
// Keyed by CONTAINER NAME rather than by container id, because a rollback
// contends for a name: two rollbacks of two different executions of the same
// container are exactly the collision worth preventing.
func (s *RollbackService) reserve(containerName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.inFlight >= s.cfg.MaxConcurrent {
		return false
	}
	if _, busy := s.containers[containerName]; busy {
		return false
	}

	s.inFlight++
	s.containers[containerName] = struct{}{}
	return true
}

// expire abandons requests that waited past their deadline.
//
// Queued only. An expired request never started, so abandoning it changes
// nothing on the host -- which is exactly why the repository restricts the
// statement to that one state.
func (s *RollbackService) expire(ctx context.Context) {
	expired, err := s.store.ExpireStale(ctx, s.now().UTC(), rollbackExpiryBatch)
	if err != nil {
		s.logger.WarnContext(ctx, "could not expire stale rollback requests",
			slog.String("error", err.Error()))
		return
	}
	if expired > 0 {
		s.logger.InfoContext(ctx, "rollback requests expired before they could start",
			slog.Int64("count", expired))
	}
}

// runRetention prunes old records.
func (s *RollbackService) runRetention(ctx context.Context) {
	if s.cfg.RetentionAge <= 0 {
		return
	}

	cutoff := s.now().UTC().Add(-s.cfg.RetentionAge)
	pruned, err := s.store.Prune(ctx, cutoff, rollbackPruneBatch)
	if err != nil {
		s.logger.WarnContext(ctx, "rollback retention pass failed",
			slog.String("error", err.Error()))
		return
	}
	if pruned > 0 {
		s.logger.InfoContext(ctx, "pruned old rollback records", slog.Int64("removed", pruned))
	}
}

// ------------------------------------------------------- audit attribution --

// auditOutcome records what a finished rollback did to the host.
//
// The same design as the recreation's, and for the same reason: the rollback
// record is an operational history an operator reads to understand one
// rollback, while the audit log is the security record that answers "what has
// been done to this host, and by whom".
//
// It reads the final state BACK from the store rather than trusting the caller
// to report what it did, is written on a detached bounded context so it lands
// even when the parent is cancelled, and can never fail the rollback.
func (s *RollbackService) auditOutcome(ctx context.Context, requested domain.Rollback) {
	if s.audit == nil {
		return
	}

	writeCtx, cancel := GraceContext(ctx, rollbackWriteGrace, rollbackWriteGrace)
	defer cancel()

	final, err := s.store.Get(writeCtx, requested.RollbackID)
	if err != nil {
		// The row could not be read back, so the outcome is genuinely unknown.
		// Recording "failed" would be a guess; recording nothing would lose the
		// event. The row says what is true: HarborMaster does not know.
		s.audit.RecordAction(writeCtx, requesterActor(requested.RequestedBy),
			domain.AuditRollbackFailed, domain.AuditFailed,
			domain.AuditTargetRollback, requested.RollbackID, requested.ContainerName,
			"the rollback finished but its record could not be read back")
		return
	}
	if !final.State.Terminal() {
		// Abandoned by a shutdown. The restart recovery pass settles it.
		return
	}

	action := domain.AuditRollbackFailed
	outcome := domain.AuditFailed
	if final.State == domain.RollbackSucceeded {
		action = domain.AuditRollbackCompleted
		outcome = domain.AuditSucceeded
	}

	s.audit.RecordAction(writeCtx, requesterActor(final.RequestedBy),
		action, outcome, domain.AuditTargetRollback,
		final.RollbackID, final.ContainerName, rollbackOutcomeReason(final))
}

// rollbackOutcomeReason renders the conclusion in HarborMaster's own words.
//
// Never the failure MESSAGE, which is built for an operator reading the
// rollback page. This is a fixed sentence plus a closed-vocabulary failure
// name, because the value reaches a page an administrator reads and a column
// that must stay bounded.
//
// The most important thing it says is whether the host was left changed and
// whether anything is serving. That is what decides whether somebody has to act
// now or merely tidy up later.
func rollbackOutcomeReason(final domain.Rollback) string {
	switch {
	case final.State == domain.RollbackSucceeded:
		return "the original container is running under its own name again; " +
			"the replacement is stopped and kept as evidence"
	case final.Checkpoint.HostChanged():
		return "the rollback failed after changing this host and needs attention (" +
			string(final.Failure) + ")"
	case final.State == domain.RollbackCancelled:
		return "cancelled before anything on this host was changed"
	case final.State == domain.RollbackExpired:
		return "expired in the queue; nothing on this host was changed"
	default:
		return "refused or failed before anything on this host was changed (" +
			string(final.Failure) + ")"
	}
}
