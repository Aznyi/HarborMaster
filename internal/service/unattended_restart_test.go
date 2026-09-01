package service_test

import (
	"context"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Restarting in the middle, and doing it again.
//
// # Why the boundaries matter more than the happy path
//
// An unattended system restarts. It restarts when the host reboots, when an
// operator upgrades it, and -- worst -- when it crashes partway through
// changing a container. The question every one of these asks is the same: does
// the work resume from what was written down, exactly once?
//
// The failure they exist to catch is DUPLICATION, and duplication here is not
// an untidy log. A second acquisition wastes a pull; a second execution stops a
// container that is already being replaced; a second rollback moves a container
// that is already back. So each test restarts at a boundary and then asserts a
// COUNT -- of durable rows and of things actually done to the host.
//
// Nothing is deduplicated on the test side. The counts come from the database
// and from the host's own operation log.

// restartAt stops the world, rebuilds it over the same database, and starts it.
func restartAt(rig *unattendedRig) {
	rig.restart(rigOptions{})
}

// ------------------------------------------- after a plan, before acting --

func TestARestartBetweenPlanningAndActingRunsTheUpdateOnce(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
	})
	defer rig.stop()

	seedDiscovery(t, rig, domain.UpdateMinor)

	// The process dies after planning and before any decision pass.
	restartAt(rig)

	rig.decide()
	rig.await("the update to settle after a restart", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State.Terminal()
	})

	if got := rig.terminalExecution().State; got != domain.ExecutionSucceeded {
		t.Fatalf("the recreation ended %q\n\nhost: %v", got, rig.host.operations())
	}
	assertExactlyOneLifecycle(t, rig)
}

// ------------------------------------ after an acquisition, before executing --

func TestARestartBetweenAcquisitionAndExecutionDoesNotPullOrRecreateTwice(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
	})
	defer rig.stop()

	seedDiscovery(t, rig, domain.UpdateMinor)
	rig.decide()

	// Only the acquisition worker runs, so the world stops with an image
	// downloaded and nothing recreated -- the boundary a restart is most likely
	// to land on, because the pull is the long part.
	rig.startAcquisitionsOnly()
	rig.await("the image to be acquired", func() bool {
		acquisitions, _, err := rig.db.Acquisitions.List(context.Background(),
			store.AcquisitionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(acquisitions) == 1 && acquisitions[0].State.Terminal()
	})
	if got := rig.host.countOps("create:"); got != 0 {
		t.Fatalf("a container was recreated before the restart: %v", rig.host.operations())
	}

	restartAt(rig)

	// The follower picks the acquisition up from its durable row. No second
	// decision pass is run: the work already exists and must be finished.
	rig.await("the recreation to settle after a restart", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State.Terminal()
	})

	if got := rig.terminalExecution().State; got != domain.ExecutionSucceeded {
		t.Fatalf("the recreation ended %q\n\nhost: %v", got, rig.host.operations())
	}
	assertExactlyOneLifecycle(t, rig)
	if got := len(rig.host.pulls()); got != 1 {
		t.Errorf("the image was pulled %d times across the restart, want 1", got)
	}
}

// ------------------------------------------ after a failure, before rollback --

// TestARestartBetweenFailureAndRollbackRecoversExactlyOnce is the boundary that
// matters most.
//
// The process fails an update and dies before it can recover it. The container
// is down -- stopped, replaced by something that does not work -- and the only
// record of what to do about it is on disk. A restart has to finish the job,
// and has to finish it once.
//
// The world comes back WITH the rollback capability, which is both a restart
// landing in the gap and an operator enabling rollback after a bad night. No
// new decision pass is run: the recovery has to come from the durable record of
// the failure, not from re-deciding the container.
func TestARestartBetweenFailureAndRollbackRecoversExactlyOnce(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
	})
	defer rig.stop()

	rig.host.mu.Lock()
	rig.host.badImage = c4cNextDigest
	rig.host.mu.Unlock()

	seedDiscovery(t, rig, domain.UpdateMinor)
	rig.decide()

	// Acquisition and execution run; the rollback worker does not exist in this
	// process, so the world stops with a failed update and no recovery.
	rig.startWithoutRollback()
	rig.await("the recreation to fail", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State.Terminal()
	})
	if got := rig.terminalExecution().State; got != domain.ExecutionFailed {
		t.Fatalf("the recreation ended %q, want failed", got)
	}
	if got := rig.rollbackCount(); got != 0 {
		t.Fatalf("a rollback was created before the restart: %d", got)
	}

	restartAt(rig)

	// No rig.decide() here, deliberately. Anything that happens now happens
	// because the follower read a durable row.
	rig.await("the recovery to settle after a restart", func() bool {
		rollbacks, _, err := rig.db.Rollbacks.List(context.Background(),
			store.RollbackFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(rollbacks) == 1 && rollbacks[0].State.Terminal()
	})

	rollbacks, _, err := rig.db.Rollbacks.List(context.Background(),
		store.RollbackFilter{Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatalf("list rollbacks: %v", err)
	}
	if rollbacks[0].State != domain.RollbackSucceeded {
		t.Fatalf("the rollback ended %q; host: %v",
			rollbacks[0].State, rig.host.operations())
	}
	if !rollbacks[0].Automatic() {
		t.Error("the recovery is not marked automatic")
	}

	// Exactly one of everything, and the service is back.
	if got := rig.rollbackCount(); got != 1 {
		t.Errorf("rollbacks = %d, want 1", got)
	}
	assertExactlyOneLifecycle(t, rig)

	restored, present := rig.host.byName(c4cName)
	if !present || restored.id != c4cContainerID || !restored.running {
		t.Errorf("the original was not restored: %+v", restored)
	}

	// And the operator is told the truth, in the right order.
	rig.await("the recovered notification", func() bool {
		return len(notificationsFor(rig, domain.EventUpdateRecovered)) > 0
	})
	assertOneOf(t, rig, domain.EventExecutionFailed)
	assertOneOf(t, rig, domain.EventUpdateRecovered)
	assertNoneOf(t, rig, domain.EventExecutionSucceeded, domain.EventRollbackStarted)
}

// ------------------------------------------------ after everything settled --

func TestARestartAfterASettledUpdateStartsNothingNew(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
	})
	defer rig.stop()

	seedDiscovery(t, rig, domain.UpdateMinor)
	rig.decide()
	rig.start()
	rig.await("the update to settle", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State.Terminal()
	})
	rig.await("the success notification", func() bool {
		return len(notificationsFor(rig, domain.EventExecutionSucceeded)) > 0
	})

	successes := len(notificationsFor(rig, domain.EventExecutionSucceeded))
	creates := rig.host.countOps("create:")

	// The world restarts and re-discovers, exactly as it would after a reboot.
	restartAt(rig)
	rig.refreshInventory()
	rig.syncIntelReferences()
	rig.evaluateCompliance()
	rig.plan()
	runPassesAndSettle(rig, 5)

	rig.awaitNoChange("a restart after success starts nothing", func() bool {
		return rig.host.countOps("create:") == creates &&
			rig.executionCount() == 1 && rig.rollbackCount() == 0
	})

	// And the operator is not told again about an update that finished before
	// the process restarted.
	if got := len(notificationsFor(rig, domain.EventExecutionSucceeded)); got != successes {
		t.Errorf("%d success notifications after the restart, %d before.\n\n"+
			"A terminal outcome is reported once. Re-announcing it on every "+
			"restart is how an operator learns to ignore the channel.",
			got, successes)
	}
}

// assertExactlyOneLifecycle pins the counts that duplication would move.
func assertExactlyOneLifecycle(t *testing.T, rig *unattendedRig) {
	t.Helper()

	if got := rig.acquisitionCount(); got != 1 {
		t.Errorf("acquisitions = %d, want 1", got)
	}
	if got := rig.executionCount(); got != 1 {
		t.Errorf("executions = %d, want 1", got)
	}
	// The host's own log, which is the claim that matters: a duplicate row is
	// untidy, a duplicate CREATE is a container stopped twice.
	if got := rig.host.countOps("create:"); got != 1 {
		t.Errorf("the host performed %d creates, want 1\n\n%v",
			got, rig.host.operations())
	}
	if got := rig.host.countOps("capture:"); got > 1 {
		t.Errorf("the host was captured %d times, want at most 1", got)
	}
}
