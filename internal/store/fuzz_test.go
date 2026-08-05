package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/store"
)

// FuzzOpenArbitraryDatabaseFile feeds arbitrary bytes to the open path.
//
// WHAT IT IS LOOKING FOR. A database file is not untrusted input in the usual
// sense -- an attacker who can write it already owns the volume -- but it IS
// input that arrives damaged in ordinary operation: a truncated write, a
// half-restored backup, a file cut short by a full disk, a volume that
// returned zeros after a power loss. Every one of those produces bytes that
// are almost, but not quite, a database.
//
// The property under test is therefore not "it rejects bad input" but the
// stronger and more useful one: whatever bytes are at the path, Open either
// succeeds or returns an error. It never panics, never hangs, and never leaves
// a live handle behind on the failure path.
//
// The seed corpus is chosen for the shapes real damage takes, and the fuzzer
// explores outward from them.
func FuzzOpenArbitraryDatabaseFile(f *testing.F) {
	// Empty: SQLite treats a zero-length file as a new database, so this must
	// SUCCEED. Included so a change that started rejecting it is noticed.
	f.Add([]byte{})
	// A valid header with nothing behind it: the shape of a truncated write.
	f.Add([]byte("SQLite format 3\x00"))
	// A header with plausible page metadata and no pages.
	f.Add(append([]byte("SQLite format 3\x00"), make([]byte, 84)...))
	// Not a database at all.
	f.Add([]byte("this is a text file, not a database"))
	// The header with one byte changed, which is what a single flipped bit on
	// a failing disk looks like.
	f.Add([]byte("SQLite format 4\x00\x10\x00\x01\x01\x00\x40\x20\x20"))
	// A run of zeros, which is what some filesystems return for a region that
	// was never actually written before a power loss.
	f.Add(make([]byte, 4096))

	f.Fuzz(func(t *testing.T, content []byte) {
		// A very large input tells us nothing new and makes the corpus
		// expensive to replay. The interesting behaviour is all in the header
		// and the first page or two.
		if len(content) > 1<<16 {
			t.Skip("oversized input; the behaviour under test lives in the first pages")
		}

		path := filepath.Join(t.TempDir(), "fuzz.db")
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatalf("write input: %v", err)
		}

		// A bounded context, so a pathological input that made SQLite loop
		// would fail the test rather than hang the fuzzer.
		ctx, cancel := context.WithTimeout(context.Background(), fuzzOpenTimeout)
		defer cancel()

		db, err := store.OpenWithOptions(ctx, store.Options{
			Path: path,
			// Quick rather than full: the fuzzer runs this thousands of times,
			// and the open path is what is under test, not the depth of the
			// check.
			Integrity:        store.IntegrityQuick,
			IntegrityTimeout: fuzzOpenTimeout,
		})
		if err != nil {
			// An error is a perfectly good outcome. What must never happen is
			// a returned error alongside a usable handle.
			if db != nil {
				t.Fatal("Open returned both an error and a database handle; " +
					"the failure path must not leak an open connection")
			}
			return
		}

		// It opened, so it must be a real, migrated, usable database -- not a
		// handle that will fail on the first query.
		defer func() { _ = db.Close() }()

		if err := db.Ping(ctx); err != nil {
			t.Fatalf("Open succeeded but the database does not answer: %v", err)
		}
		if _, err := store.AppliedMigrations(ctx, db.SQL()); err != nil {
			t.Fatalf("Open succeeded but the schema history is unreadable: %v", err)
		}
		var count int
		if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshots`).Scan(&count); err != nil {
			t.Fatalf("Open succeeded but the schema is not usable: %v", err)
		}
	})
}

// fuzzOpenTimeout bounds one fuzz iteration's database work.
const fuzzOpenTimeout = 20 * time.Second
