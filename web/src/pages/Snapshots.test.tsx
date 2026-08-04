import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type {
  ReadinessReport,
  Snapshot,
  SnapshotDetail,
  SnapshotDiff,
} from "../api/snapshotTypes";
import { SnapshotDetailPage } from "./SnapshotDetail";
import { SnapshotReadinessPage } from "./SnapshotReadiness";
import { Snapshots } from "./Snapshots";

/**
 * The value that must never appear in rendered output.
 *
 * It is not merely masked in the UI: the API never sends it, so a test that
 * finds it has caught a genuine leak somewhere behind the component.
 */
const SECRET = "s3cr3t-must-not-render";
const DIGEST = "digest-must-not-render";

function snapshot(overrides: Partial<Snapshot> = {}): Snapshot {
  return {
    id: 1,
    containerId: "c0ffee000000",
    containerName: "web",
    imageReference: "nginx:1.27",
    specVersion: 1,
    checksum: "a".repeat(64),
    trigger: "manual",
    readinessStatus: "warning",
    createdAt: "2026-05-01T12:00:00Z",
    ...overrides,
  };
}

function snapshotDetail(): SnapshotDetail {
  return {
    ...snapshot(),
    spec: { specVersion: 1, identity: { containerName: "web" } },
    environment: [
      { position: 0, key: "PATH", classification: "normal", present: true, value: "/usr/bin" },
      // No `value`, no digest: exactly what the server sends for a secret.
      { position: 1, key: "DB_PASSWORD", classification: "sensitive", present: true, length: 22 },
    ],
    mounts: [{ destination: "/data", type: "volume", volumeName: "web-data" }],
    networks: [{ networkName: "bridge", aliases: ["web"] }],
  };
}

function readinessReport(): ReadinessReport {
  return {
    snapshotId: 1,
    status: "warning",
    evaluatedAt: "2026-05-01T12:00:00Z",
    inventoryGeneration: 7,
    inventoryCompletedAt: "2026-05-01T11:59:00Z",
    inventoryAgeSeconds: 60,
    inventoryStale: false,
    checks: [
      { id: "daemon_reachable", status: "ready", detail: "the container runtime responded" },
      {
        id: "secrets_available",
        status: "warning",
        detail: "1 sensitive value must be supplied externally at restore time",
      },
      {
        id: "mount_sources",
        status: "unverifiable",
        detail: "1 bind mount source cannot be verified",
      },
    ],
  };
}

function emptyDiff(): SnapshotDiff {
  return { fromSnapshotId: 1, groups: [], identical: true };
}

/** Stubs the snapshot endpoints. */
function stubSnapshotApi(overrides: {
  list?: unknown;
  detail?: unknown;
  readiness?: unknown;
  diff?: unknown;
} = {}) {
  const json = (body: unknown, status = 200) =>
    Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
    );

  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);

      if (url.includes("/restore-readiness")) {
        return json(overrides.readiness ?? readinessReport());
      }
      if (url.includes("/diff")) {
        return json(overrides.diff ?? emptyDiff());
      }
      if (/\/snapshots\/\d+/.test(url)) {
        return json(overrides.detail ?? snapshotDetail());
      }
      if (url.includes("/snapshots")) {
        return json(
          overrides.list ?? {
            items: [snapshot()],
            pagination: {
              page: 1,
              pageSize: 25,
              totalItems: 1,
              totalPages: 1,
              hasNext: false,
              hasPrevious: false,
            },
          },
        );
      }
      return json({});
    }),
  );
}

beforeEach(() => {
  vi.unstubAllGlobals();
});

describe("Snapshots list", () => {
  it("renders captured snapshots", async () => {
    stubSnapshotApi();
    render(
      <MemoryRouter initialEntries={["/snapshots"]}>
        <Snapshots />
      </MemoryRouter>,
    );

    expect(await screen.findByRole("link", { name: "web" })).toBeInTheDocument();
    expect(screen.getByText(/Manual/)).toBeInTheDocument();
    expect(screen.getByText(/Warning/)).toBeInTheDocument();
  });

  it("says plainly that there is no restore", async () => {
    stubSnapshotApi();
    render(
      <MemoryRouter initialEntries={["/snapshots"]}>
        <Snapshots />
      </MemoryRouter>,
    );

    await screen.findByRole("link", { name: "web" });
    expect(screen.getByText(/never changes a container/i)).toBeInTheDocument();
  });

  it("shows an empty state when nothing has been captured", async () => {
    stubSnapshotApi({
      list: {
        items: [],
        pagination: {
          page: 1,
          pageSize: 25,
          totalItems: 0,
          totalPages: 0,
          hasNext: false,
          hasPrevious: false,
        },
      },
    });
    render(
      <MemoryRouter initialEntries={["/snapshots"]}>
        <Snapshots />
      </MemoryRouter>,
    );

    expect(await screen.findByText(/No snapshots yet/i)).toBeInTheDocument();
  });

  it("surfaces a connectivity failure rather than an empty table", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.reject(new Error("network down"))));
    render(
      <MemoryRouter initialEntries={["/snapshots"]}>
        <Snapshots />
      </MemoryRouter>,
    );

    expect(
      await screen.findByText(/Cannot reach the HarborMaster backend/i),
    ).toBeInTheDocument();
  });
});

describe("Snapshot detail", () => {
  function renderDetail() {
    return render(
      <MemoryRouter initialEntries={["/snapshots/1"]}>
        <Routes>
          <Route path="/snapshots/:id" element={<SnapshotDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );
  }

  it("renders capture metadata", async () => {
    stubSnapshotApi();
    renderDetail();

    expect(await screen.findByText("nginx:1.27")).toBeInTheDocument();
    expect(screen.getByText("a".repeat(64))).toBeInTheDocument();
  });

  it("never renders a secret value or a digest", async () => {
    // The fixture mirrors the real payload: a sensitive entry has no value.
    // This also guards the case where a future change starts sending one.
    stubSnapshotApi({
      detail: {
        ...snapshotDetail(),
        environment: [
          {
            position: 0,
            key: "DB_PASSWORD",
            classification: "sensitive",
            present: true,
            length: 22,
            // Deliberately smuggled in: the component must not render them
            // even when they somehow appear in the payload.
            value: SECRET,
            digest: DIGEST,
          },
        ],
      },
    });
    renderDetail();

    // Switch to the environment tab.
    await userEvent.click(await screen.findByRole("button", { name: /environment/i }));
    await waitFor(() => expect(screen.getByText("DB_PASSWORD")).toBeInTheDocument());

    expect(screen.queryByText(SECRET)).toBeNull();
    expect(screen.queryByText(DIGEST)).toBeNull();
    expect(document.body.innerHTML).not.toContain(SECRET);
    expect(document.body.innerHTML).not.toContain(DIGEST);
    // ...and the operator is told WHY there is nothing to see.
    expect(screen.getByText(/not stored/i)).toBeInTheDocument();
  });

  it("explains that secrets block a restore", async () => {
    stubSnapshotApi();
    renderDetail();

    await userEvent.click(await screen.findByRole("button", { name: /environment/i }));
    await waitFor(() =>
      expect(screen.getByText(/never stores secret values/i)).toBeInTheDocument(),
    );
  });

  it("offers no restore control", async () => {
    stubSnapshotApi();
    renderDetail();

    await screen.findByText("nginx:1.27");
    for (const label of [/restore this/i, /roll back/i, /^apply$/i, /recreate/i]) {
      expect(screen.queryByRole("button", { name: label })).toBeNull();
    }
  });
});

describe("Restore readiness", () => {
  function renderReadiness() {
    return render(
      <MemoryRouter initialEntries={["/snapshots/1/readiness"]}>
        <Routes>
          <Route path="/snapshots/:id/readiness" element={<SnapshotReadinessPage />} />
        </Routes>
      </MemoryRouter>,
    );
  }

  it("renders every check with its verdict", async () => {
    stubSnapshotApi();
    renderReadiness();

    expect(await screen.findByText(/Docker daemon reachable/i)).toBeInTheDocument();
    expect(screen.getByText(/Secrets available/i)).toBeInTheDocument();
    expect(screen.getByText(/Mount sources/i)).toBeInTheDocument();
  });

  it("states that the report is informational and HarborMaster cannot restore", async () => {
    stubSnapshotApi();
    renderReadiness();

    expect(await screen.findByText(/informational/i)).toBeInTheDocument();
    expect(screen.getByText(/cannot restore, recreate, or modify/i)).toBeInTheDocument();
  });

  it("shows inventory provenance so a stale verdict is visible", async () => {
    stubSnapshotApi();
    renderReadiness();

    expect(await screen.findByText(/Inventory age/i)).toBeInTheDocument();
    expect(screen.getByText(/Generation/i)).toBeInTheDocument();
  });

  it("warns when the inventory is stale", async () => {
    stubSnapshotApi({
      readiness: {
        ...readinessReport(),
        inventoryStale: true,
        inventoryAgeSeconds: 7200,
      },
    });
    renderReadiness();

    expect(
      await screen.findByText(/older than the configured freshness threshold/i),
    ).toBeInTheDocument();
  });

  it("renders an unverifiable check as a warning rather than a pass", async () => {
    stubSnapshotApi();
    renderReadiness();

    expect(await screen.findByText(/Unverifiable/i)).toBeInTheDocument();
  });
});
