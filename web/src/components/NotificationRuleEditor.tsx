import { useCallback, useState } from "react";

import type {
  NotificationDestination,
  NotificationEvent,
  NotificationRule,
  NotificationRuleRequest,
  NotificationSeverity,
} from "../api/notificationTypes";
import {
  NOTIFICATION_EVENT_GROUPS,
  NOTIFICATION_EVENT_LABELS,
  NOTIFICATION_SEVERITY_LABELS,
  NOTIFICATION_SEVERITY_ORDER,
} from "../api/notificationTypes";
import {
  useCreateNotificationRule,
  useUpdateNotificationRule,
} from "../hooks/useNotifications";

/**
 * The rule editor.
 *
 * # The two settings that decide whether this subsystem is useful or ignored
 *
 *  1. **The severity threshold.** Too low and an operator gets a message for
 *     every routine pull; within a week they mute the channel and the failed
 *     rollback goes unread. The default is "Warnings and worse" and the option
 *     labels say what each one actually admits.
 *  2. **The cooldown.** Without one, a container failing every fifteen minutes
 *     sends ninety-six messages a day about one problem. Zero is allowed —
 *     somebody may genuinely want every occurrence — but it is a choice the
 *     form makes visible rather than a default nobody noticed.
 *
 * # Empty events means EVERY event
 *
 * The opposite of an update policy's selector, where empty means nothing. The
 * form says so explicitly, because the two live in the same product and the
 * cost of guessing wrong here is an extra message rather than an unintended
 * container change.
 */
export function NotificationRuleEditor({
  rule,
  destinations,
  onCancel,
  onSaved,
}: {
  rule: NotificationRule | null;
  destinations: NotificationDestination[];
  onCancel: () => void;
  onSaved: (warnings: string[]) => void;
}) {
  const editing = rule !== null;

  const [name, setName] = useState(rule?.name ?? "");
  const [enabled, setEnabled] = useState(rule?.enabled ?? true);
  const [severity, setSeverity] = useState<NotificationSeverity>(
    rule?.minimumSeverity ?? "warning",
  );
  const [events, setEvents] = useState<NotificationEvent[]>(rule?.events ?? []);
  const [selected, setSelected] = useState<string[]>(rule?.destinations ?? []);
  const [cooldown, setCooldown] = useState(String(rule?.cooldownSeconds ?? 900));

  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState("");

  const create = useCreateNotificationRule();
  const update = useUpdateNotificationRule();

  const toggleEvent = useCallback((event: NotificationEvent) => {
    setEvents((current) =>
      current.includes(event)
        ? current.filter((entry) => entry !== event)
        : [...current, event],
    );
  }, []);

  const toggleDestination = useCallback((destinationId: string) => {
    setSelected((current) =>
      current.includes(destinationId)
        ? current.filter((entry) => entry !== destinationId)
        : [...current, destinationId],
    );
  }, []);

  const submit = useCallback(async () => {
    setBusy(true);
    setFailure("");
    try {
      const parsed = Number.parseInt(cooldown, 10);
      const body: NotificationRuleRequest = {
        name: name.trim(),
        enabled,
        minimumSeverity: severity,
        events,
        destinations: selected,
        cooldownSeconds: Number.isFinite(parsed) && parsed >= 0 ? parsed : 0,
      };

      const result = editing
        ? await update(rule.ruleId, body)
        : await create(body);
      onSaved(result.warnings ?? []);
    } catch (error) {
      setFailure(
        error instanceof Error ? error.message : "The rule could not be saved.",
      );
    } finally {
      setBusy(false);
    }
  }, [
    cooldown,
    create,
    editing,
    enabled,
    events,
    name,
    onSaved,
    rule,
    selected,
    severity,
    update,
  ]);

  const live = destinations.filter((entry) => !entry.archived);

  return (
    <form
      className="space-y-4 rounded-xl border border-border-subtle bg-surface-raised px-4 py-4"
      onSubmit={(event) => {
        event.preventDefault();
        void submit();
      }}
    >
      <h3 className="text-sm font-semibold">
        {editing ? `Edit ${rule.name}` : "New rule"}
      </h3>

      <label className="block space-y-1">
        <span className="text-sm font-medium">Name</span>
        <input
          type="text"
          required
          maxLength={120}
          value={name}
          onChange={(event) => setName(event.target.value)}
          className="w-full rounded-lg border border-border-subtle bg-surface px-2.5 py-1.5 text-sm"
        />
      </label>

      <fieldset className="space-y-2">
        <legend className="text-sm font-medium">Send at or above</legend>
        {NOTIFICATION_SEVERITY_ORDER.map((entry) => (
          <label key={entry} className="flex items-start gap-2 text-sm">
            <input
              type="radio"
              name="minimumSeverity"
              className="mt-1"
              checked={severity === entry}
              onChange={() => setSeverity(entry)}
            />
            <span>
              <span className="font-medium">
                {NOTIFICATION_SEVERITY_LABELS[entry]}
              </span>
              <span className="block text-xs text-content-muted">
                {severityHint(entry)}
              </span>
            </span>
          </label>
        ))}
      </fieldset>

      <fieldset className="space-y-3">
        <legend className="text-sm font-medium">Events</legend>
        <p className="text-xs text-content-muted">
          {events.length === 0
            ? "Nothing selected, which means EVERY event that meets the severity threshold above."
            : `${events.length} selected. Only these are sent.`}
        </p>
        {NOTIFICATION_EVENT_GROUPS.map((group) => (
          <div key={group.label} className="space-y-1">
            <p className="text-xs font-medium uppercase tracking-wide text-content-muted">
              {group.label}
            </p>
            <p className="text-xs text-content-muted">{group.hint}</p>
            <div className="grid gap-1 sm:grid-cols-2">
              {group.events.map((event) => (
                <label key={event} className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={events.includes(event)}
                    onChange={() => toggleEvent(event)}
                  />
                  {NOTIFICATION_EVENT_LABELS[event] ?? event}
                </label>
              ))}
            </div>
          </div>
        ))}
      </fieldset>

      <fieldset className="space-y-1">
        <legend className="text-sm font-medium">Send to</legend>
        {live.length === 0 ? (
          <p className="text-xs text-content-muted">
            There are no destinations. A rule needs somewhere to route to.
          </p>
        ) : (
          live.map((destination) => (
            <label
              key={destination.destinationId}
              className="flex items-center gap-2 text-sm"
            >
              <input
                type="checkbox"
                checked={selected.includes(destination.destinationId)}
                onChange={() => toggleDestination(destination.destinationId)}
              />
              {destination.name}
              {!destination.enabled ? (
                <span className="text-xs text-content-muted">(disabled)</span>
              ) : null}
            </label>
          ))
        )}
      </fieldset>

      <label className="block space-y-1">
        <span className="text-sm font-medium">Cooldown</span>
        <input
          type="number"
          min={0}
          max={86400}
          value={cooldown}
          onChange={(event) => setCooldown(event.target.value)}
          className="w-32 rounded-lg border border-border-subtle bg-surface px-2.5 py-1.5 text-sm"
        />
        <span className="block text-xs text-content-muted">
          Seconds. A repeat of the same underlying thing inside this window is
          suppressed and recorded rather than sent. Zero sends every occurrence,
          which for a container failing every fifteen minutes is ninety-six
          messages a day about one problem.
        </span>
      </label>

      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={enabled}
          onChange={(event) => setEnabled(event.target.checked)}
        />
        Enabled — a disabled rule routes nothing
      </label>

      {failure ? (
        <p role="alert" className="text-sm text-danger">
          {failure}
        </p>
      ) : null}

      <div className="flex flex-wrap gap-2">
        <button
          type="submit"
          disabled={busy || selected.length === 0}
          className="rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium disabled:opacity-50"
        >
          {busy ? "Saving…" : editing ? "Save changes" : "Create rule"}
        </button>
        <button
          type="button"
          disabled={busy}
          onClick={onCancel}
          className="rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm"
        >
          Cancel
        </button>
      </div>
    </form>
  );
}

/** What each threshold actually admits, in the words that matter. */
function severityHint(severity: NotificationSeverity): string {
  switch (severity) {
    case "info":
      return "Including routine progress: every update discovered, every image pulled, every container recreated.";
    case "warning":
      return "Failures, refusals, pauses, and drift. The set most deployments want.";
    case "critical":
      return "Only a failed recreation, a failed rollback, a failed backup, or a failed integrity check.";
    default:
      return "";
  }
}
