package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// errAcquisitionUnavailable stands in for a transient submission failure -- the
// acquisition service busy, its concurrency cap reached, or its database write
// refused. Not a judgement about the container.
var errAcquisitionUnavailable = errors.New("the acquisition could not be submitted")

// The handoff from a reattachment's ACQUISITION to its RECREATION.
//
// # The defect these tests were written for
//
// The follower produced a rebind plan, requested the image, recorded the
// acquisition id, moved the member to `acquired` -- and stopped. There was no
// step five. Nothing in production code ever called RequestExecution for a
// member, so a dependent that had been detached by its provider's replacement
// stayed detached forever, while the operation sat in `rebindRunning` looking
// like work in progress.
//
// Live evidence, Stage 5, Docker 29.7.2:
//
//	operation depop_d3cd0f23e201acdddc00 state=rebindRunning providerVerified=true
//	  member hm16-dependent state=acquired plan=plan_… acq=acq_… exec=''
//	acquisitions for the dependent: 1
//	executions  for the dependent: 0
//
// One acquisition, no execution, for as long as the process ran.
//
// # Why the existing follower tests did not catch it
//
// Because they performed the missing transition themselves. Both
// TestTheFollowerCompletesAnOperationOnlyOnVerifiedExecutions and
// TestTheFollowerFailsTheOperationWithoutUndoingAnything called
// `markMemberExecution(...)` between two ticks -- writing by hand the execution
// id the follower was supposed to have written -- and then asserted that the
// operation concluded correctly from it. Which it did. The half of the machine
// under test was supplied by the test.
//
// And TestASecondFollowTickDoesNotDuplicateRebindWork asserted that after three
// ticks the member was still `acquired`, which pinned the defect as correct.
// That assertion is now the opposite one.
//
// So these tests never write an execution id. Every id asserted on here is one
// the follower chose, and the completion test below reads the member's own
// ExecutionID to decide which record to answer for.
//
// # What the handoff must get right
//
// Requesting a recreation is the step that changes the host, so each of the
// four things an acquisition record can say gets its own test:
//
//	succeeded     request the recreation, exactly once
//	still running wait; a pull in flight is not a pull that finished
//	failed        fail the member; never execute; never retry
//	unreadable    wait; a record that could not be read establishes nothing
//
// The third and fourth are the pair CLAUDE.md invariant 5 is about, and they
// are the two that look identical to a single `if acquisition.State !=
// succeeded`.

// followerAcquisition returns the acquisition id the follower recorded for the
// dependent, failing the test if the earlier steps regressed.
func followerAcquisition(t *testing.T, h *followerHarness, operationID string) string {
	t.Helper()

	member, found := h.dep.memberOf(operationID, depDependent)
	if !found {
		t.Fatal("the member disappeared")
	}
	if member.AcquisitionID == "" {
		t.Fatal("no acquisition was recorded for the reattachment; the steps this " +
			"test builds on have regressed")
	}
	return member.AcquisitionID
}

// The core regression: an acquired reattachment is driven into its recreation.
func TestTheFollowerDrivesAnAcquiredRebindIntoItsRecreation(t *testing.T) {
	t.Parallel()

	harness := newFollowerHarness(t)
	operationID := harness.providerVerified(t)
	ctx := context.Background()

	// Tick 1: the plan is written and the image is requested.
	harness.tick(ctx)
	acquisitionID := followerAcquisition(t, harness, operationID)

	// Tick 2: the acquisition has succeeded, so the recreation is requested.
	harness.tick(ctx)

	requests := harness.pipeline.recorded("execute")
	if len(requests) != 1 {
		t.Fatalf("%d recreations were requested for the reattachment, want 1.\n\n"+
			"A dependent whose provider has been replaced holds a namespace "+
			"reference to a container that no longer exists. The acquisition "+
			"only proves the image is present locally; nothing is reattached "+
			"until the recreation runs. Without this step the member stops at "+
			"`acquired` and the workload stays detached indefinitely.",
			len(requests))
	}

	// Through the EXISTING seam, naming the acquisition and nothing else. Every
	// other detail -- container id, image, digest, namespace target -- is
	// derived by the execution service's own preflight from records
	// HarborMaster wrote, per invariant 10.
	if requests[0].id != acquisitionID {
		t.Fatalf("the recreation named %q, want the member's acquisition %q",
			requests[0].id, acquisitionID)
	}

	member, _ := harness.dep.memberOf(operationID, depDependent)
	if member.ExecutionID == "" {
		t.Fatal("the member records no execution id; a restart would not know a " +
			"recreation had been requested and would request a second one")
	}
	if member.State != domain.MemberExecuting {
		t.Fatalf("member state = %q, want executing", member.State)
	}

	// Requested is not done. Only the execution record clears a member, and
	// this one has not answered yet.
	if member.State.Clears() {
		t.Fatal("a submitted recreation cleared the member")
	}
}

// The whole chain closes: provider verified -> plan -> acquire -> execute ->
// verified -> operation succeeded.
//
// The execution record is answered for the id the FOLLOWER chose, so nothing
// here supplies a transition the production code was supposed to make.
func TestTheFollowerCarriesAReattachmentAllTheWayToVerified(t *testing.T) {
	t.Parallel()

	harness := newFollowerHarness(t)
	operationID := harness.providerVerified(t)
	ctx := context.Background()

	harness.tick(ctx) // plan and acquire
	harness.tick(ctx) // recreate

	member, _ := harness.dep.memberOf(operationID, depDependent)
	if member.ExecutionID == "" {
		t.Fatal("no recreation was requested")
	}

	// That recreation reached verified success -- after the health proof, the
	// digest verification, the preservation comparison and the network check
	// the execution service performs.
	harness.dep.executions.records[member.ExecutionID] = domain.Execution{
		State: domain.ExecutionSucceeded,
	}

	harness.tick(ctx) // conclude

	final, _ := harness.dep.memberOf(operationID, depDependent)
	if final.State != domain.MemberVerified {
		t.Fatalf("member state = %q, want verified", final.State)
	}
	operation, err := harness.dep.operations.Get(ctx, operationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if operation.State != domain.OperationSucceeded {
		t.Fatalf("operation state = %q, want succeeded", operation.State)
	}
}

// An acquisition still in flight does not start a recreation.
//
// The pull is changing the host right now. Recreating the container from an
// image that is still transferring is the exact race the acquisition/execution
// split exists to prevent.
func TestAnUnsettledAcquisitionDoesNotStartARecreation(t *testing.T) {
	t.Parallel()

	for name, state := range map[string]domain.AcquisitionState{
		"queued":     domain.AcquisitionQueued,
		"validating": domain.AcquisitionValidating,
		"pulling":    domain.AcquisitionPulling,
		"verifying":  domain.AcquisitionVerifying,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			harness := newFollowerHarness(t)
			operationID := harness.providerVerified(t)
			ctx := context.Background()

			harness.tick(ctx)
			acquisitionID := followerAcquisition(t, harness, operationID)
			harness.pipeline.setAcquisition(acquisitionID, domain.Acquisition{State: state})

			harness.tick(ctx)
			harness.tick(ctx)

			if requests := harness.pipeline.recorded("execute"); len(requests) != 0 {
				t.Fatalf("%d recreations were requested while the pull was %s",
					len(requests), name)
			}
			member, _ := harness.dep.memberOf(operationID, depDependent)
			if member.State != domain.MemberAcquired {
				t.Fatalf("member state = %q, want acquired; an unfinished pull is "+
					"neither a success nor a failure", member.State)
			}
		})
	}
}

// An acquisition that FAILED never reaches a recreation, and is not retried.
func TestAFailedAcquisitionNeverReachesARecreation(t *testing.T) {
	t.Parallel()

	for name, state := range map[string]domain.AcquisitionState{
		"failed":    domain.AcquisitionFailed,
		"cancelled": domain.AcquisitionCancelled,
		"expired":   domain.AcquisitionExpired,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			harness := newFollowerHarness(t)
			operationID := harness.providerVerified(t)
			ctx := context.Background()

			harness.tick(ctx)
			acquisitionID := followerAcquisition(t, harness, operationID)
			harness.pipeline.setAcquisition(acquisitionID, domain.Acquisition{State: state})

			harness.tick(ctx)

			if requests := harness.pipeline.recorded("execute"); len(requests) != 0 {
				t.Fatalf("%d recreations were requested after the pull %s.\n\n"+
					"Recreating from an image that was never confirmed present "+
					"locally is a recreation that fails with the container "+
					"already stopped.", len(requests), name)
			}

			// The member settled, so the operator is told rather than left
			// watching an operation that will never move.
			member, _ := harness.dep.memberOf(operationID, depDependent)
			if member.State != domain.MemberFailed {
				t.Fatalf("member state = %q, want failed", member.State)
			}
			operation, err := harness.dep.operations.Get(ctx, operationID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if operation.State != domain.OperationFailed {
				t.Fatalf("operation state = %q, want failed", operation.State)
			}
			if operation.Failure != domain.OperationFailureRebind {
				t.Fatalf("failure = %q, want rebindFailed", operation.Failure)
			}

			// And nothing is retried blindly on the next tick: not the pull, not
			// the recreation.
			harness.tick(ctx)
			harness.tick(ctx)
			if requests := harness.pipeline.recorded("acquire"); len(requests) != 1 {
				t.Fatalf("%d acquisitions after a settled failure, want the first one only",
					len(requests))
			}
			if requests := harness.pipeline.recorded("execute"); len(requests) != 0 {
				t.Fatalf("%d recreations after a settled failure", len(requests))
			}

			// No group rollback, here as everywhere.
			if rollbacks := harness.pipeline.recorded("rollback"); len(rollbacks) != 0 {
				t.Fatalf("%d rollbacks were requested; Phase 16 has no group rollback",
					len(rollbacks))
			}
		})
	}
}

// An acquisition record that cannot be READ is not a failed acquisition.
//
// CLAUDE.md invariant 5, in its second half: a check that could not be
// performed establishes nothing, and must not be read as failure. Failing the
// member here would leave the dependent detached and the operation terminal
// over a transient database read -- with a pulled image sitting on the host and
// nothing left to use it.
func TestAnUnreadableAcquisitionIsNotReadAsAFailure(t *testing.T) {
	t.Parallel()

	harness := newFollowerHarness(t)
	operationID := harness.providerVerified(t)
	ctx := context.Background()

	harness.tick(ctx)
	acquisitionID := followerAcquisition(t, harness, operationID)
	harness.pipeline.forgetAcquisition(acquisitionID)

	harness.tick(ctx)

	if requests := harness.pipeline.recorded("execute"); len(requests) != 0 {
		t.Fatalf("%d recreations were requested from an unreadable acquisition",
			len(requests))
	}
	member, _ := harness.dep.memberOf(operationID, depDependent)
	if member.State != domain.MemberAcquired {
		t.Fatalf("member state = %q, want acquired; an unreadable record "+
			"establishes neither success nor failure", member.State)
	}
	if member.State.Settled() {
		t.Fatal("an unreadable acquisition settled the member")
	}
	operation, err := harness.dep.operations.Get(ctx, operationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if operation.State == domain.OperationFailed {
		t.Fatal("the operation failed because a record could not be read")
	}

	// And it recovers: once the record is readable again, the handoff proceeds.
	harness.pipeline.setAcquisition(acquisitionID,
		domain.Acquisition{State: domain.AcquisitionSucceeded})
	harness.tick(ctx)

	if requests := harness.pipeline.recorded("execute"); len(requests) != 1 {
		t.Fatalf("%d recreations once the record was readable again, want 1",
			len(requests))
	}
}

// Repeated ticks request exactly one recreation.
func TestRepeatedTicksRequestOneRecreation(t *testing.T) {
	t.Parallel()

	harness := newFollowerHarness(t)
	operationID := harness.providerVerified(t)
	ctx := context.Background()

	for range 6 {
		harness.tick(ctx)
	}

	if count := harness.dep.plans.count(); count != 1 {
		t.Fatalf("%d rebind plans after six ticks, want 1", count)
	}
	if requests := harness.pipeline.recorded("acquire"); len(requests) != 1 {
		t.Fatalf("%d acquisitions after six ticks, want 1", len(requests))
	}
	if requests := harness.pipeline.recorded("execute"); len(requests) != 1 {
		t.Fatalf("%d recreations after six ticks, want 1.\n\n"+
			"Each one stops and replaces a running container.", len(requests))
	}
	member, _ := harness.dep.memberOf(operationID, depDependent)
	if member.State != domain.MemberExecuting {
		t.Fatalf("member state = %q after repeated ticks, want executing", member.State)
	}
}

// A restart reuses the recorded execution and requests no second one.
//
// The one that matters most: a second recreation is a second stop-and-replace
// of a container that may already have been reattached correctly.
func TestARestartDoesNotRequestASecondRecreation(t *testing.T) {
	t.Parallel()

	harness := newFollowerHarness(t)
	operationID := harness.providerVerified(t)
	ctx := context.Background()

	harness.tick(ctx)
	harness.tick(ctx)

	before, _ := harness.dep.memberOf(operationID, depDependent)
	if before.ExecutionID == "" {
		t.Fatal("instance #1 requested no recreation")
	}
	executions := len(harness.pipeline.recorded("execute"))

	// A brand new engine over the same stores, sharing no field with the one
	// that wrote the rows.
	harness.engine = harness.build()
	harness.tick(ctx)
	harness.tick(ctx)

	if got := len(harness.pipeline.recorded("execute")); got != executions {
		t.Fatalf("recreations %d -> %d across a restart; a container was replaced twice",
			executions, got)
	}
	after, _ := harness.dep.memberOf(operationID, depDependent)
	if after.ExecutionID != before.ExecutionID {
		t.Fatalf("execution id changed across a restart: %q -> %q",
			before.ExecutionID, after.ExecutionID)
	}
}

// A member whose plan already existed still gets its acquisition submitted.
//
// # The second defect in the same function
//
// Step three iterated `result.Created` only -- the plans produced by THIS tick.
// A member whose plan already existed comes back under `Reused`, so on every
// tick after the first it was skipped entirely.
//
// That is invisible on the happy path, because the tick that creates the plan is
// also the tick that submits the acquisition. It matters when that submission
// fails: the plan is durable, so every later tick reports it as reused, and the
// member is never looked at again. One transient error stranded the dependent
// permanently, with a plan on disk and nothing driving it.
//
// The live logs showed exactly this shape -- "could not request the image for a
// reattachment", repeatedly, and then silence once the plan stabilised.
func TestATransientAcquisitionFailureDoesNotStrandTheMember(t *testing.T) {
	t.Parallel()

	harness := newFollowerHarness(t)
	operationID := harness.providerVerified(t)
	ctx := context.Background()

	// Tick 1: the plan is written; the acquisition request fails.
	harness.pipeline.acquireErr = errAcquisitionUnavailable
	harness.tick(ctx)

	member, _ := harness.dep.memberOf(operationID, depDependent)
	if member.PlanID == "" {
		t.Fatal("no plan was written")
	}
	if member.AcquisitionID != "" {
		t.Fatalf("an acquisition id was recorded from a failed request: %q",
			member.AcquisitionID)
	}

	// Tick 2: the transient condition has cleared. The plan is now REUSED
	// rather than created, and the member must still be picked up.
	harness.pipeline.acquireErr = nil
	harness.tick(ctx)

	retried, _ := harness.dep.memberOf(operationID, depDependent)
	if retried.AcquisitionID == "" {
		t.Fatal("the member was never retried after a transient acquisition " +
			"failure; its plan is durable, so every later tick reports it as " +
			"reused and it is skipped forever")
	}
	if retried.PlanID != member.PlanID {
		t.Fatalf("a second plan was written: %q -> %q", member.PlanID, retried.PlanID)
	}
	if count := harness.dep.plans.count(); count != 1 {
		t.Fatalf("%d rebind plans, want 1", count)
	}

	// And it proceeds normally from there.
	harness.tick(ctx)
	if requests := harness.pipeline.recorded("execute"); len(requests) != 1 {
		t.Fatalf("%d recreations, want 1", len(requests))
	}
}
