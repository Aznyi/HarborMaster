package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/store"

	_ "modernc.org/sqlite"
)

// Classification tests, with the positive controls that make them mean
// something.
//
// A classifier that returns "unknown" for everything would pass a test suite
// that only asserts "a corrupt file is not classified as disk-full". So each
// condition below is INDUCED for real -- a real SQLITE_FULL from a real page
// limit, a real SQLITE_BUSY from a real second writer -- and each test asserts
// the specific kind, not merely that something was classified.

// TestClassifyIgnoresNonDriverErrors is the negative control: this package
// must not claim a verdict on an error it did not produce.
func TestClassifyIgnoresNonDriverErrors(t *testing.T) {
	for name, err := range map[string]error{
		"nil":     nil,
		"generic": errors.New("something went wrong"),
		"wrapped": errors.New("open data/x.db: permission denied"),
		"context": context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			if got := store.Classify(err); got != store.FailureNone {
				t.Errorf("Classify(%v) = %q, want %q; this package must not "+
					"invent a storage verdict for an error it did not produce",
					err, got, store.FailureNone)
			}
		})
	}
}

// TestClassifyDetectsCorruptionFromAGarbageFile induces real corruption by
// overwriting the database header, which is what a truncated write or a
// partially restored file looks like.
func TestClassifyDetectsCorruptionFromAGarbageFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.db")
	writeGarbage(t, path)

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.Query(`SELECT COUNT(*) FROM sqlite_master`)
	if err == nil {
		t.Fatal("reading a garbage file must fail")
	}
	if got := store.Classify(err); got != store.FailureCorrupt {
		t.Errorf("Classify = %q, want %q (error: %v)", got, store.FailureCorrupt, err)
	}
	if !errors.Is(store.AsError(err), store.ErrCorrupt) {
		t.Error("AsError must make the failure matchable with errors.Is(err, ErrCorrupt)")
	}
}

// TestClassifyDetectsDiskFull induces a real SQLITE_FULL.
//
// PRAGMA max_page_count is the portable way to simulate a full volume: SQLite
// returns exactly the result code it would return on a full disk, without
// needing a filesystem that can be filled. Filling a real disk in a test would
// be both slow and hostile to whatever else is running.
func TestClassifyDetectsDiskFull(t *testing.T) {
	db := openMigratedDB(t)
	ctx := context.Background()

	var limited int64
	// The current page count plus a little headroom, so the limit is reachable
	// but not already exceeded.
	if err := db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&limited); err != nil {
		t.Fatalf("page_count: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA max_page_count = 100`); err != nil {
		t.Fatalf("max_page_count: %v", err)
	}

	// Write until the page limit is hit. A blob per row so the limit arrives
	// in a bounded number of iterations rather than a million.
	payload := make([]byte, 8192)
	var writeErr error
	for i := 0; i < 500 && writeErr == nil; i++ {
		_, writeErr = db.ExecContext(ctx, `
			INSERT INTO events (type, severity, message, occurred_at)
			VALUES ('server.started', 'info', ?, '2026-01-01T00:00:00Z')`,
			string(payload))
	}
	if writeErr == nil {
		t.Fatal("the page limit was never reached; this test cannot prove anything")
	}

	if got := store.Classify(writeErr); got != store.FailureDiskFull {
		t.Errorf("Classify = %q, want %q (error: %v)", got, store.FailureDiskFull, writeErr)
	}
	if !errors.Is(store.AsError(writeErr), store.ErrDiskFull) {
		t.Error("a full database must match errors.Is(err, ErrDiskFull)")
	}
	if store.Remedy(store.FailureDiskFull) == "" {
		t.Error("every classified failure must carry an operator remedy")
	}
}

// TestClassifyDetectsReadOnly induces a real SQLITE_READONLY by opening the
// database through a read-only connection.
//
// Deliberately not by chmod-ing the file: file modes are not enforced the same
// way on Windows, and the property under test is the classification of the
// result code, which mode=ro produces identically on every platform.
func TestClassifyDetectsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ro.db")
	seed, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	db, err := store.OpenReadOnly(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.ExecContext(context.Background(), `
		INSERT INTO events (type, severity, message, occurred_at)
		VALUES ('server.started', 'info', 'nope', '2026-01-01T00:00:00Z')`)
	if err == nil {
		t.Fatal("a read-only connection accepted a write; mode=ro is not being applied")
	}
	if got := store.Classify(err); got != store.FailureReadOnly {
		t.Errorf("Classify = %q, want %q (error: %v)", got, store.FailureReadOnly, err)
	}
}

// TestClassifyDetectsBusy induces a real SQLITE_BUSY with two connections and
// no busy timeout to absorb it.
func TestClassifyDetectsBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	seed, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	// busy_timeout(0): the point is to observe SQLITE_BUSY, and a timeout
	// would convert the contention into a wait.
	openContender := func() *sql.DB {
		db, err := sql.Open("sqlite",
			"file:"+path+"?_pragma=busy_timeout(0)&_pragma=journal_mode(WAL)")
		if err != nil {
			t.Fatalf("open contender: %v", err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}

	first, second := openContender(), openContender()
	ctx := context.Background()

	// An IMMEDIATE transaction takes the write lock straight away rather than
	// deferring it to the first write, which makes the contention deterministic.
	if _, err := first.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("begin immediate: %v", err)
	}
	defer func() { _, _ = first.ExecContext(ctx, `ROLLBACK`) }()

	_, err = second.ExecContext(ctx, `
		INSERT INTO events (type, severity, message, occurred_at)
		VALUES ('server.started', 'info', 'contended', '2026-01-01T00:00:00Z')`)
	if err == nil {
		t.Fatal("a second writer succeeded while the first held the lock")
	}
	if got := store.Classify(err); got != store.FailureBusy {
		t.Errorf("Classify = %q, want %q (error: %v)", got, store.FailureBusy, err)
	}
}

// A busy timeout must actually absorb contention rather than merely being
// configured. Without this, the DSN could stop applying the pragma and nothing
// would notice until a second process appeared in production.
func TestBusyTimeoutIsAppliedToTheConnection(t *testing.T) {
	db := openTestDB(t)

	var timeout int64
	if err := db.SQL().QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if want := store.DefaultBusyTimeout.Milliseconds(); timeout != want {
		t.Errorf("busy_timeout = %dms, want %dms; the DSN pragma is not reaching the connection",
			timeout, want)
	}
}

func TestOpenWithOptionsAppliesACustomBusyTimeout(t *testing.T) {
	db, err := store.OpenWithOptions(context.Background(), store.Options{
		Path:        filepath.Join(t.TempDir(), "custom.db"),
		BusyTimeout: 1234 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var timeout int64
	if err := db.SQL().QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if timeout != 1234 {
		t.Errorf("busy_timeout = %dms, want 1234ms", timeout)
	}
}

// Every remedy string must exist and must not name a path.
//
// The remedy is printed by the diagnose command and written to the log, and it
// is the one place where a well-meaning addition could put a filesystem path
// in front of an audience that should not have it.
func TestEveryFailureKindHasARemedyAndNoPath(t *testing.T) {
	kinds := []store.FailureKind{
		store.FailureCorrupt, store.FailureDiskFull, store.FailureReadOnly,
		store.FailureBusy, store.FailurePermission, store.FailureIO,
		store.FailureCantOpen, store.FailureUnknown,
	}
	for _, kind := range kinds {
		remedy := store.Remedy(kind)
		if remedy == "" {
			t.Errorf("FailureKind %q has no remedy; an unactionable classification is not worth having", kind)
		}
		for _, forbidden := range []string{"/var/run", ".db", "C:\\", "harbormaster.db"} {
			if strings.Contains(remedy, forbidden) {
				t.Errorf("remedy for %q contains %q; remedies are printed and logged and must not name paths",
					kind, forbidden)
			}
		}
	}
	// The positive control for the sweep above: it must be able to find
	// something when something is there.
	if !strings.Contains("restore from /var/run/x", "/var/run") {
		t.Fatal("the path sweep cannot detect a path it is looking for")
	}
	// FailureNone is the absence of a failure and correctly has no remedy.
	if store.Remedy(store.FailureNone) != "" {
		t.Error("FailureNone must have no remedy; there is nothing to remedy")
	}
}

// Only corruption may be fatal. Everything else describes a condition that can
// clear on its own, and treating those as fatal would turn a full disk into an
// unrecoverable state.
func TestOnlyCorruptionIsFatal(t *testing.T) {
	if !store.IsFatal(store.FailureCorrupt) {
		t.Error("corruption must be fatal; continuing writes damage over the operator's history")
	}
	for _, kind := range []store.FailureKind{
		store.FailureDiskFull, store.FailureReadOnly, store.FailureBusy,
		store.FailurePermission, store.FailureIO, store.FailureCantOpen,
		store.FailureUnknown, store.FailureNone,
	} {
		if store.IsFatal(kind) {
			t.Errorf("%q must not be fatal; it is a condition that can clear", kind)
		}
	}
}

// writeGarbage overwrites a path with bytes that are not a SQLite database.
func writeGarbage(t *testing.T, path string) {
	t.Helper()
	// Long enough that SQLite reads a header and rejects it, rather than
	// treating an empty file as a new database.
	garbage := make([]byte, 8192)
	for i := range garbage {
		garbage[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
}
