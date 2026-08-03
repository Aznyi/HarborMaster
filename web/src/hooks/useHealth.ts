import { useCallback } from "react";

import { getHealth, getVersion } from "../api/client";
import type { HealthReport, VersionInfo } from "../api/types";
import { useApiResource, type ResourceState } from "./useApiResource";

/** How often the shell re-probes connectivity. */
export const HEALTH_POLL_INTERVAL_MS = 10_000;

/** Polls GET /api/v1/health so the shell can show live connectivity. */
export function useHealth(
  pollIntervalMs: number = HEALTH_POLL_INTERVAL_MS,
): ResourceState<HealthReport> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getHealth({ signal }),
    [],
  );
  return useApiResource<HealthReport>(fetcher, { pollIntervalMs });
}

/** Fetches GET /api/v1/version once; build metadata does not change at runtime. */
export function useVersion(): ResourceState<VersionInfo> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getVersion({ signal }),
    [],
  );
  return useApiResource<VersionInfo>(fetcher);
}
