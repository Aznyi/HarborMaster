package service_test

import (
	"context"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Test I: two dependents, one reattaches, one does not.
//
// # The state this leaves the host in, and why it is the right one
//
// The provider is replaced. One dependent is attached to the replacement. The
// other is still attached to a namespace that no longer exists.
//
// The tempting response is to undo something. HarborMaster does not, and the
// reasoning is in domain.GroupRollbackIsNotPerformed: rolling the provider back
// would break the dependent that is currently CORRECT, and reverting a
// successful rebind is a third unattended mutation decided at the moment
// HarborMaster has just demonstrated its model of the host was wrong.
//
// So the operation fails, the operator is told exactly which container is still
// detached, and nothing else moves. This test pins every part of that.

// twoDependentHarness is a provider with two mandatory hard dependents.
func newTwoDependentHarness(t *testing.T) *followerHarness {
	t.Helper()

	harness := newFollowerHarness(t)

	// The second dependent, also bound to the provider's original identity.
	harness.dep.store.setNamespace(depDependent2, dependent2ID,
		"container:"+providerAID, true)
	harness.dep.store.endpoints = append(harness.dep.store.endpoints,
		domain.DependencyEndpoint{
			Name: depDependent2, ContainerID: dependent2ID,
			ImageRef: depImageRef, Present: true,
		})
	return harness
}

func TestTwoDependentPartialRebindFailure(t *testing.T) {
	t.Parallel()

	const (
		executionA = "exec_0123456789abcdef0a01"
		executionB = "exec_0123456789abcdef0b01"
	)

	harness := newTwoDependentHarness(t)
	operationID := harness.providerVerified(t)
	ctx := context.Background()

	// ---- tick 1: both dependents get plans and acquisitions ----------------

	harness.tick(ctx)

	// POSITIVE CONTROL. If this is zero the rest of the test proves nothing:
	// "no duplicate work" is trivially true when no work happened at all.
	if plans := harness.dep.plans.count(); plans != 2 {
		t.Fatalf("%d rebind plans, want 2 (one per mandatory dependent)", plans)
	}
	acquisitions := harness.pipeline.recorded("acquire")
	if len(acquisitions) != 2 {
		t.Fatalf("%d acquisitions, want 2", len(acquisitions))
	}

	memberA, foundA := harness.dep.memberOf(operationID, depDependent)
	memberB, foundB := harness.dep.memberOf(operationID, depDependent2)
	if !foundA || !foundB {
		t.Fatal("a member disappeared")
	}
	if memberA.PlanID == "" || memberB.PlanID == "" {
		t.Fatalf("a member carries no plan: A=%q B=%q", memberA.PlanID, memberB.PlanID)
	}
	if memberA.PlanID == memberB.PlanID {
		t.Fatal("both dependents share one plan; each needs its own recreation")
	}
	if memberA.AcquisitionID == "" || memberB.AcquisitionID == "" {
		t.Fatalf("a member carries no acquisition: A=%q B=%q",
			memberA.AcquisitionID, memberB.AcquisitionID)
	}

	// ---- the executions: A verifies, B fails -------------------------------

	harness.dep.executions.records[executionA] = domain.Execution{
		State: domain.ExecutionSucceeded,
	}
	harness.dep.executions.records[executionB] = domain.Execution{
		State: domain.ExecutionFailed,
	}
	harness.dep.operations.markMemberExecution(operationID, depDependent,
		executionA, domain.MemberExecuting)
	harness.dep.operations.markMemberExecution(operationID, depDependent2,
		executionB, domain.MemberExecuting)

	// ---- tick 2: the outcome is derived from those records -----------------

	harness.tick(ctx)

	operation, err := harness.dep.operations.Get(ctx, operationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// The operation FAILED, for the rebind's reason.
	if operation.State != domain.OperationFailed {
		t.Fatalf("operation state = %q, want failed", operation.State)
	}
	if operation.Failure != domain.OperationFailureRebind {
		t.Fatalf("failure = %q, want rebindFailed", operation.Failure)
	}
	if operation.State == domain.OperationSucceeded {
		t.Fatal("the operation succeeded with a dependent still detached")
	}
	if !operation.State.NeedsOperator() {
		t.Fatal("a failed operation does not report that it needs a person")
	}

	// A is verified; B is not cleared.
	finalA, _ := harness.dep.memberOf(operationID, depDependent)
	finalB, _ := harness.dep.memberOf(operationID, depDependent2)
	if finalA.State != domain.MemberVerified {
		t.Fatalf("A state = %q, want verified", finalA.State)
	}
	if finalB.State.Clears() {
		t.Fatalf("B state = %q, which clears; a failed reattachment must not", finalB.State)
	}
	if finalB.State != domain.MemberFailed {
		t.Fatalf("B state = %q, want failed", finalB.State)
	}

	// ---- NOTHING was undone ------------------------------------------------

	if rollbacks := harness.pipeline.recorded("rollback"); len(rollbacks) != 0 {
		t.Fatalf("%d rollbacks requested; Phase 16 performs no group rollback and no "+
			"provider rollback: reverting the provider would break A, which is "+
			"currently correct", len(rollbacks))
	}

	// ---- and a further tick repeats nothing ---------------------------------

	plansBefore := harness.dep.plans.count()
	acquisitionsBefore := len(harness.pipeline.recorded("acquire"))
	executionsBefore := len(harness.pipeline.recorded("execute"))

	harness.tick(ctx)
	harness.tick(ctx)

	if got := harness.dep.plans.count(); got != plansBefore {
		t.Errorf("plans %d -> %d after further ticks; work was duplicated",
			plansBefore, got)
	}
	if got := len(harness.pipeline.recorded("acquire")); got != acquisitionsBefore {
		t.Errorf("acquisitions %d -> %d after further ticks", acquisitionsBefore, got)
	}
	if got := len(harness.pipeline.recorded("execute")); got != executionsBefore {
		t.Errorf("executions %d -> %d after further ticks", executionsBefore, got)
	}

	// A is untouched: same plan, same acquisition, same execution, still verified.
	afterA, _ := harness.dep.memberOf(operationID, depDependent)
	if afterA.PlanID != finalA.PlanID || afterA.AcquisitionID != finalA.AcquisitionID ||
		afterA.ExecutionID != finalA.ExecutionID {
		t.Errorf("the verified dependent was touched again: %+v -> %+v", finalA, afterA)
	}
	if afterA.State != domain.MemberVerified {
		t.Errorf("A state changed after completion: %q", afterA.State)
	}

	// B is not blindly retried.
	afterB, _ := harness.dep.memberOf(operationID, depDependent2)
	if afterB.State != domain.MemberFailed {
		t.Errorf("B state = %q after further ticks, want failed", afterB.State)
	}

	// The operation stays terminal.
	if final, _ := harness.dep.operations.Get(ctx, operationID); final.State != domain.OperationFailed {
		t.Errorf("operation state = %q after further ticks, want failed", final.State)
	}
}

// The provider itself is never rolled back by the dependency machinery.
//
// Asserted separately because it is the single most consequential thing this
// subsystem must not do: the provider's replacement is what the successfully
// rebound dependent is attached to.
func TestAFailedRebindNeverRollsTheProviderBack(t *testing.T) {
	t.Parallel()

	const executionB = "exec_0123456789abcdef0b02"

	harness := newFollowerHarness(t)
	operationID := harness.providerVerified(t)
	ctx := context.Background()

	harness.tick(ctx)

	harness.dep.executions.records[executionB] = domain.Execution{
		State: domain.ExecutionFailed,
	}
	harness.dep.operations.markMemberExecution(operationID, depDependent,
		executionB, domain.MemberExecuting)

	harness.tick(ctx)
	harness.tick(ctx)

	if rollbacks := harness.pipeline.recorded("rollback"); len(rollbacks) != 0 {
		t.Fatalf("%d rollbacks requested after a failed reattachment", len(rollbacks))
	}

	// The provider's execution record is untouched by the dependency code.
	operation, _ := harness.dep.operations.Get(ctx, operationID)
	if operation.ProviderExecutionID != depExecutionA {
		t.Fatalf("the provider execution changed: %q", operation.ProviderExecutionID)
	}
	recovered, err := harness.dep.service().RecoverOperation(ctx, operationID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !recovered.ProviderVerified {
		t.Fatal("the provider stopped being verified after a dependent failed")
	}
}
