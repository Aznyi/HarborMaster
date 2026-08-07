package domain

import (
	"testing"
	"time"

	_ "time/tzdata"
)

func basePolicy() UpdatePolicy {
	return UpdatePolicy{
		PolicyID:              "upd_aaaaaaaaaaaaaaaaaaaa",
		Name:                  "base",
		Enabled:               true,
		Selector:              UpdateSelector{Include: []string{"web"}},
		Strategy:              StrategyMinor,
		MinimumRecommendation: RecommendProceed,
		Mode:                  ModeAutomatic,
		Window: MaintenanceWindow{
			Timezone: "Europe/London",
			Weekdays: []int{int(time.Saturday)},
			Start:    "02:00",
			End:      "04:00",
		},
		Failure: UpdateFailureHandling{AutoRollback: true},
	}
}

func TestParseUpdateLabelsIgnoresForeignNamespaces(t *testing.T) {
	overrides := ParseUpdateLabels(map[string]string{
		"com.example.update.enabled": "false",
		"maintainer":                 "nobody",
	})
	if overrides.Disabled || len(overrides.Unknown) != 0 || len(overrides.Invalid) != 0 {
		t.Fatalf("only io.harbormaster.update.* is read, got %+v", overrides)
	}
}

func TestLabelCannotEnrolAContainer(t *testing.T) {
	// The asymmetry the whole design rests on: anyone who can run `docker run`
	// can set a label, so a label must never be able to opt a container INTO
	// automation. Only out of it.
	overrides := ParseUpdateLabels(map[string]string{LabelUpdateEnabled: "true"})
	if overrides.Disabled {
		t.Fatal("enabled=true must not set Disabled")
	}

	// And there is nowhere for it to be recorded as an opt-in either: the
	// override type has no Enabled field, so nothing downstream can consult one.
	effective := Resolve(basePolicy(), map[string]string{LabelUpdateEnabled: "true"})
	if effective.Disabled {
		t.Fatal("enabled=true changes nothing about a policy that already governs")
	}
}

func TestLabelDisableAndPauseAlwaysWin(t *testing.T) {
	disabled := Resolve(basePolicy(), map[string]string{LabelUpdateEnabled: "false"})
	if !disabled.Disabled {
		t.Fatal("enabled=false must disable automation for this container")
	}
	if disabled.Reason == "" {
		t.Fatal("a disabled container must say which clause disabled it")
	}

	paused := Resolve(basePolicy(), map[string]string{LabelUpdatePause: "true"})
	if !paused.Disabled {
		t.Fatal("pause=true must stop automation")
	}

	notPaused := Resolve(basePolicy(), map[string]string{LabelUpdatePause: "false"})
	if notPaused.Disabled {
		t.Fatal("pause=false leaves the policy alone")
	}
}

func TestLabelStrategyMayNarrowButNeverWiden(t *testing.T) {
	policy := basePolicy() // minor

	narrowed := Resolve(policy, map[string]string{LabelUpdateStrategy: string(StrategyDigestOnly)})
	if narrowed.Strategy != StrategyDigestOnly {
		t.Fatalf("strategy = %q, want the narrower label value", narrowed.Strategy)
	}

	widened := Resolve(policy, map[string]string{LabelUpdateStrategy: string(StrategyMajor)})
	if widened.Strategy != StrategyMinor {
		t.Fatalf("strategy = %q, want the policy's ceiling; a container must not label its way to major", widened.Strategy)
	}

	same := Resolve(policy, map[string]string{LabelUpdateStrategy: string(StrategyMinor)})
	if same.Strategy != StrategyMinor {
		t.Fatalf("strategy = %q, want minor", same.Strategy)
	}
}

func TestUnrecognisedStrategyLabelIsNeverAdopted(t *testing.T) {
	policy := basePolicy()
	effective := Resolve(policy, map[string]string{LabelUpdateStrategy: "everything"})
	if effective.Strategy != StrategyMinor {
		t.Fatalf("strategy = %q, want the policy's; an unreadable label must not change the ceiling", effective.Strategy)
	}
	if len(effective.Overrides.Invalid) != 1 || effective.Overrides.Invalid[0] != LabelUpdateStrategy {
		t.Fatalf("the invalid label must be reported, got %v", effective.Overrides.Invalid)
	}
}

func TestLabelWindowReplacesTimesButKeepsTheZoneAndDays(t *testing.T) {
	policy := basePolicy()
	effective := Resolve(policy, map[string]string{LabelUpdateWindow: "23:00-01:00"})

	if effective.Window.Start != "23:00" || effective.Window.End != "01:00" {
		t.Fatalf("window times = %s-%s, want the label's", effective.Window.Start, effective.Window.End)
	}
	if effective.Window.Timezone != "Europe/London" {
		t.Fatalf("timezone = %q, want the policy's; a container must not choose its own zone", effective.Window.Timezone)
	}
	if len(effective.Window.Weekdays) != 1 || effective.Window.Weekdays[0] != int(time.Saturday) {
		t.Fatalf("weekdays = %v, want the policy's", effective.Window.Weekdays)
	}
	if !effective.Window.CrossesMidnight() {
		t.Fatal("23:00-01:00 crosses midnight")
	}
}

func TestLabelWindowIsIgnoredWhenThePolicyHasNone(t *testing.T) {
	// An always-open policy is a deliberate statement. A label narrowing it
	// would be safe, but a label that could turn "always open" into a
	// half-configured window with no zone would not be, so the policy wins.
	policy := basePolicy()
	policy.Window = MaintenanceWindow{AlwaysOpen: true}

	effective := Resolve(policy, map[string]string{LabelUpdateWindow: "02:00-04:00"})
	if !effective.Window.AlwaysOpen {
		t.Fatalf("window = %+v, want the policy's always-open window", effective.Window)
	}
}

func TestMalformedWindowLabelIsReportedNotApplied(t *testing.T) {
	for _, value := range []string{"", "02:00", "02:00-", "-04:00", "2am-4am", "02:00-02:00", "02:00-04:00-06:00"} {
		effective := Resolve(basePolicy(), map[string]string{LabelUpdateWindow: value})
		if effective.Window.Start != "02:00" || effective.Window.End != "04:00" {
			t.Fatalf("%q was applied: window is now %s-%s", value, effective.Window.Start, effective.Window.End)
		}
		if len(effective.Overrides.Invalid) != 1 {
			t.Fatalf("%q must be reported invalid, got %v", value, effective.Overrides.Invalid)
		}
	}
}

func TestLabelRollbackOverrideGoesBothWays(t *testing.T) {
	policy := basePolicy() // AutoRollback true

	off := Resolve(policy, map[string]string{LabelUpdateRollback: "false"})
	if off.AutoRollback {
		t.Fatal("rollback=false must turn automatic rollback off")
	}

	policy.Failure.AutoRollback = false
	on := Resolve(policy, map[string]string{LabelUpdateRollback: "true"})
	if !on.AutoRollback {
		t.Fatal("rollback=true must turn automatic rollback on")
	}
	// Enabling rollback is not a widening: rollback only ever returns the
	// container to the state it was already in, so a label may ask for it.
}

func TestUnknownAndInvalidLabelsAreSurfacedNotDropped(t *testing.T) {
	overrides := ParseUpdateLabels(map[string]string{
		"io.harbormaster.update.enabledd": "false", // a typo in a safety control
		"io.harbormaster.update.strategy": "banana",
		"io.harbormaster.update.enabled":  "perhaps",
		"io.harbormaster.update.pause":    "sometimes",
	})

	if len(overrides.Unknown) != 1 || overrides.Unknown[0] != "io.harbormaster.update.enabledd" {
		t.Fatalf("unknown = %v, want the misspelled key", overrides.Unknown)
	}
	if len(overrides.Invalid) != 3 {
		t.Fatalf("invalid = %v, want all three unreadable values", overrides.Invalid)
	}
	// A misspelled enabled=false must NOT have disabled anything, which is
	// exactly why it has to be reported.
	if overrides.Disabled {
		t.Fatal("a misspelled key must not take effect")
	}
	// Sorted, so two containers with the same problem read identically.
	for i := 1; i < len(overrides.Invalid); i++ {
		if overrides.Invalid[i] < overrides.Invalid[i-1] {
			t.Fatalf("invalid keys must be sorted, got %v", overrides.Invalid)
		}
	}
}

func TestResolveCarriesThePolicyForAttribution(t *testing.T) {
	policy := basePolicy()
	effective := Resolve(policy, nil)

	if effective.Policy.PolicyID != policy.PolicyID {
		t.Fatal("the governing policy must survive resolution so a decision can name it")
	}
	if effective.Mode != policy.Mode {
		t.Fatalf("mode = %q, want the policy's; no label may change the mode", effective.Mode)
	}
}

func TestNoLabelCanChangeTheMode(t *testing.T) {
	// Mode is the one setting that decides whether the host may be touched at
	// all. There is deliberately no label for it, and nothing that sets one.
	policy := basePolicy()
	policy.Mode = ModeObserve

	effective := Resolve(policy, map[string]string{
		"io.harbormaster.update.mode":      "automatic",
		"io.harbormaster.update.automatic": "true",
		LabelUpdateEnabled:                 "true",
	})
	if effective.Mode != ModeObserve {
		t.Fatalf("mode = %q, want observe; no label may promote a policy to automatic", effective.Mode)
	}
	if effective.Mode.Mutates() {
		t.Fatal("a label must never make a policy able to change the host")
	}
	if len(effective.Overrides.Unknown) != 2 {
		t.Fatalf("the two invented keys must be reported, got %v", effective.Overrides.Unknown)
	}
}

func TestStrategyRankRefusesTheUnknown(t *testing.T) {
	// An unrecognised strategy ranks as the most permissive so `narrower` will
	// never adopt it. If this ever inverted, an unknown label value would
	// silently become the ceiling.
	if narrower(UpdateStrategy("everything"), StrategyDigestOnly) {
		t.Fatal("an unrecognised strategy must never be treated as narrower")
	}
	if !narrower(StrategyDigestOnly, StrategyPatch) {
		t.Fatal("digestOnly is narrower than patch")
	}
	if narrower(StrategyMajor, StrategyPatch) {
		t.Fatal("major is not narrower than patch")
	}
}
