import type {
  ChangePlan,
  FactorSeverity,
  Recommendation,
  RiskBand,
} from "../api/planTypes";
import {
  RECOMMENDATION_LABELS,
  RECOMMENDATION_MEANING,
  RISK_BAND_LABELS,
} from "../api/planTypes";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

/**
 * Change plan badges.
 *
 * Built on the shared StatusBadge, so a plan looks like the rest of the app and
 * inherits the rule that colour is never the only signal: every badge carries a
 * label, and the tone only reinforces it.
 *
 * The one rendering decision that matters more than the others: "cannot advise"
 * must never look like "proceed". It gets its own tone and a tooltip that says
 * what it is NOT, because reading a gap in evidence as a clean bill of health is
 * the most costly mistake this page could invite.
 */

const bandTones: Record<RiskBand, BadgeTone> = {
  critical: "danger",
  high: "danger",
  medium: "warn",
  low: "neutral",
  veryLow: "ok",
};

/**
 * The risk band, with its score in the tooltip.
 *
 * The band leads and the number follows, because the band is the honest unit: a
 * bare "47" implies a measurement, while "medium" is honest about being a
 * judgement built from named factors.
 */
export function RiskBandBadge({
  band,
  score,
}: {
  band: RiskBand;
  score?: number;
}) {
  const meaning: Record<RiskBand, string> = {
    critical: "Several things argue against this change at once",
    high: "Enough is in question that a person should look",
    medium: "Worth reading the reasoning before acting",
    low: "Little in the evidence argues against this",
    veryLow: "Nothing notable found",
  };

  const title =
    score === undefined
      ? meaning[band]
      : `${meaning[band]} (score ${score} of 100)`;

  return (
    <StatusBadge tone={bandTones[band]} label={RISK_BAND_LABELS[band]} title={title} />
  );
}

const recommendationTones: Record<Recommendation, BadgeTone> = {
  proceed: "ok",
  proceedWithCaution: "warn",
  manualReview: "warn",
  notRecommended: "danger",
  // Deliberately NOT "ok". An absence of evidence must not be rendered as an
  // absence of risk.
  unknown: "neutral",
};

/** What HarborMaster suggests, with what it actually means in the tooltip. */
export function RecommendationBadge({
  recommendation,
}: {
  recommendation: Recommendation;
}) {
  return (
    <StatusBadge
      tone={recommendationTones[recommendation]}
      label={RECOMMENDATION_LABELS[recommendation]}
      title={RECOMMENDATION_MEANING[recommendation]}
    />
  );
}

const factorTones: Record<FactorSeverity, BadgeTone> = {
  blocker: "danger",
  warning: "warn",
  caution: "warn",
  unknown: "neutral",
  info: "neutral",
};

/** How much one contributing factor weighed. */
export function FactorSeverityBadge({ severity }: { severity: FactorSeverity }) {
  const meaning: Record<FactorSeverity, string> = {
    blocker: "Argues against the change on its own, whatever the score",
    warning: "Should pull a person in before acting",
    caution: "Worth reading before acting",
    unknown: "Missing evidence. Not a finding of safety",
    info: "Stated for context; argues neither way",
  };

  const label: Record<FactorSeverity, string> = {
    blocker: "blocker",
    warning: "warning",
    caution: "caution",
    unknown: "unverified",
    info: "context",
  };

  return (
    <StatusBadge
      tone={factorTones[severity]}
      label={label[severity]}
      title={meaning[severity]}
    />
  );
}

/**
 * Whether this plan is still the standing assessment.
 *
 * Rendered only when it is not. A superseded plan is not wrong — it is what was
 * believed at the time, which is the whole reason it is kept.
 */
export function SupersededBadge({ plan }: { plan: ChangePlan }) {
  if (!plan.superseded) return null;

  return (
    <StatusBadge
      tone="neutral"
      label="superseded"
      title="A newer assessment exists for this container. This one is kept as the record of what was believed at the time"
    />
  );
}

/**
 * The proposed change, rendered as a before and after.
 *
 * # Three cases, and only one of them is "the publisher republished the tag"
 *
 * This component used to have two: the reference moved, or it did not — and it
 * explained "did not" as a republished tag every time. That sentence was
 * printed over images whose tag listing had simply exceeded its budget, and
 * over `harbormaster:local`, an image that has never been in a registry at all.
 * A confident wrong explanation is worse than none, because an operator acts on
 * it.
 *
 * So the cases are now distinguished by the UPDATE TYPE, which is the field
 * that actually knows:
 *
 *  - `digest` — the reference is the same and the content moved. The only case
 *    where the republished-tag sentence is true.
 *  - nothing proposed — no target reference at all. Say that, and let the
 *    plan's own reason explain why the evidence was insufficient.
 *  - otherwise — a genuine reference change, rendered as an arrow.
 *
 * An arrow between two identical strings is never rendered: it would suggest
 * editing something that does not need editing.
 */
export function ProposedChange({ plan }: { plan: ChangePlan }) {
  const proposed = plan.proposedImage ?? "";
  const referenceMoved = proposed !== "" && proposed !== plan.currentImage;
  const republished = !referenceMoved && plan.updateType === "digest";

  return (
    <div className="space-y-1">
      <p className="font-mono text-sm break-all text-content">
        {plan.currentImage}
        {referenceMoved && (
          <>
            <span aria-hidden="true" className="mx-2 text-content-muted">
              →
            </span>
            <span className="sr-only"> becomes </span>
            {proposed}
          </>
        )}
      </p>
      {republished && (
        <p className="text-xs text-content-muted">
          The reference does not change. The publisher republished this tag, so
          the content moved underneath it.
        </p>
      )}
      {!referenceMoved && !republished && (
        <p className="text-xs text-content-muted">
          No target was proposed, so there is nothing to move onto yet.
        </p>
      )}
    </div>
  );
}

/**
 * The digest pair, when both sides are known.
 *
 * An absent digest is stated rather than hidden: it is why several plans read
 * "cannot advise", and a blank would leave that unexplained.
 */
export function PlanDigests({ plan }: { plan: ChangePlan }) {
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
      <dt className="text-content-muted">Running</dt>
      <dd className="font-mono break-all text-content">
        {plan.currentDigest || <span className="text-content-muted">not known</span>}
      </dd>
      <dt className="text-content-muted">Proposed</dt>
      <dd className="font-mono break-all text-content">
        {plan.proposedDigest || <span className="text-content-muted">not resolved</span>}
      </dd>
    </dl>
  );
}
