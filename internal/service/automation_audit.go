package service

import (
	"context"
	"strconv"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Automation's audit trail.
//
// # Why an automated change is audited more, not less
//
// Every other write path in HarborMaster answers "who asked". Automation is the
// one where the answer is "nobody did", and that makes the trail more important
// rather than less: the only account of why the host changed at 02:14 is the
// one HarborMaster wrote at 02:14.
//
// # What is recorded here, and what deliberately is not
//
// Recorded here: the administration of the policies, the passes, and the safety
// interventions -- pauses, resumes, approvals.
//
// NOT recorded here: the mutations. Those are already audited by the
// acquisition, execution, and rollback services, because automation reaches
// exactly the same code path an operator's request reaches. Auditing them again
// would make the "host changes" counter over-report the one number an
// administrator most needs to be able to trust.
//
// # Bounded and detached
//
// Every write goes through GraceContext, so an audit write cannot outlive
// shutdown and cannot fail the operation it describes. An audit that could
// block a rollback would be a safety control that made the system less safe.

// automationAuditGrace bounds a detached audit write.
const automationAuditGrace = 5 * time.Second

// recordAudit appends one automation audit event.
//
// Nil-safe: a test that is not about attribution wires no recorder, and the
// engine must not branch on that at every call site.
func (s *AutomationService) recordAudit(
	ctx context.Context,
	action domain.AuditAction,
	outcome domain.AuditOutcome,
	targetID string,
	actor Actor,
	reason string,
) {
	if s.audit == nil {
		return
	}

	targetType := domain.AuditTargetAutomation
	if action == domain.AuditUpdatePolicyCreated ||
		action == domain.AuditUpdatePolicyUpdated ||
		action == domain.AuditUpdatePolicyArchived {
		targetType = domain.AuditTargetUpdatePolicy
	}

	// Detached and bounded. The caller's context may be a pass that is about to
	// end; the record of what it did must survive that.
	writeCtx, cancel := GraceContext(ctx, automationAuditGrace, automationAuditGrace)
	defer cancel()

	s.audit.RecordAction(writeCtx, actor, action, outcome, targetType,
		// The target id is a run id, a policy id, or a container NAME, all of
		// which HarborMaster generated or read from its own inventory. Bounded
		// anyway, because a bound that is never reached costs nothing.
		domain.SanitiseDisplayText(targetID, domain.MaxAuditTargetIDBytes),
		"", reason)
}

// auditRunOutcome records what a finished pass did.
//
// Only for a pass that CHANGED something, or that failed. A scheduled pass that
// found a closed window and did nothing writes its run row and stops there:
// ninety-six identical "nothing happened" audit events a day would bury the one
// that matters.
func (s *AutomationService) auditRunOutcome(
	ctx context.Context,
	run domain.AutomationRun,
	request passRequest,
) {
	if s.audit == nil {
		return
	}
	if run.State != domain.RunFailed && run.Submitted == 0 {
		return
	}

	action := domain.AuditAutomationRunCompleted
	outcome := domain.AuditSucceeded
	if run.State == domain.RunFailed {
		action = domain.AuditAutomationRunFailed
		outcome = domain.AuditFailed
	}

	// HarborMaster's own sentence, built from its own counters. No error text,
	// no daemon string, no caller input.
	reason := "considered " + strconv.Itoa(run.Considered) +
		", submitted " + strconv.Itoa(run.Submitted) +
		", skipped " + strconv.Itoa(run.Skipped) +
		", failed " + strconv.Itoa(run.Failed)
	if run.DryRun {
		reason = "dry run: " + reason
	}

	s.recordAudit(ctx, action, outcome, run.RunID,
		requesterActor(request.requestedBy), reason)
}
