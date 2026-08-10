package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Filesystem permissions on the database and the files SQLite keeps beside it.
//
// # What is in these files
//
// The database holds the Argon2id verifier for every account, the keyed digests
// of every live session and of the one-time bootstrap token, the security audit
// log, and a description of every container on the host including the digests
// of its secret environment values. No plaintext secret is in there by
// invariant, but a verifier is still the thing an offline cracker wants, and a
// session digest is the thing a replay wants.
//
// SQLite creates the database subject to the process umask, which on an
// ordinary host yields 0644 — readable by every account on the machine. That is
// what this file exists to correct.
//
// # Why tightening the main database is most of the work
//
// SQLite does not create the -wal, -shm, and -journal sidecars with the umask.
// It derives their mode from the MAIN DATABASE FILE, so a database held at 0600
// produces sidecars at 0600 without anything here touching them. The sidecars
// are still tightened explicitly, because a database created by an older
// HarborMaster may already have 0644 sidecars sitting next to it when this
// runs, and because relying on an implementation detail of SQLite without
// checking it would be relying on it.
//
// # Only HarborMaster's own files
//
// Exactly the configured database path and the three sidecar names SQLite
// derives from it. The parent directory is not touched: it may be a mount point
// or a directory the operator shares with something else, and its mode is
// reported by `harbormaster diagnose` rather than changed.

// DatabaseFileMode is the mode HarborMaster holds its database and sidecars at.
//
// Owner only. Group is excluded deliberately: the container runs as a single
// unprivileged user, and a group-readable database is readable by every process
// that happens to share that group.
const DatabaseFileMode fs.FileMode = 0o600

// databaseSidecarSuffixes are the files SQLite creates beside the database.
//
// The same list backup.go refuses as a backup destination, for the same reason:
// these carry database pages, so they carry whatever the database carries.
var databaseSidecarSuffixes = []string{"-wal", "-shm", "-journal"}

// ErrPermissionsUnenforceable reports that a security-relevant file could not be
// restricted to its owner.
//
// Returned rather than logged. A database whose permissions could not be
// established is one whose exposure is unknown, and starting anyway would mean
// serving password verifiers out of a file that may be world readable while
// reporting nothing.
var ErrPermissionsUnenforceable = errors.New("database permissions could not be restricted to the owner")

// PermissionReport describes what securing the database files did.
//
// Held on OpenReport so an operator can see, and a test can assert, that the
// step ran rather than assuming it from the absence of an error.
type PermissionReport struct {
	// Enforced reports that this platform's mode bits are the real access
	// control. False on Windows -- see permissions_windows.go.
	Enforced bool
	// Mode is the database's mode after the pass. Zero when not enforced.
	Mode fs.FileMode
	// Tightened lists the files whose mode this pass had to change, by suffix
	// ("" for the database itself). Empty when everything was already correct,
	// which is the steady state after the first start.
	Tightened []string
}

// secureDatabaseFiles restricts the database and its sidecars to their owner.
//
// Called once the database file exists and before any caller can read from it.
// A file that is absent is skipped: the sidecars exist only while a connection
// or an unfinished transaction needs them.
//
// On a platform where mode bits are not the access control this does nothing
// and says so, rather than reporting a restriction it did not make.
func secureDatabaseFiles(path string) (PermissionReport, error) {
	report := PermissionReport{Enforced: posixPermissionsEnforced()}
	if !report.Enforced {
		return report, nil
	}

	// The database first: SQLite derives the sidecars' mode from it, so
	// tightening it is what keeps FUTURE sidecars restricted.
	tightened, err := restrictFile(path, true)
	if err != nil {
		return report, err
	}
	if tightened {
		report.Tightened = append(report.Tightened, "")
	}

	for _, suffix := range databaseSidecarSuffixes {
		tightened, err := restrictFile(path+suffix, false)
		if err != nil {
			return report, err
		}
		if tightened {
			report.Tightened = append(report.Tightened, suffix)
		}
	}

	if info, statErr := os.Stat(path); statErr == nil {
		report.Mode = info.Mode().Perm()
	}
	return report, nil
}

// restrictFile brings one file down to DatabaseFileMode, reporting whether it
// had to change anything.
//
// `required` distinguishes the database, whose absence here is a bug in the
// caller, from a sidecar that legitimately may not exist.
//
// Only ever narrows. The mode is compared first so a correctly-permissioned
// file costs a stat and no write, which matters because this runs on every
// start.
func restrictFile(path string, required bool) (bool, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if required {
			return false, fmt.Errorf("%w: %s is missing", ErrPermissionsUnenforceable, describePath(path))
		}
		return false, nil
	case err != nil:
		return false, fmt.Errorf("%w: cannot inspect %s: %w",
			ErrPermissionsUnenforceable, describePath(path), err)
	}

	// A symlink is not followed and not chmod'ed. Following one would let
	// whoever planted it choose which file HarborMaster relaxes or tightens,
	// and os.Chmod follows symlinks.
	if info.Mode()&fs.ModeSymlink != 0 {
		return false, fmt.Errorf("%w: %s is a symbolic link", ErrPermissionsUnenforceable, describePath(path))
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%w: %s is not a regular file", ErrPermissionsUnenforceable, describePath(path))
	}

	if info.Mode().Perm()&^DatabaseFileMode == 0 {
		return false, nil
	}
	if err := os.Chmod(path, DatabaseFileMode); err != nil {
		return false, fmt.Errorf("%w: cannot restrict %s: %w",
			ErrPermissionsUnenforceable, describePath(path), err)
	}
	return true, nil
}

// describePath names a file for an error message.
//
// The base name only. These errors reach an operator's console and a log line,
// and the full path is the operator's own configuration rather than something
// worth repeating into every message.
func describePath(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[i+1:]
		}
	}
	return path
}
