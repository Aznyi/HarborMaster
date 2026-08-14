package service

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The needsWork vocabulary, pinned against the whole reason set.
//
// # Why this is an internal test
//
// needsWork is unexported and decides one thing: whether an upstream container
// counts as having outstanding work. Getting it wrong in the permissive
// direction RELEASES dependents on a container HarborMaster never assessed,
// which is how a dependency relationship silently stops constraining anything.
//
// That is exactly the defect Stage 4a found. The original read the ENGINE'S
// VERDICT -- treating `notSelected`, `noPolicy`, `notEligible` and `noPlan` as
// "nothing to do" -- when all four mean HarborMaster DID NOT LOOK.
//
// Testing it through the pass would need a fixture per reason, several of which
// are awkward to reach. Testing it directly makes the rule readable as a table,
// which is what stops it drifting back.

// TestOnlyAnAssessedNoUpdateClearsAnUpstream walks the reason vocabulary.
//
// The rule, stated once: exactly ONE reason means "assessed, and there is
// nothing to do". Every other reason -- including every reason that will be
// added later -- means work is outstanding or was never established, and both
// hold dependents.
func TestOnlyAnAssessedNoUpdateClearsAnUpstream(t *testing.T) {
	t.Parallel()

	// The reasons that mean HarborMaster established nothing. Each of these
	// released dependents before the Stage 4a fix.
	unestablished := []domain.AutomationReason{
		domain.ReasonNotSelected,
		domain.ReasonNoPolicy,
		domain.ReasonNotEligible,
		domain.ReasonNoPlan,
	}
	// The reasons that mean there IS work and it is not being done.
	deferred := []domain.AutomationReason{
		domain.ReasonPolicyOff,
		domain.ReasonLabelOff,
		domain.ReasonLabelPaused,
		domain.ReasonPaused,
		domain.ReasonStrategy,
		domain.ReasonRecommendation,
		domain.ReasonWindowClosed,
		domain.ReasonWindowInvalid,
		domain.ReasonInFlight,
		domain.ReasonConcurrency,
		domain.ReasonRunLimit,
		domain.ReasonRegistryLimit,
		domain.ReasonRefused,
		domain.ReasonError,
		domain.ReasonSelfUpdate,
		domain.ReasonObserveMode,
		domain.ReasonDryRunMode,
		domain.ReasonNeedApproval,
	}

	// unknown is the zero assessment: HarborMaster established nothing.
	unknown := domain.CurrentAssessment{}
	// current is a positive finding that the container needs no update.
	current := domain.CurrentAssessment{Established: true, Reason: "assessed and current"}

	for _, reason := range append(append([]domain.AutomationReason{}, unestablished...), deferred...) {
		decision := domain.AutomationDecision{
			Verdict: domain.VerdictSkip,
			Reason:  reason,
		}
		if !needsWork(decision, unknown) {
			t.Errorf("reason %q cleared an upstream with no assessment\n"+
				"\tOnly `noUpdate` -- assessed, and nothing proposed -- may release a "+
				"dependent. Everything else is either outstanding work or a state "+
				"HarborMaster did not establish, and both must hold.", reason)
		}
	}

	// The one reason that clears without any assessment.
	settled := domain.AutomationDecision{
		Verdict: domain.VerdictSkip,
		Reason:  domain.ReasonNoUpdate,
	}
	if needsWork(settled, unknown) {
		t.Fatal("an assessed container with nothing proposed did not clear its dependents")
	}

	// ---- the Stage 5c dimension: a positive current assessment ------------
	//
	// `noPlan` is the ONE reason the assessment explains, because it is the one
	// the planner produces for a container it assessed and found current. Every
	// other reason stays held even with a positive assessment: an excluded,
	// opted-out, paused or refused container is not released by its image
	// happening to be up to date.
	noPlan := domain.AutomationDecision{
		Verdict: domain.VerdictSkip,
		Reason:  domain.ReasonNoPlan,
	}
	if needsWork(noPlan, current) {
		t.Fatal("a container with no plan row and a POSITIVE current assessment did " +
			"not clear its dependents\n" +
			"\tThe planner writes no row for a container it found current, so this " +
			"is the only evidence that distinguishes 'looked, and it is fine' from " +
			"'did not look'. Blocker 1 of Stage 5b.")
	}

	for _, reason := range append(append([]domain.AutomationReason{}, unestablished...), deferred...) {
		if reason == domain.ReasonNoPlan {
			continue
		}
		decision := domain.AutomationDecision{
			Verdict: domain.VerdictSkip,
			Reason:  reason,
		}
		if !needsWork(decision, current) {
			t.Errorf("reason %q was cleared by a current assessment\n"+
				"\tThe assessment explains `noPlan` and nothing else. A container a "+
				"policy excluded, opted out of, paused, or declined must stay held "+
				"however up to date its image is -- otherwise the assessment is a "+
				"general override and the dependency gate has stopped subtracting.",
				reason)
		}
	}

	// A reason this build does not recognise holds, rather than clearing --
	// with or without an assessment. A reason added later gets the safe answer
	// by default in both dimensions.
	invented := domain.AutomationDecision{
		Verdict: domain.VerdictSkip,
		Reason:  domain.AutomationReason("inventedLater"),
	}
	if !needsWork(invented, unknown) {
		t.Fatal("an unrecognised reason cleared an upstream")
	}
	if !needsWork(invented, current) {
		t.Fatal("an unrecognised reason cleared an upstream when an assessment was present")
	}

	// Every verdict that acknowledges an available change is work, whatever the
	// reason beside it says -- including the two that will not act on it, and
	// including when a stale assessment claims the container is current.
	for _, verdict := range []domain.AutomationVerdict{
		domain.VerdictUpdate,
		domain.VerdictWouldUpdate,
		domain.VerdictAwaitingApproval,
	} {
		decision := domain.AutomationDecision{Verdict: verdict, Reason: domain.ReasonNoUpdate}
		if !needsWork(decision, unknown) {
			t.Errorf("verdict %q did not count as work", verdict)
		}
		if !needsWork(decision, current) {
			t.Errorf("verdict %q was cleared by an assessment; a container with a "+
				"change waiting is work whatever an assessment says", verdict)
		}
	}
}

// The decision pass does not evaluate live rebindability.
//
// # The second Stage 4a defect, pinned
//
// The pass once passed HasHardDependents from the graph with ProviderAssessed
// false, on the reasoning that it should report what it could not check. The
// effect was that EVERY namespace provider was blocked on every pass, forever:
// nothing a decision pass does can clear an unassessed provider, because the
// assessment is a live-host question.
//
// So the separation is: the pass decides ORDER from persisted facts, and the
// worker-side execution preflight proves rebindability against the live host
// immediately before StopContainer. This test asserts the pass leaves both
// fields at their zero value, which is what "did not ask" looks like.
func TestTheDecisionPassDoesNotEvaluateLiveRebindability(t *testing.T) {
	t.Parallel()

	// A provider with a hard dependent, and an eligible decision for it.
	graph, err := domain.BuildDependencyGraph(
		[]string{"gluetun", "sonarr"},
		[]domain.WorkloadDependency{{
			Dependent: "sonarr", Dependency: "gluetun",
			Source: domain.DependencyNetworkNamespace,
		}})
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}

	// The inputs the pass builds, with the two invariant-A fields left unset.
	verdict := DecideDependency(DependencyInput{
		Container: "gluetun",
		Verdict:   domain.VerdictUpdate,
		Reason:    domain.ReasonEligible,
		Graph:     graph,
		Facts: map[string]DependencyFact{
			"gluetun": {Present: true, Running: true},
			"sonarr":  {Present: true, Running: true},
		},
	})

	if verdict.Verdict != domain.VerdictUpdate {
		t.Fatalf("a namespace provider was blocked by the decision pass: %q (%s)\n"+
			"\tLive rebindability belongs to the execution preflight. Blocking here "+
			"makes a provider permanently un-updatable, because nothing the pass "+
			"does can ever clear it.", verdict.Verdict, verdict.Detail)
	}
	if verdict.State != domain.DependencySatisfied {
		t.Fatalf("state = %q, want dependencySatisfied", verdict.State)
	}

	// And the gate STILL blocks when the caller does assert an unperformed
	// assessment -- so the check itself is intact, it is simply not the pass's
	// to make.
	asserted := DecideDependency(DependencyInput{
		Container:         "gluetun",
		Verdict:           domain.VerdictUpdate,
		Reason:            domain.ReasonEligible,
		Graph:             graph,
		Facts:             map[string]DependencyFact{"gluetun": {Present: true, Running: true}},
		HasHardDependents: true,
		ProviderAssessed:  false,
	})
	if asserted.Verdict != domain.VerdictSkip {
		t.Fatal("an explicitly unassessed provider was not blocked; the check is gone")
	}
}
