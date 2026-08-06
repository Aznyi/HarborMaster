import { useCallback } from "react";

import {
  generateChangePlans,
  getChangePlan,
  getContainerPlans,
  listChangePlans,
} from "../api/client";
import type {
  ChangePlan,
  PlanContainerResponse,
  PlanListResponse,
  PlanQuery,
} from "../api/planTypes";
import { useApiResource, type ResourceState } from "./useApiResource";

/**
 * Change plan resources.
 *
 * Three read hooks plus one write, and the write asks HarborMaster to
 * regenerate its own analysis. There is deliberately no hook that applies,
 * executes, approves, or schedules a change: HarborMaster has no such
 * capability and the API exposes no such endpoint.
 */

/** The plan listing, filtered and paged server-side, with the estate summary. */
export function usePlans(query: PlanQuery): ResourceState<PlanListResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => listChangePlans(query, { signal }),
    // The query object is rebuilt each render, so the key below is what
    // actually drives refetching.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(query)],
  );

  return useApiResource<PlanListResponse>(fetcher, { key: JSON.stringify(query) });
}

/** One plan, with its full reasoning. */
export function usePlan(planId: string): ResourceState<ChangePlan> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getChangePlan(planId, { signal }),
    [planId],
  );
  return useApiResource<ChangePlan>(fetcher, { key: planId });
}

/** One container's current plan and the reasoning timeline behind it. */
export function useContainerPlans(
  containerId: string,
  page = 1,
): ResourceState<PlanContainerResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) =>
      getContainerPlans(containerId, page, 25, { signal }),
    [containerId, page],
  );
  return useApiResource<PlanContainerResponse>(fetcher, {
    key: `${containerId}:${page}`,
  });
}

/**
 * Requests a plan generation pass.
 *
 * Resolves once the request is ACCEPTED, not once the pass finishes: the server
 * schedules it and answers 202. Callers refresh the list afterwards rather than
 * expecting the result inline.
 *
 * Takes no argument. There is no target to supply, and nothing is applied.
 */
export function requestPlanGeneration(): Promise<unknown> {
  return generateChangePlans();
}
