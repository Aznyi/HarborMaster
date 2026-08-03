import { useCallback, useMemo } from "react";

import {
  getContainer,
  getFilterOptions,
  getInventory,
  listContainers,
  listImages,
} from "../api/client";
import type {
  ContainerDetail,
  ContainerQuery,
  ContainerSummary,
  FilterOptions,
  ImageUsage,
  InventoryStatus,
  ListResponse,
} from "../api/inventoryTypes";
import { useApiResource, type ResourceState } from "./useApiResource";

/** How often the dashboard re-reads inventory status. */
export const INVENTORY_POLL_INTERVAL_MS = 10_000;

/** Polls GET /api/v1/inventory. */
export function useInventory(pollIntervalMs = INVENTORY_POLL_INTERVAL_MS): ResourceState<InventoryStatus> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getInventory({ signal }),
    [],
  );
  return useApiResource<InventoryStatus>(fetcher, { pollIntervalMs });
}

/**
 * Fetches a page of containers for the given query.
 *
 * The query is serialised into the fetcher's identity, so changing a filter
 * refetches from the server. Nothing is filtered or paged in the browser.
 */
export function useContainerPage(query: ContainerQuery): ResourceState<ListResponse<ContainerSummary>> {
  // Serialised so a structurally equal query does not retrigger the effect on
  // every render, which would poll the API continuously.
  const key = useMemo(() => JSON.stringify(query), [query]);

  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) =>
      listContainers(JSON.parse(key) as ContainerQuery, { signal }),
    [key],
  );
  return useApiResource<ListResponse<ContainerSummary>>(fetcher, { key });
}

/** Fetches one container's detail. */
export function useContainerDetail(id: string): ResourceState<ContainerDetail> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getContainer(id, { signal }),
    [id],
  );
  return useApiResource<ContainerDetail>(fetcher, { key: id });
}

/** Fetches the filter vocabularies the inventory actually contains. */
export function useFilterOptions(): ResourceState<FilterOptions> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getFilterOptions({ signal }),
    [],
  );
  return useApiResource<FilterOptions>(fetcher);
}

/** Fetches a page of images. */
export function useImagePage(page: number, pageSize = 25): ResourceState<ListResponse<ImageUsage>> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => listImages(page, pageSize, { signal }),
    [page, pageSize],
  );
  return useApiResource<ListResponse<ImageUsage>>(fetcher, { key: `${page}:${pageSize}` });
}
