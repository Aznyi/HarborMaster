package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Saved behaviours resolved against the live inventory (C2.2).
//
// # Why presence is resolved in the query
//
// The automation workspace shows which containers carry a saved update
// behaviour. Two facts are needed per row -- the behaviour, and whether the
// container is still here -- and the naive way to get the second is to read the
// list and then look up each container. That is one query per preference on a
// page load, which is the request explosion this batch exists to avoid.
//
// So presence is resolved in the SAME statement. These tests pin that it is
// resolved correctly, and in particular that the CURRENT id is returned rather
// than the one stored beside the preference: the stored id is evidence of when
// the choice was made, and is stale the moment an update recreates the
// container.

// commitContainers commits an inventory containing exactly these names.
//
// A container absent from a later commit is marked not-present rather than
// deleted, which is how a preference comes to outlive its container.
func commitContainers(t *testing.T, db *store.DB, names ...string) {
	t.Helper()

	records := make([]store.ContainerRecord, 0, len(names))
	for _, name := range names {
		id := name + "-id"
		records = append(records, store.ContainerRecord{
			Detail: domain.ContainerDetail{
				Overview: domain.ContainerSummary{
					HostID:    domain.LocalHostID,
					ID:        id,
					ShortID:   domain.ShortenID(id),
					Name:      name,
					Image:     domain.ParseImageRef("ghcr.io/acme/service:1.0.0"),
					ImageID:   "sha256:image1",
					State:     domain.StateRunning,
					Health:    domain.HealthNone,
					CreatedAt: preferenceNow,
					Present:   true,
				},
				State:       domain.StateDetail{State: domain.StateRunning, RawState: "running"},
				Labels:      []domain.Label{},
				Environment: []domain.EnvVar{},
				Mounts:      []domain.Mount{},
				Networks:    []domain.NetworkAttachment{},
				Warnings:    []domain.InventoryWarning{},
			},
			RawJSON: []byte(`{"Id":"` + id + `"}`),
		})
	}

	if _, err := db.Inventory.CommitRefresh(context.Background(), store.RefreshCommit{
		Host:       domain.Host{ID: domain.LocalHostID, Name: "local", Runtime: domain.RuntimeDocker},
		Containers: records,
		Record: domain.RefreshRecord{
			Trigger:          domain.TriggerManual,
			StartedAt:        time.Now().UTC(),
			ContainersListed: len(records),
			Checksum:         time.Now().UTC().Format(time.RFC3339Nano),
		},
		Now: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CommitRefresh: %v", err)
	}
}

func setPreference(t *testing.T, db *store.DB, name string, behavior domain.UpdateBehavior, id string) {
	t.Helper()
	if _, err := db.ContainerPreferences.SetContainerPreference(context.Background(),
		domain.ContainerUpdatePreference{
			ContainerName: name,
			Behavior:      behavior,
			ContainerID:   id,
		}, "usr_1", "operator", preferenceNow); err != nil {
		t.Fatalf("set %q: %v", name, err)
	}
}

func TestPresenceIsResolvedForEverySavedBehaviour(t *testing.T) {
	db, ctx := preferenceRepo(t)

	commitContainers(t, db, "vaultwarden", "grafana")
	setPreference(t, db, "vaultwarden", domain.BehaviorAutomatic, "vaultwarden-id")
	setPreference(t, db, "grafana", domain.BehaviorMonitorOnly, "grafana-id")

	rows, err := db.ContainerPreferences.ListContainerPreferencesWithPresence(ctx)
	if err != nil {
		t.Fatalf("ListContainerPreferencesWithPresence: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// Ordered by name, so a page renders the same list twice running.
	if rows[0].ContainerName != "grafana" || rows[1].ContainerName != "vaultwarden" {
		t.Errorf("rows are not ordered by name: %q, %q", rows[0].ContainerName, rows[1].ContainerName)
	}
	for _, row := range rows {
		if !row.Present {
			t.Errorf("%q is in the inventory but was reported absent", row.ContainerName)
		}
		if row.CurrentContainerID != row.ContainerName+"-id" {
			t.Errorf("%q resolved to id %q", row.ContainerName, row.CurrentContainerID)
		}
	}
}

func TestAPreferenceForARemovedContainerIsKeptAndMarkedAbsent(t *testing.T) {
	db, ctx := preferenceRepo(t)

	commitContainers(t, db, "vaultwarden", "grafana")
	setPreference(t, db, "vaultwarden", domain.BehaviorAutomatic, "vaultwarden-id")
	setPreference(t, db, "grafana", domain.BehaviorReviewFirst, "grafana-id")

	// grafana is removed from the host.
	commitContainers(t, db, "vaultwarden")

	rows, err := db.ContainerPreferences.ListContainerPreferencesWithPresence(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// The row is KEPT. A read must not delete an operator's setting, and a name
	// that comes back should find its choice waiting.
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 -- a read must not delete a preference", len(rows))
	}
	byName := map[string]store.ContainerPreferenceRow{}
	for _, row := range rows {
		byName[row.ContainerName] = row
	}
	if byName["grafana"].Present {
		t.Error("grafana is gone from the host but was reported present")
	}
	if byName["grafana"].CurrentContainerID != "" {
		t.Errorf("an absent container resolved to an id: %q", byName["grafana"].CurrentContainerID)
	}
	if byName["grafana"].Behavior != domain.BehaviorReviewFirst {
		t.Errorf("the stored behaviour was altered: %q", byName["grafana"].Behavior)
	}
	if !byName["vaultwarden"].Present {
		t.Error("vaultwarden is still running and was reported absent")
	}
}

func TestPresenceReportsTheCurrentIdNotTheStoredOne(t *testing.T) {
	db, ctx := preferenceRepo(t)

	// The choice is made while the container has one id...
	commitContainers(t, db, "vaultwarden")
	setPreference(t, db, "vaultwarden", domain.BehaviorAutomatic, "the-old-id")

	rows, err := db.ContainerPreferences.ListContainerPreferencesWithPresence(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	// ...and the id reported is the one the container HAS, re-resolved from the
	// name. A recreation replaces the id, so echoing the stored one would send
	// an operator to a container that no longer exists.
	if rows[0].CurrentContainerID != "vaultwarden-id" {
		t.Errorf("reported id %q; the stored id must not be echoed", rows[0].CurrentContainerID)
	}
	if rows[0].ContainerID != "the-old-id" {
		t.Errorf("the stored evidence was rewritten: %q", rows[0].ContainerID)
	}
}

func TestNoSavedBehavioursIsAnEmptyListNotAnError(t *testing.T) {
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "vaultwarden")

	rows, err := db.ContainerPreferences.ListContainerPreferencesWithPresence(ctx)
	if err != nil {
		t.Fatalf("an estate with no overrides must not be an error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want none", len(rows))
	}
}

func TestResolvingPresenceDoesNotCostAQueryPerPreference(t *testing.T) {
	// The property that keeps the workspace page cheap. A regression to a
	// per-row lookup would still satisfy every assertion above, so the cost is
	// measured the way this package already measures it: seed a lot, and fail
	// if the read stops being flat.
	db, ctx := preferenceRepo(t)

	const preferences = 400
	names := make([]string, 0, preferences)
	for i := 0; i < preferences; i++ {
		names = append(names, fmt.Sprintf("svc-%04d", i))
	}
	commitContainers(t, db, names...)
	for _, name := range names {
		setPreference(t, db, name, domain.BehaviorReviewFirst, name+"-id")
	}

	start := time.Now()
	rows, err := db.ContainerPreferences.ListContainerPreferencesWithPresence(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != preferences {
		t.Fatalf("got %d rows, want %d", len(rows), preferences)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("resolving %d preferences took %s; a per-container query has crept in",
			preferences, elapsed)
	}
	t.Logf("resolved %d preferences in %s", len(rows), elapsed)
}
