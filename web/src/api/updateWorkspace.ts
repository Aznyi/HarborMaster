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

/**
 * What HarborMaster makes of the change, for somebody deciding.
 *
 * # Why three kinds of "no verdict" rather than one
 *
 * They have different remedies, and collapsing them told an operator to go and
 * investigate a hundred containers about which there was nothing to
 * investigate.
 *
 *   - `untracked`  -- the reference can never be looked up. Nothing to do, ever.
 *   - `unchecked`  -- no lookup has happened yet. Wait; it resolves itself.
 *   - `unknown`    -- a lookup happened and no reliable conclusion came out of
 *                     it. This is the one that may deserve a person.
 *
 * The split is read off `plan.registryStatus`, a required field with a closed
 * server-side vocabulary. Nothing here parses an image string or guesses from a
 * tag name.
 */
export type AssessmentKind =
  | "ready"
  | "review"
  | "against"
  | "untracked"
  | "unchecked"
  | "unknown";

export interface Assessment {
  kind: AssessmentKind;
  label: string;
  tone: BadgeTone;
  /** The planner's own sentence. Never replaced, only supplemented. */
  summary: string;
}

/**
 * Why HarborMaster reached no verdict, when it reached none.
 *
 * Read from `registryStatus`, which the server sets from the outcome of the
 * most recent lookup and which the risk model already treats as decisive: both
 * `unsupported` and `pending` contribute an unknown-severity factor, and an
 * unknown-severity factor forces the recommendation to `unknown`. So a plan in
 * either state can never carry a real verdict, and reading the reason off it
 * cannot override one.
 *
 * Every other status -- `ok`, `failed`, `rateLimited`, `unauthorized`,
 * `notFound` -- means a lookup was attempted and either succeeded without
 * settling the question or did not succeed. All of those are "cannot
 * determine", because the operator's next move is the same for all of them:
 * look, or wait and look again.
 */
function undeterminedAssessment(plan: ChangePlan, summary: string): Assessment {
  switch (plan.registryStatus) {
    case "unsupported":
      return {
        kind: "untracked",
        label: "Not tracked",
        tone: "neutral",
        summary:
          summary ||
          "This image reference names no registry that can be looked up, so " +
            "no update will ever be found for it.",
      };
    case "pending":
      return {
        kind: "unchecked",
        label: "Not checked yet",
        tone: "neutral",
        summary:
          summary ||
          "This image has not been looked up yet. The check runs on its own " +
            "schedule; nothing is required.",
      };
    default:
      return {
        kind: "unknown",
        label: "Cannot determine",
        tone: "neutral",
        summary:
          summary ||
          "HarborMaster looked and could not reach a reliable conclusion. " +
            "This is the absence of a verdict, not a mild one.",
      };
  }
}

/**
 * Reads the assessment off the plan.
 *
 * A plan with no proposed target has no verdict regardless of its risk band. It
 * proposes nothing, so there is nothing to be ready or unready about -- and its
 * band is scored against a change that does not exist. WHY there is no target
 * is then read from the registry status, because "there is nothing to find" and
 * "we could not find out" are different answers.
 */
export function assessmentOf(plan: ChangePlan): Assessment {
  const summary = plan.risk?.summary ?? "";

  if (!plan.proposedImage || !plan.proposedDigest) {
    return undeterminedAssessment(
      plan,
      summary || "No target was proposed, so there is nothing to move onto yet.",
    );
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
      // Only reached when the planner declined to advise, so the reason is
      // read here too rather than being flattened into one grey label.
      return undeterminedAssessment(plan, summary);
  }
}

/** The kinds that carry no verdict, and so offer nothing to apply. */
export const UNDETERMINED_KINDS = [
  "untracked",
  "unchecked",
  "unknown",
] as const satisfies readonly AssessmentKind[];

/** True when the row proposes nothing an operator could act on. */
export function isUndetermined(kind: AssessmentKind): boolean {
  return (UNDETERMINED_KINDS as readonly AssessmentKind[]).includes(kind);
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

/**
 * The counts the header reports.
 *
 * # The invariant
 *
 * `ready + needsReview + notRecommended + untracked + unchecked + undetermined`
 * equals the number of rows, exactly. Every row lands in one bucket and no row
 * lands in two, so the cards can be read as a partition rather than as six
 * overlapping filters. `available` is a DERIVED total over the first three and
 * is the only figure that double-counts anything, which is why it is named for
 * what it is rather than sitting among the others as a seventh state.
 */
export interface UpdateSummary {
  /** Rows that propose a move somewhere. The sum of the first three below. */
  available: number;

  ready: number;
  needsReview: number;
  notRecommended: number;

  /** No verdict, and no lookup is possible. */
  untracked: number;
  /** No verdict yet, because nothing has been looked up. */
  unchecked: number;
  /** No verdict, after a lookup that settled nothing. */
  undetermined: number;
}

export function summarise(rows: readonly UpdateRowModel[]): UpdateSummary {
  const counts: Record<AssessmentKind, number> = {
    ready: 0,
    review: 0,
    against: 0,
    untracked: 0,
    unchecked: 0,
    unknown: 0,
  };

  for (const row of rows) counts[row.assessment.kind] += 1;

  return {
    // "Available" is what proposes a move somewhere, which is what an operator
    // means by the word. A row with no verdict proposes nothing, and none of
    // the three no-verdict kinds is folded in here.
    available: counts.ready + counts.review + counts.against,
    ready: counts.ready,
    needsReview: counts.review,
    notRecommended: counts.against,
    untracked: counts.untracked,
    unchecked: counts.unchecked,
    undetermined: counts.unknown,
  };
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
      // The tab's badge counts exactly this set -- see `availableTabCount`.
      return rows.filter(
        (row) => row.assessment.kind === "ready" || row.assessment.kind === "against",
      );
    case "review":
      return rows.filter((row) => row.assessment.kind === "review");
    case "all":
      return [...rows];
  }
}

/**
 * What the "Available" tab's badge says.
 *
 * The badge used to report `ready` while the tab itself also listed
 * `notRecommended` rows, so a tab labelled 3 could open onto five rows. One
 * function now answers both questions.
 */
export function availableTabCount(summary: UpdateSummary): number {
  return summary.ready + summary.notRecommended;
}
