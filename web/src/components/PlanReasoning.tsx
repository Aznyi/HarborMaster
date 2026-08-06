import type { ChangePlan, RiskFactor } from "../api/planTypes";
import { PLAN_RULE_LABELS } from "../api/planTypes";
import {
  FactorSeverityBadge,
  ProposedChange,
  RecommendationBadge,
  RiskBandBadge,
  SupersededBadge,
} from "./PlanBadges";

/**
 * The reasoning behind one assessment, and the timeline of assessments.
 *
 * # Why the factors are always shown
 *
 * A score without its reasoning is a number an operator has to take on trust,
 * and a risk model nobody can audit is worse than no risk model. Every factor
 * names the rule that produced it, so a verdict is traceable to a specific,
 * named piece of code rather than to an opaque total.
 *
 * Factors arrive in RULE ORDER, which is fixed server-side, and are rendered in
 * that order rather than re-sorted: the order is chosen to read as an argument
 * — what the change IS, then what is known about it, then what the container's
 * current state says about absorbing it.
 *
 * # Everything here is text
 *
 * Every string on a plan originates in HarborMaster's own vocabulary. None of
 * it is registry-supplied or caller-supplied, and none of it is rendered as
 * markup.
 */

/** The factor list: every rule that contributed, and what it contributed. */
export function PlanReasoning({ factors }: { factors: RiskFactor[] }) {
  if (!factors || factors.length === 0) {
    return (
      <p className="text-sm text-content-muted">
        No reasoning was recorded for this plan.
      </p>
    );
  }

  return (
    <ol className="space-y-3">
      {factors.map((factor) => (
        <li
          key={factor.rule}
          className="rounded-lg border border-border-subtle bg-surface p-3"
        >
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm font-medium text-content">
              {PLAN_RULE_LABELS[factor.rule] ?? factor.rule}
            </span>
            <FactorSeverityBadge severity={factor.severity} />
            {/* A zero-point factor is stated rather than scored, and saying so
                stops the list reading as though everything on it counted
                against the change. */}
            <span className="ml-auto text-xs tabular-nums text-content-muted">
              {factor.points > 0 ? `+${factor.points}` : "no score"}
            </span>
          </div>
          <p className="mt-1 text-sm text-content-muted">{factor.detail}</p>
        </li>
      ))}
    </ol>
  );
}

/**
 * One plan as a card: the verdict, the proposed change, and the reasoning.
 *
 * `compact` renders the summary without the factor list, for a listing where
 * the full reasoning would drown the page.
 */
export function PlanCard({
  plan,
  compact = false,
}: {
  plan: ChangePlan;
  compact?: boolean;
}) {
  return (
    <article className="space-y-3 rounded-xl border border-border-subtle bg-surface-raised p-4">
      <header className="flex flex-wrap items-center gap-2">
        <RiskBandBadge band={plan.risk.riskBand} score={plan.risk.riskScore} />
        <RecommendationBadge recommendation={plan.risk.recommendation} />
        <SupersededBadge plan={plan} />
        <time
          dateTime={plan.generatedAt}
          className="ml-auto text-xs text-content-muted"
        >
          {new Date(plan.generatedAt).toLocaleString()}
        </time>
      </header>

      <ProposedChange plan={plan} />

      <p className="text-sm text-content">{plan.risk.summary}</p>

      {!compact && (
        <section aria-label="Reasoning" className="space-y-2">
          <h4 className="text-xs font-semibold uppercase tracking-wide text-content-muted">
            Why
          </h4>
          <PlanReasoning factors={plan.risk.factors} />
        </section>
      )}

      <footer className="text-xs text-content-muted">
        Assessed by rule set {plan.plannerVersion}
      </footer>
    </article>
  );
}

/**
 * The reasoning timeline: how the assessment of one container has changed.
 *
 * Newest first, so it reads backwards from the current verdict. A superseded
 * entry keeps its place: removing it would erase exactly the history that makes
 * a verdict reviewable rather than merely stated, and it is the history the
 * immutable-plan design exists to protect.
 */
export function PlanTimeline({ plans }: { plans: ChangePlan[] }) {
  if (!plans || plans.length === 0) return null;

  return (
    <ol className="relative space-y-4 border-l border-border-subtle pl-5">
      {plans.map((plan) => (
        <li key={plan.planId} className="relative">
          <span
            aria-hidden="true"
            className={`absolute -left-[1.4rem] top-1.5 size-2.5 rounded-full ring-2 ring-surface ${dotForBand(
              plan,
            )}`}
          />

          <div className="flex flex-wrap items-center gap-2">
            <RiskBandBadge band={plan.risk.riskBand} score={plan.risk.riskScore} />
            <RecommendationBadge recommendation={plan.risk.recommendation} />
            <SupersededBadge plan={plan} />
          </div>

          <p className="mt-1 text-sm text-content">{plan.risk.summary}</p>

          <p className="mt-1 text-xs text-content-muted">
            <time dateTime={plan.generatedAt}>
              {new Date(plan.generatedAt).toLocaleString()}
            </time>
            {" · "}
            <span className="font-mono break-all">{plan.proposedImage}</span>
          </p>
        </li>
      ))}
    </ol>
  );
}

/** The timeline dot, tinted by band. Never the only signal: a badge sits beside it. */
function dotForBand(plan: ChangePlan): string {
  switch (plan.risk.riskBand) {
    case "critical":
    case "high":
      return "bg-danger";
    case "medium":
      return "bg-warn";
    case "low":
      return "bg-content-muted";
    default:
      return "bg-ok";
  }
}
