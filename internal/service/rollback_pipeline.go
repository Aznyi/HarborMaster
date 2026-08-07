package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The rollback pipeline.
//
// # Where the point of no return is
//
//	queued → validating │ stoppingReplacement → restoringName → startingOriginal
//	└── changes nothing ┘ └──────── the host is being changed ────────┘
//	    freely cancellable          cancellation refused
//
// The transition into stoppingReplacement is THE MUTATION POINT. Before it, an
// operator can cancel and nothing has happened. After it, the rollback must
// reach a recorded conclusion: a container an operator depends on has been
// stopped, and abandoning the operation would leave it neither running nor
// recorded as down.
//
// # A checkpoint after every mutation, before the next
//
// Four mutations, four checkpoints, in strict alternation. The checkpoint says
// what is TRUE of the host; the state says what HarborMaster was doing. After a
// crash only the first matters.
//
// If a checkpoint write FAILS, the pipeline stops. It does not retry the
// mutation. Repeating a stop, a rename, or a start against a host whose
// recorded state is uncertain is exactly how a recoverable situation becomes an
// unrecoverable one.
//
// # It never removes anything
//
// The replacement is stopped and parked, and stays on the host. It is the
// evidence of why the recreation was backed out, and the rollback capability
// has no remove method to destroy it with.

// Bounds on the mutating half.
const (
	// rollbackWriteGrace bounds one detached record write. Every terminal write
	// and every checkpoint gets its own budget, so a failure to RECORD cannot
	// be caused by the mutation budget expiring.
	rollbackWriteGrace = 10 * time.Second
	// rollbackShutdownGrace is how long an in-flight rollback may keep running
	// after shutdown begins. Under the server's own shutdown timeout, so a
	// rollback cannot be the reason a shutdown is not graceful.
	rollbackShutdownGrace = 10 * time.Second
	// rollbackMutationMargin is added to the computed budget for the round
	// trips the timeouts do not cover.
	rollbackMutationMargin = 60 * time.Second
)

// rollbackWork is one rollback in flight.
type rollbackWork struct {
	rollback domain.Rollback
	decision rollbackDecision

	// checkpoint mirrors the last durably recorded checkpoint, so the terminal
	// paths can build a recovery plan without re-reading.
	checkpoint domain.RollbackCheckpoint
	// replacementParkedName is the derived name, once the rename has landed.
	replacementParkedName string

	verification domain.RollbackVerification
}

// execute runs one rollback from claim to conclusion.
func (s *RollbackService) execute(ctx context.Context, rollback domain.Rollback) {
	id := rollback.RollbackID
	work := &rollbackWork{
		rollback:     rollback,
		verification: newRollbackVerification(),
	}

	// The pre-mutation context. Cancellable by an operator, and registered so
	// Cancel can reach it.
	preCtx, cancelPre := context.WithCancel(ctx)
	defer cancelPre()

	s.mu.Lock()
	s.cancels[id] = cancelPre
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.cancels, id)
		delete(s.containers, rollback.ContainerName)
		s.inFlight--
		s.mu.Unlock()

		// The OUTCOME reaches the security audit log from exactly one place.
		// See ExecutionService.reportOutcome for why a deferred read-back beats
		// an audit call on each terminal path.
		s.reportOutcome(ctx, rollback)
	}()

	// ---- claim -------------------------------------------------------------

	claimed, err := s.store.Advance(preCtx, store.RollbackChange{
		RollbackID:  id,
		From:        []domain.RollbackState{domain.RollbackQueued},
		To:          domain.RollbackValidating,
		Detail:      "rechecking both container identities against the live host",
		MarkStarted: true,
	}, s.now().UTC())
	if err != nil {
		s.logger.WarnContext(ctx, "could not claim rollback",
			slog.String("rollbackId", id), slog.String("error", err.Error()))
		return
	}
	if !claimed {
		// Cancelled or expired between being listed and being claimed. Another
		// path owns it now.
		return
	}

	// ---- revalidate --------------------------------------------------------
	//
	// The whole preflight, again, against the live host. Minutes may have
	// passed since the request was accepted, and a person may have been moving
	// containers by hand in that time.
	decision, refusal, assessErr := s.assess(preCtx, rollback.ExecutionID, id, s.now().UTC())
	if assessErr != nil {
		s.failBeforeMutation(ctx, work, domain.RollbackFailurePreflight,
			domain.RollbackFailurePreflight.Explain())
		return
	}
	if refusal != domain.RollbackRefusalNone {
		s.refuse(ctx, work, refusal)
		return
	}

	// The recorded identities must still be the ones the fresh assessment
	// derived. A divergence means the execution record changed under us, which
	// is not a thing to roll back through.
	if decision.OriginalID != rollback.OriginalID ||
		decision.ReplacementID != rollback.ReplacementID ||
		decision.ContainerName != rollback.ContainerName {
		s.refuse(ctx, work, domain.RollbackRefusalOriginalIdentity)
		return
	}
	work.decision = decision

	// The parked name for the replacement, derived from HarborMaster's own
	// values. Checked before the mutation point: a name that cannot be derived
	// is a rollback that would strand the replacement holding the production
	// name.
	work.replacementParkedName = domain.RollbackParkedName(decision.ContainerName, id)
	if work.replacementParkedName == "" {
		s.refuse(ctx, work, domain.RollbackRefusalNameUnavailable)
		return
	}

	// A last cancellation check on the very edge of the mutation point. An
	// operator who pressed cancel while validation was running gets what they
	// asked for rather than a container that was stopped a moment later.
	if preCtx.Err() != nil {
		return
	}

	// ======================= THE MUTATION POINT ===========================
	//
	// From here the operator's cancel function is unregistered and the pipeline
	// runs on a context derived from the WORKER's, not the operator's.
	s.mu.Lock()
	delete(s.cancels, id)
	s.mu.Unlock()

	mutateCtx, cancelMutate := GraceContext(ctx, rollbackShutdownGrace, s.mutationBudget())
	defer cancelMutate()

	s.mutate(mutateCtx, ctx, work)
}

// mutationBudget bounds the whole mutating half.
//
// Computed from the configured timeouts rather than fixed, so a deployment that
// allows a ten-minute startup does not have its rollbacks cut off at five.
func (s *RollbackService) mutationBudget() time.Duration {
	return s.cfg.StopTimeout + s.cfg.StartupTimeout + s.cfg.StabilityPeriod + rollbackMutationMargin
}

// mutate runs the half of the pipeline that changes the host.
//
// parent is the worker's context, used only for the detached terminal writes:
// the record of what happened must be written even when the mutation context
// has expired, because a failure with no record is the one outcome worse than
// the failure itself.
func (s *RollbackService) mutate(ctx, parent context.Context, work *rollbackWork) {
	id := work.rollback.RollbackID

	moved, err := s.store.Advance(ctx, store.RollbackChange{
		RollbackID: id,
		From:       []domain.RollbackState{domain.RollbackValidating},
		To:         domain.RollbackStoppingReplacement,
		Detail:     "stopping the replacement container",
	}, s.now().UTC())
	if err != nil || !moved {
		// Could not even record the intent. Nothing has been changed, so
		// stopping here is free -- and it is the only safe option, because a
		// mutation whose intent went unrecorded is one recovery cannot reason
		// about.
		s.failBeforeMutation(parent, work, domain.RollbackFailurePersistence,
			domain.RollbackFailurePersistence.Explain())
		return
	}

	// ---- 1. stop the replacement -------------------------------------------

	if s.shuttingDown(parent, work) {
		return
	}
	if err := s.rollbacker.StopReplacement(ctx, docker.RollbackStopRequest{
		ReplacementID: work.decision.ReplacementID,
		Timeout:       s.cfg.StopTimeout,
	}); err != nil {
		s.failAfterMutation(parent, work, s.classify(err, domain.RollbackFailureStop),
			domain.RollbackFailureStop.Explain())
		return
	}
	if !s.checkpoint(ctx, parent, work, store.RollbackCheckpointWrite{
		RollbackID:  id,
		Checkpoint:  domain.RollbackCheckpointReplacementStopped,
		Detail:      "the replacement container is stopped",
		MarkMutated: true,
	}) {
		return
	}

	// ---- 2. park the replacement -------------------------------------------

	if s.shuttingDown(parent, work) {
		return
	}
	moved, err = s.store.Advance(ctx, store.RollbackChange{
		RollbackID: id,
		From:       []domain.RollbackState{domain.RollbackStoppingReplacement},
		To:         domain.RollbackRestoringName,
		Detail:     "moving the replacement aside so the original can take its name back",
	}, s.now().UTC())
	if err != nil || !moved {
		s.failAfterMutation(parent, work, domain.RollbackFailurePersistence,
			domain.RollbackFailurePersistence.Explain())
		return
	}

	if err := s.rollbacker.ParkReplacement(ctx, docker.RollbackParkRequest{
		ReplacementID: work.decision.ReplacementID,
		ParkedName:    work.replacementParkedName,
	}); err != nil {
		s.failAfterMutation(parent, work, s.classify(err, domain.RollbackFailureRename),
			domain.RollbackFailureRename.Explain())
		return
	}
	if !s.checkpoint(ctx, parent, work, store.RollbackCheckpointWrite{
		RollbackID:            id,
		Checkpoint:            domain.RollbackCheckpointReplacementParked,
		Detail:                "the replacement container was renamed aside",
		ReplacementParkedName: work.replacementParkedName,
	}) {
		return
	}

	// ---- 3. restore the original's name ------------------------------------

	if s.shuttingDown(parent, work) {
		return
	}
	if err := s.rollbacker.RestoreOriginalName(ctx, docker.RollbackRestoreRequest{
		OriginalID: work.decision.OriginalID,
		Name:       work.decision.ContainerName,
	}); err != nil {
		s.failAfterMutation(parent, work, s.classify(err, domain.RollbackFailureRename),
			domain.RollbackFailureRename.Explain())
		return
	}
	if !s.checkpoint(ctx, parent, work, store.RollbackCheckpointWrite{
		RollbackID: id,
		Checkpoint: domain.RollbackCheckpointOriginalRestored,
		Detail:     "the original container carries its own name again",
	}) {
		return
	}

	// ---- 4. start the original ---------------------------------------------

	if s.shuttingDown(parent, work) {
		return
	}
	moved, err = s.store.Advance(ctx, store.RollbackChange{
		RollbackID: id,
		From:       []domain.RollbackState{domain.RollbackRestoringName},
		To:         domain.RollbackStartingOriginal,
		Detail:     "starting the original container",
	}, s.now().UTC())
	if err != nil || !moved {
		s.failAfterMutation(parent, work, domain.RollbackFailurePersistence,
			domain.RollbackFailurePersistence.Explain())
		return
	}

	if err := s.rollbacker.StartOriginal(ctx, docker.RollbackStartRequest{
		OriginalID: work.decision.OriginalID,
	}); err != nil {
		s.failAfterMutation(parent, work, s.classify(err, domain.RollbackFailureStart),
			domain.RollbackFailureStart.Explain())
		return
	}
	if !s.checkpoint(ctx, parent, work, store.RollbackCheckpointWrite{
		RollbackID: id,
		Checkpoint: domain.RollbackCheckpointOriginalStarted,
		Detail:     "the original container is running and not yet proved",
	}) {
		return
	}

	// ---- 5. prove it -------------------------------------------------------

	moved, err = s.store.Advance(ctx, store.RollbackChange{
		RollbackID: id,
		From:       []domain.RollbackState{domain.RollbackStartingOriginal},
		To:         domain.RollbackVerifyingOriginal,
		Detail:     "proving the restored original",
	}, s.now().UTC())
	if err != nil || !moved {
		s.failAfterMutation(parent, work, domain.RollbackFailurePersistence,
			domain.RollbackFailurePersistence.Explain())
		return
	}

	if failure := s.verify(ctx, parent, work); failure != domain.RollbackFailureNone {
		s.failAfterMutation(parent, work, failure, failure.Explain())
		return
	}

	if !s.checkpoint(ctx, parent, work, store.RollbackCheckpointWrite{
		RollbackID: id,
		Checkpoint: domain.RollbackCheckpointOriginalVerified,
		Detail:     "the original container passed every verification",
	}) {
		return
	}

	s.succeed(ctx, parent, work)
}

// succeed records the conclusion.
//
// # There is nothing to clean up, and that is deliberate
//
// A recreation's success ends by REMOVING the parked original. A rollback's
// success ends by writing a record and stopping: the parked replacement stays
// on the host as the evidence of why the recreation was backed out, and the
// rollback capability has no remove method to destroy it with.
//
// So the last act is a durable write, and a failure to write it is a failure of
// the rollback -- the containers are where they should be, but HarborMaster
// cannot prove it recorded that, and acting as though it had is how an
// uncertain record becomes a wrong one.
func (s *RollbackService) succeed(ctx, parent context.Context, work *rollbackWork) {
	id := work.rollback.RollbackID

	recorded, err := s.store.Advance(ctx, store.RollbackChange{
		RollbackID:   id,
		From:         []domain.RollbackState{domain.RollbackVerifyingOriginal},
		To:           domain.RollbackSucceeded,
		Checkpoint:   domain.RollbackCheckpointOriginalVerified,
		Detail:       "the rollback is complete",
		Message:      "the original container is running under its own name and passed every verification",
		Verification: &work.verification,
	}, s.now().UTC())
	if err != nil || !recorded {
		s.failAfterMutation(parent, work, domain.RollbackFailurePersistence,
			domain.RollbackFailurePersistence.Explain())
		return
	}

	s.logger.InfoContext(ctx, "rollback complete",
		slog.String("rollbackId", id),
		slog.String("executionId", work.rollback.ExecutionID),
		slog.String("containerName", work.decision.ContainerName),
		slog.String("parkedReplacement", work.replacementParkedName))
}

// ---------------------------------------------------------------- failing --

// refuse records a preflight refusal.
//
// A refusal always happens BEFORE the mutation point, so it is recorded with
// the specific check that said no and with the plain statement that nothing on
// the host was changed.
func (s *RollbackService) refuse(
	ctx context.Context,
	work *rollbackWork,
	refusal domain.RollbackRefusal,
) {
	writeCtx, cancel := GraceContext(ctx, rollbackWriteGrace, rollbackWriteGrace)
	defer cancel()

	if _, err := s.store.Advance(writeCtx, store.RollbackChange{
		RollbackID: work.rollback.RollbackID,
		To:         domain.RollbackFailed,
		Failure:    domain.RollbackFailurePreflight,
		Refusal:    refusal,
		Message:    refusal.Explain(),
		Detail:     "refused before anything on this host was changed",
		Recovery:   domain.BuildRollbackRecoveryPlan(s.recoveryContext(work)),
	}, s.now().UTC()); err != nil {
		s.logger.WarnContext(ctx, "could not record rollback refusal",
			slog.String("rollbackId", work.rollback.RollbackID),
			slog.String("error", err.Error()))
	}

	s.logRefusal(ctx, work.rollback.RollbackID, refusal)
}

// failBeforeMutation records a failure that changed nothing.
func (s *RollbackService) failBeforeMutation(
	ctx context.Context,
	work *rollbackWork,
	failure domain.RollbackFailure,
	message string,
) {
	writeCtx, cancel := GraceContext(ctx, rollbackWriteGrace, rollbackWriteGrace)
	defer cancel()

	if _, err := s.store.Advance(writeCtx, store.RollbackChange{
		RollbackID:   work.rollback.RollbackID,
		To:           domain.RollbackFailed,
		Failure:      failure,
		Message:      message,
		Detail:       "failed before anything on this host was changed",
		Verification: &work.verification,
		Recovery:     domain.BuildRollbackRecoveryPlan(s.recoveryContext(work)),
	}, s.now().UTC()); err != nil {
		s.logger.WarnContext(ctx, "could not record rollback failure",
			slog.String("rollbackId", work.rollback.RollbackID),
			slog.String("error", err.Error()))
	}
}

// failAfterMutation records a failure that left containers on the host.
//
// The record carries the checkpoint, both identities, and a recovery plan. It
// is written on the PARENT context with its own budget: the mutation context
// may already have expired, and a failure with no record is worse than the
// failure.
//
// It does NOT try to put anything back. HarborMaster has just demonstrated its
// model of the host is wrong, and an automatic correction at that moment is the
// unattended mutation this whole design exists to avoid.
func (s *RollbackService) failAfterMutation(
	ctx context.Context,
	work *rollbackWork,
	failure domain.RollbackFailure,
	message string,
) {
	writeCtx, cancel := GraceContext(ctx, rollbackWriteGrace, rollbackWriteGrace)
	defer cancel()

	plan := domain.BuildRollbackRecoveryPlan(s.recoveryContext(work))

	if _, err := s.store.Advance(writeCtx, store.RollbackChange{
		RollbackID:            work.rollback.RollbackID,
		To:                    domain.RollbackFailed,
		Failure:               failure,
		Message:               message,
		Detail:                "failed after changing this host; both containers are preserved",
		ReplacementParkedName: work.replacementParkedName,
		Verification:          &work.verification,
		Recovery:              plan,
	}, s.now().UTC()); err != nil {
		s.logger.ErrorContext(ctx, "could not record a rollback failure that changed the host",
			slog.String("rollbackId", work.rollback.RollbackID),
			slog.String("error", err.Error()))
	}

	// ERROR rather than WARN. Containers were left in an arrangement a person
	// has to settle, and this line is what an operator watching logs sees.
	s.logger.ErrorContext(ctx, "rollback failed after changing this host",
		slog.String("rollbackId", work.rollback.RollbackID),
		slog.String("executionId", work.rollback.ExecutionID),
		slog.String("containerName", work.rollback.ContainerName),
		slog.String("failure", string(failure)),
		slog.String("checkpoint", string(work.checkpoint)),
		slog.String("originalId", domain.ShortenID(work.rollback.OriginalID)),
		slog.String("replacementId", domain.ShortenID(work.rollback.ReplacementID)))
}

// ----------------------------------------------------------- checkpointing --

// checkpoint records that one mutation completed, and reports whether to
// continue.
//
// Returns false when the write failed, which STOPS the pipeline. HarborMaster
// does not retry the mutation: repeating a stop, a rename, or a start against a
// host whose recorded state is uncertain is how a recoverable situation becomes
// an unrecoverable one.
func (s *RollbackService) checkpoint(
	ctx, parent context.Context,
	work *rollbackWork,
	write store.RollbackCheckpointWrite,
) bool {
	// Deliberately NOT the mutation context. A checkpoint whose write is
	// cancelled because the mutation budget expired is the exact case this
	// design exists to avoid: the host was changed and the change went
	// unrecorded. It gets its own bounded, detached budget.
	writeCtx, cancel := GraceContext(parent, rollbackWriteGrace, rollbackWriteGrace)
	defer cancel()

	recorded, err := s.store.Checkpoint(writeCtx, write, s.now().UTC())
	if err != nil {
		// Logged at ERROR. HarborMaster has changed a host and cannot prove it
		// recorded the fact, which is the most serious condition this feature
		// can produce.
		s.logger.ErrorContext(ctx, "could not record a rollback checkpoint; stopping rather than acting again",
			slog.String("rollbackId", work.rollback.RollbackID),
			slog.String("checkpoint", string(write.Checkpoint)),
			slog.String("error", err.Error()))

		s.failAfterMutation(parent, work, domain.RollbackFailurePersistence,
			domain.RollbackFailurePersistence.Explain())
		return false
	}
	if !recorded {
		// The row is no longer active: something else settled it. Stop rather
		// than keep mutating a host on behalf of a record somebody else owns.
		s.logger.WarnContext(ctx, "rollback was settled by another path mid-flight",
			slog.String("rollbackId", work.rollback.RollbackID),
			slog.String("checkpoint", string(write.Checkpoint)))
		return false
	}

	work.checkpoint = write.Checkpoint
	if write.ReplacementParkedName != "" {
		work.replacementParkedName = write.ReplacementParkedName
	}
	return true
}

// shuttingDown reports that the process is stopping, and records the fact.
//
// Checked at every step boundary of the mutating half, which is what makes
// shutdown fast in the common case: the mutation context carries a grace period
// so a Docker call already in flight can finish, but there is no reason to
// START another one when the process is on its way out.
//
// The record is written with the checkpoint intact, so the restart recovery
// pass reads a settled row rather than an active one.
func (s *RollbackService) shuttingDown(parent context.Context, work *rollbackWork) bool {
	if parent.Err() == nil {
		return false
	}

	// The PARENT, cancelled though it is. failAfterMutation derives a detached
	// grace context from it, so the record still lands; passing a fresh
	// background context would drop the last link to the work being abandoned
	// for no benefit.
	s.failAfterMutation(parent, work, domain.RollbackFailureInterrupted,
		domain.RollbackFailureInterrupted.Explain())
	return true
}

// classify maps a Docker adapter error onto a failure classification.
//
// The daemon becoming unreachable is told apart from the operation failing,
// because they call for different things from an operator. The error's TEXT is
// never used: it can carry the socket path, a command line, and internal state.
func (s *RollbackService) classify(
	err error,
	fallback domain.RollbackFailure,
) domain.RollbackFailure {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return domain.RollbackFailureTimeout
	case errors.Is(err, context.Canceled):
		// Only reachable past the mutation point, where the only cancellation
		// is shutdown. Recorded as interrupted, which is what the restart
		// recovery pass will also call it.
		return domain.RollbackFailureInterrupted
	case errors.Is(err, docker.ErrUnreachable):
		return domain.RollbackFailureDockerUnavailable
	}
	return fallback
}

// recoveryContext builds the input to a recovery plan.
func (s *RollbackService) recoveryContext(work *rollbackWork) domain.RollbackRecoveryContext {
	return domain.RollbackRecoveryContext{
		RollbackID:            work.rollback.RollbackID,
		ExecutionID:           work.rollback.ExecutionID,
		ContainerName:         work.rollback.ContainerName,
		OriginalID:            work.rollback.OriginalID,
		ParkedName:            work.rollback.ParkedName,
		ReplacementID:         work.rollback.ReplacementID,
		ReplacementParkedName: work.replacementParkedName,
		Checkpoint:            work.checkpoint,
		Failure:               domain.RollbackFailureNone,
		MutationAttempted:     work.checkpoint.HostChanged(),
	}
}

// newRollbackVerification returns a verification with every proof unknown.
//
// Unknown rather than zero-valued, because the zero value of a string type is
// the empty string and an empty verdict rendered in a UI reads as "nothing to
// report" rather than "never checked".
func newRollbackVerification() domain.RollbackVerification {
	return domain.RollbackVerification{
		Health:       domain.VerificationUnknown,
		Image:        domain.VerificationUnknown,
		Preservation: domain.VerificationUnknown,
		Network:      domain.VerificationUnknown,
	}
}
