import type { UpdatePolicyRequest } from "./automationTypes";
import { AUTOMATION_REASON_LABELS, type AutomationReason } from "./automationTypes";

/**
 * Automation readiness: what a policy could do against the estate right now.
 *
 * # What these numbers are, and what they are not
 *
 * Every count comes from decisions the real decision function and the real
 * dependency gate produced on the server. There is no eligibility rule in this
 * file, and there must never be one: a frontend approximation would drift from
 * the engine, and the operator would be reading a number automation does not
 * honour.
 *
 * So this module carries types, a request builder, and wording. It does not
 * decide anything.
 *
 * # An observation, never a promise
 *
 * `eligible` describes the estate at `evaluatedAt`. A container can stop being
 * eligible a second later -- an image is republished, a pass takes it, someone
 * pauses it. Every sentence rendered from this must be phrased as a reading
 * rather than as a guarantee, which is why the panel says "based on current
 * assessment" and never "these will update".
 */

/** One reason, and how many of this policy's containers carry it. */
export interface AutomationReadinessGroup {
  reason: AutomationReason;
  /** HarborMaster's own sentence. Rendered when no label maps the reason. */
  explanation: string;
  count: number;
  /** A bounded sample, not the whole set. */
  containers?: string[];
}

/** What one policy could do against the estate as currently assessed. */
export interface AutomationReadinessReport {
  evaluatedAt: string;
  /** The estate was cut at the target bound, so the counts describe a prefix. */
  truncated: boolean;

  considered: number;
  governed: number;

  eligible: number;
  observing: number;
  awaitingApproval: number;

  groups: AutomationReadinessGroup[];
}

export interface AutomationReadinessResponse {
  readiness: AutomationReadinessReport;
  engineEnabled: boolean;
}

/**
 * The request body.
 *
 * The policy configuration, plus the identifier of the stored policy being
 * edited when there is one. Nothing else: a recommendation, a verdict, an
 * update type, an eligibility, a dependency state, a snapshot status and a risk
 * score are all facts the server establishes, and the type has nowhere to put
 * one.
 */
export type AutomationReadinessRequest = UpdatePolicyRequest & {
  policyId?: string;
};

/**
 * The label for a group, from the existing closed vocabulary.
 *
 * Falls back to the server's own sentence rather than to the raw enum: a
 * reason this build does not know about is still something HarborMaster can
 * describe, and `dependencyWaiting` must never reach the DOM.
 */
export function readinessGroupLabel(group: AutomationReadinessGroup): string {
  return AUTOMATION_REASON_LABELS[group.reason] ?? group.explanation;
}

/**
 * The headline sentence.
 *
 * Deliberately about the present tense and the current assessment. "Will
 * update" would be a promise about a future pass, which depends on evidence
 * that has not been gathered yet.
 */
export function readinessHeadline(
  report: AutomationReadinessReport,
  engineEnabled: boolean,
): string {
  if (report.governed === 0) {
    return "This policy does not currently govern any container.";
  }
  if (report.eligible === 0) {
    return "No container is currently eligible under this policy.";
  }
  const containers =
    report.eligible === 1 ? "1 container is" : `${report.eligible} containers are`;
  if (!engineEnabled) {
    return `${containers} currently eligible, but the update engine is switched off.`;
  }
  return `${containers} currently eligible.`;
}
