package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/notify"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The notification engine's tests.
//
// The properties that matter here are almost all NEGATIVE: what the engine must
// not do to the thing it is reporting on, and what must never reach a delivery
// record. Those get the tests; the happy path is one line at the bottom of most
// of them.

// ------------------------------------------------------------------ doubles --

// fakeNotificationStore is an in-memory NotificationStore.
type fakeNotificationStore struct {
	mu sync.Mutex

	destinations []domain.NotificationDestination
	rules        []domain.NotificationRule
	secrets      map[string]domain.NotificationSecret

	deliveries []domain.NotificationDelivery
	completed  []completion

	// suppress is what ShouldSuppress returns; suppressErr makes it fail.
	suppress    bool
	suppressErr error
	// cooldowns records the window the engine asked for, per event. The floor a
	// level-triggered event carries is invisible unless the value that reaches
	// the store is checked.
	cooldowns map[string]time.Duration
	// secretErr makes the credential read fail.
	secretErr error
	// recordErr makes every delivery write fail, standing in for a database
	// that is busy or locked.
	recordErr error
}

type completion struct {
	deliveryID  string
	result      domain.DeliveryResult
	attempts    int
	statusCode  int
	detail      string
	nextAttempt *time.Time
}

func newFakeNotificationStore() *fakeNotificationStore {
	return &fakeNotificationStore{secrets: map[string]domain.NotificationSecret{}}
}

func (f *fakeNotificationStore) ActiveDestinations(context.Context) ([]domain.NotificationDestination, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.NotificationDestination, len(f.destinations))
	copy(out, f.destinations)
	return out, nil
}

func (f *fakeNotificationStore) ActiveRules(context.Context) ([]domain.NotificationRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.NotificationRule, len(f.rules))
	copy(out, f.rules)
	return out, nil
}

func (f *fakeNotificationStore) DestinationByID(
	_ context.Context,
	destinationID string,
) (domain.NotificationDestination, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, destination := range f.destinations {
		if destination.DestinationID == destinationID {
			return destination, nil
		}
	}
	return domain.NotificationDestination{}, store.ErrNotFound
}

func (f *fakeNotificationStore) Secret(
	_ context.Context,
	destinationID string,
) (domain.NotificationSecret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.secretErr != nil {
		return domain.NotificationSecret{}, f.secretErr
	}
	return f.secrets[destinationID], nil
}

func (f *fakeNotificationStore) RecordDelivery(
	_ context.Context,
	delivery domain.NotificationDelivery,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return f.recordErr
	}
	f.deliveries = append(f.deliveries, delivery)
	return nil
}

func (f *fakeNotificationStore) CompleteDelivery(
	_ context.Context,
	deliveryID string,
	result domain.DeliveryResult,
	attempts, statusCode int,
	detail string,
	nextAttemptAt *time.Time,
	_ time.Time,
	_ int64,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, completion{
		deliveryID:  deliveryID,
		result:      result,
		attempts:    attempts,
		statusCode:  statusCode,
		detail:      detail,
		nextAttempt: nextAttemptAt,
	})
	for i := range f.deliveries {
		if f.deliveries[i].DeliveryID == deliveryID {
			f.deliveries[i].Result = result
			f.deliveries[i].Attempts = attempts
			f.deliveries[i].Error = detail
			f.deliveries[i].NextAttemptAt = nextAttemptAt
		}
	}
	return nil
}

func (f *fakeNotificationStore) RecordDestinationResult(
	context.Context, string, domain.DeliveryResult, string, time.Time,
) error {
	return nil
}

func (f *fakeNotificationStore) DueRetries(
	_ context.Context, now time.Time, _ int,
) ([]domain.NotificationDelivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var due []domain.NotificationDelivery
	for _, delivery := range f.deliveries {
		if delivery.Result == domain.DeliveryRetrying && delivery.NextAttemptAt != nil &&
			!delivery.NextAttemptAt.After(now) {
			due = append(due, delivery)
		}
	}
	return due, nil
}

func (f *fakeNotificationStore) ShouldSuppress(
	_ context.Context, _, dedupKey string, cooldown time.Duration, _ time.Time,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cooldowns == nil {
		f.cooldowns = map[string]time.Duration{}
	}
	f.cooldowns[dedupKey] = cooldown
	return f.suppress, f.suppressErr
}

// recorded returns every delivery row written so far.
func (f *fakeNotificationStore) recorded() []domain.NotificationDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.NotificationDelivery(nil), f.deliveries...)
}

// cooldownFor returns the window the engine asked for on one dedup key.
func (f *fakeNotificationStore) cooldownFor(dedupKey string) (time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cooldown, seen := f.cooldowns[dedupKey]
	return cooldown, seen
}

func (f *fakeNotificationStore) PruneDeliveries(context.Context, time.Time, int) (int, error) {
	return 0, nil
}

func (f *fakeNotificationStore) ListDeliveries(
	context.Context, store.DeliveryFilter,
) ([]domain.NotificationDelivery, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.NotificationDelivery, len(f.deliveries))
	copy(out, f.deliveries)
	return out, len(out), nil
}

func (f *fakeNotificationStore) DeliveryByID(
	context.Context, string,
) (domain.NotificationDelivery, error) {
	return domain.NotificationDelivery{}, store.ErrNotFound
}

func (f *fakeNotificationStore) DeliverySummary(
	context.Context,
) (domain.NotificationSummary, error) {
	return domain.NotificationSummary{}, nil
}

func (f *fakeNotificationStore) CountNotificationConfiguration(
	context.Context,
) (int, int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.destinations), len(f.rules), 0, nil
}

func (f *fakeNotificationStore) records() []domain.NotificationDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.NotificationDelivery, len(f.deliveries))
	copy(out, f.deliveries)
	return out
}

func (f *fakeNotificationStore) outcomes() []completion {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]completion, len(f.completed))
	copy(out, f.completed)
	return out
}

// fakeSender is a NotificationSender that records what it was asked to send.
type fakeSender struct {
	mu       sync.Mutex
	sent     []notify.SendRequest
	result   notify.Result
	blockOn  chan struct{}
	onSend   func()
	sendWait time.Duration
}

func (f *fakeSender) Send(ctx context.Context, request notify.SendRequest) notify.Result {
	f.mu.Lock()
	f.sent = append(f.sent, request)
	result := f.result
	block := f.blockOn
	hook := f.onSend
	wait := f.sendWait
	f.mu.Unlock()

	if hook != nil {
		hook()
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	return result
}

func (f *fakeSender) requests() []notify.SendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]notify.SendRequest, len(f.sent))
	copy(out, f.sent)
	return out
}

// ------------------------------------------------------------------ helpers --

func testNotificationConfig() config.Notifications {
	return config.Notifications{
		Enabled:           true,
		QueueSize:         16,
		Workers:           2,
		MaxPerDestination: 1,
		DeliveryTimeout:   time.Second,
		MaxAttempts:       3,
		RetryBackoff:      time.Minute,
		RetryInterval:     time.Second,
		RetentionAge:      time.Hour,
		PruneInterval:     time.Hour,
	}
}

func testDestination(id, name string) domain.NotificationDestination {
	return domain.NotificationDestination{
		DestinationID: id,
		Name:          name,
		Channel:       domain.ChannelWebhook,
		Enabled:       true,
		Endpoint:      "https://hooks.example.test",
	}
}

func testRule(id string, destinations ...string) domain.NotificationRule {
	return domain.NotificationRule{
		RuleID:          id,
		Name:            "rule " + id,
		Enabled:         true,
		MinimumSeverity: domain.NotifyInfo,
		Destinations:    destinations,
	}
}

func testNotification() domain.Notification {
	return domain.Notification{
		Event:         domain.EventUpdateDiscovered,
		Severity:      domain.NotifyInfo,
		Title:         "An update is available",
		Body:          "nginx has a newer image.",
		ContainerName: "web",
		DedupKey:      "web:1.2.3",
		OccurredAt:    time.Unix(1700000000, 0).UTC(),
	}
}

// -------------------------------------------------------------------- tests --

// A slow destination must not be able to stall the thing being reported on.
//
// This is the whole reason Raise has no error return: a caller in the middle of
// a rollback has nothing to do with one and must not wait for one.
func TestRaiseNeverBlocksEvenWhenNobodyIsDelivering(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	sender := &fakeSender{result: notify.Result{OK: true}}
	cfg := testNotificationConfig()
	cfg.QueueSize = 2

	service := NewNotificationService(NotificationOptions{
		Store: fake, Sender: sender, Config: cfg,
		Logger: quietLogger(), Now: func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})

	// No Run, so nothing drains the queue. Every one of these must return.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			service.Raise(testNotification())
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Raise blocked when the queue was full; a notification stalled its own caller")
	}

	// And the drops were counted rather than silently forgotten.
	service.mu.Lock()
	dropped := service.dropped
	service.mu.Unlock()
	if dropped != 48 {
		t.Fatalf("dropped = %d, want 48 (50 raised, 2 queued)", dropped)
	}
}

// One notification, two rules naming one destination, one message.
//
// An operator with a broad rule and a specific one must not be told twice.
func TestTwoRulesNamingOneDestinationDeliverOnce(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{testDestination("ndst_aaaaaaaaaaaaaaaaaaaa", "chat")}
	fake.rules = []domain.NotificationRule{
		testRule("nrul_bbbbbbbbbbbbbbbbbbbb", "ndst_aaaaaaaaaaaaaaaaaaaa"),
		testRule("nrul_cccccccccccccccccccc", "ndst_aaaaaaaaaaaaaaaaaaaa"),
	}
	sender := &fakeSender{result: notify.Result{OK: true}}

	service := newTestNotificationService(fake, sender, testNotificationConfig())
	service.route(context.Background(), queued{notification: testNotification()})

	if got := len(sender.requests()); got != 1 {
		t.Fatalf("sent %d messages for one notification; want exactly 1", got)
	}
}

// A requeue for a busy destination must not re-run the rules.
//
// The defect this pins: requeueing an UNROUTED notification sends it through
// every rule a second time, so every destination that already took it takes it
// again. One saturated destination becomes duplicate messages everywhere else.
func TestARequeueForABusyDestinationDoesNotResendToTheOthers(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{
		testDestination("ndst_ffffffffffffffffffff", "fast"),
		testDestination("ndst_55555555555555555555", "slow"),
	}
	fake.rules = []domain.NotificationRule{testRule("nrul_aaaaaaaaaaaaaaaaaaaa", "ndst_ffffffffffffffffffff", "ndst_55555555555555555555")}

	sender := &fakeSender{result: notify.Result{OK: true}}
	cfg := testNotificationConfig()
	cfg.MaxPerDestination = 1
	service := newTestNotificationService(fake, sender, cfg)

	// Hold the slow destination's only slot, so routing must requeue it.
	if !service.claim(context.Background(), "ndst_55555555555555555555") {
		t.Fatal("could not take the slot the test needs")
	}

	service.route(context.Background(), queued{notification: testNotification()})

	// The fast destination got its one message.
	if got := len(sender.requests()); got != 1 {
		t.Fatalf("sent %d messages on the first pass; want 1 (the slow one was busy)", got)
	}

	// And what was put back is TARGETED at the slow destination, carrying its
	// rule, rather than being an unrouted notification that would go through the
	// rules -- and back to the fast destination -- all over again.
	// Read from the DEFERRAL channel, not the fresh queue. A busy-destination
	// retry is kept off `queue` deliberately, so a blocked endpoint's backlog
	// cannot displace notifications other destinations are still waiting for.
	select {
	case item := <-service.deferred:
		if item.destinationID != "ndst_55555555555555555555" {
			t.Fatalf("requeued destinationId = %q, want ndst_slow; an unrouted requeue "+
				"would re-deliver to every destination that already took this",
				item.destinationID)
		}
		if item.rule.RuleID != "nrul_aaaaaaaaaaaaaaaaaaaa" {
			t.Fatalf("requeued rule = %q, want nrul_all", item.rule.RuleID)
		}
		if item.requeues != 1 {
			t.Fatalf("requeues = %d, want 1", item.requeues)
		}
	default:
		t.Fatal("nothing was requeued for the busy destination")
	}
}

// A permanently saturated destination must not make one notification circulate
// forever.
func TestRequeuingIsBounded(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	destination := testDestination("ndst_cccccccccccccccccccc", "stuck")
	fake.destinations = []domain.NotificationDestination{destination}
	fake.rules = []domain.NotificationRule{testRule("nrul_aaaaaaaaaaaaaaaaaaaa", "ndst_cccccccccccccccccccc")}

	sender := &fakeSender{result: notify.Result{OK: true}}
	cfg := testNotificationConfig()
	cfg.MaxPerDestination = 1
	service := newTestNotificationService(fake, sender, cfg)

	// Permanently occupied.
	service.claim(context.Background(), "ndst_cccccccccccccccccccc")

	ctx := context.Background()
	item := queued{notification: testNotification()}
	for pass := 0; pass < maxNotificationRequeues+2; pass++ {
		service.route(ctx, item)
		select {
		case next := <-service.deferred:
			// The backlog ration is released when an item leaves the channel, the
			// same as a worker does, so the next pass may defer again.
			service.releaseDeferral(next.destinationID)
			item = next
		default:
			// Nothing put back: the bound was reached.
			goto settled
		}
	}
	t.Fatalf("the notification was still circulating after %d passes", maxNotificationRequeues+2)

settled:
	records := fake.records()
	if len(records) != 1 || records[0].Result != domain.DeliveryDropped {
		t.Fatalf("records = %+v; want exactly one dropped record so an operator can see "+
			"the notification was lost", records)
	}
	if records[0].Error == "" {
		t.Fatal("the dropped record carries no reason")
	}
}

// A failure that is not retryable is a dead letter on the first attempt.
//
// A revoked webhook URL returns 403 forever. Retrying it four times helps
// nobody and multiplies the load on somebody else's server.
func TestANonRetryableFailureIsADeadLetterImmediately(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{testDestination("ndst_aaaaaaaaaaaaaaaaaaaa", "chat")}
	fake.rules = []domain.NotificationRule{testRule("nrul_aaaaaaaaaaaaaaaaaaaa", "ndst_aaaaaaaaaaaaaaaaaaaa")}

	sender := &fakeSender{result: notify.Result{
		OK: false, Retryable: false, StatusCode: 403,
		Reason: notify.FailureRejected, Detail: notify.FailureRejected.Explain(),
	}}
	service := newTestNotificationService(fake, sender, testNotificationConfig())
	service.route(context.Background(), queued{notification: testNotification()})

	outcomes := fake.outcomes()
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %d, want 1", len(outcomes))
	}
	if outcomes[0].result != domain.DeliveryFailed {
		t.Fatalf("result = %q, want %q on the first attempt", outcomes[0].result, domain.DeliveryFailed)
	}
	if outcomes[0].attempts != 1 {
		t.Fatalf("attempts = %d, want 1", outcomes[0].attempts)
	}
	if outcomes[0].nextAttempt != nil {
		t.Fatal("a dead letter was scheduled for a retry")
	}
}

// A retryable failure retries, and stops.
func TestRetriesAreBoundedAndBackOff(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	destination := testDestination("ndst_aaaaaaaaaaaaaaaaaaaa", "chat")
	fake.destinations = []domain.NotificationDestination{destination}
	fake.rules = []domain.NotificationRule{testRule("nrul_aaaaaaaaaaaaaaaaaaaa", "ndst_aaaaaaaaaaaaaaaaaaaa")}

	sender := &fakeSender{result: notify.Result{
		OK: false, Retryable: true, StatusCode: 503,
		Reason: notify.FailureServer, Detail: notify.FailureServer.Explain(),
	}}
	cfg := testNotificationConfig()
	cfg.MaxAttempts = 3
	service := newTestNotificationService(fake, sender, cfg)

	ctx := context.Background()
	service.route(ctx, queued{notification: testNotification()})

	delivery := fake.records()[0]
	for attempt := 2; attempt <= cfg.MaxAttempts+1; attempt++ {
		service.attempt(ctx, delivery, destination, attempt)
	}

	outcomes := fake.outcomes()
	if len(outcomes) != cfg.MaxAttempts+1 {
		t.Fatalf("outcomes = %d, want %d", len(outcomes), cfg.MaxAttempts+1)
	}
	for i, outcome := range outcomes[:cfg.MaxAttempts-1] {
		if outcome.result != domain.DeliveryRetrying {
			t.Fatalf("attempt %d result = %q, want %q", i+1, outcome.result, domain.DeliveryRetrying)
		}
		if outcome.nextAttempt == nil {
			t.Fatalf("attempt %d scheduled no retry", i+1)
		}
	}
	if last := outcomes[cfg.MaxAttempts-1]; last.result != domain.DeliveryFailed {
		t.Fatalf("attempt %d result = %q, want %q: retries must stop at the configured bound",
			cfg.MaxAttempts, last.result, domain.DeliveryFailed)
	}

	// And the backoff grows, and is capped.
	if service.backoff(1) != cfg.RetryBackoff {
		t.Fatalf("backoff(1) = %s, want %s", service.backoff(1), cfg.RetryBackoff)
	}
	if service.backoff(2) != 2*cfg.RetryBackoff {
		t.Fatalf("backoff(2) = %s, want %s", service.backoff(2), 2*cfg.RetryBackoff)
	}
	if got := service.backoff(50); got != maxNotificationBackoff {
		t.Fatalf("backoff(50) = %s, want the cap %s: an ever-growing delay eventually "+
			"never fires", got, maxNotificationBackoff)
	}
}

// A cooldown suppresses, and says it suppressed.
//
// An operator asking "why was I not told" must be able to see that HarborMaster
// decided not to, and which rule decided.
func TestACooldownSuppressesAndRecordsWhy(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{testDestination("ndst_aaaaaaaaaaaaaaaaaaaa", "chat")}
	rule := testRule("nrul_aaaaaaaaaaaaaaaaaaaa", "ndst_aaaaaaaaaaaaaaaaaaaa")
	rule.CooldownSeconds = 300
	fake.rules = []domain.NotificationRule{rule}
	fake.suppress = true

	sender := &fakeSender{result: notify.Result{OK: true}}
	service := newTestNotificationService(fake, sender, testNotificationConfig())
	service.route(context.Background(), queued{notification: testNotification()})

	if got := len(sender.requests()); got != 0 {
		t.Fatalf("sent %d messages despite the cooldown", got)
	}
	records := fake.records()
	if len(records) != 1 || records[0].Result != domain.DeliverySuppressed {
		t.Fatalf("records = %+v; want one suppressed record", records)
	}
	if records[0].RuleID != "nrul_aaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("the suppressed record does not name the rule that decided: %q", records[0].RuleID)
	}
}

// A cooldown lookup that FAILS must not swallow the notification.
//
// Invariant 5's converse: a check that could not be performed establishes
// nothing. Losing a message about a failed rollback because a suppression query
// errored would be the wrong trade.
func TestAFailedCooldownLookupStillDelivers(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{testDestination("ndst_aaaaaaaaaaaaaaaaaaaa", "chat")}
	fake.rules = []domain.NotificationRule{testRule("nrul_aaaaaaaaaaaaaaaaaaaa", "ndst_aaaaaaaaaaaaaaaaaaaa")}
	fake.suppressErr = errors.New("database is busy")

	sender := &fakeSender{result: notify.Result{OK: true}}
	service := newTestNotificationService(fake, sender, testNotificationConfig())
	service.route(context.Background(), queued{notification: testNotification()})

	if got := len(sender.requests()); got != 1 {
		t.Fatalf("sent %d messages; a suppression lookup that failed must not suppress", got)
	}
}

// A credential that cannot be read must not be treated as "no credential".
//
// Sending a webhook with an empty URL, or an SMTP session with no password,
// would either fail confusingly or -- worse -- succeed against the wrong place.
func TestACredentialThatCannotBeReadRefusesTheSend(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{testDestination("ndst_aaaaaaaaaaaaaaaaaaaa", "chat")}
	fake.rules = []domain.NotificationRule{testRule("nrul_aaaaaaaaaaaaaaaaaaaa", "ndst_aaaaaaaaaaaaaaaaaaaa")}
	fake.secretErr = errors.New("the secrets table is unreadable")

	sender := &fakeSender{result: notify.Result{OK: true}}
	service := newTestNotificationService(fake, sender, testNotificationConfig())
	service.route(context.Background(), queued{notification: testNotification()})

	if got := len(sender.requests()); got != 0 {
		t.Fatalf("attempted %d sends without a credential", got)
	}
	outcomes := fake.outcomes()
	if len(outcomes) != 1 || outcomes[0].result != domain.DeliveryFailed {
		t.Fatalf("outcomes = %+v; want one failure", outcomes)
	}
	// And the failure sentence is HarborMaster's, not the database's.
	if strings.Contains(outcomes[0].detail, "secrets table") {
		t.Fatalf("the delivery record carries the underlying error text: %q", outcomes[0].detail)
	}
}

// Nothing a destination knows about reaches a delivery record.
//
// The delivery history is served to operators and rendered in a browser. A URL
// whose path IS the credential -- which is exactly what Slack, Discord, and
// Teams webhooks are -- must never appear in one.
func TestADeliveryRecordNeverCarriesACredential(t *testing.T) {
	t.Parallel()

	const webhookURL = "https://hooks.example.test/services/T00/B00/XXXXSECRETXXXX"

	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{testDestination("ndst_aaaaaaaaaaaaaaaaaaaa", "chat")}
	fake.rules = []domain.NotificationRule{testRule("nrul_aaaaaaaaaaaaaaaaaaaa", "ndst_aaaaaaaaaaaaaaaaaaaa")}
	fake.secrets["ndst_aaaaaaaaaaaaaaaaaaaa"] = domain.NotificationSecret{
		URL:          webhookURL,
		SMTPPassword: "hunter2",
	}

	sender := &fakeSender{result: notify.Result{
		OK: false, Retryable: false, StatusCode: 404,
		Reason: notify.FailureRejected, Detail: notify.FailureRejected.Explain(),
	}}
	service := newTestNotificationService(fake, sender, testNotificationConfig())
	service.route(context.Background(), queued{notification: testNotification()})

	for _, record := range fake.records() {
		rendered := record.Title + record.Body + record.Error + record.DestinationName +
			record.ContainerName + record.DedupKey
		for _, secret := range []string{webhookURL, "XXXXSECRETXXXX", "hunter2"} {
			if strings.Contains(rendered, secret) {
				t.Fatalf("a delivery record carries %q", secret)
			}
		}
	}
	for _, outcome := range fake.outcomes() {
		for _, secret := range []string{webhookURL, "XXXXSECRETXXXX", "hunter2"} {
			if strings.Contains(outcome.detail, secret) {
				t.Fatalf("a delivery outcome carries %q", secret)
			}
		}
	}
}

// The severity threshold and the event selection actually filter.
func TestARuleOnlyMatchesWhatItSelected(t *testing.T) {
	t.Parallel()

	notification := testNotification()
	notification.Severity = domain.NotifyInfo
	notification.Event = domain.EventUpdateDiscovered

	cases := []struct {
		name string
		rule domain.NotificationRule
		want bool
	}{
		{
			name: "an empty event list means every event",
			rule: domain.NotificationRule{Enabled: true, MinimumSeverity: domain.NotifyInfo},
			want: true,
		},
		{
			name: "a threshold above the notification does not match",
			rule: domain.NotificationRule{Enabled: true, MinimumSeverity: domain.NotifyCritical},
			want: false,
		},
		{
			name: "an event list that omits the event does not match",
			rule: domain.NotificationRule{
				Enabled:         true,
				MinimumSeverity: domain.NotifyInfo,
				Events:          []domain.NotificationEvent{domain.EventDriftDetected},
			},
			want: false,
		},
		{
			name: "a disabled rule never matches",
			rule: domain.NotificationRule{Enabled: false, MinimumSeverity: domain.NotifyInfo},
			want: false,
		},
		{
			name: "an archived rule never matches",
			rule: domain.NotificationRule{
				Enabled: true, Archived: true, MinimumSeverity: domain.NotifyInfo,
			},
			want: false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := testCase.rule.Matches(notification); got != testCase.want {
				t.Fatalf("Matches() = %v, want %v", got, testCase.want)
			}
		})
	}
}

// A disabled subsystem delivers nothing, and says so rather than pretending.
func TestADisabledSubsystemDeliversNothing(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{testDestination("ndst_aaaaaaaaaaaaaaaaaaaa", "chat")}
	fake.rules = []domain.NotificationRule{testRule("nrul_aaaaaaaaaaaaaaaaaaaa", "ndst_aaaaaaaaaaaaaaaaaaaa")}
	sender := &fakeSender{result: notify.Result{OK: true}}

	cfg := testNotificationConfig()
	cfg.Enabled = false
	service := newTestNotificationService(fake, sender, cfg)

	service.Raise(testNotification())
	if got := len(service.queue); got != 0 {
		t.Fatalf("queued %d notifications while disabled", got)
	}
	if err := service.RaiseTest("ndst_aaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, ErrNotificationsDisabled) {
		t.Fatalf("RaiseTest error = %v, want %v", err, ErrNotificationsDisabled)
	}
	// But the history stays readable, so an operator can configure before
	// switching sending on.
	if !service.Readable() {
		t.Fatal("a disabled subsystem hid its own history")
	}
}

// A test send names a destination, and only a well-formed identifier is
// accepted -- the id reaches a database lookup and a queue.
func TestATestSendRefusesAnIdentifierItDidNotIssue(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	sender := &fakeSender{result: notify.Result{OK: true}}
	service := newTestNotificationService(fake, sender, testNotificationConfig())

	for _, bad := range []string{
		"", "ndst_", "../../etc/passwd", "ndst_a' OR 1=1 --",
		"ndst_" + strings.Repeat("a", 200), "ndst_ZZZZ",
	} {
		if err := service.RaiseTest(bad); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("RaiseTest(%q) error = %v, want %v", bad, err, store.ErrNotFound)
		}
	}
}

// A test send travels the same queue and the same transport as everything else.
//
// A test that took a different path would prove the wrong thing.
func TestATestSendUsesTheSamePathAsARealNotification(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{testDestination("ndst_aaaaaaaaaaaaaaaaaaaa", "chat")}
	// No rules at all: a test must reach its destination anyway.
	sender := &fakeSender{result: notify.Result{OK: true}}
	service := newTestNotificationService(fake, sender, testNotificationConfig())

	if err := service.RaiseTest("ndst_aaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatalf("RaiseTest: %v", err)
	}
	item := <-service.queue
	service.route(context.Background(), item)

	requests := sender.requests()
	if len(requests) != 1 {
		t.Fatalf("sent %d messages for a test send, want 1", len(requests))
	}
	if requests[0].Notification.Event != domain.EventTest {
		t.Fatalf("event = %q, want %q", requests[0].Notification.Event, domain.EventTest)
	}
}

// A destination switched off while a retry was pending is settled, not retried
// forever.
func TestARetryForADisabledDestinationIsSettled(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	destination := testDestination("ndst_aaaaaaaaaaaaaaaaaaaa", "chat")
	fake.destinations = []domain.NotificationDestination{destination}
	fake.rules = []domain.NotificationRule{testRule("nrul_aaaaaaaaaaaaaaaaaaaa", "ndst_aaaaaaaaaaaaaaaaaaaa")}

	sender := &fakeSender{result: notify.Result{
		OK: false, Retryable: true, StatusCode: 503, Reason: notify.FailureServer,
	}}
	now := time.Unix(1700000000, 0).UTC()
	service := NewNotificationService(NotificationOptions{
		Store: fake, Sender: sender, Config: testNotificationConfig(),
		Logger: quietLogger(), Now: func() time.Time { return now },
	})

	ctx := context.Background()
	service.route(ctx, queued{notification: testNotification()})
	if fake.records()[0].Result != domain.DeliveryRetrying {
		t.Fatalf("the first attempt did not schedule a retry: %+v", fake.records()[0])
	}

	// The operator switches the destination off.
	fake.mu.Lock()
	fake.destinations[0].Enabled = false
	fake.mu.Unlock()

	sentBefore := len(sender.requests())
	now = now.Add(time.Hour)
	service.sweepRetries(ctx)

	if got := len(sender.requests()); got != sentBefore {
		t.Fatalf("a disabled destination was still sent to (%d -> %d)", sentBefore, got)
	}
	outcomes := fake.outcomes()
	last := outcomes[len(outcomes)-1]
	if last.result != domain.DeliveryFailed {
		t.Fatalf("last result = %q, want %q: a retry for a disabled destination must settle",
			last.result, domain.DeliveryFailed)
	}
}

// One slow destination must not occupy every worker.
func TestOneDestinationCannotStarveTheOthers(t *testing.T) {
	t.Parallel()

	cfg := testNotificationConfig()
	cfg.MaxPerDestination = 2
	service := newTestNotificationService(newFakeNotificationStore(),
		&fakeSender{result: notify.Result{OK: true}}, cfg)

	first := service.claim(context.Background(), "ndst_aaaaaaaaaaaaaaaaaaaa")
	second := service.claim(context.Background(), "ndst_aaaaaaaaaaaaaaaaaaaa")
	if !first || !second {
		t.Fatalf("the first two claims were refused (%v, %v)", first, second)
	}
	if service.claim(context.Background(), "ndst_aaaaaaaaaaaaaaaaaaaa") {
		t.Fatalf("a third concurrent delivery to one destination was allowed past the "+
			"limit of %d", cfg.MaxPerDestination)
	}
	// Another destination is unaffected.
	if !service.claim(context.Background(), "ndst_bbbbbbbbbbbbbbbbbbbb") {
		t.Fatal("one busy destination blocked a different one")
	}

	// And releasing returns every token.
	//
	// The bookkeeping is a token bucket per destination rather than a counter, so
	// what must be true after every release is that each bucket is FULL again --
	// no token leaked, and the next delivery claims immediately. The map keeps one
	// fixed-size bucket per destination, which is bounded by the number of
	// destinations an administrator created rather than by traffic.
	service.release("ndst_aaaaaaaaaaaaaaaaaaaa")
	service.release("ndst_aaaaaaaaaaaaaaaaaaaa")
	service.release("ndst_bbbbbbbbbbbbbbbbbbbb")

	service.mu.Lock()
	buckets := make(map[string]int, len(service.slots))
	for id, slots := range service.slots {
		buckets[id] = len(slots)
	}
	service.mu.Unlock()

	for id, available := range buckets {
		if available != cfg.MaxPerDestination {
			t.Fatalf("destination %s has %d of %d tokens after every release; a leaked "+
				"token permanently reduces that destination's throughput",
				id, available, cfg.MaxPerDestination)
		}
	}
	if len(buckets) != 2 {
		t.Fatalf("slots holds %d buckets, want one per destination used (2)", len(buckets))
	}
}

// The email relay credential comes from configuration, never from the row.
func TestTheEmailRelayCredentialComesFromConfiguration(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	destination := testDestination("ndst_eeeeeeeeeeeeeeeeeeee", "ops mailbox")
	destination.Channel = domain.ChannelEmail
	destination.EmailTo = []string{"ops@example.test"}
	destination.EmailFrom = "harbormaster@example.test"
	fake.destinations = []domain.NotificationDestination{destination}
	fake.rules = []domain.NotificationRule{testRule("nrul_aaaaaaaaaaaaaaaaaaaa", "ndst_eeeeeeeeeeeeeeeeeeee")}

	sender := &fakeSender{result: notify.Result{OK: true}}
	service := NewNotificationService(NotificationOptions{
		Store: fake, Sender: sender, Config: testNotificationConfig(),
		SMTP:       domain.SMTPSettings{Host: "smtp.example.test", Port: 587, StartTLS: true},
		SMTPSecret: domain.NotificationSecret{SMTPUsername: "relay", SMTPPassword: "hunter2"},
		Logger:     quietLogger(), Now: func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})

	service.route(context.Background(), queued{notification: testNotification()})

	requests := sender.requests()
	if len(requests) != 1 {
		t.Fatalf("sent %d messages, want 1", len(requests))
	}
	if requests[0].Secret.SMTPPassword != "hunter2" {
		t.Fatal("the relay credential did not reach the send")
	}
	if requests[0].SMTP.Host != "smtp.example.test" {
		t.Fatalf("relay host = %q", requests[0].SMTP.Host)
	}
	// And it did not come from, or get written to, the destination row.
	for _, record := range fake.records() {
		if strings.Contains(record.Title+record.Body+record.Error, "hunter2") {
			t.Fatal("the relay credential reached a delivery record")
		}
	}
}

// Every engine bound has a floor, so a zero-value configuration is safe rather
// than unbounded.
func TestAZeroConfigurationGetsBoundsRatherThanNone(t *testing.T) {
	t.Parallel()

	service := NewNotificationService(NotificationOptions{
		Store: newFakeNotificationStore(), Sender: &fakeSender{}, Logger: quietLogger(),
	})

	if service.cfg.QueueSize < 1 || cap(service.queue) < 1 {
		t.Fatalf("queue is unbounded or empty: cfg=%d cap=%d", service.cfg.QueueSize, cap(service.queue))
	}
	for name, value := range map[string]int{
		"workers":           service.cfg.Workers,
		"maxPerDestination": service.cfg.MaxPerDestination,
		"maxAttempts":       service.cfg.MaxAttempts,
	} {
		if value < 1 {
			t.Fatalf("%s = %d, want a positive bound", name, value)
		}
	}
	for name, value := range map[string]time.Duration{
		"deliveryTimeout": service.cfg.DeliveryTimeout,
		"retryBackoff":    service.cfg.RetryBackoff,
		"retryInterval":   service.cfg.RetryInterval,
		"pruneInterval":   service.cfg.PruneInterval,
	} {
		if value <= 0 {
			t.Fatalf("%s = %s, want a positive bound", name, value)
		}
	}
}

// Run stops when its context does, and does not leave workers behind.
func TestTheEngineStopsWhenItsContextDoes(t *testing.T) {
	t.Parallel()

	fake := newFakeNotificationStore()
	sender := &fakeSender{result: notify.Result{OK: true}}
	cfg := testNotificationConfig()
	cfg.RetryInterval = 10 * time.Millisecond
	cfg.PruneInterval = 10 * time.Millisecond
	service := newTestNotificationService(fake, sender, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		service.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when its context was cancelled; a shutdown that " +
			"cannot complete is not graceful")
	}
}

// quietLogger discards log output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestNotificationService builds a service with a fixed clock and a quiet
// logger.
func newTestNotificationService(
	fake *fakeNotificationStore,
	sender NotificationSender,
	cfg config.Notifications,
) *NotificationService {
	return NewNotificationService(NotificationOptions{
		Store: fake, Sender: sender, Config: cfg, Logger: quietLogger(),
		Now: func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
}
