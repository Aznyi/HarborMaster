package service_test

import (
	"context"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Invariant A: a hard namespace provider cannot enter mutation on an absence.
//
// # Why this test is about CALL ORDER rather than about a return value
//
// The live experiment against Docker 29.6.2 established the exact boundary. A
// container sharing a provider's network namespace breaks the moment the
// provider is STOPPED -- not when it is removed, not when the replacement is
// created. It is left `Up`, with a PID, with no network, and nothing logs it.
//
// So "the request was refused" is not the property worth asserting. The property
// is that NOTHING ON THE HOST WAS TOUCHED: no stop, no rename, no create, no
// remove. A refusal that arrives after the stop is not a refusal, it is an
// outage with an error message.
//
// These tests therefore drive the real pipeline against the recording mutator
// and assert on its call log.

// A provider whose dependent cannot be rebound is refused, and the host is
// never touched.
func TestAProviderWhoseDependentCannotBeReboundIsRefusedBeforeAnyMutation(t *testing.T) {
	t.Parallel()

	harness := newExecHarness(t, func(h *execHarness) {
		h.dependencies = blockingDependencies{
			dependent: "sonarr",
			refusal:   domain.RebindRefusalDigestUnestablished,
		}
	})

	_, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})

	if got := executionRefusalFrom(t, err); got != domain.ExecutionRefusalDependentsNotRebindable {
		t.Fatalf("refusal = %q, want dependentsNotRebindable", got)
	}

	// THE assertion. Not "an error was returned" -- nothing happened.
	if calls := harness.mutator.Calls; len(calls) != 0 {
		t.Fatalf("the host was changed despite the refusal: %v", calls)
	}
}

// A check that could not be PERFORMED is not a check that passed.
//
// The distinction CLAUDE.md's invariant 5 draws in both directions: failing
// closed when a property cannot be established, without reading an
// unperformable check as a failure of the thing itself. Here the graph was
// unavailable, so nothing about the dependents is known, so the provider does
// not move.
func TestAProviderIsRefusedWhenTheAssessmentCouldNotBePerformed(t *testing.T) {
	t.Parallel()

	harness := newExecHarness(t, func(h *execHarness) {
		h.dependencies = unavailableDependencies{}
	})

	_, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})

	if got := executionRefusalFrom(t, err); got != domain.ExecutionRefusalDependentsNotRebindable {
		t.Fatalf("refusal = %q, want dependentsNotRebindable", got)
	}
	if calls := harness.mutator.Calls; len(calls) != 0 {
		t.Fatalf("the host was changed despite an unusable assessment: %v", calls)
	}
}

// An unwired dependency subsystem refuses rather than skipping.
//
// This cannot happen in a wired deployment -- the composition root always
// supplies it, and TestCompositionRootSuppliesDependencyEvidence fails the build
// if that stops being true. The behaviour is pinned anyway, because "the check
// is absent" must never be the quiet path to "the check passed".
func TestAnUnwiredDependencySubsystemRefusesTheRecreation(t *testing.T) {
	t.Parallel()

	harness := newExecHarness(t, func(h *execHarness) {
		h.dependencies = nil
	})

	_, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})

	if got := executionRefusalFrom(t, err); got != domain.ExecutionRefusalDependentsNotRebindable {
		t.Fatalf("refusal = %q, want dependentsNotRebindable", got)
	}
	if calls := harness.mutator.Calls; len(calls) != 0 {
		t.Fatalf("the host was changed with no dependency evidence at all: %v", calls)
	}
}

// The ordinary container is unaffected. A recreation of something nothing
// shares a namespace with proceeds exactly as it did before Phase 16.
func TestAnOrdinaryContainerIsNotGatedByInvariantA(t *testing.T) {
	t.Parallel()

	harness := newExecHarness(t)

	execution, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if execution.ExecutionID == "" {
		t.Fatal("the request was accepted but produced no execution")
	}
}

// Every rebind refusal blocks the provider.
//
// Walked as a table rather than spot-checked, because the vocabulary is the
// safety surface: a refusal added later that did not block would be a hole, and
// this is what makes adding one without thinking about it fail.
func TestEveryRebindRefusalBlocksTheProvider(t *testing.T) {
	t.Parallel()

	for _, refusal := range domain.RebindRefusals {
		t.Run(string(refusal), func(t *testing.T) {
			t.Parallel()

			harness := newExecHarness(t, func(h *execHarness) {
				h.dependencies = blockingDependencies{
					dependent: "sonarr",
					refusal:   refusal,
				}
			})

			_, err := harness.service.Request(context.Background(),
				service.ExecutionRequest{AcquisitionID: execAcquisitionID})

			if got := executionRefusalFrom(t, err); got != domain.ExecutionRefusalDependentsNotRebindable {
				t.Fatalf("refusal %q produced %q, want dependentsNotRebindable", refusal, got)
			}
			if calls := harness.mutator.Calls; len(calls) != 0 {
				t.Fatalf("refusal %q let the host be changed: %v", refusal, calls)
			}
		})
	}
}
