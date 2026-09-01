package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// "Turn it on and walk away", proved.
//
// These are the tests the release rests on. Every one of them drives the REAL
// services through their OWN schedulers and asserts on durable rows and on what
// actually happened to the modelled host. None of them calls Request on the
// acquisition, execution, or rollback service: if one did, it would be proving
// that a manual pipeline works, which was never in doubt.
//
// The one exception is Scenario C, where an operator's approval is the product
// contract and is therefore the thing being tested.

// seedDiscovery walks the read-only half of the pipeline: the inventory sees
// the container, the projection lists its reference, the registry answers, and
// the planner writes a plan.
//
// All four steps are the production code. Only the registry ANSWER is seeded,
// because the alternative is a real registry.
func seedDiscovery(t *testing.T, rig *unattendedRig, update domain.UpdateType) domain.ChangePlan {
	t.Helper()

	rig.refreshInventory()
	rig.syncIntelReferences()
	rig.publishUpdate(update, domain.CheckOK)
	// Compliance, before planning: the execution preflight refuses without a
	// recent evaluation, and the plan carries the compliance evidence.
	rig.evaluateCompliance()
	rig.plan()

	plan, err := rig.db.Plans.Current(context.Background(), c4cContainerID)
	if err != nil {
		t.Fatalf("no current plan after discovery: %v", err)
	}
	return plan
}

// ------------------------------------- Scenario A: full unattended success --

func TestScenarioAAnEligibleWorkloadUpdatesItselfEndToEnd(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
	})
	defer rig.stop()

	// 1-3. Discovery and planning, through the real inventory, projection and
	// planner.
	plan := seedDiscovery(t, rig, domain.UpdateMinor)
	if plan.ProposedDigest != c4cNextDigest {
		t.Fatalf("the plan proposes %q, want the digest the registry advertised",
			plan.ProposedDigest)
	}

	// 4. One decision pass -- the same call the scheduler's ticker makes.
	run, decisions := rig.decide()
	if run.Submitted != 1 {
		t.Fatalf("the pass submitted %d, want 1\n\ndecisions: %+v", run.Submitted, decisions)
	}

	// The world starts AFTER the submission. The scheduler loop closes out
	// runs a restart interrupted when it starts, and a pass still in flight
	// looks exactly like one -- so the pass finishes first. It is also the
	// stronger test: the follower has to find DURABLE work rather than work
	// it watched being created.
	rig.start()

	// 5-11. Everything from here is the schedulers' own work. The test only
	// waits and looks.
	rig.await("the recreation to settle", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State.Terminal()
	})

	execution := rig.terminalExecution()
	if execution.State != domain.ExecutionSucceeded {
		t.Fatalf("the recreation ended %q (failure %q, checkpoint %q)\n\nhost: %v",
			execution.State, execution.Failure, execution.Checkpoint, rig.host.operations())
	}

	// 6. Acquired by immutable digest, never by tag.
	pulls := rig.host.pulls()
	if len(pulls) != 1 || pulls[0] != c4cNextDigest {
		t.Fatalf("pulls = %v, want exactly the proposed digest", pulls)
	}

	// 8/10. The workload is running, under its own name, on the new image.
	live, present := rig.host.byName(c4cName)
	if !present {
		t.Fatalf("nothing answers to %q any more\n\nhost: %v", c4cName, rig.host.operations())
	}
	if !live.running {
		t.Error("the replacement is not running")
	}
	if live.id == c4cContainerID {
		t.Error("the container was never actually replaced")
	}
	if !strings.Contains(live.image, c4cNextDigest) {
		t.Errorf("the replacement runs %q, want the new digest", live.image)
	}

	// 11. The original is removed, and only after the success was recorded.
	original, known := rig.host.snapshotOf(c4cContainerID)
	if !known {
		t.Fatal("the original vanished from the modelled host entirely")
	}
	if !original.removed {
		t.Error("the original was left behind after a successful update")
	}
	ops := rig.host.operations()
	if position(ops, "remove:") < position(ops, "start:"+c4cName) {
		t.Errorf("the original was removed before the replacement started: %v", ops)
	}

	// 13/15. One attempt, no rollback.
	if got := rig.executionCount(); got != 1 {
		t.Errorf("executions = %d, want 1", got)
	}
	if got := rig.rollbackCount(); got != 0 {
		t.Errorf("rollbacks = %d, want 0: a successful update needs no undoing", got)
	}
	if got := rig.host.countOps("create:"); got != 1 {
		t.Errorf("the host performed %d creates, want exactly 1\n\n%v", got, ops)
	}

	// 14. Exactly one logical success notification, and nothing that reads as a
	// failure or a recovery.
	//
	// Awaited rather than asserted outright: the outcome report is detached
	// from the pipeline by design -- a notification must never be able to hold
	// up a recreation -- so it lands shortly AFTER the record is terminal.
	rig.await("the success notification", func() bool {
		return len(notificationsFor(rig, domain.EventExecutionSucceeded)) > 0
	})
	assertOneOf(t, rig, domain.EventExecutionSucceeded)
	assertNoneOf(t, rig, domain.EventExecutionFailed, domain.EventUpdateRecovered,
		domain.EventRollbackStarted, domain.EventRollbackSucceeded, domain.EventRollbackFailed)

	// 16. Later ticks do nothing.
	//
	// Discovery is re-run first, exactly as production does after a recreation:
	// the inventory refreshes, the projection re-reads it, and the planner
	// re-assesses. Skipping that would leave a plan proposing a digest the
	// container is already running, which is a stale world rather than an
	// unchanged one.
	rig.refreshInventory()
	rig.syncIntelReferences()
	rig.evaluateCompliance()
	rig.plan()

	creates := rig.host.countOps("create:")
	executions := rig.executionCount()
	for i := 0; i < 5; i++ {
		rig.decide()
	}
	rig.awaitNoChange("no second recreation", func() bool {
		return rig.host.countOps("create:") == creates &&
			rig.executionCount() == executions && rig.rollbackCount() == 0
	})
	if creates != 1 {
		t.Errorf("the host performed %d creates in total, want 1", creates)
	}
}

// ------------------------- Scenario B: automatic failure and auto-recovery --

func TestScenarioBAFailedAutomaticUpdateRecoversItself(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
	})
	defer rig.stop()

	// The new image is bad: a container created from it never becomes healthy.
	// The daemon telling the truth, not a check being weakened.
	rig.host.mu.Lock()
	rig.host.badImage = c4cNextDigest
	rig.host.mu.Unlock()

	seedDiscovery(t, rig, domain.UpdateMinor)
	rig.start()

	if run, decisions := rig.decide(); run.Submitted != 1 {
		t.Fatalf("submitted = %d, want 1\n\n%+v", run.Submitted, decisions)
	}

	// 1-9. Detection, acquisition, execution, failure, and rollback are all the
	// schedulers' own work.
	rig.await("the rollback to settle", func() bool {
		rollbacks, _, err := rig.db.Rollbacks.List(context.Background(),
			store.RollbackFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(rollbacks) == 1 && rollbacks[0].State.Terminal()
	})

	execution := rig.terminalExecution()
	if execution.State != domain.ExecutionFailed {
		t.Fatalf("the recreation ended %q, want failed\n\nhost: %v",
			execution.State, rig.host.operations())
	}

	rollbacks, _, err := rig.db.Rollbacks.List(context.Background(),
		store.RollbackFilter{Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatalf("list rollbacks: %v", err)
	}
	if rollbacks[0].State != domain.RollbackSucceeded {
		t.Fatalf("the rollback ended %q, want succeeded\n\nhost: %v",
			rollbacks[0].State, rig.host.operations())
	}

	// 7. The rollback was AUTOMATIC, read from the durable request key.
	if !rollbacks[0].Automatic() {
		t.Error("the rollback is not marked automatic, so every message about it " +
			"will describe it as something an operator asked for")
	}

	// 9. The original service is restored, running, under its own name.
	restored, present := rig.host.byName(c4cName)
	if !present {
		t.Fatalf("nothing answers to %q after the recovery\n\nhost: %v",
			c4cName, rig.host.operations())
	}
	if restored.id != c4cContainerID {
		t.Errorf("the container answering to %q is %q, want the original %q",
			c4cName, restored.id, c4cContainerID)
	}
	if !restored.running {
		t.Error("THE RESTORED CONTAINER IS NOT RUNNING.\n\n" +
			"An automatic rollback that leaves the service down is worse than " +
			"no rollback: the operator was told it recovered.")
	}
	if !strings.Contains(restored.image, c4cCurrentRef) {
		t.Errorf("the restored container runs %q, want the original image", restored.image)
	}

	// 6. The failed replacement is retained as evidence.
	if quarantined := findQuarantined(rig); quarantined == "" {
		t.Error("the failed replacement was not retained; there is nothing left " +
			"to inspect to find out why the update did not work")
	}

	// 13/14. Failure then recovered, and no rollback-start noise.
	events := eventsOf(rig.notifier.all())
	assertOneOf(t, rig, domain.EventExecutionFailed)
	assertOneOf(t, rig, domain.EventUpdateRecovered)
	assertNoneOf(t, rig, domain.EventRollbackStarted, domain.EventRollbackSucceeded)
	// 12. And nothing anywhere says this update succeeded.
	assertNoneOf(t, rig, domain.EventExecutionSucceeded)

	if eventPosition(events, domain.EventExecutionFailed) >
		eventPosition(events, domain.EventUpdateRecovered) {
		t.Errorf("the recovery was announced before the failure: %v", events)
	}

	// 14. The recovered message stands alone.
	recovered := notificationFor(rig, domain.EventUpdateRecovered)
	body := strings.ToLower(recovered.Body)
	// The three things an operator must read without opening anything: it did
	// not work, it was undone, and nothing will try again.
	if !strings.Contains(body, "did not pass") && !strings.Contains(body, "fail") {
		t.Errorf("the recovered body never says the update did not work: %s", recovered.Body)
	}
	if !strings.Contains(body, "roll") {
		t.Errorf("the recovered body never says the container was put back: %s", recovered.Body)
	}
	if !strings.Contains(body, "paused") {
		t.Errorf("the recovered body never says automation is paused: %s", recovered.Body)
	}

	// 15/16. One rollback, and repeated passes do not retry the failed attempt.
	if got := rig.rollbackCount(); got != 1 {
		t.Errorf("rollbacks = %d, want 1", got)
	}
	for i := 0; i < 5; i++ {
		rig.decide()
	}
	rig.awaitNoChange("no retry of the failed attempt", func() bool {
		return rig.executionCount() == 1 && rig.rollbackCount() == 1 &&
			rig.host.countOps("create:") == 1
	})

	// The pause is what stops the retry, and it must be recorded.
	if pause, err := rig.db.Automation.PauseFor(context.Background(), c4cName); err != nil {
		t.Errorf("no pause was recorded after an automatic rollback: %v", err)
	} else if pause.Reason != domain.PauseRolledBack {
		t.Errorf("pause reason = %q, want %q", pause.Reason, domain.PauseRolledBack)
	}
}

// ------------------------------------------------ Scenario C: review first --

func TestScenarioCReviewFirstWaitsThenContinuesOnApproval(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cPolicyWithMode(domain.ModeApprove)}
	})
	defer rig.stop()

	seedDiscovery(t, rig, domain.UpdateMinor)
	rig.start()

	// Repeated passes over an unchanged world.
	var lastRunID string
	var pending domain.AutomationDecision
	for i := 0; i < 5; i++ {
		run, decisions := rig.decide()
		lastRunID = run.RunID
		for _, decision := range decisions {
			if decision.Verdict == domain.VerdictAwaitingApproval {
				pending = decision
			}
		}
	}
	if pending.PlanID == "" {
		t.Fatal("no decision awaited approval")
	}

	// Zero mutation, held across many ticks.
	rig.awaitNoChange("review-first mutates nothing", func() bool {
		return rig.acquisitionCount() == 0 && rig.executionCount() == 0 &&
			len(rig.host.operations()) == 0
	})

	// One logical review notification, whatever the tick count.
	assertOneOf(t, rig, domain.EventApprovalRequired)

	// The approval -- the ONE manual call this file makes, because it is the
	// product contract being tested.
	if _, err := rig.automation.Approve(context.Background(), lastRunID, c4cName,
		domain.Requester{UserID: "usr_0011223344556677889a", Username: "colby"},
		service.Actor{}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// And from here the schedulers carry it the rest of the way. Nothing below
	// submits anything.
	rig.await("the approved update to settle", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State.Terminal()
	})

	execution := rig.terminalExecution()
	if execution.State != domain.ExecutionSucceeded {
		t.Fatalf("the approved recreation ended %q\n\nhost: %v",
			execution.State, rig.host.operations())
	}
	live, present := rig.host.byName(c4cName)
	if !present || !strings.Contains(live.image, c4cNextDigest) {
		t.Errorf("the workload is not running the approved image: %+v", live)
	}
	// Detached from the pipeline by design, so awaited rather than assumed.
	rig.await("the success notification", func() bool {
		return len(notificationsFor(rig, domain.EventExecutionSucceeded)) > 0
	})
	assertOneOf(t, rig, domain.EventExecutionSucceeded)

	// The review notification was raised on every pass and is ONE logical
	// message: the dedup key is the plan, so the engine's suppression collapses
	// them. assertOneOf checks the keys rather than the raise count, which is
	// the distinction that matters to an operator.
	if raises := len(notificationsFor(rig, domain.EventApprovalRequired)); raises < 2 {
		t.Errorf("only %d approval raises; the repeated-tick case is not being "+
			"exercised at all", raises)
	}
}

// ----------------------------------------------- Scenario D: monitor only --

func TestScenarioDMonitorOnlyNeverMutates(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cPolicyWithMode(domain.ModeObserve)}
	})
	defer rig.stop()

	seedDiscovery(t, rig, domain.UpdateMinor)
	rig.start()

	for i := 0; i < 5; i++ {
		rig.decide()
	}

	rig.awaitNoChange("monitor-only mutates nothing", func() bool {
		return rig.acquisitionCount() == 0 && rig.executionCount() == 0 &&
			rig.rollbackCount() == 0 && len(rig.host.operations()) == 0
	})

	// The container is untouched: same id, same name, same image, still running.
	live, present := rig.host.byName(c4cName)
	if !present || live.id != c4cContainerID || !live.running ||
		live.image != c4cCurrentRef {
		t.Errorf("the workload changed under monitor-only: %+v", live)
	}

	// C4B's preserved decision: discovery still REPORTS. The planner raises it,
	// so it works with automation in any mode.
	assertOneOf(t, rig, domain.EventUpdateDiscovered)
	assertNoneOf(t, rig, domain.EventAcquisitionSucceeded, domain.EventExecutionSucceeded,
		domain.EventExecutionFailed, domain.EventUpdateRecovered)
}

// --------------------------------------------------- helpers and asserts --

// position returns the index of the first operation carrying a prefix, or a
// large number when it never happened.
func position(items []string, prefix string) int {
	for i, item := range items {
		if strings.HasPrefix(item, prefix) {
			return i
		}
	}
	return 1 << 30
}

// eventPosition returns where an event first appears in a raised sequence.
func eventPosition(events []domain.NotificationEvent, want domain.NotificationEvent) int {
	for i, event := range events {
		if event == want {
			return i
		}
	}
	return 1 << 30
}

// notificationsFor returns every notification raised for one event.
func notificationsFor(rig *unattendedRig, event domain.NotificationEvent) []domain.Notification {
	var out []domain.Notification
	for _, notification := range rig.notifier.all() {
		if notification.Event == event {
			out = append(out, notification)
		}
	}
	return out
}

func notificationFor(rig *unattendedRig, event domain.NotificationEvent) domain.Notification {
	matches := notificationsFor(rig, event)
	if len(matches) == 0 {
		return domain.Notification{}
	}
	return matches[0]
}

// assertOneOf requires exactly one LOGICAL notification for an event.
//
// Logical rather than literal: two raises carrying the same dedup key are one
// message to an operator, because the engine's suppression collapses them. What
// must never happen is two DIFFERENT keys for one lifecycle outcome.
func assertOneOf(t *testing.T, rig *unattendedRig, event domain.NotificationEvent) {
	t.Helper()

	matches := notificationsFor(rig, event)
	if len(matches) == 0 {
		t.Errorf("no %q notification was raised\n\nraised: %v",
			event, eventsOf(rig.notifier.all()))
		return
	}

	keys := map[string]struct{}{}
	for _, notification := range matches {
		keys[notification.DedupKey] = struct{}{}
	}
	if len(keys) != 1 {
		t.Errorf("%q was raised with %d distinct dedup keys, want 1: %v\n\n"+
			"Distinct keys are distinct messages. An operator would be told the "+
			"same thing several times and would reasonably read it as several "+
			"separate incidents.", event, len(keys), keys)
	}
}

// assertNoneOf requires that none of these events was raised at all.
func assertNoneOf(t *testing.T, rig *unattendedRig, events ...domain.NotificationEvent) {
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

// findQuarantined returns the name of a retained failed replacement, if any.
//
// Either marker counts. A failed recreation parks its replacement under
// `.hm-failed-`; a rollback that then runs renames it again to
// `.hm-rolledback-`. Both are the same artefact being kept for the same reason,
// and which name it ends up under depends only on whether a rollback followed.
func findQuarantined(rig *unattendedRig) string {
	for name := range rig.host.live() {
		if strings.Contains(name, domain.QuarantineNameSuffix) ||
			strings.Contains(name, domain.RollbackParkedNameSuffix) {
			return name
		}
	}
	return ""
}
