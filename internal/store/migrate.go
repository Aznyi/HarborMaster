package store

import (
	"context"
	"database/sql"
	"embed"
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
}

// Migrate applies every embedded migration that has not been applied yet.
//
// Each migration runs inside its own transaction together with the bookkeeping
// insert, so a failure leaves the database at the last complete migration
// rather than half-applied. Applied migrations are matched by filename.
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return err
	}

	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if _, ok := applied[m.name]; ok {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
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
		`INSERT INTO schema_migrations (name) VALUES (?)`, m.name); err != nil {
		return err
	}
	return tx.Commit()
}

func appliedMigrations(ctx context.Context, db *sql.DB) (map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		applied[name] = struct{}{}
	}
	return applied, rows.Err()
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
		migrations = append(migrations, migration{name: entry.Name(), sql: string(content)})
	}

	// Filenames are zero-padded and ordered, so lexical order is apply order.
	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].name < migrations[j].name
	})
	return migrations, nil
}
