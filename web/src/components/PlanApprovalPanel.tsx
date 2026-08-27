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
 * "Manual review required" — and the one control that answers it.
 *
 * # Why this panel exists at all
 *
 * A plan whose recommendation is `manualReview` used to be a dead end: it could
 * be downloaded and never applied, because HarborMaster had no way to record
 * that the review it asked for had happened. The refusal said so in as many
 * words and offered no remedy.
 *
 * # What the button does, and what it does not
 *
 * It records that a person reviewed THIS exact plan. It does not change the
 * container, does not download anything, and does not alter the plan: the risk
 * score, the factors and the recommendation stay exactly as the planner wrote
 * them.
 *
 * That is why the wording never says Force, Override or Ignore. The operator is
 * not overruling HarborMaster — they are supplying the single thing it asked
 * for, and every other safety check still runs before anything is replaced.
 */
export function PlanApprovalPanel({
  plan,
  onChanged,
}: {
  plan: ChangePlan;
  /** Called after an approval or withdrawal, so the page can refresh. */
  onChanged?: () => void;
}) {
  const session = useSession();
  const mayApprove = Boolean(
    session.user?.permissions.includes("plan:approve"),
  );

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
          message:
            "HarborMaster could not check whether this plan has been reviewed.",
        });
      });
  }, [plan.planId]);

  useEffect(load, [load]);

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

  // Only a plan that ASKS for review has anything to approve. Offering the
  // control anywhere else would imply the others need one.
  if (plan.risk.recommendation !== "manualReview") return null;

  const approved = state.status === "known" && state.data.valid;

  return (
    <section
      aria-labelledby="plan-approval-heading"
      className="space-y-3 rounded-lg border border-warn/40 bg-warn-soft px-3 py-3"
    >
      <h3 id="plan-approval-heading" className="text-sm font-semibold">
        Manual review required
      </h3>
      <p className="max-w-prose text-sm">
        HarborMaster found this update but will not replace the running container
        until an authorised person reviews this exact plan.
      </p>

      <div aria-live="polite" className="space-y-2">
        {state.status === "loading" ? (
          <p className="text-sm text-content-muted">Checking for a review…</p>
        ) : null}

        {state.status === "error" ? (
          <p className="text-sm text-danger">{state.message}</p>
        ) : null}

        {state.status === "known" && !state.data.valid ? (
          <p className="text-sm text-content-muted" data-testid="approval-stale">
            <strong>
              {state.data.refusal
                ? PLAN_APPROVAL_REFUSAL_LABELS[state.data.refusal]
                : "This approval no longer applies"}
            </strong>
            {state.data.explanation ? ` — ${state.data.explanation}` : null}
          </p>
        ) : null}

        {approved && state.status === "known" ? (
          <div className="space-y-1 text-sm" data-testid="approval-granted">
            <p className="font-medium">
              {approvalSummary(state.data.approval)}{" "}
              <time dateTime={state.data.approval.approvedAt}>
                {new Date(state.data.approval.approvedAt).toLocaleString()}
              </time>
            </p>
            <p className="text-content-muted">
              This approval applies only to this exact plan. HarborMaster will
              still run every normal safety check before changing the container.
            </p>
          </div>
        ) : null}
      </div>

      {mayApprove ? (
        <div className="flex flex-wrap gap-2">
          {approved ? (
            <button
              type="button"
              className="min-h-11 rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs"
              disabled={busy}
              onClick={() => void act("revoke")}
            >
              {busy ? "Withdrawing…" : "Withdraw approval"}
            </button>
          ) : (
            <button
              type="button"
              className="min-h-11 rounded-lg border border-warn/40 bg-surface px-2.5 py-1 text-xs font-medium text-warn"
              disabled={busy || state.status === "loading"}
              onClick={() => void act("approve")}
            >
              {busy ? "Recording…" : "Approve this exact update"}
            </button>
          )}
        </div>
      ) : (
        <p className="text-xs text-content-muted">
          Reviewing an update needs the plan approval permission.
        </p>
      )}
    </section>
  );
}
