package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Readiness semantics, and the Stage 17.3 presets measured through them.
//
// # Why the plans here are assessed rather than asserted
//
// Every plan below takes its recommendation from `domain.AssessRisk` over real
// inputs. Setting `Recommendation` by hand would make these tests agree with
// whatever the risk model does, which is exactly the drift they exist to catch
// -- and it would let the flagship case pass while the real one failed.
//
// # The preset field values
//
// Mirrored from `web/src/api/automationPresets.ts`, which is the only place
// they are defined. Go cannot import it, so the contract is written in two
// halves that must agree: `TestEveryPresetShapeIsAPolicyTheAPIAccepts` pins
// that these shapes are policies the API accepts, and the frontend's own tests
// pin that the presets compile to them. This file measures what they DO.

var readinessAt = time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)

// presetPolicyFor builds the policy one Stage 17.3 preset compiles.
func presetPolicyFor(
	name string,
	strategy domain.UpdateStrategy,
	mode domain.AutomationMode,
) domain.UpdatePolicy {
	policy := domain.UpdatePolicy{
		PolicyID:              domain.AutomationReadinessCandidatePolicyID,
		Name:                  name,
		Enabled:               true,
		Scope:                 domain.ScopeAllEligible,
		Strategy:              strategy,
		MinimumRecommendation: domain.RecommendCaution,
		Mode:                  mode,
		Window:                domain.MaintenanceWindow{AlwaysOpen: true},
		Failure:               domain.UpdateFailureHandling{AutoRollback: true, PauseAfterFailures: 2},
	}
	policy.Normalise()
	return policy
}

// assessedPlan builds a plan whose risk comes from the real model.
func assessedPlan(
	name, tag string,
	kind domain.UpdateType,
	publishedHoursAgo int,
) domain.ChangePlan {
	published := readinessAt.Add(-time.Duration(publishedHoursAgo) * time.Hour)

	risk := domain.AssessRisk(domain.PlanInputs{
		ContainerID:    "container-" + name,
		ContainerName:  name,
		CurrentImage:   "nginx:" + tag,
		ProposedImage:  "nginx:" + tag,
		CurrentDigest:  "sha256:" + repeatHex('a'),
		ProposedDigest: "sha256:" + repeatHex('b'),
		CurrentTag:     tag,
		UpdateType:     kind,

		// Phase 17.2 snapshot assurance is what makes this non-zero in practice.
		SnapshotID:       7,
		RestoreReadiness: domain.ReadinessReady,
		RegistryStatus:   domain.CheckOK,
		LocalPlatform:    domain.Platform{OS: "linux", Architecture: "amd64"},

		ProposedPublishedAt: &published,
		ContainerCount:      1,
		EvaluatedAt:         readinessAt,
	})

	return domain.ChangePlan{
		PlanID:         "plan_" + padAutoID(planSeedFor(name)),
		ContainerID:    "container-" + name,
		ContainerName:  name,
		CurrentImage:   "nginx:" + tag,
		ProposedImage:  "nginx:" + tag,
		CurrentDigest:  "sha256:" + repeatHex('a'),
		ProposedDigest: "sha256:" + repeatHex('b'),
		UpdateType:     kind,
		Risk:           risk,
	}
}

// readinessTarget builds one screened target.
func readinessTarget(name string, labels map[string]string) store.AutomationTarget {
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

// presetHarness wires an estate with no saved policies, so a candidate policy
// is the only rule in play and attribution is unambiguous.
func presetHarness(
	t *testing.T,
	targets []store.AutomationTarget,
	plans map[string]domain.ChangePlan,
) *automationHarness {
	t.Helper()

	harness := newAutomationHarness(t, []domain.UpdatePolicy{}...)
	harness.policies.policies = nil
	harness.evidence.targets = targets
	harness.evidence.plans = plans
	harness.evidence.inFlight = map[string]bool{}
	harness.now = readinessAt

	options := harness.optionsWithSelf(domain.SelfIdentity{ContainerName: "harbormaster"})
	harness.engine = service.NewAutomationService(options)
	return harness
}

// TestFollowCurrentTagCountsTheWatchtowerWorkload is the Stage 17.3 flagship
// case, measured end to end through readiness.
//
// A healthy `image:latest` whose digest moved, with current snapshot evidence,
// under the Follow current tag preset. Stage 17.3 established that this scores
// 32 -- `medium` on caution factors alone -- and therefore needs the caution
// floor. This asserts the consequence an operator actually sees: the container
// is COUNTED as currently eligible.
func TestFollowCurrentTagCountsTheWatchtowerWorkload(t *testing.T) {
	plan := assessedPlan("web", "latest", domain.UpdateDigest, 6)
	if plan.Risk.Recommendation != domain.RecommendCaution {
		t.Fatalf("fixture: expected the measured recommendation %q, got %q (score %d)",
			domain.RecommendCaution, plan.Risk.Recommendation, plan.Risk.Score)
	}

	harness := presetHarness(t,
		[]store.AutomationTarget{readinessTarget("web", nil)},
		map[string]domain.ChangePlan{"container-web": plan})

	policy := presetPolicyFor("Follow current tag",
		domain.StrategyDigestOnly, domain.ModeAutomatic)

	report, _, err := harness.engine.Readiness(context.Background(), &policy)
	if err != nil {
		t.Fatalf("readiness: %v", err)
	}

	if report.Eligible != 1 {
		t.Fatalf("eligible = %d, want 1 (governed %d, groups %+v)",
			report.Eligible, report.Governed, report.Groups)
	}
	if report.Governed != 1 {
		t.Fatalf("governed = %d, want 1", report.Governed)
	}
	if len(report.Groups) != 0 {
		t.Fatalf("nothing should need explaining, got %+v", report.Groups)
	}
}

// TestReadinessDoesNotCountWhatAutomationWillNotDo is the negative half of §8.
//
// Each case is a container the preset governs and would not update. The point
// is not merely that Eligible is 0 -- it is that the REASON is the accurate one,
// because the reason is what the operator reads.
func TestReadinessDoesNotCountWhatAutomationWillNotDo(t *testing.T) {
	cases := []struct {
		name     string
		plan     *domain.ChangePlan
		labels   map[string]string
		strategy domain.UpdateStrategy
		want     domain.AutomationReason
	}{
		{
			// The CEILING refuses it, not the recommendation.
			//
			// Step 7 runs before step 8, so a major update under a minor
			// ceiling never reaches the recommendation gate -- and that is the
			// narrower true answer for an operator: the policy does not permit
			// major versions at all, whatever the planner thought of this one.
			// No preset compiles a major ceiling, so this is the reason a
			// preset-configured policy always gives for a major version.
			name:     "a major version is outside every preset's ceiling",
			plan:     ptr(assessedPlan("web", "1.27.4", domain.UpdateMajor, 6)),
			strategy: domain.StrategyMinor,
			want:     domain.ReasonStrategy,
		},
		{
			// Inside the ceiling, and still held: this one reaches step 8.
			name:     "manual review is held for a person",
			plan:     ptr(manualReviewPlan("web")),
			strategy: domain.StrategyDigestOnly,
			want:     domain.ReasonRecommendation,
		},
		{
			name:     "an unknown assessment is not permission",
			plan:     ptr(unknownPlan("web")),
			strategy: domain.StrategyMinor,
			want:     domain.ReasonStrategy,
		},
		{
			name:     "nothing to do is not the same as cannot tell",
			plan:     ptr(noUpdatePlan("web")),
			strategy: domain.StrategyMinor,
			want:     domain.ReasonNoUpdate,
		},
		{
			name:     "an opted-out container is never counted",
			plan:     ptr(assessedPlan("web", "latest", domain.UpdateDigest, 6)),
			labels:   map[string]string{domain.LabelUpdateEnabled: "false"},
			strategy: domain.StrategyDigestOnly,
			want:     domain.ReasonLabelOff,
		},
		{
			name:     "a container with no plan cannot be assessed",
			plan:     nil,
			strategy: domain.StrategyDigestOnly,
			want:     domain.ReasonNoPlan,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plans := map[string]domain.ChangePlan{}
			if tc.plan != nil {
				plans["container-web"] = *tc.plan
			}
			harness := presetHarness(t,
				[]store.AutomationTarget{readinessTarget("web", tc.labels)}, plans)

			policy := presetPolicyFor("preset", tc.strategy, domain.ModeAutomatic)
			report, decisions, err := harness.engine.Readiness(context.Background(), &policy)
			if err != nil {
				t.Fatalf("readiness: %v", err)
			}

			if report.Eligible != 0 {
				t.Fatalf("eligible = %d, want 0", report.Eligible)
			}
			if got := decisions[0].Reason; got != tc.want {
				t.Fatalf("reason = %q, want %q (detail: %s)",
					got, tc.want, decisions[0].Detail)
			}
		})
	}
}

// TestReadinessNeverCountsHarborMaster is the self-update case.
//
// Checked separately because it is refused at step 0, before the policy is even
// looked up -- so it must be absent from the count whatever the policy says,
// including a broad one that selects everything.
func TestReadinessNeverCountsHarborMaster(t *testing.T) {
	plan := assessedPlan("harbormaster", "latest", domain.UpdateDigest, 6)
	harness := presetHarness(t,
		[]store.AutomationTarget{readinessTarget("harbormaster", nil)},
		map[string]domain.ChangePlan{"container-harbormaster": plan})

	policy := presetPolicyFor("Follow current tag",
		domain.StrategyDigestOnly, domain.ModeAutomatic)

	report, decisions, err := harness.engine.Readiness(context.Background(), &policy)
	if err != nil {
		t.Fatalf("readiness: %v", err)
	}
	if report.Eligible != 0 {
		t.Fatalf("eligible = %d, want 0", report.Eligible)
	}
	if decisions[0].Reason != domain.ReasonSelfUpdate {
		t.Fatalf("reason = %q, want %q", decisions[0].Reason, domain.ReasonSelfUpdate)
	}
}

// TestObserveIsCountedApartFromEligible keeps the two answers separate.
//
// "Would update, but the mode forbids it" and "will update" are different
// answers to the operator's question, and collapsing them would let an observe
// policy report a number that reads as a promise to act.
func TestObserveIsCountedApartFromEligible(t *testing.T) {
	plan := assessedPlan("web", "latest", domain.UpdateDigest, 6)
	harness := presetHarness(t,
		[]store.AutomationTarget{readinessTarget("web", nil)},
		map[string]domain.ChangePlan{"container-web": plan})

	policy := presetPolicyFor("Observe only", domain.StrategyPatch, domain.ModeObserve)

	report, _, err := harness.engine.Readiness(context.Background(), &policy)
	if err != nil {
		t.Fatalf("readiness: %v", err)
	}

	if report.Eligible != 0 {
		t.Fatalf("eligible = %d, want 0: an observe policy changes nothing", report.Eligible)
	}
	if report.Observing != 1 {
		t.Fatalf("observing = %d, want 1", report.Observing)
	}
	// And it is explained rather than silently absent.
	if len(report.Groups) != 1 || report.Groups[0].Reason != domain.ReasonObserveMode {
		t.Fatalf("groups = %+v, want one observeMode group", report.Groups)
	}
	if report.Groups[0].Explanation == "" {
		t.Fatal("a group must carry HarborMaster's own sentence, not just an enum")
	}
}

// TestReadinessAttributesOnlyThisPolicysContainers pins what N means.
//
// A container another policy outranks is not this policy's to count. Getting
// this wrong would credit a preview with containers it will never govern, which
// is the over-reporting direction.
func TestReadinessAttributesOnlyThisPolicysContainers(t *testing.T) {
	plan := assessedPlan("web", "latest", domain.UpdateDigest, 6)

	harness := presetHarness(t,
		[]store.AutomationTarget{readinessTarget("web", nil)},
		map[string]domain.ChangePlan{"container-web": plan})

	// A saved policy that outranks any candidate on priority.
	saved := presetPolicyFor("existing", domain.StrategyDigestOnly, domain.ModeAutomatic)
	saved.PolicyID = "upd_" + repeatHexN('1', 20)
	saved.Priority = 100
	saved.Normalise()
	harness.policies.policies = []domain.UpdatePolicy{saved}

	candidate := presetPolicyFor("candidate", domain.StrategyDigestOnly, domain.ModeAutomatic)

	report, decisions, err := harness.engine.Readiness(context.Background(), &candidate)
	if err != nil {
		t.Fatalf("readiness: %v", err)
	}

	// The estate still has one eligible container -- but under the OTHER policy.
	if decisions[0].PolicyID != saved.PolicyID {
		t.Fatalf("the higher-priority policy must win, got %q", decisions[0].PolicyID)
	}
	if report.Governed != 0 {
		t.Fatalf("governed = %d, want 0: this candidate governs nothing here", report.Governed)
	}
	if report.Eligible != 0 {
		t.Fatalf("eligible = %d, want 0", report.Eligible)
	}
	if report.Considered != 1 {
		t.Fatalf("considered = %d, want 1", report.Considered)
	}
}

// TestReadinessPreviewsAnEditWithoutTouchingTheStoredPolicy is the Stage 17.3
// compatibility case the brief names.
//
// A stored policy with the STRICT floor previews according to what is stored,
// not according to the preset that now uses the caution floor. Nothing is
// written, and the stored policy is unchanged afterwards.
func TestReadinessPreviewsAnEditWithoutTouchingTheStoredPolicy(t *testing.T) {
	plan := assessedPlan("web", "latest", domain.UpdateDigest, 6)
	harness := presetHarness(t,
		[]store.AutomationTarget{readinessTarget("web", nil)},
		map[string]domain.ChangePlan{"container-web": plan})

	// The pre-17.3 shape: digest-only, automatic, and the STRICT floor.
	stored := presetPolicyFor("legacy", domain.StrategyDigestOnly, domain.ModeAutomatic)
	stored.PolicyID = "upd_" + repeatHexN('2', 20)
	stored.MinimumRecommendation = domain.RecommendProceed
	stored.Normalise()
	harness.policies.policies = []domain.UpdatePolicy{stored}

	before := stored

	// Previewing it as stored: the strict floor refuses the caution-band
	// workload, exactly as it does in a real pass.
	report, decisions, err := harness.engine.Readiness(context.Background(), &stored)
	if err != nil {
		t.Fatalf("readiness: %v", err)
	}
	if report.Eligible != 0 {
		t.Fatalf("eligible = %d, want 0: the stored floor is strict", report.Eligible)
	}
	if decisions[0].Reason != domain.ReasonRecommendation {
		t.Fatalf("reason = %q, want %q", decisions[0].Reason, domain.ReasonRecommendation)
	}

	// Previewing the EDIT: the same policy with the preset's floor. The preview
	// measures the edit rather than what is stored.
	edited := stored
	edited.MinimumRecommendation = domain.RecommendCaution
	edited.Normalise()

	report, _, err = harness.engine.Readiness(context.Background(), &edited)
	if err != nil {
		t.Fatalf("readiness: %v", err)
	}
	if report.Eligible != 1 {
		t.Fatalf("eligible = %d, want 1 under the edited floor", report.Eligible)
	}

	// And the stored policy is untouched by either preview.
	if harness.policies.policies[0].MinimumRecommendation != before.MinimumRecommendation {
		t.Fatal("previewing an edit must not rewrite the stored policy")
	}
	if len(harness.store.runs) != 0 {
		t.Fatal("a readiness preview must not record an automation run")
	}
}

func ptr[T any](value T) *T { return &value }

// manualReviewPlan is a digest move a person has to look at.
//
// Inside every acting preset's ceiling, so it reaches the recommendation gate
// rather than being refused earlier. The recommendation is measured, not set:
// a failed restore-readiness check is a WARNING-severity factor, and a warning
// is what turns the caution band into `manualReview`.
func manualReviewPlan(name string) domain.ChangePlan {
	published := readinessAt.Add(-6 * time.Hour)

	risk := domain.AssessRisk(domain.PlanInputs{
		ContainerID:    "container-" + name,
		ContainerName:  name,
		CurrentImage:   "nginx:latest",
		ProposedImage:  "nginx:latest",
		CurrentDigest:  "sha256:" + repeatHex('a'),
		ProposedDigest: "sha256:" + repeatHex('b'),
		CurrentTag:     "latest",
		UpdateType:     domain.UpdateDigest,

		SnapshotID: 7,
		// The warning factor.
		RestoreReadiness: domain.ReadinessNotReady,
		RegistryStatus:   domain.CheckOK,
		LocalPlatform:    domain.Platform{OS: "linux", Architecture: "amd64"},

		ProposedPublishedAt: &published,
		ContainerCount:      1,
		EvaluatedAt:         readinessAt,
	})

	plan := assessedPlan(name, "latest", domain.UpdateDigest, 6)
	plan.Risk = risk
	return plan
}

// unknownPlan is an assessment HarborMaster could not complete.
func unknownPlan(name string) domain.ChangePlan {
	plan := assessedPlan(name, "1.27.4", domain.UpdateUnknown, 6)
	plan.UpdateType = domain.UpdateUnknown
	return plan
}

// noUpdatePlan is a positive finding that nothing needs doing.
func noUpdatePlan(name string) domain.ChangePlan {
	plan := assessedPlan(name, "1.27.4", domain.UpdateNone, 6)
	plan.UpdateType = domain.UpdateNone
	plan.ProposedImage = ""
	return plan
}

// repeatHexN builds a run of one character.
func repeatHexN(c byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = c
	}
	return string(out)
}

// TestReadinessWritesNothing is the §4 invariant, asserted behaviourally.
//
// The architecture guards prove the readiness source names no mutation path.
// This proves the observable half: after a preview, nothing on the estate or in
// the store has moved. Both matter -- a structural guard cannot see a write
// reached through a helper, and a behavioural one cannot see a capability that
// exists but was not exercised.
func TestReadinessWritesNothing(t *testing.T) {
	plan := assessedPlan("web", "latest", domain.UpdateDigest, 6)
	harness := presetHarness(t,
		[]store.AutomationTarget{readinessTarget("web", nil)},
		map[string]domain.ChangePlan{"container-web": plan})

	policy := presetPolicyFor("Follow current tag",
		domain.StrategyDigestOnly, domain.ModeAutomatic)

	// Called repeatedly, because the editor calls it on every edit and a write
	// that only happens on the second call would be worse, not better.
	for range 3 {
		if _, _, err := harness.engine.Readiness(context.Background(), &policy); err != nil {
			t.Fatalf("readiness: %v", err)
		}
	}

	harness.store.mu.Lock()
	runs, recorded, pauses := len(harness.store.runs), len(harness.store.decisions), len(harness.store.pauses)
	harness.store.mu.Unlock()

	switch {
	case runs != 0:
		t.Fatalf("a preview recorded %d automation runs", runs)
	case recorded != 0:
		t.Fatalf("a preview recorded %d decisions", recorded)
	case pauses != 0:
		t.Fatalf("a preview recorded %d pauses", pauses)
	}

	// No acquisition, no execution, no rollback. The pipeline is the ONLY way
	// this service can affect the host, and it was never reached.
	for _, kind := range []string{"acquire", "execute", "rollback"} {
		if got := len(harness.pipeline.recorded(kind)); got != 0 {
			t.Fatalf("a preview made %d %s requests", got, kind)
		}
	}

	// And the policy set is exactly as it was: a candidate is never stored.
	if len(harness.policies.policies) != 0 {
		t.Fatalf("a preview added %d policies", len(harness.policies.policies))
	}
}
