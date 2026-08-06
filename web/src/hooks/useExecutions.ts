import { useCallback } from "react";

import {
  cancelExecution,
  getExecution,
  listExecutions,
  requestExecution,
} from "../api/client";
import type {
  Execution,
  ExecutionDetailResponse,
  ExecutionListResponse,
  ExecutionQuery,
} from "../api/executionTypes";
import { useApiResource, type ResourceState } from "./useApiResource";

/**
 * Container recreation resources.
 *
 * Two reads and two writes, and the writes are the only ones in the app that
 * change something RUNNING.
 *
 * There is deliberately no hook that rolls back, restores, retries, or forces a
 * recreation — and no server capability behind one. A failed recreation is
 * settled by an operator using the recovery plan the record carries.
 */

/**
 * How often an active recreation is re-read.
 *
 * Faster than the acquisition poll, because the interesting part of a
 * recreation lasts seconds rather than minutes and an operator watching one is
 * watching a container they depend on. Still polled rather than streamed: the
 * server writes a bounded number of events, so there is nothing a live channel
 * would add beyond a second thing to keep working.
 */
const ACTIVE_POLL_MS = 1500;

/** The recreation history, filtered and paged server-side, with the summary. */
export function useExecutions(
  query: ExecutionQuery,
  options: { pollWhileActive?: boolean } = {},
): ResourceState<ExecutionListResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => listExecutions(query, { signal }),
    // The query object is rebuilt each render, so the key below is what
    // actually drives refetching.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(query)],
  );

  return useApiResource<ExecutionListResponse>(fetcher, {
    key: JSON.stringify(query),
    // Polled only while something is actually moving. A settled list is
    // re-read on navigation like every other page.
    ...(options.pollWhileActive ? { pollIntervalMs: ACTIVE_POLL_MS } : {}),
  });
}

/** One recreation with its audit trail. */
export function useExecution(
  executionId: string,
  options: { poll?: boolean } = {},
): ResourceState<ExecutionDetailResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getExecution(executionId, { signal }),
    [executionId],
  );

  return useApiResource<ExecutionDetailResponse>(fetcher, {
    key: executionId,
    ...(options.poll ? { pollIntervalMs: ACTIVE_POLL_MS } : {}),
  });
}

/**
 * Asks HarborMaster to recreate a container on an image it already acquired.
 *
 * Resolves once the request is ACCEPTED, not once the recreation finishes. The
 * caller refreshes afterwards rather than expecting the result inline.
 *
 * Takes an acquisition id and nothing else. There is no container, image, or
 * option to supply.
 */
export function recreateForAcquisition(
  acquisitionId: string,
  requestKey?: string,
): Promise<Execution> {
  return requestExecution(
    requestKey ? { acquisitionId, requestKey } : { acquisitionId },
  );
}

/**
 * Stops a recreation that has not yet changed anything.
 *
 * Rejects with a 409 past the mutation point. The UI does not offer the control
 * there — see isCancellable — but the server is the authority and the race is
 * real: a recreation can pass the mutation point between the render and the
 * click.
 */
export function stopExecution(executionId: string): Promise<Execution> {
  return cancelExecution(executionId);
}
