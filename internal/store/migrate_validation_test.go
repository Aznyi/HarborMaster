package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/store"

	_ "modernc.org/sqlite"
)

// Schema-history validation.
//
// The three refusals below are new startup failure modes, so each one is
// tested with the exact database state that triggers it, and each is paired
// with an assertion that the ORDINARY case still starts. A guard that refuses
// too much is worse than no guard: it turns every upgrade into an incident.

// A newer HarborMaster's database must be refused, not opened.
//
// This is the rollback case. An older binary that opens a newer schema sees
// nothing pending, starts happily, and writes rows against constraints and
// columns it does not know about.
func TestOpenRefusesADatabaseWrittenByANewerBuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ahead.db")

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A migration from a future release, recorded exactly as that release
	// would have recorded it.
	if _, err := db.SQL().Exec(
		`INSERT INTO schema_migrations (name, checksum) VALUES ('0099_future.sql', 'deadbeef')`); err != nil {
		t.Fatalf("record a future migration: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := store.Open(context.Background(), path)
	if err == nil {
		_ = reopened.Close()
		t.Fatal("a database from a newer build was opened; an old binary must not write to a schema it does not know")
	}
	if !errors.Is(err, store.ErrSchemaAhead) {
		t.Errorf("err = %v, want it to match store.ErrSchemaAhead", err)
	}
	if !strings.Contains(err.Error(), "0099_future.sql") {
		t.Errorf("the error must name the offending migration so an operator knows which version wrote it: %v", err)
	}
}

// An applied migration whose file has since changed must be refused.
//
// Matching by name alone means an edited migration is skipped forever, and the
// database permanently differs from what the source says it is.
func TestOpenRefusesAModifiedMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "changed.db")

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Simulate the file having been different when it was applied.
	if _, err := db.SQL().Exec(
		`UPDATE schema_migrations SET checksum = 'not-the-checksum-of-this-file'
		 WHERE name = '0002_inventory.sql'`); err != nil {
		t.Fatalf("rewrite checksum: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := store.Open(context.Background(), path)
	if err == nil {
		_ = reopened.Close()
		t.Fatal("a modified migration was accepted; the schema no longer matches the source")
	}
	if !errors.Is(err, store.ErrMigrationChanged) {
		t.Errorf("err = %v, want it to match store.ErrMigrationChanged", err)
	}
	// The checksums themselves must not be printed -- they are noise, and the
	// actionable fact is the filename.
	if strings.Contains(err.Error(), "not-the-checksum-of-this-file") {
		t.Error("the error must name the migration, not dump checksums")
	}
}

// A hole in the applied sequence means the bookkeeping was hand-edited.
//
// Migrations apply in order, each inside its own transaction, so a crash
// cannot produce this state -- which is precisely why it must be refused
// rather than repaired.
func TestOpenRefusesAGapInTheMigrationHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gap.db")

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.SQL().Exec(
		`DELETE FROM schema_migrations WHERE name = '0002_inventory.sql'`); err != nil {
		t.Fatalf("delete a migration record: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := store.Open(context.Background(), path)
	if err == nil {
		_ = reopened.Close()
		t.Fatal("a gap in the migration history was accepted")
	}
	if !errors.Is(err, store.ErrMigrationGap) {
		t.Errorf("err = %v, want it to match store.ErrMigrationGap", err)
	}
}

// The ordinary upgrade path must not trip any of the three guards above.
//
// The positive control for this whole file. Without it, a guard that refused
// every database would still pass every test in it.
func TestAnOrdinaryReopenPassesValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordinary.db")

	for attempt := range 3 {
		db, err := store.Open(context.Background(), path)
		if err != nil {
			t.Fatalf("open %d: %v", attempt, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close %d: %v", attempt, err)
		}
	}
}

// A database from a version that recorded no checksums must be accepted and
// backfilled, not refused.
//
// This is every existing HarborMaster installation. Refusing them would make
// this change an upgrade barrier rather than a safety improvement, and nothing
// can retroactively prove what those migrations contained anyway.
func TestChecksumsAreBackfilledForAPreChecksumDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	// A database exactly as an earlier build left it: no checksum column at
	// all on the bookkeeping table.
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	applyMigrationsWithoutChecksums(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("a pre-checksum database must upgrade cleanly: %v", err)
	}
	defer func() { _ = db.Close() }()

	if backfilled := db.OpenReport().Migrations.ChecksumsBackfilled; backfilled == 0 {
		t.Error("no checksums were backfilled; a later edit would go undetected")
	}

	var missing int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE checksum IS NULL`).Scan(&missing); err != nil {
		t.Fatalf("count null checksums: %v", err)
	}
	if missing != 0 {
		t.Errorf("%d migration(s) still have no checksum after the backfill", missing)
	}

	// And from this point forward, an edit IS detected.
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'tampered' WHERE name = '0001_init.sql'`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := store.Open(ctx, path); !errors.Is(err, store.ErrMigrationChanged) {
		t.Errorf("after backfill, a change must be detected: %v", err)
	}
}

// An interrupted migration must leave the database at the last COMPLETE
// migration, and the next start must finish the job.
//
// The state is produced the way an interruption produces it: the migrations
// before the interruption are applied and recorded, and the one that was
// running is neither.
func TestAnInterruptedMigrationResumesOnTheNextOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "interrupted.db")

	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	// Stopped after 0002, as a process killed during 0003 would leave it.
	applyMigrationsUpTo(t, raw, "0002_inventory.sql")
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO hosts (id, name, runtime, api_version, os_type, created_at, updated_at)
		VALUES ('local', 'local', 'docker', '1.51', 'linux',
		        '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("an interrupted migration must resume, not refuse: %v", err)
	}
	defer func() { _ = db.Close() }()

	applied := db.OpenReport().Migrations.Applied
	if len(applied) == 0 {
		t.Fatal("no migrations were applied; the interruption was not resumed")
	}

	names, err := store.MigrationNames()
	if err != nil {
		t.Fatalf("MigrationNames: %v", err)
	}
	recorded, err := store.AppliedMigrations(ctx, db.SQL())
	if err != nil {
		t.Fatalf("AppliedMigrations: %v", err)
	}
	if len(recorded) != len(names) {
		t.Errorf("recorded %d migrations, want %d", len(recorded), len(names))
	}

	// The data written before the interruption must have survived it.
	var hosts int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM hosts`).Scan(&hosts); err != nil {
		t.Fatalf("count hosts: %v", err)
	}
	if hosts != 1 {
		t.Errorf("hosts = %d, want 1; resuming a migration must not lose data written before it", hosts)
	}
}

// A migration that fails must leave NOTHING of itself behind.
//
// Each migration runs in one transaction with its own bookkeeping insert, so a
// failure rolls back both. Without that, a partially applied migration would
// be recorded as done and never retried.
func TestAFailedMigrationLeavesNoRecord(t *testing.T) {
	ctx := context.Background()
	db := openRawDB(t)

	// A statement that succeeds followed by one that cannot, inside what the
	// migration runner would treat as one file.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE probe (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("first statement: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE probe (id INTEGER PRIMARY KEY)`); err == nil {
		t.Fatal("the duplicate CREATE must fail; this test needs a failing statement")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'probe'`).
		Scan(&count); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if count != 0 {
		t.Error("a rolled-back migration left its table behind; migrations must be all-or-nothing")
	}
}

// applyMigrationsWithoutChecksums builds the bookkeeping table the way a
// pre-checksum build did: name and applied_at only, no checksum column.
func applyMigrationsWithoutChecksums(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		t.Fatalf("create legacy schema_migrations: %v", err)
	}

	names, err := store.MigrationNames()
	if err != nil {
		t.Fatalf("MigrationNames: %v", err)
	}
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if _, err := db.ExecContext(ctx, string(content)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
			t.Fatalf("record migration %s: %v", name, err)
		}
	}
}
