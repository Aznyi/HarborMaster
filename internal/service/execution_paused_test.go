package service_test

import (
	"errors"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// A paused container cannot be stopped: the daemon refuses `stop` and `kill`
// until it is unpaused. Letting one into the transaction produced a failed
// stop, an "uncertain stop" plan marked urgent, and an operator sent to check
// on a container that was exactly as they left it. It is refused in the
// preflight, before anything is issued, with the state refusal.

func TestAPausedContainerIsRefusedBeforeAnyMutation(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.evidence.container.Overview.State = domain.StatePaused
		h.evidence.container.State.State = domain.StatePaused
		h.evidence.container.State.Paused = true
	})

	_, err := harness.service.Request(t.Context(), service.ExecutionRequest{
		AcquisitionID: execAcquisitionID,
	})
	var refused service.ErrExecutionRefused
	if !errors.As(err, &refused) || refused.Refusal != domain.ExecutionRefusalContainerState {
		t.Fatalf("got %v, want a containerState refusal", err)
	}
	if ops := harness.mutator.Ops(); len(ops) != 0 {
		t.Errorf("the host was touched: %v", ops)
	}
}

// TestTheOtherStatesKeepTheirCurrentAdmission pins that nothing else moved:
// running is admitted, exited and created are still admitted (their semantics
// are a later concern), and the transitional and dead states stay refused.
func TestTheOtherStatesKeepTheirCurrentAdmission(t *testing.T) {
	cases := []struct {
		state    domain.ContainerState
		admitted bool
	}{
		{domain.StateRunning, true},
		{domain.StateExited, true},
		{domain.StateCreated, true},
		{domain.StatePaused, false},
		{domain.StateRestarting, false},
		{domain.StateRemoving, false},
		{domain.StateDead, false},
	}
	for _, testCase := range cases {
		t.Run(string(testCase.state), func(t *testing.T) {
			harness := newExecHarness(t, func(h *execHarness) {
				h.evidence.container.Overview.State = testCase.state
				h.evidence.container.State.State = testCase.state
			})
			_, err := harness.service.Request(t.Context(), service.ExecutionRequest{
				AcquisitionID: execAcquisitionID,
			})
			if testCase.admitted && err != nil {
				t.Fatalf("a %s container was refused: %v", testCase.state, err)
			}
			var refused service.ErrExecutionRefused
			if !testCase.admitted && (!errors.As(err, &refused) ||
				refused.Refusal != domain.ExecutionRefusalContainerState) {
				t.Fatalf("a %s container gave %v, want a containerState refusal", testCase.state, err)
			}
		})
	}
}
