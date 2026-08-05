package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration is one embedded SQL file.
type migration struct {
	name string
	sql  string
	// checksum is the SHA-256 of the file's contents, hex encoded. It is what
	// lets a later run tell "this migration has already been applied" from
	// "a migration by this name was applied, but not this one".
	checksum string
}

// Migration bookkeeping and its validation.
//
// Applying a migration is the easy half. The half that goes wrong in
// production is deciding, on a database somebody else's binary has already
// touched, whether the schema in front of you is the schema this binary was
// built against. Three ways it can differ, and all three used to pass silently:
//
//   - THE SCHEMA IS AHEAD. A newer HarborMaster ran, applied 0006, and the
//     operator rolled back to this binary. Matching by filename, this binary
//     sees nothing pending and runs happily against a schema it does not
//     understand -- writing rows a column constraint it has never seen may
//     reject, or ignoring a column the newer version requires.
//   - A MIGRATION CHANGED. An applied file was edited afterwards. The name is
//     recorded, so it is skipped forever, and the database permanently differs
//     from what the source says it is.
//   - THE SEQUENCE HAS A GAP. 0004 is recorded and 0003 is not. Migrations are
//     applied in order inside one transaction each, so a crash cannot produce
//     this: it means the bookkeeping was edited by hand.
//
// Each of the three means "the schema is not what this binary believes", and
// each is refused. This is a fail-closed decision made deliberately: refusing
// to start is loud, reversible, and leaves the data intact, while continuing
// writes damage that is discovered later and cannot be undone.
//
// The checksum column is added to the bookkeeping table by this code rather
// than by a migration file, because a migration that alters the migration
// table is a bootstrapping problem: a fresh database would create the column
// and then try to add it again. Rows that predate the column carry NULL, which
// is treated as "applied by a version that did not record checksums" -- it is
// accepted and backfilled, because nothing can retroactively prove what those
// files contained. That limitation is documented rather than hidden.

// Errors a schema validation can produce. They are distinct so startup can say
// which of the three happened, and so a test can assert on the reason rather
// than on message text.
var (
	// ErrSchemaAhead reports that the database carries a migration this binary
	// does not know about, which means a newer version created it.
	ErrSchemaAhead = errors.New("database schema is newer than this build")
	// ErrMigrationChanged reports that an applied migration's contents no
	// longer match the embedded file of the same name.
	ErrMigrationChanged = errors.New("an applied migration has been modified")
	// ErrMigrationGap reports that migrations were applied out of order.
	ErrMigrationGap = errors.New("migration history has a gap")
)

// MigrateResult describes what one Migrate call did, so startup can log it.
type MigrateResult struct {
	// Applied names the migrations this call ran.
	Applied []string
	// AlreadyApplied counts migrations that were already recorded.
	AlreadyApplied int
	// ChecksumsBackfilled counts rows that predated checksum recording and
	// have now been given one.
	ChecksumsBackfilled int
}

// Migrate validates the recorded schema history, then applies whatever is
// pending.
//
// Each migration runs inside its own transaction together with its bookkeeping
// insert, so an interruption leaves the database at the last COMPLETE
// migration rather than half-applied. Validation happens first, in full,
// before any migration runs: a database that is ahead of this binary must be
// refused before it is written to, not after.
func Migrate(ctx context.Context, db *sql.DB) (MigrateResult, error) {
	var result MigrateResult

	if err := ensureMigrationsTable(ctx, db); err != nil {
		return result, err
	}

	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return result, err
	}

	migrations, err := loadMigrations()
	if err != nil {
		return result, err
	}

	if err := validateHistory(migrations, applied); err != nil {
		return result, err
	}

	for _, m := range migrations {
		record, ok := applied[m.name]
		if ok {
			result.AlreadyApplied++
			if !record.checksum.Valid {
				if err := backfillChecksum(ctx, db, m); err != nil {
					return result, err
				}
				result.ChecksumsBackfilled++
			}
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return result, fmt.Errorf("apply migration %s: %w", m.name, AsError(err))
		}
		result.Applied = append(result.Applied, m.name)
	}
	return result, nil
}

// validateHistory compares what the database records against what this binary
// embeds, and refuses every disagreement.
func validateHistory(migrations []migration, applied map[string]appliedMigration) error {
	known := make(map[string]migration, len(migrations))
	for _, m := range migrations {
		known[m.name] = m
	}

	// The schema being AHEAD is checked first, and deliberately so: it is the
	// case where continuing does the most damage, and it is also the one an
	// operator is most likely to have caused on purpose by rolling a release
	// back.
	unknown := make([]string, 0)
	for name := range applied {
		if _, ok := known[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf(
			"%w: it records migration(s) %s, which this build does not contain; "+
				"run the newer HarborMaster, or restore a backup taken before the upgrade",
			ErrSchemaAhead, strings.Join(unknown, ", "))
	}

	changed := make([]string, 0)
	for _, m := range migrations {
		record, ok := applied[m.name]
		if !ok || !record.checksum.Valid {
			continue
		}
		if record.checksum.String != m.checksum {
			changed = append(changed, m.name)
		}
	}
	if len(changed) > 0 {
		// The checksums themselves are deliberately not printed. They would be
		// noise to an operator, and the actionable fact is which file no longer
		// matches what was applied.
		return fmt.Errorf(
			"%w: %s no longer matches the file that was applied; "+
				"an applied migration must never be edited -- add a new one instead",
			ErrMigrationChanged, strings.Join(changed, ", "))
	}

	// Migrations apply in lexical order, so the applied set must be a prefix of
	// the embedded set. A hole means the bookkeeping was edited.
	seenPending := false
	missing := make([]string, 0)
	for _, m := range migrations {
		_, ok := applied[m.name]
		switch {
		case !ok:
			seenPending = true
		case seenPending:
			// Applied, but something before it is not.
			missing = append(missing, m.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"%w: %s is recorded as applied while an earlier migration is not; "+
				"the schema_migrations table has been edited and no longer describes the schema",
			ErrMigrationGap, strings.Join(missing, ", "))
	}

	return nil
}

// MigrationNames returns the embedded migrations in application order. It
// exists so tests can assert the set without reaching into the embed.FS.
func MigrationNames() ([]string, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(migrations))
	for _, m := range migrations {
		names = append(names, m.name)
	}
	return names, nil
}

// ensureMigrationsTable creates the bookkeeping table and brings it up to the
// shape this version expects.
//
// The checksum column is added here rather than in a migration file: a
// migration that alters the migration table cannot run on a fresh database
// whose bootstrap CREATE already includes the column.
func ensureMigrationsTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", AsError(err))
	}

	hasChecksum, err := columnExists(ctx, db, "schema_migrations", "checksum")
	if err != nil {
		return err
	}
	if hasChecksum {
		return nil
	}
	// Nullable with no default: a row that predates this column must stay
	// distinguishable from one whose checksum was actually recorded, or the
	// verification above would compare against an invented value.
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE schema_migrations ADD COLUMN checksum TEXT`); err != nil {
		return fmt.Errorf("add schema_migrations.checksum: %w", AsError(err))
	}
	return nil
}

// columnExists reports whether a table has a column.
//
// The table name is a compile-time constant at every call site; PRAGMA
// arguments cannot be bound, so this takes the name as a parameter to a
// quoted-identifier pragma call rather than concatenating it into the pragma
// text. `pragma_table_info` is the table-valued form, which does accept a
// bound parameter.
func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
		table, column).Scan(&count); err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, AsError(err))
	}
	return count > 0, nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (name, checksum) VALUES (?, ?)`,
		m.name, m.checksum); err != nil {
		return err
	}
	return tx.Commit()
}

// backfillChecksum records the checksum of a migration applied by a version
// that did not record one.
//
// This CANNOT verify that the file applied then is the file embedded now --
// nothing can, the evidence does not exist. It records what is true from this
// point forward, so a later edit is caught even though an earlier one is not.
func backfillChecksum(ctx context.Context, db *sql.DB, m migration) error {
	if _, err := db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = ? WHERE name = ? AND checksum IS NULL`,
		m.checksum, m.name); err != nil {
		return fmt.Errorf("backfill checksum for %s: %w", m.name, AsError(err))
	}
	return nil
}

// appliedMigration is one row of the bookkeeping table.
type appliedMigration struct {
	name string
	// checksum is invalid for a row written before checksums were recorded.
	checksum sql.NullString
}

func appliedMigrations(ctx context.Context, db *sql.DB) (map[string]appliedMigration, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[string]appliedMigration)
	for rows.Next() {
		var record appliedMigration
		if err := rows.Scan(&record.name, &record.checksum); err != nil {
			return nil, err
		}
		applied[record.name] = record
	}
	return applied, rows.Err()
}

// AppliedMigrations returns the names of the migrations a database records, in
// order. It exists for the diagnose and backup-verification paths, which
// compare a database's history against this build's without applying anything.
func AppliedMigrations(ctx context.Context, db *sql.DB) ([]string, error) {
	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(applied))
	for name := range applied {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		content, err := migrationFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read embedded migration %s: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(content)
		migrations = append(migrations, migration{
			name:     entry.Name(),
			sql:      string(content),
			checksum: hex.EncodeToString(sum[:]),
		})
	}

	// Filenames are zero-padded and ordered, so lexical order is apply order.
	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].name < migrations[j].name
	})
	return migrations, nil
}
