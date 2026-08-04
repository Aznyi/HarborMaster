package service_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Event engine tests.
//
// Nothing here sleeps to wait for a state change. Timing-dependent behaviour --
// backoff, debounce, reconnection -- is driven through injected clock and
// jitter functions, and everything else is asserted with eventually(), which
// polls a condition on a short interval and fails fast. A test that sleeps for
// a fixed duration is either slow, flaky, or both, and it hides the concurrency
// defect it was meant to catch.

// pollInterval is short enough that a passing test finishes quickly and long
// enough not to spin a core.
const pollInterval = 2 * time.Millisecond

// eventually polls until condition holds or the deadline passes.
func eventually(t *testing.T, timeout time.Duration, describe string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("timed out waiting for %s", describe)
}

// testEventConfig is a configuration tuned for tests: tiny intervals so the
// engine reacts within a poll or two, and reconciliation and pruning pushed far
// enough out that they only fire when a test asks for them.
func testEventConfig() config.Events {
	return config.Events{
		Enabled:           true,
		ReconnectInitial:  time.Millisecond,
		ReconnectMax:      10 * time.Millisecond,
		ReconnectFactor:   2,
		BufferSize:        64,
		BatchSize:         8,
		BatchFlush:        5 * time.Millisecond,
		DedupWindow:       time.Minute,
		RefreshDebounce:   5 * time.Millisecond,
		ReconcileInterval: time.Hour,
		RetentionAge:      0,
		RetentionCount:    0,
		PruneInterval:     time.Hour,
		StreamSubscribers: 4,
		StreamBuffer:      8,
		StreamReplay:      50,
		StreamHeartbeat:   time.Hour,
	}
}

// engineHarness bundles an engine with everything it was built from, so a test
// can drive Docker and inspect persistence.
type engineHarness struct {
	engine    *service.EventService
	inventory *service.InventoryService
	fake      *docker.Fake
	db        *store.DB
	cancel    context.CancelFunc
	done      chan struct{}
}

// newEngine builds and starts an event engine.
//
// The returned harness stops the engine on cleanup and asserts Run returned,
// which is what proves shutdown completes rather than merely being requested.
func newEngine(t *testing.T, tweak ...func(*config.Events)) *engineHarness {
	t.Helper()
	return newEngineWith(t, fakeWith(2), nil, tweak...)
}

func newEngineWith(t *testing.T, fake *docker.Fake, jitter func(time.Duration) time.Duration, tweak ...func(*config.Events)) *engineHarness {
	t.Helper()

	db := openDB(t)
	cfg := testEventConfig()
	for _, apply := range tweak {
		apply(&cfg)
	}

	inventory := service.NewInventoryService(service.InventoryOptions{
		Runtime:    fake,
		Inventory:  db.Inventory,
		Containers: db.Containers,
		Logger:     discardLogger(),
		Config:     config.Inventory{Enabled: true, Workers: 4},
	})

	engine := service.NewEventService(service.EventOptions{
		Runtime:   fake,
		Events:    db.DockerEvents,
		Inventory: inventory,
		Logger:    discardLogger(),
		Config:    cfg,
		Jitter:    jitter,
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

// stop shuts the engine down and waits for Run to return.
func (h *engineHarness) stop(t *testing.T) {
	t.Helper()
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// seedInventory commits one full refresh, so targeted writes have a generation
// to join.
func (h *engineHarness) seedInventory(t *testing.T) {
	t.Helper()
	if _, err := h.inventory.Refresh(context.Background(), domain.TriggerStartup); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}
}

// emit pushes an event into the live subscription, failing the test if the
// engine is not reading.
func (h *engineHarness) emit(t *testing.T, event domain.DockerEvent) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if !h.fake.Emit(ctx, event) {
		t.Fatal("the engine did not accept an event; is the stream connected?")
	}
}

// waitConnected blocks until the engine reports a live stream.
func (h *engineHarness) waitConnected(t *testing.T) {
	t.Helper()
	eventually(t, 5*time.Second, "the event stream to connect", func() bool {
		return h.engine.Status(context.Background()).State == domain.ConnStateConnected
	})
}

// containerEvent builds a container event with a distinct fingerprint.
func containerEvent(fingerprint, containerID string, action domain.DockerEventAction) domain.DockerEvent {
	return domain.DockerEvent{
		Fingerprint: fingerprint,
		HostID:      domain.LocalHostID,
		Type:        domain.EventTypeContainer,
		Action:      action,
		ActorID:     containerID,
		ActorName:   containerID,
		Scope:       "local",
		Attributes:  map[string]string{"name": containerID},
		DockerTime:  time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC),
	}
}

// ------------------------------------------------------------- lifecycle --

func TestEngineConnectsAtStartup(t *testing.T) {
	harness := newEngine(t)
	harness.waitConnected(t)

	status := harness.engine.Status(context.Background())
	if !status.Enabled {
		t.Error("the engine must report itself enabled")
	}
	if status.ConnectedSince == nil {
		t.Error("connectedSince must be set once the stream is live")
	}
	if status.QueueCapacity != testEventConfig().BufferSize {
		t.Errorf("queueCapacity = %d, want the configured buffer size", status.QueueCapacity)
	}
}

// A daemon that is down when HarborMaster starts is an ordinary condition on a
// host where Docker comes up second. It must produce retries and a degraded
// status, never a crash.
func TestEngineSurvivesDockerUnavailableAtStartup(t *testing.T) {
	fake := fakeWith(1)
	fake.SetStreamErr(errors.New("cannot connect to the docker daemon"))

	harness := newEngineWith(t, fake, nil)

	eventually(t, 5*time.Second, "the engine to retry a failed connection", func() bool {
		return fake.Subscriptions() >= 2
	})

	status := harness.engine.Status(context.Background())
	if status.State == domain.ConnStateConnected {
		t.Error("the engine must not report connected while Docker is unavailable")
	}
	// The failure must be sanitised, never the raw daemon error.
	if strings.Contains(status.LastError, "cannot connect to the docker daemon") {
		t.Errorf("lastError = %q, want a sanitised summary", status.LastError)
	}

	degraded, reason := harness.engine.Degraded()
	if !degraded {
		t.Error("a disconnected stream must degrade health")
	}
	if reason == "" {
		t.Error("a degraded engine must explain itself")
	}
}

// A disabled engine connects to nothing, reports disabled, and does not degrade
// health: running on periodic reconciliation alone is a supported mode.
func TestDisabledEngineDoesNothing(t *testing.T) {
	fake := fakeWith(1)
	harness := newEngineWith(t, fake, nil, func(cfg *config.Events) {
		cfg.Enabled = false
	})

	// Run returns immediately when disabled.
	harness.stop(t)

	if fake.Subscriptions() != 0 {
		t.Errorf("a disabled engine subscribed %d times, want 0", fake.Subscriptions())
	}

	status := harness.engine.Status(context.Background())
	if status.Enabled {
		t.Error("status must report the engine disabled")
	}
	if degraded, _ := harness.engine.Degraded(); degraded {
		t.Error("a disabled engine must not degrade health")
	}
	if _, err := harness.engine.Subscribe(); !errors.Is(err, service.ErrEventsDisabled) {
		t.Errorf("Subscribe on a disabled engine = %v, want ErrEventsDisabled", err)
	}
}

// A daemon that is not answering must never be reported as connected, and must
// not cause a reconciliation attempt on every retry.
//
// The SDK's Events call reports a failed HTTP request on the error channel
// rather than returning it, so "subscribing succeeded" is not evidence the
// stream opened. This is the regression test for that: a live smoke run against
// a host with no Docker socket logged "docker event stream connected" and then
// a failed reconciliation, once per backoff cycle.
func TestUnreachableDaemonIsNeverReportedAsConnected(t *testing.T) {
	fake := fakeWith(1)
	fake.SetPingErr(errors.New("cannot connect to the docker daemon"))
	fake.SetStreamErr(errors.New("cannot connect to the docker daemon"))

	harness := newEngineWith(t, fake, nil)

	// Let several backoff cycles elapse.
	eventually(t, 5*time.Second, "several connection attempts", func() bool {
		return harness.engine.Status(context.Background()).Counters.Reconnects >= 3
	})

	status := harness.engine.Status(context.Background())
	if status.State == domain.ConnStateConnected {
		t.Error("an unreachable daemon must never be reported as connected")
	}
	if status.ConnectedSince != nil {
		t.Error("connectedSince must stay unset while the daemon is unreachable")
	}
	if status.LastConnectedAt != nil {
		t.Error("lastConnectedAt must stay unset; no connection ever opened")
	}
	// The important one: no wasted reconciliation per failed attempt.
	if got := status.Counters.FullReconciliations; got != 0 {
		t.Errorf("fullReconciliations = %d, want 0; a failed connection must not reconcile", got)
	}
}

// The Ping gate must not stop a healthy daemon from connecting.
func TestReachableDaemonStillConnects(t *testing.T) {
	harness := newEngine(t)
	harness.waitConnected(t)

	status := harness.engine.Status(context.Background())
	if status.ConnectedSince == nil || status.LastConnectedAt == nil {
		t.Error("a healthy daemon must record its connection")
	}
}

func TestEngineReconnectsAfterStreamLoss(t *testing.T) {
	harness := newEngine(t)
	harness.waitConnected(t)

	if !harness.fake.FailStream(errors.New("connection reset by peer")) {
		t.Fatal("could not fail the stream")
	}

	eventually(t, 5*time.Second, "the engine to reconnect", func() bool {
		return harness.fake.Subscriptions() >= 2
	})
	harness.waitConnected(t)

	if got := harness.engine.Status(context.Background()).Counters.Reconnects; got < 1 {
		t.Errorf("reconnectCount = %d, want at least 1", got)
	}
}

// A reconnect must resume from where the stream stopped, so the daemon can
// replay what it still holds. Best effort, which is why reconnection also
// reconciles -- but asking for nothing would be strictly worse.
func TestReconnectResumesFromTheLastEvent(t *testing.T) {
	harness := newEngine(t)
	harness.waitConnected(t)

	harness.emit(t, containerEvent("fp-resume", "container-000", domain.ActionStart))
	eventually(t, 5*time.Second, "the event to be observed", func() bool {
		return harness.engine.Status(context.Background()).LastEventAt != nil
	})

	harness.fake.FailStream(errors.New("stream closed"))
	eventually(t, 5*time.Second, "the engine to resubscribe", func() bool {
		return harness.fake.Subscriptions() >= 2
	})

	harness.fake.SinceValues = append([]time.Time(nil), harness.fake.SinceValues...)
	if len(harness.fake.SinceValues) < 2 {
		t.Fatalf("recorded %d subscriptions, want at least 2", len(harness.fake.SinceValues))
	}
	if harness.fake.SinceValues[0].IsZero() == false {
		t.Error("the first subscription must start from now, not from the epoch")
	}
	if harness.fake.SinceValues[1].IsZero() {
		t.Error("a reconnect must ask the daemon to resume rather than start fresh")
	}
}

// The backoff must grow, be capped, and be jittered. The jitter function is
// injected so the sequence of nominal delays is directly observable.
func TestBackoffGrowsAndIsCapped(t *testing.T) {
	var (
		mu      sync.Mutex
		delays  []time.Duration
		fake    = fakeWith(1)
		observe = func(delay time.Duration) time.Duration {
			mu.Lock()
			delays = append(delays, delay)
			mu.Unlock()
			// Collapse the wait so the test does not spend real time on it. The
			// SEQUENCE of nominal delays is what is under test, not the sleeping.
			return 0
		}
	)
	fake.SetStreamErr(errors.New("daemon is down"))

	newEngineWith(t, fake, observe, func(cfg *config.Events) {
		cfg.ReconnectInitial = 10 * time.Millisecond
		cfg.ReconnectMax = 40 * time.Millisecond
		cfg.ReconnectFactor = 2
	})

	eventually(t, 5*time.Second, "several backoff delays", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(delays) >= 5
	})

	mu.Lock()
	observed := append([]time.Duration(nil), delays...)
	mu.Unlock()

	want := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		40 * time.Millisecond, // capped
		40 * time.Millisecond,
	}
	for i, expected := range want {
		if observed[i] != expected {
			t.Errorf("delay %d = %s, want %s (sequence %v)", i, observed[i], expected, observed[:len(want)])
		}
	}
}

// The default jitter must stay within [50%, 100%] of the nominal delay. Full
// jitter down to zero would retry instantly against a daemon that is down,
// which is what the backoff exists to prevent.
func TestDefaultJitterStaysWithinBounds(t *testing.T) {
	fake := fakeWith(1)
	fake.SetStreamErr(errors.New("down"))

	// A nil jitter selects the production one.
	harness := newEngineWith(t, fake, nil, func(cfg *config.Events) {
		cfg.ReconnectInitial = 4 * time.Millisecond
		cfg.ReconnectMax = 4 * time.Millisecond
	})

	eventually(t, 5*time.Second, "a backoff to be scheduled", func() bool {
		return harness.engine.Status(context.Background()).CurrentBackoffMS >= 0 &&
			fake.Subscriptions() >= 2
	})

	status := harness.engine.Status(context.Background())
	if status.CurrentBackoffMS > 4 {
		t.Errorf("currentBackoffMs = %d, want no more than the 4ms maximum", status.CurrentBackoffMS)
	}
}

// Shutdown must not wait out a backoff delay. A sixty-second maximum would
// otherwise add sixty seconds to every shutdown.
func TestShutdownDoesNotWaitForTheBackoff(t *testing.T) {
	fake := fakeWith(1)
	fake.SetStreamErr(errors.New("down"))

	harness := newEngineWith(t, fake, func(time.Duration) time.Duration {
		// A backoff far longer than any acceptable shutdown.
		return 30 * time.Second
	}, func(cfg *config.Events) {
		cfg.ReconnectInitial = 30 * time.Second
		cfg.ReconnectMax = 30 * time.Second
	})

	eventually(t, 5*time.Second, "the engine to enter backoff", func() bool {
		return harness.engine.Status(context.Background()).State == domain.ConnStateReconnecting
	})

	start := time.Now()
	harness.stop(t)

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("shutdown took %s; cancellation must interrupt the backoff wait", elapsed)
	}
}

func TestCleanShutdownClosesTheDockerStream(t *testing.T) {
	harness := newEngine(t)
	harness.waitConnected(t)

	harness.stop(t)

	if open := harness.fake.OpenStreams(); open != 0 {
		t.Errorf("%d docker subscriptions still open after shutdown; they leak goroutines", open)
	}
	if state := harness.engine.Status(context.Background()).State; state != domain.ConnStateStopped {
		t.Errorf("state = %q after shutdown, want stopped", state)
	}
}

// ------------------------------------------------------------ processing --

func TestEventsArePersistedInObservedOrder(t *testing.T) {
	harness := newEngine(t)
	harness.waitConnected(t)

	for i := range 5 {
		harness.emit(t, containerEvent(fmt.Sprintf("fp-order-%d", i), "container-000", domain.ActionStart))
	}

	eventually(t, 5*time.Second, "all events to be persisted", func() bool {
		count, _ := harness.db.DockerEvents.Count(context.Background())
		return count == 5
	})

	events, _, err := harness.db.DockerEvents.List(context.Background(), store.DockerEventFilter{
		Sort: "sequence", Direction: store.SortAsc, Page: store.Page{Limit: 50},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i, event := range events {
		want := fmt.Sprintf("fp-order-%d", i)
		if event.Fingerprint != want {
			t.Errorf("event %d = %q, want %q; observation order must be preserved",
				i, event.Fingerprint, want)
		}
	}
}

// The same fingerprint inside the window is a duplicate delivery. It must be
// counted, not stored, and must not schedule a second refresh.
func TestDuplicateEventsAreSuppressed(t *testing.T) {
	harness := newEngine(t)
	harness.waitConnected(t)

	duplicate := containerEvent("fp-same", "container-000", domain.ActionStart)
	for range 4 {
		harness.emit(t, duplicate)
	}

	eventually(t, 5*time.Second, "the duplicates to be counted", func() bool {
		return harness.engine.Status(context.Background()).Counters.Deduplicated >= 3
	})
	// Persistence is batched, so the count lags the counter by up to one flush
	// interval. Waiting for the write rather than reading immediately is what
	// keeps this deterministic instead of racing the batch timer.
	eventually(t, 5*time.Second, "the surviving event to be persisted", func() bool {
		count, _ := harness.db.DockerEvents.Count(context.Background())
		return count == 1
	})

	// Give a further flush interval a chance to reveal a wrongly stored
	// duplicate before concluding only one was written.
	time.Sleep(3 * testEventConfig().BatchFlush)
	if count, _ := harness.db.DockerEvents.Count(context.Background()); count != 1 {
		t.Errorf("stored %d events, want 1; duplicates must not be persisted", count)
	}
}

// A genuinely repeated action at a different instant is a real state
// transition. Suppressing it would lose information.
func TestRepeatedActionsAtDifferentTimesAreNotSuppressed(t *testing.T) {
	harness := newEngine(t)
	harness.waitConnected(t)

	harness.emit(t, containerEvent("fp-start-1", "container-000", domain.ActionStart))
	harness.emit(t, containerEvent("fp-start-2", "container-000", domain.ActionStart))

	eventually(t, 5*time.Second, "both starts to be stored", func() bool {
		count, _ := harness.db.DockerEvents.Count(context.Background())
		return count == 2
	})

	if got := harness.engine.Status(context.Background()).Counters.Deduplicated; got != 0 {
		t.Errorf("deduplicated = %d, want 0; distinct events must not be merged", got)
	}
}

func TestEventsRecordTheirClassification(t *testing.T) {
	harness := newEngine(t)
	harness.waitConnected(t)

	harness.emit(t, containerEvent("fp-classify", "container-000", domain.ActionStart))
	// An exec has no inventory consequence and must be recorded as ignored.
	harness.emit(t, containerEvent("fp-exec", "container-000", "exec_create"))

	eventually(t, 5*time.Second, "both events to be stored", func() bool {
		count, _ := harness.db.DockerEvents.Count(context.Background())
		return count == 2
	})

	events, _, err := harness.db.DockerEvents.List(context.Background(), store.DockerEventFilter{
		Sort: "sequence", Direction: store.SortAsc, Page: store.Page{Limit: 10},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if events[0].Result != domain.ResultProcessed ||
		events[0].RefreshRequested != domain.RefreshContainer {
		t.Errorf("start recorded as %s/%s, want processed/container",
			events[0].Result, events[0].RefreshRequested)
	}
	if events[1].Result != domain.ResultIgnored ||
		events[1].RefreshRequested != domain.RefreshNone {
		t.Errorf("exec_create recorded as %s/%s, want ignored/none",
			events[1].Result, events[1].RefreshRequested)
	}
}

// --------------------------------------------------------------- refresh --

func TestContainerEventTriggersATargetedRefresh(t *testing.T) {
	harness := newEngine(t)
	harness.seedInventory(t)
	harness.waitConnected(t)

	// Through the accessor, not the field: the engine inspects from its worker
	// goroutines while this test reads, which is a data race on the bare field.
	before := harness.fake.InspectCallCount()

	harness.emit(t, containerEvent("fp-targeted", "container-000", domain.ActionStart))

	eventually(t, 5*time.Second, "a targeted refresh to run", func() bool {
		return harness.engine.Status(context.Background()).Counters.TargetedRefreshes >= 1
	})

	if harness.fake.InspectCallCount() <= before {
		t.Error("a targeted refresh must re-inspect the container rather than trust the event")
	}
	// A targeted refresh must not sweep the whole host.
	if got := harness.engine.Status(context.Background()).Counters.FullReconciliations; got > 1 {
		t.Errorf("fullReconciliations = %d; a container event must not force a sweep", got)
	}
}

// A burst representing one lifecycle transition must collapse into a single
// refresh. This is the whole point of the debounce window.
func TestRapidEventsCoalesceIntoOneRefresh(t *testing.T) {
	harness := newEngine(t, func(cfg *config.Events) {
		// Long enough that the whole burst lands inside one window.
		cfg.RefreshDebounce = 150 * time.Millisecond
	})
	harness.seedInventory(t)
	harness.waitConnected(t)

	// `docker restart` produces roughly this.
	for i, action := range []domain.DockerEventAction{
		domain.ActionKill, domain.ActionDie, domain.ActionStop,
		domain.ActionStart, domain.ActionHealthStatus,
	} {
		harness.emit(t, containerEvent(fmt.Sprintf("fp-burst-%d", i), "container-000", action))
	}

	eventually(t, 5*time.Second, "the coalesced refresh to run", func() bool {
		return harness.engine.Status(context.Background()).Counters.TargetedRefreshes >= 1
	})

	// Give the window time to have produced a second refresh if it were going to.
	time.Sleep(200 * time.Millisecond)

	if got := harness.engine.Status(context.Background()).Counters.TargetedRefreshes; got != 1 {
		t.Errorf("targetedRefreshes = %d, want 1; five events for one container must coalesce", got)
	}
}

// Events for different containers must NOT coalesce with each other.
func TestDifferentContainersRefreshSeparately(t *testing.T) {
	harness := newEngine(t)
	harness.seedInventory(t)
	harness.waitConnected(t)

	harness.emit(t, containerEvent("fp-a", "container-000", domain.ActionStart))
	harness.emit(t, containerEvent("fp-b", "container-001", domain.ActionStart))

	eventually(t, 5*time.Second, "both containers to refresh", func() bool {
		return harness.engine.Status(context.Background()).Counters.TargetedRefreshes >= 2
	})
}

// A destroy is the one terminal conclusion an event carries. It marks the
// container absent rather than re-inspecting something that no longer exists.
func TestDestroyMarksTheContainerAbsent(t *testing.T) {
	harness := newEngine(t)
	harness.seedInventory(t)
	harness.waitConnected(t)

	harness.emit(t, containerEvent("fp-destroy", "container-000", domain.ActionDestroy))

	eventually(t, 5*time.Second, "the container to be marked absent", func() bool {
		detail, err := harness.db.Containers.Get(context.Background(), "container-000")
		return err == nil && !detail.Overview.Present
	})

	// The row is retained, not deleted, so its history survives.
	if _, err := harness.db.Containers.Get(context.Background(), "container-000"); err != nil {
		t.Errorf("the container row must be retained after a destroy: %v", err)
	}
}

// A daemon reload means the configuration under everything changed. Nothing
// narrower than a full sweep is honest.
func TestDaemonReloadForcesAFullReconciliation(t *testing.T) {
	harness := newEngine(t)
	harness.seedInventory(t)
	harness.waitConnected(t)

	before := harness.engine.Status(context.Background()).Counters.FullReconciliations

	harness.emit(t, domain.DockerEvent{
		Fingerprint: "fp-reload",
		HostID:      domain.LocalHostID,
		Type:        domain.EventTypeDaemon,
		Action:      domain.ActionReload,
		Scope:       "local",
		Attributes:  map[string]string{},
		DockerTime:  time.Now().UTC(),
	})

	eventually(t, 5*time.Second, "the reconciliation to run", func() bool {
		return harness.engine.Status(context.Background()).Counters.FullReconciliations > before
	})
}

// A full reconciliation covers every targeted request, so it must discard them
// rather than doing the same work twice.
func TestFullReconciliationOutranksTargetedRequests(t *testing.T) {
	harness := newEngine(t, func(cfg *config.Events) {
		// Long enough that the targeted requests are still pending when the
		// reload arrives.
		cfg.RefreshDebounce = 300 * time.Millisecond
	})
	harness.seedInventory(t)
	harness.waitConnected(t)

	for i := range 3 {
		harness.emit(t, containerEvent(fmt.Sprintf("fp-pending-%d", i),
			fmt.Sprintf("container-%03d", i), domain.ActionStart))
	}

	harness.emit(t, domain.DockerEvent{
		Fingerprint: "fp-reload-precedence",
		HostID:      domain.LocalHostID,
		Type:        domain.EventTypeDaemon,
		Action:      domain.ActionReload,
		Scope:       "local",
		Attributes:  map[string]string{},
		DockerTime:  time.Now().UTC(),
	})

	eventually(t, 5*time.Second, "the reconciliation to run", func() bool {
		return harness.engine.Status(context.Background()).Counters.FullReconciliations >= 1
	})

	// The pending targeted work must have been dropped, not run afterwards.
	time.Sleep(400 * time.Millisecond)
	status := harness.engine.Status(context.Background())
	if status.Counters.TargetedRefreshes > 0 {
		t.Errorf("targetedRefreshes = %d; a full sweep must discard pending targeted work",
			status.Counters.TargetedRefreshes)
	}
	if status.PendingRefreshes != 0 {
		t.Errorf("pendingRefreshes = %d, want 0", status.PendingRefreshes)
	}
}

// A container event with no actor cannot be targeted. Escalating is correct;
// guessing would not be.
func TestUnmappableEventEscalatesToReconciliation(t *testing.T) {
	harness := newEngine(t)
	harness.seedInventory(t)
	harness.waitConnected(t)

	before := harness.engine.Status(context.Background()).Counters.FullReconciliations

	event := containerEvent("fp-no-actor", "", domain.ActionStart)
	harness.emit(t, event)

	eventually(t, 5*time.Second, "the escalation to run", func() bool {
		return harness.engine.Status(context.Background()).Counters.FullReconciliations > before
	})

	events, _, err := harness.db.DockerEvents.List(context.Background(), store.DockerEventFilter{
		Page: store.Page{Limit: 10},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) == 0 || events[0].Result != domain.ResultWarning {
		t.Errorf("an unmappable event must be recorded as a warning, got %v", events)
	}
	if events[0].Error == "" {
		t.Error("a warning must carry an operator-facing reason")
	}
}

// A queue overflow means events were lost, so nothing narrower than a
// reconciliation is honest about the gap.
func TestQueueOverflowRequestsReconciliation(t *testing.T) {
	// A one-slot queue and a slow persistence path make an overflow reachable
	// deterministically rather than by racing.
	harness := newEngine(t, func(cfg *config.Events) {
		cfg.BufferSize = 1
		cfg.BatchSize = 1
		cfg.BatchFlush = time.Second
	})
	harness.seedInventory(t)
	harness.waitConnected(t)

	// Push far more than the queue can hold. Emit is best effort here: the
	// point is to overflow, and a refused send means the reader is already
	// backed up, which is the same condition.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := range 200 {
		harness.fake.Emit(ctx, containerEvent(fmt.Sprintf("fp-flood-%03d", i),
			fmt.Sprintf("container-%03d", i%2), domain.ActionStart))
	}

	eventually(t, 10*time.Second, "the overflow to be counted", func() bool {
		return harness.engine.Status(context.Background()).Counters.Dropped > 0
	})
	eventually(t, 10*time.Second, "the overflow reconciliation", func() bool {
		return harness.engine.Status(context.Background()).Counters.FullReconciliations >= 1
	})
}

// ------------------------------------------------------------ broadcast --

func TestSubscribersReceiveLiveEvents(t *testing.T) {
	harness := newEngine(t)
	harness.waitConnected(t)

	subscription, err := harness.engine.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer subscription.Close()

	harness.emit(t, containerEvent("fp-live", "container-000", domain.ActionStart))

	select {
	case event := <-subscription.Events:
		if event.Fingerprint != "fp-live" {
			t.Errorf("received %q, want fp-live", event.Fingerprint)
		}
		// Only persisted events are broadcast, so the sequence is assigned.
		if event.Sequence == 0 {
			t.Error("a broadcast event must carry its assigned sequence")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the subscriber received no event")
	}
}

// A subscriber that stops reading must not stall event processing for anyone
// else. This is the single most important property of the broadcaster.
func TestSlowSubscriberDoesNotBlockProcessing(t *testing.T) {
	harness := newEngine(t, func(cfg *config.Events) {
		cfg.StreamBuffer = 2
	})
	harness.waitConnected(t)

	// Subscribed and deliberately never read from.
	slow, err := harness.engine.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer slow.Close()

	const total = 20
	for i := range total {
		harness.emit(t, containerEvent(fmt.Sprintf("fp-slow-%02d", i), "container-000", domain.ActionStart))
	}

	// Processing must complete regardless of the stalled subscriber.
	eventually(t, 5*time.Second, "every event to be persisted despite a stalled subscriber", func() bool {
		count, _ := harness.db.DockerEvents.Count(context.Background())
		return count == total
	})
}

func TestSubscriberLimitIsEnforced(t *testing.T) {
	harness := newEngine(t, func(cfg *config.Events) {
		cfg.StreamSubscribers = 2
	})

	first, err := harness.engine.Subscribe()
	if err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}
	defer first.Close()

	second, err := harness.engine.Subscribe()
	if err != nil {
		t.Fatalf("second Subscribe: %v", err)
	}

	if _, err := harness.engine.Subscribe(); !errors.Is(err, service.ErrTooManySubscribers) {
		t.Fatalf("third Subscribe = %v, want ErrTooManySubscribers", err)
	}

	// Closing one must free its slot, or a client that disconnects would
	// permanently consume capacity.
	second.Close()
	third, err := harness.engine.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe after a close: %v", err)
	}
	third.Close()
}

// A departing subscriber's channel must close, or its handler would block
// forever and hold the HTTP server's drain open.
func TestSubscriptionCloseIsIdempotent(t *testing.T) {
	harness := newEngine(t)

	subscription, err := harness.engine.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	subscription.Close()
	// A second close must not panic on an already-closed channel.
	subscription.Close()

	select {
	case _, open := <-subscription.Events:
		if open {
			t.Error("a closed subscription must not deliver events")
		}
	case <-time.After(time.Second):
		t.Error("a closed subscription's channel must be closed")
	}
}

// Shutdown must release subscribers so their handlers return.
func TestShutdownReleasesSubscribers(t *testing.T) {
	harness := newEngine(t)
	harness.waitConnected(t)

	subscription, err := harness.engine.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer subscription.Close()

	harness.stop(t)

	select {
	case _, open := <-subscription.Events:
		if open {
			t.Error("shutdown must close subscriber channels")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown left a subscriber blocked; the http drain would hang")
	}
}

// ------------------------------------------------------------ retention --

func TestPruneNowEnforcesRetention(t *testing.T) {
	harness := newEngine(t, func(cfg *config.Events) {
		cfg.RetentionCount = 3
	})
	harness.waitConnected(t)

	for i := range 10 {
		harness.emit(t, containerEvent(fmt.Sprintf("fp-ret-%02d", i), "container-000", domain.ActionStart))
	}
	eventually(t, 5*time.Second, "the events to be stored", func() bool {
		count, _ := harness.db.DockerEvents.Count(context.Background())
		return count == 10
	})

	removed, err := harness.engine.PruneNow(context.Background())
	if err != nil {
		t.Fatalf("PruneNow: %v", err)
	}
	if removed != 7 {
		t.Errorf("removed %d events, want 7", removed)
	}

	status := harness.engine.Status(context.Background())
	if status.StoredEvents != 3 {
		t.Errorf("storedEvents = %d, want 3", status.StoredEvents)
	}
	if status.Retention.MaxCount != 3 {
		t.Errorf("retention.maxCount = %d, want the configured value", status.Retention.MaxCount)
	}
}

// ----------------------------------------------------------- degradation --

func TestConnectedEngineIsNotDegraded(t *testing.T) {
	harness := newEngine(t)
	harness.waitConnected(t)

	eventually(t, 5*time.Second, "the engine to report healthy", func() bool {
		degraded, _ := harness.engine.Degraded()
		return !degraded
	})
}

// ------------------------------------------------------- classification --

// The mapping from event to action is the part most likely to be wrong in a way
// nothing else catches, so it gets its own table.
func TestClassifyEvent(t *testing.T) {
	tests := []struct {
		name        string
		event       domain.DockerEvent
		wantRequest domain.RefreshRequest
		wantResult  domain.EventProcessingResult
		wantTarget  string
	}{
		{
			name:        "container start",
			event:       containerEvent("f", "abc", domain.ActionStart),
			wantRequest: domain.RefreshContainer, wantResult: domain.ResultProcessed, wantTarget: "abc",
		},
		{
			name:        "container die",
			event:       containerEvent("f", "abc", domain.ActionDie),
			wantRequest: domain.RefreshContainer, wantResult: domain.ResultProcessed, wantTarget: "abc",
		},
		{
			name:        "container health status",
			event:       containerEvent("f", "abc", domain.ActionHealthStatus),
			wantRequest: domain.RefreshContainer, wantResult: domain.ResultProcessed, wantTarget: "abc",
		},
		{
			name:        "container oom",
			event:       containerEvent("f", "abc", domain.ActionOOM),
			wantRequest: domain.RefreshContainer, wantResult: domain.ResultProcessed, wantTarget: "abc",
		},
		{
			name:        "container destroy",
			event:       containerEvent("f", "abc", domain.ActionDestroy),
			wantRequest: domain.RefreshContainerAbsent, wantResult: domain.ResultProcessed, wantTarget: "abc",
		},
		{
			name:        "container exec is ignored",
			event:       containerEvent("f", "abc", "exec_start"),
			wantRequest: domain.RefreshNone, wantResult: domain.ResultIgnored,
		},
		{
			name:        "container event with no actor escalates",
			event:       containerEvent("f", "", domain.ActionStart),
			wantRequest: domain.RefreshFull, wantResult: domain.ResultWarning,
		},
		{
			name: "image pull",
			event: domain.DockerEvent{
				Type: domain.EventTypeImage, Action: domain.ActionPull,
				ActorID: "nginx:1.27", Attributes: map[string]string{},
			},
			wantRequest: domain.RefreshImage, wantResult: domain.ResultProcessed, wantTarget: "nginx:1.27",
		},
		{
			name: "image delete is a catalog pass",
			event: domain.DockerEvent{
				Type: domain.EventTypeImage, Action: domain.ActionDelete,
				ActorID: "sha256:gone", Attributes: map[string]string{},
			},
			wantRequest: domain.RefreshImageCatalog, wantResult: domain.ResultProcessed,
		},
		{
			name: "network create",
			event: domain.DockerEvent{
				Type: domain.EventTypeNetwork, Action: domain.ActionCreate,
				ActorID: "net1", Attributes: map[string]string{},
			},
			wantRequest: domain.RefreshNetworks, wantResult: domain.ResultProcessed,
		},
		{
			name: "network connect prefers the container",
			event: domain.DockerEvent{
				Type: domain.EventTypeNetwork, Action: domain.ActionConnect,
				ActorID:    "net1",
				Attributes: map[string]string{"container": "abc"},
			},
			wantRequest: domain.RefreshContainer, wantResult: domain.ResultProcessed, wantTarget: "abc",
		},
		{
			name: "volume create",
			event: domain.DockerEvent{
				Type: domain.EventTypeVolume, Action: domain.ActionCreate,
				ActorID: "data", Attributes: map[string]string{},
			},
			wantRequest: domain.RefreshVolumes, wantResult: domain.ResultProcessed,
		},
		{
			name: "volume mount prefers the container",
			event: domain.DockerEvent{
				Type: domain.EventTypeVolume, Action: domain.ActionMount,
				ActorID:    "data",
				Attributes: map[string]string{"container": "abc"},
			},
			wantRequest: domain.RefreshContainer, wantResult: domain.ResultProcessed, wantTarget: "abc",
		},
		{
			name: "daemon reload sweeps",
			event: domain.DockerEvent{
				Type: domain.EventTypeDaemon, Action: domain.ActionReload,
				Attributes: map[string]string{},
			},
			wantRequest: domain.RefreshFull, wantResult: domain.ResultProcessed,
		},
		{
			name: "unknown type is ignored",
			event: domain.DockerEvent{
				Type: domain.EventTypeOther, Action: "entangle",
				Attributes: map[string]string{},
			},
			wantRequest: domain.RefreshNone, wantResult: domain.ResultIgnored,
		},
		{
			name:        "unknown container action is ignored",
			event:       containerEvent("f", "abc", "warp-drive"),
			wantRequest: domain.RefreshNone, wantResult: domain.ResultIgnored,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := service.ClassifyEvent(tc.event)
			if got.Request != tc.wantRequest {
				t.Errorf("request = %q, want %q", got.Request, tc.wantRequest)
			}
			if got.Result != tc.wantResult {
				t.Errorf("result = %q, want %q", got.Result, tc.wantResult)
			}
			if tc.wantTarget != "" && got.Target != tc.wantTarget {
				t.Errorf("target = %q, want %q", got.Target, tc.wantTarget)
			}
		})
	}
}
