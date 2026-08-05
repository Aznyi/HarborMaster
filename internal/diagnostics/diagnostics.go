// Package diagnostics implements `harbormaster diagnose` and
// `harbormaster backup`.
//
// WHY THIS IS A COMMAND AND NOT AN ENDPOINT.
//
// Everything below -- filesystem paths, free space, journal mode, page counts,
// schema history, when the daemon was last reachable -- is exactly the
// reconnaissance an attacker wants and exactly the information an operator
// needs. HarborMaster's API is unauthenticated by design in this phase, so an
// endpoint serving this would hand host layout to anything that can reach the
// port. A command requires shell access to the container or the host, which is
// a privilege an operator already has and an attacker's HTTP request does not.
//
// So the rule is: no diagnostic route, now or later, until authentication
// exists. The `harbormaster healthcheck` command remains the machine-readable
// surface, and it deliberately reports far less.
//
// WHAT IT MAY PRINT. Counts, fixed-vocabulary states, timestamps, sizes,
// modes, and the operator's own configured paths. Never a row's contents,
// never an environment variable's value, never a secret digest, never a raw
// Docker error. The report is a description of HarborMaster's storage, not of
// what is stored in it.
package diagnostics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/store"
	"github.com/Aznyi/HarborMaster/internal/version"
)

// Exit codes. Distinct values so a script can tell "HarborMaster is fine" from
// "HarborMaster has a problem" from "the diagnosis itself could not run".
const (
	// ExitOK means every check passed.
	ExitOK = 0
	// ExitFindings means the diagnosis completed and found problems.
	ExitFindings = 1
	// ExitFailed means the diagnosis could not be performed.
	ExitFailed = 2
)

// Severity ranks a finding.
type Severity string

const (
	// SeverityInfo is an observation worth printing, not a problem.
	SeverityInfo Severity = "info"
	// SeverityWarning is a condition that will become a problem.
	SeverityWarning Severity = "warning"
	// SeverityCritical is a condition that needs action now.
	SeverityCritical Severity = "critical"
)

// Finding is one thing the diagnosis established.
type Finding struct {
	Severity Severity
	// Summary states the condition in one line.
	Summary string
	// Remedy states what to do about it. Empty for informational findings.
	Remedy string
}

// Report is the whole diagnosis.
type Report struct {
	Build      version.Info
	DatabaseAt string

	// File describes the database file as the filesystem sees it. Populated
	// even when the database itself cannot be opened, which is the case an
	// operator most needs answered.
	File FileReport

	// Opened reports whether the database could be read. Everything below it
	// is populated only when it is true.
	Opened bool
	// OpenError explains why not.
	OpenError string

	Stats      store.Stats
	Integrity  store.IntegrityReport
	Migrations MigrationReport
	Engine     EngineReport
	Inventory  InventoryReport
	Counts     map[string]int64

	Findings []Finding
}

// FileReport is what stat can say without opening the database.
type FileReport struct {
	Exists bool
	// SizeBytes is the main database file. WAL and SHM are reported
	// separately, because a large WAL with a small database is itself a
	// finding: it means checkpointing is not happening.
	SizeBytes    int64
	Mode         os.FileMode
	ModifiedAt   time.Time
	WALExists    bool
	WALSizeBytes int64
	SHMExists    bool
	// DirectoryWritable reports whether new files can be created alongside the
	// database. SQLite needs this even to READ a WAL database, which is a
	// failure mode that otherwise reads as inexplicable.
	DirectoryWritable bool
	DirectoryMode     os.FileMode
}

// MigrationReport compares the database's schema history to this build's.
type MigrationReport struct {
	Applied  []string
	Expected []string
	// Missing are migrations this build has that the database does not.
	Missing []string
	// Unknown are migrations the database has that this build does not, which
	// means a newer HarborMaster wrote it.
	Unknown []string
}

// EngineReport is the event engine's persisted state.
type EngineReport struct {
	Present            bool
	LastConnectedAt    string
	LastDisconnectedAt string
	LastEventAt        string
	LastReconciledAt   string
	ReconnectCount     int64
}

// InventoryReport is the last thing the inventory engine recorded.
type InventoryReport struct {
	Present     bool
	Generation  int64
	LastTrigger string
	LastState   string
	LastStarted string
	// RunningRefreshes counts refresh rows still marked running. A nonzero
	// count on a stopped HarborMaster means a process died mid-refresh.
	RunningRefreshes int64
}

// walSizeWarning is the WAL size past which checkpointing is presumed stuck.
//
// SQLite checkpoints automatically at roughly 1000 pages, which at the default
// page size is about 4 MiB. A log an order of magnitude past that means
// something is holding a read transaction open indefinitely, and the database
// file is no longer where the data is.
const walSizeWarning = 64 << 20 // 64 MiB

// diagnoseTimeout bounds the whole diagnosis, integrity check included.
const diagnoseTimeout = 5 * time.Minute

// Diagnose inspects HarborMaster's storage and returns what it found.
//
// It NEVER opens a Docker connection. A diagnostic that talks to a privileged
// socket to answer a question about a database file would be adding a
// capability for the sake of a report, and the question "is Docker reachable"
// already has an answer in `harbormaster healthcheck`.
//
// It never writes, and it never migrates. The database is opened read-only, so
// running this against a live server, a stopped one, or a copy is safe and
// produces the same answer.
func Diagnose(ctx context.Context, cfg config.Config) Report {
	ctx, cancel := context.WithTimeout(ctx, diagnoseTimeout)
	defer cancel()

	report := Report{
		Build:      version.Get(),
		DatabaseAt: cfg.Store.Path,
		Counts:     make(map[string]int64),
		Findings:   make([]Finding, 0),
	}

	report.File = inspectFile(cfg.Store.Path)
	report.Findings = append(report.Findings, fileFindings(report.File)...)

	if !report.File.Exists {
		// Not necessarily wrong: a HarborMaster that has never started has no
		// database. Reported as information, and there is nothing more to do.
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityInfo,
			Summary:  "no database file exists yet",
			Remedy:   "this is expected before the first start; HarborMaster creates it",
		})
		return report
	}

	db, err := store.OpenReadOnly(ctx, cfg.Store.Path, cfg.Store.BusyTimeout)
	if err != nil {
		report.OpenError = err.Error()
		report.Findings = append(report.Findings, openFailureFinding(err, report.File))
		return report
	}
	defer func() { _ = db.Close() }()
	report.Opened = true

	report.Stats, err = store.ReadStats(ctx, db)
	if err != nil {
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityWarning,
			Summary:  "storage statistics could not be read",
			Remedy:   "the database opened but did not answer; check the log for the driver error",
		})
	} else {
		report.Findings = append(report.Findings, statsFindings(report.Stats)...)
	}

	// The FULL check here, unlike at startup. Nothing is waiting on a
	// diagnosis, and an operator running it is asking the expensive question.
	report.Integrity, err = store.CheckIntegrity(ctx, db, store.IntegrityFull, 0)
	switch {
	case err != nil:
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityWarning,
			Summary:  "integrity check could not be run",
			Remedy:   "the database opened but refused the check; treat it as suspect and take a backup",
		})
	case report.Integrity.Damaged():
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityCritical,
			Summary:  report.Integrity.Summary(),
			Remedy:   store.Remedy(store.FailureCorrupt),
		})
	case report.Integrity.Incomplete:
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityWarning,
			Summary:  report.Integrity.Summary(),
			Remedy:   "re-run when the host is quieter; nothing can be concluded from an incomplete check",
		})
	}

	report.Migrations = inspectMigrations(ctx, db)
	report.Findings = append(report.Findings, migrationFindings(report.Migrations)...)

	report.Engine = inspectEngine(ctx, db)
	report.Inventory = inspectInventory(ctx, db)
	report.Findings = append(report.Findings, inventoryFindings(report.Inventory)...)

	report.Counts = countRows(ctx, db)

	return report
}

// Worst returns the highest severity in the report.
func (r Report) Worst() Severity {
	worst := SeverityInfo
	for _, finding := range r.Findings {
		switch finding.Severity {
		case SeverityCritical:
			return SeverityCritical
		case SeverityWarning:
			worst = SeverityWarning
		}
	}
	return worst
}

// ExitCode maps the report onto a process exit code.
//
// A warning is a finding, not a failure of the diagnosis, so it exits 1 the
// same as a critical would: both mean "something needs attention", which is
// what a monitoring script keys on.
//
// A database that could not be OPENED is still a successful diagnosis when it
// produced a verdict -- "this file is corrupt, restore a backup" is precisely
// the answer the operator ran the command for, and reporting it as "could not
// run" would tell them to look at the diagnostic instead of at the database.
// ExitFailed is therefore reserved for the case where the file is present and
// nothing at all could be established about it.
func (r Report) ExitCode() int {
	switch r.Worst() {
	case SeverityCritical, SeverityWarning:
		return ExitFindings
	}
	if !r.Opened && r.File.Exists {
		return ExitFailed
	}
	return ExitOK
}

// inspectFile stats the database and its sidecars.
func inspectFile(path string) FileReport {
	report := FileReport{}

	if info, err := os.Stat(path); err == nil {
		report.Exists = true
		report.SizeBytes = info.Size()
		report.Mode = info.Mode().Perm()
		report.ModifiedAt = info.ModTime().UTC()
	}

	if info, err := os.Stat(path + "-wal"); err == nil {
		report.WALExists = true
		report.WALSizeBytes = info.Size()
	}
	if _, err := os.Stat(path + "-shm"); err == nil {
		report.SHMExists = true
	}

	dir := filepath.Dir(path)
	if info, err := os.Stat(dir); err == nil {
		report.DirectoryMode = info.Mode().Perm()
	}
	report.DirectoryWritable = directoryWritable(dir)

	return report
}

// directoryWritable reports whether a file can be created in dir.
//
// Probed by creating and removing a file rather than by reading the mode,
// because the mode does not account for ownership, ACLs, a read-only mount, or
// a full disk -- all of which are exactly the conditions this is trying to
// detect. The probe file is created in the directory being tested with a fixed
// name, and removed immediately; it never carries content.
func directoryWritable(dir string) bool {
	probe, err := os.CreateTemp(dir, ".harbormaster-write-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	_ = probe.Close()
	// A probe left behind would be litter in the operator's data directory.
	_ = os.Remove(name)
	return true
}

func fileFindings(file FileReport) []Finding {
	findings := make([]Finding, 0)

	if !file.Exists {
		return findings
	}

	if !file.DirectoryWritable {
		findings = append(findings, Finding{
			Severity: SeverityCritical,
			Summary:  "the data directory is not writable",
			Remedy: "HarborMaster cannot write the database, its write-ahead log, or its " +
				"shared-memory index; check the mount options, the volume's free space, and ownership",
		})
	}

	// 0o077 is the world-and-group bits. A database describing container
	// configuration, mounts, and secret digests should be owner-only.
	//
	// Not checked on Windows, where Go synthesises a POSIX mode -- every
	// regular file reports 0666 regardless of its actual ACL. Reporting that
	// as a finding would be permanently wrong, and a diagnostic that always
	// warns is a diagnostic an operator learns to ignore.
	if runtime.GOOS != "windows" && file.Mode != 0 && file.Mode&0o077 != 0 {
		findings = append(findings, Finding{
			Severity: SeverityWarning,
			Summary:  fmt.Sprintf("the database file is readable beyond its owner (mode %#o)", file.Mode),
			Remedy:   "chmod 0600 the database file; it describes container configuration and secret digests",
		})
	}

	if file.WALSizeBytes > walSizeWarning {
		findings = append(findings, Finding{
			Severity: SeverityWarning,
			Summary: fmt.Sprintf("the write-ahead log is %s, far past the automatic checkpoint threshold",
				humanBytes(file.WALSizeBytes)),
			Remedy: "a reader is holding a transaction open and preventing checkpoints; " +
				"restarting HarborMaster checkpoints the log on close",
		})
	}

	if file.WALExists && !file.SHMExists {
		// The shape a crashed process leaves: committed transactions in the
		// log, and no shared-memory index because nothing has it open.
		findings = append(findings, Finding{
			Severity: SeverityInfo,
			Summary:  "a write-ahead log is present with no active connection",
			Remedy: "this is normal after an unclean stop; the next start replays it automatically " +
				"and no data is lost",
		})
	}

	return findings
}

// openFailureFinding turns a failed read-only open into an actionable line.
func openFailureFinding(err error, file FileReport) Finding {
	// The commonest case, and the one an operator would otherwise misread as
	// corruption: a database left with a hot log by a crashed process cannot
	// be opened read-only, because replaying the log is a write.
	if file.WALExists && store.Classify(err) == store.FailureCantOpen {
		return Finding{
			Severity: SeverityWarning,
			Summary:  "the database could not be opened read-only because its write-ahead log needs replaying",
			Remedy: "start HarborMaster once to replay the log, then re-run this command; " +
				"the data is intact and this is not corruption",
		}
	}

	if kind := store.Classify(err); kind != store.FailureNone {
		return Finding{
			Severity: SeverityCritical,
			Summary:  "the database could not be opened: " + string(kind),
			Remedy:   store.Remedy(kind),
		}
	}
	return Finding{
		Severity: SeverityCritical,
		Summary:  "the database could not be opened",
		Remedy:   "check that the path is a HarborMaster database and that the process can read it",
	}
}

func statsFindings(stats store.Stats) []Finding {
	findings := make([]Finding, 0)

	if stats.JournalMode != "wal" {
		findings = append(findings, Finding{
			Severity: SeverityWarning,
			Summary:  fmt.Sprintf("the journal mode is %q rather than WAL", stats.JournalMode),
			Remedy: "the filesystem probably cannot support WAL's shared-memory index; " +
				"readers will block on the writer and the crash-recovery profile differs from the documented one",
		})
	}

	if !stats.ForeignKeysOn {
		findings = append(findings, Finding{
			Severity: SeverityWarning,
			Summary:  "foreign key enforcement is off on this connection",
			Remedy: "HarborMaster enables it in its connection string; seeing it off here means " +
				"something opened the database differently",
		})
	}

	// A freelist larger than the live data means the file is mostly holes. Not
	// a fault -- it is what heavy retention pruning leaves behind -- but it is
	// why a database can be far bigger than its contents.
	if stats.PageCount > 0 && stats.FreelistCount*2 > stats.PageCount {
		findings = append(findings, Finding{
			Severity: SeverityInfo,
			Summary: fmt.Sprintf("%d of %d pages are free space left by pruning",
				stats.FreelistCount, stats.PageCount),
			Remedy: "`harbormaster backup` writes a compacted copy; the live file is not reclaimed automatically",
		})
	}

	return findings
}

func inspectMigrations(ctx context.Context, db *sql.DB) MigrationReport {
	report := MigrationReport{
		Applied:  make([]string, 0),
		Expected: make([]string, 0),
		Missing:  make([]string, 0),
		Unknown:  make([]string, 0),
	}

	applied, err := store.AppliedMigrations(ctx, db)
	if err != nil {
		return report
	}
	report.Applied = applied

	expected, err := store.MigrationNames()
	if err != nil {
		return report
	}
	report.Expected = expected

	appliedSet := make(map[string]struct{}, len(applied))
	for _, name := range applied {
		appliedSet[name] = struct{}{}
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, name := range expected {
		expectedSet[name] = struct{}{}
	}

	for _, name := range expected {
		if _, ok := appliedSet[name]; !ok {
			report.Missing = append(report.Missing, name)
		}
	}
	for _, name := range applied {
		if _, ok := expectedSet[name]; !ok {
			report.Unknown = append(report.Unknown, name)
		}
	}
	return report
}

func migrationFindings(report MigrationReport) []Finding {
	findings := make([]Finding, 0)

	if len(report.Unknown) > 0 {
		findings = append(findings, Finding{
			Severity: SeverityCritical,
			Summary: fmt.Sprintf("the database records %d migration(s) this build does not contain",
				len(report.Unknown)),
			Remedy: "a newer HarborMaster wrote this database; run that version, or restore a backup " +
				"taken before the upgrade -- do not delete the database",
		})
	}
	if len(report.Missing) > 0 {
		findings = append(findings, Finding{
			Severity: SeverityInfo,
			Summary:  fmt.Sprintf("%d migration(s) are pending", len(report.Missing)),
			Remedy:   "they are applied automatically on the next start",
		})
	}
	return findings
}

func inspectEngine(ctx context.Context, db *sql.DB) EngineReport {
	report := EngineReport{}

	var (
		connected    sql.NullString
		disconnected sql.NullString
		lastEvent    sql.NullString
		reconciled   sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT last_connected_at, last_disconnected_at, last_event_at,
		       last_reconciled_at, reconnect_count
		FROM event_engine_state
		ORDER BY host_id
		LIMIT 1`).Scan(&connected, &disconnected, &lastEvent, &reconciled, &report.ReconnectCount)
	if err != nil {
		// No row is the ordinary state before the engine has ever connected.
		return report
	}

	report.Present = true
	report.LastConnectedAt = connected.String
	report.LastDisconnectedAt = disconnected.String
	report.LastEventAt = lastEvent.String
	report.LastReconciledAt = reconciled.String
	return report
}

func inspectInventory(ctx context.Context, db *sql.DB) InventoryReport {
	report := InventoryReport{}

	var (
		trigger sql.NullString
		state   sql.NullString
		started sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT generation, trigger, state, started_at
		FROM inventory_refreshes
		ORDER BY id DESC
		LIMIT 1`).Scan(&report.Generation, &trigger, &state, &started)
	if err == nil {
		report.Present = true
		report.LastTrigger = trigger.String
		report.LastState = state.String
		report.LastStarted = started.String
	}

	// A refresh row stuck in `running` is the fingerprint of a process that
	// died mid-sweep. HarborMaster writes the row only on completion today, so
	// this should always be zero; asking is how a regression in that
	// invariant would be noticed rather than assumed away.
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM inventory_refreshes WHERE state = 'running'`).
		Scan(&report.RunningRefreshes); err != nil {
		report.RunningRefreshes = 0
	}

	return report
}

func inventoryFindings(report InventoryReport) []Finding {
	findings := make([]Finding, 0)

	if report.RunningRefreshes > 0 {
		findings = append(findings, Finding{
			Severity: SeverityWarning,
			Summary: fmt.Sprintf("%d inventory refresh row(s) are still marked running",
				report.RunningRefreshes),
			Remedy: "a HarborMaster process stopped mid-refresh; the inventory itself is unaffected " +
				"because a refresh commits atomically, and the next successful refresh supersedes it",
		})
	}
	if report.Present && report.LastState == "failed" {
		findings = append(findings, Finding{
			Severity: SeverityInfo,
			Summary:  "the most recent inventory refresh failed",
			Remedy:   "usually a Docker daemon that was unreachable; `harbormaster healthcheck` reports current reachability",
		})
	}
	return findings
}

// diagnosticCounts are the tables whose row counts the report includes.
//
// Literal queries against an explicit list, never a name interpolated into
// SQL. The counts describe volume only; no row contents are read.
var diagnosticCounts = []struct {
	name  string
	query string
}{
	{"containers", `SELECT COUNT(*) FROM containers`},
	{"containers_absent", `SELECT COUNT(*) FROM containers WHERE present = 0`},
	{"images", `SELECT COUNT(*) FROM images`},
	{"networks", `SELECT COUNT(*) FROM networks`},
	{"volumes", `SELECT COUNT(*) FROM volumes`},
	{"snapshots", `SELECT COUNT(*) FROM snapshots`},
	{"docker_events", `SELECT COUNT(*) FROM docker_events`},
	{"audit_events", `SELECT COUNT(*) FROM events`},
	{"inventory_refreshes", `SELECT COUNT(*) FROM inventory_refreshes`},
	{"inventory_warnings", `SELECT COUNT(*) FROM inventory_warnings`},
}

func countRows(ctx context.Context, db *sql.DB) map[string]int64 {
	counts := make(map[string]int64, len(diagnosticCounts))
	for _, table := range diagnosticCounts {
		var count int64
		if err := db.QueryRowContext(ctx, table.query).Scan(&count); err != nil {
			// A table that cannot be counted is omitted rather than reported
			// as zero. Zero and unreadable are different facts.
			continue
		}
		counts[table.name] = count
	}
	return counts
}

// humanBytes renders a size for an operator rather than for a parser.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit && exp < 3; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// RunBackup writes a verified backup and reports the outcome.
//
// The destination is an operator-supplied argument on a local command line,
// not untrusted input: whoever can run this already has the privileges the
// path would grant. It is still refused when it would overwrite an existing
// file or collide with the live database, because those are mistakes rather
// than attacks, and the cost of one is the backup an operator was relying on.
func RunBackup(ctx context.Context, out io.Writer, cfg config.Config, destination string) int {
	// Write errors are ignored throughout. The destination is stdout; a failure
	// to write means the stream is gone, and there is nowhere left to report
	// it. The EXIT CODE carries the verdict regardless of whether the text
	// arrived, which is what a script keys on.
	say := func(format string, args ...any) {
		_, _ = fmt.Fprintf(out, format, args...)
	}

	if strings.TrimSpace(destination) == "" {
		say("backup: a destination path is required\n")
		return ExitFailed
	}

	// Opened read-WRITE, because VACUUM INTO runs as a statement on a normal
	// connection. It still writes nothing to the source: the statement reads a
	// consistent snapshot and writes only the destination. Migrations DO run,
	// which is correct here -- backing up a database while declining to bring
	// it to the schema this build expects would produce a copy that cannot be
	// restored into this build.
	db, err := store.OpenWithOptions(ctx, store.Options{
		Path:        cfg.Store.Path,
		BusyTimeout: cfg.Store.BusyTimeout,
		// The source is verified as part of the backup, so checking it twice
		// on the way in would only double the wait.
		Integrity: store.IntegrityOff,
	})
	if err != nil {
		say("backup: cannot open the database: %v\n", err)
		if kind := store.Classify(err); kind != store.FailureNone {
			say("backup: %s\n", store.Remedy(kind))
		}
		return ExitFailed
	}
	// Close takes no context by design: it is the io.Closer contract, and its
	// internal write-ahead-log checkpoint deliberately uses a fresh bounded
	// context rather than this one. Reusing ctx would abort the checkpoint the
	// moment the caller's context ended, which is exactly when it is needed.
	//nolint:contextcheck // Close owns its own bounded context; see store.DB.Close.
	defer func() { _ = db.Close() }()

	result, err := store.Backup(ctx, db.SQL(), db.Path(), destination)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrBackupExists):
			say("backup: %s already exists; refusing to overwrite it\n", destination)
		case errors.Is(err, store.ErrBackupPathConflict):
			say("backup: the destination is the live database or one of its sidecars\n")
		default:
			say("backup: %v\n", err)
		}
		return ExitFailed
	}

	say("backup: wrote %s (%s) in %s\n",
		result.Path, humanBytes(result.SizeBytes), result.Duration.Round(time.Millisecond))
	say("backup: verified -- %s, %d migrations, %d snapshots, %d docker events\n",
		result.Verification.Integrity.Summary(),
		len(result.Verification.Migrations),
		result.Verification.TableCounts["snapshots"],
		result.Verification.TableCounts["docker_events"])
	return ExitOK
}
