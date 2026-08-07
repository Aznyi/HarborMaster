package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Notification persistence tests.
//
// The properties that matter here are the ones that keep a credential out of
// everything except the one method that is allowed to return it, and the ones
// that keep the tables bounded. The rest -- create, read, list -- is covered
// incidentally by the tests that need it.

func newTestDestination(name string) domain.NotificationDestination {
	destination := domain.NotificationDestination{
		DestinationID: domain.NewNotificationDestinationID(),
		Name:          name,
		Channel:       domain.ChannelWebhook,
		Enabled:       true,
		Endpoint:      "https://hooks.example.test",
	}
	destination.Normalise()
	return destination
}

func createDestination(
	t *testing.T,
	db *store.DB,
	destination domain.NotificationDestination,
	secret domain.NotificationSecret,
) domain.NotificationDestination {
	t.Helper()
	created, err := db.Notifications.CreateDestination(
		context.Background(), destination, secret, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateDestination: %v", err)
	}
	return created
}

func newTestNotificationRule(name string, destinations ...string) domain.NotificationRule {
	rule := domain.NotificationRule{
		RuleID:          domain.NewNotificationRuleID(),
		Name:            name,
		Enabled:         true,
		MinimumSeverity: domain.NotifyInfo,
		Destinations:    destinations,
	}
	rule.Normalise()
	return rule
}

func createNotificationRule(
	t *testing.T,
	db *store.DB,
	rule domain.NotificationRule,
) domain.NotificationRule {
	t.Helper()
	created, err := db.Notifications.CreateRule(context.Background(), rule, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	return created
}

// recordDelivery writes a delivery the way the engine does: a pending row, then
// its outcome. The schema refuses a settled delivery with no attempts, which is
// what makes "this succeeded but was never tried" unrepresentable.
func recordDelivery(
	t *testing.T,
	db *store.DB,
	destination domain.NotificationDestination,
	rule domain.NotificationRule,
	result domain.DeliveryResult,
	queuedAt time.Time,
	nextAttemptAt *time.Time,
) domain.NotificationDelivery {
	t.Helper()
	ctx := context.Background()

	delivery := domain.NotificationDelivery{
		DeliveryID:      domain.NewNotificationDeliveryID(),
		DestinationID:   destination.DestinationID,
		DestinationName: destination.Name,
		Channel:         destination.Channel,
		RuleID:          rule.RuleID,
		RuleName:        rule.Name,
		Event:           domain.EventUpdateDiscovered,
		Severity:        domain.NotifyInfo,
		Title:           "An update is available",
		Result:          domain.DeliveryPending,
		QueuedAt:        queuedAt,
	}
	if err := db.Notifications.RecordDelivery(ctx, delivery); err != nil {
		t.Fatalf("RecordDelivery: %v", err)
	}
	if result == domain.DeliveryPending {
		return delivery
	}
	if err := db.Notifications.CompleteDelivery(ctx, delivery.DeliveryID, result, 1, 200,
		"", nextAttemptAt, queuedAt, 12); err != nil {
		t.Fatalf("CompleteDelivery: %v", err)
	}
	delivery.Result = result
	return delivery
}

// ------------------------------------------------------------ credentials --

// The credential is reachable through exactly one method.
//
// Every other read path returns the public record. This is what makes "a
// handler that never loads a secret cannot leak one" true rather than hoped
// for: there is nothing in what a handler gets back that could carry it.
func TestOnlyOneReadPathReturnsACredential(t *testing.T) {
	t.Parallel()

	const webhookURL = "https://hooks.example.test/services/T000/B000/SECRETPATHVALUE"

	db := openTestDB(t)
	ctx := context.Background()
	created := createDestination(t, db, newTestDestination("chat"),
		domain.NotificationSecret{URL: webhookURL})

	// The public record -- from every path that returns one.
	byID, err := db.Notifications.DestinationByID(ctx, created.DestinationID)
	if err != nil {
		t.Fatalf("DestinationByID: %v", err)
	}
	listed, _, err := db.Notifications.ListDestinations(ctx, false, store.Page{Limit: 10})
	if err != nil {
		t.Fatalf("ListDestinations: %v", err)
	}
	active, err := db.Notifications.ActiveDestinations(ctx)
	if err != nil {
		t.Fatalf("ActiveDestinations: %v", err)
	}

	records := append([]domain.NotificationDestination{byID}, listed...)
	records = append(records, active...)
	for _, record := range records {
		rendered := record.Name + record.Description + record.Endpoint +
			record.TitlePrefix + record.EmailFrom + record.LastError +
			strings.Join(record.EmailTo, " ")
		if strings.Contains(rendered, "SECRETPATHVALUE") {
			t.Fatalf("a public destination record carries the webhook path, which IS the "+
				"credential for Slack, Discord, and Teams: %q", rendered)
		}
		if record.Endpoint != "https://hooks.example.test" {
			t.Fatalf("endpoint = %q, want the scheme and host only", record.Endpoint)
		}
	}

	// And the one method that is allowed to returns it.
	secret, err := db.Notifications.Secret(ctx, created.DestinationID)
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if secret.URL != webhookURL {
		t.Fatalf("Secret().URL = %q, want the stored URL", secret.URL)
	}
}

// Archiving a destination destroys its credential.
//
// An archived destination cannot send, so keeping its URL would be keeping a
// credential for no reason.
func TestArchivingADestinationDestroysItsCredential(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	created := createDestination(t, db, newTestDestination("chat"),
		domain.NotificationSecret{URL: "https://hooks.example.test/t/SECRET"})

	if err := db.Notifications.ArchiveDestination(ctx, created.DestinationID, time.Now().UTC()); err != nil {
		t.Fatalf("ArchiveDestination: %v", err)
	}

	secret, err := db.Notifications.Secret(ctx, created.DestinationID)
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if secret.URL != "" {
		t.Fatalf("the credential survived archiving: %q", secret.URL)
	}
}

// An edit that does not mention the credential leaves it alone.
//
// Otherwise renaming a destination would silently blank a URL the operator may
// no longer have a copy of.
func TestAnEditThatDoesNotMentionTheCredentialKeepsIt(t *testing.T) {
	t.Parallel()

	const webhookURL = "https://hooks.example.test/t/KEEPTHISVALUE"

	db := openTestDB(t)
	ctx := context.Background()
	created := createDestination(t, db, newTestDestination("chat"),
		domain.NotificationSecret{URL: webhookURL})

	renamed := "operations chat"
	if _, err := db.Notifications.UpdateDestination(ctx, created.DestinationID,
		store.DestinationChange{Name: &renamed}, time.Now().UTC()); err != nil {
		t.Fatalf("UpdateDestination: %v", err)
	}

	secret, err := db.Notifications.Secret(ctx, created.DestinationID)
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if secret.URL != webhookURL {
		t.Fatalf("an edit that did not mention the URL changed it: %q", secret.URL)
	}
}

// ------------------------------------------------------------- referential --

// A destination a rule still routes to cannot be archived.
//
// Otherwise a rule would silently stop delivering and nothing would say so.
func TestADestinationARuleRoutesToCannotBeArchived(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	destination := createDestination(t, db, newTestDestination("chat"),
		domain.NotificationSecret{URL: "https://hooks.example.test/t/x"})
	rule := createNotificationRule(t, db,
		newTestNotificationRule("everything", destination.DestinationID))

	err := db.Notifications.ArchiveDestination(ctx, destination.DestinationID, time.Now().UTC())
	if !errors.Is(err, store.ErrDestinationInUse) {
		t.Fatalf("ArchiveDestination error = %v, want %v", err, store.ErrDestinationInUse)
	}

	// Once the rule is gone the destination may go.
	if err := db.Notifications.ArchiveRule(ctx, rule.RuleID, time.Now().UTC()); err != nil {
		t.Fatalf("ArchiveRule: %v", err)
	}
	if err := db.Notifications.ArchiveDestination(ctx, destination.DestinationID,
		time.Now().UTC()); err != nil {
		t.Fatalf("ArchiveDestination after the rule went: %v", err)
	}
}

// Two live destinations cannot share a name.
//
// The name is what an operator picks a destination by in a rule editor, and two
// with one name is a configuration nobody can reason about.
func TestTwoLiveDestinationsCannotShareAName(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	createDestination(t, db, newTestDestination("chat"), domain.NotificationSecret{
		URL: "https://hooks.example.test/t/a"})

	_, err := db.Notifications.CreateDestination(ctx, newTestDestination("chat"),
		domain.NotificationSecret{URL: "https://hooks.example.test/t/b"}, time.Now().UTC())
	if !errors.Is(err, store.ErrDestinationNameTaken) {
		t.Fatalf("CreateDestination error = %v, want %v", err, store.ErrDestinationNameTaken)
	}
}

// ---------------------------------------------------------- deduplication --

// The cooldown check and the record it makes are one transaction.
//
// Two notifications with one key arriving together would otherwise both read
// "not sent recently" and both send.
func TestASecondNotificationInsideTheCooldownIsSuppressed(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	rule := createNotificationRule(t, db, newTestNotificationRule("everything",
		domain.NewNotificationDestinationID()))

	now := time.Now().UTC()
	first, err := db.Notifications.ShouldSuppress(ctx, rule.RuleID, "web:1.2.3", 5*time.Minute, now)
	if err != nil {
		t.Fatalf("ShouldSuppress: %v", err)
	}
	if first {
		t.Fatal("the first occurrence was suppressed")
	}

	second, err := db.Notifications.ShouldSuppress(ctx, rule.RuleID, "web:1.2.3",
		5*time.Minute, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ShouldSuppress: %v", err)
	}
	if !second {
		t.Fatal("a repeat inside the cooldown was not suppressed")
	}

	// Past the window it sends again.
	third, err := db.Notifications.ShouldSuppress(ctx, rule.RuleID, "web:1.2.3",
		5*time.Minute, now.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("ShouldSuppress: %v", err)
	}
	if third {
		t.Fatal("a notification past the cooldown was still suppressed")
	}

	// A different key is unaffected, and a different rule is unaffected.
	other, err := db.Notifications.ShouldSuppress(ctx, rule.RuleID, "db:2.0.0",
		5*time.Minute, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ShouldSuppress: %v", err)
	}
	if other {
		t.Fatal("a different dedup key was suppressed by an unrelated one")
	}
}

// A zero cooldown suppresses nothing and records nothing.
//
// An operator who asked for every occurrence gets every one, and the dedup
// table does not grow for rules that never use it.
func TestAZeroCooldownSuppressesNothing(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	rule := createNotificationRule(t, db, newTestNotificationRule("everything",
		domain.NewNotificationDestinationID()))

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		suppressed, err := db.Notifications.ShouldSuppress(ctx, rule.RuleID, "web:1.2.3", 0, now)
		if err != nil {
			t.Fatalf("ShouldSuppress: %v", err)
		}
		if suppressed {
			t.Fatal("a zero cooldown suppressed a notification")
		}
	}
}

// ------------------------------------------------------------- retention --

// Retention deletes settled history and leaves work in flight alone.
//
// Pruning a pending or retrying delivery would lose a message that is still
// going to be sent, and the retry sweep would never find it again.
func TestRetentionKeepsWorkInFlightAndBoundsTheDedupTable(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	destination := createDestination(t, db, newTestDestination("chat"),
		domain.NotificationSecret{URL: "https://hooks.example.test/t/x"})
	rule := createNotificationRule(t, db,
		newTestNotificationRule("everything", destination.DestinationID))

	old := time.Now().UTC().Add(-48 * time.Hour)
	retryAt := old.Add(time.Minute)
	for _, settled := range []struct {
		result domain.DeliveryResult
		next   *time.Time
	}{
		{domain.DeliverySucceeded, nil},
		{domain.DeliveryFailed, nil},
		{domain.DeliveryPending, nil},
		{domain.DeliveryRetrying, &retryAt},
	} {
		recordDelivery(t, db, destination, rule, settled.result, old, settled.next)
	}

	// A dedup key from the same era.
	if _, err := db.Notifications.ShouldSuppress(ctx, rule.RuleID, "web:1.2.3",
		time.Hour, old); err != nil {
		t.Fatalf("ShouldSuppress: %v", err)
	}

	deleted, err := db.Notifications.PruneDeliveries(ctx, time.Now().UTC().Add(-24*time.Hour), 0)
	if err != nil {
		t.Fatalf("PruneDeliveries: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("pruned %d deliveries, want 2 (succeeded and failed; pending and "+
			"retrying are still work)", deleted)
	}

	remaining, _, err := db.Notifications.ListDeliveries(ctx, store.DeliveryFilter{
		Page: store.Page{Limit: 50}})
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	for _, delivery := range remaining {
		if delivery.Result != domain.DeliveryPending && delivery.Result != domain.DeliveryRetrying {
			t.Fatalf("a settled delivery survived retention: %q", delivery.Result)
		}
	}

	// And the dedup key went with the cutoff, so that table does not become the
	// one that grows forever.
	suppressed, err := db.Notifications.ShouldSuppress(ctx, rule.RuleID, "web:1.2.3",
		time.Hour, old.Add(time.Minute))
	if err != nil {
		t.Fatalf("ShouldSuppress: %v", err)
	}
	if suppressed {
		t.Fatal("a deduplication key survived retention; that table would grow forever")
	}
}

// A retry becomes due only when its time comes, and only while it is retrying.
func TestOnlyDueRetriesAreReturned(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	destination := createDestination(t, db, newTestDestination("chat"),
		domain.NotificationSecret{URL: "https://hooks.example.test/t/x"})

	now := time.Now().UTC()
	soon := now.Add(-time.Minute)
	later := now.Add(time.Hour)

	for _, schedule := range []struct {
		result domain.DeliveryResult
		next   *time.Time
	}{
		{domain.DeliveryRetrying, &soon},
		{domain.DeliveryRetrying, &later},
		{domain.DeliverySucceeded, nil},
		{domain.DeliveryFailed, nil},
	} {
		recordDelivery(t, db, destination, domain.NotificationRule{}, schedule.result,
			now, schedule.next)
	}

	due, err := db.Notifications.DueRetries(ctx, now, 50)
	if err != nil {
		t.Fatalf("DueRetries: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("DueRetries returned %d, want 1: only a retrying delivery whose time "+
			"has come is due", len(due))
	}
}

// The delivery listing is bounded even when the caller asks for everything.
func TestTheDeliveryListingIsBounded(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	destination := createDestination(t, db, newTestDestination("chat"),
		domain.NotificationSecret{URL: "https://hooks.example.test/t/x"})

	now := time.Now().UTC()
	for i := 0; i < 40; i++ {
		recordDelivery(t, db, destination, domain.NotificationRule{},
			domain.DeliverySucceeded, now, nil)
	}

	for _, limit := range []int{0, -1, 100000} {
		page, total, err := db.Notifications.ListDeliveries(ctx, store.DeliveryFilter{
			Page: store.Page{Limit: limit}})
		if err != nil {
			t.Fatalf("ListDeliveries(limit=%d): %v", limit, err)
		}
		if len(page) > 40 {
			t.Fatalf("ListDeliveries(limit=%d) returned %d rows", limit, len(page))
		}
		if total != 40 {
			t.Fatalf("total = %d, want 40", total)
		}
	}
}

// A filter's identifiers and vocabularies never become SQL text.
func TestADeliveryFilterCannotInjectSQL(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	destination := createDestination(t, db, newTestDestination("chat"),
		domain.NotificationSecret{URL: "https://hooks.example.test/t/x"})
	recordDelivery(t, db, destination, domain.NotificationRule{},
		domain.DeliverySucceeded, time.Now().UTC(), nil)

	for _, hostile := range []string{
		"' OR 1=1 --",
		"'; DROP TABLE notification_deliveries; --",
		"%",
		"_",
	} {
		if _, _, err := db.Notifications.ListDeliveries(ctx, store.DeliveryFilter{
			DestinationID: hostile,
			ContainerName: hostile,
			Results:       []domain.DeliveryResult{domain.DeliveryResult(hostile)},
			Events:        []domain.NotificationEvent{domain.NotificationEvent(hostile)},
			Page:          store.Page{Limit: 10},
		}); err != nil {
			t.Fatalf("ListDeliveries(%q): %v", hostile, err)
		}
	}

	// The table is still there, with its row.
	_, total, err := db.Notifications.ListDeliveries(ctx, store.DeliveryFilter{
		Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1; the table did not survive the hostile filters", total)
	}
}

// A destination's health is denormalised, so a list can say "this is not
// working" without joining the history.
func TestConsecutiveFailuresCountUpAndResetOnSuccess(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	destination := createDestination(t, db, newTestDestination("chat"),
		domain.NotificationSecret{URL: "https://hooks.example.test/t/x"})

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if err := db.Notifications.RecordDestinationResult(ctx, destination.DestinationID,
			domain.DeliveryFailed, "the destination could not be reached", now); err != nil {
			t.Fatalf("RecordDestinationResult: %v", err)
		}
	}
	failing, err := db.Notifications.DestinationByID(ctx, destination.DestinationID)
	if err != nil {
		t.Fatalf("DestinationByID: %v", err)
	}
	if failing.ConsecutiveFailures != 3 {
		t.Fatalf("consecutiveFailures = %d, want 3", failing.ConsecutiveFailures)
	}

	if err := db.Notifications.RecordDestinationResult(ctx, destination.DestinationID,
		domain.DeliverySucceeded, "", now); err != nil {
		t.Fatalf("RecordDestinationResult: %v", err)
	}
	recovered, err := db.Notifications.DestinationByID(ctx, destination.DestinationID)
	if err != nil {
		t.Fatalf("DestinationByID: %v", err)
	}
	if recovered.ConsecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d after a success, want 0", recovered.ConsecutiveFailures)
	}
	if recovered.LastError != "" {
		t.Fatalf("lastError = %q after a success, want empty", recovered.LastError)
	}
}
