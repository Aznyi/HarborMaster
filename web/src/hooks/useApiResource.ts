import { useCallback, useEffect, useRef, useState } from "react";

import { ApiError } from "../api/client";

/**
 * The four states every API-backed view must handle.
 *
 * `disconnected` is separate from `error` on purpose: "the backend is
 * unreachable" and "the backend rejected the request" call for different
 * messages and different remedies.
 */
export type ResourceStatus = "loading" | "ready" | "disconnected" | "error";

export interface ResourceState<T> {
  status: ResourceStatus;
  data: T | null;
  error: ApiError | null;
  /** True while a background refresh runs over already-loaded data. */
  refreshing: boolean;
  /** Fetches again immediately. */
  refresh: () => void;
}

export interface UseApiResourceOptions {
  /** Poll interval in milliseconds. Omit or pass 0 to fetch once. */
  pollIntervalMs?: number;
  /**
   * Refetch key. Changing it refetches immediately.
   *
   * Needed because the fetcher is held in a ref, so a new closure alone does
   * not restart the effect -- that is what keeps polling stable across
   * renders. A query-dependent resource (a filtered, sorted, paged list) must
   * therefore state explicitly what it depends on.
   */
  key?: string;
}

/**
 * Fetches an API resource, optionally polling, and exposes an explicit state
 * machine rather than a bare loading boolean.
 *
 * Stale responses are discarded: only the newest in-flight request may write to
 * state, so a slow reply cannot overwrite a newer one.
 */
export function useApiResource<T>(
  fetcher: (options: { signal: AbortSignal }) => Promise<T>,
  { pollIntervalMs = 0, key = "" }: UseApiResourceOptions = {},
): ResourceState<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [status, setStatus] = useState<ResourceStatus>("loading");
  const [refreshing, setRefreshing] = useState(false);
  const [reloadToken, setReloadToken] = useState(0);

  // Held in a ref so changing the fetcher identity does not restart polling.
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;

  const refresh = useCallback(() => setReloadToken((token) => token + 1), []);

  useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;

    const load = async (isBackground: boolean) => {
      if (isBackground) setRefreshing(true);
      try {
        const result = await fetcherRef.current({ signal: controller.signal });
        if (cancelled) return;
        setData(result);
        setError(null);
        setStatus("ready");
      } catch (caught) {
        if (cancelled) return;
        const apiError =
          caught instanceof ApiError
            ? caught
            : new ApiError("invalid_response", "Unexpected client error");
        setError(apiError);
        setStatus(apiError.isConnectivity ? "disconnected" : "error");
      } finally {
        if (!cancelled) setRefreshing(false);
      }
    };

    void load(false);

    if (pollIntervalMs <= 0) {
      return () => {
        cancelled = true;
        controller.abort();
      };
    }

    const timer = setInterval(() => void load(true), pollIntervalMs);
    return () => {
      cancelled = true;
      controller.abort();
      clearInterval(timer);
    };
  }, [pollIntervalMs, reloadToken, key]);

  return { status, data, error, refreshing, refresh };
}
