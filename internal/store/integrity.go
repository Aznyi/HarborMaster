package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Startup integrity validation.
//
// SQLite does not notice that a database is damaged until it reads the damaged
// page. Without a check at open, a file corrupted by a power loss, a full
// disk, or a filesystem fault surfaces hours later as an inexplicable query
// error in the middle of a refresh -- at which point the operator has no idea
// when the damage happened or which backup predates it.
//
// So the check runs once, at open, before anything writes.
//
// QUICK BY DEFAULT, and the distinction matters. `integrity_check` reads every
// page and validates every index; on a large database that is minutes of I/O
// on the startup path. `quick_check` skips the index-content verification and
// is the cheaper check that still catches the damage that actually happens:
// malformed headers, truncated files, broken page links, corrupt B-tree
// structure.
//
// FAIL CLOSED ON DAMAGE, DEGRADE ON UNCERTAINTY. Detected corruption refuses
// the open, because writing further history over a malformed image destroys
// the backup window an operator still has. A check that could not COMPLETE --
// cancelled, or past its timeout -- is reported as incomplete and does not
// refuse: a false-positive outage on a large but healthy database is worse
// than a late diagnosis, and every other reliability control still applies.

// IntegrityMode selects how thoroughly a database is validated at open.
type IntegrityMode string

const (
	// IntegrityOff performs no check. Supported for an operator who runs the
	// check out of band and cannot afford it on the startup path.
	//
	// It does NOT make a damaged database openable. SQLite refuses a malformed
	// image on its own, before this check would run; what "off" governs is
	// whether HarborMaster looks for damage proactively, not whether the
	// engine's own refusal is honoured.
	IntegrityOff IntegrityMode = "off"
	// IntegrityQuick runs PRAGMA quick_check. The default.
	IntegrityQuick IntegrityMode = "quick"
	// IntegrityFull runs PRAGMA integrity_check plus PRAGMA foreign_key_check.
	IntegrityFull IntegrityMode = "full"
)

// ValidIntegrityMode reports whether mode is one of the three understood
// values. Configuration uses it so an unrecognised setting is a startup error
// rather than a silent fallback that skips the check.
func ValidIntegrityMode(mode IntegrityMode) bool {
	switch mode {
	case IntegrityOff, IntegrityQuick, IntegrityFull:
		return true
	default:
		return false
	}
}

// maxIntegrityProblems bounds how many problems SQLite reports and how many
// this package retains.
//
// SQLite's check pragmas accept a limit and stop there, which matters: a
// thoroughly destroyed file can otherwise produce a row per damaged page, and
// the point of the report is to tell an operator the database is unusable, not
// to enumerate every way in which it is.
const maxIntegrityProblems = 20

// Literal SQL. The problem limit is baked in as a constant rather than
// formatted in, because PRAGMA arguments cannot be bound parameters and a
// formatted PRAGMA is a string-built query no matter how trusted the value.
const (
	quickCheckSQL     = `PRAGMA quick_check(20)`
	fullCheckSQL      = `PRAGMA integrity_check(20)`
	foreignKeyCheck   = `PRAGMA foreign_key_check`
	journalModeQuery  = `PRAGMA journal_mode`
	pageCountQuery    = `PRAGMA page_count`
	pageSizeQuery     = `PRAGMA page_size`
	freelistQuery     = `PRAGMA freelist_count`
	userVersionQuery  = `PRAGMA user_version`
	foreignKeysQuery  = `PRAGMA foreign_keys`
	busyTimeoutQuery  = `PRAGMA busy_timeout`
	synchronousQuery  = `PRAGMA synchronous`
	encodingQuery     = `PRAGMA encoding`
	walAutoCheckpoint = `PRAGMA wal_autocheckpoint`
)

// A compile-time assertion that the limit written into the pragma text above
// and the limit this package enforces are the same number. PRAGMA arguments
// cannot be bound, so the two are necessarily written twice; a negative array
// length is a compile error, so they cannot drift apart silently.
var (
	_ [maxIntegrityProblems - 20]struct{}
	_ [20 - maxIntegrityProblems]struct{}
)

// IntegrityReport is the outcome of one validation pass.
type IntegrityReport struct {
	// Mode is the check that was requested.
	Mode IntegrityMode
	// OK reports that the check ran to completion and found nothing wrong. It
	// is false both for a damaged database and for a check that could not
	// finish, so a caller must consult Incomplete before concluding damage.
	OK bool
	// Incomplete reports that the check was cancelled or timed out. Nothing
	// can be concluded about the database from an incomplete check.
	Incomplete bool
	// Problems are the lines SQLite reported, capped at maxIntegrityProblems.
	Problems []string
	// Truncated reports that SQLite hit its own reporting limit, so there are
	// more problems than are listed.
	Truncated bool
	// ForeignKeyViolations counts rows PRAGMA foreign_key_check returned. Only
	// populated in full mode.
	ForeignKeyViolations int
	// Duration is how long the check took, for an operator deciding whether
	// the full check is affordable on their database.
	Duration time.Duration
}

// Damaged reports whether the check positively established corruption.
//
// Distinct from !OK, which is also true for a check that could not finish. The
// decision to refuse an open must be made on damage that was OBSERVED, never
// on the absence of evidence.
func (r IntegrityReport) Damaged() bool {
	return !r.OK && !r.Incomplete
}

// Summary renders a one-line verdict for a log or the diagnose command.
//
// It reports counts and the mode, never a file path, so it is safe wherever it
// is printed.
func (r IntegrityReport) Summary() string {
	switch {
	case r.Mode == IntegrityOff:
		return "integrity check disabled by configuration"
	case r.Incomplete:
		return fmt.Sprintf("%s integrity check did not complete after %s", r.Mode, r.Duration.Round(time.Microsecond))
	case r.OK:
		return fmt.Sprintf("%s integrity check passed in %s", r.Mode, r.Duration.Round(time.Microsecond))
	default:
		suffix := ""
		if r.Truncated {
			suffix = " (reporting limit reached; there may be more)"
		}
		return fmt.Sprintf("%s integrity check found %d problem(s)%s",
			r.Mode, len(r.Problems), suffix)
	}
}

// CheckIntegrity validates the database and returns what it found.
//
// It returns an error only when the check could not be ATTEMPTED -- a database
// handle that will not answer at all. A database that answers and reports
// damage is a successful check with a negative result, and is reported through
// the report rather than through an error, because the caller's decision
// depends on which of the two happened.
func CheckIntegrity(ctx context.Context, db *sql.DB, mode IntegrityMode, timeout time.Duration) (IntegrityReport, error) {
	report := IntegrityReport{Mode: mode}
	if mode == IntegrityOff {
		report.OK = true
		return report, nil
	}
	if !ValidIntegrityMode(mode) {
		return report, fmt.Errorf("unknown integrity mode %q", mode)
	}

	// The check is bounded whether or not the caller bounded its own context.
	// An unbounded integrity_check on a damaged file can read for a very long
	// time, and this runs on the startup path.
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	query := quickCheckSQL
	if mode == IntegrityFull {
		query = fullCheckSQL
	}

	started := time.Now()
	problems, truncated, err := runCheckPragma(ctx, db, query)
	report.Duration = time.Since(started)

	if err != nil {
		if incompleteCheck(ctx, err) {
			report.Incomplete = true
			return report, nil
		}
		// A corrupt file can fail the pragma itself rather than answering it.
		// That is a positive finding, not an inability to check.
		if Classify(err) == FailureCorrupt {
			report.Problems = []string{"database image is malformed"}
			return report, nil
		}
		return report, fmt.Errorf("run %s: %w", mode, AsError(err))
	}

	report.Problems = problems
	report.Truncated = truncated

	if mode == IntegrityFull && len(problems) == 0 {
		violations, fkErr := countForeignKeyViolations(ctx, db)
		if fkErr != nil {
			if incompleteCheck(ctx, fkErr) {
				report.Incomplete = true
				return report, nil
			}
			return report, fmt.Errorf("run foreign key check: %w", AsError(fkErr))
		}
		report.ForeignKeyViolations = violations
		if violations > 0 {
			report.Problems = append(report.Problems,
				fmt.Sprintf("%d foreign key violation(s)", violations))
		}
		report.Duration = time.Since(started)
	}

	report.OK = len(report.Problems) == 0
	return report, nil
}

// incompleteCheck reports whether an error means the check was cut short
// rather than that it found something.
func incompleteCheck(ctx context.Context, err error) bool {
	return ctx.Err() != nil ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}

// runCheckPragma executes a check pragma and collects what it reported.
//
// SQLite answers a healthy database with the single row "ok"; anything else is
// a problem description. The row count is capped independently of the pragma's
// own limit, so a driver that ignored the limit still cannot make this
// function allocate without bound.
func runCheckPragma(ctx context.Context, db *sql.DB, query string) (problems []string, truncated bool, err error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var line string
		if scanErr := rows.Scan(&line); scanErr != nil {
			return nil, false, scanErr
		}
		if line == "ok" {
			continue
		}
		if len(problems) >= maxIntegrityProblems {
			truncated = true
			break
		}
		problems = append(problems, line)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return problems, truncated, nil
}

// countForeignKeyViolations counts the rows PRAGMA foreign_key_check returns.
//
// The rows themselves are deliberately discarded: they name tables and rowids,
// which is more detail than a health report needs, and the count is what tells
// an operator whether the referential graph is intact. Counting is bounded so
// a comprehensively broken database cannot make this loop expensive.
func countForeignKeyViolations(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx, foreignKeyCheck)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	count := 0
	for rows.Next() {
		count++
		if count >= maxIntegrityProblems {
			break
		}
	}
	return count, rows.Err()
}

// JournalMode reports the journal mode the connection actually has.
//
// Asking is not redundant with requesting WAL in the DSN. SQLite silently
// falls back to a rollback journal when the file lives on a filesystem that
// cannot support WAL's shared-memory index -- several network filesystems, and
// some container volume drivers. The durability and concurrency profile then
// differs from the one every comment in this package assumes, with no signal
// at all unless something asks.
func JournalMode(ctx context.Context, db *sql.DB) (string, error) {
	var mode string
	if err := db.QueryRowContext(ctx, journalModeQuery).Scan(&mode); err != nil {
		return "", AsError(err)
	}
	return mode, nil
}

// Stats are the storage-level numbers the diagnose command reports.
//
// Every field is a count or a fixed vocabulary word. None of them is derived
// from container data, so the whole struct is safe to print.
type Stats struct {
	JournalMode       string
	PageSize          int64
	PageCount         int64
	FreelistCount     int64
	SizeBytes         int64
	Encoding          string
	ForeignKeysOn     bool
	BusyTimeoutMS     int64
	Synchronous       int64
	UserVersion       int64
	WALAutocheckpoint int64
}

// ReadStats collects the storage-level numbers.
//
// A failure on any single pragma is fatal to the whole call rather than being
// papered over with a zero: a diagnostic that silently reports 0 pages for a
// database it could not read would be worse than one that says it failed.
func ReadStats(ctx context.Context, db *sql.DB) (Stats, error) {
	var stats Stats

	if err := db.QueryRowContext(ctx, journalModeQuery).Scan(&stats.JournalMode); err != nil {
		return stats, fmt.Errorf("read journal mode: %w", AsError(err))
	}
	if err := db.QueryRowContext(ctx, encodingQuery).Scan(&stats.Encoding); err != nil {
		return stats, fmt.Errorf("read encoding: %w", AsError(err))
	}

	for _, field := range []struct {
		query string
		name  string
		into  *int64
	}{
		{pageSizeQuery, "page size", &stats.PageSize},
		{pageCountQuery, "page count", &stats.PageCount},
		{freelistQuery, "freelist count", &stats.FreelistCount},
		{busyTimeoutQuery, "busy timeout", &stats.BusyTimeoutMS},
		{synchronousQuery, "synchronous", &stats.Synchronous},
		{userVersionQuery, "user version", &stats.UserVersion},
		{walAutoCheckpoint, "wal autocheckpoint", &stats.WALAutocheckpoint},
	} {
		if err := db.QueryRowContext(ctx, field.query).Scan(field.into); err != nil {
			return stats, fmt.Errorf("read %s: %w", field.name, AsError(err))
		}
	}

	var foreignKeys int64
	if err := db.QueryRowContext(ctx, foreignKeysQuery).Scan(&foreignKeys); err != nil {
		return stats, fmt.Errorf("read foreign keys pragma: %w", AsError(err))
	}
	stats.ForeignKeysOn = foreignKeys != 0
	stats.SizeBytes = stats.PageSize * stats.PageCount

	return stats, nil
}
