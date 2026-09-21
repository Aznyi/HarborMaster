package store_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/store"
)

// A migration that rebuilds a table must not lose the rows of the tables that
// reference it.
//
// # The defect
//
// With foreign keys on, SQLite's DROP TABLE performs an implicit DELETE FROM
// before removing the table, and ON DELETE CASCADE actions fire on it. A
// rebuild that copies a parent table, drops the original, and renames the copy
// therefore deletes every child row -- silently, inside a migration that
// otherwise succeeds. The rebuild of `executions` in 0028 did exactly that to
// `execution_events`. Every rebuild from now on runs with foreign keys off on
// its own connection, and is checked for orphans before it commits.
func TestARebuildMigrationPreservesTheRowsThatReferenceTheTable(t *testing.T) {
	names, err := store.MigrationNames()
	if err != nil {
		t.Fatalf("MigrationNames: %v", err)
	}
	// Stop just before the first rebuild this test is about.
	stop := -1
	for i, name := range names {
		if strings.HasPrefix(name, "0036_") {
			stop = i
		}
	}
	if stop < 0 {
		t.Fatal("no 0036 migration; this test is looking at the wrong sequence")
	}

	path := filepath.Join(t.TempDir(), "harbormaster.db")
	raw := openUnmigrated(t, path)
	applyThrough(t, raw, names[:stop])

	ctx := context.Background()
	for _, statement := range []string{
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
	closeRawDB(t, raw)

	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer func() { _ = db.Close() }()

	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM execution_events WHERE execution_id = 'exec_00112233445566778899'`: 2,
		`SELECT COUNT(*) FROM rollback_events WHERE rollback_id = 'rbk_0123456789abcdef0123'`:    1,
		`SELECT COUNT(*) FROM executions WHERE execution_id = 'exec_00112233445566778899'`:       1,
		`SELECT COUNT(*) FROM rollbacks WHERE rollback_id = 'rbk_0123456789abcdef0123'`:          1,
	} {
		var count int
		if err := db.SQL().QueryRowContext(ctx, query).Scan(&count); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if count != want {
			t.Errorf("%s = %d, want %d\n\nthe rebuild cascaded into the events table", query, count, want)
		}
	}
	var enabled int
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("foreign keys are %d after the upgrade (err %v); the rebuild left them off", enabled, err)
	}
}
