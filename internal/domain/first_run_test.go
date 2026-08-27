package domain_test

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The first-run projection.
//
// # The invariant these defend
//
//	"assessment pending" is never rendered as "nothing needs updating",
//	and neither is ever rendered as "HarborMaster could not tell".
//
// Those three are the states an operator most needs told apart, and the
// cheapest way to get them wrong is to reach for a count before the question
// has been asked.

// ready is a fully working installation. Each test breaks one thing.
func ready() domain.FirstRunFacts {
	return domain.FirstRunFacts{
		Features: domain.Features{
			Inventory: true, Planner: true, Snapshots: true,
			Acquisition: true, Execution: true, Rollback: true, Automation: true,
		},
		InventoryEstablished: true,
		Assessed:             true,
		Policies:             1,
		ActingPolicies:       1,
		Eligible:             3,
		ReadinessKnown:       true,
	}
}

func TestTheFirstRunStatesAreDistinguishable(t *testing.T) {
	cases := []struct {
		name    string
		breakIt func(*domain.FirstRunFacts)
		want    domain.FirstRunState
	}{
		{"nothing is known yet", func(f *domain.FirstRunFacts) {
			f.InventoryEstablished = false
		}, domain.FirstRunInventoryPending},

		{"planning is switched off", func(f *domain.FirstRunFacts) {
			f.Features.Planner = false
		}, domain.FirstRunAssessmentUnavailable},

		{"the first pass has not finished", func(f *domain.FirstRunFacts) {
			f.Assessed = false
		}, domain.FirstRunAssessmentPending},

		{"the engine is off", func(f *domain.FirstRunFacts) {
			f.Features.Automation = false
		}, domain.FirstRunEngineDisabled},

		{"nothing tells it what to do", func(f *domain.FirstRunFacts) {
			f.Policies, f.ActingPolicies = 0, 0
		}, domain.FirstRunNoPolicy},

		{"every policy only watches", func(f *domain.FirstRunFacts) {
			f.ActingPolicies = 0
		}, domain.FirstRunObserveOnly},

		{"a container is paused", func(f *domain.FirstRunFacts) {
			f.PausedContainers = 2
		}, domain.FirstRunNeedsAttention},

		{"a plan needs a person", func(f *domain.FirstRunFacts) {
			f.ManualReviews = 1
		}, domain.FirstRunNeedsAttention},

		{"readiness could not be established", func(f *domain.FirstRunFacts) {
			f.ReadinessKnown = false
		}, domain.FirstRunUnknown},

		{"nothing currently qualifies", func(f *domain.FirstRunFacts) {
			f.Eligible = 0
		}, domain.FirstRunNothingEligible},

		{"everything is working", func(*domain.FirstRunFacts) {}, domain.FirstRunActive},
	}

	seen := map[domain.FirstRunState]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := ready()
			tc.breakIt(&facts)

			got := domain.DescribeFirstRun(facts)
			if got != tc.want {
				t.Fatalf("state = %q, want %q", got, tc.want)
			}
			if got.Explain() == "" {
				t.Fatal("every state must carry HarborMaster's own sentence")
			}
			seen[got] = true
		})
	}

	// Non-vacuity: the table reaches every state, so none can rot unnoticed.
	for _, state := range []domain.FirstRunState{
		domain.FirstRunInventoryPending, domain.FirstRunAssessmentPending,
		domain.FirstRunAssessmentUnavailable, domain.FirstRunEngineDisabled,
		domain.FirstRunNoPolicy, domain.FirstRunObserveOnly,
		domain.FirstRunNeedsAttention, domain.FirstRunNothingEligible,
		domain.FirstRunActive, domain.FirstRunUnknown,
	} {
		if !seen[state] {
			t.Errorf("no case reaches %q", state)
		}
	}
}

// TestAnUnassessedEstateIsNeverReportedAsSettled is the §4 invariant.
//
// Whatever the readiness count happens to say -- including zero, which is what
// an empty plan table produces -- an estate nobody has assessed must report as
// PENDING. Getting this wrong tells an operator their containers are up to date
// when nothing has looked at them.
func TestAnUnassessedEstateIsNeverReportedAsSettled(t *testing.T) {
	for _, eligible := range []int{0, 1, 50} {
		facts := ready()
		facts.Assessed = false
		facts.Eligible = eligible
		facts.ReadinessKnown = true

		got := domain.DescribeFirstRun(facts)
		if got != domain.FirstRunAssessmentPending {
			t.Fatalf("eligible=%d gave %q; an unassessed estate is always pending",
				eligible, got)
		}
		if got == domain.FirstRunNothingEligible {
			t.Fatal("an unassessed estate was reported as settled")
		}
	}
}

// TestAFailedReadinessIsNeverZero is the other half.
func TestAFailedReadinessIsNeverZero(t *testing.T) {
	facts := ready()
	facts.ReadinessKnown = false
	facts.Eligible = 0

	if got := domain.DescribeFirstRun(facts); got != domain.FirstRunUnknown {
		t.Fatalf("state = %q, want %q: a check that failed is not a count of zero",
			got, domain.FirstRunUnknown)
	}
}

// TestADisabledEngineIsNotObserveMode keeps two very different remedies apart.
//
// Observe mode is a POLICY choice an operator made and can change in the UI.
// A disabled engine is a DEPLOYMENT setting they cannot. Reporting one as the
// other sends them to the wrong place.
func TestADisabledEngineIsNotObserveMode(t *testing.T) {
	disabled := ready()
	disabled.Features.Automation = false

	observing := ready()
	observing.ActingPolicies = 0

	if domain.DescribeFirstRun(disabled) == domain.DescribeFirstRun(observing) {
		t.Fatal("a disabled engine and an observe-only policy set report the same state")
	}
	// And an operator with an ACTING policy on a disabled engine is still told
	// about the engine, not congratulated on the policy.
	if got := domain.DescribeFirstRun(disabled); got != domain.FirstRunEngineDisabled {
		t.Fatalf("state = %q, want %q", got, domain.FirstRunEngineDisabled)
	}
}

// TestOnlyOperatorActionableStatesAskForSetup keeps the dashboard honest.
func TestOnlyOperatorActionableStatesAskForSetup(t *testing.T) {
	wants := map[domain.FirstRunState]bool{
		domain.FirstRunEngineDisabled: true,
		domain.FirstRunNoPolicy:       true,

		// Waiting on HarborMaster, not on the operator. An item telling them to
		// act here asks them to fix something that is not broken.
		domain.FirstRunInventoryPending:  false,
		domain.FirstRunAssessmentPending: false,
		domain.FirstRunUnknown:           false,
		// Working as configured.
		domain.FirstRunObserveOnly:     false,
		domain.FirstRunNothingEligible: false,
		domain.FirstRunActive:          false,
		domain.FirstRunNeedsAttention:  false,
		// A deployment decision, surfaced in Settings rather than as setup.
		domain.FirstRunAssessmentUnavailable: false,
	}
	for state, want := range wants {
		if got := state.NeedsSetup(); got != want {
			t.Errorf("%q.NeedsSetup() = %v, want %v", state, got, want)
		}
	}
}

// TestTheCapabilityListIsOneAnOperatorCanActuallyApply is the regression.
//
// # The defect this caught
//
// Rollback was originally required only when some policy asked for automatic
// rollback, reasoning that nobody should be told to enable a capability their
// policies never use. But config validation refuses to START with automation
// enabled and rollback disabled, whatever any policy says.
//
// So a deployment with acquisition, execution and automation on and rollback
// off was reported as needing nothing -- while being a combination the process
// refuses to boot into. Onboarding would print those exact variables, an
// operator would apply them and recreate the container, and HarborMaster would
// not come back.
//
// A capability list is only useful if applying ALL of it produces a process
// that starts.
func TestTheCapabilityListIsOneAnOperatorCanActuallyApply(t *testing.T) {
	// The combination config refuses to start on.
	refusedAtStartup := domain.Features{
		Acquisition: true, Execution: true, Automation: true, Rollback: false,
	}

	missing := domain.MissingForAutomation(refusedAtStartup, domain.RequiredForAutomation())
	if len(missing) != 1 || missing[0] != "rollback" {
		t.Fatalf("missing = %v, want [rollback]\n"+
			"\tAUTOMATION_ENABLED without ROLLBACK_ENABLED is refused at startup; "+
			"reporting it as complete hands an operator instructions that stop "+
			"HarborMaster from booting", missing)
	}

	// And the whole set really is satisfiable.
	complete := domain.Features{
		Acquisition: true, Execution: true, Automation: true, Rollback: true,
	}
	if missing := domain.MissingForAutomation(
		complete, domain.RequiredForAutomation()); len(missing) != 0 {
		t.Fatalf("missing = %v; the complete set must report nothing missing", missing)
	}
}

func TestMissingCapabilitiesAreNamedInAFixedOrder(t *testing.T) {
	missing := domain.MissingForAutomation(
		domain.Features{}, domain.RequiredForAutomation())

	want := []string{"acquisition", "execution", "automation", "rollback"}
	if len(missing) != len(want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}
	for i := range want {
		if missing[i] != want[i] {
			t.Fatalf("missing = %v, want %v", missing, want)
		}
	}
}
