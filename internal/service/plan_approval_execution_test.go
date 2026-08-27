package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// What an approval buys, and what it emphatically does not.
//
// # The shape of every test here
//
// Each one starts from a `manualReview` plan WITH a valid approval -- so the
// recommendation gate is satisfied -- and then breaks something else. The
// assertion is that the something else still refuses.
//
// That shape matters. A test that only proved "approved plans execute" would
// pass just as happily if the approval had been implemented as a bypass that
// skipped the rest of the preflight, which is the one implementation that must
// never ship.

// stubApprovals answers the single question the preflight asks.
type stubApprovals struct {
	refusal domain.PlanApprovalRefusal
	err     error
	asked   int
}

func (s *stubApprovals) ApprovalFor(
	context.Context, domain.ChangePlan,
) (domain.PlanApprovalRefusal, error) {
	s.asked++
	if s.err != nil {
		return "", s.err
	}
	return s.refusal, nil
}

// reviewed makes the plan ask for review and records that it was approved.
func reviewed(h *execHarness) {
	h.evidence.plan.Risk.Recommendation = domain.RecommendManualReview
	h.evidence.current.Risk.Recommendation = domain.RecommendManualReview
	h.approvals = &stubApprovals{refusal: domain.PlanApprovalRefusalNone}
}

// TestAnApprovedManualReviewPlanReachesTheRestOfThePreflight is the positive
// case: the gate that used to be a dead end now lets the plan through.
func TestAnApprovedManualReviewPlanReachesTheRestOfThePreflight(t *testing.T) {
	harness := newExecHarness(t, reviewed)

	execution := harness.request(t)
	if execution.Refusal != "" {
		t.Fatalf("refusal = %q; an approved manual-review plan must pass the "+
			"recommendation gate", execution.Refusal)
	}
}

// TestAnUnapprovedManualReviewPlanIsRefusedByName is the negative case, and the
// reason the vocabulary gained a value.
func TestAnUnapprovedManualReviewPlanIsRefusedByName(t *testing.T) {
	cases := []struct {
		name  string
		state func(*execHarness)
	}{
		{"no approval service at all", func(h *execHarness) {
			h.evidence.plan.Risk.Recommendation = domain.RecommendManualReview
			h.evidence.current.Risk.Recommendation = domain.RecommendManualReview
		}},
		{"nobody has approved it", func(h *execHarness) {
			h.evidence.plan.Risk.Recommendation = domain.RecommendManualReview
			h.evidence.current.Risk.Recommendation = domain.RecommendManualReview
			h.approvals = &stubApprovals{refusal: domain.PlanApprovalRefusalRevoked}
		}},
		{"the approval was withdrawn", func(h *execHarness) {
			h.evidence.plan.Risk.Recommendation = domain.RecommendManualReview
			h.evidence.current.Risk.Recommendation = domain.RecommendManualReview
			h.approvals = &stubApprovals{refusal: domain.PlanApprovalRefusalRevoked}
		}},
		{"the approval has already been used", func(h *execHarness) {
			h.evidence.plan.Risk.Recommendation = domain.RecommendManualReview
			h.evidence.current.Risk.Recommendation = domain.RecommendManualReview
			h.approvals = &stubApprovals{refusal: domain.PlanApprovalRefusalAlreadyActed}
		}},
		{"the evidence moved under the approval", func(h *execHarness) {
			h.evidence.plan.Risk.Recommendation = domain.RecommendManualReview
			h.evidence.current.Risk.Recommendation = domain.RecommendManualReview
			h.approvals = &stubApprovals{refusal: domain.PlanApprovalRefusalEvidenceChanged}
		}},
		{"the approval could not be established", func(h *execHarness) {
			h.evidence.plan.Risk.Recommendation = domain.RecommendManualReview
			h.evidence.current.Risk.Recommendation = domain.RecommendManualReview
			// Fails CLOSED: a check that could not be performed establishes
			// nothing, and nothing is not permission.
			h.approvals = &stubApprovals{err: store.ErrNotFound}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newExecHarness(t, tc.state)

			_, err := harness.service.Request(context.Background(),
				service.ExecutionRequest{AcquisitionID: execAcquisitionID})
			if err == nil {
				t.Fatal("the recreation was allowed without a review")
			}
			if got := executionRefusalFrom(t, err); got != domain.ExecutionRefusalApprovalMissing {
				t.Fatalf("refusal = %q, want %q", got, domain.ExecutionRefusalApprovalMissing)
			}
		})
	}
}

// TestApprovalCannotAuthoriseAVerdictNobodyMayApprove.
//
// `notRecommended` is the model arguing against the change and `unknown` is a
// gap in evidence. Neither is something a person can vouch for, so both refuse
// as `recommendation` whatever an approval says -- and the approval store is
// not even consulted, which is the stronger property.
func TestApprovalCannotAuthoriseAVerdictNobodyMayApprove(t *testing.T) {
	for _, recommendation := range []domain.Recommendation{
		domain.RecommendAgainst, domain.RecommendUnknown,
	} {
		t.Run(string(recommendation), func(t *testing.T) {
			approvals := &stubApprovals{refusal: domain.PlanApprovalRefusalNone}
			harness := newExecHarness(t, func(h *execHarness) {
				h.evidence.plan.Risk.Recommendation = recommendation
				h.evidence.current.Risk.Recommendation = recommendation
				h.approvals = approvals
			})

			_, err := harness.service.Request(context.Background(),
				service.ExecutionRequest{AcquisitionID: execAcquisitionID})
			if err == nil {
				t.Fatal("the recreation was allowed")
			}
			if got := executionRefusalFrom(t, err); got != domain.ExecutionRefusalRecommendation {
				t.Fatalf("refusal = %q, want %q", got, domain.ExecutionRefusalRecommendation)
			}
			if approvals.asked != 0 {
				t.Fatal("the approval store was consulted for a verdict no approval " +
					"can authorise; it must not even be asked")
			}
		})
	}
}

// TestApprovalDoesNotBypassAnyOtherRefusal is §18, and the most important test
// in this stage.
//
// Every case is an APPROVED manual-review plan with one other thing wrong. The
// approval satisfies the review requirement and nothing else, so each of these
// must still refuse -- by its own name, not by approvalMissing.
func TestApprovalDoesNotBypassAnyOtherRefusal(t *testing.T) {
	cases := []struct {
		name    string
		want    domain.ExecutionRefusal
		breakIt func(*execHarness)
	}{
		{"a newer plan exists", domain.ExecutionRefusalPlanSuperseded,
			func(h *execHarness) {
				h.evidence.current.PlanID = "plan_ffffffffffffffffffff"
			}},

		{"the plan changed since the download", domain.ExecutionRefusalPlanChanged,
			func(h *execHarness) {
				h.evidence.plan.InputDigest = strings.Repeat("e", 64)
				h.evidence.current.InputDigest = strings.Repeat("e", 64)
			}},

		{"the container is gone", domain.ExecutionRefusalContainerMissing,
			func(h *execHarness) { h.evidence.containerErr = store.ErrNotFound }},

		{"the inventory is stale", domain.ExecutionRefusalInventoryStale,
			func(h *execHarness) { h.evidence.refresh = nil }},

		{"there is no configuration snapshot", domain.ExecutionRefusalSnapshotMissing,
			func(h *execHarness) { h.evidence.baselineErr = store.ErrNotFound }},

		{"the policy evaluation is stale", domain.ExecutionRefusalPolicyStale,
			func(h *execHarness) { h.evidence.policyErr = store.ErrNotFound }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newExecHarness(t, func(h *execHarness) {
				reviewed(h)
				tc.breakIt(h)
			})

			_, err := harness.service.Request(context.Background(),
				service.ExecutionRequest{AcquisitionID: execAcquisitionID})
			if err == nil {
				t.Fatalf("an approved plan with %s was ACCEPTED; the approval "+
					"bypassed a safety gate", tc.name)
			}
			if got := executionRefusalFrom(t, err); got != tc.want {
				t.Fatalf("refusal = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheApprovalGateRunsBeforeAnythingIsStopped pins the ordering.
//
// A refused execution must leave the host exactly as it was. If the approval
// check had been placed after the mutation began, a missing approval would stop
// a container and then decline to replace it.
func TestTheApprovalGateRunsBeforeAnythingIsStopped(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.evidence.plan.Risk.Recommendation = domain.RecommendManualReview
		h.evidence.current.Risk.Recommendation = domain.RecommendManualReview
		h.approvals = &stubApprovals{refusal: domain.PlanApprovalRefusalRevoked}
	})

	_, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if err == nil {
		t.Fatal("the recreation was allowed without a review")
	}
	if got := executionRefusalFrom(t, err); got != domain.ExecutionRefusalApprovalMissing {
		t.Fatalf("refusal = %q", got)
	}
	// Refused at the PREFLIGHT, so the mutator was never reached: nothing was
	// stopped, renamed, created or removed.
	if calls := harness.mutator.Calls; len(calls) != 0 {
		t.Fatalf("a refused execution touched the host: %v", calls)
	}
}

var _ = service.ExecutionOptions{}
