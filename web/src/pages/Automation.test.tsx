import { render, screen, waitFor, within } from "@testing-library/react";
import axe from "axe-core";
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
import { NEW_POLICY_SEMANTICS } from "../api/automationPresets";
import { UPDATE_STRATEGY_LABELS } from "../api/automationTypes";
import { Automation } from "./Automation";
import { AutomationPaused } from "./AutomationPaused";
import { AutomationRun as AutomationRunPage } from "./AutomationRun";
import { UpdatePolicies } from "./UpdatePolicies";
import { healthState } from "../test/fixtures";
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

      // Readiness: a POST that changes nothing, fired by the policy editor as
      // the form changes. Answered explicitly so the panel renders a real
      // state rather than falling through to the catch-all `{}`.
      if (url.includes("/automation/readiness")) {
        return jsonResponse({
          readiness: {
            evaluatedAt: "2026-08-06T12:00:00Z",
            truncated: false,
            considered: 1,
            governed: 1,
            eligible: 0,
            observing: 1,
            awaitingApproval: 0,
            groups: [],
          },
          engineEnabled: true,
        });
      }
      // The three extra reads the onboarding panel makes. Stubbed explicitly
      // so the panel renders a REAL state rather than falling through to the
      // catch-all and reporting "unknown" for the wrong reason.
      if (url.includes("/inventory")) {
        return jsonResponse({ enabled: true, generation: 7, counts: {}, warnings: [] });
      }
      if (url.includes("/plans")) {
        return jsonResponse({
          items: [],
          pagination: pagination(0),
          summary: {},
          planner: {
            enabled: true,
            plannerVersion: "1",
            running: false,
            pending: false,
            lastRunAt: "2026-08-06T02:00:00Z",
            lastGenerated: 0,
            lastUnchanged: 0,
            lastSkipped: 0,
          },
        });
      }
      if (url.includes("/health")) {
        return jsonResponse({
          status: "healthy",
          checkedAt: "2026-08-06T12:00:00Z",
          uptimeSeconds: 60,
          database: { status: "healthy" },
          docker: { status: "healthy" },
          features: {
            inventory: true, events: true, snapshots: true, drift: true,
            policy: true, planner: true, imageIntel: true,
            acquisition: true, execution: true, rollback: true, automation: true,
            notifications: false, notificationsAllowPrivate: false,
          },
        });
      }
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
  renderPage(<Automation health={healthState()} />);

  const notice = await screen.findByText(/switched off in this deployment/i);
  expect(notice).toBeInTheDocument();
  expect(notice.textContent).toMatch(/nothing will act on them/i);
});

it("says what automatic mode actually does when the engine is on", async () => {
  stub();
  renderPage(<Automation health={healthState()} />);

  const notice = await screen.findByText(/The update engine is on/i);
  // Not "automation is enabled". What HAPPENS.
  expect(notice.textContent).toMatch(/stop and replace matching containers without asking/i);
});

// ------------------------------------------------------------- the status --

it("says when automation may next act, from the server's own calculation", async () => {
  stub();
  renderPage(<Automation health={healthState()} />);

  await screen.findByText("Maintenance window");
  expect(screen.getByText("Closed")).toBeInTheDocument();
  // The hint is built from nextWindowOpensAt, which the SERVER computed in the
  // policy's timezone. The browser never recomputes it.
  expect(screen.getByText(/next opens/i)).toBeInTheDocument();
});

// ----------------------------------------------------------- the controls --

it("offers the dry run before the control that acts", async () => {
  stub();
  renderPage(<Automation health={healthState()} />);

  const dryRun = await screen.findByRole("button", { name: "Dry run" });
  const run = screen.getByRole("button", { name: "Run pass" });

  // The safe control comes first in the document, so it is the one the eye
  // lands on.
  expect(dryRun.compareDocumentPosition(run) & Node.DOCUMENT_POSITION_FOLLOWING)
    .toBeTruthy();
});

it("sends only a dryRun flag when a pass is run", async () => {
  stub();
  renderPage(<Automation health={healthState()} />);

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
  renderPage(<Automation health={healthState()} />, "viewer");

  await screen.findByText("Maintenance window");
  expect(screen.queryByRole("button", { name: "Dry run" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Run pass" })).not.toBeInTheDocument();
});

it("refuses to run a pass while one is running", async () => {
  stub({ status: sampleStatus({ running: true }) });
  renderPage(<Automation health={healthState()} />);

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
  renderPage(<Automation health={healthState()} />);

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

  await userEvent.click(
    await screen.findByRole("button", { name: "Resume automatic updates" }),
  );

  // The confirmation has to say what resume is NOT, because "resume" reads as
  // "retry" and that is the misreading with consequences.
  expect(await screen.findByText("Resume automatic updates?")).toBeInTheDocument();
  expect(
    screen.getByText(/does not retry the failed update or change the container now/i),
  ).toBeInTheDocument();
  expect(
    screen.getByText(/evaluate it again using current snapshots/i),
  ).toBeInTheDocument();
  // And the consequence for the count the card above reports.
  expect(screen.getByText(/failure count is reset to zero/i)).toBeInTheDocument();

  // Never framed as a retry.
  expect(document.body.textContent).not.toMatch(/force update|run update/i);

  await userEvent.click(
    screen.getByRole("button", { name: "Resume automatic updates" }),
  );

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
  expect(screen.getAllByText("Observe only").length).toBeGreaterThan(0);
  expect(screen.getAllByText("Automatic").length).toBeGreaterThan(0);
  expect(
    screen.getByTitle(/stops and replaces containers without asking/i),
  ).toBeInTheDocument();
});

it("says plainly when a policy governs nothing", async () => {
  stub({ policies: [samplePolicy({ selector: {} })] });
  renderPage(<UpdatePolicies />);

  await screen.findByText("Nightly patches");
  // The summary and the Scope row both say it. An empty selector governs
  // nothing, and a card that left that to be inferred from a blank field would
  // read as "everything" to somebody scanning.
  expect(screen.getByText(/will observe no containers/i)).toBeInTheDocument();
  expect(screen.getByText("No containers")).toBeInTheDocument();
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

  // The setting that makes a new policy safe is the MODE. `observe` is the one
  // value of the four that cannot change a host -- `Mutates()` is true for
  // `automatic` alone -- so nothing this form arrives at by itself can stop,
  // replace, or pull anything.
  //
  // Scoped to the mode group. The preset group above it also offers an option
  // headed "Observe only" -- the preset that compiles to this very mode -- and
  // the two are told apart by their fieldset rather than by their heading.
  const modes = within(
    screen.getByText("How should updates happen?").closest("fieldset") as HTMLElement,
  );
  expect(modes.getByRole("radio", { name: /Observe only/ })).toBeChecked();

  // The ceiling is whatever the opening preset compiles, and it is asserted
  // through that preset rather than as a literal.
  //
  // It used to be pinned to "same tag only" independently, which is how the
  // form came to show "Observe only" while holding a ceiling that preset does
  // not write. Under `observe` the ceiling decides what is REPORTED, not what
  // may happen, and reporting under the ceiling that "Keep containers safely
  // updated" would apply is what makes the observe policy a preview of it.
  expect(NEW_POLICY_SEMANTICS.mode).toBe("observe");
  expect(
    screen.getByRole("radio", {
      name: new RegExp(
        "^" + UPDATE_STRATEGY_LABELS[NEW_POLICY_SEMANTICS.strategy],
      ),
    }),
  ).toBeChecked();

  // And the breadth defaults to a choice that governs nothing until the
  // operator picks a container. "All eligible containers" must never be what a
  // form arrives at by itself.
  expect(
    screen.getByRole("radio", { name: /All eligible containers/ }),
  ).not.toBeChecked();
  expect(screen.getByRole("radio", { name: /Selected containers/ })).toBeChecked();
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

// HarborMaster refuses to update itself, and the page says so.
//
// The alternative is a silent absence: an operator whose HarborMaster container
// never appears in a plan cannot tell "refused on purpose" from "not noticed".
it("says which container HarborMaster will not update", async () => {
  stub({
    status: sampleStatus({
      self: {
        containerId: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        containerName: "harbormaster",
        source: "runtime",
      },
    }),
  });

  render(
    <TestSessionProvider session={testSession({ user: testUser("administrator") })}>
      <MemoryRouter initialEntries={["/automation"]}>
        <Automation health={healthState()} />
      </MemoryRouter>
    </TestSessionProvider>,
  );

  expect(
    await screen.findByText(/HarborMaster will not update itself/i),
  ).toBeInTheDocument();
  expect(screen.getByText("harbormaster")).toBeInTheDocument();
  expect(
    screen.getByText(/docker compose pull && docker compose up -d/),
  ).toBeInTheDocument();
});

// Running outside a container excludes nothing, so there is nothing to explain.
it("says nothing about self-update when there is no identity", async () => {
  stub({ status: sampleStatus() });

  render(
    <TestSessionProvider session={testSession({ user: testUser("administrator") })}>
      <MemoryRouter initialEntries={["/automation"]}>
        <Automation health={healthState()} />
      </MemoryRouter>
    </TestSessionProvider>,
  );

  await screen.findAllByText(/update engine/i);
  expect(screen.queryByText(/will not update itself/i)).toBeNull();
});

// ------------------------------------------------------- pause accessibility --

it("lets a keyboard user answer the resume confirmation either way", async () => {
  stub({
    pauses: [
      {
        containerName: "web",
        reason: "automaticRollback",
        failures: 2,
        detail: "the replacement failed verification and the previous container was restored",
        pausedAt: "2026-08-06T02:05:00Z",
      },
    ],
  });
  renderPage(<AutomationPaused />);

  await userEvent.click(
    await screen.findByRole("button", { name: "Resume automatic updates" }),
  );

  // The confirmation is a labelled group, so a screen reader announces what is
  // being confirmed rather than reading two unattached buttons.
  const group = screen.getByRole("group", {
    name: /Resume automatic updates for web\?/i,
  });
  expect(group).toBeInTheDocument();

  // Focus lands on the action the operator asked for.
  const confirm = within(group).getByRole("button", {
    name: "Resume automatic updates",
  });
  expect(confirm).toHaveFocus();

  // Escape answers "no". A control that asks a question must be answerable
  // without reaching for the mouse.
  await userEvent.keyboard("{Escape}");
  expect(
    screen.queryByRole("group", { name: /Resume automatic updates for web\?/i }),
  ).not.toBeInTheDocument();

  // Nothing was sent by opening and dismissing the confirmation.
  expect(requests.find((r) => r.url.includes("/automation/resume"))).toBeUndefined();
});

it("explains a pause without relying on colour", async () => {
  stub({
    pauses: [
      {
        containerName: "hm13-failguard",
        reason: "automaticRollback",
        failures: 2,
        detail: "the replacement failed verification and the previous container was restored",
        pausedAt: "2026-08-06T02:05:00Z",
      },
    ],
  });
  renderPage(<AutomationPaused />);

  await screen.findByText("hm13-failguard");

  // The reason, the count and the recorded detail are all TEXT. An operator who
  // cannot distinguish the badge's colour still gets the whole answer.
  expect(screen.getByText("Rolled back")).toBeInTheDocument();
  expect(screen.getByText("2")).toBeInTheDocument();
  expect(
    screen.getByText(/replacement failed verification/i),
  ).toBeInTheDocument();
});

it("has no serious or critical axe findings on the paused view", async () => {
  stub({
    pauses: [
      {
        containerName: "web",
        reason: "repeatedFailure",
        failures: 3,
        pausedAt: "2026-08-06T02:05:00Z",
      },
    ],
  });
  renderPage(<AutomationPaused />);
  await screen.findByText("web");

  await userEvent.click(
    screen.getByRole("button", { name: "Resume automatic updates" }),
  );

  const results = await axe.run(document.body, {
    resultTypes: ["violations"],
    runOnly: { type: "tag", values: ["wcag2a", "wcag2aa"] },
  });
  const serious = results.violations.filter(
    (violation) => violation.impact === "serious" || violation.impact === "critical",
  );
  expect(serious.map((violation) => `${violation.id}: ${violation.help}`)).toEqual([]);
});

// --------------------------------------------------- onboarding side effects --

// TestOpeningTheAutomationPageChangesNothing is Stage 17.8b §18.
//
// Onboarding reads. An operator opening a page to find out what HarborMaster is
// doing must not, by opening it, cause HarborMaster to do something -- and the
// most dangerous version of that would be a page that "helpfully" generated
// plans, created a policy, or resumed a pause on load.
it("changes nothing when the automation page is opened", async () => {
  stub({ status: sampleStatus({ enabled: true, policies: 0 }) });
  renderPage(<Automation health={healthState()} />);

  await screen.findByTestId("automation-onboarding");

  // Every request the page made was a READ.
  const writes = requests.filter((r) => r.method !== "GET" && r.method !== "HEAD");
  expect(writes.map((r) => `${r.method} ${r.url}`)).toEqual([]);

  // And specifically none of the things that would change the estate.
  for (const path of [
    "/plans/generate",
    "/update-policies",
    "/automation/run",
    "/automation/approve",
    "/automation/resume",
    "/automation/pause",
    "/acquisitions",
    "/executions",
    "/plan-approvals",
  ]) {
    expect(
      requests.some((r) => r.url.includes(path) && r.method !== "GET"),
    ).toBe(false);
  }
});

// The request budget, asserted rather than assumed.
it("reads each endpoint once and never per container", async () => {
  stub({ status: sampleStatus({ enabled: true }) });
  renderPage(<Automation health={healthState()} />);

  await screen.findByTestId("automation-onboarding");

  const counts = new Map<string, number>();
  for (const request of requests) {
    // Group by path, ignoring query strings.
    const path = request.url.split("?")[0]!;
    counts.set(path, (counts.get(path) ?? 0) + 1);
  }

  for (const [path, count] of counts) {
    expect(
      count,
      `${path} was requested ${count} times; each endpoint is read once per cycle`,
    ).toBeLessThanOrEqual(1);
  }

  // No per-container reads: nothing addresses a single container by id.
  expect(requests.some((r) => /\/containers\/[^/?]+/.test(r.url))).toBe(false);
});
