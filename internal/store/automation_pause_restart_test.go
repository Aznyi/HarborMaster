package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Stage 17.6 §24: a pause, and its clearing, both survive a restart.
//
// # Why this is worth its own test
//
// The safety property is "a failure-induced pause never clears itself". A pause
// held in memory would clear itself on the next restart -- and a crash loop is
// exactly the situation in which HarborMaster restarts, so the one case where
// the guarantee matters most is the one an in-memory implementation would fail.
//
// The converse matters too and is the half more likely to regress: an operator
// who investigated a failure and resumed must not find the pause back after a
// restart, or they will conclude the control does not work and stop using it.
//
// Both halves are asserted against a REOPENED database rather than a second
// handle, so nothing about the result can come from process-local state.

// reopenableDB opens a store at a path the test controls, so it can be closed
// and opened again.
func reopenableDB(t *testing.T) (string, *store.DB) {
	t.Helper()

	template, err := migratedTemplate()
	if err != nil {
		t.Fatalf("build migrated template: %v", err)
	}
	path := filepath.Join(t.TempDir(), "harbormaster.db")
	if err := os.WriteFile(path, template, 0o600); err != nil {
		t.Fatalf("seed test database: %v", err)
	}

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return path, db
}

func reopen(t *testing.T, path string, db *store.DB) *store.DB {
	t.Helper()

	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

func TestAPauseSurvivesARestartAndAResumeSurvivesOneToo(t *testing.T) {
	ctx := context.Background()
	path, db := reopenableDB(t)
	now := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)

	if _, err := db.Automation.Pause(ctx, domain.PausedContainer{
		ContainerName: "web",
		ContainerID:   "container-before",
		Reason:        domain.PauseRolledBack,
		Detail:        "the replacement failed verification and the previous container was restored",
		Failures:      2,
		PausedAt:      now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// ---- restart 1: the pause is still in force ---------------------------
	db = reopen(t, path, db)

	pause, err := db.Automation.PauseFor(ctx, "web")
	if err != nil {
		t.Fatalf("PauseFor after restart: %v", err)
	}
	if !pause.Active(now) {
		t.Fatal("the pause cleared itself across a restart")
	}
	// The evidence an operator needs to understand it survived too.
	if pause.Reason != domain.PauseRolledBack || pause.Failures != 2 {
		t.Fatalf("the pause lost its evidence: reason %q, failures %d",
			pause.Reason, pause.Failures)
	}
	if pause.Detail == "" {
		t.Fatal("the recorded detail did not survive the restart")
	}

	active, err := db.Automation.ActivePauses(ctx)
	if err != nil {
		t.Fatalf("ActivePauses: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("active pauses after restart = %d, want 1", len(active))
	}

	// ---- resume, then restart 2: it stays cleared -------------------------
	if err := db.Automation.Resume(ctx, "web",
		domain.Requester{UserID: "usr_1", Username: "colby"}, now); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	db = reopen(t, path, db)

	active, err = db.Automation.ActivePauses(ctx)
	if err != nil {
		t.Fatalf("ActivePauses after resume and restart: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("the pause came back after a restart: %d active", len(active))
	}

	// And the record is still there, attributed: resuming deactivates, it does
	// not delete. The question "who let this container be updated again" has to
	// stay answerable.
	//
	// Read through the HISTORY rather than PauseFor, which answers with the
	// ACTIVE pause and correctly reports none now.
	history, _, err := db.Automation.ListPauses(ctx, false, store.Page{})
	if err != nil {
		t.Fatalf("ListPauses after resume: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("pause history = %d rows, want 1: a resume deactivates rather than deletes",
			len(history))
	}
	cleared := history[0]
	if cleared.AcknowledgedAt == nil {
		t.Fatal("the resume was not recorded on the pause")
	}
	if cleared.AcknowledgedBy.Username != "colby" {
		t.Fatalf("acknowledged by %q, want the operator who did it",
			cleared.AcknowledgedBy.Username)
	}
	if cleared.Active(now) {
		t.Fatal("an acknowledged pause must not be active")
	}
}
