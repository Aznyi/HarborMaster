import { useState } from "react";
import { Link, useParams } from "react-router";

import type { ChangePlan } from "../api/planTypes";
import { AcquireImageAction } from "../components/AcquireImageAction";
import { DetailSection } from "../components/DetailSection";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import { PlanDigests } from "../components/PlanBadges";
import { PlanCard, PlanTimeline } from "../components/PlanReasoning";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { useContainerPlans } from "../hooks/usePlans";

/**
 * One container's planning view.
 *
 * Two things on this page, and the distinction between them is the point:
 *
 *  - The CURRENT plan: what HarborMaster thinks of the change on offer now.
 *  - The REASONING TIMELINE: how that assessment has changed over time, which
 *    is what makes a verdict reviewable rather than merely stated.
 *
 * A container with no plan is rendered as "no change proposed", never as
 * "assessed and found safe". The two are different, and collapsing them would
 * be the worst thing this page could do.
 */
export function ContainerPlan() {
  const { id = "" } = useParams();
  const [page, setPage] = useState(1);
  const state = useContainerPlans(id, page);

  if (state.status === "loading") {
    return <LoadingState label="Loading the container's change plan" />;
  }
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }

  // Tolerate a malformed payload rather than throwing.
  const current = state.data?.current;
  const history: ChangePlan[] = state.data?.history ?? [];
  // The current plan leads the page, so showing it again at the top of the
  // timeline would read as two separate assessments of the same moment.
  const earlier = history.filter((plan) => plan.planId !== current?.planId);

  return (
    <div className="space-y-6">
      <PageIntro
        title="Change plan"
        description={
          "What HarborMaster thinks of the image change on offer for this " +
          "container, and how that assessment has changed. Analysis only — " +
          "nothing here pulls an image or changes the container."
        }
      />

      <p className="text-sm text-content-muted">
        <Link
          to={`/containers/${id}`}
          className="underline underline-offset-2 hover:text-content"
        >
          Back to the container
        </Link>
      </p>

      {current ? (
        <>
          <DetailSection title="Current assessment">
            <PlanCard plan={current} />
          </DetailSection>

          <DetailSection title="What is being compared">
            <PlanDigests plan={current} />
            <PlanEvidence plan={current} />
          </DetailSection>

          {/* The one action in HarborMaster that changes the host. Offered from
              the plan because the plan is what approves it -- there is nowhere
              in the UI to aim a download freely. */}
          <DetailSection
            title="Acquire this image"
            description={
              "Downloads the proposed image to this host so it is ready to " +
              "use. It does not update the container."
            }
          >
            <AcquireImageAction plan={current} onRequested={state.refresh} />
          </DetailSection>
        </>
      ) : (
        <EmptyState
          title="No change is proposed for this container"
          description={
            "That is not the same as a change assessed and found safe. A plan " +
            "appears once image intelligence finds a newer version for this " +
            "container's image, or once it cannot establish whether there is " +
            "one."
          }
        />
      )}

      {earlier.length > 0 && (
        <DetailSection
          title="Earlier assessments"
          description={
            "How HarborMaster's view of this container has changed. Each entry " +
            "is what was believed at the time and is never edited."
          }
        >
          <PlanTimeline plans={earlier} />
        </DetailSection>
      )}

      {state.data?.pagination && (
        <Pagination
          pagination={state.data.pagination}
          onPageChange={setPage}
          busy={state.refreshing}
        />
      )}
    </div>
  );
}

/**
 * What the assessment rested on.
 *
 * Each row links to the feature that owns the underlying record, so an operator
 * can check the input rather than take the plan's word for it. A plan
 * references these summaries; it does not copy the records, which have one
 * home each.
 */
function PlanEvidence({ plan }: { plan: ChangePlan }) {
  return (
    <dl className="mt-4 grid grid-cols-[auto_1fr] gap-x-3 gap-y-2 text-xs">
      <dt className="text-content-muted">Snapshot</dt>
      <dd className="text-content">
        {plan.snapshotAvailable && plan.snapshotId ? (
          <Link
            to={`/snapshots/${plan.snapshotId}`}
            className="underline underline-offset-2"
          >
            #{plan.snapshotId}
          </Link>
        ) : (
          "none recorded"
        )}
        {plan.snapshotAvailable && ` · restore readiness ${plan.restoreReadiness}`}
      </dd>

      <dt className="text-content-muted">Drift</dt>
      <dd className="text-content">
        {plan.driftOpen === 0 ? (
          "matches the baseline"
        ) : (
          <Link
            to={`/drift/container/${encodeURIComponent(plan.containerId)}`}
            className="underline underline-offset-2"
          >
            {plan.driftOpen} unresolved
            {plan.driftMaxSeverity && ` · worst ${plan.driftMaxSeverity}`}
          </Link>
        )}
      </dd>

      <dt className="text-content-muted">Policy</dt>
      <dd className="text-content">
        {plan.policyOpen === 0 ? (
          "compliant"
        ) : (
          <Link
            to={`/policy/container/${encodeURIComponent(plan.containerId)}`}
            className="underline underline-offset-2"
          >
            {plan.policyOpen} open
            {plan.policyMaxSeverity && ` · worst ${plan.policyMaxSeverity}`}
          </Link>
        )}
      </dd>

      <dt className="text-content-muted">Registry</dt>
      <dd className="text-content">
        {plan.registryStatus}
        {/* HarborMaster's own words for a non-OK status, never the registry's. */}
        {plan.registryDetail && ` · ${plan.registryDetail}`}
      </dd>

      {plan.proposedPublishedAt && (
        <>
          <dt className="text-content-muted">Published</dt>
          <dd className="text-content">
            <time dateTime={plan.proposedPublishedAt}>
              {new Date(plan.proposedPublishedAt).toLocaleString()}
            </time>
          </dd>
        </>
      )}
    </dl>
  );
}
