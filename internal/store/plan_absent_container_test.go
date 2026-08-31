package store_test

import (
	"errors"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// A plan is current only while the container it assessed still exists (C3D).
//
// # The gap C3C left
//
// C3C taught the four current-state consumers that settled registry evidence
// retires a plan. It did not teach them the other reason a plan stops being
// current: the container instance it assessed is gone. So a removed container's
// last plan stayed in the dashboard counts and the currentOnly listing
// indefinitely -- an operator saw an update for a container that was not there,
// and could not act on it because the preflights refused the missing container.
//
// # Two reasons, two fragments, one definition
//
// These are different facts and are kept apart deliberately:
//
//	retiredByEvidenceSQL  the registry answered and there is nothing newer
//	containerPresentSQL   the container instance still exists
//
// currentPlanSQL composes them, and every current-state consumer renders that
// and nothing else. Folding presence into "retired by evidence" would have been
// one fewer fragment and a name that lied.
//
// # What does NOT change
//
// No change_plans row is written, altered or deleted by any of this. Currency
// is derived; a plan for an absent container is still fully readable as
// history, and becomes current again by itself if the container comes back.

// absentPlanImage is a reference with no intelligence record in these tests, so
// retirement-by-evidence never fires and PRESENCE is the only thing deciding.
const absentPlanImage = "ghcr.io/acme/absent:1.0.0"

// presentContainer commits one container, and absentContainer removes it by
// committing an inventory that no longer lists it -- the real path, not a
// hand-written UPDATE.
func presentContainer(t *testing.T, db *store.DB, names ...string) {
	t.Helper()
	commitContainersWithImage(t, db, absentPlanImage, names...)
}

// planForContainer writes one plan against a committed container.
func planForContainer(t *testing.T, db *store.DB, name string) domain.ChangePlan {
	t.Helper()
	return currencyPlan(t, db, name+"-id", name, domain.UpdatePatch, domain.RecommendProceed)
}

// ------------------------------------------------- the absent-container rule --

func TestAnAbsentContainerHasNoCurrentPlan(t *testing.T) {
	db, ctx := preferenceRepo(t)
	presentContainer(t, db, "svc-a")
	plan := planForContainer(t, db, "svc-a")

	// While present, it is the current decision.
	if _, err := db.Plans.Current(ctx, "svc-a-id"); err != nil {
		t.Fatalf("a present container's newest plan must be current: %v", err)
	}

	// The container is removed from the host. Committed through the real
	// inventory path, which is what sets present = 0.
	presentContainer(t, db, "other")

	if _, err := db.Plans.Current(ctx, "svc-a-id"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Current returned a plan for a container that is gone (err %v)", err)
	}

	// AND THE ROW IS UNTOUCHED. Currency is derived; history is not.
	if got, err := db.Plans.Get(ctx, plan.PlanID); err != nil || got.PlanID != plan.PlanID {
		t.Fatalf("the historical plan was lost: %v", err)
	}
	history, total, err := db.Plans.History(ctx, "svc-a-id", store.Page{})
	if err != nil || total != 1 || len(history) != 1 {
		t.Fatalf("history: rows=%d total=%d err=%v", len(history), total, err)
	}
	all, allTotal, err := db.Plans.List(ctx, store.PlanFilter{CurrentOnly: false})
	if err != nil || allTotal != 1 {
		t.Fatalf("historical listing: total=%d err=%v", allTotal, err)
	}
	if all[0].PlanID != plan.PlanID {
		t.Errorf("the historical listing lost the plan: %+v", all[0])
	}
}

func TestAnAbsentContainerLeavesEveryDashboardFigure(t *testing.T) {
	db, ctx := preferenceRepo(t)
	presentContainer(t, db, "svc-a")
	planForContainer(t, db, "svc-a")

	before, err := db.Plans.Summary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if before.Plans != 1 || before.Containers != 1 ||
		before.ByUpdateType[domain.UpdatePatch] != 1 ||
		before.ByRecommendation[domain.RecommendProceed] != 1 {
		t.Fatalf("before removal the plan is not counted: %+v", before)
	}

	presentContainer(t, db, "other")

	after, err := db.Plans.Summary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if after.Plans != 0 {
		t.Errorf("plans = %d, want 0", after.Plans)
	}
	if after.Containers != 0 {
		t.Errorf("containers = %d, want 0", after.Containers)
	}
	if after.ByUpdateType[domain.UpdatePatch] != 0 {
		t.Errorf("an available-update count includes a departed container: %d",
			after.ByUpdateType[domain.UpdatePatch])
	}
	if after.ByRecommendation[domain.RecommendProceed] != 0 {
		t.Errorf("an actionable count includes a departed container: %d",
			after.ByRecommendation[domain.RecommendProceed])
	}
	if after.ByBand[domain.RiskVeryLow] != 0 {
		t.Errorf("a risk band includes a departed container: %d",
			after.ByBand[domain.RiskVeryLow])
	}
}

func TestAnAbsentContainerLeavesTheCurrentOnlyListing(t *testing.T) {
	db, ctx := preferenceRepo(t)
	presentContainer(t, db, "svc-a")
	plan := planForContainer(t, db, "svc-a")

	listed, total, err := db.Plans.List(ctx, store.PlanFilter{CurrentOnly: true})
	if err != nil || len(listed) != 1 || total != 1 {
		t.Fatalf("before removal: rows=%d total=%d err=%v", len(listed), total, err)
	}

	presentContainer(t, db, "other")

	listed, total, err = db.Plans.List(ctx, store.PlanFilter{CurrentOnly: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 0 || total != 0 {
		t.Fatalf("a departed container's plan is still listed as current: rows=%d total=%d",
			len(listed), total)
	}

	// Still readable as history, with its identity intact.
	if got, err := db.Plans.Get(ctx, plan.PlanID); err != nil || got.ContainerID != "svc-a-id" {
		t.Fatalf("history lost the plan or its container id: %+v (err %v)", got, err)
	}
}

func TestAnAbsentContainerLeaksNoPlanMetadataIntoAttention(t *testing.T) {
	// The C3C bug in another shape. Even if a caller asks about a departed
	// container by id, its historical plan's proposed image and update type
	// must not come back as CURRENT state.
	db, ctx := preferenceRepo(t)
	presentContainer(t, db, "svc-a")
	planForContainer(t, db, "svc-a")

	keys := []store.ContainerKey{{ID: "svc-a-id", Name: "svc-a"}}

	before, err := db.Containers.Attention(ctx, keys)
	if err != nil {
		t.Fatalf("attention: %v", err)
	}
	if !before["svc-a-id"].PlanKnown {
		t.Fatal("a present container's plan did not reach its attention row")
	}

	presentContainer(t, db, "other")

	after, err := db.Containers.Attention(ctx, keys)
	if err != nil {
		t.Fatalf("attention: %v", err)
	}
	row := after["svc-a-id"]
	if row.PlanKnown {
		t.Error("a departed container's plan is still projected as current")
	}
	if row.UpdateType != "" {
		t.Errorf("updateType leaked: %q", row.UpdateType)
	}
	if row.ProposedImage != "" {
		t.Errorf("proposedImage leaked: %q", row.ProposedImage)
	}
	if row.Recommendation != "" {
		t.Errorf("recommendation leaked: %q", row.Recommendation)
	}
	// And the assessment says so rather than inventing a verdict.
	if got := domain.AssessContainer(row); got.UpdateType != "" || got.ProposedImage != "" {
		t.Errorf("the attention projection carried plan metadata: %+v", got)
	}
}

// -------------------------------------------------- presence is not a tombstone --

func TestAReturningContainerBecomesCurrentAgain(t *testing.T) {
	// Currency is DERIVED. Absence is current evidence, not a destructive
	// lifecycle transition, so nothing had to be written when the container
	// left and nothing has to be undone when it comes back.
	db, ctx := preferenceRepo(t)
	presentContainer(t, db, "svc-a")
	plan := planForContainer(t, db, "svc-a")

	presentContainer(t, db, "other")
	if _, err := db.Plans.Current(ctx, "svc-a-id"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("still current while absent: %v", err)
	}

	// The SAME container id is reported again.
	presentContainer(t, db, "svc-a")

	current, err := db.Plans.Current(ctx, "svc-a-id")
	if err != nil {
		t.Fatalf("a returning container's plan did not become current again: %v", err)
	}
	if current.PlanID != plan.PlanID {
		t.Errorf("current = %q, want the original plan %q", current.PlanID, plan.PlanID)
	}
	// Nothing was rewritten to achieve it.
	if _, total, err := db.Plans.History(ctx, "svc-a-id", store.Page{}); err != nil || total != 1 {
		t.Fatalf("history total = %d (err %v), want 1", total, err)
	}
}

// ---------------------------------------------- a new container, same name --

func TestANewContainerDoesNotInheritTheOldPlan(t *testing.T) {
	// C3C's identity decision, preserved: plans are keyed to the container ID,
	// and a recreation starts a new lineage. The replacement must begin with no
	// assessment rather than adopting one made about a different container.
	db, ctx := preferenceRepo(t)
	presentContainer(t, db, "svc-a")
	old := planForContainer(t, db, "svc-a")

	// Same NAME, new Docker id -- what a recreation produces.
	commitContainersAs(t, db, map[string]string{"svc-a": "svc-a-v2-id"})

	if _, err := db.Plans.Current(ctx, "svc-a-v2-id"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the replacement inherited a plan: %v", err)
	}
	if _, err := db.Plans.Current(ctx, "svc-a-id"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the departed container still has a current plan: %v", err)
	}

	// The old plan is history, still naming the container it actually assessed.
	got, err := db.Plans.Get(ctx, old.PlanID)
	if err != nil {
		t.Fatalf("the historical plan was lost: %v", err)
	}
	if got.ContainerID != "svc-a-id" {
		t.Errorf("the historical plan's container id moved to %q", got.ContainerID)
	}

	// And nothing is current anywhere: the replacement needs its own assessment.
	listed, total, err := db.Plans.List(ctx, store.PlanFilter{CurrentOnly: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 0 || total != 0 {
		t.Fatalf("rows=%d total=%d, want none current", len(listed), total)
	}
}

// ------------------------------------- presence dominates registry state --

func TestAbsenceDominatesEveryRegistryState(t *testing.T) {
	// The interaction matrix. Whatever the registry says about the image, a
	// plan for a container that is not there is not a current decision.
	cases := []struct {
		name    string
		status  domain.CheckStatus
		update  domain.UpdateType
		settled bool
		// wantCurrentWhilePresent is what the registry state alone decides.
		wantCurrentWhilePresent bool
	}{
		{"pending", domain.CheckPending, domain.UpdateNone, false, true},
		{"settled with an update", domain.CheckOK, domain.UpdatePatch, true, true},
		{"settled with no update", domain.CheckOK, domain.UpdateNone, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := preferenceRepo(t)
			presentContainer(t, db, "svc-a")
			planForContainer(t, db, "svc-a")
			seedIntel(t, db, absentPlanImage, tc.status, tc.update, tc.settled)

			_, err := db.Plans.Current(ctx, "svc-a-id")
			gotPresent := err == nil
			if gotPresent != tc.wantCurrentWhilePresent {
				t.Fatalf("while PRESENT current=%v, want %v (err %v)",
					gotPresent, tc.wantCurrentWhilePresent, err)
			}

			// Now absent. Every row must agree: not current.
			presentContainer(t, db, "other")

			if _, err := db.Plans.Current(ctx, "svc-a-id"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("Current while absent: %v", err)
			}
			if _, total, err := db.Plans.List(ctx,
				store.PlanFilter{CurrentOnly: true}); err != nil || total != 0 {
				t.Errorf("currentOnly total = %d (err %v), want 0", total, err)
			}
			if summary, err := db.Plans.Summary(ctx); err != nil || summary.Plans != 0 {
				t.Errorf("summary plans = %d (err %v), want 0", summary.Plans, err)
			}
			// And the registry record itself was not touched.
			if _, err := db.ImageIntel.Get(ctx, canonicalOf(t, absentPlanImage)); err != nil {
				t.Errorf("image intelligence was deleted: %v", err)
			}
		})
	}
}

// canonicalOf resolves a reference the way the write path does.
func canonicalOf(t *testing.T, raw string) string {
	t.Helper()
	reference, err := domain.NormalizeImageRef(raw)
	if err != nil {
		t.Fatalf("normalise %q: %v", raw, err)
	}
	return reference.Canonical
}
