/**
 * First-run and engine state, composed from facts the server already
 * established.
 *
 * # What this file may decide, and what it may not
 *
 * It answers exactly one question:
 *
 *     which onboarding state should the operator be shown?
 *
 * It does NOT answer:
 *
 *     is this container eligible to update?
 *
 * That is the backend's, and every number here comes from a server response:
 * capability flags from the health report, policy counts from the automation
 * status, assessment progress from the planner, and eligibility from the
 * automation preview. Nothing is recomputed.
 *
 * # Why the projection is duplicated at all
 *
 * `internal/domain/first_run.go` holds the same state machine. Two
 * implementations is a drift risk, and the mitigation is a contract test:
 * `firstRun.contract.test.ts` enumerates the Go states from the Go source and
 * fails if this file does not represent every one of them, with the same
 * precedence.
 *
 * The alternative -- a backend endpoint that composed four existing reads --
 * would add a route and a service whose only job is to concatenate responses
 * the client already has. The contract test buys the same safety for less.
 */

/**
 * The states, in the Go file's order.
 *
 * This is a copy of `domain.FirstRunState`. It is checked against the Go source
 * rather than trusted.
 */
export type FirstRunState =
  | "inventoryPending"
  | "assessmentPending"
  | "assessmentUnavailable"
  | "engineDisabled"
  | "noPolicy"
  | "observeOnly"
  | "nothingEligible"
  | "active"
  | "needsAttention"
  | "unknown";

/** The facts the projection reads. Mirrors `domain.FirstRunFacts`. */
export interface FirstRunFacts {
  /** Capability flags, from /health. */
  features: {
    planner: boolean;
    automation: boolean;
  } | null;

  /** Whether HarborMaster knows what is running. */
  inventoryEstablished: boolean;
  /** Whether the planner has completed at least one pass, from /plans. */
  assessed: boolean;

  policies: number;
  actingPolicies: number;

  pausedContainers: number;
  manualReviews: number;

  eligible: number;
  /**
   * Whether the eligibility answer could be established at all.
   *
   * The pair matters more than the number: a preview request that failed must
   * never be rendered as "no containers are eligible". Those are opposite
   * messages, and the wrong one sends an operator to rewrite a policy that was
   * never the problem.
   */
  readinessKnown: boolean;
}

/**
 * Project the facts onto one state.
 *
 * The order of the checks IS the semantics, and it is the same order as
 * `domain.DescribeFirstRun`. Each question makes the ones below it meaningless:
 * there is no point saying no container is eligible when nothing has been
 * assessed, or that no policy exists when the engine that would run one is off.
 */
export function describeFirstRun(facts: FirstRunFacts): FirstRunState {
  // A missing capability report is a fact we do not have, not a disabled
  // feature. Guessing either way would be worse than saying so.
  if (!facts.features) return "unknown";

  if (!facts.inventoryEstablished) return "inventoryPending";

  if (!facts.features.planner) return "assessmentUnavailable";
  if (!facts.assessed) return "assessmentPending";

  if (!facts.features.automation) return "engineDisabled";

  if (facts.policies === 0) return "noPolicy";
  if (facts.actingPolicies === 0) return "observeOnly";

  if (facts.pausedContainers > 0 || facts.manualReviews > 0) return "needsAttention";

  if (!facts.readinessKnown) return "unknown";
  if (facts.eligible === 0) return "nothingEligible";
  return "active";
}

/** The heading an operator reads first. */
export const FIRST_RUN_HEADINGS: Record<FirstRunState, string> = {
  inventoryPending: "HarborMaster is starting up",
  assessmentPending: "HarborMaster is assessing your containers",
  assessmentUnavailable: "Update assessment is switched off",
  engineDisabled: "Automatic updates are disabled",
  noPolicy: "Choose how HarborMaster should handle updates",
  observeOnly: "HarborMaster is observing updates",
  nothingEligible: "Automatic updates are configured",
  active: "Automatic updates are active",
  needsAttention: "Automatic updates need your attention",
  unknown: "HarborMaster could not establish current automation readiness",
};

/**
 * What is true, in one sentence.
 *
 * Present tense, and never a promise: "N containers are currently eligible"
 * describes the estate as assessed now, and the count can change a second
 * later. The word "will" appears only about safety checks, which do always run.
 */
export function firstRunExplanation(
  state: FirstRunState,
  facts: FirstRunFacts,
): string {
  switch (state) {
    case "inventoryPending":
      return "HarborMaster is still establishing what is running on this host. No action is required.";
    case "assessmentPending":
      return (
        "Inventory is available, but HarborMaster has not finished its first " +
        "update assessment yet. The planner runs automatically after an " +
        "inventory refresh, so no action is required."
      );
    case "assessmentUnavailable":
      return (
        "Change planning is switched off in this deployment, so HarborMaster " +
        "is not assessing containers for updates."
      );
    case "engineDisabled":
      return (
        "HarborMaster can inventory containers and evaluate update policies, " +
        "but it will not automatically recreate containers while the " +
        "automation engine is disabled."
      );
    case "noPolicy":
      return (
        "The update engine is running, and no policy tells it what to do yet."
      );
    case "observeOnly":
      return (
        "Your policies are evaluating updates but will not automatically " +
        "change containers."
      );
    case "nothingEligible":
      return (
        "Based on the current assessment, no containers are currently " +
        "eligible for automatic update."
      );
    case "active": {
      const containers =
        facts.eligible === 1 ? "1 container is" : `${facts.eligible} containers are`;
      return (
        `Based on the current assessment, ${containers} currently eligible ` +
        "under your automatic update policies. HarborMaster will still run " +
        "its normal safety checks before each update."
      );
    }
    case "needsAttention":
      return (
        "Automatic updates are configured, and something is waiting for a person."
      );
    case "unknown":
      return (
        "One of the checks behind this page did not answer, so HarborMaster " +
        "cannot say what it would do right now. This is not the same as " +
        "having nothing to do."
      );
  }
}

/**
 * What an unattended update needs from the DEPLOYMENT, when the server has not
 * said.
 *
 * The server names the set on `AutomationStatus.requiredCapabilities`, but it
 * names nothing when no policy is set to act -- nothing is required to do
 * nothing. The onboarding panel still has to answer "what would automation
 * need here", and this is that answer.
 *
 * # Why rollback is in it
 *
 * HarborMaster REFUSES TO START with the automation engine enabled and
 * rollback disabled, whatever any policy asks for. This list is printed to an
 * operator as environment variables to apply, so a list without rollback is a
 * set that stops the process booting -- onboarding instructions that take the
 * installation down.
 *
 * This mirrors `domain.RequiredForAutomation()` in Go, and an architecture test
 * fails the build if the two stop agreeing.
 */
export const REQUIRED_FOR_AUTOMATION: readonly string[] = [
  "acquisition",
  "execution",
  "automation",
  "rollback",
];

/**
 * Whether this state is waiting on the OPERATOR.
 *
 * Mirrors `FirstRunState.NeedsSetup`. Used to decide whether the Dashboard
 * spends attention on it: an item telling somebody to act while HarborMaster is
 * still starting up asks them to fix something that is not broken.
 */
export function firstRunNeedsSetup(state: FirstRunState): boolean {
  return state === "engineDisabled" || state === "noPolicy";
}
