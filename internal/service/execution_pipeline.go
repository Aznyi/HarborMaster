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

// The recreation pipeline.
//
// # The shape, and why it is this shape
//
//	 1. claim         queued -> validating
//	 2. revalidate    the full preflight, again
//	 3. capture       validating -> capturing, read the live configuration
//	 4. names         derive and re-check the parked and quarantine names
//	 ---------------- THE MUTATION POINT -------------------------------------
//	 5. stop          the original                     -> checkpoint
//	 6. park          rename the original aside        -> checkpoint
//	 7. create        the replacement, under the name  -> checkpoint
//	 8. start         the replacement                  -> checkpoint
//	 9. prove         health, image, preservation, network
//	10. record        the success, durably             -> checkpoint
//	11. remove        the parked original              -> checkpoint
//
// Steps 1 to 4 change nothing and are freely cancellable. Steps 5 onward are
// not cancellable at all: a recreation that has stopped a container must reach
// a RECORDED conclusion, because an abandoned one leaves a host in a state
// nobody chose and nobody wrote down.
//
// # Every mutation is followed by a checkpoint, and a failed checkpoint stops
// everything
//
// A checkpoint says what is TRUE OF THE HOST, and it is written before the next
// mutation is attempted. If one cannot be written, HarborMaster no longer knows
// whether its own last action is recorded -- so it stops. It does not retry the
// mutation, because repeating a stop, a rename, or a remove against a host
// whose recorded state is uncertain is how a recoverable situation becomes an
// unrecoverable one.
//
// # The original outlives every intermediate failure
//
// It is parked, not removed, and it is removed only after all four proofs pass
// AND the success is durably recorded. Any failure before that leaves both
// containers on the host with a manual recovery plan attached.

// Pipeline bounds.
const (
	// executionShutdownGrace is how long an in-flight mutation may run past
	// shutdown.
	//
	// Short on purpose, and deliberately under service.DefaultShutdownGrace so
	// this feature cannot be the reason a shutdown overruns. It is not enough
	// to finish a whole recreation and is not meant to be: it covers the Docker
	// call already in flight and its checkpoint, after which the restart
	// recovery pass reads that checkpoint and reports honestly.
	//
	// The pipeline also checks for shutdown BETWEEN steps (see shuttingDown),
	// so in the common case it stops at the next boundary rather than using any
	// of this grace at all. Trying to finish the whole pipeline would mean
	// holding the process open past the orchestrator's deadline, which does not
	// buy time -- it buys a SIGKILL at an arbitrary point instead of a clean
	// stop at a known one.
	executionShutdownGrace = 10 * time.Second

	// executionWriteGrace bounds a detached terminal write. The failure that
	// brought us there is frequently a cancelled context, and the record must
	// still be written.
	executionWriteGrace = 15 * time.Second

	// executionMutationMargin is added to the computed mutation budget to cover
	// the round trips the timeouts themselves do not account for.
	executionMutationMargin = 2 * time.Minute
)

// pipeline carries the state of one recreation through its steps.
//
// A struct rather than a long parameter list, because the failure paths all
// need the same six values and threading them through would make the one thing
// that must be obvious -- what has already been done to the host -- the thing
// that is hardest to see.
type pipeline struct {
	execution domain.Execution
	decision  executionDecision

	// captured is the opaque live configuration. The service cannot read it.
	captured *docker.CapturedConfig
	// expected is the value-free projection to compare the replacement against.
	expected domain.PreservationSummary

	replacementID string
	checkpoint    domain.ExecutionCheckpoint
	// mutationAttempted records that a mutation was ISSUED, whether or not it
	// was confirmed. It is what lets recovery distinguish "nothing was changed"
	// from "something may have been changed and we did not find out".
	mutationAttempted bool

	verification domain.ExecutionVerification
}

// execute runs one recreation from end to end.
//
// Every path out of this function writes a terminal record. A recreation that
// stopped without saying why would be worse than one that failed loudly: an
// operator would not know whether their container had been replaced.
func (s *ExecutionService) execute(ctx context.Context, execution domain.Execution) {
	id := execution.ExecutionID
	work := &pipeline{
		execution:    execution,
		verification: newVerification(),
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
		delete(s.containers, execution.ContainerID)
		s.inFlight--
		s.mu.Unlock()

		// The OUTCOME reaches the security audit log from exactly one place.
		//
		// The pipeline has a dozen terminal paths -- refused, cancelled,
		// failed before the mutation, failed after it, succeeded, succeeded
		// with the original left behind -- and an audit call on each would be
		// a list that a future path forgets to join. This deferred call reads
		// the FINAL state back from the store instead, so a path that reaches
		// a conclusion is audited whether or not its author remembered to.
		s.auditOutcome(ctx, execution)
	}()

	// ---- claim -----------------------------------------------------------

	claimed, err := s.store.Advance(preCtx, store.ExecutionChange{
		ExecutionID: id,
		From:        []domain.ExecutionState{domain.ExecutionQueued},
		To:          domain.ExecutionValidating,
		Detail:      "rechecking every prerequisite",
		MarkStarted: true,
	}, s.now().UTC())
	if err != nil {
		s.logger.WarnContext(ctx, "could not claim execution",
			slog.String("executionId", id), slog.String("error", err.Error()))
		return
	}
	if !claimed {
		// Cancelled or expired between being listed and being claimed. Another
		// path owns it now.
		return
	}

	// ---- revalidate ------------------------------------------------------
	//
	// The second preflight, and the one that matters. The first ran when the
	// operator asked, which may have been minutes ago.

	decision, err := s.preflight(preCtx, execution.AcquisitionID)
	if err != nil {
		s.failBeforeMutation(ctx, work, domain.ExecutionFailureInternal,
			"HarborMaster could not complete the safety checks")
		return
	}
	if decision.Refusal != domain.ExecutionRefusalNone {
		s.refuse(ctx, work, decision.Refusal)
		return
	}
	// The container must be the one the request was recorded against. A plan
	// that now names a different container is not this execution's plan, and
	// acting on it would replace something the operator never approved.
	if decision.ContainerID != execution.ContainerID {
		s.refuse(ctx, work, domain.ExecutionRefusalContainerChanged)
		return
	}
	work.decision = decision

	// ---- capture ---------------------------------------------------------

	moved, err := s.store.Advance(preCtx, store.ExecutionChange{
		ExecutionID: id,
		From:        []domain.ExecutionState{domain.ExecutionValidating},
		To:          domain.ExecutionCapturing,
		Detail:      "reading the container's current configuration",
	}, s.now().UTC())
	if err != nil || !moved {
		return
	}

	captured, err := s.capturer.CaptureConfig(preCtx, execution.ContainerID)
	if err != nil {
		s.failBeforeMutation(ctx, work, domain.ExecutionFailureCapture,
			domain.ExecutionFailureCapture.Explain())
		return
	}
	if !captured.Valid() {
		s.failBeforeMutation(ctx, work, domain.ExecutionFailureCapture,
			domain.ExecutionFailureCapture.Explain())
		return
	}
	// The capture must describe the container this execution is about. A
	// mismatch means the id resolved to something else between the preflight
	// and the read, and creating from it would reproduce the wrong container.
	if captured.ContainerID != execution.ContainerID ||
		captured.ContainerName != decision.ContainerName {
		s.failBeforeMutation(ctx, work, domain.ExecutionFailureCapture,
			domain.ExecutionFailureCapture.Explain())
		return
	}
	work.captured = captured

	// The projection the replacement will be compared against, built from the
	// ORIGINAL while it still exists. Built before anything is stopped, because
	// after that the original's live configuration is no longer readable in the
	// state that matters.
	work.expected = captured.Summary(s.digester())
	work.expected.DigestKeyID = s.digestKeyID()
	if len(work.expected.Fields) == 0 {
		s.failBeforeMutation(ctx, work, domain.ExecutionFailureCapture,
			domain.ExecutionFailureCapture.Explain())
		return
	}

	// ---- names -----------------------------------------------------------

	if !derivedNames(&work.decision, id) {
		s.refuse(ctx, work, domain.ExecutionRefusalNameUnavailable)
		return
	}
	// Re-checked against the LIVE host, as late as possible. A collision found
	// after the original is parked is a recreation that can neither complete
	// nor be undone.
	if !s.nameAvailable(preCtx, work.decision.ParkedName, work.decision.QuarantineName) {
		s.refuse(ctx, work, domain.ExecutionRefusalNameUnavailable)
		return
	}

	// A last cancellation check on the very edge of the mutation point. An
	// operator who pressed cancel while the capture was running gets what they
	// asked for rather than a container that was stopped a moment later.
	if preCtx.Err() != nil {
		return
	}

	// ======================= THE MUTATION POINT ===========================
	//
	// From here the operator's cancel function is unregistered and the pipeline
	// runs on a context derived from the WORKER's, not the operator's. A
	// recreation that has begun changing the host must reach a recorded
	// conclusion.
	s.mu.Lock()
	delete(s.cancels, id)
	s.mu.Unlock()

	mutateCtx, cancelMutate := GraceContext(ctx, executionShutdownGrace, s.mutationBudget())
	defer cancelMutate()

	s.mutate(mutateCtx, ctx, work)
}

// mutationBudget bounds the whole mutating half of the pipeline.
//
// Computed from the configured timeouts rather than fixed, so a deployment that
// allows a ten-minute startup does not have its recreations cut off at five --
// and so the bound is visibly derived from settings an operator can see.
func (s *ExecutionService) mutationBudget() time.Duration {
	return s.cfg.StopTimeout + s.cfg.StartupTimeout + s.cfg.StabilityPeriod + executionMutationMargin
}

// mutate runs the half of the pipeline that changes the host.
//
// parent is the worker's context, used only for the detached terminal writes:
// the record of what happened must be written even when the mutation context
// has expired, because a failure with no record is the one outcome worse than
// the failure itself.
func (s *ExecutionService) mutate(ctx, parent context.Context, work *pipeline) {
	id := work.execution.ExecutionID
	decision := work.decision

	moved, err := s.store.Advance(ctx, store.ExecutionChange{
		ExecutionID: id,
		From:        []domain.ExecutionState{domain.ExecutionCapturing},
		To:          domain.ExecutionCreating,
		Detail:      "stopping the original container and creating its replacement",
		ParkedName:  decision.ParkedName,
	}, s.now().UTC())
	if err != nil || !moved {
		// Could not even record the intent. Nothing has been changed, so
		// stopping here is free -- and it is the only safe option, because a
		// mutation whose intent is unrecorded is a mutation recovery cannot
		// reason about.
		s.failAfterMutation(parent, work, domain.ExecutionFailurePersistence,
			domain.ExecutionFailurePersistence.Explain())
		return
	}

	// ---- stop the original ------------------------------------------------

	work.mutationAttempted = true
	if err := s.mutator.StopContainer(ctx, docker.StopRequest{
		ContainerID: work.execution.ContainerID,
		Timeout:     s.cfg.StopTimeout,
	}); err != nil {
		s.failAfterMutation(parent, work, classifyMutationFailure(err, domain.ExecutionFailureStop),
			domain.ExecutionFailureStop.Explain())
		return
	}
	if !s.checkpoint(ctx, parent, work, store.ExecutionCheckpointWrite{
		ExecutionID: id,
		Checkpoint:  domain.CheckpointOriginalStopped,
		Detail:      "the original container was stopped",
		MarkMutated: true,
	}) {
		return
	}

	// ---- park the original ------------------------------------------------
	//
	// Renamed rather than removed. The replacement needs the name, and the
	// original needs to survive: it is the only way back if the replacement
	// does not work.

	if s.shuttingDown(parent, work) {
		return
	}
	if err := s.mutator.RenameContainer(ctx, docker.RenameRequest{
		ContainerID: work.execution.ContainerID,
		NewName:     decision.ParkedName,
	}); err != nil {
		s.failAfterMutation(parent, work, classifyMutationFailure(err, domain.ExecutionFailureRename),
			domain.ExecutionFailureRename.Explain())
		return
	}
	if !s.checkpoint(ctx, parent, work, store.ExecutionCheckpointWrite{
		ExecutionID: id,
		Checkpoint:  domain.CheckpointOriginalParked,
		Detail:      "the original container was renamed to " + decision.ParkedName,
		ParkedName:  decision.ParkedName,
	}) {
		return
	}

	// ---- create the replacement -------------------------------------------

	if s.shuttingDown(parent, work) {
		return
	}
	replacementID, err := s.mutator.CreateContainer(ctx, docker.CreateRequest{
		Captured: work.captured,
		Image:    decision.Target,
		Name:     decision.ContainerName,
	})
	if err != nil {
		s.failAfterMutation(parent, work, classifyMutationFailure(err, domain.ExecutionFailureCreate),
			domain.ExecutionFailureCreate.Explain())
		return
	}
	work.replacementID = replacementID
	if !s.checkpoint(ctx, parent, work, store.ExecutionCheckpointWrite{
		ExecutionID:   id,
		Checkpoint:    domain.CheckpointReplacementCreated,
		Detail:        "the replacement container was created",
		ReplacementID: replacementID,
	}) {
		return
	}

	// ---- start the replacement --------------------------------------------

	if s.shuttingDown(parent, work) {
		return
	}
	moved, err = s.store.Advance(ctx, store.ExecutionChange{
		ExecutionID:   id,
		From:          []domain.ExecutionState{domain.ExecutionCreating},
		To:            domain.ExecutionStarting,
		Detail:        "starting the replacement container",
		ReplacementID: replacementID,
	}, s.now().UTC())
	if err != nil || !moved {
		s.failAfterMutation(parent, work, domain.ExecutionFailurePersistence,
			domain.ExecutionFailurePersistence.Explain())
		return
	}

	if err := s.mutator.StartContainer(ctx, docker.StartRequest{ContainerID: replacementID}); err != nil {
		s.failAfterMutation(parent, work, classifyMutationFailure(err, domain.ExecutionFailureStart),
			domain.ExecutionFailureStart.Explain())
		return
	}
	if !s.checkpoint(ctx, parent, work, store.ExecutionCheckpointWrite{
		ExecutionID: id,
		Checkpoint:  domain.CheckpointReplacementStarted,
		Detail:      "the replacement container was started",
	}) {
		return
	}

	// ---- prove it ---------------------------------------------------------

	moved, err = s.store.Advance(ctx, store.ExecutionChange{
		ExecutionID: id,
		From:        []domain.ExecutionState{domain.ExecutionStarting},
		To:          domain.ExecutionVerifying,
		Detail:      "proving the replacement before removing the original",
	}, s.now().UTC())
	if err != nil || !moved {
		s.failAfterMutation(parent, work, domain.ExecutionFailurePersistence,
			domain.ExecutionFailurePersistence.Explain())
		return
	}

	// verify receives the PARENT as well, so its polling loop stops at
	// shutdown rather than waiting out the mutation grace. Verification is
	// entirely reads: aborting it changes nothing, and the checkpoint already
	// records that the replacement was started.
	if failure := s.verify(ctx, parent, work); failure != domain.ExecutionFailureNone {
		s.failAfterMutation(parent, work, failure, failure.Explain())
		return
	}

	if !s.checkpoint(ctx, parent, work, store.ExecutionCheckpointWrite{
		ExecutionID: id,
		Checkpoint:  domain.CheckpointReplacementVerified,
		Detail:      "the replacement passed health, image, configuration, and network verification",
	}) {
		return
	}

	s.succeed(ctx, parent, work)
}

// succeed records the outcome and then, and only then, removes the original.
//
// # The order is the safety property
//
// The success is written FIRST. If that write does not land, HarborMaster
// cannot prove afterwards that the replacement was ever verified -- so it stops
// with both containers on the host and a recovery plan, rather than removing
// the only thing that could restore service on the strength of a record it
// could not write.
//
// The removal is therefore the last act, and its own failure is NOT a failure
// of the recreation: the replacement is running, proved, and serving. A parked
// container that could not be removed is tidying, and it is reported as such.
func (s *ExecutionService) succeed(ctx, parent context.Context, work *pipeline) {
	id := work.execution.ExecutionID

	recorded, err := s.store.Advance(ctx, store.ExecutionChange{
		ExecutionID:   id,
		From:          []domain.ExecutionState{domain.ExecutionVerifying},
		To:            domain.ExecutionSucceeded,
		Detail:        "verified; removing the original",
		Message:       "the replacement is running the approved image and passed every verification",
		ReplacementID: work.replacementID,
		Verification:  &work.verification,
	}, s.now().UTC())
	if err != nil || !recorded {
		// Fails closed by INACTION: the original is still parked and intact,
		// because the removal below is never reached.
		s.failAfterMutation(parent, work, domain.ExecutionFailurePersistence,
			domain.ExecutionFailurePersistence.Explain())
		return
	}

	if err := s.mutator.RemoveContainer(ctx, docker.RemoveRequest{
		ContainerID: work.execution.ContainerID,
	}); err != nil {
		// The recreation SUCCEEDED -- the replacement is serving the approved
		// image and every proof passed. What failed is housekeeping, and
		// reporting it as a failed recreation would be inaccurate in the
		// direction that causes an operator to roll back something that works.
		s.logger.WarnContext(ctx, "the parked original could not be removed after a successful recreation",
			slog.String("executionId", id),
			slog.String("parkedName", work.decision.ParkedName),
			slog.String("error", err.Error()))

		plan := domain.BuildRecoveryPlan(s.recoveryContext(work))
		if _, writeErr := s.store.Advance(parent, store.ExecutionChange{
			ExecutionID: id,
			From:        []domain.ExecutionState{domain.ExecutionSucceeded},
			To:          domain.ExecutionSucceeded,
			Detail:      "the parked original could not be removed and is still on this host",
			Message: "the replacement is running the approved image; the original could not be " +
				"removed and is still present under its parked name",
			Recovery: plan,
		}, s.now().UTC()); writeErr != nil {
			s.logger.WarnContext(ctx, "could not record the leftover parked container",
				slog.String("executionId", id), slog.String("error", writeErr.Error()))
		}
		return
	}

	if !s.checkpoint(ctx, parent, work, store.ExecutionCheckpointWrite{
		ExecutionID:     id,
		Checkpoint:      domain.CheckpointOriginalRemoved,
		Detail:          "the original container was removed; the recreation is complete",
		OriginalRemoved: true,
	}) {
		// The host is in the desired state and the record understates it by one
		// checkpoint. Logged rather than treated as a failure: reporting a
		// working replacement as failed would be the more harmful inaccuracy.
		s.logger.WarnContext(ctx, "the original was removed but the checkpoint was not recorded",
			slog.String("executionId", id))
		return
	}

	s.logger.WarnContext(ctx, "container recreated",
		slog.String("executionId", id),
		slog.String("containerName", work.decision.ContainerName),
		slog.String("replacementId", domain.ShortenID(work.replacementID)),
		slog.String("fromImage", work.execution.OldImage),
		slog.String("toDigest", work.decision.Target.Digest),
		slog.Bool("originalRemoved", true))
}

// ------------------------------------------------------------ checkpoints --

// checkpoint records that one mutation completed, and reports whether the
// pipeline may continue.
//
// A false return means the checkpoint did not land, and the caller must return
// immediately. It has already written the terminal record and the recovery
// plan; the pipeline must not attempt another mutation, because it can no
// longer say what it has already done.
func (s *ExecutionService) checkpoint(
	ctx, parent context.Context,
	work *pipeline,
	write store.ExecutionCheckpointWrite,
) bool {
	// Deliberately NOT the mutation context. A checkpoint whose write is
	// cancelled because the mutation budget expired is the exact case this
	// design exists to avoid: the host was changed and the change went
	// unrecorded. It gets its own bounded, detached budget.
	writeCtx, cancel := GraceContext(parent, executionWriteGrace, executionWriteGrace)
	defer cancel()

	if err := s.store.Checkpoint(writeCtx, write, s.now().UTC()); err != nil {
		// Logged at ERROR. HarborMaster has changed a host and cannot prove it
		// recorded the fact, which is the most serious condition this feature
		// can produce.
		s.logger.ErrorContext(ctx, "could not record an execution checkpoint; stopping rather than acting again",
			slog.String("executionId", work.execution.ExecutionID),
			slog.String("checkpoint", string(write.Checkpoint)),
			slog.String("error", err.Error()))

		s.failAfterMutation(parent, work, domain.ExecutionFailurePersistence,
			domain.ExecutionFailurePersistence.Explain())
		return false
	}

	work.checkpoint = write.Checkpoint
	if write.ReplacementID != "" {
		work.replacementID = write.ReplacementID
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
// pass reasons from exactly what was done. That is the whole reason stopping
// here is safe: an interrupted recreation is a recoverable one precisely
// because it stopped at a boundary where the checkpoint was current.
func (s *ExecutionService) shuttingDown(parent context.Context, work *pipeline) bool {
	if parent.Err() == nil {
		return false
	}

	s.logger.Warn("shutting down partway through a recreation; stopping at a recorded point",
		slog.String("executionId", work.execution.ExecutionID),
		slog.String("containerName", work.decision.ContainerName),
		slog.String("checkpoint", string(work.checkpoint)))

	s.failAfterMutation(parent, work, domain.ExecutionFailureInterrupted,
		domain.ExecutionFailureInterrupted.Explain())
	return true
}

// ---------------------------------------------------------------- failing --

// refuse records a preflight refusal.
//
// A refusal is the safety model working, and it always happens BEFORE the
// mutation point -- so it is recorded with the specific check that said no, and
// with the plain statement that nothing on the host was changed.
func (s *ExecutionService) refuse(ctx context.Context, work *pipeline, refusal domain.ExecutionRefusal) {
	writeCtx, cancel := GraceContext(ctx, executionWriteGrace, executionWriteGrace)
	defer cancel()

	if _, err := s.store.Advance(writeCtx, store.ExecutionChange{
		ExecutionID: work.execution.ExecutionID,
		To:          domain.ExecutionFailed,
		Failure:     domain.ExecutionFailurePreflight,
		Refusal:     refusal,
		Message:     refusal.Explain(),
		Detail:      "refused before anything on this host was changed",
	}, s.now().UTC()); err != nil {
		s.logger.WarnContext(ctx, "could not record execution refusal",
			slog.String("executionId", work.execution.ExecutionID),
			slog.String("error", err.Error()))
	}

	s.logger.InfoContext(ctx, "container recreation refused by preflight",
		slog.String("executionId", work.execution.ExecutionID),
		slog.String("refusal", string(refusal)))
}

// failBeforeMutation records a failure that changed nothing.
//
// No recovery plan beyond the informational one: there is nothing on the host
// to settle, and saying so plainly is the most useful thing the record can do.
func (s *ExecutionService) failBeforeMutation(
	ctx context.Context,
	work *pipeline,
	failure domain.ExecutionFailure,
	message string,
) {
	writeCtx, cancel := GraceContext(ctx, executionWriteGrace, executionWriteGrace)
	defer cancel()

	plan := domain.BuildRecoveryPlan(s.recoveryContext(work))

	if _, err := s.store.Advance(writeCtx, store.ExecutionChange{
		ExecutionID:  work.execution.ExecutionID,
		To:           domain.ExecutionFailed,
		Failure:      failure,
		Message:      message,
		Detail:       "failed before anything on this host was changed",
		Verification: &work.verification,
		Recovery:     plan,
	}, s.now().UTC()); err != nil {
		s.logger.WarnContext(ctx, "could not record execution failure",
			slog.String("executionId", work.execution.ExecutionID),
			slog.String("error", err.Error()))
	}
}

// failAfterMutation quarantines the replacement, preserves both containers, and
// records a manual recovery plan.
//
// # What it does not do
//
// It does not roll back. The original is not renamed back, not restarted, and
// not touched at all -- it is exactly where the pipeline left it, stopped and
// parked. The replacement is stopped and renamed aside so that a container
// which failed verification is not left serving under the production name, and
// neither container is removed, because both are evidence.
//
// Every step here is BEST EFFORT and every one is independent. A quarantine
// that cannot be completed must not prevent the terminal record from being
// written: the record is what tells the operator where to look, and it is more
// valuable than the tidiness it describes.
func (s *ExecutionService) failAfterMutation(
	ctx context.Context,
	work *pipeline,
	failure domain.ExecutionFailure,
	message string,
) {
	id := work.execution.ExecutionID

	// Detached and bounded. The failure that brought us here is frequently an
	// expired mutation budget, and the quarantine and the record must both
	// still happen.
	writeCtx, cancel := GraceContext(ctx, executionWriteGrace, s.quarantineBudget())
	defer cancel()

	quarantined := s.quarantine(writeCtx, work, failure)

	plan := domain.BuildRecoveryPlan(s.recoveryContext(work))

	change := store.ExecutionChange{
		ExecutionID:   id,
		To:            domain.ExecutionFailed,
		Failure:       failure,
		Message:       message,
		Detail:        "failed after the host was changed; both containers preserved",
		ReplacementID: work.replacementID,
		ParkedName:    work.decision.ParkedName,
		Verification:  &work.verification,
		Recovery:      plan,
		MarkMutated:   work.mutationAttempted,
	}
	if quarantined {
		change.QuarantineName = work.decision.QuarantineName
	}

	if _, err := s.store.Advance(writeCtx, change, s.now().UTC()); err != nil {
		// The last resort. There is nothing further to try, and the log is the
		// only remaining channel -- so it carries everything an operator needs
		// to find the containers by hand.
		s.logger.ErrorContext(ctx, "a recreation failed after changing the host and the record could not be written",
			slog.String("executionId", id),
			slog.String("containerId", domain.ShortenID(work.execution.ContainerID)),
			slog.String("containerName", work.decision.ContainerName),
			slog.String("parkedName", work.decision.ParkedName),
			slog.String("replacementId", domain.ShortenID(work.replacementID)),
			slog.String("checkpoint", string(work.checkpoint)),
			slog.String("failure", string(failure)),
			slog.String("error", err.Error()))
		return
	}

	s.logger.ErrorContext(ctx, "container recreation failed after changing the host",
		slog.String("executionId", id),
		slog.String("containerName", work.decision.ContainerName),
		slog.String("checkpoint", string(work.checkpoint)),
		slog.String("failure", string(failure)),
		slog.String("parkedName", work.decision.ParkedName),
		slog.Bool("originalPreserved", true),
		slog.Bool("replacementPreserved", work.replacementID != ""))
}

// quarantine stops a failed replacement and renames it aside.
//
// Reports whether the rename succeeded, which is what decides whether the
// record can promise the operator a name to look for.
func (s *ExecutionService) quarantine(
	ctx context.Context,
	work *pipeline,
	failure domain.ExecutionFailure,
) bool {
	if work.replacementID == "" {
		return false
	}
	// A create that failed produced no container to quarantine, and a
	// persistence failure must not trigger further mutations -- that is the
	// entire reason it is a distinct classification.
	if failure == domain.ExecutionFailurePersistence {
		return false
	}

	// Stopped first. A container that failed verification must not be left
	// serving under the production name, and the rename below does not stop it.
	if err := s.mutator.StopContainer(ctx, docker.StopRequest{
		ContainerID: work.replacementID,
		Timeout:     s.cfg.StopTimeout,
	}); err != nil {
		s.logger.WarnContext(ctx, "could not stop the failed replacement",
			slog.String("executionId", work.execution.ExecutionID),
			slog.String("error", err.Error()))
		// Continue anyway. Renaming a running container is legal, and moving it
		// off the production name is worth doing even if it is still up.
	}

	if err := s.mutator.RenameContainer(ctx, docker.RenameRequest{
		ContainerID: work.replacementID,
		NewName:     work.decision.QuarantineName,
	}); err != nil {
		s.logger.WarnContext(ctx, "could not rename the failed replacement aside",
			slog.String("executionId", work.execution.ExecutionID),
			slog.String("error", err.Error()))
		return false
	}

	// Best effort, like everything in this function: a quarantine that is done
	// but unrecorded is better than one that is not done at all, and the
	// terminal record below carries the name regardless.
	if err := s.store.Checkpoint(ctx, store.ExecutionCheckpointWrite{
		ExecutionID:    work.execution.ExecutionID,
		Checkpoint:     domain.CheckpointReplacementQuarantined,
		Detail:         "the failed replacement was stopped and renamed to " + work.decision.QuarantineName,
		QuarantineName: work.decision.QuarantineName,
	}, s.now().UTC()); err != nil {
		s.logger.WarnContext(ctx, "could not record the quarantine checkpoint",
			slog.String("executionId", work.execution.ExecutionID),
			slog.String("error", err.Error()))
	} else {
		work.checkpoint = domain.CheckpointReplacementQuarantined
	}
	return true
}

// quarantineBudget bounds the cleanup after a failure.
//
// Enough for one stop and one rename, and no more. Cleanup must not become the
// thing that holds a shutdown open.
func (s *ExecutionService) quarantineBudget() time.Duration {
	return s.cfg.StopTimeout + executionWriteGrace
}

// recoveryContext assembles what a recovery plan is built from.
func (s *ExecutionService) recoveryContext(work *pipeline) domain.RecoveryContext {
	return domain.RecoveryContext{
		ExecutionID:       work.execution.ExecutionID,
		ContainerName:     work.execution.ContainerName,
		OriginalID:        work.execution.ContainerID,
		ParkedName:        work.decision.ParkedName,
		ReplacementID:     work.replacementID,
		QuarantineName:    work.decision.QuarantineName,
		Checkpoint:        work.checkpoint,
		Failure:           domain.ExecutionFailureNone,
		MutationAttempted: work.mutationAttempted,
	}
}

// newVerification returns a verification with every proof UNKNOWN.
//
// Unknown rather than zero-valued, so a record written by a path that never
// reached verification says "not established" rather than an empty string that
// could be read as anything.
func newVerification() domain.ExecutionVerification {
	return domain.ExecutionVerification{
		Health:       domain.VerificationUnknown,
		Image:        domain.VerificationUnknown,
		Preservation: domain.VerificationUnknown,
		Network:      domain.VerificationUnknown,
	}
}

// classifyMutationFailure maps an adapter error onto a classification.
//
// The adapter has already stripped daemon text; this decides which of
// HarborMaster's own vocabulary applies. A daemon that went away is
// distinguished from a refusal, because they call for different things from an
// operator.
func classifyMutationFailure(err error, fallback domain.ExecutionFailure) domain.ExecutionFailure {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return domain.ExecutionFailureTimeout
	case errors.Is(err, context.Canceled):
		// Only reachable past the mutation point, where the only cancellation
		// is shutdown. Recorded as interrupted, which is what the restart
		// recovery pass will also call it.
		return domain.ExecutionFailureInterrupted
	case errors.Is(err, docker.ErrUnreachable):
		return domain.ExecutionFailureDockerUnavailable
	}
	return fallback
}

// ------------------------------------------------------- audit attribution --

// auditOutcome records what a finished recreation did to the host.
//
// # Why this is separate from the execution record
//
// The execution record is an operational history: an operator reads it to
// understand one recreation. The audit log is a security record: an
// administrator reads it to answer "what has been done to this host, and by
// whom". The second question is not answerable from a request row alone,
// because a request can be refused, cancelled, expired, or fail partway.
//
// # Why it reads the state back
//
// The state at the moment this runs is the conclusion. Passing an expected
// outcome in from each terminal path would mean trusting the caller to be
// honest about what it did, and a caller that got it wrong would write a
// confident audit row that disagrees with the record beside it.
//
// # Why it is bounded and cannot fail the recreation
//
// The recreation is already over. A detached, bounded context so the write
// lands even when the parent is cancelled, and every failure is logged rather
// than returned: an audit log that could fail an operation is a way to disable
// HarborMaster by filling a disk.
func (s *ExecutionService) auditOutcome(ctx context.Context, requested domain.Execution) {
	if s.audit == nil {
		return
	}

	writeCtx, cancel := GraceContext(ctx, executionWriteGrace, executionWriteGrace)
	defer cancel()

	final, err := s.store.Get(writeCtx, requested.ExecutionID)
	if err != nil {
		// The row could not be read back, so the outcome is genuinely unknown.
		// Recording "failed" would be a guess; recording nothing would lose the
		// event. The row says what is true: HarborMaster does not know.
		s.audit.RecordAction(writeCtx, requesterActor(requested.RequestedBy),
			domain.AuditExecutionFailed, domain.AuditFailed,
			domain.AuditTargetExecution, requested.ExecutionID, requested.ContainerName,
			"the recreation finished but its record could not be read back")
		return
	}

	// Still running. Reached when a shutdown abandoned the pipeline mid-flight;
	// the restart recovery pass settles the record and audits it then.
	if !final.State.Terminal() {
		return
	}

	action := domain.AuditExecutionFailed
	outcome := domain.AuditFailed
	if final.State == domain.ExecutionSucceeded {
		action = domain.AuditExecutionCompleted
		outcome = domain.AuditSucceeded
	}

	s.audit.RecordAction(writeCtx, requesterActor(final.RequestedBy),
		action, outcome, domain.AuditTargetExecution,
		final.ExecutionID, final.ContainerName, executionOutcomeReason(final))
}

// executionOutcomeReason renders the conclusion in HarborMaster's own words.
//
// Never the failure MESSAGE, which is built for an operator reading the
// recreation page and can carry a Docker error. This is a fixed sentence plus a
// closed-vocabulary failure name, because the value reaches a page an
// administrator reads and a column that must stay bounded.
//
// The most important thing it says is whether the host was left changed. That
// is the difference between a record somebody has to act on and one they do not.
func executionOutcomeReason(final domain.Execution) string {
	switch {
	case final.State == domain.ExecutionSucceeded && final.OriginalRemoved:
		return "the container was replaced on the approved image and the original removed"
	case final.State == domain.ExecutionSucceeded:
		return "the container was replaced on the approved image; the original is still present"
	case final.Checkpoint.HostChanged():
		return "the recreation failed after changing this host and needs attention (" +
			string(final.Failure) + ")"
	case final.State == domain.ExecutionCancelled:
		return "cancelled before anything on this host was changed"
	case final.State == domain.ExecutionExpired:
		return "expired in the queue; nothing on this host was changed"
	default:
		return "refused or failed before anything on this host was changed (" +
			string(final.Failure) + ")"
	}
}

// requesterActor projects a stored requester onto an audit actor.
//
// The role, session, request id, and address are deliberately absent: they
// belong to the REQUEST, which was audited with them at the time. Repeating a
// stale copy here would produce two rows that can disagree about the same fact.
func requesterActor(requester domain.Requester) Actor {
	return Actor{UserID: requester.UserID, Username: requester.Username}
}
