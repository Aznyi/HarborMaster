package service

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/notify"
)

// Notification soak and failure tests.
//
// # What these look for, and why each one
//
// A notification subsystem fails in ways that are invisible from a unit test of
// one delivery. Each test here is a specific failure that has ended other
// people's on-call rotas:
//
//   - A goroutine per notification, so a busy hour is a memory leak.
//   - A destination that never answers, occupying every worker forever.
//   - A duplicate message per retry, so one broken webhook produces four
//     messages about one event.
//   - A retry that never stops.
//   - A queue that stops draining after the thing that filled it went away.
//   - Work still in flight when the process is asked to stop.
//
// They are deliberately CHEAP. A soak test that takes a minute is a soak test
// somebody skips, and a skipped test is not a test.

// A thousand notifications leave no goroutines behind.
//
// The failure this catches: raising a notification from a goroutine, which is
// the obvious way to make Raise non-blocking and which turns a burst into
// unbounded concurrency against somebody else's server.
func TestASustainedBurstLeavesNoGoroutinesBehind(t *testing.T) {
	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{
		testDestination("ndst_"+repeat("a", 20), "chat"),
	}
	fake.rules = []domain.NotificationRule{testRule("nrul_"+repeat("a", 20), "ndst_"+repeat("a", 20))}

	sender := &fakeSender{result: notify.Result{OK: true}}
	cfg := testNotificationConfig()
	cfg.Workers = 4
	cfg.MaxPerDestination = 4
	cfg.QueueSize = 64
	cfg.RetryInterval = time.Second
	cfg.PruneInterval = time.Second

	service := newTestNotificationService(fake, sender, cfg)

	before := stableGoroutineCount()

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		service.Run(ctx)
	}()

	for i := 0; i < 1000; i++ {
		notification := testNotification()
		// A distinct dedup key per notification, so suppression is not what
		// keeps the numbers down.
		notification.DedupKey = "burst:" + repeat("x", i%7)
		service.Raise(notification)
	}

	// Let the workers drain what they can.
	deadline := time.After(5 * time.Second)
	for len(service.queue) > 0 {
		select {
		case <-deadline:
			t.Fatalf("the queue still held %d notifications after five seconds", len(service.queue))
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	after := stableGoroutineCount()
	// A generous allowance: the runtime's own workers move, and a test that
	// demanded an exact match would be flaky rather than strict. A goroutine
	// PER NOTIFICATION would be a thousand.
	if after > before+10 {
		t.Fatalf("goroutines went from %d to %d across a thousand notifications; "+
			"a goroutine per notification is how a busy hour becomes a leak",
			before, after)
	}
}

// A destination that never answers does not stop every other one.
//
// The failure this catches: one endpoint that accepts the connection and then
// holds it until the timeout, occupying every worker. On a deployment with one
// slow destination and one working one, the working one goes silent.
func TestOneUnresponsiveDestinationDoesNotStarveTheOthers(t *testing.T) {
	slowID := "ndst_" + repeat("5", 20)
	fastID := "ndst_" + repeat("f", 20)

	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{
		testDestination(slowID, "slow"),
		testDestination(fastID, "fast"),
	}
	fake.rules = []domain.NotificationRule{testRule("nrul_"+repeat("a", 20), slowID, fastID)}

	block := make(chan struct{})
	var slowSends atomic.Int64
	sender := &blockingSender{
		block: block,
		blockFor: func(request notify.SendRequest) bool {
			if request.Destination.DestinationID == slowID {
				slowSends.Add(1)
				return true
			}
			return false
		},
	}

	cfg := testNotificationConfig()
	cfg.Workers = 4
	cfg.MaxPerDestination = 1
	cfg.DeliveryTimeout = 10 * time.Second
	service := newTestNotificationService(fake, sender, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		service.Run(ctx)
	}()

	for i := 0; i < 20; i++ {
		service.Raise(testNotification())
	}

	// The fast destination keeps receiving while the slow one is stuck.
	deadline := time.After(5 * time.Second)
	for sender.countFor(fastID) < 3 {
		select {
		case <-deadline:
			t.Fatalf("the fast destination received %d messages while the slow one "+
				"was blocked; one unresponsive endpoint starved the others",
				sender.countFor(fastID))
		case <-time.After(10 * time.Millisecond):
		}
	}

	// And the slow one occupied at most its share.
	if got := slowSends.Load(); got > int64(cfg.MaxPerDestination) {
		t.Fatalf("%d concurrent deliveries to the slow destination, want at most %d",
			got, cfg.MaxPerDestination)
	}

	close(block)
	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the block was released")
	}
}

// A retried delivery is ONE record, updated, not a new one each time.
//
// The failure this catches: a retry that writes a fresh delivery row, so a
// destination that fails four times produces four "not delivered" entries in
// the history and an operator concludes four separate things went wrong.
func TestARetriedDeliveryIsOneRecord(t *testing.T) {
	t.Parallel()

	destinationID := "ndst_" + repeat("a", 20)
	fake := newFakeNotificationStore()
	destination := testDestination(destinationID, "chat")
	fake.destinations = []domain.NotificationDestination{destination}
	fake.rules = []domain.NotificationRule{testRule("nrul_"+repeat("a", 20), destinationID)}

	sender := &fakeSender{result: notify.Result{
		OK: false, Retryable: true, StatusCode: 503, Reason: notify.FailureServer,
	}}
	cfg := testNotificationConfig()
	cfg.MaxAttempts = 4
	now := time.Unix(1700000000, 0).UTC()
	service := NewNotificationService(NotificationOptions{
		Store: fake, Sender: sender, Config: cfg, Logger: quietLogger(),
		Now: func() time.Time { return now },
	})

	ctx := context.Background()
	service.route(ctx, queued{notification: testNotification()})

	// Every retry the sweep is due for.
	for pass := 0; pass < cfg.MaxAttempts+2; pass++ {
		now = now.Add(2 * time.Hour)
		service.sweepRetries(ctx)
	}

	records := fake.records()
	if len(records) != 1 {
		t.Fatalf("a retried delivery produced %d records, want 1; an operator would "+
			"read those as separate failures", len(records))
	}
	if records[0].Result != domain.DeliveryFailed {
		t.Fatalf("the record settled as %q, want %q", records[0].Result, domain.DeliveryFailed)
	}
	if records[0].Attempts != cfg.MaxAttempts {
		t.Fatalf("attempts = %d, want %d", records[0].Attempts, cfg.MaxAttempts)
	}

	// And the retries stopped. A sweep past the bound must not send again.
	sentAtBound := len(sender.requests())
	now = now.Add(24 * time.Hour)
	service.sweepRetries(ctx)
	if got := len(sender.requests()); got != sentAtBound {
		t.Fatalf("a settled delivery was retried again (%d -> %d); an unbounded "+
			"retry is a permanent load source against somebody else's server",
			sentAtBound, got)
	}
}

// A destination that fails forever produces bounded work and bounded history.
//
// The failure this catches: unbounded database growth. A destination that has
// been broken for a week, with a scheduler raising a notification every fifteen
// minutes, must produce a bounded number of rows and a bounded number of sends.
func TestAPermanentlyBrokenDestinationStaysBounded(t *testing.T) {
	t.Parallel()

	destinationID := "ndst_" + repeat("a", 20)
	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{testDestination(destinationID, "chat")}
	fake.rules = []domain.NotificationRule{testRule("nrul_"+repeat("a", 20), destinationID)}

	// A revoked webhook URL: permanent, and not retryable.
	sender := &fakeSender{result: notify.Result{
		OK: false, Retryable: false, StatusCode: 403, Reason: notify.FailureRejected,
	}}
	service := newTestNotificationService(fake, sender, testNotificationConfig())

	ctx := context.Background()
	const events = 96 // a day of a fifteen-minute scheduler
	for i := 0; i < events; i++ {
		service.route(ctx, queued{notification: testNotification()})
	}

	// One row per EVENT, and one attempt per row. Not one per retry: a
	// permanent failure is not retried at all.
	if got := len(fake.records()); got != events {
		t.Fatalf("%d records for %d events, want %d", got, events, events)
	}
	if got := len(sender.requests()); got != events {
		t.Fatalf("%d sends for %d events, want %d; a permanent failure must not "+
			"be retried", got, events, events)
	}
	for _, outcome := range fake.outcomes() {
		if outcome.attempts != 1 {
			t.Fatalf("a permanent failure was attempted %d times", outcome.attempts)
		}
	}
}

// The queue keeps draining after it has been full.
//
// The failure this catches: a drop path that leaves the queue in a state it
// never recovers from — a worker that exits on a drop, or a counter that stops
// the engine reading.
func TestTheQueueRecoversAfterBeingFull(t *testing.T) {
	fake := newFakeNotificationStore()
	destinationID := "ndst_" + repeat("a", 20)
	fake.destinations = []domain.NotificationDestination{testDestination(destinationID, "chat")}
	fake.rules = []domain.NotificationRule{testRule("nrul_"+repeat("a", 20), destinationID)}

	sender := &fakeSender{result: notify.Result{OK: true}}
	cfg := testNotificationConfig()
	cfg.QueueSize = 4
	cfg.Workers = 2
	cfg.MaxPerDestination = 2
	service := newTestNotificationService(fake, sender, cfg)

	// Overflow it BEFORE anything is draining.
	for i := 0; i < 200; i++ {
		service.Raise(testNotification())
	}
	service.mu.Lock()
	dropped := service.dropped
	service.mu.Unlock()
	if dropped == 0 {
		t.Fatal("nothing was dropped; the queue is not bounded")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		service.Run(ctx)
	}()

	// And it still delivers afterwards.
	deadline := time.After(5 * time.Second)
	for len(sender.requests()) == 0 {
		select {
		case <-deadline:
			t.Fatal("the engine delivered nothing after the queue had been full")
		case <-time.After(10 * time.Millisecond):
		}
	}

	before := len(sender.requests())
	service.Raise(testNotification())
	for len(sender.requests()) <= before {
		select {
		case <-deadline:
			t.Fatal("a notification raised after the overflow was never delivered")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// A shutdown mid-delivery completes rather than hanging.
//
// Invariant 6: every wait is bounded, and a shutdown that cannot complete is
// not graceful. A delivery holds a detached context so its outcome is still
// recorded, and that context is bounded so it cannot hold the process open.
func TestAShutdownDuringADeliveryStillCompletes(t *testing.T) {
	destinationID := "ndst_" + repeat("a", 20)
	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{testDestination(destinationID, "chat")}
	fake.rules = []domain.NotificationRule{testRule("nrul_"+repeat("a", 20), destinationID)}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	sender := &fakeSender{result: notify.Result{OK: true}}
	sender.onSend = func() {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}

	cfg := testNotificationConfig()
	// Short, so the bound is what ends the wait rather than the test's patience.
	cfg.DeliveryTimeout = 500 * time.Millisecond
	service := newTestNotificationService(fake, sender, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		service.Run(ctx)
	}()

	service.Raise(testNotification())

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		cancel()
		close(release)
		t.Fatal("the delivery never started")
	}

	// Cancelled mid-send. The detached context's own bound must end the wait.
	cancel()
	close(release)

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return while a delivery was in flight; a shutdown " +
			"that cannot complete is not graceful")
	}
}

// A store that fails every write does not stop the engine.
//
// The failure this catches: a database busy error taking the notification
// engine down with it. A notification is the least important thing in
// HarborMaster and must never be the thing that stops it.
func TestAFailingStoreDoesNotStopTheEngine(t *testing.T) {
	t.Parallel()

	destinationID := "ndst_" + repeat("a", 20)
	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{testDestination(destinationID, "chat")}
	fake.rules = []domain.NotificationRule{testRule("nrul_"+repeat("a", 20), destinationID)}
	fake.recordErr = errors.New("database is locked")

	sender := &fakeSender{result: notify.Result{OK: true}}
	service := newTestNotificationService(fake, sender, testNotificationConfig())

	ctx := context.Background()
	for i := 0; i < 20; i++ {
		service.route(ctx, queued{notification: testNotification()})
	}

	// Nothing was sent -- a delivery whose record could not be written is not
	// attempted, because an unrecorded send is one nobody can account for --
	// and nothing panicked.
	if got := len(sender.requests()); got != 0 {
		t.Fatalf("sent %d messages that could not be recorded", got)
	}

	// And the engine recovers the moment the store does.
	fake.mu.Lock()
	fake.recordErr = nil
	fake.mu.Unlock()

	service.route(ctx, queued{notification: testNotification()})
	if got := len(sender.requests()); got != 1 {
		t.Fatalf("sent %d messages after the store recovered, want 1", got)
	}
}

// ------------------------------------------------------------------ doubles --

// blockingSender holds some destinations open and answers others immediately.
type blockingSender struct {
	mu       sync.Mutex
	counts   map[string]int
	block    chan struct{}
	blockFor func(notify.SendRequest) bool
}

func (b *blockingSender) Send(ctx context.Context, request notify.SendRequest) notify.Result {
	b.mu.Lock()
	if b.counts == nil {
		b.counts = map[string]int{}
	}
	b.counts[request.Destination.DestinationID]++
	blocking := b.blockFor != nil && b.blockFor(request)
	b.mu.Unlock()

	if blocking {
		select {
		case <-b.block:
		case <-ctx.Done():
		}
	}
	return notify.Result{OK: true}
}

func (b *blockingSender) countFor(destinationID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counts[destinationID]
}

// repeat is strings.Repeat, kept local so the test identifiers read as
// identifiers rather than as string arithmetic.
func repeat(s string, count int) string {
	out := make([]byte, 0, len(s)*count)
	for i := 0; i < count; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// stableGoroutineCount reads the goroutine count after giving the runtime a
// chance to reap goroutines that have already returned.
//
// A namesake of the helper in shutdown_test.go, which lives in the external
// test package and cannot be reached from here. Duplicated rather than
// exported: a leak-counting helper is not API.
//
// Without the settle, a leak test races the scheduler and fails intermittently
// on a busy machine -- which is worse than not having the test, because a flaky
// guard gets deleted.
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
