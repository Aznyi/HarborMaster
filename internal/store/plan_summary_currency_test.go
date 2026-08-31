package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// One definition of "current", used by everything that claims to show it (C3C).
//
// # The bug
//
// C3B taught Current and the currentOnly listing that settled registry evidence
// retires a plan. The dashboard aggregate was not taught: it counted the newest
// row per container and nothing more, so a retired plan still contributed to the
// available-update count, the needs-review count, the risk bands and the
// container total. An operator saw one more update than existed and could not
// find it on any page, because the pages had already stopped listing it.
//
// # The rule
//
// Retirement is ONE SQL fragment now -- retiredByEvidenceSQL -- shared textually
// by Current, the listing, the summary and the container attention projection.
// These tests exist to prove the four agree, because two definitions of
// "current" is exactly how a dashboard comes to disagree with the page it links
// to.

// summaryPlan writes one plan for a container running currencyImage.
func summaryPlan(
	t *testing.T, db *store.DB, containerID, name string,
	update domain.UpdateType, recommendation domain.Recommendation,
) domain.ChangePlan {
	t.Helper()
	return currencyPlan(t, db, containerID, name, update, recommendation)
}

// ------------------------------------------------------ the dashboard bug --

func TestARetiredPlanLeavesTheDashboardSummary(t *testing.T) {
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")
	summaryPlan(t, db, "svc-a-id", "svc-a", domain.UpdatePatch, domain.RecommendProceed)

	before, err := db.Plans.Summary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if before.Plans != 1 || before.Containers != 1 {
		t.Fatalf("before settling: plans=%d containers=%d, want 1/1",
			before.Plans, before.Containers)
	}
	if before.ByUpdateType[domain.UpdatePatch] != 1 {
		t.Fatalf("before settling: patch=%d, want 1", before.ByUpdateType[domain.UpdatePatch])
	}

	// The registry answers: nothing newer. The plan is retired.
	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdateNone, true)

	after, err := db.Plans.Summary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if after.Plans != 0 {
		t.Errorf("plans = %d, want 0 -- a retired plan is not a current one", after.Plans)
	}
	if after.Containers != 0 {
		t.Errorf("containers = %d, want 0", after.Containers)
	}
	if after.ByUpdateType[domain.UpdatePatch] != 0 {
		t.Errorf("an available-update count still includes a retired plan: %d",
			after.ByUpdateType[domain.UpdatePatch])
	}
	if after.ByRecommendation[domain.RecommendProceed] != 0 {
		t.Errorf("an actionable count still includes a retired plan: %d",
			after.ByRecommendation[domain.RecommendProceed])
	}
	if after.ByBand[domain.RiskVeryLow] != 0 {
		t.Errorf("a risk band still includes a retired plan: %d",
			after.ByBand[domain.RiskVeryLow])
	}
}

func TestARetiredReviewPlanLeavesTheReviewCount(t *testing.T) {
	// The one an operator would act on: a review request for a container the
	// registry has already confirmed is current.
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")
	summaryPlan(t, db, "svc-a-id", "svc-a", domain.UpdateMinor, domain.RecommendManualReview)

	before, _ := db.Plans.Summary(ctx)
	if before.ByRecommendation[domain.RecommendManualReview] != 1 {
		t.Fatalf("review count before = %d, want 1",
			before.ByRecommendation[domain.RecommendManualReview])
	}

	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdateNone, true)

	after, _ := db.Plans.Summary(ctx)
	if after.ByRecommendation[domain.RecommendManualReview] != 0 {
		t.Fatalf("review count = %d, want 0 -- nothing is waiting on a person "+
			"for a container that is current",
			after.ByRecommendation[domain.RecommendManualReview])
	}
}

func TestANewActionablePlanIsCountedExactlyOnce(t *testing.T) {
	// After retirement, a real update appears. Exactly one current plan, counted
	// once -- not the retired one, and not both.
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")
	summaryPlan(t, db, "svc-a-id", "svc-a", domain.UpdateUnknown, domain.RecommendUnknown)
	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdateNone, true)

	if s, _ := db.Plans.Summary(ctx); s.Plans != 0 {
		t.Fatalf("plans = %d after retirement, want 0", s.Plans)
	}

	// The registry finds an update, and the planner writes a new plan.
	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdatePatch, true)
	fresh := summaryPlan(t, db, "svc-a-id", "svc-a", domain.UpdatePatch, domain.RecommendProceed)

	after, err := db.Plans.Summary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if after.Plans != 1 || after.Containers != 1 {
		t.Fatalf("plans=%d containers=%d, want 1/1", after.Plans, after.Containers)
	}
	if after.ByUpdateType[domain.UpdatePatch] != 1 {
		t.Errorf("patch = %d, want exactly 1", after.ByUpdateType[domain.UpdatePatch])
	}
	current, err := db.Plans.Current(ctx, "svc-a-id")
	if err != nil || current.PlanID != fresh.PlanID {
		t.Fatalf("current = %+v (err %v), want %q", current, err, fresh.PlanID)
	}
}

// ------------------------------------------- the four consumers agree --

func TestEveryCurrentStateConsumerAgrees(t *testing.T) {
	// The property C3C exists for. Four surfaces claim to show "current":
	// Current, the currentOnly listing, the dashboard summary and the container
	// attention projection. They now share one SQL fragment, and this walks all
	// four through the same transition.
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")
	plan := summaryPlan(t, db, "svc-a-id", "svc-a", domain.UpdatePatch, domain.RecommendProceed)

	assertAll := func(stage string, wantCurrent bool) {
		t.Helper()

		_, err := db.Plans.Current(ctx, "svc-a-id")
		gotCurrent := err == nil
		if !gotCurrent && !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: Current: %v", stage, err)
		}

		listed, total, err := db.Plans.List(ctx, store.PlanFilter{CurrentOnly: true})
		if err != nil {
			t.Fatalf("%s: List: %v", stage, err)
		}

		summary, err := db.Plans.Summary(ctx)
		if err != nil {
			t.Fatalf("%s: Summary: %v", stage, err)
		}

		evidence, err := db.Containers.Attention(ctx,
			[]store.ContainerKey{{ID: "svc-a-id", Name: "svc-a"}})
		if err != nil {
			t.Fatalf("%s: Attention: %v", stage, err)
		}
		gotPlanKnown := evidence["svc-a-id"].PlanKnown

		if gotCurrent != wantCurrent {
			t.Errorf("%s: Current says %v, want %v", stage, gotCurrent, wantCurrent)
		}
		if (len(listed) == 1) != wantCurrent || (total == 1) != wantCurrent {
			t.Errorf("%s: listing says rows=%d total=%d, want current=%v",
				stage, len(listed), total, wantCurrent)
		}
		if (summary.Plans == 1) != wantCurrent {
			t.Errorf("%s: summary says plans=%d, want current=%v",
				stage, summary.Plans, wantCurrent)
		}
		if gotPlanKnown != wantCurrent {
			t.Errorf("%s: attention says planKnown=%v, want %v",
				stage, gotPlanKnown, wantCurrent)
		}
	}

	assertAll("before the registry answers", true)

	// C3D: the OTHER reason a plan stops being current. Nothing about the
	// registry changed here -- the container simply left.
	commitContainers(t, db, "other")
	assertAll("while the container is absent", false)

	// And back. Currency is derived, so presence is current evidence rather
	// than a destructive transition: nothing was written when it left and
	// nothing has to be undone.
	commitContainers(t, db, "svc-a")
	assertAll("after the container returns", true)

	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdateNone, true)
	assertAll("after settled evidence retires it", false)

	// Both reasons at once. Still one answer, from all four.
	commitContainers(t, db, "other")
	assertAll("absent AND retired", false)

	// And history is untouched throughout.
	if got, err := db.Plans.Get(ctx, plan.PlanID); err != nil || got.PlanID != plan.PlanID {
		t.Fatalf("the historical plan was lost: %v", err)
	}
	all, total, err := db.Plans.List(ctx, store.PlanFilter{CurrentOnly: false})
	if err != nil || total != 1 || len(all) != 1 {
		t.Fatalf("history list: rows=%d total=%d err=%v", len(all), total, err)
	}
}

// ------------------------------------------------- canonical identity --

func TestASupportedReferenceGetsItsCanonicalIdentity(t *testing.T) {
	// The invariant migration 0033 documents:
	//   image_canonical == NormalizeImageRef(image_ref).Canonical
	db, _ := preferenceRepo(t)
	commitContainersWithImage(t, db, "nginx:1.27.0-alpine", "svc-a")

	want, err := domain.NormalizeImageRef("nginx:1.27.0-alpine")
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if got := readCanonical(t, db, "svc-a-id"); got != want.Canonical {
		t.Fatalf("image_canonical = %q, want %q", got, want.Canonical)
	}
	// The raw reference is untouched: it is the source input, not a projection.
	if got := readRawRef(t, db, "svc-a-id"); got != "nginx:1.27.0-alpine" {
		t.Errorf("image_ref was rewritten: %q", got)
	}
}

func TestAnUnsupportedReferenceAssertsNoCanonicalIdentity(t *testing.T) {
	// Empty is not "no registry"; it is "HarborMaster will not claim an
	// identity for this". Nothing may be invented for it.
	db, _ := preferenceRepo(t)
	commitContainersWithImage(t, db, "not a valid @@ reference", "svc-a")

	if got := readCanonical(t, db, "svc-a-id"); got != "" {
		t.Fatalf("image_canonical = %q, want empty -- a reference the domain "+
			"refuses must not be given an invented identity", got)
	}
}

func TestChangingTheImageUpdatesTheCanonicalIdentity(t *testing.T) {
	// The column is DERIVED, so it must follow its source on every write.
	// A stale canonical value would point currency at the wrong image.
	db, _ := preferenceRepo(t)
	commitContainersWithImage(t, db, "nginx:1.27.0-alpine", "svc-a")
	first := readCanonical(t, db, "svc-a-id")

	commitContainersWithImage(t, db, "redis:7.2-alpine", "svc-a")
	second := readCanonical(t, db, "svc-a-id")

	if first == second {
		t.Fatalf("the canonical identity did not follow the image: still %q", first)
	}
	want, _ := domain.NormalizeImageRef("redis:7.2-alpine")
	if second != want.Canonical {
		t.Fatalf("image_canonical = %q, want %q", second, want.Canonical)
	}
}

func TestTheCanonicalIdentitySurvivesReadback(t *testing.T) {
	// Persisted, not computed per read. A value that vanished on reopen would
	// make currency depend on process lifetime.
	db, _ := preferenceRepo(t)
	commitContainersWithImage(t, db, "nginx:1.27.0-alpine", "svc-a")
	want := readCanonical(t, db, "svc-a-id")
	if want == "" {
		t.Fatal("nothing was stored to read back")
	}
	// A second read through a fresh statement, after other writes.
	commitContainersWithImage(t, db, "nginx:1.27.0-alpine", "svc-a", "svc-b")
	if got := readCanonical(t, db, "svc-a-id"); got != want {
		t.Fatalf("image_canonical = %q after a later write, want %q", got, want)
	}
}

// ------------------------------------------------------------- cost --

func TestCurrencyStaysBoundedAcrossAHundredContainers(t *testing.T) {
	db, ctx := preferenceRepo(t)

	const containers = 120
	names := make([]string, 0, containers)
	for i := 0; i < containers; i++ {
		names = append(names, fmt.Sprintf("svc-%03d", i))
	}
	commitContainers(t, db, names...)
	for _, name := range names {
		summaryPlan(t, db, name+"-id", name, domain.UpdatePatch, domain.RecommendProceed)
	}
	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdatePatch, true)

	start := time.Now()
	summary, err := db.Plans.Summary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	listed, _, err := db.Plans.List(ctx, store.PlanFilter{
		CurrentOnly: true, Page: store.Page{Limit: 200},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	keys := make([]store.ContainerKey, 0, containers)
	for _, name := range names {
		keys = append(keys, store.ContainerKey{ID: name + "-id", Name: name})
	}
	if _, err := db.Containers.Attention(ctx, keys); err != nil {
		t.Fatalf("attention: %v", err)
	}
	elapsed := time.Since(start)

	if summary.Plans != containers || len(listed) != containers {
		t.Fatalf("summary=%d listed=%d, want %d", summary.Plans, len(listed), containers)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("summary + listing + attention over %d containers took %s; a "+
			"per-row read has crept in", containers, elapsed)
	}
	t.Logf("%d containers: summary + listing + attention in %s", containers, elapsed)
}

// readCanonical reads the derived column straight from the row.
func readCanonical(t *testing.T, db *store.DB, containerID string) string {
	t.Helper()
	return readContainerColumn(t, db, "image_canonical", containerID)
}

func readRawRef(t *testing.T, db *store.DB, containerID string) string {
	t.Helper()
	return readContainerColumn(t, db, "image_ref", containerID)
}

// readContainerColumn reads one column. The name is a literal from this file,
// never caller text.
func readContainerColumn(t *testing.T, db *store.DB, column, containerID string) string {
	t.Helper()
	var value string
	if err := db.SQL().QueryRowContext(context.Background(),
		`SELECT `+column+` FROM containers WHERE id = ?`, containerID).Scan(&value); err != nil {
		t.Fatalf("read %s: %v", column, err)
	}
	return value
}
