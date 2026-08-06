import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import type {
  Rollback,
  RollbackEligibility,
  RollbackEvent,
  RollbackSummary,
} from "../api/rollbackTypes";
import { RollbackContainerAction } from "../components/RollbackContainerAction";
import { RollbackDetail } from "./RollbackDetail";
import { Rollbacks } from "./Rollbacks";

/**
 * Manual rollback UI tests.
 *
 * The rollback button is the second control in HarborMaster that stops
 * something running, so the properties under test are about what an operator is
 * told before and after they use it:
 *
 *   - The confirmation says the container will be UNAVAILABLE, that rollback is
 *     never automatic, and that NOTHING IS REMOVED. All three, in as many
 *     words, and it shows both container ids.
 *   - It cannot be triggered in one click, or without reading the name.
 *   - An ineligible recreation offers no control at all, and says why.
 *   - A record that left containers behind says so above everything else, and
 *     carries the manual recovery steps.
 *   - A verification that was never reached is not rendered as a pass.
 *   - Cancellation is not offered once the host has been changed.
 *   - Daemon-adjacent text is rendered as text, never as markup.
 */

const originalFetch = globalThis.fetch;

let requests: string[] = [];
let writes: { url: string; method: string; body: string }[] = [];

const rollbackID = "rbk_00112233445566778899";
const executionID = "exec_00112233445566778899";
const originalID = "a".repeat(64);
const replacementID = "b".repeat(64);

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

function rollback(overrides: Partial<Rollback> = {}): Rollback {
  return {
    rollbackId: rollbackID,
    executionId: executionID,
    containerName: "web",
    originalId: originalID,
    parkedName: `web.hm-old-${executionID}`,
    replacementId: replacementID,
    originalImage: "nginx:1.27.0",
    originalImageId: `sha256:${"c".repeat(64)}`,
    replacementImage: "nginx:1.27.1",
    state: "succeeded",
    checkpoint: "originalVerified",
    replacementParkedName: `web.hm-rolledback-${rollbackID}`,
    verification: {
      health: "passed",
      healthChecked: true,
      healthState: "healthy",
      image: "passed",
      preservation: "passed",
      network: "passed",
    },
    requestedAt: "2026-08-06T12:00:00Z",
    expiresAt: "2026-08-06T12:10:00Z",
    ...overrides,
  };
}

function summary(overrides: Partial<RollbackSummary> = {}): RollbackSummary {
  return {
    total: 3,
    active: 0,
    succeeded: 2,
    failed: 1,
    needsAttention: 0,
    enabled: true,
    ...overrides,
  };
}

function eligibility(overrides: Partial<RollbackEligibility> = {}): RollbackEligibility {
  return {
    eligible: true,
    containerName: "web",
    originalId: originalID,
    parkedName: `web.hm-old-${executionID}`,
    replacementId: replacementID,
    originalImage: "nginx:1.27.0",
    replacementImage: "nginx:1.27.1",
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
    <MemoryRouter initialEntries={["/rollbacks"]}>
      <Routes>
        <Route path="/rollbacks" element={<Rollbacks />} />
      </Routes>
    </MemoryRouter>,
  );
}

function renderDetail() {
  return render(
    <MemoryRouter initialEntries={[`/rollbacks/${rollbackID}`]}>
      <Routes>
        <Route path="/rollbacks/:id" element={<RollbackDetail />} />
      </Routes>
    </MemoryRouter>,
  );
}

// ------------------------------------------------------------------ list --

it("lists rollbacks and puts what needs attention first", async () => {
  mockApi([
    [
      "/rollbacks",
      {
        items: [rollback()],
        pagination: pagination(1),
        summary: summary({ needsAttention: 2, succeeded: 5 }),
      },
    ],
  ]);

  renderList();

  const cards = await screen.findByLabelText("Rollback summary");
  const tiles = within(cards).getAllByText(
    /Needs attention|In progress|Rolled back|Total/,
  );
  expect(tiles[0]).toHaveTextContent("Needs attention");
  expect(within(cards).getByText("2")).toBeInTheDocument();
});

it("says plainly that a rollback causes downtime and removes nothing", async () => {
  mockApi([
    ["/rollbacks", { items: [], pagination: pagination(0), summary: summary() }],
  ]);

  renderList();

  const note = await screen.findByRole("note");
  expect(note).toHaveTextContent(/unavailable while the rollback runs/i);
  expect(note).toHaveTextContent(/never automatic/i);
  expect(note).toHaveTextContent(/nothing is removed/i);
});

// ---------------------------------------------------------------- detail --

it("puts what is true of the host above everything else on a failed rollback", async () => {
  mockApi([
    [
      `/rollbacks/${rollbackID}`,
      {
        rollback: rollback({
          state: "failed",
          failure: "start",
          checkpoint: "originalRestored",
          mutatedAt: "2026-08-06T12:00:30Z",
          message: "the original container could not be started",
          verification: {
            health: "unknown",
            healthChecked: true,
            image: "unknown",
            preservation: "unknown",
            network: "unknown",
          },
          recovery: {
            urgency: "urgent",
            situation:
              "The replacement is stopped and parked. The original carries its own name and is not running.",
            serviceInterrupted: true,
            steps: [
              {
                order: 1,
                description: "Start the original container by id",
                command: `docker start ${originalID.slice(0, 12)}`,
                destructive: false,
              },
            ],
          },
        }),
        events: [] as RollbackEvent[],
      },
    ],
  ]);

  renderDetail();

  // Two alerts, deliberately: the host-state banner and the recovery panel's
  // own "service is interrupted" line. The FIRST is the one above the fold.
  const alerts = await screen.findAllByRole("alert");
  expect(alerts[0]).toHaveTextContent(/changed this host and did not finish/i);
  expect(alerts[0]).toHaveTextContent(/is NOT running/i);

  // And the manual steps are on the page.
  expect(
    await screen.findByText(/Start the original container by id/i),
  ).toBeInTheDocument();
});

it("never renders a verification that was not reached as a pass", async () => {
  mockApi([
    [
      `/rollbacks/${rollbackID}`,
      {
        rollback: rollback({
          state: "failed",
          failure: "unhealthy",
          checkpoint: "originalStarted",
          mutatedAt: "2026-08-06T12:00:30Z",
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

  expect(await screen.findByText("Health: failed")).toBeInTheDocument();
  for (const label of ["Image", "Configuration", "Networks"]) {
    expect(screen.getByText(`${label}: not checked`)).toBeInTheDocument();
  }
  expect(screen.queryByText("Image: passed")).not.toBeInTheDocument();
});

it("does not offer cancellation once the host has been changed", async () => {
  mockApi([
    [
      `/rollbacks/${rollbackID}`,
      {
        rollback: rollback({
          state: "startingOriginal",
          checkpoint: "originalRestored",
          mutatedAt: "2026-08-06T12:00:30Z",
        }),
        events: [],
      },
    ],
  ]);

  renderDetail();

  await screen.findByText(/Starting original/i);
  expect(
    screen.queryByRole("button", { name: /cancel this rollback/i }),
  ).not.toBeInTheDocument();
});

it("offers cancellation before anything has been changed", async () => {
  mockApi([
    [
      `/rollbacks/${rollbackID}`,
      { rollback: rollback({ state: "queued", checkpoint: "" }), events: [] },
    ],
  ]);

  renderDetail();

  const button = await screen.findByRole("button", { name: /cancel this rollback/i });
  await userEvent.click(button);

  await waitFor(() => {
    expect(writes.some((write) => write.url.includes("/cancel"))).toBe(true);
  });
});

it("renders a daemon-shaped message as text, never as markup", async () => {
  const injected = '<img src=x onerror="alert(1)">';
  mockApi([
    [
      `/rollbacks/${rollbackID}`,
      {
        rollback: rollback({
          state: "failed",
          failure: "stop",
          checkpoint: "",
          message: injected,
        }),
        events: [{ state: "failed", detail: injected, at: "2026-08-06T12:00:00Z" }],
      },
    ],
  ]);

  const { container } = renderDetail();

  await screen.findByText(/Could not stop the replacement/i);
  expect(container.querySelector("img")).toBeNull();
});

// --------------------------------------------------------------- action --

it("states all three consequences before it will roll anything back", async () => {
  mockApi([]);

  render(
    <MemoryRouter>
      <RollbackContainerAction
        executionId={executionID}
        eligibility={eligibility()}
      />
    </MemoryRouter>,
  );

  await userEvent.click(
    screen.getByRole("button", { name: /roll back this recreation/i }),
  );

  const dialog = await screen.findByRole("dialog");
  expect(dialog).toHaveTextContent(/will be unavailable while this runs/i);
  expect(dialog).toHaveTextContent(/manual, and it is not automatic anywhere else/i);
  expect(dialog).toHaveTextContent(/nothing is removed/i);

  // Both container ids, so an operator can check them against `docker ps`.
  expect(dialog).toHaveTextContent(originalID.slice(0, 12));
  expect(dialog).toHaveTextContent(replacementID.slice(0, 12));
});

it("cannot be triggered without typing the container name", async () => {
  mockApi([["/rollbacks", rollback()]]);

  render(
    <MemoryRouter>
      <RollbackContainerAction
        executionId={executionID}
        eligibility={eligibility()}
      />
    </MemoryRouter>,
  );

  await userEvent.click(
    screen.getByRole("button", { name: /roll back this recreation/i }),
  );

  const confirm = await screen.findByRole("button", { name: /^Roll web back$/i });
  expect(confirm).toBeDisabled();

  await userEvent.type(screen.getByRole("textbox"), "not-the-name");
  expect(confirm).toBeDisabled();
  expect(writes).toHaveLength(0);

  await userEvent.clear(screen.getByRole("textbox"));
  await userEvent.type(screen.getByRole("textbox"), "web");
  expect(confirm).toBeEnabled();
});

it("sends only the execution id when it is confirmed", async () => {
  mockApi([["/rollbacks", rollback()]]);

  render(
    <MemoryRouter>
      <RollbackContainerAction
        executionId={executionID}
        eligibility={eligibility()}
      />
    </MemoryRouter>,
  );

  await userEvent.click(
    screen.getByRole("button", { name: /roll back this recreation/i }),
  );
  await userEvent.type(await screen.findByRole("textbox"), "web");
  await userEvent.click(screen.getByRole("button", { name: /^Roll web back$/i }));

  await waitFor(() => expect(writes).toHaveLength(1));

  const body = JSON.parse(writes[0]?.body ?? "{}") as Record<string, unknown>;
  expect(body).toEqual({ executionId: executionID });
  // Nothing that could aim the rollback somewhere else.
  for (const forbidden of [
    "containerId",
    "originalId",
    "replacementId",
    "containerName",
    "image",
    "force",
  ]) {
    expect(body).not.toHaveProperty(forbidden);
  }
});

it("offers no control at all when the recreation cannot be rolled back", async () => {
  mockApi([]);

  render(
    <MemoryRouter>
      <RollbackContainerAction
        executionId={executionID}
        eligibility={eligibility({
          eligible: false,
          refusal: "originalRemoved",
          reason: "the original container has already been removed",
        })}
      />
    </MemoryRouter>,
  );

  expect(
    screen.queryByRole("button", { name: /roll back this recreation/i }),
  ).not.toBeInTheDocument();
  expect(screen.getByRole("status")).toHaveTextContent(
    /already been removed/i,
  );
});

it("says rollback is not configured rather than that it was refused", async () => {
  mockApi([]);

  render(
    <MemoryRouter>
      <RollbackContainerAction executionId={executionID} eligibility={undefined} />
    </MemoryRouter>,
  );

  expect(
    screen.queryByRole("button", { name: /roll back this recreation/i }),
  ).not.toBeInTheDocument();
  expect(screen.getByText(/not enabled on this installation/i)).toBeInTheDocument();
  expect(screen.queryByText(/refused/i)).not.toBeInTheDocument();
});
