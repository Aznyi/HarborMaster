package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/store"
)

// Release qualification of the migration runner.
//
// Three things a release has to be able to say about its schema: a fresh
// database reaches the current version with every constraint intact; a
// database from the last shipped build reaches it without losing a row it was
// supposed to keep; and a rebuild that fails leaves the database exactly as it
// found it.

// fkViolations runs the check the constraints would have made.
func fkViolations(t *testing.T, db *sql.DB) int {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_check rows: %v", err)
	}
	return n
}

func countRows(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestAFreshDatabaseReachesTheCurrentSchemaWithEveryConstraintIntact(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "harbormaster.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	names, err := store.MigrationNames()
	if err != nil {
		t.Fatalf("MigrationNames: %v", err)
	}
	applied, err := store.AppliedMigrations(ctx, db.SQL())
	if err != nil {
		t.Fatalf("AppliedMigrations: %v", err)
	}
	if len(applied) != len(names) {
		t.Fatalf("%d migrations applied of %d embedded", len(applied), len(names))
	}
	if got := fkViolations(t, db.SQL()); got != 0 {
		t.Errorf("foreign_key_check reports %d violations on a fresh database", got)
	}
	var integrity string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Errorf("integrity_check = %q, %v", integrity, err)
	}
	if got := countRows(t, db.SQL(),
		`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE '%_rebuilt'`); got != 0 {
		t.Errorf("%d half-finished rebuild tables are left in a fresh database", got)
	}
	var enabled int
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
		t.Errorf("foreign keys are %d after a fresh migration (err %v)", enabled, err)
	}
}

// seedShippedRecords writes the rows a shipped installation would hold in
// every table a later migration rebuilds, and in every table that references
// one of them with ON DELETE CASCADE.
func seedShippedRecords(t *testing.T, raw *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO change_plans (plan_id, container_id, input_digest, generated_at)
		 VALUES ('plan_0011223344556677889', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd', '2026-01-01T00:00:00Z')`,
		`INSERT INTO acquisitions (acquisition_id, plan_id, container_id, target_registry, target_repository,
		    target_digest, state, requested_at, expires_at, created_at, updated_at)
		 VALUES ('acq_00112233445566778899', 'plan_0011223344556677889',
		    'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
		    'docker.io', 'library/nginx', 'sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
		    'succeeded', '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO acquisition_events (acquisition_id, state, detail, at)
		 VALUES ('acq_00112233445566778899', 'queued', 'requested', '2026-01-01T00:00:00Z')`,
		`INSERT INTO acquisition_events (acquisition_id, state, detail, at)
		 VALUES ('acq_00112233445566778899', 'succeeded', 'pulled', '2026-01-01T00:01:00Z')`,
		`INSERT INTO executions (execution_id, acquisition_id, plan_id, container_id, container_name,
		    target_registry, target_repository, target_digest, state, requested_at, expires_at,
		    created_at, updated_at)
		 VALUES ('exec_00112233445566778899', 'acq_00112233445566778899', 'plan_0011223344556677889',
		    'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'web',
		    'docker.io', 'library/nginx', 'sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
		    'failed', '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO execution_events (execution_id, state, detail, at)
		 VALUES ('exec_00112233445566778899', 'queued', 'requested', '2026-01-01T00:00:00Z')`,
		`INSERT INTO execution_events (execution_id, state, detail, at)
		 VALUES ('exec_00112233445566778899', 'failed', 'failed', '2026-01-01T00:01:00Z')`,
		`INSERT INTO rollbacks (rollback_id, execution_id, container_name, original_id, parked_name,
		    replacement_id, state, requested_at, expires_at, created_at, updated_at)
		 VALUES ('rbk_0123456789abcdef0123', 'exec_00112233445566778899', 'web',
		    'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'web.hm-old-exec_00112233445566778899',
		    'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc', 'succeeded',
		    '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO rollback_events (rollback_id, state, detail, at)
		 VALUES ('rbk_0123456789abcdef0123', 'queued', 'requested', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed: %v\n%s", err, statement)
		}
	}
}

// The rows an upgrade must carry across every rebuild between the shipped
// builds and now.
var shippedRowCounts = map[string]int{
	`SELECT COUNT(*) FROM change_plans WHERE plan_id = 'plan_0011223344556677889'`:              1,
	`SELECT COUNT(*) FROM acquisitions WHERE acquisition_id = 'acq_00112233445566778899'`:       1,
	`SELECT COUNT(*) FROM acquisition_events WHERE acquisition_id = 'acq_00112233445566778899'`: 2,
	`SELECT COUNT(*) FROM executions WHERE execution_id = 'exec_00112233445566778899'`:          1,
	`SELECT COUNT(*) FROM execution_events WHERE execution_id = 'exec_00112233445566778899'`:    2,
	`SELECT COUNT(*) FROM rollbacks WHERE rollback_id = 'rbk_0123456789abcdef0123'`:             1,
	`SELECT COUNT(*) FROM rollback_events WHERE rollback_id = 'rbk_0123456789abcdef0123'`:       1,
}

// TestAnUpgradeFromTheLastShippedBuildKeepsEveryRow: the shipped tags are
// v0.9.0-beta.1 at migration 0023 and v0.9.0-beta.2 at 0031. An installation
// on either reaches the current schema through the executions rebuilds in
// 0028, 0030, 0031 and 0036, the acquisitions rebuild in 0029, the change_plans
// rebuild in 0027 and the rollbacks rebuild in 0037. Every child row must
// survive every one of them.
func TestAnUpgradeFromTheLastShippedBuildKeepsEveryRow(t *testing.T) {
	names, err := store.MigrationNames()
	if err != nil {
		t.Fatalf("MigrationNames: %v", err)
	}
	for _, from := range []string{"0024_", "0032_"} {
		t.Run("from before "+from, func(t *testing.T) {
			stop := -1
			for i, name := range names {
				if strings.HasPrefix(name, from) {
					stop = i
				}
			}
			if stop < 0 {
				t.Fatalf("no %s migration", from)
			}
			path := filepath.Join(t.TempDir(), "harbormaster.db")
			raw := openUnmigrated(t, path)
			applyThrough(t, raw, names[:stop])
			seedShippedRecords(t, raw)
			closeRawDB(t, raw)

			ctx := context.Background()
			db, err := store.Open(ctx, path)
			if err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			defer func() { _ = db.Close() }()

			for query, want := range shippedRowCounts {
				if got := countRows(t, db.SQL(), query); got != want {
					t.Errorf("%s = %d, want %d\n\nthe upgrade lost rows", query, got, want)
				}
			}
			if got := fkViolations(t, db.SQL()); got != 0 {
				t.Errorf("foreign_key_check reports %d violations after the upgrade", got)
			}
			applied, err := store.AppliedMigrations(ctx, db.SQL())
			if err != nil || len(applied) != len(names) {
				t.Errorf("%d migrations applied of %d (err %v)", len(applied), len(names), err)
			}
		})
	}
}

// TestAFailedRebuildLeavesTheDatabaseAsItFoundIt: an index that already
// exists under the name 0036 creates makes the rebuild fail after the table
// has been copied, dropped and renamed inside the transaction. Nothing of that
// may survive: the rows, the constraints and the migration history must read
// exactly as they did before the attempt, and the next attempt must succeed.
func TestAFailedRebuildLeavesTheDatabaseAsItFoundIt(t *testing.T) {
	names, err := store.MigrationNames()
	if err != nil {
		t.Fatalf("MigrationNames: %v", err)
	}
	stop := -1
	for i, name := range names {
		if strings.HasPrefix(name, "0036_") {
			stop = i
		}
	}
	if stop < 0 {
		t.Fatal("no 0036 migration")
	}
	path := filepath.Join(t.TempDir(), "harbormaster.db")
	raw := openUnmigrated(t, path)
	applyThrough(t, raw, names[:stop])
	seedShippedRecords(t, raw)
	ctx := context.Background()
	// The collision: 0036 ends with CREATE INDEX idx_execution_restore_pending.
	if _, err := raw.ExecContext(ctx,
		`CREATE INDEX idx_execution_restore_pending ON execution_events (state)`); err != nil {
		t.Fatalf("plant the colliding index: %v", err)
	}
	closeRawDB(t, raw)

	if db, err := store.Open(ctx, path); err == nil {
		_ = db.Close()
		t.Fatal("the upgrade succeeded although 0036 cannot create its index")
	}

	raw = openUnmigrated(t, path)
	for query, want := range shippedRowCounts {
		if got := countRows(t, raw, query); got != want {
			t.Errorf("after the failed rebuild %s = %d, want %d", query, got, want)
		}
	}
	if got := countRows(t, raw, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'executions_rebuilt'`); got != 0 {
		t.Error("the failed rebuild left executions_rebuilt behind")
	}
	if got := countRows(t, raw, `SELECT COUNT(*) FROM schema_migrations WHERE name LIKE '0036_%'`); got != 0 {
		t.Error("the failed rebuild was recorded as applied")
	}
	if got := countRows(t, raw, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'executions'`); got != 1 {
		t.Fatalf("executions table count = %d after the failed rebuild", got)
	}
	if got := fkViolations(t, raw); got != 0 {
		t.Errorf("foreign_key_check reports %d violations after the failed rebuild", got)
	}
	if _, err := raw.ExecContext(ctx, `DROP INDEX idx_execution_restore_pending`); err != nil {
		t.Fatalf("clear the collision: %v", err)
	}
	closeRawDB(t, raw)

	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("the upgrade after clearing the collision: %v", err)
	}
	defer func() { _ = db.Close() }()
	for query, want := range shippedRowCounts {
		if got := countRows(t, db.SQL(), query); got != want {
			t.Errorf("after the retried upgrade %s = %d, want %d", query, got, want)
		}
	}
}
