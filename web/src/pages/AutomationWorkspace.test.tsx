import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { Automation } from "./Automation";
import { healthState } from "../test/fixtures";
import { TestSessionProvider, testSession, testUser } from "../test/session";

/**
 * The consolidated Automation workspace (Phase 3).
 *
 * # What this defends
 *
 * The answers to "is automatic updating on, what will it touch, what will it
 * not touch and why, when does it run, and is anything waiting for me" used to
 * be spread across four screens named after parts of the engine. They are now
 * one page.
 *
 * The properties that matter are about what consolidation must NOT do:
 *
 *   - it must not form a second opinion. Every count is a field the server
 *     sent; nothing here recomputes eligibility, a window, or a policy's reach;
 *   - it must not merge two different waiting states. A held automation
 *     DECISION and a plan needing manual review are cleared from different
 *     queues, and each link must go to the one that can clear it;
 *   - it must not grant anything. A viewer sees the state and no controls;
 *   - it must not reimplement the resume flow, the review workflow, or the
 *     policy editor.
 */

const originalFetch = globalThis.fetch;
let routes: [string, unknown, number?][] = [];
let requests: { url: string; method: string }[] = [];

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function pagination(total = 0) {
  return {
    page: 1,
    pageSize: 50,
    totalItems: total,
    totalPages: 1,
    hasNext: false,
    hasPrevious: false,
  };
}

function status(overrides: Record<string, unknown> = {}) {
  return {
    status: {
      enabled: true,
      running: false,
      policies: 2,
      enabledPolicies: 1,
      actingPolicies: 1,
      pausedContainers: 0,
      awaitingApproval: 0,
      windowOpen: true,
      nextRunAt: "2026-08-01T02:00:00Z",
      ...overrides,
    },
    history: { total: 0, completed: 0, failed: 0, submitted: 0 },
  };
}

function policy(overrides: Record<string, unknown> = {}) {
  return {
    policyId: "upd_00000000000000000001",
    name: "Keep containers updated",
    enabled: true,
    priority: 10,
    scope: "allEligible",
    selector: {},
    strategy: "minor",
    minimumRecommendation: "proceedWithCaution",
    mode: "automatic",
    window: { alwaysOpen: false, start: "02:00", end: "04:00", timezone: "UTC" },
    failure: { autoRollback: true, pauseAfterFailures: 2 },
    ...overrides,
  };
}

function pause(overrides: Record<string, unknown> = {}) {
  return {
    containerName: "hm13-failguard",
    reason: "repeatedFailure",
    detail: "the replacement did not become healthy",
    failures: 2,
    pausedAt: "2026-08-01T01:00:00Z",
    ...overrides,
  };
}

function mockApi(extra: [string, unknown, number?][] = []) {
  routes = extra;
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    requests.push({ url, method });

    if (url.includes("/auth/session")) {
      return jsonResponse({ user: testUser("administrator") });
    }
    for (const [fragment, body, code] of routes) {
      if (url.includes(fragment)) return jsonResponse(body, code ?? 200);
    }
    // Everything the page reads but this case does not care about.
    if (url.includes("/health")) {
      return jsonResponse({
        status: "healthy",
        database: { status: "up" },
        docker: { status: "up" },
        events: { status: "up" },
        checkedAt: "2026-08-01T00:00:00Z",
        uptimeSeconds: 1,
        features: { planner: true, automation: true },
      });
    }
    if (url.includes("/inventory")) return jsonResponse({ generation: 4 });
    if (url.includes("/dependencies")) return jsonResponse({ items: [], total: 0 });
    return jsonResponse({ items: [], pagination: pagination() });
  }) as typeof fetch;
}

/** The reads the workspace joins. */
function withState(options: {
  engine?: Record<string, unknown>;
  policies?: unknown[];
  pauses?: unknown[];
  dependencies?: number;
  decisions?: unknown[];
} = {}): [string, unknown, number?][] {
  return [
    ["/automation/paused", { items: options.pauses ?? [] }],
    ["/automation/upcoming", { items: options.decisions ?? [], eligible: 0, truncated: false }],
    ["/automation/runs", { items: [], pagination: pagination() }],
    ["/automation", status(options.engine)],
    ["/update-policies", { items: options.policies ?? [policy()], pagination: pagination(1) }],
    [
      "/dependencies",
      { items: [], total: options.dependencies ?? 0 },
    ],
    ["/plans", { items: [], planner: { lastRunAt: "2026-08-01T00:00:00Z" }, pagination: pagination() }],
  ];
}

function renderAutomation(role: "viewer" | "operator" | "administrator" = "administrator") {
  return render(
    <TestSessionProvider session={testSession({ user: testUser(role) })}>
      <MemoryRouter initialEntries={["/automation"]}>
        <Routes>
          <Route path="/automation" element={<Automation health={healthState()} />} />
        </Routes>
      </MemoryRouter>
    </TestSessionProvider>,
  );
}

beforeEach(() => {
  requests = [];
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

// ------------------------------------------------------------- the state --

it("says whether automatic updating is on, and what that means", async () => {
  mockApi(withState());
  renderAutomation();

  const summary = await screen.findByTestId("automation-summary");
  expect(within(summary).getByText("On")).toBeInTheDocument();
  expect(within(summary).getByText(/1 policy may update containers/i)).toBeInTheDocument();
  expect(within(summary).getByText("1 of 2")).toBeInTheDocument();
});

it("distinguishes an engine that is off from policies that only watch", async () => {
  // Two different situations that both change nothing, with opposite remedies:
  // one is a deployment setting, the other is a field in the policy editor.
  mockApi(withState({ engine: { actingPolicies: 0 } }));
  renderAutomation();

  const summary = await screen.findByTestId("automation-summary");
  expect(within(summary).getByText("Watching only")).toBeInTheDocument();
  expect(within(summary).queryByText("Off")).not.toBeInTheDocument();
});

// ---------------------------------------------------------- the settings --

it("summarises the active policy without opening the policy engine", async () => {
  mockApi(withState());
  renderAutomation();

  const settings = await screen.findByTestId("automation-settings");
  expect(within(settings).getByText("Keep containers updated")).toBeInTheDocument();
  expect(within(settings).getByText("All eligible containers")).toBeInTheDocument();
  expect(within(settings).getByText("Up to a minor version")).toBeInTheDocument();
  expect(within(settings).getByText(/02:00–04:00 UTC/)).toBeInTheDocument();
  expect(within(settings).getByText(/Roll back automatically, pause after 2 failures/)).toBeInTheDocument();
  // The mode is summarised in operational terms AND keeps its own label.
  expect(within(settings).getByText(/May change containers — Automatic/)).toBeInTheDocument();
  // The full editor is linked, not duplicated.
  expect(within(settings).getByRole("link", { name: /edit settings/i })).toHaveAttribute(
    "href",
    "/update-policies",
  );
});

it("says plainly when nothing is enabled", async () => {
  mockApi(withState({ policies: [policy({ enabled: false })] }));
  renderAutomation();

  const settings = await screen.findByTestId("automation-settings");
  expect(
    within(settings).getByText(/will not update anything on its own/i),
  ).toBeInTheDocument();
});

// --------------------------------------------------------------- waiting --

it("sends each waiting state to the queue that can clear it", async () => {
  // These are different records with different remedies. A held automation
  // DECISION is released from the approvals queue; a plan needing manual review
  // is answered in Updates. Crossing them offers a control that cannot work.
  mockApi(
    withState({
      engine: { awaitingApproval: 2 },
      decisions: [
        {
          runId: "",
          containerId: "c9",
          containerName: "bash",
          verdict: "skip",
          reason: "recommendation",
          recommendation: "manualReview",
          position: 0,
          decidedAt: "2026-08-01T00:00:00Z",
        },
      ],
    }),
  );
  renderAutomation();

  const attention = await screen.findByTestId("automation-attention");
  expect(
    within(attention).getByRole("link", { name: /review and release/i }),
  ).toHaveAttribute("href", "/automation/approvals");
  expect(
    within(attention).getByRole("link", { name: /review in updates/i }),
  ).toHaveAttribute("href", "/updates?tab=review");
});

it("shows no attention panel when nothing is waiting", async () => {
  mockApi(withState());
  renderAutomation();

  await screen.findByTestId("automation-summary");
  expect(screen.queryByTestId("automation-attention")).not.toBeInTheDocument();
});

it("reports a closed window as the reason nothing will happen", async () => {
  mockApi(
    withState({
      engine: { windowOpen: false, nextWindowOpensAt: "2026-08-02T02:00:00Z" },
    }),
  );
  renderAutomation();

  const attention = await screen.findByTestId("automation-attention");
  expect(within(attention).getByText(/maintenance window is closed/i)).toBeInTheDocument();
});

// ---------------------------------------------------------------- paused --

it("shows paused containers where an operator looks for them", async () => {
  mockApi(withState({ engine: { pausedContainers: 1 }, pauses: [pause()] }));
  renderAutomation();

  const paused = await screen.findByTestId("automation-paused");
  expect(within(paused).getByText("hm13-failguard")).toBeInTheDocument();
  // The paused page's own card, so the confirmed resume is not reimplemented.
  expect(
    within(paused).getByRole("button", { name: /resume automatic updates/i }),
  ).toBeInTheDocument();
  expect(
    within(paused).getByRole("link", { name: /full paused list/i }),
  ).toHaveAttribute("href", "/automation/paused");
});

it("offers a viewer no resume", async () => {
  mockApi(withState({ engine: { pausedContainers: 1 }, pauses: [pause()] }));
  renderAutomation("viewer");

  const paused = await screen.findByTestId("automation-paused");
  expect(within(paused).getByText("hm13-failguard")).toBeInTheDocument();
  expect(
    within(paused).queryByRole("button", { name: /resume automatic updates/i }),
  ).not.toBeInTheDocument();
});

it("shows no paused section when nothing is held", async () => {
  mockApi(withState());
  renderAutomation();

  await screen.findByTestId("automation-summary");
  expect(screen.queryByTestId("automation-paused")).not.toBeInTheDocument();
});

// -------------------------------------------------------------- ordering --

it("keeps update order to one sentence when no dependencies exist", async () => {
  mockApi(withState({ dependencies: 0 }));
  renderAutomation();

  const order = await screen.findByTestId("automation-order");
  expect(
    within(order).getByText(/No dependencies configured/i),
  ).toBeInTheDocument();
  expect(
    within(order).getByRole("link", { name: /configure dependencies/i }),
  ).toHaveAttribute("href", "/dependencies");
});

it("summarises configured dependencies rather than diagramming them", async () => {
  mockApi(withState({ dependencies: 3 }));
  renderAutomation();

  const order = await screen.findByTestId("automation-order");
  expect(within(order).getByText(/3 dependency relationships/i)).toBeInTheDocument();
});

// ---------------------------------------------------------- permissions --

it("offers a viewer no way to change anything", async () => {
  mockApi(withState({ dependencies: 2 }));
  renderAutomation("viewer");

  await screen.findByTestId("automation-summary");

  // Reading is fine; every mutation is absent.
  expect(screen.queryByRole("button", { name: /^run pass$/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /dry run/i })).not.toBeInTheDocument();
  expect(
    screen.getByRole("link", { name: /view settings/i }),
  ).toBeInTheDocument();
  expect(
    screen.getByRole("link", { name: /view dependencies/i }),
  ).toBeInTheDocument();
});

// --------------------------------------------------------------- states --

it("shows an error state rather than an empty workspace", async () => {
  mockApi([
    ["/automation/paused", { items: [] }],
    ["/automation/upcoming", { items: [], eligible: 0, truncated: false }],
    ["/automation/runs", { items: [], pagination: pagination() }],
    ["/automation", { error: { code: "internal", message: "automation status unavailable" } }, 500],
  ]);
  renderAutomation();

  await waitFor(() =>
    expect(screen.getByText(/automation status unavailable/i)).toBeInTheDocument(),
  );
});

it("reads only, and writes nothing, when the page is opened", async () => {
  mockApi(withState({ engine: { pausedContainers: 1 }, pauses: [pause()], dependencies: 2 }));
  renderAutomation();

  await screen.findByTestId("automation-summary");
  expect(requests.filter((r) => r.method !== "GET")).toEqual([]);
});
