import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { Activity } from "./Activity";
import { TestSessionProvider, testSession, testUser } from "../test/session";

/**
 * The consolidated Activity workspace (Phase 4).
 *
 * # What this defends
 *
 * "What happened to my containers?" used to require knowing that a recreation
 * is an execution, that its undo is a separate rollback record, and that a
 * download is a third one -- then reading three lists.
 *
 * The properties that matter are about correctness of the join, not the look:
 *
 *   - a failure that was ROLLED BACK successfully is recovered, not an
 *     unresolved emergency, and must leave the attention list;
 *   - a failure with no concluded rollback must be easy to find;
 *   - an execution may have SEVERAL rollback attempts; the succeeded one
 *     decides;
 *   - records whose parent fell outside the loaded page must still appear,
 *     because dropping them makes the history quietly wrong;
 *   - in-progress is never rendered as complete;
 *   - opening the page writes nothing.
 */

const originalFetch = globalThis.fetch;
let requests: { url: string; method: string }[] = [];
let routes: [string, unknown, number?][] = [];

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

function acquisition(overrides: Record<string, unknown> = {}) {
  return {
    acquisitionId: "acq_0001",
    planId: "plan_0001",
    containerId: "c1",
    containerName: "redis",
    state: "succeeded",
    requestedAt: "2026-08-01T10:00:00Z",
    completedAt: "2026-08-01T10:01:00Z",
    expiresAt: "2026-09-01T00:00:00Z",
    target: { repository: "redis", reference: "redis:7.4.1", digest: "sha256:" + "b".repeat(64) },
    ...overrides,
  };
}

function execution(overrides: Record<string, unknown> = {}) {
  return {
    executionId: "exec_0001",
    acquisitionId: "acq_0001",
    planId: "plan_0001",
    containerId: "c1",
    containerName: "redis",
    oldImage: "redis:7.4.0",
    target: { repository: "redis", reference: "redis:7.4.1", digest: "sha256:" + "b".repeat(64) },
    state: "succeeded",
    originalRemoved: false,
    verification: {},
    requestedAt: "2026-08-01T10:02:00Z",
    completedAt: "2026-08-01T10:03:00Z",
    expiresAt: "2026-09-01T00:00:00Z",
    ...overrides,
  };
}

function rollback(overrides: Record<string, unknown> = {}) {
  return {
    rollbackId: "rbk_0001",
    executionId: "exec_0001",
    containerName: "redis",
    originalId: "c1",
    parkedName: "redis.hm-old-exec_0001",
    replacementId: "c2",
    state: "succeeded",
    verification: {},
    requestedAt: "2026-08-01T10:04:00Z",
    completedAt: "2026-08-01T10:05:00Z",
    expiresAt: "2026-09-01T00:00:00Z",
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
    return jsonResponse({ items: [], pagination: pagination(), summary: {} });
  }) as typeof fetch;
}

/** The three independently-paginated reads the feed joins. */
function withRecords(options: {
  acquisitions?: unknown[];
  executions?: unknown[];
  rollbacks?: unknown[];
} = {}): [string, unknown, number?][] {
  return [
    ["/acquisitions", { items: options.acquisitions ?? [], pagination: pagination(), summary: {} }],
    ["/executions", { items: options.executions ?? [], pagination: pagination(), summary: {} }],
    ["/rollbacks", { items: options.rollbacks ?? [], pagination: pagination(), summary: {} }],
  ];
}

function renderActivity(role: "viewer" | "operator" | "administrator" = "administrator") {
  return render(
    <TestSessionProvider session={testSession({ user: testUser(role) })}>
      <MemoryRouter initialEntries={["/activity"]}>
        <Routes>
          <Route path="/activity" element={<Activity />} />
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

// -------------------------------------------------------------- the feed --

it("shows a completed update as one thing that happened", async () => {
  mockApi(withRecords({ acquisitions: [acquisition()], executions: [execution()] }));
  renderActivity();

  const row = (await screen.findByText("redis")).closest("li") as HTMLElement;
  expect(within(row).getByText("Updated")).toBeInTheDocument();
  expect(within(row).getByText(/redis:7\.4\.0 → redis:7\.4\.1/)).toBeInTheDocument();
  expect(within(row).getByText(/recreated and verified/i)).toBeInTheDocument();

  // One entry, not a download row and a recreation row.
  expect(within(screen.getByTestId("activity-feed")).getAllByRole("listitem")).toHaveLength(1);
});

it("shows a download that has not been applied, with its continuation", async () => {
  mockApi(withRecords({ acquisitions: [acquisition()] }));
  renderActivity();

  const row = (await screen.findByText("redis")).closest("li") as HTMLElement;
  expect(within(row).getByText("Ready to apply")).toBeInTheDocument();
  expect(within(row).getByText(/has not been recreated/i)).toBeInTheDocument();
  // Phase 2's control, on the acquisition record this page already holds.
  expect(
    within(row).getByRole("button", { name: /recreate container/i }),
  ).toBeInTheDocument();
});

it("does not render an in-flight operation as finished", async () => {
  mockApi(
    withRecords({
      acquisitions: [acquisition()],
      executions: [execution({ state: "verifying", completedAt: undefined })],
    }),
  );
  renderActivity();

  const row = (await screen.findByText("redis")).closest("li") as HTMLElement;
  expect(within(row).getByText("Updating")).toBeInTheDocument();
  expect(within(row).getByText(/checking that the replacement is healthy/i)).toBeInTheDocument();
  expect(within(row).queryByText("Updated")).not.toBeInTheDocument();
});

it("orders the feed newest first across record types", async () => {
  mockApi(
    withRecords({
      acquisitions: [
        acquisition(),
        acquisition({
          acquisitionId: "acq_0002",
          planId: "plan_0002",
          containerId: "c9",
          containerName: "later-download",
          requestedAt: "2026-08-01T12:00:00Z",
          completedAt: "2026-08-01T12:01:00Z",
        }),
      ],
      executions: [execution()],
    }),
  );
  renderActivity();

  await screen.findByTestId("activity-feed");
  const names = within(screen.getByTestId("activity-feed"))
    .getAllByRole("listitem")
    .map((row) => row.textContent?.match(/later-download|redis/)?.[0]);
  expect(names).toEqual(["later-download", "redis"]);
});

// ------------------------------------------------- failure and recovery --

it("surfaces an unresolved failure and offers the existing recovery route", async () => {
  mockApi(
    withRecords({
      acquisitions: [acquisition()],
      executions: [execution({ state: "failed", message: "the replacement never became healthy" })],
    }),
  );
  renderActivity();

  const attention = await screen.findByTestId("activity-attention");
  expect(within(attention).getByText("redis")).toBeInTheDocument();
  expect(within(attention).getByText("Update failed")).toBeInTheDocument();
  // Rollback lives on the execution record, where the server computes its
  // eligibility. This links there rather than offering a second control.
  expect(
    within(attention).getByRole("link", { name: /review and recover/i }),
  ).toHaveAttribute("href", "/executions/exec_0001");
});

it("treats a failure that was rolled back as recovered, not unresolved", async () => {
  mockApi(
    withRecords({
      acquisitions: [acquisition()],
      executions: [execution({ state: "failed" })],
      rollbacks: [rollback()],
    }),
  );
  renderActivity();

  const row = (await screen.findByText("redis")).closest("li") as HTMLElement;
  expect(within(row).getByText("Recovered")).toBeInTheDocument();
  expect(within(row).getByText(/original container was restored/i)).toBeInTheDocument();

  // The point of the distinction: it is no longer an emergency.
  expect(screen.queryByTestId("activity-attention")).not.toBeInTheDocument();
});

it("keeps a failed rollback as needing attention", async () => {
  mockApi(
    withRecords({
      acquisitions: [acquisition()],
      executions: [execution({ state: "failed" })],
      rollbacks: [rollback({ state: "failed", message: "the original did not start" })],
    }),
  );
  renderActivity();

  const attention = await screen.findByTestId("activity-attention");
  expect(within(attention).getByText("Rollback failed")).toBeInTheDocument();
});

it("lets a succeeded rollback attempt decide, even after a failed one", async () => {
  // The schema constrains only SUCCEEDED rollbacks per execution, so retries
  // are real and the feed must not report the first failure as the outcome.
  mockApi(
    withRecords({
      acquisitions: [acquisition()],
      executions: [execution({ state: "failed" })],
      rollbacks: [
        rollback({ rollbackId: "rbk_0002", state: "failed", requestedAt: "2026-08-01T10:06:00Z" }),
        rollback({ rollbackId: "rbk_0001", state: "succeeded" }),
      ],
    }),
  );
  renderActivity();

  const row = (await screen.findByText("redis")).closest("li") as HTMLElement;
  expect(within(row).getByText("Recovered")).toBeInTheDocument();
  expect(screen.queryByTestId("activity-attention")).not.toBeInTheDocument();
});

// -------------------------------------------- independent pagination --

it("still shows a recreation whose download fell outside the loaded page", async () => {
  // The three lists page independently. Dropping an orphan would make the
  // history quietly wrong rather than merely incomplete.
  mockApi(withRecords({ executions: [execution()] }));
  renderActivity();

  const row = (await screen.findByText("redis")).closest("li") as HTMLElement;
  expect(within(row).getByText("Updated")).toBeInTheDocument();
});

it("still shows a rollback whose recreation fell outside the loaded page", async () => {
  mockApi(withRecords({ rollbacks: [rollback()] }));
  renderActivity();

  const row = (await screen.findByText("redis")).closest("li") as HTMLElement;
  expect(within(row).getByText("Recovered")).toBeInTheDocument();
});

it("says how far back the merged window is complete", async () => {
  mockApi(
    withRecords({
      acquisitions: [acquisition()],
      executions: [execution()],
      rollbacks: [rollback()],
    }),
  );
  renderActivity();

  await screen.findByTestId("activity-feed");
  expect(screen.getByText(/complete back to/i)).toBeInTheDocument();
  expect(
    screen.getByRole("link", { name: /update history/i }),
  ).toHaveAttribute("href", "/executions");
});

// ------------------------------------------------------------ filtering --

it("filters by type and by search over loaded data", async () => {
  mockApi(
    withRecords({
      acquisitions: [
        acquisition(),
        acquisition({
          acquisitionId: "acq_0003",
          containerId: "c3",
          containerName: "plex",
          requestedAt: "2026-08-01T09:00:00Z",
        }),
      ],
      executions: [execution()],
    }),
  );
  renderActivity();

  await screen.findByTestId("activity-feed");

  await userEvent.click(screen.getByRole("tab", { name: "Downloads" }));
  expect(screen.getByText("plex")).toBeInTheDocument();
  expect(screen.queryByText("redis")).not.toBeInTheDocument();

  await userEvent.click(screen.getByRole("tab", { name: "All" }));
  await userEvent.type(screen.getByRole("searchbox", { name: /search/i }), "plex");
  expect(screen.getByText("plex")).toBeInTheDocument();
  expect(screen.queryByText("redis")).not.toBeInTheDocument();
});

// ---------------------------------------------------------- permissions --

it("offers a viewer no continuation action", async () => {
  mockApi(withRecords({ acquisitions: [acquisition()] }));
  renderActivity("viewer");

  const row = (await screen.findByText("redis")).closest("li") as HTMLElement;
  expect(
    within(row).queryByRole("button", { name: /recreate container/i }),
  ).not.toBeInTheDocument();
  expect(within(row).getByText(/needs the execution permission/i)).toBeInTheDocument();
});

it("writes nothing when the page is opened", async () => {
  mockApi(
    withRecords({
      acquisitions: [acquisition()],
      executions: [execution({ state: "failed" })],
      rollbacks: [rollback({ state: "failed" })],
    }),
  );
  renderActivity();

  await screen.findByTestId("activity-attention");
  expect(requests.filter((r) => r.method !== "GET")).toEqual([]);
});

// -------------------------------------------------------------- states --

it("shows a loading state before the first response", () => {
  mockApi(withRecords({ executions: [execution()] }));
  renderActivity();
  expect(screen.getByRole("status", { name: /loading activity/i })).toBeInTheDocument();
});

it("shows an empty state rather than inventing history", async () => {
  mockApi(withRecords());
  renderActivity();
  expect(await screen.findByText(/nothing has happened yet/i)).toBeInTheDocument();
});

it("says so when the history could not be read", async () => {
  mockApi([
    ["/acquisitions", { items: [], pagination: pagination(), summary: {} }],
    ["/rollbacks", { items: [], pagination: pagination(), summary: {} }],
    ["/executions", { error: { code: "internal", message: "update history unavailable" } }, 500],
  ]);
  renderActivity();

  await waitFor(() =>
    expect(screen.getByText(/update history unavailable/i)).toBeInTheDocument(),
  );
});
