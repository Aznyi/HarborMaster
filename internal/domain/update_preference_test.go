package domain_test

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Per-container update behaviour (C2), at the layer that decides it.
//
// # The one property everything else rests on
//
// A preference may only make automation SAFER. It narrows the governing
// policy's mode and can never widen it, so the worst a per-container choice can
// do is stop something from happening.
//
// That asymmetry is what keeps C2 a presentation of the existing engine rather
// than a second authorisation path. If a preference could raise a ceiling, the
// question "may this container be updated without asking" would have two
// answers, and the one an operator read would not always be the one that ran.

func policyIn(mode domain.AutomationMode) domain.UpdatePolicy {
	p := domain.UpdatePolicy{
		PolicyID:              domain.NewUpdatePolicyID(),
		Name:                  "governing rule",
		Enabled:               true,
		Scope:                 domain.ScopeSelector,
		Selector:              domain.UpdateSelector{Include: []string{"web"}},
		Strategy:              domain.StrategyMinor,
		MinimumRecommendation: domain.RecommendProceed,
		Mode:                  mode,
		Window:                domain.MaintenanceWindow{AlwaysOpen: true},
	}
	p.Normalise()
	return p
}

// ---------------------------------------------------- the narrowing rule --

func TestAPreferenceMayNarrowTheModeAndNeverWidenIt(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		policy     domain.AutomationMode
		preference domain.UpdateBehavior
		want       domain.AutomationMode
		applied    bool
	}{
		// Narrowing: the preference wins, because it asks for less.
		{"automatic policy, review first", domain.ModeAutomatic, domain.BehaviorReviewFirst, domain.ModeApprove, true},
		{"automatic policy, monitor only", domain.ModeAutomatic, domain.BehaviorMonitorOnly, domain.ModeObserve, true},
		{"approval policy, monitor only", domain.ModeApprove, domain.BehaviorMonitorOnly, domain.ModeObserve, true},

		// WIDENING: the policy wins, every time. This is the case an interface
		// must never render as though the operator got what they picked.
		{"approval policy, automatic requested", domain.ModeApprove, domain.BehaviorAutomatic, domain.ModeApprove, false},
		{"observe policy, automatic requested", domain.ModeObserve, domain.BehaviorAutomatic, domain.ModeObserve, false},
		{"observe policy, review first requested", domain.ModeObserve, domain.BehaviorReviewFirst, domain.ModeObserve, false},

		// Same: nothing to narrow, so nothing is claimed to have been applied.
		{"automatic policy, automatic requested", domain.ModeAutomatic, domain.BehaviorAutomatic, domain.ModeAutomatic, false},
		{"approval policy, review first requested", domain.ModeApprove, domain.BehaviorReviewFirst, domain.ModeApprove, false},

		// No preference at all behaves exactly like `automatic`.
		{"no preference", domain.ModeAutomatic, "", domain.ModeAutomatic, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			effective := domain.Resolve(policyIn(testCase.policy), nil, testCase.preference)

			if effective.Mode != testCase.want {
				t.Errorf("mode = %q, want %q", effective.Mode, testCase.want)
			}
			if effective.PreferenceApplied != testCase.applied {
				t.Errorf("preferenceApplied = %v, want %v", effective.PreferenceApplied, testCase.applied)
			}
			// The preference is always carried for attribution, applied or not:
			// the interface has to be able to say "you asked for X".
			if effective.Preference != testCase.preference {
				t.Errorf("preference = %q, want it carried unmodified", effective.Preference)
			}
		})
	}
}

func TestAPreferenceCanNeverMakeAContainerMutable(t *testing.T) {
	// The sharpest statement of the rule: for every policy mode and every
	// behaviour, the result never permits more than the policy did.
	rank := map[domain.AutomationMode]int{
		domain.ModeObserve: 0, domain.ModeDryRun: 1,
		domain.ModeApprove: 2, domain.ModeAutomatic: 3,
	}
	for _, mode := range domain.AutomationModes {
		for _, behavior := range append(domain.UpdateBehaviors, "") {
			effective := domain.Resolve(policyIn(mode), nil, behavior)
			if rank[effective.Mode] > rank[mode] {
				t.Errorf("policy %q with preference %q resolved to %q, which permits MORE",
					mode, behavior, effective.Mode)
			}
		}
	}
}

// ------------------------------------------------------ label precedence --

func TestAContainerLabelStillOutranksThePreference(t *testing.T) {
	// The label is set where the container is defined and is the operator's
	// most explicit statement. A dropdown may not overrule it.
	effective := domain.Resolve(
		policyIn(domain.ModeAutomatic),
		map[string]string{domain.LabelUpdateEnabled: "false"},
		domain.BehaviorAutomatic,
	)
	if !effective.Disabled {
		t.Fatal("a UI preference overrode io.harbormaster.update.enabled=false")
	}
	if effective.Reason == "" {
		t.Error("the refusal does not say which clause decided it")
	}
}

func TestThePauseLabelAlsoOutranksThePreference(t *testing.T) {
	effective := domain.Resolve(
		policyIn(domain.ModeAutomatic),
		map[string]string{domain.LabelUpdatePause: "true"},
		domain.BehaviorAutomatic,
	)
	if !effective.Disabled {
		t.Fatal("a UI preference overrode io.harbormaster.update.pause=true")
	}
}

// -------------------------------------------------------- the vocabulary --

func TestEachBehaviorMapsOntoAnExistingMode(t *testing.T) {
	// None of these is a new concept. Each names a mode the engine already has,
	// which is what stops C2 inventing a second set of semantics.
	if got := domain.BehaviorReviewFirst.Mode(); got != domain.ModeApprove {
		t.Errorf("reviewFirst maps to %q, want approvalRequired", got)
	}
	if got := domain.BehaviorMonitorOnly.Mode(); got != domain.ModeObserve {
		t.Errorf("monitorOnly maps to %q, want observe", got)
	}
	// Automatic imposes NO cap; it is not a mode.
	if got := domain.BehaviorAutomatic.Mode(); got != "" {
		t.Errorf("automatic maps to %q, want no cap at all", got)
	}
}

func TestTheVocabularyIsClosed(t *testing.T) {
	for _, valid := range domain.UpdateBehaviors {
		if !domain.ValidUpdateBehavior(string(valid)) {
			t.Errorf("%q is listed but not accepted", valid)
		}
	}
	for _, invalid := range []string{"", "excluded", "off", "AUTOMATIC", "automatic ", "../../etc"} {
		if domain.ValidUpdateBehavior(invalid) {
			t.Errorf("%q was accepted", invalid)
		}
		// And a caller-supplied value never becomes a silent restriction.
		if got := domain.NormaliseUpdateBehavior(invalid); got != "" && !domain.ValidUpdateBehavior(string(got)) {
			t.Errorf("normalising %q produced %q, which is not in the vocabulary", invalid, got)
		}
	}
	// Whitespace is trimmed rather than refused, which is the ordinary courtesy
	// every other normaliser in this package extends.
	if got := domain.NormaliseUpdateBehavior("  monitorOnly  "); got != domain.BehaviorMonitorOnly {
		t.Errorf("normalise trimmed to %q", got)
	}
}

func TestThereIsNoSeparateExcludedBehavior(t *testing.T) {
	// "Excluded" would be monitorOnly under a second name. Manufacturing it
	// would give an operator two controls for one state, and the version that
	// stopped WATCHING would remove the container from the Updates workspace --
	// so they would stop being told a critical patch was waiting.
	if domain.ValidUpdateBehavior("excluded") {
		t.Error("an `excluded` behaviour exists; it duplicates monitorOnly")
	}
	if len(domain.UpdateBehaviors) != 3 {
		t.Errorf("vocabulary = %v, want exactly three", domain.UpdateBehaviors)
	}
}
