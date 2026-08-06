import type { Pagination } from "./inventoryTypes";
import type { CheckStatus, UpdateType } from "./imageTypes";

/**
 * Change plan types.
 *
 * A plan is HarborMaster's ASSESSMENT of a proposed image change: how risky it
 * looks, why, and what an operator should do about it.
 *
 * # Nothing here applies a plan
 *
 * There is no mutation type in this file beyond a request to REGENERATE the
 * analysis, and no client function that could apply, execute, approve, or
 * schedule a change. HarborMaster cannot pull an image, recreate a container,
 * or roll one back, so a type describing that operation would be describing a
 * capability that does not exist.
 *
 * # Plans are immutable
 *
 * No type here has an editable field. A changed world produces a NEW plan and
 * the old one remains as the record of what was believed when a decision was
 * made — which is what makes the reasoning timeline worth rendering.
 */

/**
 * How risky a proposed change is.
 *
 * Bands rather than a bare number, because a number alone invites false
 * precision: "47" implies a measurement, while "medium" is honest about being a
 * judgement built from named factors.
 */
export type RiskBand = "veryLow" | "low" | "medium" | "high" | "critical";

/**
 * What HarborMaster suggests an operator do.
 *
 * `unknown` is deliberately NOT `proceed`. A gap in evidence is not a clean bill
 * of health, and rendering the two the same way would be the single most
 * misleading thing this page could do.
 */
export type Recommendation =
  | "proceed"
  | "proceedWithCaution"
  | "manualReview"
  | "notRecommended"
  | "unknown";

/**
 * How much one contributing factor matters.
 *
 * Separate from its point contribution: points decide the band, severity
 * decides the recommendation. A factor can add few points and still block.
 */
export type FactorSeverity = "info" | "caution" | "warning" | "blocker" | "unknown";

/** The rule that produced a factor. A closed vocabulary, fixed server-side. */
export type PlanRule =
  | "updateClassification"
  | "mutableTag"
  | "unknownDigest"
  | "registryQuality"
  | "imageAge"
  | "platform"
  | "snapshotAvailable"
  | "restoreReadiness"
  | "activeDrift"
  | "policyViolations"
  | "blastRadius";

/** Restore readiness carried over from the snapshot the plan referenced. */
export type ReadinessStatus = "unknown" | "ready" | "warning" | "notReady";

/** One named contribution to a plan's risk. */
export interface RiskFactor {
  rule: PlanRule;
  /**
   * The contribution to the score. May be zero: a factor can be worth STATING
   * without being worth scoring, and the reasoning reads better for including
   * those.
   */
  points: number;
  severity: FactorSeverity;
  /**
   * The reason, in HarborMaster's own words. Never registry-supplied text and
   * never caller input, so it is rendered as text rather than markup.
   */
  detail: string;
}

/** The verdict for one proposed change. */
export interface RiskAssessment {
  riskScore: number;
  riskBand: RiskBand;
  recommendation: Recommendation;
  summary: string;
  /** In rule order, which is fixed server-side. */
  factors: RiskFactor[];
}

/** One immutable assessment of one proposed container change. */
export interface ChangePlan {
  planId: string;
  containerId: string;
  containerName: string;

  currentImage: string;
  proposedImage: string;
  currentDigest?: string;
  proposedDigest?: string;
  updateType: UpdateType;

  snapshotId?: number;
  snapshotAvailable: boolean;
  restoreReadiness: ReadinessStatus;

  driftOpen: number;
  driftMaxSeverity?: string;
  policyOpen: number;
  policyMaxSeverity?: string;

  registryStatus: CheckStatus;
  registryDetail?: string;
  proposedPublishedAt?: string;

  risk: RiskAssessment;

  planVersion: number;
  plannerVersion: string;
  /** The fingerprint of everything the assessment read. */
  inputDigest: string;

  generatedAt: string;
  /** A newer plan exists for this container. */
  superseded: boolean;
}

/** The dashboard aggregate, over current plans only. */
export interface ChangePlanSummary {
  plans: number;
  containers: number;

  byRiskBand: Partial<Record<RiskBand, number>>;
  byRecommendation: Partial<Record<Recommendation, number>>;
  byUpdateType: Partial<Record<UpdateType, number>>;

  /** Changes an operator could make today. */
  actionable: number;
  /** Plans that ask for a person. */
  needsReview: number;
  /** Plans recommending against the change. */
  blocked: number;
  /**
   * Plans HarborMaster could not judge. Rendered beside the rest so a gap in
   * evidence stays visible rather than being absorbed into `actionable`.
   */
  undetermined: number;

  lastGeneratedAt?: string;
  plannerVersion?: string;
}

/** The generation scheduler's state. */
export interface PlannerStatus {
  enabled: boolean;
  plannerVersion: string;
  running: boolean;
  pending: boolean;
  lastRunAt?: string;
  lastGenerated: number;
  /** How much work duplicate suppression saved. */
  lastUnchanged: number;
  lastSkipped: number;
}

/** GET /plans */
export interface PlanListResponse {
  items: ChangePlan[];
  pagination: Pagination;
  summary: ChangePlanSummary;
}

/** GET /plans/container/{id} */
export interface PlanContainerResponse {
  containerId: string;
  /**
   * Absent when the container has no plan. That means NO CHANGE IS PROPOSED,
   * not that a change was judged safe — the UI must render the two differently.
   */
  current?: ChangePlan;
  history: ChangePlan[];
  pagination: Pagination;
}

/** POST /plans/generate */
export interface PlanGenerateAccepted {
  requested: boolean;
  planner: PlannerStatus;
}

/** The listing filters. Every value is a closed vocabulary. */
export interface PlanQuery {
  page?: number;
  pageSize?: number;
  band?: RiskBand[];
  recommendation?: Recommendation[];
  update?: UpdateType[];
  currentOnly?: boolean;
  minRisk?: number;
  sort?: "band" | "risk" | "recommendation" | "generatedAt" | "container" | "update" | "id";
  order?: "asc" | "desc";
}

/** Bands from least to most risky, which is the order controls offer them in. */
export const RISK_BAND_ORDER: readonly RiskBand[] = [
  "veryLow",
  "low",
  "medium",
  "high",
  "critical",
] as const;

/** Human labels. The wire values are identifiers, not prose. */
export const RISK_BAND_LABELS: Record<RiskBand, string> = {
  veryLow: "very low",
  low: "low",
  medium: "medium",
  high: "high",
  critical: "critical",
};

export const RECOMMENDATION_ORDER: readonly Recommendation[] = [
  "proceed",
  "proceedWithCaution",
  "manualReview",
  "notRecommended",
  "unknown",
] as const;

export const RECOMMENDATION_LABELS: Record<Recommendation, string> = {
  proceed: "Proceed",
  proceedWithCaution: "Proceed with caution",
  manualReview: "Manual review",
  notRecommended: "Not recommended",
  unknown: "Cannot advise",
};

/**
 * What each recommendation actually means, for the tooltip.
 *
 * `unknown` says what it is NOT, because that is the misreading worth
 * preventing.
 */
export const RECOMMENDATION_MEANING: Record<Recommendation, string> = {
  proceed: "Nothing in the available evidence argues against this change",
  proceedWithCaution: "Probably fine, but something is worth reading first",
  manualReview: "A person should look at this before acting",
  notRecommended: "The evidence argues against this change as things stand",
  unknown:
    "HarborMaster does not know enough to advise. This is NOT the same as safe — something could not be checked",
};

/** Human labels for the rules, so the reasoning reads as prose. */
export const PLAN_RULE_LABELS: Record<PlanRule, string> = {
  updateClassification: "Size of the change",
  mutableTag: "Mutable tag",
  unknownDigest: "Digest comparison",
  registryQuality: "Registry evidence",
  imageAge: "Image age",
  platform: "Platform support",
  snapshotAvailable: "Configuration snapshot",
  restoreReadiness: "Restore readiness",
  activeDrift: "Configuration drift",
  policyViolations: "Policy compliance",
  blastRadius: "Blast radius",
};
