package service_test

import (
	"context"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Scenario C: the Docker daemon restarts while HarborMaster holds a parked or
// quarantined container.
//
// The host double models the daemon's restore rule -- an `always` container
// comes back whatever stopped it, an `unless-stopped` one comes back unless a
// stop was explicitly issued -- and the tests ask the only question that
// matters: after the restart, which containers are running under which names.

// TestScenarioCAParkedContainerDoesNotComeBackAfterADaemonRestart: the
// failed update is rolled back, the original serves again with its `always`
// policy restored, and the quarantined replacement stays down across a daemon
// restart.
func TestScenarioCAParkedContainerDoesNotComeBackAfterADaemonRestart(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
	})
	defer rig.stop()

	rig.host.setRestartPolicy(c4cContainerID, domain.RestartPolicy{Name: "always"})
	rig.host.mu.Lock()
	rig.host.badImage = c4cNextDigest
	rig.host.mu.Unlock()

	seedDiscovery(t, rig, domain.UpdateMinor)
	rig.start()
	if run, _ := rig.decide(); run.Submitted != 1 {
		t.Fatalf("submitted = %d, want 1", run.Submitted)
	}
	rig.await("the rollback to settle", func() bool {
		rollbacks, _, err := rig.db.Rollbacks.List(context.Background(),
			store.RollbackFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(rollbacks) == 1 && rollbacks[0].State.Terminal()
	})
	rig.stop()

	// The original is back with the policy the operator configured, and the
	// quarantined replacement cannot restart on its own.
	if got := rig.host.restartPolicyOf(c4cContainerID); got.Name != "always" {
		t.Fatalf("the restored original has restart policy %q, want always", got.Name)
	}
	quarantinedName := findQuarantined(rig)
	if quarantinedName == "" {
		t.Fatal("no quarantined replacement was found")
	}
	quarantined, _ := rig.host.byName(quarantinedName)
	if got := quarantined.detail.Overview.RestartPolicy; got.Name != "no" {
		t.Fatalf("the quarantined replacement has restart policy %q, want no", got.Name)
	}

	// THE DAEMON RESTARTS.
	rig.host.restartDaemon()

	running := rig.host.runningNames()
	if _, ok := running[c4cName]; !ok {
		t.Errorf("the original did not come back under %q after the daemon restart: %v", c4cName, running)
	}
	for name := range running {
		if name != c4cName {
			t.Errorf("%q is running after the daemon restart; a parked or quarantined container "+
				"must never compete with the serving one", name)
		}
	}
}

// TestScenarioCParkedContainersStayDownWhenNothingWasRestored: HarborMaster
// went down after a failed update and no rollback ran. Nothing serves, and a
// daemon restart must not turn that into two containers fighting for the
// workload's ports.
func TestScenarioCParkedContainersStayDownWhenNothingWasRestored(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
	})
	defer rig.stop()

	rig.host.setRestartPolicy(c4cContainerID, domain.RestartPolicy{Name: "always"})
	rig.host.mu.Lock()
	rig.host.badImage = c4cNextDigest
	rig.host.mu.Unlock()

	seedDiscovery(t, rig, domain.UpdateMinor)
	rig.startWithoutRollback()
	if run, _ := rig.decide(); run.Submitted != 1 {
		t.Fatalf("submitted = %d, want 1", run.Submitted)
	}
	rig.await("the recreation to fail", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State == domain.ExecutionFailed
	})
	rig.stop()

	rig.host.restartDaemon()

	if running := rig.host.runningNames(); len(running) != 0 {
		t.Errorf("containers came back after the daemon restart: %v\n"+
			"the original is parked and the replacement quarantined; neither may restart by itself",
			running)
	}
}
