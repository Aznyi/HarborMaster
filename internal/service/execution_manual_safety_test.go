package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Test N: a MANUAL provider update cannot bypass the namespace safety path.
//
// # Why this matters more than a warning in the UI
//
// A warning is advice to a person looking at a screen. This is the guarantee for
// everybody else: a script, a curl, a forgotten integration, an operator who
// dismissed the dialog. The backend must hold on its own.
//
// # The manual path IS this method
//
// `POST /api/v1/executions` reaches `ExecutionService.Request`, which is what
// every test below calls. There is no separate manual pipeline to audit: the
// handler validates the body, resolves the acquisition id, and calls this.
//
// So the claim these tests establish is narrow and complete: whatever a caller
// sends, a recreation of a hard namespace provider passes providerRebindable
// and pre-stop operation persistence, or nothing on the host moves.
//
// # What is asserted
//
// The MUTATING call list, not the error. A refusal that arrives after the stop
// is not a refusal -- the live experiment showed dependents are already
// networkless by then.

// N-A. A provider whose dependents can all be reattached proceeds, through the
// same safety path rather than around it.
func TestManualProviderUpdateProceedsWhenDependentsAreRebindable(t *testing.T) {
	t.Parallel()

	harness := newExecHarness(t, func(h *execHarness) {
		h.dependencies = providerDependencies{}
	})

	execution, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if err != nil {
		t.Fatalf("a manual update of a rebindable provider was refused: %v", err)
	}

	final := harness.runOnce(t, execution)
	if final.State != domain.ExecutionSucceeded {
		t.Fatalf("state = %q (%s), want succeeded", final.State, final.Message)
	}
	// It did go through the mutation path -- this is the positive control that
	// stops the refusal tests below passing because nothing ever runs.
	if calls := mutatingCalls(harness); len(calls) == 0 {
		t.Fatal("the recreation performed no mutations; the fixture is not exercising the path")
	}
}

// N-B, C, D. A dependent that cannot be established refuses the manual update
// BEFORE anything is stopped.
//
// Each refusal is the one the live experiment made concrete: an unresolvable
// dependent, one HarborMaster cannot pin, one it must not touch.
func TestManualProviderUpdateRefusesBeforeMutation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		refusal domain.RebindRefusal
	}{
		{"a dependent cannot be resolved", domain.RebindRefusalNotPresent},
		{"a dependent has no established running digest", domain.RebindRefusalDigestUnestablished},
		{"a dependent is opted out", domain.RebindRefusalDisabled},
		{"a dependent is a preserved container", domain.RebindRefusalPreserved},
		{"a dependent is HarborMaster itself", domain.RebindRefusalHarborMaster},
		{"a dependent's namespace was never observed", domain.RebindRefusalNamespaceStale},
		{"a dependent is not recreatable", domain.RebindRefusalNotRecreatable},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			harness := newExecHarness(t, func(h *execHarness) {
				h.dependencies = blockingDependencies{
					dependent: "sonarr", refusal: testCase.refusal,
				}
			})

			_, err := harness.service.Request(context.Background(),
				service.ExecutionRequest{AcquisitionID: execAcquisitionID})

			if got := executionRefusalFrom(t, err); got != domain.ExecutionRefusalDependentsNotRebindable {
				t.Fatalf("refusal = %q, want dependentsNotRebindable", got)
			}
			// THE assertion: the host is untouched.
			if calls := mutatingCalls(harness); len(calls) != 0 {
				t.Fatalf("a manual request changed the host despite the refusal: %v", calls)
			}
		})
	}
}

// N-E. The pre-stop operation record failing to persist refuses the manual
// update, with nothing mutated.
//
// The request-time preflight passes here -- the dependents ARE rebindable --
// and the failure is in making the record durable. A provider stopped without
// that record is a provider whose dependents nobody can enumerate afterwards.
func TestManualProviderUpdateRefusesWhenTheOperationCannotPersist(t *testing.T) {
	t.Parallel()

	harness := newExecHarness(t, func(h *execHarness) {
		h.dependencies = providerDependencies{
			ensureErr: errors.New("insert dependency operation: database is locked"),
		}
	})

	execution, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	final := harness.runOnce(t, execution)
	if final.State == domain.ExecutionSucceeded {
		t.Fatal("a manual update succeeded without a durable record of its dependents")
	}
	if calls := mutatingCalls(harness); len(calls) != 0 {
		t.Fatalf("a manual request changed the host with no durable record: %v", calls)
	}
}

// N-F. A container with no hard dependents takes the ordinary path.
//
// The regression that keeps the feature honest: Phase 16 must not have made
// every manual recreation slower, stricter, or different.
func TestManualUpdateOfAnIndependentContainerIsUnchanged(t *testing.T) {
	t.Parallel()

	recorded := false
	harness := newExecHarness(t, func(h *execHarness) {
		h.dependencies = recordingStandalone{recorded: &recorded}
	})

	execution, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	final := harness.runOnce(t, execution)

	if final.State != domain.ExecutionSucceeded {
		t.Fatalf("state = %q (%s), want succeeded", final.State, final.Message)
	}
	if recorded {
		t.Fatal("a dependency operation was recorded for a container nothing depends on")
	}
}

// A manual request cannot reach the mutation path with the dependency subsystem
// absent.
//
// Unreachable in a wired deployment. Pinned because "the check is missing" must
// never be the quiet route to "the check passed" -- which is precisely what a
// caller bypassing the UI would be relying on.
func TestManualProviderUpdateRefusesWithNoDependencyEvidence(t *testing.T) {
	t.Parallel()

	harness := newExecHarness(t, func(h *execHarness) {
		h.dependencies = nil
	})

	_, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})

	if got := executionRefusalFrom(t, err); got != domain.ExecutionRefusalDependentsNotRebindable {
		t.Fatalf("refusal = %q, want dependentsNotRebindable", got)
	}
	if calls := mutatingCalls(harness); len(calls) != 0 {
		t.Fatalf("the host was changed with no dependency evidence at all: %v", calls)
	}
}

// A manual action never mutates a container other than the one requested.
//
// # The no-cascade rule, asserted rather than assumed
//
// Phase 16 adds no manual cascade. The only additional work a manual provider
// recreation may cause is the rebind machinery intrinsic to completing it
// safely -- and even that happens through the coordinator afterwards, never as
// a side effect of the request.
//
// This walks every mutating call the pipeline made and asserts each names the
// container the operator asked about.
func TestAManualUpdateMutatesOnlyTheRequestedContainer(t *testing.T) {
	t.Parallel()

	harness := newExecHarness(t, func(h *execHarness) {
		h.dependencies = providerDependencies{}
	})

	execution, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	harness.runOnce(t, execution)

	calls := mutatingCalls(harness)
	if len(calls) == 0 {
		t.Fatal("no mutations at all; the fixture is not exercising the path")
	}

	// Every mutation names the original, its replacement, or its parked form.
	// A call naming any OTHER container would be a cascade.
	permitted := map[string]struct{}{
		execContainerID:   {},
		execReplacementID: {},
	}
	for _, call := range calls {
		if call.ContainerID == "" {
			continue
		}
		if _, ok := permitted[call.ContainerID]; !ok {
			t.Errorf("a manual update mutated %s (%s), which the operator did not ask about",
				domain.ShortenID(call.ContainerID), call.Op)
		}
	}
}
