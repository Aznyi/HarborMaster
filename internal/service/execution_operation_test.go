package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Persistence precedes mutation, and every way it can fail leaves the host
// untouched.
//
// # Why the assertion is on the CALL LIST
//
// "An error was returned" is not the property. A refusal that arrives after the
// provider has been stopped is not a refusal -- it is an outage with an error
// message, and the live experiment showed that the dependents are already
// networkless by then.
//
// So every test below asserts that the mutator recorded no MUTATION: nothing
// stopped, renamed, created, started, or removed.
//
// `capture` is excluded, and deliberately rather than conveniently. Capturing a
// container's configuration is a READ -- it is `docker inspect` -- and it is the
// step that produces the configuration the record is built from, so it
// necessarily happens first. The harness's fake serves as both capturer and
// mutator, so its call list contains it; mutatingCalls is what separates the two.
//
// # Why these are separate from the invariant A tests
//
// Invariant A refuses a provider whose dependents CANNOT be reattached. These
// cover the provider whose dependents CAN — where the preflight clears, the
// pipeline proceeds, and the record of what must be reattached then fails to
// persist. That is a different boundary and a later one, and it is the boundary
// this stage added.

// A provider whose dependents are rebindable but whose operation cannot be
// recorded does not move.
func TestOperationPersistenceFailuresProduceZeroMutations(t *testing.T) {
	t.Parallel()

	// Each of these is a distinct failure inside the persistence step. They are
	// exercised through the same seam because the pipeline treats them
	// identically -- and it must: a record that did not land is a record that
	// did not land, whichever statement failed.
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "the operation row could not be inserted",
			err:  errors.New("insert dependency operation: disk full"),
		},
		{
			name: "a member row could not be inserted",
			err:  errors.New("insert dependency operation member: constraint failed"),
		},
		{
			name: "the transaction rolled back",
			err:  errors.New("commit dependency operation: database is locked"),
		},
		{
			name: "the operation could not be read back after being written",
			err:  errors.New("the persisted operation could not be reloaded"),
		},
		{
			name: "the member set was incomplete",
			err:  service.ErrOperationEvidenceIncomplete,
		},
		{
			name: "namespace observation went stale before the worker preflight",
			err:  service.ErrOperationEvidenceIncomplete,
		},
		{
			name: "a dependent stopped being recreatable",
			err:  service.ErrOperationEvidenceIncomplete,
		},
		{
			name: "a dependent lost its established running digest",
			err:  service.ErrOperationEvidenceIncomplete,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			harness := newExecHarness(t, func(h *execHarness) {
				h.dependencies = providerDependencies{ensureErr: testCase.err}
			})

			// The request-time preflight passes: invariant A cleared, because
			// this provider's dependents CAN be reattached. The failure is in
			// the worker's persistence step.
			execution, err := harness.service.Request(context.Background(),
				service.ExecutionRequest{AcquisitionID: execAcquisitionID})
			if err != nil {
				t.Fatalf("request: %v", err)
			}

			final := harness.runOnce(t, execution)

			// THE assertion.
			if calls := mutatingCalls(harness); len(calls) != 0 {
				t.Fatalf("the host was changed despite the record not persisting: %v", calls)
			}

			// And the execution reached a recorded refusal rather than hanging.
			if final.State == domain.ExecutionSucceeded {
				t.Fatalf("the execution succeeded despite the record not persisting")
			}
		})
	}
}

// The link between the operation and its execution is NOT a refusal boundary.
//
// The member set is already durable by then, which is the property that matters.
// Refusing because a cross-reference could not be written would turn a cosmetic
// problem into a container that never gets updated.
func TestAFailedExecutionLinkDoesNotRefuseTheRecreation(t *testing.T) {
	t.Parallel()

	harness := newExecHarness(t, func(h *execHarness) {
		h.dependencies = providerDependencies{
			attachErr: errors.New("could not link the operation to its execution"),
		}
	})

	execution := harness.request(t)
	harness.runOnce(t, execution)

	// It proceeded: the record landed, and only the cross-reference did not.
	if calls := mutatingCalls(harness); len(calls) == 0 {
		t.Fatal("a failed cross-reference refused a recreation whose record was durable")
	}
}

// An ordinary container is completely unaffected.
//
// The regression that matters most: a workload nothing shares a namespace with
// must take exactly the path it took before Phase 16, with no dependency
// operation recorded for it.
func TestAnIndependentContainerRecordsNoDependencyOperation(t *testing.T) {
	t.Parallel()

	recorded := false
	harness := newExecHarness(t, func(h *execHarness) {
		h.dependencies = recordingStandalone{recorded: &recorded}
	})

	execution := harness.request(t)
	harness.runOnce(t, execution)

	if recorded {
		t.Fatal("a dependency operation was recorded for a container nothing depends on")
	}
	// And the recreation went ahead down the unchanged path.
	if calls := mutatingCalls(harness); len(calls) == 0 {
		t.Fatal("an independent recreation performed no mutations")
	}
}

// recordingStandalone is standaloneDependencies that notices if anybody tries to
// record an operation for it.
type recordingStandalone struct{ recorded *bool }

func (recordingStandalone) ResolveNamespaceProvider(_ context.Context, capturedID string) (string, error) {
	return capturedID, nil
}

func (recordingStandalone) AssessProvider(
	_ context.Context, provider string,
) (domain.ProviderRebindAssessment, bool, error) {
	return domain.ProviderRebindAssessment{Provider: provider}, false, nil
}

func (r recordingStandalone) EnsureOperation(
	context.Context, string, string, domain.Requester,
) (string, bool, error) {
	// Reached, but reports "not a provider" -- so no row is written. The flag
	// below records whether anything went further than that.
	return "", false, nil
}

func (r recordingStandalone) AttachProviderExecution(context.Context, string, string) error {
	// Only called when EnsureOperation reported a provider. Reaching it for an
	// independent container is the regression this test exists to catch.
	*r.recorded = true
	return nil
}

var _ service.ExecutionDependencies = recordingStandalone{}

// mutatingCalls returns the recorded calls that CHANGED the host.
//
// An allowlist of the read, not a deny list of the writes. A sixth mutation
// added to the fake would appear here rather than being silently excluded --
// which is the direction that matters, because this helper is what every
// zero-mutation assertion in this file rests on.
func mutatingCalls(h *execHarness) []docker.FakeCall {
	var mutations []docker.FakeCall
	for _, call := range h.mutator.Calls {
		if call.Op == "capture" {
			// `docker inspect`. Reads the configuration the record is built
			// from and changes nothing.
			continue
		}
		mutations = append(mutations, call)
	}
	return mutations
}
