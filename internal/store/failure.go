package store

import (
	"errors"
	"strings"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Persistence failure classification.
//
// SQLite reports "the disk is full", "the filesystem is read-only", "another
// process holds the write lock", and "this file is not a database" as ordinary
// errors that read, to a caller, exactly like a bug in the query. They are not
// bugs, they are operating conditions, and each one has a DIFFERENT remedy:
//
//	corrupt    restore from a backup; the file cannot be repaired in place
//	disk_full  free space or grow the volume
//	read_only  fix the mount or the file mode
//	busy       another writer holds the lock; retry or stop the other process
//	permission fix ownership on the data directory
//
// Classifying them here means the answer is computed once, from the SQLite
// result code rather than from message text, and every caller -- startup,
// the diagnose command, the log -- reports the same verdict.

// FailureKind names a persistence failure by the remedy it implies.
type FailureKind string

const (
	// FailureNone means no failure was classified.
	FailureNone FailureKind = ""
	// FailureCorrupt means the database image is malformed. Not recoverable in
	// place: the remedy is a restore.
	FailureCorrupt FailureKind = "corrupt"
	// FailureDiskFull means the volume has no room for the write.
	FailureDiskFull FailureKind = "disk_full"
	// FailureReadOnly means the database or its directory cannot be written.
	FailureReadOnly FailureKind = "read_only"
	// FailureBusy means another writer holds the lock.
	FailureBusy FailureKind = "busy"
	// FailurePermission means the process lacks permission on the file.
	FailurePermission FailureKind = "permission"
	// FailureIO means the underlying storage reported an I/O error.
	FailureIO FailureKind = "io"
	// FailureCantOpen means the file could not be opened at all: a missing
	// directory, a hot write-ahead log a read-only connection cannot recover,
	// or a path that is not a file.
	FailureCantOpen FailureKind = "cannot_open"
	// FailureUnknown means the error came from SQLite but is not one of the
	// operating conditions above.
	FailureUnknown FailureKind = "unknown"
)

// Sentinel errors, so a caller can branch with errors.Is rather than by
// comparing strings. Each Classify-able condition has exactly one.
var (
	// ErrCorrupt reports a malformed database image.
	ErrCorrupt = errors.New("database image is malformed")
	// ErrDiskFull reports that the volume holding the database is full.
	ErrDiskFull = errors.New("no space left for the database")
	// ErrReadOnly reports that the database cannot be written.
	ErrReadOnly = errors.New("database is read-only")
	// ErrBusy reports that another writer holds the database lock.
	ErrBusy = errors.New("database is locked by another writer")
	// ErrPermission reports that the process cannot access the database file.
	ErrPermission = errors.New("database permissions deny access")
	// ErrIO reports a storage-level I/O failure.
	ErrIO = errors.New("database i/o error")
	// ErrCantOpen reports that the database file could not be opened.
	ErrCantOpen = errors.New("database file could not be opened")
)

// Classify maps an error to the operating condition it represents.
//
// It reads the SQLite result code, not the message: message text is not part
// of SQLite's compatibility contract and varies between versions, while the
// primary result code has been stable for the life of the library. The
// extended code is masked down to its primary code, so SQLITE_IOERR_WRITE and
// SQLITE_READONLY_DBMOVED classify with their families rather than falling
// through to unknown.
//
// A nil error, or one that did not come from SQLite, classifies as
// FailureNone -- callers must not read a verdict into an error this package
// cannot speak for.
func Classify(err error) FailureKind {
	if err == nil {
		return FailureNone
	}

	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		// Not a driver error. The one message-level case worth catching is the
		// header check the driver performs before SQLite is reached, which is
		// how a truncated or overwritten file usually presents.
		if isNotADatabaseMessage(err) {
			return FailureCorrupt
		}
		return FailureNone
	}

	// The low byte is the primary result code; the high bytes carry the
	// extended code, which refines the reason without changing the family.
	switch sqliteErr.Code() & 0xff {
	case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
		return FailureCorrupt
	case sqlite3.SQLITE_FULL:
		return FailureDiskFull
	case sqlite3.SQLITE_READONLY:
		return FailureReadOnly
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED, sqlite3.SQLITE_PROTOCOL:
		return FailureBusy
	case sqlite3.SQLITE_PERM, sqlite3.SQLITE_AUTH:
		return FailurePermission
	case sqlite3.SQLITE_IOERR:
		return FailureIO
	case sqlite3.SQLITE_CANTOPEN:
		return FailureCantOpen
	default:
		return FailureUnknown
	}
}

// isUniqueViolation reports whether an error is a uniqueness constraint
// refusing a write.
//
// Classified from the SQLite result code rather than from message text, for the
// same reason everything else in this file is: message text is a driver
// implementation detail, and a check built on it breaks quietly on an upgrade.
//
// This is not a failure in the sense the rest of this file means. A unique
// index doing its job is frequently the CORRECT outcome of a race -- two
// requests to acquire the same image arrive together, one inserts, and the
// other must be told that work is already in progress rather than being handed
// an internal error.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}

	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	// The extended code distinguishes which constraint fired; the primary code
	// says only that one did. Both spellings are accepted because a driver is
	// entitled to report either.
	switch sqliteErr.Code() {
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		return true
	}
	return sqliteErr.Code()&0xff == sqlite3.SQLITE_CONSTRAINT
}

// isNotADatabaseMessage catches the driver's own pre-SQLite header rejection.
//
// A message check is a last resort and is deliberately narrow: it matches only
// the two fixed phrases SQLite uses for a file whose header is not a database
// header, which is the shape a truncated or partially overwritten file takes.
// Everything else is classified from the result code.
func isNotADatabaseMessage(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "file is not a database") ||
		strings.Contains(message, "file is encrypted or is not a database")
}

// AsError pairs a classified failure with the sentinel a caller can match.
//
// The original error is wrapped rather than replaced, so a log still carries
// the driver's detail while errors.Is answers the question a caller actually
// asks: is this recoverable, and by whom.
func AsError(err error) error {
	if err == nil {
		return nil
	}
	sentinel := SentinelFor(Classify(err))
	if sentinel == nil {
		return err
	}
	return &classifiedError{kind: Classify(err), sentinel: sentinel, cause: err}
}

// SentinelFor returns the sentinel error for a failure kind, or nil for a kind
// that has none.
func SentinelFor(kind FailureKind) error {
	switch kind {
	case FailureCorrupt:
		return ErrCorrupt
	case FailureDiskFull:
		return ErrDiskFull
	case FailureReadOnly:
		return ErrReadOnly
	case FailureBusy:
		return ErrBusy
	case FailurePermission:
		return ErrPermission
	case FailureIO:
		return ErrIO
	case FailureCantOpen:
		return ErrCantOpen
	default:
		return nil
	}
}

// classifiedError carries both the sentinel and the underlying driver error,
// so errors.Is matches either one.
type classifiedError struct {
	kind     FailureKind
	sentinel error
	cause    error
}

func (e *classifiedError) Error() string {
	return e.sentinel.Error() + ": " + e.cause.Error()
}

// Unwrap returns both the sentinel and the cause so errors.Is matches either.
func (e *classifiedError) Unwrap() []error { return []error{e.sentinel, e.cause} }

// Kind reports the classification, for callers holding the concrete type.
func (e *classifiedError) Kind() FailureKind { return e.kind }

// Remedy returns the operator action a failure kind calls for.
//
// Prose about state, never a path and never a driver message: this string is
// safe to print from the diagnose command and to put in a log, and it must
// stay that way if it is ever surfaced anywhere less private.
func Remedy(kind FailureKind) string {
	switch kind {
	case FailureCorrupt:
		return "the database image is malformed; restore the most recent verified backup, " +
			"as a corrupt SQLite file cannot be repaired in place"
	case FailureDiskFull:
		return "the volume holding the database is full; free space or grow the volume, then restart"
	case FailureReadOnly:
		return "the database or its directory is not writable; check the mount options and the file mode"
	case FailureBusy:
		return "another process holds the database write lock; stop the other writer or raise the busy timeout"
	case FailurePermission:
		return "the process cannot access the database file; check ownership of the data directory"
	case FailureIO:
		return "the storage layer reported an I/O error; check the volume and the host's kernel log"
	case FailureCantOpen:
		return "the database file could not be opened; check that the path exists and the directory is writable"
	case FailureUnknown:
		return "the database reported an error that does not match a known operating condition; see the log"
	default:
		return ""
	}
}

// IsFatal reports whether a failure kind means HarborMaster must not continue.
//
// Corruption is the only one. Everything else is a condition that can clear:
// a full disk gets space, a busy lock is released, a read-only mount is
// remounted. Corruption does not clear, and continuing on a malformed image
// risks writing more damage over the operator's history.
func IsFatal(kind FailureKind) bool { return kind == FailureCorrupt }
