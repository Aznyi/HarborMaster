import type { Requester } from "./automationTypes";

/**
 * Plan approval: the record that a person reviewed one immutable change plan.
 *
 * # What an approval is, in the operator's terms
 *
 * The planner sometimes says a change needs a person to look at it. Until now
 * that was the end of the road: the update could be downloaded and never
 * applied, because nothing could record that the looking had happened.
 *
 * An approval records it. It does NOT lower the risk score, remove a factor, or
 * change the recommendation — the plan stays exactly as the planner wrote it,
 * and the approval sits next to it. That is what keeps the audit trail honest:
 * "a person approved this exact digest" and "the model thought it was fine" stay
 * separately answerable.
 *
 * # It approves; it does not apply
 *
 * Approving changes no container. The operator then drives the ordinary
 * download and recreation, and every safety check runs again — snapshot
 * assurance, dependency ordering, digest pinning, policy compliance,
 * preservation, verification, and the refusal to update HarborMaster itself.
 *
 * So the wording here never says "force", "override" or "ignore". The operator
 * is not overruling HarborMaster; they are supplying the one thing it asked for.
 */

/** What an approval currently is. */
export type PlanApprovalState = "active" | "revoked";

/** Why an approval does not authorise its plan. */
export type PlanApprovalRefusal =
  | "notApprovable"
  | "superseded"
  | "revoked"
  | "evidenceChanged"
  | "alreadyActed";

export interface PlanApproval {
  planId: string;
  state: PlanApprovalState;
  approvedBy?: Requester;
  approvedAt: string;
  revokedBy?: Requester;
  revokedAt?: string;
}

export interface PlanApprovalResponse {
  approval: PlanApproval;
  /** Whether it currently authorises the plan. */
  valid: boolean;
  refusal?: PlanApprovalRefusal;
  /** HarborMaster's own sentence for the refusal. */
  explanation?: string;
}

/**
 * The label for a refusal.
 *
 * A raw enum must never reach the DOM. The server also sends its own sentence,
 * which is rendered alongside — this is the short form for a heading.
 */
export const PLAN_APPROVAL_REFUSAL_LABELS: Record<PlanApprovalRefusal, string> = {
  notApprovable: "Nothing to approve",
  superseded: "A newer plan replaced this one",
  revoked: "Approval withdrawn",
  evidenceChanged: "The evidence changed",
  alreadyActed: "Already applied",
};

/**
 * The sentence shown once a plan is approved.
 *
 * Says what the approval covers and, just as importantly, what it does not:
 * every check still runs. An operator who reads this should not expect the
 * container to change until they ask for it.
 */
export function approvalSummary(approval: PlanApproval): string {
  const who = approval.approvedBy?.username ?? "an unrecorded account";
  return `Approved by ${who}`;
}
