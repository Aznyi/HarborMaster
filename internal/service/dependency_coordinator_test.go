package service_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// ProduceRebindPlans, and the eight ways the world can move under it.
//
// # What a stale rebind would actually do
//
// A rebind RECREATES a container. If it is built from evidence that has expired
// -- the dependent was fixed by hand, removed, opted out, or repointed -- then
// HarborMaster stops and replaces a container for a reason that is no longer
// true. That is the failure these tests exist to make impossible, and none of
// them is theoretical: every one is something `docker compose up` does between a
// provider being replaced and the next coordinator pass.
//
// Each case states the resulting member state explicitly, because "no plan was
// produced" is not sufficient: a member left `pending` forever and a member
// correctly `blocked` are different outcomes for an operator.

// The baseline. Everything is as the operation recorded it, and one plan is
// produced.
func TestProduceRebindPlansBuildsThePlanTheDependentNeeds(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()

	result, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}

	planID, created := result.Created[depDependent]
	if !created {
		t.Fatalf("no plan was created; skipped = %v, satisfied = %v",
			result.Skipped, result.Satisfied)
	}
	if len(result.Created) != 1 {
		t.Fatalf("created = %v, want exactly one", result.Created)
	}

	plan, err := harness.plans.Get(context.Background(), planID)
	if err != nil {
		t.Fatalf("the created plan was not stored: %v", err)
	}

	// The shape a rebind must have.
	if plan.UpdateType != domain.UpdateRebind {
		t.Errorf("updateType = %q, want rebind", plan.UpdateType)
	}
	if plan.CurrentImage != plan.ProposedImage {
		t.Errorf("a rebind moved the image: %q -> %q", plan.CurrentImage, plan.ProposedImage)
	}
	if plan.ProposedDigest != depDigestX {
		t.Errorf("proposedDigest = %q, want the digest the container is running (%q)",
			plan.ProposedDigest, depDigestX)
	}
	if plan.ContainerName != depDependent {
		t.Errorf("containerName = %q", plan.ContainerName)
	}

	// The member carries the plan and the provider identity it targets.
	member, found := harness.memberOf(operation.OperationID, depDependent)
	if !found {
		t.Fatal("the member disappeared")
	}
	if member.PlanID != planID {
		t.Errorf("member planId = %q, want %q", member.PlanID, planID)
	}
	if member.State != domain.MemberPlanCreated {
		t.Errorf("member state = %q, want planCreated", member.State)
	}

	// Producing a plan is an ASSESSMENT. The operation is no closer to done.
	recovered, err := harness.service().RecoverOperation(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered.Complete {
		t.Fatal("creating a plan was read as completing the operation")
	}
}

// The plan targets the provider's CURRENT identity, not the one recorded.
func TestARebindPlanTargetsTheCurrentProviderIdentity(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()

	if _, err := harness.service().ProduceRebindPlans(
		context.Background(), operation.OperationID); err != nil {
		t.Fatalf("produce: %v", err)
	}

	member, _ := harness.memberOf(operation.OperationID, depDependent)
	if member.TargetProviderID != providerBID {
		t.Fatalf("targetProviderId = %q, want the verified replacement %q",
			member.TargetProviderID, providerBID)
	}
	if member.ExpectedProviderID != providerAID {
		t.Fatalf("expectedProviderId = %q, want the replaced provider %q",
			member.ExpectedProviderID, providerAID)
	}
}

// Nothing is produced until the provider has actually succeeded.
func TestNoRebindPlansUntilTheProviderIsVerified(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	// The provider's execution has not finished.
	harness.executions = fakeExecutions{records: map[string]domain.Execution{
		depExecutionA: {State: domain.ExecutionCreating},
	}}
	operation := harness.seedOperation()

	result, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if len(result.Created) != 0 {
		t.Fatalf("plans were produced before the provider succeeded: %v", result.Created)
	}
	if harness.plans.count() != 0 {
		t.Fatal("a plan row was written before the provider succeeded")
	}
}

// ------------------------------------------------------------ 9A .. 9H --

// 9A. The dependent was recreated by hand and is ALREADY on the replacement.
//
// Nothing to repair. The member clears rather than being planned, because a
// rebind here would stop and replace a container that is already correct.
func TestTOCTOUDependentAlreadyRebound(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()

	// Somebody ran `docker compose up sonarr`: new id, and it now names B.
	harness.store.setNamespace(depDependent, newDependent, "container:"+providerBID, true)
	harness.store.mutateEndpoint(depDependent, func(e *domain.DependencyEndpoint) {
		e.ContainerID = newDependent
	})

	result, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}

	if len(result.Created) != 0 {
		t.Fatalf("a stale rebind was planned for a container already correctly attached: %v",
			result.Created)
	}
	if harness.plans.count() != 0 {
		t.Fatal("a plan row was written")
	}
	if len(result.Satisfied) != 1 || result.Satisfied[0] != depDependent {
		t.Fatalf("satisfied = %v, want [%s]", result.Satisfied, depDependent)
	}

	// THE PINNED STATE: verified. The requirement is met, so the member clears
	// and stops holding the operation open.
	member, _ := harness.memberOf(operation.OperationID, depDependent)
	if member.State != domain.MemberVerified {
		t.Fatalf("member state = %q, want verified", member.State)
	}
}

// 9B. The dependent is gone.
func TestTOCTOUDependentRemoved(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()
	harness.store.removeContainer(depDependent)

	result, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}

	if len(result.Created) != 0 || harness.plans.count() != 0 {
		t.Fatalf("a plan was produced for a container that no longer exists: %v", result.Created)
	}
	// THE PINNED STATE: skipped with dependentNotPresent. No target is guessed.
	if got := result.Skipped[depDependent]; got != domain.RebindRefusalNotPresent {
		t.Fatalf("skipped = %q, want dependentNotPresent", got)
	}

	// And the operation cannot succeed: the member still holds it open.
	recovered, _ := harness.service().RecoverOperation(context.Background(), operation.OperationID)
	if recovered.Complete {
		t.Fatal("the operation completed with a member that could not be reattached")
	}
}

// 9C. The dependent no longer shares that namespace at all, or shares a
// different one. Across all three namespace kinds.
func TestTOCTOUNamespaceModeChanged(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		modes domain.NamespaceModes
		want  domain.RebindRefusal
	}{
		{
			name:  "recreated onto a bridge network",
			modes: domain.NamespaceModes{Network: "bridge", Observed: true},
			want:  domain.RebindRefusalProviderMismatch,
		},
		{
			name:  "repointed at an unrelated container",
			modes: domain.NamespaceModes{Network: "container:" + providerCID, Observed: true},
			want:  domain.RebindRefusalProviderMismatch,
		},
		{
			name:  "namespace facts stopped being observed",
			modes: domain.NamespaceModes{Network: "container:" + providerAID, Observed: false},
			want:  domain.RebindRefusalNamespaceStale,
		},
		{
			name: "the reference became unreadable",
			modes: domain.NamespaceModes{
				Network: "container:gluetun", Observed: true,
			},
			want: domain.RebindRefusalNamespaceStale,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			harness := newDepHarness()
			operation := harness.seedOperation()
			harness.store.setModes(depDependent, dependentID, testCase.modes)

			result, err := harness.service().ProduceRebindPlans(
				context.Background(), operation.OperationID)
			if err != nil {
				t.Fatalf("produce: %v", err)
			}
			if len(result.Created) != 0 || harness.plans.count() != 0 {
				t.Fatalf("a stale plan was produced: %v", result.Created)
			}
			if got := result.Skipped[depDependent]; got != testCase.want {
				t.Fatalf("skipped = %q, want %q", got, testCase.want)
			}
		})
	}
}

// 9C, the IPC and PID halves.
//
// The member's SOURCE decides which mode is consulted. A dependent that shares
// an IPC namespace is assessed against IpcMode, and a change to its network mode
// is irrelevant to it.
func TestTOCTOUCoversEveryNamespaceKind(t *testing.T) {
	t.Parallel()

	for _, source := range domain.DiscoveredDependencySources {
		t.Run(string(source), func(t *testing.T) {
			t.Parallel()

			harness := newDepHarness()
			// The member is about THIS namespace.
			operation, _ := harness.operations.Create(context.Background(),
				domain.DependencyOperation{
					Provider:            depProvider,
					ProviderExecutionID: depExecutionA,
					State:               domain.OperationProviderVerified,
					Members: []domain.DependencyMember{{
						Dependent:          depDependent,
						Provider:           depProvider,
						Source:             source,
						ExpectedProviderID: providerAID,
						State:              domain.MemberPending,
					}},
				}, fixedNow())

			// The dependent declares the stale reference on that same namespace.
			modes := domain.NamespaceModes{Observed: true}
			switch source {
			case domain.DependencyNetworkNamespace:
				modes.Network = "container:" + providerAID
			case domain.DependencyIPCNamespace:
				modes.IPC = "container:" + providerAID
			case domain.DependencyPIDNamespace:
				modes.PID = "container:" + providerAID
			}
			harness.store.setModes(depDependent, dependentID, modes)

			result, err := harness.service().ProduceRebindPlans(
				context.Background(), operation.OperationID)
			if err != nil {
				t.Fatalf("produce: %v", err)
			}
			if len(result.Created) != 1 {
				t.Fatalf("%s: created = %v, skipped = %v; want one plan",
					source, result.Created, result.Skipped)
			}

			// And with that namespace cleared, no plan.
			harness.store.setModes(depDependent, dependentID,
				domain.NamespaceModes{Observed: true})
			second := newDepHarness()
			second.store.setModes(depDependent, dependentID,
				domain.NamespaceModes{Observed: true})
			operation2, _ := second.operations.Create(context.Background(),
				domain.DependencyOperation{
					Provider:            depProvider,
					ProviderExecutionID: depExecutionA,
					State:               domain.OperationProviderVerified,
					Members: []domain.DependencyMember{{
						Dependent: depDependent, Provider: depProvider, Source: source,
						ExpectedProviderID: providerAID, State: domain.MemberPending,
					}},
				}, fixedNow())

			cleared, err := second.service().ProduceRebindPlans(
				context.Background(), operation2.OperationID)
			if err != nil {
				t.Fatalf("produce: %v", err)
			}
			if len(cleared.Created) != 0 {
				t.Fatalf("%s: a plan was produced for a container sharing nothing", source)
			}
		})
	}
}

// 9D. The provider changed identity AGAIN, from B to C.
//
// PINNED BEHAVIOUR: the coordinator RECOMPUTES rather than refusing. The
// dependent still names A, the current provider is now C, so the transition it
// needs is A -> C and that is what is built. A plan targeting B is never
// produced, because B is no longer what the provider is.
func TestTOCTOUProviderChangedIdentityAgain(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()

	// The provider was replaced a second time.
	harness.store.setNamespace(depProvider, providerCID, "bridge", true)
	harness.store.mutateEndpoint(depProvider, func(e *domain.DependencyEndpoint) {
		e.ContainerID = providerCID
	})

	result, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("created = %v, skipped = %v; want one plan against the CURRENT provider",
			result.Created, result.Skipped)
	}

	member, _ := harness.memberOf(operation.OperationID, depDependent)
	if member.TargetProviderID != providerCID {
		t.Fatalf("targetProviderId = %q, want the current provider %q; a plan aimed at "+
			"a provider identity that is no longer current would reattach the container "+
			"to a namespace that does not exist",
			member.TargetProviderID, providerCID)
	}
	if member.TargetProviderID == providerBID {
		t.Fatal("the plan targets the superseded provider identity")
	}
}

// 9E. The owner opted the dependent out after the operation was created.
//
// A dependency relationship does not override an opt-out. It never has, in any
// direction: the label is a statement that HarborMaster should leave the
// container alone, and "except when repairing it" is how an opt-out stops
// meaning anything.
func TestTOCTOUDependentOptedOut(t *testing.T) {
	t.Parallel()

	for _, label := range []string{
		domain.LabelHarborMasterEnabled,
		domain.LabelUpdateEnabled,
	} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()

			harness := newDepHarness()
			operation := harness.seedOperation()
			harness.store.mutateEndpoint(depDependent, func(e *domain.DependencyEndpoint) {
				e.Labels = map[string]string{label: "false"}
			})

			result, err := harness.service().ProduceRebindPlans(
				context.Background(), operation.OperationID)
			if err != nil {
				t.Fatalf("produce: %v", err)
			}
			if len(result.Created) != 0 || harness.plans.count() != 0 {
				t.Fatalf("a rebind was planned for an opted-out container: %v", result.Created)
			}
			if got := result.Skipped[depDependent]; got != domain.RebindRefusalDisabled {
				t.Fatalf("skipped = %q, want optedOut", got)
			}
		})
	}
}

// 9F. The dependent became a preserved/quarantined container.
//
// Tested through the AUTHORITATIVE evidence -- the endpoint's Derived flag,
// which the repository sets -- as well as through the derived-name pattern, so
// the stronger signal is the one actually exercised.
func TestTOCTOUDependentBecamePreserved(t *testing.T) {
	t.Parallel()

	t.Run("authoritative derived flag", func(t *testing.T) {
		t.Parallel()

		harness := newDepHarness()
		operation := harness.seedOperation()
		harness.store.mutateEndpoint(depDependent, func(e *domain.DependencyEndpoint) {
			e.Derived = true
		})

		result, err := harness.service().ProduceRebindPlans(
			context.Background(), operation.OperationID)
		if err != nil {
			t.Fatalf("produce: %v", err)
		}
		if len(result.Created) != 0 || harness.plans.count() != 0 {
			t.Fatalf("a preserved container was planned for recreation: %v", result.Created)
		}
		if got := result.Skipped[depDependent]; got != domain.RebindRefusalPreserved {
			t.Fatalf("skipped = %q, want preservedContainer", got)
		}
	})

	t.Run("derived name", func(t *testing.T) {
		t.Parallel()

		parked := depDependent + domain.ParkedNameSuffix + "exec_0123456789abcdef0123"
		harness := newDepHarness()
		harness.store.setNamespace(parked, dependent2ID, "container:"+providerAID, true)
		harness.store.endpoints = append(harness.store.endpoints, domain.DependencyEndpoint{
			Name: parked, ContainerID: dependent2ID, ImageRef: depImageRef, Present: true,
		})
		harness.lineage.set(dependent2ID, depImageRef, depDigestX)

		operation, _ := harness.operations.Create(context.Background(),
			domain.DependencyOperation{
				Provider:            depProvider,
				ProviderExecutionID: depExecutionA,
				State:               domain.OperationProviderVerified,
				Members: []domain.DependencyMember{{
					Dependent: parked, Provider: depProvider,
					Source:             domain.DependencyNetworkNamespace,
					ExpectedProviderID: providerAID, State: domain.MemberPending,
				}},
			}, fixedNow())

		result, err := harness.service().ProduceRebindPlans(
			context.Background(), operation.OperationID)
		if err != nil {
			t.Fatalf("produce: %v", err)
		}
		if len(result.Created) != 0 {
			t.Fatalf("a parked container was planned for recreation: %v", result.Created)
		}
		if got := result.Skipped[parked]; got != domain.RebindRefusalPreserved {
			t.Fatalf("skipped = %q, want preservedContainer", got)
		}
	})
}

// 9G. The dependent turns out to be HarborMaster itself.
//
// The identity is supplied through the real SelfIdentity path and matched by
// SelfMatch, exactly as every other self-update refusal is. Nothing here weakens
// the identity check to make the fixture convenient.
func TestTOCTOUDependentIsHarborMaster(t *testing.T) {
	t.Parallel()

	for _, identity := range []struct {
		name string
		self domain.SelfIdentity
	}{
		{"by container name", domain.SelfIdentity{ContainerName: depDependent}},
		{"by container id", domain.SelfIdentity{ContainerID: dependentID}},
	} {
		t.Run(identity.name, func(t *testing.T) {
			t.Parallel()

			harness := newDepHarness()
			harness.self = identity.self
			operation := harness.seedOperation()

			result, err := harness.service().ProduceRebindPlans(
				context.Background(), operation.OperationID)
			if err != nil {
				t.Fatalf("produce: %v", err)
			}
			if len(result.Created) != 0 || harness.plans.count() != 0 {
				t.Fatalf("a rebind was planned for HarborMaster itself: %v", result.Created)
			}
			if got := result.Skipped[depDependent]; got != domain.RebindRefusalHarborMaster {
				t.Fatalf("skipped = %q, want harborMasterContainer", got)
			}
		})
	}
}

// 9H. The dependent is running a different digest than when the operation was
// created.
//
// PINNED BEHAVIOUR: the plan is built against the digest it is running NOW.
// The historical digest is never assumed -- recreating on a digest the container
// has since moved off would silently change what executes while claiming to
// repair a network attachment.
func TestTOCTOURunningDigestChanged(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()

	// It was updated by hand between the operation being created and now.
	harness.lineage.set(dependentID, depImageRef, depDigestY)

	result, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	planID, created := result.Created[depDependent]
	if !created {
		t.Fatalf("no plan was produced; skipped = %v", result.Skipped)
	}

	plan, err := harness.plans.Get(context.Background(), planID)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.ProposedDigest != depDigestY {
		t.Fatalf("proposedDigest = %q, want the digest running NOW (%q)",
			plan.ProposedDigest, depDigestY)
	}
	if plan.ProposedDigest == depDigestX {
		t.Fatal("the plan proposes the historical digest; recreating on it would change " +
			"what executes while claiming to repair a network attachment")
	}
}

// The digest becoming UNESTABLISHED refuses rather than falling back to a tag.
func TestTOCTOURunningDigestLost(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()
	harness.lineage.set(dependentID, depImageRef, "")

	result, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if len(result.Created) != 0 || harness.plans.count() != 0 {
		t.Fatalf("a plan was produced with no established digest: %v", result.Created)
	}
	if got := result.Skipped[depDependent]; got != domain.RebindRefusalDigestUnestablished {
		t.Fatalf("skipped = %q, want runningDigestUnestablished", got)
	}
}

// ---------------------------------------------------------- dedup A .. F --

// A. Calling twice produces one plan.
func TestDedupRepeatedProduceReusesThePlan(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()

	first, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("first produce: %v", err)
	}
	second, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("second produce: %v", err)
	}

	if len(second.Created) != 0 {
		t.Fatalf("a second plan was created: %v", second.Created)
	}
	if second.Reused[depDependent] != first.Created[depDependent] {
		t.Fatalf("reused = %q, want the first plan %q",
			second.Reused[depDependent], first.Created[depDependent])
	}
	if harness.plans.count() != 1 {
		t.Fatalf("%d plan rows were written, want 1", harness.plans.count())
	}
}

// B. Concurrent callers collapse to one plan. Run under -race.
func TestDedupConcurrentProduceCollapsesToOnePlan(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()

	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			// Each goroutine gets its OWN service, sharing only the stores --
			// which is the situation two processes would be in.
			_, _ = harness.service().ProduceRebindPlans(
				context.Background(), operation.OperationID)
		}()
	}
	wait.Wait()

	member, _ := harness.memberOf(operation.OperationID, depDependent)
	if member.PlanID == "" {
		t.Fatal("no plan id was recorded on the member")
	}
	// The member carries exactly ONE plan id, and it is one that exists.
	if _, err := harness.plans.Get(context.Background(), member.PlanID); err != nil {
		t.Fatalf("the member names a plan that does not exist: %v", err)
	}
}

// C and D. A restart, and a member that already carries a plan.
func TestDedupPlanSurvivesARestart(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()

	first, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	planID := first.Created[depDependent]

	// A NEW service over the same stores. No field is shared.
	after, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce after restart: %v", err)
	}
	if len(after.Created) != 0 {
		t.Fatalf("a restart produced a second plan: %v", after.Created)
	}
	if after.Reused[depDependent] != planID {
		t.Fatalf("reused = %q, want %q", after.Reused[depDependent], planID)
	}
	if harness.plans.count() != 1 {
		t.Fatalf("%d plan rows, want 1", harness.plans.count())
	}
}

// E. The plan row exists but the member link never landed.
//
// The member has no plan id, so a replacement IS built -- which is the safe
// direction. The orphan is harmless: a plan causes nothing to happen, and
// nothing points at it.
func TestDedupAnOrphanedPlanIsReplacedRatherThanTrusted(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()

	// Simulate the interrupted link: a plan row with no member pointing at it.
	orphan := domain.ChangePlan{
		PlanID: domain.NewPlanID(), ContainerName: depDependent,
		UpdateType: domain.UpdateRebind,
	}
	if _, err := harness.plans.InsertPlans(context.Background(),
		[]domain.ChangePlan{orphan}, fixedNow()); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	result, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	planID, created := result.Created[depDependent]
	if !created {
		t.Fatalf("no plan was built after an interrupted link; skipped = %v", result.Skipped)
	}
	if planID == orphan.PlanID {
		t.Fatal("the orphan was adopted; nothing established that it describes this transition")
	}

	// Exactly one plan is EXECUTABLE, in the sense that exactly one is named by
	// a member. The orphan is unreferenced.
	member, _ := harness.memberOf(operation.OperationID, depDependent)
	if member.PlanID != planID {
		t.Fatalf("member planId = %q, want the new plan %q", member.PlanID, planID)
	}
}

// F. The member's plan cannot be READ.
//
// PINNED: unreadable is not evidence of absence. No replacement is built,
// because "the row could not be fetched" says nothing about whether it is there
// -- and building a second plan on that basis is how one member ends up with two.
func TestDedupAnUnreadablePlanIsNotReplaced(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()

	first, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	planID := first.Created[depDependent]

	// The store now fails reads. NOT "not found" -- fails.
	harness.plans.getErr = errUnreadable

	after, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if len(after.Created) != 0 {
		t.Fatalf("an unreadable plan was replaced: %v", after.Created)
	}
	if after.Reused[depDependent] != planID {
		t.Fatalf("reused = %q, want the existing plan %q", after.Reused[depDependent], planID)
	}
	if harness.plans.count() != 1 {
		t.Fatalf("%d plan rows, want 1", harness.plans.count())
	}
}

// A plan built for a SUPERSEDED provider identity is stale and is replaced.
//
// The other half of dedup: reuse is not unconditional. A plan whose target is no
// longer the provider's identity would reattach the container to a namespace
// that no longer exists.
func TestDedupAPlanForASupersededProviderIsRebuilt(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	operation := harness.seedOperation()

	first, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	firstPlan := first.Created[depDependent]

	// The provider is replaced again.
	harness.store.setNamespace(depProvider, providerCID, "bridge", true)
	harness.store.mutateEndpoint(depProvider, func(e *domain.DependencyEndpoint) {
		e.ContainerID = providerCID
	})

	after, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	rebuilt, created := after.Created[depDependent]
	if !created {
		t.Fatalf("a plan targeting a superseded provider was reused; skipped = %v, reused = %v",
			after.Skipped, after.Reused)
	}
	if rebuilt == firstPlan {
		t.Fatal("the same plan id was reused for a different provider transition")
	}

	member, _ := harness.memberOf(operation.OperationID, depDependent)
	if member.TargetProviderID != providerCID {
		t.Fatalf("targetProviderId = %q, want %q", member.TargetProviderID, providerCID)
	}
}

// ------------------------------------------------- UpdateNone regression --

// An unchanged image alone does not make something a rebind, and UpdateNone
// stays inert.
func TestAnUnchangedImagePlanIsNotARebind(t *testing.T) {
	t.Parallel()

	// The shape a caller might hope is enough: same image both sides, a real
	// digest, no evidence.
	lookalike := domain.ChangePlan{
		PlanID:         domain.NewPlanID(),
		ContainerName:  depDependent,
		CurrentImage:   depImageRef,
		ProposedImage:  depImageRef,
		ProposedDigest: depDigestX,
		UpdateType:     domain.UpdateNone,
	}

	// It is structurally valid -- and inert, because no strategy permits it.
	if !lookalike.ValidTarget() {
		t.Fatal("the fixture is not a structurally valid plan")
	}
	for _, strategy := range domain.UpdateStrategies {
		if strategy.Permits(lookalike.UpdateType) {
			t.Fatalf("strategy %q permits an updateNone plan", strategy)
		}
	}

	// And relabelling it does not help: BuildRebindPlan is the only producer of
	// UpdateRebind, and it refuses without established evidence.
	_, refusal := domain.BuildRebindPlan(
		domain.RebindEvidence{},
		domain.RebindCandidate{
			Name: depDependent, Provider: depProvider,
			Present: true, NamespacesObserved: true, Recreatable: true,
			RunningReference: depImageRef, RunningDigest: depDigestX,
		},
		domain.SelfIdentity{},
		domain.PlanInputs{},
		domain.NewPlanID(),
		fixedNow(),
	)
	if refusal != domain.RebindRefusalNoEvidence {
		t.Fatalf("refusal = %q, want noEvidence", refusal)
	}
}

// An OPERATOR relationship can never produce a rebind, however it is recorded.
func TestAnOperatorMemberProducesNoRebindPlan(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	// A member claiming an operator source. The schema refuses to store one;
	// this proves the coordinator refuses to act on one even if it appeared.
	operation, _ := harness.operations.Create(context.Background(), domain.DependencyOperation{
		Provider:            depProvider,
		ProviderExecutionID: depExecutionA,
		State:               domain.OperationProviderVerified,
		Members: []domain.DependencyMember{{
			Dependent: depDependent, Provider: depProvider,
			Source:             domain.DependencyOperator,
			ExpectedProviderID: providerAID, State: domain.MemberPending,
		}},
	}, fixedNow())

	result, err := harness.service().ProduceRebindPlans(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if len(result.Created) != 0 || harness.plans.count() != 0 {
		t.Fatalf("an operator relationship produced a rebind: %v", result.Created)
	}
	if got := result.Skipped[depDependent]; got != domain.RebindRefusalNoEvidence {
		t.Fatalf("skipped = %q, want noEvidence", got)
	}
}

func fixedNow() (t time.Time) { return time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC) }
