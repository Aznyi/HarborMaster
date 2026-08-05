import { useCallback } from "react";

import {
  getImageDetail,
  getImageHistory,
  listImageUpdates,
  refreshImageMetadata,
} from "../api/client";
import type {
  ImageDetail,
  ImageHistoryResponse,
  ImageUpdateQuery,
  ImageUpdatesResponse,
} from "../api/imageTypes";
import { useApiResource, type ResourceState } from "./useApiResource";

/**
 * Image intelligence resources.
 *
 * Read hooks plus one write, and the write schedules a metadata collection
 * pass. There is deliberately no hook that pulls, deletes, prunes, or applies
 * an update: HarborMaster has no such capability and the API exposes no such
 * endpoint.
 */

/** The updates list, filtered and paged server-side, with the estate summary. */
export function useImageUpdates(
  query: ImageUpdateQuery,
): ResourceState<ImageUpdatesResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => listImageUpdates(query, { signal }),
    // The query object is rebuilt each render, so the key below is what
    // actually drives refetching.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(query)],
  );

  return useApiResource<ImageUpdatesResponse>(fetcher, {
    key: JSON.stringify(query),
  });
}

/** One local image with the registry intelligence for its references. */
export function useImageDetail(id: string): ResourceState<ImageDetail> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getImageDetail(id, { signal }),
    [id],
  );
  return useApiResource<ImageDetail>(fetcher, { key: id });
}

/** One image's observed changes, newest first. */
export function useImageHistory(
  id: string,
  page = 1,
): ResourceState<ImageHistoryResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getImageHistory(id, page, 25, { signal }),
    [id, page],
  );
  return useApiResource<ImageHistoryResponse>(fetcher, { key: `${id}:${page}` });
}

/**
 * Requests a metadata collection pass.
 *
 * Resolves once the request is ACCEPTED, not once the pass finishes: the server
 * schedules it and answers 202. Callers refresh the list afterwards rather than
 * expecting the result inline.
 *
 * Takes no argument. There is no target to supply.
 */
export function requestImageRefresh(): Promise<unknown> {
  return refreshImageMetadata();
}
