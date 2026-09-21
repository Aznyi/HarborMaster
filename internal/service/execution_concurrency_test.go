package service_test

import (
	"errors"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Recreation versus rollback.
//
// The rollback preflight refuses while a recreation of the same container is
// in flight. The recreation preflight must refuse the other way round too: a
// rollback that is stopping and renaming the containers under a name is a
// destructive operation on that workload, and a second one must not start
// until it has reached a recorded conclusion. Both directions are checked at
// request time and again inside the worker immediately before the mutation.

func TestARecreationIsRefusedWhileARollbackOfTheSameWorkloadIsActive(t *testing.T) {
	for _, state := range []domain.RollbackState{
		domain.RollbackQueued,
		domain.RollbackValidating,
		domain.RollbackStoppingReplacement,
		domain.RollbackRestoringName,
		domain.RollbackStartingOriginal,
		domain.RollbackVerifyingOriginal,
	} {
		t.Run(string(state), func(t *testing.T) {
			harness := newExecHarness(t, func(h *execHarness) {
				h.evidence.rollbackFor = "web"
				h.evidence.rollbackState = state
			})

			_, err := harness.service.Request(t.Context(), service.ExecutionRequest{
				AcquisitionID: execAcquisitionID,
			})
			var refused service.ErrExecutionRefused
			if !errors.As(err, &refused) || refused.Refusal != domain.ExecutionRefusalConflict {
				t.Fatalf("got %v, want a conflict refusal", err)
			}
			if ops := harness.mutator.Ops(); len(ops) != 0 {
				t.Errorf("the host was touched: %v", ops)
			}
		})
	}
}

// TestASettledRollbackDoesNotBlockARecreation: only an ACTIVE rollback
// conflicts. A rollback that finished last week is history.
func TestASettledRollbackDoesNotBlockARecreation(t *testing.T) {
	for _, state := range []domain.RollbackState{
		domain.RollbackSucceeded, domain.RollbackFailed,
		domain.RollbackCancelled, domain.RollbackExpired,
	} {
		t.Run(string(state), func(t *testing.T) {
			harness := newExecHarness(t, func(h *execHarness) {
				h.evidence.rollbackFor = "web"
				h.evidence.rollbackState = state
			})
			if _, err := harness.service.Request(t.Context(), service.ExecutionRequest{
				AcquisitionID: execAcquisitionID,
			}); err != nil {
				t.Fatalf("a settled rollback blocked a recreation: %v", err)
			}
		})
	}
}

// TestARollbackOfAnotherWorkloadDoesNotBlockARecreation: the guard is per
// workload. Two unrelated containers may be worked on at once when the
// configured concurrency allows it.
func TestARollbackOfAnotherWorkloadDoesNotBlockARecreation(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.evidence.rollbackFor = "api"
		h.evidence.rollbackState = domain.RollbackStoppingReplacement
	})
	if _, err := harness.service.Request(t.Context(), service.ExecutionRequest{
		AcquisitionID: execAcquisitionID,
	}); err != nil {
		t.Fatalf("a rollback of another workload blocked this recreation: %v", err)
	}
}

// TestARollbackThatStartsAfterTheRequestIsCaughtByTheWorkerPreflight: the
// request passed; a rollback was requested before the worker claimed the
// recreation. The second preflight, immediately before the first mutation,
// refuses -- and nothing on the host is touched.
func TestARollbackThatStartsAfterTheRequestIsCaughtByTheWorkerPreflight(t *testing.T) {
	harness := newExecHarness(t)
	execution := harness.request(t)

	harness.evidence.mu.Lock()
	harness.evidence.rollbackFor = "web"
	harness.evidence.rollbackState = domain.RollbackValidating
	harness.evidence.mu.Unlock()

	final := harness.runOnce(t, execution)
	if final.State != domain.ExecutionFailed || final.Refusal != domain.ExecutionRefusalConflict {
		t.Fatalf("state %q refusal %q, want failed/conflict", final.State, final.Refusal)
	}
	if ops := harness.mutator.Ops(); len(ops) != 0 {
		t.Errorf("the host was touched after a rollback had begun: %v", ops)
	}
}

// TestAFailedRollbackLookupRefusesTheRecreation: a check that could not be
// performed establishes nothing, and nothing is not permission.
func TestAFailedRollbackLookupRefusesTheRecreation(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.evidence.rollbackErr = errors.New("database is locked")
	})
	if _, err := harness.service.Request(t.Context(), service.ExecutionRequest{
		AcquisitionID: execAcquisitionID,
	}); err == nil {
		t.Fatal("a recreation was accepted while the rollback check could not be performed")
	}
	if ops := harness.mutator.Ops(); len(ops) != 0 {
		t.Errorf("the host was touched: %v", ops)
	}
}
