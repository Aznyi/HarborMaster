//go:build windows

package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/store"
)

// The Windows half of the permission contract.
//
// Go synthesises a file's mode on Windows from the read-only attribute alone,
// so a chmod to 0600 there changes nothing an attacker has to get past — the
// real answer lives in an ACL this package neither reads nor writes.
//
// The requirement is therefore not "restrict the file" but "do not claim to
// have restricted it". A security control that reports success it did not
// achieve is worse than an absent one, because an operator stops looking.

func TestOnWindowsThePermissionPassReportsItselfSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbormaster.db")

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	report := db.OpenReport().Permissions

	if report.Enforced {
		t.Error("the permission pass claims POSIX mode bits are enforced on Windows; " +
			"they are not, and reporting so would be a control that lies")
	}
	if report.Mode != 0 {
		t.Errorf("reported mode = %#o, want 0\n"+
			"\ta synthesised mode must not be presented as an established permission",
			report.Mode)
	}
	if len(report.Tightened) != 0 {
		t.Errorf("reported %v as tightened on a platform where nothing was changed",
			report.Tightened)
	}
}

// Opening still succeeds. The database is usable on Windows for development;
// what changes is only what HarborMaster claims about its exposure.
func TestOnWindowsTheDatabaseStillOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbormaster.db")

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if got := db.OpenReport().JournalMode; got == "" {
		t.Error("the database opened without reporting a journal mode")
	}
}
