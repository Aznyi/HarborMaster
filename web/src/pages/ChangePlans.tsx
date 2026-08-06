import { useMemo, useState } from "react";
import { Link } from "react-router";

import { ApiError } from "../api/client";
import type { UpdateType } from "../api/imageTypes";
import { UPDATE_TYPE_LABELS, UPDATE_TYPE_ORDER } from "../api/imageTypes";
import type {
  ChangePlan,
  ChangePlanSummary,
  Recommendation,
  RiskBand,
} from "../api/planTypes";
import {
  RECOMMENDATION_LABELS,
  RECOMMENDATION_ORDER,
  RISK_BAND_LABELS,
  RISK_BAND_ORDER,
} from "../api/planTypes";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import {
  ProposedChange,
  RecommendationBadge,
  RiskBandBadge,
  SupersededBadge,
} from "../components/PlanBadges";
import { PlanReasoning } from "../components/PlanReasoning";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { requestPlanGeneration, usePlans } from "../hooks/usePlans";

const PAGE_SIZE = 25;

/**
 * The change plans dashboard.
 *
 * Everything here is an ASSESSMENT of a PROPOSED change. Nothing on this page
 * applies one — there is no apply, execute, or approve control, because
 * HarborMaster has no such capability and the API exposes no such endpoint. The
 * only write asks HarborMaster to re-run its own analysis.
 *
 * The presentation rule that matters most: "cannot advise" is never rendered as
 * a quiet success. It has its own count in the summary, its own badge tone, and
 * a tooltip that says what it is not.
 */
export function ChangePlans() {
  const [page, setPage] = useState(1);
  const [band, setBand] = useState<RiskBand | "">("");
  const [recommendation, setRecommendation] = useState<Recommendation | "">("");
  const [update, setUpdate] = useState<UpdateType | "">("");
  const [actionError, setActionError] = useState<string | null>(null);
  const [generating, setGenerating] = useState(false);

  const query = useMemo(
    () => ({
      page,
      pageSize: PAGE_SIZE,
      ...(band ? { band: [band] } : {}),
      ...(recommendation ? { recommendation: [recommendation] } : {}),
      ...(update ? { update: [update] } : {}),
    }),
    [page, band, recommendation, update],
  );

  const plans = usePlans(query);

  const generate = async () => {
    setActionError(null);
    setGenerating(true);
    try {
      await requestPlanGeneration();
      // The server answered 202 — the pass is scheduled, not finished — so the
      // view is refreshed rather than assumed to be current.
      plans.refresh();
    } catch (caught) {
      setActionError(
        caught instanceof ApiError
          ? caught.message
          : "The planning pass could not be requested.",
      );
    } finally {
      setGenerating(false);
    }
  };

  const summary = plans.data?.summary;

  return (
    <div className="space-y-6">
      <PageIntro
        title="Change plans"
        description={
          "What HarborMaster thinks of each proposed image change: how risky it " +
          "looks, why, and what to do about it. Plans are analysis — nothing " +
          "here pulls an image, changes a container, or schedules anything."
        }
      />

      <SummaryCards
        state={plans}
        summary={summary}
        onGenerate={generate}
        generating={generating}
      />

      {actionError && (
        <p
          role="alert"
          className="rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger"
        >
          {actionError}
        </p>
      )}

      <section className="space-y-4">
        <Filters
          band={band}
          recommendation={recommendation}
          update={update}
          onBand={(value) => {
            setBand(value);
            setPage(1);
          }}
          onRecommendation={(value) => {
            setRecommendation(value);
            setPage(1);
          }}
          onUpdate={(value) => {
            setUpdate(value);
            setPage(1);
          }}
        />

        <PlanList state={plans} onPage={setPage} />
      </section>
    </div>
  );
}

/** The summary cards. */
function SummaryCards({
  state,
  summary,
  onGenerate,
  generating,
}: {
  state: ReturnType<typeof usePlans>;
  summary: ChangePlanSummary | undefined;
  onGenerate: () => void;
  generating: boolean;
}) {
  if (state.status === "loading") return <LoadingState label="Loading change plans" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }
  // Tolerate a null or malformed payload rather than throwing: a view that
  // crashes on unexpected input turns a backend hiccup into a blank screen.
  if (!summary) return <LoadingState label="Loading change plans" />;

  return (
    <section aria-label="Plan summary" className="space-y-3">
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Card
          label="Ready to act on"
          value={summary.actionable}
          hint="Proceed, or proceed with caution"
          tone={summary.actionable > 0 ? "ok" : "neutral"}
        />
        <Card
          label="Needs a person"
          value={summary.needsReview}
          hint="Enough is in question to warrant a look"
          tone={summary.needsReview > 0 ? "warn" : "neutral"}
        />
        <Card
          label="Argued against"
          value={summary.blocked}
          hint="Something should be settled first"
          tone={summary.blocked > 0 ? "danger" : "neutral"}
        />
        <Card
          label="Cannot advise"
          value={summary.undetermined}
          // Sits beside the rest deliberately. A dashboard that folded these
          // into "ready to act on" would report a partly-assessed estate as a
          // clean one.
          hint="Something could not be checked. Not a finding of safety"
          tone={summary.undetermined > 0 ? "warn" : "neutral"}
        />
      </div>

      {summary.undetermined > 0 && (
        <p
          role="status"
          className="rounded-lg border border-border-subtle bg-surface px-3 py-2 text-xs text-content-muted"
        >
          {summary.undetermined} of {summary.plans} plans could not be judged,
          usually because a registry did not answer or a digest could not be
          resolved. Those are gaps in evidence, not findings of safety.
        </p>
      )}

      <div className="flex flex-wrap items-center gap-3">
        <BandBreakdown summary={summary} />

        <button
          type="button"
          onClick={onGenerate}
          disabled={generating}
          className="ml-auto rounded-md border border-border-subtle bg-surface-raised px-3 py-1.5 text-sm font-medium text-content disabled:opacity-60"
          // Says what it does. It regenerates HarborMaster's own analysis; it
          // applies nothing, and promising a finished result would be a lie
          // about a pass that runs in the background.
          title="Re-runs the risk model over HarborMaster's own records. Nothing is pulled, changed, or scheduled. Returns as soon as it is queued."
        >
          {generating ? "Requesting…" : "Reassess"}
        </button>
      </div>

      <p className="text-xs text-content-muted">
        {summary.plans} {summary.plans === 1 ? "plan" : "plans"} across{" "}
        {summary.containers}{" "}
        {summary.containers === 1 ? "container" : "containers"}
        {summary.lastGeneratedAt && (
          <>
            {" · last assessed "}
            <time dateTime={summary.lastGeneratedAt}>
              {new Date(summary.lastGeneratedAt).toLocaleString()}
            </time>
          </>
        )}
        {summary.plannerVersion && ` · rule set ${summary.plannerVersion}`}
      </p>
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
  value: number | string;
  hint: string;
  tone?: "neutral" | "ok" | "warn" | "danger";
}) {
  const toneClasses = {
    neutral: "border-border-subtle",
    ok: "border-ok/40",
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

/** A compact distribution of risk bands. */
function BandBreakdown({ summary }: { summary: ChangePlanSummary }) {
  const present = RISK_BAND_ORDER.filter((band) => (summary.byRiskBand[band] ?? 0) > 0);
  if (present.length === 0) return null;

  return (
    <div className="flex flex-wrap items-center gap-2" aria-label="Plans by risk band">
      {present.map((band) => (
        <span key={band} className="inline-flex items-center gap-1.5">
          <RiskBandBadge band={band} />
          <span className="text-xs text-content-muted">
            {summary.byRiskBand[band] ?? 0}
          </span>
        </span>
      ))}
    </div>
  );
}

function Filters({
  band,
  recommendation,
  update,
  onBand,
  onRecommendation,
  onUpdate,
}: {
  band: RiskBand | "";
  recommendation: Recommendation | "";
  update: UpdateType | "";
  onBand: (value: RiskBand | "") => void;
  onRecommendation: (value: Recommendation | "") => void;
  onUpdate: (value: UpdateType | "") => void;
}) {
  return (
    <div className="flex flex-wrap items-end gap-3">
      <label className="flex flex-col gap-1 text-xs text-content-muted">
        Risk
        <select
          className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
          value={band}
          onChange={(event) => onBand(event.target.value as RiskBand | "")}
        >
          <option value="">All risk levels</option>
          {RISK_BAND_ORDER.map((value) => (
            <option key={value} value={value}>
              {RISK_BAND_LABELS[value]}
            </option>
          ))}
        </select>
      </label>

      <label className="flex flex-col gap-1 text-xs text-content-muted">
        Recommendation
        <select
          className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
          value={recommendation}
          onChange={(event) =>
            onRecommendation(event.target.value as Recommendation | "")
          }
        >
          <option value="">All recommendations</option>
          {RECOMMENDATION_ORDER.map((value) => (
            <option key={value} value={value}>
              {RECOMMENDATION_LABELS[value]}
            </option>
          ))}
        </select>
      </label>

      <label className="flex flex-col gap-1 text-xs text-content-muted">
        Change
        <select
          className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
          value={update}
          onChange={(event) => onUpdate(event.target.value as UpdateType | "")}
        >
          <option value="">All changes</option>
          {UPDATE_TYPE_ORDER.map((value) => (
            <option key={value} value={value}>
              {UPDATE_TYPE_LABELS[value]}
            </option>
          ))}
        </select>
      </label>
    </div>
  );
}

function PlanList({
  state,
  onPage,
}: {
  state: ReturnType<typeof usePlans>;
  onPage: (page: number) => void;
}) {
  if (state.status === "loading") return <LoadingState label="Loading change plans" />;
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
        title="No plans match these filters"
        description={
          "A container with no plan has no change proposed for it — which is " +
          "not the same as a change that was assessed and found safe. Plans " +
          "appear once image intelligence finds a newer version, or once it " +
          "cannot establish whether there is one."
        }
      />
    );
  }

  return (
    <div className="space-y-3">
      <ul className="space-y-2">
        {items.map((plan) => (
          <PlanRow key={plan.planId} plan={plan} />
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

/** One plan, with its reasoning behind a disclosure. */
function PlanRow({ plan }: { plan: ChangePlan }) {
  return (
    <li className="rounded-lg border border-border-subtle bg-surface p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-2">
          <div className="flex flex-wrap items-center gap-2">
            <RiskBandBadge band={plan.risk.riskBand} score={plan.risk.riskScore} />
            <RecommendationBadge recommendation={plan.risk.recommendation} />
            <SupersededBadge plan={plan} />
          </div>

          <Link
            to={`/plans/container/${encodeURIComponent(plan.containerId)}`}
            className="text-sm font-medium text-content hover:underline"
          >
            {plan.containerName}
          </Link>

          <ProposedChange plan={plan} />

          <p className="text-sm text-content-muted">{plan.risk.summary}</p>
        </div>

        <time
          dateTime={plan.generatedAt}
          className="shrink-0 text-xs text-content-muted"
        >
          {new Date(plan.generatedAt).toLocaleString()}
        </time>
      </div>

      {/* Collapsed by default: the reasoning is the point, but twenty of them
          expanded at once would drown the list. */}
      <details className="mt-3">
        <summary className="cursor-pointer text-xs font-medium text-content-muted">
          Why this verdict ({plan.risk.factors?.length ?? 0}{" "}
          {plan.risk.factors?.length === 1 ? "factor" : "factors"})
        </summary>
        <div className="mt-2">
          <PlanReasoning factors={plan.risk.factors} />
        </div>
      </details>
    </li>
  );
}
