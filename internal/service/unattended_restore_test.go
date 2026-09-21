package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Restart-policy failures and manual restoration, end to end through the real
// services and their own schedulers.

// awaitRestoreSettled waits for the one execution's restore to reach a verdict.
func awaitRestoreSettled(rig *unattendedRig) domain.Execution {
	rig.t.Helper()
	rig.await("the restore to settle", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 &&
			executions[0].State.Terminal() && executions[0].Restore.State.Settled()
	})
	return rig.terminalExecution()
}

// ---- Scenario B4: the original's restart policy cannot be suspended --------

func TestScenarioB4AnOriginalWhoseRestartCannotBeSuspendedIsRestored(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
	})
	defer rig.stop()

	rig.host.setRestartPolicy(c4cContainerID, domain.RestartPolicy{Name: "always"})
	rig.host.mu.Lock()
	rig.host.failSuspendOf = domain.ParkedNameSuffix
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

	execution := rig.terminalExecution()
	if execution.Failure != domain.ExecutionFailureRestartPolicy {
		t.Fatalf("failure %q, want restartPolicy\n\nhost: %v", execution.Failure, rig.host.operations())
	}
	if got := rig.host.countOps("create:"); got != 0 {
		t.Errorf("a replacement was created beside an original that can restart by itself (%d creates)", got)
	}
	rollbacks, _, _ := rig.db.Rollbacks.List(context.Background(), store.RollbackFilter{Page: store.Page{Limit: 10}})
	if rollbacks[0].State != domain.RollbackSucceeded {
		t.Fatalf("the rollback ended %q/%q/%q\n\nhost: %v",
			rollbacks[0].State, rollbacks[0].Failure, rollbacks[0].Refusal, rig.host.operations())
	}
	restored, present := rig.host.byName(c4cName)
	if !present || restored.id != c4cContainerID || !restored.running {
		t.Fatalf("the original is not serving under %q\n\nhost: %v", c4cName, rig.host.operations())
	}
	if got := rig.host.restartPolicyOf(c4cContainerID); got.Name != "always" {
		t.Errorf("the restored original has restart policy %q, want always", got.Name)
	}
	rig.stop()

	rig.host.restartDaemon()
	running := rig.host.runningNames()
	if len(running) != 1 {
		t.Errorf("after a daemon restart %v are running; want only %q", running, c4cName)
	}
}

// ---- Scenario C3: the quarantine's restart policy cannot be suspended ------

// The replacement is stopped and off the production name, but it could come
// back after a daemon restart. The rollback that follows parks it again under
// its own name and suspends it there -- a second chance the record has to
// earn, not assume -- and after that a daemon restart brings back only the
// original.
func TestScenarioC3AQuarantineWhoseRestartCannotBeSuspendedIsSecuredByTheRestore(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
	})
	defer rig.stop()

	rig.host.setRestartPolicy(c4cContainerID, domain.RestartPolicy{Name: "always"})
	rig.host.mu.Lock()
	rig.host.badImage = c4cNextDigest
	rig.host.failSuspendOf = domain.QuarantineNameSuffix
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

	execution := rig.terminalExecution()
	if execution.Checkpoint == domain.CheckpointReplacementQuarantined {
		t.Error("the record calls the replacement quarantined although its restart could not be suspended")
	}
	if execution.QuarantineName == "" {
		t.Error("the record does not say where the replacement was moved")
	}
	rollbacks, _, _ := rig.db.Rollbacks.List(context.Background(), store.RollbackFilter{Page: store.Page{Limit: 10}})
	if rollbacks[0].State != domain.RollbackSucceeded {
		t.Fatalf("the rollback ended %q/%q\n\nhost: %v", rollbacks[0].State, rollbacks[0].Failure, rig.host.operations())
	}
	if rollbacks[0].Recovery != nil {
		t.Errorf("the rollback carries a plan although it secured the replacement: %+v", rollbacks[0].Recovery)
	}
	rig.stop()

	rig.host.restartDaemon()
	running := rig.host.runningNames()
	if _, ok := running[c4cName]; !ok || len(running) != 1 {
		t.Errorf("after a daemon restart %v are running; want only %q", running, c4cName)
	}
}

// ---- Scenario M: a manual update fails and is restored without anyone asking

// manualUpdate drives the manual path an operator uses: a plan, an acquisition
// request, then an execution request. Both requests carry the operator.
func manualUpdate(t *testing.T, rig *unattendedRig) domain.Execution {
	t.Helper()
	operator := domain.Requester{UserID: "usr_0011223344556677889a", Username: "colby"}

	// A snapshot first, as the update page has an operator do: a plan assessed
	// with no configuration snapshot lands on manual review, and this scenario
	// is about the failure path, not the approval gate.
	rig.refreshInventory()
	if result := rig.assurance.EnsureCurrent(context.Background(), c4cContainerID,
		domain.SnapshotTriggerManual); !result.Usable() {
		t.Fatalf("snapshot assurance: %+v", result)
	}
	plan := seedDiscovery(t, rig, domain.UpdateMinor)
	acquisition, err := rig.acquisitions.Request(context.Background(), service.AcquisitionRequest{
		PlanID: plan.PlanID, RequestedBy: operator,
	})
	if err != nil {
		t.Fatalf("acquisition refused: %v", err)
	}
	rig.await("the acquisition to succeed", func() bool {
		current, err := rig.db.Acquisitions.Get(context.Background(), acquisition.AcquisitionID)
		return err == nil && current.State == domain.AcquisitionSucceeded
	})
	rig.refreshInventory()
	rig.evaluateCompliance()

	execution, err := rig.executions.Request(context.Background(), service.ExecutionRequest{
		AcquisitionID: acquisition.AcquisitionID, RequestedBy: operator,
	})
	if err != nil {
		t.Fatalf("execution refused: %v", err)
	}
	return execution
}

func TestScenarioMAManualUpdateThatFailsIsRestoredWithoutAnyoneAsking(t *testing.T) {
	rig := newUnattendedRig(t)
	defer rig.stop()

	rig.host.setRestartPolicy(c4cContainerID, domain.RestartPolicy{Name: "always"})
	rig.host.mu.Lock()
	rig.host.badImage = c4cNextDigest
	rig.host.mu.Unlock()
	rig.start()

	requested := manualUpdate(t, rig)
	if requested.Automatic() {
		t.Fatal("the manual request reads as automatic")
	}

	execution := awaitRestoreSettled(rig)
	if execution.State != domain.ExecutionFailed {
		t.Fatalf("the update ended %q, want failed", execution.State)
	}
	if execution.Restore.State != domain.RestoreRestored {
		t.Fatalf("restore = %+v, want restored\n\nhost: %v", execution.Restore, rig.host.operations())
	}
	if execution.Recovery == nil || execution.Recovery.ServiceInterrupted {
		t.Errorf("the record still says the service is down: %+v", execution.Recovery)
	}

	rollback, err := rig.db.Rollbacks.Get(context.Background(), execution.Restore.RollbackID)
	if err != nil {
		t.Fatalf("the restore names rollback %q which cannot be read: %v", execution.Restore.RollbackID, err)
	}
	if rollback.State != domain.RollbackSucceeded || !rollback.ManualRestore() || rollback.Automatic() {
		t.Errorf("rollback = %q manualRestore=%v automatic=%v", rollback.State, rollback.ManualRestore(), rollback.Automatic())
	}
	if rollback.RequestedBy.Username != "colby" {
		t.Errorf("the restore is attributed to %+v, want the operator who asked for the update", rollback.RequestedBy)
	}

	restored, present := rig.host.byName(c4cName)
	if !present || restored.id != c4cContainerID || !restored.running {
		t.Fatalf("the original is not serving under %q\n\nhost: %v", c4cName, rig.host.operations())
	}
	if got := rig.host.restartPolicyOf(c4cContainerID); got.Name != "always" {
		t.Errorf("the restored original has restart policy %q, want always", got.Name)
	}
	if quarantined := findQuarantined(rig); quarantined == "" {
		t.Error("the failed replacement was not kept as evidence")
	}
	if got := rig.host.countOps("remove:"); got != 0 {
		t.Errorf("removes = %d, want 0", got)
	}

	// The operator is told the update failed, then that it was restored -- and
	// not that a rollback "started" or "succeeded" as if they had asked for one.
	assertOneOf(t, rig, domain.EventExecutionFailed)
	assertOneOf(t, rig, domain.EventUpdateRecovered)
	assertNoneOf(t, rig, domain.EventRollbackStarted, domain.EventRollbackSucceeded, domain.EventExecutionSucceeded)
	failed := notificationFor(rig, domain.EventExecutionFailed)
	if body := strings.ToLower(failed.Body); !strings.Contains(body, "restor") {
		t.Errorf("the failure message does not say a restore is under way: %s", failed.Body)
	}
	recovered := notificationFor(rig, domain.EventUpdateRecovered)
	if body := strings.ToLower(recovered.Body); strings.Contains(body, "unattended") || strings.Contains(body, "paused") {
		t.Errorf("a manual restore is described as an unattended, pausing update: %s", recovered.Body)
	}
	// Nothing paused automation: no policy governs this container.
	if _, err := rig.db.Automation.PauseFor(context.Background(), c4cName); err == nil {
		t.Error("a manual restore paused automation for a container no policy governs")
	}
}

func TestScenarioM2AManualFailureWithoutTheRollbackCapabilityIsReportedAsNotRestorable(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) { o.rollbackDisabled = true })
	defer rig.stop()

	rig.host.mu.Lock()
	rig.host.badImage = c4cNextDigest
	rig.host.mu.Unlock()
	rig.start()

	manualUpdate(t, rig)
	execution := awaitRestoreSettled(rig)
	if execution.Restore.State != domain.RestoreUnavailable {
		t.Fatalf("restore = %+v, want unavailable", execution.Restore)
	}
	if execution.Recovery == nil || !execution.Recovery.ServiceInterrupted {
		t.Error("the record does not say the service is down")
	}
	if running := rig.host.runningNames(); len(running) != 0 {
		t.Errorf("something is running with rollback disabled: %v", running)
	}
}
