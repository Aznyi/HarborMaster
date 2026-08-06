package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Phase 10.1 regression tests: the update pipeline's persistence.
//
// Three defects lived here, and all three were invisible because the values
// they produced were plausible:
//
//   - container_count was derived by joining a RAW image reference to a
//     CANONICAL one, so it was always zero.
//   - the same join appeared twice, in two hand-maintained SELECTs, and the
//     copies drifted.
//   - a change plan could be stored with a proposed reference and a digest that
//     were never resolved for each other.

const pipelineDigestA = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
const pipelineDigestB = "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"

// TestContainerCountSurvivesTheRawToCanonicalMapping is the regression test for
// the always-zero count.
func TestContainerCountSurvivesTheRawToCanonicalMapping(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// The seed carries the CANONICAL reference and a count computed while the
	// raw-to-canonical mapping was still known. Nothing downstream can
	// reconstruct that mapping, which is why the count is stored.
	if _, err := db.ImageIntel.SyncReferences(ctx, []store.ImageReferenceSeed{{
		Reference:      "docker.io/library/busybox:1.36",
		Familiar:       "busybox:1.36",
		Kind:           domain.RegistryDockerHub,
		Registry:       "docker.io",
		Namespace:      "library",
		Repository:     "library/busybox",
		Tag:            "1.36",
		LocalDigest:    pipelineDigestA,
		ContainerCount: 3,
		Supported:      true,
	}}, now); err != nil {
		t.Fatalf("sync: %v", err)
	}

	records, _, err := db.ImageIntel.List(ctx, store.ImageIntelFilter{Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("%d records, want 1", len(records))
	}
	if records[0].ContainerCount != 3 {
		t.Errorf("containerCount = %d, want 3", records[0].ContainerCount)
	}
	if records[0].LocalDigest != pipelineDigestA {
		t.Errorf("localDigest = %q, want the running image's digest", records[0].LocalDigest)
	}
}

// TestTheLatestTagKeepsItsOwnDigestThroughPersistence.
//
// The two are written together and read back together. A round trip that lost
// the digest would leave a tag nothing can pin, which the planner then refuses
// to propose -- correct, but for the wrong reason.
func TestTheLatestTagKeepsItsOwnDigestThroughPersistence(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := db.ImageIntel.SyncReferences(ctx, []store.ImageReferenceSeed{{
		Reference:   "docker.io/library/busybox:1.36",
		Familiar:    "busybox:1.36",
		Kind:        domain.RegistryDockerHub,
		Registry:    "docker.io",
		Repository:  "library/busybox",
		Tag:         "1.36",
		LocalDigest: pipelineDigestA,
		Supported:   true,
	}}, now); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if err := db.ImageIntel.RecordCheck(ctx, store.CheckOutcome{
		Reference: "docker.io/library/busybox:1.36",
		Status:    domain.CheckOK,
		// The CURRENT tag's digest...
		RemoteDigest: pipelineDigestA,
		Update:       domain.UpdateMinor,
		// ...and the PROPOSED tag with a different one of its own.
		LatestTag:    "1.38",
		LatestDigest: pipelineDigestB,
		UpdateReason: "a newer tag is published in the same series",
		NextCheckAt:  now.Add(time.Hour),
	}, now); err != nil {
		t.Fatalf("apply outcome: %v", err)
	}

	records, _, err := db.ImageIntel.List(ctx, store.ImageIntelFilter{Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	record := records[0]

	if record.LatestTag != "1.38" {
		t.Errorf("latestTag = %q", record.LatestTag)
	}
	if record.LatestDigest != pipelineDigestB {
		t.Errorf("latestDigest = %q, want the proposed tag's own digest", record.LatestDigest)
	}
	if record.LatestDigest == record.RemoteDigest {
		t.Error("the proposed tag was persisted with the CURRENT tag's digest")
	}
}

// TestAPlanWithACrossedTargetCannotBeStored is the persistence gate.
//
// A reference with no usable digest is unpinnable, and acquisition pins its
// pull to exactly this value. Refusing the whole batch is deliberate: a planner
// producing unpinnable plans is a defect, and dropping some of its output
// silently would hide it.
func TestAPlanWithACrossedTargetCannotBeStored(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	base := domain.ChangePlan{
		PlanID:           domain.NewPlanID(),
		ContainerID:      "container-a",
		ContainerName:    "web",
		CurrentImage:     "busybox:1.36",
		UpdateType:       domain.UpdateMinor,
		RestoreReadiness: domain.ReadinessUnknown,
		RegistryStatus:   domain.CheckOK,
		InputDigest:      strings.Repeat("f", 64),
		GeneratedAt:      now,
		PlanVersion:      domain.PlanSchemaVersion,
		PlannerVersion:   domain.PlannerVersion,
		Risk: domain.RiskAssessment{
			Score:          10,
			Band:           domain.RiskLow,
			Recommendation: domain.RecommendCaution,
			Summary:        "a minor version change",
		},
	}

	cases := map[string]domain.ChangePlan{
		"a reference with no digest": func() domain.ChangePlan {
			p := base
			p.ProposedImage = "busybox:1.38"
			return p
		}(),
		"a digest with no reference": func() domain.ChangePlan {
			p := base
			p.ProposedDigest = pipelineDigestB
			return p
		}(),
		"a malformed digest": func() domain.ChangePlan {
			p := base
			p.ProposedImage = "busybox:1.38"
			p.ProposedDigest = "sha256:abcd"
			return p
		}(),
	}

	for name, plan := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := db.Plans.InsertPlans(ctx, []domain.ChangePlan{plan}, now)
			if err == nil {
				t.Fatal("an unpinnable plan was stored")
			}
			if !strings.Contains(err.Error(), "do not belong together") {
				t.Errorf("err = %v, want the crossed-target refusal", err)
			}
		})
	}

	// A whole pair, and a plan proposing nothing, both store.
	whole := base
	whole.PlanID = domain.NewPlanID()
	whole.ProposedImage = "busybox:1.38"
	whole.ProposedDigest = pipelineDigestB
	if _, err := db.Plans.InsertPlans(ctx, []domain.ChangePlan{whole}, now); err != nil {
		t.Errorf("a well-formed plan was refused: %v", err)
	}

	nothing := base
	nothing.PlanID = domain.NewPlanID()
	nothing.ContainerID = "container-b"
	nothing.InputDigest = strings.Repeat("e", 64)
	if _, err := db.Plans.InsertPlans(ctx, []domain.ChangePlan{nothing}, now); err != nil {
		t.Errorf("a plan proposing nothing was refused: %v", err)
	}
}
