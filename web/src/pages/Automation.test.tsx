import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import type {
  AutomationDecision,
  AutomationRun,
  AutomationStatus,
  PausedContainer,
  UpdatePolicy,
} from "../api/automationTypes";
import { Automation } from "./Automation";
import { AutomationPaused } from "./AutomationPaused";
import { AutomationRun as AutomationRunPage } from "./AutomationRun";
import { UpdatePolicies } from "./UpdatePolicies";
import { TestSessionProvider, testSession, testUser } from "../test/session";

/**
 * Automation UI tests.
 *
 * Automation is the only thing in HarborMaster that changes the host with
 * nobody watching, so the properties under test are all about what an operator
 * is TOLD, and what they cannot do by accident:
 *
 *   - A page whose engine is off says so, in as many words, before any control.
 *   - A page whose engine is on says what "automatic" actually means.
 *   - The dry-run control is offered before the one that acts.
 *   - Approving a held decision takes two presses and states that the container
 *     will be stopped.
 *   - A viewer sees the record and is offered no control at all.
 *   - The reason a container was skipped is rendered from the closed
 *     vocabulary, so "why not" is answerable from the page.
 *   - Daemon-adjacent text is rendered as text, never as markup.
 */

const originalFetch = globalThis.fetch;

let requests: { url: string; method: string; body: string }[] = [];

const runID = "run_00112233445566778899";
const policyID = "upd_00112233445566778899";

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

function sampleStatus(overrides: Partial<AutomationStatus> = {}): AutomationStatus {
  return {
    enabled: true,
    running: false,
    policies: 2,
    enabledPolicies: 1,
    pausedContainers: 0,
    awaitingApproval: 0,
    windowOpen: false,
    nextWindowOpensAt: "2026-08-07T02:00:00Z",
    lastRunAt: "2026-08-06T02:00:00Z",
    nextRunAt: "2026-08-06T02:15:00Z",
    ...overrides,
  };
}

function sampleRun(overrides: Partial<AutomationRun> = {}): AutomationRun {
  return {
    runId: runID,
    trigger: "schedule",
    state: "completed",
    considered: 3,
    eligible: 1,
    submitted: 1,
    skipped: 2,
    failed: 0,
    startedAt: "2026-08-06T02:00:00Z",
    completedAt: "2026-08-06T02:00:04Z",
    ...overrides,
  };
}

function sampleDecision(overrides: Partial<AutomationDecision> = {}): AutomationDecision {
  return {
    runId: runID,
    containerName: "web",
    verdict: "update",
    reason: "eligible",
    detail: "every check passed",
    currentImage: "nginx:1.27.3",
    proposedImage: "nginx:1.27.4",
    position: 0,
    decidedAt: "2026-08-06T02:00:01Z",
    ...overrides,
  };
}

function samplePolicy(overrides: Partial<UpdatePolicy> = {}): UpdatePolicy {
  return {
    policyId: policyID,
    name: "Nightly patches",
    enabled: true,
    priority: 10,
    selector: { include: ["web"] },
    strategy: "patch",
    minimumRecommendation: "proceed",
    mode: "observe",
    window: { alwaysOpen: false, timezone: "UTC", start: "02:00", end: "04:00" },
    failure: { autoRollback: true, pauseAfterFailures: 2 },
    ...overrides,
  };
}

interface StubOptions {
  status?: AutomationStatus;
  runs?: AutomationRun[];
  decisions?: AutomationDecision[];
  upcoming?: AutomationDecision[];
  pauses?: PausedContainer[];
  policies?: UpdatePolicy[];
}

function stub(options: StubOptions = {}) {
  requests = [];

  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      requests.push({
        url,
        method: init?.method ?? "GET",
        body: typeof init?.body === "string" ? init.body : "",
      });

      if (url.includes("/automation/upcoming")) {
        const items = options.upcoming ?? [];
        return jsonResponse({
          items,
          eligible: items.filter((d) => d.verdict !== "skip").length,
        });
      }
      if (url.includes("/automation/runs/")) {
        return jsonResponse({
          run: options.runs?.[0] ?? sampleRun(),
          decisions: options.decisions ?? [],
          pagination: pagination(options.decisions?.length ?? 0),
        });
      }
      if (url.includes("/automation/runs")) {
        const items = options.runs ?? [];
        return jsonResponse({
          items,
          pagination: pagination(items.length),
          summary: { total: items.length },
        });
      }
      if (url.includes("/automation/paused")) {
        const items = options.pauses ?? [];
        return jsonResponse({ items, pagination: pagination(items.length) });
      }
      if (url.includes("/automation/run")) {
        return jsonResponse({
          run: sampleRun({ trigger: "dryRun", dryRun: true, submitted: 0 }),
          decisions: [],
          pagination: pagination(0),
        });
      }
      if (url.includes("/automation/approve")) {
        return jsonResponse(sampleDecision({ acquisitionId: "acq_1" }), 202);
      }
      if (url.includes("/automation/resume")) {
        return new Response(null, { status: 204 });
      }
      if (url.includes("/automation")) {
        return jsonResponse({
          status: options.status ?? sampleStatus(),
          history: { total: 1 },
        });
      }
      if (url.includes("/update-policies")) {
        const items = options.policies ?? [];
        return jsonResponse({ items, pagination: pagination(items.length) });
      }
      return jsonResponse({});
    }),
  );
}

function renderPage(node: React.ReactNode, role: "viewer" | "operator" | "administrator" = "administrator") {
  return render(
    <TestSessionProvider session={testSession({ user: testUser(role) })}>
      <MemoryRouter>{node}</MemoryRouter>
    </TestSessionProvider>,
  );
}

beforeEach(() => {
  requests = [];
});

afterEach(() => {
  vi.unstubAllGlobals();
  globalThis.fetch = originalFetch;
});

// ------------------------------------------------------------- the notice --

it("says the engine is off before offering anything", async () => {
  stub({ status: sampleStatus({ enabled: false }) });
  renderPage(<Automation />);

  const notice = await screen.findByText(/switched off in this deployment/i);
  expect(notice).toBeInTheDocument();
  expect(notice.textContent).toMatch(/nothing will act on them/i);
});

it("says what automatic mode actually does when the engine is on", async () => {
  stub();
  renderPage(<Automation />);

  const notice = await screen.findByText(/The update engine is on/i);
  // Not "automation is enabled". What HAPPENS.
  expect(notice.textContent).toMatch(/stop and replace matching containers without asking/i);
});

// ------------------------------------------------------------- the status --

it("says when automation may next act, from the server's own calculation", async () => {
  stub();
  renderPage(<Automation />);

  await screen.findByText("Maintenance window");
  expect(screen.getByText("Closed")).toBeInTheDocument();
  // The hint is built from nextWindowOpensAt, which the SERVER computed in the
  // policy's timezone. The browser never recomputes it.
  expect(screen.getByText(/next opens/i)).toBeInTheDocument();
});

// ----------------------------------------------------------- the controls --

it("offers the dry run before the control that acts", async () => {
  stub();
  renderPage(<Automation />);

  const dryRun = await screen.findByRole("button", { name: "Dry run" });
  const run = screen.getByRole("button", { name: "Run pass" });

  // The safe control comes first in the document, so it is the one the eye
  // lands on.
  expect(dryRun.compareDocumentPosition(run) & Node.DOCUMENT_POSITION_FOLLOWING)
    .toBeTruthy();
});

it("sends only a dryRun flag when a pass is run", async () => {
  stub();
  renderPage(<Automation />);

  await userEvent.click(await screen.findByRole("button", { name: "Dry run" }));

  await waitFor(() => {
    const post = requests.find((r) => r.method === "POST" && r.url.includes("/automation/run"));
    expect(post).toBeTruthy();
    // The whole body. There is nothing else a caller could send.
    expect(JSON.parse(post!.body)).toEqual({ dryRun: true });
  });
});

it("offers no pass control to a viewer", async () => {
  stub();
  renderPage(<Automation />, "viewer");

  await screen.findByText("Maintenance window");
  expect(screen.queryByRole("button", { name: "Dry run" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Run pass" })).not.toBeInTheDocument();
});

it("refuses to run a pass while one is running", async () => {
  stub({ status: sampleStatus({ running: true }) });
  renderPage(<Automation />);

  const run = await screen.findByRole("button", { name: "Run pass" });
  expect(run).toBeDisabled();
  expect(screen.getByText(/already running/i)).toBeInTheDocument();
});

// ------------------------------------------------------------ the preview --

it("renders why each container would be skipped", async () => {
  stub({
    upcoming: [
      sampleDecision({
        containerName: "cache",
        verdict: "skip",
        reason: "windowClosed",
        detail: "the maintenance window is 02:00-04:00 UTC, every day",
        position: 1,
      }),
    ],
  });
  renderPage(<Automation />);

  // Hidden by default: the preview leads with what WOULD change.
  await screen.findByText(/Nothing would be updated/i);

  await userEvent.click(screen.getByLabelText(/Show skipped containers/i));

  expect(await screen.findByText("cache")).toBeInTheDocument();
  // From the closed vocabulary, not from server prose.
  expect(screen.getByText("Outside the maintenance window")).toBeInTheDocument();
});

// ----------------------------------------------------------- the approval --

it("takes two presses to approve, and says the container will be stopped", async () => {
  stub({
    runs: [sampleRun()],
    decisions: [
      sampleDecision({
        verdict: "awaitingApproval",
        reason: "approvalRequired",
      }),
    ],
  });

  render(
    <TestSessionProvider session={testSession({ user: testUser("administrator") })}>
      <MemoryRouter initialEntries={[`/automation/runs/${runID}`]}>
        <Routes>
          <Route path="/automation/runs/:id" element={<AutomationRunPage />} />
        </Routes>
      </MemoryRouter>
    </TestSessionProvider>,
  );

  const approve = await screen.findByRole("button", { name: "Approve" });
  await userEvent.click(approve);

  // Nothing has been sent yet.
  expect(requests.some((r) => r.url.includes("/automation/approve"))).toBe(false);

  const confirmation = await screen.findByText(/stops/i);
  expect(confirmation.textContent).toMatch(/starts a replacement/i);

  await userEvent.click(screen.getByRole("button", { name: /Yes, update it/i }));

  await waitFor(() => {
    const post = requests.find((r) => r.url.includes("/automation/approve"));
    expect(post).toBeTruthy();
    // The two fields SELECT a held decision. No image, no digest, no plan.
    expect(JSON.parse(post!.body)).toEqual({
      runId: runID,
      containerName: "web",
    });
  });
});

it("offers no approval to a viewer", async () => {
  stub({
    runs: [sampleRun()],
    decisions: [sampleDecision({ verdict: "awaitingApproval", reason: "approvalRequired" })],
  });

  render(
    <TestSessionProvider session={testSession({ user: testUser("viewer") })}>
      <MemoryRouter initialEntries={[`/automation/runs/${runID}`]}>
        <Routes>
          <Route path="/automation/runs/:id" element={<AutomationRunPage />} />
        </Routes>
      </MemoryRouter>
    </TestSessionProvider>,
  );

  await screen.findByText("web");
  expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
});

it("renders decisions in the pass's own order", async () => {
  stub({
    runs: [sampleRun()],
    decisions: [
      sampleDecision({ containerName: "zulu", position: 0 }),
      sampleDecision({ containerName: "alpha", position: 1 }),
    ],
  });

  render(
    <TestSessionProvider session={testSession()}>
      <MemoryRouter initialEntries={[`/automation/runs/${runID}`]}>
        <Routes>
          <Route path="/automation/runs/:id" element={<AutomationRunPage />} />
        </Routes>
      </MemoryRouter>
    </TestSessionProvider>,
  );

  await screen.findByText("zulu");
  const rows = screen.getAllByRole("row").slice(1);
  // Execution order, not alphabetical: that is what makes a dry run readable.
  expect(rows).toHaveLength(2);
  expect(within(rows[0] as HTMLElement).getByText("zulu")).toBeInTheDocument();
  expect(within(rows[1] as HTMLElement).getByText("alpha")).toBeInTheDocument();
});

// -------------------------------------------------------------- the pauses --

it("says a rollback pause will not clear itself", async () => {
  stub({
    pauses: [
      {
        containerName: "web",
        reason: "automaticRollback",
        detail: "the recreation failed (unhealthy) and was rolled back automatically",
        failures: 1,
        pausedAt: "2026-08-06T02:05:00Z",
      },
    ],
  });
  renderPage(<AutomationPaused />);

  await screen.findByText("web");
  expect(screen.getByText("Rolled back")).toBeInTheDocument();
  expect(screen.getByText("only when a person clears it")).toBeInTheDocument();
});

it("warns that resuming resets the failure count", async () => {
  stub({
    pauses: [
      {
        containerName: "web",
        reason: "repeatedFailure",
        failures: 2,
        pausedAt: "2026-08-06T02:05:00Z",
      },
    ],
  });
  renderPage(<AutomationPaused />);

  await userEvent.click(await screen.findByRole("button", { name: "Resume" }));

  const warning = await screen.findByText(/resets/i);
  expect(warning.textContent).toMatch(/failure count to zero/i);

  await userEvent.click(screen.getByRole("button", { name: /Yes, resume automation/i }));

  await waitFor(() => {
    const post = requests.find((r) => r.url.includes("/automation/resume"));
    expect(post).toBeTruthy();
    expect(JSON.parse(post!.body)).toEqual({ containerName: "web" });
  });
});

// ------------------------------------------------------------- the policies --

it("marks an automatic policy differently from one that only watches", async () => {
  stub({
    policies: [
      samplePolicy({ mode: "observe" }),
      samplePolicy({
        policyId: "upd_11111111111111111111",
        name: "Unattended",
        mode: "automatic",
      }),
    ],
  });
  renderPage(<UpdatePolicies />);

  await screen.findByText("Nightly patches");

  // Both labels are present as text, so the distinction is not colour-only.
  expect(screen.getAllByText("Observe").length).toBeGreaterThan(0);
  expect(screen.getAllByText("Automatic").length).toBeGreaterThan(0);
  expect(
    screen.getByTitle(/stops and replaces containers without asking/i),
  ).toBeInTheDocument();
});

it("says plainly when a policy governs nothing", async () => {
  stub({ policies: [samplePolicy({ selector: {} })] });
  renderPage(<UpdatePolicies />);

  await screen.findByText("Nightly patches");
  expect(screen.getByText(/nothing \(the selector is empty\)/i)).toBeInTheDocument();
});

it("offers no editor to an operator", async () => {
  // Writing a policy is a standing grant of the operator's most dangerous
  // permission, so it is an administrator's.
  stub({ policies: [samplePolicy()] });
  renderPage(<UpdatePolicies />, "operator");

  await screen.findByText("Nightly patches");
  expect(screen.queryByRole("button", { name: "New policy" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
});

it("defaults a new policy to the safe settings", async () => {
  stub({ policies: [] });
  renderPage(<UpdatePolicies />);

  await userEvent.click(await screen.findByRole("button", { name: "New policy" }));

  // Observe and digest-only: the two settings that change the least.
  const mode = screen.getByLabelText(/Mode/i) as HTMLSelectElement;
  const ceiling = screen.getByLabelText(/Ceiling/i) as HTMLSelectElement;
  expect(mode.value).toBe("observe");
  expect(ceiling.value).toBe("digestOnly");
});

it("renders a policy name as text rather than markup", async () => {
  // A policy name is administrator-typed and reaches a page other people read.
  stub({
    policies: [samplePolicy({ name: "<img src=x onerror=alert(1)>" })],
  });
  const { container } = renderPage(<UpdatePolicies />);

  await screen.findByText("<img src=x onerror=alert(1)>");
  expect(container.querySelector("img")).toBeNull();
});
