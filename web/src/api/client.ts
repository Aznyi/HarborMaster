/**
 * Typed client for the HarborMaster REST API.
 *
 * Every call funnels through `request`, so error handling, timeouts, and JSON
 * decoding behave identically across the app. Callers get either a typed value
 * or an `ApiError` -- never a raw `Response` or an unparsed body.
 */

import type {
  DeliveryQuery,
  NotificationConfigQuery,
  NotificationDelivery,
  NotificationDeliveryListResponse,
  NotificationDestination,
  NotificationDestinationListResponse,
  NotificationDestinationRequest,
  NotificationDestinationResult,
  NotificationRuleListResponse,
  NotificationRuleRequest,
  NotificationRuleResult,
  NotificationStatus,
  TestNotificationResponse,
} from "./notificationTypes";
import type {
  ApiErrorCode,
  ApiErrorResponse,
  HealthReport,
  VersionInfo,
} from "./types";
import type {
  ContainerDetail,
  ContainerQuery,
  ContainerListRow,
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
import type {
  CreateSnapshotRequest,
  DiffGroupName,
  ReadinessReport,
  Snapshot,
  SnapshotDetail,
  SnapshotDiff,
  SnapshotQuery,
} from "./snapshotTypes";
import type {
  ContainerDrift,
  DriftQuery,
  DriftRecord,
  DriftSummary,
  OperatorStatus,
} from "./driftTypes";
import type {
  ImageDetail,
  ImageHistoryResponse,
  ImageRefreshAccepted,
  ImageUpdateQuery,
  ImageUpdatesResponse,
} from "./imageTypes";
import type {
  ContainerPolicy,
  PolicyDefinition,
  PolicyEvaluateAccepted,
  PolicyOperatorStatus,
  PolicyQuery,
  PolicyRequest,
  PolicyRuleCatalogue,
  PolicySummary,
  PolicyViolation,
  PolicyViolationQuery,
} from "./policyTypes";
import type {
  ChangePlan,
  PlanContainerResponse,
  PlanGenerateAccepted,
  PlanListResponse,
  PlanQuery,
} from "./planTypes";
import type {
  Acquisition,
  AcquisitionDetailResponse,
  AcquisitionListResponse,
  AcquisitionQuery,
  AcquisitionRequest,
} from "./acquisitionTypes";
import type {
  Execution,
  ExecutionDetailResponse,
  ExecutionListResponse,
  ExecutionQuery,
  ExecutionRequest,
} from "./executionTypes";
import type {
  AutomationDecision,
  AutomationApprovalListResponse,
  AutomationPauseListResponse,
  AutomationRunDetailResponse,
  AutomationRunListResponse,
  AutomationRunQuery,
  AutomationStatusResponse,
  AutomationUpcomingResponse,
  PausedContainer,
  UpdatePolicyListResponse,
  UpdatePolicyQuery,
  UpdatePolicyRequest,
  UpdatePolicyResult,
} from "./automationTypes";
import type {
  Rollback,
  RollbackDetailResponse,
  RollbackListResponse,
  RollbackQuery,
  RollbackRequest,
} from "./rollbackTypes";
import type {
  AuditListResponse,
  AuditQuery,
  AuditSummary,
  BootstrapRequest,
  BootstrapStatus,
  ChangePasswordRequest,
  CreateUserRequest,
  CreatedUser,
  LoginRequest,
  PublicUser,
  RoleCatalogue,
  SessionListResponse,
  SessionResponse,
  UpdateUserRequest,
  UserListResponse,
} from "./authTypes";

/** Versioned base path. Relative, so the app works behind any mount point. */
export const API_BASE = "/api/v1";

/** Requests are bounded so a hung backend cannot leave the UI spinning. */
const DEFAULT_TIMEOUT_MS = 10_000;

/** The header the server reads the per-session CSRF token from. */
export const CSRF_HEADER = "X-HarborMaster-CSRF";

/**
 * The CSRF token for the current session.
 *
 * # Why a module variable and not storage
 *
 * `localStorage` and `sessionStorage` survive the tab, are readable by any
 * script that reaches this origin, and are exactly where a successful XSS looks
 * first. A module variable dies with the page, which is the same lifetime as
 * the value's usefulness.
 *
 * It is not a secret in the way the session token is -- it is useless without
 * the cookie -- but there is no reason to persist it, so it is not persisted.
 */
let csrfToken = "";

/** Sets the CSRF token for subsequent writes. */
export function setCsrfToken(token: string): void {
  csrfToken = token;
}

/** Clears the CSRF token. Called on sign-out and on any 401. */
export function clearCsrfToken(): void {
  csrfToken = "";
}

/** True when a token is held, so the UI can tell "signed in" from "not". */
export function hasCsrfToken(): boolean {
  return csrfToken !== "";
}

type UnauthenticatedListener = (code: ApiErrorCode) => void;

const unauthenticatedListeners = new Set<UnauthenticatedListener>();

/**
 * Registers a listener for "this session is no longer usable".
 *
 * Returns an unsubscribe function. The session provider is the only subscriber;
 * it exists so a 401 from any page in the app produces ONE transition to the
 * sign-in screen rather than an error toast on each view that happened to be
 * polling.
 */
export function onUnauthenticated(listener: UnauthenticatedListener): () => void {
  unauthenticatedListeners.add(listener);
  return () => {
    unauthenticatedListeners.delete(listener);
  };
}

function notifyUnauthenticated(code: ApiErrorCode): void {
  for (const listener of unauthenticatedListeners) listener(code);
}

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
 * Performs a request and decodes the JSON body.
 *
 * The caller's signal and the internal timeout are combined, so either can
 * abort the request.
 *
 * A 204 resolves to `undefined`, because a No Content response has no body to
 * decode. `DELETE /policies/{id}` is the one endpoint that answers this way.
 */
async function request<T>(
  path: string,
  options: RequestOptions = {},
  method: "GET" | "POST" | "PATCH" | "DELETE" = "GET",
  body?: unknown,
): Promise<T> {
  const { signal, timeoutMs = DEFAULT_TIMEOUT_MS } = options;
  const timeoutController = new AbortController();
  const timer = setTimeout(() => timeoutController.abort(), timeoutMs);

  const signals = [timeoutController.signal];
  if (signal) signals.push(signal);

  const headers: Record<string, string> = { Accept: "application/json" };
  // Set only when there is a body. The server requires it on writes, which also
  // forces a CORS preflight for any cross-origin caller -- a plain form POST
  // cannot set this header, so it cannot reach the write endpoints.
  if (body !== undefined) headers["Content-Type"] = "application/json";

  // The CSRF token on every state-changing request.
  //
  // A cross-site page can make the browser send our cookie, but it cannot read
  // this token and cannot set a custom header without a preflight that fails.
  // Held in memory only -- see `csrfToken` below.
  if (method !== "GET" && csrfToken) headers[CSRF_HEADER] = csrfToken;

  let response: Response;
  try {
    response = await fetch(`${API_BASE}${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      // The session cookie must travel with every request, and ONLY to this
      // origin. "same-origin" is what keeps it from being attached to a
      // cross-origin dev proxy by accident.
      credentials: "same-origin",
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
    const failure = await toApiError(response, requestId);
    // A session that has ended is not an error the calling page can act on --
    // every subsequent request would fail the same way. It is announced once,
    // and the shell re-renders as the sign-in page.
    if (failure.status === 401) {
      csrfToken = "";
      notifyUnauthenticated(failure.code);
    }
    throw failure;
  }

  // No Content. Returning undefined rather than attempting a decode, which
  // would fail and be reported as a malformed response for a request that in
  // fact succeeded.
  if (response.status === 204) return undefined as T;

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
  setIf("includePreserved", query.includePreserved);
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
): Promise<ListResponse<ContainerListRow>> {
  return request<ListResponse<ContainerListRow>>(
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

// ------------------------------------------------------------- snapshots --

/**
 * Builds the snapshot list query string.
 *
 * Empty values are omitted rather than sent blank, so the request URL reflects
 * exactly which filters are active -- which is also what makes it assertable.
 */
export function buildSnapshotQuery(query: SnapshotQuery): string {
  const params = new URLSearchParams();

  const setIf = (key: string, value: string | number | undefined) => {
    if (value === undefined || value === "") return;
    params.set(key, String(value));
  };

  setIf("page", query.page);
  setIf("pageSize", query.pageSize);
  setIf("containerId", query.containerId);
  setIf("checksum", query.checksum);
  setIf("since", query.since);
  setIf("until", query.until);
  setIf("sort", query.sort);
  setIf("direction", query.direction);

  // Repeated rather than comma-joined: both are accepted, and repetition
  // survives a value that itself contains a comma.
  for (const trigger of query.trigger ?? []) params.append("trigger", trigger);
  for (const readiness of query.readiness ?? []) params.append("readiness", readiness);

  const encoded = params.toString();
  return encoded ? `?${encoded}` : "";
}

/** GET /api/v1/snapshots */
export function listSnapshots(
  query: SnapshotQuery = {},
  options?: RequestOptions,
): Promise<ListResponse<Snapshot>> {
  return request<ListResponse<Snapshot>>(
    `/snapshots${buildSnapshotQuery(query)}`,
    options,
  );
}

/** GET /api/v1/snapshots/{id} */
export function getSnapshot(
  id: number,
  options?: RequestOptions,
): Promise<SnapshotDetail> {
  return request<SnapshotDetail>(`/snapshots/${id}`, options);
}

/**
 * POST /api/v1/snapshots
 *
 * Captures a container's configuration. This does NOT change the container:
 * capture reads HarborMaster's inventory and writes HarborMaster's database.
 *
 * A 200 with `deduplicated: true` means the configuration was unchanged and the
 * existing snapshot was returned; nothing was created. A 409 means a capture is
 * already running for that container.
 */
export function createSnapshot(
  body: CreateSnapshotRequest,
  options?: RequestOptions,
): Promise<Snapshot> {
  return request<Snapshot>("/snapshots", options, "POST", body);
}

/**
 * GET /api/v1/snapshots/{id}/diff
 *
 * `against` is a snapshot id, or "current" to compare with live configuration.
 * Comparing against current writes nothing.
 */
export function getSnapshotDiff(
  id: number,
  against: number | "current" = "current",
  opts: { groups?: DiffGroupName[]; includeUnchanged?: boolean } = {},
  options?: RequestOptions,
): Promise<SnapshotDiff> {
  const params = new URLSearchParams();
  params.set("against", String(against));
  if (opts.includeUnchanged) params.set("includeUnchanged", "true");
  for (const group of opts.groups ?? []) params.append("group", group);

  return request<SnapshotDiff>(`/snapshots/${id}/diff?${params.toString()}`, options);
}

/**
 * GET /api/v1/snapshots/{id}/restore-readiness
 *
 * Informational. HarborMaster cannot restore a container; this reports whether
 * restoration WOULD succeed, so the gap is visible before it matters.
 */
export function getSnapshotReadiness(
  id: number,
  options?: RequestOptions,
): Promise<ReadinessReport> {
  return request<ReadinessReport>(`/snapshots/${id}/restore-readiness`, options);
}

/* ------------------------------------------------------------------ drift -- */

/**
 * Configuration drift.
 *
 * Four reads and one write. The write moves a record's STATUS on
 * HarborMaster's own row: there is deliberately no function here that
 * evaluates, resolves, remediates, or rolls anything back, because the API
 * exposes no such endpoint and HarborMaster has no such capability.
 */

/** Builds the drift list query string from validated, closed vocabularies. */
function buildDriftQuery(query: DriftQuery): string {
  const params = new URLSearchParams();

  if (query.page && query.page > 1) params.set("page", String(query.page));
  if (query.pageSize) params.set("pageSize", String(query.pageSize));
  if (query.containerId) params.set("containerId", query.containerId);
  if (query.sort) params.set("sort", query.sort);
  if (query.order) params.set("order", query.order);
  // Sent only when explicitly set, so the server's own default stands
  // otherwise and the two cannot disagree about what "default" means.
  if (query.openOnly !== undefined) params.set("openOnly", String(query.openOnly));

  for (const category of query.category ?? []) params.append("category", category);
  for (const severity of query.severity ?? []) params.append("severity", severity);
  for (const status of query.status ?? []) params.append("status", status);

  const rendered = params.toString();
  return rendered ? `?${rendered}` : "";
}

/** GET /api/v1/drift */
export function listDrift(
  query: DriftQuery = {},
  options?: RequestOptions,
): Promise<ListResponse<DriftRecord>> {
  return request<ListResponse<DriftRecord>>(`/drift${buildDriftQuery(query)}`, options);
}

/** GET /api/v1/drift/{id} */
export function getDriftRecord(
  id: number,
  options?: RequestOptions,
): Promise<DriftRecord> {
  return request<DriftRecord>(`/drift/${id}`, options);
}

/** GET /api/v1/drift/summary */
export function getDriftSummary(options?: RequestOptions): Promise<DriftSummary> {
  return request<DriftSummary>("/drift/summary", options);
}

/** GET /api/v1/drift/container/{id} */
export function getContainerDrift(
  containerId: string,
  query: DriftQuery = {},
  options?: RequestOptions,
): Promise<ContainerDrift> {
  return request<ContainerDrift>(
    `/drift/container/${encodeURIComponent(containerId)}${buildDriftQuery(query)}`,
    options,
  );
}

/**
 * PATCH /api/v1/drift/{id}
 *
 * Moves a drift record's status. **This changes nothing outside
 * HarborMaster's own database** — no container, image, or snapshot is touched.
 *
 * The status type is `OperatorStatus`, not `DriftStatus`: `active` and
 * `resolved` are engine-owned and the server rejects them. Narrowing it here
 * means a caller that tries finds out at compile time rather than from a 400.
 */
export function updateDriftStatus(
  id: number,
  status: OperatorStatus,
  note?: string,
  options?: RequestOptions,
): Promise<DriftRecord> {
  return request<DriftRecord>(`/drift/${id}`, options, "PATCH", {
    status,
    ...(note ? { note } : {}),
  });
}

/* ---------------------------------------------------------------- policy -- */

/**
 * The policy engine.
 *
 * Reads plus the create/update/withdraw surface. Every write acts on
 * HARBORMASTER'S OWN ROWS: a policy is a rule that configuration is CHECKED
 * AGAINST, never one that is applied to Docker. There is deliberately no
 * function here that enforces, remediates, or applies anything, because the API
 * exposes no such endpoint and HarborMaster has no such capability.
 */

/** Builds the policy list query string. */
function buildPolicyQuery(query: PolicyQuery): string {
  const params = new URLSearchParams();

  if (query.page && query.page > 1) params.set("page", String(query.page));
  if (query.pageSize) params.set("pageSize", String(query.pageSize));
  if (query.search) params.set("search", query.search);
  if (query.sort) params.set("sort", query.sort);
  if (query.order) params.set("order", query.order);
  if (query.enabled !== undefined) params.set("enabled", String(query.enabled));
  if (query.includeArchived !== undefined) {
    params.set("includeArchived", String(query.includeArchived));
  }

  const rendered = params.toString();
  return rendered ? `?${rendered}` : "";
}

/** Builds the violation list query string from closed vocabularies. */
function buildViolationQuery(query: PolicyViolationQuery): string {
  const params = new URLSearchParams();

  if (query.page && query.page > 1) params.set("page", String(query.page));
  if (query.pageSize) params.set("pageSize", String(query.pageSize));
  if (query.containerId) params.set("containerId", query.containerId);
  if (query.policyId) params.set("policyId", query.policyId);
  if (query.sort) params.set("sort", query.sort);
  if (query.order) params.set("order", query.order);
  // Sent only when explicitly set, so the server's own default stands
  // otherwise and the two cannot disagree about what "default" means.
  if (query.openOnly !== undefined) params.set("openOnly", String(query.openOnly));

  for (const rule of query.rule ?? []) params.append("rule", rule);
  for (const severity of query.severity ?? []) params.append("severity", severity);
  for (const status of query.status ?? []) params.append("status", status);

  const rendered = params.toString();
  return rendered ? `?${rendered}` : "";
}

/** GET /api/v1/policies */
export function listPolicies(
  query: PolicyQuery = {},
  options?: RequestOptions,
): Promise<ListResponse<PolicyDefinition>> {
  return request<ListResponse<PolicyDefinition>>(
    `/policies${buildPolicyQuery(query)}`,
    options,
  );
}

/** GET /api/v1/policies/{id} */
export function getPolicy(
  policyId: string,
  options?: RequestOptions,
): Promise<PolicyDefinition> {
  return request<PolicyDefinition>(
    `/policies/${encodeURIComponent(policyId)}`,
    options,
  );
}

/**
 * GET /api/v1/policy-rules
 *
 * The rule catalogue the editor is built from. Fetched rather than hardcoded:
 * a second copy in the frontend would eventually offer a rule the backend
 * rejects, and the operator would find out only after filling in the form.
 */
export function getPolicyRules(
  options?: RequestOptions,
): Promise<PolicyRuleCatalogue> {
  return request<PolicyRuleCatalogue>("/policy-rules", options);
}

/**
 * POST /api/v1/policies
 *
 * There is no `policyId` in the request type. The identifier is generated by
 * the server and is immutable; a body that cannot express the change is a
 * stronger guarantee than a check that rejects it.
 */
export function createPolicy(
  body: PolicyRequest,
  options?: RequestOptions,
): Promise<PolicyDefinition> {
  return request<PolicyDefinition>("/policies", options, "POST", body);
}

/** PATCH /api/v1/policies/{id} — a partial update; omitted fields are left alone. */
export function updatePolicy(
  policyId: string,
  body: PolicyRequest,
  options?: RequestOptions,
): Promise<PolicyDefinition> {
  return request<PolicyDefinition>(
    `/policies/${encodeURIComponent(policyId)}`,
    options,
    "PATCH",
    body,
  );
}

/**
 * DELETE /api/v1/policies/{id}
 *
 * **Archives rather than destroys.** A violation references its policy and the
 * history has to survive the rule being withdrawn, so the definition is
 * retained, marked archived, and its open violations resolve. Answers 204.
 */
export function archivePolicy(
  policyId: string,
  options?: RequestOptions,
): Promise<void> {
  return request<void>(
    `/policies/${encodeURIComponent(policyId)}`,
    options,
    "DELETE",
  );
}

/** GET /api/v1/policy-violations */
export function listPolicyViolations(
  query: PolicyViolationQuery = {},
  options?: RequestOptions,
): Promise<ListResponse<PolicyViolation>> {
  return request<ListResponse<PolicyViolation>>(
    `/policy-violations${buildViolationQuery(query)}`,
    options,
  );
}

/** GET /api/v1/policy-violations/{id} */
export function getPolicyViolation(
  id: number,
  options?: RequestOptions,
): Promise<PolicyViolation> {
  return request<PolicyViolation>(`/policy-violations/${id}`, options);
}

/** GET /api/v1/policy-summary */
export function getPolicySummary(options?: RequestOptions): Promise<PolicySummary> {
  return request<PolicySummary>("/policy-summary", options);
}

/** GET /api/v1/policy-violations/container/{id} */
export function getContainerPolicy(
  containerId: string,
  query: PolicyViolationQuery = {},
  options?: RequestOptions,
): Promise<ContainerPolicy> {
  return request<ContainerPolicy>(
    `/policy-violations/container/${encodeURIComponent(containerId)}${buildViolationQuery(query)}`,
    options,
  );
}

/**
 * PATCH /api/v1/policy-violations/{id}
 *
 * Moves a violation's status. **This changes nothing outside HarborMaster's own
 * database**, and it does NOT suppress re-evaluation: the next pass still
 * applies the rule and still resolves the violation once the container
 * complies.
 *
 * The status type is `PolicyOperatorStatus`, not `PolicyViolationStatus`:
 * `active` and `resolved` are engine-owned and the server rejects them.
 */
export function updateViolationStatus(
  id: number,
  status: PolicyOperatorStatus,
  note?: string,
  options?: RequestOptions,
): Promise<PolicyViolation> {
  return request<PolicyViolation>(`/policy-violations/${id}`, options, "PATCH", {
    status,
    ...(note ? { note } : {}),
  });
}

/**
 * POST /api/v1/policy/evaluate
 *
 * Requests a compliance pass and returns once it is *accepted*, not once it
 * finishes: the server schedules it through the same coalescing queue its own
 * passes use and answers 202. Poll the summary and watch `lastEvaluatedAt`.
 */
export function evaluatePolicies(
  options?: RequestOptions,
): Promise<PolicyEvaluateAccepted> {
  return request<PolicyEvaluateAccepted>("/policy/evaluate", options, "POST", {});
}

/* ----------------------------------------------------- image intelligence -- */

/**
 * Image intelligence.
 *
 * Three reads and one write, and the write SCHEDULES a metadata collection
 * pass. There is deliberately no function here that pulls, deletes, prunes, or
 * applies an update — HarborMaster has no such capability and the API exposes no
 * such endpoint.
 *
 * # No destination crosses this boundary
 *
 * None of these functions accepts a registry, a host, a URL, or a scheme, and
 * `refreshImageMetadata` takes no argument at all. Registry destinations come
 * only from image references the inventory already holds.
 */

/** Builds the updates list query string from closed vocabularies. */
function buildImageUpdateQuery(query: ImageUpdateQuery): string {
  const params = new URLSearchParams();

  if (query.page && query.page > 1) params.set("page", String(query.page));
  if (query.pageSize) params.set("pageSize", String(query.pageSize));
  if (query.search) params.set("search", query.search);
  if (query.sort) params.set("sort", query.sort);
  if (query.order) params.set("order", query.order);
  // Sent only when explicitly set, so the server's own default stands otherwise
  // and the two cannot disagree about what "default" means.
  if (query.updatesOnly !== undefined) {
    params.set("updatesOnly", String(query.updatesOnly));
  }
  if (query.inUseOnly !== undefined) {
    params.set("inUseOnly", String(query.inUseOnly));
  }

  for (const update of query.update ?? []) params.append("update", update);
  for (const status of query.status ?? []) params.append("status", status);
  // A registry filter narrows the stored column. It is matched exactly against
  // a value HarborMaster itself derived, and cannot introduce a destination.
  for (const registry of query.registry ?? []) params.append("registry", registry);

  const rendered = params.toString();
  return rendered ? `?${rendered}` : "";
}

/** GET /api/v1/images/updates */
export function listImageUpdates(
  query: ImageUpdateQuery = {},
  options?: RequestOptions,
): Promise<ImageUpdatesResponse> {
  return request<ImageUpdatesResponse>(
    `/images/updates${buildImageUpdateQuery(query)}`,
    options,
  );
}

/**
 * GET /api/v1/images/{id}
 *
 * Returns the local image together with what the registry reports about every
 * reference resolving to it. A strict superset of the pre-existing response.
 */
export function getImageDetail(
  id: string,
  options?: RequestOptions,
): Promise<ImageDetail> {
  return request<ImageDetail>(`/images/${encodeURIComponent(id)}`, options);
}

/** GET /api/v1/images/{id}/history */
export function getImageHistory(
  id: string,
  page = 1,
  pageSize = 25,
  options?: RequestOptions,
): Promise<ImageHistoryResponse> {
  const params = new URLSearchParams();
  if (page > 1) params.set("page", String(page));
  params.set("pageSize", String(pageSize));

  return request<ImageHistoryResponse>(
    `/images/${encodeURIComponent(id)}/history?${params.toString()}`,
    options,
  );
}

/**
 * POST /api/v1/images/refresh
 *
 * Schedules a metadata collection pass and returns once it is *accepted*, not
 * once it finishes: the server answers 202 and the pass runs in the background,
 * bounded by the same concurrency limits every scheduled pass uses.
 *
 * **It takes no argument, and that is deliberate.** There is no target, no
 * registry, and no reference — the pass covers whatever the inventory already
 * holds.
 */
export function refreshImageMetadata(
  options?: RequestOptions,
): Promise<ImageRefreshAccepted> {
  return request<ImageRefreshAccepted>("/images/refresh", options, "POST");
}

/**
 * Change plans.
 *
 * Three reads and one write, and the write GENERATES HarborMaster's own
 * analysis of HarborMaster's own database. There is deliberately no function
 * here that applies, executes, approves, or schedules a change — HarborMaster
 * has no such capability and the API exposes no such endpoint.
 *
 * There is also no update and no delete. A plan records what was believed at
 * one moment; a function that edited one would destroy exactly the property
 * that makes plans worth keeping.
 */

/** Builds the plan listing query string from closed vocabularies. */
function buildPlanQuery(query: PlanQuery): string {
  const params = new URLSearchParams();

  if (query.page && query.page > 1) params.set("page", String(query.page));
  if (query.pageSize) params.set("pageSize", String(query.pageSize));
  if (query.sort) params.set("sort", query.sort);
  if (query.order) params.set("order", query.order);
  // Sent only when explicitly set, so the server's own default stands otherwise
  // and the two cannot disagree about what "default" means.
  if (query.currentOnly !== undefined) {
    params.set("currentOnly", String(query.currentOnly));
  }
  if (query.minRisk !== undefined && query.minRisk > 0) {
    params.set("minRisk", String(query.minRisk));
  }

  for (const band of query.band ?? []) params.append("band", band);
  for (const value of query.recommendation ?? []) params.append("recommendation", value);
  for (const update of query.update ?? []) params.append("update", update);

  const rendered = params.toString();
  return rendered ? `?${rendered}` : "";
}

/** GET /api/v1/plans */
export function listChangePlans(
  query: PlanQuery = {},
  options?: RequestOptions,
): Promise<PlanListResponse> {
  return request<PlanListResponse>(`/plans${buildPlanQuery(query)}`, options);
}

/** GET /api/v1/plans/{id} */
export function getChangePlan(
  planId: string,
  options?: RequestOptions,
): Promise<ChangePlan> {
  return request<ChangePlan>(`/plans/${encodeURIComponent(planId)}`, options);
}

/**
 * GET /api/v1/plans/container/{id}
 *
 * The container's current plan and the history that led to it — the reasoning
 * timeline.
 */
export function getContainerPlans(
  containerId: string,
  page = 1,
  pageSize = 25,
  options?: RequestOptions,
): Promise<PlanContainerResponse> {
  const params = new URLSearchParams();
  if (page > 1) params.set("page", String(page));
  params.set("pageSize", String(pageSize));

  return request<PlanContainerResponse>(
    `/plans/container/${encodeURIComponent(containerId)}?${params.toString()}`,
    options,
  );
}

/**
 * POST /api/v1/plans/generate
 *
 * Schedules a plan generation pass and returns once it is *accepted*, not once
 * it finishes: the server answers 202 and the pass runs in the background.
 *
 * **It generates ANALYSIS.** Nothing is pulled, no container is touched, and no
 * change is scheduled. It takes no argument because there is no target to
 * supply.
 */
export function generateChangePlans(
  options?: RequestOptions,
): Promise<PlanGenerateAccepted> {
  return request<PlanGenerateAccepted>("/plans/generate", options, "POST");
}

/**
 * Image acquisition.
 *
 * THE ONLY FUNCTIONS IN THIS CLIENT THAT CHANGE THE DOCKER HOST, and the change
 * is one thing: downloading an approved, digest-pinned image into the local
 * store.
 *
 * **No container is changed by any of them.** There is deliberately no function
 * that applies an image, recreates a container, restores one, or removes an
 * image — HarborMaster has no such capability and the API exposes no such
 * endpoint.
 *
 * # No target crosses this boundary
 *
 * `requestAcquisition` takes a PLAN ID. There is no registry, repository,
 * digest, tag, or pull option parameter anywhere below; the target is derived
 * server-side from the plan and revalidated immediately before the download.
 */

/** Builds the acquisition listing query string from closed vocabularies. */
function buildAcquisitionQuery(query: AcquisitionQuery): string {
  const params = new URLSearchParams();

  if (query.page && query.page > 1) params.set("page", String(query.page));
  if (query.pageSize) params.set("pageSize", String(query.pageSize));
  if (query.sort) params.set("sort", query.sort);
  if (query.order) params.set("order", query.order);
  if (query.activeOnly !== undefined) {
    params.set("activeOnly", String(query.activeOnly));
  }

  for (const state of query.state ?? []) params.append("state", state);
  for (const failure of query.failure ?? []) params.append("failure", failure);

  const rendered = params.toString();
  return rendered ? `?${rendered}` : "";
}

/** GET /api/v1/acquisitions */
export function listAcquisitions(
  query: AcquisitionQuery = {},
  options?: RequestOptions,
): Promise<AcquisitionListResponse> {
  return request<AcquisitionListResponse>(
    `/acquisitions${buildAcquisitionQuery(query)}`,
    options,
  );
}

/** GET /api/v1/acquisitions/{id} */
export function getAcquisition(
  acquisitionId: string,
  options?: RequestOptions,
): Promise<AcquisitionDetailResponse> {
  return request<AcquisitionDetailResponse>(
    `/acquisitions/${encodeURIComponent(acquisitionId)}`,
    options,
  );
}

/**
 * POST /api/v1/acquisitions
 *
 * Asks HarborMaster to download the image an approved change plan names.
 * Returns once the request is *accepted*, not once the download finishes: the
 * server answers 202 and the transfer runs in the background.
 *
 * **It changes no container.** The image lands in the local store; acting on it
 * afterwards is an operator's job with their own tooling.
 */
export function requestAcquisition(
  body: AcquisitionRequest,
  options?: RequestOptions,
): Promise<Acquisition> {
  return request<Acquisition>("/acquisitions", options, "POST", body);
}

/**
 * POST /api/v1/acquisitions/{id}/cancel
 *
 * Stops work HarborMaster is doing. It changes nothing on the host beyond
 * ending a transfer partway, which leaves the daemon's image store as it was.
 */
export function cancelAcquisition(
  acquisitionId: string,
  options?: RequestOptions,
): Promise<Acquisition> {
  return request<Acquisition>(
    `/acquisitions/${encodeURIComponent(acquisitionId)}/cancel`,
    options,
    "POST",
  );
}

/**
 * Container recreation.
 *
 * # The only calls in this client that change something RUNNING
 *
 * `requestExecution` takes an ACQUISITION ID. There is no container, image,
 * digest, command, mount, capability, timeout, or force parameter anywhere
 * below, and the server rejects unknown fields rather than ignoring them.
 * Everything about what will happen is derived server-side from the
 * acquisition, and revalidated immediately before the first mutation.
 *
 * There is deliberately no restore, retry, or force call, and no server
 * capability behind one. Manual rollback exists and is its own resource below:
 * a separate route, a separate permission, a separate audit record.
 */

/** Builds the execution listing query string from closed vocabularies. */
function buildExecutionQuery(query: ExecutionQuery): string {
  const params = new URLSearchParams();

  if (query.page && query.page > 1) params.set("page", String(query.page));
  if (query.pageSize) params.set("pageSize", String(query.pageSize));
  if (query.sort) params.set("sort", query.sort);
  if (query.order) params.set("order", query.order);
  if (query.activeOnly !== undefined) {
    params.set("activeOnly", String(query.activeOnly));
  }
  if (query.needsAttention !== undefined) {
    params.set("needsAttention", String(query.needsAttention));
  }

  for (const state of query.state ?? []) params.append("state", state);
  for (const failure of query.failure ?? []) params.append("failure", failure);

  const rendered = params.toString();
  return rendered ? `?${rendered}` : "";
}

/** GET /api/v1/executions */
export function listExecutions(
  query: ExecutionQuery = {},
  options?: RequestOptions,
): Promise<ExecutionListResponse> {
  return request<ExecutionListResponse>(
    `/executions${buildExecutionQuery(query)}`,
    options,
  );
}

/** GET /api/v1/executions/{id} */
export function getExecution(
  executionId: string,
  options?: RequestOptions,
): Promise<ExecutionDetailResponse> {
  return request<ExecutionDetailResponse>(
    `/executions/${encodeURIComponent(executionId)}`,
    options,
  );
}

/**
 * POST /api/v1/executions
 *
 * Asks HarborMaster to recreate one container on an image it has already
 * downloaded and verified. Returns once the request is *accepted*, not once the
 * recreation finishes: the server answers 202 and the work runs in the
 * background.
 *
 * **This stops a running container.** The original is parked rather than
 * removed, and it is removed only after the replacement passes every proof.
 */
export function requestExecution(
  body: ExecutionRequest,
  options?: RequestOptions,
): Promise<Execution> {
  return request<Execution>("/executions", options, "POST", body);
}

/**
 * POST /api/v1/executions/{id}/cancel
 *
 * Stops a recreation that has not yet changed anything. Past the mutation point
 * the server answers 409: a recreation that has stopped a container must reach
 * a recorded conclusion.
 */
export function cancelExecution(
  executionId: string,
  options?: RequestOptions,
): Promise<Execution> {
  return request<Execution>(
    `/executions/${encodeURIComponent(executionId)}/cancel`,
    options,
    "POST",
  );
}

/**
 * Manual rollback.
 *
 * # The second pair of calls that change something RUNNING
 *
 * `requestRollback` takes an EXECUTION ID. There is no container id, name,
 * image, snapshot, or Docker option anywhere below, and the server rejects
 * unknown fields rather than ignoring them. Both container identities are read
 * from HarborMaster's own record of the recreation and re-verified against the
 * live host before anything moves.
 *
 * There is deliberately no remove call. A rollback stops and parks the
 * replacement and leaves it there as the evidence of why the recreation was
 * backed out; the server holds no capability that could destroy it.
 */

/** Builds the rollback listing query string from closed vocabularies. */
function buildRollbackQuery(query: RollbackQuery): string {
  const params = new URLSearchParams();

  if (query.page && query.page > 1) params.set("page", String(query.page));
  if (query.pageSize) params.set("pageSize", String(query.pageSize));
  if (query.executionId) params.set("executionId", query.executionId);
  if (query.activeOnly !== undefined) {
    params.set("activeOnly", String(query.activeOnly));
  }
  if (query.needsAttention !== undefined) {
    params.set("needsAttention", String(query.needsAttention));
  }

  for (const state of query.state ?? []) params.append("state", state);
  for (const failure of query.failure ?? []) params.append("failure", failure);

  const rendered = params.toString();
  return rendered ? `?${rendered}` : "";
}

/** GET /api/v1/rollbacks */
export function listRollbacks(
  query: RollbackQuery = {},
  options?: RequestOptions,
): Promise<RollbackListResponse> {
  return request<RollbackListResponse>(
    `/rollbacks${buildRollbackQuery(query)}`,
    options,
  );
}

/** GET /api/v1/rollbacks/{id} */
export function getRollback(
  rollbackId: string,
  options?: RequestOptions,
): Promise<RollbackDetailResponse> {
  return request<RollbackDetailResponse>(
    `/rollbacks/${encodeURIComponent(rollbackId)}`,
    options,
  );
}

/**
 * POST /api/v1/rollbacks
 *
 * Asks HarborMaster to undo one recreation. Returns once the request is
 * *accepted*, not once the rollback finishes: the server answers 202 and the
 * work runs in the background.
 *
 * **This stops a running container and starts a different one in its place.**
 * There is a gap between the two, so the container is unavailable while the
 * rollback runs.
 */
export function requestRollback(
  body: RollbackRequest,
  options?: RequestOptions,
): Promise<Rollback> {
  return request<Rollback>("/rollbacks", options, "POST", body);
}

/**
 * POST /api/v1/rollbacks/{id}/cancel
 *
 * Stops a rollback that has not yet changed anything. Past the mutation point
 * the server answers 409: a rollback that has stopped a container must reach a
 * recorded conclusion.
 */
export function cancelRollback(
  rollbackId: string,
  options?: RequestOptions,
): Promise<Rollback> {
  return request<Rollback>(
    `/rollbacks/${encodeURIComponent(rollbackId)}/cancel`,
    options,
    "POST",
  );
}

/* ------------------------------------------------- identity and sessions -- */

/**
 * Authentication.
 *
 * # No token is ever handled here
 *
 * `login` and `bootstrap` return a CSRF token and a user; the SESSION token
 * arrives as a `Set-Cookie` header the browser stores and this code cannot see.
 * There is deliberately no function that reads, stores, or sends a session
 * token, because there is no way for this code to obtain one.
 */

/** GET /api/v1/auth/bootstrap — whether this installation has been claimed. */
export function getBootstrapStatus(
  options?: RequestOptions,
): Promise<BootstrapStatus> {
  return request<BootstrapStatus>("/auth/bootstrap", options);
}

/**
 * POST /api/v1/auth/bootstrap
 *
 * Creates the first administrator. Reachable only while the installation is
 * unclaimed; afterwards the endpoint answers 404, because for that installation
 * it genuinely no longer exists.
 *
 * The token is the one-time value printed in the server log at startup, so
 * claiming requires access to the log rather than merely reaching the port.
 */
export function bootstrapInstallation(
  body: BootstrapRequest,
  options?: RequestOptions,
): Promise<SessionResponse> {
  return request<SessionResponse>("/auth/bootstrap", options, "POST", body);
}

/**
 * POST /api/v1/auth/login
 *
 * Every credential failure -- unknown username, wrong password, disabled
 * account -- answers 401 with the same message. That is deliberate on the
 * server's side, and this client must not try to distinguish them either.
 */
export function login(
  body: LoginRequest,
  options?: RequestOptions,
): Promise<SessionResponse> {
  return request<SessionResponse>("/auth/login", options, "POST", body);
}

/** POST /api/v1/auth/logout — ends this session only. */
export function logout(options?: RequestOptions): Promise<void> {
  return request<void>("/auth/logout", options, "POST");
}

/** GET /api/v1/auth/session — the caller's identity and a fresh CSRF token. */
export function getSession(options?: RequestOptions): Promise<SessionResponse> {
  return request<SessionResponse>("/auth/session", options);
}

/**
 * POST /api/v1/auth/password
 *
 * Requires the current password even though the request is authenticated: a
 * session is possession, the current password is knowledge, and requiring both
 * stops a stolen session becoming permanent account control.
 *
 * The session ROTATES. Every session on the account ends and a new cookie
 * replaces this one, so the returned CSRF token must be installed or the next
 * write will be refused.
 */
export function changePassword(
  body: ChangePasswordRequest,
  options?: RequestOptions,
): Promise<SessionResponse> {
  return request<SessionResponse>("/auth/password", options, "POST", body);
}

/** GET /api/v1/auth/sessions — the caller's OWN live sessions. */
export function listOwnSessions(
  options?: RequestOptions,
): Promise<SessionListResponse> {
  return request<SessionListResponse>("/auth/sessions", options);
}

/** POST /api/v1/auth/sessions/{id}/revoke — end one of the caller's sessions. */
export function revokeOwnSession(
  sessionId: string,
  options?: RequestOptions,
): Promise<void> {
  return request<void>(
    `/auth/sessions/${encodeURIComponent(sessionId)}/revoke`,
    options,
    "POST",
  );
}

/* ----------------------------------------------------- user administration -- */

/**
 * Account administration.
 *
 * Every function here needs `user:manage`, which only an administrator holds.
 * Hiding the UI is not the control -- the server refuses regardless -- but a
 * button that always fails is a bad interface, so the pages check too.
 *
 * Note the absence of a delete: disabling preserves the history an audit record
 * depends on, and an account that never existed is not the same as one that was
 * turned off.
 */

/** GET /api/v1/users */
export function listUsers(
  page = 1,
  pageSize = 50,
  options?: RequestOptions,
): Promise<UserListResponse> {
  const params = new URLSearchParams();
  if (page > 1) params.set("page", String(page));
  params.set("pageSize", String(pageSize));
  return request<UserListResponse>(`/users?${params.toString()}`, options);
}

/** GET /api/v1/users/{id} */
export function getUser(
  userId: string,
  options?: RequestOptions,
): Promise<PublicUser> {
  return request<PublicUser>(`/users/${encodeURIComponent(userId)}`, options);
}

/**
 * POST /api/v1/users
 *
 * Omitting the password makes the server generate one and return it ONCE. That
 * is the safer default: an administrator never has to invent a password for
 * somebody else, and never has one they chose sitting in their clipboard.
 */
export function createUser(
  body: CreateUserRequest,
  options?: RequestOptions,
): Promise<CreatedUser> {
  return request<CreatedUser>("/users", options, "POST", body);
}

/**
 * PATCH /api/v1/users/{id}
 *
 * Changes a role, a status, or both. The server refuses an administrator
 * modifying their own account and refuses removing the last one.
 */
export function updateUser(
  userId: string,
  body: UpdateUserRequest,
  options?: RequestOptions,
): Promise<PublicUser> {
  return request<PublicUser>(
    `/users/${encodeURIComponent(userId)}`,
    options,
    "PATCH",
    body,
  );
}

/**
 * POST /api/v1/users/{id}/password-reset
 *
 * Sets a generated temporary password and returns it once, revoking every
 * session on the account. There is no parameter for choosing the password: an
 * administrator who picks one picks one they know.
 */
export function resetUserPassword(
  userId: string,
  options?: RequestOptions,
): Promise<{ temporaryPassword: string }> {
  return request<{ temporaryPassword: string }>(
    `/users/${encodeURIComponent(userId)}/password-reset`,
    options,
    "POST",
  );
}

/**
 * GET /api/v1/roles
 *
 * The role catalogue, fetched rather than hardcoded so the picker and the
 * authorization middleware read the same source of truth.
 */
export function getRoles(options?: RequestOptions): Promise<RoleCatalogue> {
  return request<RoleCatalogue>("/roles", options);
}

/* --------------------------------------------------- the security audit log -- */

/** Builds the audit query string from closed vocabularies. */
function buildAuditQuery(query: AuditQuery): string {
  const params = new URLSearchParams();

  if (query.page && query.page > 1) params.set("page", String(query.page));
  if (query.pageSize) params.set("pageSize", String(query.pageSize));
  if (query.actorUserId) params.set("actorUserId", query.actorUserId);
  if (query.targetType) params.set("targetType", query.targetType);
  if (query.targetId) params.set("targetId", query.targetId);
  if (query.since) params.set("since", query.since);
  if (query.securityOnly !== undefined) {
    params.set("securityOnly", String(query.securityOnly));
  }

  for (const action of query.action ?? []) params.append("action", action);
  for (const outcome of query.outcome ?? []) params.append("outcome", outcome);

  const rendered = params.toString();
  return rendered ? `?${rendered}` : "";
}

/** GET /api/v1/audit — needs `audit:read`. */
export function listAuditEvents(
  query: AuditQuery = {},
  options?: RequestOptions,
): Promise<AuditListResponse> {
  return request<AuditListResponse>(`/audit${buildAuditQuery(query)}`, options);
}

/** GET /api/v1/audit/summary */
export function getAuditSummary(options?: RequestOptions): Promise<AuditSummary> {
  return request<AuditSummary>("/audit/summary", options);
}

/* ----------------------------------------------------------- automation -- */

/**
 * The update engine, and the rules that drive it.
 *
 * **This is the only part of the API that can cause a host change nobody is
 * watching.** Read the request bodies below and note what they cannot say:
 * running a pass takes a boolean, approving takes a run id and a container name
 * that must match a decision the server already recorded, and pausing takes a
 * container name the inventory already knows.
 *
 * There is no function here that takes an image, a tag, a digest, or a
 * registry, and no server route that would accept one.
 *
 * There is also no call that removes a policy row: `archiveUpdatePolicy`
 * withdraws a rule and keeps the record of what it caused, because an auditor
 * asking what changed the estate last quarter must not get a different answer
 * because somebody tidied up this quarter.
 */

/** Builds the update policy listing query string from closed vocabularies. */
function buildUpdatePolicyQuery(query: UpdatePolicyQuery): string {
  const params = new URLSearchParams();

  if (query.page && query.page > 1) params.set("page", String(query.page));
  if (query.pageSize) params.set("pageSize", String(query.pageSize));
  if (query.sort) params.set("sort", query.sort);
  if (query.order) params.set("order", query.order);
  if (query.enabled !== undefined) params.set("enabled", String(query.enabled));
  if (query.includeArchived !== undefined) {
    params.set("includeArchived", String(query.includeArchived));
  }
  if (query.search) params.set("search", query.search);

  for (const mode of query.mode ?? []) params.append("mode", mode);

  const rendered = params.toString();
  return rendered ? `?${rendered}` : "";
}

/** GET /api/v1/update-policies */
export function listUpdatePolicies(
  query: UpdatePolicyQuery = {},
  options?: RequestOptions,
): Promise<UpdatePolicyListResponse> {
  return request<UpdatePolicyListResponse>(
    `/update-policies${buildUpdatePolicyQuery(query)}`,
    options,
  );
}

/** GET /api/v1/update-policies/{id} */
export function getUpdatePolicy(
  policyId: string,
  options?: RequestOptions,
): Promise<UpdatePolicyResult> {
  return request<UpdatePolicyResult>(
    `/update-policies/${encodeURIComponent(policyId)}`,
    options,
  );
}

/**
 * POST /api/v1/update-policies — needs `automation:manage`.
 *
 * Creates a standing rule that lets HarborMaster change containers unattended.
 * The response carries `warnings`: settings that are legal but worth seeing.
 */
export function createUpdatePolicy(
  body: UpdatePolicyRequest,
  options?: RequestOptions,
): Promise<UpdatePolicyResult> {
  return request<UpdatePolicyResult>("/update-policies", options, "POST", body);
}

/** PATCH /api/v1/update-policies/{id} — needs `automation:manage`. */
export function updateUpdatePolicy(
  policyId: string,
  body: UpdatePolicyRequest,
  options?: RequestOptions,
): Promise<UpdatePolicyResult> {
  return request<UpdatePolicyResult>(
    `/update-policies/${encodeURIComponent(policyId)}`,
    options,
    "PATCH",
    body,
  );
}

/**
 * DELETE /api/v1/update-policies/{id} — needs `automation:manage`.
 *
 * ARCHIVES. The rule stops governing immediately and the history it caused
 * remains.
 */
export function archiveUpdatePolicy(
  policyId: string,
  options?: RequestOptions,
): Promise<void> {
  return request<void>(
    `/update-policies/${encodeURIComponent(policyId)}`,
    options,
    "DELETE",
  );
}

/** GET /api/v1/automation */
export function getAutomationStatus(
  options?: RequestOptions,
): Promise<AutomationStatusResponse> {
  return request<AutomationStatusResponse>("/automation", options);
}

/**
 * GET /api/v1/automation/upcoming
 *
 * What the next pass WOULD do, without doing any of it. A read: it writes no
 * run, no decisions, and reaches no service.
 */
export function getAutomationUpcoming(
  options?: RequestOptions,
): Promise<AutomationUpcomingResponse> {
  return request<AutomationUpcomingResponse>("/automation/upcoming", options);
}

/** Builds the run listing query string from closed vocabularies. */
function buildAutomationRunQuery(query: AutomationRunQuery): string {
  const params = new URLSearchParams();

  if (query.page && query.page > 1) params.set("page", String(query.page));
  if (query.pageSize) params.set("pageSize", String(query.pageSize));
  if (query.acted !== undefined) params.set("acted", String(query.acted));

  for (const state of query.state ?? []) params.append("state", state);
  for (const trigger of query.trigger ?? []) params.append("trigger", trigger);

  const rendered = params.toString();
  return rendered ? `?${rendered}` : "";
}

/** GET /api/v1/automation/runs */
export function listAutomationRuns(
  query: AutomationRunQuery = {},
  options?: RequestOptions,
): Promise<AutomationRunListResponse> {
  return request<AutomationRunListResponse>(
    `/automation/runs${buildAutomationRunQuery(query)}`,
    options,
  );
}

/** GET /api/v1/automation/runs/{id} */
export function getAutomationRun(
  runId: string,
  options?: RequestOptions,
): Promise<AutomationRunDetailResponse> {
  return request<AutomationRunDetailResponse>(
    `/automation/runs/${encodeURIComponent(runId)}`,
    options,
  );
}

/**
 * GET /api/v1/automation/approvals — the decisions waiting for a person.
 *
 * A read. Approving is a separate call with its own permission; this only says
 * what is outstanding, so an operator can find it without opening the archived
 * pass that produced it.
 */
export function listAutomationApprovals(
  options?: RequestOptions & {
    page?: number;
    pageSize?: number;
    /** Narrow to one container, for a container's own page. */
    container?: string;
  },
): Promise<AutomationApprovalListResponse> {
  const params = new URLSearchParams();
  if (options?.page) params.set("page", String(options.page));
  if (options?.pageSize) params.set("pageSize", String(options.pageSize));
  if (options?.container) params.set("container", options.container);
  const suffix = params.toString() ? `?${params}` : "";
  return request<AutomationApprovalListResponse>(
    `/automation/approvals${suffix}`,
    options,
  );
}

/** GET /api/v1/automation/paused */
export function listAutomationPauses(
  options?: RequestOptions & { all?: boolean },
): Promise<AutomationPauseListResponse> {
  const suffix = options?.all ? "?all=true" : "";
  return request<AutomationPauseListResponse>(
    `/automation/paused${suffix}`,
    options,
  );
}

/**
 * POST /api/v1/automation/run — needs `automation:run`.
 *
 * Starts a decision pass now. **One field, and it is a boolean.** With
 * `dryRun` the pass decides everything and changes nothing; the run is still
 * recorded, so an operator can read afterwards what would have happened.
 *
 * Resolves once the pass has DECIDED, not once the work it submitted finishes.
 */
export function runAutomationPass(
  dryRun: boolean,
  options?: RequestOptions,
): Promise<AutomationRunDetailResponse> {
  return request<AutomationRunDetailResponse>(
    "/automation/run",
    options,
    "POST",
    { dryRun },
  );
}

/**
 * POST /api/v1/automation/approve — needs `automation:approve`.
 *
 * Releases a decision a policy held for a person. **The two fields SELECT one
 * of HarborMaster's own held decisions; they do not describe one.** The server
 * re-derives the plan and refuses if it has moved on since the decision.
 */
export function approveAutomationDecision(
  runId: string,
  containerName: string,
  options?: RequestOptions,
): Promise<AutomationDecision> {
  return request<AutomationDecision>("/automation/approve", options, "POST", {
    runId,
    containerName,
  });
}

/** POST /api/v1/automation/pause — needs `automation:pause`. */
export function pauseAutomation(
  containerName: string,
  reason: string,
  options?: RequestOptions,
): Promise<PausedContainer> {
  return request<PausedContainer>("/automation/pause", options, "POST", {
    containerName,
    ...(reason ? { reason } : {}),
  });
}

/**
 * POST /api/v1/automation/resume — needs `automation:pause`.
 *
 * Records WHO cleared it and resets the container's failure counters: an
 * operator who investigated and fixed the problem must not be one failure away
 * from the same pause.
 */
export function resumeAutomation(
  containerName: string,
  options?: RequestOptions,
): Promise<void> {
  return request<void>("/automation/resume", options, "POST", {
    containerName,
  });
}

// ------------------------------------------------------------ notifications --

/**
 * The notification endpoints.
 *
 * # The webhook URL travels one way
 *
 * `createNotificationDestination` and `updateNotificationDestination` SEND a
 * URL. Nothing returns one. Every read below resolves to a
 * `NotificationDestination`, whose `endpoint` is a scheme and host — there is
 * no field on that type for a credential and no endpoint that produces one.
 *
 * # There is no target in a test send
 *
 * `testNotificationDestination` takes an identifier HarborMaster issued and no
 * body of its own. A function with nowhere to put a URL cannot be talked into
 * pointing this host's outbound egress somewhere new.
 */

function buildNotificationConfigQuery(query: NotificationConfigQuery): string {
  const params = new URLSearchParams();
  if (query.page && query.page > 1) params.set("page", String(query.page));
  if (query.pageSize) params.set("pageSize", String(query.pageSize));
  // Sent only when explicitly set, so the server's own default stands
  // otherwise and the two cannot disagree about what "default" means.
  if (query.includeArchived !== undefined) {
    params.set("includeArchived", String(query.includeArchived));
  }
  const rendered = params.toString();
  return rendered ? `?${rendered}` : "";
}

function buildDeliveryQuery(query: DeliveryQuery): string {
  const params = new URLSearchParams();
  if (query.page && query.page > 1) params.set("page", String(query.page));
  if (query.pageSize) params.set("pageSize", String(query.pageSize));
  if (query.destinationId) params.set("destinationId", query.destinationId);
  if (query.container) params.set("container", query.container);
  if (query.failed !== undefined) params.set("failed", String(query.failed));

  for (const result of query.result ?? []) params.append("result", result);
  for (const event of query.event ?? []) params.append("event", event);

  const rendered = params.toString();
  return rendered ? `?${rendered}` : "";
}

/** GET /api/v1/notifications — state, counts, and the closed vocabularies. */
export function getNotificationStatus(
  options?: RequestOptions,
): Promise<NotificationStatus> {
  return request<NotificationStatus>("/notifications", options);
}

/** GET /api/v1/notifications/destinations */
export function listNotificationDestinations(
  query: NotificationConfigQuery = {},
  options?: RequestOptions,
): Promise<NotificationDestinationListResponse> {
  return request<NotificationDestinationListResponse>(
    `/notifications/destinations${buildNotificationConfigQuery(query)}`,
    options,
  );
}

/** GET /api/v1/notifications/destinations/{id} — the public record. */
export function getNotificationDestination(
  destinationId: string,
  options?: RequestOptions,
): Promise<NotificationDestination> {
  return request<NotificationDestination>(
    `/notifications/destinations/${encodeURIComponent(destinationId)}`,
    options,
  );
}

/**
 * POST /api/v1/notifications/destinations — needs `notification:manage`.
 *
 * The body carries a CREDENTIAL. The response does not.
 */
export function createNotificationDestination(
  body: NotificationDestinationRequest,
  options?: RequestOptions,
): Promise<NotificationDestinationResult> {
  return request<NotificationDestinationResult>(
    "/notifications/destinations",
    options,
    "POST",
    body,
  );
}

/**
 * PATCH /api/v1/notifications/destinations/{id}
 *
 * **Omitting `url` keeps the stored credential.** Supplying one replaces it.
 * `channel` is refused: a destination whose stored credential is the wrong
 * shape for what it now is would be a state every validation ran before it
 * could exist.
 */
export function updateNotificationDestination(
  destinationId: string,
  body: NotificationDestinationRequest,
  options?: RequestOptions,
): Promise<NotificationDestinationResult> {
  return request<NotificationDestinationResult>(
    `/notifications/destinations/${encodeURIComponent(destinationId)}`,
    options,
    "PATCH",
    body,
  );
}

/**
 * DELETE /api/v1/notifications/destinations/{id}
 *
 * Archives rather than destroys: delivery records reference the row. The
 * stored credential does NOT survive — an archived destination cannot send, so
 * keeping its URL would be keeping a credential for no reason. Answers 204, or
 * 409 while a rule still routes here.
 */
export function archiveNotificationDestination(
  destinationId: string,
  options?: RequestOptions,
): Promise<void> {
  return request<void>(
    `/notifications/destinations/${encodeURIComponent(destinationId)}`,
    options,
    "DELETE",
  );
}

/**
 * POST /api/v1/notifications/destinations/{id}/test
 *
 * The destination is named by an identifier HarborMaster issued, and the
 * message is a fixed sentence — there is nowhere in this request to put a URL,
 * an address, or text. Queued rather than sent inline, travelling the same path
 * as a real notification, so its outcome appears in the delivery history.
 */
export function testNotificationDestination(
  destinationId: string,
  options?: RequestOptions,
): Promise<TestNotificationResponse> {
  return request<TestNotificationResponse>(
    `/notifications/destinations/${encodeURIComponent(destinationId)}/test`,
    options,
    "POST",
    {},
  );
}

/** GET /api/v1/notifications/rules */
export function listNotificationRules(
  query: NotificationConfigQuery = {},
  options?: RequestOptions,
): Promise<NotificationRuleListResponse> {
  return request<NotificationRuleListResponse>(
    `/notifications/rules${buildNotificationConfigQuery(query)}`,
    options,
  );
}

/** POST /api/v1/notifications/rules — needs `notification:manage`. */
export function createNotificationRule(
  body: NotificationRuleRequest,
  options?: RequestOptions,
): Promise<NotificationRuleResult> {
  return request<NotificationRuleResult>(
    "/notifications/rules",
    options,
    "POST",
    body,
  );
}

/** PATCH /api/v1/notifications/rules/{id} — omitted fields are left alone. */
export function updateNotificationRule(
  ruleId: string,
  body: NotificationRuleRequest,
  options?: RequestOptions,
): Promise<NotificationRuleResult> {
  return request<NotificationRuleResult>(
    `/notifications/rules/${encodeURIComponent(ruleId)}`,
    options,
    "PATCH",
    body,
  );
}

/** DELETE /api/v1/notifications/rules/{id} — archives. Answers 204. */
export function archiveNotificationRule(
  ruleId: string,
  options?: RequestOptions,
): Promise<void> {
  return request<void>(
    `/notifications/rules/${encodeURIComponent(ruleId)}`,
    options,
    "DELETE",
  );
}

/** GET /api/v1/notifications/deliveries — needs only `notification:read`. */
export function listNotificationDeliveries(
  query: DeliveryQuery = {},
  options?: RequestOptions,
): Promise<NotificationDeliveryListResponse> {
  return request<NotificationDeliveryListResponse>(
    `/notifications/deliveries${buildDeliveryQuery(query)}`,
    options,
  );
}

/** GET /api/v1/notifications/deliveries/{id} */
export function getNotificationDelivery(
  deliveryId: string,
  options?: RequestOptions,
): Promise<NotificationDelivery> {
  return request<NotificationDelivery>(
    `/notifications/deliveries/${encodeURIComponent(deliveryId)}`,
    options,
  );
}
