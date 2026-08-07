import { useCallback, useState } from "react";

import type {
  NotificationChannel,
  NotificationDestination,
  NotificationDestinationRequest,
} from "../api/notificationTypes";
import {
  NOTIFICATION_CHANNEL_DESCRIPTIONS,
  NOTIFICATION_CHANNEL_LABELS,
  NOTIFICATION_CHANNEL_ORDER,
} from "../api/notificationTypes";
import {
  useCreateNotificationDestination,
  useUpdateNotificationDestination,
} from "../hooks/useNotifications";

/**
 * The destination editor.
 *
 * # This is the only form in HarborMaster that accepts a credential
 *
 * A Slack, Discord, or Teams webhook URL is a bearer token in the shape of a
 * path. Three consequences shape this component:
 *
 *  1. **The URL field is never pre-filled on an edit.** The server does not
 *     return the stored value and this component does not ask for it. The
 *     placeholder says what leaving it blank does.
 *  2. **`autoComplete` is off and the field is a password input.** A browser
 *     that remembered a webhook URL would be a browser holding a credential
 *     for a channel, and a shoulder-surfer reading one off a screen is the
 *     realistic threat during a demo or a screen-share.
 *  3. **The channel cannot be changed after creation.** A stored credential of
 *     the wrong shape for what a destination now is would be a state every
 *     validation ran before it could exist. The server refuses it too.
 *
 * # Validation lives on the server
 *
 * This form does not re-implement the URL rules. It cannot: the decisive check
 * is on the RESOLVED ADDRESS at dial time, which a browser cannot perform. What
 * it does is say the rules plainly, so a refusal is expected rather than
 * surprising.
 */
export function NotificationDestinationEditor({
  destination,
  channels,
  onCancel,
  onSaved,
}: {
  destination: NotificationDestination | null;
  channels: NotificationChannel[];
  onCancel: () => void;
  onSaved: (warnings: string[]) => void;
}) {
  const editing = destination !== null;

  const [name, setName] = useState(destination?.name ?? "");
  const [description, setDescription] = useState(destination?.description ?? "");
  const [channel, setChannel] = useState<NotificationChannel>(
    destination?.channel ?? "slack",
  );
  const [enabled, setEnabled] = useState(destination?.enabled ?? true);
  const [titlePrefix, setTitlePrefix] = useState(destination?.titlePrefix ?? "");
  const [url, setUrl] = useState("");
  const [emailTo, setEmailTo] = useState((destination?.emailTo ?? []).join(", "));
  const [emailFrom, setEmailFrom] = useState(destination?.emailFrom ?? "");

  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState("");

  const create = useCreateNotificationDestination();
  const update = useUpdateNotificationDestination();

  // The channel list comes from the server, so the picker is built from the
  // same source of truth the sender uses rather than from a list kept in sync
  // by hand. Falls back to the local order when the status has not loaded.
  const options =
    channels.length > 0
      ? NOTIFICATION_CHANNEL_ORDER.filter((entry) => channels.includes(entry))
      : NOTIFICATION_CHANNEL_ORDER;

  const submit = useCallback(async () => {
    setBusy(true);
    setFailure("");
    try {
      const body: NotificationDestinationRequest = {
        name: name.trim(),
        description: description.trim(),
        enabled,
        titlePrefix: titlePrefix.trim(),
      };
      if (!editing) body.channel = channel;

      // Sent only when the field was filled in. An empty box on an EDIT means
      // "leave the stored credential alone", which is what lets somebody rename
      // a destination without re-typing a URL they may not have kept.
      if (url.trim() !== "") body.url = url.trim();

      if (channel === "email") {
        body.emailTo = emailTo
          .split(",")
          .map((entry) => entry.trim())
          .filter((entry) => entry !== "");
        body.emailFrom = emailFrom.trim();
      }

      const result = editing
        ? await update(destination.destinationId, body)
        : await create(body);

      // The URL is cleared from component state the moment it has been sent, so
      // it does not sit in memory behind a cancelled form.
      setUrl("");
      onSaved(result.warnings ?? []);
    } catch (error) {
      setFailure(
        error instanceof Error
          ? error.message
          : "The destination could not be saved.",
      );
    } finally {
      setBusy(false);
    }
  }, [
    channel,
    create,
    description,
    destination,
    editing,
    emailFrom,
    emailTo,
    enabled,
    name,
    onSaved,
    titlePrefix,
    update,
    url,
  ]);

  return (
    <form
      className="space-y-4 rounded-xl border border-border-subtle bg-surface-raised px-4 py-4"
      onSubmit={(event) => {
        event.preventDefault();
        void submit();
      }}
    >
      <h3 className="text-sm font-semibold">
        {editing ? `Edit ${destination.name}` : "New destination"}
      </h3>

      <Field id="destination-name" label="Name" hint="What you will recognise it by in a rule.">
        <input
          id="destination-name"
          type="text"
          required
          maxLength={120}
          value={name}
          onChange={(event) => setName(event.target.value)}
          className="w-full rounded-lg border border-border-subtle bg-surface px-2.5 py-1.5 text-sm"
        />
      </Field>

      <Field id="destination-description" label="Description" hint="Optional.">
        <input
          id="destination-description"
          type="text"
          maxLength={500}
          value={description}
          onChange={(event) => setDescription(event.target.value)}
          className="w-full rounded-lg border border-border-subtle bg-surface px-2.5 py-1.5 text-sm"
        />
      </Field>

      <Field
        id="destination-channel"
        label="Channel"
        hint={
          editing
            ? "A destination's channel cannot be changed. Create a new destination instead."
            : NOTIFICATION_CHANNEL_DESCRIPTIONS[channel]
        }
      >
        <select
          id="destination-channel"
          value={channel}
          disabled={editing}
          onChange={(event) =>
            setChannel(event.target.value as NotificationChannel)
          }
          className="w-full rounded-lg border border-border-subtle bg-surface px-2.5 py-1.5 text-sm disabled:opacity-60"
        >
          {options.map((entry) => (
            <option key={entry} value={entry}>
              {NOTIFICATION_CHANNEL_LABELS[entry]}
            </option>
          ))}
        </select>
      </Field>

      {channel === "email" ? (
        <>
          <Field
            id="destination-email-from"
            label="From"
            hint="The envelope sender. Your relay may require a specific address."
          >
            <input
              id="destination-email-from"
              type="email"
              value={emailFrom}
              onChange={(event) => setEmailFrom(event.target.value)}
              className="w-full rounded-lg border border-border-subtle bg-surface px-2.5 py-1.5 text-sm"
            />
          </Field>
          <Field
            id="destination-email-to"
            label="To"
            hint="Comma-separated. Every recipient receives every notification routed here."
          >
            <input
              id="destination-email-to"
              type="text"
              value={emailTo}
              onChange={(event) => setEmailTo(event.target.value)}
              className="w-full rounded-lg border border-border-subtle bg-surface px-2.5 py-1.5 text-sm"
            />
          </Field>
          <p className="rounded-lg border border-border-subtle bg-surface-sunken px-3 py-2 text-xs text-content-muted">
            The relay and its password come from this deployment&rsquo;s
            environment —{" "}
            <code className="font-mono">HARBORMASTER_SMTP_HOST</code> and{" "}
            <code className="font-mono">HARBORMASTER_SMTP_PASSWORD_FILE</code> —
            so a mail password is never stored in HarborMaster&rsquo;s database.
          </p>
        </>
      ) : (
        <Field
          id="destination-url"
          label="Webhook URL"
          hint={
            editing
              ? "Leave blank to keep the stored URL. Enter one to replace it."
              : "HTTPS only, and a hostname rather than an IP address. The path is a credential: it is stored once and never shown again."
          }
        >
          <input
            id="destination-url"
            type="password"
            autoComplete="off"
            spellCheck={false}
            required={!editing}
            maxLength={2048}
            value={url}
            placeholder={
              editing ? "unchanged" : "https://hooks.example.com/services/…"
            }
            onChange={(event) => setUrl(event.target.value)}
            className="w-full rounded-lg border border-border-subtle bg-surface px-2.5 py-1.5 font-mono text-sm"
          />
        </Field>
      )}

      <Field
        id="destination-title-prefix"
        label="Title prefix"
        hint="Optional. Prepended to every message, so several HarborMaster deployments in one channel are distinguishable."
      >
        <input
          id="destination-title-prefix"
          type="text"
          maxLength={60}
          value={titlePrefix}
          placeholder="[prod]"
          onChange={(event) => setTitlePrefix(event.target.value)}
          className="w-full rounded-lg border border-border-subtle bg-surface px-2.5 py-1.5 text-sm"
        />
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={enabled}
          onChange={(event) => setEnabled(event.target.checked)}
        />
        Enabled — a disabled destination receives nothing, whatever a rule says
      </label>

      {failure ? (
        <p role="alert" className="text-sm text-danger">
          {failure}
        </p>
      ) : null}

      <div className="flex flex-wrap gap-2">
        <button
          type="submit"
          disabled={busy}
          className="rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium disabled:opacity-50"
        >
          {busy ? "Saving…" : editing ? "Save changes" : "Create destination"}
        </button>
        <button
          type="button"
          disabled={busy}
          onClick={() => {
            setUrl("");
            onCancel();
          }}
          className="rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm"
        >
          Cancel
        </button>
      </div>
    </form>
  );
}

/**
 * One labelled control.
 *
 * The label is bound by `htmlFor` rather than by wrapping, and the HINT sits
 * outside the label. Wrapping would make the control's accessible name the
 * label AND the hint, so "Name" and "Webhook URL" would both match half the
 * form -- which is a real accessibility defect and not only a test problem.
 */
function Field({
  id,
  label,
  hint,
  children,
}: {
  id: string;
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="space-y-1">
      <label htmlFor={id} className="block text-sm font-medium">
        {label}
      </label>
      {children}
      {hint ? <p className="text-xs text-content-muted">{hint}</p> : null}
    </div>
  );
}
