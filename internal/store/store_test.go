package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// openTestDB returns a migrated database in a per-test temporary directory.
func openTestDB(t *testing.T) *store.DB {
	t.Helper()

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "harbormaster.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return db
}

func TestOpenCreatesMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "harbormaster.db")

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store in a missing directory: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbormaster.db")

	first, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	// Re-opening replays Migrate; already-applied files must be skipped.
	second, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer func() { _ = second.Close() }()

	names, err := store.MigrationNames()
	if err != nil {
		t.Fatalf("migration names: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("expected at least one embedded migration")
	}

	var applied int
	if err := second.SQL().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if applied != len(names) {
		t.Errorf("applied migrations = %d, want %d", applied, len(names))
	}
}

func newSnapshot(containerID, checksum string) domain.Snapshot {
	return domain.Snapshot{
		ContainerID:   containerID,
		ContainerName: "web",
		Source:        domain.SnapshotSourceManual,
		Image:         "nginx:1.27",
		ImageID:       "sha256:abc",
		Spec:          []byte(`{"image":"nginx:1.27"}`),
		Checksum:      checksum,
		CreatedAt:     time.Now().UTC(),
	}
}

const (
	checksumA = "0000000000000000000000000000000000000000000000000000000000000001"
	checksumB = "0000000000000000000000000000000000000000000000000000000000000002"
)

func TestSnapshotRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	created, err := db.Snapshots.Create(ctx, newSnapshot("container-a", checksumA))
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("expected an assigned ID")
	}

	got, err := db.Snapshots.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if got.ContainerID != "container-a" || got.Checksum != checksumA {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if string(got.Spec) != `{"image":"nginx:1.27"}` {
		t.Errorf("spec mismatch: %s", got.Spec)
	}
	// Timestamps must survive the text encoding used by SQLite.
	if got.CreatedAt.IsZero() || got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt = %v, want a non-zero UTC time", got.CreatedAt)
	}
}

// Re-capturing an unchanged configuration must not add history noise.
func TestSnapshotCreateIsIdempotentPerChecksum(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first, err := db.Snapshots.Create(ctx, newSnapshot("container-a", checksumA))
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := db.Snapshots.Create(ctx, newSnapshot("container-a", checksumA))
	if err != nil {
		t.Fatalf("second create: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("duplicate checksum created a new row: %d then %d", first.ID, second.ID)
	}
	count, err := db.Snapshots.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("snapshot count = %d, want 1", count)
	}
}

func TestSnapshotListByContainerIsNewestFirst(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	older := newSnapshot("container-a", checksumA)
	older.CreatedAt = time.Now().UTC().Add(-time.Hour)
	newer := newSnapshot("container-a", checksumB)

	if _, err := db.Snapshots.Create(ctx, older); err != nil {
		t.Fatalf("create older: %v", err)
	}
	if _, err := db.Snapshots.Create(ctx, newer); err != nil {
		t.Fatalf("create newer: %v", err)
	}
	if _, err := db.Snapshots.Create(ctx, newSnapshot("container-b", checksumA)); err != nil {
		t.Fatalf("create other container: %v", err)
	}

	got, err := db.Snapshots.ListByContainer(ctx, "container-a", store.Page{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (other containers must be excluded)", len(got))
	}
	if !got[0].CreatedAt.After(got[1].CreatedAt) {
		t.Errorf("results are not newest first: %v then %v", got[0].CreatedAt, got[1].CreatedAt)
	}
}

func TestSnapshotGetMissingReturnsErrNotFound(t *testing.T) {
	db := openTestDB(t)

	_, err := db.Snapshots.Get(context.Background(), 404)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want store.ErrNotFound", err)
	}
}

// An empty result must serialise as [], not null.
func TestSnapshotListReturnsEmptySliceNotNil(t *testing.T) {
	db := openTestDB(t)

	got, err := db.Snapshots.List(context.Background(), store.Page{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got == nil {
		t.Fatal("expected a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestSnapshotPageLimitIsBounded(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := range 5 {
		s := newSnapshot("container-a", checksumA[:63]+string(rune('0'+i)))
		if _, err := db.Snapshots.Create(ctx, s); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	got, err := db.Snapshots.List(ctx, store.Page{Limit: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

func TestEventAppendAndList(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	appended, err := db.Events.Append(ctx, domain.Event{
		Type:     domain.EventDockerConnected,
		Severity: domain.SeverityInfo,
		Message:  "docker engine reachable",
	})
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	if appended.ID == 0 {
		t.Error("expected an assigned ID")
	}
	if appended.OccurredAt.IsZero() {
		t.Error("expected OccurredAt to be defaulted")
	}

	got, err := db.Events.List(ctx, store.Page{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Type != domain.EventDockerConnected {
		t.Errorf("type = %q", got[0].Type)
	}
}

func TestEventSeverityDefaultsToInfo(t *testing.T) {
	db := openTestDB(t)

	got, err := db.Events.Append(context.Background(), domain.Event{
		Type:    domain.EventServerStarted,
		Message: "started",
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if got.Severity != domain.SeverityInfo {
		t.Errorf("severity = %q, want %q", got.Severity, domain.SeverityInfo)
	}
}

// The CHECK constraint is the last line of defence against a typo'd severity
// reaching the audit log.
func TestEventRejectsUnknownSeverity(t *testing.T) {
	db := openTestDB(t)

	_, err := db.Events.Append(context.Background(), domain.Event{
		Type:     domain.EventServerStarted,
		Severity: domain.EventSeverity("catastrophic"),
		Message:  "started",
	})
	if err == nil {
		t.Fatal("expected the severity CHECK constraint to reject the insert")
	}
}
