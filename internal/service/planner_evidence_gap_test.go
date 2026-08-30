package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The evidence-gap rule, end to end.
//
// # The contract
//
// `domain.UpdateNone` is a POSITIVE claim -- "the image is current: the tag has
// not moved and no newer comparable tag exists" -- and the attention model
// turns it into `AttentionUpToDate`, "HarborMaster looked and found nothing to
// do". `internal/domain/attention.go` states the rule in its header:
//
//	Absent evidence produces AttentionNotChecked, never AttentionUpToDate.
//	[...] one says HarborMaster looked and found nothing to do, the other
//	says HarborMaster has not looked.
//
// # Why the rule can be broken without the attention model being wrong
//
// `AssessContainer` is a pure function of the evidence handed to it, and it
// upholds the rule for the input it is given. But `image_intel.update_type` is
// `NOT NULL DEFAULT 'none'` and is written ONLY by a successful check, so a
// reference that has never been compared carries `none` as a column default.
// A planner that passed that through handed the attention model an input that
// was already a lie, and "up to date" came out the far end -- for a container
// nothing had ever looked at.
//
// Batch B1 closed this for `unsupported`. This file closes it for every status
// and pins the whole state space, because the leak was never really about
// `unsupported`: it was about the difference between a stored verdict and a
// stored default, which `unsupported` merely made visible.
//
// # What each case walks
//
//	image intel -> planner -> ChangePlan -> ContainerEvidence -> AssessContainer
//
// asserting BOTH the planner-facing value and the user-facing verdict, because
// this defect was invisible in each layer alone.

// evidenceFor builds the ContainerEvidence the container list would project
// from a plan, exactly as store.ContainerRepository.gatherPlans does: PlanKnown
// with the plan's update type, recommendation and proposed image, and nothing
// else.
func evidenceFor(plan domain.ChangePlan, hasPlan bool) domain.ContainerEvidence {
	if !hasPlan {
		return domain.ContainerEvidence{}
	}
	return domain.ContainerEvidence{
		PlanKnown:      true,
		UpdateType:     plan.UpdateType,
		Recommendation: plan.Risk.Recommendation,
		ProposedImage:  plan.ProposedImage,
	}
}

// gapIntel is one image intelligence row in a named state.
//
// LastSuccessAt is the discriminator under test: nil means no lookup has ever
// answered for this reference, so its update column is the schema default
// rather than an observation.
func gapIntel(status domain.CheckStatus, everSucceeded bool, stored domain.UpdateType) domain.ImageIntel {
	intel := domain.ImageIntel{
		Reference:  "docker.io/library/nginx:1.27.0",
		Familiar:   "nginx:1.27.0",
		Kind:       domain.RegistryDockerHub,
		Registry:   "docker.io",
		Repository: "library/nginx",
		Tag:        "1.27.0",
		Platform:   domain.Platform{OS: "linux", Architecture: "amd64"},
		Status:     status,
		Update:     stored,
	}
	if everSucceeded {
		answered := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		intel.LastSuccessAt = &answered
	}
	// A real finding names the tag and digest it resolved, or the planner
	// correctly proposes nothing and the case would not test what it claims to.
	if stored.Available() {
		intel.LatestTag = "1.27.1"
		intel.LatestDigest = planLatestDigest
	}
	return intel
}

func TestRegistryEvidenceReachesTheContainerListHonestly(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		status        domain.CheckStatus
		everSucceeded bool
		stored        domain.UpdateType

		// wantPlan is false where the planner declines to write a non-event.
		wantPlan bool
		// wantUpdateType is the planner-facing semantics.
		wantUpdateType domain.UpdateType
		// wantAttention is the user-facing semantics.
		wantAttention domain.AttentionState
	}{
		// ---------------------------------------------------- ok --
		{
			// The one state that positively established the image is current.
			// The planner writes no row for it -- a plan describes a PROPOSED
			// CHANGE and there is none -- so the container list reports the
			// absence of an assessment, which is the honest answer and the
			// pre-existing behaviour.
			name: "ok and nothing newer", status: domain.CheckOK,
			everSucceeded: true, stored: domain.UpdateNone,
			wantPlan: false, wantAttention: domain.AttentionNotChecked,
		},
		{
			name: "ok with a newer image", status: domain.CheckOK,
			everSucceeded: true, stored: domain.UpdatePatch,
			wantPlan: true, wantUpdateType: domain.UpdatePatch,
			wantAttention: domain.AttentionCannotAdvise,
		},

		// ----------------------------------------------- pending --
		{
			// The headline latent case. Nothing has run; `none` is the schema
			// default. This reported "Up to date" before B1.1.
			name: "pending, never compared", status: domain.CheckPending,
			everSucceeded: false, stored: domain.UpdateNone,
			wantPlan: true, wantUpdateType: domain.UpdateUnknown,
			wantAttention: domain.AttentionCannotAdvise,
		},

		// ------------------------------------------------ failed --
		{
			// A first lookup that never answered. Same default, same lie.
			name: "failed on the first lookup", status: domain.CheckFailed,
			everSucceeded: false, stored: domain.UpdateNone,
			wantPlan: true, wantUpdateType: domain.UpdateUnknown,
			wantAttention: domain.AttentionCannotAdvise,
		},
		{
			// EVIDENCE RETENTION. A real comparison established "current", and
			// a later transient failure must not erase it. This is the genuine
			// route to "Up to date" and it must keep working.
			name: "failed after a clean comparison", status: domain.CheckFailed,
			everSucceeded: true, stored: domain.UpdateNone,
			wantPlan: true, wantUpdateType: domain.UpdateNone,
			wantAttention: domain.AttentionUpToDate,
		},
		{
			name: "failed after finding a patch", status: domain.CheckFailed,
			everSucceeded: true, stored: domain.UpdatePatch,
			wantPlan: true, wantUpdateType: domain.UpdatePatch,
			wantAttention: domain.AttentionCannotAdvise,
		},

		// ------------------------------------------- rate limited --
		{
			name: "rate limited, never compared", status: domain.CheckRateLimited,
			everSucceeded: false, stored: domain.UpdateNone,
			wantPlan: true, wantUpdateType: domain.UpdateUnknown,
			wantAttention: domain.AttentionCannotAdvise,
		},
		{
			name: "rate limited after finding a minor", status: domain.CheckRateLimited,
			everSucceeded: true, stored: domain.UpdateMinor,
			wantPlan: true, wantUpdateType: domain.UpdateMinor,
			wantAttention: domain.AttentionCannotAdvise,
		},

		// ------------------------------------------- unauthorized --
		{
			// A private repository. HarborMaster holds no credentials BY
			// DESIGN, so this never succeeds and its `none` is never an
			// observation -- every private-registry container reported "Up to
			// date" before B1.1.
			name: "unauthorized, never compared", status: domain.CheckUnauthorized,
			everSucceeded: false, stored: domain.UpdateNone,
			wantPlan: true, wantUpdateType: domain.UpdateUnknown,
			wantAttention: domain.AttentionCannotAdvise,
		},
		{
			// A repository that WAS public and has just been made private. The
			// last real answer stands.
			name: "unauthorized after a clean comparison", status: domain.CheckUnauthorized,
			everSucceeded: true, stored: domain.UpdateNone,
			wantPlan: true, wantUpdateType: domain.UpdateNone,
			wantAttention: domain.AttentionUpToDate,
		},

		// ----------------------------------------------- notFound --
		{
			// "Often a locally built image that was never published" -- the
			// commonest homelab case, and it reported "Up to date" forever.
			name: "not found, never compared", status: domain.CheckNotFound,
			everSucceeded: false, stored: domain.UpdateNone,
			wantPlan: true, wantUpdateType: domain.UpdateUnknown,
			wantAttention: domain.AttentionCannotAdvise,
		},
		{
			name: "not found after a clean comparison", status: domain.CheckNotFound,
			everSucceeded: true, stored: domain.UpdateNone,
			wantPlan: true, wantUpdateType: domain.UpdateNone,
			wantAttention: domain.AttentionUpToDate,
		},

		// -------------------------------------------- unsupported --
		{
			// B1's case, still pinned here.
			name: "unsupported, never compared", status: domain.CheckUnsupported,
			everSucceeded: false, stored: domain.UpdateNone,
			wantPlan: true, wantUpdateType: domain.UpdateUnknown,
			wantAttention: domain.AttentionCannotAdvise,
		},
		{
			// A reference that WAS comparable before a normalisation rule
			// tightened around it. Unlike a failure, this gap never closes:
			// the row is never queued again, so its verdict can never be
			// refreshed or contradicted. Reported as unknown whatever it
			// happens to hold.
			name: "unsupported with a frozen prior finding", status: domain.CheckUnsupported,
			everSucceeded: true, stored: domain.UpdatePatch,
			wantPlan: true, wantUpdateType: domain.UpdateUnknown,
			wantAttention: domain.AttentionCannotAdvise,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakePlanStore()
			fake.candidates = []store.PlanCandidate{
				candidate("container-a", "web", "nginx:1.27.0"),
			}
			fake.intel["docker.io/library/nginx:1.27.0"] =
				gapIntel(testCase.status, testCase.everSucceeded, testCase.stored)

			planner := plannerAt(fake, plannerNow(t))
			if _, err := planner.Generate(context.Background()); err != nil {
				t.Fatalf("Generate: %v", err)
			}

			plans := fake.plans()
			if testCase.wantPlan && len(plans) != 1 {
				t.Fatalf("wrote %d plans, want 1", len(plans))
			}
			if !testCase.wantPlan && len(plans) != 0 {
				t.Fatalf("wrote %d plans, want none", len(plans))
			}

			var plan domain.ChangePlan
			if testCase.wantPlan {
				plan = plans[0]

				// PLANNER-FACING SEMANTICS.
				if plan.UpdateType != testCase.wantUpdateType {
					t.Errorf("plan update type = %q, want %q",
						plan.UpdateType, testCase.wantUpdateType)
				}
				// The registry status always reaches the plan as itself. The
				// fix reclassifies what was COMPARED, never what was ASKED.
				if plan.RegistryStatus != testCase.status {
					t.Errorf("plan registry status = %q, want %q",
						plan.RegistryStatus, testCase.status)
				}
			}

			// USER-FACING SEMANTICS.
			state := domain.AssessContainer(evidenceFor(plan, testCase.wantPlan)).State
			if state != testCase.wantAttention {
				t.Errorf("attention = %q, want %q", state, testCase.wantAttention)
			}

			// THE INVARIANT, stated once more independently of the table: a
			// verdict of "up to date" requires a comparison that actually
			// happened.
			if state == domain.AttentionUpToDate && !testCase.everSucceeded {
				t.Error("a container whose registry lookup has never succeeded " +
					"is reported as up to date")
			}
		})
	}
}

// ------------------------------------------------------- negative proofs --

// A never-compared record must never emit UpdateNone, whatever its status.
//
// Stated separately from the table so that adding a status to CheckStatuses
// without a table row still fails this test.
func TestNoNeverComparedRecordEmitsUpdateNone(t *testing.T) {
	for _, status := range domain.CheckStatuses {
		t.Run(string(status), func(t *testing.T) {
			fake := newFakePlanStore()
			fake.candidates = []store.PlanCandidate{
				candidate("container-a", "web", "nginx:1.27.0"),
			}
			// The schema default, exactly as a freshly seeded row carries it.
			fake.intel["docker.io/library/nginx:1.27.0"] =
				gapIntel(status, false, domain.UpdateNone)

			planner := plannerAt(fake, plannerNow(t))
			if _, err := planner.Generate(context.Background()); err != nil {
				t.Fatalf("Generate: %v", err)
			}

			plans := fake.plans()
			if len(plans) == 0 {
				// No plan is also honest: the container list then reports the
				// absence of an assessment.
				return
			}
			if plans[0].UpdateType == domain.UpdateNone {
				t.Errorf("status %q emitted UpdateNone from a record that has "+
					"never been compared; its update column is the schema default", status)
			}
			state := domain.AssessContainer(evidenceFor(plans[0], true)).State
			if state == domain.AttentionUpToDate {
				t.Errorf("status %q reached the container list as up to date", status)
			}
		})
	}
}

// Moving a plan from `none` to `unknown` must change what an operator is TOLD
// and nothing about what may be DONE.
func TestTheReclassificationCreatesNoActionableState(t *testing.T) {
	fake := newFakePlanStore()
	fake.candidates = []store.PlanCandidate{
		candidate("container-a", "web", "nginx:1.27.0"),
	}
	fake.intel["docker.io/library/nginx:1.27.0"] =
		gapIntel(domain.CheckPending, false, domain.UpdateNone)

	planner := plannerAt(fake, plannerNow(t))
	if _, err := planner.Generate(context.Background()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	plan := fake.plans()[0]

	if plan.UpdateType.Available() {
		t.Error("the reclassified plan reports an update as available")
	}
	if plan.ProposedImage != "" || plan.ProposedDigest != "" {
		t.Errorf("a target was invented: %q / %q", plan.ProposedImage, plan.ProposedDigest)
	}
	if plan.Risk.Recommendation != domain.RecommendUnknown {
		t.Errorf("recommendation = %q, want unknown", plan.Risk.Recommendation)
	}

	// No strategy may ever permit the value the fix introduces. UpdateNone was
	// refused by all of them and UpdateUnknown must be too, so the ceiling
	// cannot be widened by a presentation change.
	for _, strategy := range domain.UpdateStrategies {
		if strategy.Permits(domain.UpdateUnknown) {
			t.Errorf("strategy %q permits an undetermined update", strategy)
		}
	}
}

// Automation still does nothing, through the real decision function.
//
// The engine's no-update gate reads `UpdateType == UpdateNone || ProposedImage
// == ""`. The fix stops the first clause firing for these plans, so the second
// clause is now load-bearing -- which is worth proving rather than assuming.
func TestAutomationStillDeclinesAReclassifiedPlan(t *testing.T) {
	fake := newFakePlanStore()
	fake.candidates = []store.PlanCandidate{
		candidate("container-a", "web", "nginx:1.27.0"),
	}
	fake.intel["docker.io/library/nginx:1.27.0"] =
		gapIntel(domain.CheckNotFound, false, domain.UpdateNone)

	planner := plannerAt(fake, plannerNow(t))
	if _, err := planner.Generate(context.Background()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	plan := fake.plans()[0]
	if plan.UpdateType != domain.UpdateUnknown {
		t.Fatalf("fixture did not produce the reclassified plan: %q", plan.UpdateType)
	}

	// The otherwise fully eligible input -- an automatic policy, no pause, no
	// window to miss -- with only the plan swapped.
	input := eligibleInput()
	input.Plan = plan
	input.HasPlan = true

	outcome := service.DecideAutomation(input)
	if outcome.Eligible() {
		t.Fatal("automation accepted a plan that proposes nothing")
	}
	if outcome.Decision.Reason != domain.ReasonNoUpdate {
		t.Errorf("reason = %q, want %q", outcome.Decision.Reason, domain.ReasonNoUpdate)
	}
}
