package store_test

// Catalog set-replacement semantics for the targeted network and volume
// writes.
//
// These cover the case that had no unit test and consequently shipped broken:
// a successful read that returns NOTHING. On a host whose only volume is then
// removed, the correct catalog is empty, and "empty" must mean "delete the rest"
// rather than "do nothing" -- otherwise a removed volume is retained until the
// next full reconciliation, which is fifteen minutes away by default.
//
// Networks hid the bug in practice because bridge, host, and none always exist,
// so a network read is never empty on a real daemon. Volumes have no such floor.

import (
	"context"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// seedGeneration commits one refresh so the targeted writes have a generation
// to join. Without it they fail with ErrNoInventory.
func seedGeneration(t *testing.T, db *store.DB) {
	t.Helper()
	commitOf(t, db, records(buildContainer("id-alpha", "alpha")))
}

func volumeNames(t *testing.T, db *store.DB) []string {
	t.Helper()

	volumes, _, err := db.Volumes.List(context.Background(), store.Page{Limit: 200})
	if err != nil {
		t.Fatalf("list volumes: %v", err)
	}
	names := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		names = append(names, volume.Name)
	}
	return names
}

// The regression: removing the last volume on the host must empty the catalog.
func TestReplaceVolumesWithAnEmptyCatalogRemovesEveryVolume(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedGeneration(t, db)

	if err := db.Inventory.ReplaceVolumes(ctx,
		[]domain.Volume{{Name: "only-volume", Driver: "local"}}, now); err != nil {
		t.Fatalf("seed the catalog: %v", err)
	}
	if got := volumeNames(t, db); len(got) != 1 || got[0] != "only-volume" {
		t.Fatalf("after seeding, volumes = %v, want [only-volume]", got)
	}

	// The daemon now reports no volumes at all, which is what a successful
	// list returns once the only volume is removed.
	if err := db.Inventory.ReplaceVolumes(ctx, []domain.Volume{}, now); err != nil {
		t.Fatalf("replace with an empty catalog: %v", err)
	}

	if got := volumeNames(t, db); len(got) != 0 {
		t.Errorf("volumes = %v, want none; a removed volume must not survive a "+
			"successful read that reported zero volumes", got)
	}
}

// Networks take the OPPOSITE rule, and deliberately so.
//
// A daemon always has bridge, host, and none, so a read of zero networks is
// reporting something that cannot be true and is far more likely to be a
// degraded response. Blanking the catalog on that would be worse than keeping a
// stale row until the next reconciliation. This is the case the volume rule
// must not be generalised to.
func TestReplaceNetworksIgnoresAnEmptyCatalog(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedGeneration(t, db)

	if err := db.Inventory.ReplaceNetworks(ctx,
		[]domain.Network{{ID: "net-only", Name: "only-network", Driver: "bridge"}}, now); err != nil {
		t.Fatalf("seed the catalog: %v", err)
	}

	if err := db.Inventory.ReplaceNetworks(ctx, []domain.Network{}, now); err != nil {
		t.Fatalf("replace with an empty catalog: %v", err)
	}

	networks, _, err := db.Networks.List(ctx, store.Page{Limit: 200})
	if err != nil {
		t.Fatalf("list networks: %v", err)
	}
	if len(networks) != 1 {
		t.Errorf("networks = %d, want the catalog left alone by an impossible empty read", len(networks))
	}
}

// Networks still prune normally when the read is non-empty.
func TestReplaceNetworksRemovesOnlyAbsentNetworks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedGeneration(t, db)

	if err := db.Inventory.ReplaceNetworks(ctx, []domain.Network{
		{ID: "net-keep", Name: "keep-me", Driver: "bridge"},
		{ID: "net-drop", Name: "remove-me", Driver: "bridge"},
	}, now); err != nil {
		t.Fatalf("seed the catalog: %v", err)
	}

	if err := db.Inventory.ReplaceNetworks(ctx,
		[]domain.Network{{ID: "net-keep", Name: "keep-me", Driver: "bridge"}}, now); err != nil {
		t.Fatalf("replace: %v", err)
	}

	networks, _, err := db.Networks.List(ctx, store.Page{Limit: 200})
	if err != nil {
		t.Fatalf("list networks: %v", err)
	}
	if len(networks) != 1 || networks[0].Name != "keep-me" {
		t.Errorf("networks = %+v, want only keep-me", networks)
	}
}

// The ordinary case must keep working: a non-empty catalog removes only what is
// absent from it, and leaves the rest.
func TestReplaceVolumesRemovesOnlyAbsentVolumes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedGeneration(t, db)

	if err := db.Inventory.ReplaceVolumes(ctx, []domain.Volume{
		{Name: "keep-me", Driver: "local"},
		{Name: "remove-me", Driver: "local"},
	}, now); err != nil {
		t.Fatalf("seed the catalog: %v", err)
	}

	if err := db.Inventory.ReplaceVolumes(ctx,
		[]domain.Volume{{Name: "keep-me", Driver: "local"}}, now); err != nil {
		t.Fatalf("replace: %v", err)
	}

	got := volumeNames(t, db)
	if len(got) != 1 || got[0] != "keep-me" {
		t.Errorf("volumes = %v, want [keep-me]", got)
	}
}
