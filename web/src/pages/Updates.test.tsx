import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import type { Acquisition } from "../api/acquisitionTypes";
import type { AutomationDecision } from "../api/automationTypes";
import type { ChangePlan } from "../api/planTypes";
import { TestSessionProvider, testSession, testUser } from "../test/session";
import { Updates } from "./Updates";

/**
 * The consolidated Updates workspace.
 *
 * # What this is defending
 *
 * Applying an update used to mean four screens: the review list decided, the
 * container plan page downloaded, the acquisition page recreated, and nothing
 * on any of them named the next one. The workspace puts the whole sequence on
 * one page.
 *
 * The properties that matter are about what it did NOT do to get there:
 *
 *   - the three server operations remain three, in the order the server
 *     requires. Nothing here approves and downloads in one click, and nothing
 *     downloads and recreates in one click;
 *   - each step is still anchored to the record that authorises it -- a
 *     download names a plan, a recreation names a succeeded acquisition;
 *   - every confirmation the individual actions used to show, they still show;
 *   - permissions are unchanged. A viewer gains nothing from consolidation.
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

function pagination(total: number) {
  return {
    page: 1,
    pageSize: 100,
    totalItems: total,
    totalPages: 1,
    hasNext: false,
    hasPrevious: false,
  };
}

function plan(overrides: Partial<ChangePlan> = {}): ChangePlan {
  return {
    planId: "plan_00000000000000000001",
    containerId: "c1",
    containerName: "redis",
    currentImage: "redis:7.2.4",
    proposedImage: "redis:7.2.5",
    currentDigest: "sha256:" + "a".repeat(64),
    proposedDigest: "sha256:" + "b".repeat(64),
    updateType: "patch",
    snapshotAvailable: true,
    restoreReadiness: "ready",
    driftOpen: 0,
    policyOpen: 0,
    registryStatus: "ok",
    planVersion: 1,
    plannerVersion: "1",
    inputDigest: "abc",
    generatedAt: "2026-08-01T00:00:00Z",
    superseded: false,
    risk: {
      riskScore: 10,
      riskBand: "low",
      recommendation: "proceed",
      summary: "Nothing in the evidence argues against this change",
      factors: [],
    },
    ...overrides,
  } as ChangePlan;
}

function decision(overrides: Partial<AutomationDecision> = {}): AutomationDecision {
  return {
    runId: "",
    containerId: "c1",
    containerName: "redis",
    verdict: "skip",
    reason: "noPolicy",
    position: 0,
    decidedAt: "2026-08-01T00:00:00Z",
    ...overrides,
  } as AutomationDecision;
}

function acquisition(overrides: Partial<Acquisition> = {}): Acquisition {
  return {
    acquisitionId: "acq_00000000000000000001",
    planId: "plan_00000000000000000001",
    containerId: "c1",
    containerName: "redis",
    state: "succeeded",
    requestedAt: "2026-08-01T00:00:00Z",
    target: { repository: "redis", reference: "redis:7.2.5", digest: "sha256:" + "b".repeat(64) },
    ...overrides,
  } as unknown as Acquisition;
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
    for (const [fragment, body, status] of routes) {
      if (url.includes(fragment)) return jsonResponse(body, status ?? 200);
    }
    return jsonResponse({ items: [], pagination: pagination(0) });
  }) as typeof fetch;
}

function renderUpdates(role: "viewer" | "operator" | "administrator" = "administrator") {
  return render(
    <TestSessionProvider session={testSession({ user: testUser(role) })}>
      <MemoryRouter initialEntries={["/updates"]}>
        <Routes>
          <Route path="/updates" element={<Updates />} />
        </Routes>
      </MemoryRouter>
    </TestSessionProvider>,
  );
}

/** The three reads the workspace joins, with one ready update by default. */
function withRows(
  plans: ChangePlan[],
  decisions: AutomationDecision[] = [],
  acquisitions: Acquisition[] = [],
): [string, unknown, number?][] {
  return [
    ["/automation/upcoming", { items: decisions, eligible: 0, truncated: false }],
    ["/acquisitions", { items: acquisitions, pagination: pagination(acquisitions.length) }],
    ["/plans", { items: plans, pagination: pagination(plans.length) }],
  ];
}

beforeEach(() => {
  requests = [];
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

// -------------------------------------------------------------- the list --

it("shows an available update with its assessment and automation context", async () => {
  mockApi(
    withRows(
      [plan()],
      [decision({ verdict: "wouldUpdate", reason: "eligible" })],
    ),
  );
  renderUpdates();

  const row = await screen.findByText("redis");
  const card = row.closest("li") as HTMLElement;

  expect(within(card).getByText(/redis:7\.2\.4/)).toBeInTheDocument();
  expect(within(card).getByText(/redis:7\.2\.5/)).toBeInTheDocument();
  expect(within(card).getByText("Ready")).toBeInTheDocument();
  // The engine's own answer, so the operator knows not to click anything.
  expect(within(card).getByText("Automatic")).toBeInTheDocument();
});

it("separates what needs a person from what does not", async () => {
  mockApi(
    withRows([
      plan(),
      plan({
        planId: "plan_00000000000000000002",
        containerId: "c2",
        containerName: "bash",
        currentImage: "bash:4.4",
        proposedImage: "bash:5.3",
        updateType: "major",
        risk: {
          riskScore: 50,
          riskBand: "high",
          recommendation: "manualReview",
          summary: "Worth a look first: this is a major version change",
          factors: [],
        },
      } as Partial<ChangePlan>),
    ]),
  );
  renderUpdates();

  // Available shows the ready one only.
  await screen.findByText("redis");
  expect(screen.queryByText("bash")).not.toBeInTheDocument();

  await userEvent.click(screen.getByRole("tab", { name: /needs review/i }));

  expect(await screen.findByText("bash")).toBeInTheDocument();
  expect(screen.queryByText("redis")).not.toBeInTheDocument();
  // And it says WHY, on the row, without navigating anywhere.
  expect(screen.getByText(/major version change/i)).toBeInTheDocument();
});

it("counts what could not be assessed separately from what is available", async () => {
  mockApi(
    withRows([
      plan(),
      plan({
        planId: "plan_00000000000000000003",
        containerId: "c3",
        containerName: "sftpgo",
        proposedImage: "",
        proposedDigest: undefined,
        updateType: "unknown",
        risk: {
          riskScore: 52,
          riskBand: "high",
          recommendation: "unknown",
          summary: "Cannot advise: the registry did not answer",
          factors: [],
        },
      } as Partial<ChangePlan>),
    ]),
  );
  renderUpdates();

  await screen.findByText("redis");
  const cannot = screen.getByText("Cannot advise", { selector: "dt" }).closest("div");
  // A gap in evidence is not an available update.
  expect(within(cannot as HTMLElement).getByText("1")).toBeInTheDocument();
});

// ------------------------------------------------------------- the actions --

it("offers the download for a ready update, behind its own confirmation", async () => {
  mockApi(withRows([plan()]));
  renderUpdates();

  const card = (await screen.findByText("redis")).closest("li") as HTMLElement;
  const acquire = within(card).getByRole("button", { name: /acquire image/i });

  await userEvent.click(acquire);

  // The two-step control is intact: the first click confirms, it does not act.
  expect(
    requests.some((r) => r.method === "POST" && r.url.includes("/acquisitions")),
  ).toBe(false);
});

it("offers review first, and the download only once the review is recorded", async () => {
  const review = plan({
    risk: {
      riskScore: 50,
      riskBand: "high",
      recommendation: "manualReview",
      summary: "Worth a look first",
      factors: [],
    },
  } as Partial<ChangePlan>);

  mockApi([
    ...withRows([review]),
    // No approval yet: a 404 is the ordinary state of an unreviewed plan.
    ["/plan-approvals/", { error: { code: "not_found" } }, 404],
  ]);
  renderUpdates();

  await userEvent.click(await screen.findByRole("tab", { name: /needs review/i }));
  const card = (await screen.findByText("redis")).closest("li") as HTMLElement;

  expect(
    await within(card).findByRole("button", { name: /approve this exact update/i }),
  ).toBeInTheDocument();
  // The next step is NOT offered until the review exists.
  expect(
    within(card).queryByRole("button", { name: /acquire image/i }),
  ).not.toBeInTheDocument();
});

it("reveals the download in place once a review already stands", async () => {
  const review = plan({
    risk: {
      riskScore: 50,
      riskBand: "high",
      recommendation: "manualReview",
      summary: "Worth a look first",
      factors: [],
    },
  } as Partial<ChangePlan>);

  mockApi([
    ...withRows([review]),
    [
      "/plan-approvals/",
      {
        approval: {
          planId: review.planId,
          state: "active",
          approvedBy: { userId: "u1", username: "colby" },
          approvedAt: "2026-08-01T01:00:00Z",
        },
        valid: true,
      },
    ],
  ]);
  renderUpdates();

  await userEvent.click(await screen.findByRole("tab", { name: /needs review/i }));
  const card = (await screen.findByText("redis")).closest("li") as HTMLElement;

  // This is the split that used to send an operator to another page.
  expect(
    await within(card).findByRole("button", { name: /acquire image/i }),
  ).toBeInTheDocument();
  expect(within(card).getByText(/approved by colby/i)).toBeInTheDocument();
});

it("offers the recreation once the image is downloaded and verified", async () => {
  mockApi(withRows([plan()], [], [acquisition()]));
  renderUpdates();

  expect(
    await screen.findByText(/image downloaded and verified/i),
  ).toBeInTheDocument();

  const card = screen.getByText("redis").closest("li") as HTMLElement;
  expect(
    within(card).getByRole("button", { name: /recreate container/i }),
  ).toBeInTheDocument();
});

it("does not offer a recreation for a download that has not succeeded", async () => {
  // Only a SUCCEEDED acquisition authorises a recreation, which is the whole
  // reason that step is anchored to the acquisition record.
  mockApi(withRows([plan()], [], [acquisition({ state: "pulling" } as Partial<Acquisition>)]));
  renderUpdates();

  // Awaited: the three reads resolve independently, and the row only knows
  // about the download once the acquisitions read lands.
  expect(await screen.findByText(/downloading/i)).toBeInTheDocument();

  const card = screen.getByText("redis").closest("li") as HTMLElement;
  expect(
    within(card).queryByRole("button", { name: /recreate container/i }),
  ).not.toBeInTheDocument();
});

// --------------------------------------------------------- permissions --

it("offers a viewer no action at all", async () => {
  mockApi(withRows([plan()]));
  renderUpdates("viewer");

  const card = (await screen.findByText("redis")).closest("li") as HTMLElement;
  expect(
    within(card).queryByRole("button", { name: /acquire image/i }),
  ).not.toBeInTheDocument();
  expect(within(card).getByText(/needs the image acquisition permission/i)).toBeInTheDocument();
  // And no registry check either.
  expect(
    screen.queryByRole("button", { name: /check for updates/i }),
  ).not.toBeInTheDocument();
});

it("offers a viewer no recreation on a downloaded image", async () => {
  mockApi(withRows([plan()], [], [acquisition()]));
  renderUpdates("viewer");

  expect(
    await screen.findByText(/needs the execution permission/i),
  ).toBeInTheDocument();

  const card = screen.getByText("redis").closest("li") as HTMLElement;
  expect(
    within(card).queryByRole("button", { name: /recreate container/i }),
  ).not.toBeInTheDocument();
});

// ------------------------------------------------------ check for updates --

it("checks for updates through the existing registry endpoint", async () => {
  mockApi([...withRows([plan()]), ["/images/refresh", { accepted: true }]]);
  renderUpdates();

  await screen.findByText("redis");
  await userEvent.click(screen.getByRole("button", { name: /check for updates/i }));

  await waitFor(() =>
    expect(
      requests.some((r) => r.method === "POST" && r.url.includes("/images/refresh")),
    ).toBe(true),
  );
  // One registry path, not a second one built for this page.
  expect(
    requests.filter((r) => r.method === "POST" && !r.url.includes("/images/refresh")),
  ).toEqual([]);
});

it("reports a failed check rather than claiming success", async () => {
  mockApi([
    ...withRows([plan()]),
    ["/images/refresh", { error: { code: "unavailable", message: "registry engine is disabled" } }, 503],
  ]);
  renderUpdates();

  await screen.findByText("redis");
  await userEvent.click(screen.getByRole("button", { name: /check for updates/i }));

  expect(await screen.findByRole("alert")).toHaveTextContent(/registry engine is disabled/i);
});

// ------------------------------------------------------------ page states --

it("shows a loading state before the first response", () => {
  mockApi(withRows([plan()]));
  renderUpdates();
  expect(screen.getByRole("status", { name: /loading updates/i })).toBeInTheDocument();
});

it("shows an empty state rather than inventing rows", async () => {
  mockApi(withRows([]));
  renderUpdates();
  expect(await screen.findByText(/no updates available/i)).toBeInTheDocument();
});

it("says so when the plans could not be read", async () => {
  mockApi([
    ["/automation/upcoming", { items: [], eligible: 0, truncated: false }],
    ["/acquisitions", { items: [], pagination: pagination(0) }],
    ["/plans", { error: { code: "internal", message: "change plans could not be read" } }, 500],
  ]);
  renderUpdates();

  expect(await screen.findByRole("alert")).toHaveTextContent(/change plans could not be read/i);
});
