package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// What an operator is actually told, driven through the real rollback service.
//
// The vocabulary tests next door prove each sentence is right. These prove the
// SERVICE picks the right one from the record it already has -- which is the
// part that was wrong before C4B, and the part a change to the pipeline could
// break without touching a single sentence.
//
// Nothing here adds state. Every discriminator is read off the rollback row the
// service was going to read anyway.

// automaticRollback tunes a harness into the automation case: the same request
// the automation follower submits, carrying the operator who started the pass.
//
// That combination is the whole point. It is what made the old code report an
// automatic rollback as operator-initiated.
func automaticRollback(t *testing.T) (*rbHarness, domain.Rollback) {
	t.Helper()

	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.notifier = &recordingNotifier{}
	})

	rollback, err := harness.service.Request(context.Background(),
		service.RollbackRequest{
			ExecutionID: rbExecutionID,
			RequestKey:  domain.AutomationRequestKeyPrefix + "rollback:" + rbExecutionID,
			// An operator triggered the automation pass. They are carried onto
			// the request for the audit trail, and they did NOT ask for this
			// rollback.
			RequestedBy: domain.Requester{
				UserID: "usr_0011223344556677889a", Username: "colby",
			},
		})
	if err != nil {
		t.Fatalf("rollback refused: %v", err)
	}
	return harness, rollback
}

// errRollbackStartRefused stands in for a daemon that will not start the
// original again. Its text never reaches a notification; the pipeline's own
// closed failure vocabulary does.
var errRollbackStartRefused = errors.New("the daemon refused to start the container")

func eventsOf(notifications []domain.Notification) []domain.NotificationEvent {
	events := make([]domain.NotificationEvent, 0, len(notifications))
	for _, notification := range notifications {
		events = append(events, notification.Event)
	}
	return events
}

func TestASuccessfulAutomaticRollbackReportsRecoveredAndNothingElse(t *testing.T) {
	// The sequence C4B exists to fix. Before it, this produced "rolling back"
	// and then "was rolled back" -- two messages, neither of which said the
	// update had failed, and the second of which reads as a success.
	harness, rollback := automaticRollback(t)

	final := harness.runOnce(t, rollback)
	if final.State != domain.RollbackSucceeded {
		t.Fatalf("the rollback did not succeed: %q", final.State)
	}

	sent := harness.notifier.all()
	if len(sent) != 1 {
		t.Fatalf("%d notifications for one automatic rollback, want 1: %v\n\n"+
			"An automatic rollback is one stage of a sequence whose ends already "+
			"speak. A message in the middle arrives looking like a third problem.",
			len(sent), eventsOf(sent))
	}
	if sent[0].Event != domain.EventUpdateRecovered {
		t.Fatalf("the event is %q, want %q\n\n"+
			"rollback.succeeded describes the rollback. The operator needs the "+
			"UPDATE described: it failed, and the container was put back.",
			sent[0].Event, domain.EventUpdateRecovered)
	}
	if sent[0].ContainerName != final.ContainerName {
		t.Errorf("container name = %q, want %q", sent[0].ContainerName, final.ContainerName)
	}
}

func TestASuccessfulManualRollbackKeepsItsExistingContract(t *testing.T) {
	// The pre-existing behaviour, unchanged. An operator who started a rollback
	// gets told it began and gets told it finished -- the comment on the
	// started notification explains why that one is raised at request time --
	// and nothing calls their deliberate action a recovery.
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.notifier = &recordingNotifier{}
	})

	rollback := harness.request(t)
	final := harness.runOnce(t, rollback)
	if final.State != domain.RollbackSucceeded {
		t.Fatalf("the rollback did not succeed: %q", final.State)
	}

	got := eventsOf(harness.notifier.all())
	want := []domain.NotificationEvent{
		domain.EventRollbackStarted, domain.EventRollbackSucceeded,
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
	for _, notification := range harness.notifier.all() {
		if notification.Event == domain.EventUpdateRecovered {
			t.Error("an operator's own rollback was reported as an automatic recovery")
		}
	}
}

func TestAnAutomaticRollbackRaisesNoStartedNotification(t *testing.T) {
	harness, rollback := automaticRollback(t)

	// Asserted at REQUEST time, before the worker runs: the started
	// notification is raised from Request, so waiting for the outcome would
	// test the wrong moment.
	for _, notification := range harness.notifier.all() {
		if notification.Event == domain.EventRollbackStarted {
			t.Fatal("an automatic rollback announced that it had started.\n\n" +
				"The operator has just been told the update failed and is about " +
				"to be told what became of the container. The message between " +
				"those two adds nothing and reads as a third alarm.")
		}
	}
	_ = rollback
}

func TestAFailedAutomaticRollbackIsStillReported(t *testing.T) {
	// The one outcome that must never be quieted. Suppressing the started
	// message for automatic rollbacks must not have suppressed the failure:
	// this is a container that is neither on its new image nor its old one.
	harness, rollback := automaticRollback(t)

	// Break the restart so the rollback cannot put the original back.
	harness.host.setErr(&harness.host.startErr, errRollbackStartRefused)

	final := harness.runOnce(t, rollback)
	if final.State != domain.RollbackFailed {
		t.Fatalf("the rollback did not fail: %q", final.State)
	}

	sent := harness.notifier.all()
	if len(sent) != 1 {
		t.Fatalf("%d notifications, want 1: %v", len(sent), eventsOf(sent))
	}
	if sent[0].Event != domain.EventRollbackFailed {
		t.Fatalf("the event is %q, want %q", sent[0].Event, domain.EventRollbackFailed)
	}
	if sent[0].Severity != domain.NotifyCritical {
		t.Errorf("severity = %q, want critical", sent[0].Severity)
	}
	if sent[0].Event == domain.EventUpdateRecovered {
		t.Error("a FAILED rollback was reported as a recovery")
	}
}

func TestReportingTheSameOutcomeTwiceIsOneLogicalNotification(t *testing.T) {
	// Restart and retry behaviour, expressed as the property that matters. The
	// outcome reporter reads a terminal record; a process that restarted, or a
	// worker that re-reported, must produce the same dedup key rather than a
	// new logical message.
	//
	// Deduplication itself is the engine's job and is durable in the database;
	// what is checked here is that the SOURCE offers a stable identity for it
	// to work with, because a key derived from time or from message text would
	// defeat it silently.
	harness, rollback := automaticRollback(t)

	final := harness.runOnce(t, rollback)
	if final.State != domain.RollbackSucceeded {
		t.Fatalf("the rollback did not succeed: %q", final.State)
	}
	first := harness.notifier.all()

	// The same terminal record, reported again -- what a restarted process
	// re-reading a settled row would do.
	service.NotifyUpdateRecovered(harness.notifier, final.ContainerName,
		final.ReplacementImage, final.OriginalImage,
		final.RollbackID, final.ExecutionID)

	sent := harness.notifier.all()
	if sent[len(sent)-1].DedupKey != first[0].DedupKey {
		t.Errorf("re-reporting the same rollback produced the key %q, "+
			"first time %q\n\n"+
			"Two keys mean two logical notifications, and the operator is told "+
			"twice that a container they already dealt with fell over.",
			sent[len(sent)-1].DedupKey, first[0].DedupKey)
	}
}

func TestADisabledNotifierChangesNoRollbackOutcome(t *testing.T) {
	// Delivery is secondary. A deployment with notifications off must reach the
	// same recorded outcome as one with them on, and the notifier being absent
	// must not be a branch the pipeline can trip over.
	off := newRollbackHarness(t)
	on := newRollbackHarness(t, func(h *rbHarness) {
		h.notifier = &recordingNotifier{}
	})

	quiet := off.runOnce(t, off.request(t))
	loud := on.runOnce(t, on.request(t))

	if quiet.State != loud.State {
		t.Errorf("with notifications off the rollback ended %q; with them on, %q\n\n"+
			"A webhook must never be able to change what happened to a container.",
			quiet.State, loud.State)
	}
	if quiet.State != domain.RollbackSucceeded {
		t.Errorf("state = %q, want succeeded", quiet.State)
	}
}

func TestANotifierThatPanicsOnEveryCallStillLetsARollbackSucceed(t *testing.T) {
	// The strongest form of "delivery failure cannot change lifecycle state".
	// A destination that is unreachable is the ordinary case and is handled
	// inside the engine; this is the case where the notification path itself is
	// broken, which is the one that could take the pipeline with it.
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.notifier = &recordingNotifier{}
	})
	harness.notifier.explode = true

	final := harness.runOnce(t, harness.request(t))

	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state = %q, want succeeded\n\n"+
			"A container that was correctly restored must not be recorded as "+
			"anything else because a notification could not be raised.",
			final.State)
	}
}
