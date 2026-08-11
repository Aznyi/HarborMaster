import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import type { Acquisition } from "../api/acquisitionTypes";
import type {
  Execution,
  ExecutionEvent,
  ExecutionSummary,
} from "../api/executionTypes";
import { RecreateContainerAction } from "../components/RecreateContainerAction";
import { ExecutionDetail } from "./ExecutionDetail";
import { Executions } from "./Executions";

/**
 * Container recreation UI tests.
 *
 * The recreate button is the only control in HarborMaster that stops something
 * running, so the properties under test are about what an operator is told
 * before and after they use it:
 *
 *   - The confirmation says the container will be STOPPED AND RECREATED, that
 *     the image is already downloaded, and that ROLLBACK IS NOT AUTOMATIC. All
 *     three, in as many words.
 *   - It cannot be triggered in one click, or without reading the name.
 *   - A record that left containers behind says so above everything else, and
 *     carries the manual recovery steps.
 *   - A verification that was never reached is not rendered as a pass.
 *   - Cancellation is not offered once the host has been changed.
 *   - Daemon-adjacent text is rendered as text, never as markup.
 */

const originalFetch = globalThis.fetch;

let requests: string[] = [];
let writes: { url: string; method: string; body: string }[] = [];

const testDigest = `sha256:${"a".repeat(64)}`;
const executionID = "exec_00112233445566778899";
const acquisitionID = "acq_00112233445566778899";
const planID = "plan_00112233445566778899";
const containerID = "c".repeat(64);
const replacementID = "d".repeat(64);

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function pagination(totalItems: number) {
  return {
    page: 1,
    pageSize: 25,
    totalItems,
    totalPages: Math.max(1, Math.ceil(totalItems / 25)),
    hasNext: false,
    hasPrevious: false,
  };
}

function execution(overrides: Partial<Execution> = {}): Execution {
  return {
    executionId: executionID,
    acquisitionId: acquisitionID,
    planId: planID,
    containerId: containerID,
    containerName: "web",
    oldImage: "nginx:1.27.0",
    target: {
      registry: "docker.io",
      repository: "library/nginx",
      digest: testDigest,
      reference: "nginx:1.27.1",
      platform: { os: "linux", architecture: "amd64" },
    },
    state: "succeeded",
    checkpoint: "originalRemoved",
    originalRemoved: true,
    verification: {
      health: "passed",
      healthChecked: true,
      healthState: "healthy",
      image: "passed",
      preservation: "passed",
      network: "passed",
    },
    requestedAt: "2026-08-06T12:00:00Z",
    expiresAt: "2026-08-06T12:15:00Z",
    planDigest: "f".repeat(64),
    ...overrides,
  };
}

function summary(overrides: Partial<ExecutionSummary> = {}): ExecutionSummary {
  return {
    total: 4,
    active: 0,
    succeeded: 3,
    failed: 1,
    needsAttention: 0,
    byState: { succeeded: 3, failed: 1 },
    byFailure: { unhealthy: 1 },
    enabled: true,
    ...overrides,
  };
}

function acquisition(overrides: Partial<Acquisition> = {}): Acquisition {
  return {
    acquisitionId: acquisitionID,
    planId: planID,
    containerId: containerID,
    containerName: "web",
    target: {
      registry: "docker.io",
      repository: "library/nginx",
      digest: testDigest,
      reference: "nginx:1.27.1",
      platform: { os: "linux", architecture: "amd64" },
    },
    state: "succeeded",
    requestedAt: "2026-08-06T11:00:00Z",
    expiresAt: "2026-08-06T12:00:00Z",
    planDigest: "f".repeat(64),
    ...overrides,
  };
}

/** Installs a fetch double routing by path, matched IN THE ORDER GIVEN. */
function mockApi(routes: [string, unknown, number?][]) {
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString();
    const method = init?.method ?? "GET";
    requests.push(url);
    if (method !== "GET") {
      writes.push({ url, method, body: String(init?.body ?? "") });
    }

    for (const [fragment, body, status] of routes) {
      if (url.includes(fragment)) return jsonResponse(body, status ?? 200);
    }
    return new Response("{}", { status: 404 });
  }) as typeof fetch;
}

beforeEach(() => {
  requests = [];
  writes = [];
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

function renderList() {
  return render(
    <MemoryRouter initialEntries={["/executions"]}>
      <Routes>
        <Route path="/executions" element={<Executions />} />
      </Routes>
    </MemoryRouter>,
  );
}

function renderDetail() {
  return render(
    <MemoryRouter initialEntries={[`/executions/${executionID}`]}>
      <Routes>
        <Route path="/executions/:id" element={<ExecutionDetail />} />
      </Routes>
    </MemoryRouter>,
  );
}

// ------------------------------------------------------------------ list --

it("lists recreations and puts what needs attention first", async () => {
  mockApi([
    [
      "/executions",
      {
        items: [execution()],
        pagination: pagination(1),
        summary: summary({ needsAttention: 2 }),
      },
    ],
  ]);

  renderList();

  const cards = await screen.findByLabelText("Recreation summary");
  const tiles = within(cards).getAllByText(
    /Needs attention|In progress|Recreated|Total/,
  );
  expect(tiles[0]).toHaveTextContent("Needs attention");
  expect(within(cards).getByText("2")).toBeInTheDocument();
});

it("says plainly that rollback is not automatic", async () => {
  mockApi([
    ["/executions", { items: [], pagination: pagination(0), summary: summary() }],
  ]);

  renderList();

  const note = await screen.findByRole("note");
  expect(note).toHaveTextContent(/not rolled back automatically/i);
  // The correction: it must NOT claim rollback is categorically never automatic.
  expect(note).toHaveTextContent(/update policy/i);
  expect(note).toHaveTextContent(/stops it and replaces it/i);
});

it("states when recreation is switched off rather than showing an empty list", async () => {
  mockApi([
    [
      "/executions",
      {
        items: [],
        pagination: pagination(0),
        summary: summary({ enabled: false, total: 0, succeeded: 0, failed: 0 }),
      },
    ],
  ]);

  renderList();

  expect(
    await screen.findByText(/recreation is switched off in this deployment/i),
  ).toBeInTheDocument();
});

// ---------------------------------------------------------------- detail --

it("puts the host state above everything else on a failed recreation", async () => {
  mockApi([
    [
      "/executions/",
      {
        execution: execution({
          state: "failed",
          failure: "unhealthy",
          checkpoint: "replacementQuarantined",
          originalRemoved: false,
          mutatedAt: "2026-08-06T12:00:05Z",
          replacementId: replacementID,
          parkedName: `web.hm-old-${executionID}`,
          quarantineName: `web.hm-failed-${executionID}`,
          verification: {
            health: "failed",
            healthChecked: true,
            healthState: "unhealthy",
            image: "unknown",
            preservation: "unknown",
            network: "unknown",
          },
          recovery: {
            urgency: "urgent",
            serviceInterrupted: true,
            situation:
              "Both containers are on this host and neither is serving.",
            steps: [
              {
                order: 1,
                description: "Look at why the replacement did not work.",
                command: `docker logs web.hm-failed-${executionID}`,
                destructive: false,
              },
              {
                order: 2,
                description: "Give the original its name back.",
                command: `docker rename web.hm-old-${executionID} web`,
                destructive: false,
              },
            ],
          },
        }),
        events: [] as ExecutionEvent[],
      },
    ],
  ]);

  renderDetail();

  // Two alerts, deliberately: the host-state banner at the top and the
  // "not currently serving" line inside the recovery panel. Both are urgent
  // and both are meant to be read.
  const alerts = await screen.findAllByRole("alert");
  const banner = alerts.find((element) =>
    /changed this host and did not finish/i.test(element.textContent ?? ""),
  );
  expect(banner).toBeDefined();
  expect(banner).toHaveTextContent(/is NOT running/);

  // And the recovery steps are present, with the commands readable.
  const recovery = screen.getByLabelText("Recovery steps");
  expect(recovery).toHaveTextContent("Both containers are on this host");
  expect(recovery).toHaveTextContent(`docker rename web.hm-old-${executionID} web`);
});

it("never renders an unreached verification as a pass", async () => {
  mockApi([
    [
      "/executions/",
      {
        execution: execution({
          state: "failed",
          failure: "unhealthy",
          checkpoint: "replacementQuarantined",
          originalRemoved: false,
          verification: {
            health: "failed",
            healthChecked: true,
            healthState: "unhealthy",
            image: "unknown",
            preservation: "unknown",
            network: "unknown",
          },
        }),
        events: [],
      },
    ],
  ]);

  renderDetail();

  // "not checked" rather than anything that could read as success.
  expect(await screen.findByText("Image: not checked")).toBeInTheDocument();
  expect(screen.getByText("Configuration: not checked")).toBeInTheDocument();
  expect(screen.getByText("Networks: not checked")).toBeInTheDocument();
  expect(screen.getByText("Health: failed")).toBeInTheDocument();

  expect(screen.queryByText("Image: passed")).not.toBeInTheDocument();
});

it("says nothing was changed when a recreation was refused before mutating", async () => {
  mockApi([
    [
      "/executions/",
      {
        execution: execution({
          state: "failed",
          failure: "preflight",
          refusal: "planSuperseded",
          checkpoint: "",
          originalRemoved: false,
          verification: {
            health: "unknown",
            healthChecked: false,
            image: "unknown",
            preservation: "unknown",
            network: "unknown",
          },
        }),
        events: [],
      },
    ],
  ]);

  renderDetail();

  expect(
    await screen.findByText(/Nothing on this host was changed/i),
  ).toBeInTheDocument();
  expect(screen.queryByLabelText("Recovery steps")).not.toBeInTheDocument();
});

it("does not offer cancellation once the host has been changed", async () => {
  mockApi([
    [
      "/executions/",
      {
        execution: execution({
          state: "verifying",
          checkpoint: "replacementStarted",
          originalRemoved: false,
          mutatedAt: "2026-08-06T12:00:05Z",
          replacementId: replacementID,
        }),
        events: [],
      },
    ],
  ]);

  renderDetail();

  await screen.findByText(/Verifying/);
  expect(
    screen.queryByRole("button", { name: /cancel this recreation/i }),
  ).not.toBeInTheDocument();
});

it("offers cancellation while nothing has been changed", async () => {
  mockApi([
    [
      "/executions/",
      {
        execution: execution({
          state: "validating",
          checkpoint: "",
          originalRemoved: false,
          verification: {
            health: "unknown",
            healthChecked: false,
            image: "unknown",
            preservation: "unknown",
            network: "unknown",
          },
        }),
        events: [],
      },
    ],
  ]);

  renderDetail();

  const button = await screen.findByRole("button", {
    name: /cancel this recreation/i,
  });
  expect(button).toBeInTheDocument();
});

it("shows the configuration differences without showing a secret", async () => {
  mockApi([
    [
      "/executions/",
      {
        execution: execution({
          state: "failed",
          failure: "preservation",
          checkpoint: "replacementQuarantined",
          originalRemoved: false,
          mutatedAt: "2026-08-06T12:00:05Z",
          verification: {
            health: "passed",
            healthChecked: true,
            healthState: "healthy",
            image: "passed",
            preservation: "failed",
            network: "unknown",
            preservationReport: {
              status: "failed",
              checked: 40,
              matched: 39,
              differences: [
                {
                  field: "environment",
                  kind: "changed",
                  expected: "DB_PASSWORD=digest:aaaa",
                  actual: "DB_PASSWORD=digest:bbbb",
                },
              ],
            },
          },
        }),
        events: [],
      },
    ],
  ]);

  renderDetail();

  expect(await screen.findByText("environment")).toBeInTheDocument();
  expect(screen.getByText("DB_PASSWORD=digest:aaaa")).toBeInTheDocument();
  // The whole point of the keyed digest: the value never appears.
  expect(screen.queryByText(/hunter2/)).not.toBeInTheDocument();
});

it("renders a hostile message as text rather than markup", async () => {
  const hostile = '<img src=x onerror="alert(1)">';
  mockApi([
    [
      "/executions/",
      {
        execution: execution({
          state: "failed",
          failure: "internal",
          checkpoint: "",
          originalRemoved: false,
          message: hostile,
          verification: {
            health: "unknown",
            healthChecked: false,
            image: "unknown",
            preservation: "unknown",
            network: "unknown",
          },
        }),
        events: [{ state: "failed", detail: hostile, at: "2026-08-06T12:00:00Z" }],
      },
    ],
  ]);

  const { container } = renderDetail();

  await screen.findByText(/Nothing on this host was changed/i);
  expect(container.querySelector("img")).toBeNull();
});

// ------------------------------------------------------------- the action --

it("requires two steps and the container name before recreating", async () => {
  const user = userEvent.setup();
  mockApi([["/executions", execution({ state: "queued" }), 202]]);

  render(
    <MemoryRouter>
      <RecreateContainerAction acquisition={acquisition()} />
    </MemoryRouter>,
  );

  // One click opens the confirmation; it does not act.
  await user.click(screen.getByRole("button", { name: /recreate container/i }));
  expect(writes).toHaveLength(0);

  const dialog = screen.getByRole("dialog");

  // The three facts that must be stated.
  expect(dialog).toHaveTextContent(/will be stopped and recreated/i);
  expect(dialog).toHaveTextContent(/image is already on this host/i);
  expect(dialog).toHaveTextContent(/Rollback is NOT automatic/i);

  // And the digest, not just the tag.
  expect(dialog).toHaveTextContent(testDigest);

  // The confirm button is disabled until the name is typed.
  const confirm = within(dialog).getByRole("button", {
    name: /stop and recreate web/i,
  });
  expect(confirm).toBeDisabled();

  await user.type(within(dialog).getByRole("textbox"), "web");
  expect(confirm).toBeEnabled();

  await user.click(confirm);

  await waitFor(() => expect(writes).toHaveLength(1));

  const write = writes[0]!;
  expect(write.method).toBe("POST");
  expect(write.url).toContain("/executions");
});

it("sends an acquisition id and nothing else", async () => {
  const user = userEvent.setup();
  mockApi([["/executions", execution({ state: "queued" }), 202]]);

  render(
    <MemoryRouter>
      <RecreateContainerAction acquisition={acquisition()} />
    </MemoryRouter>,
  );

  await user.click(screen.getByRole("button", { name: /recreate container/i }));
  await user.type(screen.getByRole("textbox"), "web");
  await user.click(screen.getByRole("button", { name: /stop and recreate web/i }));

  await waitFor(() => expect(writes).toHaveLength(1));

  const body = JSON.parse(writes[0]!.body) as Record<string, unknown>;
  expect(Object.keys(body)).toEqual(["acquisitionId"]);
  expect(body.acquisitionId).toBe(acquisitionID);
});

it("does not offer the action for an acquisition that did not succeed", () => {
  render(
    <MemoryRouter>
      <RecreateContainerAction acquisition={acquisition({ state: "failed" })} />
    </MemoryRouter>,
  );

  expect(
    screen.queryByRole("button", { name: /recreate container/i }),
  ).not.toBeInTheDocument();
  expect(
    screen.getByText(/has not been downloaded and verified/i),
  ).toBeInTheDocument();
});

it("reports a refusal without acting", async () => {
  const user = userEvent.setup();
  mockApi([
    [
      "/executions",
      {
        error: {
          code: "conflict",
          message:
            "that acquisition has already been used for a recreation; generate a fresh plan and acquire the image again",
        },
        refusal: "acquisitionConsumed",
      },
      409,
    ],
  ]);

  render(
    <MemoryRouter>
      <RecreateContainerAction acquisition={acquisition()} />
    </MemoryRouter>,
  );

  await user.click(screen.getByRole("button", { name: /recreate container/i }));
  await user.type(screen.getByRole("textbox"), "web");
  await user.click(screen.getByRole("button", { name: /stop and recreate web/i }));

  const alert = await screen.findByRole("alert");
  expect(alert).toHaveTextContent(/already been used for a recreation/i);
});
