package domain_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Currency, approval and the permanently uncomparable image (C3B).
//
// # The fail-open this closes
//
// PlanApprovalValid took the current plan's id as a bare string, and an empty
// one meant two opposite things: "nothing is current" and "we did not look".
// It was read as the second, so the check was SKIPPED -- and an approval
// granted while the registry was still pending stayed valid after the registry
// answered "nothing newer". It authorised a change that no longer existed.
//
// Currency is now an explicit pair. An answer is an answer whether or not it
// names a plan; only a real failure leaves it unestablished.

func currencyPlan() domain.ChangePlan {
	return domain.ChangePlan{
		PlanID:         "plan_00112233445566778899",
		ContainerID:    "container-a",
		ContainerName:  "web",
		CurrentImage:   "nginx:1.27.0",
		ProposedImage:  "nginx:1.27.1",
		ProposedDigest: "sha256:" + strings.Repeat("b", 64),
		UpdateType:     domain.UpdatePatch,
		InputDigest:    strings.Repeat("a", 64),
		Risk: domain.RiskAssessment{
			Recommendation: domain.RecommendManualReview,
			Band:           domain.RiskMedium,
		},
	}
}

func currencyApproval(plan domain.ChangePlan) domain.PlanApproval {
	return domain.PlanApproval{
		PlanID:                 plan.PlanID,
		State:                  domain.PlanApprovalActive,
		ApprovedInputDigest:    plan.InputDigest,
		ApprovedProposedDigest: plan.ProposedDigest,
		ApprovedBy:             domain.Requester{UserID: "usr_1", Username: "operator"},
	}
}

// ------------------------------------------------------------- currency --

func TestAnApprovalStopsAuthorisingWhenNothingIsCurrent(t *testing.T) {
	t.Parallel()

	// THE FAIL-OPEN. Settled registry evidence retired the plan, so the
	// container has NO current plan -- and the approval must stop authorising
	// it. Before C3B the empty id read as "we did not look" and the check was
	// skipped.
	plan := currencyPlan()
	got := domain.PlanApprovalValid(currencyApproval(plan), plan, "", true, false)

	if got != domain.PlanApprovalRefusalSuperseded {
		t.Fatalf("refusal = %q, want superseded -- an approval must not outlive "+
			"the plan it authorises", got)
	}
	if got.Explain() == "" {
		t.Error("a refusal must carry HarborMaster's own sentence")
	}
}

func TestUnresolvedCurrencyAssertsNothing(t *testing.T) {
	t.Parallel()

	// The other half of the pair, and why it is a pair. A lookup that FAILED
	// establishes nothing, so it must not by itself refuse: the remaining
	// checks decide, exactly as they did before currency was consulted at all.
	plan := currencyPlan()
	got := domain.PlanApprovalValid(currencyApproval(plan), plan, "", false, false)

	if got != domain.PlanApprovalRefusalNone {
		t.Fatalf("refusal = %q; an unestablished currency must not be read as "+
			"a verdict either way", got)
	}
}

func TestAResolvedDifferentPlanStillSupersedes(t *testing.T) {
	t.Parallel()

	plan := currencyPlan()
	got := domain.PlanApprovalValid(currencyApproval(plan), plan, "plan_ffffffffffffffffffff", true, false)
	if got != domain.PlanApprovalRefusalSuperseded {
		t.Fatalf("refusal = %q, want superseded", got)
	}
}

func TestAResolvedMatchingPlanAuthorises(t *testing.T) {
	t.Parallel()

	plan := currencyPlan()
	got := domain.PlanApprovalValid(currencyApproval(plan), plan, plan.PlanID, true, false)
	if got != domain.PlanApprovalRefusalNone {
		t.Fatalf("refusal = %q, want none (%s)", got, got.Explain())
	}
}

// --------------------------------------------------- not comparable --

func TestAnImageWithNoRegistryIsNotComparable(t *testing.T) {
	t.Parallel()

	// A locally built image is never queued for a lookup, so no later pass
	// will change the answer. Reporting "cannot determine" forever invites an
	// operator to wait for something that cannot arrive.
	got := domain.AssessContainer(domain.ContainerEvidence{
		Present: true, State: domain.StateRunning,
		CheckNotComparable: true,
	})
	if got.State != domain.AttentionNotComparable {
		t.Fatalf("state = %q, want notComparable", got.State)
	}
}

func TestOnlyThePermanentStatusIsNotComparable(t *testing.T) {
	t.Parallel()

	// Every transient status keeps its existing, correctly non-committal
	// verdict. `unauthorized`, `failed`, `rateLimited` and `pending` may all
	// resolve into a real answer on a later pass, and calling any of them
	// permanent would tell an operator to stop waiting for one.
	got := domain.AssessContainer(domain.ContainerEvidence{
		Present: true, State: domain.StateRunning,
		CheckNotComparable: false,
		CheckStatus:        domain.CheckUnauthorized,
	})
	if got.State == domain.AttentionNotComparable {
		t.Fatal("a transient failure was reported as permanently uncomparable")
	}
	if got.State != domain.AttentionNotChecked {
		t.Fatalf("state = %q, want notChecked", got.State)
	}
}

func TestNotComparableDefersToEveryStrongerAnswer(t *testing.T) {
	t.Parallel()

	// It describes the ABSENCE of a comparison, so anything that describes the
	// container itself outranks it.
	for _, probe := range []struct {
		name  string
		apply func(*domain.ContainerEvidence)
		want  domain.AttentionState
	}{
		{"unhealthy", func(e *domain.ContainerEvidence) { e.Health = domain.HealthUnhealthy },
			domain.AttentionUnhealthy},
		{"paused", func(e *domain.ContainerEvidence) { e.AutomationPaused = true },
			domain.AttentionPaused},
		{"preserved", func(e *domain.ContainerEvidence) { e.Preserved = domain.PreservedOriginal },
			domain.AttentionPreserved},
		{"awaiting approval", func(e *domain.ContainerEvidence) { e.AwaitingApproval = true },
			domain.AttentionApprovalRequired},
	} {
		evidence := domain.ContainerEvidence{
			Present: true, State: domain.StateRunning, CheckNotComparable: true,
		}
		probe.apply(&evidence)
		if got := domain.AssessContainer(evidence); got.State != probe.want {
			t.Errorf("%s = %q, want %q", probe.name, got.State, probe.want)
		}
	}
}

func TestNotComparableIsInThePrecedenceOrder(t *testing.T) {
	t.Parallel()

	// A state missing from AttentionOrder ranks below every other one and
	// sorts unpredictably, so adding one means placing it.
	rank := domain.AttentionRank(domain.AttentionNotComparable)
	if rank >= len(domain.AttentionOrder) {
		t.Fatal("notComparable is not in AttentionOrder")
	}
	// Below cannotAdvise, which is the TRANSIENT version of the same shape and
	// may still resolve into an update.
	if rank <= domain.AttentionRank(domain.AttentionCannotAdvise) {
		t.Error("a permanent non-answer outranked a transient one")
	}
	// Above upToDate, which is a positive verdict.
	if rank >= domain.AttentionRank(domain.AttentionUpToDate) {
		t.Error("notComparable ranked below a positive verdict")
	}
}
