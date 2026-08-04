/**
 * Typed client for the HarborMaster REST API.
 *
 * Every call funnels through `request`, so error handling, timeouts, and JSON
 * decoding behave identically across the app. Callers get either a typed value
 * or an `ApiError` -- never a raw `Response` or an unparsed body.
 */

import type {
  ApiErrorCode,
  ApiErrorResponse,
  HealthReport,
  VersionInfo,
} from "./types";
import type {
  ContainerDetail,
  ContainerQuery,
  ContainerSummary,
  FilterOptions,
  ImageUsage,
  InventoryStatus,
  ListResponse,
  RawInspection,
  RefreshAccepted,
} from "./inventoryTypes";
import type {
  DockerEvent,
  DockerEventDetail,
  DockerEventQuery,
  EventEngineStatus,
  EventFilterOptions,
} from "./eventTypes";

/** Versioned base path. Relative, so the app works behind any mount point. */
export const API_BASE = "/api/v1";

/** Requests are bounded so a hung backend cannot leave the UI spinning. */
const DEFAULT_TIMEOUT_MS = 10_000;

/**
 * A failed API call.
 *
 * `isConnectivity` distinguishes "the backend is unreachable" from "the backend
 * answered with an error", which the UI renders very differently.
 */
export class ApiError extends Error {
  readonly code: ApiErrorCode;
  readonly status: number;
  readonly requestId: string | undefined;

  constructor(
    code: ApiErrorCode,
    message: string,
    status = 0,
    requestId?: string,
  ) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.status = status;
    this.requestId = requestId;
  }

  /** True when the request never produced a server response. */
  get isConnectivity(): boolean {
    return this.code === "network_error";
  }
}

export interface RequestOptions {
  signal?: AbortSignal | undefined;
  timeoutMs?: number;
}

/**
 * Performs a GET and decodes the JSON body.
 *
 * The caller's signal and the internal timeout are combined, so either can
 * abort the request.
 */
async function request<T>(
  path: string,
  options: RequestOptions = {},
  method: "GET" | "POST" = "GET",
): Promise<T> {
  const { signal, timeoutMs = DEFAULT_TIMEOUT_MS } = options;
  const timeoutController = new AbortController();
  const timer = setTimeout(() => timeoutController.abort(), timeoutMs);

  const signals = [timeoutController.signal];
  if (signal) signals.push(signal);

  let response: Response;
  try {
    response = await fetch(`${API_BASE}${path}`, {
      method,
      headers: { Accept: "application/json" },
      // No credentials: the API is unauthenticated and must not attach cookies
      // to a cross-origin dev proxy by accident.
      credentials: "omit",
      signal: AbortSignal.any(signals),
    });
  } catch {
    // A caller-initiated abort is a cancellation, not a connectivity failure.
    if (signal?.aborted) {
      throw new ApiError("network_error", "Request cancelled");
    }
    throw new ApiError(
      "network_error",
      timeoutController.signal.aborted
        ? "The backend did not respond in time"
        : "Cannot reach the HarborMaster backend",
    );
  } finally {
    clearTimeout(timer);
  }

  const requestId = response.headers.get("X-Request-ID") ?? undefined;

  if (!response.ok) {
    throw await toApiError(response, requestId);
  }

  try {
    return (await response.json()) as T;
  } catch {
    throw new ApiError(
      "invalid_response",
      "The backend returned a response that could not be read",
      response.status,
      requestId,
    );
  }
}

/** Converts a non-2xx response into an ApiError, tolerating a non-JSON body. */
async function toApiError(
  response: Response,
  requestId: string | undefined,
): Promise<ApiError> {
  try {
    const body = (await response.json()) as Partial<ApiErrorResponse>;
    if (body.error?.code && body.error?.message) {
      return new ApiError(
        body.error.code,
        body.error.message,
        response.status,
        body.requestId ?? requestId,
      );
    }
  } catch {
    // Fall through: a proxy or gateway may have returned HTML.
  }

  return new ApiError(
    response.status >= 500 ? "internal_error" : "invalid_response",
    `Request failed with status ${response.status}`,
    response.status,
    requestId,
  );
}

/** GET /api/v1/health */
export function getHealth(options?: RequestOptions): Promise<HealthReport> {
  return request<HealthReport>("/health", options);
}

/** GET /api/v1/version */
export function getVersion(options?: RequestOptions): Promise<VersionInfo> {
  return request<VersionInfo>("/version", options);
}

/** GET /api/v1/inventory */
export function getInventory(options?: RequestOptions): Promise<InventoryStatus> {
  return request<InventoryStatus>("/inventory", options);
}

/** GET /api/v1/inventory/filters */
export function getFilterOptions(options?: RequestOptions): Promise<FilterOptions> {
  return request<FilterOptions>("/inventory/filters", options);
}

/**
 * POST /api/v1/inventory/refresh
 *
 * Returns once the refresh is *accepted*, not once it finishes: the server
 * runs it in the background and answers 202. Poll getInventory and watch
 * `inProgress` and `generation` for completion.
 *
 * A 409 (a refresh already running) surfaces as an ApiError with code
 * "conflict", which the caller should treat as information rather than failure.
 */
export function refreshInventory(options?: RequestOptions): Promise<RefreshAccepted> {
  return request<RefreshAccepted>("/inventory/refresh", options, "POST");
}

/**
 * Builds the container list query string.
 *
 * Empty values are omitted rather than sent blank, so the request URL reflects
 * exactly which filters are active -- which is also what makes the request
 * assertable in tests.
 */
export function buildContainerQuery(query: ContainerQuery): string {
  const params = new URLSearchParams();

  const setIf = (key: string, value: string | number | boolean | undefined) => {
    if (value === undefined || value === "" ) return;
    params.set(key, String(value));
  };

  setIf("page", query.page);
  setIf("pageSize", query.pageSize);
  setIf("search", query.search?.trim() || undefined);
  setIf("project", query.project);
  setIf("service", query.service);
  setIf("image", query.image);
  setIf("restartPolicy", query.restartPolicy);
  setIf("labelKey", query.labelKey);
  setIf("labelValue", query.labelValue);
  setIf("harbormasterEnabled", query.harbormasterEnabled);
  setIf("includeAbsent", query.includeAbsent);
  setIf("sort", query.sort);
  setIf("direction", query.direction);

  // Repeated rather than comma-joined: both are accepted, and repetition
  // survives a value that itself contains a comma.
  for (const state of query.state ?? []) params.append("state", state);
  for (const health of query.health ?? []) params.append("health", health);

  const encoded = params.toString();
  return encoded ? `?${encoded}` : "";
}

/** GET /api/v1/containers */
export function listContainers(
  query: ContainerQuery = {},
  options?: RequestOptions,
): Promise<ListResponse<ContainerSummary>> {
  return request<ListResponse<ContainerSummary>>(
    `/containers${buildContainerQuery(query)}`,
    options,
  );
}

/** GET /api/v1/containers/{id} */
export function getContainer(
  id: string,
  options?: RequestOptions,
): Promise<ContainerDetail> {
  return request<ContainerDetail>(`/containers/${encodeURIComponent(id)}`, options);
}

/**
 * GET /api/v1/containers/{id}/raw
 *
 * The payload is redacted server-side. It is fetched only when the operator
 * opens the Raw tab, so the large body is never part of a normal page load.
 */
export function getContainerRaw(
  id: string,
  options?: RequestOptions,
): Promise<RawInspection> {
  return request<RawInspection>(`/containers/${encodeURIComponent(id)}/raw`, options);
}

/** GET /api/v1/images */
export function listImages(
  page = 1,
  pageSize = 25,
  options?: RequestOptions,
): Promise<ListResponse<ImageUsage>> {
  return request<ListResponse<ImageUsage>>(
    `/images?page=${page}&pageSize=${pageSize}`,
    options,
  );
}

/** GET /api/v1/images/{id} */
export function getImage(id: string, options?: RequestOptions): Promise<ImageUsage> {
  return request<ImageUsage>(`/images/${encodeURIComponent(id)}`, options);
}

// --------------------------------------------------------- docker events --

/**
 * Builds the event list query string.
 *
 * Empty values are omitted rather than sent blank, so the request URL reflects
 * exactly which filters are active -- which is also what makes it assertable.
 */
export function buildEventQuery(query: DockerEventQuery): string {
  const params = new URLSearchParams();

  const setIf = (key: string, value: string | number | undefined) => {
    if (value === undefined || value === "") return;
    params.set(key, String(value));
  };

  setIf("page", query.page);
  setIf("pageSize", query.pageSize);
  setIf("actorId", query.actorId);
  setIf("project", query.project);
  setIf("service", query.service);
  setIf("search", query.search?.trim() || undefined);
  setIf("since", query.since);
  setIf("until", query.until);
  setIf("sort", query.sort);
  setIf("direction", query.direction);

  // Repeated rather than comma-joined: both are accepted, and repetition
  // survives a value that itself contains a comma.
  for (const type of query.type ?? []) params.append("type", type);
  for (const action of query.action ?? []) params.append("action", action);
  for (const result of query.result ?? []) params.append("result", result);

  const encoded = params.toString();
  return encoded ? `?${encoded}` : "";
}

/** GET /api/v1/events */
export function listDockerEvents(
  query: DockerEventQuery = {},
  options?: RequestOptions,
): Promise<ListResponse<DockerEvent>> {
  return request<ListResponse<DockerEvent>>(`/events${buildEventQuery(query)}`, options);
}

/** GET /api/v1/events/{id} -- the id is the local sequence number. */
export function getDockerEvent(
  sequence: number,
  options?: RequestOptions,
): Promise<DockerEventDetail> {
  return request<DockerEventDetail>(`/events/${sequence}`, options);
}

/** GET /api/v1/event-engine */
export function getEventEngine(options?: RequestOptions): Promise<EventEngineStatus> {
  return request<EventEngineStatus>("/event-engine", options);
}

/** GET /api/v1/event-filters */
export function getEventFilterOptions(
  options?: RequestOptions,
): Promise<EventFilterOptions> {
  return request<EventFilterOptions>("/event-filters", options);
}

/**
 * The SSE endpoint's URL.
 *
 * `lastEventId` is passed as a query parameter because EventSource cannot set a
 * request header on its first connection. On an automatic reconnect the browser
 * sends the Last-Event-ID header itself, and the server accepts either.
 */
export function eventStreamUrl(lastEventId?: number): string {
  if (lastEventId === undefined || lastEventId <= 0) {
    return `${API_BASE}/events/stream`;
  }
  return `${API_BASE}/events/stream?lastEventId=${lastEventId}`;
}
