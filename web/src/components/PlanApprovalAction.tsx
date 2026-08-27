import { useCallback, useEffect, useState } from "react";

import {
  PLAN_APPROVAL_REFUSAL_LABELS,
  approvalSummary,
  type PlanApprovalResponse,
} from "../api/planApproval";
import type { ChangePlan } from "../api/planTypes";
import { approvePlan, getPlanApproval, revokePlanApproval } from "../api/client";
import { useSession } from "../hooks/useSession";

/**
 * The one control that answers "manual review required".
 *
 * # Why this lives on the review list and nowhere else
 *
 * It used to be a panel on the container's plan page, one click below the list
 * named "Update reviews". So the page named for the act could not perform it,
 * and an operator could be routed to three different screens for one held
 * update -- only one of which could do anything.
 *
 * There is exactly one place to approve now, and it is the row you are already
 * reading the verdict on.
 *
 * # What the button does, and what it does not
 *
 * It records that a person reviewed THIS exact plan. It does not change the
 * container, does not download anything, and does not alter the plan: the risk
 * score, the factors and the recommendation stay exactly as the planner wrote
 * them.
 *
 * That is why the wording never says Force, Override or Ignore. The operator is
 * not overruling HarborMaster -- they are supplying the single thing it asked
 * for, and every other safety check still runs before anything is replaced.
 */
export function PlanApprovalAction({
  plan,
  onChanged,
  onApprovalKnown,
}: {
  plan: ChangePlan;
  /** Called after an approval or withdrawal, for a caller that wants to refresh. */
  onChanged?: () => void;
  /**
   * Reports whether a VALID approval stands, whenever that becomes known.
   *
   * The Updates workspace needs it to decide whether to offer the next step.
   * It cannot read the recommendation instead: approving deliberately does not
   * change the assessment, so a reviewed plan still says `manualReview` and
   * would otherwise be offered the review control forever.
   */
  onApprovalKnown?: (approved: boolean) => void;
}) {
  const session = useSession();
  const mayApprove = Boolean(session.user?.permissions.includes("plan:approve"));

  // Only a plan that ASKS for review has anything to approve. Read before the
  // effect, because it also decides whether to make the request at all: this
  // renders inside a list, and asking the server about every settled plan on
  // the page would be a request per row for an answer that cannot matter.
  const needsReview = plan.risk.recommendation === "manualReview";

  const [state, setState] = useState<
    | { status: "loading" }
    | { status: "none" }
    | { status: "known"; data: PlanApprovalResponse }
    | { status: "error"; message: string }
  >({ status: "loading" });
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    getPlanApproval(plan.planId)
      .then((data) => setState({ status: "known", data }))
      // A 404 is the ordinary state of a plan nobody has reviewed yet, not an
      // error. Anything else is reported rather than rendered as "not approved",
      // because those are opposite messages.
      .catch((error: unknown) => {
        const status = (error as { status?: number } | null)?.status;
        if (status === 404) {
          setState({ status: "none" });
          return;
        }
        setState({
          status: "error",
          message: "HarborMaster could not check whether this plan has been reviewed.",
        });
      });
  }, [plan.planId]);

  useEffect(() => {
    if (!needsReview) return;
    load();
  }, [needsReview, load]);

  const approved = state.status === "known" && state.data.valid;

  // Reported from an effect rather than during render: a caller that stores it
  // would otherwise be set during this component's render pass.
  useEffect(() => {
    if (state.status === "loading") return;
    onApprovalKnown?.(approved);
  }, [approved, onApprovalKnown, state.status]);

  const act = useCallback(
    async (what: "approve" | "revoke") => {
      setBusy(true);
      try {
        if (what === "approve") {
          setState({ status: "known", data: await approvePlan(plan.planId) });
        } else {
          await revokePlanApproval(plan.planId);
          setState({ status: "none" });
        }
        onChanged?.();
      } catch {
        setState({
          status: "error",
          message:
            what === "approve"
              ? "HarborMaster could not record the approval. Nothing was changed."
              : "HarborMaster could not withdraw the approval.",
        });
      } finally {
        setBusy(false);
      }
    },
    [onChanged, plan.planId],
  );

  if (!needsReview) return null;

  return (
    <div
      className="flex shrink-0 flex-col items-end gap-1 text-right"
      data-testid="plan-approval-action"
    >
      <div aria-live="polite" className="text-xs">
        {state.status === "loading" ? (
          <span className="text-content-muted">Checking for a review…</span>
        ) : null}

        {state.status === "error" ? (
          <span className="text-danger">{state.message}</span>
        ) : null}

        {state.status === "known" && !state.data.valid ? (
          <span className="text-content-muted" data-testid="approval-stale">
            <strong>
              {state.data.refusal
                ? PLAN_APPROVAL_REFUSAL_LABELS[state.data.refusal]
                : "This approval no longer applies"}
            </strong>
            {state.data.explanation ? ` — ${state.data.explanation}` : null}
          </span>
        ) : null}

        {approved && state.status === "known" ? (
          <span className="text-content-muted" data-testid="approval-granted">
            {approvalSummary(state.data.approval)}{" "}
            <time dateTime={state.data.approval.approvedAt}>
              {new Date(state.data.approval.approvedAt).toLocaleString()}
            </time>
            {/* Approving is not applying, and the compact form must still say
                so: an operator who reads "Approved" and nothing else has been
                told the update happened. */}
            <span className="block">
              Recorded. Every safety check still runs before the container
              changes.
            </span>
          </span>
        ) : null}
      </div>

      {mayApprove ? (
        approved ? (
          <button
            type="button"
            // Named per container: several rows can offer this at once, and
            // three buttons all called "Withdraw approval" are ambiguous to
            // anyone not looking at the screen.
            aria-label={`Withdraw approval for ${plan.containerName}`}
            className="min-h-11 rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs"
            disabled={busy}
            onClick={() => void act("revoke")}
          >
            {busy ? "Withdrawing…" : "Withdraw approval"}
          </button>
        ) : (
          <button
            type="button"
            aria-label={`Approve this exact update for ${plan.containerName}`}
            className="min-h-11 rounded-lg border border-warn/40 bg-surface px-2.5 py-1 text-xs font-medium text-warn"
            disabled={busy || state.status === "loading"}
            onClick={() => void act("approve")}
          >
            {busy ? "Recording…" : "Approve this exact update"}
          </button>
        )
      ) : (
        <span className="text-xs text-content-muted">
          Reviewing an update needs the plan approval permission.
        </span>
      )}
    </div>
  );
}
