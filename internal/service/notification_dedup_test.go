package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/notify"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Not telling an operator the same thing ninety-six times a day.
//
// # The two halves
//
// Deduplication in HarborMaster is a dedup KEY the raiser derives from a
// durable lifecycle identity, and a WINDOW the engine resolves from the rule
// and the event. Both have to be right, and they fail in different ways: a bad
// key sends duplicates that look like separate incidents, a missing window
// sends duplicates that look like the same incident happening over and over.
//
// The keys are checked next door, against the raisers. This file checks the
// window: that the engine asks for the right one, and that the answer survives
// a restart -- because a suppression window held in memory would let every
// process restart re-send everything that was standing at the time.

// deliveredWindow drives one notification through the engine and returns the
// cooldown the engine asked the store for.
func deliveredWindow(
	t *testing.T,
	notification domain.Notification,
	ruleCooldown time.Duration,
) time.Duration {
	t.Helper()

	fake := newFakeNotificationStore()
	fake.destinations = []domain.NotificationDestination{
		testDestination("ndst_0123456789abcdef0123", "ops"),
	}
	rule := testRule("nrul_0123456789abcdef0123", "ndst_0123456789abcdef0123")
	rule.Events = []domain.NotificationEvent{notification.Event}
	rule.CooldownSeconds = int(ruleCooldown / time.Second)
	fake.rules = []domain.NotificationRule{rule}

	engine := newTestNotificationService(fake,
		&fakeSender{result: notify.Result{OK: true}}, testNotificationConfig())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stopped := make(chan struct{})
	go func() { defer close(stopped); engine.Run(ctx) }()

	engine.Raise(notification)

	deadline := time.After(4 * time.Second)
	for {
		if cooldown, seen := fake.cooldownFor(notification.DedupKey); seen {
			cancel()
			<-stopped
			return cooldown
		}
		select {
		case <-deadline:
			cancel()
			<-stopped
			t.Fatal("the engine never evaluated a suppression window")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestTheEngineAsksForTheEventsFloorWhenARuleSetsNoCooldown(t *testing.T) {
	// The default. A rule created without a cooldown gets zero, and before C4B
	// that meant an update awaiting approval was announced on every scheduler
	// pass for as long as nobody approved it.
	approval := domain.Notification{
		Event:      domain.EventApprovalRequired,
		Severity:   domain.NotifyWarning,
		Title:      "web needs approval before it can be updated",
		DedupKey:   "approval:plan_0123456789abcdef0123",
		OccurredAt: time.Unix(1700000000, 0).UTC(),
	}

	got := deliveredWindow(t, approval, 0)

	if want := domain.EventApprovalRequired.MinimumCooldown(); got != want {
		t.Errorf("the engine asked for a %s window, want the event's floor of %s\n\n"+
			"A zero window on a condition re-derived every pass is ninety-six "+
			"identical messages a day at the default interval, and an operator "+
			"who gets those turns the channel off -- which loses the failed "+
			"rollback too.", got, want)
	}
}

func TestTheEngineHonoursALongerRuleCooldown(t *testing.T) {
	// A floor, never a cap. An operator asking for more quiet than the floor
	// gets exactly what they asked for.
	approval := domain.Notification{
		Event:      domain.EventApprovalRequired,
		Severity:   domain.NotifyWarning,
		Title:      "web needs approval before it can be updated",
		DedupKey:   "approval:plan_0123456789abcdef0123",
		OccurredAt: time.Unix(1700000000, 0).UTC(),
	}

	if got := deliveredWindow(t, approval, 6*time.Hour); got != 6*time.Hour {
		t.Errorf("the engine asked for %s, want the rule's own 6h", got)
	}
}

func TestTheEngineDoesNotThrottleAnEdgeTriggeredEvent(t *testing.T) {
	// The negative that keeps the floor honest. A failed recreation is raised
	// when a recreation failed; a floor here would swallow the SECOND real
	// failure, which is a different container or the same one failing again
	// after somebody thought they had fixed it.
	failure := domain.Notification{
		Event:      domain.EventExecutionFailed,
		Severity:   domain.NotifyCritical,
		Title:      "web could not be updated",
		DedupKey:   "execution:exec_0123456789abcdef0123",
		OccurredAt: time.Unix(1700000000, 0).UTC(),
	}

	if got := deliveredWindow(t, failure, 0); got != 0 {
		t.Errorf("the engine asked for a %s window on a failed recreation, want none", got)
	}
}

// ------------------------------------------------------- across a restart --

// TestASuppressionWindowSurvivesARestart is the property a scheduler depends on.
//
// The engine holds no suppression state in memory: the window lives in a table.
// Without that, every restart would re-announce everything that was standing at
// the time -- and a process that restarts often, which is exactly what a
// deployment with a crash-looping container does, would be the noisiest one.
//
// Real SQLite and a real repository, opened TWICE over the same file, because
// the property is about what the second process sees.
func TestASuppressionWindowSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbormaster.db")
	now := time.Unix(1700000000, 0).UTC()

	const (
		ruleID   = "nrul_0123456789abcdef0123"
		dedupKey = "approval:plan_0123456789abcdef0123"
	)
	cooldown := domain.EventApprovalRequired.MinimumCooldown()

	first := openNotificationDB(t, path)
	suppressed, err := first.Notifications.ShouldSuppress(
		context.Background(), ruleID, dedupKey, cooldown, now)
	if err != nil {
		t.Fatalf("first evaluation: %v", err)
	}
	if suppressed {
		t.Fatal("the FIRST occurrence was suppressed; nobody would ever be told")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A new process over the same database, one scheduler tick later.
	second := openNotificationDB(t, path)
	suppressed, err = second.Notifications.ShouldSuppress(
		context.Background(), ruleID, dedupKey, cooldown, now.Add(15*time.Minute))
	if err != nil {
		t.Fatalf("second evaluation: %v", err)
	}
	if !suppressed {
		t.Error("a restarted process re-announced a condition that had not changed.\n\n" +
			"This is the notification storm: a deployment whose container is " +
			"crash-looping restarts often, and would be told about the same " +
			"pending approval on every one of them.")
	}

	// And the window still ENDS. Suppression that never lifted would be a lost
	// message rather than a quiet one.
	suppressed, err = second.Notifications.ShouldSuppress(
		context.Background(), ruleID, dedupKey, cooldown, now.Add(cooldown+time.Minute))
	if err != nil {
		t.Fatalf("third evaluation: %v", err)
	}
	if suppressed {
		t.Error("the window never expired; an operator who missed the first " +
			"message is never reminded")
	}
}

func TestDeduplicationIsKeyedOnIdentityAndNotOnText(t *testing.T) {
	// Two containers with the same problem produce the same SENTENCE shape and
	// must still both be reported. Keying on text would report one and swallow
	// the other, and the swallowed one is a container nobody looks at.
	path := filepath.Join(t.TempDir(), "harbormaster.db")
	now := time.Unix(1700000000, 0).UTC()
	db := openNotificationDB(t, path)
	cooldown := time.Hour

	for _, key := range []string{"approval:plan_A", "approval:plan_B"} {
		suppressed, err := db.Notifications.ShouldSuppress(
			context.Background(), "nrul_0123456789abcdef0123", key, cooldown, now)
		if err != nil {
			t.Fatalf("evaluate %s: %v", key, err)
		}
		if suppressed {
			t.Errorf("%s was suppressed by a different plan's window", key)
		}
	}
}

// openNotificationDB opens a real database at path.
func openNotificationDB(t *testing.T, path string) *store.DB {
	t.Helper()

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
