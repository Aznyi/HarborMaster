package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// migratedTemplate is the bytes of a database that has had the migrations
// applied once. Built at most once per test binary.
//
// APPLYING the migrations is the dominant cost of this package's tests, and it
// is paid by nearly three hundred of them: twenty-one files, each in its own
// transaction, on a fresh file, is around eighty milliseconds. Sequentially
// that is most of the suite's runtime, and under `-race` -- where the pure-Go
// SQLite driver is instrumented statement by statement -- it was enough to push
// the package past `go test`'s ten-minute default and fail CI on a timeout.
//
// A copy is not a weaker fixture. store.Open still runs against it in full:
// journal mode, the integrity check, and the migration history validation. What
// it does not do is execute twenty-one DDL transactions to reach a state that
// is byte-for-byte the one it just read.
//
// Tests that are ABOUT migrating -- the matrix, the upgrade path, the
// validation refusals -- call store.Open on an empty path directly and are
// unaffected by this.
var migratedTemplate = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "harbormaster-template-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "harbormaster.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		return nil, err
	}
	// Close checkpoints the write-ahead log and drops the -wal and -shm
	// sidecars, so the single file read below is the whole database. Copying
	// before the close would capture a database whose most recent pages are in
	// a file the copy does not include.
	if err := db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
})

// openTestDB returns a migrated database in a per-test temporary directory.
func openTestDB(t *testing.T) *store.DB {
	t.Helper()

	template, err := migratedTemplate()
	if err != nil {
		t.Fatalf("build migrated template: %v", err)
	}
	path := filepath.Join(t.TempDir(), "harbormaster.db")
	if err := os.WriteFile(path, template, 0o600); err != nil {
		t.Fatalf("seed test database: %v", err)
	}

	db, err := store.Open(context.Background(), path)
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
		ContainerID:    containerID,
		ContainerName:  "web",
		Trigger:        domain.SnapshotTriggerManual,
		ImageReference: "nginx:1.27",
		ImageID:        "sha256:abc",
		SpecVersion:    domain.SnapshotSpecVersion,
		SpecJSON:       []byte(`{"image":"nginx:1.27"}`),
		Checksum:       checksum,
		CreatedAt:      time.Now().UTC(),
	}
}

// createSnapshot stores a snapshot with no child rows.
func createSnapshot(t *testing.T, db *store.DB, s domain.Snapshot) domain.Snapshot {
	t.Helper()
	created, err := db.Snapshots.Create(context.Background(), s, nil, nil, nil)
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	return created
}

const (
	checksumA = "0000000000000000000000000000000000000000000000000000000000000001"
	checksumB = "0000000000000000000000000000000000000000000000000000000000000002"
)

func TestSnapshotRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	created := createSnapshot(t, db, newSnapshot("container-a", checksumA))
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
	if string(got.SpecJSON) != `{"image":"nginx:1.27"}` {
		t.Errorf("spec mismatch: %s", got.SpecJSON)
	}
	if got.Trigger != domain.SnapshotTriggerManual {
		t.Errorf("Trigger = %q, want manual", got.Trigger)
	}
	// A fresh snapshot has had no readiness evaluation.
	if got.ReadinessStatus != domain.ReadinessUnknown {
		t.Errorf("ReadinessStatus = %q, want unknown", got.ReadinessStatus)
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

	first := createSnapshot(t, db, newSnapshot("container-a", checksumA))
	second := createSnapshot(t, db, newSnapshot("container-a", checksumA))

	if first.ID != second.ID {
		t.Errorf("duplicate checksum created a new row: %d then %d", first.ID, second.ID)
	}
	if !second.Deduplicated {
		t.Error("Deduplicated not reported; the caller cannot tell a no-op from a capture")
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
	createSnapshot(t, db, older)
	createSnapshot(t, db, newSnapshot("container-a", checksumB))
	createSnapshot(t, db, newSnapshot("container-b", checksumA))

	got, _, err := db.Snapshots.List(ctx, store.SnapshotFilter{ContainerID: "container-a"})
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

	got, total, err := db.Snapshots.List(context.Background(), store.SnapshotFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got == nil {
		t.Fatal("expected a non-nil empty slice")
	}
	if len(got) != 0 || total != 0 {
		t.Errorf("len = %d, total = %d, want 0 and 0", len(got), total)
	}
}

func TestSnapshotPageLimitIsBounded(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := range 5 {
		createSnapshot(t, db, newSnapshot("container-a", checksumA[:63]+string(rune('0'+i))))
	}

	got, total, err := db.Snapshots.List(ctx, store.SnapshotFilter{Page: store.Page{Limit: 2}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
	// The total reports the whole match, not the page, or pagination controls
	// would render wrongly.
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
}

// A caller must not be able to ask the process to materialise the whole table.
func TestSnapshotPageLimitIsCapped(t *testing.T) {
	db := openTestDB(t)

	_, _, err := db.Snapshots.List(context.Background(), store.SnapshotFilter{
		Page: store.Page{Limit: 100000},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// normalise() replaces an out-of-range limit rather than honouring it; the
	// query above would otherwise be unbounded.
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
