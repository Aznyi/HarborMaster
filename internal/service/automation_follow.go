package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The follower: advancing work a pass already submitted.
//
// # Why this is a separate loop with no memory
//
// A decision pass submits an acquisition and stops. Everything after that --
// the pull completing, the recreation being requested, the recreation failing
// verification, the rollback -- happens over minutes, and a process can restart
// at any point in it.
//
// So the follower holds NO state. Every time it runs it re-reads the decisions
// of the most recent passes from the database, looks up what happened to the
// records they name, and takes the one next step each is owed. Restarting
// HarborMaster mid-update loses nothing: the next follower tick reads the same
// rows and continues.
//
// # It is the same three requests, again
//
// The follower submits ExecutionRequest{AcquisitionID} and
// RollbackRequest{ExecutionID}. Both take exactly one identifier, and that
// identifier comes from a decision row HarborMaster wrote. The recreation
// service re-verifies the acquisition against the live host; the rollback
// service re-verifies the execution's two container identities. The follower
// establishes nothing itself and is trusted for nothing.
//
// # Automatic rollback is a policy decision made earlier
//
// Whether a failed update may be rolled back automatically was decided by the
// effective policy at DECISION time and is re-read here from the policy that
// governed the decision. A rollback ALWAYS pauses the container afterwards,
// whatever the failure counters say: a rollback means the change was wrong and
// the host was moved twice, and retrying that on a timer is how a bad image
// becomes an outage loop.

// maxFollowDecisions bounds one follower tick.
//
// A tick that found more outstanding updates than this works the newest and
// leaves the rest for the next tick, five seconds to thirty later. It is a
// backstop against a pathological backlog, not an expected limit.
const maxFollowDecisions = 200

// follow advances every update automation started that has not finished.
//
// One query, asked of the DECISIONS rather than of recent runs. What the
// follower owes a next step is a property of the decision -- it named an
// acquisition and has not reached a rollback -- and not of which passes happen
// to be recent. An approval is promoted after its pass has already finished,
// and a slow update can outlast several scheduler ticks; both would fall out of
// a window over runs, and both would be abandoned with an acquired image and no
// recreation.
func (s *AutomationService) follow(ctx context.Context) {
	decisions, err := s.store.PendingDecisions(ctx, maxFollowDecisions)
	if err != nil {
		s.logger.ErrorContext(ctx, "automation could not read its outstanding work",
			slog.Any("error", err))
		return
	}

	for _, decision := range decisions {
		if ctx.Err() != nil {
			return
		}
		s.advance(ctx, decision)
	}
}

// advance takes the one next step a decision is owed.
//
// Exactly one step per tick, deliberately. A decision that needs an execution
// requested and then a rollback requested gets them on separate ticks, so every
// step re-reads the world rather than acting on what the previous step believed
// about it.
func (s *AutomationService) advance(ctx context.Context, decision domain.AutomationDecision) {
	switch {
	case decision.AcquisitionID == "":
		// A decision with no acquisition never acted. Nothing to follow.
		return
	case decision.RollbackID != "":
		// The last step in the pipeline. Terminal for the follower.
		return
	case decision.ExecutionID == "":
		s.advanceAcquisition(ctx, decision)
	default:
		s.advanceExecution(ctx, decision)
	}
}

// advanceAcquisition requests the recreation once the pull has succeeded.
func (s *AutomationService) advanceAcquisition(ctx context.Context, decision domain.AutomationDecision) {
	acquisition, err := s.pipeline.Acquisition(ctx, decision.AcquisitionID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.logger.WarnContext(ctx, "automation could not read an acquisition it started",
				slog.String("acquisitionId", decision.AcquisitionID), slog.Any("error", err))
		}
		return
	}

	switch {
	case !acquisition.State.Terminal():
		// Still pulling. Nothing to do; the next tick will look again.
		return

	case acquisition.State != domain.AcquisitionSucceeded:
		// The image never arrived. Nothing was recreated and nothing on the
		// host changed, so this is a failure to COUNT rather than one to roll
		// back: there is nothing to undo.
		s.recordFailure(ctx, decision,
			"the image could not be acquired ("+string(acquisition.State)+")")
		return
	}

	execution, err := s.pipeline.RequestExecution(ctx, ExecutionRequest{
		// The acquisition id, and nothing else. Every container identity the
		// recreation uses is derived from the acquisition and re-verified
		// against the live host by the execution service's own preflight.
		AcquisitionID: decision.AcquisitionID,
		RequestKey:    "automation:execute:" + decision.AcquisitionID,
		// The account the pass was attributed to. Read back off the decision's
		// run rather than remembered, so a restart does not lose it.
		RequestedBy: s.requesterForRun(ctx, decision.RunID),
	})
	if err != nil {
		var refused ErrExecutionRefused
		if errors.As(err, &refused) {
			// The preflight refused. The host is unchanged, so this is a
			// failure to count and not one to undo.
			s.recordFailure(ctx, decision,
				"the recreation preflight refused: "+refused.Refusal.Explain())
			return
		}
		s.logger.ErrorContext(ctx, "automation could not request a recreation",
			slog.String("acquisitionId", decision.AcquisitionID), slog.Any("error", err))
		return
	}

	if err := s.store.AttachExecution(ctx, decision.RunID,
		decision.AcquisitionID, execution.ExecutionID); err != nil {
		// The recreation is already under way. Losing the link makes the
		// history harder to read; it does not make the host unsafe, and
		// refusing to continue would be worse.
		s.logger.ErrorContext(ctx, "automation could not link a recreation to its decision",
			slog.String("executionId", execution.ExecutionID), slog.Any("error", err))
	}

	s.logger.InfoContext(ctx, "automation requested a container recreation",
		slog.String("runId", decision.RunID),
		slog.String("executionId", execution.ExecutionID),
		slog.String("containerName", domain.SanitiseDisplayText(decision.ContainerName, domain.MaxContainerNameBytes)))
}

// advanceExecution handles the recreation's outcome.
func (s *AutomationService) advanceExecution(ctx context.Context, decision domain.AutomationDecision) {
	execution, err := s.pipeline.Execution(ctx, decision.ExecutionID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.logger.WarnContext(ctx, "automation could not read a recreation it started",
				slog.String("executionId", decision.ExecutionID), slog.Any("error", err))
		}
		return
	}
	if !execution.State.Terminal() {
		return
	}

	if execution.State == domain.ExecutionSucceeded {
		if err := s.store.RecordSuccess(ctx, decision.ContainerName, s.now()); err != nil {
			s.logger.ErrorContext(ctx, "automation could not record a successful update",
				slog.String("containerName", domain.SanitiseDisplayText(decision.ContainerName, domain.MaxContainerNameBytes)),
				slog.Any("error", err))
		}
		// Terminal. Without this the decision matches the follower's query
		// forever and the same completed update is re-read on every tick.
		s.settle(ctx, decision)
		s.logger.InfoContext(ctx, "automation completed a container update",
			slog.String("runId", decision.RunID),
			slog.String("executionId", execution.ExecutionID),
			slog.String("containerName", domain.SanitiseDisplayText(decision.ContainerName, domain.MaxContainerNameBytes)))
		return
	}

	// The recreation did not succeed. Two separate questions follow, in order:
	// may this be rolled back, and should this container be paused.
	s.handleFailedExecution(ctx, decision, execution)
}

// handleFailedExecution rolls back when the policy permits, then counts the
// failure.
func (s *AutomationService) handleFailedExecution(
	ctx context.Context,
	decision domain.AutomationDecision,
	execution domain.Execution,
) {
	detail := "the recreation did not succeed (" + string(execution.Failure) + ")"

	rolledBack := false
	if s.shouldRollback(ctx, decision, execution) {
		rollback, err := s.pipeline.RequestRollback(ctx, RollbackRequest{
			// The execution id, and nothing else. Both container identities are
			// read off the execution record by the rollback service and
			// re-verified against the live host before anything moves.
			ExecutionID: decision.ExecutionID,
			RequestKey:  "automation:rollback:" + decision.ExecutionID,
			RequestedBy: s.requesterForRun(ctx, decision.RunID),
		})
		switch {
		case err == nil:
			rolledBack = true
			// Carried onto the local copy so the PAUSE the failure produces
			// names the rollback that caused it. The stored decision is updated
			// separately by AttachRollback below; without this the pause row
			// would say "rolled back" and have nothing to point at.
			decision.RollbackID = rollback.RollbackID
			detail = "the recreation failed (" + string(execution.Failure) +
				") and was rolled back automatically"
			if attachErr := s.store.AttachRollback(ctx, decision.RunID,
				decision.ExecutionID, rollback.RollbackID); attachErr != nil {
				s.logger.ErrorContext(ctx, "automation could not link a rollback to its decision",
					slog.String("rollbackId", rollback.RollbackID), slog.Any("error", attachErr))
			}
			s.logger.WarnContext(ctx, "automation rolled back a failed update",
				slog.String("runId", decision.RunID),
				slog.String("rollbackId", rollback.RollbackID),
				slog.String("containerName", domain.SanitiseDisplayText(decision.ContainerName, domain.MaxContainerNameBytes)))

		case errors.Is(err, ErrRollbackRefused):
			// The rollback preflight said no -- most often because the
			// recreation did not get far enough for there to be an arrangement
			// to undo, which means the host is already in its original state.
			var refused RollbackRefusedError
			if errors.As(err, &refused) {
				detail += "; automatic rollback was refused: " + refused.Refusal.Explain()
			}

		default:
			detail += "; automatic rollback could not be requested"
			s.logger.ErrorContext(ctx, "automation could not request a rollback",
				slog.String("executionId", decision.ExecutionID), slog.Any("error", err))
		}
	}

	s.recordFailureAndMaybePause(ctx, decision, detail, rolledBack, execution.ExecutionID)
	// Terminal either way: a rollback was submitted, or the failure was counted
	// and the container may have been paused. The follower is done with this
	// decision; the rollback has its own record and its own recovery path.
	s.settle(ctx, decision)
}

// shouldRollback reports whether this failure may be undone automatically.
//
// Three conditions, all required. The capability must exist in this deployment,
// the governing policy must permit it, and the recreation must have got far
// enough for there to be an arrangement to undo. The third is the rollback
// service's own judgement and is re-made by its preflight; it is checked here
// only to avoid asking for something that is certain to be refused.
func (s *AutomationService) shouldRollback(
	ctx context.Context,
	decision domain.AutomationDecision,
	execution domain.Execution,
) bool {
	if !s.pipeline.RollbackEnabled() {
		return false
	}
	if !domain.RollbackSufficientCheckpoint(execution.Checkpoint) {
		return false
	}
	if execution.OriginalRemoved {
		// The original is gone. There is nothing to put back, and the rollback
		// service would refuse.
		return false
	}
	return s.policyPermitsRollback(ctx, decision)
}

// policyPermitsRollback re-reads the governing policy's setting.
//
// Re-read rather than carried on the decision, so an administrator who turns
// automatic rollback on after a failure gets the behaviour they just asked for
// on the follow-through. Fails CLOSED: a policy that cannot be read does not
// authorise a second mutation.
func (s *AutomationService) policyPermitsRollback(
	ctx context.Context,
	decision domain.AutomationDecision,
) bool {
	if decision.PolicyID == "" {
		return false
	}
	policies, err := s.policies.ActivePolicies(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "automation could not re-read the governing policy; not rolling back",
			slog.String("policyId", decision.PolicyID), slog.Any("error", err))
		return false
	}
	for _, policy := range policies {
		if policy.PolicyID == decision.PolicyID {
			return policy.Failure.AutoRollback
		}
	}
	// The policy was withdrawn between the decision and the failure. Not an
	// authorisation to act.
	return false
}

// recordFailure counts a failure that left the host unchanged.
//
// A failed pull or a refused recreation preflight. Terminal for the follower:
// there is nothing to undo and nothing further to advance, and the next pass
// will decide the container again from scratch.
func (s *AutomationService) recordFailure(
	ctx context.Context,
	decision domain.AutomationDecision,
	detail string,
) {
	s.recordFailureAndMaybePause(ctx, decision, detail, false, "")
	s.settle(ctx, decision)
}

// settle marks a decision terminal for the follower.
//
// Called on every ending: the update succeeded, the pull failed, a preflight
// refused, or a rollback was submitted. A failure to write it is logged and not
// raised -- the host is already where it is going to be, and the cost is a
// decision the follower looks at again.
func (s *AutomationService) settle(ctx context.Context, decision domain.AutomationDecision) {
	if err := s.store.SettleDecision(ctx, decision.RunID, decision.ContainerName, s.now()); err != nil {
		s.logger.ErrorContext(ctx, "automation could not settle a decision",
			slog.String("runId", decision.RunID),
			slog.String("containerName",
				domain.SanitiseDisplayText(decision.ContainerName, domain.MaxContainerNameBytes)),
			slog.Any("error", err))
	}
}

// recordFailureAndMaybePause counts the failure and pauses when it is time to
// stop trying.
//
// # Two reasons to pause, and one of them is unconditional
//
// A ROLLBACK always pauses. The change was wrong AND the host was moved twice
// to discover that, and an engine that retries such a thing on a timer turns
// one bad image into a repeated outage.
//
// Repeated failure pauses at the policy's threshold, counted within the
// policy's window. Zero disables it, which the policy warnings say is not
// recommended.
func (s *AutomationService) recordFailureAndMaybePause(
	ctx context.Context,
	decision domain.AutomationDecision,
	detail string,
	rolledBack bool,
	executionID string,
) {
	policy, found := s.policyFor(ctx, decision.PolicyID)

	windowHours := domain.DefaultPauseWindowHours
	threshold := domain.DefaultPauseAfterFailures
	cooldownHours := 0
	if found {
		windowHours = policy.Failure.PauseWindowHours
		threshold = policy.Failure.PauseAfterFailures
		cooldownHours = policy.Failure.CooldownHours
	}

	count, err := s.store.RecordFailure(ctx, decision.ContainerName, detail, windowHours, s.now())
	if err != nil {
		s.logger.ErrorContext(ctx, "automation could not record a failure",
			slog.String("containerName", domain.SanitiseDisplayText(decision.ContainerName, domain.MaxContainerNameBytes)),
			slog.Any("error", err))
		// Continue anyway. A rollback must still pause even if the counter
		// could not be written -- failing to count must never mean failing to
		// stop.
	}

	reason := domain.PauseRepeatedFailure
	shouldPause := threshold > 0 && count.Windowed >= threshold
	if rolledBack {
		reason = domain.PauseRolledBack
		shouldPause = true
	}
	if !shouldPause {
		s.logger.WarnContext(ctx, "an automated update failed",
			slog.String("runId", decision.RunID),
			slog.String("containerName", domain.SanitiseDisplayText(decision.ContainerName, domain.MaxContainerNameBytes)),
			slog.Int("failuresInWindow", count.Windowed),
			slog.Int("pauseThreshold", threshold))
		return
	}

	pause := domain.PausedContainer{
		ContainerName: decision.ContainerName,
		ContainerID:   decision.ContainerID,
		Reason:        reason,
		Detail:        detail,
		Failures:      count.Windowed,
		PolicyID:      decision.PolicyID,
		ExecutionID:   executionID,
		RollbackID:    decision.RollbackID,
		PausedAt:      s.now().UTC(),
	}
	// A cooldown is opt-in. Without one, only an acknowledgement clears the
	// pause -- the safer reading of "this went wrong repeatedly", and the
	// default.
	//
	// A rollback NEVER gets a cooldown, whatever the policy says: the container
	// was moved twice and a person needs to look at it.
	if cooldownHours > 0 && !rolledBack {
		resume := pause.PausedAt.Add(time.Duration(cooldownHours) * time.Hour)
		pause.ResumeAfter = &resume
	}

	if _, err := s.store.Pause(ctx, pause); err != nil {
		if errors.Is(err, store.ErrPauseActive) {
			// Already paused. Nothing to do and nothing wrong: two failures
			// close together both reached the threshold.
			return
		}
		s.logger.ErrorContext(ctx, "automation could not pause a container",
			slog.String("containerName", domain.SanitiseDisplayText(decision.ContainerName, domain.MaxContainerNameBytes)),
			slog.Any("error", err))
		return
	}

	s.logger.WarnContext(ctx, "automation paused a container",
		slog.String("containerName", domain.SanitiseDisplayText(decision.ContainerName, domain.MaxContainerNameBytes)),
		slog.String("reason", string(reason)),
		slog.Int("failuresInWindow", count.Windowed))

	s.recordAudit(ctx, domain.AuditAutomationPaused, domain.AuditSucceeded,
		decision.ContainerName, Actor{}, reason.Explain())
}

// policyFor reads one governing policy, or reports that it is gone.
func (s *AutomationService) policyFor(ctx context.Context, policyID string) (domain.UpdatePolicy, bool) {
	if policyID == "" {
		return domain.UpdatePolicy{}, false
	}
	policies, err := s.policies.ActivePolicies(ctx)
	if err != nil {
		return domain.UpdatePolicy{}, false
	}
	for _, policy := range policies {
		if policy.PolicyID == policyID {
			return policy, true
		}
	}
	return domain.UpdatePolicy{}, false
}

// requesterForRun reads back the account a pass was attributed to.
//
// Read from the run rather than remembered in memory, so a restart between the
// acquisition and the recreation does not turn an operator's manual pass into
// an unattributed one.
func (s *AutomationService) requesterForRun(ctx context.Context, runID string) domain.Requester {
	run, err := s.store.RunByID(ctx, runID)
	if err != nil {
		return domain.Requester{}
	}
	return run.RequestedBy
}

// ------------------------------------------------------------- retention --

// prune deletes run history past the configured horizon.
func (s *AutomationService) prune(ctx context.Context) {
	if s.cfg.RetentionAge <= 0 {
		return
	}
	cutoff := s.now().Add(-s.cfg.RetentionAge)
	deleted, err := s.store.PruneRuns(ctx, cutoff, 1000)
	if err != nil {
		s.logger.ErrorContext(ctx, "could not prune automation history", slog.Any("error", err))
		return
	}
	if deleted > 0 {
		s.logger.Info("pruned automation history",
			slog.Int("runs", deleted),
			slog.Duration("retention", s.cfg.RetentionAge))
	}
}
