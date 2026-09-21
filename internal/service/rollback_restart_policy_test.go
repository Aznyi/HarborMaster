package service_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// A rollback restores the original's restart policy, not merely its process.
//
// The recreation suspended the parked original's policy so a daemon restart
// could not bring it back while a replacement was serving. A rollback that
// started the original and left the suspension in place would have silently
// turned `restart: always` into `restart: no` for good -- the workload would
// run until its next crash and then stay down.

func TestARollbackRestoresTheOriginalsRestartPolicy(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.evidence.execution.OriginalRestartPolicy = domain.RestartPolicy{Name: "always"}
		h.host.with(rbOriginalID, func(c *rbContainer) {
			c.detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "no"}
		})
		h.host.with(rbReplacementID, func(c *rbContainer) {
			c.detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		})
	})

	final := harness.runOnce(t, harness.request(t))
	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state %q/%q/%q, want succeeded", final.State, final.Failure, final.Refusal)
	}

	if got := harness.host.restartPolicyOf(rbOriginalID); got.Name != "always" {
		t.Errorf("the restored original has restart policy %q, want always", got.Name)
	}
	// The parked replacement must not come back after a daemon restart either.
	if got := harness.host.restartPolicyOf(rbReplacementID); got.Name != "no" {
		t.Errorf("the parked replacement has restart policy %q, want no", got.Name)
	}

	// The policy is restored AFTER the name and BEFORE the start, so the
	// container that starts is the container the operator configured.
	want := []string{
		"stop:" + rbReplacementID,
		"park:" + harness.host.nameOf(rbReplacementID),
		"suspend:" + rbReplacementID,
		"restore:" + rbContainerName,
		"restore-restart:" + rbOriginalID + ":always",
		"start:" + rbOriginalID,
	}
	if got := harness.host.operations(); !equalStrings(got, want) {
		t.Errorf("host operations\n got %v\nwant %v", got, want)
	}
}

// TestARollbackOfAnOlderRecordLeavesTheRestartPolicyAlone: a record written
// before the policy was recorded describes a recreation that never suspended
// anything, so there is nothing to restore.
func TestARollbackOfAnOlderRecordLeavesTheRestartPolicyAlone(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.evidence.execution.OriginalRestartPolicy = domain.RestartPolicy{}
	})

	final := harness.runOnce(t, harness.request(t))
	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state %q/%q, want succeeded", final.State, final.Failure)
	}
	for _, op := range harness.host.operations() {
		if strings.HasPrefix(op, "restore-restart:") {
			t.Errorf("a restart policy was written from a record that holds none: %v", harness.host.operations())
		}
	}
}

// TestARollbackThatCannotRestoreTheRestartPolicyDoesNotSettleAsSucceeded: the
// original is not started with the wrong policy and the rollback is not
// reported as complete. The plan tells the operator exactly which policy to
// put back.
func TestARollbackThatCannotRestoreTheRestartPolicyDoesNotSettleAsSucceeded(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.evidence.execution.OriginalRestartPolicy = domain.RestartPolicy{Name: "on-failure", MaximumRetryCount: 3}
		h.host.setErr(&h.host.restoreRestartErr, docker.ErrMutationFailed)
	})

	final := harness.runOnce(t, harness.request(t))
	if final.State != domain.RollbackFailed {
		t.Fatalf("state %q, want failed", final.State)
	}
	if final.Checkpoint != domain.RollbackCheckpointOriginalRestored {
		t.Errorf("checkpoint %q, want originalRestored", final.Checkpoint)
	}
	if harness.host.running(rbOriginalID) {
		t.Error("the original was started before its restart policy was put back")
	}
	if final.Recovery == nil {
		t.Fatal("no recovery plan")
	}
	var mentioned bool
	for _, step := range final.Recovery.Steps {
		if strings.Contains(step.Command, "docker update --restart=on-failure:3") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("the plan never tells the operator to restore the policy: %+v", final.Recovery.Steps)
	}
}

// The no-replacement path restores the policy too: a create failure parked and
// suspended the original exactly as a verification failure would have.
func TestANoReplacementRollbackRestoresTheOriginalsRestartPolicy(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.host.remove(rbReplacementID)
		execution := rbCreateFailedExecution(h.base)
		execution.OriginalRestartPolicy = domain.RestartPolicy{Name: "unless-stopped"}
		h.evidence.execution = execution
		h.host.with(rbOriginalID, func(c *rbContainer) {
			c.detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "no"}
		})
	})

	final := harness.runOnce(t, harness.request(t))
	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state %q/%q, want succeeded", final.State, final.Failure)
	}
	if got := harness.host.restartPolicyOf(rbOriginalID); got.Name != "unless-stopped" {
		t.Errorf("the restored original has restart policy %q, want unless-stopped", got.Name)
	}
	want := []string{
		"restore:" + rbContainerName,
		"restore-restart:" + rbOriginalID + ":unless-stopped",
		"start:" + rbOriginalID,
	}
	if got := harness.host.operations(); !equalStrings(got, want) {
		t.Errorf("host operations\n got %v\nwant %v", got, want)
	}
}

// TestARollbackWhoseParkedReplacementCannotBeNeutralisedSaysSo: the original
// is restored and serving, so the rollback SUCCEEDED -- but the replacement it
// parked still carries a policy that could bring it back beside the original
// after a daemon restart. The record does not call that safe: it carries an
// attention plan naming the container and the command.
func TestARollbackWhoseParkedReplacementCannotBeNeutralisedSaysSo(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.evidence.execution.OriginalRestartPolicy = domain.RestartPolicy{Name: "always"}
		h.host.with(rbReplacementID, func(c *rbContainer) {
			c.detail.Overview.RestartPolicy = domain.RestartPolicy{Name: "always"}
		})
		h.host.setErr(&h.host.suspendErr, docker.ErrMutationFailed)
	})

	final := harness.runOnce(t, harness.request(t))
	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state %q/%q, want succeeded: the original is serving", final.State, final.Failure)
	}
	if !harness.host.running(rbOriginalID) {
		t.Fatal("the original is not running")
	}
	if final.Recovery == nil {
		t.Fatal("no plan; the record calls the parked replacement safe when it is not")
	}
	if final.Recovery.ServiceInterrupted || final.Recovery.Urgency == domain.RecoveryUrgent {
		t.Errorf("the plan says the service is down or urgent: %+v", final.Recovery)
	}
	var neutralise bool
	for _, step := range final.Recovery.Steps {
		if strings.Contains(step.Command, "docker update --restart=no "+final.ReplacementParkedName) {
			neutralise = true
		}
	}
	if !neutralise {
		t.Errorf("the plan does not name the neutralising command: %+v", final.Recovery.Steps)
	}
	if !strings.Contains(strings.ToLower(final.Message), "restart") {
		t.Errorf("the message does not mention the restart hazard: %q", final.Message)
	}
}

// TestARollbackWhoseParkedReplacementIsNeutralisedCarriesNoPlan pins the
// ordinary case: nothing to warn about, no plan.
func TestARollbackWhoseParkedReplacementIsNeutralisedCarriesNoPlan(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.evidence.execution.OriginalRestartPolicy = domain.RestartPolicy{Name: "always"}
	})
	final := harness.runOnce(t, harness.request(t))
	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state %q, want succeeded", final.State)
	}
	if final.Recovery != nil {
		t.Errorf("a clean rollback carries a plan: %+v", final.Recovery)
	}
}
