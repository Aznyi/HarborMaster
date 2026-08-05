package service_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Shutdown-bound tests.
//
// The failure these guard against is the one that does not show up as a failed
// test: a process that shuts down "successfully" but takes minutes to do it,
// because a detached goroutine ignored the signal. Every assertion below is
// therefore about TIME as well as outcome.

// GraceContext must not be cancelled the instant its parent is. That is the
// half that lets a transaction commit rather than being rolled back for no
// reason.
func TestGraceContextSurvivesCancellationForTheGracePeriod(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())

	ctx, cancel := service.GraceContext(parent, 500*time.Millisecond, time.Minute)
	defer cancel()

	cancelParent()

	// Immediately after the parent is cancelled, the child must still be live.
	select {
	case <-ctx.Done():
		t.Fatal("the grace context died with its parent; work in flight would be abandoned mid-transaction")
	case <-time.After(50 * time.Millisecond):
	}

	if err := ctx.Err(); err != nil {
		t.Errorf("ctx.Err() = %v during the grace period, want nil", err)
	}
}

// And it must not survive it forever. This is the half that stops a shutdown
// from outliving the orchestrator's deadline.
func TestGraceContextIsCancelledAfterTheGracePeriod(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())

	ctx, cancel := service.GraceContext(parent, 100*time.Millisecond, time.Minute)
	defer cancel()

	started := time.Now()
	cancelParent()

	select {
	case <-ctx.Done():
		elapsed := time.Since(started)
		if elapsed < 50*time.Millisecond {
			t.Errorf("cancelled after %s; the grace period was not honoured", elapsed)
		}
		if elapsed > 5*time.Second {
			t.Errorf("cancelled after %s; the grace period was not enforced", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the grace context was never cancelled; shutdown would hang")
	}
}

// The maximum bound applies even when the parent is never cancelled, so work
// that simply never finishes cannot run forever.
func TestGraceContextIsBoundedEvenWithoutCancellation(t *testing.T) {
	ctx, cancel := service.GraceContext(context.Background(), time.Hour, 100*time.Millisecond)
	defer cancel()

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Errorf("ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the maximum bound was not applied")
	}
}

// With no grace, the context is an ordinary child and dies with its parent.
func TestGraceContextWithNoGraceDiesWithItsParent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := service.GraceContext(parent, 0, time.Minute)
	defer cancel()

	cancelParent()

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a zero grace must cancel with the parent")
	}
}

// The watchdog goroutine must not leak. A helper that leaks one goroutine per
// reconciliation would be a slow memory leak in the component that runs most
// often.
func TestGraceContextLeaksNoGoroutines(t *testing.T) {
	before := stableGoroutineCount()

	for range 200 {
		parent, cancelParent := context.WithCancel(context.Background())
		ctx, cancel := service.GraceContext(parent, time.Hour, time.Hour)
		// The ordinary path: the work finishes and calls cancel while the
		// parent is still live.
		cancel()
		cancelParent()
		<-ctx.Done()
	}

	after := stableGoroutineCount()
	// A small allowance for runtime bookkeeping; a leak would be 200.
	if after > before+10 {
		t.Errorf("goroutines went from %d to %d; the watchdog is leaking", before, after)
	}
}

// Calling the returned cancel twice must be safe. A CancelFunc may be called
// any number of times, and closing a channel twice panics.
func TestGraceContextCancelIsIdempotent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	_, cancel := service.GraceContext(parent, time.Second, time.Minute)
	cancel()
	cancel()
	cancel()
}

func TestWaitGroupTimeoutReportsCompletionAndTimeout(t *testing.T) {
	t.Run("completes", func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(10 * time.Millisecond)
		}()

		if !service.WaitGroupTimeout(&wg, 5*time.Second) {
			t.Error("a group that finishes must report true")
		}
	})

	t.Run("times out", func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Add(1)
		defer wg.Done() // Released only after the assertion below.

		started := time.Now()
		if service.WaitGroupTimeout(&wg, 50*time.Millisecond) {
			t.Error("a group that does not finish must report false")
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Errorf("the wait took %s; the bound was not applied", elapsed)
		}
	})

	t.Run("zero timeout does not wait", func(t *testing.T) {
		var wg sync.WaitGroup
		if service.WaitGroupTimeout(&wg, 0) {
			t.Error("a zero timeout must not report success; it waits for nothing")
		}
	})
}

// ------------------------------------------------ service-level shutdown --

// A background refresh started by the manual endpoint must be waited for by
// Run, not abandoned. An untracked goroutine writing to a database the process
// is closing is how "graceful shutdown" produces an error in the log.
func TestInventoryRunWaitsForABackgroundRefresh(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(3)

	inventory := service.NewInventoryService(service.InventoryOptions{
		Runtime:       fake,
		Inventory:     db.Inventory,
		Containers:    db.Containers,
		Logger:        discardLogger(),
		Config:        config.Inventory{Enabled: true, Workers: 2, RefreshOnStartup: false},
		ShutdownGrace: 5 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		inventory.Run(ctx)
	}()

	started, _ := inventory.TriggerAsync(domain.TriggerManual)
	if !started {
		t.Fatal("the background refresh did not start")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return; the background refresh is not being waited for or not bounded")
	}

	// Having WAITED for it, the refresh must have completed rather than been
	// abandoned: a generation exists.
	generation, _, err := db.Inventory.CurrentGeneration(context.Background())
	if err != nil {
		t.Fatalf("CurrentGeneration: %v", err)
	}
	if generation == 0 {
		t.Error("the background refresh did not commit; Run returned before it finished")
	}
}

// A background refresh against a daemon that never answers must not hold
// shutdown open past the grace period.
func TestInventoryShutdownIsBoundedWhenDockerHangs(t *testing.T) {
	db := openDB(t)

	// A runtime whose Ping blocks until its context is cancelled, which is
	// what a daemon that has stopped answering looks like.
	hanging := &hangingRuntime{released: make(chan struct{})}
	defer close(hanging.released)

	inventory := service.NewInventoryService(service.InventoryOptions{
		Runtime:    hanging,
		Inventory:  db.Inventory,
		Containers: db.Containers,
		Logger:     discardLogger(),
		Config:     config.Inventory{Enabled: true, Workers: 2, RefreshOnStartup: false},
		// Short, so the test asserts the bound rather than waiting on it.
		ShutdownGrace: 200 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		inventory.Run(ctx)
	}()

	if started, _ := inventory.TriggerAsync(domain.TriggerManual); !started {
		t.Fatal("the background refresh did not start")
	}
	// Let it reach the hanging Ping before shutting down.
	eventually(t, 5*time.Second, "the refresh to reach the runtime", func() bool {
		return hanging.calls() > 0
	})

	started := time.Now()
	cancel()

	select {
	case <-done:
		// Two grace periods plus slack: awaitBackground waits once, cancels,
		// then waits once more.
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Errorf("shutdown took %s; the bound was not applied", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("shutdown never completed against a hanging daemon; this is the hang the bound exists to prevent")
	}
}

// hangingRuntime is a docker.Runtime whose Ping never returns on its own.
//
// It models the failure that matters at shutdown: a daemon that accepted the
// connection and then stopped answering, so the call blocks rather than
// erroring.
type hangingRuntime struct {
	mu       sync.Mutex
	pings    int
	released chan struct{}
}

func (r *hangingRuntime) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pings
}

func (r *hangingRuntime) Ping(ctx context.Context) (docker.Info, error) {
	r.mu.Lock()
	r.pings++
	r.mu.Unlock()

	select {
	case <-ctx.Done():
		return docker.Info{}, ctx.Err()
	case <-r.released:
		return docker.Info{APIVersion: "1.51"}, nil
	}
}

func (r *hangingRuntime) ListContainers(ctx context.Context) ([]domain.ContainerSummary, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *hangingRuntime) InspectContainer(ctx context.Context, id string) (*docker.Inspection, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *hangingRuntime) InspectImage(ctx context.Context, id string) (*domain.Image, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *hangingRuntime) ListNetworks(ctx context.Context) ([]domain.Network, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *hangingRuntime) ListVolumes(ctx context.Context) ([]domain.Volume, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *hangingRuntime) StreamEvents(ctx context.Context, since time.Time) (*docker.EventSubscription, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// stableGoroutineCount reads the goroutine count after giving the runtime a
// chance to reap goroutines that have already returned.
//
// Without the settle, a leak test races the scheduler and fails intermittently
// on a busy machine -- which is worse than not having the test, because a
// flaky guard gets deleted.
func stableGoroutineCount() int {
	previous := runtime.NumGoroutine()
	for range 20 {
		time.Sleep(10 * time.Millisecond)
		current := runtime.NumGoroutine()
		if current == previous {
			return current
		}
		previous = current
	}
	return previous
}
