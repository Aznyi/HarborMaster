package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The unattended promise, against a real daemon.
//
// Every assertion below is made twice over: once against HarborMaster's own
// durable rows, and once against the daemon's event stream. The two answer
// different questions -- what HarborMaster meant to do, and what actually
// happened to the host -- and a release is only safe when they agree.

// ------------------------------- PART 1: real automatic success --

func TestRealDockerAnAutomaticUpdateCompletesUnattended(t *testing.T) {
	const name = "hm-c4c1-success"

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.policies = []domain.UpdatePolicy{c4c1Policy(name, domain.ModeAutomatic)}
	})

	originalID := containerID(name)
	if originalID == "" {
		t.Fatal("the disposable workload is not on the host")
	}

	rig.seed(domain.UpdateMinor, domain.CheckOK)

	plan, err := rig.db.Plans.Current(context.Background(), originalID)
	if err != nil {
		t.Fatalf("no current plan: %v", err)
	}
	if plan.ProposedDigest != rig.nextDigest {
		t.Fatalf("the plan proposes %q, want the registry's digest %q",
			plan.ProposedDigest, rig.nextDigest)
	}

	// From here on, every container event belongs to HarborMaster. The rig's own
	// `docker run` that created the workload is before this mark, and counting
	// it would make "exactly one create" mean two.
	sinceDecision := time.Now().UTC()

	// One decision pass, then the schedulers do everything else. Nothing below
	// calls Request on any service.
	if run, decisions := rig.decide(); run.Submitted != 1 {
		t.Fatalf("submitted = %d, want 1\n\n  %s\n\n  plan: %s",
			run.Submitted, rig.mine(decisions), rig.planFactors(originalID))
	}
	rig.start()

	rig.await("the recreation to settle", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State.Terminal()
	})

	execution := rig.terminalExecution()
	if execution.State != domain.ExecutionSucceeded {
		t.Fatalf("the recreation ended %q (failure %q, checkpoint %q)",
			execution.State, execution.Failure, execution.Checkpoint)
	}

	// ---- the daemon's account ----
	events := dockerEvents(t, rig.startedAt)
	assertOnlyDisposableContainersTouched(t, events)

	byHarborMaster := dockerEvents(t, sinceDecision)
	if got := countEvents(byHarborMaster, "create"); got != 1 {
		t.Errorf("the daemon created %d containers for HarborMaster, want exactly 1\n\t%s",
			got, strings.Join(byHarborMaster, "\n\t"))
	}
	for _, action := range []string{"stop", "rename", "destroy"} {
		if countEvents(events, action) == 0 {
			t.Errorf("the daemon never reported %q; the recreation did not "+
				"happen the way the record claims\n\t%s",
				action, strings.Join(events, "\n\t"))
		}
	}

	// ---- the host afterwards ----
	replacementID := containerID(name)
	if replacementID == "" {
		t.Fatal("nothing answers to the workload name any more")
	}
	if replacementID == originalID {
		t.Error("the container was never replaced")
	}
	if !isRunning(name) {
		t.Error("the replacement is not running")
	}
	if got := runningImage(name); !strings.Contains(got, rig.nextDigest) {
		t.Errorf("the replacement was created from %q, want the approved digest %q",
			got, rig.nextDigest)
	}
	// The original is gone, and the daemon says so.
	if containerID(originalID) != "" {
		t.Error("the original container is still on the host after a settled update")
	}
	// No parked original left behind on the successful path.
	for _, leftover := range disposableContainers(t) {
		if strings.Contains(leftover, domain.ParkedNameSuffix) {
			t.Errorf("a parked original was leaked: %q", leftover)
		}
	}

	// ---- counts ----
	if got := rig.count("acquisitions"); got != 1 {
		t.Errorf("acquisitions = %d, want 1", got)
	}
	if got := rig.count("executions"); got != 1 {
		t.Errorf("executions = %d, want 1", got)
	}
	if got := rig.count("rollbacks"); got != 0 {
		t.Errorf("rollbacks = %d, want 0", got)
	}

	// ---- notifications ----
	rig.await("the success notification", func() bool {
		return len(notificationsFor2(rig, domain.EventExecutionSucceeded)) > 0
	})
	assertOneLogical(t, rig, domain.EventExecutionSucceeded)
	assertNone(t, rig, domain.EventExecutionFailed, domain.EventUpdateRecovered,
		domain.EventRollbackStarted, domain.EventRollbackSucceeded, domain.EventRollbackFailed)

	// ---- repeated passes do nothing ----
	rig.refreshInventory()
	rig.syncIntel()
	rig.evaluateCompliance()
	rig.plan()

	creates := countEvents(dockerEvents(t, sinceDecision), "create")
	for i := 0; i < 5; i++ {
		rig.decide()
	}
	rig.awaitNoChange("no second lifecycle", func() bool {
		return rig.count("executions") == 1 && rig.count("rollbacks") == 0 &&
			countEvents(dockerEvents(t, sinceDecision), "create") == creates
	})
}

// ------------------------- PART 2: real failure and auto-recovery --

func TestRealDockerAFailedAutomaticUpdateRecoversItself(t *testing.T) {
	const name = "hm-c4c1-recovery"

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		// The workload declares a health check that asserts its own version --
		// the kind an application really ships. It passes on 3.20 and FAILS on
		// 3.21, so the replacement is genuinely unhealthy and HarborMaster's
		// own verification path reaches that conclusion from the daemon.
		// Nothing writes execution state directly.
		o.healthCheck = c4c1VersionCheck
		o.policies = []domain.UpdatePolicy{c4c1Policy(name, domain.ModeAutomatic)}
	})

	originalID := containerID(name)
	rig.seed(domain.UpdateMinor, domain.CheckOK)

	// Everything after this mark is HarborMaster's doing; the rig's own
	// `docker run` is before it.
	sinceDecision := time.Now().UTC()

	if run, decisions := rig.decide(); run.Submitted != 1 {
		t.Fatalf("submitted = %d, want 1\n\n  %s\n\n  plan: %s",
			run.Submitted, rig.mine(decisions), rig.planFactors(originalID))
	}
	rig.start()

	rig.await("the rollback to settle", func() bool {
		rollbacks, _, err := rig.db.Rollbacks.List(context.Background(),
			store.RollbackFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(rollbacks) == 1 && rollbacks[0].State.Terminal()
	})

	if got := rig.terminalExecution().State; got != domain.ExecutionFailed {
		t.Fatalf("the recreation ended %q, want failed", got)
	}

	rollbacks, _, err := rig.db.Rollbacks.List(context.Background(),
		store.RollbackFilter{Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatalf("list rollbacks: %v", err)
	}
	if rollbacks[0].State != domain.RollbackSucceeded {
		t.Fatalf("the rollback ended %q", rollbacks[0].State)
	}
	if !rollbacks[0].Automatic() {
		t.Error("the recovery is not marked automatic")
	}

	// ---- the daemon says the service is back ----
	events := dockerEvents(t, rig.startedAt)
	assertOnlyDisposableContainersTouched(t, events)

	restoredID := containerID(name)
	if restoredID != originalID {
		t.Errorf("the container answering to %q is %q, want the original %q",
			name, restoredID, originalID)
	}
	if !isRunning(name) {
		t.Error("THE RESTORED SERVICE IS NOT RUNNING.\n\n" +
			"An automatic rollback that leaves the workload down is worse than " +
			"none: the operator was told it recovered.")
	}
	if got := runningImage(name); !strings.Contains(got, c4c1CurrentRef) &&
		!strings.Contains(got, rig.currentDigest) {
		t.Errorf("the restored container runs %q, want the original image", got)
	}

	// The failed replacement is retained as evidence, under one of the two
	// markers -- quarantined by the execution, or parked by the rollback.
	retained := ""
	for _, leftover := range disposableContainers(t) {
		if strings.Contains(leftover, domain.QuarantineNameSuffix) ||
			strings.Contains(leftover, domain.RollbackParkedNameSuffix) {
			retained = leftover
		}
	}
	if retained == "" {
		t.Error("the failed replacement was not retained; nothing is left to " +
			"inspect to find out why the update did not work")
	}

	// ---- exactly one of everything ----
	if got := rig.count("executions"); got != 1 {
		t.Errorf("executions = %d, want 1", got)
	}
	if got := rig.count("rollbacks"); got != 1 {
		t.Errorf("rollbacks = %d, want 1", got)
	}
	byHarborMaster := dockerEvents(t, sinceDecision)
	if got := countEvents(byHarborMaster, "create"); got != 1 {
		t.Errorf("the daemon created %d containers for HarborMaster, want 1\n\t%s",
			got, strings.Join(byHarborMaster, "\n\t"))
	}
	// The rollback restored the ORIGINAL rather than creating a third
	// container: a recovery that made a new one would be a different container
	// wearing the workload's name.
	if got := countEvents(byHarborMaster, "start"); got < 2 {
		t.Errorf("the daemon reported %d starts, want at least 2 (the failed "+
			"replacement and the restored original)\n\t%s",
			got, strings.Join(byHarborMaster, "\n\t"))
	}

	// ---- the operator is told the truth ----
	rig.await("the recovered notification", func() bool {
		return len(notificationsFor2(rig, domain.EventUpdateRecovered)) > 0
	})
	assertOneLogical(t, rig, domain.EventExecutionFailed)
	assertOneLogical(t, rig, domain.EventUpdateRecovered)
	assertNone(t, rig, domain.EventExecutionSucceeded, domain.EventRollbackStarted,
		domain.EventRollbackSucceeded)

	recovered := notificationsFor2(rig, domain.EventUpdateRecovered)[0]
	body := strings.ToLower(recovered.Body)
	if !strings.Contains(body, "did not pass") && !strings.Contains(body, "fail") {
		t.Errorf("the recovered body never says the update failed: %s", recovered.Body)
	}
	if !strings.Contains(body, "roll") {
		t.Errorf("the recovered body never says it was put back: %s", recovered.Body)
	}

	// ---- the pause is recorded ----
	if pause, err := rig.db.Automation.PauseFor(context.Background(), name); err != nil {
		t.Errorf("no pause after an automatic rollback: %v", err)
	} else if pause.Reason != domain.PauseRolledBack {
		t.Errorf("pause reason = %q, want %q", pause.Reason, domain.PauseRolledBack)
	}

	// ---- and it is not retried ----
	for i := 0; i < 5; i++ {
		rig.decide()
	}
	rig.awaitNoChange("no retry of the failed attempt", func() bool {
		return rig.count("executions") == 1 && rig.count("rollbacks") == 1
	})
}

// ------------------------------------ PART 3: real review first --

func TestRealDockerReviewFirstWaitsThenContinuesOnApproval(t *testing.T) {
	const name = "hm-c4c1-review"

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.policies = []domain.UpdatePolicy{c4c1Policy(name, domain.ModeApprove)}
	})

	originalID := containerID(name)
	rig.seed(domain.UpdateMinor, domain.CheckOK)
	rig.start()

	var lastRunID string
	for i := 0; i < 5; i++ {
		run, _ := rig.decide()
		lastRunID = run.RunID
	}

	// Nothing happened to the host, and the daemon agrees.
	rig.awaitNoChange("review-first mutates nothing", func() bool {
		return rig.count("acquisitions") == 0 && rig.count("executions") == 0
	})

	before := dockerEvents(t, rig.startedAt)
	assertOnlyDisposableContainersTouched(t, before)
	for _, action := range []string{"create", "stop", "rename", "destroy", "kill"} {
		if got := countEvents(before, action); got != 0 {
			t.Errorf("the daemon reported %d %q events before approval\n\t%s",
				got, action, strings.Join(before, "\n\t"))
		}
	}
	if containerID(name) != originalID {
		t.Error("the container changed before approval")
	}
	assertOneLogical(t, rig, domain.EventApprovalRequired)

	// The ONE legitimate manual action in this file.
	if _, err := rig.automation.Approve(context.Background(), lastRunID, name,
		domain.Requester{UserID: "usr_0011223344556677889a", Username: "colby"},
		service.Actor{}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// From here the schedulers finish it alone.
	rig.await("the approved update to settle", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State.Terminal()
	})
	if got := rig.terminalExecution().State; got != domain.ExecutionSucceeded {
		t.Fatalf("the approved recreation ended %q", got)
	}

	after := dockerEvents(t, rig.startedAt)
	assertOnlyDisposableContainersTouched(t, after)
	if got := countEvents(after, "create"); got != 1 {
		t.Errorf("the daemon created %d containers after approval, want 1", got)
	}
	if got := runningImage(name); !strings.Contains(got, rig.nextDigest) {
		t.Errorf("the workload runs %q, want the approved digest", got)
	}
	rig.await("the success notification", func() bool {
		return len(notificationsFor2(rig, domain.EventExecutionSucceeded)) > 0
	})
}

// ----------------------------------- PART 4: real monitor only --

func TestRealDockerMonitorOnlyNeverMutates(t *testing.T) {
	const name = "hm-c4c1-monitor"

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.policies = []domain.UpdatePolicy{c4c1Policy(name, domain.ModeObserve)}
	})

	originalID := containerID(name)
	rig.seed(domain.UpdateMinor, domain.CheckOK)
	rig.start()

	for i := 0; i < 5; i++ {
		rig.decide()
	}
	// And a restart, because "it did nothing yet" is not "it will never".
	rig.restart()
	rig.refreshInventory()
	for i := 0; i < 3; i++ {
		rig.decide()
	}

	rig.awaitNoChange("monitor-only mutates nothing", func() bool {
		return rig.count("acquisitions") == 0 && rig.count("executions") == 0 &&
			rig.count("rollbacks") == 0
	})

	// The daemon's own account, which is the proof an empty table cannot give.
	events := dockerEvents(t, rig.startedAt)
	assertOnlyDisposableContainersTouched(t, events)
	for _, action := range []string{"create", "stop", "rename", "destroy", "kill"} {
		if got := countEvents(events, action); got != 0 {
			t.Errorf("the daemon reported %d %q events under monitor-only\n\t%s",
				got, action, strings.Join(events, "\n\t"))
		}
	}
	if containerID(name) != originalID || !isRunning(name) {
		t.Error("the workload changed under monitor-only")
	}

	// Reporting still happens: C4B's preserved decision.
	assertOneLogical(t, rig, domain.EventUpdateDiscovered)
	assertNone(t, rig, domain.EventAcquisitionSucceeded, domain.EventExecutionSucceeded,
		domain.EventExecutionFailed, domain.EventUpdateRecovered)
}

// ------------------------------------------------------------- helpers --

// disposableContainers lists every hm-c4c1 container except the registry.
func disposableContainers(t *testing.T) []string {
	t.Helper()

	raw := dockerRun(t, "ps", "-a", "--filter", "name=hm-c4c1-", "--format", "{{.Names}}")
	var out []string
	for _, name := range strings.Fields(raw) {
		if name != "hm-c4c1-registry" {
			out = append(out, name)
		}
	}
	return out
}

func notificationsFor2(rig *realRig, event domain.NotificationEvent) []domain.Notification {
	var out []domain.Notification
	for _, notification := range rig.notifier.all() {
		if notification.Event == event {
			out = append(out, notification)
		}
	}
	return out
}

// assertOneLogical requires exactly one LOGICAL notification: several raises
// sharing one dedup key are one message to an operator.
func assertOneLogical(t *testing.T, rig *realRig, event domain.NotificationEvent) {
	t.Helper()

	matches := notificationsFor2(rig, event)
	if len(matches) == 0 {
		t.Errorf("no %q notification\n\nraised: %v", event, eventsOf(rig.notifier.all()))
		return
	}
	keys := map[string]struct{}{}
	for _, notification := range matches {
		keys[notification.DedupKey] = struct{}{}
	}
	if len(keys) != 1 {
		t.Errorf("%q was raised with %d distinct dedup keys, want 1: %v",
			event, len(keys), keys)
	}
}

func assertNone(t *testing.T, rig *realRig, events ...domain.NotificationEvent) {
	t.Helper()

	raised := map[domain.NotificationEvent]bool{}
	for _, notification := range rig.notifier.all() {
		raised[notification.Event] = true
	}
	for _, event := range events {
		if raised[event] {
			t.Errorf("%q was raised and must not have been\n\nraised: %v",
				event, eventsOf(rig.notifier.all()))
		}
	}
}
