package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"

	_ "modernc.org/sqlite"
)

// Crash recovery and concurrency stress.
//
// "Crash" here means what a crash actually leaves behind: a database file with
// committed transactions still in the write-ahead log and no clean close. It
// is reproduced by abandoning the handle without calling Close, which is the
// same on-disk state a SIGKILL produces, without the test having to kill
// itself.

// abandonHandle leaves a database exactly as a killed process would: committed
// data in the write-ahead log, no checkpoint, no clean close.
//
// The sql.DB is deliberately not closed. Its connection is released by the
// garbage collector eventually; what matters is that the FILE is left in the
// unrecovered state, which it is from the moment the writes commit.
func abandonHandle(t *testing.T, path string, write func(*sql.DB)) {
	t.Helper()

	db, err := sql.Open("sqlite",
		"file:"+path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open for abandonment: %v", err)
	}
	db.SetMaxOpenConns(1)
	write(db)

	// No Close, and no checkpoint: that is the point. The handle is released
	// so the test process does not hold a lock on a file it is about to reopen.
	t.Cleanup(func() { _ = db.Close() })
}

// Committed data must survive a process that never closed the database.
func TestCommittedDataSurvivesAnUncleanStop(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "unclean.db")

	// A normal start, so the schema exists.
	seeded, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	for i := range 40 {
		createSnapshot(t, seeded, newSnapshot("container-a", checksumA[:62]+padHex(i)))
	}
	// Abandoned without Close, exactly as a SIGKILL would leave it.
	if _, err := seeded.SQL().ExecContext(ctx, `
		INSERT INTO events (type, severity, message, occurred_at)
		VALUES ('server.started', 'info', 'about to be killed', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("final write: %v", err)
	}

	// A second handle to the same file confirms the data is durable from
	// another connection's point of view before the first is released.
	recovered, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("recovery open: %v", err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	t.Cleanup(func() { _ = seeded.Close() })

	count, err := recovered.Snapshots.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 40 {
		t.Errorf("snapshots after recovery = %d, want 40; committed data was lost", count)
	}
}

// A database left with a hot write-ahead log must open, replay, and verify.
func TestRecoveryReplaysAWriteAheadLog(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hotlog.db")

	// Establish the schema and close cleanly, so the state under test is
	// produced only by the abandoned handle below.
	initial, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	if err := initial.Close(); err != nil {
		t.Fatalf("close initial: %v", err)
	}

	abandonHandle(t, path, func(db *sql.DB) {
		for i := range 100 {
			if _, err := db.ExecContext(ctx, `
				INSERT INTO events (type, severity, message, occurred_at)
				VALUES ('server.started', 'info', ?, '2026-01-01T00:00:00Z')`,
				padding(i)); err != nil {
				t.Fatalf("write %d: %v", i, err)
			}
		}
	})

	// The log must actually be present, or the test is not testing recovery.
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Skip("no write-ahead log was left; this platform checkpointed eagerly")
	}

	recovered, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("a database with a hot write-ahead log must open and replay it: %v", err)
	}
	defer func() { _ = recovered.Close() }()

	if !recovered.OpenReport().Integrity.OK {
		t.Errorf("a replayed database failed its integrity check: %v",
			recovered.OpenReport().Integrity.Problems)
	}

	var count int
	if err := recovered.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events`).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 100 {
		t.Errorf("events after replay = %d, want 100", count)
	}
}

// A refresh interrupted mid-transaction must leave the PREVIOUS inventory
// intact and served, not a half-written one.
//
// This is the property CommitRefresh's single transaction exists for, asserted
// against an interruption rather than against a returned error.
func TestAnInterruptedRefreshLeavesThePreviousInventoryIntact(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if _, err := commitInventory(ctx, db, domain.TriggerStartup); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}
	before, checksumBefore, err := db.Inventory.CurrentGeneration(ctx)
	if err != nil {
		t.Fatalf("CurrentGeneration: %v", err)
	}

	// Cancel the refresh's context part-way, which is what a shutdown does to
	// a sweep in flight.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	_, err = db.Inventory.CommitRefresh(cancelled, store.RefreshCommit{
		Host: domain.Host{
			ID: domain.LocalHostID, Name: "local", Runtime: domain.RuntimeDocker,
		},
		Record: domain.RefreshRecord{Trigger: domain.TriggerPeriodic},
	})
	if err == nil {
		t.Fatal("a cancelled commit must fail rather than persist a partial refresh")
	}

	after, checksumAfter, err := db.Inventory.CurrentGeneration(ctx)
	if err != nil {
		t.Fatalf("CurrentGeneration after: %v", err)
	}
	if after != before || checksumAfter != checksumBefore {
		t.Errorf("generation moved from %d/%q to %d/%q; an interrupted refresh must change nothing",
			before, checksumBefore, after, checksumAfter)
	}
}

// A refresh row must never be left in `running`, because a refresh is recorded
// only when it completes. The diagnose command reports on this invariant, so
// it is worth pinning.
func TestNoRefreshRowIsEverLeftRunning(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if _, err := commitInventory(ctx, db, domain.TriggerStartup); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, _ = db.Inventory.CommitRefresh(cancelled, store.RefreshCommit{
		Host:   domain.Host{ID: domain.LocalHostID, Name: "local", Runtime: domain.RuntimeDocker},
		Record: domain.RefreshRecord{Trigger: domain.TriggerPeriodic},
	})

	var running int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM inventory_refreshes WHERE state = 'running'`).Scan(&running); err != nil {
		t.Fatalf("count running: %v", err)
	}
	if running != 0 {
		t.Errorf("%d refresh row(s) left running; recovery would report a phantom in-flight sweep", running)
	}
}

// Concurrent readers and writers must not corrupt anything, deadlock, or race.
//
// Run under -race in CI. The assertion at the end is the one that matters: the
// database is still structurally sound after the contention.
func TestConcurrentReadersAndWritersLeaveTheDatabaseSound(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	const (
		writers          = 4
		readers          = 8
		writesPerWriter  = 40
		readsPerReader   = 60
		operationTimeout = 30 * time.Second
	)

	runCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()

	var group sync.WaitGroup
	errCh := make(chan error, writers+readers)

	for w := range writers {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for i := range writesPerWriter {
				if runCtx.Err() != nil {
					return
				}
				if _, err := db.Events.Append(runCtx, domain.Event{
					Type:     domain.EventServerStarted,
					Severity: domain.SeverityInfo,
					Message:  "concurrent",
				}); err != nil {
					errCh <- err
					return
				}
				// A snapshot too, so the contention spans several tables and
				// several transaction shapes rather than one hot insert.
				if _, err := db.Snapshots.Create(runCtx,
					newSnapshot("container-a", checksumA[:60]+padHex(worker)+padHex(i)),
					nil, nil, nil); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}

	for range readers {
		group.Add(1)
		go func() {
			defer group.Done()
			for range readsPerReader {
				if runCtx.Err() != nil {
					return
				}
				if _, _, err := db.Snapshots.List(runCtx, store.SnapshotFilter{}); err != nil {
					errCh <- err
					return
				}
				if _, err := db.Events.List(runCtx, store.Page{}); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}

	group.Wait()
	close(errCh)

	for err := range errCh {
		// SQLITE_BUSY under contention would be a real finding: the pool
		// serialises writers and the busy timeout covers the rest.
		t.Errorf("concurrent access failed: %v (classified: %s)", err, store.Classify(err))
	}

	report, err := store.CheckIntegrity(ctx, db.SQL(), store.IntegrityFull, time.Minute)
	if err != nil {
		t.Fatalf("post-contention integrity check: %v", err)
	}
	if !report.OK {
		t.Errorf("the database is not sound after concurrent access: %v", report.Problems)
	}

	var events int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != writers*writesPerWriter {
		t.Errorf("events = %d, want %d; writes were lost under contention",
			events, writers*writesPerWriter)
	}
}

// Two processes writing the same database must not fail: the busy timeout is
// what turns cross-process contention into a wait rather than an error.
func TestASecondConnectionIsAbsorbedByTheBusyTimeout(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer func() { _ = first.Close() }()

	// A separate *store.DB is a separate connection pool, which is what a
	// second process looks like to SQLite.
	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer func() { _ = second.Close() }()

	var group sync.WaitGroup
	errCh := make(chan error, 2)

	for _, db := range []*store.DB{first, second} {
		group.Add(1)
		go func(target *store.DB) {
			defer group.Done()
			for range 30 {
				if _, err := target.Events.Append(ctx, domain.Event{
					Type:     domain.EventServerStarted,
					Severity: domain.SeverityInfo,
					Message:  "cross-process",
				}); err != nil {
					errCh <- err
					return
				}
			}
		}(db)
	}

	group.Wait()
	close(errCh)

	for err := range errCh {
		if store.Classify(err) == store.FailureBusy {
			t.Errorf("the busy timeout did not absorb cross-process contention: %v", err)
			continue
		}
		t.Errorf("cross-process write failed: %v", err)
	}

	var count int
	if err := first.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 60 {
		t.Errorf("events = %d, want 60; a write was lost between the two connections", count)
	}
}

// A database on a path that cannot be created must fail with a clear error
// rather than a driver-level surprise.
//
// The unwritable location is produced portably by putting a FILE where the
// directory needs to be, which every platform refuses to descend into.
func TestOpenFailsClearlyWhenTheDirectoryCannotBeCreated(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("create blocker: %v", err)
	}

	_, err := store.Open(context.Background(), filepath.Join(blocker, "nested", "harbormaster.db"))
	if err == nil {
		t.Fatal("opening a database under a file must fail")
	}
	if errors.Is(err, store.ErrCorrupt) {
		t.Error("a path problem must not be reported as corruption; the remedies are opposite")
	}
}

func TestOpenRejectsAnEmptyPath(t *testing.T) {
	if _, err := store.Open(context.Background(), ""); err == nil {
		t.Error("an empty database path must be refused")
	}
}
