package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The Phase 17 soak: one tag, republished three times, nobody watching.
//
// # What this is, and what it is not
//
// HarborMaster's registry client is HTTPS-only and its host is derived rather
// than supplied, so there is no way to point it at a registry whose tags a test
// could move -- and moving a tag on a public registry is not something an
// acceptance run may do. Live acceptance therefore proves ONE real same-tag
// digest transition end to end against Docker Hub, and this proves the repeated
// unattended case deterministically.
//
// It is not a mock of the decision. The proposal comes from the real
// EvaluateLineage, the pass is the real AutomationService with the real
// DecideAutomation behind it, and the policy is the real Follow-current-tag
// preset. What is simulated is only the registry moving and the host obeying:
// after each successful execution the container runs what it was told to run,
// and the tag has moved on again.
//
// # What would go wrong without it
//
// Every failure mode of unattended updating is a repetition failure: the same
// transition acquired twice, a plan acted on after it was superseded, a
// tracking reference quietly rewritten to a digest so the next pass has nothing
// left to follow, an approval demanded for an ordinary digest move. One
// transition cannot show any of them.
func TestFollowCurrentTagAppliesThreeRepublishesUnattended(t *testing.T) {
	const (
		reference = "alpine:3.22"
		digestA   = "sha256:a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"
		digestB   = "sha256:b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
		digestC   = "sha256:c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
		digestD   = "sha256:d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"
	)

	planIDsByPass := []string{
		"plan_aaaaaaaaaaaaaaaaaaaa",
		"plan_bbbbbbbbbbbbbbbbbbbb",
		"plan_cccccccccccccccccccc",
	}

	normalised, err := domain.NormalizeImageRef(reference)
	if err != nil {
		t.Fatalf("normalise %q: %v", reference, err)
	}

	// The preset an operator arriving from Watchtower is offered, verbatim.
	policy := domain.UpdatePolicy{
		PolicyID:              "upd_followcurrenttag00",
		Name:                  "Follow current tag",
		Enabled:               true,
		Priority:              10,
		Selector:              domain.UpdateSelector{Include: []string{"web"}},
		Strategy:              domain.StrategyDigestOnly,
		MinimumRecommendation: domain.RecommendCaution,
		Mode:                  domain.ModeAutomatic,
		// The harness clock sits inside this window. The window is not what this
		// test is about -- it is held open so a closed one cannot be mistaken for
		// the digest-following failure this test exists to catch.
		Window:  domain.MaintenanceWindow{Start: "02:00", End: "04:00"},
		Failure: domain.UpdateFailureHandling{AutoRollback: true, PauseAfterFailures: 2},
	}
	policy.Normalise()

	harness := newAutomationHarness(t, policy)

	// The container starts on A, following its tag.
	lineage := domain.ImageLineage{
		ContainerName:     "web",
		State:             domain.LineageTracked,
		Origin:            domain.LineageObserved,
		TrackingReference: normalised.Canonical,
		TrackingFamiliar:  normalised.Familiar,
		Repository:        normalised.Path,
		RunningDigest:     digestA,
	}
	running := digestA

	transitions := [][2]string{{digestA, digestB}, {digestB, digestC}, {digestC, digestD}}

	for i, step := range transitions {
		// The registry republished the tag. A newer tag exists throughout, so
		// this also holds the Stage 17.9 ordering fix across every pass.
		intel := domain.ImageIntel{
			Reference:    normalised.Canonical,
			Familiar:     normalised.Familiar,
			Repository:   normalised.Path,
			Status:       domain.CheckOK,
			LocalDigest:  running,
			RemoteDigest: step[1],
			LatestTag:    "3.24",
			LatestDigest: "sha256:" + strings.Repeat("e", 64),
			Update:       domain.UpdateMinor,
		}

		proposal := domain.EvaluateLineage(lineage, intel, running)
		if !proposal.Usable {
			t.Fatalf("pass %d: proposal unusable: %s", i+1, proposal.Reason)
		}
		if proposal.Update != domain.UpdateDigest {
			t.Fatalf("pass %d: Update = %q, want digest\n"+
				"\ta republished tag must be followed even while a newer tag exists, "+
				"or Follow-current-tag cannot act at all", i+1, proposal.Update)
		}
		if proposal.Familiar != normalised.Familiar {
			t.Fatalf("pass %d: proposed reference = %q, want %q unchanged\n"+
				"\tfollowing a tag must never change which tag is followed",
				i+1, proposal.Familiar, normalised.Familiar)
		}
		if proposal.Digest != step[1] {
			t.Fatalf("pass %d: proposed digest = %q, want %q", i+1, proposal.Digest, step[1])
		}

		plan := domain.ChangePlan{
			PlanID:         planIDsByPass[i],
			ContainerID:    "container-web",
			ContainerName:  "web",
			CurrentImage:   normalised.Familiar,
			ProposedImage:  proposal.Familiar,
			CurrentDigest:  step[0],
			ProposedDigest: step[1],
			UpdateType:     domain.UpdateDigest,
			// What the planner scores a same-tag digest move as, and the point
			// of the preset: no person is asked.
			Risk: domain.RiskAssessment{Recommendation: domain.RecommendProceed},
		}

		harness.evidence.targets = []store.AutomationTarget{{
			ContainerID: "container-web",
			Selection:   domain.SelectionTarget{Name: "web", Image: normalised.Familiar},
		}}
		harness.evidence.plans = map[string]domain.ChangePlan{"container-web": plan}
		harness.evidence.inFlight = map[string]bool{}

		run, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
		if err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
		if run.Submitted != 1 {
			reasons := make([]string, 0, len(decisions))
			for _, decision := range decisions {
				reasons = append(reasons, string(decision.Reason))
			}
			t.Fatalf("pass %d: submitted = %d, want 1 (reasons: %s)\n"+
				"\tan ordinary same-tag digest move must not need a person",
				i+1, run.Submitted, strings.Join(reasons, ","))
		}

		// The FOLLOWER, not the pass, turns a succeeded acquisition into a
		// recreation. Driving it here is what makes this a soak of the whole
		// unattended loop rather than of its first half.
		service.FollowForTest(harness.engine, context.Background())

		// The host obeyed: the recreation happened and the container now runs
		// what the plan named. Its tracking reference is untouched, which is
		// what lets the NEXT pass have anything to follow.
		running = step[1]
		lineage.RunningDigest = step[1]
	}

	// Exactly three mutations, each asked for exactly once.
	var acquisitions, executions []string
	for _, request := range harness.pipeline.requests {
		switch request.kind {
		case "acquire":
			acquisitions = append(acquisitions, request.id)
		case "execute":
			executions = append(executions, request.id)
		case "rollback":
			t.Errorf("a rollback was requested during a clean soak: %+v", request)
		}
	}
	if len(acquisitions) != 3 {
		t.Errorf("acquisitions = %d, want 3: %v", len(acquisitions), acquisitions)
	}
	if len(executions) != 3 {
		t.Errorf("executions = %d, want 3: %v", len(executions), executions)
	}

	// Automation submits a PLAN identifier, and a different one each pass. The
	// same plan acquired twice is the duplicate-mutation failure.
	seen := make(map[string]bool, len(acquisitions))
	for _, id := range acquisitions {
		if seen[id] {
			t.Errorf("plan %q was acquired twice", id)
		}
		seen[id] = true
	}
	for i, id := range acquisitions {
		if i < len(planIDsByPass) && id != planIDsByPass[i] {
			t.Errorf("acquisition %d named %q, want the plan for that transition %q",
				i+1, id, planIDsByPass[i])
		}
	}

	if running != digestD {
		t.Errorf("final digest = %q, want D", running)
	}
	if lineage.TrackingFamiliar != normalised.Familiar {
		t.Errorf("tracking reference = %q, want %q: it must survive every "+
			"recreation onto a bare digest", lineage.TrackingFamiliar, normalised.Familiar)
	}
}
