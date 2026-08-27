import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import type { ChangePlan, ChangePlanSummary, RiskFactor } from "../api/planTypes";
import { ChangePlans } from "./ChangePlans";
import { ContainerPlan } from "./ContainerPlan";
import { TestSessionProvider, testSession, testUser } from "../test/session";

/**
 * Change plan UI tests.
 *
 * Three properties matter most and are asserted repeatedly:
 *
 *   - "Cannot advise" must NOT read as good news. It means HarborMaster could
 *     not establish something, which is the opposite of a clean bill of health,
 *     and rendering the two the same way is the worst mistake this feature
 *     could make.
 *   - "No plan" must NOT read as "assessed and safe". A container with nothing
 *     proposed has not been given a passing verdict.
 *   - There must be no control anywhere that applies, executes, or approves a
 *     change. The one write requests HarborMaster's own analysis.
 */

const originalFetch = globalThis.fetch;

let requests: string[] = [];
let writes: { url: string; method: string }[] = [];

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
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

function factor(overrides: Partial<RiskFactor> = {}): RiskFactor {
  return {
    rule: "updateClassification",
    points: 5,
    severity: "info",
    detail: "this is a patch version change",
    ...overrides,
  };
}

function plan(overrides: Partial<ChangePlan> = {}): ChangePlan {
  return {
    planId: "plan_00112233445566778899",
    containerId: "container-a",
    containerName: "web",
    currentImage: "nginx:1.27.0",
    proposedImage: "nginx:1.27.1",
    currentDigest: "sha256:aaa",
    proposedDigest: "sha256:bbb",
    updateType: "patch",
    snapshotId: 7,
    snapshotAvailable: true,
    restoreReadiness: "ready",
    driftOpen: 0,
    policyOpen: 0,
    registryStatus: "ok",
    risk: {
      riskScore: 5,
      riskBand: "veryLow",
      recommendation: "proceed",
      summary: "Nothing in the available evidence argues against this change.",
      factors: [factor()],
    },
    planVersion: 1,
    plannerVersion: "1",
    inputDigest: "ccc",
    generatedAt: "2026-08-05T12:00:00Z",
    superseded: false,
    ...overrides,
  };
}

function summary(overrides: Partial<ChangePlanSummary> = {}): ChangePlanSummary {
  return {
    plans: 12,
    containers: 12,
    byRiskBand: { veryLow: 4, medium: 5, critical: 3 },
    byRecommendation: { proceed: 4, manualReview: 5, notRecommended: 3 },
    byUpdateType: { patch: 4, major: 8 },
    actionable: 4,
    needsReview: 5,
    blocked: 3,
    undetermined: 0,
    lastGeneratedAt: "2026-08-05T12:00:00Z",
    plannerVersion: "1",
    ...overrides,
  };
}

/** Installs a fetch double routing by path, matched IN THE ORDER GIVEN. */
function mockApi(routes: [string, unknown][]) {
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString();
    const method = init?.method ?? "GET";
    requests.push(url);
    if (method !== "GET") writes.push({ url, method });

    for (const [fragment, body] of routes) {
      if (url.includes(fragment)) return jsonResponse(body);
    }
    return new Response("{}", { status: 404 });
  }) as typeof fetch;
}

function listRoutes(
  items: ChangePlan[],
  overrides: Partial<ChangePlanSummary> = {},
): [string, unknown][] {
  return [
    [
      "/plans",
      { items, pagination: pagination(items.length), summary: summary(overrides) },
    ],
  ];
}

beforeEach(() => {
  requests = [];
  writes = [];
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

function renderPlans() {
  return render(
    <MemoryRouter initialEntries={["/plans"]}>
      <Routes>
        <Route path="/plans" element={<ChangePlans />} />
      </Routes>
    </MemoryRouter>,
  );
}

function renderContainerPlan() {
  // Wrapped in a session because the plan page now carries the manual-review
  // approval panel, which asks whether this operator may approve. In the app
  // every page is inside a session; this fixture simply had no need of one
  // before.
  return render(
    <TestSessionProvider session={testSession({ user: testUser("operator") })}>
      <MemoryRouter initialEntries={["/plans/container/container-a"]}>
        <Routes>
          <Route path="/plans/container/:id" element={<ContainerPlan />} />
        </Routes>
      </MemoryRouter>
    </TestSessionProvider>,
  );
}

// ------------------------------------------------------------------ list --

it("renders a plan with its verdict and the proposed change", async () => {
  mockApi(listRoutes([plan()]));
  renderPlans();

  expect(await screen.findByText("web")).toBeInTheDocument();
  expect(screen.getByText(/nginx:1\.27\.0/)).toBeInTheDocument();
  expect(screen.getByText(/nginx:1\.27\.1/)).toBeInTheDocument();
  expect(
    screen.getByText("Nothing in the available evidence argues against this change."),
  ).toBeInTheDocument();
});

it("shows the reasoning behind a verdict", async () => {
  mockApi(
    listRoutes([
      plan({
        risk: {
          riskScore: 55,
          riskBand: "high",
          recommendation: "manualReview",
          summary: "Worth a look first: this is a major version change.",
          factors: [
            factor({
              rule: "updateClassification",
              points: 40,
              severity: "warning",
              detail: "this is a major version change",
            }),
            factor({
              rule: "activeDrift",
              points: 15,
              severity: "caution",
              detail: "2 unresolved differences from the baseline snapshot",
            }),
          ],
        },
      }),
    ]),
  );
  renderPlans();

  const disclosure = await screen.findByText(/Why this verdict \(2 factors\)/);
  await userEvent.click(disclosure);

  // Every factor names the RULE that produced it, so a score is traceable to a
  // specific, named piece of code rather than to an opaque total.
  expect(screen.getByText("Size of the change")).toBeInTheDocument();
  expect(screen.getByText("Configuration drift")).toBeInTheDocument();
  expect(screen.getByText("this is a major version change")).toBeInTheDocument();
  expect(screen.getByText("+40")).toBeInTheDocument();
});

// A factor worth stating but not scoring must not read as though it counted
// against the change.
it("distinguishes a stated factor from a scored one", async () => {
  mockApi(
    listRoutes([
      plan({
        risk: {
          riskScore: 0,
          riskBand: "veryLow",
          recommendation: "proceed",
          summary: "Nothing argues against this change.",
          factors: [
            factor({
              rule: "imageAge",
              points: 0,
              severity: "info",
              detail: "the proposed image does not record when it was published",
            }),
          ],
        },
      }),
    ]),
  );
  renderPlans();

  await userEvent.click(await screen.findByText(/Why this verdict/));
  expect(screen.getByText("no score")).toBeInTheDocument();
});

// ----------------------------------------------------- the unknown verdict --

it("does not render 'cannot advise' as good news", async () => {
  mockApi(
    listRoutes(
      [
        plan({
          risk: {
            riskScore: 20,
            riskBand: "low",
            recommendation: "unknown",
            summary:
              "Cannot advise: the most recent registry lookup did not succeed.",
            factors: [
              factor({
                rule: "registryQuality",
                points: 15,
                severity: "unknown",
                detail: "the most recent registry lookup did not succeed",
              }),
            ],
          },
        }),
      ],
      { undetermined: 3, actionable: 4, plans: 12 },
    ),
  );
  renderPlans();

  // The badge says it cannot advise, and its tooltip says what that is NOT.
  const badge = await screen.findByTitle(/NOT the same as safe/i);
  expect(badge).toHaveTextContent(/cannot advise/i);

  // And the count sits beside the rest of the summary rather than being folded
  // into "ready to act on".
  const summarySection = screen.getByLabelText("Plan summary");
  expect(within(summarySection).getByText("Cannot advise")).toBeInTheDocument();
  expect(
    within(summarySection).getByText(/Not a finding of safety/i),
  ).toBeInTheDocument();
  expect(
    screen.getByText(/3 of 12 plans could not be judged/),
  ).toBeInTheDocument();
});

it("says nothing about undetermined plans when there are none", async () => {
  mockApi(listRoutes([plan()], { undetermined: 0 }));
  renderPlans();

  await screen.findByText("web");
  expect(screen.queryByText(/could not be judged/)).not.toBeInTheDocument();
});

// ------------------------------------------------------------- generation --

it("requests a reassessment without applying anything", async () => {
  mockApi([
    ["/plans/generate", { requested: true, planner: { enabled: true } }],
    ...listRoutes([plan()]),
  ]);
  renderPlans();

  const button = await screen.findByRole("button", { name: /reassess/i });
  // The control says what it does: it re-runs analysis and applies nothing.
  expect(button).toHaveAttribute(
    "title",
    expect.stringContaining("Nothing is pulled, changed, or scheduled"),
  );

  await userEvent.click(button);

  await waitFor(() => {
    expect(writes).toHaveLength(1);
  });
  expect(writes[0]?.method).toBe("POST");
  expect(writes[0]?.url).toContain("/plans/generate");
});

// There is no control that applies a plan, because there is no such capability.
it("offers no control that applies, executes, or approves a change", async () => {
  mockApi(listRoutes([plan()]));
  renderPlans();

  await screen.findByText("web");

  for (const forbidden of [/^apply/i, /^execute/i, /^approve/i, /^roll ?back/i, /^pull/i]) {
    expect(screen.queryByRole("button", { name: forbidden })).not.toBeInTheDocument();
  }
});

// -------------------------------------------------------- container view --

it("renders the current plan and the reasoning timeline", async () => {
  const current = plan();
  const earlier = plan({
    planId: "plan_aabbccddeeff00112233",
    superseded: true,
    generatedAt: "2026-07-01T12:00:00Z",
    risk: {
      riskScore: 45,
      riskBand: "medium",
      recommendation: "proceedWithCaution",
      summary: "Probably fine, but note: the container tracks a mutable tag.",
      factors: [factor()],
    },
  });

  mockApi([
    [
      "/plans/container/container-a",
      {
        containerId: "container-a",
        current,
        history: [current, earlier],
        pagination: pagination(2),
      },
    ],
  ]);
  renderContainerPlan();

  expect(await screen.findByLabelText("Current assessment")).toBeInTheDocument();

  // The earlier assessment is kept, and marked as no longer standing.
  const timeline = screen.getByLabelText("Earlier assessments");
  expect(
    within(timeline).getByText(/the container tracks a mutable tag/),
  ).toBeInTheDocument();
  expect(within(timeline).getByTitle(/newer assessment exists/i)).toBeInTheDocument();
});

it("states that no change is proposed rather than showing an empty page", async () => {
  mockApi([
    [
      "/plans/container/container-a",
      { containerId: "container-a", history: [], pagination: pagination(0) },
    ],
  ]);
  renderContainerPlan();

  expect(
    await screen.findByText("No change is proposed for this container"),
  ).toBeInTheDocument();
  // And it says explicitly what that does not mean.
  expect(
    screen.getByText(/not the same as a change assessed and found safe/i),
  ).toBeInTheDocument();
});

// A digest-only update keeps the reference, and saying so in words avoids
// suggesting there is something to edit.
it("explains a change that moves only the digest", async () => {
  mockApi([
    [
      "/plans/container/container-a",
      {
        containerId: "container-a",
        current: plan({
          proposedImage: "nginx:1.27.0",
          updateType: "digest",
        }),
        history: [],
        pagination: pagination(0),
      },
    ],
  ]);
  renderContainerPlan();

  expect(
    await screen.findByText(/The reference does not change/),
  ).toBeInTheDocument();
});

// The evidence a plan rested on links to the feature that owns each record, so
// an operator can check the input rather than take the plan's word for it.
it("links to the evidence behind an assessment", async () => {
  mockApi([
    [
      "/plans/container/container-a",
      {
        containerId: "container-a",
        current: plan({
          driftOpen: 2,
          driftMaxSeverity: "high",
          policyOpen: 1,
          policyMaxSeverity: "critical",
        }),
        history: [],
        pagination: pagination(0),
      },
    ],
  ]);
  renderContainerPlan();

  const evidence = await screen.findByLabelText("What is being compared");
  expect(within(evidence).getByRole("link", { name: /2 unresolved/ })).toHaveAttribute(
    "href",
    "/drift/container/container-a",
  );
  expect(within(evidence).getByRole("link", { name: /1 open/ })).toHaveAttribute(
    "href",
    "/policy/container/container-a",
  );
  expect(within(evidence).getByRole("link", { name: "#7" })).toHaveAttribute(
    "href",
    "/snapshots/7",
  );
});

// ------------------------------------------------------------- filtering --

it("sends only closed-vocabulary filters to the server", async () => {
  mockApi(listRoutes([plan()]));
  renderPlans();

  await screen.findByText("web");

  await userEvent.selectOptions(screen.getByLabelText("Risk"), "critical");
  await waitFor(() => {
    expect(requests.some((url) => url.includes("band=critical"))).toBe(true);
  });

  await userEvent.selectOptions(
    screen.getByLabelText("Recommendation"),
    "notRecommended",
  );
  await waitFor(() => {
    expect(
      requests.some((url) => url.includes("recommendation=notRecommended")),
    ).toBe(true);
  });
});

// Text on a plan originates in HarborMaster's own vocabulary, but the container
// NAME comes from Docker. React escapes it; this pins that it is never treated
// as markup.
it("renders a hostile container name as text", async () => {
  mockApi(listRoutes([plan({ containerName: "<img src=x onerror=alert(1)>" })]));
  const { container } = renderPlans();

  expect(await screen.findByText("<img src=x onerror=alert(1)>")).toBeInTheDocument();
  expect(container.querySelector("img")).toBeNull();
});
