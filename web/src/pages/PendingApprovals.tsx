import { useCallback, useState } from "react";
import { Link } from "react-router";

import type { AutomationDecision } from "../api/automationTypes";
import { useAutomationApprovals } from "../hooks/useAutomation";
// The same approval control the pass detail uses: it holds the two-step
// confirmation, the permission check and the error surface, and a second copy
// would be a second thing to keep correct.
import { ApproveButton as ApproveDecision } from "./AutomationRun";
import { ApprovalDependencyNote } from "../components/DependencyStatus";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import { ScrollArea } from "../components/DetailSection";
import { UpdateBadge } from "../components/ImageBadges";
import { RecommendationBadge } from "../components/PlanBadges";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";

const PAGE_SIZE = 25;

/**
 * The decisions waiting for a person.
 *
 * # Why this page exists
 *
 * A policy in `approvalRequired` mode records a decision and stops. Finding
 * those decisions used to mean opening the automation pass that produced them
 * and reading its table row by row -- and a pass records a row for every
 * container it considered, so two outstanding approvals sat among twenty
 * "skipped: no policy selects it". Nothing pointed at which pass, and the
 * dashboard's count linked only to the automation landing page.
 *
 * # What it does not do
 *
 * It does not approve anything itself. The control below is the same one the
 * pass detail uses, calling the same endpoint under the same permission, so
 * every check that guarded an approval still guards it: the decision must still
 * be awaiting approval, its change plan must still be the container's current
 * one, automation must not be paused for it, and the acquisition and recreation
 * preflights run afterwards exactly as before.
 *
 * Approval is permission to proceed through those checks. It is not permission
 * to skip them, and the copy here says so.
 */
export function PendingApprovals() {
  const [page, setPage] = useState(1);
  const approvals = useAutomationApprovals(page, PAGE_SIZE);

  const refresh = useCallback(() => {
    approvals.refresh();
  }, [approvals]);

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Waiting for approval"
        description="Updates an approval-required policy has decided on and is holding until a person releases them. Approving one sends it through the same checks an operator's own update would take; it does not skip any of them."
      />

      <ApprovalList approvals={approvals} onPage={setPage} onApproved={refresh} />
    </div>
  );
}

function ApprovalList({
  approvals,
  onPage,
  onApproved,
}: {
  approvals: ReturnType<typeof useAutomationApprovals>;
  onPage: (page: number) => void;
  onApproved: () => void;
}) {
  if (approvals.status === "loading") {
    return <LoadingState label="Loading approvals" />;
  }
  if (approvals.status === "disconnected") {
    return <DisconnectedState onRetry={approvals.refresh} />;
  }
  if (approvals.error) {
    return <ErrorState error={approvals.error} onRetry={approvals.refresh} />;
  }
  if (!approvals.data) {
    return <LoadingState label="Loading approvals" />;
  }

  const items = approvals.data.items;

  if (items.length === 0) {
    return (
      <EmptyState
        title="Nothing is waiting for you"
        description="No policy is holding an update for approval. A policy in Approval required mode adds one here when it finds a change it will not make on its own."
      />
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="rounded-xl border border-border-subtle bg-surface-raised">
        <ScrollArea>
          <table className="w-full min-w-[52rem] text-left text-sm">
            <caption className="sr-only">
              Updates waiting for approval, page {approvals.data.pagination.page} of{" "}
              {approvals.data.pagination.totalPages}
            </caption>
            <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
              <tr>
                <th scope="col" className="px-4 py-3 font-medium">Container</th>
                <th scope="col" className="px-4 py-3 font-medium">Change</th>
                <th scope="col" className="px-4 py-3 font-medium">Type</th>
                <th scope="col" className="px-4 py-3 font-medium">Assessment</th>
                <th scope="col" className="px-4 py-3 font-medium">Policy</th>
                <th scope="col" className="px-4 py-3 font-medium">Decided</th>
                {/* The action column has no visible label, but it still needs
                    an accessible one. `relative` is load-bearing: sr-only is
                    absolutely positioned, and without a containing block on the
                    cell it escapes the horizontal scroll container and widens
                    the PAGE by its own offset -- 376px of body scroll at 390px
                    wide, on a page whose table was already scrolling correctly. */}
                <th scope="col" className="relative px-4 py-3 font-medium">
                  <span className="sr-only">Approve</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {items.map((decision) => (
                <ApprovalRow
                  key={`${decision.runId}-${decision.containerName}`}
                  decision={decision}
                  onApproved={onApproved}
                />
              ))}
            </tbody>
          </table>
        </ScrollArea>
      </div>

      <Pagination pagination={approvals.data.pagination} onPageChange={onPage} />

      <p role="note" className="rounded-lg border border-border-subtle bg-surface-raised px-3 py-2 text-xs text-content-muted">
        Approving releases the update into the normal pipeline. HarborMaster
        re-reads the container, re-checks that the change plan is still the
        current one, downloads the image by digest, and verifies the replacement
        before removing the original — the same checks it makes for an update you
        start yourself. A decision whose plan has moved on since it was recorded
        is refused rather than applied.
      </p>
    </div>
  );
}

function ApprovalRow({
  decision,
  onApproved,
}: {
  decision: AutomationDecision;
  onApproved: () => void;
}) {
  return (
    <tr className="border-b border-border-subtle last:border-0 align-top">
      <td className="px-4 py-3">
        {/* The container is the object an operator recognises, so it links to
            the container rather than to the decision record. */}
        {decision.containerId ? (
          <Link
            to={`/containers/${decision.containerId}`}
            className="font-medium text-accent hover:underline"
          >
            {decision.containerName}
          </Link>
        ) : (
          <span className="font-medium">{decision.containerName}</span>
        )}
        {/* Approval is AUTHORISATION, not a bypass. A container whose
            dependency is still being updated has not been released ahead of it,
            and a row that said only "waiting for approval" would leave an
            operator believing their click starts the update immediately. */}
        <ApprovalDependencyNote decision={decision} />
      </td>

      <td className="px-4 py-3">
        <div className="flex flex-col gap-0.5 font-mono text-xs">
          <span className="text-content-muted break-all">{decision.currentImage || "unknown"}</span>
          <span className="break-all">{decision.proposedImage || "unknown"}</span>
        </div>
      </td>

      <td className="px-4 py-3">
        {decision.updateType ? <UpdateBadge update={decision.updateType} /> : null}
      </td>

      <td className="px-4 py-3">
        <div className="flex flex-col gap-1">
          {decision.recommendation ? (
            <RecommendationBadge recommendation={decision.recommendation} />
          ) : null}
          {/* The evidence behind the verdict, one click away rather than
              required reading. */}
          {decision.containerId ? (
            <Link
              to={`/containers/${decision.containerId}?tab=plan`}
              className="text-xs text-accent hover:underline"
            >
              Read the assessment
            </Link>
          ) : null}
        </div>
      </td>

      <td className="px-4 py-3 text-xs text-content-muted">
        {decision.policyName || "—"}
      </td>

      <td className="px-4 py-3 text-xs text-content-muted">
        <time dateTime={decision.decidedAt}>
          {new Date(decision.decidedAt).toLocaleString()}
        </time>
      </td>

      <td className="px-4 py-3">
        <ApproveDecision
          runId={decision.runId}
          containerName={decision.containerName}
          onApproved={onApproved}
        />
      </td>
    </tr>
  );
}
