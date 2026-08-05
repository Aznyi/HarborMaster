package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Event engine recovery across a restart.
//
// Docker replays nothing across a daemon restart and keeps only a bounded ring
// of recent events, so a resume is best effort by nature. What must be true is
// that the resume WINDOW is bounded: an unbounded one turns a long outage into
// a request for a very large replay, and the recovery path becomes the load
// spike.

// A restart must resume from where the last run stopped, not from nothing.
func TestEngineResumesFromPersistedStateAfterARestart(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)

	// State as a previous run would have left it: an event seen a minute ago.
	lastEvent := time.Now().UTC().Add(-time.Minute)
	if err := db.DockerEvents.SaveState(ctx, domain.LocalHostID, store.EngineState{
		LastEventAt:    &lastEvent,
		ReconnectCount: 7,
	}); err != nil {
		t.Fatalf("seed engine state: %v", err)
	}

	fake := fakeWith(1)
	engine := newEngineOnDB(t, db, fake)
	defer engine.stop(t)

	engine.waitConnected(t)

	since := fake.FirstEventsSince()
	if since.IsZero() {
		t.Fatal("the first connection after a restart asked for the whole stream from nothing; " +
			"persisted state exists and must be used")
	}
	if since.Before(lastEvent) {
		t.Errorf("resumed from %s, which is before the last event seen at %s", since, lastEvent)
	}

	// The reconnect count must have survived too, or "has this been flapping"
	// resets on every restart.
	if got := engine.engine.Status(ctx).Counters.Reconnects; got < 7 {
		t.Errorf("reconnects = %d, want at least the persisted 7", got)
	}
}

// A long outage must not produce an unbounded replay request.
//
// This is the bound that turns "HarborMaster was off for a month" from a
// request for a month of events into a request for the last hour, backed by
// the full reconciliation that every connection performs anyway.
func TestEngineClampsTheResumeWindowAfterALongOutage(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)

	// An event from long before any daemon would still hold it.
	ancient := time.Now().UTC().Add(-30 * 24 * time.Hour)
	if err := db.DockerEvents.SaveState(ctx, domain.LocalHostID, store.EngineState{
		LastEventAt: &ancient,
	}); err != nil {
		t.Fatalf("seed engine state: %v", err)
	}

	fake := fakeWith(1)
	engine := newEngineOnDB(t, db, fake)
	defer engine.stop(t)

	engine.waitConnected(t)

	since := fake.FirstEventsSince()
	if since.IsZero() {
		t.Fatal("no resume point was used at all")
	}
	// The clamp is one hour; allow generous slack for test scheduling while
	// still failing an unbounded window by a factor of hundreds.
	oldest := time.Now().UTC().Add(-2 * time.Hour)
	if since.Before(oldest) {
		t.Errorf("resumed from %s, more than two hours ago; the replay window is not bounded "+
			"and a long outage would ask the daemon for its whole ring", since)
	}
}

// A first-ever start has nothing to resume from and must ask for nothing,
// rather than inventing a window.
func TestAFirstStartResumesFromNothing(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)

	engine := newEngineOnDB(t, db, fake)
	defer engine.stop(t)

	engine.waitConnected(t)

	if since := fake.FirstEventsSince(); !since.IsZero() {
		t.Errorf("a first start asked to resume from %s; there is nothing to resume from", since)
	}
}

// Every connection must request a full reconciliation, because the resume is
// best effort and a full sweep is the only thing that can be trusted.
func TestEveryConnectionRequestsAReconciliation(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(2)

	engine := newEngineOnDB(t, db, fake)
	defer engine.stop(t)

	engine.waitConnected(t)

	eventually(t, 5*time.Second, "a reconciliation after connecting", func() bool {
		return engine.engine.Status(context.Background()).Counters.FullReconciliations > 0
	})
}

// newEngineOnDB builds an engine against an EXISTING database, so a test can
// seed the persisted state a previous run would have left.
func newEngineOnDB(t *testing.T, db *store.DB, fake *docker.Fake) *engineHarness {
	t.Helper()

	cfg := testEventConfig()

	inventory := service.NewInventoryService(service.InventoryOptions{
		Runtime:    fake,
		Inventory:  db.Inventory,
		Containers: db.Containers,
		Logger:     discardLogger(),
		Config:     config.Inventory{Enabled: true, Workers: 2},
	})

	engine := service.NewEventService(service.EventOptions{
		Runtime:       fake,
		Events:        db.DockerEvents,
		Inventory:     inventory,
		Logger:        discardLogger(),
		Config:        cfg,
		ShutdownGrace: time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.Run(ctx)
	}()

	harness := &engineHarness{
		engine: engine, inventory: inventory, fake: fake, db: db,
		cancel: cancel, done: done,
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the event engine did not shut down; a goroutine is stuck")
		}
	})
	return harness
}
