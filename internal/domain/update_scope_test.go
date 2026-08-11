package domain

import (
	"errors"
	"testing"
)

// The allEligible scope, and the distinction the whole feature rests on.
//
// Broad SELECTION is not broad AUTHORISATION, and "eligible" is not "present".
// The tests below are the evidence for both claims. They are deliberately
// written against the domain functions rather than through a service, because
// these are the properties that must hold no matter which caller asks.

// screened is a container that passed the eligibility screening: an ordinary
// workload with a usable name and image.
func screened(name, image string, labels map[string]string) SelectionTarget {
	return SelectionTarget{
		Name:        name,
		Image:       image,
		Labels:      labels,
		Eligibility: ScreenTarget(name, image, labels),
	}
}

// broadPolicy is a policy in the broad scope, in force.
func broadPolicy(id string) UpdatePolicy {
	return UpdatePolicy{
		PolicyID: id,
		Name:     id,
		Enabled:  true,
		Scope:    ScopeAllEligible,
		Strategy: StrategyPatch,
		Mode:     ModeObserve,
	}
}

// ---------------------------------------------------- what it does select --

func TestAllEligibleSelectsAnOrdinaryWorkload(t *testing.T) {
	policy := broadPolicy("upd_aaaaaaaaaaaaaaaaaaaa")
	target := screened("web", "nginx:1.27", nil)

	if !policy.Governs(target, SelfIdentity{}) {
		t.Fatal("an ordinary screened workload must be selected by the broad scope")
	}
}

// --------------------------------------------- what it must never select --

// The list from the phase brief, one subtest each. Every one of these is a
// container that EXISTS, so each is also a proof that existence alone is not
// eligibility.
func TestAllEligibleRefusesContainersItMayNotEnrol(t *testing.T) {
	self := SelfIdentity{
		ContainerID:   "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ContainerName: "harbormaster",
		ImageRef:      "ghcr.io/aznyi/harbormaster:0.9.0-beta.1",
	}

	cases := []struct {
		name   string
		target SelectionTarget
		want   string
	}{
		{
			name:   "HarborMaster's own container, by name",
			target: screened("harbormaster", "ghcr.io/aznyi/harbormaster:0.9.0-beta.1", nil),
			want:   "this is the container HarborMaster is running in",
		},
		{
			name: "a second copy of HarborMaster, by repository",
			// A different container, a different tag, the same software. It
			// would be the same mistake to update it from inside the first.
			target: screened("harbormaster-standby", "ghcr.io/aznyi/harbormaster:0.8.0", nil),
			want:   "this is the container HarborMaster is running in",
		},
		{
			name: "a container labelled as HarborMaster",
			target: screened("mystery", "example.com/app:1", map[string]string{
				LabelSelfIdentity: "true",
			}),
			want: "this is the container HarborMaster is running in",
		},
		{
			name:   "the parked original of an earlier update",
			target: screened("web.hm-old-exec_0123456789abcdef0123", "nginx:1.26", nil),
			want:   "parked original",
		},
		{
			name:   "the quarantined replacement of a failed update",
			target: screened("web.hm-failed-exec_0123456789abcdef0123", "nginx:1.27", nil),
			want:   "quarantined",
		},
		{
			name: "a container opted out estate-wide",
			target: screened("legacy", "example.com/legacy:1", map[string]string{
				LabelHarborMasterEnabled: "false",
			}),
			want: LabelHarborMasterEnabled + "=false",
		},
		{
			name:   "a workload with no image reference",
			target: screened("ghost", "", nil),
			want:   "could recreate",
		},
		{
			name: "a container nobody screened",
			// The zero TargetEligibility. This is the failure mode the positive
			// facts exist for: a caller that built a target by hand, or a
			// repository that could not read the labels, must produce a target
			// the broad scope declines rather than one it waves through.
			target: SelectionTarget{Name: "web", Image: "nginx:1.27"},
			want:   "could recreate",
		},
	}

	policy := broadPolicy("upd_aaaaaaaaaaaaaaaaaaaa")
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if policy.Governs(testCase.target, self) {
				t.Fatal("the broad scope must not select this container")
			}
			selectable, why := testCase.target.BroadlySelectable(self)
			if selectable {
				t.Fatal("BroadlySelectable must agree with Governs")
			}
			if !contains(why, testCase.want) {
				t.Fatalf("reason %q does not explain %q", why, testCase.want)
			}
		})
	}
}

// A named container is an operator pointing at one, and the selector scope
// honours that. The self-update refusal is not the selector's job -- it is
// applied afterwards, three more times, and this test exists so that a future
// reader does not "fix" the selector and believe they have added protection.
func TestSelectorScopeStillReachesHarborMasterAndIsRefusedLater(t *testing.T) {
	self := SelfIdentity{ContainerName: "harbormaster"}
	policy := UpdatePolicy{
		PolicyID: "upd_aaaaaaaaaaaaaaaaaaaa",
		Name:     "explicit",
		Enabled:  true,
		Scope:    ScopeSelector,
		Selector: UpdateSelector{Include: []string{"harbormaster"}},
	}
	target := screened("harbormaster", "ghcr.io/aznyi/harbormaster:1", nil)

	if !policy.Governs(target, self) {
		t.Fatal("an explicit selector reaches the container it names")
	}
	// And the refusal that actually protects it is the decision function's, not
	// this one's. Proved in the service package by
	// TestDecideRefusesSelfEvenUnderAllEligible.
}

// ------------------------------------------------------------- exclusion --

func TestExclusionOutranksTheBroadScope(t *testing.T) {
	policy := broadPolicy("upd_aaaaaaaaaaaaaaaaaaaa")
	policy.Selector.Exclude = []string{"database"}

	if policy.Governs(screened("database", "postgres:16", nil), SelfIdentity{}) {
		t.Fatal("an excluded container must never be selected, in any scope")
	}
	if !policy.Governs(screened("web", "nginx:1.27", nil), SelfIdentity{}) {
		t.Fatal("excluding one container must not exclude the others")
	}
}

func TestExclusionIsCaseInsensitiveAndTrimmed(t *testing.T) {
	policy := broadPolicy("upd_aaaaaaaaaaaaaaaaaaaa")
	policy.Selector.Exclude = []string{"  DataBase  "}

	if policy.Governs(screened("database", "postgres:16", nil), SelfIdentity{}) {
		t.Fatal("exclusion must match the way the selector always has")
	}
}

// ----------------------------------------------------------- fail closed --

func TestUnknownScopeGovernsNothing(t *testing.T) {
	policy := broadPolicy("upd_aaaaaaaaaaaaaaaaaaaa")
	policy.Scope = "everythingEverywhere"
	policy.Selector = UpdateSelector{Include: []string{"web"}}

	if policy.Governs(screened("web", "nginx:1.27", nil), SelfIdentity{}) {
		t.Fatal("a scope this build does not understand must govern nothing")
	}
}

// The empty scope is the one value that is NOT an unknown scope: it means the
// policy predates the field, and the narrow reading is what it always had.
func TestEmptyScopeIsTheNarrowScope(t *testing.T) {
	policy := UpdatePolicy{
		PolicyID: "upd_aaaaaaaaaaaaaaaaaaaa",
		Name:     "legacy",
		Enabled:  true,
		Selector: UpdateSelector{Include: []string{"web"}},
	}
	if policy.Scope != "" {
		t.Fatal("this fixture is meant to have no scope at all")
	}
	if !policy.Governs(screened("web", "nginx:1.27", nil), SelfIdentity{}) {
		t.Fatal("a policy with no scope keeps the breadth its selector gave it")
	}
	if policy.Governs(screened("api", "nginx:1.27", nil), SelfIdentity{}) {
		t.Fatal("and it must not have become broad")
	}
}

// An empty selector with no scope governs nothing. The single most important
// backward-compatibility property: the old "safe empty" reading survives.
func TestEmptySelectorWithNoScopeStillGovernsNothing(t *testing.T) {
	policy := UpdatePolicy{
		PolicyID: "upd_aaaaaaaaaaaaaaaaaaaa",
		Name:     "empty",
		Enabled:  true,
	}
	if policy.Governs(screened("web", "nginx:1.27", nil), SelfIdentity{}) {
		t.Fatal("an empty selector governs nothing, and always did")
	}
}

// ------------------------------------------------------------ no promotion --

// A label may move a container OUT of the broad scope and may never move one
// in. The mirror of the rule ParseUpdateLabels enforces for the policy
// overrides, and the reason neither is a second authorisation system: labels
// are written by anyone who can run `docker run`.
func TestNoLabelCanPromoteAContainerIntoScope(t *testing.T) {
	policy := UpdatePolicy{
		PolicyID: "upd_aaaaaaaaaaaaaaaaaaaa",
		Name:     "narrow",
		Enabled:  true,
		Scope:    ScopeSelector,
		Selector: UpdateSelector{Include: []string{"web"}},
	}

	hopeful := screened("attacker", "example.com/evil:1", map[string]string{
		LabelHarborMasterEnabled: "true",
		LabelUpdateEnabled:       "true",
		LabelSelfIdentity:        "false",
		"io.harbormaster.scope":  "allEligible",
	})
	if policy.Governs(hopeful, SelfIdentity{}) {
		t.Fatal("no label may enrol a container the policy does not name")
	}

	// And under the broad scope, only the negative direction is honoured: a
	// container that opts out stays out, and one that "opts in" gains nothing
	// it did not already have from the scope itself.
	broad := broadPolicy("upd_bbbbbbbbbbbbbbbbbbbb")
	optedOut := screened("quiet", "example.com/app:1", map[string]string{
		LabelHarborMasterEnabled: "false",
	})
	if broad.Governs(optedOut, SelfIdentity{}) {
		t.Fatal("an opt-out label must be honoured by the broad scope")
	}
}

// An unreadable value is not an opt-out. A typo in a safety control must not
// silently remove a container from a policy an operator believes covers it --
// the same reasoning ParseUpdateLabels applies to its Invalid list.
func TestUnreadableOptOutLabelIsNotAnOptOut(t *testing.T) {
	for _, value := range []string{"perhaps", "", "FALSEY", "2"} {
		eligibility := ScreenTarget("web", "nginx:1.27", map[string]string{
			LabelHarborMasterEnabled: value,
		})
		if eligibility.OptedOut {
			t.Fatalf("%q must not read as an opt-out", value)
		}
	}
	for _, value := range []string{"false", "FALSE", " 0 ", "no", "off"} {
		eligibility := ScreenTarget("web", "nginx:1.27", map[string]string{
			LabelHarborMasterEnabled: value,
		})
		if !eligibility.OptedOut {
			t.Fatalf("%q must read as an opt-out", value)
		}
	}
}

// --------------------------------------------------------- policy choice --

// Adding a catch-all must not take containers away from the specific rules an
// operator wrote for them.
func TestNarrowerScopeWinsAtEqualPriority(t *testing.T) {
	// The broad policy sorts FIRST by id, so under the old id-only tie-break it
	// would have won. That is the regression this test exists for.
	broad := broadPolicy("upd_aaaaaaaaaaaaaaaaaaaa")
	narrow := UpdatePolicy{
		PolicyID: "upd_zzzzzzzzzzzzzzzzzzzz",
		Name:     "the web tier",
		Enabled:  true,
		Scope:    ScopeSelector,
		Selector: UpdateSelector{Include: []string{"web"}},
	}
	target := screened("web", "nginx:1.27", nil)

	for _, order := range [][]UpdatePolicy{{broad, narrow}, {narrow, broad}} {
		best, ok := SelectUpdatePolicy(order, target, SelfIdentity{})
		if !ok || best.PolicyID != narrow.PolicyID {
			t.Fatalf("selected %q, want the specific policy", best.PolicyID)
		}
	}
}

// An explicit priority still outranks the scope. The operator's ordering is the
// first key and nothing below it may override one they set.
func TestExplicitPriorityStillOutranksScope(t *testing.T) {
	broad := broadPolicy("upd_aaaaaaaaaaaaaaaaaaaa")
	broad.Priority = 10
	narrow := UpdatePolicy{
		PolicyID: "upd_bbbbbbbbbbbbbbbbbbbb",
		Name:     "the web tier",
		Enabled:  true,
		Scope:    ScopeSelector,
		Selector: UpdateSelector{Include: []string{"web"}},
	}

	best, ok := SelectUpdatePolicy([]UpdatePolicy{broad, narrow},
		screened("web", "nginx:1.27", nil), SelfIdentity{})
	if !ok || best.PolicyID != broad.PolicyID {
		t.Fatalf("selected %q, want the higher-priority policy", best.PolicyID)
	}
}

// ------------------------------------------------------------ validation --

func TestValidateRefusesInclusionClausesUnderAllEligible(t *testing.T) {
	base := func() UpdatePolicy {
		return UpdatePolicy{
			PolicyID:              "upd_aaaaaaaaaaaaaaaaaaaa",
			Name:                  "everything",
			Enabled:               true,
			Scope:                 ScopeAllEligible,
			Strategy:              StrategyPatch,
			Mode:                  ModeObserve,
			MinimumRecommendation: RecommendProceed,
			Window:                MaintenanceWindow{AlwaysOpen: true},
		}
	}

	contradictory := []struct {
		name     string
		selector UpdateSelector
	}{
		{"include", UpdateSelector{Include: []string{"web"}}},
		{"images", UpdateSelector{Images: []string{"nginx:*"}}},
		{"labels", UpdateSelector{Labels: map[string]string{"tier": "front"}}},
	}
	for _, testCase := range contradictory {
		t.Run(testCase.name, func(t *testing.T) {
			policy := base()
			policy.Selector = testCase.selector
			policy.Normalise()
			err := policy.Validate(DefaultUpdatePolicyLimits())
			if err == nil {
				t.Fatal("a broad policy carrying an inclusion clause must be refused")
			}
			var validation PolicyValidationError
			if !asPolicyValidationError(err, &validation) || validation.Field != "selector" {
				t.Fatalf("want a selector validation error, got %v", err)
			}
		})
	}

	// Exclusions are the one clause that survives.
	policy := base()
	policy.Selector = UpdateSelector{Exclude: []string{"database"}}
	policy.Normalise()
	if err := policy.Validate(DefaultUpdatePolicyLimits()); err != nil {
		t.Fatalf("a broad policy with only exclusions is legal: %v", err)
	}
}

func TestValidateStillRefusesAnEmptySelectorUnderTheNarrowScope(t *testing.T) {
	policy := UpdatePolicy{
		PolicyID:              "upd_aaaaaaaaaaaaaaaaaaaa",
		Name:                  "empty",
		Enabled:               true,
		Strategy:              StrategyPatch,
		Mode:                  ModeObserve,
		MinimumRecommendation: RecommendProceed,
		Window:                MaintenanceWindow{AlwaysOpen: true},
	}
	policy.Normalise()
	if policy.Scope != ScopeSelector {
		t.Fatalf("Normalise must default the scope to selector, got %q", policy.Scope)
	}
	if err := policy.Validate(DefaultUpdatePolicyLimits()); err == nil {
		t.Fatal("an empty selector must still be refused")
	}
}

func TestValidateRefusesAnUnknownScopeByName(t *testing.T) {
	policy := UpdatePolicy{
		PolicyID:              "upd_aaaaaaaaaaaaaaaaaaaa",
		Name:                  "odd",
		Enabled:               true,
		Scope:                 "everything",
		Selector:              UpdateSelector{Include: []string{"web"}},
		Strategy:              StrategyPatch,
		Mode:                  ModeObserve,
		MinimumRecommendation: RecommendProceed,
		Window:                MaintenanceWindow{AlwaysOpen: true},
	}
	policy.Normalise()
	if policy.Scope != "everything" {
		t.Fatal("Normalise must not rewrite a scope that was supplied")
	}
	err := policy.Validate(DefaultUpdatePolicyLimits())
	var validation PolicyValidationError
	if !asPolicyValidationError(err, &validation) || validation.Field != "scope" {
		t.Fatalf("want a scope validation error, got %v", err)
	}
}

// The bare wildcard stays refused. `allEligible` is the supported way to say
// "everything", and it must not have made the accidental way legal.
func TestBareWildcardImagePatternIsStillRefused(t *testing.T) {
	policy := UpdatePolicy{
		PolicyID:              "upd_aaaaaaaaaaaaaaaaaaaa",
		Name:                  "sneaky",
		Enabled:               true,
		Scope:                 ScopeSelector,
		Selector:              UpdateSelector{Images: []string{"*"}},
		Strategy:              StrategyPatch,
		Mode:                  ModeObserve,
		MinimumRecommendation: RecommendProceed,
		Window:                MaintenanceWindow{AlwaysOpen: true},
	}
	policy.Normalise()
	if err := policy.Validate(DefaultUpdatePolicyLimits()); err == nil {
		t.Fatal("a bare wildcard must still be refused by name")
	}
}

func TestBroadAutomaticPolicyEarnsAWarning(t *testing.T) {
	policy := UpdatePolicy{
		Name:     "everything",
		Scope:    ScopeAllEligible,
		Strategy: StrategyPatch,
		Mode:     ModeAutomatic,
		Window:   MaintenanceWindow{AlwaysOpen: false, Start: "02:00", End: "04:00"},
		Failure:  UpdateFailureHandling{AutoRollback: true, PauseAfterFailures: 2},
	}
	warnings := policy.Warnings()
	if len(warnings) == 0 {
		t.Fatal("a broad automatic policy must earn a warning")
	}
	found := false
	for _, warning := range warnings {
		if contains(warning, "every eligible container") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no warning mentions the breadth: %v", warnings)
	}
}

// ------------------------------------------------------------- helpers --

func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func asPolicyValidationError(err error, target *PolicyValidationError) bool {
	// errors.As rather than a type assertion: a validation error that a future
	// caller wraps must still be recognised, and a test that silently stopped
	// matching would report the wrong field rather than fail.
	return errors.As(err, target)
}
