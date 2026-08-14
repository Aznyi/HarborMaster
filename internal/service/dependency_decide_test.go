package service_test

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// The gate can only SUBTRACT.
//
// This is the property the whole phase rests on, and it is the one an operator
// has to be able to trust without reading the code: a dependency relationship is
// not a route into an update policy's scope, past an exclusion, past an opt-out
// label, or past the self-update refusal. It can hold a container back. It can
// never let one through.
//
// Walked EXHAUSTIVELY over the verdict vocabulary crossed with every shape of
// dependency situation, rather than spot-checked, because a single branch that
// wrote VerdictUpdate would be the whole guarantee gone and would look
// unremarkable in a diff.

func graphOf(t *testing.T, nodes []string, edges []domain.WorkloadDependency) domain.DependencyGraph {
	t.Helper()

	graph, err := domain.BuildDependencyGraph(nodes, edges)
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}
	return graph
}

func operatorEdge(dependent, dependency string) domain.WorkloadDependency {
	return domain.WorkloadDependency{
		Dependent: dependent, Dependency: dependency, Source: domain.DependencyOperator,
	}
}

func namespaceEdge(dependent, dependency string) domain.WorkloadDependency {
	return domain.WorkloadDependency{
		Dependent: dependent, Dependency: dependency,
		Source: domain.DependencyNetworkNamespace,
	}
}

// satisfied is a dependency that needs nothing and is up.
var satisfied = service.DependencyFact{Present: true, Running: true}

func TestDependencyGateCanOnlySubtract(t *testing.T) {
	t.Parallel()

	everyVerdict := []domain.AutomationVerdict{
		domain.VerdictUpdate,
		domain.VerdictWouldUpdate,
		domain.VerdictAwaitingApproval,
		domain.VerdictSkip,
		// A verdict this build does not recognise. Included deliberately: the
		// gate must not "upgrade" an unknown into an update either.
		domain.AutomationVerdict("inventedLater"),
	}

	// Every shape of dependency situation the gate can be in.
	situations := map[string]service.DependencyInput{
		"independent": {
			Container: "api",
			Graph:     graphOf(t, []string{"api"}, nil),
			Facts:     map[string]service.DependencyFact{},
		},
		"dependency satisfied": {
			Container: "api",
			Graph: graphOf(t, []string{"api", "postgres"},
				[]domain.WorkloadDependency{operatorEdge("api", "postgres")}),
			Facts: map[string]service.DependencyFact{"postgres": satisfied},
		},
		"dependency still working": {
			Container: "api",
			Graph: graphOf(t, []string{"api", "postgres"},
				[]domain.WorkloadDependency{operatorEdge("api", "postgres")}),
			Facts: map[string]service.DependencyFact{
				"postgres": {Present: true, Running: true, NeedsWork: true, Eligible: true},
			},
		},
		"dependency not permitted to update": {
			Container: "api",
			Graph: graphOf(t, []string{"api", "postgres"},
				[]domain.WorkloadDependency{operatorEdge("api", "postgres")}),
			Facts: map[string]service.DependencyFact{
				"postgres": {Present: true, Running: true, NeedsWork: true, Eligible: false},
			},
		},
		"dependency verified": {
			Container: "api",
			Graph: graphOf(t, []string{"api", "postgres"},
				[]domain.WorkloadDependency{operatorEdge("api", "postgres")}),
			Facts: map[string]service.DependencyFact{
				"postgres": {Present: true, Running: true, NeedsWork: true, Eligible: true, Verified: true},
			},
		},
		"nothing established about the dependency": {
			Container: "api",
			Graph: graphOf(t, []string{"api", "postgres"},
				[]domain.WorkloadDependency{operatorEdge("api", "postgres")}),
			Facts: map[string]service.DependencyFact{},
		},
		"cycle": {
			Container: "api",
			Graph: graphOf(t, []string{"api", "postgres"}, []domain.WorkloadDependency{
				operatorEdge("api", "postgres"), operatorEdge("postgres", "api"),
			}),
			Facts: map[string]service.DependencyFact{"postgres": satisfied},
		},
		"dependency absent from the estate": {
			Container: "api",
			Graph: graphOf(t, []string{"api"},
				[]domain.WorkloadDependency{operatorEdge("api", "postgres")}),
			Facts: map[string]service.DependencyFact{},
		},
		"container not in the graph at all": {
			Container: "api",
			Graph:     graphOf(t, []string{"other"}, nil),
			Facts:     map[string]service.DependencyFact{},
		},
		"discovery could not resolve a namespace": {
			Container: "sonarr",
			Graph:     graphOf(t, []string{"sonarr"}, nil),
			Problems: []domain.DependencyProblem{{
				Container: "sonarr",
				Source:    domain.DependencyNetworkNamespace,
				Refusal:   domain.DiscoveryUnknownContainer,
			}},
			Facts: map[string]service.DependencyFact{},
		},
		"provider whose dependent cannot be rebound": {
			Container: "gluetun",
			Graph: graphOf(t, []string{"gluetun", "sonarr"},
				[]domain.WorkloadDependency{namespaceEdge("sonarr", "gluetun")}),
			Facts:             map[string]service.DependencyFact{},
			HasHardDependents: true,
			ProviderAssessed:  true,
			Provider: domain.ProviderRebindAssessment{
				Provider: "gluetun",
				Blocked: []domain.ProviderBlock{{
					Dependent: "sonarr", Refusal: domain.RebindRefusalDigestUnestablished,
				}},
			},
		},
		"provider never assessed": {
			Container: "gluetun",
			Graph: graphOf(t, []string{"gluetun", "sonarr"},
				[]domain.WorkloadDependency{namespaceEdge("sonarr", "gluetun")}),
			Facts:             map[string]service.DependencyFact{},
			HasHardDependents: true,
			ProviderAssessed:  false,
		},
		"provider whose dependents can all be rebound": {
			Container: "gluetun",
			Graph: graphOf(t, []string{"gluetun", "sonarr"},
				[]domain.WorkloadDependency{namespaceEdge("sonarr", "gluetun")}),
			Facts:             map[string]service.DependencyFact{},
			HasHardDependents: true,
			ProviderAssessed:  true,
			Provider: domain.ProviderRebindAssessment{
				Provider: "gluetun", Rebindable: []string{"sonarr"},
			},
		},
	}

	for name, base := range situations {
		for _, verdict := range everyVerdict {
			t.Run(name+"/"+string(verdict), func(t *testing.T) {
				t.Parallel()

				input := base
				input.Verdict = verdict
				input.Reason = domain.ReasonEligible

				got := service.DecideDependency(input)

				// THE INVARIANT, in two halves.
				//
				// 1. The output verdict is the input verdict or a refusal.
				//    Nothing else is reachable.
				if got.Verdict != verdict && got.Verdict != domain.VerdictSkip {
					t.Fatalf("verdict %q became %q, which is neither the input nor a refusal",
						verdict, got.Verdict)
				}
				// 2. An update can only come OUT of an update going in.
				if got.Verdict == domain.VerdictUpdate && verdict != domain.VerdictUpdate {
					t.Fatalf("verdict %q was promoted to update", verdict)
				}

				// A state is always established, even when the verdict was left
				// alone: an operator asking why something is waiting needs the
				// answer whether or not the gate acted.
				if got.State == domain.DependencyStateInvalid {
					t.Fatal("the gate produced no dependency state")
				}
				if !domain.ValidDependencyState(string(got.State)) {
					t.Fatalf("state %q is not in the vocabulary", got.State)
				}

				// A non-eligible decision keeps the reason it arrived with. The
				// gate makes decisions more restrictive; it does not relabel
				// ones that were already going nowhere.
				if verdict != domain.VerdictUpdate && got.Reason != domain.ReasonEligible {
					t.Fatalf("reason on a %q verdict became %q", verdict, got.Reason)
				}
			})
		}
	}
}

// The positive path, stated separately so a gate that refused everything would
// fail rather than passing the invariant above trivially.
func TestDependencyGateClearsWhenEverythingIsSatisfied(t *testing.T) {
	t.Parallel()

	got := service.DecideDependency(service.DependencyInput{
		Container: "api",
		Verdict:   domain.VerdictUpdate,
		Reason:    domain.ReasonEligible,
		Graph: graphOf(t, []string{"api", "postgres"},
			[]domain.WorkloadDependency{operatorEdge("api", "postgres")}),
		Facts: map[string]service.DependencyFact{"postgres": satisfied},
	})

	if got.Verdict != domain.VerdictUpdate {
		t.Fatalf("verdict = %q, want update", got.Verdict)
	}
	if got.State != domain.DependencySatisfied {
		t.Fatalf("state = %q, want dependencySatisfied", got.State)
	}
}

// "Satisfied" requires VERIFIED success, never an accepted request.
func TestSatisfactionRequiresVerifiedSuccessRatherThanSubmission(t *testing.T) {
	t.Parallel()

	// The dependency needs work, is permitted to do it, and has been submitted
	// -- but has not been verified. That does NOT clear the dependent.
	submitted := service.DependencyFact{
		Present: true, Running: true, NeedsWork: true, Eligible: true, Verified: false,
	}
	if submitted.Satisfies() {
		t.Fatal("a submitted-but-unverified dependency reported as satisfying")
	}

	verified := submitted
	verified.Verified = true
	if !verified.Satisfies() {
		t.Fatal("a verified dependency did not report as satisfying")
	}

	// A container that needs no work satisfies only when it is up.
	if (service.DependencyFact{Present: true}).Satisfies() {
		t.Fatal("a present-but-stopped dependency reported as satisfying")
	}
	if !satisfied.Satisfies() {
		t.Fatal("a present, running, work-free dependency did not satisfy")
	}
	// The zero value establishes nothing and must satisfy nothing.
	if (service.DependencyFact{}).Satisfies() {
		t.Fatal("the zero fact reported as satisfying")
	}
}

// A dependency that is not permitted to update blocks the DEPENDENT and never
// enrols the dependency.
//
// The phase brief's policy-isolation rule, at the level the gate can enforce it:
// the answer is dependencyIneligible on B, and there is no output at all that
// says anything about A.
func TestAnIneligibleDependencyBlocksTheDependentRatherThanEnrollingItself(t *testing.T) {
	t.Parallel()

	got := service.DecideDependency(service.DependencyInput{
		Container: "b",
		Verdict:   domain.VerdictUpdate,
		Reason:    domain.ReasonEligible,
		Graph: graphOf(t, []string{"a", "b"},
			[]domain.WorkloadDependency{operatorEdge("b", "a")}),
		Facts: map[string]service.DependencyFact{
			"a": {Present: true, Running: true, NeedsWork: true, Eligible: false},
		},
	})

	if got.State != domain.DependencyIneligible {
		t.Fatalf("state = %q, want dependencyIneligible", got.State)
	}
	if got.Verdict != domain.VerdictSkip {
		t.Fatalf("verdict = %q, want skip", got.Verdict)
	}
	if got.BlockedBy != "a" {
		t.Fatalf("blockedBy = %q, want a", got.BlockedBy)
	}
	if got.Reason != domain.ReasonDependencyIneligible {
		t.Fatalf("reason = %q", got.Reason)
	}
}

// An upstream that needs no update and is healthy is NOT recreated merely
// because something depends on it.
func TestAStableDependencyIsNotDraggedIntoAnUpdate(t *testing.T) {
	t.Parallel()

	got := service.DecideDependency(service.DependencyInput{
		Container: "sonarr",
		Verdict:   domain.VerdictUpdate,
		Reason:    domain.ReasonEligible,
		Graph: graphOf(t, []string{"sonarr", "gluetun"},
			[]domain.WorkloadDependency{namespaceEdge("sonarr", "gluetun")}),
		Facts: map[string]service.DependencyFact{"gluetun": satisfied},
	})

	if got.Verdict != domain.VerdictUpdate || got.State != domain.DependencySatisfied {
		t.Fatalf("verdict = %q, state = %q; a healthy provider should simply clear",
			got.Verdict, got.State)
	}
}

// Every dependency reason maps back to a state, and the mapping is total.
func TestDependencyReasonMappingIsTotal(t *testing.T) {
	t.Parallel()

	for _, state := range domain.DependencyStates {
		reason := domain.DependencyReasonFor(state)
		if reason == "" {
			t.Errorf("state %q has no reason", state)
		}
		if reason.Explain() == "" {
			t.Errorf("reason %q has no explanation", reason)
		}
	}
	// An unrecognised state maps to blocked, not to anything permissive.
	if got := domain.DependencyReasonFor(domain.DependencyState("inventedLater")); got != domain.ReasonDependencyBlocked {
		t.Fatalf("an unrecognised state mapped to %q", got)
	}
}
