package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/store"
)

func TestIntegrityCheckPassesOnAHealthyDatabase(t *testing.T) {
	db := openTestDB(t)

	for _, mode := range []store.IntegrityMode{store.IntegrityQuick, store.IntegrityFull} {
		t.Run(string(mode), func(t *testing.T) {
			report, err := store.CheckIntegrity(context.Background(), db.SQL(), mode, 30*time.Second)
			if err != nil {
				t.Fatalf("CheckIntegrity: %v", err)
			}
			if !report.OK {
				t.Errorf("a freshly migrated database failed its %s check: %v", mode, report.Problems)
			}
			if report.Damaged() {
				t.Error("a healthy database must not be reported as damaged")
			}
			if report.Incomplete {
				t.Error("the check must have completed")
			}
		})
	}
}

// IntegrityOff must not run a check, and must not claim to have found damage.
func TestIntegrityOffReportsOKWithoutChecking(t *testing.T) {
	db := openTestDB(t)

	report, err := store.CheckIntegrity(context.Background(), db.SQL(), store.IntegrityOff, time.Second)
	if err != nil {
		t.Fatalf("CheckIntegrity: %v", err)
	}
	if !report.OK || report.Damaged() {
		t.Error("a disabled check must report OK and never damage; it establishes nothing either way")
	}
	if report.Duration != 0 {
		t.Error("a disabled check must not have taken time")
	}
}

func TestIntegrityRejectsAnUnknownMode(t *testing.T) {
	db := openTestDB(t)

	if _, err := store.CheckIntegrity(context.Background(), db.SQL(),
		store.IntegrityMode("thorough"), time.Second); err == nil {
		t.Error("an unknown mode must be an error; silently skipping the check is the failure this prevents")
	}
}

// A corrupt database must be REFUSED at open, not opened and written to.
//
// This is the fail-closed decision the phase turns on: continuing to write
// history over a malformed image destroys the window in which a backup still
// predates the damage.
func TestOpenRefusesACorruptDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")

	// A real database first, so the file has a valid header and SQLite gets
	// far enough to discover that the PAGES are wrong. A file of pure garbage
	// is rejected earlier, by a different code path, and is covered separately.
	seed, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedRows(t, seed)
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	corruptPages(t, path)

	db, err := store.Open(context.Background(), path)
	if err == nil {
		_ = db.Close()
		t.Fatal("a corrupt database was opened; HarborMaster must refuse rather than write over damage")
	}
	if !errors.Is(err, store.ErrCorrupt) {
		t.Errorf("err = %v, want it to match store.ErrCorrupt", err)
	}
}

// Pure garbage at the path is corruption too, and must be refused the same way
// rather than surfacing as an inexplicable driver error.
func TestOpenRefusesAFileThatIsNotADatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notadb.db")
	writeGarbage(t, path)

	db, err := store.Open(context.Background(), path)
	if err == nil {
		_ = db.Close()
		t.Fatal("a file that is not a database was accepted")
	}
	// Either classification is acceptable -- SQLite reports NOTADB or CORRUPT
	// depending on where it gives up -- but it must be one of them, and it
	// must never look like "the file is missing".
	if !errors.Is(err, store.ErrCorrupt) && store.Classify(err) != store.FailureCorrupt {
		t.Errorf("err = %v; a non-database file must classify as corruption", err)
	}
}

// Turning the check off must actually skip it -- otherwise the setting is a
// lie and an operator who cannot afford the check on the startup path has no
// way to say so.
func TestIntegrityOffSkipsTheCheckAtOpen(t *testing.T) {
	db, err := store.OpenWithOptions(context.Background(), store.Options{
		Path:      filepath.Join(t.TempDir(), "unchecked.db"),
		Integrity: store.IntegrityOff,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	report := db.OpenReport().Integrity
	if report.Mode != store.IntegrityOff {
		t.Errorf("recorded mode = %q, want off", report.Mode)
	}
	if report.Duration != 0 {
		t.Error("a skipped check must not have taken time")
	}
	if len(report.Problems) != 0 {
		t.Error("a skipped check must report no problems; it established nothing")
	}
}

// Turning the check off does NOT make a damaged database openable, and that is
// deliberate rather than an oversight in the setting.
//
// SQLite refuses a malformed image itself, on the very first statement, well
// before HarborMaster's check would run. The setting governs whether
// HarborMaster looks for damage proactively; it cannot and must not override
// the database engine's own refusal to read a file it knows is broken. Pinning
// this stops a future change from "fixing" the setting by suppressing the
// engine's error, which would put HarborMaster back to writing over damage.
func TestDisablingTheCheckStillFailsClosedOnRealCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "salvage.db")

	seed, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedRows(t, seed)
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
	corruptPages(t, path)

	db, err := store.OpenWithOptions(context.Background(), store.Options{
		Path:      path,
		Integrity: store.IntegrityOff,
	})
	if err == nil {
		_ = db.Close()
		t.Fatal("a database SQLite itself cannot read was opened; the failure must stay closed " +
			"regardless of the integrity setting")
	}
	if !errors.Is(err, store.ErrCorrupt) {
		t.Errorf("err = %v, want it to match store.ErrCorrupt so the caller can print the restore remedy", err)
	}
}

// A check that cannot finish must be reported as incomplete, and must NOT be
// read as damage. The distinction is the whole reason Damaged() exists.
func TestAnIncompleteCheckIsNotDamage(t *testing.T) {
	db := openTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Already cancelled: the check cannot run at all.

	report, err := store.CheckIntegrity(ctx, db.SQL(), store.IntegrityFull, time.Second)
	if err != nil {
		t.Fatalf("a cancelled check must not be an error, it must be an incomplete report: %v", err)
	}
	if !report.Incomplete {
		t.Fatal("a cancelled check must report Incomplete")
	}
	if report.Damaged() {
		t.Error("an incomplete check must never be read as damage; nothing was established")
	}
	if report.OK {
		t.Error("an incomplete check must not report OK either")
	}
}

// An incomplete check at OPEN must not refuse startup. A slow disk is not a
// reason to take the service down.
func TestOpenContinuesWhenTheIntegrityCheckCannotComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slow.db")

	// A one-nanosecond budget guarantees the check cannot finish.
	db, err := store.OpenWithOptions(context.Background(), store.Options{
		Path:             path,
		Integrity:        store.IntegrityFull,
		IntegrityTimeout: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("an incomplete check must not refuse startup: %v", err)
	}
	defer func() { _ = db.Close() }()

	if !db.OpenReport().Integrity.Incomplete {
		t.Error("the open report must record that the check did not complete")
	}
	if db.OpenReport().Integrity.Damaged() {
		t.Error("an incomplete check must not be recorded as damage")
	}
}

func TestOpenRecordsTheJournalMode(t *testing.T) {
	db := openTestDB(t)

	report := db.OpenReport()
	if report.JournalMode != "wal" {
		t.Errorf("journal mode = %q, want wal on an ordinary filesystem", report.JournalMode)
	}
	if !report.WALActive() {
		t.Error("WALActive must agree with the recorded journal mode")
	}
	if !report.WALRequested {
		t.Error("the report must record that WAL was requested, so a fallback is distinguishable")
	}
}

func TestReadStatsReportsTheConnectionSettings(t *testing.T) {
	db := openTestDB(t)

	stats, err := store.ReadStats(context.Background(), db.SQL())
	if err != nil {
		t.Fatalf("ReadStats: %v", err)
	}
	if stats.JournalMode != "wal" {
		t.Errorf("JournalMode = %q, want wal", stats.JournalMode)
	}
	if !stats.ForeignKeysOn {
		t.Error("foreign keys must be on; referential integrity is off by default in SQLite")
	}
	if stats.PageSize <= 0 || stats.PageCount <= 0 {
		t.Errorf("page size %d and count %d must both be positive on a migrated database",
			stats.PageSize, stats.PageCount)
	}
	if stats.SizeBytes != stats.PageSize*stats.PageCount {
		t.Error("SizeBytes must be derived from the page size and count")
	}
	if stats.Encoding == "" {
		t.Error("encoding must be reported")
	}
}

func TestValidIntegrityMode(t *testing.T) {
	for _, mode := range []store.IntegrityMode{store.IntegrityOff, store.IntegrityQuick, store.IntegrityFull} {
		if !store.ValidIntegrityMode(mode) {
			t.Errorf("%q must be valid", mode)
		}
	}
	for _, mode := range []string{"", "OFF", "quick_check", "yes", "1"} {
		if store.ValidIntegrityMode(store.IntegrityMode(mode)) {
			t.Errorf("%q must not be valid; the vocabulary is closed", mode)
		}
	}
}

// corruptPages overwrites the database's content pages while leaving the
// header intact.
//
// The header is preserved so SQLite opens the file and discovers the damage
// while reading, which is what real corruption looks like: a power loss or a
// bad sector damages pages, not the first hundred bytes.
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
	// Past the first page (the header and the schema root) so the file still
	// opens, into the pages that hold rows and index entries.
	const firstPage = 4096
	if info.Size() <= firstPage {
		t.Fatalf("database is only %d bytes; seed more data before corrupting it", info.Size())
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

// seedRows writes enough data that the database spans several pages, which is
// what makes page-level corruption possible to induce.
func seedRows(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()

	for i := range 200 {
		if _, err := db.SQL().ExecContext(ctx, `
			INSERT INTO events (type, severity, message, occurred_at)
			VALUES ('server.started', 'info', ?, '2026-01-01T00:00:00Z')`,
			padding(i)); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	// Checkpoint so the rows are in the database file rather than only in the
	// write-ahead log; corrupting the main file must actually corrupt the data.
	if _, err := db.SQL().ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}

func padding(seed int) string {
	buf := make([]byte, 512)
	for i := range buf {
		buf[i] = byte('a' + (seed+i)%26)
	}
	return string(buf)
}
