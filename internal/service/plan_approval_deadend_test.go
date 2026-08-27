package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Stage 17.7 step 1: the two dead ends, reproduced before either is fixed.
//
// # Why both are worth pinning permanently
//
// Neither is a crash. Both are paths that LOOK like they work: a request is
// accepted, an image is downloaded, a button is offered and pressed -- and then
// the container is never replaced. An operator's reasonable conclusion is that
// HarborMaster is broken, and the actual cause is three layers away from
// anything they can see.
//
// After the fix these become regressions in the ordinary sense: they assert the
// path now COMPLETES rather than that it dies. Keeping the reproduction shape
// means the failure mode has a name and a test, instead of being rediscovered.

// -------------------------------------------------------------- CASE B --

// TestMajorApprovalIsDecorative reproduces the automation dead end.
//
// The sequence the design note identified:
//
//	major update
//	  -> DecideAutomation step 7 returns awaitingApproval BEFORE step 8 looks
//	     at the recommendation
//	  -> the decision appears in Pending Approvals
//	  -> ReleaseDecision submits an acquisition
//	  -> the acquisition preflight PERMITS manualReview, so the image downloads
//	  -> the execution preflight REFUSES manualReview
//
// So the operator approves, an image is pulled, and nothing is ever replaced.
//
// Every major update reaches this: 40 points at warning severity lands in the
// medium band, and medium + a warning is `manualReview` -- see
// automation_preset_floor_test.go, which measures it.
func TestMajorApprovalIsDecorative(t *testing.T) {
	harness := newAutomationHarness(t, majorApprovalPolicy())
	harness.now = readinessAt

	plan := assessedPlan("web", "1.27.4", domain.UpdateMajor, 6)
	if plan.Risk.Recommendation != domain.RecommendManualReview {
		t.Fatalf("fixture: a major update must measure as %q, got %q (score %d)",
			domain.RecommendManualReview, plan.Risk.Recommendation, plan.Risk.Score)
	}
	harness.evidence.targets = []store.AutomationTarget{readinessTarget("web", nil)}
	harness.evidence.plans = map[string]domain.ChangePlan{"container-web": plan}

	// The deployment-wide rule: a person must approve every major update.
	options := harness.options()
	options.Config.RequireApprovalForMajor = true
	harness.engine = service.NewAutomationService(options)

	// 1. The pass holds it for a person -- and does so at step 7, before the
	//    recommendation is ever consulted.
	run, decisions, err := harness.engine.RunNow(
		context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if decisions[0].Verdict != domain.VerdictAwaitingApproval {
		t.Fatalf("verdict = %q, want awaitingApproval (reason %q)",
			decisions[0].Verdict, decisions[0].Reason)
	}
	if decisions[0].Reason != domain.ReasonNeedApproval {
		t.Fatalf("reason = %q, want %q", decisions[0].Reason, domain.ReasonNeedApproval)
	}

	// 2. A person releases it, and 3. an acquisition is submitted.
	_, releaseErr := harness.engine.Approve(context.Background(), run.RunID, "web",
		domain.Requester{UserID: "usr_1", Username: "colby"}, service.Actor{})

	acquisitions := len(harness.pipeline.recorded("acquire"))

	if releaseErr == nil && acquisitions == 1 {
		t.Log("DEAD END REPRODUCED: the approval was accepted and an image " +
			"acquisition was submitted for a plan the execution preflight refuses")
		return
	}

	// After the Stage 17.7 fix this is the expected shape: the release is
	// refused up front, no acquisition is submitted, and the message points at
	// the plan-review workflow instead of pretending the approval was enough.
	if acquisitions != 0 {
		t.Fatalf("%d acquisitions were submitted for a manual-review plan", acquisitions)
	}
	if releaseErr == nil {
		t.Fatal("the release reported success but submitted nothing")
	}
	if !strings.Contains(strings.ToLower(releaseErr.Error()), "review") {
		t.Fatalf("the refusal must direct the operator to the plan review, got: %v",
			releaseErr)
	}
}

// majorApprovalPolicy permits major versions, so only the deployment-wide rule
// and the recommendation stand between the plan and an update.
func majorApprovalPolicy() domain.UpdatePolicy {
	policy := automaticPolicy()
	policy.Strategy = domain.StrategyMajor
	policy.MinimumRecommendation = domain.RecommendCaution
	policy.Window = domain.MaintenanceWindow{AlwaysOpen: true}
	policy.Selector = domain.UpdateSelector{Include: []string{"web"}}
	policy.Normalise()
	return policy
}
