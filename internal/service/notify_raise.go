package service

import (
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
func raise(notifier Notifier, notification domain.Notification) {
	if notifier == nil {
		return
	}
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
	hostChanged bool,
) {
	body := reason
	if hostChanged {
		body += " This container was left changed and needs attention."
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
func NotifyRollbackStarted(notifier Notifier, containerName, rollbackID string, automatic bool) {
	trigger := "An operator"
	if automatic {
		trigger = "HarborMaster"
	}
	raise(notifier, domain.Notification{
		Event:         domain.EventRollbackStarted,
		Severity:      domain.NotifyWarning,
		Title:         "Rolling " + containerName + " back",
		Body:          trigger + " started a rollback to the previous image.",
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

// NotifyRollbackFailed reports the worst outcome the pipeline has.
//
// Critical without exception. A rollback that fails is a container that is
// neither on its new image nor back on its old one, and it is the one event in
// HarborMaster that always needs a person.
func NotifyRollbackFailed(notifier Notifier, containerName, rollbackID, reason string) {
	raise(notifier, domain.Notification{
		Event:    domain.EventRollbackFailed,
		Severity: domain.NotifyCritical,
		Title:    containerName + " could not be rolled back",
		Body: "The rollback did not complete. This container needs attention: " +
			reason,
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
