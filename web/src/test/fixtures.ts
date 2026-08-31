import { vi } from "vitest";
import type {
  ContainerDependencies,
  DependencyGraph,
  DependencyListing,
  DependencyMember,
  DependencyOperationListing,
  DependencyOperationSummary,
  WorkloadDependency,
} from "../api/dependencyTypes";

import type {
  ContainerDetail,
  ContainerAttention,
  ContainerListRow,
  ContainerBehaviorSummary,
  ContainerSummary,
  FilterOptions,
  ImageUsage,
  InventoryStatus,
  ListResponse,
  RawInspection,
} from "../api/inventoryTypes";
import type {
  DockerEvent,
  EventEngineStatus,
  EventFilterOptions,
} from "../api/eventTypes";
import type { HealthReport, VersionInfo } from "../api/types";
import type { ResourceState } from "../hooks/useApiResource";
import type { Role } from "../api/authTypes";
import { permissionsFor } from "./session";

export const healthyReport: HealthReport = {
  status: "healthy",
  database: { status: "up", latencyMs: 1 },
  docker: { status: "up", latencyMs: 4, version: "1.51" },
  checkedAt: "2026-08-03T09:20:11.482Z",
  uptimeSeconds: 120,
};

/**
 * The shell's health read, as a page receives it.
 *
 * `useHealth()` is called once at the shell level and passed down, so a page
 * test supplies the resource rather than a fetch stub. Every capability is on:
 * a test that cares about one being off overrides it.
 */
export function healthState(
  features: Partial<NonNullable<HealthReport["features"]>> = {},
): ResourceState<HealthReport> {
  return {
    status: "ready",
    data: {
      ...healthyReport,
      features: {
        inventory: true, events: true, snapshots: true, drift: true,
        policy: true, planner: true, imageIntel: true, acquisition: true,
        execution: true, rollback: true, automation: true,
        notifications: false, notificationsAllowPrivate: false,
        ...features,
      },
    },
    error: null,
    refresh: () => {},
    refreshing: false,
  } as unknown as ResourceState<HealthReport>;
}

export const buildInfo: VersionInfo = {
  version: "v0.2.0",
  commit: "9f2c1ab",
  buildDate: "2026-08-03T08:00:00Z",
  goVersion: "go1.26.5",
  platform: "linux/amd64",
};

export function inventoryStatus(overrides: Partial<InventoryStatus> = {}): InventoryStatus {
  return {
    enabled: true,
    runtime: "docker",
    docker: { status: "up", version: "1.51" },
    state: "succeeded",
    inProgress: false,
    generation: 4,
    checksum: "abcdef0123456789",
    lastAttempt: {
      generation: 4,
      trigger: "periodic",
      state: "succeeded",
      startedAt: "2026-08-03T09:00:00Z",
      finishedAt: "2026-08-03T09:00:02Z",
      durationMs: 2000,
      containersListed: 3,
      containersInspected: 3,
      containersFailed: 0,
      imagesInspected: 2,
      networksListed: 1,
      volumesListed: 1,
      warningCount: 0,
      checksum: "abcdef0123456789",
    },
    lastSuccess: {
      generation: 4,
      trigger: "periodic",
      state: "succeeded",
      startedAt: "2026-08-03T09:00:00Z",
      finishedAt: "2026-08-03T09:00:02Z",
      durationMs: 2000,
      containersListed: 3,
      containersInspected: 3,
      containersFailed: 0,
      imagesInspected: 2,
      networksListed: 1,
      volumesListed: 1,
      warningCount: 0,
      checksum: "abcdef0123456789",
    },
    counts: {
      containers: 3,
      absent: 0,
      running: 2,
      stopped: 1,
      paused: 0,
      restarting: 0,
      healthy: 1,
      unhealthy: 1,
      images: 2,
      networks: 1,
      volumes: 1,
      warnings: 0,
      byState: { running: 2, exited: 1 },
    },
    warnings: [],
    ...overrides,
  };
}

export function containerSummary(overrides: Partial<ContainerSummary> = {}): ContainerSummary {
  return {
    hostId: "local",
    id: "abcdef0123456789",
    shortId: "abcdef012345",
    name: "web",
    image: { raw: "nginx:1.27", repository: "nginx", tag: "1.27" },
    imageId: "sha256:img1",
    state: "running",
    status: "Up 2 hours",
    health: "healthy",
    createdAt: "2026-08-01T12:00:00Z",
    restartCount: 0,
    restartPolicy: { name: "unless-stopped" },
    compose: { managed: true, project: "shop", service: "web", oneOff: false },
    harbormaster: {},
    ports: [
      { containerPort: 80, protocol: "tcp", hostIp: "127.0.0.1", hostPort: 8080, published: true },
    ],
    present: true,
    firstSeenAt: "2026-08-01T12:00:00Z",
    lastSeenAt: "2026-08-03T09:00:00Z",
    generation: 4,
    warningCount: 0,
    ...overrides,
  };
}

/**
 * The attention block the server attaches to every list row.
 *
 * Defaults to the honest baseline -- assessed, following a tag, nothing to do
 * -- so a test that cares about a particular verdict says so and every other
 * test gets a row that is not accidentally alarming.
 */
export function containerAttention(
  overrides: Partial<ContainerAttention> = {},
): ContainerAttention {
  return {
    state: "upToDate",
    updateType: "none",
    recommendation: "proceed",
    tracking: "nginx:1.27",
    trackingKnown: true,
    awaitingApproval: false,
    automationPaused: false,
    openViolations: 0,
    openDrift: 0,
    ...overrides,
  };
}

/** A list row: a summary with its attention block. */
export function containerRow(
  summary: Partial<ContainerSummary> = {},
  attention: Partial<ContainerAttention> = {},
): ContainerListRow {
  return {
    ...containerSummary(summary),
    attention: containerAttention(attention),
  };
}

export function containerPage(
  items: (ContainerSummary | ContainerListRow)[] = [containerRow()],
  totalItems = items.length,
): ListResponse<ContainerListRow> {
  return {
    items: items.map((item) =>
      "attention" in item ? item : { ...item, attention: containerAttention() },
    ),
    pagination: {
      page: 1,
      pageSize: 25,
      totalItems,
      totalPages: Math.max(Math.ceil(totalItems / 25), 1),
      hasNext: totalItems > 25,
      hasPrevious: false,
    },
  };
}

/**
 * The saved-behaviour summary an estate with no overrides returns.
 *
 * Every field the schema declares `required`, including a count key for EVERY
 * behaviour. The server sends real zeros rather than omitting keys, so a
 * fixture that omitted them would let a page pass here and read `undefined` in
 * production.
 */
export function emptyBehaviorSummary(
  overrides: Partial<ContainerBehaviorSummary> = {},
): ContainerBehaviorSummary {
  return {
    items: [],
    counts: { automatic: 0, reviewFirst: 0, monitorOnly: 0 },
    total: 0,
    stale: 0,
    ...overrides,
  };
}

export function containerDetail(overrides: Partial<ContainerDetail> = {}): ContainerDetail {
  return {
    overview: containerSummary(),
    state: {
      state: "running",
      rawState: "running",
      status: "Up 2 hours",
      running: true,
      paused: false,
      restarting: false,
      dead: false,
      oomKilled: false,
      restartCount: 0,
      health: "healthy",
    },
    image: {
      id: "sha256:img1",
      shortId: "img1",
      repoTags: ["nginx:1.27"],
      repoDigests: [],
      size: 187000000,
      architecture: "amd64",
      os: "linux",
    },
    process: {
      hostname: "web",
      user: "1000:1000",
      workingDir: "/app",
      command: ["nginx", "-g", "daemon off;"],
      tty: false,
      stdinOpen: false,
    },
    environment: [
      { name: "PORT", value: "8080", sensitivity: "normal" },
      { name: "DB_PASSWORD", value: "********", sensitivity: "sensitive" },
    ],
    labels: [{ key: "app", value: "web", source: "user" }],
    ports: [
      { containerPort: 80, protocol: "tcp", hostIp: "127.0.0.1", hostPort: 8080, published: true },
      { containerPort: 443, protocol: "tcp", published: false },
    ],
    mounts: [
      { type: "volume", destination: "/data", volumeName: "shop_data", readOnly: false },
    ],
    networks: [
      { networkName: "frontend", ipv4Address: "172.20.0.2", aliases: ["web"] },
    ],
    resources: { memoryBytes: 536870912, cpuShares: 512 },
    security: {
      privileged: false,
      readonlyRootfs: true,
      noNewPrivileges: true,
      capDrop: ["ALL"],
    },
    logging: { driver: "json-file", options: [] },
    compose: { managed: true, project: "shop", service: "web", oneOff: false },
    harbormaster: {},
    warnings: [],
    ...overrides,
  };
}

export const filterOptions: FilterOptions = {
  states: ["created", "running", "paused", "restarting", "removing", "exited", "dead", "unknown"],
  health: ["none", "starting", "healthy", "unhealthy"],
  projects: ["shop", "blog"],
  images: ["nginx", "redis"],
  sortFields: ["created", "health", "image", "name", "project", "restartCount", "service", "started", "state", "lastSeen"],
};

export const rawInspection: RawInspection = {
  containerId: "abcdef0123456789",
  redacted: true,
  notice:
    "Sensitive values have been removed. This payload is for troubleshooting only and cannot be used to recreate the container exactly.",
  inspection: { Id: "abcdef0123456789", Config: { Env: ["DB_PASSWORD=********"] } },
};

export function imagePage(items: ImageUsage[] = [defaultImageUsage()]): ListResponse<ImageUsage> {
  return {
    items,
    pagination: {
      page: 1,
      pageSize: 25,
      totalItems: items.length,
      totalPages: 1,
      hasNext: false,
      hasPrevious: false,
    },
  };
}

export function defaultImageUsage(): ImageUsage {
  return {
    image: {
      id: "sha256:img1",
      shortId: "img1",
      repoTags: ["nginx:1.27"],
      repoDigests: ["nginx@sha256:aaa"],
      createdAt: "2026-07-01T00:00:00Z",
      size: 187000000,
      architecture: "amd64",
      os: "linux",
    },
    containerCount: 2,
  };
}

// ---------------------------------------------------------- docker events --

export function dockerEvent(overrides: Partial<DockerEvent> = {}): DockerEvent {
  return {
    sequence: 1,
    fingerprint: "fp-1",
    hostId: "local",
    type: "container",
    action: "start",
    actorId: "abcdef0123456789",
    actorName: "web",
    scope: "local",
    attributes: {
      name: "web",
      // Already masked, as everything the API returns must be.
      DB_PASSWORD: "********",
    },
    composeProject: "shop",
    composeService: "web",
    dockerTime: "2026-08-03T09:00:00Z",
    observedAt: "2026-08-03T09:00:01Z",
    result: "processed",
    refreshRequested: "container",
    createdAt: "2026-08-03T09:00:01Z",
    ...overrides,
  };
}

export function eventPage(
  items: DockerEvent[] = [dockerEvent()],
  totalItems = items.length,
): ListResponse<DockerEvent> {
  return {
    items,
    pagination: {
      page: 1,
      pageSize: 25,
      totalItems,
      totalPages: Math.max(Math.ceil(totalItems / 25), 1),
      hasNext: totalItems > 25,
      hasPrevious: false,
    },
  };
}

export function eventEngineStatus(
  overrides: Partial<EventEngineStatus> = {},
): EventEngineStatus {
  return {
    enabled: true,
    state: "connected",
    connectedSince: "2026-08-03T09:00:00Z",
    lastConnectedAt: "2026-08-03T09:00:00Z",
    lastEventAt: "2026-08-03T09:05:00Z",
    lastReconciliationAt: "2026-08-03T09:00:05Z",
    currentBackoffMs: 0,
    queueDepth: 0,
    queueCapacity: 1024,
    pendingRefreshes: 0,
    overflowPending: false,
    subscribers: 1,
    subscriberLimit: 16,
    counters: {
      eventsReceived: 42,
      eventsPersisted: 40,
      eventsDeduplicated: 2,
      eventsDropped: 0,
      targetedRefreshes: 38,
      fullReconciliations: 1,
      reconnectCount: 0,
      refreshFailures: 0,
      eventsPruned: 0,
    },
    retention: { maxAgeSeconds: 604800, maxCount: 50000, intervalSeconds: 3600 },
    storedEvents: 40,
    ...overrides,
  };
}

export const eventFilterOptions: EventFilterOptions = {
  types: ["container", "image", "network", "volume", "daemon", "other"],
  actions: ["start", "stop", "die", "pull"],
  results: ["processed", "deduplicated", "ignored", "warning", "failed"],
  projects: ["shop", "blog"],
  sortFields: ["action", "dockerTime", "name", "observed", "project", "result", "sequence", "type"],
};

/**
 * A controllable stand-in for EventSource.
 *
 * The hook takes its stream factory as an option precisely so tests never open
 * a real connection: nothing here depends on a network, a timer, or jsdom
 * having an EventSource implementation.
 */
export class FakeEventStream {
  readonly url: string;
  onerror: ((event: Event) => void) | null = null;
  onopen: ((event: Event) => void) | null = null;
  closed = false;

  private listeners = new Map<string, ((event: MessageEvent) => void)[]>();

  constructor(url: string) {
    this.url = url;
  }

  addEventListener(type: string, listener: (event: MessageEvent) => void): void {
    const existing = this.listeners.get(type) ?? [];
    existing.push(listener);
    this.listeners.set(type, existing);
  }

  close(): void {
    this.closed = true;
  }

  /** Delivers a named frame, as the server would. */
  emit(type: string, payload: unknown): void {
    const data = typeof payload === "string" ? payload : JSON.stringify(payload);
    for (const listener of this.listeners.get(type) ?? []) {
      listener(new MessageEvent(type, { data }));
    }
  }

  /** Signals that the connection opened. */
  open(): void {
    this.onopen?.(new Event("open"));
  }

  /** Signals a dropped connection, which EventSource retries on its own. */
  fail(): void {
    this.onerror?.(new Event("error"));
  }
}

/**
 * Builds a stream factory and exposes the streams it created, so a test can
 * drive the connection it caused.
 */
export function fakeStreamFactory(): {
  factory: (url: string) => FakeEventStream;
  streams: FakeEventStream[];
} {
  const streams: FakeEventStream[] = [];
  return {
    factory: (url: string) => {
      const stream = new FakeEventStream(url);
      streams.push(stream);
      return stream;
    },
    streams,
  };
}

/**
 * A stubbed endpoint that ANSWERS with an HTTP error.
 *
 * Distinct from passing an Error, which models the backend being unreachable.
 * The UI renders those two very differently -- "check the request" versus
 * "check the server is running" -- so a test must be able to produce each.
 */
export class HttpFailure {
  constructor(
    readonly status: number,
    readonly code: string,
    readonly message: string,
  ) {}
}

/** A recorded request, so tests can assert on the URLs the UI produced. */
export interface RecordedRequest {
  url: string;
  method: string;
}

export interface ApiStubOptions {
  health?: HealthReport | Error;
  inventory?: InventoryStatus | Error | HttpFailure;
  containers?: ListResponse<ContainerSummary> | Error;
  detail?: ContainerDetail | Error;
  containerBehaviors?: ContainerBehaviorSummary | Error;
  raw?: RawInspection | Error;
  images?: ListResponse<ImageUsage> | Error;
  filters?: FilterOptions;
  /** Status code and body for POST /inventory/refresh. */
  refresh?: { status: number; body: unknown };

  /** The identity /auth/session returns, or a failure to sign in with. */
  session?: unknown | Error | HttpFailure;
  /** The bootstrap status, for the unclaimed-installation path. */
  bootstrap?: unknown | Error | HttpFailure;
  /** The response to POST /auth/login. */
  login?: unknown | Error | HttpFailure;
  /** The account list and the create/update responses. */
  users?: unknown | Error | HttpFailure;

  events?: ListResponse<DockerEvent> | Error | HttpFailure;
  eventEngine?: EventEngineStatus | Error | HttpFailure;
  /**
   * The dependency read Container Detail issues.
   *
   * Defaults to a container with no relationships. A fixture that omitted this
   * would leave every Container Detail test making an unmocked request, which
   * is how adding one section broke seven unrelated tests.
   */
  dependencies?: ContainerDependencies | Error | HttpFailure;
  /** GET /dependencies -- every relationship in force. */
  dependencyListing?: DependencyListing | Error | HttpFailure;
  /** GET /dependencies/graph -- the deterministic update order. */
  dependencyGraph?: DependencyGraph | Error | HttpFailure;
  /** GET /dependencies/operations -- coordinated provider updates. */
  dependencyOperations?: DependencyOperationListing | Error | HttpFailure;
  /**
   * POST /executions -- the recreation request.
   *
   * An HttpFailure here is the PREFLIGHT refusing, which is the case the manual
   * update UX exists for: HarborMaster declines before it stops anything, and
   * the operator has to be told which check said no.
   */
  execution?: unknown | Error | HttpFailure;
  /** POST /dependencies and DELETE /dependencies/{id}. */
  dependencyWrite?: unknown | Error | HttpFailure;
  eventFilters?: EventFilterOptions;
}

/**
 * Installs a fetch stub covering every endpoint the UI calls, and returns the
 * list of requests it received.
 *
 * Routing by URL rather than call order means a test can assert on the request
 * the UI actually built -- which is how the filter and pagination tests verify
 * that work happens on the server, not in the browser.
 */
/**
 * The signed-in identity every suite gets by default.
 *
 * An administrator, so a page under test is not incidentally refused by a
 * permission check the test was not written to exercise. The authorization
 * behaviour itself is checked in Session.test.tsx and on the backend.
 */
export function sessionResponse(role: Role = "administrator") {
  return {
    user: {
      userId: "usr_test0000000000000000",
      username: "tester",
      role,
      status: "active",
      permissions: permissionsFor(role),
      mustChangePassword: false,
      createdAt: "2026-08-01T09:00:00Z",
    },
    csrfToken: "test-csrf-token",
    expiresAt: "2026-08-10T09:00:00Z",
  };
}

/**
 * An empty list envelope, in the shape every list endpoint guarantees.
 *
 * `items` and `pagination` are both `required` in the schema, so a page reading
 * this gets its genuine empty state rather than a crash on a missing field.
 */
export function emptyPage(): { items: never[]; pagination: Record<string, unknown> } {
  return {
    items: [],
    pagination: {
      page: 1,
      pageSize: 25,
      totalItems: 0,
      totalPages: 1,
      hasNext: false,
      hasPrevious: false,
    },
  };
}

export function stubApi(options: ApiStubOptions = {}): RecordedRequest[] {
  const requests: RecordedRequest[] = [];

  const json = (body: unknown, status = 200) =>
    Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
    );

  const respond = (value: unknown | Error | undefined, fallback: unknown) => {
    // An HttpFailure is the server ANSWERING with an error, which the UI must
    // render differently from an Error, which is the server being unreachable.
    // The two states have different remedies, so the stub has to tell them
    // apart or a test cannot distinguish them either.
    if (value instanceof HttpFailure) {
      return json({ error: { code: value.code, message: value.message } }, value.status);
    }
    if (value instanceof Error) return Promise.reject(value);
    return json(value ?? fallback);
  };

  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      requests.push({ url, method: init?.method ?? "GET" });

      // Identity first. The app asks who it is talking to before anything
      // else, and an unanswered /auth/session would leave every suite on the
      // sign-in page rather than the view under test.
      if (url.includes("/auth/login")) {
        return respond(options.login, sessionResponse());
      }
      if (url.includes("/auth/session")) {
        return respond(options.session, sessionResponse());
      }
      // Automation. The dashboard reads it on every render, so a suite about
      // something else must not fall over on an unanswered request.
      if (url.includes("/automation")) {
        return json({
          status: { enabled: false, running: false, policies: 0, enabledPolicies: 0 },
          history: { total: 0 },
        });
      }
      if (url.includes("/update-policies")) {
        return json({
          items: [],
          pagination: {
            page: 1, pageSize: 25, totalItems: 0, totalPages: 1,
            hasNext: false, hasPrevious: false,
          },
        });
      }

      if (url.includes("/auth/bootstrap")) {
        return respond(options.bootstrap, { completed: true, tokenRequired: true });
      }
      if (url.includes("/users")) {
        // A created account, or the list. The method distinguishes them.
        if ((init?.method ?? "GET") === "POST") {
          return respond(options.users, {
            user: {
              userId: "usr_created00000000000000",
              username: "newoperator",
              role: "operator",
              status: "active",
              permissions: permissionsFor("operator"),
              mustChangePassword: true,
              createdAt: "2026-08-05T09:00:00Z",
            },
            temporaryPassword: "a-generated-temporary-password",
          });
        }
        return respond(options.users, {
          items: [sessionResponse().user],
          pagination: {
            page: 1, pageSize: 50, totalItems: 1, totalPages: 1,
            hasNext: false, hasPrevious: false,
          },
        });
      }

      // Dependency routes, before the looser "/containers" match below:
      // "/dependencies/container/{id}" contains "/containers" only by
      // coincidence, and a prefix match would answer it with a container.
      //
      // Ordered most specific first. The bare "/dependencies" list route is a
      // PREFIX of the other two, so matching it first would swallow both.
      if (url.includes("/dependencies/graph")) {
        return respond(options.dependencyGraph, emptyDependencyGraph());
      }
      if (url.includes("/dependencies/operations")) {
        return respond(options.dependencyOperations, {
          items: [],
          total: 0,
          limit: 50,
        });
      }
      if (url.includes("/dependencies/container/")) {
        // The default is a container with NO relationships, which is what most
        // fixtures describe. A test about dependencies overrides it.
        return respond(options.dependencies, emptyContainerDependencies());
      }
      if (url.includes("/dependencies")) {
        const method = init?.method ?? "GET";
        if (method === "POST") {
          return respond(options.dependencyWrite, operatorDependency());
        }
        if (method === "DELETE") {
          if (options.dependencyWrite instanceof HttpFailure) {
            return respond(options.dependencyWrite, null);
          }
          if (options.dependencyWrite instanceof Error) {
            return Promise.reject(options.dependencyWrite);
          }
          return Promise.resolve(new Response(null, { status: 204 }));
        }
        return respond(options.dependencyListing, emptyDependencyListing());
      }

      // The recreation request. Matched before the looser routes below so a
      // preflight refusal can be modelled: it is the one response this control
      // exists to render.
      if (url.includes("/executions") && (init?.method ?? "GET") === "POST") {
        return respond(options.execution, {
          executionId: "exe_0123456789abcdef0123",
          state: "queued",
        });
      }

      // Event routes are matched next: "/event-engine" and "/events" would
      // both be swallowed by a looser prefix match further down.
      if (url.includes("/event-engine")) {
        return respond(options.eventEngine, eventEngineStatus());
      }
      if (url.includes("/event-filters")) {
        return respond(options.eventFilters, eventFilterOptions);
      }
      if (url.includes("/events")) {
        return respond(options.events, eventPage());
      }

      if (url.includes("/inventory/refresh")) {
        const refresh = options.refresh ?? {
          status: 202,
          body: {
            accepted: true,
            trigger: "manual",
            startedAt: "2026-08-03T09:30:00Z",
            message: "refresh started; poll GET /api/v1/inventory for completion",
          },
        };
        return json(refresh.body, refresh.status);
      }
      if (url.includes("/inventory/filters")) {
        return respond(options.filters, filterOptions);
      }
      if (url.includes("/inventory")) {
        return respond(options.inventory, inventoryStatus());
      }
      if (url.includes("/raw")) {
        return respond(options.raw, rawInspection);
      }
      /*
        The saved-behaviour summary, BEFORE the "/containers/" detail branch.

        "/containers/update-behaviors" contains "/containers/", so the detail
        branch first would answer a summary request with a ContainerDetail --
        exactly the class of lie C2.1 removed from this file.
      */
      if (url.includes("/containers/update-behaviors")) {
        return respond(options.containerBehaviors, emptyBehaviorSummary());
      }
      if (url.includes("/containers/")) {
        return respond(options.detail, containerDetail());
      }
      if (url.includes("/containers")) {
        return respond(options.containers, containerPage());
      }
      if (url.includes("/images")) {
        return respond(options.images, imagePage());
      }
      if (url.includes("/health")) {
        return respond(options.health, healthyReport);
      }
      if (url.includes("/version")) {
        return json(buildInfo);
      }
      /*
       * SUMMARIES BEFORE LISTS.
       *
       * `/drift/summary` also contains `/drift`, so a list branch placed first
       * would answer it with `{ items, pagination }` -- a list envelope served
       * as a summary, which is the very shape this batch exists to eliminate.
       * Ordering is the whole contract of a substring router, so the more
       * specific path is matched first and the shape it returns is its own.
       *
       * Both objects carry every field the server guarantees, so a page reading
       * them shows its genuine zero state.
       */
      if (url.includes("/drift/summary")) {
        return json({
          total: 0,
          open: 0,
          bySeverity: {},
          byStatus: {},
          byCategory: {},
          containersWithDrift: 0,
          containersEvaluated: 0,
          incomplete: false,
        });
      }
      if (url.includes("/policy-summary")) {
        return json({
          policies: 0,
          policiesTotal: 0,
          total: 0,
          open: 0,
          bySeverity: {},
          byStatus: {},
          byRule: {},
          containersEvaluated: 0,
          containersCompliant: 0,
        });
      }

      /*
       * The list endpoints a suite may reach incidentally, answered with a real
       * EMPTY list envelope rather than left to the fallback.
       *
       * `{ items: [], pagination }` is the shape the server guarantees --
       * `required: [items, pagination]` in the schema -- so a page rendering
       * this shows its genuine empty state. Before the fallback was made
       * honest these fell through to the /version payload, which had no
       * `items` at all: the empty state such a suite asserted was produced by
       * a malformed response rather than by an empty list.
       */
      if (
        url.includes("/snapshots") ||
        url.includes("/drift") ||
        url.includes("/policy-violations") ||
        url.includes("/notifications")
      ) {
        return json(emptyPage());
      }

      /*
       * An endpoint this stub does not model answers 404, not 200.
       *
       * # Why this is not merely tidiness
       *
       * It used to `return json(buildInfo)` -- so EVERY unmodelled endpoint was
       * answered with the /version payload. That object is truthy and has none
       * of the fields any other endpoint promises, so a page that guarded one
       * level and dereferenced the next crashed on a response that could never
       * come from the server:
       *
       *     summary.bySeverity.critical            Drift, PolicyViolations
       *     destinations.data?.items.length        Notifications
       *
       * Both shapes are guaranteed by the real contract -- `bySeverity` is
       * `make()`d by the store and `required` in the schema, and every list
       * envelope is `required: [items, pagination]` -- so the production code
       * was right and the fixture was lying.
       *
       * The failures were INTERMITTENT because the bad render happened when a
       * fetch resolved after its test had finished: landing outside the test
       * window makes Vitest report an unhandled error rather than a failure,
       * and which side of the boundary it lands on is a timing race.
       *
       * A 404 is what an unmodelled endpoint honestly is. A suite that needs an
       * endpoint answered adds it above, with the shape the server actually
       * returns.
       */
      return json(
        {
          error: {
            code: "not_found",
            message:
              "the shared API stub does not model " +
              new URL(url, "http://localhost").pathname +
              "; add it to stubApi with the shape the server returns",
          },
        },
        404,
      );
    }),
  );

  return requests;
}

/** Returns the query parameters of the most recent container list request. */
export function lastContainerQuery(requests: RecordedRequest[]): URLSearchParams {
  const match = [...requests]
    .reverse()
    .find((request) => request.url.includes("/containers?") || request.url.endsWith("/containers"));
  if (!match) return new URLSearchParams();

  const index = match.url.indexOf("?");
  return new URLSearchParams(index >= 0 ? match.url.slice(index + 1) : "");
}

/** Returns the query parameters of the most recent event list request. */
export function lastEventQuery(requests: RecordedRequest[]): URLSearchParams {
  const match = [...requests]
    .reverse()
    .find(
      (request) =>
        request.url.includes("/events?") ||
        request.url.endsWith("/events"),
    );
  if (!match) return new URLSearchParams();

  const index = match.url.indexOf("?");
  return new URLSearchParams(index >= 0 ? match.url.slice(index + 1) : "");
}

/** A container with no dependency relationships, in either direction. */
export function emptyContainerDependencies(
  container = "web",
): ContainerDependencies {
  return {
    container,
    dependsOn: [],
    dependedOnBy: [],
    state: "dependencySatisfied",
    detail: "every container this one depends on is verified or stable",
  };
}

/**
 * An estate with no relationships at all.
 *
 * The POSITIVELY established empty case: discovery ran and found nothing. A
 * fixture that wants "HarborMaster could not establish this" supplies problems
 * or an HttpFailure, because the UI must render those two differently.
 */
export function emptyDependencyListing(): DependencyListing {
  return { items: [], total: 0 };
}

export function emptyDependencyGraph(): DependencyGraph {
  return { stages: [], edges: [] };
}

/** One detected namespace relationship: sonarr rides gluetun's network. */
export function namespaceDependency(
  overrides: Partial<WorkloadDependency> = {},
): WorkloadDependency {
  return {
    dependent: "sonarr",
    dependency: "gluetun",
    source: "dockerNetworkNamespace",
    kind: "Network namespace",
    origin: "Detected by HarborMaster",
    hard: true,
    why: "the dependent's Docker configuration shares this container's network namespace",
    deletable: false,
    ...overrides,
  };
}

/** One mandatory reattachment inside a coordinated provider update. */
export function rebindMember(
  overrides: Partial<DependencyMember> = {},
): DependencyMember {
  return {
    operationId: "depop_0123456789abcdef0123",
    dependent: "sonarr",
    provider: "gluetun",
    source: "dockerNetworkNamespace",
    state: "verified",
    ...overrides,
  };
}

/**
 * A coordinated provider update that ended part-finished.
 *
 * The default is the case worth rendering: the provider succeeded, two
 * reattachments verified, one failed, and HarborMaster will not roll any of it
 * backward.
 */
export function operationSummary(
  overrides: Partial<DependencyOperationSummary> = {},
): DependencyOperationSummary {
  return {
    operation: {
      operationId: "depop_0123456789abcdef0123",
      provider: "gluetun",
      state: "failed",
      failure: "rebindFailed",
      createdAt: "2026-08-13T09:00:00Z",
      updatedAt: "2026-08-13T09:04:00Z",
      members: [
        rebindMember({ dependent: "sonarr", state: "verified" }),
        rebindMember({ dependent: "radarr", state: "verified" }),
        rebindMember({ dependent: "qbittorrent", state: "failed" }),
      ],
    },
    providerVerified: true,
    complete: false,
    needsAttention: true,
    ...overrides,
  };
}

export function operationListing(
  items: DependencyOperationSummary[] = [operationSummary()],
): DependencyOperationListing {
  return { items, total: items.length, limit: 50 };
}

/** One configured ordering, which an administrator may remove. */
export function operatorDependency(
  overrides: Partial<WorkloadDependency> = {},
): WorkloadDependency {
  return {
    dependencyId: "dep_0123456789abcdef0123",
    dependent: "api",
    dependency: "postgres",
    source: "operator",
    kind: "Application ordering",
    origin: "Configured by you",
    hard: false,
    why: "an operator recorded that the dependent needs this container",
    deletable: true,
    ...overrides,
  };
}
