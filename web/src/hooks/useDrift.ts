import { useCallback } from "react";

import {
  getContainerDrift,
  getDriftSummary,
  listDrift,
  updateDriftStatus,
} from "../api/client";
import type {
  ContainerDrift,
  DriftQuery,
  DriftRecord,
  DriftSummary,
  OperatorStatus,
} from "../api/driftTypes";
import type { ListResponse } from "../api/inventoryTypes";
import { useApiResource, type ResourceState } from "./useApiResource";

/**
 * Drift resources.
 *
 * Read hooks plus ONE write, and the write moves a status on HarborMaster's own
 * record. There is deliberately no hook that evaluates, resolves, remediates,
 * or rolls anything back: HarborMaster has no such capability and the API
 * exposes no such endpoint.
 */

/** The drift list, filtered and paged server-side. */
export function useDrift(query: DriftQuery): ResourceState<ListResponse<DriftRecord>> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => listDrift(query, { signal }),
    // The query object is rebuilt each render, so the key below is what
    // actually drives refetching.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(query)],
  );

  return useApiResource<ListResponse<DriftRecord>>(fetcher, {
    key: JSON.stringify(query),
  });
}

/** The dashboard aggregate. */
export function useDriftSummary(): ResourceState<DriftSummary> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getDriftSummary({ signal }),
    [],
  );
  return useApiResource<DriftSummary>(fetcher, { key: "drift-summary" });
}

/**
 * One container's drift, with the evaluation that produced it.
 *
 * The evaluation is what lets the UI distinguish "no drift" from "never
 * checked" — rendering an empty list as a clean bill of health for a container
 * with no baseline would be the worst thing this page could do.
 */
export function useContainerDrift(
  containerId: string,
  query: DriftQuery = {},
): ResourceState<ContainerDrift> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) =>
      getContainerDrift(containerId, query, { signal }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [containerId, JSON.stringify(query)],
  );

  return useApiResource<ContainerDrift>(fetcher, {
    key: `${containerId}:${JSON.stringify(query)}`,
  });
}

/**
 * Moves a drift record's status.
 *
 * Typed to OperatorStatus rather than DriftStatus: `active` and `resolved` are
 * engine-owned and the server rejects them, so a caller that tries finds out at
 * compile time rather than from a 400.
 */
export function setDriftStatus(
  id: number,
  status: OperatorStatus,
  note?: string,
): Promise<DriftRecord> {
  return updateDriftStatus(id, status, note);
}
