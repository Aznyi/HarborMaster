/**
 * Types mirroring the HarborMaster REST API.
 *
 * These are hand-maintained against `api/openapi.yaml`; that document is the
 * contract, and this file must be updated alongside it.
 */

/** Reachability of a single dependency. */
export type ComponentStatus = "up" | "down";

/**
 * Overall health verdict.
 *
 * `degraded` means the Docker Engine is unreachable but HarborMaster is still
 * serving -- a normal state the UI renders as "disconnected", not an outage.
 */
export type OverallStatus = "healthy" | "degraded" | "unhealthy";

export interface HealthComponent {
  status: ComponentStatus;
  /** Short, operator-safe explanation when the dependency is down. */
  detail?: string;
  latencyMs?: number;
  version?: string;
}

export interface HealthReport {
  status: OverallStatus;
  database: HealthComponent;
  docker: HealthComponent;
  /** ISO-8601 timestamp, UTC. */
  checkedAt: string;
  uptimeSeconds: number;
}

export interface VersionInfo {
  version: string;
  commit: string;
  buildDate: string;
  goVersion: string;
  platform: string;
}

/** Stable machine-readable error identifiers. Branch on these, not on prose. */
export type ApiErrorCode =
  | "not_found"
  | "method_not_allowed"
  | "payload_too_large"
  | "internal_error"
  | "invalid_request"
  | "ambiguous_id"
  | "conflict"
  | "service_unavailable"
  | "feature_disabled"
  /** Client-side only: the request never reached the server. */
  | "network_error"
  /** Client-side only: the response was not the JSON shape we expect. */
  | "invalid_response";

export interface ApiErrorBody {
  code: ApiErrorCode;
  message: string;
}

export interface ApiErrorResponse {
  error: ApiErrorBody;
  requestId?: string;
}
