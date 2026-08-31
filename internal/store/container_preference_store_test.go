package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Per-container update behaviour, in the database (C2).
//
// # The property this file exists for
//
// A preference has to survive the thing it authorises. HarborMaster updates a
// container by RECREATING it, and the replacement has a different Docker id --
// so a preference keyed on the id would be discarded by the first update it
// permitted, at the worst possible moment.
//
// The key is therefore the container NAME, which is the identity
// `automation_pauses` and `image_lineage` already use for the same reason. The
// recreation test below is the one that would have caught the wrong choice.

func preferenceRepo(t *testing.T) (*store.DB, context.Context) {
	t.Helper()
	return openTestDB(t), context.Background()
}

var preferenceNow = time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)

func TestAPreferenceRoundTrips(t *testing.T) {
	db, ctx := preferenceRepo(t)

	stored, err := db.ContainerPreferences.SetContainerPreference(ctx,
		domain.ContainerUpdatePreference{
			ContainerName: "vaultwarden",
			Behavior:      domain.BehaviorReviewFirst,
			ContainerID:   "abc123",
		}, "usr_1", "operator", preferenceNow)
	if err != nil {
		t.Fatalf("SetContainerPreference: %v", err)
	}
	if stored.Behavior != domain.BehaviorReviewFirst || stored.ContainerName != "vaultwarden" {
		t.Fatalf("stored = %+v", stored)
	}
	if stored.SetByUsername != "operator" {
		t.Errorf("the choice records no actor: %+v", stored)
	}

	read, err := db.ContainerPreferences.ContainerPreference(ctx, "vaultwarden")
	if err != nil {
		t.Fatalf("ContainerPreference: %v", err)
	}
	if read.Behavior != domain.BehaviorReviewFirst {
		t.Errorf("read back %q", read.Behavior)
	}
}

func TestAnAbsentPreferenceIsNotAnError(t *testing.T) {
	db, ctx := preferenceRepo(t)
	// "Nobody has chosen" is a state the caller must be able to tell from a
	// failure, because it means "inherit" rather than "something went wrong".
	if _, err := db.ContainerPreferences.ContainerPreference(ctx, "never-set"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestChoosingTwiceEditsOneRow(t *testing.T) {
	db, ctx := preferenceRepo(t)
	for _, behavior := range []domain.UpdateBehavior{
		domain.BehaviorAutomatic, domain.BehaviorMonitorOnly, domain.BehaviorReviewFirst,
	} {
		if _, err := db.ContainerPreferences.SetContainerPreference(ctx,
			domain.ContainerUpdatePreference{ContainerName: "web", Behavior: behavior},
			"usr_1", "operator", preferenceNow); err != nil {
			t.Fatalf("set %q: %v", behavior, err)
		}
	}

	all, err := db.ContainerPreferences.ListContainerPreferences(ctx)
	if err != nil {
		t.Fatalf("ListContainerPreferences: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("stored %d rows for one container; the order of rows would decide the answer", len(all))
	}
	if all[0].Behavior != domain.BehaviorReviewFirst {
		t.Errorf("last choice = %q, want the most recent", all[0].Behavior)
	}
}

// THE C2 PROPERTY. A preference must outlive the recreation it authorised.
func TestAPreferenceSurvivesTheContainerBeingRecreated(t *testing.T) {
	db, ctx := preferenceRepo(t)

	// The operator chooses, while the container has one id.
	if _, err := db.ContainerPreferences.SetContainerPreference(ctx,
		domain.ContainerUpdatePreference{
			ContainerName: "vaultwarden",
			Behavior:      domain.BehaviorMonitorOnly,
			ContainerID:   "0000000000000000000000000000000000000000000000000000000000000000",
		}, "usr_1", "operator", preferenceNow); err != nil {
		t.Fatalf("set: %v", err)
	}

	// HarborMaster updates the container. The replacement has a DIFFERENT id
	// and the same name -- which is exactly what a recreation produces.
	const replacementID = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

	// The preference is still found, by the identity that did not change.
	read, err := db.ContainerPreferences.ContainerPreference(ctx, "vaultwarden")
	if err != nil {
		t.Fatalf("the preference did not survive recreation: %v", err)
	}
	if read.Behavior != domain.BehaviorMonitorOnly {
		t.Errorf("behaviour after recreation = %q, want monitorOnly", read.Behavior)
	}
	// And the recorded id is evidence only: writing the new one changes nothing
	// about which preference applies.
	if _, err := db.ContainerPreferences.SetContainerPreference(ctx,
		domain.ContainerUpdatePreference{
			ContainerName: "vaultwarden",
			Behavior:      read.Behavior,
			ContainerID:   replacementID,
		}, "usr_1", "operator", preferenceNow); err != nil {
		t.Fatalf("re-record after recreation: %v", err)
	}
	again, err := db.ContainerPreferences.ContainerPreference(ctx, "vaultwarden")
	if err != nil || again.Behavior != domain.BehaviorMonitorOnly {
		t.Fatalf("after recording the new id: %+v (err %v)", again, err)
	}

	all, _ := db.ContainerPreferences.ListContainerPreferences(ctx)
	if len(all) != 1 {
		t.Errorf("recreation produced %d preference rows; the id must not be part of the key", len(all))
	}
}

func TestClearingReturnsAContainerToInheriting(t *testing.T) {
	db, ctx := preferenceRepo(t)
	if _, err := db.ContainerPreferences.SetContainerPreference(ctx,
		domain.ContainerUpdatePreference{ContainerName: "web", Behavior: domain.BehaviorMonitorOnly},
		"usr_1", "operator", preferenceNow); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := db.ContainerPreferences.ClearContainerPreference(ctx, "web"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := db.ContainerPreferences.ContainerPreference(ctx, "web"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want the container to be inheriting again", err)
	}
	// Clearing what was never set is the state the caller asked for.
	if err := db.ContainerPreferences.ClearContainerPreference(ctx, "never-set"); err != nil {
		t.Errorf("clearing an absent preference failed: %v", err)
	}
}

func TestClearingOneContainerLeavesTheOthersAlone(t *testing.T) {
	db, ctx := preferenceRepo(t)
	for _, name := range []string{"web", "database", "cache"} {
		if _, err := db.ContainerPreferences.SetContainerPreference(ctx,
			domain.ContainerUpdatePreference{ContainerName: name, Behavior: domain.BehaviorReviewFirst},
			"usr_1", "operator", preferenceNow); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	if err := db.ContainerPreferences.ClearContainerPreference(ctx, "database"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	all, err := db.ContainerPreferences.ContainerPreferences(ctx)
	if err != nil {
		t.Fatalf("ContainerPreferences: %v", err)
	}
	if len(all) != 2 || all["web"] != domain.BehaviorReviewFirst || all["cache"] != domain.BehaviorReviewFirst {
		t.Errorf("remaining preferences = %v", all)
	}
	if _, still := all["database"]; still {
		t.Error("the cleared container still has a preference")
	}
}

// The engine reads every preference once a pass, by name.
func TestThePassReadsEveryPreferenceByName(t *testing.T) {
	db, ctx := preferenceRepo(t)
	want := map[string]domain.UpdateBehavior{
		"web":      domain.BehaviorAutomatic,
		"database": domain.BehaviorReviewFirst,
		"cache":    domain.BehaviorMonitorOnly,
	}
	for name, behavior := range want {
		if _, err := db.ContainerPreferences.SetContainerPreference(ctx,
			domain.ContainerUpdatePreference{ContainerName: name, Behavior: behavior},
			"usr_1", "operator", preferenceNow); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}

	got, err := db.ContainerPreferences.ContainerPreferences(ctx)
	if err != nil {
		t.Fatalf("ContainerPreferences: %v", err)
	}
	for name, behavior := range want {
		if got[name] != behavior {
			t.Errorf("%s = %q, want %q", name, got[name], behavior)
		}
	}
}

// The schema refuses a behaviour outside the vocabulary, so a value that got
// past validation could not be stored and later read back as something nobody
// chose.
func TestTheSchemaRefusesAnUnknownBehavior(t *testing.T) {
	db, ctx := preferenceRepo(t)
	_, err := db.SQL().ExecContext(ctx, `
		INSERT INTO container_update_preferences
			(container_name, behavior, created_at, updated_at)
		VALUES ('web', 'excluded', '2026-09-02T10:00:00Z', '2026-09-02T10:00:00Z')`)
	if err == nil {
		t.Fatal("the database accepted a behaviour outside the closed vocabulary")
	}
}
