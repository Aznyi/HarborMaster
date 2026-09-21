package service_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The mutation failures the audit found untested.
//
// Each is a statement about what the host looks like afterwards and what the
// record says about it -- the two things an operator has when a recreation
// stops partway.

// TestAFailedRemovalAfterSuccessIsHousekeepingNotAFailure: the replacement is
// proved and serving, the success is recorded, and only then does the removal
// of the parked original fail. The recreation SUCCEEDED; the record says the
// original is still there, and the plan is tidying, not an outage.
func TestAFailedRemovalAfterSuccessIsHousekeepingNotAFailure(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.RemoveErr = docker.ErrMutationFailed
	})

	final := harness.runOnce(t, harness.request(t))

	if final.State != domain.ExecutionSucceeded {
		t.Fatalf("state %q/%q, want succeeded: a removal that failed after the proof is housekeeping",
			final.State, final.Failure)
	}
	if final.OriginalRemoved {
		t.Error("the record claims the original was removed")
	}
	if !harness.mutator.Present(execContainerID) {
		t.Fatal("the original is gone although its removal reported failure")
	}
	if name := harness.mutator.NameOf(execContainerID); !strings.Contains(name, domain.ParkedNameSuffix) {
		t.Errorf("the leftover original is named %q, want its parked name", name)
	}
	replacement := harness.mutator.Containers[final.ReplacementID]
	if replacement == nil || !replacement.Running || harness.mutator.NameOf(final.ReplacementID) != "web" {
		t.Error("the replacement is not serving under the production name")
	}
	if final.Recovery == nil {
		t.Fatal("no recovery plan for the leftover original")
	}
	if final.Recovery.ServiceInterrupted {
		t.Error("the plan says the service is down; the replacement is serving")
	}
	if final.Recovery.Urgency == domain.RecoveryUrgent {
		t.Error("tidying a leftover container is not urgent")
	}
}

// TestAParkRenameThatFailsLeavesTheOriginalStoppedUnderItsOwnName: the stop
// landed and the rename did not. Nothing is created, the original still
// answers to its name, and the record is one the rollback can restore from.
func TestAParkRenameThatFailsLeavesTheOriginalStoppedUnderItsOwnName(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.RenameErr = docker.ErrNameConflict
	})

	final := harness.runOnce(t, harness.request(t))

	if final.Failure != domain.ExecutionFailureRename {
		t.Fatalf("failure %q, want rename", final.Failure)
	}
	if final.Checkpoint != domain.CheckpointOriginalStopped {
		t.Errorf("checkpoint %q, want originalStopped", final.Checkpoint)
	}
	if ops := harness.mutator.Ops(); containsString(ops, "create") {
		t.Errorf("a replacement was created after the park failed: %v", ops)
	}
	if name := harness.mutator.NameOf(execContainerID); name != "web" {
		t.Errorf("the original is named %q, want its own name", name)
	}
	if harness.mutator.Containers[execContainerID].Running {
		t.Error("the original is running; the stop landed before the rename failed")
	}
	if final.ReplacementID != "" {
		t.Errorf("the record names a replacement %q that was never created", final.ReplacementID)
	}
	if !domain.RollbackSufficientCheckpoint(final.Checkpoint) ||
		!domain.RollbackRestoresWithoutReplacement(final.Checkpoint) {
		t.Error("the record is not one the rollback can restore from")
	}
	if final.Recovery == nil || !final.Recovery.ServiceInterrupted {
		t.Error("the plan does not say the service is down")
	}
}

// TestAQuarantineNameCollisionStillStopsTheReplacementAndKeepsBoth: the
// replacement failed verification and the derived quarantine name is taken.
// The replacement is stopped anyway, nothing is removed, and the record
// carries no quarantine name it cannot vouch for.
func TestAQuarantineNameCollisionStillStopsTheReplacementAndKeepsBoth(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		unhealthy := execReplacementDetail()
		unhealthy.State.Health = domain.HealthUnhealthy
		unhealthy.Overview.Health = domain.HealthUnhealthy
		h.runtime.Inspections[execReplacementID] = &docker.Inspection{Detail: unhealthy}
	})
	execution := harness.request(t)

	// Something already holds the name the quarantine will derive. The
	// preflight cannot see it: it checks the read-only runtime, and this
	// container appeared on the mutating side after the check -- the late
	// collision the quarantine has to survive.
	quarantineName, _ := domain.QuarantineContainerName("web", execution.ExecutionID)
	harness.mutator.AddContainer(&docker.FakeContainer{
		ID: docker.FakeContainerID(88), Name: quarantineName, Image: "busybox",
	})

	final := harness.runOnce(t, execution)

	if final.Failure != domain.ExecutionFailureUnhealthy {
		t.Fatalf("failure %q, want unhealthy", final.Failure)
	}
	if final.QuarantineName != "" {
		t.Errorf("the record promises a quarantine name %q the rename never produced", final.QuarantineName)
	}
	if final.Checkpoint == domain.CheckpointReplacementQuarantined {
		t.Error("the checkpoint claims a quarantine that did not happen")
	}
	if harness.mutator.Containers[final.ReplacementID].Running {
		t.Error("the failed replacement is still running under the production name")
	}
	if !harness.mutator.Present(execContainerID) || !harness.mutator.Present(final.ReplacementID) {
		t.Error("a container was removed after a failed quarantine")
	}
	if final.Recovery == nil || !final.Recovery.ServiceInterrupted {
		t.Error("the plan does not say the service is down")
	}
}
