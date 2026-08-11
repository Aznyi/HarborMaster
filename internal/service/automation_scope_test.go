package service_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// The decision function under a BROAD policy.
//
// The domain tests prove that the scope selects the right containers. These
// prove the thing that matters more: that selecting a container is not the same
// as being allowed to change it, and that every refusal DecideAutomation makes
// still fires when the policy reaching the container is "all eligible".

// broadAutomaticPolicy is the most dangerous policy the product can express:
// every eligible container, unattended, no window.
func broadAutomaticPolicy() domain.UpdatePolicy {
	policy := domain.UpdatePolicy{
		PolicyID:              "upd_bbbbbbbbbbbbbbbbbbbb",
		Name:                  "Keep everything current",
		Enabled:               true,
		Scope:                 domain.ScopeAllEligible,
		Strategy:              domain.StrategyPatch,
		MinimumRecommendation: domain.RecommendProceed,
		Mode:                  domain.ModeAutomatic,
		Window:                domain.MaintenanceWindow{AlwaysOpen: true},
		Failure:               domain.UpdateFailureHandling{AutoRollback: true, PauseAfterFailures: 2},
	}
	policy.Normalise()
	return policy
}

// screenedTarget is a container as the repository would have screened it.
func screenedTarget(name, image string, labels map[string]string) domain.SelectionTarget {
	return domain.SelectionTarget{
		Name:        name,
		Image:       image,
		Labels:      labels,
		Eligibility: domain.ScreenTarget(name, image, labels),
	}
}

// broadInput is a container that would be updated, under a broad policy, unless
// a check declines it. Every test below changes exactly one thing.
func broadInput() service.AutomationInput {
	return service.AutomationInput{
		Target:      screenedTarget("web", "nginx:1.27.3", nil),
		ContainerID: "container-web",
		Policies:    []domain.UpdatePolicy{broadAutomaticPolicy()},
		Plan:        patchPlan(),
		HasPlan:     true,
		Now:         decideAt,
	}
}

// The control. If this stops passing, every refusal test below is passing for
// the wrong reason.
func TestBroadPolicyUpdatesAnOrdinaryContainer(t *testing.T) {
	outcome := service.DecideAutomation(broadInput())

	if outcome.Decision.Verdict != domain.VerdictUpdate {
		t.Fatalf("verdict %q (%s): a broad automatic policy must update an ordinary "+
			"container that passed every check",
			outcome.Decision.Verdict, outcome.Decision.Detail)
	}
}

// ------------------------------------------------------- the self refusal --

// Check 0 fires under the broad scope exactly as it does under a selector.
//
// The scope declines to SELECT HarborMaster, and this is the second, independent
// refusal: it holds even if the selection layer were changed, which is why the
// domain test for the selector scope points at this one.
func TestDecideRefusesSelfEvenUnderAllEligible(t *testing.T) {
	input := broadInput()
	input.Target = screenedTarget("harbormaster", "ghcr.io/aznyi/harbormaster:1", nil)
	input.ContainerID = "container-harbormaster"
	input.Self = domain.SelfIdentity{ContainerName: "harbormaster"}

	outcome := service.DecideAutomation(input)

	if outcome.Decision.Verdict != domain.VerdictSkip {
		t.Fatalf("verdict %q, want skip", outcome.Decision.Verdict)
	}
	if outcome.Decision.Reason != domain.ReasonSelfUpdate {
		t.Fatalf("reason %q, want %q\n"+
			"\tthe broad scope must not be a way around the one refusal that cannot "+
			"be configured off", outcome.Decision.Reason, domain.ReasonSelfUpdate)
	}
}

// And with the identity unknown, the broad scope still declines it -- for a
// different reason, from a different layer. A deployment where every detection
// probe failed is covered by the acquisition and execution preflights, and this
// records that the scope is not what those two rely on.
func TestBroadScopeDoesNotDependOnSelfDetectionSucceeding(t *testing.T) {
	input := broadInput()
	input.Target = screenedTarget("harbormaster", "ghcr.io/aznyi/harbormaster:1", nil)
	input.Self = domain.SelfIdentity{}

	outcome := service.DecideAutomation(input)

	// Nothing here can refuse it: no identity, an ordinary name, a valid plan.
	// The refusal that remains is the execution preflight's, which this test
	// cannot reach -- so the honest assertion is that the decision is REACHED
	// rather than that it is refused.
	if outcome.Decision.Verdict != domain.VerdictUpdate {
		t.Fatalf("verdict %q: with no identity there is nothing here to refuse on, "+
			"and the protection is the preflight's", outcome.Decision.Verdict)
	}
}

// ------------------------------------- every other refusal, under breadth --

// One table, one check each, all under the broad policy. If the scope ever
// starts bypassing a check, exactly one row here fails and names it.
func TestEveryRefusalStillFiresUnderTheBroadScope(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*service.AutomationInput)
		verdict domain.AutomationVerdict
		reason  domain.AutomationReason
	}{
		{
			name: "a paused container",
			mutate: func(input *service.AutomationInput) {
				input.IsPaused = true
				input.Pause = domain.PausedContainer{
					ContainerName: "web",
					Reason:        domain.PauseRepeatedFailure,
					Failures:      3,
				}
			},
			verdict: domain.VerdictSkip,
			reason:  domain.ReasonPaused,
		},
		{
			name: "the container's own update opt-out",
			mutate: func(input *service.AutomationInput) {
				input.Target = screenedTarget("web", "nginx:1.27.3", map[string]string{
					domain.LabelUpdateEnabled: "false",
				})
			},
			verdict: domain.VerdictSkip,
			reason:  domain.ReasonLabelOff,
		},
		{
			name: "the container's own pause label",
			mutate: func(input *service.AutomationInput) {
				input.Target = screenedTarget("web", "nginx:1.27.3", map[string]string{
					domain.LabelUpdatePause: "true",
				})
			},
			verdict: domain.VerdictSkip,
			reason:  domain.ReasonLabelPaused,
		},
		{
			name: "no change plan",
			mutate: func(input *service.AutomationInput) {
				input.HasPlan = false
			},
			verdict: domain.VerdictSkip,
			reason:  domain.ReasonNoPlan,
		},
		{
			name: "a change larger than the ceiling",
			mutate: func(input *service.AutomationInput) {
				plan := patchPlan()
				plan.UpdateType = domain.UpdateMinor
				input.Plan = plan
			},
			verdict: domain.VerdictSkip,
			reason:  domain.ReasonStrategy,
		},
		{
			name: "a recommendation below the minimum",
			mutate: func(input *service.AutomationInput) {
				plan := patchPlan()
				plan.Risk.Recommendation = domain.RecommendCaution
				input.Plan = plan
			},
			verdict: domain.VerdictSkip,
			reason:  domain.ReasonRecommendation,
		},
		{
			name: "a closed maintenance window",
			mutate: func(input *service.AutomationInput) {
				policy := broadAutomaticPolicy()
				policy.Window = domain.MaintenanceWindow{Start: "22:00", End: "23:00"}
				policy.Normalise()
				input.Policies = []domain.UpdatePolicy{policy}
			},
			verdict: domain.VerdictSkip,
			reason:  domain.ReasonWindowClosed,
		},
		{
			name: "an unresolvable timezone",
			mutate: func(input *service.AutomationInput) {
				policy := broadAutomaticPolicy()
				policy.Window = domain.MaintenanceWindow{
					Timezone: "Mars/Olympus", Start: "02:00", End: "04:00",
				}
				input.Policies = []domain.UpdatePolicy{policy}
			},
			verdict: domain.VerdictSkip,
			reason:  domain.ReasonWindowInvalid,
		},
		{
			name: "work already in flight",
			mutate: func(input *service.AutomationInput) {
				input.InFlight = true
			},
			verdict: domain.VerdictSkip,
			reason:  domain.ReasonInFlight,
		},
		{
			name: "observe mode",
			mutate: func(input *service.AutomationInput) {
				policy := broadAutomaticPolicy()
				policy.Mode = domain.ModeObserve
				input.Policies = []domain.UpdatePolicy{policy}
			},
			verdict: domain.VerdictWouldUpdate,
			reason:  domain.ReasonObserveMode,
		},
		{
			name: "approval required",
			mutate: func(input *service.AutomationInput) {
				policy := broadAutomaticPolicy()
				policy.Mode = domain.ModeApprove
				input.Policies = []domain.UpdatePolicy{policy}
			},
			verdict: domain.VerdictAwaitingApproval,
			reason:  domain.ReasonNeedApproval,
		},
		{
			name: "the deployment-wide major version rule",
			mutate: func(input *service.AutomationInput) {
				policy := broadAutomaticPolicy()
				policy.Strategy = domain.StrategyMajor
				input.Policies = []domain.UpdatePolicy{policy}
				plan := patchPlan()
				plan.UpdateType = domain.UpdateMajor
				input.Plan = plan
				input.RequireApprovalForMajor = true
			},
			verdict: domain.VerdictAwaitingApproval,
			reason:  domain.ReasonNeedApproval,
		},
		{
			name: "an excluded container",
			mutate: func(input *service.AutomationInput) {
				policy := broadAutomaticPolicy()
				policy.Selector.Exclude = []string{"web"}
				input.Policies = []domain.UpdatePolicy{policy}
			},
			verdict: domain.VerdictSkip,
			reason:  domain.ReasonNotEligible,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			input := broadInput()
			testCase.mutate(&input)

			outcome := service.DecideAutomation(input)
			if outcome.Decision.Verdict != testCase.verdict {
				t.Fatalf("verdict %q, want %q (%s)",
					outcome.Decision.Verdict, testCase.verdict, outcome.Decision.Detail)
			}
			if outcome.Decision.Reason != testCase.reason {
				t.Fatalf("reason %q, want %q (%s)",
					outcome.Decision.Reason, testCase.reason, outcome.Decision.Detail)
			}
		})
	}
}

// ------------------------------------------------------- what it reports --

// A container a broad policy passed over gets an answer about the CONTAINER,
// not an answer about a selector that does not exist.
func TestBroadScopeReportsWhyItPassedAContainerOver(t *testing.T) {
	cases := []struct {
		name   string
		target domain.SelectionTarget
		want   string
	}{
		{
			name:   "a parked original",
			target: screenedTarget("web.hm-old-exec_0123456789abcdef0123", "nginx:1.27", nil),
			want:   "evidence",
		},
		{
			name: "an opted-out container",
			target: screenedTarget("legacy", "app:1", map[string]string{
				domain.LabelHarborMasterEnabled: "false",
			}),
			want: domain.LabelHarborMasterEnabled + "=false",
		},
		{
			name:   "an unscreened container",
			target: domain.SelectionTarget{Name: "mystery", Image: "app:1"},
			want:   "could recreate",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			input := broadInput()
			input.Target = testCase.target

			outcome := service.DecideAutomation(input)
			if outcome.Decision.Reason != domain.ReasonNotEligible {
				t.Fatalf("reason %q, want %q", outcome.Decision.Reason, domain.ReasonNotEligible)
			}
			if !strings.Contains(outcome.Decision.Detail, testCase.want) {
				t.Fatalf("detail %q does not explain %q", outcome.Decision.Detail, testCase.want)
			}
		})
	}
}

// With no broad policy in force, an unmatched container still gets the old
// answer. A new reason must not have displaced a correct one.
func TestNarrowPolicyStillReportsNotSelected(t *testing.T) {
	input := broadInput()
	narrow := domain.UpdatePolicy{
		PolicyID: "upd_aaaaaaaaaaaaaaaaaaaa",
		Name:     "api only",
		Enabled:  true,
		Scope:    domain.ScopeSelector,
		Selector: domain.UpdateSelector{Include: []string{"api"}},
	}
	narrow.Normalise()
	input.Policies = []domain.UpdatePolicy{narrow}

	outcome := service.DecideAutomation(input)
	if outcome.Decision.Reason != domain.ReasonNotSelected {
		t.Fatalf("reason %q, want %q", outcome.Decision.Reason, domain.ReasonNotSelected)
	}
}

// And with no policies at all, the answer is still that there are none.
func TestNoPoliciesStillReportsNoPolicy(t *testing.T) {
	input := broadInput()
	input.Policies = nil

	outcome := service.DecideAutomation(input)
	if outcome.Decision.Reason != domain.ReasonNoPolicy {
		t.Fatalf("reason %q, want %q", outcome.Decision.Reason, domain.ReasonNoPolicy)
	}
}

// ------------------------------------------------- selection is not action --

// The claim in one test: a broad policy in observe mode records a decision for
// every container and submits nothing.
func TestBroadObservePolicySubmitsNothing(t *testing.T) {
	policy := broadAutomaticPolicy()
	policy.Mode = domain.ModeObserve
	policy.Normalise()

	estate := []domain.SelectionTarget{
		screenedTarget("web", "nginx:1.27.3", nil),
		screenedTarget("api", "nginx:1.27.3", nil),
		screenedTarget("worker", "nginx:1.27.3", nil),
	}

	for _, target := range estate {
		input := broadInput()
		input.Target = target
		input.Policies = []domain.UpdatePolicy{policy}

		outcome := service.DecideAutomation(input)
		if outcome.Eligible() {
			t.Fatalf("%s: observe mode must never produce an actionable verdict", target.Name)
		}
		if outcome.Decision.Verdict != domain.VerdictWouldUpdate {
			t.Fatalf("%s: verdict %q, want wouldUpdate", target.Name, outcome.Decision.Verdict)
		}
		if outcome.Decision.AcquisitionID != "" {
			t.Fatalf("%s: observe mode recorded an acquisition", target.Name)
		}
	}
}
