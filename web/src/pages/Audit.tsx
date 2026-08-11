import { useCallback, useMemo, useState } from "react";

import type { AuditEvent, AuditOutcome, AuditQuery } from "../api/authTypes";
import { getAuditSummary, listAuditEvents } from "../api/client";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { useApiResource } from "../hooks/useApiResource";
import { useSession } from "../hooks/useSession";
import { NotPermitted } from "./Users";

const PAGE_SIZE = 50;

/**
 * The security audit log.
 *
 * # What a row is, and is not
 *
 * Who did what, to what, from where, and whether it worked. It is not a request
 * log: there is no body, no header, and no environment value anywhere in the
 * schema, because a request body can carry a password or a registry credential
 * and a log that held those would be a worse liability than no log.
 *
 * Every value on this page is rendered as a text node. The server bounds and
 * sanitises each field before it is stored, and nothing here interprets any of
 * them as markup.
 */
export function Audit() {
  const session = useSession();
  const [page, setPage] = useState(1);
  const [outcome, setOutcome] = useState<AuditOutcome | "">("");
  const [securityOnly, setSecurityOnly] = useState(false);

  const query = useMemo<AuditQuery>(
    () => ({
      page,
      pageSize: PAGE_SIZE,
      ...(outcome ? { outcome: [outcome] } : {}),
      ...(securityOnly ? { securityOnly: true } : {}),
    }),
    [page, outcome, securityOnly],
  );

  const events = useApiResource(
    useCallback(
      ({ signal }: { signal: AbortSignal }) => listAuditEvents(query, { signal }),
      [query],
    ),
    { key: JSON.stringify(query) },
  );

  const summary = useApiResource(
    useCallback(
      ({ signal }: { signal: AbortSignal }) => getAuditSummary({ signal }),
      [],
    ),
  );

  if (!session.can("audit:read")) {
    return <NotPermitted />;
  }

  const pagination = events.data?.pagination;

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Security audit"
        description="Every authentication, authorization decision, and state change, attributed to the account that caused it. Records are append-only; nothing in HarborMaster can edit or delete one."
      />

      {summary.data ? (
        <dl className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          <Counter label="Records kept" value={summary.data.total} />
          <Counter
            label={`Failed sign-ins (${summary.data.windowHours}h)`}
            value={summary.data.failedLogins}
            tone={summary.data.failedLogins > 0 ? "warn" : "neutral"}
          />
          <Counter
            label={`Refused (${summary.data.windowHours}h)`}
            value={summary.data.deniedActions}
            tone={summary.data.deniedActions > 0 ? "danger" : "neutral"}
          />
          <Counter
            label={`Host changes (${summary.data.windowHours}h)`}
            value={summary.data.privilegedActions}
          />
        </dl>
      ) : null}

      <div className="flex flex-wrap items-end gap-3 rounded-xl border border-border-subtle bg-surface-raised p-4">
        <div className="flex flex-col gap-1">
          <label htmlFor="audit-outcome" className="text-sm font-medium">
            Outcome
          </label>
          <select
            id="audit-outcome"
            value={outcome}
            onChange={(event) => {
              setOutcome(event.target.value as AuditOutcome | "");
              setPage(1);
            }}
            className="rounded-lg border border-border-subtle bg-surface px-3 py-2 text-sm"
          >
            <option value="">every outcome</option>
            <option value="succeeded">succeeded</option>
            <option value="failed">failed</option>
            <option value="denied">denied</option>
          </select>
        </div>

        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={securityOnly}
            onChange={(event) => {
              setSecurityOnly(event.target.checked);
              setPage(1);
            }}
          />
          Sign-ins and account changes only
        </label>
      </div>

      {events.status === "loading" ? <LoadingState label="Loading audit records" /> : null}
      {events.status === "disconnected" ? <DisconnectedState /> : null}
      {events.status === "error" && events.error ? (
        <ErrorState error={events.error} onRetry={events.refresh} />
      ) : null}

      {events.status === "ready" && events.data ? (
        events.data.items.length === 0 ? (
          <EmptyState
            title="Nothing recorded"
            description="No audit record matches these filters."
          />
        ) : (
          <>
            <AuditTable events={events.data.items} />
            {pagination ? (
              <Pagination pagination={pagination} onPageChange={setPage} />
            ) : null}
          </>
        )
      ) : null}
    </div>
  );
}

function Counter({
  label,
  value,
  tone = "neutral",
}: {
  label: string;
  value: number;
  tone?: "neutral" | "ok" | "warn" | "danger";
}) {
  const toneClass = {
    neutral: "text-content",
    ok: "text-ok",
    warn: "text-warn",
    danger: "text-danger",
  }[tone];

  return (
    <div className="rounded-xl border border-border-subtle bg-surface-raised px-4 py-3">
      <dt className="text-xs uppercase tracking-wide text-content-muted">{label}</dt>
      <dd className={`text-lg font-semibold ${toneClass}`}>{value}</dd>
    </div>
  );
}

function AuditTable({ events }: { events: AuditEvent[] }) {
  return (
    <div
      className="overflow-x-auto rounded-xl border border-border-subtle bg-surface-raised"
      tabIndex={0}
    >
      <table className="w-full text-left text-sm">
        <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
          <tr>
            <th scope="col" className="px-4 py-3">When</th>
            <th scope="col" className="px-4 py-3">Action</th>
            <th scope="col" className="px-4 py-3">Outcome</th>
            <th scope="col" className="px-4 py-3">Actor</th>
            <th scope="col" className="px-4 py-3">Target</th>
            <th scope="col" className="px-4 py-3">From</th>
            <th scope="col" className="px-4 py-3">Detail</th>
          </tr>
        </thead>
        <tbody>
          {events.map((event) => (
            <tr key={event.eventId} className="border-b border-border-subtle last:border-0">
              <td className="whitespace-nowrap px-4 py-3 text-content-muted">
                {formatWhen(event.occurredAt)}
              </td>
              <th scope="row" className="px-4 py-3 font-medium">
                {event.action}
              </th>
              <td className="px-4 py-3">
                <OutcomeBadge outcome={event.outcome} />
              </td>
              <td className="px-4 py-3">
                {event.actorUsername ? (
                  <>
                    {event.actorUsername}
                    {event.actorRole ? (
                      <span className="ml-1 text-xs text-content-muted">
                        ({event.actorRole})
                      </span>
                    ) : null}
                  </>
                ) : (
                  <span className="text-content-muted">—</span>
                )}
              </td>
              <td className="px-4 py-3">
                {event.targetName || event.targetId ? (
                  <>
                    {event.targetName || event.targetId}
                    {event.targetType ? (
                      <span className="ml-1 text-xs text-content-muted">
                        ({event.targetType})
                      </span>
                    ) : null}
                  </>
                ) : (
                  <span className="text-content-muted">—</span>
                )}
              </td>
              <td className="px-4 py-3 text-content-muted">
                {event.clientAddr ?? "—"}
              </td>
              <td className="px-4 py-3 text-content-muted">{event.reason ?? "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function OutcomeBadge({ outcome }: { outcome: AuditOutcome }) {
  const style = {
    succeeded: "bg-ok-soft text-ok",
    failed: "bg-warn-soft text-warn",
    denied: "bg-danger-soft text-danger",
  }[outcome];

  return (
    <span className={`rounded px-1.5 py-0.5 text-xs font-medium ${style}`}>
      {outcome}
    </span>
  );
}

function formatWhen(iso: string): string {
  const when = new Date(iso);
  return Number.isNaN(when.getTime()) ? iso : when.toLocaleString();
}
