package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The approval validator, exhaustively.
//
// Pure, so every case can be stated without a database. This is where the
// stage's central rule lives: an approval authorises ONE immutable plan, and
// stops authorising it the moment anything about that binding changes.

func approvedPlan() domain.ChangePlan {
	return domain.ChangePlan{
		PlanID:         "plan_" + strings.Repeat("1", 20),
		ContainerID:    "container-web",
		ContainerName:  "web",
		ProposedDigest: "sha256:" + strings.Repeat("b", 64),
		InputDigest:    strings.Repeat("e", 64),
		Risk: domain.RiskAssessment{
			Score:          54,
			Band:           domain.RiskHigh,
			Recommendation: domain.RecommendManualReview,
		},
	}
}

func standingApproval(plan domain.ChangePlan) domain.PlanApproval {
	return domain.PlanApproval{
		PlanID:                 plan.PlanID,
		State:                  domain.PlanApprovalActive,
		ApprovedInputDigest:    plan.InputDigest,
		ApprovedProposedDigest: plan.ProposedDigest,
		ApprovedBy:             domain.Requester{UserID: "usr_1", Username: "colby"},
		ApprovedAt:             time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC),
	}
}

func TestOnlyManualReviewIsApprovable(t *testing.T) {
	approvable := map[domain.Recommendation]bool{
		domain.RecommendManualReview: true,
		domain.RecommendProceed:      false,
		domain.RecommendCaution:      false,
		domain.RecommendAgainst:      false,
		domain.RecommendUnknown:      false,
	}
	for recommendation, want := range approvable {
		if got := domain.PlanApprovable(recommendation); got != want {
			t.Errorf("PlanApprovable(%q) = %v, want %v", recommendation, got, want)
		}
	}
	// Non-vacuity: every verdict in the vocabulary is covered above.
	if len(approvable) != len(domain.Recommendations) {
		t.Fatalf("the recommendation vocabulary has %d values and this test covers %d",
			len(domain.Recommendations), len(approvable))
	}
}

func TestAStandingApprovalAuthorisesItsOwnPlan(t *testing.T) {
	plan := approvedPlan()
	refusal := domain.PlanApprovalValid(standingApproval(plan), plan, plan.PlanID, false)
	if refusal != domain.PlanApprovalRefusalNone {
		t.Fatalf("refusal = %q, want none (%s)", refusal, refusal.Explain())
	}
}

// TestAnApprovalStopsAuthorisingWhenTheBindingBreaks is the whole rule.
//
// Each case changes exactly one thing about the binding between the human
// judgement and the evidence. All of them must refuse, because an approval that
// survived any of them would be authorising a change nobody reviewed.
func TestAnApprovalStopsAuthorisingWhenTheBindingBreaks(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*domain.PlanApproval, *domain.ChangePlan, *string, *bool)
		want   domain.PlanApprovalRefusal
	}{
		{"it was withdrawn", func(a *domain.PlanApproval, _ *domain.ChangePlan, _ *string, _ *bool) {
			a.State = domain.PlanApprovalRevoked
		}, domain.PlanApprovalRefusalRevoked},

		{"it names a different plan", func(a *domain.PlanApproval, _ *domain.ChangePlan, _ *string, _ *bool) {
			a.PlanID = "plan_" + strings.Repeat("9", 20)
		}, domain.PlanApprovalRefusalSuperseded},

		{"a newer plan replaced it", func(_ *domain.PlanApproval, _ *domain.ChangePlan, current *string, _ *bool) {
			*current = "plan_" + strings.Repeat("2", 20)
		}, domain.PlanApprovalRefusalSuperseded},

		{"the proposed digest moved", func(_ *domain.PlanApproval, p *domain.ChangePlan, _ *string, _ *bool) {
			p.ProposedDigest = "sha256:" + strings.Repeat("c", 64)
		}, domain.PlanApprovalRefusalEvidenceChanged},

		{"the input digest moved", func(_ *domain.PlanApproval, p *domain.ChangePlan, _ *string, _ *bool) {
			p.InputDigest = strings.Repeat("f", 64)
		}, domain.PlanApprovalRefusalEvidenceChanged},

		{"the plan no longer asks for review", func(_ *domain.PlanApproval, p *domain.ChangePlan, _ *string, _ *bool) {
			p.Risk.Recommendation = domain.RecommendProceed
		}, domain.PlanApprovalRefusalNotApprovable},

		{"it has already changed the host", func(_ *domain.PlanApproval, _ *domain.ChangePlan, _ *string, mutated *bool) {
			*mutated = true
		}, domain.PlanApprovalRefusalAlreadyActed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := approvedPlan()
			approval := standingApproval(plan)
			current := plan.PlanID
			mutated := false

			tc.mutate(&approval, &plan, &current, &mutated)

			got := domain.PlanApprovalValid(approval, plan, current, mutated)
			if got != tc.want {
				t.Fatalf("refusal = %q, want %q", got, tc.want)
			}
			if got.Explain() == "" {
				t.Fatal("a refusal must carry HarborMaster's own sentence")
			}
		})
	}
}

// TestApprovalIsNotTransferredToANewPlan is §27, stated directly.
//
// The planner writes a new plan when the evidence moves. The old approval names
// the old plan, so it authorises nothing about the new one -- and there is no
// code path that could transfer it, because the binding is the identifier.
func TestApprovalIsNotTransferredToANewPlan(t *testing.T) {
	first := approvedPlan()
	approval := standingApproval(first)

	// The evidence moved: a new plan, new identifier, new proposed digest.
	second := approvedPlan()
	second.PlanID = "plan_" + strings.Repeat("2", 20)
	second.ProposedDigest = "sha256:" + strings.Repeat("c", 64)
	second.InputDigest = strings.Repeat("f", 64)

	if refusal := domain.PlanApprovalValid(approval, second, second.PlanID, false); refusal == domain.PlanApprovalRefusalNone {
		t.Fatal("an approval of one plan authorised a different one")
	}

	// And the first plan is now superseded, so the original approval has
	// stopped authorising that too.
	if refusal := domain.PlanApprovalValid(approval, first, second.PlanID, false); refusal != domain.PlanApprovalRefusalSuperseded {
		t.Fatalf("refusal = %q, want %q", refusal, domain.PlanApprovalRefusalSuperseded)
	}
}

// TestApprovalSurvivesRetriesUntilTheHostIsTouched is §15.
//
// A failed HTTP request, a restart, or a preflight refusal must not consume a
// human decision. Only an execution that actually changed the host does.
func TestApprovalSurvivesRetriesUntilTheHostIsTouched(t *testing.T) {
	plan := approvedPlan()
	approval := standingApproval(plan)

	// Any number of attempts that did not mutate: still valid.
	for range 5 {
		if refusal := domain.PlanApprovalValid(approval, plan, plan.PlanID, false); refusal != domain.PlanApprovalRefusalNone {
			t.Fatalf("a retry consumed the approval: %q", refusal)
		}
	}

	// The first one that mutated spends it.
	if refusal := domain.PlanApprovalValid(approval, plan, plan.PlanID, true); refusal != domain.PlanApprovalRefusalAlreadyActed {
		t.Fatalf("refusal = %q, want %q", refusal, domain.PlanApprovalRefusalAlreadyActed)
	}
}

func TestPlanApprovalStateVocabularyIsClosed(t *testing.T) {
	if len(domain.PlanApprovalStates) != 2 {
		t.Fatalf("expected two states, found %d: a third needs a use case and a "+
			"database CHECK", len(domain.PlanApprovalStates))
	}
	for _, state := range domain.PlanApprovalStates {
		if !domain.ValidPlanApprovalState(string(state)) {
			t.Errorf("%q is in the vocabulary but not accepted by the validator", state)
		}
	}
	if domain.ValidPlanApprovalState("consumed") {
		t.Error("`consumed` is derivable from executions.mutated_at and must not " +
			"become a stored state")
	}
}
