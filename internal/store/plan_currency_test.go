package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// When a plan stops being HarborMaster's current decision (C3B).
//
// # The two ends of currency
//
// A plan is superseded when a NEWER PLAN replaces it. That derivation has
// always existed and it is correct as far as it goes.
//
// It cannot see the other end. The planner's own rule is to write NO plan for a
// container whose registry comparison settled on "nothing newer" -- a plan is a
// proposed change, and there is none. Plans are written before the first check
// completes, when nothing is established and the honest verdict is `unknown`;
// once the check settles, the planner skips that container permanently and
// never writes a superseding row. The pre-check plan stays newest forever.
//
// So "newest row" and "current decision" are different questions, and these
// pin that PlanRepository.Current answers the second.
//
// # What must NOT change
//
// The historical row. Nothing here deletes, rewrites or hides a plan: History
// still returns it, and an execution that ran under it is still explicable.

const currencyImage = "ghcr.io/acme/service:1.0.0"

// currencyFingerprint returns a fresh plan input digest.
var currencyPlanCounter int

func currencyFingerprint() string {
	currencyPlanCounter++
	return strings.Repeat("a", 60) + fmt.Sprintf("%04d", currencyPlanCounter)
}

// currencyPlan writes one plan for a container, with a chosen verdict.
func currencyPlan(
	t *testing.T, db *store.DB, containerID, name string,
	update domain.UpdateType, recommendation domain.Recommendation,
) domain.ChangePlan {
	t.Helper()
	plan := domain.ChangePlan{
		PlanID:        domain.NewPlanID(),
		ContainerID:   containerID,
		ContainerName: name,
		CurrentImage:  currencyImage,
		UpdateType:    update,
		Risk: domain.RiskAssessment{
			Recommendation: recommendation,
			Band:           domain.RiskVeryLow,
			Summary:        "test",
		},
		RegistryStatus:   domain.CheckPending,
		RestoreReadiness: domain.ReadinessUnknown,
		PlanVersion:      domain.PlanSchemaVersion,
		PlannerVersion:   domain.PlannerVersion,
		// A distinct fingerprint per plan. Duplicate suppression compares this,
		// so two plans built from identical inputs are deliberately ONE row --
		// correct behaviour, and not what these tests exercise.
		InputDigest: currencyFingerprint(),
		GeneratedAt: time.Now().UTC(),
	}
	if _, err := db.Plans.InsertPlans(context.Background(),
		[]domain.ChangePlan{plan}, time.Now().UTC()); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	stored, err := db.Plans.Get(context.Background(), plan.PlanID)
	if err != nil {
		t.Fatalf("read back plan: %v", err)
	}
	return stored
}

// ------------------------------------------------- the C3A case, at the store --

func TestASettledComparisonRetiresTheNewestPlan(t *testing.T) {
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")

	// A plan written while the registry had not answered: `unknown`, which is
	// the honest verdict at that moment (B1.1).
	stale := currencyPlan(t, db, "svc-a-id", "svc-a", domain.UpdateUnknown, domain.RecommendUnknown)

	// Before the check settles it is the current decision, correctly.
	if _, err := db.Plans.Current(ctx, "svc-a-id"); err != nil {
		t.Fatalf("a pre-check plan must be current until evidence says otherwise: %v", err)
	}

	// The registry answers: nothing newer.
	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdateNone, true)

	_, err := db.Plans.Current(ctx, "svc-a-id")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Current returned a plan the planner would no longer write (err %v); "+
			"settled evidence must retire it", err)
	}

	// AND THE ROW IS STILL THERE. Retirement is not deletion.
	history, total, err := db.Plans.History(ctx, "svc-a-id", store.Page{})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if total != 1 || len(history) != 1 || history[0].PlanID != stale.PlanID {
		t.Fatalf("the historical plan was lost: total=%d rows=%d", total, len(history))
	}
	if got, err := db.Plans.Get(ctx, stale.PlanID); err != nil || got.PlanID != stale.PlanID {
		t.Fatalf("the plan is no longer readable by id: %+v (err %v)", got, err)
	}
}

func TestAnUnsettledComparisonRetiresNothing(t *testing.T) {
	// The failure direction that matters. A check that never answered
	// establishes nothing, so it must not retire a plan -- doing so would HIDE
	// an actionable update behind a lookup that failed.
	for _, status := range []domain.CheckStatus{
		domain.CheckPending,
		domain.CheckFailed,
		domain.CheckUnauthorized,
		domain.CheckRateLimited,
		domain.CheckNotFound,
		domain.CheckUnsupported,
	} {
		t.Run(string(status), func(t *testing.T) {
			db, ctx := preferenceRepo(t)
			commitContainers(t, db, "svc-a")
			currencyPlan(t, db, "svc-a-id", "svc-a", domain.UpdatePatch, domain.RecommendProceed)
			seedIntel(t, db, currencyImage, status, domain.UpdateNone, false)

			if _, err := db.Plans.Current(ctx, "svc-a-id"); err != nil {
				t.Fatalf("status %q retired a plan on evidence that never "+
					"answered: %v", status, err)
			}
		})
	}
}

func TestASettledUpdateDoesNotRetireItsOwnPlan(t *testing.T) {
	// A comparison that answered and DID find an update leaves the plan alone:
	// it is exactly the plan the planner would write today.
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")
	currencyPlan(t, db, "svc-a-id", "svc-a", domain.UpdatePatch, domain.RecommendProceed)
	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdatePatch, true)

	if _, err := db.Plans.Current(ctx, "svc-a-id"); err != nil {
		t.Fatalf("an actionable plan was retired: %v", err)
	}
}

func TestANewerPlanStillSupersedesTheOlderOne(t *testing.T) {
	// The original derivation, unchanged. Exactly one current plan.
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")

	first := currencyPlan(t, db, "svc-a-id", "svc-a", domain.UpdateUnknown, domain.RecommendUnknown)
	second := currencyPlan(t, db, "svc-a-id", "svc-a", domain.UpdatePatch, domain.RecommendProceed)
	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdatePatch, true)

	current, err := db.Plans.Current(ctx, "svc-a-id")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current.PlanID != second.PlanID {
		t.Fatalf("current = %q, want the newer plan %q", current.PlanID, second.PlanID)
	}

	// And the older one reports itself superseded, still readable.
	older, err := db.Plans.Get(ctx, first.PlanID)
	if err != nil {
		t.Fatalf("the older plan is unreadable: %v", err)
	}
	if !older.Superseded {
		t.Error("the older plan does not report itself superseded")
	}
}

func TestRetirementSurvivesRecreationUnderTheSameName(t *testing.T) {
	// A recreation gives the workload a NEW container id. The old id's plan is
	// history and the new container starts with none; neither may be presented
	// as a current decision for a container that has settled evidence.
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")
	old := currencyPlan(t, db, "svc-a-id", "svc-a", domain.UpdatePatch, domain.RecommendProceed)

	// The update is applied: same name, new id.
	commitContainersAs(t, db, map[string]string{"svc-a": "svc-a-v2-id"})
	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdateNone, true)

	// The replacement has no plan at all.
	if _, err := db.Plans.Current(ctx, "svc-a-v2-id"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the replacement reported a current plan: %v", err)
	}
	// The old container is gone, so its plan is not a current decision either.
	// It remains readable as history.
	if got, err := db.Plans.Get(ctx, old.PlanID); err != nil || got.ContainerID != "svc-a-id" {
		t.Fatalf("the historical plan lost its original container id: %+v (err %v)", got, err)
	}
}

func TestADisappearedContainerKeepsItsPlanReadable(t *testing.T) {
	// Nothing about a removed container may delete or rewrite what HarborMaster
	// decided while it was there.
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a", "other")
	plan := currencyPlan(t, db, "svc-a-id", "svc-a", domain.UpdatePatch, domain.RecommendProceed)

	commitContainers(t, db, "other")

	if got, err := db.Plans.Get(ctx, plan.PlanID); err != nil || got.PlanID != plan.PlanID {
		t.Fatalf("the plan of a removed container was lost: %v", err)
	}
	_, total, err := db.Plans.History(ctx, "svc-a-id", store.Page{})
	if err != nil || total != 1 {
		t.Fatalf("history for a removed container: total=%d err=%v", total, err)
	}
}

func TestRetirementIsIdempotent(t *testing.T) {
	// Reading currency repeatedly writes nothing and produces no second
	// record. Retirement is derived; there is nothing to duplicate.
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")
	currencyPlan(t, db, "svc-a-id", "svc-a", domain.UpdateUnknown, domain.RecommendUnknown)
	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdateNone, true)

	for i := 0; i < 5; i++ {
		if _, err := db.Plans.Current(ctx, "svc-a-id"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	_, total, err := db.Plans.History(ctx, "svc-a-id", store.Page{})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if total != 1 {
		t.Fatalf("reading currency created rows: %d plans exist, want 1", total)
	}
}

func TestTheCurrentOnlyListingExcludesRetiredPlans(t *testing.T) {
	// The Updates workspace reads this listing. A retired plan surfacing here
	// is an operator being offered a review for a container the registry has
	// already confirmed is current.
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")
	retired := currencyPlan(t, db, "svc-a-id", "svc-a", domain.UpdateUnknown, domain.RecommendUnknown)

	// Before settling it is listed, correctly.
	before, beforeTotal, err := db.Plans.List(ctx, store.PlanFilter{CurrentOnly: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(before) != 1 || beforeTotal != 1 {
		t.Fatalf("before settling: %d rows, total %d, want 1/1", len(before), beforeTotal)
	}

	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdateNone, true)

	after, afterTotal, err := db.Plans.List(ctx, store.PlanFilter{CurrentOnly: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("a retired plan is still listed as current: %+v", after)
	}
	// The total moves with the rows. A caller told "1 current plan" and shown
	// none would read it as a rendering bug.
	if afterTotal != 0 {
		t.Fatalf("total = %d, want 0", afterTotal)
	}

	// And it is still there for anyone asking for history.
	all, allTotal, err := db.Plans.List(ctx, store.PlanFilter{CurrentOnly: false})
	if err != nil {
		t.Fatalf("history list: %v", err)
	}
	if allTotal != 1 || len(all) != 1 || all[0].PlanID != retired.PlanID {
		t.Fatalf("history lost the retired plan: %d rows, total %d", len(all), allTotal)
	}
}

func TestTheListingRetirementIsBatched(t *testing.T) {
	// One pass over the page, not a lookup per row. A page of plans sharing an
	// image must cost the same as a page of one.
	db, ctx := preferenceRepo(t)

	names := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		names = append(names, fmt.Sprintf("svc-%03d", i))
	}
	commitContainers(t, db, names...)
	for _, name := range names {
		currencyPlan(t, db, name+"-id", name, domain.UpdatePatch, domain.RecommendProceed)
	}
	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdatePatch, true)

	start := time.Now()
	listed, total, err := db.Plans.List(ctx, store.PlanFilter{
		CurrentOnly: true, Page: store.Page{Limit: 200},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != len(names) || total != len(names) {
		t.Fatalf("got %d rows / total %d, want %d", len(listed), total, len(names))
	}
	if elapsed > 2*time.Second {
		t.Fatalf("listing %d current plans took %s; a per-row lookup has crept in",
			len(names), elapsed)
	}
}

func TestCurrencyCostsOneExtraBoundedRead(t *testing.T) {
	// Currency is a per-container question asked by preflights, so it must stay
	// a small bounded lookup rather than growing with the estate.
	db, ctx := preferenceRepo(t)

	names := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		names = append(names, "svc-"+string(rune('a'+i/26))+string(rune('a'+i%26)))
	}
	commitContainers(t, db, names...)
	for _, name := range names {
		currencyPlan(t, db, name+"-id", name, domain.UpdatePatch, domain.RecommendProceed)
	}
	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdatePatch, true)

	start := time.Now()
	for _, name := range names {
		if _, err := db.Plans.Current(ctx, name+"-id"); err != nil {
			t.Fatalf("current for %s: %v", name, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("%d currency reads took %s; the lookup is not bounded", len(names), elapsed)
	}
	t.Logf("%d currency reads in %s", len(names), elapsed)
}
