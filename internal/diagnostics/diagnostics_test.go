package diagnostics_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/diagnostics"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// testConfig returns a configuration pointing at a database path inside a
// per-test temporary directory.
func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Store: config.Store{
			Path:             filepath.Join(t.TempDir(), "harbormaster.db"),
			BusyTimeout:      2 * time.Second,
			IntegrityCheck:   config.IntegrityCheckQuick,
			IntegrityTimeout: 30 * time.Second,
		},
	}
}

// seedDatabase creates a migrated database with a little content.
func seedDatabase(t *testing.T, cfg config.Config) {
	t.Helper()

	db, err := store.Open(context.Background(), cfg.Store.Path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Events.Append(context.Background(), domain.Event{
		Type:     domain.EventServerStarted,
		Severity: domain.SeverityInfo,
		Message:  "started",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestDiagnoseReportsAHealthyDatabase(t *testing.T) {
	cfg := testConfig(t)
	seedDatabase(t, cfg)

	report := diagnostics.Diagnose(context.Background(), cfg)

	if !report.Opened {
		t.Fatalf("the database could not be opened: %s", report.OpenError)
	}
	if !report.File.Exists {
		t.Error("the database file must be reported as existing")
	}
	if report.Stats.JournalMode != "wal" {
		t.Errorf("journal mode = %q, want wal", report.Stats.JournalMode)
	}
	if !report.Integrity.OK {
		t.Errorf("a healthy database failed its check: %v", report.Integrity.Problems)
	}
	if len(report.Migrations.Unknown) != 0 || len(report.Migrations.Missing) != 0 {
		t.Errorf("schema mismatch on a freshly migrated database: %+v", report.Migrations)
	}
	if report.Counts["audit_events"] != 1 {
		t.Errorf("audit_events = %d, want 1", report.Counts["audit_events"])
	}
	if report.Worst() == diagnostics.SeverityCritical {
		t.Errorf("a healthy database produced a critical finding: %+v", report.Findings)
	}
}

// A missing database is information, not a failure: it is what a first start
// looks like.
func TestDiagnoseOnAMissingDatabaseIsNotAFailure(t *testing.T) {
	cfg := testConfig(t)

	report := diagnostics.Diagnose(context.Background(), cfg)

	if report.File.Exists {
		t.Fatal("the file must not exist for this test to mean anything")
	}
	if report.ExitCode() != diagnostics.ExitOK {
		t.Errorf("exit code = %d, want %d; a first start has no database yet",
			report.ExitCode(), diagnostics.ExitOK)
	}
	if report.Worst() != diagnostics.SeverityInfo {
		t.Errorf("worst severity = %q, want info", report.Worst())
	}
}

// A corrupt database must be reported as critical, with the restore remedy.
func TestDiagnoseReportsCorruptionAsCritical(t *testing.T) {
	cfg := testConfig(t)
	seedRowsInto(t, cfg.Store.Path)
	corruptPages(t, cfg.Store.Path)

	report := diagnostics.Diagnose(context.Background(), cfg)

	if report.Worst() != diagnostics.SeverityCritical {
		t.Fatalf("worst severity = %q, want critical; findings: %+v", report.Worst(), report.Findings)
	}
	// A corrupt database that could not be opened is still a SUCCESSFUL
	// diagnosis: "restore a backup" is the answer the operator ran the command
	// for. Reporting it as "could not run" would point them at the diagnostic
	// instead of at the database.
	if got := report.ExitCode(); got != diagnostics.ExitFindings {
		t.Errorf("exit code = %d, want %d (findings), not %d (could not run)",
			got, diagnostics.ExitFindings, diagnostics.ExitFailed)
	}

	var remedied bool
	for _, finding := range report.Findings {
		if finding.Severity == diagnostics.SeverityCritical && finding.Remedy != "" {
			remedied = true
		}
	}
	if !remedied {
		t.Error("a critical finding must carry a remedy; an unactionable alarm is not a diagnosis")
	}
}

// A database a newer build wrote must be reported, and must NOT suggest
// deleting it.
func TestDiagnoseReportsANewerSchemaWithoutSuggestingDeletion(t *testing.T) {
	cfg := testConfig(t)
	seedDatabase(t, cfg)

	db, err := store.Open(context.Background(), cfg.Store.Path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.SQL().Exec(
		`INSERT INTO schema_migrations (name, checksum) VALUES ('0099_future.sql', 'x')`); err != nil {
		t.Fatalf("record a future migration: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	report := diagnostics.Diagnose(context.Background(), cfg)

	if len(report.Migrations.Unknown) != 1 {
		t.Fatalf("unknown migrations = %v, want one", report.Migrations.Unknown)
	}
	if report.Worst() != diagnostics.SeverityCritical {
		t.Errorf("worst = %q, want critical; an old binary on a new schema is not a warning", report.Worst())
	}

	var found bool
	for _, finding := range report.Findings {
		if strings.Contains(finding.Remedy, "do not delete") {
			found = true
		}
	}
	if !found {
		t.Error("the remedy must warn against deleting the database; that is the destructive mistake here")
	}
}

// The report must never carry a row's contents.
//
// The whole justification for `diagnose` being safe is that it describes
// storage rather than what is stored. This is the sweep that keeps it true.
func TestDiagnoseOutputCarriesNoRowContents(t *testing.T) {
	cfg := testConfig(t)

	const secretish = "SUPER-SECRET-CONTAINER-NAME-9182"

	db, err := store.Open(context.Background(), cfg.Store.Path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Events.Append(context.Background(), domain.Event{
		Type:     domain.EventServerStarted,
		Severity: domain.SeverityInfo,
		Message:  secretish,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	report := diagnostics.Diagnose(context.Background(), cfg)
	var out bytes.Buffer
	diagnostics.Render(&out, report)

	if strings.Contains(out.String(), secretish) {
		t.Error("the diagnosis printed a row's contents; it must describe storage, not what is stored")
	}

	// The positive control: the sweep must be able to find the string when it
	// IS present, or it proves nothing.
	if !strings.Contains(secretish+" in a haystack", secretish) {
		t.Fatal("the sweep cannot detect the value it is looking for")
	}

	// And it must have actually seen the row, or the test above passed only
	// because nothing was read at all.
	if report.Counts["audit_events"] != 1 {
		t.Fatalf("audit_events = %d, want 1; the diagnosis did not read the table",
			report.Counts["audit_events"])
	}
}

func TestRenderProducesAReadableReport(t *testing.T) {
	cfg := testConfig(t)
	seedDatabase(t, cfg)

	report := diagnostics.Diagnose(context.Background(), cfg)
	var out bytes.Buffer
	diagnostics.Render(&out, report)

	text := out.String()
	for _, want := range []string{
		"HarborMaster diagnosis", "Database file", "Storage", "Schema",
		"Event engine", "Inventory", "Row counts", "Findings",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report is missing the %q section", want)
		}
	}
	// No escape sequences: the output is read through `docker logs` and pasted
	// into issues.
	if strings.ContainsRune(text, '\x1b') {
		t.Error("the report contains a terminal escape sequence")
	}
}

// Rendering a report from a database that could not be opened must not panic
// and must still say what was learned from the filesystem.
func TestRenderHandlesAnUnopenedDatabase(t *testing.T) {
	cfg := testConfig(t)

	report := diagnostics.Diagnose(context.Background(), cfg)
	var out bytes.Buffer
	diagnostics.Render(&out, report)

	if !strings.Contains(out.String(), "Database file") {
		t.Error("the filesystem section must be rendered even when the database cannot be opened")
	}
	if !strings.Contains(out.String(), "Findings") {
		t.Error("findings must be rendered even when the database cannot be opened")
	}
}

// Diagnose must never write to the database. A diagnostic that migrates or
// creates anything is a diagnostic an operator cannot run safely.
func TestDiagnoseDoesNotModifyTheDatabase(t *testing.T) {
	cfg := testConfig(t)
	seedDatabase(t, cfg)

	before, err := os.Stat(cfg.Store.Path)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}
	beforeContent, err := os.ReadFile(cfg.Store.Path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	report := diagnostics.Diagnose(context.Background(), cfg)
	if !report.Opened {
		t.Fatalf("the database did not open: %s", report.OpenError)
	}

	after, err := os.Stat(cfg.Store.Path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	afterContent, err := os.ReadFile(cfg.Store.Path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}

	if before.Size() != after.Size() || !bytes.Equal(beforeContent, afterContent) {
		t.Error("the diagnosis modified the database; it must open read-only and migrate nothing")
	}
}

// ------------------------------------------------------------- backup --

func TestRunBackupWritesAndVerifies(t *testing.T) {
	cfg := testConfig(t)
	seedDatabase(t, cfg)
	dest := filepath.Join(t.TempDir(), "backup.db")

	var out bytes.Buffer
	if code := diagnostics.RunBackup(context.Background(), &out, cfg, dest); code != diagnostics.ExitOK {
		t.Fatalf("exit code = %d, want %d; output: %s", code, diagnostics.ExitOK, out.String())
	}
	if !strings.Contains(out.String(), "verified") {
		t.Errorf("the backup must report that it was verified: %s", out.String())
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("the backup was not written: %v", err)
	}
}

func TestRunBackupRefusesAnEmptyDestination(t *testing.T) {
	cfg := testConfig(t)
	seedDatabase(t, cfg)

	var out bytes.Buffer
	if code := diagnostics.RunBackup(context.Background(), &out, cfg, "  "); code == diagnostics.ExitOK {
		t.Error("an empty destination must not report success")
	}
}

func TestRunBackupRefusesToOverwrite(t *testing.T) {
	cfg := testConfig(t)
	seedDatabase(t, cfg)
	dest := filepath.Join(t.TempDir(), "existing.db")

	if err := os.WriteFile(dest, []byte("previous"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out bytes.Buffer
	code := diagnostics.RunBackup(context.Background(), &out, cfg, dest)
	if code == diagnostics.ExitOK {
		t.Fatal("overwriting an existing backup must not report success")
	}
	if !strings.Contains(out.String(), "refusing to overwrite") {
		t.Errorf("the reason must be stated: %s", out.String())
	}
}

// seedRowsInto creates a database with enough pages that corrupting it is
// possible.
func seedRowsInto(t *testing.T, path string) {
	t.Helper()

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := range 200 {
		if _, err := db.SQL().Exec(`
			INSERT INTO events (type, severity, message, occurred_at)
			VALUES ('server.started', 'info', ?, '2026-01-01T00:00:00Z')`,
			strings.Repeat("x", 512)+string(rune('a'+i%26))); err != nil {
			t.Fatalf("seed row: %v", err)
		}
	}
	if _, err := db.SQL().Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// corruptPages damages the database's content pages while leaving the header
// intact, which is what a bad sector or an interrupted write produces.
func corruptPages(t *testing.T, path string) {
	t.Helper()

	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	const firstPage = 4096
	if info.Size() <= firstPage {
		t.Fatalf("database is only %d bytes; seed more data first", info.Size())
	}

	junk := make([]byte, info.Size()-firstPage)
	for i := range junk {
		junk[i] = byte(255 - i%256)
	}
	if _, err := file.WriteAt(junk, firstPage); err != nil {
		t.Fatalf("write corruption: %v", err)
	}
	if err := file.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
}
