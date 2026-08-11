package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Automation persistence tests.
//
// The properties that matter here are the SAFETY ones. A second pass must not
// start while one is running. A pause must survive the recreation that changes
// the container's id. A resumed container must start from zero. A decision must
// not be able to claim it acted when its mode could not have. And withdrawing a
// policy must never destroy the record of what it did.

func newUpdatePolicy(name string) domain.UpdatePolicy {
	policy := domain.UpdatePolicy{
		PolicyID:              domain.NewUpdatePolicyID(),
		Name:                  name,
		Enabled:               true,
		Priority:              10,
		Selector:              domain.UpdateSelector{Include: []string{"web"}},
		Strategy:              domain.StrategyPatch,
		MinimumRecommendation: domain.RecommendProceed,
		Mode:                  domain.ModeObserve,
		Window:                domain.MaintenanceWindow{Timezone: "UTC", Start: "02:00", End: "04:00"},
	}
	policy.Normalise()
	return policy
}

func createUpdatePolicy(t *testing.T, db *store.DB, policy domain.UpdatePolicy) domain.UpdatePolicy {
	t.Helper()
	created, err := db.UpdatePolicies.CreateUpdatePolicy(context.Background(), policy, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateUpdatePolicy: %v", err)
	}
	return created
}

func startRun(t *testing.T, db *store.DB, trigger domain.AutomationTrigger) domain.AutomationRun {
	t.Helper()
	run, err := db.Automation.StartRun(context.Background(), domain.AutomationRun{
		RunID:     domain.NewAutomationRunID(),
		Trigger:   trigger,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	return run
}

// ------------------------------------------------------------- policies --

func TestUpdatePolicyRoundTripsEverySubStructure(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := newUpdatePolicy("Nightly patches")
	policy.Description = "Patch the web tier during the maintenance window"
	policy.Selector = domain.UpdateSelector{
		Labels:  map[string]string{"tier": "front"},
		Images:  []string{"ghcr.io/acme/*"},
		Include: []string{"web", "api"},
		Exclude: []string{"database"},
	}
	policy.Window = domain.MaintenanceWindow{
		Timezone: "Europe/London",
		Weekdays: []int{0, 6},
		Start:    "22:00",
		End:      "02:00",
	}
	policy.Limits = domain.UpdateLimits{
		MaxConcurrent: 3, MaxPerRegistry: 2, MaxPerRun: 20,
		AcquisitionTimeoutSeconds: 900, RecreateTimeoutSeconds: 420, HealthTimeoutSeconds: 90,
	}
	policy.Failure = domain.UpdateFailureHandling{
		AutoRollback: true, PauseAfterFailures: 3, PauseWindowHours: 12,
		CooldownHours: 6, MaxRetries: 2,
	}

	created := createUpdatePolicy(t, db, policy)
	loaded, err := db.UpdatePolicies.UpdatePolicyByID(ctx, created.PolicyID)
	if err != nil {
		t.Fatalf("UpdatePolicyByID: %v", err)
	}

	if loaded.Selector.Labels["tier"] != "front" ||
		len(loaded.Selector.Images) != 1 ||
		len(loaded.Selector.Include) != 2 ||
		len(loaded.Selector.Exclude) != 1 {
		t.Fatalf("selector did not survive the round trip: %+v", loaded.Selector)
	}
	if loaded.Window.Timezone != "Europe/London" || len(loaded.Window.Weekdays) != 2 ||
		!loaded.Window.CrossesMidnight() {
		t.Fatalf("window did not survive the round trip: %+v", loaded.Window)
	}
	if loaded.Limits != policy.Limits {
		t.Fatalf("limits = %+v, want %+v", loaded.Limits, policy.Limits)
	}
	if loaded.Failure != policy.Failure {
		t.Fatalf("failure handling = %+v, want %+v", loaded.Failure, policy.Failure)
	}
}

func TestUpdatePolicyNameIsUniqueAmongLivePolicies(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first := createUpdatePolicy(t, db, newUpdatePolicy("Nightly patches"))

	_, err := db.UpdatePolicies.CreateUpdatePolicy(ctx, newUpdatePolicy("Nightly patches"), time.Now().UTC())
	if !errors.Is(err, store.ErrUpdatePolicyNameTaken) {
		t.Fatalf("want ErrUpdatePolicyNameTaken, got %v", err)
	}

	// Archiving frees the name: the withdrawn rule is history and no longer
	// competes for the label an operator reads on a dashboard.
	if err := db.UpdatePolicies.ArchiveUpdatePolicy(ctx, first.PolicyID, time.Now().UTC()); err != nil {
		t.Fatalf("ArchiveUpdatePolicy: %v", err)
	}
	if _, err := db.UpdatePolicies.CreateUpdatePolicy(ctx, newUpdatePolicy("Nightly patches"), time.Now().UTC()); err != nil {
		t.Fatalf("the name must be reusable once the first policy is archived: %v", err)
	}
}

func TestArchivedUpdatePolicyLeavesEvaluationAndCannotBeEdited(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createUpdatePolicy(t, db, newUpdatePolicy("Nightly patches"))
	if err := db.UpdatePolicies.ArchiveUpdatePolicy(ctx, policy.PolicyID, time.Now().UTC()); err != nil {
		t.Fatalf("ArchiveUpdatePolicy: %v", err)
	}

	active, err := db.UpdatePolicies.ActivePolicies(ctx)
	if err != nil {
		t.Fatalf("ActivePolicies: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("an archived policy must not be loaded for evaluation, got %d", len(active))
	}

	name := "Renamed"
	_, err = db.UpdatePolicies.ApplyUpdatePolicy(ctx, policy.PolicyID,
		store.UpdatePolicyChange{Name: &name}, time.Now().UTC())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("editing an archived policy must be refused, got %v", err)
	}

	// Archiving twice is not an error the second time round -- it is a no-op
	// that reports nothing was there to archive.
	if err := db.UpdatePolicies.ArchiveUpdatePolicy(ctx, policy.PolicyID, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("re-archiving must report not found, got %v", err)
	}
}

func TestUpdatePolicyEditLeavesUnsuppliedFieldsAlone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := newUpdatePolicy("Nightly patches")
	policy.Mode = domain.ModeAutomatic
	policy.Priority = 42
	created := createUpdatePolicy(t, db, policy)

	// Only the description is supplied. Everything else must survive, and in
	// particular `enabled` must not be silently turned off by its absence.
	description := "Now with a reason"
	updated, err := db.UpdatePolicies.ApplyUpdatePolicy(ctx, created.PolicyID,
		store.UpdatePolicyChange{Description: &description}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ApplyUpdatePolicy: %v", err)
	}
	if !updated.Enabled || updated.Priority != 42 || updated.Mode != domain.ModeAutomatic {
		t.Fatalf("an unsupplied field was changed: %+v", updated)
	}
	if updated.Description != description {
		t.Fatalf("description = %q, want %q", updated.Description, description)
	}
}

func TestActivePoliciesIsOrderedForSelection(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	low := newUpdatePolicy("Low")
	low.Priority = 1
	high := newUpdatePolicy("High")
	high.Priority = 100
	disabled := newUpdatePolicy("Disabled")
	disabled.Priority = 500
	disabled.Enabled = false

	createUpdatePolicy(t, db, low)
	createUpdatePolicy(t, db, high)
	createUpdatePolicy(t, db, disabled)

	active, err := db.UpdatePolicies.ActivePolicies(ctx)
	if err != nil {
		t.Fatalf("ActivePolicies: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("want two active policies, got %d", len(active))
	}
	if active[0].Name != "High" {
		t.Fatalf("highest priority must come first, got %q", active[0].Name)
	}
}

func TestUpdatePolicySchemaRefusesAnUnautomatableRecommendation(t *testing.T) {
	// The schema is the backstop for the validation in internal/domain: a code
	// path that skipped Validate must still be unable to store a policy that
	// automates a verdict asking for human review.
	db := openTestDB(t)

	policy := newUpdatePolicy("Reckless")
	policy.MinimumRecommendation = domain.RecommendManualReview

	if _, err := db.UpdatePolicies.CreateUpdatePolicy(context.Background(), policy, time.Now().UTC()); err == nil {
		t.Fatal("the schema must refuse a policy that automates manualReview")
	}
}

// ------------------------------------------------------------------ runs --

func TestOnlyOnePassMayRunAtATime(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first := startRun(t, db, domain.AutoTriggerSchedule)

	_, err := db.Automation.StartRun(ctx, domain.AutomationRun{
		RunID:     domain.NewAutomationRunID(),
		Trigger:   domain.AutoTriggerManual,
		StartedAt: time.Now().UTC(),
	})
	if !errors.Is(err, store.ErrAutomationRunActive) {
		t.Fatalf("want ErrAutomationRunActive, got %v", err)
	}

	if err := db.Automation.FinishRun(ctx, first.RunID, domain.RunCompleted,
		domain.AutomationRun{Considered: 3}, time.Now().UTC()); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	// With the first pass finished, a second may start.
	startRun(t, db, domain.AutoTriggerManual)
}

func TestInterruptedRunDoesNotBlockTheNextPassForever(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	startRun(t, db, domain.AutoTriggerSchedule)

	count, err := db.Automation.InterruptRuns(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("InterruptRuns: %v", err)
	}
	if count != 1 {
		t.Fatalf("interrupted %d runs, want 1", count)
	}

	// The recovery sweep freed the single-run slot.
	startRun(t, db, domain.AutoTriggerStartup)

	runs, _, err := db.Automation.ListRuns(ctx, store.AutomationRunFilter{
		States: []domain.AutomationRunState{domain.RunInterrupted},
	})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Message == "" {
		t.Fatalf("the interrupted run must say why: %+v", runs)
	}
}

func TestFinishRunCannotOverwriteAnInterruptedRun(t *testing.T) {
	// A goroutine that survived the recovery sweep must not be able to report
	// success for a pass the sweep already recorded as cut short.
	db := openTestDB(t)
	ctx := context.Background()

	run := startRun(t, db, domain.AutoTriggerSchedule)
	if _, err := db.Automation.InterruptRuns(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("InterruptRuns: %v", err)
	}

	err := db.Automation.FinishRun(ctx, run.RunID, domain.RunCompleted,
		domain.AutomationRun{}, time.Now().UTC())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	reloaded, err := db.Automation.RunByID(ctx, run.RunID)
	if err != nil {
		t.Fatalf("RunByID: %v", err)
	}
	if reloaded.State != domain.RunInterrupted {
		t.Fatalf("state = %q, want interrupted", reloaded.State)
	}
}

func TestRunRoundTripsItsRequesterAndCounters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	started := time.Now().UTC()
	run, err := db.Automation.StartRun(ctx, domain.AutomationRun{
		RunID:       domain.NewAutomationRunID(),
		Trigger:     domain.AutoTriggerManual,
		StartedAt:   started,
		RequestedBy: domain.Requester{UserID: "usr_1", Username: "colby"},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	finished := started.Add(90 * time.Second)
	if err := db.Automation.FinishRun(ctx, run.RunID, domain.RunCompleted, domain.AutomationRun{
		Considered: 12, Eligible: 4, Submitted: 3, Skipped: 8, Failed: 1,
		Message: "one container was already in flight", DurationMs: 90000,
	}, finished); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	loaded, err := db.Automation.RunByID(ctx, run.RunID)
	if err != nil {
		t.Fatalf("RunByID: %v", err)
	}
	if loaded.RequestedBy.Username != "colby" || loaded.RequestedBy.UserID != "usr_1" {
		t.Fatalf("requester did not survive: %+v", loaded.RequestedBy)
	}
	if loaded.Considered != 12 || loaded.Submitted != 3 || loaded.Failed != 1 {
		t.Fatalf("counters did not survive: %+v", loaded)
	}
	if loaded.CompletedAt == nil || !loaded.CompletedAt.Equal(finished) {
		t.Fatalf("completedAt = %v, want %v", loaded.CompletedAt, finished)
	}
	if loaded.DurationMs != 90000 {
		t.Fatalf("duration = %d, want 90000", loaded.DurationMs)
	}
}

// ------------------------------------------------------------- decisions --

func decisionFor(run domain.AutomationRun, name string, verdict domain.AutomationVerdict) domain.AutomationDecision {
	return domain.AutomationDecision{
		RunID:         run.RunID,
		ContainerID:   "container-" + name,
		ContainerName: name,
		Verdict:       verdict,
		Reason:        domain.ReasonEligible,
		DecidedAt:     time.Now().UTC(),
	}
}

func TestDecisionsReadBackInDecisionOrder(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	run := startRun(t, db, domain.AutoTriggerDryRun)

	decisions := make([]domain.AutomationDecision, 0, 5)
	for i, name := range []string{"eee", "ddd", "ccc", "bbb", "aaa"} {
		decision := decisionFor(run, name, domain.VerdictWouldUpdate)
		decision.Position = i
		decisions = append(decisions, decision)
	}
	written, err := db.Automation.RecordDecisions(ctx, decisions)
	if err != nil {
		t.Fatalf("RecordDecisions: %v", err)
	}
	if written != 5 {
		t.Fatalf("wrote %d decisions, want 5", written)
	}

	loaded, total, err := db.Automation.ListDecisions(ctx,
		store.AutomationDecisionFilter{RunID: run.RunID})
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if total != 5 {
		t.Fatalf("total = %d, want 5", total)
	}
	// The pass's own sequence, not alphabetical and not insertion-id order by
	// accident: a dry run's whole value is "in what order would this happen".
	for i, want := range []string{"eee", "ddd", "ccc", "bbb", "aaa"} {
		if loaded[i].ContainerName != want {
			t.Fatalf("position %d = %q, want %q", i, loaded[i].ContainerName, want)
		}
	}
}

func TestSchemaRefusesADecisionThatClaimsToHaveActedInDryRun(t *testing.T) {
	// The engine checks the mode. This is the backstop: a future code path that
	// forgot would be refused by the database rather than silently recorded as
	// a dry run that changed the host.
	db := openTestDB(t)
	run := startRun(t, db, domain.AutoTriggerDryRun)

	decision := decisionFor(run, "web", domain.VerdictWouldUpdate)
	decision.AcquisitionID = "acq_0123456789abcdef0123"

	if _, err := db.Automation.RecordDecisions(context.Background(),
		[]domain.AutomationDecision{decision}); err == nil {
		t.Fatal("a wouldUpdate decision must not be able to name an acquisition")
	}
}

func TestSchemaRefusesAnExecutionWithoutAnAcquisition(t *testing.T) {
	// The pipeline's ordering, made unrepresentable in reverse.
	db := openTestDB(t)
	run := startRun(t, db, domain.AutoTriggerSchedule)

	decision := decisionFor(run, "web", domain.VerdictUpdate)
	decision.ExecutionID = "exe_0123456789abcdef0123"

	if _, err := db.Automation.RecordDecisions(context.Background(),
		[]domain.AutomationDecision{decision}); err == nil {
		t.Fatal("an execution cannot exist without the acquisition it consumed")
	}
}

func TestAttachExecutionOnlyLinksWorkTheDecisionStarted(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	run := startRun(t, db, domain.AutoTriggerSchedule)
	decision := decisionFor(run, "web", domain.VerdictUpdate)
	decision.AcquisitionID = "acq_0123456789abcdef0123"
	if _, err := db.Automation.RecordDecisions(ctx, []domain.AutomationDecision{decision}); err != nil {
		t.Fatalf("RecordDecisions: %v", err)
	}

	// A different acquisition id matches nothing and changes nothing.
	if err := db.Automation.AttachExecution(ctx, run.RunID,
		"acq_ffffffffffffffffffff", "exe_0123456789abcdef0123"); err != nil {
		t.Fatalf("AttachExecution: %v", err)
	}
	loaded, _, err := db.Automation.ListDecisions(ctx, store.AutomationDecisionFilter{RunID: run.RunID})
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if loaded[0].ExecutionID != "" {
		t.Fatalf("a decision was linked to work it did not start: %q", loaded[0].ExecutionID)
	}

	if err := db.Automation.AttachExecution(ctx, run.RunID,
		decision.AcquisitionID, "exe_0123456789abcdef0123"); err != nil {
		t.Fatalf("AttachExecution: %v", err)
	}
	loaded, _, err = db.Automation.ListDecisions(ctx, store.AutomationDecisionFilter{RunID: run.RunID})
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if loaded[0].ExecutionID != "exe_0123456789abcdef0123" {
		t.Fatalf("executionId = %q, want the attached one", loaded[0].ExecutionID)
	}
}

func TestPendingDecisionsIsTheFollowersWholeQuestion(t *testing.T) {
	// The follower asks "what did I start that has not finished". That is a
	// property of the DECISIONS, not of which passes are recent: an approval is
	// promoted after its pass has already finished, and a slow update can
	// outlast several scheduler ticks. An earlier shape walked recent runs and
	// abandoned both.
	db := openTestDB(t)
	ctx := context.Background()

	run := startRun(t, db, domain.AutoTriggerSchedule)

	outstanding := decisionFor(run, "web", domain.VerdictUpdate)
	outstanding.AcquisitionID = "acq_0123456789abcdef0123"
	outstanding.Position = 0

	finished := decisionFor(run, "api", domain.VerdictUpdate)
	finished.AcquisitionID = "acq_1123456789abcdef0123"
	finished.ExecutionID = "exe_0123456789abcdef0123"
	finished.RollbackID = "rbk_0123456789abcdef0123"
	finished.Position = 1

	neverActed := decisionFor(run, "cache", domain.VerdictSkip)
	neverActed.Reason = domain.ReasonWindowClosed
	neverActed.Position = 2

	if _, err := db.Automation.RecordDecisions(ctx, []domain.AutomationDecision{
		outstanding, finished, neverActed,
	}); err != nil {
		t.Fatalf("RecordDecisions: %v", err)
	}
	// "Finished" means SETTLED, not "has a rollback id". The two came apart in
	// Phase 11: a successful update never gets a rollback id at all, so the
	// terminal marker had to be its own fact -- and a decision that reached a
	// rollback is settled by the follower alongside it.
	if err := db.Automation.SettleDecision(ctx, run.RunID, "api", time.Now().UTC()); err != nil {
		t.Fatalf("SettleDecision: %v", err)
	}

	pending, err := db.Automation.PendingDecisions(ctx, 100)
	if err != nil {
		t.Fatalf("PendingDecisions: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want only the outstanding one: %+v", len(pending), pending)
	}
	if pending[0].ContainerName != "web" {
		t.Fatalf("pending names %q, want web", pending[0].ContainerName)
	}
}

func TestPendingDecisionsSurvivesTheRunFallingOutOfRecentHistory(t *testing.T) {
	// The defect, stated as a test: an approved decision on an old pass must
	// still be followed through, however many passes have run since.
	db := openTestDB(t)
	ctx := context.Background()

	old := startRun(t, db, domain.AutoTriggerSchedule)
	decision := decisionFor(old, "web", domain.VerdictUpdate)
	decision.AcquisitionID = "acq_0123456789abcdef0123"
	if _, err := db.Automation.RecordDecisions(ctx, []domain.AutomationDecision{decision}); err != nil {
		t.Fatalf("RecordDecisions: %v", err)
	}
	if err := db.Automation.FinishRun(ctx, old.RunID, domain.RunCompleted,
		domain.AutomationRun{}, time.Now().UTC()); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	// Ten passes go by, none of them submitting anything.
	for i := 0; i < 10; i++ {
		later := startRun(t, db, domain.AutoTriggerSchedule)
		if err := db.Automation.FinishRun(ctx, later.RunID, domain.RunCompleted,
			domain.AutomationRun{}, time.Now().UTC()); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
	}

	pending, err := db.Automation.PendingDecisions(ctx, 100)
	if err != nil {
		t.Fatalf("PendingDecisions: %v", err)
	}
	if len(pending) != 1 || pending[0].ContainerName != "web" {
		t.Fatalf("the outstanding update must still be found, got %+v", pending)
	}
}

func TestDecisionSurvivesItsPolicyBeingWithdrawn(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	policy := createUpdatePolicy(t, db, newUpdatePolicy("Nightly patches"))
	run := startRun(t, db, domain.AutoTriggerSchedule)

	decision := decisionFor(run, "web", domain.VerdictSkip)
	decision.PolicyID = policy.PolicyID
	decision.PolicyName = policy.Name
	if _, err := db.Automation.RecordDecisions(ctx, []domain.AutomationDecision{decision}); err != nil {
		t.Fatalf("RecordDecisions: %v", err)
	}

	if err := db.UpdatePolicies.ArchiveUpdatePolicy(ctx, policy.PolicyID, time.Now().UTC()); err != nil {
		t.Fatalf("ArchiveUpdatePolicy: %v", err)
	}

	loaded, _, err := db.Automation.ListDecisions(ctx, store.AutomationDecisionFilter{RunID: run.RunID})
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(loaded) != 1 || loaded[0].PolicyName != "Nightly patches" {
		t.Fatalf("the decision must still name the policy that made it: %+v", loaded)
	}
}

func TestCountAwaitingApprovalOnlyCountsTheLatestPass(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	older := startRun(t, db, domain.AutoTriggerSchedule)
	if _, err := db.Automation.RecordDecisions(ctx, []domain.AutomationDecision{
		decisionFor(older, "web", domain.VerdictAwaitingApproval),
		decisionFor(older, "api", domain.VerdictAwaitingApproval),
	}); err != nil {
		t.Fatalf("RecordDecisions: %v", err)
	}
	if err := db.Automation.FinishRun(ctx, older.RunID, domain.RunCompleted,
		domain.AutomationRun{}, time.Now().UTC()); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	newer := startRun(t, db, domain.AutoTriggerSchedule)
	if _, err := db.Automation.RecordDecisions(ctx, []domain.AutomationDecision{
		decisionFor(newer, "web", domain.VerdictAwaitingApproval),
	}); err != nil {
		t.Fatalf("RecordDecisions: %v", err)
	}

	count, err := db.Automation.CountAwaitingApproval(ctx)
	if err != nil {
		t.Fatalf("CountAwaitingApproval: %v", err)
	}
	// Last week's proposal is not the question "should this happen now".
	if count != 1 {
		t.Fatalf("awaiting approval = %d, want 1", count)
	}
}

func TestListingDecisionsCanMatchTheApprovalCount(t *testing.T) {
	// The defect this pins, found against a live host: the approvals queue
	// listed every pass's held decisions while the dashboard counted only the
	// latest pass's. Two outstanding approvals read as twenty-eight rows, and
	// the number that sent the operator to the page said two.
	//
	// The list and the count must answer the same question, so the list can
	// apply the same restriction.
	db := openTestDB(t)
	ctx := context.Background()

	older := startRun(t, db, domain.AutoTriggerSchedule)
	if _, err := db.Automation.RecordDecisions(ctx, []domain.AutomationDecision{
		decisionFor(older, "web", domain.VerdictAwaitingApproval),
		decisionFor(older, "api", domain.VerdictAwaitingApproval),
	}); err != nil {
		t.Fatalf("RecordDecisions: %v", err)
	}
	if err := db.Automation.FinishRun(ctx, older.RunID, domain.RunCompleted,
		domain.AutomationRun{}, time.Now().UTC()); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	newer := startRun(t, db, domain.AutoTriggerSchedule)
	if _, err := db.Automation.RecordDecisions(ctx, []domain.AutomationDecision{
		decisionFor(newer, "web", domain.VerdictAwaitingApproval),
	}); err != nil {
		t.Fatalf("RecordDecisions: %v", err)
	}

	filter := store.AutomationDecisionFilter{
		Verdicts:      []domain.AutomationVerdict{domain.VerdictAwaitingApproval},
		LatestRunOnly: true,
	}
	items, total, err := db.Automation.ListDecisions(ctx, filter)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}

	count, err := db.Automation.CountAwaitingApproval(ctx)
	if err != nil {
		t.Fatalf("CountAwaitingApproval: %v", err)
	}
	if total != count {
		t.Fatalf("the queue holds %d and the counter says %d; they must agree", total, count)
	}
	if len(items) != 1 || items[0].ContainerName != "web" {
		t.Fatalf("want only the latest pass's held decision, got %+v", items)
	}

	// And the unrestricted listing still sees the whole history, because that
	// is what the pass detail is for.
	filter.LatestRunOnly = false
	_, all, err := db.Automation.ListDecisions(ctx, filter)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if all != 3 {
		t.Fatalf("the history holds %d held decisions, want 3", all)
	}
}

func TestPruningRunsTakesTheirDecisionsWithThem(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	old := time.Now().UTC().Add(-90 * 24 * time.Hour)
	run, err := db.Automation.StartRun(ctx, domain.AutomationRun{
		RunID:     domain.NewAutomationRunID(),
		Trigger:   domain.AutoTriggerSchedule,
		StartedAt: old,
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := db.Automation.RecordDecisions(ctx, []domain.AutomationDecision{
		decisionFor(run, "web", domain.VerdictSkip),
	}); err != nil {
		t.Fatalf("RecordDecisions: %v", err)
	}
	if err := db.Automation.FinishRun(ctx, run.RunID, domain.RunCompleted,
		domain.AutomationRun{}, old); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	pruned, err := db.Automation.PruneRuns(ctx, time.Now().UTC().Add(-30*24*time.Hour), 100)
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned %d runs, want 1", pruned)
	}

	_, total, err := db.Automation.ListDecisions(ctx, store.AutomationDecisionFilter{RunID: run.RunID})
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if total != 0 {
		t.Fatalf("%d decisions outlived their run", total)
	}
}

func TestPruningNeverTakesARunningPass(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// A pass that started long ago and is still running is a bug worth seeing,
	// not a row to quietly delete.
	if _, err := db.Automation.StartRun(ctx, domain.AutomationRun{
		RunID:     domain.NewAutomationRunID(),
		Trigger:   domain.AutoTriggerSchedule,
		StartedAt: time.Now().UTC().Add(-90 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	pruned, err := db.Automation.PruneRuns(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("pruned %d running passes, want 0", pruned)
	}
}

// ---------------------------------------------------------------- pauses --

func TestPauseIsKeyedOnTheNameSoItSurvivesRecreation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.Automation.Pause(ctx, domain.PausedContainer{
		ContainerName: "web",
		ContainerID:   "container-before",
		Reason:        domain.PauseRolledBack,
		Detail:        "the update was rolled back",
		Failures:      1,
		PausedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// The container is recreated and gets a new id. The pause must still apply.
	pause, err := db.Automation.PauseFor(ctx, "web")
	if err != nil {
		t.Fatalf("PauseFor: %v", err)
	}
	if pause.ContainerID != "container-before" {
		t.Fatalf("the recorded id is history, got %q", pause.ContainerID)
	}
	if !pause.Active(time.Now().UTC()) {
		t.Fatal("a pause with no cooldown stays active until it is acknowledged")
	}
}

func TestOnlyOneActivePausePerContainer(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	pause := domain.PausedContainer{
		ContainerName: "web",
		Reason:        domain.PauseRepeatedFailure,
		PausedAt:      time.Now().UTC(),
	}
	if _, err := db.Automation.Pause(ctx, pause); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, err := db.Automation.Pause(ctx, pause); !errors.Is(err, store.ErrPauseActive) {
		t.Fatalf("want ErrPauseActive, got %v", err)
	}

	// Acknowledging frees the slot, and the earlier pause stays in the history.
	if err := db.Automation.Resume(ctx, "web",
		domain.Requester{UserID: "usr_1", Username: "colby"}, time.Now().UTC()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := db.Automation.Pause(ctx, pause); err != nil {
		t.Fatalf("re-pausing an acknowledged container must be allowed: %v", err)
	}

	_, total, err := db.Automation.ListPauses(ctx, false, store.Page{})
	if err != nil {
		t.Fatalf("ListPauses: %v", err)
	}
	if total != 2 {
		t.Fatalf("pause history = %d rows, want 2", total)
	}
}

func TestResumeRecordsWhoDidItAndClearsTheCount(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.Automation.RecordFailure(ctx, "web", "health check failed", 24, time.Now().UTC()); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if _, err := db.Automation.RecordFailure(ctx, "web", "health check failed", 24, time.Now().UTC()); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if _, err := db.Automation.Pause(ctx, domain.PausedContainer{
		ContainerName: "web",
		Reason:        domain.PauseRepeatedFailure,
		Failures:      2,
		PausedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	if err := db.Automation.Resume(ctx, "web",
		domain.Requester{UserID: "usr_1", Username: "colby"}, time.Now().UTC()); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	pauses, _, err := db.Automation.ListPauses(ctx, false, store.Page{})
	if err != nil {
		t.Fatalf("ListPauses: %v", err)
	}
	if pauses[0].AcknowledgedAt == nil || pauses[0].AcknowledgedBy.Username != "colby" {
		t.Fatalf("the acknowledgement must name who made it: %+v", pauses[0])
	}
	if pauses[0].Active(time.Now().UTC()) {
		t.Fatal("an acknowledged pause no longer blocks automation")
	}

	// An operator who investigated and fixed the problem must not be one
	// failure away from the same pause.
	count, err := db.Automation.FailureCount(ctx, "web")
	if err != nil {
		t.Fatalf("FailureCount: %v", err)
	}
	if count.Consecutive != 0 || count.Windowed != 0 {
		t.Fatalf("resuming must clear the count, got %+v", count)
	}
}

func TestResumeMustNameAnAccount(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.Automation.Pause(ctx, domain.PausedContainer{
		ContainerName: "web",
		Reason:        domain.PauseOperator,
		PausedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := db.Automation.Resume(ctx, "web", domain.Requester{}, time.Now().UTC()); err == nil {
		t.Fatal("clearing a safety pause must record who cleared it")
	}
}

func TestPauseCooldownExpires(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()
	resume := now.Add(2 * time.Hour)
	if _, err := db.Automation.Pause(ctx, domain.PausedContainer{
		ContainerName: "web",
		Reason:        domain.PauseRepeatedFailure,
		PausedAt:      now,
		ResumeAfter:   &resume,
	}); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	pause, err := db.Automation.PauseFor(ctx, "web")
	if err != nil {
		t.Fatalf("PauseFor: %v", err)
	}
	if !pause.Active(now.Add(time.Hour)) {
		t.Fatal("the pause still blocks inside the cooldown")
	}
	if pause.Active(now.Add(3 * time.Hour)) {
		t.Fatal("the pause no longer blocks once the cooldown has elapsed")
	}
	// It is still returned by ActivePauses, because whether an elapsed cooldown
	// blocks is the scheduler's decision against one clock reading, not SQL's
	// against a different one.
	active, err := db.Automation.ActivePauses(ctx)
	if err != nil {
		t.Fatalf("ActivePauses: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("want the pause returned for the scheduler to judge, got %d", len(active))
	}
}

// -------------------------------------------------------------- failures --

func TestFailureWindowResetsWhenItHasElapsed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	start := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := db.Automation.RecordFailure(ctx, "web", "first", 24, start); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	// Two days later the 24-hour window has elapsed, so this is failure one of
	// a new window rather than failure two of the old one.
	count, err := db.Automation.RecordFailure(ctx, "web", "second", 24, time.Now().UTC())
	if err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if count.Windowed != 1 {
		t.Fatalf("windowed = %d, want 1 after the window elapsed", count.Windowed)
	}
	// The consecutive count is a different question and keeps climbing.
	if count.Consecutive != 2 {
		t.Fatalf("consecutive = %d, want 2", count.Consecutive)
	}
}

func TestFailuresInsideTheWindowAccumulate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()
	for i := 1; i <= 3; i++ {
		count, err := db.Automation.RecordFailure(ctx, "web",
			fmt.Sprintf("attempt %d", i), 24, now.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatalf("RecordFailure: %v", err)
		}
		if count.Windowed != i {
			t.Fatalf("after %d failures windowed = %d", i, count.Windowed)
		}
	}
}

func TestSuccessClearsTheConsecutiveCountButNotTheWindow(t *testing.T) {
	// A container that fails, succeeds, then fails again inside the same window
	// has failed twice in that window. A policy saying "pause after two
	// failures in 24 hours" meant exactly that, and clearing the window on any
	// success would make the setting unreachable for the flapping container it
	// exists to catch.
	db := openTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()
	if _, err := db.Automation.RecordFailure(ctx, "web", "first", 24, now); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if err := db.Automation.RecordSuccess(ctx, "web", now.Add(time.Minute)); err != nil {
		t.Fatalf("RecordSuccess: %v", err)
	}

	count, err := db.Automation.FailureCount(ctx, "web")
	if err != nil {
		t.Fatalf("FailureCount: %v", err)
	}
	if count.Consecutive != 0 {
		t.Fatalf("consecutive = %d, want 0 after a success", count.Consecutive)
	}
	if count.Windowed != 1 {
		t.Fatalf("windowed = %d, want the window preserved", count.Windowed)
	}

	next, err := db.Automation.RecordFailure(ctx, "web", "second", 24, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if next.Windowed != 2 {
		t.Fatalf("windowed = %d, want 2 -- the flap must reach the threshold", next.Windowed)
	}
}

func TestFailureCountOfAContainerThatNeverFailedIsZeroNotAnError(t *testing.T) {
	db := openTestDB(t)

	count, err := db.Automation.FailureCount(context.Background(), "never-seen")
	if err != nil {
		t.Fatalf("a container with no record has a count of zero, not an error: %v", err)
	}
	if count.Consecutive != 0 || count.LastFailureAt != nil {
		t.Fatalf("unexpected count: %+v", count)
	}
}
