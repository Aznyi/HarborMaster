import { useCallback, useMemo, useState } from "react";
import { Link } from "react-router";

import { UPDATE_TYPE_LABELS } from "../api/imageTypes";
import { RECOMMENDATION_LABELS } from "../api/planTypes";
import {
  buildUpdateRows,
  filterRows,
  summarise,
  type UpdateRowModel,
  type UpdateTab,
} from "../api/updateWorkspace";
import { refreshImageMetadata } from "../api/client";
import { EmptyState, ErrorState, LoadingState } from "../components/States";
import { PageIntro } from "../components/PageIntro";
import { PlanReasoning } from "../components/PlanReasoning";
import { StatusBadge } from "../components/StatusBadge";
import { UpdateAction } from "../components/UpdateAction";
import { useAcquisitions } from "../hooks/useAcquisitions";
import { useAutomationUpcoming } from "../hooks/useAutomation";
import { usePlans } from "../hooks/usePlans";
import { useSession } from "../hooks/useSession";

/**
 * The Updates workspace.
 *
 * # What this replaced
 *
 * A Phase 1 landing page that listed five other pages, and behind it a workflow
 * spread across four screens: the review list decided, a container plan page
 * downloaded, an acquisition page recreated, and nothing on any of them said
 * what the next screen was.
 *
 * # What it is
 *
 * One row per container, joining three reads the server already serves -- the
 * current change plans, what the next automation pass would do, and any
 * download already under way. The row shows the decision and offers the next
 * step of the existing pipeline in place.
 *
 * # What it deliberately is not
 *
 * It is not a second update engine. Every write goes through the same three
 * components and the same three endpoints as before, in the order the server
 * already requires, each still anchored to the record that authorises it. This
 * page contributes no opinion about whether a container may be updated: the
 * assessment is the planner's and the automation context is the engine's.
 */
export function Updates() {
  const [tab, setTab] = useState<UpdateTab>("available");
  const [checking, setChecking] = useState(false);
  const [checkError, setCheckError] = useState<string | null>(null);

  const session = useSession();
  // One page of each. The workspace is a working list, not an archive; the
  // specialised pages remain for the full history.
  const plans = usePlans({ page: 1, pageSize: 100 });
  const upcoming = useAutomationUpcoming();
  const acquisitions = useAcquisitions({ page: 1, pageSize: 100 });

  const refreshAll = useCallback(() => {
    plans.refresh();
    upcoming.refresh();
    acquisitions.refresh();
  }, [acquisitions, plans, upcoming]);

  const rows = useMemo(
    () =>
      buildUpdateRows(
        plans.data?.items ?? [],
        upcoming.data?.items ?? [],
        acquisitions.data?.items ?? [],
      ),
    [acquisitions.data, plans.data, upcoming.data],
  );

  const summary = useMemo(() => summarise(rows), [rows]);
  const shown = useMemo(() => filterRows(rows, tab), [rows, tab]);

  const check = async () => {
    setChecking(true);
    setCheckError(null);
    try {
      // The SAME endpoint the Images page uses. No second registry path.
      await refreshImageMetadata();
      refreshAll();
    } catch (error) {
      setCheckError(
        error instanceof Error
          ? error.message
          : "HarborMaster could not start a registry check.",
      );
    } finally {
      setChecking(false);
    }
  };

  const mayRefresh = session.can("image:refresh");

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Updates"
        description="What each container could move to, what HarborMaster makes of it, and what happens next. Nothing here changes a container until you say so."
      />

      <div className="flex flex-wrap items-center gap-3">
        {mayRefresh ? (
          <button
            type="button"
            onClick={() => void check()}
            disabled={checking}
            className="min-h-11 rounded-lg border border-border-subtle bg-surface-raised px-4 py-2 text-sm font-medium"
          >
            {checking ? "Checking…" : "Check for updates"}
          </button>
        ) : null}
        <p className="text-xs text-content-muted">
          Asks every registry what it serves now. It downloads nothing.
        </p>
      </div>

      {checkError ? (
        <p role="alert" className="text-sm text-danger">
          {checkError}
        </p>
      ) : null}

      <SummaryCounts summary={summary} />

      <Tabs tab={tab} onChange={setTab} summary={summary} total={rows.length} />

      {plans.status === "loading" ? (
        <LoadingState label="Loading updates" />
      ) : plans.error ? (
        <ErrorState error={plans.error} onRetry={refreshAll} />
      ) : shown.length === 0 ? (
        <EmptyState
          title={emptyTitle(tab)}
          description={emptyDescription(tab)}
        />
      ) : (
        <ul className="flex flex-col gap-3" data-testid="update-rows">
          {shown.map((row) => (
            <UpdateRow key={row.plan.planId} row={row} onChanged={refreshAll} />
          ))}
        </ul>
      )}
    </div>
  );
}

function emptyTitle(tab: UpdateTab): string {
  if (tab === "review") return "Nothing is waiting for you";
  if (tab === "available") return "No updates available";
  return "No change plans yet";
}

function emptyDescription(tab: UpdateTab): string {
  if (tab === "review") {
    return "No update currently needs a person to look at it.";
  }
  if (tab === "available") {
    return "Every container is running what its registry serves, as far as the last check established. Use Check for updates to ask again.";
  }
  return "HarborMaster has not assessed anything yet. This fills in after the first update assessment.";
}

/**
 * The four numbers.
 *
 * "Cannot advise" is counted separately and never folded into the others: a gap
 * in evidence is not a finding of safety, and adding it to "available" would
 * claim HarborMaster knows about updates it explicitly could not assess.
 */
function SummaryCounts({
  summary,
}: {
  summary: ReturnType<typeof summarise>;
}) {
  const cards = [
    { label: "Available", value: summary.available, hint: "Propose a change" },
    { label: "Ready", value: summary.ready, hint: "Assessed and actionable" },
    { label: "Need review", value: summary.needsReview, hint: "Waiting for a person" },
    {
      label: "Cannot advise",
      value: summary.undetermined,
      hint: "Not a finding of safety",
    },
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

function Tabs({
  tab,
  onChange,
  summary,
  total,
}: {
  tab: UpdateTab;
  onChange: (tab: UpdateTab) => void;
  summary: ReturnType<typeof summarise>;
  total: number;
}) {
  const tabs: { id: UpdateTab; label: string; count: number }[] = [
    { id: "available", label: "Available", count: summary.ready },
    { id: "review", label: "Needs review", count: summary.needsReview },
    { id: "all", label: "All", count: total },
  ];

  return (
    <div role="tablist" aria-label="Update views" className="flex flex-wrap gap-2">
      {tabs.map((entry) => (
        <button
          key={entry.id}
          type="button"
          role="tab"
          aria-selected={tab === entry.id}
          onClick={() => onChange(entry.id)}
          className={`min-h-11 rounded-lg border px-3 py-1.5 text-sm font-medium transition-colors ${
            tab === entry.id
              ? "border-accent bg-accent-soft text-accent"
              : "border-border-subtle text-content-muted hover:text-content"
          }`}
        >
          {entry.label}
          <span className="ml-2 text-xs">{entry.count}</span>
        </button>
      ))}
    </div>
  );
}

/**
 * One container's update.
 *
 * A grid rather than a table row: it reflows to one column on a phone without a
 * second implementation, and a 64-character digest wraps instead of pushing the
 * page sideways. The detail sits behind a disclosure, so the list stays about
 * the decision.
 */
function UpdateRow({
  row,
  onChanged,
}: {
  row: UpdateRowModel;
  onChanged: () => void;
}) {
  const { plan, assessment, automation } = row;

  return (
    <li className="rounded-xl border border-border-subtle bg-surface-raised p-4">
      <div className="grid gap-4 lg:grid-cols-[minmax(0,2fr)_minmax(0,1fr)_minmax(0,1fr)_minmax(0,1.2fr)]">
        <div className="min-w-0">
          <Link
            to={`/containers/${encodeURIComponent(plan.containerId)}`}
            className="text-sm font-semibold hover:underline"
          >
            {plan.containerName}
          </Link>
          <p className="mt-1 break-all font-mono text-xs text-content-muted">
            {plan.currentImage}
            {plan.proposedImage ? (
              <>
                {" → "}
                <span className="text-content">{plan.proposedImage}</span>
              </>
            ) : null}
          </p>
        </div>

        <div className="min-w-0">
          <StatusBadge tone={assessment.tone} label={assessment.label} />
          {plan.updateType ? (
            <p className="mt-1 text-xs text-content-muted">
              {UPDATE_TYPE_LABELS[plan.updateType]}
            </p>
          ) : null}
        </div>

        <div className="min-w-0">
          <p className="text-sm">{automation.label}</p>
          {automation.detail ? (
            <p className="mt-1 text-xs text-content-muted">{automation.detail}</p>
          ) : null}
        </div>

        <div className="min-w-0">
          <UpdateAction row={row} onChanged={onChanged} />
        </div>
      </div>

      {assessment.summary ? (
        <p className="mt-3 max-w-prose text-sm text-content-muted">
          {assessment.summary}
        </p>
      ) : null}

      <details className="mt-3">
        <summary className="flex min-h-6 cursor-pointer items-center text-xs font-medium text-content-muted">
          Details and reasoning
        </summary>
        <div className="mt-3 flex flex-col gap-3">
          <dl className="grid gap-2 text-xs sm:grid-cols-2">
            <Detail label="Recommendation">
              {RECOMMENDATION_LABELS[plan.risk.recommendation]}
            </Detail>
            <Detail label="Change">
              {UPDATE_TYPE_LABELS[plan.updateType]}
            </Detail>
            <Detail label="Current digest">{plan.currentDigest ?? "—"}</Detail>
            <Detail label="Proposed digest">{plan.proposedDigest ?? "—"}</Detail>
            <Detail label="Assessed">
              {new Date(plan.generatedAt).toLocaleString()}
            </Detail>
            <Detail label="Plan">{plan.planId}</Detail>
          </dl>

          <PlanReasoning factors={plan.risk.factors} />

          <p className="text-xs text-content-muted">
            <Link
              to={`/plans/container/${encodeURIComponent(plan.containerId)}`}
              className="text-accent hover:underline"
            >
              Full plan history for this container
            </Link>
          </p>
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
