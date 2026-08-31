package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Real SQLite, real migrations, the real inventory path. No fakes.
func TestC3DRealDatabasePresenceTransition(t *testing.T) {
	db, ctx := preferenceRepo(t)
	presentContainer(t, db, "svc-a")
	plan := planForContainer(t, db, "svc-a")

	readPresent := func() int {
		var present int
		if err := db.SQL().QueryRowContext(context.Background(),
			`SELECT present FROM containers WHERE id = ?`, "svc-a-id").Scan(&present); err != nil {
			t.Fatalf("read present: %v", err)
		}
		return present
	}

	if readPresent() != 1 {
		t.Fatal("the container is not present to begin with")
	}
	if _, err := db.Plans.Current(ctx, "svc-a-id"); err != nil {
		t.Fatalf("present=1 must allow a current plan: %v", err)
	}

	// Marked absent through the REAL inventory mechanism.
	presentContainer(t, db, "other")

	if got := readPresent(); got != 0 {
		t.Fatalf("present = %d after the refresh, want 0", got)
	}
	if _, err := db.Plans.Current(ctx, "svc-a-id"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Current: %v", err)
	}
	if s, err := db.Plans.Summary(ctx); err != nil || s.Plans != 0 {
		t.Errorf("summary plans = %d (err %v), want 0", s.Plans, err)
	}
	if _, total, err := db.Plans.List(ctx, store.PlanFilter{CurrentOnly: true}); err != nil || total != 0 {
		t.Errorf("currentOnly total = %d (err %v), want 0", total, err)
	}
	if _, total, err := db.Plans.History(ctx, "svc-a-id", store.Page{}); err != nil || total != 1 {
		t.Errorf("history total = %d (err %v), want 1", total, err)
	}
	if got, err := db.Plans.Get(ctx, plan.PlanID); err != nil || got.PlanID != plan.PlanID {
		t.Errorf("the plan is unreadable: %v", err)
	}
	// The row itself was never touched.
	var rows int
	if err := db.SQL().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM change_plans WHERE plan_id = ?`, plan.PlanID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("change_plans rows for the plan = %d, want 1", rows)
	}
	_ = domain.UpdatePatch
}
