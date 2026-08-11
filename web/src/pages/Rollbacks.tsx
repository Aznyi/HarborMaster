import { useMemo, useState } from "react";
import { Link } from "react-router";

import type {
  Rollback,
  RollbackState,
  RollbackSummary,
} from "../api/rollbackTypes";
import {
  ROLLBACK_STATE_LABELS,
  ROLLBACK_STATE_ORDER,
  isRollbackActive,
  rollbackNeedsAttention,
} from "../api/rollbackTypes";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import {
  RollbackCheckpointBadge,
  RollbackFailureBadge,
  RollbackStateBadge,
  RollbackWarningNotice,
} from "../components/RollbackBadges";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { useRollbacks } from "../hooks/useRollbacks";

const PAGE_SIZE = 25;

/**
 * The manual rollback history.
 *
 * Every row is a record of a container being swapped back to what it was
 * running before a recreation. Laid out around one question: is anything wrong
 * right now.
 *
 * The list polls only while something is actually moving, so a settled estate
 * costs nothing to leave open.
 */
export function Rollbacks() {
  const [page, setPage] = useState(1);
  const [state, setState] = useState<RollbackState | "">("");
  const [attentionOnly, setAttentionOnly] = useState(false);

  const query = useMemo(
    () => ({
      page,
      pageSize: PAGE_SIZE,
      ...(state ? { state: [state] } : {}),
      ...(attentionOnly ? { needsAttention: true } : {}),
    }),
    [page, state, attentionOnly],
  );

  const rollbacks = useRollbacks(query, { pollWhileActive: true });
  const summary = rollbacks.data?.summary;

  return (
    <div className="space-y-6">
      <PageIntro
        title="Rollbacks"
        description={
          "Recreations an operator has undone, and what happened to each " +
          "attempt. Nothing here happens automatically."
        }
      />

      <RollbackWarningNotice />

      <SummaryCards state={rollbacks} summary={summary} />

      <section className="space-y-4">
        <Filters
          state={state}
          attentionOnly={attentionOnly}
          onState={(value) => {
            setState(value);
            setPage(1);
          }}
          onAttentionOnly={(value) => {
            setAttentionOnly(value);
            setPage(1);
          }}
        />

        <RollbackList state={rollbacks} onPage={setPage} />
      </section>
    </div>
  );
}

function SummaryCards({
  state,
  summary,
}: {
  state: ReturnType<typeof useRollbacks>;
  summary: RollbackSummary | undefined;
}) {
  if (state.status === "loading") return <LoadingState label="Loading rollbacks" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }
  // Tolerate a null or malformed payload rather than throwing.
  if (!summary) return <LoadingState label="Loading rollbacks" />;

  // A deployment that has not opted in is stated explicitly, so an empty list
  // is not read as "nothing has ever been rolled back".
  if (!summary.enabled) {
    return (
      <p
        role="status"
        className="rounded-lg border border-border-subtle bg-surface px-3 py-2 text-sm text-content-muted"
      >
        Manual rollback is switched off in this deployment. HarborMaster holds
        no ability to stop a serving container and start the one it replaced,
        and records already stored remain readable.
      </p>
    );
  }

  return (
    <section
      aria-label="Rollback summary"
      className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4"
    >
      {/* Needs attention comes FIRST, and is the only card that can be red.
          It is the reason an operator opens this page. */}
      <Card
        label="Needs attention"
        value={summary.needsAttention}
        hint="Left containers on this host"
        tone={summary.needsAttention > 0 ? "danger" : "neutral"}
      />
      <Card label="In progress" value={summary.active} hint="Running now" />
      <Card
        label="Rolled back"
        value={summary.succeeded}
        hint="Original restored and verified"
      />
      <Card label="Total" value={summary.total} hint="Every attempt recorded" />
    </section>
  );
}

function Card({
  label,
  value,
  hint,
  tone = "neutral",
}: {
  label: string;
  value: number;
  hint: string;
  tone?: "neutral" | "warn" | "danger";
}) {
  const toneClasses = {
    neutral: "border-border-subtle",
    warn: "border-warn/40",
    danger: "border-danger/40",
  }[tone];

  return (
    <div className={`rounded-lg border ${toneClasses} bg-surface p-4`}>
      <p className="text-xs font-medium uppercase tracking-wide text-content-muted">
        {label}
      </p>
      <p className="mt-1 text-2xl font-semibold text-content">{value}</p>
      <p className="mt-1 text-xs text-content-muted">{hint}</p>
    </div>
  );
}

function Filters({
  state,
  attentionOnly,
  onState,
  onAttentionOnly,
}: {
  state: RollbackState | "";
  attentionOnly: boolean;
  onState: (value: RollbackState | "") => void;
  onAttentionOnly: (value: boolean) => void;
}) {
  return (
    <div className="flex flex-wrap items-end gap-3">
      <label className="flex flex-col gap-1 text-xs text-content-muted">
        State
        <select
          className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
          value={state}
          onChange={(event) => onState(event.target.value as RollbackState | "")}
        >
          <option value="">All states</option>
          {ROLLBACK_STATE_ORDER.map((value) => (
            <option key={value} value={value}>
              {ROLLBACK_STATE_LABELS[value]}
            </option>
          ))}
        </select>
      </label>

      <label className="flex items-center gap-2 pb-1.5 text-sm text-content">
        <input
          type="checkbox"
          className="h-6 w-6 shrink-0 rounded border-border-subtle"
          checked={attentionOnly}
          onChange={(event) => onAttentionOnly(event.target.checked)}
        />
        Only ones that left containers behind
      </label>
    </div>
  );
}

function RollbackList({
  state,
  onPage,
}: {
  state: ReturnType<typeof useRollbacks>;
  onPage: (page: number) => void;
}) {
  if (state.status === "loading") return <LoadingState label="Loading rollbacks" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }

  const data = state.data;
  const items = data?.items ?? [];

  if (items.length === 0) {
    return (
      <EmptyState
        title="No rollbacks match these filters"
        description={
          "A rollback is requested from a recreation that left its original " +
          "container in place. Nothing here happens on a schedule — " +
          "HarborMaster never undoes a recreation on its own."
        }
      />
    );
  }

  return (
    <div className="space-y-3">
      <ul className="space-y-2">
        {items.map((rollback) => (
          <RollbackRow key={rollback.rollbackId} rollback={rollback} />
        ))}
      </ul>
      {data?.pagination && (
        <Pagination
          pagination={data.pagination}
          onPageChange={onPage}
          busy={state.refreshing}
        />
      )}
    </div>
  );
}

/** One rollback. */
function RollbackRow({ rollback }: { rollback: Rollback }) {
  const attention = rollbackNeedsAttention(rollback);

  return (
    <li
      className={`rounded-lg border bg-surface p-4 ${
        attention ? "border-danger/40" : "border-border-subtle"
      }`}
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-2">
          <div className="flex flex-wrap items-center gap-2">
            <RollbackStateBadge state={rollback.state} />
            {rollback.failure && <RollbackFailureBadge failure={rollback.failure} />}
            {/* The checkpoint is shown on anything that is not a clean
                success, because it is what says what is on the host. */}
            {rollback.state !== "succeeded" && (
              <RollbackCheckpointBadge
                checkpoint={rollback.checkpoint}
                mutated={Boolean(rollback.mutatedAt)}
              />
            )}
          </div>

          <Link
            to={`/rollbacks/${encodeURIComponent(rollback.rollbackId)}`}
            className="flex min-h-6 items-center text-sm font-medium text-content hover:underline"
          >
            {rollback.containerName}
          </Link>

          <p className="font-mono text-xs break-all text-content-muted">
            {rollback.replacementImage || "—"} → {rollback.originalImage || "—"}
          </p>

          {attention && rollback.recovery && (
            <p className="text-sm font-medium text-danger">
              {rollback.recovery.serviceInterrupted
                ? "This container is not running."
                : "Containers were left on this host."}{" "}
              <Link
                to={`/rollbacks/${encodeURIComponent(rollback.rollbackId)}`}
                className="underline underline-offset-2"
              >
                See the recovery steps
              </Link>
            </p>
          )}

          {!attention && rollback.message && (
            <p className="text-sm text-content-muted">{rollback.message}</p>
          )}

          {isRollbackActive(rollback.state) && (
            <p className="text-sm text-content-muted">
              {ROLLBACK_STATE_LABELS[rollback.state]}…
            </p>
          )}
        </div>

        <time
          dateTime={rollback.requestedAt}
          className="shrink-0 text-xs text-content-muted"
        >
          {new Date(rollback.requestedAt).toLocaleString()}
        </time>
      </div>
    </li>
  );
}
