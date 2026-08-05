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

  let response: Response;
  try {
    response = await fetch(`${API_BASE}${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
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
