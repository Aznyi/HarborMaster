import { useMemo, useState } from "react";
import { Link } from "react-router";

import type {
  DriftCategory,
  DriftRecord,
  DriftSeverity,
  DriftSummary,
} from "../api/driftTypes";
import { CATEGORY_ORDER, SEVERITY_ORDER } from "../api/driftTypes";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import {
  CategoryBadge,
  ChangeKindBadge,
  DriftStatusBadge,
  DriftValues,
  SeverityBadge,
} from "../components/DriftBadges";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { useDrift, useDriftSummary } from "../hooks/useDrift";

const PAGE_SIZE = 25;

/**
 * The drift dashboard.
 *
 * Drift is an OBSERVATION: the differences between a container's baseline
 * snapshot and its configuration now. Nothing on this page changes a
 * container — there is no remediation control, because HarborMaster has no
 * such capability and the API exposes no such endpoint.
 */
export function Drift() {
  const [page, setPage] = useState(1);
  const [severity, setSeverity] = useState<DriftSeverity | "">("");
  const [category, setCategory] = useState<DriftCategory | "">("");
  const [showResolved, setShowResolved] = useState(false);

  const query = useMemo(
    () => ({
      page,
      pageSize: PAGE_SIZE,
      openOnly: !showResolved,
      ...(severity ? { severity: [severity] } : {}),
      ...(category ? { category: [category] } : {}),
    }),
    [page, severity, category, showResolved],
  );

  const summary = useDriftSummary();
  const drift = useDrift(query);

  return (
    <div className="space-y-6">
      <PageIntro
        title="Configuration drift"
        description={
          "Differences between each container's baseline snapshot and its " +
          "configuration now. HarborMaster reports drift; it cannot change a " +
          "container to resolve it."
        }
      />

      <SummaryCards state={summary} />

      <section className="space-y-4">
        <Filters
          severity={severity}
          category={category}
          showResolved={showResolved}
          onSeverity={(value) => {
            setSeverity(value);
            setPage(1);
          }}
          onCategory={(value) => {
            setCategory(value);
            setPage(1);
          }}
          onShowResolved={(value) => {
            setShowResolved(value);
            setPage(1);
          }}
        />

        <DriftList state={drift} onPage={setPage} />
      </section>
    </div>
  );
}

/** The summary cards. */
function SummaryCards({
  state,
}: {
  state: ReturnType<typeof useDriftSummary>;
}) {
  if (state.status === "loading") return <LoadingState label="Loading drift summary" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }

  // Tolerate a null or malformed payload rather than throwing: a view that
  // crashes on unexpected input turns a backend hiccup into a blank screen.
  const summary = state.data;
  if (!summary) return <LoadingState label="Loading drift summary" />;

  return (
    <section aria-label="Drift summary" className="space-y-3">
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Card
          label="Open drift"
          value={summary.open}
          hint={`${summary.total} recorded in total`}
        />
        <Card
          label="Containers affected"
          value={summary.containersWithDrift}
          // Stated as "of N evaluated" rather than "of N containers": the
          // difference between those two is the whole point of tracking
          // evaluations, and implying the rest were checked would be a lie.
          hint={`of ${summary.containersEvaluated} evaluated`}
        />
        <Card
          label="Critical"
          value={summary.bySeverity.critical ?? 0}
          hint="Lost containment boundary"
          tone={(summary.bySeverity.critical ?? 0) > 0 ? "danger" : "neutral"}
        />
        <Card
          label="High"
          value={summary.bySeverity.high ?? 0}
          hint="Wider attack surface"
          tone={(summary.bySeverity.high ?? 0) > 0 ? "warn" : "neutral"}
        />
      </div>

      {summary.incomplete && (
        <p
          role="status"
          className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-xs text-warn"
        >
          At least one container could not be fully compared, so these counts
          are a floor rather than a total. A container with no baseline snapshot
          has nothing to compare against.
        </p>
      )}

      <SeverityBreakdown summary={summary} />
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

/** A compact severity distribution, so the shape of the estate is visible. */
function SeverityBreakdown({ summary }: { summary: DriftSummary }) {
  const total = SEVERITY_ORDER.reduce(
    (sum, severity) => sum + (summary.bySeverity[severity] ?? 0),
    0,
  );
  if (total === 0) return null;

  return (
    <div className="flex flex-wrap items-center gap-2" aria-label="Open drift by severity">
      {SEVERITY_ORDER.map((severity) => {
        const count = summary.bySeverity[severity] ?? 0;
        if (count === 0) return null;
        return (
          <span key={severity} className="inline-flex items-center gap-1.5">
            <SeverityBadge severity={severity} />
            <span className="text-xs text-content-muted">{count}</span>
          </span>
        );
      })}
    </div>
  );
}

function Filters({
  severity,
  category,
  showResolved,
  onSeverity,
  onCategory,
  onShowResolved,
}: {
  severity: DriftSeverity | "";
  category: DriftCategory | "";
  showResolved: boolean;
  onSeverity: (value: DriftSeverity | "") => void;
  onCategory: (value: DriftCategory | "") => void;
  onShowResolved: (value: boolean) => void;
}) {
  return (
    <div className="flex flex-wrap items-end gap-3">
      <label className="flex flex-col gap-1 text-xs text-content-muted">
        Severity
        <select
          className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
          value={severity}
          onChange={(event) => onSeverity(event.target.value as DriftSeverity | "")}
        >
          <option value="">All severities</option>
          {SEVERITY_ORDER.map((value) => (
            <option key={value} value={value}>
              {value}
            </option>
          ))}
        </select>
      </label>

      <label className="flex flex-col gap-1 text-xs text-content-muted">
        Category
        <select
          className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
          value={category}
          onChange={(event) => onCategory(event.target.value as DriftCategory | "")}
        >
          <option value="">All categories</option>
          {CATEGORY_ORDER.map((value) => (
            <option key={value} value={value}>
              {value}
            </option>
          ))}
        </select>
      </label>

      <label className="flex items-center gap-2 pb-1.5 text-sm text-content">
        <input
          type="checkbox"
          className="size-4 rounded border-border-subtle"
          checked={showResolved}
          onChange={(event) => onShowResolved(event.target.checked)}
        />
        Include resolved
      </label>
    </div>
  );
}

function DriftList({
  state,
  onPage,
}: {
  state: ReturnType<typeof useDrift>;
  // The current page comes back in the pagination metadata, so it is not
  // threaded in separately: two sources for one value is two things to keep
  // in step.
  onPage: (page: number) => void;
}) {
  if (state.status === "loading") return <LoadingState label="Loading drift" />;
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
        title="No drift matches these filters"
        description={
          "A container with no drift matches its baseline snapshot. A container " +
          "that has never been evaluated is not shown here at all — check its " +
          "detail page to tell the two apart."
        }
      />
    );
  }

  return (
    <div className="space-y-3">
      <ul className="space-y-2">
        {items.map((record) => (
          <DriftRow key={record.id} record={record} />
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

/** One drift record. */
function DriftRow({ record }: { record: DriftRecord }) {
  return (
    <li className="rounded-lg border border-border-subtle bg-surface p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <div className="flex flex-wrap items-center gap-2">
            <SeverityBadge severity={record.severity} />
            <CategoryBadge category={record.category} />
            <ChangeKindBadge kind={record.kind} />
            <DriftStatusBadge status={record.status} />
          </div>

          <p className="font-mono text-sm text-content break-all">{record.field}</p>

          <p className="text-xs text-content-muted">
            <Link
              to={`/containers/${record.containerId}`}
              className="underline underline-offset-2 hover:text-content"
            >
              {record.containerName || record.containerId.slice(0, 12)}
            </Link>
            {" · first seen "}
            <time dateTime={record.detectedAt}>
              {new Date(record.detectedAt).toLocaleString()}
            </time>
          </p>

          {record.reason && (
            <p className="text-xs text-content-muted">{record.reason}</p>
          )}
        </div>

        <div className="min-w-0 max-w-md shrink-0">
          <DriftValues record={record} />
        </div>
      </div>
    </li>
  );
}
