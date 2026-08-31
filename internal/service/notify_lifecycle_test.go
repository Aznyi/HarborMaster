package service_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// The three update outcomes, and keeping them three.
//
// # What this file is defending
//
// HarborMaster's persisted lifecycle already distinguishes an update that
// worked, an update that failed and left a container broken, and an update that
// failed and was undone automatically. Before C4B the notifications collapsed
// the third into the other two: an operator whose container broke and was
// restored while they slept got a CRITICAL "could not be updated", then
// "rolling back", then "was rolled back" -- three alarms, none of which said
// the service was fine, and the last of which reads as a success.
//
// Every test here is written so its failure message says what the operator
// would be told wrongly. A notification that lies about whether a container is
// running is worse than no notification: it is the one an operator acts on.

// --------------------------------------------------- the three outcomes --

func TestTheThreeUpdateOutcomesAreThreeDifferentEvents(t *testing.T) {
	t.Parallel()

	notifier := &recordingNotifier{}

	service.NotifyExecutionSucceeded(notifier, "web", "nginx:1.27.1", "exec_1")
	service.NotifyExecutionFailed(notifier, "web", "exec_2", "it did not verify.", true, false)
	service.NotifyUpdateRecovered(notifier, "web",
		"nginx:1.27.1", "nginx:1.27.0", "rb_1", "exec_3")

	sent := notifier.all()
	if len(sent) != 3 {
		t.Fatalf("%d notifications, want 3", len(sent))
	}

	events := map[domain.NotificationEvent]bool{}
	for _, notification := range sent {
		if events[notification.Event] {
			t.Fatalf("two outcomes share the event %q\n\n"+
				"Succeeded, failed, and recovered are three different things "+
				"that happened to a container. A rule that selects one must not "+
				"select another.", notification.Event)
		}
		events[notification.Event] = true
	}

	if !events[domain.EventExecutionSucceeded] ||
		!events[domain.EventExecutionFailed] ||
		!events[domain.EventUpdateRecovered] {
		t.Errorf("the three outcomes are %v", events)
	}
}

func TestARecoveredUpdateIsNeverCalledSuccessful(t *testing.T) {
	t.Parallel()

	// THE CENTRAL RULE OF THIS BATCH. A recovered update is a FAILED update
	// whose damage was undone. Telling an operator it succeeded tells them the
	// image they approved is running, and it is not.
	notifier := &recordingNotifier{}
	service.NotifyUpdateRecovered(notifier, "web",
		"nginx:1.27.1", "nginx:1.27.0", "rb_1", "exec_3")

	recovered := notifier.all()[0]

	if recovered.Event == domain.EventExecutionSucceeded {
		t.Fatal("a recovered update was raised as execution.succeeded")
	}
	if recovered.Severity == domain.NotifyInfo {
		t.Errorf("severity = %q\n\n"+
			"Info is the severity of a thing that went well. An operator whose "+
			"threshold is 'warnings and worse' must still hear that an "+
			"unattended update failed.", recovered.Severity)
	}

	text := strings.ToLower(recovered.Title + " " + recovered.Body)
	for _, forbidden := range []string{
		"was updated", "update succeeded", "updated successfully", "successfully updated",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("a recovered update says %q:\n\t%s\n\t%s",
				forbidden, recovered.Title, recovered.Body)
		}
	}
	// And it must positively say the two things that are true.
	if !strings.Contains(text, "fail") {
		t.Error("the recovered message never says the update failed.\n\n" +
			"An operator who reads only the title must not come away thinking " +
			"this was a normal update.")
	}
	if !strings.Contains(text, "restor") && !strings.Contains(text, "rolled") {
		t.Error("the recovered message never says the container was put back.\n\n" +
			"Without that it is indistinguishable from an unrecovered failure, " +
			"and an operator gets out of bed for a container that is running.")
	}
}

func TestARecoveredUpdateCarriesTheContextToUnderstandIt(t *testing.T) {
	t.Parallel()

	notifier := &recordingNotifier{}
	service.NotifyUpdateRecovered(notifier, "web",
		"nginx:1.27.1", "nginx:1.27.0", "rb_0123456789abcdef0123", "exec_0123456789abcdef")

	recovered := notifier.all()[0]

	if recovered.ContainerName != "web" {
		t.Errorf("container name = %q", recovered.ContainerName)
	}

	values := map[string]string{}
	for _, f := range recovered.Fields {
		values[f.Label] = f.Value
	}
	if values["Attempted"] != "nginx:1.27.1" {
		t.Errorf("the attempted target is %q; without it an operator cannot tell "+
			"which image is bad", values["Attempted"])
	}
	if values["Running"] != "nginx:1.27.0" {
		t.Errorf("what is running now is %q; it is the answer to the only "+
			"urgent question", values["Running"])
	}
	if values["Rollback"] == "" || values["Execution"] == "" {
		t.Error("the recovered message names neither record, so nothing can be " +
			"looked up afterwards")
	}
}

// ------------------------------------------------- manual versus automatic --

func TestAManualFailureDoesNotImplyAutomaticRecovery(t *testing.T) {
	t.Parallel()

	// The product promise. HarborMaster does NOT undo an update a person asked
	// for. A message that leaves that open is a message an operator waits on.
	notifier := &recordingNotifier{}
	service.NotifyExecutionFailed(notifier, "web", "exec_1",
		"it did not pass verification.", true, false)

	body := strings.ToLower(notifier.all()[0].Body)

	if !strings.Contains(body, "does not roll back") {
		t.Errorf("a manual failure does not say HarborMaster will not undo it:\n\t%s\n\n"+
			"Silence here reads as 'something is handling it'. Nothing is.", body)
	}
	if strings.Contains(body, "will attempt to roll it back") {
		t.Errorf("a manual failure promises an automatic rollback:\n\t%s", body)
	}
}

func TestAnAutomaticFailurePromisesOnlyAnAttempt(t *testing.T) {
	t.Parallel()

	notifier := &recordingNotifier{}
	service.NotifyExecutionFailed(notifier, "web", "exec_1",
		"it did not pass verification.", true, true)

	body := strings.ToLower(notifier.all()[0].Body)

	if !strings.Contains(body, "roll it back") {
		t.Errorf("an automatic failure says nothing about what happens next:\n\t%s", body)
	}
	// Hedged, because whether a rollback is permitted is the policy's business
	// and is decided AFTER this message leaves.
	if !strings.Contains(body, "if the policy allows") {
		t.Errorf("an automatic failure promises a rollback unconditionally:\n\t%s\n\n"+
			"Whether one happens is the governing policy's decision, made after "+
			"this message was sent. Promising it produces an operator who waits "+
			"for a recovery that was never permitted.", body)
	}
}

func TestAutomaticIsReadFromTheDurableRequestKeyNotTheRequester(t *testing.T) {
	t.Parallel()

	// The defect this replaced: an operator who triggers an automation pass is
	// carried onto every request that pass submits, so "has a requester" is not
	// "a person asked for this". Reading it that way reported an automatic
	// rollback as one an operator started.
	operator := domain.Requester{UserID: "usr_0011223344556677889a", Username: "colby"}

	automatic := domain.Rollback{
		RequestKey:  domain.AutomationRequestKeyPrefix + "rollback:exec_1",
		RequestedBy: operator,
	}
	if !automatic.Automatic() {
		t.Error("a rollback automation submitted during an operator-triggered pass " +
			"is reported as operator-initiated.\n\n" +
			"This is the message that tells somebody a person is dealing with " +
			"a container that nobody is dealing with.")
	}

	manual := domain.Rollback{RequestKey: "rb:req:abc", RequestedBy: operator}
	if manual.Automatic() {
		t.Error("an operator's own rollback is reported as automatic")
	}
	// The zero value is the important negative: an unattributed manual request
	// must not become automatic just because nobody was named.
	if (domain.Rollback{}).Automatic() {
		t.Error("a rollback with no request key at all is reported as automatic")
	}

	if !(domain.Execution{
		RequestKey: domain.AutomationRequestKeyPrefix + "execute:acq_1",
	}).Automatic() {
		t.Error("an automation-submitted recreation is reported as manual")
	}
	if (domain.Execution{RequestKey: "exec:req:abc"}).Automatic() {
		t.Error("an operator's own recreation is reported as automatic")
	}
}

func TestAFailedAutomaticRollbackSaysTheUpdateIsUnrecovered(t *testing.T) {
	t.Parallel()

	notifier := &recordingNotifier{}
	service.NotifyRollbackFailed(notifier, "web", "rb_1", "it did not verify.", true)
	service.NotifyRollbackFailed(notifier, "api", "rb_2", "it did not verify.", false)

	sent := notifier.all()
	for _, notification := range sent {
		if notification.Severity != domain.NotifyCritical {
			t.Errorf("%s severity = %q; a container that is neither on its new "+
				"image nor its old one always needs a person",
				notification.ContainerName, notification.Severity)
		}
	}

	automatic := strings.ToLower(sent[0].Title + " " + sent[0].Body)
	if !strings.Contains(automatic, "unattended") {
		t.Errorf("a failed AUTOMATIC rollback does not say the update was "+
			"unattended:\n\t%s\n\n"+
			"The operator has not been watching. That is the difference "+
			"between this and a rollback they started themselves.", automatic)
	}

	manual := strings.ToLower(sent[1].Title + " " + sent[1].Body)
	if strings.Contains(manual, "unattended") {
		t.Errorf("an operator's own failed rollback is described as unattended:\n\t%s", manual)
	}
}

// ------------------------------------------------------------ dedup keys --

func TestEveryLifecycleNotificationKeysOnADurableIdentity(t *testing.T) {
	t.Parallel()

	// Deduplication must be derived from the record the outcome belongs to, so
	// that the same durable event processed twice -- by a retry, by a restart,
	// by two passes reading the same row -- is one logical notification.
	//
	// Never from time, and never from the message text: the first makes every
	// occurrence unique, the second makes two different containers with the
	// same problem look like one.
	notifier := &recordingNotifier{}

	service.NotifyExecutionSucceeded(notifier, "web", "nginx:1.27.1", "exec_A")
	service.NotifyExecutionFailed(notifier, "web", "exec_B", "no.", false, true)
	service.NotifyUpdateRecovered(notifier, "web", "a", "b", "rb_C", "exec_D")
	service.NotifyRollbackSucceeded(notifier, "web", "rb_E")
	service.NotifyRollbackFailed(notifier, "web", "rb_F", "no.", false)
	service.NotifyApprovalRequired(notifier, "web", "plan_G", "a major change.")

	wantIdentity := []string{"exec_A", "exec_B", "rb_C", "rb_E", "rb_F", "plan_G"}
	sent := notifier.all()
	if len(sent) != len(wantIdentity) {
		t.Fatalf("%d notifications, want %d", len(sent), len(wantIdentity))
	}

	seen := map[string]bool{}
	for i, notification := range sent {
		key := notification.DedupKey
		if key == "" {
			t.Errorf("%q has no dedup key, so every occurrence is a new message",
				notification.Event)
			continue
		}
		if !strings.Contains(key, wantIdentity[i]) {
			t.Errorf("%q keys on %q, which does not name the record %q it reports.\n\n"+
				"A key that is not a lifecycle identity cannot survive a "+
				"restart, and cannot make a retry the same logical message.",
				notification.Event, key, wantIdentity[i])
		}
		if seen[key] {
			t.Errorf("two different outcomes share the dedup key %q; one would "+
				"swallow the other", key)
		}
		seen[key] = true
	}
}

func TestARecoveredUpdateAndAPlainRollbackDoNotShareAKey(t *testing.T) {
	t.Parallel()

	// Both report the same rollback record. If they shared a key, a rule with a
	// cooldown would deliver whichever arrived first and silently drop the
	// other -- and the one that matters is the recovered one.
	notifier := &recordingNotifier{}
	service.NotifyUpdateRecovered(notifier, "web", "a", "b", "rb_1", "exec_1")
	service.NotifyRollbackSucceeded(notifier, "web", "rb_1")

	sent := notifier.all()
	if sent[0].DedupKey == sent[1].DedupKey {
		t.Errorf("both notifications key on %q", sent[0].DedupKey)
	}
}

// ----------------------------------------- the level-triggered cooldown --

func TestLevelTriggeredEventsCarryACooldownFloor(t *testing.T) {
	t.Parallel()

	// The two events a scheduler pass re-derives from unchanged state. Without
	// a floor, the default rule cooldown of zero turns one standing condition
	// into a message every pass -- ninety-six a day at the default interval --
	// and an operator who gets ninety-six messages turns the channel off, which
	// loses the failed rollback too.
	for _, event := range []domain.NotificationEvent{
		domain.EventApprovalRequired,
		domain.EventSchedulerError,
	} {
		if event.MinimumCooldown() <= 0 {
			t.Errorf("%q has no cooldown floor, so a rule with the default "+
				"cooldown of zero notifies on every scheduler pass", event)
		}
		if got := event.EffectiveCooldown(0); got != event.MinimumCooldown() {
			t.Errorf("%q with a zero rule cooldown resolves to %s, want the "+
				"floor %s", event, got, event.MinimumCooldown())
		}
	}
}

func TestEdgeTriggeredEventsAreNotThrottled(t *testing.T) {
	t.Parallel()

	// The floor exists for events re-derived from unchanged state. Everything
	// else is raised at the moment something CHANGED, by a caller that already
	// established it had not been seen before -- and throttling those would
	// silently drop a second real failure.
	for _, event := range []domain.NotificationEvent{
		domain.EventExecutionSucceeded, domain.EventExecutionFailed,
		domain.EventUpdateRecovered,
		domain.EventRollbackStarted, domain.EventRollbackSucceeded,
		domain.EventRollbackFailed,
		domain.EventUpdateDiscovered, domain.EventAcquisitionSucceeded,
		domain.EventAcquisitionFailed, domain.EventAutomationPaused,
		domain.EventDriftDetected, domain.EventPolicyViolation,
		domain.EventRegistryUnavailable, domain.EventRebindFailed,
		domain.EventBackupFailed, domain.EventIntegrityFailed, domain.EventTest,
	} {
		if got := event.MinimumCooldown(); got != 0 {
			t.Errorf("%q carries a floor of %s.\n\n"+
				"This event is raised when something changed. A floor would "+
				"drop the SECOND real occurrence -- a different container "+
				"failing, or the same one failing again after somebody thought "+
				"they had fixed it.", event, got)
		}
	}
}

func TestARuleCooldownLongerThanTheFloorStillWins(t *testing.T) {
	t.Parallel()

	// A floor, never a cap. Nothing here may make an event noisier than an
	// operator asked for.
	day := 24 * time.Hour
	if got := domain.EventApprovalRequired.EffectiveCooldown(day); got != day {
		t.Errorf("a one-day rule cooldown resolved to %s; the floor overrode an "+
			"operator's own setting", got)
	}
	if got := domain.EventExecutionFailed.EffectiveCooldown(time.Minute); got != time.Minute {
		t.Errorf("an edge-triggered event with a rule cooldown resolved to %s, "+
			"want the rule's own minute", got)
	}
}

// ------------------------------------------------------------- redaction --

func TestNoLifecycleNotificationCarriesSensitiveMaterial(t *testing.T) {
	t.Parallel()

	// The payload is assembled from a closed set of fields, so this walks the
	// whole notification for anything that could have come from a daemon, a
	// registry, or an environment. The values below are what a leak would look
	// like if one of these helpers were ever handed a raw error or a captured
	// configuration.
	const (
		secretEnv   = "POSTGRES_PASSWORD=hunter2"
		secretToken = "ghp_0123456789abcdefghijklmnopqrstuvwxyz"
		socketPath  = "/var/run/docker.sock"
	)

	notifier := &recordingNotifier{}
	// Every helper gets values a caller legitimately holds. None of them is a
	// secret, and none of the helpers has a parameter one could arrive through.
	service.NotifyExecutionSucceeded(notifier, "web", "nginx:1.27.1", "exec_1")
	service.NotifyExecutionFailed(notifier, "web", "exec_2",
		"the recreation did not succeed (imageMismatch).", true, true)
	service.NotifyUpdateRecovered(notifier, "web",
		"nginx:1.27.1", "nginx:1.27.0", "rb_1", "exec_2")
	service.NotifyRollbackStarted(notifier, "web", "rb_2")
	service.NotifyRollbackSucceeded(notifier, "web", "rb_2")
	service.NotifyRollbackFailed(notifier, "web", "rb_3",
		"the rollback did not succeed (verify).", true)
	service.NotifyApprovalRequired(notifier, "web", "plan_1", "a major version change.")

	for _, notification := range notifier.all() {
		walk := notification.Title + "\n" + notification.Body
		for _, f := range notification.Fields {
			walk += "\n" + f.Label + "=" + f.Value
		}
		walk += "\n" + notification.DedupKey + "\n" + notification.ContainerName

		for _, forbidden := range []string{
			secretEnv, secretToken, socketPath, "PASSWORD", "hunter2",
			"Bearer ", "Authorization",
		} {
			if strings.Contains(walk, forbidden) {
				t.Errorf("%q carries %q:\n%s", notification.Event, forbidden, walk)
			}
		}
	}
}
