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

// Plan approval: recording that a person reviewed one immutable change plan.
//
// # What this service is allowed to be
//
// A reader of plans and a writer of approvals. That is the whole of it.
//
// It holds no Docker capability -- there is nowhere on its options struct to put
// one -- and it creates no plan, no acquisition and no execution. Approving is
// not applying: the operator still drives the ordinary pipeline afterwards, and
// every preflight in it still runs. Architecture guards hold both halves.
//
// # What an approval means, and what it cannot mean
//
// Exactly one proposition:
//
//	the human review this plan asks for has happened
//
// It does not lower the plan's risk score, remove a factor, or change its
// recommendation. The plan is insert-only and stays as the planner wrote it, so
// the assessment an operator reviewed remains the assessment that authorised the
// change -- which is the only way the audit trail means anything.
//
// Everything else that decides whether a container may be replaced is still
// decided by the execution preflight, immediately before anything is stopped.

// PlanApprovalStore is the persistence this service needs.
//
// Four methods, all narrow. Nothing here can reach a plan, a container, or the
// host.
type PlanApprovalStore interface {
	Approve(ctx context.Context, approval domain.PlanApproval, at time.Time) (domain.PlanApproval, error)
	Active(ctx context.Context, planID string) (domain.PlanApproval, error)
	Revoke(ctx context.Context, planID string, by domain.Requester, at time.Time) error
	// PlanHasMutated is the derived consumption rule: whether an execution of
	// this plan has already changed the host.
	PlanHasMutated(ctx context.Context, planID string) (bool, error)
}

// PlanApprovalPlans reads the immutable evidence an approval is bound to.
//
// READS only. This service must never be able to write a plan: an approval that
// could rewrite the thing it approves is the failure this whole design exists to
// prevent.
type PlanApprovalPlans interface {
	Get(ctx context.Context, planID string) (domain.ChangePlan, error)
	Current(ctx context.Context, containerID string) (domain.ChangePlan, error)
}

// PlanApprovalOptions configures the service.
//
// Note what is absent: no Docker runtime, no mutator, no acquisition or
// execution pipeline, no registry client. There is nowhere to put one.
type PlanApprovalOptions struct {
	Store  PlanApprovalStore
	Plans  PlanApprovalPlans
	Audit  *AuditRecorder
	Now    func() time.Time
	Logger *slog.Logger
}

// PlanApprovalService records and validates human approvals of change plans.
type PlanApprovalService struct {
	store  PlanApprovalStore
	plans  PlanApprovalPlans
	audit  *AuditRecorder
	now    func() time.Time
	logger *slog.Logger
}

// NewPlanApprovalService builds the service.
func NewPlanApprovalService(opts PlanApprovalOptions) *PlanApprovalService {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &PlanApprovalService{
		store:  opts.Store,
		plans:  opts.Plans,
		audit:  opts.Audit,
		now:    now,
		logger: logger,
	}
}

// ErrPlanNotApprovable reports that this plan cannot be approved.
var ErrPlanNotApprovable = errors.New("this plan cannot be approved")

// Approve records that a person reviewed one plan.
//
// # What is checked before the row is written
//
// The plan must exist, must ASK for review, and must still be the container's
// current plan. The last one matters most: approving a superseded plan would
// record a judgement about evidence that has already been replaced, and the
// operator would believe they had authorised something they had not.
//
// Nothing is re-derived. No registry is called, no planning is run, no snapshot
// is captured. Approval is a judgement about evidence the planner already
// stored; establishing fresh evidence is the execution preflight's job and it
// does it immediately before acting.
func (s *PlanApprovalService) Approve(
	ctx context.Context,
	planID string,
	by domain.Requester,
	actor Actor,
) (domain.PlanApproval, error) {
	plan, err := s.plans.Get(ctx, planID)
	if err != nil {
		return domain.PlanApproval{}, err
	}

	if !domain.PlanApprovable(plan.Risk.Recommendation) {
		s.record(ctx, domain.AuditPlanApproved, domain.AuditDenied, plan, actor,
			domain.PlanApprovalRefusalNotApprovable.Explain())
		return domain.PlanApproval{}, fmt.Errorf("%w: %s",
			ErrPlanNotApprovable, domain.PlanApprovalRefusalNotApprovable.Explain())
	}

	if refusal := s.supersededBy(ctx, plan); refusal != domain.PlanApprovalRefusalNone {
		s.record(ctx, domain.AuditPlanApproved, domain.AuditDenied, plan, actor,
			refusal.Explain())
		return domain.PlanApproval{}, fmt.Errorf("%w: %s", ErrPlanNotApprovable, refusal.Explain())
	}

	approval, err := s.store.Approve(ctx, domain.PlanApproval{
		PlanID: plan.PlanID,
		// Copied from the PLAN, never from a caller. See the migration header.
		ApprovedInputDigest:    plan.InputDigest,
		ApprovedProposedDigest: plan.ProposedDigest,
		ApprovedBy:             by,
	}, s.now().UTC())

	switch {
	case errors.Is(err, store.ErrPlanApprovalActive):
		// Idempotent by intent: a second approval of the same plan is the same
		// authorisation, not a new one. Hand back what already stands rather
		// than failing a double-click.
		existing, readErr := s.store.Active(ctx, plan.PlanID)
		if readErr != nil {
			return domain.PlanApproval{}, readErr
		}
		return existing, nil
	case err != nil:
		return domain.PlanApproval{}, err
	}

	s.record(ctx, domain.AuditPlanApproved, domain.AuditSucceeded, plan, actor,
		"reviewed and approved this change plan")
	return approval, nil
}

// Get returns the plan's standing approval and whether it is currently valid.
//
// The refusal is returned alongside rather than instead of the approval: an
// operator looking at a plan whose approval has been overtaken needs to see both
// that somebody approved it and why that no longer authorises anything.
func (s *PlanApprovalService) Get(
	ctx context.Context,
	planID string,
) (domain.PlanApproval, domain.PlanApprovalRefusal, error) {
	plan, err := s.plans.Get(ctx, planID)
	if err != nil {
		return domain.PlanApproval{}, "", err
	}

	approval, err := s.store.Active(ctx, planID)
	if err != nil {
		return domain.PlanApproval{}, "", err
	}

	refusal, err := s.validate(ctx, approval, plan)
	if err != nil {
		return domain.PlanApproval{}, "", err
	}
	return approval, refusal, nil
}

// Revoke withdraws a standing approval.
//
// It does not touch the plan, and it does not stop an execution that has already
// begun changing the host -- there is no safe cancellation of a recreation in
// flight, and pretending otherwise would be worse than saying so. What it does
// is remove the authority for any FUTURE attempt, which the execution preflight
// then refuses as approvalMissing.
func (s *PlanApprovalService) Revoke(
	ctx context.Context,
	planID string,
	by domain.Requester,
	actor Actor,
) error {
	plan, err := s.plans.Get(ctx, planID)
	if err != nil {
		return err
	}
	if err := s.store.Revoke(ctx, planID, by, s.now().UTC()); err != nil {
		return err
	}
	s.record(ctx, domain.AuditPlanApprovalRevoked, domain.AuditSucceeded, plan, actor,
		"withdrew the approval for this change plan")
	return nil
}

// ApprovalFor is what the EXECUTION PREFLIGHT asks.
//
// # One validator, and this is it
//
// The preflight, the read endpoint and the UI must agree about what "approved"
// means. They do because they all end up here, and here ends up in the pure
// domain.PlanApprovalValid -- so the rule can be exercised exhaustively without
// a database and cannot be reimplemented differently in two places.
//
// Returns the empty refusal when the plan is genuinely approved. Any other value
// is a reason the execution must refuse.
func (s *PlanApprovalService) ApprovalFor(
	ctx context.Context,
	plan domain.ChangePlan,
) (domain.PlanApprovalRefusal, error) {
	approval, err := s.store.Active(ctx, plan.PlanID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// No approval at all. Not an error: it is the ordinary state of a plan
		// nobody has reviewed yet, and the preflight turns it into a refusal
		// that names the remedy.
		return domain.PlanApprovalRefusalRevoked, nil
	case err != nil:
		return "", err
	}
	return s.validate(ctx, approval, plan)
}

// validate applies the whole rule, including the derived consumption check.
func (s *PlanApprovalService) validate(
	ctx context.Context,
	approval domain.PlanApproval,
	plan domain.ChangePlan,
) (domain.PlanApprovalRefusal, error) {
	// Whether this plan has already changed the host. Read from the executions
	// table rather than stored on the approval, so a spent approval cannot look
	// live because a flag was not written.
	mutated, err := s.store.PlanHasMutated(ctx, plan.PlanID)
	if err != nil {
		// Could not establish that the host is untouched, so it is treated as
		// touched. Fails closed: the cost is one extra human decision, and the
		// alternative is a second unattended recreation on a spent approval.
		s.logger.WarnContext(ctx,
			"could not establish whether an approved plan has already been applied; "+
				"treating the approval as spent",
			slog.Any("error", err))
		mutated = true
	}

	currentID := ""
	if current, err := s.plans.Current(ctx, plan.ContainerID); err == nil {
		currentID = current.PlanID
	}

	return domain.PlanApprovalValid(approval, plan, currentID, mutated), nil
}

// supersededBy reports whether a newer plan has replaced this one.
func (s *PlanApprovalService) supersededBy(
	ctx context.Context,
	plan domain.ChangePlan,
) domain.PlanApprovalRefusal {
	current, err := s.plans.Current(ctx, plan.ContainerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// The container has no current plan at all, so this one cannot be it.
		return domain.PlanApprovalRefusalSuperseded
	case err != nil:
		// Cannot establish currency. Refuses rather than approving something
		// that may already have been replaced.
		return domain.PlanApprovalRefusalSuperseded
	case current.PlanID != plan.PlanID:
		return domain.PlanApprovalRefusalSuperseded
	default:
		return domain.PlanApprovalRefusalNone
	}
}

// record writes the audit row.
//
// The container name comes from the PLAN, never from a caller, and the reason is
// HarborMaster's own sentence from the closed vocabulary. Nothing a requester
// typed reaches the audit log, because nothing a requester types is accepted.
func (s *PlanApprovalService) record(
	ctx context.Context,
	action domain.AuditAction,
	outcome domain.AuditOutcome,
	plan domain.ChangePlan,
	actor Actor,
	reason string,
) {
	if s.audit == nil {
		return
	}
	s.audit.RecordAction(ctx, actor, action, outcome,
		domain.AuditTargetPlan, plan.PlanID, plan.ContainerName, reason)
}
