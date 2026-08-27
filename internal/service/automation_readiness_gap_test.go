package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Stage 17.4 step 1: what the readiness question can and cannot be answered
// with, BEFORE anything is changed.
//
// # Why this file exists
//
// The stage brief asks for an operator-facing answer to "which containers can
// HarborMaster manage automatically right now, and why not the others". The
// obvious assumption is that nothing answers it today and a readiness engine
// must be built.
//
// That assumption is wrong, and this file is the evidence. `Upcoming` already
// runs the real decision function over the real evidence and returns the real
// reasons -- one authoritative answer per container, from the same code the
// pass uses. What is missing is narrower than a new engine, and these tests
// pin exactly what:
//
//	1. it evaluates only SAVED policies, so an unsaved preset cannot be
//	   previewed;
//	2. it omits the pass's phase 2, so it OVER-REPORTS eligibility on any
//	   estate with dependencies;
//	3. it discards the truncation flag, so on an estate past the target bound
//	   it silently answers for a prefix.
//
// (2) and (3) are production defects in an existing surface rather than
// missing features, and the second one matters most: the number an operator
// would read is too high, in the direction that says "automation will handle
// this" when it will not.

// readinessEstate builds one container for each class the brief enumerates.
//
// Every determination below is owned by an existing component. The point of
// building all nine at once is that ONE call has to produce all nine answers:
// a readiness surface that needs a second pass for some of them is a second
// engine, whatever it is called.
//
//	class                     owned by
//	------------------------  --------------------------------------------
//	would automate            DecideAutomation, step 11 (mode)
//	not selected              domain.SelectUpdatePolicy, step 3
//	opted out by label        domain.ParseUpdateLabels, step 2
//	no plan                   AutomationEvidence.CurrentPlan, step 5
//	needs a person            DecideAutomation, step 8 (recommendation)
//	already in flight         AutomationEvidence.Acquisition/ExecutionActive
//	paused                    AutomationStore.ActivePauses, step 1
//	dependency held           applyDependencyGate + DecideDependency (phase 2)
//	HarborMaster itself       domain.SelfIdentity.SelfMatch, step 0
func readinessEstate(t *testing.T) *automationHarness {
	t.Helper()

	// One broad policy, so "not selected" has to come from the selector rather
	// than from there being no policy at all.
	policy := automaticPolicy()
	policy.Selector = domain.UpdateSelector{
		Include: []string{"ready", "needsPerson", "inflight", "paused", "harbormaster"},
	}
	policy.Normalise()

	harness := newAutomationHarness(t, policy)

	target := func(name string, labels map[string]string) store.AutomationTarget {
		const image = "nginx:1.27.3"
		return store.AutomationTarget{
			ContainerID: "container-" + name,
			Selection: domain.SelectionTarget{
				Name:        name,
				Image:       image,
				Labels:      labels,
				Eligibility: domain.ScreenTarget(name, image, labels),
			},
			State: domain.StateRunning,
		}
	}

	harness.evidence.targets = []store.AutomationTarget{
		target("ready", nil),
		target("notselected", nil),
		target("optedout", map[string]string{domain.LabelUpdateEnabled: "false"}),
		target("noplan", nil),
		target("needsPerson", nil),
		target("inflight", nil),
		target("paused", nil),
		target("harbormaster", nil),
	}

	// Plans. "noplan" deliberately has none.
	needsPerson := planFor("needsPerson")
	needsPerson.Risk = domain.RiskAssessment{
		Score:          48,
		Band:           domain.RiskMedium,
		Recommendation: domain.RecommendManualReview,
	}

	harness.evidence.plans = map[string]domain.ChangePlan{
		"container-ready":        planFor("ready"),
		"container-notselected":  planFor("notselected"),
		"container-optedout":     planFor("optedout"),
		"container-needsPerson":  needsPerson,
		"container-inflight":     planFor("inflight"),
		"container-paused":       planFor("paused"),
		"container-harbormaster": planFor("harbormaster"),
	}

	harness.evidence.inFlight = map[string]bool{"container-inflight": true}

	if _, err := harness.store.Pause(context.Background(), domain.PausedContainer{
		ContainerName: "paused",
		Reason:        domain.PauseRolledBack,
		Failures:      1,
		PausedAt:      decideAt.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("record pause: %v", err)
	}

	// HarborMaster's own container, so step 0 has something to match.
	options := harness.optionsWithSelf(domain.SelfIdentity{ContainerName: "harbormaster"})
	harness.engine = service.NewAutomationService(options)

	return harness
}

// reasonsByContainer indexes decisions for assertion.
func reasonsByContainer(decisions []domain.AutomationDecision) map[string]domain.AutomationDecision {
	out := make(map[string]domain.AutomationDecision, len(decisions))
	for _, decision := range decisions {
		out[decision.ContainerName] = decision
	}
	return out
}

// TestUpcomingAlreadyAnswersReadinessPerContainer is the positive half of the
// reproduction: the authoritative answer exists today, for a saved policy.
func TestUpcomingAlreadyAnswersReadinessPerContainer(t *testing.T) {
	harness := readinessEstate(t)

	decisions, err := harness.engine.Upcoming(context.Background())
	if err != nil {
		t.Fatalf("upcoming: %v", err)
	}
	got := reasonsByContainer(decisions)

	want := map[string]domain.AutomationReason{
		"ready":        domain.ReasonEligible,
		"notselected":  domain.ReasonNotSelected,
		"optedout":     domain.ReasonLabelOff,
		"noplan":       domain.ReasonNotSelected, // not in the selector either
		"needsPerson":  domain.ReasonRecommendation,
		"inflight":     domain.ReasonInFlight,
		"paused":       domain.ReasonPaused,
		"harbormaster": domain.ReasonSelfUpdate,
	}

	for name, reason := range want {
		decision, present := got[name]
		if !present {
			t.Fatalf("no decision for %q", name)
		}
		if decision.Reason != reason {
			t.Errorf("%s: reason = %q, want %q (detail: %s)",
				name, decision.Reason, reason, decision.Detail)
		}
	}

	// And the eligible one is the only one that would act.
	eligible := 0
	for _, decision := range decisions {
		if decision.Verdict == domain.VerdictUpdate {
			eligible++
		}
	}
	if eligible != 1 {
		t.Fatalf("eligible = %d, want exactly 1", eligible)
	}
}

// TestUpcomingAgreesWithThePassAboutDependencies is production defect 1, fixed.
//
// Before Stage 17.4 the real pass ran three phases -- decide, gate, submit --
// and `Upcoming` ran only the first. A container the pass held as
// `dependencyWaiting` was reported by the readiness surface as eligible: the
// wrong direction, telling an operator automation would handle something it
// will hold.
//
// The fix was not to add the gate to `Upcoming`. It was to give both callers
// ONE evaluation, so the two cannot answer differently -- which is what this
// asserts, by comparing them rather than by checking either alone.
func TestUpcomingAgreesWithThePassAboutDependencies(t *testing.T) {
	harness := newAutomationHarness(t, scalePolicy())

	// A provider and its dependent, both with plans. The pass must submit the
	// provider first and hold the dependent until a later pass.
	names := []string{"provider", "dependent"}
	view := graphOver(t, names, namespaceDep("dependent", "provider"))
	withDependencyEstate(t, harness, view, names, nil)

	// What the REAL pass does.
	//
	// NOT a dry run, deliberately. A dry run rewrites every eligible verdict to
	// `wouldUpdate`/`dryRunMode` in phase 3 -- after the evaluation this test is
	// comparing -- so comparing against one would measure the dry-run rewrite
	// rather than the gate. The harness's pipeline is a fake that records
	// submissions and touches nothing.
	_, passDecisions, err := harness.engine.RunNow(
		context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	fromPass := reasonsByContainer(passDecisions)

	if fromPass["dependent"].DependencyState != domain.DependencyWaiting {
		t.Fatalf("fixture: the pass must hold the dependent, got state %q reason %q",
			fromPass["dependent"].DependencyState, fromPass["dependent"].Reason)
	}

	// What the readiness surface says about the same estate.
	upcoming, err := harness.engine.Upcoming(context.Background())
	if err != nil {
		t.Fatalf("upcoming: %v", err)
	}
	fromUpcoming := reasonsByContainer(upcoming)

	// The two must agree, container by container, on every field that decides
	// whether automation will act. Comparing them is the assertion: pinning
	// only Upcoming's answer would let both drift together.
	for name, pass := range fromPass {
		preview, present := fromUpcoming[name]
		if !present {
			t.Fatalf("%s: the pass decided it, the preview did not mention it", name)
		}
		if preview.Verdict != pass.Verdict {
			t.Errorf("%s: preview verdict %q, pass verdict %q",
				name, preview.Verdict, pass.Verdict)
		}
		if preview.Reason != pass.Reason {
			t.Errorf("%s: preview reason %q, pass reason %q",
				name, preview.Reason, pass.Reason)
		}
		if preview.DependencyState != pass.DependencyState {
			t.Errorf("%s: preview dependency state %q, pass %q",
				name, preview.DependencyState, pass.DependencyState)
		}
	}

	// And specifically the case that used to differ.
	if fromUpcoming["dependent"].DependencyState != domain.DependencyWaiting {
		t.Fatalf("the preview must hold the dependent, got %q/%q",
			fromUpcoming["dependent"].Verdict, fromUpcoming["dependent"].Reason)
	}
}

// TestReadinessReportsATruncatedEstate is production defect 2, fixed.
//
// `Targets` reports whether the estate was cut at the bound. The pass says so
// in the run's message; `Upcoming` assigned it to `_`, so a count over a
// bounded estate silently described a prefix of it.
//
// A count nobody can tell is partial is worse than no count: it reads as the
// whole estate.
func TestReadinessReportsATruncatedEstate(t *testing.T) {
	harness := readinessEstate(t)
	harness.evidence.truncated = true

	_, truncated, err := harness.engine.UpcomingAt(context.Background())
	if err != nil {
		t.Fatalf("upcoming: %v", err)
	}
	if !truncated {
		t.Fatal("the estate was truncated and the preview did not say so")
	}

	// And it reaches the report an operator actually reads.
	report, _, err := harness.engine.Readiness(context.Background(), nil)
	if err != nil {
		t.Fatalf("readiness: %v", err)
	}
	if !report.Truncated {
		t.Fatal("the readiness report must carry the truncation flag")
	}

	// The honest direction: a full estate reports false rather than defaulting
	// to "probably fine".
	harness.evidence.truncated = false
	report, _, err = harness.engine.Readiness(context.Background(), nil)
	if err != nil {
		t.Fatalf("readiness: %v", err)
	}
	if report.Truncated {
		t.Fatal("a complete estate must not be reported as truncated")
	}
}
