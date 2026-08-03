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
async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { signal, timeoutMs = DEFAULT_TIMEOUT_MS } = options;
  const timeoutController = new AbortController();
  const timer = setTimeout(() => timeoutController.abort(), timeoutMs);

  const signals = [timeoutController.signal];
  if (signal) signals.push(signal);

  let response: Response;
  try {
    response = await fetch(`${API_BASE}${path}`, {
      method: "GET",
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
