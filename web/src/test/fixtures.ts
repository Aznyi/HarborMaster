import { vi } from "vitest";

import type {
  ContainerDetail,
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
import type { Role } from "../api/authTypes";
import { permissionsFor } from "./session";

export const healthyReport: HealthReport = {
  status: "healthy",
  database: { status: "up", latencyMs: 1 },
  docker: { status: "up", latencyMs: 4, version: "1.51" },
  checkedAt: "2026-08-03T09:20:11.482Z",
  uptimeSeconds: 120,
};

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

export function containerPage(
  items: ContainerSummary[] = [containerSummary()],
  totalItems = items.length,
): ListResponse<ContainerSummary> {
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
      return json(buildInfo);
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
