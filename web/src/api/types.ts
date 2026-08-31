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
  events?: HealthComponent;
  /** Which capabilities this deployment has. Authenticated callers only. */
  features?: Features;
  /** ISO-8601 timestamp, UTC. */
  checkedAt: string;
  uptimeSeconds: number;
}

/**
 * Which capabilities a deployment turned on.
 *
 * # Booleans, never values
 *
 * No path, no address, no interval, no credential. Each field says whether a
 * capability EXISTS in this process, which is what lets an operator looking at
 * an empty page tell "switched off" from "not working" — two explanations that
 * are otherwise indistinguishable, and that lead somewhere very different.
 */
export interface Features {
  inventory: boolean;
  events: boolean;
  snapshots: boolean;
  drift: boolean;
  policy: boolean;
  planner: boolean;
  imageIntel: boolean;

  /** Downloads an approved, digest-pinned image. Touches no container. */
  acquisition: boolean;
  /** STOPS A RUNNING CONTAINER and replaces it. */
  execution: boolean;
  /** Stops the replacement and starts the original. */
  rollback: boolean;
  /** REMOVES IMAGES a settled update superseded. Cannot be undone. */
  imageCleanup: boolean;
  /** Changes containers with nobody watching. */
  automation: boolean;

  /** HarborMaster's second outbound egress. */
  notifications: boolean;
  /** The one relaxation of the notification address guard. */
  notificationsAllowPrivate: boolean;
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
  /** No usable session. The shell renders the sign-in page. */
  | "unauthenticated"
  /** Authenticated, but the role does not hold the required permission. */
  | "forbidden"
  /** A state-changing request with a missing or wrong CSRF token. */
  | "csrf_required"
  /** The account must set a new password before doing anything else. */
  | "password_change_required"
  /** The installation has no administrator yet. */
  | "bootstrap_required"
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
