package service

import (
	"log/slog"
	"strconv"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Every notification HarborMaster raises is written here.
//
// # Why they are all in one file
//
// Because the security property is a property of the SENTENCES, not of the
// senders. The rule is that a notification carries nothing but HarborMaster's
// own words and HarborMaster's own identifiers — no environment value, no
// registry credential, no session data, no raw Docker error. That is easy to
// check when every sentence is in one place and impossible to check when a
// dozen services each compose their own.
//
// So the services do not build notifications. They call a function here with
// identifiers they already hold, and this file decides what is said.
//
// # What may appear in one
//
//   - A container NAME, which is the operator's own label for their own thing.
//   - An image REFERENCE, which they typed into their own compose file.
//   - An identifier HarborMaster generated: a plan, an acquisition, an
//     execution, a rollback.
//   - A fixed phrase from a closed vocabulary: a refusal reason, a delivery
//     result, a recommendation.
//   - A count.
//
// # What may not
//
// Anything else. In particular, never `err.Error()`. A Docker error carries
// paths, mounts, and occasionally an environment value; a registry error
// carries the URL, which for a private registry carries the host and sometimes
// the credential. Where a failure needs describing, it is described by the
// closed vocabulary the pipeline already produced for it.

// Notifier is the notification capability a service holds.
//
// Deliberately one method, and deliberately with no error return. A service in
// the middle of a container recreation cannot wait for a webhook and has nothing
// useful to do with a delivery failure — see the notification engine's header
// for why that shapes the whole subsystem.
//
// Every holder's field is optional. A deployment with notifications switched off
// wires nothing, and `raise` below does nothing.
type Notifier interface {
	Raise(notification domain.Notification)
}

// raise sends a notification when a notifier is wired.
//
// Nil-safe, because that is the normal case: notifications are off by default
// and every service must work identically without one.
//
// # Why a recover, in a codebase that otherwise lets panics travel
//
// Because this is the ONE call a container pipeline makes whose result it is
// forbidden to care about. The Notifier interface has no error return
// precisely so a service in the middle of a recreation cannot wait on a
// webhook and cannot branch on one failing. That promise is only worth
// anything if the call cannot take the pipeline with it.
//
// Without this, a defect anywhere in the notification path -- the engine, a
// channel formatter, a future consumer -- would unwind through the caller and
// abandon a rollback between stopping one container and starting another. The
// message is the least valuable thing on that stack; losing it is the correct
// trade, and it is the trade the rest of the subsystem already makes when a
// queue is full.
//
// It is deliberately NOT a general error-swallowing habit. This is the only
// recover in the service package, it covers one call, and the panic is logged
// at error rather than discarded -- a notification path that panics is a bug,
// and this makes it a visible bug instead of an outage.
func raise(notifier Notifier, notification domain.Notification) {
	if notifier == nil {
		return
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			// The event NAME only. A panic value can carry anything, including
			// whatever the notification was carrying, and this log line is
			// subject to the same rule as the notification itself.
			slog.Default().Error("a notification could not be raised; "+
				"the operation it describes was not affected",
				slog.String("event", string(notification.Event)))
		}
	}()

	notifier.Raise(notification)
}

// field is a bounded label/value pair.
func field(label, value string) domain.NotificationField {
	return domain.NotificationField{Label: label, Value: value}
}

// ---------------------------------------------------------------- updates --

// NotifyUpdateDiscovered reports that a newer image exists for a container.
//
// Deduplicated on the container and the version it was told about, so a
// scheduler running every fifteen minutes does not say the same thing ninety-six
// times a day.
func NotifyUpdateDiscovered(
	notifier Notifier,
	containerName, currentVersion, availableVersion string,
	classification domain.UpdateType,
) {
	raise(notifier, domain.Notification{
		Event:         domain.EventUpdateDiscovered,
		Severity:      domain.NotifyInfo,
		Title:         "An update is available for " + containerName,
		Body:          "HarborMaster found a newer image in the registry.",
		ContainerName: containerName,
		Fields: []domain.NotificationField{
			field("Running", currentVersion),
			field("Available", availableVersion),
			field("Change", string(classification)),
		},
		DedupKey: "update:" + containerName + ":" + availableVersion,
	})
}

// NotifyApprovalRequired reports an update that automation will not perform
// unattended.
func NotifyApprovalRequired(
	notifier Notifier,
	containerName, planID, reason string,
) {
	raise(notifier, domain.Notification{
		Event:         domain.EventApprovalRequired,
		Severity:      domain.NotifyWarning,
		Title:         containerName + " needs approval before it can be updated",
		Body:          "Automation will not perform this update unattended. " + reason,
		ContainerName: containerName,
		Fields:        []domain.NotificationField{field("Plan", planID)},
		DedupKey:      "approval:" + planID,
	})
}

// ----------------------------------------------------------- acquisitions --

// NotifyAcquisitionSucceeded reports a pulled image.
func NotifyAcquisitionSucceeded(notifier Notifier, containerName, imageRef, acquisitionID string) {
	raise(notifier, domain.Notification{
		Event:         domain.EventAcquisitionSucceeded,
		Severity:      domain.NotifyInfo,
		Title:         "Image pulled for " + containerName,
		Body:          "The image was pulled and its digest and platform were verified.",
		ContainerName: containerName,
		Fields: []domain.NotificationField{
			field("Image", imageRef),
			field("Acquisition", acquisitionID),
		},
		DedupKey: "acquisition:" + acquisitionID,
	})
}

// NotifyAcquisitionFailed reports a pull that did not happen.
//
// The reason is the pipeline's own closed vocabulary, never the registry's error
// text: a registry error carries the URL, and for a private registry that
// carries the host and sometimes the credential.
func NotifyAcquisitionFailed(
	notifier Notifier,
	containerName, imageRef, acquisitionID, reason string,
) {
	raise(notifier, domain.Notification{
		Event:         domain.EventAcquisitionFailed,
		Severity:      domain.NotifyWarning,
		Title:         "Could not pull the image for " + containerName,
		Body:          reason,
		ContainerName: containerName,
		Fields: []domain.NotificationField{
			field("Image", imageRef),
			field("Acquisition", acquisitionID),
		},
		DedupKey: "acquisition:" + acquisitionID,
	})
}

// ------------------------------------------------------------- executions --

// NotifyExecutionSucceeded reports a container recreated onto a new image.
func NotifyExecutionSucceeded(notifier Notifier, containerName, imageRef, executionID string) {
	raise(notifier, domain.Notification{
		Event:         domain.EventExecutionSucceeded,
		Severity:      domain.NotifyInfo,
		Title:         containerName + " was updated",
		Body:          "The container was recreated on the new image and passed verification.",
		ContainerName: containerName,
		Fields: []domain.NotificationField{
			field("Image", imageRef),
			field("Execution", executionID),
		},
		DedupKey: "execution:" + executionID,
	})
}

// NotifyExecutionFailed reports a recreation that did not complete.
//
// `hostChanged` is the fact that decides whether somebody has to act now. A
// recreation that failed before touching the host left a running container
// alone; one that failed after did not, and that is a different message.
func NotifyExecutionFailed(
	notifier Notifier,
	containerName, executionID, reason string,
	hostChanged, automatic bool,
) {
	body := reason
	if hostChanged {
		body += " This container was left changed and needs attention."
	}
	// What happens NEXT, which is the question an operator reading this at two
	// in the morning actually has. The two answers are different products, not
	// different phrasings: HarborMaster never undoes an update a person asked
	// for, and saying nothing here has let a manual failure read as though
	// something were coming to fix it.
	//
	// Deliberately hedged for the automatic case. Whether a rollback is
	// permitted is the governing policy's business and is decided after this
	// message leaves, so this promises an attempt at most. The recovered or
	// rollback-failed message that follows is the one that states the outcome.
	if automatic {
		body += " HarborMaster will attempt to roll it back if the policy allows."
	} else {
		body += " HarborMaster does not roll back an update you asked for; " +
			"roll it back from the update page if you want the previous image."
	}
	raise(notifier, domain.Notification{
		Event:         domain.EventExecutionFailed,
		Severity:      domain.NotifyCritical,
		Title:         containerName + " could not be updated",
		Body:          body,
		ContainerName: containerName,
		Fields:        []domain.NotificationField{field("Execution", executionID)},
		DedupKey:      "execution:" + executionID,
	})
}

// -------------------------------------------------------------- rollbacks --

// NotifyRollbackStarted reports a container being put back.
//
// Raised for a MANUAL rollback only. An automatic one is one stage of a
// sequence that ends in a message saying what became of the container, and
// "rolling back" between "could not be updated" and "was recovered" tells an
// operator nothing they are not about to be told properly.
func NotifyRollbackStarted(notifier Notifier, containerName, rollbackID string) {
	raise(notifier, domain.Notification{
		Event:         domain.EventRollbackStarted,
		Severity:      domain.NotifyWarning,
		Title:         "Rolling " + containerName + " back",
		Body:          "An operator started a rollback to the previous image.",
		ContainerName: containerName,
		Fields:        []domain.NotificationField{field("Rollback", rollbackID)},
		DedupKey:      "rollback:" + rollbackID + ":started",
	})
}

// NotifyRollbackSucceeded reports a container restored.
func NotifyRollbackSucceeded(notifier Notifier, containerName, rollbackID string) {
	raise(notifier, domain.Notification{
		Event:         domain.EventRollbackSucceeded,
		Severity:      domain.NotifyWarning,
		Title:         containerName + " was rolled back",
		Body:          "The container is running its previous image again and passed verification.",
		ContainerName: containerName,
		Fields:        []domain.NotificationField{field("Rollback", rollbackID)},
		DedupKey:      "rollback:" + rollbackID + ":succeeded",
	})
}

// NotifyUpdateRecovered reports the third update outcome: failed, then fixed.
//
// # Why this is not rollback.succeeded
//
// Because the subject is the UPDATE, not the rollback. An operator who rolled a
// container back by hand knows what they did and gets rollback.succeeded. An
// operator whose container was updated, broke, and was put back while they
// slept needs one sentence that says all three things -- and needs it not to
// say "succeeded", which is what a reader takes from a message about a rollback
// that worked.
//
// # Why it is not the last word on the update either
//
// It says the service is restored. It does not say everything is fine: the
// image somebody approved is not the one running, and automation pauses the
// container after a rollback, so nothing will try again until a person looks.
// The body says so, because an operator who reads "recovered" and stops
// reading has been told the container is up, which is true.
func NotifyUpdateRecovered(
	notifier Notifier,
	containerName, attemptedImage, restoredImage, rollbackID, executionID string,
) {
	raise(notifier, domain.Notification{
		Event:    domain.EventUpdateRecovered,
		Severity: domain.NotifyWarning,
		Title:    containerName + " failed to update and was restored automatically",
		// The images are in the BODY as well as in the fields, and that is
		// deliberate rather than redundant. The delivery row carries the title
		// and the body; it does not carry the fields, so a retry, a restart, or
		// the delivery history in the interface all show the body and only the
		// body. The two facts an operator needs at three in the morning -- what
		// broke and what is running now -- must be in the part that always
		// survives. See the note in the C4B report on the fields gap.
		Body: "The unattended update to " + attemptedImage + " did not pass " +
			"verification, so HarborMaster rolled the container back. It is " +
			"running " + restoredImage + " again and passed verification. " +
			"Automation is paused for this container until somebody releases it.",
		ContainerName: containerName,
		Fields: []domain.NotificationField{
			field("Attempted", attemptedImage),
			field("Running", restoredImage),
			field("Execution", executionID),
			field("Rollback", rollbackID),
		},
		// The ROLLBACK, which is the terminal record this reports. One logical
		// notification per rollback that reached a succeeded state, whatever a
		// restart or a retry does around it.
		DedupKey: "rollback:" + rollbackID + ":recovered",
	})
}

// NotifyRollbackFailed reports the worst outcome the pipeline has.
//
// Critical without exception. A rollback that fails is a container that is
// neither on its new image nor back on its old one, and it is the one event in
// HarborMaster that always needs a person.
func NotifyRollbackFailed(
	notifier Notifier,
	containerName, rollbackID, reason string,
	automatic bool,
) {
	// Both need a person now. They are different sentences because they
	// describe different situations: one operator is watching a rollback they
	// started, the other has not been told anything yet except that an
	// unattended update failed.
	title := containerName + " could not be rolled back"
	lead := "The rollback did not complete. This container needs attention: "
	if automatic {
		title = containerName + " failed to update and could NOT be restored"
		lead = "The unattended update failed and the automatic rollback did " +
			"not complete either. This container needs attention now: "
	}
	raise(notifier, domain.Notification{
		Event:         domain.EventRollbackFailed,
		Severity:      domain.NotifyCritical,
		Title:         title,
		Body:          lead + reason,
		ContainerName: containerName,
		Fields:        []domain.NotificationField{field("Rollback", rollbackID)},
		DedupKey:      "rollback:" + rollbackID + ":failed",
	})
}

// ------------------------------------------------------------- automation --

// NotifyAutomationPaused reports a container automation has stopped acting on.
func NotifyAutomationPaused(notifier Notifier, containerName, reason string, failures int) {
	raise(notifier, domain.Notification{
		Event:         domain.EventAutomationPaused,
		Severity:      domain.NotifyWarning,
		Title:         "Automation is paused for " + containerName,
		Body:          reason + " Automation will not act on this container until it is resumed.",
		ContainerName: containerName,
		Fields: []domain.NotificationField{
			field("Consecutive failures", strconv.Itoa(failures)),
		},
		DedupKey: "paused:" + containerName,
	})
}

// NotifySchedulerError reports a pass that could not complete.
//
// The detail is HarborMaster's own summary of which stage failed, never the
// underlying error.
func NotifySchedulerError(notifier Notifier, stage, detail string) {
	raise(notifier, domain.Notification{
		Event:    domain.EventSchedulerError,
		Severity: domain.NotifyWarning,
		Title:    "An automation pass could not complete",
		Body:     detail,
		Fields:   []domain.NotificationField{field("Stage", stage)},
		DedupKey: "scheduler:" + stage,
	})
}

// ----------------------------------------------------------- dependencies --

// NotifyRebindFailed reports a container that could not be reattached.
//
// # Why this is the only dependency event
//
// It is the one dependency condition that can leave a workload broken with no
// other signal. A container sharing a replaced provider's namespace and not
// reattached is attached to a namespace that no longer exists: Docker reports
// nothing, the container keeps running, its network stops working, and
// HarborMaster does not retry.
//
// A dependency LOOP changes nothing about a running container and would be
// re-raised on every pass. A container WAITING on its dependency is the system
// working. A dependency BLOCK is HarborMaster declining, which is the safe
// direction and usually clears itself. None of those is worth waking somebody.
//
// # What the sentence carries
//
// Two container names and one HarborMaster-generated identifier. No image, no
// digest, no refusal text from a daemon, and no error string — the state is a
// value from a closed vocabulary and it is rendered as a fixed phrase.
//
// Deduplicated on the pair, so an operator watching a five-dependent provider
// fail gets five distinct messages rather than one repeated, and gets each of
// them once.
func NotifyRebindFailed(
	notifier Notifier,
	dependent, provider, operationID string,
) {
	raise(notifier, domain.Notification{
		Event:    domain.EventRebindFailed,
		Severity: domain.NotifyCritical,
		Title:    "HarborMaster could not reattach " + dependent,
		Body: dependent + " shares " + provider + "'s namespace and could not be " +
			"reattached to its replacement. It may have no working network until " +
			"somebody recreates it. HarborMaster does not retry a reattachment " +
			"by itself, and no image version was changed.",
		ContainerName: dependent,
		Fields: []domain.NotificationField{
			field("Shares the namespace of", provider),
			field("Operation", operationID),
		},
		DedupKey: "rebind:" + dependent + ":" + provider,
	})
}

// ------------------------------------------------------ drift and policy --

// NotifyDriftDetected reports a container that no longer matches its snapshot.
func NotifyDriftDetected(notifier Notifier, containerName string, changes int, severity string) {
	level := domain.NotifyInfo
	if severity == "high" || severity == "critical" {
		level = domain.NotifyWarning
	}
	raise(notifier, domain.Notification{
		Event:         domain.EventDriftDetected,
		Severity:      level,
		Title:         containerName + " has drifted from its recorded configuration",
		Body:          "HarborMaster found differences between this container and the snapshot it took.",
		ContainerName: containerName,
		Fields: []domain.NotificationField{
			field("Changes", strconv.Itoa(changes)),
			field("Severity", severity),
		},
		DedupKey: "drift:" + containerName,
	})
}

// NotifyPolicyViolation reports compliance rules a container newly fails.
//
// One message per container per pass, not one per rule. A container that fails
// six rules of one misconfiguration is one thing wrong, and six messages about
// it would be the fastest way to teach an operator to ignore the channel.
func NotifyPolicyViolation(
	notifier Notifier,
	containerName string,
	newViolations int,
	worstSeverity, exampleRule string,
) {
	level := domain.NotifyInfo
	if worstSeverity == "high" || worstSeverity == "critical" {
		level = domain.NotifyWarning
	}
	raise(notifier, domain.Notification{
		Event:         domain.EventPolicyViolation,
		Severity:      level,
		Title:         containerName + " does not meet its compliance policy",
		Body:          "HarborMaster found compliance rules this container no longer satisfies.",
		ContainerName: containerName,
		Fields: []domain.NotificationField{
			field("New violations", strconv.Itoa(newViolations)),
			field("Worst severity", worstSeverity),
			field("Example rule", exampleRule),
		},
		DedupKey: "violation:" + containerName + ":" + worstSeverity,
	})
}

// ------------------------------------------------------------ the platform --

// NotifyRegistryUnavailable reports that update discovery cannot see a registry.
//
// The HOST only, never the URL and never the error. A private registry's URL is
// a piece of infrastructure detail, and its error text has been known to carry
// the credential that failed.
func NotifyRegistryUnavailable(notifier Notifier, registryHost string, failures int) {
	raise(notifier, domain.Notification{
		Event:    domain.EventRegistryUnavailable,
		Severity: domain.NotifyWarning,
		Title:    "A registry cannot be reached",
		Body: "HarborMaster cannot check for updates against this registry. " +
			"Containers using it will keep running; they will not be told about new images.",
		Fields: []domain.NotificationField{
			field("Registry", registryHost),
			field("Consecutive failures", strconv.Itoa(failures)),
		},
		DedupKey: "registry:" + registryHost,
	})
}

// NotifyBackupFailed reports a database backup that did not complete.
//
// # Nothing in the server calls this today, and that is not an oversight
//
// Backup is a COMMAND, not a scheduled task — invariant 7, because it reports
// and writes host detail no HTTP surface may offer. It runs in a separate
// process invocation with no notification engine in it, so the running server
// never observes a backup outcome and cannot raise this.
//
// The event and this function exist because the moment HarborMaster grows a
// scheduled backup, the notification must not be the thing somebody forgets.
// Until then `backup.failed` is a selectable event that never fires, which is
// recorded as a beta limitation rather than hidden.
func NotifyBackupFailed(notifier Notifier, detail string) {
	raise(notifier, domain.Notification{
		Event:    domain.EventBackupFailed,
		Severity: domain.NotifyCritical,
		Title:    "A HarborMaster backup failed",
		Body: "The database backup did not complete. Without it there is nothing to " +
			"restore from: " + detail,
		DedupKey: "backup",
	})
}

// NotifyIntegrityFailed reports a database integrity check that did not pass.
func NotifyIntegrityFailed(notifier Notifier, detail string) {
	raise(notifier, domain.Notification{
		Event:    domain.EventIntegrityFailed,
		Severity: domain.NotifyCritical,
		Title:    "The HarborMaster database failed its integrity check",
		Body: "This is a sign of storage corruption and needs attention: " + detail +
			" See docs/engineering/reliability.md for the recovery runbook.",
		DedupKey: "integrity",
	})
}
