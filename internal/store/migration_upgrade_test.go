package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"

	_ "modernc.org/sqlite"
)

// Upgrading an EXISTING database must preserve its data.
//
// Migration 0003 rebuilds `inventory_refreshes` to widen a CHECK constraint,
// which SQLite cannot alter in place. A rebuild is exactly the kind of
// migration that silently loses rows, and the data at risk is an operator's
// refresh history. A fresh-database test would never catch it, because a fresh
// database has nothing to lose.
//
// This applies 0001 and 0002 the way an installed Phase 2 deployment would,
// writes real data, and only then applies 0003.

// applyMigrationsUpTo runs the numbered migrations up to and including `last`,
// recording them in schema_migrations so a later Migrate call skips them.
func applyMigrationsUpTo(t *testing.T, db *sql.DB, last string) {
	t.Helper()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
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
		if name == last {
			return
		}
	}
	t.Fatalf("migration %s not found", last)
}

func TestUpgradeFromPhase2PreservesData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "phase2.db")

	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// A database as Phase 2 left it: 0001 and 0002 only.
	applyMigrationsUpTo(t, raw, "0002_inventory.sql")

	// Populate it the way a running Phase 2 deployment would.
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO hosts (id, name, runtime, api_version, os_type, created_at, updated_at)
		VALUES ('local', 'local', 'docker', '1.51', 'linux',
		        '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed host: %v", err)
	}

	for _, refresh := range []struct {
		generation int
		trigger    string
		checksum   string
	}{
		{1, "startup", "checksum-one"},
		{2, "periodic", "checksum-two"},
		{3, "manual", "checksum-three"},
	} {
		if _, err := raw.ExecContext(ctx, `
			INSERT INTO inventory_refreshes
				(host_id, generation, trigger, state, started_at, finished_at,
				 duration_ms, containers_listed, checksum)
			VALUES ('local', ?, ?, 'succeeded', '2026-08-01T00:00:00Z',
			        '2026-08-01T00:00:02Z', 2000, 7, ?)`,
			refresh.generation, refresh.trigger, refresh.checksum); err != nil {
			t.Fatalf("seed refresh: %v", err)
		}
	}

	if err := raw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Now upgrade, exactly as the application does at startup.
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Every refresh row must have survived the table rebuild.
	var count int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM inventory_refreshes`).Scan(&count); err != nil {
		t.Fatalf("count refreshes: %v", err)
	}
	if count != 3 {
		t.Errorf("refreshes = %d, want 3; the rebuild lost history", count)
	}

	// Including their content, not just their number.
	generation, checksum, err := db.Inventory.CurrentGeneration(ctx)
	if err != nil {
		t.Fatalf("CurrentGeneration: %v", err)
	}
	if generation != 3 {
		t.Errorf("generation = %d, want 3", generation)
	}
	if checksum != "checksum-three" {
		t.Errorf("checksum = %q, want checksum-three", checksum)
	}

	// The rebuilt table must still auto-increment from where it left off, or a
	// new refresh would collide with a historical ID.
	last, err := db.Inventory.LastRefresh(ctx, true)
	if err != nil {
		t.Fatalf("LastRefresh: %v", err)
	}
	if last == nil || last.ID != 3 {
		t.Errorf("last refresh = %+v, want id 3", last)
	}

	// The widened constraint must accept the new trigger.
	if _, err := db.Inventory.CommitRefresh(ctx, store.RefreshCommit{
		Host:   domain.Host{ID: domain.LocalHostID, Name: "local", Runtime: domain.RuntimeDocker},
		Record: domain.RefreshRecord{Trigger: domain.TriggerReconcile},
	}); err != nil {
		t.Fatalf("a reconcile refresh must be storable after the upgrade: %v", err)
	}

	// And the old triggers must still be accepted, or the rebuild narrowed the
	// vocabulary instead of widening it.
	for _, trigger := range []domain.RefreshTrigger{
		domain.TriggerStartup, domain.TriggerPeriodic, domain.TriggerManual,
	} {
		if _, err := db.Inventory.CommitRefresh(ctx, store.RefreshCommit{
			Host:   domain.Host{ID: domain.LocalHostID, Name: "local", Runtime: domain.RuntimeDocker},
			Record: domain.RefreshRecord{Trigger: trigger},
		}); err != nil {
			t.Errorf("trigger %q must still be accepted after the upgrade: %v", trigger, err)
		}
	}

	// The indexes the rebuild dropped must have been recreated, or every
	// refresh query silently becomes a table scan.
	for _, index := range []string{"idx_refreshes_started", "idx_refreshes_succeeded"} {
		var name string
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`,
			index).Scan(&name); err != nil {
			t.Errorf("index %s missing after the rebuild: %v", index, err)
		}
	}

	// The new tables must exist, and the event history must be usable.
	if _, _, err := db.DockerEvents.Append(ctx, []domain.DockerEvent{
		buildEvent("fp-after-upgrade"),
	}); err != nil {
		t.Errorf("event history must work after an upgrade: %v", err)
	}
}

// Re-running migrations must not disturb existing DATA.
//
// store_test.go already covers that a second Migrate skips applied files; this
// covers the consequence that matters after 0003, which rebuilds a table: a
// restart must not replay that rebuild and lose rows.
func TestReopeningPreservesInventoryData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "twice.db")

	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := commitInventory(ctx, first, domain.TriggerStartup); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	generation, _, err := second.Inventory.CurrentGeneration(ctx)
	if err != nil {
		t.Fatalf("CurrentGeneration: %v", err)
	}
	if generation != 1 {
		t.Errorf("generation = %d, want 1; re-running migrations disturbed the data", generation)
	}
}
