import { useCallback } from "react";

import {
  cancelAcquisition,
  getAcquisition,
  listAcquisitions,
  requestAcquisition,
} from "../api/client";
import type {
  Acquisition,
  AcquisitionDetailResponse,
  AcquisitionListResponse,
  AcquisitionQuery,
} from "../api/acquisitionTypes";
import { useApiResource, type ResourceState } from "./useApiResource";

/**
 * Image acquisition resources.
 *
 * Two reads and two writes, and the writes are the only ones in the app that
 * reach the Docker host. Even so, neither changes a container: one downloads an
 * approved image into the local store, and the other stops a download.
 *
 * There is deliberately no hook that applies an image, recreates a container,
 * or rolls one back.
 */

/**
 * How often an active acquisition's detail is re-read.
 *
 * Polled rather than streamed: a transfer emits progress far faster than a
 * person can read it, the server already rate-limits what it records, and
 * adding a second live channel for one page would be a poor trade. Two seconds
 * is fast enough to feel live and slow enough to be free.
 */
const ACTIVE_POLL_MS = 2000;

/** The acquisition history, filtered and paged server-side, with the summary. */
export function useAcquisitions(
  query: AcquisitionQuery,
  options: { pollWhileActive?: boolean } = {},
): ResourceState<AcquisitionListResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => listAcquisitions(query, { signal }),
    // The query object is rebuilt each render, so the key below is what
    // actually drives refetching.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(query)],
  );

  return useApiResource<AcquisitionListResponse>(fetcher, {
    key: JSON.stringify(query),
    // Polled only while something is actually moving. A settled list is
    // re-read on navigation like every other page.
    ...(options.pollWhileActive ? { pollIntervalMs: ACTIVE_POLL_MS } : {}),
  });
}

/** One acquisition with its audit trail. */
export function useAcquisition(
  acquisitionId: string,
  options: { poll?: boolean } = {},
): ResourceState<AcquisitionDetailResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getAcquisition(acquisitionId, { signal }),
    [acquisitionId],
  );

  return useApiResource<AcquisitionDetailResponse>(fetcher, {
    key: acquisitionId,
    ...(options.poll ? { pollIntervalMs: ACTIVE_POLL_MS } : {}),
  });
}

/**
 * Asks HarborMaster to acquire the image an approved plan names.
 *
 * Resolves once the request is ACCEPTED, not once the download finishes. The
 * caller refreshes afterwards rather than expecting the result inline.
 *
 * Takes a plan id and nothing else. There is no target to supply.
 */
export function acquireForPlan(planId: string, requestKey?: string): Promise<Acquisition> {
  return requestAcquisition(requestKey ? { planId, requestKey } : { planId });
}

/** Stops an acquisition an operator no longer wants. */
export function stopAcquisition(acquisitionId: string): Promise<Acquisition> {
  return cancelAcquisition(acquisitionId);
}
