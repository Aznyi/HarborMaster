// Package store owns HarborMaster's SQLite persistence.
//
// The driver is modernc.org/sqlite, a pure-Go implementation, so the binary
// builds without CGO and the container image can run as an unprivileged user
// on a minimal base image.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	// Registers the pure-Go "sqlite" driver.
	_ "modernc.org/sqlite"
)

// DB wraps the SQL handle together with the repositories built on it.
type DB struct {
	sql       *sql.DB
	Snapshots *SnapshotRepository
	Events    *EventRepository
}

// Open opens (creating if necessary) the SQLite database at path, applies the
// embedded migrations, and returns a ready DB.
//
// The parent directory is created with 0o750 so the database is not world
// readable; snapshots may describe privileged container configuration.
func Open(ctx context.Context, path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	sqlDB, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// SQLite tolerates exactly one writer. Serialising here is simpler and
	// more predictable than relying on busy-timeout retries alone.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(time.Hour)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := Migrate(ctx, sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	return &DB{
		sql:       sqlDB,
		Snapshots: &SnapshotRepository{db: sqlDB},
		Events:    &EventRepository{db: sqlDB},
	}, nil
}

// SQL exposes the underlying handle for tests and for repositories that live
// outside this package.
func (d *DB) SQL() *sql.DB { return d.sql }

// Ping verifies the database is reachable.
func (d *DB) Ping(ctx context.Context) error { return d.sql.PingContext(ctx) }

// Close releases the database handle.
func (d *DB) Close() error { return d.sql.Close() }

// dsn builds the modernc.org/sqlite connection string.
//
// WAL keeps reads from blocking the single writer, foreign_keys enforces
// referential integrity (off by default in SQLite), and busy_timeout absorbs
// brief contention instead of surfacing SQLITE_BUSY.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	return "file:" + path + "?" + q.Encode()
}
