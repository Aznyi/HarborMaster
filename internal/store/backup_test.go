package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// backupDB returns a database with enough content that a backup is worth
// taking, plus the path it lives at.
func backupDB(t *testing.T) (*store.DB, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "harbormaster.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for i := range 25 {
		createSnapshot(t, db, newSnapshot("container-a", checksumA[:62]+padHex(i)))
	}
	if _, err := db.Events.Append(context.Background(), domain.Event{
		Type:     domain.EventServerStarted,
		Severity: domain.SeverityInfo,
		Message:  "started",
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	return db, path
}

func padHex(i int) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[(i/16)%16], hex[i%16]})
}

func TestBackupRoundTripsAndVerifies(t *testing.T) {
	db, path := backupDB(t)
	dest := filepath.Join(t.TempDir(), "backup.db")

	result, err := store.Backup(context.Background(), db.SQL(), path, dest)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if result.SizeBytes <= 0 {
		t.Error("the backup is empty")
	}
	if !result.Verification.OK {
		t.Errorf("the backup did not verify: %v", result.Verification.Problems)
	}
	if !result.Verification.MigrationsMatch {
		t.Error("the backup's schema history must match this build's")
	}
	if got := result.Verification.TableCounts["snapshots"]; got != 25 {
		t.Errorf("snapshots in backup = %d, want 25", got)
	}

	// The copy must match the SOURCE's journal mode. VACUUM INTO writes a new
	// database with SQLite's defaults and does not carry journal mode across,
	// so without normalisation a backup comes out in rollback-journal mode --
	// which makes `diagnose` warn about a perfectly good backup, and makes a
	// restored database run with a different durability profile until
	// something happens to convert it.
	readOnly, err := store.OpenReadOnly(context.Background(), dest, time.Second)
	if err != nil {
		t.Fatalf("open backup read-only: %v", err)
	}
	defer func() { _ = readOnly.Close() }()

	mode, err := store.JournalMode(context.Background(), readOnly)
	if err != nil {
		t.Fatalf("JournalMode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("backup journal mode = %q, want wal to match the source; "+
			"diagnosing this backup would warn about a healthy file", mode)
	}

	// And the copy must be independently openable, not merely countable.
	restored, err := store.Open(context.Background(), dest)
	if err != nil {
		t.Fatalf("the backup could not be opened as a database: %v", err)
	}
	defer func() { _ = restored.Close() }()

	count, err := restored.Snapshots.Count(context.Background())
	if err != nil {
		t.Fatalf("count in restored: %v", err)
	}
	if count != 25 {
		t.Errorf("restored snapshots = %d, want 25", count)
	}
}

// The copy must be consistent even while the source is being written.
//
// This is the property a `cp` of a WAL database does not have, and the reason
// VACUUM INTO is used instead. The assertion is not "the backup has every row"
// -- it cannot, a snapshot is a point in time -- but "the backup is a valid,
// internally consistent database", which a torn copy would not be.
func TestBackupIsConsistentWhileTheDatabaseIsBeingWritten(t *testing.T) {
	db, path := backupDB(t)
	dest := filepath.Join(t.TempDir(), "concurrent.db")
	ctx := context.Background()

	stop := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := db.Events.Append(ctx, domain.Event{
				Type:     domain.EventServerStarted,
				Severity: domain.SeverityInfo,
				Message:  "concurrent write",
			}); err != nil {
				return
			}
		}
	}()

	result, err := store.Backup(ctx, db.SQL(), path, dest)
	close(stop)
	writers.Wait()

	if err != nil {
		t.Fatalf("Backup during concurrent writes: %v", err)
	}
	if !result.Verification.OK {
		t.Errorf("a backup taken during writes did not verify: %v", result.Verification.Problems)
	}
	if !result.Verification.Integrity.OK {
		t.Error("the backup is not internally consistent; VACUUM INTO did not give a clean snapshot")
	}
}

// Overwriting the previous backup is how one bad backup destroys the good one.
func TestBackupRefusesToOverwriteAnExistingFile(t *testing.T) {
	db, path := backupDB(t)
	dest := filepath.Join(t.TempDir(), "existing.db")

	if err := os.WriteFile(dest, []byte("previous backup"), 0o600); err != nil {
		t.Fatalf("seed destination: %v", err)
	}

	_, err := store.Backup(context.Background(), db.SQL(), path, dest)
	if !errors.Is(err, store.ErrBackupExists) {
		t.Fatalf("err = %v, want store.ErrBackupExists", err)
	}

	// And the existing file must be untouched.
	content, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("read destination: %v", readErr)
	}
	if string(content) != "previous backup" {
		t.Error("the refused backup modified the existing file anyway")
	}
}

// Backing up onto the live database, or onto one of its sidecars, would
// destroy the thing being backed up.
func TestBackupRefusesTheLiveDatabaseAndItsSidecars(t *testing.T) {
	db, path := backupDB(t)

	for _, dest := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		t.Run(filepath.Base(dest), func(t *testing.T) {
			_, err := store.Backup(context.Background(), db.SQL(), path, dest)
			if !errors.Is(err, store.ErrBackupPathConflict) {
				t.Errorf("err = %v, want store.ErrBackupPathConflict", err)
			}
		})
	}
}

func TestBackupRefusesAnEmptyDestination(t *testing.T) {
	db, path := backupDB(t)

	if _, err := store.Backup(context.Background(), db.SQL(), path, "   "); err == nil {
		t.Error("an empty destination must be refused")
	}
}

// A backup carries the whole database. It must not be group- or world-readable.
func TestBackupIsCreatedOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Go synthesises a POSIX mode on Windows; every regular file reports
		// 0666 regardless of its ACL, so the assertion cannot mean anything.
		t.Skip("file modes are not POSIX on Windows")
	}

	db, path := backupDB(t)
	dest := filepath.Join(t.TempDir(), "perms.db")

	if _, err := store.Backup(context.Background(), db.SQL(), path, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup mode = %#o, want 0600; it holds container configuration and secret digests", perm)
	}
}

func TestBackupCreatesAMissingDestinationDirectory(t *testing.T) {
	db, path := backupDB(t)
	dest := filepath.Join(t.TempDir(), "nested", "deeper", "backup.db")

	if _, err := store.Backup(context.Background(), db.SQL(), path, dest); err != nil {
		t.Fatalf("Backup into a missing directory: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("the backup was not written: %v", err)
	}
}

// Verification must FAIL on a damaged copy. Without this the verification is
// decorative, and a backup that cannot be restored still reports success.
func TestVerifyBackupDetectsADamagedCopy(t *testing.T) {
	db, path := backupDB(t)
	dest := filepath.Join(t.TempDir(), "damaged.db")

	if _, err := store.Backup(context.Background(), db.SQL(), path, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	corruptPages(t, dest)

	verification, err := store.VerifyBackup(context.Background(), dest)
	if err != nil {
		// A comprehensively damaged file fails to open, which is also a
		// detection -- what must not happen is a clean verification.
		return
	}
	if verification.OK {
		t.Error("verification passed on a corrupted backup; the check is not checking anything")
	}
	if len(verification.Problems) == 0 {
		t.Error("a failed verification must say what is wrong")
	}
}

// A destination path containing a quote must be treated as a path, not as SQL.
//
// VACUUM INTO takes a bound parameter here rather than interpolated text; this
// is the test that would fail if someone "simplified" it to concatenation.
func TestBackupDestinationIsBoundNotInterpolated(t *testing.T) {
	db, path := backupDB(t)

	// A single quote is the character that matters: it terminates a SQL string
	// literal. It is a legal filename character on every platform HarborMaster
	// targets, so this test runs everywhere rather than skipping on the one
	// where a syntax error would be least likely to be noticed.
	dest := filepath.Join(t.TempDir(), `back'up.db`)

	result, err := store.Backup(context.Background(), db.SQL(), path, dest)
	if err != nil {
		t.Fatalf("a path containing quotes must be a path, not SQL: %v", err)
	}
	if !result.Verification.OK {
		t.Errorf("verification failed: %v", result.Verification.Problems)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("the file was not created under its literal name: %v", err)
	}
}

// A backup taken by an older build has fewer migrations. That is not damage,
// but an operator must be told before restoring it.
func TestVerifyBackupReportsAMismatchedSchemaHistory(t *testing.T) {
	db, path := backupDB(t)
	dest := filepath.Join(t.TempDir(), "older.db")

	if _, err := store.Backup(context.Background(), db.SQL(), path, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Make the copy look like it came from a build with one migration fewer.
	older, err := store.OpenWithOptions(context.Background(), store.Options{
		Path:      dest,
		Integrity: store.IntegrityOff,
	})
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	if _, err := older.SQL().Exec(
		`INSERT INTO schema_migrations (name, checksum) VALUES ('0099_future.sql', 'x')`); err != nil {
		t.Fatalf("add a migration record: %v", err)
	}
	if err := older.Close(); err != nil {
		t.Fatalf("close copy: %v", err)
	}

	verification, err := store.VerifyBackup(context.Background(), dest)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if verification.MigrationsMatch {
		t.Error("a differing schema history must not be reported as matching")
	}
	if verification.OK {
		t.Error("a schema-history mismatch must fail verification; it is what stops a bad restore")
	}
	if !strings.Contains(strings.Join(verification.Problems, " "), "migration") {
		t.Errorf("the problem must mention the schema history: %v", verification.Problems)
	}
}

func TestVerifyBackupRejectsAMissingFile(t *testing.T) {
	_, err := store.VerifyBackup(context.Background(),
		filepath.Join(t.TempDir(), "does-not-exist.db"))
	if err == nil {
		t.Error("verifying a file that does not exist must be an error")
	}
}

// A read-only open must never create the file it was asked to read, or a typo
// in a diagnose invocation would silently produce an empty database.
func TestOpenReadOnlyDoesNotCreateTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")

	if _, err := store.OpenReadOnly(context.Background(), path, time.Second); err == nil {
		t.Fatal("opening a missing database read-only must fail")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a read-only open created the database file")
	}
}

// Closing must checkpoint, so the next start has no log to replay and a file
// copy taken afterwards is complete.
func TestCloseCheckpointsTheWriteAheadLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := range 50 {
		createSnapshot(t, db, newSnapshot("container-a", checksumA[:62]+padHex(i)))
	}

	// Before the close, the log holds the writes.
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Skip("this platform checkpointed eagerly; the property under test cannot be observed")
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	info, err := os.Stat(path + "-wal")
	if err == nil && info.Size() > 0 {
		t.Errorf("the write-ahead log is still %d bytes after close; it was not checkpointed", info.Size())
	}
}
