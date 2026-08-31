import { TestSessionProvider } from "../test/session";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import fs from "node:fs";
import path from "node:path";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import type { AutomationDecision } from "../api/automationTypes";
import { SessionProvider } from "../hooks/useSession";
import { containerDetail, sessionResponse } from "../test/fixtures";
import { ContainerDetailPage } from "./ContainerDetail";
import { PendingApprovals } from "./PendingApprovals";

/**
 * The approval queue.
 *
 * An approval-required policy decides on an update and holds it. Before this
 * page existed the only trace of that was a row inside an archived automation
 * pass -- among a row for every other container the pass considered and
 * skipped -- and the dashboard's "waiting for approval" count linked to the
 * automation landing page, which does not list them.
 *
 * The properties under test are about whether an operator can FIND the thing
 * that is waiting for them, and whether they are told the truth about what
 * approving does:
 *
 *   - The queue lists what is held, names the container and the change, and
 *     offers the same approve control the pass detail offers.
 *   - Nothing waiting reads as "nothing waiting", not as an error and not as
 *     an empty area.
 *   - The container's own page says a decision is waiting for it, and asks the
 *     server about THAT container rather than scanning a page of the queue.
 *   - A deployment without automation, or a viewer without the permission to
 *     read it, gets no error panel bolted to every container page -- a lookup
 *     that could not be performed establishes nothing either way.
 *   - The copy never claims approval skips a check.
 */

const originalFetch = globalThis.fetch;

let requests: string[] = [];
let writes: { url: string; method: string; body: string }[] = [];

const runID = "run_00112233445566778899";
const containerID = "abcdef0123456789";

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

function decision(overrides: Partial<AutomationDecision> = {}): AutomationDecision {
  return {
    runId: runID,
    containerId: containerID,
    containerName: "web",
    verdict: "awaitingApproval",
    reason: "approvalRequired",
    policyName: "Nightly patches",
    currentImage: "nginx:1.27.0",
    proposedImage: "nginx:1.27.1",
    updateType: "patch",
    recommendation: "safe",
    decidedAt: "2026-08-06T02:00:01Z",
    ...overrides,
  } as AutomationDecision;
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
    // Identity first: the approve control asks who it is talking to before it
    // decides whether to render itself at all.
    if (url.includes("/auth/session")) return jsonResponse(sessionResponse());
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

function renderQueue() {
  return render(
    <MemoryRouter initialEntries={["/automation/approvals"]}>
      <SessionProvider>
        <Routes>
          <Route path="/automation/approvals" element={<PendingApprovals />} />
        </Routes>
      </SessionProvider>
    </MemoryRouter>,
  );
}

function renderContainer() {
  return render(
    <TestSessionProvider>
      <MemoryRouter initialEntries={[`/containers/${containerID}`]}>
      <Routes>
        <Route path="/containers/:id" element={<ContainerDetailPage />} />
      </Routes>
    </MemoryRouter>
    </TestSessionProvider>,
  );
}

// ------------------------------------------------------------ the queue --

it("lists what is being held, with the container and the change", async () => {
  mockApi([["/automation/approvals", { items: [decision()], pagination: pagination(1) }]]);

  renderQueue();

  const row = await screen.findByRole("row", { name: /web/ });
  expect(within(row).getByText("nginx:1.27.0")).toBeInTheDocument();
  expect(within(row).getByText("nginx:1.27.1")).toBeInTheDocument();
  expect(within(row).getByText(/Nightly patches/)).toBeInTheDocument();
});

it("offers the same approve control the pass detail offers", async () => {
  mockApi([["/automation/approvals", { items: [decision()], pagination: pagination(1) }]]);

  renderQueue();

  const row = await screen.findByRole("row", { name: /web/ });
  expect(within(row).getByRole("button", { name: /approve/i })).toBeInTheDocument();
});

it("does not approve in one click", async () => {
  // The same two-step confirmation the pass detail has. Reusing that control
  // rather than writing a second one is what keeps this true.
  mockApi([["/automation/approvals", { items: [decision()], pagination: pagination(1) }]]);

  renderQueue();

  const row = await screen.findByRole("row", { name: /web/ });
  await userEvent.click(within(row).getByRole("button", { name: /approve/i }));

  expect(writes).toHaveLength(0);
});

it("says nothing is waiting rather than showing an empty area", async () => {
  mockApi([["/automation/approvals", { items: [], pagination: pagination(0) }]]);

  renderQueue();

  expect(await screen.findByText(/nothing is waiting/i)).toBeInTheDocument();
});

it("never claims approving skips a check", async () => {
  mockApi([["/automation/approvals", { items: [decision()], pagination: pagination(1) }]]);

  const { container } = renderQueue();
  await screen.findByRole("row", { name: /web/ });

  const text = container.textContent ?? "";
  for (const claim of [
    /skips? the checks/i,
    /without (further |any )?checks/i,
    /bypass/i,
    /immediately applies/i,
  ]) {
    expect(text).not.toMatch(claim);
  }
  // And states the opposite, in as many words.
  expect(text).toMatch(/does not skip/i);
});

// --------------------------------------------------------- large estates --

it("stays a list at two, and names each container", async () => {
  mockApi([
    [
      "/automation/approvals",
      {
        items: [
          decision(),
          decision({ containerName: "database", containerId: "b".repeat(16) }),
        ],
        pagination: pagination(2),
      },
    ],
  ]);

  renderQueue();

  expect(await screen.findByRole("row", { name: /web/ })).toBeInTheDocument();
  expect(screen.getByRole("row", { name: /database/ })).toBeInTheDocument();
  // One approve control per row, never one for the page.
  expect(screen.getAllByRole("button", { name: /approve/i })).toHaveLength(2);
});

it("pages a queue too long for one screen rather than rendering all of it", async () => {
  // A host with hundreds of containers under an approval-required policy can
  // produce a queue longer than anyone will read. The server bounds the page;
  // the UI must show that there is more rather than implying this is all.
  const items = Array.from({ length: 25 }, (_, index) =>
    decision({
      containerName: `svc-${index}`,
      containerId: String(index).padStart(16, "0"),
    }),
  );
  mockApi([
    [
      "/automation/approvals",
      {
        items,
        pagination: {
          page: 1,
          pageSize: 25,
          totalItems: 60,
          totalPages: 3,
          hasNext: true,
          hasPrevious: false,
        },
      },
    ],
  ]);

  renderQueue();

  await screen.findByRole("row", { name: /svc-0/ });
  expect(screen.getAllByRole("button", { name: /approve/i })).toHaveLength(25);
  // The page says where it is, and offers the rest -- an unqualified list of
  // 25 would read as the whole queue.
  expect(screen.getAllByText(/page 1 of 3/i).length).toBeGreaterThan(0);
  expect(screen.getByRole("button", { name: /next/i })).toBeEnabled();
});

it("offers no approve-everything control", async () => {
  // Approving is per decision, deliberately. A control that released a page of
  // held updates in one click would be a new capability, not a convenience.
  const items = Array.from({ length: 25 }, (_, index) =>
    decision({
      containerName: `svc-${index}`,
      containerId: String(index).padStart(16, "0"),
    }),
  );
  mockApi([
    ["/automation/approvals", { items, pagination: pagination(25) }],
  ]);

  const { container } = renderQueue();
  await screen.findByRole("row", { name: /svc-0/ });

  for (const bulk of [/approve all/i, /approve every/i, /select all/i]) {
    expect(container.textContent ?? "").not.toMatch(bulk);
  }
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
});

// ------------------------------------------------- container discoverability --

it("tells the container's own page that a decision is waiting for it", async () => {
  mockApi([
    ["/automation/approvals", { items: [decision()], pagination: pagination(1) }],
    [`/containers/${containerID}`, containerDetail()],
  ]);

  renderContainer();

  // A live region, so a screen reader is told about it rather than having to
  // find it, and it carries the change it is asking about.
  const heading = await screen.findByText(/waiting for your approval/i);
  const notice = heading.closest("section");
  if (!notice) throw new Error("the notice is not a section");
  // A live region, so a screen reader is told about it rather than having to
  // find it, and it carries the change it is asking about.
  expect(notice).toHaveAttribute("role", "status");
  expect(within(notice).getByText("nginx:1.27.0")).toBeInTheDocument();
  expect(within(notice).getByText("nginx:1.27.1")).toBeInTheDocument();
  expect(
    within(notice).getByRole("link", { name: /review and approve/i }),
  ).toHaveAttribute("href", "/automation/approvals");
});

it("asks about that container rather than scanning the queue", async () => {
  mockApi([
    ["/automation/approvals", { items: [decision()], pagination: pagination(1) }],
    [`/containers/${containerID}`, containerDetail()],
  ]);

  renderContainer();
  await screen.findByText(/waiting for your approval/i);

  const approvalRequest = requests.find((url) => url.includes("/automation/approvals"));
  expect(approvalRequest).toBeDefined();
  expect(approvalRequest).toContain("container=web");
});

it("says nothing on a container with nothing waiting", async () => {
  mockApi([
    ["/automation/approvals", { items: [], pagination: pagination(0) }],
    [`/containers/${containerID}`, containerDetail()],
  ]);

  renderContainer();
  await screen.findByRole("tablist", { name: /container sections/i });

  expect(screen.queryByText(/waiting for your approval/i)).not.toBeInTheDocument();
});

it("stays silent when the lookup could not be performed", async () => {
  // Automation is optional and reading it needs a permission. A viewer on a
  // deployment without the engine must not get an error panel welded to every
  // container page for a subsystem they cannot use -- and a failed lookup is
  // not evidence that something IS waiting either.
  for (const [status, code] of [
    [503, "disabled"],
    [403, "forbidden"],
  ] as [number, string][]) {
    mockApi([
      [
        "/automation/approvals",
        { error: { code, message: "not available" } },
        status,
      ],
      [`/containers/${containerID}`, containerDetail()],
    ]);

    const view = renderContainer();
    await screen.findByRole("tablist", { name: /container sections/i });

    await waitFor(() => {
      expect(
        requests.some((url) => url.includes("/automation/approvals")),
      ).toBe(true);
    });
    expect(screen.queryByText(/waiting for your approval/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    view.unmount();
    requests = [];
  }
});

it("does not offer an approve control on the container page", async () => {
  // Every panel on the container detail is read-only, and approving is a
  // mutation. The page states the fact and links to where it is done.
  mockApi([
    ["/automation/approvals", { items: [decision()], pagination: pagination(1) }],
    [`/containers/${containerID}`, containerDetail()],
  ]);

  renderContainer();
  await screen.findByText(/waiting for your approval/i);

  expect(screen.queryByRole("button", { name: /approve/i })).not.toBeInTheDocument();
  expect(writes).toHaveLength(0);
});

// ------------------------------------------------------------- layout --

it("keeps its screen-reader-only header text inside the scrolling table", () => {
  // Measured in a real browser, not inferred here: `sr-only` is absolutely
  // positioned, so a cell that does not establish a containing block lets it
  // escape the table's horizontal scroll container and widen the PAGE by its
  // own offset. The approvals table added 376px of body scroll at 390px wide
  // while every other route in the app measured zero.
  //
  // jsdom computes no layout, so this pins the CAUSE rather than the symptom:
  // an sr-only span inside a header cell needs `relative` on that cell.
  const source = fs.readFileSync(
    path.join(__dirname, "PendingApprovals.tsx"),
    "utf8",
  );
  const headerCells = source.split("<th").slice(1);
  const offenders = headerCells
    .map((cell) => cell.slice(0, cell.indexOf("</th>")))
    .filter((cell) => cell.includes("sr-only"))
    .filter((cell) => !cell.slice(0, cell.indexOf(">")).includes("relative"));

  expect(offenders).toEqual([]);
});

/**
 * The Stage 17.9 regression, at the page where it was found.
 *
 * Stage 17.7 stopped offering an Approve button for a decision whose PLAN needs
 * manual review -- releasing one downloaded an image and the recreation was
 * then refused, so the button was an action that could not work. That guard was
 * written into the automation RUN page's markup.
 *
 * This page renders the same control and did not repeat it. So the one screen
 * whose entire job is holding decisions for a person still offered the dead
 * button, for the case that reaches it most often: every major version update
 * measures as manualReview, and the deployment-wide major rule holds it here.
 *
 * The guard now lives inside the control, which is why both pages have it.
 */
it("offers review, not approval, when the plan needs manual review", async () => {
  mockApi([
    ["/automation/approvals", {
      items: [decision({
        containerName: "hm17-bash",
        currentImage: "bash:4.4",
        proposedImage: "bash:5.3",
        updateType: "major",
        recommendation: "manualReview",
      })],
      pagination: pagination(1),
    }],
  ]);

  renderQueue();

  const row = (await screen.findByText("hm17-bash")).closest("tr");
  expect(row).not.toBeNull();

  // The action that cannot work must not be offered.
  expect(
    within(row as HTMLElement).queryByRole("button", { name: /approve/i }),
  ).not.toBeInTheDocument();

  // And the one that can is named, with somewhere to do it.
  expect(within(row as HTMLElement).getByText(/Manual review required/i)).toBeInTheDocument();
  // To the page that can actually approve it. Deep-linking to the container's
  // plan page sent the operator somewhere that shows the same verdict and has
  // no control on it.
  const link = within(row as HTMLElement).getByRole("link", { name: /Review plan/i });
  expect(link).toHaveAttribute("href", "/plans");
});

/** An ordinary held decision still gets the approve control. */
it("still offers approval for a decision a person may release", async () => {
  mockApi([
    ["/automation/approvals", {
      items: [decision({ recommendation: "proceed" })],
      pagination: pagination(1),
    }],
  ]);

  renderQueue();

  const row = (await screen.findByText("web")).closest("tr");
  expect(
    within(row as HTMLElement).getByRole("button", { name: /approve/i }),
  ).toBeInTheDocument();
});
