import type {
  DeliveryResult,
  NotificationChannel,
  NotificationDestination,
  NotificationSeverity,
} from "../api/notificationTypes";
import {
  DELIVERY_RESULT_DESCRIPTIONS,
  DELIVERY_RESULT_LABELS,
  DELIVERY_RESULT_TONE,
  NOTIFICATION_CHANNEL_DESCRIPTIONS,
  NOTIFICATION_CHANNEL_LABELS,
  NOTIFICATION_EVENT_LABELS,
  NOTIFICATION_SEVERITY_LABELS,
} from "../api/notificationTypes";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

/**
 * The notification vocabulary, rendered.
 *
 * # The two words nobody guesses correctly
 *
 * `suppressed` and `dropped` sit either side of "nothing arrived", and they
 * lead in opposite directions. Suppressed is HarborMaster working exactly as
 * configured; dropped is HarborMaster LOSING something. So suppressed is
 * neutral and dropped is a danger tone, and both carry the sentence that says
 * which is which.
 *
 * Nothing here is colour-only. Every badge's text says what the colour says.
 */

/** What happened to one delivery. */
export function DeliveryResultBadge({ result }: { result: DeliveryResult }) {
  const tones: Record<"ok" | "warn" | "bad" | "muted", BadgeTone> = {
    ok: "ok",
    warn: "warn",
    bad: "danger",
    muted: "neutral",
  };
  const tone = tones[DELIVERY_RESULT_TONE[result] ?? "muted"];

  return (
    <StatusBadge
      tone={tone}
      label={DELIVERY_RESULT_LABELS[result] ?? result}
      title={DELIVERY_RESULT_DESCRIPTIONS[result] ?? result}
    />
  );
}

/** Where a destination sends. Always neutral: a channel is not a risk level. */
export function NotificationChannelBadge({
  channel,
}: {
  channel: NotificationChannel;
}) {
  return (
    <StatusBadge
      tone="neutral"
      label={NOTIFICATION_CHANNEL_LABELS[channel] ?? channel}
      title={NOTIFICATION_CHANNEL_DESCRIPTIONS[channel] ?? channel}
    />
  );
}

/**
 * How much a notification matters.
 *
 * Critical is a danger tone because a failed rollback is the one event in
 * HarborMaster that always needs a person.
 */
export function NotificationSeverityBadge({
  severity,
}: {
  severity: NotificationSeverity;
}) {
  const tones: Record<NotificationSeverity, BadgeTone> = {
    info: "neutral",
    warning: "warn",
    critical: "danger",
  };
  const labels: Record<NotificationSeverity, string> = {
    info: "Info",
    warning: "Warning",
    critical: "Critical",
  };

  return <StatusBadge tone={tones[severity]} label={labels[severity] ?? severity} />;
}

/** A notification event, in the words an operator recognises. */
export function notificationEventLabel(event: string): string {
  return NOTIFICATION_EVENT_LABELS[event] ?? event;
}

/** A rule's severity threshold, in words. */
export function severityThresholdLabel(severity: NotificationSeverity): string {
  return NOTIFICATION_SEVERITY_LABELS[severity] ?? severity;
}

/**
 * Says plainly that nothing is being delivered.
 *
 * # Why this exists
 *
 * Destinations and rules stay EDITABLE when delivery is switched off, which is
 * the right behaviour — somebody configures and reviews before turning sending
 * on — and the easiest thing in this subsystem to misread. A page that showed a
 * carefully-built set of rules and an empty history, with nothing saying why,
 * would let somebody believe they were being alerted when they were not.
 */
export function NotificationsDisabledNotice({ enabled }: { enabled: boolean }) {
  if (enabled) return null;

  return (
    <div
      role="status"
      className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-sm text-warn"
    >
      <p className="font-medium">Nothing is being delivered.</p>
      <p className="mt-1">
        Notifications are switched off in this deployment. Destinations and
        rules can still be created and reviewed, and past deliveries are still
        listed, but nothing new will be sent. Set{" "}
        <code className="font-mono text-xs">
          HARBORMASTER_NOTIFICATIONS_ENABLED=true
        </code>{" "}
        and restart to turn delivery on.
      </p>
    </div>
  );
}

/**
 * Says that a destination is not working.
 *
 * A revoked webhook URL breaks notifications SILENTLY, which is the worst
 * possible failure for a subsystem whose whole job is to tell you things. This
 * is the one place that failure becomes visible without reading the history.
 */
export function DestinationHealth({
  destination,
}: {
  destination: NotificationDestination;
}) {
  if (destination.consecutiveFailures === 0) {
    if (!destination.lastAttemptAt) {
      return (
        <span className="text-xs text-content-muted">
          Nothing has been sent here yet. Send a test to prove it works.
        </span>
      );
    }
    return null;
  }

  const attempts =
    destination.consecutiveFailures === 1
      ? "The last delivery to this destination failed."
      : `The last ${destination.consecutiveFailures} deliveries to this destination failed.`;

  return (
    <p
      role="status"
      className="rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-xs text-danger"
    >
      <span className="font-medium">{attempts}</span>{" "}
      {destination.lastError
        ? destination.lastError
        : "HarborMaster has no further detail."}{" "}
      Nothing routed here is reaching anybody.
    </p>
  );
}

/**
 * Renders a cooldown in words.
 *
 * Zero is stated explicitly rather than shown as "0s": "every occurrence" is
 * what the setting MEANS, and it is a choice somebody should see they made.
 */
export function describeCooldown(seconds: number): string {
  if (seconds <= 0) return "every occurrence is sent";
  if (seconds < 60) return `at most one every ${seconds}s`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) {
    return `at most one every ${minutes} minute${minutes === 1 ? "" : "s"}`;
  }
  const hours = Math.round(minutes / 60);
  return `at most one every ${hours} hour${hours === 1 ? "" : "s"}`;
}

/** Renders a rule's event selection in words. */
export function describeEvents(events: string[] | undefined): string {
  if (!events || events.length === 0) return "every event";
  if (events.length <= 3) {
    return events.map(notificationEventLabel).join(", ");
  }
  return `${events.length} selected events`;
}
