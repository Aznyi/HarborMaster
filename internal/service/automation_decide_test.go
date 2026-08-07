package service_test

import (
	"testing"
	"time"

	_ "time/tzdata"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Decision tests.
//
// DecideAutomation is a pure function of its inputs, which is what lets the
// most consequential judgement HarborMaster makes be tested exhaustively
// without a host, a database, or a clock.
//
// The tests below are organised by the check they exercise, in the order the
// function applies them, because that order IS the security design: cheapest
// and most absolute first, so a container that must never be touched is refused
// before anything expensive or fallible runs.

// decideAt is the instant every test decides against: 03:00 UTC on a Sunday,
// which is inside the fixture window and on a fixture weekday.
var decideAt = time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)

func automaticPolicy() domain.UpdatePolicy {
	policy := domain.UpdatePolicy{
		PolicyID:              "upd_aaaaaaaaaaaaaaaaaaaa",
		Name:                  "Nightly patches",
		Enabled:               true,
		Priority:              10,
		Selector:              domain.UpdateSelector{Include: []string{"web"}},
		Strategy:              domain.StrategyPatch,
		MinimumRecommendation: domain.RecommendProceed,
		Mode:                  domain.ModeAutomatic,
		Window:                domain.MaintenanceWindow{Start: "02:00", End: "04:00"},
		Failure:               domain.UpdateFailureHandling{AutoRollback: true, PauseAfterFailures: 2},
	}
	policy.Normalise()
	return policy
}

func patchPlan() domain.ChangePlan {
	return domain.ChangePlan{
		PlanID:         "plan_0123456789abcdef0123",
		ContainerID:    "container-web",
		ContainerName:  "web",
		CurrentImage:   "nginx:1.27.3",
		ProposedImage:  "nginx:1.27.4",
		CurrentDigest:  "sha256:" + repeatHex('a'),
		ProposedDigest: "sha256:" + repeatHex('b'),
		UpdateType:     domain.UpdatePatch,
		Risk:           domain.RiskAssessment{Recommendation: domain.RecommendProceed},
	}
}

func repeatHex(c byte) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = c
	}
	return string(out)
}

func eligibleInput() service.AutomationInput {
	return service.AutomationInput{
		Target: domain.SelectionTarget{
			Name:  "web",
			Image: "nginx:1.27.3",
		},
		ContainerID: "container-web",
		Policies:    []domain.UpdatePolicy{automaticPolicy()},
		Plan:        patchPlan(),
		HasPlan:     true,
		Now:         decideAt,
	}
}

func TestAnEligibleContainerIsSubmitted(t *testing.T) {
	outcome := service.DecideAutomation(eligibleInput())

	if !outcome.Eligible() {
		t.Fatalf("verdict = %q, reason = %q, detail = %q",
			outcome.Decision.Verdict, outcome.Decision.Reason, outcome.Decision.Detail)
	}
	if outcome.Decision.PlanID != "plan_0123456789abcdef0123" {
		t.Fatalf("the decision must name the plan it acted on, got %q", outcome.Decision.PlanID)
	}
	if outcome.Decision.PolicyID != "upd_aaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("the decision must name the governing policy, got %q", outcome.Decision.PolicyID)
	}
	if outcome.Decision.ProposedDigest != patchPlan().ProposedDigest {
		t.Fatal("the decision must carry the digest the plan resolved")
	}
}

// ------------------------------------------------- the order of the checks --

func TestAPauseOutranksEveryPolicy(t *testing.T) {
	// Checked FIRST, and before the policy lookup. A pause was recorded because
	// something went wrong, and a policy edit must not clear it.
	input := eligibleInput()
	input.IsPaused = true
	input.Pause = domain.PausedContainer{
		ContainerName: "web",
		Reason:        domain.PauseRolledBack,
		Failures:      1,
		PausedAt:      decideAt.Add(-time.Hour),
	}

	outcome := service.DecideAutomation(input)
	if outcome.Eligible() {
		t.Fatal("a paused container must never be updated")
	}
	if outcome.Decision.Reason != domain.ReasonPaused {
		t.Fatalf("reason = %q, want automationPaused", outcome.Decision.Reason)
	}
	if outcome.Decision.Detail == "" {
		t.Fatal("a paused container must say why, and whether a person has to clear it")
	}
}

func TestAnExpiredCooldownStopsBlocking(t *testing.T) {
	input := eligibleInput()
	resume := decideAt.Add(-time.Minute)
	input.IsPaused = true
	input.Pause = domain.PausedContainer{
		ContainerName: "web",
		Reason:        domain.PauseRepeatedFailure,
		PausedAt:      decideAt.Add(-2 * time.Hour),
		ResumeAfter:   &resume,
	}

	if outcome := service.DecideAutomation(input); !outcome.Eligible() {
		t.Fatalf("an elapsed cooldown no longer blocks: reason = %q", outcome.Decision.Reason)
	}
}

func TestALabelOptOutIsHonouredEvenWithNoPolicy(t *testing.T) {
	// Read before the policy is selected, so the label means the same thing
	// whatever the policy set looks like.
	input := eligibleInput()
	input.Policies = nil
	input.Target.Labels = map[string]string{domain.LabelUpdateEnabled: "false"}

	outcome := service.DecideAutomation(input)
	if outcome.Decision.Reason != domain.ReasonLabelOff {
		t.Fatalf("reason = %q, want labelDisabled", outcome.Decision.Reason)
	}
	if outcome.Governed {
		t.Fatal("no policy governs, and the label decided before that was even asked")
	}
}

func TestALabelCannotEnrolAContainerNoPolicySelected(t *testing.T) {
	// The asymmetry the whole design rests on: anyone who can run `docker run`
	// can set a label, so a label must never opt a container INTO automation.
	input := eligibleInput()
	input.Target.Name = "not-selected"
	input.Target.Labels = map[string]string{
		domain.LabelUpdateEnabled:  "true",
		domain.LabelUpdateStrategy: "major",
	}

	outcome := service.DecideAutomation(input)
	if outcome.Eligible() {
		t.Fatal("a label must never make an unselected container eligible")
	}
	if outcome.Decision.Reason != domain.ReasonNotSelected {
		t.Fatalf("reason = %q, want notSelected", outcome.Decision.Reason)
	}
}

func TestNoPolicyAndNoPoliciesAtAllReadDifferently(t *testing.T) {
	// "Nothing is configured" and "nothing matches" are different problems with
	// different fixes, and a UI that rendered them the same would send an
	// operator to the wrong page.
	empty := eligibleInput()
	empty.Policies = nil
	if got := service.DecideAutomation(empty).Decision.Reason; got != domain.ReasonNoPolicy {
		t.Fatalf("reason = %q, want noPolicy", got)
	}

	unmatched := eligibleInput()
	unmatched.Target.Name = "cache"
	if got := service.DecideAutomation(unmatched).Decision.Reason; got != domain.ReasonNotSelected {
		t.Fatalf("reason = %q, want notSelected", got)
	}
}

func TestAContainerWithNoPlanIsNotUpdated(t *testing.T) {
	// A missing plan means the planner has not assessed this container, and
	// automating a change nobody assessed is exactly what must not happen.
	input := eligibleInput()
	input.HasPlan = false
	input.Plan = domain.ChangePlan{}

	outcome := service.DecideAutomation(input)
	if outcome.Eligible() {
		t.Fatal("a container with no change plan must not be updated")
	}
	if outcome.Decision.Reason != domain.ReasonNoPlan {
		t.Fatalf("reason = %q, want noPlan", outcome.Decision.Reason)
	}
}

func TestACrossedPlanTargetIsRefused(t *testing.T) {
	// The Phase 10.1 defect class: a plan whose proposed reference and proposed
	// digest were not resolved for each other. The engine must not be the
	// component that carries one forward.
	input := eligibleInput()
	input.Plan.ProposedImage = ""
	input.Plan.ProposedDigest = "sha256:" + repeatHex('c')
	input.Plan.UpdateType = domain.UpdatePatch

	outcome := service.DecideAutomation(input)
	if outcome.Eligible() {
		t.Fatal("a plan with no proposed reference proposes nothing")
	}
}

func TestTheStrategyIsACeiling(t *testing.T) {
	cases := []struct {
		name       string
		strategy   domain.UpdateStrategy
		updateType domain.UpdateType
		eligible   bool
	}{
		{"digestOnly refuses a patch", domain.StrategyDigestOnly, domain.UpdatePatch, false},
		{"digestOnly permits a digest move", domain.StrategyDigestOnly, domain.UpdateDigest, true},
		{"patch permits a patch", domain.StrategyPatch, domain.UpdatePatch, true},
		{"patch refuses a minor", domain.StrategyPatch, domain.UpdateMinor, false},
		{"minor permits a minor", domain.StrategyMinor, domain.UpdateMinor, true},
		{"minor refuses a major", domain.StrategyMinor, domain.UpdateMajor, false},
		// The two nobody may automate, whatever the ceiling says.
		{"major refuses an unknown", domain.StrategyMajor, domain.UpdateUnknown, false},
		{"major refuses a prerelease", domain.StrategyMajor, domain.UpdatePrerelease, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := automaticPolicy()
			policy.Strategy = tc.strategy

			input := eligibleInput()
			input.Policies = []domain.UpdatePolicy{policy}
			input.Plan.UpdateType = tc.updateType

			outcome := service.DecideAutomation(input)
			if outcome.Eligible() != tc.eligible {
				t.Fatalf("eligible = %v, want %v (reason %q)",
					outcome.Eligible(), tc.eligible, outcome.Decision.Reason)
			}
			if !tc.eligible && outcome.Decision.Reason != domain.ReasonStrategy &&
				outcome.Decision.Reason != domain.ReasonNoUpdate {
				t.Fatalf("reason = %q, want strategyCeiling", outcome.Decision.Reason)
			}
		})
	}
}

func TestTheDeploymentMayRefuseUnattendedMajorUpdates(t *testing.T) {
	// A policy may say major; the deployment says not without a person.
	policy := automaticPolicy()
	policy.Strategy = domain.StrategyMajor

	input := eligibleInput()
	input.Policies = []domain.UpdatePolicy{policy}
	input.Plan.UpdateType = domain.UpdateMajor
	input.RequireApprovalForMajor = true

	outcome := service.DecideAutomation(input)
	if outcome.Eligible() {
		t.Fatal("the deployment-wide refusal must hold over the policy's strategy")
	}
	if outcome.Decision.Verdict != domain.VerdictAwaitingApproval {
		t.Fatalf("verdict = %q, want awaitingApproval -- the change is not refused, it is held",
			outcome.Decision.Verdict)
	}

	// Off, the policy's own ceiling governs.
	input.RequireApprovalForMajor = false
	if outcome := service.DecideAutomation(input); !outcome.Eligible() {
		t.Fatalf("with the override off a major-strategy policy applies a major: reason %q",
			outcome.Decision.Reason)
	}
}

func TestTheRecommendationGateIsAnAllowlist(t *testing.T) {
	cases := []struct {
		minimum  domain.Recommendation
		actual   domain.Recommendation
		eligible bool
	}{
		{domain.RecommendProceed, domain.RecommendProceed, true},
		{domain.RecommendProceed, domain.RecommendCaution, false},
		{domain.RecommendCaution, domain.RecommendProceed, true},
		{domain.RecommendCaution, domain.RecommendCaution, true},
		// The three that mean a person has to look.
		{domain.RecommendCaution, domain.RecommendManualReview, false},
		{domain.RecommendCaution, domain.RecommendAgainst, false},
		{domain.RecommendCaution, domain.RecommendUnknown, false},
	}
	for _, tc := range cases {
		policy := automaticPolicy()
		policy.MinimumRecommendation = tc.minimum

		input := eligibleInput()
		input.Policies = []domain.UpdatePolicy{policy}
		input.Plan.Risk.Recommendation = tc.actual

		outcome := service.DecideAutomation(input)
		if outcome.Eligible() != tc.eligible {
			t.Fatalf("minimum %q with actual %q: eligible = %v, want %v",
				tc.minimum, tc.actual, outcome.Eligible(), tc.eligible)
		}
	}
}

func TestAClosedWindowRefusesAndSaysWhen(t *testing.T) {
	input := eligibleInput()
	input.Now = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) // midday, window is 02:00-04:00

	outcome := service.DecideAutomation(input)
	if outcome.Eligible() {
		t.Fatal("a closed maintenance window refuses")
	}
	if outcome.Decision.Reason != domain.ReasonWindowClosed {
		t.Fatalf("reason = %q, want windowClosed", outcome.Decision.Reason)
	}
	if outcome.Decision.Detail == "" {
		t.Fatal("a closed window must say when it opens")
	}
}

func TestAnUnresolvableTimezoneFailsClosed(t *testing.T) {
	policy := automaticPolicy()
	policy.Window = domain.MaintenanceWindow{
		Timezone: "Mars/Olympus_Mons",
		Start:    "00:00",
		End:      "23:59",
	}

	input := eligibleInput()
	input.Policies = []domain.UpdatePolicy{policy}

	outcome := service.DecideAutomation(input)
	if outcome.Eligible() {
		t.Fatal("a window nobody can evaluate authorises nothing")
	}
	if outcome.Decision.Reason != domain.ReasonWindowInvalid {
		t.Fatalf("reason = %q, want windowUnresolvable", outcome.Decision.Reason)
	}
}

func TestWorkAlreadyInFlightIsNotDuplicated(t *testing.T) {
	input := eligibleInput()
	input.InFlight = true

	outcome := service.DecideAutomation(input)
	if outcome.Eligible() {
		t.Fatal("a second update on top of the first is two concurrent recreations")
	}
	if outcome.Decision.Reason != domain.ReasonInFlight {
		t.Fatalf("reason = %q, want alreadyInFlight", outcome.Decision.Reason)
	}
}

func TestAClosedWindowIsReportedBeforeInFlight(t *testing.T) {
	// Two true answers; the window is the more useful one, because it is the
	// one an operator can act on.
	input := eligibleInput()
	input.InFlight = true
	input.Now = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	if got := service.DecideAutomation(input).Decision.Reason; got != domain.ReasonWindowClosed {
		t.Fatalf("reason = %q, want windowClosed", got)
	}
}

// ------------------------------------------------------------------ modes --

func TestOnlyAutomaticModeActs(t *testing.T) {
	cases := []struct {
		mode    domain.AutomationMode
		verdict domain.AutomationVerdict
		reason  domain.AutomationReason
	}{
		{domain.ModeObserve, domain.VerdictWouldUpdate, domain.ReasonObserveMode},
		{domain.ModeDryRun, domain.VerdictWouldUpdate, domain.ReasonDryRunMode},
		{domain.ModeApprove, domain.VerdictAwaitingApproval, domain.ReasonNeedApproval},
		{domain.ModeAutomatic, domain.VerdictUpdate, domain.ReasonEligible},
	}
	for _, tc := range cases {
		policy := automaticPolicy()
		policy.Mode = tc.mode

		input := eligibleInput()
		input.Policies = []domain.UpdatePolicy{policy}

		outcome := service.DecideAutomation(input)
		if outcome.Decision.Verdict != tc.verdict || outcome.Decision.Reason != tc.reason {
			t.Fatalf("%s: verdict = %q reason = %q, want %q / %q",
				tc.mode, outcome.Decision.Verdict, outcome.Decision.Reason, tc.verdict, tc.reason)
		}
		if tc.mode != domain.ModeAutomatic && outcome.Eligible() {
			t.Fatalf("%s must not act", tc.mode)
		}
	}
}

func TestAnUnrecognisedModeFailsClosed(t *testing.T) {
	// A mode this build does not understand must not fall through into the
	// branch that changes the host.
	policy := automaticPolicy()
	policy.Mode = domain.AutomationMode("yolo")

	input := eligibleInput()
	input.Policies = []domain.UpdatePolicy{policy}

	outcome := service.DecideAutomation(input)
	if outcome.Eligible() {
		t.Fatal("an unrecognised mode must never act")
	}
	if outcome.Decision.Reason != domain.ReasonRefused {
		t.Fatalf("reason = %q, want refusedByService", outcome.Decision.Reason)
	}
}

// ------------------------------------------------------------ precedence --

func TestLabelNarrowingIsAppliedToTheDecision(t *testing.T) {
	// A minor-strategy policy plus a digestOnly label refuses a patch update.
	policy := automaticPolicy()
	policy.Strategy = domain.StrategyMinor

	input := eligibleInput()
	input.Policies = []domain.UpdatePolicy{policy}
	input.Target.Labels = map[string]string{
		domain.LabelUpdateStrategy: string(domain.StrategyDigestOnly),
	}

	outcome := service.DecideAutomation(input)
	if outcome.Eligible() {
		t.Fatal("the narrowed ceiling must govern the decision")
	}
	if outcome.Decision.Reason != domain.ReasonStrategy {
		t.Fatalf("reason = %q, want strategyCeiling", outcome.Decision.Reason)
	}
}

func TestAWindowLabelNarrowsTheDecisionsTiming(t *testing.T) {
	input := eligibleInput()
	// The policy's window is 02:00-04:00 and it is 03:00. The label moves the
	// window to 05:00-06:00, which is closed now.
	input.Target.Labels = map[string]string{domain.LabelUpdateWindow: "05:00-06:00"}

	outcome := service.DecideAutomation(input)
	if outcome.Eligible() {
		t.Fatal("the label's window must govern the decision")
	}
	if outcome.Decision.Reason != domain.ReasonWindowClosed {
		t.Fatalf("reason = %q, want windowClosed", outcome.Decision.Reason)
	}
}

func TestTheHighestPriorityPolicyDecides(t *testing.T) {
	observe := automaticPolicy()
	observe.PolicyID = "upd_bbbbbbbbbbbbbbbbbbbb"
	observe.Name = "Observe everything"
	observe.Mode = domain.ModeObserve
	observe.Priority = 100

	automatic := automaticPolicy()
	automatic.Priority = 1

	input := eligibleInput()
	input.Policies = []domain.UpdatePolicy{automatic, observe}

	outcome := service.DecideAutomation(input)
	if outcome.Decision.PolicyID != observe.PolicyID {
		t.Fatalf("policy = %q, want the priority-100 one", outcome.Decision.PolicyID)
	}
	if outcome.Eligible() {
		t.Fatal("the winning policy is in observe mode and must not act")
	}
}

// -------------------------------------------------------------- budgeting --

func TestTheRunBudgetStopsAfterMaxPerRun(t *testing.T) {
	// The policy's own limits are raised so the ENGINE ceiling is the one under
	// test. With the defaults the policy's maxConcurrent of 1 would refuse the
	// second update first, which is correct but is a different test.
	policy := automaticPolicy()
	policy.Limits.MaxConcurrent = 8
	policy.Limits.MaxPerRegistry = 8
	policy.Limits.MaxPerRun = 8

	budget := service.NewAutomationBudget(2, 10, 0, nil)
	effective := domain.Resolve(policy, nil)

	for i := 0; i < 2; i++ {
		if _, ok := budget.Admit(domain.AutomationDecision{}, effective); !ok {
			t.Fatalf("update %d should have been admitted", i+1)
		}
	}
	reason, ok := budget.Admit(domain.AutomationDecision{}, effective)
	if ok {
		t.Fatal("the third update exceeds maxPerRun")
	}
	if reason != domain.ReasonRunLimit {
		t.Fatalf("reason = %q, want runLimit", reason)
	}
}

func TestTheConcurrencyBudgetCountsWorkAlreadyOutstanding(t *testing.T) {
	// Two updates already in flight against a ceiling of two: this pass may
	// start nothing, even though it has started nothing itself.
	policy := automaticPolicy()
	policy.Limits.MaxConcurrent = 8
	policy.Limits.MaxPerRegistry = 8
	policy.Limits.MaxPerRun = 8

	budget := service.NewAutomationBudget(10, 2, 2, nil)
	effective := domain.Resolve(policy, nil)

	reason, ok := budget.Admit(domain.AutomationDecision{}, effective)
	if ok {
		t.Fatal("outstanding work counts against the concurrency ceiling")
	}
	if reason != domain.ReasonConcurrency {
		t.Fatalf("reason = %q, want concurrencyLimit", reason)
	}
}

func TestAPolicyCannotRaiseTheEngineCeiling(t *testing.T) {
	// A policy permitting fifty concurrent updates against an engine ceiling of
	// one must still get one.
	policy := automaticPolicy()
	policy.Limits.MaxConcurrent = 50
	policy.Limits.MaxPerRun = 50
	policy.Limits.MaxPerRegistry = 50
	effective := domain.Resolve(policy, nil)

	budget := service.NewAutomationBudget(1, 1, 0, nil)
	if _, ok := budget.Admit(domain.AutomationDecision{}, effective); !ok {
		t.Fatal("the first update is admitted")
	}
	if _, ok := budget.Admit(domain.AutomationDecision{}, effective); ok {
		t.Fatal("a policy must not be able to raise the engine-wide ceiling")
	}
}

func TestThePerRegistryLimitBucketsByHost(t *testing.T) {
	policy := automaticPolicy()
	policy.Limits.MaxConcurrent = 8
	policy.Limits.MaxPerRegistry = 1
	policy.Limits.MaxPerRun = 8
	effective := domain.Resolve(policy, nil)

	registryOf := func(reference string) string {
		if len(reference) > 7 && reference[:7] == "ghcr.io" {
			return "ghcr.io"
		}
		return "docker.io"
	}
	budget := service.NewAutomationBudget(8, 8, 0, registryOf)

	if _, ok := budget.Admit(domain.AutomationDecision{ProposedImage: "ghcr.io/a/b:1"}, effective); !ok {
		t.Fatal("the first ghcr.io update is admitted")
	}
	// A second against the same registry is refused...
	reason, ok := budget.Admit(domain.AutomationDecision{ProposedImage: "ghcr.io/c/d:1"}, effective)
	if ok {
		t.Fatal("a second simultaneous pull from one registry exceeds the per-registry limit")
	}
	if reason != domain.ReasonRegistryLimit {
		t.Fatalf("reason = %q, want registryLimit", reason)
	}
	// ...but one against a different registry is not.
	if _, ok := budget.Admit(domain.AutomationDecision{ProposedImage: "nginx:1.27"}, effective); !ok {
		t.Fatal("a different registry has its own budget")
	}
}

// --------------------------------------------------------------- scale --

func TestDecidingTenThousandContainersIsBounded(t *testing.T) {
	// The decision function runs once per container per pass. On a large estate
	// that is the hot path, and it must stay linear and allocation-modest.
	//
	// This is a correctness test with a timing assertion attached: what it
	// really pins is that no check inside the function became quadratic, which
	// a selector or a policy loop could easily do.
	policies := make([]domain.UpdatePolicy, 0, 20)
	for i := 0; i < 20; i++ {
		policy := automaticPolicy()
		policy.PolicyID = "upd_" + repeatHex('0')[:19] + string(rune('a'+i%16))
		policy.Name = "policy-" + string(rune('a'+i%16))
		policy.Priority = i
		policy.Selector = domain.UpdateSelector{
			Images: []string{"ghcr.io/acme/*", "nginx:1.27.*"},
			Labels: map[string]string{"tier": "front"},
		}
		policies = append(policies, policy)
	}

	const containers = 10000
	start := time.Now()
	eligible := 0
	for i := 0; i < containers; i++ {
		input := service.AutomationInput{
			Target: domain.SelectionTarget{
				Name:   "container-" + string(rune('a'+i%26)),
				Image:  "nginx:1.27.3",
				Labels: map[string]string{"tier": "front", "app": "web"},
			},
			ContainerID: "id-" + string(rune('a'+i%26)),
			Policies:    policies,
			Plan:        patchPlan(),
			HasPlan:     true,
			Now:         decideAt,
		}
		if service.DecideAutomation(input).Eligible() {
			eligible++
		}
	}
	elapsed := time.Since(start)

	if eligible != containers {
		t.Fatalf("%d of %d containers were eligible", eligible, containers)
	}
	// Generous by two orders of magnitude. A quadratic regression would blow
	// through it; ordinary machine-to-machine variation will not.
	if elapsed > 10*time.Second {
		t.Fatalf("deciding %d containers took %s, which is not a linear pass", containers, elapsed)
	}
	t.Logf("decided %d containers against %d policies in %s", containers, len(policies), elapsed)
}
