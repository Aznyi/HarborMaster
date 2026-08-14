package store_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// A well-formed digest, so the repository's pinnability check passes and the
// CHECK constraint is what this test actually reaches.
const vocabularyDigest = "sha256:" +
	"4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1"

// The plan vocabulary and the schema that stores it.
//
// # Why this test exists
//
// `domain.UpdateTypes` and the `update_type` CHECK on `change_plans` are ONE
// vocabulary in two places. Phase 16 added `rebind` to the Go half and left the
// SQL half behind, and every layer above the store used a fake plan repository,
// so nothing failed until a real daemon was involved.
//
// What that cost, observed in Stage 5 live acceptance: HarborMaster updated a
// network namespace provider, verified it, recorded the dependency operation,
// and then could not insert the reattachment plan. The follower retried every
// ten seconds forever while the dependent sat attached to a container id that
// no longer existed -- running, reporting nothing wrong, with no network.
//
// That is the exact silent breakage the whole phase exists to prevent, and the
// phase caused it. This is the guard that fails the build instead.
//
// The same shape of defect has now occurred five times: migrations 0014, 0017,
// 0021 and 0026 each widened an audit CHECK after the Go vocabulary had moved.
// `TestEveryAuditTargetTypeIsAcceptedByTheSchema` is the equivalent guard for
// that column, and it caught 0026 before any host was touched. This one is its
// counterpart for plans.

func TestEveryUpdateTypeIsAcceptedByTheSchema(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Non-vacuity: if the vocabulary moved somewhere else, this test would
	// otherwise iterate an empty list and pass having stored nothing.
	if len(domain.UpdateTypes) < 8 {
		t.Fatalf("found %d update types; the vocabulary is not where this test "+
			"thinks it is", len(domain.UpdateTypes))
	}
	var sawRebind bool
	for _, updateType := range domain.UpdateTypes {
		if updateType == domain.UpdateRebind {
			sawRebind = true
		}
	}
	if !sawRebind {
		t.Fatal("domain.UpdateTypes does not contain rebind; this test would not " +
			"cover the case it was written for")
	}

	for index, updateType := range domain.UpdateTypes {
		// A fully-formed plan: the repository refuses an unpinnable target and
		// an inconsistent risk band before the CHECK is ever reached, so a
		// half-built fixture would fail for the wrong reason and hide the one
		// this test is about.
		plan := domain.ChangePlan{
			PlanID:           domain.NewPlanID(),
			ContainerID:      "container-" + string(updateType),
			ContainerName:    "hm-vocabulary-" + string(updateType),
			CurrentImage:     "alpine:3.22.0",
			ProposedImage:    "alpine:3.22.1",
			ProposedDigest:   vocabularyDigest,
			UpdateType:       updateType,
			RestoreReadiness: domain.ReadinessUnknown,
			RegistryStatus:   domain.CheckOK,
			InputDigest:      strings.Repeat(strconv.Itoa(index%10), 64),
			GeneratedAt:      now,
			PlanVersion:      domain.PlanSchemaVersion,
			PlannerVersion:   domain.PlannerVersion,
			Risk: domain.RiskAssessment{
				Score:          10,
				Band:           domain.RiskLow,
				Recommendation: domain.RecommendCaution,
				Summary:        "vocabulary check",
			},
		}

		if _, err := db.Plans.InsertPlans(ctx, []domain.ChangePlan{plan}, now); err != nil {
			t.Errorf("the schema refuses update type %q: %v\n\n"+
				"domain.UpdateTypes and the update_type CHECK on change_plans are one "+
				"vocabulary in two places. A refused plan insert is not a failed "+
				"request -- the dependency follower logs it and retries -- so this "+
				"ships as a reattachment that can never happen, leaving a container "+
				"attached to a namespace that no longer exists. Widen the CHECK in a "+
				"new migration; see 0027_plan_rebind_update_type.sql.",
				updateType, err)
		}
	}
}

// A rebind plan reads back as a rebind, with the image it does NOT move.
//
// The insert succeeding is not the whole claim: a rebind is a plan whose
// proposed image equals its current one, and a column that silently coerced or
// dropped the type would produce a plan that recreated a container onto
// something else.
func TestARebindPlanRoundTripsWithoutMovingTheImage(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	const digest = vocabularyDigest
	plan := domain.ChangePlan{
		PlanID:           domain.NewPlanID(),
		ContainerID:      "container-rebind",
		ContainerName:    "hm-rebind-roundtrip",
		CurrentImage:     "alpine:3.22.1",
		ProposedImage:    "alpine:3.22.1",
		CurrentDigest:    digest,
		ProposedDigest:   digest,
		UpdateType:       domain.UpdateRebind,
		RestoreReadiness: domain.ReadinessUnknown,
		RegistryStatus:   domain.CheckOK,
		InputDigest:      strings.Repeat("a", 64),
		GeneratedAt:      now,
		PlanVersion:      domain.PlanSchemaVersion,
		PlannerVersion:   domain.PlannerVersion,
		Risk: domain.RiskAssessment{
			Score:          10,
			Band:           domain.RiskLow,
			Recommendation: domain.RecommendCaution,
			Summary:        "a reattachment, not a version change",
		},
	}

	if _, err := db.Plans.InsertPlans(ctx, []domain.ChangePlan{plan}, now); err != nil {
		t.Fatalf("insert rebind plan: %v", err)
	}

	stored, err := db.Plans.Get(ctx, plan.PlanID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.UpdateType != domain.UpdateRebind {
		t.Errorf("update type read back as %q, want %q", stored.UpdateType, domain.UpdateRebind)
	}
	if stored.CurrentImage != stored.ProposedImage {
		t.Errorf("a rebind plan moved the image: %q -> %q",
			stored.CurrentImage, stored.ProposedImage)
	}
	if stored.ProposedDigest != digest {
		t.Errorf("the pinned digest did not survive: %q", stored.ProposedDigest)
	}
}
