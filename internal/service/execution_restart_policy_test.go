package service_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Parked and quarantined containers must not restart by themselves.
//
// Docker restarts a stopped `always` container when the daemon restarts, and an
// `unless-stopped` one unless it was explicitly stopped -- which a replacement
// that was created and never started was not. A parked original or a
// quarantined replacement that comes back after a reboot races the serving
// container for its ports. So the moment HarborMaster parks or quarantines a
// container it suspends the policy, and it records the original's policy so a
// rollback can put it back.

// suspendsFor returns the container ids a run suspended restart on.
func suspendsFor(mutator *docker.FakeMutator) []string {
	var ids []string
	for _, call := range mutator.Calls {
		if call.Op == "suspendRestart" {
			ids = append(ids, call.ContainerID)
		}
	}
	return ids
}

func TestAParkedOriginalAndAQuarantinedReplacementCannotRestartByThemselves(t *testing.T) {
	for _, policy := range []string{"always", "unless-stopped", "on-failure"} {
		t.Run(policy, func(t *testing.T) {
			harness := newExecHarness(t, func(h *execHarness) {
				original := h.mutator.Containers[execContainerID]
				original.Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: policy}
				h.runtime.Inspections[execContainerID].Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: policy}
				h.evidence.container.Overview.RestartPolicy = domain.RestartPolicy{Name: policy}

				unhealthy := execReplacementDetail()
				unhealthy.State.Health = domain.HealthUnhealthy
				unhealthy.Overview.Health = domain.HealthUnhealthy
				h.runtime.Inspections[execReplacementID] = &docker.Inspection{Detail: unhealthy}
			})

			final := harness.runOnce(t, harness.request(t))
			if final.Failure != domain.ExecutionFailureUnhealthy {
				t.Fatalf("failure = %q, want unhealthy", final.Failure)
			}

			original := harness.mutator.Containers[execContainerID]
			if got := original.Detail.Overview.RestartPolicy.Name; got != "no" {
				t.Errorf("the parked original still has restart policy %q; a daemon restart would bring it back", got)
			}
			replacement := harness.mutator.Containers[final.ReplacementID]
			if got := replacement.Detail.Overview.RestartPolicy.Name; got != "no" {
				t.Errorf("the quarantined replacement still has restart policy %q", got)
			}
			// The policy is RECORDED, so a rollback can restore it.
			if got := final.OriginalRestartPolicy.Name; got != policy {
				t.Errorf("recorded original restart policy %q, want %q", got, policy)
			}
			// Exactly the two containers HarborMaster parked, and nothing else.
			if got := suspendsFor(harness.mutator); !equalStrings(got, []string{execContainerID, final.ReplacementID}) {
				t.Errorf("restart was suspended on %v, want the original then the replacement", got)
			}
		})
	}
}

func TestAContainerThatNeverRestartsIsNotTouched(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.Containers[execContainerID].Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "no"}
		h.runtime.Inspections[execContainerID].Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "no"}
		h.evidence.container.Overview.RestartPolicy = domain.RestartPolicy{Name: "no"}
		h.mutator.CreateErr = docker.ErrMutationFailed
	})

	final := harness.runOnce(t, harness.request(t))
	if final.Failure != domain.ExecutionFailureCreate {
		t.Fatalf("failure = %q, want create", final.Failure)
	}
	if got := suspendsFor(harness.mutator); len(got) != 0 {
		t.Errorf("restart was suspended on %v for a container whose policy is already no", got)
	}
	if got := final.OriginalRestartPolicy.Name; got != "no" {
		t.Errorf("recorded original restart policy %q, want no", got)
	}
}

func TestASuccessfulRecreationKeepsTheReplacementsRestartPolicy(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.Containers[execContainerID].Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		h.runtime.Inspections[execContainerID].Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		h.evidence.container.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		replacement := execReplacementDetail()
		replacement.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		h.runtime.Inspections[execReplacementID] = &docker.Inspection{Detail: replacement}
	})

	final := harness.runOnce(t, harness.request(t))
	if final.State != domain.ExecutionSucceeded {
		t.Fatalf("state %q/%q, want succeeded", final.State, final.Failure)
	}
	replacement := harness.mutator.Containers[final.ReplacementID]
	if got := replacement.Detail.Overview.RestartPolicy.Name; got != "always" {
		t.Errorf("the new production container has restart policy %q, want always", got)
	}
	// The parked original was suspended before it was removed; the replacement
	// was never touched.
	if got := suspendsFor(harness.mutator); !equalStrings(got, []string{execContainerID}) {
		t.Errorf("restart was suspended on %v, want only the parked original", got)
	}
}

// TestAFailedRestartSuspensionStopsBeforeTheCreate: the parked original still
// carries a policy that could start it by itself after a daemon restart.
// Creating a replacement beside it would set up exactly the race the
// suspension exists to prevent, so the recreation stops here -- with the
// original parked, nothing created, and a record the rollback can restore
// from.
func TestAFailedRestartSuspensionStopsBeforeTheCreate(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.Containers[execContainerID].Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		h.runtime.Inspections[execContainerID].Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		h.evidence.container.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		h.mutator.SuspendRestartErrFor = domain.ParkedNameSuffix
	})

	final := harness.runOnce(t, harness.request(t))
	if final.State != domain.ExecutionFailed || final.Failure != domain.ExecutionFailureRestartPolicy {
		t.Fatalf("state %q/%q, want failed/restartPolicy", final.State, final.Failure)
	}
	if final.Checkpoint != domain.CheckpointOriginalParked {
		t.Errorf("checkpoint %q, want originalParked", final.Checkpoint)
	}
	if ops := harness.mutator.Ops(); containsString(ops, "create") {
		t.Errorf("a replacement was created beside an original that can restart by itself: %v", ops)
	}
	if final.ReplacementID != "" {
		t.Errorf("the record names a replacement %q that was never created", final.ReplacementID)
	}
	original := harness.mutator.Containers[execContainerID]
	if original.Running || !strings.Contains(original.Name, domain.ParkedNameSuffix) {
		t.Errorf("the original is running=%v under %q; want stopped and parked", original.Running, original.Name)
	}
	if got := original.Detail.Overview.RestartPolicy.Name; got != "always" {
		t.Errorf("the original's policy is %q; a failed suspension must not have changed it", got)
	}
	if final.OriginalRestartPolicy.Name != "always" {
		t.Errorf("recorded policy %q, want always", final.OriginalRestartPolicy.Name)
	}
	// The plan is honest about the hazard, and the record is one the rollback
	// can restore from without a replacement.
	if final.Recovery == nil || !final.Recovery.ServiceInterrupted {
		t.Fatal("the plan does not say the service is down")
	}
	var neutralise bool
	for _, step := range final.Recovery.Steps {
		if strings.Contains(step.Command, "docker update --restart=no") {
			neutralise = true
		}
	}
	if !neutralise {
		t.Errorf("the plan never tells the operator the parked original can restart by itself: %+v", final.Recovery.Steps)
	}
	if !domain.RollbackRestoresWithoutReplacement(final.Checkpoint) {
		t.Error("the record is not one the rollback can restore from")
	}
}

// TestAQuarantineWhoseRestartCannotBeSuspendedIsNotRecordedAsQuarantined: the
// replacement is stopped and moved off the production name, which is the part
// that protects service NOW. The checkpoint that would call it quarantined is
// withheld, because "quarantined" means it cannot come back -- and the plan
// tells the operator exactly what is still unsafe.
func TestAQuarantineWhoseRestartCannotBeSuspendedIsNotRecordedAsQuarantined(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.Containers[execContainerID].Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		h.runtime.Inspections[execContainerID].Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		h.evidence.container.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		unhealthy := execReplacementDetail()
		unhealthy.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		unhealthy.State.Health = domain.HealthUnhealthy
		unhealthy.Overview.Health = domain.HealthUnhealthy
		h.runtime.Inspections[execReplacementID] = &docker.Inspection{Detail: unhealthy}
		h.mutator.SuspendRestartErrFor = domain.QuarantineNameSuffix
	})

	final := harness.runOnce(t, harness.request(t))
	if final.Failure != domain.ExecutionFailureUnhealthy {
		t.Fatalf("failure %q, want unhealthy", final.Failure)
	}
	if final.Checkpoint == domain.CheckpointReplacementQuarantined {
		t.Error("the checkpoint calls the replacement quarantined although it can restart by itself")
	}
	if final.QuarantineName == "" {
		t.Error("the record does not say where the replacement was moved")
	}
	replacement := harness.mutator.Containers[final.ReplacementID]
	if replacement.Running || !strings.Contains(replacement.Name, domain.QuarantineNameSuffix) {
		t.Errorf("the replacement is running=%v under %q; want stopped under its quarantine name", replacement.Running, replacement.Name)
	}
	if !harness.mutator.Present(execContainerID) || !harness.mutator.Present(final.ReplacementID) {
		t.Error("a container was removed; evidence must be kept")
	}
	var neutralise bool
	for _, step := range final.Recovery.Steps {
		if strings.Contains(step.Command, "docker update --restart=no "+final.QuarantineName) {
			neutralise = true
		}
	}
	if !neutralise {
		t.Errorf("the plan never tells the operator the quarantined replacement can restart by itself: %+v", final.Recovery.Steps)
	}
	// The original's own suspension landed, so the record is still one the
	// rollback can act on.
	if !domain.RollbackSufficientCheckpoint(final.Checkpoint) {
		t.Error("the record is not rollbackable")
	}
}

func TestRestartSuspensionNeverTouchesAnUnrelatedContainer(t *testing.T) {
	bystander := docker.FakeContainerID(77)
	harness := newExecHarness(t, func(h *execHarness) {
		other := &docker.FakeContainer{ID: bystander, Name: "db", Image: "postgres:16", Running: true}
		other.Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		h.mutator.AddContainer(other)
		h.mutator.Containers[execContainerID].Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		h.runtime.Inspections[execContainerID].Detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		h.evidence.container.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		h.mutator.CreateErr = docker.ErrMutationFailed
	})

	harness.runOnce(t, harness.request(t))

	if got := harness.mutator.Containers[bystander].Detail.Overview.RestartPolicy.Name; got != "always" {
		t.Errorf("an unrelated container's restart policy became %q", got)
	}
	for _, id := range suspendsFor(harness.mutator) {
		if id == bystander {
			t.Error("restart was suspended on a container the recreation never touched")
		}
	}
}
