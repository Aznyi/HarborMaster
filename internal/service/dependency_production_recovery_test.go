package service_test

import (
	"context"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Restart recovery, over operations the PRODUCTION path created.
//
// # Why these exist alongside the fixture tests
//
// The Stage 3b recovery tests hand-built operation rows, because at the time
// nothing created them. That proved the derivation logic and nothing about the
// rows it would meet in practice.
//
// These call EnsureOperation -- the same method the execution pipeline calls
// immediately before StopContainer -- and then recover over what it wrote. If
// the production path ever recorded a member set that recovery could not read,
// these would fail and the fixture tests would not.
//
// # The restart model
//
// Every test builds a service, does something, DISCARDS it, and builds another
// over the same stores. The two share no field: no map, no channel, no cached
// graph. Anything that survives does so because it is in a row.

// buildOperation creates the operation the way the execution pipeline does.
//
// The estate is rewound to BEFORE the replacement first, which is when this runs
// in production: the provider is still on its original id and the dependent's
// reference still resolves. See depHarness.beforeReplacement.
func buildOperation(t *testing.T, harness *depHarness) string {
	t.Helper()

	harness.beforeReplacement()

	operationID, isProvider, err := harness.service().EnsureOperation(
		context.Background(), depProvider, "plan_0123456789abcdef0123",
		domain.Requester{UserID: "usr_1", Username: "ada"})
	if err != nil {
		t.Fatalf("ensure operation: %v", err)
	}
	if !isProvider {
		t.Fatal("gluetun was not recognised as a namespace provider")
	}
	if operationID == "" {
		t.Fatal("no operation id was returned")
	}
	return operationID
}

// The production path records the member set the graph implies.
func TestEnsureOperationRecordsTheMandatoryMemberSet(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operationID := buildOperation(t, harness)

	operation, err := harness.operations.Get(context.Background(), operationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(operation.Members) != 1 {
		t.Fatalf("members = %d, want 1", len(operation.Members))
	}

	member := operation.Members[0]
	if member.Dependent != depDependent || member.Provider != depProvider {
		t.Fatalf("member = %s -> %s", member.Dependent, member.Provider)
	}
	if member.Source != domain.DependencyNetworkNamespace {
		t.Fatalf("source = %q, want the hard namespace source", member.Source)
	}
	// The id the dependent NAMES today: the one about to go stale.
	if member.ExpectedProviderID != providerAID {
		t.Fatalf("expectedProviderId = %q, want %q", member.ExpectedProviderID, providerAID)
	}
	if member.State != domain.MemberPending {
		t.Fatalf("state = %q, want pending", member.State)
	}
	// Attribution survives.
	if operation.RequestedBy.Username != "ada" {
		t.Fatalf("requestedBy = %+v", operation.RequestedBy)
	}
}

// A. The operation is persisted, then the process restarts before the provider
// is stopped.
//
// The same operation is reused. A second one would mean two records claiming to
// govern the same provider, and a duplicate member set.
func TestProductionRestartBeforeProviderStopReusesTheOperation(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	first := buildOperation(t, harness)

	// The restart: a brand new service over the same rows.
	second, isProvider, err := harness.service().EnsureOperation(
		context.Background(), depProvider, "plan_0123456789abcdef0123",
		domain.Requester{UserID: "usr_1", Username: "ada"})
	if err != nil {
		t.Fatalf("ensure after restart: %v", err)
	}
	if !isProvider {
		t.Fatal("the provider stopped being recognised after a restart")
	}
	if second != first {
		t.Fatalf("a second operation %q was created; the first was %q", second, first)
	}

	// No duplicate members either.
	operation, _ := harness.operations.Get(context.Background(), first)
	if len(operation.Members) != 1 {
		t.Fatalf("members = %d after a restart, want 1", len(operation.Members))
	}
	// And nothing was read as succeeded.
	recovered, err := harness.service().RecoverOperation(context.Background(), first)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered.Complete {
		t.Fatal("a freshly recorded operation was read as complete")
	}
}

// B. The provider reaches verified success, then the process restarts before
// any plan is produced.
//
// The operation stays incomplete, the member is discovered as outstanding, and
// plans are produced exactly once.
func TestProductionRestartAfterProviderVerifiedProducesPlansOnce(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operationID := buildOperation(t, harness)

	// The provider's recreation succeeded.
	// The provider was replaced: it now holds a new id, so every dependent's
	// reference has gone stale.
	harness.afterReplacement(providerBID)
	harness.operations.markProviderExecution(operationID, depExecutionA)

	// Restart. A new service establishes what remains from the rows alone.
	recovered, err := harness.service().RecoverOperation(context.Background(), operationID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !recovered.ProviderVerified {
		t.Fatal("the provider's verified execution was not recognised after a restart")
	}
	if recovered.Complete {
		t.Fatal("a verified provider was read as a completed operation")
	}
	if len(recovered.Outstanding) != 1 {
		t.Fatalf("outstanding = %d, want 1", len(recovered.Outstanding))
	}

	// Another restart, then produce. Exactly one plan.
	result, err := harness.service().ProduceRebindPlans(context.Background(), operationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("created = %v, want one plan", result.Created)
	}
	if harness.plans.count() != 1 {
		t.Fatalf("%d plan rows, want 1", harness.plans.count())
	}
}

// C. Member A has a plan, member B does not, then a restart.
//
// A's plan is reused and B becomes eligible. Neither is duplicated.
func TestProductionRestartWithOneMemberPlannedContinuesTheOther(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	// A second dependent, also bound to the old provider.
	harness.store.setNamespace(depDependent2, dependent2ID, "container:"+providerAID, true)
	harness.store.endpoints = append(harness.store.endpoints, domain.DependencyEndpoint{
		Name: depDependent2, ContainerID: dependent2ID, ImageRef: depImageRef, Present: true,
	})

	operationID := buildOperation(t, harness)
	// The provider was replaced: it now holds a new id, so every dependent's
	// reference has gone stale.
	harness.afterReplacement(providerBID)
	harness.operations.markProviderExecution(operationID, depExecutionA)

	operation, _ := harness.operations.Get(context.Background(), operationID)
	if len(operation.Members) != 2 {
		t.Fatalf("members = %d, want 2", len(operation.Members))
	}

	// First pass plans both.
	first, err := harness.service().ProduceRebindPlans(context.Background(), operationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if len(first.Created) != 2 {
		t.Fatalf("created = %v, want two plans", first.Created)
	}

	// Restart, produce again: both reused, nothing new.
	second, err := harness.service().ProduceRebindPlans(context.Background(), operationID)
	if err != nil {
		t.Fatalf("produce after restart: %v", err)
	}
	if len(second.Created) != 0 {
		t.Fatalf("a restart created duplicate plans: %v", second.Created)
	}
	for _, dependent := range []string{depDependent, depDependent2} {
		if second.Reused[dependent] != first.Created[dependent] {
			t.Errorf("%s: reused %q, want %q",
				dependent, second.Reused[dependent], first.Created[dependent])
		}
	}
	if harness.plans.count() != 2 {
		t.Fatalf("%d plan rows, want 2", harness.plans.count())
	}
}

// D. A member's rebind execution reached verified success, then a restart.
//
// It is not repeated, and the remaining member continues.
func TestProductionRestartDoesNotRepeatAVerifiedMember(t *testing.T) {
	t.Parallel()

	const memberExecution = "exec_0123456789abcdef0bb0"

	harness := newDepHarness()
	harness.store.setNamespace(depDependent2, dependent2ID, "container:"+providerAID, true)
	harness.store.endpoints = append(harness.store.endpoints, domain.DependencyEndpoint{
		Name: depDependent2, ContainerID: dependent2ID, ImageRef: depImageRef, Present: true,
	})

	operationID := buildOperation(t, harness)
	// The provider was replaced: it now holds a new id, so every dependent's
	// reference has gone stale.
	harness.afterReplacement(providerBID)
	harness.operations.markProviderExecution(operationID, depExecutionA)

	// sonarr's rebind ran and succeeded.
	harness.executions.records[memberExecution] = domain.Execution{State: domain.ExecutionSucceeded}
	harness.operations.markMemberExecution(operationID, depDependent, memberExecution,
		domain.MemberExecuting)

	// Restart and recover.
	recovered, err := harness.service().RecoverOperation(context.Background(), operationID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered.Complete {
		t.Fatal("the operation completed with one member still outstanding")
	}
	if len(recovered.Outstanding) != 1 || recovered.Outstanding[0].Dependent != depDependent2 {
		t.Fatalf("outstanding = %+v, want only %s", recovered.Outstanding, depDependent2)
	}

	// And plan production does not touch the verified one.
	result, err := harness.service().ProduceRebindPlans(context.Background(), operationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if _, replanned := result.Created[depDependent]; replanned {
		t.Fatal("a verified member was planned again")
	}
	if _, planned := result.Created[depDependent2]; !planned {
		t.Fatalf("the outstanding member was not planned; skipped = %v", result.Skipped)
	}
}

// E. A member's execution exists but has not finished.
//
// The execution service's own recovery owns it. Dependency code does not clear
// it, does not fail it, and does not submit another.
func TestProductionRestartLeavesAnUnfinishedMemberExecutionAlone(t *testing.T) {
	t.Parallel()

	const memberExecution = "exec_0123456789abcdef0cc0"

	harness := newDepHarness()
	operationID := buildOperation(t, harness)
	// The provider was replaced: it now holds a new id, so every dependent's
	// reference has gone stale.
	harness.afterReplacement(providerBID)
	harness.operations.markProviderExecution(operationID, depExecutionA)

	// Mid-flight, and the member row optimistically says verified.
	harness.executions.records[memberExecution] = domain.Execution{State: domain.ExecutionCreating}
	harness.operations.markMemberExecution(operationID, depDependent, memberExecution,
		domain.MemberVerified)

	recovered, err := harness.service().RecoverOperation(context.Background(), operationID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered.Complete {
		t.Fatal("an unfinished execution was read as a completed rebind")
	}
	if len(recovered.Outstanding) != 1 ||
		recovered.Outstanding[0].State != domain.MemberExecuting {
		t.Fatalf("outstanding = %+v, want one member in executing", recovered.Outstanding)
	}

	// No second plan is produced for a member whose execution is running.
	before := harness.plans.count()
	if _, err := harness.service().ProduceRebindPlans(context.Background(), operationID); err != nil {
		t.Fatalf("produce: %v", err)
	}
	if harness.plans.count() != before {
		t.Fatal("a plan was produced for a member whose execution is still running")
	}
}

// F. A terminal operation creates no work.
func TestProductionRestartDoesNotReopenATerminalOperation(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operationID := buildOperation(t, harness)

	if err := harness.operations.AdvanceOperation(context.Background(), operationID,
		domain.OperationFailed, domain.OperationFailureRebind, fixedNow()); err != nil {
		t.Fatalf("advance: %v", err)
	}

	open, err := harness.service().Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("a terminal operation was reopened: %+v", open)
	}

	before := harness.plans.count()
	if _, err := harness.service().ProduceRebindPlans(context.Background(), operationID); err != nil {
		t.Fatalf("produce: %v", err)
	}
	if harness.plans.count() != before {
		t.Fatal("a plan was regenerated for a terminal operation")
	}

	// And a NEW operation may now be created, because the old one concluded.
	fresh, isProvider, err := harness.service().EnsureOperation(context.Background(),
		depProvider, "plan_0123456789abcdef0123", domain.Requester{})
	if err != nil {
		t.Fatalf("ensure after conclusion: %v", err)
	}
	if !isProvider || fresh == operationID {
		t.Fatalf("expected a new operation; got %q (previous %q)", fresh, operationID)
	}
}

// An ordinary container gets no operation from the production path either.
func TestEnsureOperationRecordsNothingForAnIndependentContainer(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	// Nothing shares sonarr's namespace.
	operationID, isProvider, err := harness.service().EnsureOperation(
		context.Background(), depDependent, "plan_0123456789abcdef0123", domain.Requester{})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if isProvider {
		t.Fatal("a container nothing depends on was treated as a provider")
	}
	if operationID != "" {
		t.Fatalf("an operation %q was recorded for an independent container", operationID)
	}
	if len(harness.operations.operations) != 0 {
		t.Fatalf("%d operation rows were written", len(harness.operations.operations))
	}
}

// The no-process-local-state proof, stated as its own test.
//
// Instance #1 does the work. It is then unreachable -- the variable is gone and
// nothing holds a reference. Instance #2 is constructed from the stores alone
// and must reach the same conclusions.
func TestRecoveryRequiresNoProcessLocalState(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()

	harness.beforeReplacement()

	var operationID string
	var plannedID string
	func() {
		// Instance #1, scoped so it cannot leak into the assertions below.
		first := harness.service()

		id, isProvider, err := first.EnsureOperation(context.Background(),
			depProvider, "plan_0123456789abcdef0123", domain.Requester{Username: "ada"})
		if err != nil || !isProvider {
			t.Fatalf("ensure: id=%q isProvider=%v err=%v", id, isProvider, err)
		}
		operationID = id

		// The provider is replaced and its execution verifies.
		harness.afterReplacement(providerBID)
		harness.operations.markProviderExecution(id, depExecutionA)

		produced, err := first.ProduceRebindPlans(context.Background(), id)
		if err != nil {
			t.Fatalf("produce: %v", err)
		}
		plannedID = produced.Created[depDependent]
		if plannedID == "" {
			t.Fatal("instance #1 produced no plan")
		}
	}()

	// Instance #2. Built from the stores, sharing nothing with instance #1.
	second := harness.service()

	recovered, err := second.RecoverOperation(context.Background(), operationID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !recovered.ProviderVerified {
		t.Fatal("instance #2 did not establish that the provider succeeded")
	}
	if recovered.Complete {
		t.Fatal("instance #2 read the operation as complete")
	}
	if len(recovered.Outstanding) != 1 {
		t.Fatalf("instance #2 found %d outstanding members, want 1", len(recovered.Outstanding))
	}
	if recovered.Outstanding[0].PlanID != plannedID {
		t.Fatalf("instance #2 sees plan %q, instance #1 created %q",
			recovered.Outstanding[0].PlanID, plannedID)
	}

	// And it does not duplicate the work.
	again, err := second.ProduceRebindPlans(context.Background(), operationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if len(again.Created) != 0 {
		t.Fatalf("instance #2 recreated work instance #1 had already done: %v", again.Created)
	}
	if harness.plans.count() != 1 {
		t.Fatalf("%d plan rows, want 1", harness.plans.count())
	}
}

// The whole operation succeeds only when every member is verified.
func TestProductionOperationSucceedsOnlyWhenEveryMemberIsVerified(t *testing.T) {
	t.Parallel()

	const memberExecution = "exec_0123456789abcdef0dd0"

	harness := newDepHarness()
	operationID := buildOperation(t, harness)
	// The provider was replaced: it now holds a new id, so every dependent's
	// reference has gone stale.
	harness.afterReplacement(providerBID)
	harness.operations.markProviderExecution(operationID, depExecutionA)

	// Provider verified, member not: NOT succeeded.
	state, err := harness.service().ConcludeOperation(context.Background(), operationID)
	if err != nil {
		t.Fatalf("conclude: %v", err)
	}
	if state == domain.OperationSucceeded {
		t.Fatal("the operation succeeded with a member still pending")
	}

	// The member's rebind now succeeds too.
	harness.executions.records[memberExecution] = domain.Execution{State: domain.ExecutionSucceeded}
	harness.operations.markMemberExecution(operationID, depDependent, memberExecution,
		domain.MemberExecuting)

	state, err = harness.service().ConcludeOperation(context.Background(), operationID)
	if err != nil {
		t.Fatalf("conclude: %v", err)
	}
	if state != domain.OperationSucceeded {
		t.Fatalf("state = %q, want succeeded", state)
	}
}

var _ = service.ErrOperationEvidenceIncomplete
