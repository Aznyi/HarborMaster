import type { Acquisition } from "./acquisitionTypes";
import type { AutomationDecision } from "./automationTypes";
import { AUTOMATION_REASON_LABELS } from "./automationTypes";
import type { ChangePlan } from "./planTypes";
import type { BadgeTone } from "../components/StatusBadge";

/**
 * The Updates workspace's view model.
 *
 * # Why this is a separate, pure module
 *
 * The workspace joins three server reads -- change plans, what the next
 * automation pass would do, and any download already under way -- and the
 * joining is where a consolidation goes wrong. Keeping it here means the
 * mapping is testable without mounting a page, and means the components below
 * it render a decision rather than making one.
 *
 * # What it does NOT do
 *
 * It invents no state. Every label is derived from a value the server sent, and
 * the recommendation, the update classification and the automation reason keep
 * the meanings they have everywhere else in the application. Where a label is
 * shortened for the list, the full server vocabulary is still shown in the
 * row's details.
 */

/** What HarborMaster makes of the change, for somebody deciding. */
export type AssessmentKind = "ready" | "review" | "against" | "unknown";

export interface Assessment {
  kind: AssessmentKind;
  label: string;
  tone: BadgeTone;
  /** The planner's own sentence. Never replaced, only supplemented. */
  summary: string;
}

/**
 * Reads the assessment off the plan.
 *
 * A plan with no proposed target is `unknown` regardless of its risk band. It
 * proposes nothing, so there is nothing to be ready or unready about -- and its
 * band is scored against a change that does not exist.
 */
export function assessmentOf(plan: ChangePlan): Assessment {
  const summary = plan.risk?.summary ?? "";

  if (!plan.proposedImage || !plan.proposedDigest) {
    return {
      kind: "unknown",
      label: "Cannot advise",
      tone: "neutral",
      summary:
        summary ||
        "No target was proposed, so there is nothing to move onto yet.",
    };
  }

  switch (plan.risk?.recommendation) {
    case "proceed":
      return { kind: "ready", label: "Ready", tone: "ok", summary };
    case "proceedWithCaution":
      return { kind: "ready", label: "Ready — read first", tone: "warn", summary };
    case "manualReview":
      return { kind: "review", label: "Review required", tone: "warn", summary };
    case "notRecommended":
      return { kind: "against", label: "Not recommended", tone: "danger", summary };
    default:
      return { kind: "unknown", label: "Cannot advise", tone: "neutral", summary };
  }
}

/** What automation will do about this container, if anything. */
export interface AutomationContext {
  label: string;
  /** The engine's own explanation, when it gave one. */
  detail?: string;
  /** True when the operator can expect this to happen without them. */
  handsOff: boolean;
}

/**
 * Reads the automation context off the decision the engine already published.
 *
 * `/automation/upcoming` is what the next pass WOULD do, evaluated now. Using it
 * means this page never forms a second opinion about eligibility: if it says a
 * container is skipped for a closed window, that is the engine's answer and not
 * a guess made here.
 */
export function automationOf(decision?: AutomationDecision): AutomationContext {
  if (!decision) {
    return {
      label: "Manual",
      detail: "No automation policy covers this container.",
      handsOff: false,
    };
  }

  const detail = decision.detail || AUTOMATION_REASON_LABELS[decision.reason];

  if (decision.verdict === "update" || decision.verdict === "wouldUpdate") {
    return {
      label: "Automatic",
      detail: "The next automation pass will apply this.",
      handsOff: true,
    };
  }
  if (decision.verdict === "awaitingApproval") {
    return { label: "Needs approval", detail, handsOff: false };
  }

  // A skip. The REASON is what an operator needs, because the remedies differ
  // completely -- a paused container needs resuming, a closed window needs
  // waiting, an unselected one needs a policy.
  switch (decision.reason) {
    case "automationPaused":
    case "labelPaused":
      return { label: "Paused", detail, handsOff: false };
    case "notSelected":
    case "noPolicy":
    case "policyDisabled":
    case "labelDisabled":
    case "selfUpdate":
      return { label: "Not automated", detail, handsOff: false };
    case "observeMode":
      return { label: "Observing", detail, handsOff: false };
    // Held behind something else, which Phase 5's container page already
    // named this way. The two surfaces describe one state, so they say one
    // thing: an operator moving between them must not think they are looking
    // at different subsystems.
    case "dependencyWaiting":
    case "dependencyBlocked":
      return { label: "Waiting on a dependency", detail, handsOff: true };
    case "windowClosed":
      return { label: "Outside window", detail, handsOff: true };
    case "alreadyInFlight":
      return { label: "In progress", detail, handsOff: true };
    default:
      return { label: "Manual", detail, handsOff: false };
  }
}

/** One container's row: everything three reads say about the same update. */
export interface UpdateRowModel {
  plan: ChangePlan;
  decision?: AutomationDecision;
  /** The most recent download for THIS plan, when one exists. */
  acquisition?: Acquisition;
  assessment: Assessment;
  automation: AutomationContext;
}

/**
 * Joins the three reads into one row per container.
 *
 * Superseded plans are dropped: the planner writes a new row rather than
 * editing one, so the list would otherwise show the same container several
 * times with stale proposals.
 */
export function buildUpdateRows(
  plans: readonly ChangePlan[],
  decisions: readonly AutomationDecision[],
  acquisitions: readonly Acquisition[],
): UpdateRowModel[] {
  const byContainer = new Map<string, ChangePlan>();
  for (const plan of plans) {
    if (plan.superseded) continue;
    const seen = byContainer.get(plan.containerId);
    // Newest wins, matching the server's own "current plan" rule.
    if (!seen || plan.generatedAt > seen.generatedAt) {
      byContainer.set(plan.containerId, plan);
    }
  }

  const decisionFor = new Map<string, AutomationDecision>();
  for (const decision of decisions) {
    if (decision.containerId) decisionFor.set(decision.containerId, decision);
  }

  // Keyed by PLAN, not by container: a download authorised by an earlier plan
  // must not be offered as the next step for a newer one.
  const acquisitionFor = new Map<string, Acquisition>();
  for (const acquisition of acquisitions) {
    const seen = acquisitionFor.get(acquisition.planId);
    if (!seen || acquisition.acquisitionId > seen.acquisitionId) {
      acquisitionFor.set(acquisition.planId, acquisition);
    }
  }

  return [...byContainer.values()]
    .map((plan) => ({
      plan,
      decision: decisionFor.get(plan.containerId),
      acquisition: acquisitionFor.get(plan.planId),
      assessment: assessmentOf(plan),
      automation: automationOf(decisionFor.get(plan.containerId)),
    }))
    .sort((a, b) => a.plan.containerName.localeCompare(b.plan.containerName));
}

/** The counts the header reports. */
export interface UpdateSummary {
  available: number;
  ready: number;
  needsReview: number;
  undetermined: number;
}

export function summarise(rows: readonly UpdateRowModel[]): UpdateSummary {
  let ready = 0;
  let needsReview = 0;
  let undetermined = 0;

  for (const row of rows) {
    switch (row.assessment.kind) {
      case "ready":
        ready += 1;
        break;
      case "review":
        needsReview += 1;
        break;
      case "unknown":
        undetermined += 1;
        break;
      default:
        break;
    }
  }

  // "Available" is what proposes a move somewhere, which is what an operator
  // means by the word. A plan that could not be assessed proposes nothing.
  const available = rows.filter(
    (row) => row.assessment.kind !== "unknown",
  ).length;

  return { available, ready, needsReview, undetermined };
}

/** Which rows a tab shows. */
export type UpdateTab = "available" | "review" | "all";

export function filterRows(
  rows: readonly UpdateRowModel[],
  tab: UpdateTab,
): UpdateRowModel[] {
  switch (tab) {
    case "available":
      // Actionable now: it proposes something and is not waiting on a person.
      return rows.filter(
        (row) => row.assessment.kind === "ready" || row.assessment.kind === "against",
      );
    case "review":
      return rows.filter((row) => row.assessment.kind === "review");
    case "all":
      return [...rows];
  }
}
