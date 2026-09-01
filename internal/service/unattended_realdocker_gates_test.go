package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The gates and the boundaries, against a real daemon.
//
// Each of these starts from a rig that Parts 1 and 2 prove DOES update, and
// changes exactly one thing. If the gate under test stopped working the update
// would go through and the daemon would say so, which is what makes a
// "nothing happened" assertion worth making.

// assertRealInert requires that HarborMaster changed nothing on the host.
func assertRealInert(t *testing.T, rig *realRig, name, why string) {
	t.Helper()

	rig.awaitNoChange(why, func() bool {
		return rig.count("acquisitions") == 0 && rig.count("executions") == 0 &&
			rig.count("rollbacks") == 0
	})

	events := dockerEvents(t, rig.startedAt)
	assertOnlyDisposableContainersTouched(t, events)
	for _, action := range []string{"create", "stop", "rename", "destroy", "kill"} {
		if got := countEvents(events, action); got != 0 {
			t.Errorf("%s: the daemon reported %d %q events\n\t%s",
				why, got, action, strings.Join(events, "\n\t"))
		}
	}
	if !isRunning(name) {
		t.Errorf("%s: the workload is no longer running", why)
	}
}

// ------------------------------------------- PART 5: a retired plan --

func TestRealDockerARetiredPlanNeverActs(t *testing.T) {
	const name = "hm-c4c1-retired"

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.policies = []domain.UpdatePolicy{c4c1Policy(name, domain.ModeAutomatic)}
	})

	originalID := containerID(name)
	rig.seed(domain.UpdateMinor, domain.CheckOK)

	plan, err := rig.db.Plans.Current(context.Background(), originalID)
	if err != nil {
		t.Fatalf("no current plan to retire: %v", err)
	}

	// The registry settles: there is NO update after all. Recorded through the
	// repository's own path, so the plan is retired by the same settled
	// evidence C3B/C3C defined -- not by editing a row.
	rig.publishUpdate(domain.UpdateNone, domain.CheckOK)

	if _, err := rig.db.Plans.Current(context.Background(), originalID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the plan is still current after settled no-update evidence: %v", err)
	}

	rig.start()
	for i := 0; i < 5; i++ {
		rig.decide()
	}
	assertRealInert(t, rig, name, "a retired plan must never be acted on")

	// A LATE acquisition against the retired plan, through the legitimate
	// service interface. This is the one negative test that submits a request:
	// it exists to prove the refusal, and the refusal is the assertion.
	_, err = rig.acquisitions.Request(context.Background(), service.AcquisitionRequest{
		PlanID:      plan.PlanID,
		RequestedBy: domain.Requester{UserID: "usr_0011223344556677889a", Username: "colby"},
	})
	if err == nil {
		t.Fatal("an acquisition against a RETIRED plan was accepted.\n\n" +
			"The plan says to move to an image the registry has since said is " +
			"not an update. Acting on it changes a container for no reason.")
	}
	var refused service.ErrAcquisitionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("the refusal is %v, want a typed acquisition refusal", err)
	}
	t.Logf("late acquisition refused: %s", refused.Refusal.Explain())

	// The history is still readable: a retired plan is evidence, not rubbish.
	if _, err := rig.db.Plans.Get(context.Background(), plan.PlanID); err != nil {
		t.Errorf("the retired plan is no longer readable: %v", err)
	}
}

// --------------------------------------- PART 6: a superseded plan --

func TestRealDockerASupersededPlanNeverActs(t *testing.T) {
	const name = "hm-c4c1-superseded"

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.policies = []domain.UpdatePolicy{c4c1Policy(name, domain.ModeAutomatic)}
	})

	originalID := containerID(name)

	// P1 proposes a DIFFERENT target from the one the second pass will find.
	rig.refreshInventory()
	rig.syncIntel()
	rig.publishUpdateTo(domain.UpdateMinor, domain.CheckOK, "3.21", supersededDigest(t))
	rig.evaluateCompliance()
	rig.plan()

	first, err := rig.db.Plans.Current(context.Background(), originalID)
	if err != nil {
		t.Fatalf("no first plan: %v", err)
	}

	// Newer evidence produces P2 for the same container.
	rig.publishUpdate(domain.UpdateMinor, domain.CheckOK)
	rig.plan()

	second, err := rig.db.Plans.Current(context.Background(), originalID)
	if err != nil {
		t.Fatalf("no second plan: %v", err)
	}
	if second.PlanID == first.PlanID {
		t.Fatalf("the newer evidence did not produce a new plan; both are %q",
			first.PlanID)
	}

	// A late acquisition against P1 is refused: it is no longer current.
	_, err = rig.acquisitions.Request(context.Background(), service.AcquisitionRequest{
		PlanID:      first.PlanID,
		RequestedBy: domain.Requester{UserID: "usr_0011223344556677889a", Username: "colby"},
	})
	if err == nil {
		t.Fatal("an acquisition against a SUPERSEDED plan was accepted.\n\n" +
			"P1 proposes a target a newer assessment has replaced. Acting on it " +
			"moves the container somewhere nobody currently recommends.")
	}
	t.Logf("late acquisition against P1 refused: %v", err)

	// P1 is still readable as history.
	if _, err := rig.db.Plans.Get(context.Background(), first.PlanID); err != nil {
		t.Errorf("the superseded plan is no longer readable: %v", err)
	}

	// And whatever the schedulers do, no Docker operation is attributable to P1.
	rig.start()
	rig.decide()
	rig.await("the current plan to be acted on", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State.Terminal()
	})
	execution := rig.terminalExecution()
	if execution.PlanID == first.PlanID {
		t.Errorf("an execution was attributed to the superseded plan %q", first.PlanID)
	}
	if execution.PlanID != second.PlanID {
		t.Errorf("the execution names plan %q, want the current %q",
			execution.PlanID, second.PlanID)
	}
}

// ------------------------------------ PART 8: the maintenance window --

func TestRealDockerAClosedMaintenanceWindowBlocksThenResumes(t *testing.T) {
	const name = "hm-c4c1-window"

	// A window that is CLOSED at the rig's current clock and opens later. The
	// clock moves; wall time never waits.
	closed := time.Now().UTC().Add(4 * time.Hour)
	policy := c4c1Policy(name, domain.ModeAutomatic)
	policy.Window = domain.MaintenanceWindow{
		Start: closed.Format("15:04"),
		End:   closed.Add(2 * time.Hour).Format("15:04"),
	}
	policy.Normalise()

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.policies = []domain.UpdatePolicy{policy}
	})

	rig.seed(domain.UpdateMinor, domain.CheckOK)
	rig.start()

	for i := 0; i < 3; i++ {
		run, decisions := rig.decide()
		if run.Submitted != 0 {
			t.Fatalf("a pass outside the window submitted %d\n\n  %s",
				run.Submitted, rig.mine(decisions))
		}
	}
	assertRealInert(t, rig, name, "an update outside its maintenance window must wait")

	// The clock moves into the window. Nothing else changes, and nothing
	// submits work by hand.
	rig.advance(4*time.Hour + 30*time.Minute)

	// Four and a half hours later the inventory and the compliance evaluation
	// have been refreshed, because their own schedulers ran. Re-running them
	// here is modelling that, not working around a gate: without it the
	// execution preflight correctly refuses evidence that is now hours old,
	// which is the freshness rule doing its job rather than the window failing.
	rig.refreshInventory()
	rig.evaluateCompliance()
	rig.plan()

	rig.decide()
	rig.await("the update to run once the window opened", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State.Terminal()
	})
	if got := rig.terminalExecution().State; got != domain.ExecutionSucceeded {
		t.Errorf("inside the window the recreation ended %q", got)
	}
	if got := runningImage(name); !strings.Contains(got, rig.nextDigest) {
		t.Errorf("the workload runs %q, want the approved digest", got)
	}
}

// ----------------------------- PART 9: restart against a real daemon --

func TestRealDockerARestartBetweenAcquisitionAndExecutionRunsOnce(t *testing.T) {
	const name = "hm-c4c1-restart-a"

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.policies = []domain.UpdatePolicy{c4c1Policy(name, domain.ModeAutomatic)}
	})

	rig.seed(domain.UpdateMinor, domain.CheckOK)
	sinceDecision := time.Now().UTC()
	rig.decide()

	// Only the acquisition worker runs, so the process stops with an image
	// pulled and nothing recreated.
	rig.startAcquisitionsOnly()
	rig.await("the image to be acquired", func() bool {
		acquisitions, _, err := rig.db.Acquisitions.List(context.Background(),
			store.AcquisitionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(acquisitions) == 1 && acquisitions[0].State.Terminal()
	})
	if got := countEvents(dockerEvents(t, sinceDecision), "create"); got != 0 {
		t.Fatalf("a container was recreated before the restart: %d", got)
	}

	rig.restart()

	rig.await("the recreation to settle after a restart", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State.Terminal()
	})
	if got := rig.terminalExecution().State; got != domain.ExecutionSucceeded {
		t.Fatalf("the recreation ended %q", got)
	}

	events := dockerEvents(t, sinceDecision)
	assertOnlyDisposableContainersTouched(t, events)
	if got := countEvents(events, "create"); got != 1 {
		t.Errorf("the daemon created %d containers across the restart, want 1\n\t%s",
			got, strings.Join(events, "\n\t"))
	}
	if got := rig.count("acquisitions"); got != 1 {
		t.Errorf("acquisitions = %d, want 1", got)
	}
	if got := rig.count("executions"); got != 1 {
		t.Errorf("executions = %d, want 1", got)
	}
}

func TestRealDockerARestartBetweenFailureAndRollbackRecoversOnce(t *testing.T) {
	const name = "hm-c4c1-restart-b"

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.healthCheck = c4c1VersionCheck
		o.policies = []domain.UpdatePolicy{c4c1Policy(name, domain.ModeAutomatic)}
	})

	originalID := containerID(name)
	rig.seed(domain.UpdateMinor, domain.CheckOK)
	sinceDecision := time.Now().UTC()
	rig.decide()

	// The rollback worker does not exist in this process, so it stops with a
	// failed update and a container that is down.
	rig.startWithoutRollback()
	rig.await("the recreation to fail", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 && executions[0].State.Terminal()
	})
	if got := rig.terminalExecution().State; got != domain.ExecutionFailed {
		t.Fatalf("the recreation ended %q, want failed", got)
	}
	if got := rig.count("rollbacks"); got != 0 {
		t.Fatalf("a rollback happened before the restart: %d", got)
	}

	// The process comes back with the rollback capability. No new decision
	// pass: the recovery has to come from the durable record.
	rig.restart()

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
		t.Fatalf("the rollback ended %q", rollbacks[0].State)
	}

	if got := rig.count("rollbacks"); got != 1 {
		t.Errorf("rollbacks = %d, want 1", got)
	}
	if got := countEvents(dockerEvents(t, sinceDecision), "create"); got != 1 {
		t.Errorf("the daemon created %d containers, want 1", got)
	}
	if containerID(name) != originalID || !isRunning(name) {
		t.Error("the original service was not restored after the restart")
	}
}

// -------------------------------- PART 10: C4A cleanup interaction --

func TestRealDockerCleanupKeepsTheImagesARecoveryNeeds(t *testing.T) {
	const name = "hm-c4c1-cleanup"

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.healthCheck = c4c1VersionCheck
		o.policies = []domain.UpdatePolicy{c4c1Policy(name, domain.ModeAutomatic)}
	})

	rig.seed(domain.UpdateMinor, domain.CheckOK)
	rig.decide()
	rig.start()

	rig.await("the recovery to settle", func() bool {
		rollbacks, _, err := rig.db.Rollbacks.List(context.Background(),
			store.RollbackFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(rollbacks) == 1 && rollbacks[0].State.Terminal()
	})

	// A real cleanup pass, with the real pruner. If it decided wrongly it would
	// really remove an image.
	pass := rig.cleanup.RunPass(context.Background())
	t.Logf("cleanup pass: considered=%d removed=%d retained=%d refused=%d failed=%d",
		pass.Considered, pass.Removed, pass.Retained, pass.Refused, pass.Failed)

	if pass.Removed != 0 {
		t.Errorf("cleanup removed %d images while a recovery was still the most "+
			"recent thing that happened.\n\n"+
			"The original image is what the restored container is running and "+
			"the failed target is the only evidence of why the update did not "+
			"work. Neither is cleanup's to take.", pass.Removed)
	}

	// Both images are still on the host, and the daemon says so.
	for _, reference := range []string{c4c1CurrentRef, c4c1NextRef} {
		if imageIDOf(t, reference) == "" {
			t.Errorf("%s is gone from the host after a cleanup pass", reference)
		}
	}

	// And cleanup said nothing to the operator.
	assertNone(t, rig, domain.EventExecutionSucceeded)
	for _, notification := range rig.notifier.all() {
		if strings.Contains(strings.ToLower(notification.Title), "image") &&
			strings.Contains(strings.ToLower(notification.Title), "remov") {
			t.Errorf("routine cleanup raised an operator notification: %q",
				notification.Title)
		}
	}
}

// TestRealDockerCleanupKeepsASupersededImageInsideItsWindow is the non-vacuous
// half.
//
// The recovery test above reports `considered=0`, and that is correct: a FAILED
// update leaves no settled candidate, because a candidate requires a succeeded
// execution that removed its parked original. Correct, but it means that test
// alone would pass on a cleanup pass that could not see anything at all.
//
// So this one runs a SUCCESSFUL update first. There is now a real candidate --
// the image the workload was moved off -- and cleanup must still keep it,
// because it is the only superseded generation and it is minutes rather than
// weeks old. Two independent reasons, both of which must hold.
func TestRealDockerCleanupKeepsASupersededImageInsideItsWindow(t *testing.T) {
	const name = "hm-c4c1-cleanup-keep"

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.policies = []domain.UpdatePolicy{c4c1Policy(name, domain.ModeAutomatic)}
	})

	rig.seed(domain.UpdateMinor, domain.CheckOK)
	rig.decide()
	rig.start()
	rig.await("the update to settle", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 &&
			executions[0].State == domain.ExecutionSucceeded
	})

	// The parked original is removed AFTER the success is durably recorded --
	// that ordering is C4A's safety property -- so `original_removed` lands a
	// moment after the record turns terminal. A candidate read at the instant
	// of settlement is legitimately too early, which is a real property of the
	// pipeline rather than a flake to paper over.
	rig.await("the execution to record that the original was removed", func() bool {
		var flag int
		if err := rig.db.SQL().QueryRowContext(context.Background(),
			`SELECT original_removed FROM executions LIMIT 1`).Scan(&flag); err != nil {
			return false
		}
		return flag == 1
	})

	// The candidate exists. Without this the pass below proves nothing.
	candidates, err := rig.db.ImageRetention.ImageCleanupCandidates(context.Background())
	if err != nil {
		t.Fatalf("read cleanup candidates: %v", err)
	}
	found := false
	for _, candidate := range candidates {
		if candidate.ContainerName == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("a settled successful update produced no cleanup candidate; "+
			"the pass below would be examining nothing (%d candidates)",
			len(candidates))
	}

	pass := rig.cleanup.RunPass(context.Background())
	t.Logf("cleanup pass after a success: considered=%d removed=%d retained=%d",
		pass.Considered, pass.Removed, pass.Retained)

	if pass.Considered == 0 {
		t.Fatal("the pass considered nothing despite a candidate existing")
	}
	if pass.Removed != 0 {
		t.Errorf("cleanup removed %d images minutes after the update that "+
			"superseded them.\n\n"+
			"Two rules each forbid it on their own: it is the only superseded "+
			"generation for this workload, and it is far inside the retention "+
			"age. An operator who wanted to go back would find nothing to go "+
			"back to.", pass.Removed)
	}
	if imageIDOf(t, c4c1CurrentRef) == "" {
		t.Error("the previous image is gone from the host")
	}
}

// ------------------------------------------------------------ helpers --

// supersededDigest is a real digest for a DIFFERENT alpine tag.
//
// Used only to make a first plan that a second, newer assessment replaces. It
// is a genuine registry digest, so the first plan is a well-formed plan rather
// than a malformed one that would be refused for the wrong reason.
func supersededDigest(t *testing.T) string {
	t.Helper()
	dockerRun(t, "pull", "-q", "alpine:3.21")
	return digestOf(t, "alpine:3.21")
}
