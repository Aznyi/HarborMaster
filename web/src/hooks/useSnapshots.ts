import { useCallback } from "react";

import {
  getSnapshot,
  getSnapshotDiff,
  getSnapshotReadiness,
  listSnapshots,
} from "../api/client";
import type { ListResponse } from "../api/inventoryTypes";
import type {
  DiffGroupName,
  ReadinessReport,
  Snapshot,
  SnapshotDetail,
  SnapshotDiff,
  SnapshotQuery,
} from "../api/snapshotTypes";
import { useApiResource, type ResourceState } from "./useApiResource";

/**
 * Snapshot resources.
 *
 * Read-only, like the rest of the snapshot UI. There is deliberately no hook
 * that restores a snapshot: HarborMaster has no such capability and the API
 * exposes no such endpoint.
 */

/** The snapshot list, filtered and paged server-side. */
export function useSnapshots(query: SnapshotQuery): ResourceState<ListResponse<Snapshot>> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => listSnapshots(query, { signal }),
    // The query object is rebuilt each render, so the key below is what
    // actually drives refetching.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(query)],
  );

  return useApiResource<ListResponse<Snapshot>>(fetcher, { key: JSON.stringify(query) });
}

/** One snapshot with its canonical document and derived sections. */
export function useSnapshot(id: number): ResourceState<SnapshotDetail> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getSnapshot(id, { signal }),
    [id],
  );
  return useApiResource<SnapshotDetail>(fetcher, { key: String(id) });
}

/** A snapshot's restore-readiness evaluation, computed fresh on each request. */
export function useSnapshotReadiness(id: number): ResourceState<ReadinessReport> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getSnapshotReadiness(id, { signal }),
    [id],
  );
  return useApiResource<ReadinessReport>(fetcher, { key: `readiness:${id}` });
}

/**
 * A comparison against another snapshot or against live configuration.
 *
 * Comparing against "current" reads live configuration and writes nothing.
 */
export function useSnapshotDiff(
  id: number,
  against: number | "current",
  options: { groups?: DiffGroupName[]; includeUnchanged?: boolean } = {},
): ResourceState<SnapshotDiff> {
  const key = `diff:${id}:${against}:${options.includeUnchanged ? "all" : "changed"}:${(options.groups ?? []).join(",")}`;

  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getSnapshotDiff(id, against, options, { signal }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [key],
  );

  return useApiResource<SnapshotDiff>(fetcher, { key });
}
