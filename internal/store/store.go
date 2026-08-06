// Package store owns HarborMaster's SQLite persistence.
//
// The driver is modernc.org/sqlite, a pure-Go implementation, so the binary
// builds without CGO and the container image can run as an unprivileged user
// on a minimal base image.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	// Registers the pure-Go "sqlite" driver.
	_ "modernc.org/sqlite"
)

// Defaults for the reliability settings. Each is also documented in
// .env.example, where the operator-facing explanation lives.
const (
	// DefaultBusyTimeout is how long a statement waits for another writer's
	// lock before returning SQLITE_BUSY.
	//
	// It exists for CROSS-PROCESS contention. Within the process, MaxOpenConns(1)
	// already serialises writers, so this bounds the wait against a second
	// HarborMaster, a backup tool, or an operator with a sqlite3 shell open on
	// the same file.
	DefaultBusyTimeout = 5 * time.Second

	// DefaultIntegrityMode is the check performed at open. Quick rather than
	// full: see integrity.go for why the cheap check is the right default on
	// the startup path.
	DefaultIntegrityMode = IntegrityQuick

	// DefaultIntegrityTimeout bounds that check. Past it the check is reported
	// as incomplete and startup continues, because a false-positive outage on
	// a large but healthy database is worse than a late diagnosis.
	DefaultIntegrityTimeout = 30 * time.Second
)

// DB wraps the SQL handle together with the repositories built on it.
type DB struct {
	sql       *sql.DB
	Snapshots *SnapshotRepository
	Events    *EventRepository

	// Inventory repositories. Snapshots above and Inventory here are different
	// concepts: a snapshot is an immutable point-in-time capture, while the
	// inventory is the current observed state, replaced by each refresh.
	Inventory  *InventoryRepository
	Containers *ContainerRepository
	Images     *ImageRepository
	Networks   *NetworkRepository
	Volumes    *VolumeRepository

	// DockerEvents is the observational Docker event history. Distinct from
	// Events above: that is HarborMaster's audit log of its OWN actions, while
	// this records what the Docker daemon reported.
	DockerEvents *DockerEventRepository

	// Drift holds the differences between a container's baseline snapshot and
	// its current configuration. It references snapshots rather than copying
	// them: a snapshot is immutable evidence and must have one home.
	Drift *DriftRepository

	// Policies holds administrator-defined compliance rules and the violations
	// they produce. Distinct from Drift above: drift measures change from a
	// baseline, a policy measures compliance with a rule. A container can drift
	// while staying compliant, and comply while never having drifted.
	Policies *PolicyRepository

	// ImageIntel holds what registries report about the images the inventory
	// references: digests, update availability, and per-host health. It reads
	// registries over HTTPS and stores what it learned; nothing behind it can
	// pull, push, or remove an image.
	ImageIntel *ImageIntelRepository

	// Plans holds change plans: immutable assessments of proposed image
	// changes, built by combining the inventory, snapshots, readiness, drift,
	// policy compliance, and image intelligence. A plan is analysis, never an
	// instruction -- nothing executes one, and nothing behind this can.
	Plans *PlanRepository

	// Acquisitions holds the audit records for image acquisition -- the one
	// operation in HarborMaster that changes the Docker host. Every row is a
	// record of a download; none of them describes a container being changed,
	// because that capability does not exist.
	Acquisitions *AcquisitionRepository

	// path is retained so a backup can refuse to write over the live database.
	path string
	// openReport records what the reliability checks found at open, so the
	// health and diagnose paths can report it without re-running them.
	openReport OpenReport
}

// Options configures how a database is opened.
//
// Every field has a safe zero value, so Open(ctx, path) remains the whole API
// for a caller with nothing to say about reliability tuning.
type Options struct {
	// Path is the SQLite database file. Required.
	Path string
	// BusyTimeout bounds the wait for another process's write lock. Zero
	// selects DefaultBusyTimeout.
	BusyTimeout time.Duration
	// Integrity selects the check performed at open. Empty selects
	// DefaultIntegrityMode.
	Integrity IntegrityMode
	// IntegrityTimeout bounds that check. Zero selects DefaultIntegrityTimeout.
	IntegrityTimeout time.Duration
	// RequireWAL refuses to open when the write-ahead log could not be
	// enabled. Off by default: a rollback journal is slower and gives readers
	// less concurrency, but it is CORRECT, and refusing to start on a
	// filesystem that cannot do WAL would be a harsh default. An operator who
	// depends on WAL's crash profile can turn the warning into a refusal.
	RequireWAL bool
	// Logger receives the open-time reliability findings. Nil uses the default
	// logger.
	Logger *slog.Logger
}

// OpenReport records what the open-time reliability checks established.
//
// Retained on the DB so the diagnose command and the health endpoint can
// report the startup verdict without paying for the check again.
type OpenReport struct {
	// Integrity is the validation result. Its Mode is IntegrityOff when the
	// check was disabled.
	Integrity IntegrityReport
	// JournalMode is the mode the connection actually got, which is not
	// necessarily the mode the DSN asked for.
	JournalMode string
	// WALRequested reports that WAL was asked for, so a caller can tell "we
	// got a rollback journal because we asked for one" from "we asked for WAL
	// and the filesystem refused".
	WALRequested bool
	// Migrations records what the migration pass did.
	Migrations MigrateResult
}

// WALActive reports whether the connection ended up in write-ahead-log mode.
func (r OpenReport) WALActive() bool {
	return r.JournalMode == "wal"
}

// Open opens (creating if necessary) the SQLite database at path, validates
// it, applies the embedded migrations, and returns a ready DB.
//
// It is the simple form of OpenWithOptions and takes every default.
func Open(ctx context.Context, path string) (*DB, error) {
	return OpenWithOptions(ctx, Options{Path: path})
}

// OpenWithOptions opens the database with explicit reliability settings.
//
// The order is deliberate and each step gates the next:
//
//  1. Create the directory. A missing or unwritable data directory is the most
//     common deployment mistake and produces the clearest error here.
//  2. Connect and ping, so a broken path fails before anything else runs.
//  3. Confirm the journal mode. WAL is requested in the DSN, but SQLite falls
//     back silently on a filesystem that cannot support it.
//  4. Validate integrity, BEFORE migrating. A migration writes; writing to a
//     database already known to be malformed destroys the operator's remaining
//     backup window.
//  5. Migrate, which validates the recorded history first and refuses a schema
//     this build does not understand.
func OpenWithOptions(ctx context.Context, opts Options) (*DB, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.Path == "" {
		return nil, errors.New("open database: no path configured")
	}
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = DefaultBusyTimeout
	}
	if opts.Integrity == "" {
		opts.Integrity = DefaultIntegrityMode
	}
	if !ValidIntegrityMode(opts.Integrity) {
		return nil, fmt.Errorf("open database: unknown integrity mode %q", opts.Integrity)
	}
	if opts.IntegrityTimeout <= 0 {
		opts.IntegrityTimeout = DefaultIntegrityTimeout
	}

	// The parent directory is created with 0o750 so the database is not world
	// readable; snapshots may describe privileged container configuration.
	if dir := filepath.Dir(opts.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	sqlDB, err := sql.Open("sqlite", dsn(opts.Path, opts.BusyTimeout))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", AsError(err))
	}

	// SQLite tolerates exactly one writer. Serialising here is simpler and
	// more predictable than relying on busy-timeout retries alone.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(time.Hour)

	// Any failure past this point must close the handle. A returned error with
	// a live connection behind it leaks a file descriptor and, worse, keeps a
	// lock on a database the caller believes it never opened.
	closeOnError := func(err error) error {
		_ = sqlDB.Close()
		return err
	}

	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, closeOnError(fmt.Errorf("connect to database: %w", AsError(err)))
	}

	report := OpenReport{WALRequested: true}

	report.JournalMode, err = JournalMode(ctx, sqlDB)
	if err != nil {
		return nil, closeOnError(fmt.Errorf("confirm journal mode: %w", err))
	}
	if !report.WALActive() {
		if opts.RequireWAL {
			return nil, closeOnError(fmt.Errorf(
				"write-ahead logging could not be enabled (journal mode is %q) and it is required by configuration; "+
					"the database is probably on a filesystem that cannot support WAL's shared-memory index",
				report.JournalMode))
		}
		logger.Warn("write-ahead logging is not active; the database is running with a rollback journal",
			slog.String("journalMode", report.JournalMode),
			slog.String("effect", "readers block on the writer and the crash-recovery profile differs from the documented one"))
	}

	report.Integrity, err = CheckIntegrity(ctx, sqlDB, opts.Integrity, opts.IntegrityTimeout)
	if err != nil {
		return nil, closeOnError(fmt.Errorf("validate database: %w", err))
	}
	switch {
	case report.Integrity.Damaged():
		// Fail closed. Migrating or serving on a malformed image writes more
		// history over the damage and shortens the window in which a backup
		// still predates it.
		logger.Error("database failed its integrity check; refusing to start",
			slog.String("summary", report.Integrity.Summary()),
			slog.String("remedy", Remedy(FailureCorrupt)))
		for _, problem := range report.Integrity.Problems {
			logger.Error("integrity problem", slog.String("detail", problem))
		}
		return nil, closeOnError(fmt.Errorf("%w: %s", ErrCorrupt, report.Integrity.Summary()))

	case report.Integrity.Incomplete:
		// Nothing was established, so nothing is concluded. Refusing here would
		// turn a slow disk into an outage.
		logger.Warn("database integrity check did not complete; continuing without a verdict",
			slog.String("summary", report.Integrity.Summary()),
			slog.String("remedy", "run `harbormaster diagnose` when the host is quieter, or raise the integrity timeout"))

	case opts.Integrity != IntegrityOff:
		logger.Info("database integrity verified", slog.String("summary", report.Integrity.Summary()))
	}

	report.Migrations, err = Migrate(ctx, sqlDB)
	if err != nil {
		return nil, closeOnError(fmt.Errorf("migrate database: %w", err))
	}
	if len(report.Migrations.Applied) > 0 {
		logger.Info("applied database migrations",
			slog.Int("count", len(report.Migrations.Applied)),
			slog.Any("names", report.Migrations.Applied))
	}
	if report.Migrations.ChecksumsBackfilled > 0 {
		logger.Info("recorded checksums for migrations applied by an earlier version",
			slog.Int("count", report.Migrations.ChecksumsBackfilled))
	}

	return &DB{
		sql:        sqlDB,
		Snapshots:  &SnapshotRepository{db: sqlDB},
		Events:     &EventRepository{db: sqlDB},
		Inventory:  &InventoryRepository{db: sqlDB},
		Containers: &ContainerRepository{db: sqlDB},
		Images:     &ImageRepository{db: sqlDB},
		Networks:   &NetworkRepository{db: sqlDB},
		Volumes:    &VolumeRepository{db: sqlDB},

		DockerEvents: &DockerEventRepository{db: sqlDB},
		Drift:        &DriftRepository{db: sqlDB},
		Policies:     &PolicyRepository{db: sqlDB},
		ImageIntel:   &ImageIntelRepository{db: sqlDB},
		Plans:        &PlanRepository{db: sqlDB},
		Acquisitions: &AcquisitionRepository{db: sqlDB},

		path:       opts.Path,
		openReport: report,
	}, nil
}

// OpenReadOnly opens a database for inspection WITHOUT migrating it.
//
// Two properties matter, and both are why this is not a flag on Open:
//
//   - It never writes. The diagnose and backup-verification paths must be
//     usable against a database another process owns, and against one whose
//     schema this binary would otherwise "helpfully" upgrade.
//   - It never migrates. Running migrations as a side effect of asking a
//     database a question is exactly the surprise a diagnostic must not be.
//
// It returns the raw handle rather than a *DB, because a read-only connection
// cannot honour the repositories' write methods and handing out a type that
// looks like it can would be a trap.
//
// LIMITATION: a database left with an unrecovered write-ahead log by a crashed
// process cannot be opened read-only, because replaying the log requires
// writing. The caller gets a FailureCantOpen and should report the condition
// rather than treat it as a missing file.
func OpenReadOnly(ctx context.Context, path string, busyTimeout time.Duration) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("open database: no path configured")
	}
	if busyTimeout <= 0 {
		busyTimeout = DefaultBusyTimeout
	}

	// Existence is checked first so a missing file reports as a missing file
	// rather than as an opaque driver error. SQLite would otherwise create it,
	// which a read-only inspection must never do.
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("database file: %w", err)
	}

	sqlDB, err := sql.Open("sqlite", readOnlyDSN(path, busyTimeout))
	if err != nil {
		return nil, fmt.Errorf("open database read-only: %w", AsError(err))
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("connect to database read-only: %w", AsError(err))
	}
	return sqlDB, nil
}

// SQL exposes the underlying handle for tests and for repositories that live
// outside this package.
func (d *DB) SQL() *sql.DB { return d.sql }

// Path reports the database file this handle was opened on.
func (d *DB) Path() string { return d.path }

// OpenReport returns what the open-time reliability checks found.
func (d *DB) OpenReport() OpenReport { return d.openReport }

// Ping verifies the database is reachable.
func (d *DB) Ping(ctx context.Context) error { return d.sql.PingContext(ctx) }

// Close releases the database handle.
//
// It checkpoints the write-ahead log first, on a bounded context. A clean
// checkpoint means the next start has no log to replay, which shortens
// recovery and leaves the file in the state a backup tool expects. Failing to
// checkpoint is not an error: WAL replay on the next open is correct, just
// slower, and refusing to close would be worse than closing untidily.
func (d *DB) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), checkpointTimeout)
	defer cancel()

	// TRUNCATE rather than PASSIVE: PASSIVE gives up when a reader is still
	// attached, which at shutdown is precisely when it would not run.
	if _, err := d.sql.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		slog.Default().Debug("could not checkpoint the write-ahead log before close",
			slog.String("error", err.Error()))
	}
	return d.sql.Close()
}

// checkpointTimeout bounds the shutdown checkpoint. Short on purpose: a
// checkpoint that cannot finish quickly is not worth delaying shutdown for,
// because the next open replays the log correctly anyway.
const checkpointTimeout = 5 * time.Second

// dsn builds the modernc.org/sqlite connection string.
//
// WAL keeps reads from blocking the single writer, foreign_keys enforces
// referential integrity (off by default in SQLite), and busy_timeout absorbs
// cross-process contention instead of surfacing SQLITE_BUSY immediately.
func dsn(path string, busyTimeout time.Duration) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busyTimeout.Milliseconds(), 10)+")")
	q.Add("_pragma", "synchronous(NORMAL)")
	return "file:" + path + "?" + q.Encode()
}

// readOnlyDSN builds a connection string that cannot write.
//
// mode=ro is enforced by SQLite itself rather than by convention, so a
// mistaken write through this handle is an error rather than a modification.
// The journal mode is deliberately NOT set: changing it is a write, and a
// read-only connection must take the database as it finds it.
func readOnlyDSN(path string, busyTimeout time.Duration) string {
	q := url.Values{}
	q.Add("mode", "ro")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busyTimeout.Milliseconds(), 10)+")")
	return "file:" + path + "?" + q.Encode()
}
