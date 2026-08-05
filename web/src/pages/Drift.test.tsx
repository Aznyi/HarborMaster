import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { DriftRecord, DriftSummary } from "../api/driftTypes";
import { ContainerDrift } from "./ContainerDrift";
import { Drift } from "./Drift";

/**
 * Drift UI tests.
 *
 * Two properties matter most and are asserted repeatedly:
 *
 *   - A SECRET-BACKED field never renders a value. The API does not send one,
 *     and the UI must not invent one or render `undefined` where a password
 *     would have been.
 *   - "No drift" and "never evaluated" must READ differently. Showing an empty
 *     list as a clean bill of health for a container that was never comparable
 *     is the worst thing this feature could do.
 */

const originalFetch = globalThis.fetch;

/** Captures the request URLs the page produced, which is how filter tests work. */
let requests: string[] = [];

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

function driftRecord(overrides: Partial<DriftRecord> = {}): DriftRecord {
  return {
    id: 1,
    containerId: "abc123def456",
    containerName: "web",
    snapshotId: 7,
    detectedAt: "2026-08-05T12:00:00Z",
    lastSeenAt: "2026-08-05T12:30:00Z",
    inventoryGeneration: 4,
    category: "security",
    field: "privileged",
    kind: "modified",
    severity: "critical",
    previousValue: "false",
    currentValue: "true",
    status: "active",
    reason: "the container now runs privileged",
    ...overrides,
  };
}

function summary(overrides: Partial<DriftSummary> = {}): DriftSummary {
  return {
    total: 3,
    open: 2,
    bySeverity: { critical: 1, low: 1 },
    byStatus: { active: 2, resolved: 1 },
    byCategory: { security: 1, labels: 1 },
    containersWithDrift: 1,
    containersEvaluated: 5,
    lastEvaluatedAt: "2026-08-05T12:30:00Z",
    incomplete: false,
    ...overrides,
  };
}

/** Installs a fetch double routing by path. */
function mockApi(routes: Record<string, unknown>) {
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    requests.push(url);

    for (const [fragment, body] of Object.entries(routes)) {
      if (url.includes(fragment)) return jsonResponse(body);
    }
    return new Response("{}", { status: 404 });
  }) as typeof fetch;
}

beforeEach(() => {
  requests = [];
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

function renderDrift() {
  return render(
    <MemoryRouter initialEntries={["/drift"]}>
      <Routes>
        <Route path="/drift" element={<Drift />} />
      </Routes>
    </MemoryRouter>,
  );
}

function renderContainerDrift(containerId = "abc123def456") {
  return render(
    <MemoryRouter initialEntries={[`/drift/container/${containerId}`]}>
      <Routes>
        <Route path="/drift/container/:id" element={<ContainerDrift />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("Drift dashboard", () => {
  it("renders the summary cards and a drift record", async () => {
    mockApi({
      "/drift/summary": summary(),
      "/drift": { items: [driftRecord()], pagination: pagination(1) },
    });

    renderDrift();

    expect(await screen.findByText("privileged")).toBeInTheDocument();
    expect(screen.getByText("Open drift")).toBeInTheDocument();
    // The evaluated count is stated so "1 of 5 evaluated" cannot be misread as
    // "1 of 5 containers".
    expect(screen.getByText(/of 5 evaluated/)).toBeInTheDocument();
  });

  it("defaults to open drift only", async () => {
    mockApi({
      "/drift/summary": summary(),
      "/drift": { items: [], pagination: pagination(0) },
    });

    renderDrift();
    await screen.findByText(/No drift matches these filters/);

    const listRequest = requests.find((url) => url.includes("/drift?") || url.endsWith("/drift"));
    expect(listRequest).toBeDefined();
    expect(listRequest).toContain("openOnly=true");
  });

  it("sends filters to the server rather than filtering in the browser", async () => {
    mockApi({
      "/drift/summary": summary(),
      "/drift": { items: [driftRecord()], pagination: pagination(1) },
    });

    renderDrift();
    await screen.findByText("privileged");

    requests = [];
    await userEvent.selectOptions(screen.getByLabelText("Severity"), "critical");

    await waitFor(() => {
      expect(requests.some((url) => url.includes("severity=critical"))).toBe(true);
    });
  });

  it("asks for resolved records when the operator opts in", async () => {
    mockApi({
      "/drift/summary": summary(),
      "/drift": { items: [driftRecord()], pagination: pagination(1) },
    });

    renderDrift();
    await screen.findByText("privileged");

    requests = [];
    await userEvent.click(screen.getByLabelText("Include resolved"));

    await waitFor(() => {
      expect(requests.some((url) => url.includes("openOnly=false"))).toBe(true);
    });
  });

  it("warns when the summary is incomplete", async () => {
    mockApi({
      "/drift/summary": summary({ incomplete: true }),
      "/drift": { items: [], pagination: pagination(0) },
    });

    renderDrift();

    // A summary that hid this would read as "these are all the differences".
    expect(
      await screen.findByText(/counts are a floor rather than a total/),
    ).toBeInTheDocument();
  });

  it("never renders a value for a secret-backed field", async () => {
    const secret = "SUPER-SECRET-PASSWORD-9182";
    mockApi({
      "/drift/summary": summary(),
      "/drift": {
        items: [
          driftRecord({
            id: 2,
            category: "environment",
            field: "DB_PASSWORD",
            sensitive: true,
            // The API never sends these; if a future change did, the UI must
            // still not render them.
            previousValue: undefined,
            currentValue: undefined,
          }),
        ],
        pagination: pagination(1),
      },
    });

    renderDrift();

    expect(await screen.findByText("DB_PASSWORD")).toBeInTheDocument();
    expect(screen.getByText(/Value withheld/)).toBeInTheDocument();
    // Neither the secret nor a stray "undefined" reaches the page.
    expect(screen.queryByText(new RegExp(secret))).not.toBeInTheDocument();
    expect(screen.queryByText(/undefined/)).not.toBeInTheDocument();
  });

  it("explains an unverifiable comparison rather than showing nothing", async () => {
    mockApi({
      "/drift/summary": summary(),
      "/drift": {
        items: [
          driftRecord({
            id: 3,
            category: "environment",
            field: "API_TOKEN",
            kind: "unverifiable",
            sensitive: true,
          }),
        ],
        pagination: pagination(1),
      },
    });

    renderDrift();

    expect(await screen.findByText("API_TOKEN")).toBeInTheDocument();
    expect(screen.getByText("unverifiable")).toBeInTheDocument();
  });
});

describe("Container drift", () => {
  it("distinguishes never-evaluated from clean", async () => {
    mockApi({
      "/drift/container/": {
        containerId: "abc123def456",
        records: [],
        pagination: pagination(0),
        // No evaluation: this container has never been compared.
      },
    });

    renderContainerDrift();

    expect(
      await screen.findByText(/has never been evaluated for drift/),
    ).toBeInTheDocument();
    expect(screen.getByText(/This container has not been evaluated/)).toBeInTheDocument();
  });

  it("reports a clean container as clean once it has been evaluated", async () => {
    mockApi({
      "/drift/container/": {
        containerId: "abc123def456",
        records: [],
        pagination: pagination(0),
        evaluation: {
          containerId: "abc123def456",
          containerName: "web",
          snapshotId: 7,
          evaluatedAt: "2026-08-05T12:30:00Z",
          inventoryGeneration: 4,
          driftCount: 0,
          complete: true,
        },
      },
    });

    renderContainerDrift();

    expect(await screen.findByText(/No drift against the baseline/)).toBeInTheDocument();
    expect(screen.queryByText(/never been evaluated/)).not.toBeInTheDocument();
  });

  it("warns when the evaluation could not compare everything", async () => {
    mockApi({
      "/drift/container/": {
        containerId: "abc123def456",
        records: [driftRecord()],
        pagination: pagination(1),
        evaluation: {
          containerId: "abc123def456",
          containerName: "web",
          snapshotId: 0,
          evaluatedAt: "2026-08-05T12:30:00Z",
          inventoryGeneration: 4,
          driftCount: 1,
          complete: false,
          reason: "the comparison exceeded its size budget",
        },
      },
    });

    renderContainerDrift();

    expect(
      await screen.findByText(/could not compare everything/),
    ).toBeInTheDocument();
    expect(screen.getByText(/the list may be incomplete/)).toBeInTheDocument();
  });

  it("offers only the operator statuses, never resolve", async () => {
    mockApi({
      "/drift/container/": {
        containerId: "abc123def456",
        records: [driftRecord()],
        pagination: pagination(1),
        evaluation: {
          containerId: "abc123def456",
          containerName: "web",
          snapshotId: 7,
          evaluatedAt: "2026-08-05T12:30:00Z",
          inventoryGeneration: 4,
          driftCount: 1,
          complete: true,
        },
      },
    });

    renderContainerDrift();
    // findAllByText, not findByText: the page renders each field twice on
    // purpose -- once in the timeline and once in the detail list.
    await screen.findAllByText("privileged");

    for (const status of ["acknowledged", "ignored", "expected"]) {
      expect(screen.getByRole("button", { name: status })).toBeInTheDocument();
    }
    // Resolution is engine-owned. A button asserting it would make the list
    // stop describing reality.
    expect(screen.queryByRole("button", { name: /resolve/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /remediate/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /roll ?back/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /revert/i })).not.toBeInTheDocument();
  });

  it("issues a PATCH when an operator changes the status", async () => {
    const record = driftRecord();
    globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      requests.push(`${init?.method ?? "GET"} ${url}`);

      if (init?.method === "PATCH") {
        return jsonResponse({ ...record, status: "ignored" });
      }
      return jsonResponse({
        containerId: "abc123def456",
        records: [record],
        pagination: pagination(1),
        evaluation: {
          containerId: "abc123def456",
          containerName: "web",
          snapshotId: 7,
          evaluatedAt: "2026-08-05T12:30:00Z",
          inventoryGeneration: 4,
          driftCount: 1,
          complete: true,
        },
      });
    }) as typeof fetch;

    renderContainerDrift();
    // findAllByText, not findByText: the page renders each field twice on
    // purpose -- once in the timeline and once in the detail list.
    await screen.findAllByText("privileged");

    await userEvent.click(screen.getByRole("button", { name: "ignored" }));

    await waitFor(() => {
      expect(requests.some((entry) => entry.startsWith("PATCH"))).toBe(true);
    });
  });

  it("renders a timeline entry for each difference", async () => {
    mockApi({
      "/drift/container/": {
        containerId: "abc123def456",
        records: [
          driftRecord(),
          driftRecord({
            id: 2,
            field: "com.example.owner",
            category: "labels",
            severity: "low",
            detectedAt: "2026-08-04T09:00:00Z",
            resolvedAt: "2026-08-05T09:00:00Z",
            status: "resolved",
          }),
        ],
        pagination: pagination(2),
        evaluation: {
          containerId: "abc123def456",
          containerName: "web",
          snapshotId: 7,
          evaluatedAt: "2026-08-05T12:30:00Z",
          inventoryGeneration: 4,
          driftCount: 2,
          complete: true,
        },
      },
    });

    renderContainerDrift();

    // Both differences appear, and a RESOLVED entry keeps its place with an end
    // point rather than disappearing -- erasing it would remove exactly the
    // history the timeline exists for.
    await screen.findAllByText("privileged");
    expect(screen.getAllByText("com.example.owner").length).toBeGreaterThan(0);
    expect(screen.getAllByText(/resolved/).length).toBeGreaterThan(0);
  });

  it("shows the disconnected state when the backend is unreachable", async () => {
    globalThis.fetch = vi.fn(async () => {
      throw new TypeError("network down");
    }) as unknown as typeof fetch;

    renderContainerDrift();

    expect(await screen.findByText(/Cannot reach the HarborMaster backend/i)).toBeInTheDocument();
  });
});

function pagination(totalItems: number) {
  return {
    page: 1,
    pageSize: 25,
    totalItems,
    totalPages: totalItems === 0 ? 0 : 1,
    hasNext: false,
    hasPrevious: false,
  };
}

// A guard on the sweep above: the secret matcher must be able to find a value
// that IS rendered, or the leak assertion passes vacuously.
describe("the secret sweep", () => {
  it("detects a value that is actually rendered", async () => {
    const marker = "PLAINTEXT-MARKER-4471";
    mockApi({
      "/drift/summary": summary(),
      "/drift": {
        items: [
          driftRecord({
            id: 9,
            category: "labels",
            field: "com.example.marker",
            sensitive: false,
            previousValue: "",
            currentValue: marker,
          }),
        ],
        pagination: pagination(1),
      },
    });

    renderDrift();

    const row = await screen.findByText("com.example.marker");
    expect(within(row.closest("li") as HTMLElement).getByText(marker)).toBeInTheDocument();
  });
});
