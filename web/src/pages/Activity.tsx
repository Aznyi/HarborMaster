import { formatMoment } from "../api/presentation";
import { useMemo, useState } from "react";
import { Link } from "react-router";

import {
  buildActivityFeed,
  describeChange,
  feedHorizon,
  filterActivity,
  summariseActivity,
  type ActivityEntry,
  type ActivityKind,
} from "../api/activityFeed";
import { EmptyState, ErrorState, LoadingState } from "../components/States";
import { PageIntro } from "../components/PageIntro";
import { RecreateContainerAction } from "../components/RecreateContainerAction";
import { StatusBadge } from "../components/StatusBadge";
import { useAcquisitions } from "../hooks/useAcquisitions";
import { useExecutions } from "../hooks/useExecutions";
import { useRollbacks } from "../hooks/useRollbacks";
import { useSession } from "../hooks/useSession";

/**
 * The Activity workspace.
 *
 * # What this replaced
 *
 * A Phase 1 landing page listing three others, and behind it a history split by
 * RECORD TYPE: image downloads, update history, rollbacks. Finding out whether a
 * container came back up meant knowing that a recreation is an "execution", that
 * its undo is a separate "rollback" record, and then reading both lists.
 *
 * # The reads, and their bound
 *
 * One page of each of the three lists, joined on stored foreign keys. The lists
 * paginate independently, so the merged feed is complete only within the loaded
 * window -- which the page states rather than implies. See `feedHorizon`.
 *
 * # Where the actions come from
 *
 * Continuation reuses Phase 2's RecreateContainerAction, which needs only the
 * acquisition record this page already holds. Rollback does NOT get a control
 * here: its eligibility is computed by the server on the execution DETAIL
 * response, so offering the button in a list would mean one detail request per
 * failed row. The entry links to that page, where the existing control lives
 * with the verdict that governs it.
 */
export function Activity() {
  const [kind, setKind] = useState<ActivityKind>("all");
  const [search, setSearch] = useState("");

  const session = useSession();
  // Bounded reads. A history page is not a reason to fetch unlimited history,
  // and each of these is the same list its specialised page already requests.
  const acquisitions = useAcquisitions({ page: 1, pageSize: 50 });
  const executions = useExecutions({ page: 1, pageSize: 50 });
  const rollbacks = useRollbacks({ page: 1, pageSize: 50 });

  const refreshAll = () => {
    acquisitions.refresh();
    executions.refresh();
    rollbacks.refresh();
  };

  const entries = useMemo(
    () =>
      buildActivityFeed(
        acquisitions.data?.items ?? [],
        executions.data?.items ?? [],
        rollbacks.data?.items ?? [],
      ),
    [acquisitions.data, executions.data, rollbacks.data],
  );

  const summary = useMemo(() => summariseActivity(entries), [entries]);
  const shown = useMemo(
    () => filterActivity(entries, kind, search),
    [entries, kind, search],
  );
  const horizon = useMemo(
    () =>
      feedHorizon(
        acquisitions.data?.items ?? [],
        executions.data?.items ?? [],
        rollbacks.data?.items ?? [],
      ),
    [acquisitions.data, executions.data, rollbacks.data],
  );

  const attention = entries.filter((entry) => entry.status.needsAttention);
  const loading =
    executions.status === "loading" ||
    acquisitions.status === "loading" ||
    rollbacks.status === "loading";
  const failure = executions.error ?? acquisitions.error ?? rollbacks.error;

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Activity"
        description="What HarborMaster has done to your containers: what it downloaded, what it replaced, what failed, and what it put back."
      />

      <SummaryCounts summary={summary} />

      {attention.length > 0 ? (
        <NeedsAttention entries={attention} />
      ) : null}

      <Filters
        kind={kind}
        onKind={setKind}
        search={search}
        onSearch={setSearch}
      />

      {loading ? (
        <LoadingState label="Loading activity" />
      ) : failure ? (
        <ErrorState error={failure} onRetry={refreshAll} />
      ) : shown.length === 0 ? (
        <EmptyState
          title={entries.length === 0 ? "Nothing has happened yet" : "Nothing matches"}
          description={
            entries.length === 0
              ? "HarborMaster has not downloaded an image or recreated a container. Activity fills in as it works."
              : "No activity in the loaded history matches this filter."
          }
        />
      ) : (
        <>
          <ol className="flex flex-col gap-3" data-testid="activity-feed">
            {shown.map((entry) => (
              <ActivityRow
                key={entry.key}
                entry={entry}
                mayExecute={session.can("execution:create")}
                onChanged={refreshAll}
              />
            ))}
          </ol>

          {horizon ? (
            <p className="text-xs text-content-muted">
              Complete back to {formatMoment(horizon)}. Older
              activity is held in{" "}
              <Link to="/executions" className="text-accent underline">
                update history
              </Link>
              ,{" "}
              <Link to="/acquisitions" className="text-accent underline">
                image downloads
              </Link>{" "}
              and{" "}
              <Link to="/rollbacks" className="text-accent underline">
                rollbacks
              </Link>
              , which page independently.
            </p>
          ) : null}
        </>
      )}
    </div>
  );
}

function SummaryCounts({
  summary,
}: {
  summary: ReturnType<typeof summariseActivity>;
}) {
  const cards = [
    { label: "Recent operations", value: summary.total, hint: "In the loaded window" },
    { label: "Need attention", value: summary.needsAttention, hint: "Still unresolved" },
    { label: "Recovered", value: summary.recovered, hint: "Failed, then restored" },
    { label: "In progress", value: summary.inProgress, hint: "Happening now" },
  ];

  return (
    <dl className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
      {cards.map((card) => (
        <div
          key={card.label}
          className="rounded-xl border border-border-subtle bg-surface-raised px-4 py-3"
        >
          <dt className="text-xs uppercase tracking-wide text-content-muted">
            {card.label}
          </dt>
          <dd className="mt-1 text-2xl font-semibold">{card.value}</dd>
          <dd className="text-xs text-content-muted">{card.hint}</dd>
        </div>
      ))}
    </dl>
  );
}

/**
 * Unresolved failures, at the top.
 *
 * A failure that was rolled back successfully is deliberately absent: the
 * container came back, and listing it here forever would train an operator to
 * ignore the section that exists for the ones that did not.
 */
function NeedsAttention({ entries }: { entries: ActivityEntry[] }) {
  return (
    <section
      aria-labelledby="activity-attention-heading"
      data-testid="activity-attention"
      className="flex flex-col gap-3 rounded-xl border border-danger/40 bg-danger-soft p-5"
    >
      <h2 id="activity-attention-heading" className="text-base font-semibold">
        Needs attention
      </h2>
      <p className="text-sm">
        {entries.length}{" "}
        {entries.length === 1 ? "operation" : "operations"} failed and{" "}
        {entries.length === 1 ? "has" : "have"} not been resolved.
      </p>
      <ul className="flex flex-col gap-2 text-sm">
        {entries.map((entry) => (
          <li key={entry.key} className="flex flex-wrap items-center gap-3">
            <span className="font-medium">{entry.containerName}</span>
            <span className="text-content-muted">{entry.status.label}</span>
            {entry.execution ? (
              <Link
                to={`/executions/${encodeURIComponent(entry.execution.executionId)}`}
                className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
              >
                Review and recover
              </Link>
            ) : null}
          </li>
        ))}
      </ul>
    </section>
  );
}

function Filters({
  kind,
  onKind,
  search,
  onSearch,
}: {
  kind: ActivityKind;
  onKind: (kind: ActivityKind) => void;
  search: string;
  onSearch: (value: string) => void;
}) {
  const kinds: { id: ActivityKind; label: string }[] = [
    { id: "all", label: "All" },
    { id: "updates", label: "Updates" },
    { id: "downloads", label: "Downloads" },
    { id: "failures", label: "Failures" },
    { id: "rollbacks", label: "Rollbacks" },
  ];

  return (
    <div className="flex flex-wrap items-center gap-3">
      <label className="flex min-w-0 flex-1 flex-col gap-1 sm:max-w-xs">
        <span className="text-xs text-content-muted">Search</span>
        <input
          type="search"
          value={search}
          onChange={(event) => onSearch(event.target.value)}
          placeholder="Container or image"
          className="min-h-11 rounded-lg border border-border-subtle bg-surface px-3 py-2 text-sm"
        />
      </label>

      <div role="tablist" aria-label="Activity type" className="flex flex-wrap gap-2">
        {kinds.map((entry) => (
          <button
            key={entry.id}
            type="button"
            role="tab"
            aria-selected={kind === entry.id}
            onClick={() => onKind(entry.id)}
            className={`min-h-11 rounded-lg border px-3 py-1.5 text-sm font-medium transition-colors ${
              kind === entry.id
                ? "border-accent bg-accent-soft text-accent"
                : "border-border-subtle text-content-muted hover:text-content"
            }`}
          >
            {entry.label}
          </button>
        ))}
      </div>
    </div>
  );
}

/**
 * One attempted update.
 *
 * The row answers what happened; the disclosure carries the record ids,
 * digests and per-stage timestamps that only matter once somebody is
 * investigating.
 */
function ActivityRow({
  entry,
  mayExecute,
  onChanged,
}: {
  entry: ActivityEntry;
  mayExecute: boolean;
  onChanged: () => void;
}) {
  const change = describeChange(entry);
  const readyToApply =
    entry.status.kind === "waiting" &&
    entry.acquisition?.state === "succeeded" &&
    !entry.execution;

  return (
    <li className="rounded-xl border border-border-subtle bg-surface-raised p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            {entry.containerId ? (
              <Link
                to={`/containers/${encodeURIComponent(entry.containerId)}`}
                className="text-sm font-semibold hover:underline"
              >
                {entry.containerName}
              </Link>
            ) : (
              <span className="text-sm font-semibold">{entry.containerName}</span>
            )}
            <StatusBadge tone={entry.status.tone} label={entry.status.label} />
          </div>

          {change ? (
            <p className="mt-1 break-all font-mono text-xs text-content-muted">
              {change}
            </p>
          ) : null}

          <p className="mt-1 max-w-prose text-sm text-content-muted">
            {entry.status.detail}
          </p>
        </div>

        <div className="flex shrink-0 flex-col items-end gap-2">
          {entry.at ? (
            <time
              dateTime={entry.at}
              className="text-xs text-content-muted"
            >
              {formatMoment(entry.at)}
            </time>
          ) : null}

          {/* Continuation: the same control Phase 2 offers, on the same
              acquisition record. It refuses anything but a succeeded one. */}
          {readyToApply && entry.acquisition ? (
            mayExecute ? (
              <RecreateContainerAction
                acquisition={entry.acquisition}
                onRequested={onChanged}
              />
            ) : (
              <p className="text-xs text-content-muted">
                Recreating the container needs the execution permission.
              </p>
            )
          ) : null}
        </div>
      </div>

      <details className="mt-3">
        <summary className="flex min-h-6 cursor-pointer items-center text-xs font-medium text-content-muted">
          Details
        </summary>
        <div className="mt-3 flex flex-col gap-3">
          <dl className="grid gap-2 text-xs sm:grid-cols-2">
            {entry.acquisition ? (
              <>
                <Detail label="Download">{entry.acquisition.state}</Detail>
                <Detail label="Download id">{entry.acquisition.acquisitionId}</Detail>
              </>
            ) : null}
            {entry.execution ? (
              <>
                <Detail label="Recreation">{entry.execution.state}</Detail>
                <Detail label="Recreation id">{entry.execution.executionId}</Detail>
                <Detail label="Previous image">
                  {entry.execution.oldImageDigest ?? entry.execution.oldImage}
                </Detail>
                <Detail label="Target image">
                  {entry.execution.target?.digest ?? entry.execution.target?.reference ?? "—"}
                </Detail>
              </>
            ) : null}
            {entry.rollback ? (
              <>
                <Detail label="Rollback">{entry.rollback.state}</Detail>
                <Detail label="Rollback id">{entry.rollback.rollbackId}</Detail>
              </>
            ) : null}
            {entry.rollbackAttempts.length > 1 ? (
              <Detail label="Rollback attempts">
                {String(entry.rollbackAttempts.length)}
              </Detail>
            ) : null}
          </dl>

          <div className="flex flex-wrap gap-3 text-xs">
            {entry.execution ? (
              <Link
                to={`/executions/${encodeURIComponent(entry.execution.executionId)}`}
                className="text-accent hover:underline"
              >
                Full recreation record
              </Link>
            ) : null}
            {entry.acquisition ? (
              <Link
                to={`/acquisitions/${encodeURIComponent(entry.acquisition.acquisitionId)}`}
                className="text-accent hover:underline"
              >
                Full download record
              </Link>
            ) : null}
            {entry.rollback ? (
              <Link
                to={`/rollbacks/${encodeURIComponent(entry.rollback.rollbackId)}`}
                className="text-accent hover:underline"
              >
                Full rollback record
              </Link>
            ) : null}
          </div>
        </div>
      </details>
    </li>
  );
}

function Detail({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="min-w-0">
      <dt className="text-content-muted">{label}</dt>
      <dd className="break-all font-mono text-content">{children}</dd>
    </div>
  );
}
