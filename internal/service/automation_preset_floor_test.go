package service_test

import (
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// The recommendation floor the automation presets must compile to.
//
// # Why this test is in Go when the presets are TypeScript
//
// The preset table lives in `web/src/api/automationPresets.ts`, and its tests
// pin the values it writes. What those tests CANNOT establish is whether the
// values are any good, because the thing that decides that is the Go risk model
// plus `DecideAutomation` -- neither of which the frontend can see.
//
// So the contract is written in two halves that must agree:
//
//	here                        a policy shaped like preset P must actually
//	                            submit the updates P's ceiling permits
//	automationPresets.test.ts   preset P compiles to exactly this shape
//
// If the risk model moves under the presets, this half fails and names the
// preset that stopped working. Without it, the failure is silent: policies keep
// validating, passes keep running, and containers simply never update -- which
// is indistinguishable from "there was nothing to do".
//
// # The scenario is measured, not invented
//
// Every plan below takes its recommendation from `domain.AssessRisk` over real
// inputs rather than from a hand-set `Recommendation` field. A test that set the
// verdict directly would pass whatever the risk model did, which is precisely
// the drift it exists to catch.
//
// The load-bearing case is the first one. `image:latest`, republished by its
// publisher, is the canonical Watchtower workload and the one "Follow current
// tag" exists to serve. It carries two CAUTION factors that have nothing to do
// with the change being unsafe -- the tag is mutable (12) and the image is
// newly published (8) -- and 12 + 12 + 8 = 32 lands in the `medium` band. With
// no WARNING factor present that is `proceedWithCaution`, so a policy whose
// floor is `proceed` refuses it and the preset does nothing at all.

var presetFloorAt = time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)

// presetPolicy builds a policy shaped exactly as a preset compiles one.
//
// Only the four fields a preset owns are parameters. Everything else is the
// operator's and is held constant, which is what makes a failure here point at
// the preset table rather than at targeting.
func presetPolicy(
	strategy domain.UpdateStrategy,
	floor domain.Recommendation,
) domain.UpdatePolicy {
	return presetPolicyIn(strategy, floor, domain.ModeAutomatic)
}

func presetPolicyIn(
	strategy domain.UpdateStrategy,
	floor domain.Recommendation,
	mode domain.AutomationMode,
) domain.UpdatePolicy {
	policy := domain.UpdatePolicy{
		PolicyID:              "upd_aaaaaaaaaaaaaaaaaaaa",
		Name:                  "preset",
		Enabled:               true,
		Selector:              domain.UpdateSelector{Include: []string{"web"}},
		Strategy:              strategy,
		MinimumRecommendation: floor,
		Mode:                  mode,
		Window:                domain.MaintenanceWindow{AlwaysOpen: true},
		Failure:               domain.UpdateFailureHandling{AutoRollback: true, PauseAfterFailures: 2},
	}
	policy.Normalise()
	return policy
}

// TestEveryPresetShapeIsAPolicyTheAPIAccepts closes the other half of
// Invariant B.
//
// A preset is only a configuration shortcut if what it compiles is an ORDINARY
// policy. If a preset wrote a field combination `Validate` refuses, the operator
// would meet a 400 from a control that offers no way to fix it -- and if it
// wrote one `Validate` accepts but `Normalise` rewrites, the policy stored would
// not be the policy the preset described.
//
// Mirrors the table in web/src/api/automationPresets.ts. The two must be
// changed together, which is the point: this is the half the frontend cannot
// check, because `Normalise` and `Validate` are Go.
func TestEveryPresetShapeIsAPolicyTheAPIAccepts(t *testing.T) {
	presets := []struct {
		name     string
		strategy domain.UpdateStrategy
		mode     domain.AutomationMode
		floor    domain.Recommendation
	}{
		{"observe", domain.StrategyPatch, domain.ModeObserve, domain.RecommendCaution},
		{"safeAutomatic", domain.StrategyPatch, domain.ModeAutomatic, domain.RecommendCaution},
		{"followCurrentTag", domain.StrategyDigestOnly, domain.ModeAutomatic, domain.RecommendCaution},
		{"automaticMinor", domain.StrategyMinor, domain.ModeAutomatic, domain.RecommendCaution},
	}

	for _, preset := range presets {
		t.Run(preset.name, func(t *testing.T) {
			policy := presetPolicyIn(preset.strategy, preset.floor, preset.mode)

			if err := policy.Validate(domain.DefaultUpdatePolicyLimits()); err != nil {
				t.Fatalf("the %s preset compiles a policy the API refuses: %v",
					preset.name, err)
			}

			// Normalise ran inside presetPolicyIn. Running it again must change
			// nothing: a preset whose output is rewritten on the way in would
			// describe one policy to the operator and store another.
			again := policy
			again.Normalise()

			if again.Strategy != preset.strategy {
				t.Fatalf("strategy = %q, want %q", again.Strategy, preset.strategy)
			}
			if again.Mode != preset.mode {
				t.Fatalf("mode = %q, want %q", again.Mode, preset.mode)
			}
			if again.MinimumRecommendation != preset.floor {
				t.Fatalf("floor = %q, want %q", again.MinimumRecommendation, preset.floor)
			}
			if !again.Failure.AutoRollback {
				t.Fatal("every preset promises automatic rollback")
			}
		})
	}
}

// presetPlan assesses a realistic change and returns the plan a pass would read.
func presetPlan(
	kind domain.UpdateType,
	tag string,
	containers int,
	publishedHoursAgo int,
) domain.ChangePlan {
	published := presetFloorAt.Add(-time.Duration(publishedHoursAgo) * time.Hour)

	risk := domain.AssessRisk(domain.PlanInputs{
		ContainerID:    "container-web",
		ContainerName:  "web",
		CurrentImage:   "nginx:" + tag,
		ProposedImage:  "nginx:" + tag,
		CurrentDigest:  "sha256:" + repeatHex('a'),
		ProposedDigest: "sha256:" + repeatHex('b'),
		CurrentTag:     tag,
		UpdateType:     kind,

		// Full evidence, and all of it good. Phase 17.2's snapshot assurance is
		// what makes SnapshotID non-zero in practice; before it, every one of
		// these plans also carried the 25-point WARNING factor and scored
		// `manualReview` regardless of the floor.
		SnapshotID:       7,
		RestoreReadiness: domain.ReadinessReady,
		RegistryStatus:   domain.CheckOK,
		LocalPlatform:    domain.Platform{OS: "linux", Architecture: "amd64"},

		ProposedPublishedAt: &published,
		ContainerCount:      containers,
		EvaluatedAt:         presetFloorAt,
	})

	return domain.ChangePlan{
		PlanID:         "plan_0123456789abcdef0123",
		ContainerID:    "container-web",
		ContainerName:  "web",
		CurrentImage:   "nginx:" + tag,
		ProposedImage:  "nginx:" + tag,
		CurrentDigest:  "sha256:" + repeatHex('a'),
		ProposedDigest: "sha256:" + repeatHex('b'),
		UpdateType:     kind,
		Risk:           risk,
	}
}

func presetInput(policy domain.UpdatePolicy, plan domain.ChangePlan) service.AutomationInput {
	return service.AutomationInput{
		Target:      domain.SelectionTarget{Name: "web", Image: plan.CurrentImage},
		ContainerID: "container-web",
		Policies:    []domain.UpdatePolicy{policy},
		Plan:        plan,
		HasPlan:     true,
		Now:         presetFloorAt,
	}
}

// TestPresetFloorsSubmitTheUpdatesTheirCeilingsPermit is the whole contract.
//
// A preset that permits an update type and then refuses every real instance of
// it is worse than no preset: the operator configured automation, HarborMaster
// reported "0 eligible", and nothing in the UI said why.
func TestPresetFloorsSubmitTheUpdatesTheirCeilingsPermit(t *testing.T) {
	cases := []struct {
		preset     string
		strategy   domain.UpdateStrategy
		floor      domain.Recommendation
		kind       domain.UpdateType
		tag        string
		containers int
		publishedH int
		why        string
	}{
		{
			preset: "followCurrentTag", strategy: domain.StrategyDigestOnly,
			floor: domain.RecommendCaution,
			kind:  domain.UpdateDigest, tag: "latest", containers: 1, publishedH: 6,
			why: "the canonical Watchtower workload: a mutable tag, freshly republished",
		},
		{
			preset: "followCurrentTag", strategy: domain.StrategyDigestOnly,
			floor: domain.RecommendCaution,
			kind:  domain.UpdateDigest, tag: "1.27.4", containers: 1, publishedH: 6,
			why: "an immutable tag the publisher republished",
		},
		{
			preset: "safeAutomatic", strategy: domain.StrategyPatch,
			floor: domain.RecommendCaution,
			kind:  domain.UpdateDigest, tag: "stable", containers: 2, publishedH: 6,
			why: "the patch ceiling also permits a republished digest, so it meets the same case",
		},
		{
			preset: "safeAutomatic", strategy: domain.StrategyPatch,
			floor: domain.RecommendCaution,
			kind:  domain.UpdatePatch, tag: "1.27.4", containers: 8, publishedH: 6,
			why: "a patch on a widely-deployed image, fresh off the registry",
		},
		{
			preset: "automaticMinor", strategy: domain.StrategyMinor,
			floor: domain.RecommendCaution,
			kind:  domain.UpdateMinor, tag: "1.27.4", containers: 8, publishedH: 6,
			why: "a minor version on more containers than the blast-radius threshold",
		},
		{
			preset: "automaticMinor", strategy: domain.StrategyMinor,
			floor: domain.RecommendCaution,
			kind:  domain.UpdateRebind, tag: "latest", containers: 8, publishedH: 6,
			why: "a namespace rebind, which every acting preset permits",
		},
	}

	for _, tc := range cases {
		t.Run(tc.preset+"/"+string(tc.kind)+"/"+tc.tag, func(t *testing.T) {
			plan := presetPlan(tc.kind, tc.tag, tc.containers, tc.publishedH)
			outcome := service.DecideAutomation(
				presetInput(presetPolicy(tc.strategy, tc.floor), plan))

			if !outcome.Eligible() {
				t.Fatalf(
					"preset %q must submit this update (%s)\n"+
						"  update      = %s on %q\n"+
						"  risk        = %d (%s), recommendation %q\n"+
						"  policy floor= %q\n"+
						"  verdict     = %q, reason %q\n"+
						"  detail      = %s",
					tc.preset, tc.why, tc.kind, tc.tag,
					plan.Risk.Score, plan.Risk.Band, plan.Risk.Recommendation,
					tc.floor,
					outcome.Decision.Verdict, outcome.Decision.Reason,
					outcome.Decision.Detail)
			}
		})
	}
}

// TestPresetFloorsStillRefuseWhatNeedsAPerson is the other half.
//
// Widening the floor from `proceed` to `proceedWithCaution` must not widen what
// automation may do to anything a person is supposed to look at. It admits the
// CAUTION band and nothing else: every WARNING-severity factor still produces
// `manualReview`, which no floor validation permits and no preset can reach.
func TestPresetFloorsStillRefuseWhatNeedsAPerson(t *testing.T) {
	cases := []struct {
		name     string
		plan     domain.ChangePlan
		strategy domain.UpdateStrategy
	}{
		{
			name:     "a major version is held for a person",
			plan:     presetPlan(domain.UpdateMajor, "1.27.4", 1, 6),
			strategy: domain.StrategyMinor,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome := service.DecideAutomation(
				presetInput(presetPolicy(tc.strategy, domain.RecommendCaution), tc.plan))

			if outcome.Eligible() {
				t.Fatalf(
					"this must not be automated at the caution floor\n"+
						"  risk = %d (%s), recommendation %q",
					tc.plan.Risk.Score, tc.plan.Risk.Band, tc.plan.Risk.Recommendation)
			}
		})
	}
}

// TestObserveReportsWhatTheActingPresetsWouldDo pins why the Observe preset
// carries the same floor as the acting ones.
//
// `mode: observe` cannot change a host -- `Mutates()` is true for `automatic`
// alone -- but the mode is read at step 11 of `DecideAutomation` and the
// recommendation floor at step 8. So the floor decides what an observe policy
// REPORTS, three checks before the mode decides whether it may act.
//
// An observe preset exists to preview what "Keep containers safely updated"
// would do. Give it a stricter floor than that preset and it stops previewing
// it: the container an acting policy would update is reported as skipped for
// `recommendation`, and the operator concludes automation would do nothing.
func TestObserveReportsWhatTheActingPresetsWouldDo(t *testing.T) {
	plan := presetPlan(domain.UpdateDigest, "latest", 1, 6)

	outcome := service.DecideAutomation(presetInput(
		presetPolicyIn(domain.StrategyPatch, domain.RecommendCaution, domain.ModeObserve),
		plan))

	if outcome.Decision.Verdict != domain.VerdictWouldUpdate {
		t.Fatalf(
			"an observe preset must report this as an update it would make\n"+
				"  risk    = %d (%s), recommendation %q\n"+
				"  verdict = %q, reason %q\n"+
				"  detail  = %s",
			plan.Risk.Score, plan.Risk.Band, plan.Risk.Recommendation,
			outcome.Decision.Verdict, outcome.Decision.Reason, outcome.Decision.Detail)
	}
	if outcome.Decision.Reason != domain.ReasonObserveMode {
		t.Fatalf("reason = %q, want %q", outcome.Decision.Reason, domain.ReasonObserveMode)
	}

	// And the same policy at the strict floor does NOT report it, which is the
	// behaviour that made the observe preview disagree with the preset it is
	// supposed to preview.
	strict := service.DecideAutomation(presetInput(
		presetPolicyIn(domain.StrategyPatch, domain.RecommendProceed, domain.ModeObserve),
		plan))
	if strict.Decision.Verdict == domain.VerdictWouldUpdate {
		t.Fatal("the strict floor no longer under-reports; revisit the preset table")
	}
}

// TestTheProceedFloorRefusesTheWatchtowerWorkload records the defect this
// stage fixed, so it cannot come back quietly.
//
// This is the measurement that decided the preset table. It asserts the FAILING
// behaviour deliberately: with the strict floor, the flagship preset's own
// workload is skipped with `recommendation`. If a future change to the risk
// model makes this test fail, the strict floor has become viable again and the
// preset table should be revisited -- but that must be a decision somebody
// makes, not something that drifts.
func TestTheProceedFloorRefusesTheWatchtowerWorkload(t *testing.T) {
	plan := presetPlan(domain.UpdateDigest, "latest", 1, 6)

	if plan.Risk.Recommendation != domain.RecommendCaution {
		t.Fatalf("expected the measured recommendation to be %q, got %q (score %d, band %s)",
			domain.RecommendCaution, plan.Risk.Recommendation,
			plan.Risk.Score, plan.Risk.Band)
	}

	outcome := service.DecideAutomation(
		presetInput(presetPolicy(domain.StrategyDigestOnly, domain.RecommendProceed), plan))

	if outcome.Eligible() {
		t.Fatal("the strict floor no longer refuses this; revisit the preset table")
	}
	if outcome.Decision.Reason != domain.ReasonRecommendation {
		t.Fatalf("reason = %q, want %q", outcome.Decision.Reason, domain.ReasonRecommendation)
	}
}
