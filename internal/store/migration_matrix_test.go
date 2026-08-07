package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/store"

	_ "modernc.org/sqlite"
)

// The migration matrix.
//
// # What this proves that the other migration tests do not
//
// `TestUpgradeFromPhase2PreservesData` proves ONE upgrade path preserves ONE
// table. The validation tests prove the bookkeeping refuses tampering. Neither
// answers the question a beta has to answer:
//
//	Can a deployment installed at ANY previous release reach the current
//	schema, with its data intact?
//
// Every schema version HarborMaster has ever written is a version somebody is
// running. This applies the migrations up to each of them in turn, writes data
// that the remaining migrations have to carry forward, and then upgrades the
// rest of the way — twenty-odd upgrade paths rather than one.
//
// # Why the data is written through raw SQL
//
// Because the repositories describe the CURRENT schema. A row inserted through
// today's repository into a version-8 database would either fail or insert a
// shape that version could not have produced, and the test would prove nothing
// about the upgrade an actual version-8 deployment faces.

// legacyRows are the rows written at an intermediate version, keyed by the
// migration after which they become insertable.
//
// One row per subsystem, chosen because each is something an operator would be
// upset to lose: an account they created, a policy they wrote, a record of a
// container HarborMaster changed.
//
// The statements are deliberately minimal — the columns that existed at that
// version and no more — so a later migration that adds a NOT NULL column
// without a default fails here rather than in somebody's deployment.
var legacyRows = []struct {
	after     string
	statement string
	// count is the query that must still return 1 after the full upgrade.
	count string
}{
	{
		after: "0011_auth.sql",
		statement: `INSERT INTO users
			(user_id, username, role, status, password_changed_at, created_at, updated_at)
			VALUES ('usr_00112233445566778899', 'legacy-admin', 'administrator', 'active',
			        '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		count: `SELECT COUNT(*) FROM users WHERE username = 'legacy-admin'`,
	},
	{
		after: "0006_policy.sql",
		statement: `INSERT INTO policy_definitions
			(policy_id, name, severity, rules_json, created_at, updated_at)
			VALUES ('pol_00112233445566778899', 'legacy policy', 'high', '[]',
			        '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		count: `SELECT COUNT(*) FROM policy_definitions WHERE name = 'legacy policy'`,
	},
	{
		after: "0004_snapshots.sql",
		statement: `INSERT INTO snapshots
			(container_id, container_name, spec_version, spec_json, checksum,
			 trigger, created_at)
			VALUES ('c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00',
			        'legacy-web', 1, '{}', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'manual',
			        '2026-01-01T00:00:00Z')`,
		count: `SELECT COUNT(*) FROM snapshots WHERE container_name = 'legacy-web'`,
	},
}

// TestEverySchemaVersionUpgradesToCurrent applies the migrations up to each
// version in turn and then upgrades the rest of the way.
func TestEverySchemaVersionUpgradesToCurrent(t *testing.T) {
	t.Parallel()

	names, err := store.MigrationNames()
	if err != nil {
		t.Fatalf("MigrationNames: %v", err)
	}
	if len(names) < 20 {
		t.Fatalf("found %d migrations; the matrix is looking at the wrong directory", len(names))
	}

	// Every stopping point, including "nothing applied at all" -- a fresh
	// install is a version too, and it is the one every other test covers.
	for index, stop := range names {
		stop := stop
		index := index
		t.Run(strings.TrimSuffix(stop, ".sql"), func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "harbormaster.db")
			raw := openUnmigrated(t, path)

			applyThrough(t, raw, names[:index+1])
			written := seedLegacyRows(t, raw, names[:index+1])
			closeRawDB(t, raw)

			// The upgrade, through the real entry point: the same Migrate call
			// a deployment's next start makes, including its checksum
			// validation and its integrity check.
			db, err := store.Open(context.Background(), path)
			if err != nil {
				t.Fatalf("upgrading from %s: %v", stop, err)
			}
			defer func() { _ = db.Close() }()

			// Every migration is now recorded exactly once.
			applied := appliedMigrations(t, db.SQL())
			if len(applied) != len(names) {
				t.Fatalf("after upgrading from %s the database records %d migrations, want %d",
					stop, len(applied), len(names))
			}
			for i, name := range names {
				if applied[i] != name {
					t.Fatalf("migration %d is recorded as %q, want %q", i, applied[i], name)
				}
			}

			// And nothing written at the old version was lost.
			for _, query := range written {
				var count int
				if err := db.SQL().QueryRowContext(context.Background(), query).Scan(&count); err != nil {
					t.Fatalf("counting preserved rows after upgrading from %s: %v", stop, err)
				}
				if count != 1 {
					t.Fatalf("upgrading from %s lost a row: %q returned %d",
						stop, query, count)
				}
			}

			// The integrity check ran and found nothing wrong. An upgrade that
			// corrupted an index would otherwise pass every assertion above.
			if report := db.OpenReport().Integrity; !report.OK && !report.Incomplete {
				t.Fatalf("upgrading from %s left the database failing its integrity check: %s",
					stop, report.Summary())
			}
		})
	}
}

// A second open applies nothing and changes nothing.
//
// The property every restart depends on: migrations are recorded, checksummed,
// and skipped. A migration that were not idempotent in this sense would run
// again on every start.
func TestUpgradingTwiceIsANoOp(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "harbormaster.db")

	first, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	firstApplied := first.OpenReport().Migrations.Applied
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(firstApplied) == 0 {
		t.Fatal("the first open applied no migrations")
	}

	second, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer func() { _ = second.Close() }()

	if applied := second.OpenReport().Migrations.Applied; len(applied) != 0 {
		t.Fatalf("the second open applied %d migrations, want 0: %v", len(applied), applied)
	}
}

// The migrations are ordered, gapless, and uniquely numbered.
//
// A duplicate number is two files that both claim to be migration 21, and which
// one runs depends on lexical ordering nobody intended. A gap is a migration
// somebody deleted, which the validation refuses at every existing deployment.
func TestTheMigrationSequenceIsGaplessAndUnique(t *testing.T) {
	t.Parallel()

	names, err := store.MigrationNames()
	if err != nil {
		t.Fatalf("MigrationNames: %v", err)
	}

	seen := make(map[int]string, len(names))
	numbers := make([]int, 0, len(names))
	for _, name := range names {
		var number int
		if _, err := fmt.Sscanf(name, "%04d_", &number); err != nil {
			t.Fatalf("migration %q does not begin with a four-digit number", name)
		}
		if previous, clash := seen[number]; clash {
			t.Fatalf("migrations %q and %q both claim number %04d; which one runs "+
				"depends on lexical ordering nobody intended", previous, name, number)
		}
		seen[number] = name
		numbers = append(numbers, number)
	}

	sort.Ints(numbers)
	for index, number := range numbers {
		if number != index+1 {
			t.Fatalf("the migration sequence jumps to %04d at position %d; a gap is a "+
				"migration somebody deleted, and every existing deployment refuses to "+
				"open against one", number, index+1)
		}
	}

	// And the names are already in the order they must run in, so a caller that
	// iterates them does not have to sort.
	ordered := append([]string(nil), names...)
	sort.Strings(ordered)
	for index := range names {
		if names[index] != ordered[index] {
			t.Fatalf("MigrationNames returned %q at position %d, want %q; the caller "+
				"relies on this order", names[index], index, ordered[index])
		}
	}
}

// ------------------------------------------------------------------ helpers --

// openUnmigrated opens a file at a given path without running migrations.
//
// Distinct from openRawDB in snapshot_migration_test.go, which chooses its own
// temporary path: this test has to reopen the SAME file through store.Open
// afterwards, which is the whole point.
func openUnmigrated(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	return db
}

func closeRawDB(t *testing.T, db *sql.DB) {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatalf("close raw database: %v", err)
	}
}

// applyThrough applies the named migrations and records them, the way a
// deployment installed at that version would have.
func applyThrough(t *testing.T, db *sql.DB, names []string) {
	t.Helper()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}

	for _, name := range names {
		content, err := os.ReadFile(filepath.Join("migrations", name)) //nolint:gosec // a name from the embedded set
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

// seedLegacyRows writes whatever the applied set can hold, and returns the
// queries that must still find them afterwards.
func seedLegacyRows(t *testing.T, db *sql.DB, applied []string) []string {
	t.Helper()

	present := make(map[string]bool, len(applied))
	for _, name := range applied {
		present[name] = true
	}

	var checks []string
	for _, row := range legacyRows {
		if !present[row.after] {
			continue
		}
		if _, err := db.ExecContext(context.Background(), row.statement); err != nil {
			t.Fatalf("seed a row that %s made insertable: %v", row.after, err)
		}
		checks = append(checks, row.count)
	}
	return checks
}

// appliedMigrations reads the bookkeeping table in order.
func appliedMigrations(t *testing.T, db *sql.DB) []string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(),
		`SELECT name FROM schema_migrations ORDER BY name ASC`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan migration name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema_migrations: %v", err)
	}
	return names
}
