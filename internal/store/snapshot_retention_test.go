package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// seedSnapshots creates n snapshots for a container, oldest first.
func seedSnapshots(t *testing.T, db *store.DB, containerID string, n int, base time.Time) []domain.Snapshot {
	t.Helper()

	created := make([]domain.Snapshot, 0, n)
	for i := range n {
		s := fixtureSnapshotRow()
		s.ContainerID = containerID
		// A distinct checksum per snapshot, or the dedup index collapses them.
		s.Checksum = fmt.Sprintf("%064x", i+1)
		s.CreatedAt = base.Add(time.Duration(i) * time.Minute)

		row, err := db.Snapshots.Create(context.Background(), s, nil, nil, nil)
		if err != nil {
			t.Fatalf("seed snapshot %d: %v", i, err)
		}
		created = append(created, row)
	}
	return created
}

func countFor(t *testing.T, db *store.DB, containerID string) int {
	t.Helper()
	var n int
	if err := db.SQL().QueryRow(
		`SELECT COUNT(*) FROM snapshots WHERE container_id = ?`, containerID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func newestIDFor(t *testing.T, db *store.DB, containerID string) int64 {
	t.Helper()
	var id int64
	if err := db.SQL().QueryRow(
		`SELECT id FROM snapshots WHERE container_id = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`, containerID).Scan(&id); err != nil {
		t.Fatalf("newest: %v", err)
	}
	return id
}

func TestRetentionNeverPrunesNewestPerContainer(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	a := seedSnapshots(t, db, "c1", 10, base)
	b := seedSnapshots(t, db, "c2", 10, base)

	newestA := a[len(a)-1].ID
	newestB := b[len(b)-1].ID

	result, err := db.Snapshots.PruneSnapshots(context.Background(), store.RetentionPolicy{
		MaxPerContainer: 1,
		BatchSize:       100,
		Now:             base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if result.Deleted != 18 {
		t.Errorf("deleted = %d, want 18", result.Deleted)
	}

	for _, tc := range []struct {
		container string
		newest    int64
	}{{"c1", newestA}, {"c2", newestB}} {
		if got := countFor(t, db, tc.container); got != 1 {
			t.Errorf("container %s kept %d snapshots, want 1", tc.container, got)
		}
		if got := newestIDFor(t, db, tc.container); got != tc.newest {
			t.Errorf("container %s lost its newest snapshot: kept %d, want %d",
				tc.container, got, tc.newest)
		}
	}
}

// The age policy must not be able to delete a container's only record of how it
// is configured, however old that record is.
func TestRetentionKeepsNewestEvenWhenOlderThanMaxAge(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seedSnapshots(t, db, "c1", 1, base)

	if _, err := db.Snapshots.PruneSnapshots(context.Background(), store.RetentionPolicy{
		MaxAge:    time.Hour,
		BatchSize: 100,
		// A month later: the only snapshot is far beyond MaxAge.
		Now: base.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if got := countFor(t, db, "c1"); got != 1 {
		t.Error("the newest snapshot was pruned by the age policy; it is the restore baseline")
	}
}

func TestRetentionByAgeDeletesOlderSnapshots(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seedSnapshots(t, db, "c1", 10, base)

	// Snapshots run base..base+9m. A cutoff of 5 minutes before base+10m keeps
	// those at or after base+5m, plus the newest regardless.
	if _, err := db.Snapshots.PruneSnapshots(context.Background(), store.RetentionPolicy{
		MaxAge:    5 * time.Minute,
		BatchSize: 100,
		Now:       base.Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	got := countFor(t, db, "c1")
	if got == 10 {
		t.Error("the age policy deleted nothing")
	}
	if got == 0 {
		t.Error("the age policy deleted everything, including the baseline")
	}
}

func TestRetentionDeletesInBoundedBatches(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seedSnapshots(t, db, "c1", 100, base)

	result, err := db.Snapshots.PruneSnapshots(context.Background(), store.RetentionPolicy{
		MaxPerContainer: 1,
		BatchSize:       10,
		Now:             base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if result.MaxBatchSize > 10 {
		t.Errorf("a batch deleted %d rows, want at most 10; SQLite has one writer", result.MaxBatchSize)
	}
	if result.Batches < 9 {
		t.Errorf("batches = %d; 99 deletions at 10 per batch should take at least 9", result.Batches)
	}
	if got := countFor(t, db, "c1"); got != 1 {
		t.Errorf("kept %d snapshots, want 1", got)
	}
}

func TestRetentionCascadesToChildRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := range 3 {
		s := fixtureSnapshotRow()
		s.Checksum = fmt.Sprintf("%064x", i+1)
		s.CreatedAt = base.Add(time.Duration(i) * time.Minute)

		if _, err := db.Snapshots.Create(ctx, s,
			[]domain.SnapshotEnvEntry{{
				Position: 0, Key: "PATH", Classification: domain.SensitivityNormal,
				Present: true, Value: "/usr/bin",
			}},
			[]domain.SnapshotMountRow{{Destination: "/data", Type: domain.MountTypeVolume}},
			[]domain.SnapshotNetworkRow{{NetworkName: "bridge"}},
		); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	if _, err := db.Snapshots.PruneSnapshots(ctx, store.RetentionPolicy{
		MaxPerContainer: 1, BatchSize: 100, Now: base.Add(time.Hour),
	}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	for _, table := range []string{"snapshot_environment", "snapshot_mounts", "snapshot_networks"} {
		var orphans int
		query := `SELECT COUNT(*) FROM ` + table + `
			WHERE snapshot_id NOT IN (SELECT id FROM snapshots)`
		if err := db.SQL().QueryRow(query).Scan(&orphans); err != nil {
			t.Fatalf("count orphans in %s: %v", table, err)
		}
		if orphans != 0 {
			t.Errorf("%s left %d orphaned rows after pruning", table, orphans)
		}
	}
}

// Both dimensions zero is a documented "keep everything".
func TestZeroPolicyDisablesPruning(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seedSnapshots(t, db, "c1", 10, base)

	result, err := db.Snapshots.PruneSnapshots(context.Background(), store.RetentionPolicy{
		BatchSize: 100,
		Now:       base.Add(365 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if result.Deleted != 0 {
		t.Errorf("deleted = %d with no policy configured, want 0", result.Deleted)
	}
	if got := countFor(t, db, "c1"); got != 10 {
		t.Errorf("kept %d snapshots, want all 10", got)
	}
}

func TestPruneIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seedSnapshots(t, db, "c1", 5, base)
	policy := store.RetentionPolicy{MaxPerContainer: 2, BatchSize: 100, Now: base.Add(time.Hour)}

	first, err := db.Snapshots.PruneSnapshots(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	if first.Deleted != 3 {
		t.Errorf("first prune deleted %d, want 3", first.Deleted)
	}

	second, err := db.Snapshots.PruneSnapshots(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	if second.Deleted != 0 {
		t.Errorf("second prune deleted %d, want 0", second.Deleted)
	}
}

// A cancelled context must stop the loop rather than running to completion.
func TestPruneRespectsCancellation(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedSnapshots(t, db, "c1", 50, base)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := db.Snapshots.PruneSnapshots(ctx, store.RetentionPolicy{
		MaxPerContainer: 1, BatchSize: 5, Now: base.Add(time.Hour),
	})
	if err == nil {
		t.Error("expected a cancelled prune to return an error")
	}
	if got := countFor(t, db, "c1"); got != 50 {
		t.Errorf("a cancelled prune deleted %d rows", 50-got)
	}
}

func TestPruneAcrossManyContainersKeepsEachBaseline(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := range 20 {
		seedSnapshots(t, db, fmt.Sprintf("container-%02d", i), 5, base)
	}

	if _, err := db.Snapshots.PruneSnapshots(context.Background(), store.RetentionPolicy{
		MaxPerContainer: 1, BatchSize: 7, Now: base.Add(time.Hour),
	}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	for i := range 20 {
		container := fmt.Sprintf("container-%02d", i)
		if got := countFor(t, db, container); got != 1 {
			t.Errorf("container %s kept %d snapshots, want exactly 1", container, got)
		}
	}
}
