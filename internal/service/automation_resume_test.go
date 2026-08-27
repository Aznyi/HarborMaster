package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Stage 17.6: what RESUME means, proved by what it does not do.
//
// # The one sentence this file defends
//
//	Resume removes a constraint. It does not perform an update.
//
// Every test below is a way of being wrong about that. The dangerous failure is
// not a resume that fails -- an operator sees that. It is a resume that quietly
// becomes a retry: the container the operator was investigating is replaced the
// moment they clear the pause, using the evidence that already failed once.
//
// So the assertions are mostly about ABSENCE, and absence has to be asserted
// against a fixture where the presence would otherwise happen. Every case here
// starts from a container that is paused AND has an eligible update waiting, so
// "nothing happened" is a real finding rather than an empty estate.

// resumedAt is the clock for this file.
var resumedAt = time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)

// pausedEstate builds a container that is paused and would otherwise update.
//
// The pause is `automaticRollback`, which is the strongest case: it means the
// change was wrong AND the host was moved twice. If any path retries anything
// automatically, this is the one where it would hurt most.
func pausedEstate(t *testing.T, policy domain.UpdatePolicy) *automationHarness {
	t.Helper()

	harness := newAutomationHarness(t, policy)
	harness.now = resumedAt
	harness.evidence.targets = []store.AutomationTarget{
		readinessTarget("web", nil),
	}
	harness.evidence.plans = map[string]domain.ChangePlan{
		"container-web": assessedPlan("web", "latest", domain.UpdateDigest, 6),
	}
	harness.evidence.inFlight = map[string]bool{}

	if _, err := harness.store.Pause(context.Background(), domain.PausedContainer{
		ContainerName: "web",
		Reason:        domain.PauseRolledBack,
		Detail:        "the replacement failed its health check and the previous container was restored",
		Failures:      2,
		ExecutionID:   "exec_0123456789abcdef0123",
		PausedAt:      resumedAt.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("record pause: %v", err)
	}
	return harness
}

// resumablePolicy is an automatic policy that would take `web`.
func resumablePolicy() domain.UpdatePolicy {
	policy := automaticPolicy()
	policy.Strategy = domain.StrategyDigestOnly
	policy.MinimumRecommendation = domain.RecommendCaution
	policy.Window = domain.MaintenanceWindow{AlwaysOpen: true}
	policy.Normalise()
	return policy
}

func resumeAsOperator(t *testing.T, harness *automationHarness) {
	t.Helper()
	err := harness.engine.Resume(context.Background(), "web",
		domain.Requester{UserID: "u1", Username: "op"}, service.Actor{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
}

// assertNothingWasRequested is the zero-mutation assertion, reused everywhere.
func assertNothingWasRequested(t *testing.T, harness *automationHarness, when string) {
	t.Helper()
	for _, kind := range []string{"acquire", "execute", "rollback"} {
		if got := len(harness.pipeline.recorded(kind)); got != 0 {
			t.Fatalf("%s: %d %s requests were made; resume must cause none",
				when, got, kind)
		}
	}
}

// ---------------------------------------------------------- §12 no mutation --

// TestResumeCausesNoImmediateMutation is the most important test in the stage.
func TestResumeCausesNoImmediateMutation(t *testing.T) {
	harness := pausedEstate(t, resumablePolicy())

	// The container IS eligible but for the pause: proved by asking, so the
	// absence below is meaningful rather than vacuous.
	before, err := harness.engine.Upcoming(context.Background())
	if err != nil {
		t.Fatalf("upcoming: %v", err)
	}
	if before[0].Reason != domain.ReasonPaused {
		t.Fatalf("fixture: expected the pause to hold it, got %q", before[0].Reason)
	}

	resumeAsOperator(t, harness)

	// Immediately afterwards: nothing was requested, and no run was recorded.
	assertNothingWasRequested(t, harness, "immediately after resume")

	harness.store.mu.Lock()
	runs := len(harness.store.runs)
	harness.store.mu.Unlock()
	if runs != 0 {
		t.Fatalf("resume started %d automation runs; it must start none", runs)
	}

	// The pause is gone, and the container is now merely evaluable.
	after, err := harness.engine.Upcoming(context.Background())
	if err != nil {
		t.Fatalf("upcoming: %v", err)
	}
	if after[0].Reason == domain.ReasonPaused {
		t.Fatal("the pause was not cleared")
	}

	// Only a NORMAL pass may act, and it starts from ordinary evaluation.
	if _, _, err := harness.engine.RunNow(
		context.Background(), false, domain.Requester{}); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := len(harness.pipeline.recorded("acquire")); got != 1 {
		t.Fatalf("the pass made %d acquisition requests, want 1", got)
	}
	// And what it asked for is a PLAN identifier, never an image: the pass
	// re-derived the target rather than replaying anything the pause carried.
	if id := harness.pipeline.recorded("acquire")[0].id; id != assessedPlan("web", "latest", domain.UpdateDigest, 6).PlanID {
		t.Fatalf("the pass submitted %q, which is not the current plan", id)
	}
}

// --------------------------------------------------- §13 current evidence --

// TestResumeDoesNotReplayTheFailedPlan is the staleness proof.
//
// The plan that failed is not authority for anything after a resume. The next
// pass reads whatever the planner currently says, and if that refuses the
// update then there is no update.
func TestResumeDoesNotReplayTheFailedPlan(t *testing.T) {
	harness := pausedEstate(t, resumablePolicy())
	failed := harness.evidence.plans["container-web"]

	// Between the failure and the resume, the evidence moves: the planner has
	// written a new plan, and it is one this policy will not act on.
	replacement := manualReviewPlan("web")
	replacement.PlanID = "plan_" + repeatHexN('9', 20)
	harness.evidence.plans["container-web"] = replacement

	resumeAsOperator(t, harness)
	assertNothingWasRequested(t, harness, "after resume with new evidence")

	if _, _, err := harness.engine.RunNow(
		context.Background(), false, domain.Requester{}); err != nil {
		t.Fatalf("pass: %v", err)
	}

	// The failed plan was never submitted, and neither was anything else: the
	// CURRENT evidence refuses it.
	for _, request := range harness.pipeline.recorded("acquire") {
		if request.id == failed.PlanID {
			t.Fatal("the previously failed plan was replayed after resume")
		}
	}
	if got := len(harness.pipeline.recorded("acquire")); got != 0 {
		t.Fatalf("%d acquisitions were requested; current evidence refuses this update", got)
	}
}

// ------------------------------------------------------- §14 manual review --

// TestResumeCreatesNoHiddenApproval keeps Stage 17.7 out of this stage.
func TestResumeCreatesNoHiddenApproval(t *testing.T) {
	harness := pausedEstate(t, resumablePolicy())
	harness.evidence.plans["container-web"] = manualReviewPlan("web")

	resumeAsOperator(t, harness)

	decisions, err := harness.engine.Upcoming(context.Background())
	if err != nil {
		t.Fatalf("upcoming: %v", err)
	}
	if decisions[0].Reason != domain.ReasonRecommendation {
		t.Fatalf("reason = %q, want %q: resume must not approve a manual review",
			decisions[0].Reason, domain.ReasonRecommendation)
	}
	if decisions[0].Verdict == domain.VerdictUpdate {
		t.Fatal("a manual-review plan became eligible after a resume")
	}
	// The plan's own recommendation is untouched.
	if harness.evidence.plans["container-web"].Risk.Recommendation != domain.RecommendManualReview {
		t.Fatal("resume changed a plan's recommendation")
	}
	assertNothingWasRequested(t, harness, "after resuming a manual-review container")
}

// -------------------------------------------------------- §15 dependencies --

// TestResumeDoesNotBypassDependencyOrdering pins Phase 16 through a resume.
func TestResumeDoesNotBypassDependencyOrdering(t *testing.T) {
	harness := newAutomationHarness(t, scalePolicy())
	harness.now = resumedAt

	names := []string{"provider", "dependent"}
	view := graphOver(t, names, namespaceDep("dependent", "provider"))
	withDependencyEstate(t, harness, view, names, nil)

	if _, err := harness.store.Pause(context.Background(), domain.PausedContainer{
		ContainerName: "dependent",
		Reason:        domain.PauseRepeatedFailure,
		Failures:      2,
		PausedAt:      resumedAt.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("record pause: %v", err)
	}

	if err := harness.engine.Resume(context.Background(), "dependent",
		domain.Requester{UserID: "u1", Username: "op"}, service.Actor{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	assertNothingWasRequested(t, harness, "after resuming a dependent")

	decisions, err := harness.engine.Upcoming(context.Background())
	if err != nil {
		t.Fatalf("upcoming: %v", err)
	}
	byName := reasonsByContainer(decisions)

	// The pause is gone and the DEPENDENCY state took its place. A dependency
	// hold is not a pause and must never have been converted into one.
	if byName["dependent"].Reason == domain.ReasonPaused {
		t.Fatal("the pause was not cleared")
	}
	if byName["dependent"].DependencyState != domain.DependencyWaiting {
		t.Fatalf("dependency state = %q, want %q",
			byName["dependent"].DependencyState, domain.DependencyWaiting)
	}
}

// ------------------------------------------------------------- §16 window --

// TestResumeDoesNotBypassTheMaintenanceWindow is another proof resume is not
// execution: it succeeds, and the container still cannot be updated yet.
func TestResumeDoesNotBypassTheMaintenanceWindow(t *testing.T) {
	policy := resumablePolicy()
	// A window that is closed at `resumedAt` (03:00 UTC).
	policy.Window = domain.MaintenanceWindow{Start: "22:00", End: "23:00"}
	policy.Normalise()

	harness := pausedEstate(t, policy)
	resumeAsOperator(t, harness)
	assertNothingWasRequested(t, harness, "after resuming outside the window")

	decisions, err := harness.engine.Upcoming(context.Background())
	if err != nil {
		t.Fatalf("upcoming: %v", err)
	}
	if decisions[0].Reason != domain.ReasonWindowClosed {
		t.Fatalf("reason = %q, want %q", decisions[0].Reason, domain.ReasonWindowClosed)
	}
}

// ------------------------------------------------------------ §17 opt-out --

// TestResumeDoesNotMakeAnOptedOutContainerEligible.
//
// Resume clears the pause it was asked to clear. It does not argue with the
// container's owner: the label still refuses.
func TestResumeDoesNotMakeAnOptedOutContainerEligible(t *testing.T) {
	harness := pausedEstate(t, resumablePolicy())
	harness.evidence.targets = []store.AutomationTarget{
		readinessTarget("web", map[string]string{domain.LabelUpdateEnabled: "false"}),
	}

	resumeAsOperator(t, harness)
	assertNothingWasRequested(t, harness, "after resuming an opted-out container")

	decisions, err := harness.engine.Upcoming(context.Background())
	if err != nil {
		t.Fatalf("upcoming: %v", err)
	}
	if decisions[0].Reason != domain.ReasonLabelOff {
		t.Fatalf("reason = %q, want %q", decisions[0].Reason, domain.ReasonLabelOff)
	}
}

// -------------------------------------------------------- §18 self-update --

// TestResumeCreatesNoPathToUpdatingHarborMaster.
//
// A pause record for HarborMaster's own container should not exist, but the
// question this answers is what happens if one somehow does: clearing it must
// not produce a mutation path, because the self-update refusal is step 0 and
// owes nothing to the pause.
func TestResumeCreatesNoPathToUpdatingHarborMaster(t *testing.T) {
	harness := pausedEstate(t, resumablePolicy())
	harness.evidence.targets = []store.AutomationTarget{
		readinessTarget("harbormaster", nil),
	}
	harness.evidence.plans = map[string]domain.ChangePlan{
		"container-harbormaster": assessedPlan("harbormaster", "latest", domain.UpdateDigest, 6),
	}
	if _, err := harness.store.Pause(context.Background(), domain.PausedContainer{
		ContainerName: "harbormaster",
		Reason:        domain.PauseRepeatedFailure,
		Failures:      1,
		PausedAt:      resumedAt.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("record pause: %v", err)
	}

	options := harness.optionsWithSelf(domain.SelfIdentity{ContainerName: "harbormaster"})
	harness.engine = service.NewAutomationService(options)

	if err := harness.engine.Resume(context.Background(), "harbormaster",
		domain.Requester{UserID: "u1", Username: "op"}, service.Actor{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	assertNothingWasRequested(t, harness, "after resuming HarborMaster itself")

	decisions, err := harness.engine.Upcoming(context.Background())
	if err != nil {
		t.Fatalf("upcoming: %v", err)
	}
	if decisions[0].Reason != domain.ReasonSelfUpdate {
		t.Fatalf("reason = %q, want %q", decisions[0].Reason, domain.ReasonSelfUpdate)
	}

	if _, _, err := harness.engine.RunNow(
		context.Background(), false, domain.Requester{}); err != nil {
		t.Fatalf("pass: %v", err)
	}
	assertNothingWasRequested(t, harness, "after a pass over a resumed HarborMaster")
}

// ------------------------------------------------------ §21 readiness view --

// TestReadinessKeepsAPausedContainerPausedUnderAnyCandidatePolicy.
//
// A preview must not be able to talk the estate out of a safety state. However
// permissive the candidate policy is, a paused container is reported paused --
// and after an explicit resume, the same query moves it to whatever the current
// evidence says.
func TestReadinessKeepsAPausedContainerPausedUnderAnyCandidatePolicy(t *testing.T) {
	harness := pausedEstate(t, resumablePolicy())
	harness.policies.policies = nil

	// The most permissive thing a preset can compile.
	candidate := presetPolicyFor("permissive",
		domain.StrategyMinor, domain.ModeAutomatic)

	report, decisions, err := harness.engine.Readiness(context.Background(), &candidate)
	if err != nil {
		t.Fatalf("readiness: %v", err)
	}
	if decisions[0].Reason != domain.ReasonPaused {
		t.Fatalf("reason = %q, want %q: a preview must not clear a pause",
			decisions[0].Reason, domain.ReasonPaused)
	}
	if report.Eligible != 0 {
		t.Fatalf("eligible = %d, want 0", report.Eligible)
	}
	if len(report.Groups) != 1 || report.Groups[0].Reason != domain.ReasonPaused {
		t.Fatalf("groups = %+v, want one automationPaused group", report.Groups)
	}

	// The preview did not resume anything.
	pauses, err := harness.store.ActivePauses(context.Background())
	if err != nil {
		t.Fatalf("active pauses: %v", err)
	}
	if len(pauses) != 1 {
		t.Fatalf("the preview changed the pause state: %d active pauses", len(pauses))
	}

	// After an explicit resume the same question gets a different answer.
	resumeAsOperator(t, harness)
	report, decisions, err = harness.engine.Readiness(context.Background(), &candidate)
	if err != nil {
		t.Fatalf("readiness: %v", err)
	}
	if decisions[0].Reason == domain.ReasonPaused {
		t.Fatal("readiness still reports the pause after it was cleared")
	}
	if report.Eligible != 1 {
		t.Fatalf("eligible = %d, want 1 once the pause is cleared", report.Eligible)
	}
}

// ------------------------------------------------------- §10/§23 concurrency --

// TestConcurrentResumesSettleOnce is the idempotency proof.
//
// The partial unique index and the conditional UPDATE do the work: exactly one
// caller changes a row, and the rest are told there is nothing paused. No
// process-local lock is involved, so this holds across processes too.
func TestConcurrentResumesSettleOnce(t *testing.T) {
	harness := pausedEstate(t, resumablePolicy())

	const callers = 8
	var (
		wait      sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		notFound  int
	)
	wait.Add(callers)
	for i := range callers {
		go func(int) {
			defer wait.Done()
			err := harness.engine.Resume(context.Background(), "web",
				domain.Requester{UserID: "u1", Username: "op"}, service.Actor{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, store.ErrNotFound):
				notFound++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wait.Wait()

	if succeeded != 1 {
		t.Fatalf("%d resumes succeeded, want exactly 1 (%d reported not found)",
			succeeded, notFound)
	}
	if succeeded+notFound != callers {
		t.Fatalf("%d of %d callers got a defined answer", succeeded+notFound, callers)
	}

	// One effective state change, and no work caused by any of them.
	pauses, err := harness.store.ActivePauses(context.Background())
	if err != nil {
		t.Fatalf("active pauses: %v", err)
	}
	if len(pauses) != 0 {
		t.Fatalf("%d pauses are still active", len(pauses))
	}
	assertNothingWasRequested(t, harness, "after eight concurrent resumes")
}

// TestAPassAndAResumeDoNotCorruptEachOther races a resume against evaluation.
//
// Both outcomes are safe and both are allowed: a pass that read the pause holds
// the container for this round, and a pass that read the resume evaluates it
// normally. What must never happen is a mutation caused by the resume, or a
// pause left in a state that is neither active nor cleared.
func TestAPassAndAResumeDoNotCorruptEachOther(t *testing.T) {
	harness := pausedEstate(t, resumablePolicy())

	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		if _, _, err := harness.engine.RunNow(
			context.Background(), false, domain.Requester{}); err != nil {
			t.Errorf("pass: %v", err)
		}
	}()
	go func() {
		defer wait.Done()
		// Either answer is legitimate: the pass may not have started yet.
		_ = harness.engine.Resume(context.Background(), "web",
			domain.Requester{UserID: "u1", Username: "op"}, service.Actor{})
	}()
	wait.Wait()

	// The pause is in exactly one of its two defined states.
	pauses, err := harness.store.ActivePauses(context.Background())
	if err != nil {
		t.Fatalf("active pauses: %v", err)
	}
	if len(pauses) > 1 {
		t.Fatalf("%d active pauses for one container", len(pauses))
	}

	// At most one acquisition, and if there was one it came from the PASS.
	if got := len(harness.pipeline.recorded("acquire")); got > 1 {
		t.Fatalf("%d acquisitions from one pass and one resume", got)
	}
	if got := len(harness.pipeline.recorded("execute")); got != 0 {
		t.Fatalf("%d executions were requested directly; the follower owns that step", got)
	}
}

// ------------------------------------------------------------ §28 cost --

// TestPauseCostsNothingPerContainer measures the pause path.
//
// A paused container is declined by `loadContainerEvidence` before any
// per-container read, so an estate that is entirely paused costs the
// estate-wide reads and nothing else. That matters because the pause check runs
// on every evaluation, including every readiness preview an operator triggers
// while editing a policy.
func TestPauseCostsNothingPerContainer(t *testing.T) {
	harness := pausedEstate(t, resumablePolicy())

	if _, err := harness.engine.Upcoming(context.Background()); err != nil {
		t.Fatalf("upcoming: %v", err)
	}

	reads := harness.evidence.reads()
	perContainer := reads["plan"] + reads["acquisition"] + reads["execution"]
	if perContainer != 0 {
		t.Fatalf("%d per-container reads for a paused container; the pause check "+
			"declines before any of them", perContainer)
	}
	// The estate itself is still read once.
	if reads["targets"] != 1 {
		t.Fatalf("targets read %d times, want 1", reads["targets"])
	}

	// And after a resume the container is assessed normally, which is the
	// control proving the zero above came from the pause rather than from an
	// empty fixture.
	resumeAsOperator(t, harness)
	if _, err := harness.engine.Upcoming(context.Background()); err != nil {
		t.Fatalf("upcoming after resume: %v", err)
	}
	reads = harness.evidence.reads()
	if reads["plan"] == 0 {
		t.Fatal("no plan was read after the resume; the fixture proves nothing")
	}
	t.Logf("paused: 0 per-container reads; after resume: plan=%d acquisition=%d",
		reads["plan"], reads["acquisition"])
}
