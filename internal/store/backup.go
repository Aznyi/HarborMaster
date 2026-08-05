package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Consistent backup, and the verification that makes it worth having.
//
// Copying harbormaster.db with cp is not a backup. With write-ahead logging
// the committed state is split across the database file and the -wal sidecar,
// so a file copy taken while HarborMaster is running can capture a database
// missing its most recent commits, or -- if the copy races a checkpoint -- a
// database that is internally inconsistent. It will usually seem to work,
// which is the worst property a backup procedure can have.
//
// `VACUUM INTO` is SQLite's answer: it runs inside a read transaction, so it
// sees one consistent snapshot of committed state, and it writes a fresh,
// defragmented database rather than a byte copy. It needs no exclusive lock,
// so a backup does not stop the server.
//
// AND THE COPY IS THEN VERIFIED. An unverified backup is a belief, not a
// control. Verification opens the copy read-only and establishes three things
// independently: the image is structurally sound, its schema history matches
// this build, and the tables that carry the operator's history are present and
// countable.

// Errors a backup can refuse with.
var (
	// ErrBackupExists reports that the destination already exists. A backup
	// command must never silently overwrite the previous backup: that is how
	// one bad backup destroys the good one.
	ErrBackupExists = errors.New("backup destination already exists")
	// ErrBackupPathConflict reports that the destination is the live database
	// or one of its sidecars.
	ErrBackupPathConflict = errors.New("backup destination conflicts with the live database")
)

// backupFileMode is the mode a backup is created with.
//
// 0600. A backup is a complete copy of HarborMaster's database, which carries
// container configuration, labels, mounts, and the digests of secret values.
// No plaintext secret is in there by invariant, but the file is still the most
// concentrated description of the host HarborMaster produces, and it must not
// be world readable.
const backupFileMode = 0o600

// backupDirMode matches the data directory: owner and group, never world.
const backupDirMode = 0o750

// BackupResult describes a completed backup.
type BackupResult struct {
	// Path is the file that was written.
	Path string
	// SizeBytes is its size on disk.
	SizeBytes int64
	// Duration is how long the copy took, excluding verification.
	Duration time.Duration
	// Verification is the independent check performed on the copy.
	Verification BackupVerification
}

// BackupVerification is what reading the backup back established.
type BackupVerification struct {
	// Integrity is a FULL check. A backup is written once and read years
	// later, so the expensive check is the right one here -- unlike at
	// startup, nothing is waiting on it.
	Integrity IntegrityReport
	// Migrations are the migrations the backup records.
	Migrations []string
	// MigrationsMatch reports that they are exactly this build's set.
	MigrationsMatch bool
	// TableCounts are row counts for the tables that carry history.
	TableCounts map[string]int64
	// OK reports that every check passed.
	OK bool
	// Problems describes each check that did not.
	Problems []string
}

// Backup writes a consistent copy of db to destPath and verifies it.
//
// livePath is the database the handle is open on, so the destination can be
// refused if it would collide with it. Pass DB.Path().
func Backup(ctx context.Context, db *sql.DB, livePath, destPath string) (BackupResult, error) {
	result := BackupResult{Path: destPath}

	if err := validateBackupDestination(livePath, destPath); err != nil {
		return result, err
	}

	if dir := filepath.Dir(destPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, backupDirMode); err != nil {
			return result, fmt.Errorf("create backup directory: %w", err)
		}
	}

	started := time.Now()
	// A bound parameter, not string concatenation. SQLite accepts an
	// expression after INTO, so the path never becomes part of the SQL text
	// and a path containing a quote is a path rather than a syntax error.
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, destPath); err != nil {
		// A partial file from a failed VACUUM is worse than no file: it looks
		// like a backup. SQLite removes it itself in most failure modes; this
		// is the belt to that braces.
		removePartialBackup(destPath)
		return result, fmt.Errorf("write backup: %w", AsError(err))
	}
	result.Duration = time.Since(started)

	// Tighten the mode after the fact. SQLite creates the file subject to the
	// process umask, which on a permissive host yields 0644.
	if err := os.Chmod(destPath, backupFileMode); err != nil {
		removePartialBackup(destPath)
		return result, fmt.Errorf("restrict backup permissions: %w", err)
	}

	if err := normalizeBackupJournalMode(ctx, destPath); err != nil {
		removePartialBackup(destPath)
		return result, err
	}

	info, err := os.Stat(destPath)
	if err != nil {
		return result, fmt.Errorf("stat backup: %w", err)
	}
	result.SizeBytes = info.Size()

	verification, err := VerifyBackup(ctx, destPath)
	if err != nil {
		return result, fmt.Errorf("verify backup: %w", err)
	}
	result.Verification = verification

	if !verification.OK {
		// The file is deliberately LEFT in place. It is evidence, and an
		// operator investigating why a backup failed verification needs to be
		// able to look at it. The error is what stops it being trusted.
		return result, fmt.Errorf("backup failed verification: %s",
			strings.Join(verification.Problems, "; "))
	}
	return result, nil
}

// normalizeBackupJournalMode puts the copy into the same journal mode as the
// database it came from.
//
// `VACUUM INTO` writes a brand-new database with SQLite's DEFAULTS, and journal
// mode is not among the settings it carries across: the copy comes out in
// rollback-journal mode however the source was configured. Left alone, that has
// one visible and one invisible consequence.
//
// The visible one is that diagnosing a backup before a restore reports "the
// journal mode is \"delete\" rather than WAL" -- a warning that is true of the
// file, means nothing about its health, and is exactly the sort of false alarm
// that teaches an operator to skip the verification step.
//
// The invisible one is that a restored database silently runs with a different
// durability and concurrency profile until something opens it read-write and
// converts it.
//
// Setting it here, on the copy only, removes both. The source is untouched.
func normalizeBackupJournalMode(ctx context.Context, path string) error {
	// The standard read-write DSN, which sets journal_mode(WAL). Opening it at
	// all is also a check worth having: it proves the copy is a database this
	// build can open, before anything reports the backup as written.
	db, err := sql.Open("sqlite", dsn(path, DefaultBusyTimeout))
	if err != nil {
		return fmt.Errorf("open backup to set its journal mode: %w", AsError(err))
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	mode, err := JournalMode(ctx, db)
	if err != nil {
		return fmt.Errorf("set backup journal mode: %w", err)
	}
	if mode != "wal" {
		// Not fatal. A backup in rollback-journal mode is a correct database;
		// it just does not match the source. Saying so beats failing the
		// backup over a configuration detail.
		return nil
	}

	// Checkpoint and remove the sidecars the conversion created, so the backup
	// is the single self-contained file an operator can copy elsewhere.
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint backup: %w", AsError(err))
	}
	return nil
}

// validateBackupDestination refuses destinations that would destroy data.
func validateBackupDestination(livePath, destPath string) error {
	if strings.TrimSpace(destPath) == "" {
		return errors.New("backup destination must not be empty")
	}

	// Compared after cleaning so ./data/x.db and data/x.db are recognised as
	// the same file. Symlinks are not resolved: EvalSymlinks fails on a path
	// that does not exist yet, which the destination normally does not, and
	// the check below is a guard against mistakes rather than against a
	// determined operator -- who is, after all, root on their own host.
	dest := filepath.Clean(destPath)
	live := filepath.Clean(livePath)

	if live != "" && live != "." {
		for _, forbidden := range []string{live, live + "-wal", live + "-shm", live + "-journal"} {
			if sameFilePath(dest, forbidden) {
				return fmt.Errorf("%w: %s", ErrBackupPathConflict, filepath.Base(forbidden))
			}
		}
	}

	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("%w: %s", ErrBackupExists, dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check backup destination: %w", err)
	}
	return nil
}

// sameFilePath compares two paths for equality.
//
// Case-insensitively on Windows, where the filesystem is, so a backup to
// DATA\HARBORMASTER.DB is still recognised as the live database.
func sameFilePath(a, b string) bool {
	if a == b {
		return true
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return false
}

// removePartialBackup deletes a half-written backup.
//
// The error is ignored on purpose: this runs on a path that is already
// failing, and a failure to clean up must not replace the real error with a
// less useful one. The remaining file is reported by the caller's error.
func removePartialBackup(path string) {
	_ = os.Remove(path)
}

// backupTables are the tables whose row counts a verification reports.
//
// An explicit allowlist with LITERAL queries, not a loop over
// sqlite_master building "SELECT COUNT(*) FROM " + name. The table names are
// compile-time constants of this package, so no identifier ever reaches SQL
// text from data, and the list doubles as an assertion that a backup contains
// the tables an operator would want back.
var backupTables = []struct {
	name  string
	query string
}{
	{"containers", `SELECT COUNT(*) FROM containers`},
	{"images", `SELECT COUNT(*) FROM images`},
	{"networks", `SELECT COUNT(*) FROM networks`},
	{"volumes", `SELECT COUNT(*) FROM volumes`},
	{"snapshots", `SELECT COUNT(*) FROM snapshots`},
	{"docker_events", `SELECT COUNT(*) FROM docker_events`},
	{"events", `SELECT COUNT(*) FROM events`},
	{"inventory_refreshes", `SELECT COUNT(*) FROM inventory_refreshes`},
}

// verifyTimeout bounds a whole verification pass.
//
// Generous, because a full integrity check on a large database is genuinely
// slow, and finite, because a verification that can hang is a backup command
// that can hang.
const verifyTimeout = 10 * time.Minute

// VerifyBackup opens a backup read-only and establishes that it is usable.
//
// Read-only is the point: verification must not be able to modify the artefact
// it is verifying, and a verification that migrated the backup would make the
// copy differ from the database it was taken from.
func VerifyBackup(ctx context.Context, path string) (BackupVerification, error) {
	verification := BackupVerification{
		TableCounts: make(map[string]int64, len(backupTables)),
		Problems:    make([]string, 0),
	}

	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	db, err := OpenReadOnly(ctx, path, DefaultBusyTimeout)
	if err != nil {
		return verification, err
	}
	defer func() { _ = db.Close() }()

	// The full check, not the quick one. Nothing is waiting on this, and the
	// whole purpose of a backup is to still be sound when it is finally needed.
	verification.Integrity, err = CheckIntegrity(ctx, db, IntegrityFull, 0)
	if err != nil {
		return verification, err
	}
	switch {
	case verification.Integrity.Incomplete:
		verification.Problems = append(verification.Problems,
			"integrity check did not complete")
	case !verification.Integrity.OK:
		verification.Problems = append(verification.Problems,
			verification.Integrity.Summary())
	}

	verification.Migrations, err = AppliedMigrations(ctx, db)
	if err != nil {
		verification.Problems = append(verification.Problems,
			"schema history could not be read")
		return verification, nil
	}

	expected, err := MigrationNames()
	if err != nil {
		return verification, err
	}
	verification.MigrationsMatch = equalStrings(verification.Migrations, expected)
	if !verification.MigrationsMatch {
		// Not necessarily damage: a backup taken by an older build legitimately
		// has fewer migrations. It IS something an operator must know before
		// restoring, so it is reported rather than passed.
		verification.Problems = append(verification.Problems, fmt.Sprintf(
			"schema history has %d migration(s); this build expects %d",
			len(verification.Migrations), len(expected)))
	}

	for _, table := range backupTables {
		var count int64
		if err := db.QueryRowContext(ctx, table.query).Scan(&count); err != nil {
			verification.Problems = append(verification.Problems,
				"table "+table.name+" could not be read")
			continue
		}
		verification.TableCounts[table.name] = count
	}

	verification.OK = len(verification.Problems) == 0
	return verification, nil
}

// equalStrings reports whether two ordered slices hold the same values.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
