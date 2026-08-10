//go:build !windows

package store_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Database file permissions, on a platform where mode bits are the access
// control.
//
// # What these assert, and why it is the outcome rather than the mechanism
//
// SQLite creates the database subject to the process umask, which on an
// ordinary host yields 0644 — world readable, holding every account's Argon2id
// verifier and every live session's keyed digest. Release testing found exactly
// that on a running deployment.
//
// The tests assert the resulting MODE rather than manipulating the umask to
// force the failing case. Umask is process-global and this package runs tests
// in parallel, so setting it would make unrelated tests flaky for no extra
// confidence: the contract is "the database ends up 0600", and that is what is
// checked. The tightening path specifically is covered by seeding a 0644 file
// and reopening, which needs no global state at all.
//
// permissions_windows_test.go asserts the other half of the contract: that on
// a platform where these bits are not enforced HarborMaster reports the check
// as skipped rather than claiming a restriction it did not make.

// permissionsOf returns a file's mode bits, failing the test if it is absent.
func permissionsOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", filepath.Base(path), err)
	}
	return info.Mode().Perm()
}

// openAt opens a database at path with the default options.
func openAt(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Base(path), err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// write forces a WAL frame so the sidecars exist.
func write(t *testing.T, db *store.DB) {
	t.Helper()
	if _, err := db.Events.Append(context.Background(), domain.Event{
		Type:     domain.EventServerStarted,
		Severity: domain.SeverityInfo,
		Message:  "started",
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}
}

// A database HarborMaster creates is owner-only.
func TestANewDatabaseIsCreatedOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbormaster.db")
	db := openAt(t, path)

	if got := permissionsOf(t, path); got != store.DatabaseFileMode {
		t.Fatalf("a new database is %#o, want %#o\n"+
			"\tit holds every account's password verifier and every live session's digest",
			got, store.DatabaseFileMode)
	}

	report := db.OpenReport().Permissions
	if !report.Enforced {
		t.Error("the permission pass reported itself unenforced on a POSIX platform")
	}
	if report.Mode != store.DatabaseFileMode {
		t.Errorf("reported mode = %#o, want %#o", report.Mode, store.DatabaseFileMode)
	}
}

// An existing database left world readable by an older HarborMaster is
// tightened when this build opens it.
//
// This is the upgrade path for every deployment that already exists.
func TestAnExistingWorldReadableDatabaseIsTightened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbormaster.db")

	first := openAt(t, path)
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Relaxed exactly as a pre-fix build left it.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("relax database: %v", err)
	}
	if got := permissionsOf(t, path); got != 0o644 {
		t.Fatalf("precondition failed: database is %#o, want 0644", got)
	}

	db := openAt(t, path)

	if got := permissionsOf(t, path); got != store.DatabaseFileMode {
		t.Fatalf("after opening a 0644 database it is %#o, want %#o", got, store.DatabaseFileMode)
	}
	if tightened := db.OpenReport().Permissions.Tightened; len(tightened) == 0 {
		t.Error("the report records nothing as tightened, so an operator is " +
			"never told their database had been exposed")
	}
}

// The -wal and -shm sidecars carry database pages, so they carry whatever the
// database carries.
func TestTheSidecarsAreOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbormaster.db")
	db := openAt(t, path)
	write(t, db)

	checked := 0
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := path + suffix
		info, err := os.Stat(sidecar)
		if errors.Is(err, fs.ErrNotExist) {
			// A sidecar exists only while a connection needs it. Absent is
			// fine; present and readable beyond the owner is not.
			continue
		}
		if err != nil {
			t.Fatalf("stat %s: %v", suffix, err)
		}
		checked++
		if got := info.Mode().Perm(); got != store.DatabaseFileMode {
			t.Errorf("%s is %#o, want %#o\n"+
				"\ta sidecar holds the same pages as the database",
				suffix, got, store.DatabaseFileMode)
		}
	}
	if checked == 0 {
		t.Skip("no sidecar was present to check")
	}
}

// A sidecar left behind world readable by an older build is tightened too.
func TestAnExistingWorldReadableSidecarIsTightened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbormaster.db")

	first := openAt(t, path)
	write(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A stale, world-readable WAL beside the database, as a crash under an
	// older build would leave one.
	stale := path + "-wal"
	if err := os.WriteFile(stale, []byte("stale wal"), 0o644); err != nil {
		t.Fatalf("seed stale wal: %v", err)
	}

	openAt(t, path)

	if got := permissionsOf(t, stale); got != store.DatabaseFileMode {
		t.Fatalf("a pre-existing world-readable -wal is %#o, want %#o",
			got, store.DatabaseFileMode)
	}
}

// A database that cannot be restricted stops the open rather than being served
// at an exposure HarborMaster could not establish.
func TestADatabaseReachedThroughASymlinkRefusesToOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harbormaster.db")

	target := filepath.Join(dir, "elsewhere.db")
	if err := os.WriteFile(target, []byte{}, 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := store.Open(context.Background(), path)
	if err == nil {
		t.Fatal("a database reached through a symlink was opened\n" +
			"\tos.Chmod follows symlinks, so honouring one lets whoever planted it " +
			"choose which file HarborMaster changes the mode of")
	}
	if !errors.Is(err, store.ErrPermissionsUnenforceable) {
		t.Errorf("err = %v, want ErrPermissionsUnenforceable", err)
	}
}

// Restricting is idempotent: a second start finds nothing to do.
func TestASecondOpenChangesNoPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbormaster.db")
	first := openAt(t, path)
	write(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := openAt(t, path)
	if tightened := second.OpenReport().Permissions.Tightened; len(tightened) != 0 {
		t.Errorf("a second open tightened %v; the first should have left nothing to do", tightened)
	}
}

// A backup is owner-only. Asserted beside the database because it is the same
// exposure: a backup is a complete copy of it.
func TestABackupIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harbormaster.db")
	db := openAt(t, path)
	write(t, db)

	dest := filepath.Join(dir, "backup", "copy.db")
	if _, err := store.Backup(context.Background(), db.SQL(), path, dest); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if got := permissionsOf(t, dest); got != store.DatabaseFileMode {
		t.Fatalf("backup is %#o, want %#o", got, store.DatabaseFileMode)
	}
}
