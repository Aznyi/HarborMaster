package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestUpdatePolicyIDShape(t *testing.T) {
	id := NewUpdatePolicyID()
	if !ValidUpdatePolicyID(id) {
		t.Fatalf("generated id %q must validate", id)
	}
	for _, bad := range []string{
		"", "upd_", "upd_short", "pol_0123456789abcdef0123",
		"upd_0123456789ABCDEF0123", // uppercase hex is not the generated shape
		"upd_0123456789abcdef01234",
	} {
		if ValidUpdatePolicyID(bad) {
			t.Fatalf("%q must not validate", bad)
		}
	}
	if id == NewUpdatePolicyID() {
		t.Fatal("two generated ids must differ")
	}
}

func TestStrategyPermitsIsACeilingNotAFilter(t *testing.T) {
	cases := []struct {
		strategy UpdateStrategy
		update   UpdateType
		want     bool
	}{
		// A republished tag is permitted by every strategy: the operator chose
		// the tag and only its content moved.
		{StrategyDigestOnly, UpdateDigest, true},
		{StrategyPatch, UpdateDigest, true},
		{StrategyMajor, UpdateDigest, true},

		{StrategyDigestOnly, UpdatePatch, false},
		{StrategyPatch, UpdatePatch, true},
		{StrategyMinor, UpdatePatch, true},
		{StrategyMajor, UpdatePatch, true},

		{StrategyPatch, UpdateMinor, false},
		{StrategyMinor, UpdateMinor, true},
		{StrategyMajor, UpdateMinor, true},

		{StrategyMinor, UpdateMajor, false},
		{StrategyMajor, UpdateMajor, true},

		// The three that no strategy may automate.
		{StrategyMajor, UpdateUnknown, false},
		{StrategyMajor, UpdatePrerelease, false},
		{StrategyMajor, UpdateNone, false},
	}
	for _, tc := range cases {
		if got := tc.strategy.Permits(tc.update); got != tc.want {
			t.Fatalf("%s.Permits(%s) = %v, want %v", tc.strategy, tc.update, got, tc.want)
		}
	}
}

func TestStrategyPermitsRefusesAnUnrecognisedUpdateType(t *testing.T) {
	// A type this build does not know about is one it could not size, and an
	// unsized change is exactly what a ceiling exists to refuse.
	if StrategyMajor.Permits(UpdateType("quantum")) {
		t.Fatal("an unrecognised update type must never be permitted")
	}
}

func TestAutomationModeMutatesOnlyWhenAutomatic(t *testing.T) {
	for _, mode := range []AutomationMode{ModeObserve, ModeDryRun, ModeApprove} {
		if mode.Mutates() {
			t.Fatalf("%s must not be able to change the host", mode)
		}
	}
	if !ModeAutomatic.Mutates() {
		t.Fatal("automatic mode changes the host")
	}
	if !ModeApprove.NeedsApproval() {
		t.Fatal("approvalRequired waits for a person")
	}
	if ModeAutomatic.NeedsApproval() {
		t.Fatal("automatic mode does not wait")
	}
}

func TestEmptySelectorMatchesNothing(t *testing.T) {
	selector := UpdateSelector{}
	if !selector.Empty() {
		t.Fatal("a selector with no clauses is empty")
	}
	if selector.Matches(SelectionTarget{Name: "anything", Image: "nginx:1.27"}) {
		t.Fatal("an empty selector must govern nothing; the whole-estate accident is the failure mode this guards")
	}
	// Exclude alone is still empty: it says what NOT to govern, never what to.
	excludeOnly := UpdateSelector{Exclude: []string{"other"}}
	if !excludeOnly.Empty() {
		t.Fatal("exclude alone does not make a selector non-empty")
	}
	if excludeOnly.Matches(SelectionTarget{Name: "anything"}) {
		t.Fatal("exclude alone must not govern anything")
	}
}

func TestSelectorExclusionIsCheckedFirstAndIsFinal(t *testing.T) {
	selector := UpdateSelector{
		Include: []string{"web"},
		Images:  []string{"nginx:*"},
		Labels:  map[string]string{"tier": "front"},
		Exclude: []string{"web"},
	}
	target := SelectionTarget{
		Name:   "web",
		Image:  "nginx:1.27",
		Labels: map[string]string{"tier": "front"},
	}
	if selector.Matches(target) {
		t.Fatal("exclusion must win over every inclusive clause")
	}
}

func TestSelectorMatchesAnyPopulatedClause(t *testing.T) {
	cases := []struct {
		name     string
		selector UpdateSelector
		target   SelectionTarget
		want     bool
	}{
		{
			"by name",
			UpdateSelector{Include: []string{"web"}},
			SelectionTarget{Name: "web"},
			true,
		},
		{
			"by name, case insensitive",
			UpdateSelector{Include: []string{"WEB"}},
			SelectionTarget{Name: "web"},
			true,
		},
		{
			"by name, no match",
			UpdateSelector{Include: []string{"web"}},
			SelectionTarget{Name: "cache"},
			false,
		},
		{
			"by image prefix glob",
			UpdateSelector{Images: []string{"ghcr.io/acme/*"}},
			SelectionTarget{Image: "ghcr.io/acme/api:1.2"},
			true,
		},
		{
			"by image tag glob",
			UpdateSelector{Images: []string{"nginx:1.27.*"}},
			SelectionTarget{Image: "nginx:1.27.4"},
			true,
		},
		{
			"by image glob, wrong repository",
			UpdateSelector{Images: []string{"ghcr.io/acme/*"}},
			SelectionTarget{Image: "docker.io/acme/api:1.2"},
			false,
		},
		{
			"by image, exact when no wildcard",
			UpdateSelector{Images: []string{"nginx:1.27"}},
			SelectionTarget{Image: "nginx:1.27.4"},
			false,
		},
		{
			"by label key and value",
			UpdateSelector{Labels: map[string]string{"tier": "front"}},
			SelectionTarget{Labels: map[string]string{"tier": "front", "team": "web"}},
			true,
		},
		{
			"by label key only",
			UpdateSelector{Labels: map[string]string{"tier": ""}},
			SelectionTarget{Labels: map[string]string{"tier": "anything"}},
			true,
		},
		{
			"by label, wrong value",
			UpdateSelector{Labels: map[string]string{"tier": "front"}},
			SelectionTarget{Labels: map[string]string{"tier": "back"}},
			false,
		},
		{
			// Every label clause must hold. Two required labels and one present
			// is not a match.
			"by label, partial",
			UpdateSelector{Labels: map[string]string{"tier": "front", "team": "web"}},
			SelectionTarget{Labels: map[string]string{"tier": "front"}},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.selector.Matches(tc.target); got != tc.want {
				t.Fatalf("matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMatchGlobIsBounded(t *testing.T) {
	cases := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"*", "anything", true},
		{"", "anything", false},
		{"nginx", "nginx", true},
		{"nginx", "nginx:1", false},
		{"*:latest", "nginx:latest", true},
		{"*:latest", "nginx:1.27", false},
		{"ghcr.io/*/api:*", "ghcr.io/acme/api:1.2", true},
		{"ghcr.io/*/api:*", "ghcr.io/acme/web:1.2", false},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxcyyb", false},
		// The pathological input a regex engine would backtrack on. Glob walks
		// it once.
		{strings.Repeat("a*", 8), strings.Repeat("a", 4096), true},
	}
	for _, tc := range cases {
		if got := matchGlob(tc.pattern, tc.value); got != tc.want {
			t.Fatalf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}

// --------------------------------------------------------------- selection --

func policyFixture(id string, priority int) UpdatePolicy {
	return UpdatePolicy{
		PolicyID: id,
		Name:     id,
		Enabled:  true,
		Priority: priority,
		Selector: UpdateSelector{Include: []string{"web"}},
		Strategy: StrategyPatch,
		Mode:     ModeObserve,
	}
}

func TestSelectUpdatePolicyPrefersHighestPriority(t *testing.T) {
	target := SelectionTarget{Name: "web"}
	policies := []UpdatePolicy{
		policyFixture("upd_aaaaaaaaaaaaaaaaaaaa", 10),
		policyFixture("upd_bbbbbbbbbbbbbbbbbbbb", 50),
		policyFixture("upd_cccccccccccccccccccc", 30),
	}
	best, ok := SelectUpdatePolicy(policies, target, SelfIdentity{})
	if !ok || best.PolicyID != "upd_bbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("selected %q (%v), want the priority-50 policy", best.PolicyID, ok)
	}
}

func TestSelectUpdatePolicyBreaksTiesDeterministically(t *testing.T) {
	target := SelectionTarget{Name: "web"}
	forward := []UpdatePolicy{
		policyFixture("upd_bbbbbbbbbbbbbbbbbbbb", 10),
		policyFixture("upd_aaaaaaaaaaaaaaaaaaaa", 10),
	}
	reverse := []UpdatePolicy{
		policyFixture("upd_aaaaaaaaaaaaaaaaaaaa", 10),
		policyFixture("upd_bbbbbbbbbbbbbbbbbbbb", 10),
	}
	first, _ := SelectUpdatePolicy(forward, target, SelfIdentity{})
	second, _ := SelectUpdatePolicy(reverse, target, SelfIdentity{})
	if first.PolicyID != second.PolicyID {
		t.Fatalf("row order changed the winner: %q vs %q", first.PolicyID, second.PolicyID)
	}
	if first.PolicyID != "upd_aaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("a tie must resolve to the lowest policy id, got %q", first.PolicyID)
	}
}

func TestSelectUpdatePolicySkipsDisabledAndArchived(t *testing.T) {
	target := SelectionTarget{Name: "web"}

	disabled := policyFixture("upd_aaaaaaaaaaaaaaaaaaaa", 100)
	disabled.Enabled = false
	archived := policyFixture("upd_bbbbbbbbbbbbbbbbbbbb", 90)
	archived.Archived = true
	live := policyFixture("upd_cccccccccccccccccccc", 1)

	best, ok := SelectUpdatePolicy([]UpdatePolicy{disabled, archived, live}, target, SelfIdentity{})
	if !ok || best.PolicyID != live.PolicyID {
		t.Fatalf("selected %q (%v), want the only live policy", best.PolicyID, ok)
	}

	if _, ok := SelectUpdatePolicy([]UpdatePolicy{disabled, archived}, target, SelfIdentity{}); ok {
		t.Fatal("no live policy governs this container")
	}
}

func TestSortUpdatePoliciesIsStableAndOrdered(t *testing.T) {
	policies := []UpdatePolicy{
		policyFixture("upd_cccccccccccccccccccc", 5),
		policyFixture("upd_aaaaaaaaaaaaaaaaaaaa", 50),
		policyFixture("upd_bbbbbbbbbbbbbbbbbbbb", 5),
	}
	SortUpdatePolicies(policies)
	got := []string{policies[0].PolicyID, policies[1].PolicyID, policies[2].PolicyID}
	want := []string{
		"upd_aaaaaaaaaaaaaaaaaaaa",
		"upd_bbbbbbbbbbbbbbbbbbbb",
		"upd_cccccccccccccccccccc",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sorted = %v, want %v", got, want)
		}
	}
}

// -------------------------------------------------------------- validation --

func validPolicy() UpdatePolicy {
	policy := UpdatePolicy{
		PolicyID:              NewUpdatePolicyID(),
		Name:                  "Patch the web tier overnight",
		Enabled:               true,
		Priority:              10,
		Selector:              UpdateSelector{Include: []string{"web"}},
		Strategy:              StrategyPatch,
		MinimumRecommendation: RecommendProceed,
		Mode:                  ModeObserve,
		Window:                MaintenanceWindow{Timezone: "UTC", Start: "02:00", End: "04:00"},
	}
	policy.Normalise()
	return policy
}

func TestUpdatePolicyValidateAcceptsAWellFormedPolicy(t *testing.T) {
	if err := validPolicy().Validate(DefaultUpdatePolicyLimits()); err != nil {
		t.Fatalf("a well-formed policy must validate: %v", err)
	}
}

func TestUpdatePolicyNormaliseFillsDefaultsRatherThanLeavingZeroes(t *testing.T) {
	// A zero limit must never be read as "unlimited". Normalise is where that
	// is decided, once, before storage.
	policy := UpdatePolicy{
		Name:                  " spaced ",
		Selector:              UpdateSelector{Include: []string{" web ", "web", ""}},
		Strategy:              " patch ",
		Mode:                  " observe ",
		MinimumRecommendation: " proceed ",
	}
	policy.Normalise()

	if policy.Name != "spaced" {
		t.Fatalf("name = %q, want trimmed", policy.Name)
	}
	if len(policy.Selector.Include) != 1 || policy.Selector.Include[0] != "web" {
		t.Fatalf("include = %v, want one trimmed deduplicated entry", policy.Selector.Include)
	}
	if policy.Strategy != StrategyPatch || policy.Mode != ModeObserve {
		t.Fatalf("enums were not trimmed: %q %q", policy.Strategy, policy.Mode)
	}
	if policy.Limits.MaxConcurrent != DefaultUpdateConcurrency ||
		policy.Limits.MaxPerRun != DefaultUpdatePerRun ||
		policy.Limits.AcquisitionTimeoutSeconds != DefaultAcquisitionTimeoutSecs ||
		policy.Limits.RecreateTimeoutSeconds != DefaultRecreateTimeoutSecs ||
		policy.Limits.HealthTimeoutSeconds != DefaultHealthTimeoutSecs {
		t.Fatalf("zero limits must become defaults, got %+v", policy.Limits)
	}
	if policy.Failure.PauseWindowHours != DefaultPauseWindowHours {
		t.Fatalf("pause window = %d, want the default", policy.Failure.PauseWindowHours)
	}
}

func TestUpdatePolicyValidateRejects(t *testing.T) {
	cases := []struct {
		name  string
		mutar func(*UpdatePolicy)
		field string
	}{
		{"empty name", func(p *UpdatePolicy) { p.Name = "" }, "name"},
		{
			"control characters in the name",
			func(p *UpdatePolicy) { p.Name = "web\x00tier" },
			"name",
		},
		{"unknown strategy", func(p *UpdatePolicy) { p.Strategy = "everything" }, "strategy"},
		{"unknown mode", func(p *UpdatePolicy) { p.Mode = "yolo" }, "mode"},
		{
			// The three verdicts that mean a person has to look.
			"unknown recommendation",
			func(p *UpdatePolicy) { p.MinimumRecommendation = RecommendUnknown },
			"minimumRecommendation",
		},
		{
			"manual review recommendation",
			func(p *UpdatePolicy) { p.MinimumRecommendation = RecommendManualReview },
			"minimumRecommendation",
		},
		{
			"not-recommended recommendation",
			func(p *UpdatePolicy) { p.MinimumRecommendation = RecommendAgainst },
			"minimumRecommendation",
		},
		{"negative priority", func(p *UpdatePolicy) { p.Priority = -1 }, "priority"},
		{"absurd priority", func(p *UpdatePolicy) { p.Priority = 100000 }, "priority"},
		{
			"empty selector",
			func(p *UpdatePolicy) { p.Selector = UpdateSelector{} },
			"selector",
		},
		{
			"bare wildcard image",
			func(p *UpdatePolicy) { p.Selector = UpdateSelector{Images: []string{"*"}} },
			"selector.images[0]",
		},
		{
			"unbounded concurrency",
			func(p *UpdatePolicy) { p.Limits.MaxConcurrent = 10000 },
			"limits.maxConcurrent",
		},
		{
			"per-registry above overall",
			func(p *UpdatePolicy) {
				p.Limits.MaxConcurrent = 2
				p.Limits.MaxPerRegistry = 4
			},
			"limits.maxPerRegistry",
		},
		{
			"absurd timeout",
			func(p *UpdatePolicy) { p.Limits.AcquisitionTimeoutSeconds = 86400 },
			"limits.acquisitionTimeoutSeconds",
		},
		{
			"unresolvable window zone",
			func(p *UpdatePolicy) { p.Window.Timezone = "Mars/Olympus_Mons" },
			"window",
		},
		{
			"out of range weekday",
			func(p *UpdatePolicy) { p.Window.Weekdays = []int{9} },
			"window",
		},
		{
			"absurd cooldown",
			func(p *UpdatePolicy) { p.Failure.CooldownHours = 100000 },
			"failure.cooldownHours",
		},
		{
			"absurd retries",
			func(p *UpdatePolicy) { p.Failure.MaxRetries = 1000 },
			"failure.maxRetries",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := validPolicy()
			tc.mutar(&policy)

			err := policy.Validate(DefaultUpdatePolicyLimits())
			if err == nil {
				t.Fatal("must be rejected")
			}
			var validation PolicyValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("want a PolicyValidationError, got %T", err)
			}
			if validation.Field != tc.field {
				t.Fatalf("field = %q, want %q", validation.Field, tc.field)
			}
		})
	}
}

func TestUpdatePolicyValidationNeverEchoesTheValue(t *testing.T) {
	// A validation message that reflected its input would be a way to make the
	// API respond with attacker-chosen text.
	const marker = "PoIsOnEdVaLuE"

	policy := validPolicy()
	policy.Name = marker + "\x01"
	if err := policy.Validate(DefaultUpdatePolicyLimits()); err == nil {
		t.Fatal("expected rejection")
	} else if strings.Contains(err.Error(), marker) {
		t.Fatalf("message echoed the value: %q", err.Error())
	}

	policy = validPolicy()
	policy.Strategy = UpdateStrategy(marker)
	if err := policy.Validate(DefaultUpdatePolicyLimits()); err == nil {
		t.Fatal("expected rejection")
	} else if strings.Contains(err.Error(), marker) {
		t.Fatalf("message echoed the value: %q", err.Error())
	}

	policy = validPolicy()
	policy.Window.Timezone = marker
	if err := policy.Validate(DefaultUpdatePolicyLimits()); err == nil {
		t.Fatal("expected rejection")
	} else if strings.Contains(err.Error(), marker) {
		t.Fatalf("message echoed the value: %q", err.Error())
	}
}

func TestUpdatePolicyBoundsSelectorSize(t *testing.T) {
	limits := DefaultUpdatePolicyLimits()

	policy := validPolicy()
	policy.Selector.Include = make([]string, limits.MaxSelectorEntries+1)
	for i := range policy.Selector.Include {
		policy.Selector.Include[i] = "container-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if err := policy.Validate(limits); err == nil {
		t.Fatal("an oversized include list must be rejected")
	}

	policy = validPolicy()
	policy.Selector.Images = []string{strings.Repeat("a", limits.MaxSelectorBytes+1)}
	if err := policy.Validate(limits); err == nil {
		t.Fatal("an oversized pattern must be rejected")
	}

	policy = validPolicy()
	policy.Selector.Images = []string{strings.Repeat("*a", 20)}
	if err := policy.Validate(limits); err == nil {
		t.Fatal("a pattern with more wildcards than the matcher's bound must be rejected")
	}
}

func TestUpdatePolicyWarningsNameTheDangerousCombinations(t *testing.T) {
	policy := validPolicy()
	policy.Mode = ModeAutomatic
	policy.Strategy = StrategyMajor
	policy.Window = MaintenanceWindow{AlwaysOpen: true}
	policy.Failure.AutoRollback = false
	policy.Failure.PauseAfterFailures = 0

	warnings := policy.Warnings()
	if len(warnings) < 4 {
		t.Fatalf("want a warning for each of major/no-window/no-rollback/no-pause, got %v", warnings)
	}

	// A conservative policy is quiet apart from the pause default it has set.
	safe := validPolicy()
	safe.Mode = ModeObserve
	safe.Failure.PauseAfterFailures = 2
	if got := safe.Warnings(); len(got) != 0 {
		t.Fatalf("an observe-mode policy needs no warnings, got %v", got)
	}
}

func TestAutomatableRecommendation(t *testing.T) {
	if !AutomatableRecommendation(RecommendProceed) || !AutomatableRecommendation(RecommendCaution) {
		t.Fatal("proceed and proceedWithCaution may gate automation")
	}
	for _, verdict := range []Recommendation{RecommendManualReview, RecommendAgainst, RecommendUnknown, ""} {
		if AutomatableRecommendation(verdict) {
			t.Fatalf("%q must never gate automation", verdict)
		}
	}
}
