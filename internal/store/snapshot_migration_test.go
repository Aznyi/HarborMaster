package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/store"

	_ "modernc.org/sqlite"
)

// openRawDB opens a database with no migrations applied.
func openRawDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// openMigratedDB opens a fully migrated database.
func openMigratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openRawDB(t)
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var name string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
	switch {
	case err == sql.ErrNoRows:
		return false
	case err != nil:
		t.Fatalf("query sqlite_master: %v", err)
	}
	return true
}

// A row in the Phase 1 table must survive the upgrade.
//
// No shipped code path wrote to that table, but that is a statement about
// HarborMaster's releases rather than about any given operator's database. A
// DROP would destroy a hand-written row, an experimental branch's data, or a
// restored backup, and unexpected data is worth more than a tidy schema.
func TestMigration0004PreservesLegacySnapshotRows(t *testing.T) {
	db := openRawDB(t)
	applyMigrationsUpTo(t, db, "0003_events.sql")

	checksum := strings.Repeat("a", 64)
	if _, err := db.Exec(`
		INSERT INTO snapshots
			(container_id, container_name, source, image, image_id, spec, checksum, note, created_at)
		VALUES ('c1', 'legacy-name', 'manual', 'img:1', 'sha256:x', x'7b7d', ?, 'operator note', '2026-01-01T00:00:00Z')`,
		checksum); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if !tableExists(t, db, "snapshots_legacy_phase1") {
		t.Fatal("legacy table was not preserved")
	}

	var (
		count int
		name  string
		note  string
	)
	if err := db.QueryRow(`SELECT COUNT(*) FROM snapshots_legacy_phase1`).Scan(&count); err != nil {
		t.Fatalf("count legacy rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("legacy row count = %d, want 1; operator data must not be destroyed", count)
	}

	if err := db.QueryRow(
		`SELECT container_name, note FROM snapshots_legacy_phase1`).Scan(&name, &note); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if name != "legacy-name" || note != "operator note" {
		t.Errorf("legacy row altered: name=%q note=%q", name, note)
	}
}

// The new table must be empty and separate from the legacy one.
func TestMigration0004CreatesAnEmptySnapshotsTable(t *testing.T) {
	db := openRawDB(t)
	applyMigrationsUpTo(t, db, "0003_events.sql")

	if _, err := db.Exec(`
		INSERT INTO snapshots
			(container_id, container_name, source, image, image_id, spec, checksum, note, created_at)
		VALUES ('c1', 'legacy', 'manual', '', '', x'7b7d', ?, '', '2026-01-01T00:00:00Z')`,
		strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}

	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM snapshots`).Scan(&count); err != nil {
		t.Fatalf("the new snapshots table is missing: %v", err)
	}
	if count != 0 {
		t.Errorf("new snapshots table has %d rows, want 0; legacy rows must not be migrated in", count)
	}
}

func TestMigration0004CreatesSnapshotSchema(t *testing.T) {
	db := openMigratedDB(t)

	for _, table := range []string{
		"snapshots",
		"snapshot_environment",
		"snapshot_mounts",
		"snapshot_networks",
		"snapshot_restore_checks",
	} {
		if !tableExists(t, db, table) {
			t.Errorf("table %s was not created", table)
		}
	}
}

func TestMigration0004CreatesIndexes(t *testing.T) {
	db := openMigratedDB(t)

	for _, index := range []string{
		"idx_snapshots_container_created",
		"idx_snapshots_created",
		"idx_snapshots_readiness",
		"idx_snapshots_trigger",
		"idx_snapshots_container_checksum",
		"idx_snapshot_environment_key",
		"idx_snapshot_mounts_volume",
		"idx_snapshot_networks_name",
		"idx_snapshot_restore_checks_snapshot",
	} {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&name)
		if err != nil {
			t.Errorf("index %s was not created: %v", index, err)
		}
	}
}

// The CHECK constraints are the schema-level guard behind the application's
// own validation. They must reject what the application would never write.
func TestSnapshotConstraintsRejectInvalidRows(t *testing.T) {
	db := openMigratedDB(t)

	insert := func(t *testing.T, checksum, trigger, readiness string, specVersion int) error {
		t.Helper()
		_, err := db.Exec(`
			INSERT INTO snapshots
				(container_id, container_name, spec_version, spec_json, checksum,
				 trigger, readiness_status, created_at)
			VALUES ('c1', 'web', ?, x'7b7d', ?, ?, ?, '2026-01-01T00:00:00Z')`,
			specVersion, checksum, trigger, readiness)
		return err
	}

	valid := strings.Repeat("a", 64)

	t.Run("short checksum", func(t *testing.T) {
		if err := insert(t, "abc", "manual", "unknown", 1); err == nil {
			t.Error("a malformed checksum was accepted")
		}
	})
	t.Run("unknown trigger", func(t *testing.T) {
		if err := insert(t, valid, "restore", "unknown", 1); err == nil {
			t.Error("an unknown trigger was accepted; the vocabulary must stay closed")
		}
	})
	t.Run("unknown readiness", func(t *testing.T) {
		if err := insert(t, valid, "manual", "definitely", 1); err == nil {
			t.Error("an unknown readiness status was accepted")
		}
	})
	t.Run("zero spec version", func(t *testing.T) {
		if err := insert(t, valid, "manual", "unknown", 0); err == nil {
			t.Error("a zero spec version was accepted")
		}
	})
	t.Run("valid row", func(t *testing.T) {
		if err := insert(t, strings.Repeat("c", 64), "manual", "unknown", 1); err != nil {
			t.Errorf("a valid row was rejected: %v", err)
		}
	})
}

// A sensitive environment row may never carry a value. The application already
// enforces this; the constraint is the layer that survives a bug in it.
func TestSensitiveEnvironmentRowCannotCarryAValue(t *testing.T) {
	db := openMigratedDB(t)

	var snapshotID int64
	if err := db.QueryRow(`
		INSERT INTO snapshots
			(container_id, container_name, spec_version, spec_json, checksum, trigger, created_at)
		VALUES ('c1', 'web', 1, x'7b7d', ?, 'manual', '2026-01-01T00:00:00Z')
		RETURNING id`, strings.Repeat("a", 64)).Scan(&snapshotID); err != nil {
		t.Fatal(err)
	}

	_, err := db.Exec(`
		INSERT INTO snapshot_environment
			(snapshot_id, position, key, classification, value)
		VALUES (?, 0, 'DB_PASSWORD', 'sensitive', 'hunter2')`, snapshotID)
	if err == nil {
		t.Fatal("a sensitive environment row was allowed to carry a plaintext value")
	}
}

// Deleting a snapshot must not leave orphaned child rows behind. Retention
// depends on this.
func TestSnapshotChildRowsCascadeOnDelete(t *testing.T) {
	db := openMigratedDB(t)

	var snapshotID int64
	if err := db.QueryRow(`
		INSERT INTO snapshots
			(container_id, container_name, spec_version, spec_json, checksum, trigger, created_at)
		VALUES ('c1', 'web', 1, x'7b7d', ?, 'manual', '2026-01-01T00:00:00Z')
		RETURNING id`, strings.Repeat("a", 64)).Scan(&snapshotID); err != nil {
		t.Fatal(err)
	}

	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO snapshot_environment (snapshot_id, position, key) VALUES (?, 0, 'PATH')`, []any{snapshotID}},
		{`INSERT INTO snapshot_mounts (snapshot_id, destination) VALUES (?, '/data')`, []any{snapshotID}},
		{`INSERT INTO snapshot_networks (snapshot_id, network_name) VALUES (?, 'bridge')`, []any{snapshotID}},
		{`INSERT INTO snapshot_restore_checks (snapshot_id, evaluated_at, check_id, status)
		  VALUES (?, '2026-01-01T00:00:00Z', 'image_available', 'ready')`, []any{snapshotID}},
	} {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed child row: %v", err)
		}
	}

	if _, err := db.Exec(`DELETE FROM snapshots WHERE id = ?`, snapshotID); err != nil {
		t.Fatalf("delete snapshot: %v", err)
	}

	for _, table := range []string{
		"snapshot_environment", "snapshot_mounts", "snapshot_networks", "snapshot_restore_checks",
	} {
		// The table name is a test-local constant from the loop above, never
		// caller input; snapshotID travels as a bound parameter.
		var count int
		row := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE snapshot_id = ?`, snapshotID)
		if err := row.Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s left %d orphaned rows after the snapshot was deleted", table, count)
		}
	}
}

// The dedup index is what bounds database growth and what makes Phase 3
// configuration history rather than time-series evidence.
func TestDuplicateChecksumForSameContainerIsRejected(t *testing.T) {
	db := openMigratedDB(t)
	checksum := strings.Repeat("a", 64)

	insert := func(containerID string) error {
		_, err := db.Exec(`
			INSERT INTO snapshots
				(container_id, container_name, spec_version, spec_json, checksum, trigger, created_at)
			VALUES (?, 'web', 1, x'7b7d', ?, 'manual', '2026-01-01T00:00:00Z')`,
			containerID, checksum)
		return err
	}

	if err := insert("c1"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insert("c1"); err == nil {
		t.Error("a duplicate (container_id, checksum) was accepted")
	}
	// The same configuration on a DIFFERENT container is a distinct snapshot.
	if err := insert("c2"); err != nil {
		t.Errorf("the same checksum on another container was rejected: %v", err)
	}
}

func TestMigrationNamesIncludes0004(t *testing.T) {
	names, err := store.MigrationNames()
	if err != nil {
		t.Fatalf("MigrationNames: %v", err)
	}
	for _, name := range names {
		if name == "0004_snapshots.sql" {
			return
		}
	}
	t.Errorf("0004_snapshots.sql missing from %v", names)
}
