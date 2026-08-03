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
import type { HealthReport, VersionInfo } from "../api/types";

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

/** A recorded request, so tests can assert on the URLs the UI produced. */
export interface RecordedRequest {
  url: string;
  method: string;
}

export interface ApiStubOptions {
  health?: HealthReport | Error;
  inventory?: InventoryStatus | Error;
  containers?: ListResponse<ContainerSummary> | Error;
  detail?: ContainerDetail | Error;
  raw?: RawInspection | Error;
  images?: ListResponse<ImageUsage> | Error;
  filters?: FilterOptions;
  /** Status code and body for POST /inventory/refresh. */
  refresh?: { status: number; body: unknown };
}

/**
 * Installs a fetch stub covering every endpoint the UI calls, and returns the
 * list of requests it received.
 *
 * Routing by URL rather than call order means a test can assert on the request
 * the UI actually built -- which is how the filter and pagination tests verify
 * that work happens on the server, not in the browser.
 */
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
    if (value instanceof Error) return Promise.reject(value);
    return json(value ?? fallback);
  };

  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      requests.push({ url, method: init?.method ?? "GET" });

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
