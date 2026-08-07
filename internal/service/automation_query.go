package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Reading the engine, and the two commands a person may give it.
//
// Everything in this file is either a bounded read or an explicitly
// authenticated instruction. The two commands -- approve a decision, resume a
// paused container -- are the points where a person overrides the engine, and
// both record who did it.

// ------------------------------------------------------------- reading --

// Status returns the engine's current state for the dashboard.
//
// Assembled from a handful of aggregate queries rather than from a list the
// caller counts, because this is what a dashboard polls.
func (s *AutomationService) Status(ctx context.Context) (domain.AutomationStatus, error) {
	if !s.Readable() {
		return domain.AutomationStatus{}, ErrAutomationDisabled
	}

	status := domain.AutomationStatus{Enabled: s.Enabled()}

	s.mu.Lock()
	status.Running = s.running
	s.mu.Unlock()

	total, enabled, err := s.policies.CountUpdatePolicies(ctx)
	if err != nil {
		return domain.AutomationStatus{}, err
	}
	status.Policies = total
	status.EnabledPolicies = enabled

	if status.PausedContainers, err = s.store.CountActivePauses(ctx); err != nil {
		return domain.AutomationStatus{}, err
	}
	if status.AwaitingApproval, err = s.store.CountAwaitingApproval(ctx); err != nil {
		return domain.AutomationStatus{}, err
	}

	last, err := s.store.LatestRun(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// No pass has ever run. Not an error, and not something to hide: a
		// dashboard that showed a blank "last run" and a blank "never ran"
		// identically would be lying about one of them.
	case err != nil:
		return domain.AutomationStatus{}, err
	default:
		started := last.StartedAt
		status.LastRunAt = &started
		status.LastRunID = last.RunID
		status.LastOutcome = string(last.State)
		if status.Enabled {
			next := started.Add(s.cfg.Interval)
			status.NextRunAt = &next
		}
	}

	s.fillWindowStatus(ctx, &status)
	return status, nil
}

// fillWindowStatus reports whether any enabled policy admits work now, and when
// the soonest closed one opens.
//
// Best-effort: a policy whose timezone cannot be resolved contributes nothing
// rather than failing the whole status read. That policy's own decisions still
// fail closed, which is where the safety property lives; this is a display.
func (s *AutomationService) fillWindowStatus(ctx context.Context, status *domain.AutomationStatus) {
	policies, err := s.policies.ActivePolicies(ctx)
	if err != nil || len(policies) == 0 {
		return
	}

	now := s.now().UTC()
	var (
		soonest   time.Time
		soonestID string
	)
	for _, policy := range policies {
		open, openErr := policy.Window.Open(now)
		if openErr != nil {
			continue
		}
		if open {
			status.WindowOpen = true
			status.NextWindowOpensAt = nil
			status.NextWindowPolicyID = ""
			return
		}
		next, ok := policy.Window.NextOpen(now)
		if !ok {
			continue
		}
		if soonestID == "" || next.Before(soonest) {
			soonest, soonestID = next, policy.PolicyID
		}
	}
	if soonestID != "" {
		opens := soonest.UTC()
		status.NextWindowOpensAt = &opens
		status.NextWindowPolicyID = soonestID
	}
}

// Runs returns a bounded page of passes.
func (s *AutomationService) Runs(
	ctx context.Context,
	filter store.AutomationRunFilter,
) ([]domain.AutomationRun, int, error) {
	if !s.Readable() {
		return nil, 0, ErrAutomationDisabled
	}
	return s.store.ListRuns(ctx, filter)
}

// RunDetail returns one pass with its decisions.
func (s *AutomationService) RunDetail(
	ctx context.Context,
	runID string,
	page store.Page,
) (domain.AutomationRun, []domain.AutomationDecision, int, error) {
	if !s.Readable() {
		return domain.AutomationRun{}, nil, 0, ErrAutomationDisabled
	}
	run, err := s.store.RunByID(ctx, runID)
	if err != nil {
		return domain.AutomationRun{}, nil, 0, err
	}
	decisions, total, err := s.store.ListDecisions(ctx,
		store.AutomationDecisionFilter{RunID: runID, Page: page})
	if err != nil {
		return run, nil, 0, err
	}
	return run, decisions, total, nil
}

// Decisions returns a bounded page of decisions.
func (s *AutomationService) Decisions(
	ctx context.Context,
	filter store.AutomationDecisionFilter,
) ([]domain.AutomationDecision, int, error) {
	if !s.Readable() {
		return nil, 0, ErrAutomationDisabled
	}
	return s.store.ListDecisions(ctx, filter)
}

// Summary returns the run-history aggregate.
func (s *AutomationService) Summary(ctx context.Context) (domain.AutomationRunSummary, error) {
	if !s.Readable() {
		return domain.AutomationRunSummary{}, ErrAutomationDisabled
	}
	return s.store.RunSummary(ctx)
}

// Pauses returns the containers automation will not touch.
func (s *AutomationService) Pauses(
	ctx context.Context,
	activeOnly bool,
	page store.Page,
) ([]domain.PausedContainer, int, error) {
	if !s.Readable() {
		return nil, 0, ErrAutomationDisabled
	}
	return s.store.ListPauses(ctx, activeOnly, page)
}

// Upcoming returns what the NEXT pass would do, without doing any of it.
//
// A read-only projection: it runs the same decision function over the same
// evidence and returns the decisions, writing nothing -- no run row, no decision
// rows, no request to any service. That is the difference between this and a
// dry run, which does record what it would have done.
//
// It exists because "what is automation about to do to my estate" is a question
// an operator should be able to ask without leaving a trace, and because the
// answer must come from the same code that will make the real decision rather
// than from a second implementation that can drift from it.
func (s *AutomationService) Upcoming(ctx context.Context) ([]domain.AutomationDecision, error) {
	if !s.Readable() {
		return nil, ErrAutomationDisabled
	}

	policies, err := s.policies.ActivePolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("load update policies: %w", err)
	}
	targets, _, err := s.evidence.Targets(ctx)
	if err != nil {
		return nil, fmt.Errorf("load automation targets: %w", err)
	}
	pauses, err := s.store.ActivePauses(ctx)
	if err != nil {
		return nil, fmt.Errorf("load automation pauses: %w", err)
	}
	pausedBy := make(map[string]domain.PausedContainer, len(pauses))
	for _, pause := range pauses {
		pausedBy[pause.ContainerName] = pause
	}

	now := s.now().UTC()
	decisions := make([]domain.AutomationDecision, 0, len(targets))
	for index, target := range targets {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		input := AutomationInput{
			Target:                  target.Selection,
			ContainerID:             target.ContainerID,
			Policies:                policies,
			Now:                     now,
			RequireApprovalForMajor: s.cfg.RequireApprovalForMajor,
		}
		if pause, paused := pausedBy[target.Selection.Name]; paused {
			input.Pause, input.IsPaused = pause, true
		}
		s.loadContainerEvidence(ctx, &input, pausedBy, policies)

		outcome := DecideAutomation(input)
		outcome.Decision.Position = index
		decisions = append(decisions, outcome.Decision)
	}
	return decisions, nil
}

// ------------------------------------------------------------ commands --

// ErrDecisionNotApprovable reports that a decision cannot be released.
var ErrDecisionNotApprovable = errors.New(
	"this decision is not waiting for approval")

// Approve releases one decision a policy held for a person.
//
// # What an approval is, and what it is not
//
// It is an instruction to submit the acquisition the engine already decided on,
// for the plan the engine already chose. The approver names a DECISION, and
// every identity the submission uses comes off that decision row.
//
// It is NOT a way to update something the engine did not select. There is no
// parameter here for a container, an image, a tag, or a digest, and a decision
// that was not left awaiting approval cannot be approved into existence.
//
// # Why the decision is re-derived rather than trusted
//
// The stored decision may be minutes or hours old. Before submitting, the
// engine re-reads the container's CURRENT plan and refuses if it no longer
// matches the one the decision named: approving a proposal is approving THAT
// proposal, and a registry that republished a tag in the meantime has made it a
// different one.
func (s *AutomationService) Approve(
	ctx context.Context,
	runID, containerName string,
	by domain.Requester,
	actor Actor,
) (domain.AutomationDecision, error) {
	if !s.Enabled() {
		return domain.AutomationDecision{}, ErrAutomationDisabled
	}
	if !by.Known() {
		// Refused rather than recorded as anonymous. An approval whose approver
		// is unknown is not an approval.
		return domain.AutomationDecision{}, errors.New("an approval must name the account that made it")
	}
	// Both identifiers are re-validated by SHAPE here as well as in the handler.
	// The handler is one caller; this is the only place an approval can be made,
	// and an identifier that is not the shape HarborMaster generates cannot name
	// one of its records -- so refusing early keeps arbitrary caller text out of
	// every layer below, including the log line at the end of this function.
	if !domain.ValidAutomationRunID(runID) {
		return domain.AutomationDecision{}, ErrDecisionNotApprovable
	}
	if !domain.ValidContainerName(containerName) {
		return domain.AutomationDecision{}, ErrDecisionNotApprovable
	}

	decisions, _, err := s.store.ListDecisions(ctx, store.AutomationDecisionFilter{
		RunID:         runID,
		ContainerName: containerName,
		Verdicts:      []domain.AutomationVerdict{domain.VerdictAwaitingApproval},
		Page:          store.Page{Limit: 1},
	})
	if err != nil {
		return domain.AutomationDecision{}, err
	}
	if len(decisions) == 0 {
		return domain.AutomationDecision{}, ErrDecisionNotApprovable
	}
	decision := decisions[0]

	if decision.PlanID == "" {
		return domain.AutomationDecision{}, ErrDecisionNotApprovable
	}

	// The world may have moved. Approving a stale proposal is approving a
	// change nobody read.
	current, err := s.evidence.CurrentPlan(ctx, decision.ContainerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.recordAudit(ctx, domain.AuditAutomationRejected, domain.AuditDenied,
			decision.ContainerName, actor,
			"the change plan behind this decision no longer exists")
		return domain.AutomationDecision{}, fmt.Errorf(
			"%w: the change plan behind it no longer exists", ErrDecisionNotApprovable)
	case err != nil:
		return domain.AutomationDecision{}, err
	case current.PlanID != decision.PlanID:
		s.recordAudit(ctx, domain.AuditAutomationRejected, domain.AuditDenied,
			decision.ContainerName, actor,
			"the container's change plan moved on after the decision was made")
		return domain.AutomationDecision{}, fmt.Errorf(
			"%w: a newer change plan supersedes the one it named", ErrDecisionNotApprovable)
	}

	// Paused containers are not approvable. A pause is HarborMaster refusing to
	// keep trying, and clearing it is a separate, deliberate act.
	if pause, err := s.store.PauseFor(ctx, decision.ContainerName); err == nil &&
		pause.Active(s.now()) {
		return domain.AutomationDecision{}, fmt.Errorf(
			"%w: automation is paused for this container", ErrDecisionNotApprovable)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return domain.AutomationDecision{}, err
	}

	acquisition, err := s.pipeline.RequestAcquisition(ctx, AcquisitionRequest{
		PlanID:     decision.PlanID,
		RequestKey: "automation:approve:" + runID + ":" + decision.PlanID,
		// The APPROVER, not the pass. A change a person released is that
		// person's change.
		RequestedBy: by,
	})
	if err != nil {
		var refused ErrAcquisitionRefused
		if errors.As(err, &refused) {
			s.recordAudit(ctx, domain.AuditAutomationRejected, domain.AuditDenied,
				decision.ContainerName, actor,
				"the acquisition preflight refused: "+refused.Refusal.Explain())
			return domain.AutomationDecision{}, err
		}
		return domain.AutomationDecision{}, err
	}

	// The decision is recorded as having acted by attaching the acquisition,
	// which is what makes the follower pick it up on its next tick.
	if err := s.store.PromoteApproved(ctx, runID, decision.ContainerName,
		acquisition.AcquisitionID); err != nil {
		s.logger.ErrorContext(ctx, "could not record an approved decision",
			// Shape-validated above, and sanitised again at the boundary: a
			// log line is read in a terminal, and a bound that is never
			// reached costs nothing.
			slog.String("runId", domain.SanitiseDisplayText(runID, domain.MaxAuditTargetIDBytes)),
			slog.Any("error", err))
	}

	decision.Verdict = domain.VerdictUpdate
	decision.Reason = domain.ReasonEligible
	decision.AcquisitionID = acquisition.AcquisitionID

	s.recordAudit(ctx, domain.AuditAutomationApproved, domain.AuditSucceeded,
		decision.ContainerName, actor,
		"released the automation decision for this container")
	return decision, nil
}

// Resume clears a pause, recording who cleared it.
func (s *AutomationService) Resume(
	ctx context.Context,
	containerName string,
	by domain.Requester,
	actor Actor,
) error {
	if !s.Readable() {
		return ErrAutomationDisabled
	}
	if err := s.store.Resume(ctx, containerName, by, s.now()); err != nil {
		return err
	}
	s.recordAudit(ctx, domain.AuditAutomationResumed, domain.AuditSucceeded,
		containerName, actor, "cleared the automation pause for this container")
	return nil
}

// PauseContainer stops automation for one container by hand.
//
// The container is named by NAME, and the name is checked against the
// inventory before the pause is recorded -- not because a pause is dangerous,
// but because a pause on a container that does not exist is a safety control
// an operator believes in that protects nothing.
func (s *AutomationService) PauseContainer(
	ctx context.Context,
	containerName, detail string,
	actor Actor,
) (domain.PausedContainer, error) {
	if !s.Readable() {
		return domain.PausedContainer{}, ErrAutomationDisabled
	}

	known, err := s.containerKnown(ctx, containerName)
	if err != nil {
		return domain.PausedContainer{}, err
	}
	if !known {
		return domain.PausedContainer{}, store.ErrNotFound
	}

	pause, err := s.store.Pause(ctx, domain.PausedContainer{
		ContainerName: containerName,
		Reason:        domain.PauseOperator,
		Detail:        detail,
		PausedAt:      s.now().UTC(),
	})
	if err != nil {
		return domain.PausedContainer{}, err
	}
	s.recordAudit(ctx, domain.AuditAutomationPaused, domain.AuditSucceeded,
		containerName, actor, "paused automation for this container by hand")
	return pause, nil
}

// containerKnown reports whether the inventory has a present container by that
// name.
func (s *AutomationService) containerKnown(ctx context.Context, name string) (bool, error) {
	targets, _, err := s.evidence.Targets(ctx)
	if err != nil {
		return false, err
	}
	for _, target := range targets {
		if target.Selection.Name == name {
			return true, nil
		}
	}
	return false, nil
}
