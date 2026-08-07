import { useCallback, useState } from "react";

import type {
  DeliveryResult,
  NotificationDestination,
  NotificationRule,
} from "../api/notificationTypes";
import { DELIVERY_RESULT_DESCRIPTIONS } from "../api/notificationTypes";
import { NotificationDestinationEditor } from "../components/NotificationDestinationEditor";
import { NotificationRuleEditor } from "../components/NotificationRuleEditor";
import {
  DeliveryResultBadge,
  DestinationHealth,
  NotificationChannelBadge,
  NotificationSeverityBadge,
  NotificationsDisabledNotice,
  describeCooldown,
  describeEvents,
  notificationEventLabel,
  severityThresholdLabel,
} from "../components/NotificationBadges";
import { PageIntro } from "../components/PageIntro";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import {
  useArchiveNotificationDestination,
  useArchiveNotificationRule,
  useNotificationDeliveries,
  useNotificationDestinations,
  useNotificationRules,
  useNotificationStatus,
  useTestNotificationDestination,
} from "../hooks/useNotifications";
import { useSession } from "../hooks/useSession";

/**
 * Notifications: destinations, the rules that route to them, and what was sent.
 *
 * # The three things this page must make unmissable
 *
 *  1. **Whether anything is actually being delivered.** Configuration stays
 *     editable when sending is off, which is correct and is the easiest thing
 *     here to misread. The notice at the top says so before anything else.
 *  2. **That a destination is broken.** A revoked webhook URL breaks
 *     notifications silently — the worst failure mode for a subsystem whose
 *     whole job is to tell you things. Failing destinations say so on their own
 *     card, not only in the history.
 *  3. **That `suppressed` is not `failed`.** One is a cooldown working; the
 *     other is a message nobody got. They are different words, different
 *     colours, and each carries the sentence that explains it.
 *
 * # What the page never shows
 *
 * A webhook URL or an SMTP password. Neither is in any response, so there is
 * nothing here to redact — the destination card renders `endpoint`, which is a
 * scheme and a host.
 */
export function Notifications() {
  const [editingDestination, setEditingDestination] =
    useState<NotificationDestination | null>(null);
  const [creatingDestination, setCreatingDestination] = useState(false);
  const [editingRule, setEditingRule] = useState<NotificationRule | null>(null);
  const [creatingRule, setCreatingRule] = useState(false);
  const [warnings, setWarnings] = useState<string[]>([]);
  const [notice, setNotice] = useState("");
  const [failedOnly, setFailedOnly] = useState(false);

  const status = useNotificationStatus();
  const destinations = useNotificationDestinations({ page: 1, pageSize: 50 });
  const rules = useNotificationRules({ page: 1, pageSize: 50 });
  const deliveries = useNotificationDeliveries(
    { page: 1, pageSize: 25, ...(failedOnly ? { failed: true } : {}) },
    // Polled only while something is in flight. A settled deployment sends
    // nothing for hours, so a page left open costs nothing.
    { poll: (status.data?.pending ?? 0) > 0 },
  );

  const session = useSession();
  const mayManage = Boolean(
    session.user?.permissions.includes("notification:manage"),
  );

  const refresh = useCallback(() => {
    status.refresh();
    destinations.refresh();
    rules.refresh();
    deliveries.refresh();
  }, [status, destinations, rules, deliveries]);

  return (
    <div className="space-y-6">
      <PageIntro
        title="Notifications"
        description={
          "Where HarborMaster tells you what happened, and the record of what " +
          "it sent. A notification never delays the thing it is about: a " +
          "rollback does not wait for a webhook."
        }
      />

      <NotificationsDisabledNotice enabled={Boolean(status.data?.enabled)} />

      {status.data && status.data.failing > 0 ? (
        <div
          role="status"
          className="rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger"
        >
          <span className="font-medium">
            {status.data.failing === 1
              ? "One destination is not working."
              : `${status.data.failing} destinations are not working.`}
          </span>{" "}
          Anything routed there is reaching nobody.
        </div>
      ) : null}

      {warnings.length > 0 ? (
        <div
          role="status"
          className="space-y-1 rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-sm text-warn"
        >
          <p className="font-medium">Saved. Worth knowing:</p>
          <ul className="list-disc space-y-1 pl-5">
            {warnings.map((warning) => (
              <li key={warning}>{warning}</li>
            ))}
          </ul>
        </div>
      ) : null}

      {notice ? (
        <p
          role="status"
          className="rounded-lg border border-border-subtle bg-surface-sunken px-3 py-2 text-sm text-content-muted"
        >
          {notice}
        </p>
      ) : null}

      {/* ---- destinations -------------------------------------------- */}

      <section className="space-y-3">
        <header className="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h2 className="text-base font-semibold">Destinations</h2>
            <p className="text-sm text-content-muted">
              Where messages go. A webhook URL is a credential: it is stored
              once and never shown again.
            </p>
          </div>
          {mayManage ? (
            <button
              type="button"
              className="rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
              onClick={() => {
                setCreatingDestination(true);
                setEditingDestination(null);
                setWarnings([]);
                setNotice("");
              }}
            >
              New destination
            </button>
          ) : null}
        </header>

        {creatingDestination || editingDestination ? (
          <NotificationDestinationEditor
            destination={editingDestination}
            channels={status.data?.channels ?? []}
            onCancel={() => {
              setCreatingDestination(false);
              setEditingDestination(null);
            }}
            onSaved={(saved) => {
              setCreatingDestination(false);
              setEditingDestination(null);
              setWarnings(saved);
              setNotice("");
              refresh();
            }}
          />
        ) : null}

        <DestinationList
          state={destinations}
          mayManage={mayManage}
          deliveryEnabled={Boolean(status.data?.enabled)}
          onEdit={(destination) => {
            setEditingDestination(destination);
            setCreatingDestination(false);
            setWarnings([]);
            setNotice("");
          }}
          onNotice={setNotice}
          onChanged={refresh}
        />
      </section>

      {/* ---- rules --------------------------------------------------- */}

      <section className="space-y-3">
        <header className="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h2 className="text-base font-semibold">Rules</h2>
            <p className="text-sm text-content-muted">
              What reaches which destination. A rule with no events selected
              matches every event.
            </p>
          </div>
          {mayManage ? (
            <button
              type="button"
              className="rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
              disabled={(destinations.data?.items.length ?? 0) === 0}
              onClick={() => {
                setCreatingRule(true);
                setEditingRule(null);
                setWarnings([]);
                setNotice("");
              }}
            >
              New rule
            </button>
          ) : null}
        </header>

        {creatingRule || editingRule ? (
          <NotificationRuleEditor
            rule={editingRule}
            destinations={destinations.data?.items ?? []}
            onCancel={() => {
              setCreatingRule(false);
              setEditingRule(null);
            }}
            onSaved={(saved) => {
              setCreatingRule(false);
              setEditingRule(null);
              setWarnings(saved);
              setNotice("");
              refresh();
            }}
          />
        ) : null}

        <RuleList
          state={rules}
          destinations={destinations.data?.items ?? []}
          mayManage={mayManage}
          hasDestinations={(destinations.data?.items.length ?? 0) > 0}
          onEdit={(rule) => {
            setEditingRule(rule);
            setCreatingRule(false);
            setWarnings([]);
            setNotice("");
          }}
          onChanged={refresh}
        />
      </section>

      {/* ---- history ------------------------------------------------- */}

      <section className="space-y-3">
        <header className="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h2 className="text-base font-semibold">Delivery history</h2>
            <p className="text-sm text-content-muted">
              What HarborMaster sent, and what happened to it.
            </p>
          </div>
          <label className="flex items-center gap-2 text-sm text-content-muted">
            <input
              type="checkbox"
              checked={failedOnly}
              onChange={(event) => setFailedOnly(event.target.checked)}
            />
            Only what was not delivered
          </label>
        </header>

        <DeliveryList state={deliveries} failedOnly={failedOnly} />
      </section>
    </div>
  );
}

// ---------------------------------------------------------- destinations --

function DestinationList({
  state,
  mayManage,
  deliveryEnabled,
  onEdit,
  onNotice,
  onChanged,
}: {
  state: ReturnType<typeof useNotificationDestinations>;
  mayManage: boolean;
  deliveryEnabled: boolean;
  onEdit: (destination: NotificationDestination) => void;
  onNotice: (message: string) => void;
  onChanged: () => void;
}) {
  if (state.status === "loading") {
    return <LoadingState label="Loading destinations" />;
  }
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }

  const items = state.data?.items ?? [];
  if (items.length === 0) {
    return (
      <EmptyState
        title="No destinations"
        description={
          mayManage
            ? "Nothing has anywhere to go. Add a destination, send a test to prove it works, then write a rule that routes to it."
            : "Nothing has anywhere to go. An administrator can add one."
        }
      />
    );
  }

  return (
    <div className="space-y-3">
      {items.map((destination) => (
        <DestinationCard
          key={destination.destinationId}
          destination={destination}
          mayManage={mayManage}
          deliveryEnabled={deliveryEnabled}
          onEdit={() => onEdit(destination)}
          onNotice={onNotice}
          onChanged={onChanged}
        />
      ))}
    </div>
  );
}

function DestinationCard({
  destination,
  mayManage,
  deliveryEnabled,
  onEdit,
  onNotice,
  onChanged,
}: {
  destination: NotificationDestination;
  mayManage: boolean;
  deliveryEnabled: boolean;
  onEdit: () => void;
  onNotice: (message: string) => void;
  onChanged: () => void;
}) {
  const archive = useArchiveNotificationDestination();
  const sendTest = useTestNotificationDestination();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState("");

  const withdraw = useCallback(async () => {
    setBusy(true);
    setFailure("");
    try {
      await archive(destination.destinationId);
      setConfirming(false);
      onChanged();
    } catch (error) {
      setFailure(
        error instanceof Error ? error.message : "The destination was not withdrawn.",
      );
    } finally {
      setBusy(false);
    }
  }, [archive, destination.destinationId, onChanged]);

  const test = useCallback(async () => {
    setBusy(true);
    setFailure("");
    try {
      const result = await sendTest(destination.destinationId);
      onNotice(result.detail);
      onChanged();
    } catch (error) {
      setFailure(
        error instanceof Error ? error.message : "The test could not be queued.",
      );
    } finally {
      setBusy(false);
    }
  }, [destination.destinationId, onChanged, onNotice, sendTest]);

  return (
    <article className="space-y-3 rounded-xl border border-border-subtle bg-surface-raised px-4 py-3">
      <header className="flex flex-wrap items-start justify-between gap-2">
        <div>
          <h3 className="text-sm font-semibold">
            {destination.name}
            {destination.archived ? (
              <span className="ml-2 text-xs font-normal text-content-muted">
                (withdrawn)
              </span>
            ) : null}
            {!destination.enabled && !destination.archived ? (
              <span className="ml-2 text-xs font-normal text-content-muted">
                (disabled)
              </span>
            ) : null}
          </h3>
          {destination.description ? (
            <p className="mt-1 max-w-prose text-sm text-content-muted">
              {destination.description}
            </p>
          ) : null}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <NotificationChannelBadge channel={destination.channel} />
          {destination.lastResult ? (
            <DeliveryResultBadge result={destination.lastResult} />
          ) : null}
        </div>
      </header>

      <dl className="grid gap-2 text-sm sm:grid-cols-2">
        <Detail
          label="Sends to"
          value={destination.endpoint || "—"}
          hint={
            destination.channel === "email"
              ? "The relay this deployment was configured with."
              : "The scheme and host only. The rest of the URL is a credential and is never shown."
          }
        />
        {destination.emailTo && destination.emailTo.length > 0 ? (
          <Detail label="Recipients" value={destination.emailTo.join(", ")} />
        ) : null}
        {destination.titlePrefix ? (
          <Detail label="Title prefix" value={destination.titlePrefix} />
        ) : null}
        <Detail
          label="Last attempt"
          value={
            destination.lastAttemptAt
              ? new Date(destination.lastAttemptAt).toLocaleString()
              : "never"
          }
        />
      </dl>

      <DestinationHealth destination={destination} />

      {failure ? (
        <p role="alert" className="text-xs text-danger">
          {failure}
        </p>
      ) : null}

      {mayManage && !destination.archived ? (
        <div className="flex flex-wrap gap-2">
          <button
            type="button"
            className="rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs font-medium"
            onClick={onEdit}
          >
            Edit
          </button>
          <button
            type="button"
            className="rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs font-medium disabled:opacity-50"
            disabled={busy || !destination.enabled || !deliveryEnabled}
            title={
              !deliveryEnabled
                ? "Delivery is switched off, so a test would never be sent."
                : !destination.enabled
                  ? "This destination is disabled."
                  : "Queues a test notification. Its outcome appears in the history below."
            }
            onClick={() => void test()}
          >
            {busy ? "Working…" : "Send test"}
          </button>
          {confirming ? (
            <>
              <button
                type="button"
                className="rounded-lg border border-danger/40 bg-danger-soft px-2.5 py-1 text-xs font-medium text-danger disabled:opacity-50"
                disabled={busy}
                onClick={() => void withdraw()}
              >
                {busy ? "Withdrawing…" : "Yes, withdraw it"}
              </button>
              <button
                type="button"
                className="rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs"
                disabled={busy}
                onClick={() => setConfirming(false)}
              >
                Cancel
              </button>
              <span className="self-center text-xs text-content-muted">
                Its stored credential is destroyed. Re-entering it later is the
                point.
              </span>
            </>
          ) : (
            <button
              type="button"
              className="rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs"
              onClick={() => setConfirming(true)}
            >
              Withdraw
            </button>
          )}
        </div>
      ) : null}
    </article>
  );
}

// ----------------------------------------------------------------- rules --

function RuleList({
  state,
  destinations,
  mayManage,
  hasDestinations,
  onEdit,
  onChanged,
}: {
  state: ReturnType<typeof useNotificationRules>;
  destinations: NotificationDestination[];
  mayManage: boolean;
  hasDestinations: boolean;
  onEdit: (rule: NotificationRule) => void;
  onChanged: () => void;
}) {
  if (state.status === "loading") return <LoadingState label="Loading rules" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }

  const items = state.data?.items ?? [];
  if (items.length === 0) {
    return (
      <EmptyState
        title="No rules"
        description={
          hasDestinations
            ? "Nothing is routed anywhere, so nothing will be sent. A first rule at 'Warnings and worse' tells you when something goes wrong without telling you about every routine update."
            : "Add a destination first. A rule needs somewhere to route to."
        }
      />
    );
  }

  return (
    <div className="space-y-3">
      {items.map((rule) => (
        <RuleCard
          key={rule.ruleId}
          rule={rule}
          destinations={destinations}
          mayManage={mayManage}
          onEdit={() => onEdit(rule)}
          onChanged={onChanged}
        />
      ))}
    </div>
  );
}

function RuleCard({
  rule,
  destinations,
  mayManage,
  onEdit,
  onChanged,
}: {
  rule: NotificationRule;
  destinations: NotificationDestination[];
  mayManage: boolean;
  onEdit: () => void;
  onChanged: () => void;
}) {
  const archive = useArchiveNotificationRule();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);

  const withdraw = useCallback(async () => {
    setBusy(true);
    try {
      await archive(rule.ruleId);
      setConfirming(false);
      onChanged();
    } finally {
      setBusy(false);
    }
  }, [archive, onChanged, rule.ruleId]);

  const names = rule.destinations.map((id) => {
    const match = destinations.find((entry) => entry.destinationId === id);
    return match ? match.name : "a withdrawn destination";
  });

  return (
    <article className="space-y-3 rounded-xl border border-border-subtle bg-surface-raised px-4 py-3">
      <header className="flex flex-wrap items-start justify-between gap-2">
        <h3 className="text-sm font-semibold">
          {rule.name}
          {rule.archived ? (
            <span className="ml-2 text-xs font-normal text-content-muted">
              (withdrawn)
            </span>
          ) : null}
          {!rule.enabled && !rule.archived ? (
            <span className="ml-2 text-xs font-normal text-content-muted">
              (disabled)
            </span>
          ) : null}
        </h3>
        <NotificationSeverityBadge severity={rule.minimumSeverity} />
      </header>

      <dl className="grid gap-2 text-sm sm:grid-cols-2">
        <Detail label="Matches" value={describeEvents(rule.events)} />
        <Detail
          label="At or above"
          value={severityThresholdLabel(rule.minimumSeverity)}
        />
        <Detail
          label="Sends to"
          value={names.length > 0 ? names.join(", ") : "nowhere"}
        />
        <Detail
          label="Repeats"
          value={describeCooldown(rule.cooldownSeconds)}
        />
      </dl>

      {mayManage && !rule.archived ? (
        <div className="flex flex-wrap gap-2">
          <button
            type="button"
            className="rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs font-medium"
            onClick={onEdit}
          >
            Edit
          </button>
          {confirming ? (
            <>
              <button
                type="button"
                className="rounded-lg border border-danger/40 bg-danger-soft px-2.5 py-1 text-xs font-medium text-danger disabled:opacity-50"
                disabled={busy}
                onClick={() => void withdraw()}
              >
                {busy ? "Withdrawing…" : "Yes, withdraw it"}
              </button>
              <button
                type="button"
                className="rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs"
                disabled={busy}
                onClick={() => setConfirming(false)}
              >
                Cancel
              </button>
            </>
          ) : (
            <button
              type="button"
              className="rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs"
              onClick={() => setConfirming(true)}
            >
              Withdraw
            </button>
          )}
        </div>
      ) : null}
    </article>
  );
}

// --------------------------------------------------------------- history --

function DeliveryList({
  state,
  failedOnly,
}: {
  state: ReturnType<typeof useNotificationDeliveries>;
  failedOnly: boolean;
}) {
  if (state.status === "loading") {
    return <LoadingState label="Loading delivery history" />;
  }
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }

  const items = state.data?.items ?? [];
  if (items.length === 0) {
    return (
      <EmptyState
        title={failedOnly ? "Nothing has failed" : "Nothing has been sent"}
        description={
          failedOnly
            ? "Every notification HarborMaster tried to send was accepted."
            : "No notification has been raised yet, or no rule routed one anywhere."
        }
      />
    );
  }

  return (
    <div className="overflow-x-auto rounded-xl border border-border-subtle">
      <table className="w-full min-w-[52rem] text-left text-sm">
        <thead className="bg-surface-sunken text-xs uppercase tracking-wide text-content-muted">
          <tr>
            <th scope="col" className="px-3 py-2">
              When
            </th>
            <th scope="col" className="px-3 py-2">
              What happened
            </th>
            <th scope="col" className="px-3 py-2">
              Destination
            </th>
            <th scope="col" className="px-3 py-2">
              Outcome
            </th>
            <th scope="col" className="px-3 py-2">
              Detail
            </th>
          </tr>
        </thead>
        <tbody>
          {items.map((delivery) => (
            <tr
              key={delivery.deliveryId}
              className="border-t border-border-subtle align-top"
            >
              <td className="whitespace-nowrap px-3 py-2 text-content-muted">
                {new Date(delivery.queuedAt).toLocaleString()}
              </td>
              <td className="px-3 py-2">
                <div className="font-medium">{delivery.title}</div>
                <div className="text-xs text-content-muted">
                  {notificationEventLabel(delivery.event)}
                  {delivery.containerName ? ` — ${delivery.containerName}` : ""}
                </div>
              </td>
              <td className="px-3 py-2">
                <div>{delivery.destinationName}</div>
                {delivery.ruleName ? (
                  <div className="text-xs text-content-muted">
                    via {delivery.ruleName}
                  </div>
                ) : null}
              </td>
              <td className="px-3 py-2">
                <DeliveryResultBadge result={delivery.result} />
                {delivery.attempts > 1 ? (
                  <div className="mt-1 text-xs text-content-muted">
                    {delivery.attempts} attempts
                  </div>
                ) : null}
              </td>
              <td className="px-3 py-2 text-xs text-content-muted">
                {deliveryDetail(delivery.result, delivery.error, delivery.statusCode)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/**
 * What to say about one delivery's outcome.
 *
 * HarborMaster's own sentence when there is one; the closed-vocabulary label
 * otherwise. Never a transport error, because none is stored.
 */
function deliveryDetail(
  result: DeliveryResult,
  error: string | undefined,
  statusCode: number | undefined,
): string {
  const parts: string[] = [];
  if (error) parts.push(error);
  if (statusCode && statusCode > 0) parts.push(`HTTP ${statusCode}`);
  if (parts.length > 0) return parts.join(" · ");

  // No stored detail. The DESCRIPTION rather than the label, so the column
  // says something the badge beside it did not -- "a cooldown decided not to
  // send a repeat" is the sentence that stops `suppressed` being read as a
  // failure.
  return DELIVERY_RESULT_DESCRIPTIONS[result] ?? result;
}

function Detail({
  label,
  value,
  hint,
}: {
  label: string;
  value: string;
  hint?: string;
}) {
  return (
    <div>
      <dt className="text-xs uppercase tracking-wide text-content-muted">
        {label}
      </dt>
      <dd className="mt-0.5 break-all" title={hint}>
        {value}
      </dd>
      {hint ? <p className="mt-0.5 text-xs text-content-muted">{hint}</p> : null}
    </div>
  );
}
