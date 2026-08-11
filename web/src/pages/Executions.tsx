import { useMemo, useState } from "react";
import { Link } from "react-router";

import type {
  Execution,
  ExecutionState,
  ExecutionSummary,
} from "../api/executionTypes";
import {
  EXECUTION_STATE_LABELS,
  EXECUTION_STATE_ORDER,
  isActive,
  needsAttention,
} from "../api/executionTypes";
import {
  ExecutionCheckpointBadge,
  ExecutionFailureBadge,
  ExecutionStateBadge,
  RecreationWarningNotice,
} from "../components/ExecutionBadges";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { useExecutions } from "../hooks/useExecutions";

const PAGE_SIZE = 25;

/**
 * The container recreation history.
 *
 * Every row is a record of a container being STOPPED AND REPLACED. This is the
 * only page in HarborMaster whose subject is something that changed a running
 * service, and it is laid out around one question: is anything wrong right now.
 *
 * The list polls only while something is actually moving, so a settled estate
 * costs nothing to leave open.
 */
export function Executions() {
  const [page, setPage] = useState(1);
  const [state, setState] = useState<ExecutionState | "">("");
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

  const executions = useExecutions(query, { pollWhileActive: true });
  const summary = executions.data?.summary;

  return (
    <div className="space-y-6">
      <PageIntro
        title="Update history"
        description={
          "Containers HarborMaster has stopped and replaced, and what happened " +
          "to each attempt. Every one was requested by an operator."
        }
      />

      <RecreationWarningNotice />

      <SummaryCards state={executions} summary={summary} />

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

        <ExecutionList state={executions} onPage={setPage} />
      </section>
    </div>
  );
}

function SummaryCards({
  state,
  summary,
}: {
  state: ReturnType<typeof useExecutions>;
  summary: ExecutionSummary | undefined;
}) {
  if (state.status === "loading") return <LoadingState label="Loading recreations" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }
  // Tolerate a null or malformed payload rather than throwing.
  if (!summary) return <LoadingState label="Loading recreations" />;

  // A deployment that has not opted in is stated explicitly, so an empty list
  // is not read as "nothing has ever been recreated".
  if (!summary.enabled) {
    return (
      <p
        role="status"
        className="rounded-lg border border-border-subtle bg-surface px-3 py-2 text-sm text-content-muted"
      >
        Container recreation is switched off in this deployment. HarborMaster
        holds no ability to stop or replace a container at all, and records
        already stored remain readable.
      </p>
    );
  }

  return (
    <section aria-label="Recreation summary" className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
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
        label="Recreated"
        value={summary.succeeded}
        hint="Verified and the original removed"
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
  state: ExecutionState | "";
  attentionOnly: boolean;
  onState: (value: ExecutionState | "") => void;
  onAttentionOnly: (value: boolean) => void;
}) {
  return (
    <div className="flex flex-wrap items-end gap-3">
      <label className="flex flex-col gap-1 text-xs text-content-muted">
        State
        <select
          className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
          value={state}
          onChange={(event) => onState(event.target.value as ExecutionState | "")}
        >
          <option value="">All states</option>
          {EXECUTION_STATE_ORDER.map((value) => (
            <option key={value} value={value}>
              {EXECUTION_STATE_LABELS[value]}
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

function ExecutionList({
  state,
  onPage,
}: {
  state: ReturnType<typeof useExecutions>;
  onPage: (page: number) => void;
}) {
  if (state.status === "loading") return <LoadingState label="Loading recreations" />;
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
        title="No updates match these filters"
        description={
          "A recreation is requested from an acquisition that succeeded. Nothing " +
          "here happens on a schedule — HarborMaster never replaces a container " +
          "on its own."
        }
      />
    );
  }

  return (
    <div className="space-y-3">
      <ul className="space-y-2">
        {items.map((execution) => (
          <ExecutionRow key={execution.executionId} execution={execution} />
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

/** One recreation. */
function ExecutionRow({ execution }: { execution: Execution }) {
  const attention = needsAttention(execution);

  return (
    <li
      className={`rounded-lg border bg-surface p-4 ${
        attention ? "border-danger/40" : "border-border-subtle"
      }`}
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-2">
          <div className="flex flex-wrap items-center gap-2">
            <ExecutionStateBadge state={execution.state} />
            {execution.failure && (
              <ExecutionFailureBadge failure={execution.failure} />
            )}
            {/* The checkpoint is shown on anything that is not a clean
                success, because it is what says what is on the host. */}
            {execution.state !== "succeeded" && (
              <ExecutionCheckpointBadge
                checkpoint={execution.checkpoint}
                mutated={Boolean(execution.mutatedAt)}
              />
            )}
          </div>

          <Link
            to={`/executions/${encodeURIComponent(execution.executionId)}`}
            className="flex min-h-6 items-center text-sm font-medium text-content hover:underline"
          >
            {execution.containerName}
          </Link>

          <p className="font-mono text-xs break-all text-content-muted">
            {execution.oldImage} → {execution.target.digest}
          </p>

          {attention && execution.recovery && (
            <p className="text-sm font-medium text-danger">
              {execution.recovery.serviceInterrupted
                ? "This container is not running."
                : "Containers were left on this host."}{" "}
              <Link
                to={`/executions/${encodeURIComponent(execution.executionId)}`}
                className="underline underline-offset-2"
              >
                See the recovery steps
              </Link>
            </p>
          )}

          {!attention && execution.message && (
            <p className="text-sm text-content-muted">{execution.message}</p>
          )}

          {isActive(execution.state) && (
            <p className="text-sm text-content-muted">
              {EXECUTION_STATE_LABELS[execution.state]}…
            </p>
          )}
        </div>

        <time
          dateTime={execution.requestedAt}
          className="shrink-0 text-xs text-content-muted"
        >
          {new Date(execution.requestedAt).toLocaleString()}
        </time>
      </div>
    </li>
  );
}
