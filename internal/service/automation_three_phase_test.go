package service_test

import (
	"context"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// The three-phase pass, and the layering it depends on.
//
//	Phase 1  DecideAutomation for every target
//	Phase 2  the dependency gate
//	Phase 3  stage-ordered submission
//
// The value of the split is that each phase can only do one thing. Phase 1
// decides WHETHER, phase 2 may only restrict that, and phase 3 decides WHEN.
// These tests pin the boundaries between them, because a leak in either
// direction is invisible in ordinary behaviour and changes what HarborMaster is
// willing to do.

// D. Ordering changes WHEN a submission happens and nothing else.
//
// # Why this is worth its own test
//
// The stage sorter reorders a slice of decisions. A sorter that also touched
// the decisions it was reordering would be a plan transformer wearing a
// scheduler's clothes -- and the thing it would be transforming is the digest a
// container gets recreated on.
//
// So the SAME estate is evaluated under two different graphs, and every
// submitted request is compared field by field.
func TestDependencyOrderingDoesNotMutatePlans(t *testing.T) {
	t.Parallel()

	names := []string{"api", "postgres"}

	// Two graphs that produce OPPOSITE submission orders.
	forward := graphOver(t, names, operatorDep("api", "postgres"))
	reverse := graphOver(t, names, operatorDep("postgres", "api"))

	// What each ordering actually submitted, keyed by container.
	submittedUnder := func(view dependencyView) map[string]domain.AutomationDecision {
		harness := newAutomationHarness(t, broadPolicy())
		withDependencyEstate(t, harness, view, names, nil)

		_, decisions, err := harness.engine.RunNow(
			context.Background(), false, domain.Requester{UserID: "usr_1", Username: "ada"})
		if err != nil {
			t.Fatalf("pass: %v", err)
		}

		byPlan := make(map[string]domain.AutomationDecision, len(decisions))
		for _, decision := range decisions {
			if decision.PlanID != "" {
				byPlan[decision.PlanID] = decision
			}
		}

		out := make(map[string]domain.AutomationDecision)
		for _, request := range harness.pipeline.recorded("acquire") {
			decision, ok := byPlan[request.id]
			if !ok {
				t.Fatalf("a request named plan %q, which no decision carries", request.id)
			}
			// The REQUEST carried the plan id and nothing else -- the safety
			// property the whole pipeline rests on.
			if request.id != decision.PlanID {
				t.Fatalf("request id %q != decision plan id %q", request.id, decision.PlanID)
			}
			if request.by.Username != "ada" {
				t.Fatalf("attribution = %+v, want the requesting account", request.by)
			}
			out[decision.ContainerName] = decision
		}
		return out
	}

	first := submittedUnder(forward)
	second := submittedUnder(reverse)

	// POSITIVE CONTROL: the two orderings really did submit different
	// containers. Without this, two empty maps would compare equal.
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("orderings submitted %d and %d containers, want one each", len(first), len(second))
	}
	var firstName, secondName string
	for name := range first {
		firstName = name
	}
	for name := range second {
		secondName = name
	}
	if firstName == secondName {
		t.Fatalf("both orderings submitted %q; the graphs did not change the order",
			firstName)
	}

	// THE assertion: whichever container was submitted, its plan content is
	// exactly what the planner produced for it.
	for name, decision := range mergeDecisions(first, second) {
		expected := planFor(name)
		if decision.PlanID != expected.PlanID {
			t.Errorf("%s: planId = %q, want %q", name, decision.PlanID, expected.PlanID)
		}
		if decision.ProposedImage != expected.ProposedImage {
			t.Errorf("%s: proposedImage = %q, want %q",
				name, decision.ProposedImage, expected.ProposedImage)
		}
		if decision.ProposedDigest != expected.ProposedDigest {
			t.Errorf("%s: proposedDigest = %q, want %q",
				name, decision.ProposedDigest, expected.ProposedDigest)
		}
		if decision.UpdateType != expected.UpdateType {
			t.Errorf("%s: updateType = %q, want %q",
				name, decision.UpdateType, expected.UpdateType)
		}
		if decision.Recommendation != expected.Risk.Recommendation {
			t.Errorf("%s: recommendation = %q, want %q",
				name, decision.Recommendation, expected.Risk.Recommendation)
		}
		if decision.PolicyID != broadPolicy().PolicyID {
			t.Errorf("%s: policyId = %q, want the governing policy", name, decision.PolicyID)
		}
	}
}

func mergeDecisions(sets ...map[string]domain.AutomationDecision) map[string]domain.AutomationDecision {
	merged := make(map[string]domain.AutomationDecision)
	for _, set := range sets {
		for name, decision := range set {
			merged[name] = decision
		}
	}
	return merged
}

// E. Dependency metadata is explanatory and restrictive, never executable.
//
// Every non-executable verdict is run under the MOST permissive dependency
// arrangement there is -- a graph in which everything is satisfied -- and must
// still submit nothing.
func TestDependencyMetadataCannotMakeADecisionExecutable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		policy domain.UpdatePolicy
		// unplanned makes the container have no proposed change.
		unplanned bool
	}{
		{name: "observe mode", policy: observePolicy()},
		{name: "approval held", policy: approvalPolicy()},
		{name: "nothing proposed", policy: broadPolicy(), unplanned: true},
		{name: "no policy governs it", policy: unrelatedPolicy()},
		{name: "maintenance window closed", policy: closedWindowPolicy()},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			harness := newAutomationHarness(t, testCase.policy)
			// A graph in which every dependency is satisfied.
			view := graphOver(t, []string{"web"})
			if testCase.unplanned {
				withDependencyEstate(t, harness, view, nil, []string{"web"})
			} else {
				withDependencyEstate(t, harness, view, []string{"web"}, nil)
			}

			run, decisions, err := harness.engine.RunNow(
				context.Background(), false, domain.Requester{})
			if err != nil {
				t.Fatalf("pass: %v", err)
			}

			if run.Submitted != 0 {
				t.Fatalf("run.Submitted = %d, want 0", run.Submitted)
			}
			if got := submittedNames(t, harness, decisions); len(got) != 0 {
				t.Fatalf("submitted %v under a fully satisfied graph", got)
			}

			decision := decisionFor(decisions, "web")
			if decision.Verdict == domain.VerdictUpdate {
				t.Fatalf("verdict = update; dependency metadata made a "+
					"non-executable decision executable (state %q)",
					decision.DependencyState)
			}
			// The container WAS considered -- otherwise this passes because the
			// policy governs nothing, which is a different test.
			if run.Considered != 1 {
				t.Fatalf("considered = %d, want 1", run.Considered)
			}
		})
	}
}

// dependencySatisfied on a non-update verdict does not promote it.
//
// The narrowest form of the same claim, at the gate itself: the most positive
// dependency state there is cannot turn any other verdict into an update.
func TestSatisfiedDependencyStateNeverPromotesAVerdict(t *testing.T) {
	t.Parallel()

	graph, err := domain.BuildDependencyGraph([]string{"web"}, nil)
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}

	for _, verdict := range []domain.AutomationVerdict{
		domain.VerdictWouldUpdate,
		domain.VerdictAwaitingApproval,
		domain.VerdictSkip,
		domain.AutomationVerdict("inventedLater"),
	} {
		got := service.DecideDependency(service.DependencyInput{
			Container: "web",
			Verdict:   verdict,
			Reason:    domain.ReasonObserveMode,
			Graph:     graph,
			Facts: map[string]service.DependencyFact{
				"web": {Present: true, Running: true},
			},
		})

		if got.State != domain.DependencySatisfied {
			t.Fatalf("%q: state = %q, want dependencySatisfied", verdict, got.State)
		}
		if got.Verdict == domain.VerdictUpdate {
			t.Fatalf("%q was promoted to update by a satisfied dependency state", verdict)
		}
		if got.Verdict != verdict {
			t.Fatalf("%q became %q; a satisfied gate must leave the verdict alone",
				verdict, got.Verdict)
		}
	}
}

// ------------------------------------------------------------- fixtures --

// unrelatedPolicy governs a container that is not in the estate.
func unrelatedPolicy() domain.UpdatePolicy {
	policy := automaticPolicy()
	policy.Selector = domain.UpdateSelector{Include: []string{"something-else"}}
	policy.Normalise()
	return policy
}

// closedWindowPolicy never has an open maintenance window at the test clock.
func closedWindowPolicy() domain.UpdatePolicy {
	policy := broadPolicy()
	// decideAt sits inside 02:00-04:00, which automaticPolicy uses. This one is
	// deliberately elsewhere.
	policy.Window = domain.MaintenanceWindow{Start: "12:00", End: "13:00"}
	policy.Normalise()
	policy.Limits = domain.UpdateLimits{MaxConcurrent: 20, MaxPerRun: 20}
	return policy
}
