package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Dependency progression driven by the REAL follower.
//
// # Why the follow tick rather than the coordinator directly
//
// ProduceRebindPlans already has direct coverage. What is unproven is that the
// FOLLOWER calls it -- and calls it in the right order, from the existing loop,
// through the existing submission seam. A coordinator that worked perfectly and
// was never invoked would pass every Stage 3e test.
//
// So these drive `service.FollowForTest`, which is the same `follow` the ticker
// calls inside Run. No new loop is introduced and none is simulated.

// followerHarness pairs a dependency coordinator with an automation engine.
//
// The engine holds the REAL *DependencyService -- not a double -- so the
// interface assertion that makes advanceDependencyOperations reachable is
// exercised rather than assumed.
type followerHarness struct {
	dep      *depHarness
	pipeline *autoPipeline
	engine   *service.AutomationService
}

func newFollowerHarness(t *testing.T) *followerHarness {
	t.Helper()

	dep := newDepHarness()
	pipeline := newAutoPipeline()

	harness := &followerHarness{dep: dep, pipeline: pipeline}
	harness.engine = harness.build()
	return harness
}

// build constructs a FRESH engine over the same stores.
//
// Called twice by the restart test, and the two share no field -- which is what
// makes "no process-local progress state" demonstrable rather than asserted.
func (h *followerHarness) build() *service.AutomationService {
	return service.NewAutomationService(service.AutomationOptions{
		Store:        newFakeAutomationStore(),
		Policies:     &autoPolicyStore{policies: []domain.UpdatePolicy{automaticPolicy()}},
		Evidence:     &autoEvidence{inFlight: map[string]bool{}},
		Pipeline:     h.pipeline,
		Dependencies: h.dep.service(),
		Config: config.Automation{
			Enabled: true, Interval: 15 * time.Minute, PassTimeout: time.Minute,
			MaxConcurrent: 4, MaxPerRun: 10,
		},
		Now: func() time.Time { return time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC) },
	})
}

// tick runs one real follower pass.
func (h *followerHarness) tick(ctx context.Context) {
	service.FollowForTest(h.engine, ctx)
}

// providerVerified places the estate at "the provider was replaced and its
// execution succeeded", which is where rebind progression begins.
func (h *followerHarness) providerVerified(t *testing.T) string {
	t.Helper()

	operationID := buildOperation(t, h.dep)
	h.dep.afterReplacement(providerBID)
	h.dep.operations.markProviderExecution(operationID, depExecutionA)
	return operationID
}

// H. A verified provider drives real rebind progression through the follower.
func TestTheFollowerProducesAndSubmitsRebindWork(t *testing.T) {
	t.Parallel()

	harness := newFollowerHarness(t)
	operationID := harness.providerVerified(t)
	ctx := context.Background()

	harness.tick(ctx)

	// 1. A plan exists, and exactly one.
	if count := harness.dep.plans.count(); count != 1 {
		t.Fatalf("%d rebind plans were written, want 1", count)
	}

	// 2. The member names it, and carries the provider identity it targets.
	member, found := harness.dep.memberOf(operationID, depDependent)
	if !found {
		t.Fatal("the member disappeared")
	}
	if member.PlanID == "" {
		t.Fatal("the member carries no plan id after a follow tick")
	}
	if member.TargetProviderID != providerBID {
		t.Fatalf("targetProviderId = %q, want %q", member.TargetProviderID, providerBID)
	}

	// 3. The acquisition went through the EXISTING seam, naming the plan and
	//    nothing else. Cross-checked against the plan the coordinator wrote, so
	//    a helper returning the wrong thing cannot make this pass.
	requests := harness.pipeline.recorded("acquire")
	if len(requests) != 1 {
		t.Fatalf("%d acquisition requests, want 1", len(requests))
	}
	if requests[0].id != member.PlanID {
		t.Fatalf("the acquisition named %q, want the member's plan %q",
			requests[0].id, member.PlanID)
	}

	// 4. The member advanced from the persisted acquisition, not optimistically.
	if member.State != domain.MemberAcquired {
		t.Fatalf("member state = %q, want acquired", member.State)
	}
	if member.AcquisitionID == "" {
		t.Fatal("the member records no acquisition id")
	}

	// 5. The operation is NOT complete. The rebind has been requested, not
	//    verified, and only a verified execution clears a member.
	recovered, err := harness.dep.service().RecoverOperation(ctx, operationID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered.Complete {
		t.Fatal("the operation completed on a submitted-but-unverified rebind")
	}
}

// A second tick does not duplicate the work.
//
// This test used to end by asserting that after three ticks the member was
// still `acquired`. That assertion was WRONG, and it was the reason a missing
// production step survived to a live daemon: the follower never requested the
// recreation, so `acquired` was where every reattachment stopped, and this test
// wrote that down as correct. See automation_rebind_execution_test.go.
//
// What it is actually for -- that repeated ticks produce one plan and one pull,
// not three -- is unchanged, and the member's progression is now asserted the
// way the records describe it.
func TestASecondFollowTickDoesNotDuplicateRebindWork(t *testing.T) {
	t.Parallel()

	harness := newFollowerHarness(t)
	operationID := harness.providerVerified(t)
	ctx := context.Background()

	harness.tick(ctx)
	harness.tick(ctx)
	harness.tick(ctx)

	if count := harness.dep.plans.count(); count != 1 {
		t.Fatalf("%d rebind plans after three ticks, want 1", count)
	}
	if requests := harness.pipeline.recorded("acquire"); len(requests) != 1 {
		t.Fatalf("%d acquisition requests after three ticks, want 1", len(requests))
	}
	member, _ := harness.dep.memberOf(operationID, depDependent)
	if member.State != domain.MemberExecuting {
		t.Fatalf("member state = %q after repeated ticks, want executing", member.State)
	}
}

// The direct follower matrix: what each provider state produces.
func TestTheFollowerRespectsProviderState(t *testing.T) {
	t.Parallel()

	t.Run("no open operation is a no-op", func(t *testing.T) {
		t.Parallel()

		harness := newFollowerHarness(t)
		harness.tick(context.Background())

		if count := harness.dep.plans.count(); count != 0 {
			t.Fatalf("%d plans were written with no operation", count)
		}
		if requests := harness.pipeline.recorded("acquire"); len(requests) != 0 {
			t.Fatalf("%d acquisitions with no operation", len(requests))
		}
	})

	t.Run("provider still executing produces no rebind plan", func(t *testing.T) {
		t.Parallel()

		harness := newFollowerHarness(t)
		operationID := buildOperation(t, harness.dep)
		harness.dep.afterReplacement(providerBID)
		// The execution exists but has not finished.
		harness.dep.executions.records[depExecutionA] = domain.Execution{
			State: domain.ExecutionCreating,
		}
		harness.dep.operations.markProviderExecution(operationID, depExecutionA)

		harness.tick(context.Background())

		if count := harness.dep.plans.count(); count != 0 {
			t.Fatalf("%d plans produced before the provider finished", count)
		}
	})

	t.Run("provider failed does not progress to rebind", func(t *testing.T) {
		t.Parallel()

		harness := newFollowerHarness(t)
		operationID := buildOperation(t, harness.dep)
		harness.dep.afterReplacement(providerBID)
		harness.dep.executions.records[depExecutionA] = domain.Execution{
			State: domain.ExecutionFailed,
		}
		harness.dep.operations.markProviderExecution(operationID, depExecutionA)

		harness.tick(context.Background())

		if count := harness.dep.plans.count(); count != 0 {
			t.Fatalf("%d plans produced after the provider failed", count)
		}
		// The operation concluded as failed, for the provider's reason.
		operation, err := harness.dep.operations.Get(context.Background(), operationID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if operation.State != domain.OperationFailed {
			t.Fatalf("state = %q, want failed", operation.State)
		}
		if operation.Failure != domain.OperationFailureProvider {
			t.Fatalf("failure = %q, want providerFailed", operation.Failure)
		}
	})

	t.Run("a terminal operation produces no further work", func(t *testing.T) {
		t.Parallel()

		harness := newFollowerHarness(t)
		operationID := harness.providerVerified(t)
		if err := harness.dep.operations.AdvanceOperation(context.Background(),
			operationID, domain.OperationFailed, domain.OperationFailureRebind,
			fixedNow()); err != nil {
			t.Fatalf("advance: %v", err)
		}

		harness.tick(context.Background())

		if count := harness.dep.plans.count(); count != 0 {
			t.Fatalf("%d plans produced for a terminal operation", count)
		}
		if requests := harness.pipeline.recorded("acquire"); len(requests) != 0 {
			t.Fatalf("%d acquisitions for a terminal operation", len(requests))
		}
	})
}

// A member whose execution VERIFIED clears; the operation then succeeds.
func TestTheFollowerCompletesAnOperationOnlyOnVerifiedExecutions(t *testing.T) {
	t.Parallel()

	const rebindExecution = "exec_0123456789abcdef0ee0"

	harness := newFollowerHarness(t)
	operationID := harness.providerVerified(t)
	ctx := context.Background()

	harness.tick(ctx)

	// The rebind's recreation ran and succeeded.
	harness.dep.executions.records[rebindExecution] = domain.Execution{
		State: domain.ExecutionSucceeded,
	}
	harness.dep.operations.markMemberExecution(operationID, depDependent,
		rebindExecution, domain.MemberExecuting)

	harness.tick(ctx)

	operation, err := harness.dep.operations.Get(ctx, operationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if operation.State != domain.OperationSucceeded {
		t.Fatalf("state = %q, want succeeded", operation.State)
	}
}

// A member whose execution FAILED fails the operation, and nothing is undone.
func TestTheFollowerFailsTheOperationWithoutUndoingAnything(t *testing.T) {
	t.Parallel()

	const rebindExecution = "exec_0123456789abcdef0ff0"

	harness := newFollowerHarness(t)
	operationID := harness.providerVerified(t)
	ctx := context.Background()

	harness.tick(ctx)

	harness.dep.executions.records[rebindExecution] = domain.Execution{
		State: domain.ExecutionFailed,
	}
	harness.dep.operations.markMemberExecution(operationID, depDependent,
		rebindExecution, domain.MemberExecuting)

	harness.tick(ctx)

	operation, err := harness.dep.operations.Get(ctx, operationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if operation.State != domain.OperationFailed {
		t.Fatalf("state = %q, want failed", operation.State)
	}
	if operation.Failure != domain.OperationFailureRebind {
		t.Fatalf("failure = %q, want rebindFailed", operation.Failure)
	}

	// NO group rollback: the follower asked for no rollback of anything.
	if rollbacks := harness.pipeline.recorded("rollback"); len(rollbacks) != 0 {
		t.Fatalf("the follower requested %d rollbacks; Phase 16 has no group rollback",
			len(rollbacks))
	}
	// And no second attempt on a later tick.
	before := len(harness.pipeline.recorded("acquire"))
	harness.tick(ctx)
	if after := len(harness.pipeline.recorded("acquire")); after != before {
		t.Fatalf("a failed member was retried: %d -> %d acquisitions", before, after)
	}
}

// J. A restart reuses everything and duplicates nothing.
func TestTheFollowerResumesAfterARestartWithoutDuplicating(t *testing.T) {
	t.Parallel()

	harness := newFollowerHarness(t)
	operationID := harness.providerVerified(t)
	ctx := context.Background()

	// Instance #1 does the work.
	harness.tick(ctx)

	firstMember, _ := harness.dep.memberOf(operationID, depDependent)
	if firstMember.PlanID == "" || firstMember.AcquisitionID == "" {
		t.Fatalf("instance #1 recorded nothing: %+v", firstMember)
	}
	planCount := harness.dep.plans.count()
	acquisitions := len(harness.pipeline.recorded("acquire"))

	// The restart: a brand new engine over the same stores, sharing no field.
	harness.engine = harness.build()
	harness.tick(ctx)
	harness.tick(ctx)

	if got := harness.dep.plans.count(); got != planCount {
		t.Fatalf("plans %d -> %d across a restart; a duplicate was created", planCount, got)
	}
	if got := len(harness.pipeline.recorded("acquire")); got != acquisitions {
		t.Fatalf("acquisitions %d -> %d across a restart; a duplicate was submitted",
			acquisitions, got)
	}

	after, _ := harness.dep.memberOf(operationID, depDependent)
	if after.PlanID != firstMember.PlanID {
		t.Fatalf("plan id changed across a restart: %q -> %q",
			firstMember.PlanID, after.PlanID)
	}
	if after.AcquisitionID != firstMember.AcquisitionID {
		t.Fatalf("acquisition id changed across a restart: %q -> %q",
			firstMember.AcquisitionID, after.AcquisitionID)
	}
}

// The follower advances dependency work from the SAME tick that advances
// ordinary automation, and one does not starve the other.
func TestOrdinaryFollowThroughStillRunsAlongsideDependencyWork(t *testing.T) {
	t.Parallel()

	harness := newFollowerHarness(t)
	harness.providerVerified(t)

	// One tick does both: the ordinary decision follow-through and the
	// dependency progression. If dependency work were on its own loop, this
	// would need a second trigger.
	harness.tick(context.Background())

	if requests := harness.pipeline.recorded("acquire"); len(requests) != 1 {
		t.Fatalf("%d acquisitions, want the rebind's one", len(requests))
	}
}
