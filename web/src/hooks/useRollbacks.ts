import { useCallback } from "react";

import {
  cancelRollback,
  getRollback,
  listRollbacks,
  requestRollback,
} from "../api/client";
import type {
  Rollback,
  RollbackDetailResponse,
  RollbackListResponse,
  RollbackQuery,
} from "../api/rollbackTypes";
import { useApiResource, type ResourceState } from "./useApiResource";

/**
 * Manual rollback resources.
 *
 * Two reads and two writes, and the writes are the second pair in the app that
 * change something RUNNING.
 *
 * There is deliberately no hook that removes a container, retries a rollback,
 * or forces one — and no server capability behind any of them. A failed
 * rollback is settled by an operator using the recovery plan the record
 * carries, and the replacement is kept as evidence.
 */

/**
 * How often an active rollback is re-read.
 *
 * The same cadence as a recreation, and for the same reason: the interesting
 * part lasts seconds and an operator watching one is watching a container that
 * is currently down.
 */
const ACTIVE_POLL_MS = 1500;

/** The rollback history, filtered and paged server-side, with the summary. */
export function useRollbacks(
  query: RollbackQuery,
  options: { pollWhileActive?: boolean } = {},
): ResourceState<RollbackListResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => listRollbacks(query, { signal }),
    // The query object is rebuilt each render, so the key below is what
    // actually drives refetching.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(query)],
  );

  return useApiResource<RollbackListResponse>(fetcher, {
    key: JSON.stringify(query),
    ...(options.pollWhileActive ? { pollIntervalMs: ACTIVE_POLL_MS } : {}),
  });
}

/** One rollback with its checkpoint trail. */
export function useRollback(
  rollbackId: string,
  options: { poll?: boolean } = {},
): ResourceState<RollbackDetailResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getRollback(rollbackId, { signal }),
    [rollbackId],
  );

  return useApiResource<RollbackDetailResponse>(fetcher, {
    key: rollbackId,
    ...(options.poll ? { pollIntervalMs: ACTIVE_POLL_MS } : {}),
  });
}

/**
 * Asks HarborMaster to undo one recreation.
 *
 * Resolves once the request is ACCEPTED, not once the rollback finishes. The
 * caller refreshes afterwards rather than expecting the result inline.
 *
 * Takes an execution id and nothing else. There is no container, name, image,
 * or option to supply.
 */
export function rollBackExecution(
  executionId: string,
  requestKey?: string,
): Promise<Rollback> {
  return requestRollback(
    requestKey ? { executionId, requestKey } : { executionId },
  );
}

/**
 * Stops a rollback that has not yet changed anything.
 *
 * Rejects with a 409 past the mutation point. The UI does not offer the control
 * there — see isRollbackCancellable — but the server is the authority and the
 * race is real: a rollback can pass the mutation point between the render and
 * the click.
 */
export function stopRollback(rollbackId: string): Promise<Rollback> {
  return cancelRollback(rollbackId);
}
