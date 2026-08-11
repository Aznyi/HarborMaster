import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import type {
  Acquisition,
  AcquisitionEvent,
  AcquisitionSummary,
} from "../api/acquisitionTypes";
import type { ChangePlan } from "../api/planTypes";
import { AcquireImageAction } from "../components/AcquireImageAction";
import { AcquisitionDetail } from "./AcquisitionDetail";
import { Acquisitions } from "./Acquisitions";

/**
 * Image acquisition UI tests.
 *
 * The acquire button is the only control in HarborMaster that changes the
 * Docker host, so the properties under test are about what an operator is told
 * before and after they use it:
 *
 *   - The confirmation shows the exact repository and DIGEST, not a tag.
 *   - Every view says, in as many words, that no container is updated. A green
 *     "Downloaded" badge is exactly the thing that could be misread as "the
 *     update was applied".
 *   - A digest mismatch is presented as a finding, with the evidence.
 *   - Registry and daemon text is rendered as text, never as markup.
 */

const originalFetch = globalThis.fetch;

let requests: string[] = [];
let writes: { url: string; method: string; body: string }[] = [];

const testDigest = `sha256:${"a".repeat(64)}`;
const otherDigest = `sha256:${"b".repeat(64)}`;
const acquisitionID = "acq_00112233445566778899";
const planID = "plan_00112233445566778899";

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

function acquisition(overrides: Partial<Acquisition> = {}): Acquisition {
  return {
    acquisitionId: acquisitionID,
    planId: planID,
    containerId: "container-a",
    containerName: "web",
    target: {
      registry: "docker.io",
      repository: "library/nginx",
      digest: testDigest,
      reference: "nginx:1.27.1",
      platform: { os: "linux", architecture: "amd64" },
    },
    state: "succeeded",
    requestedAt: "2026-08-05T12:00:00Z",
    expiresAt: "2026-08-05T13:00:00Z",
    planDigest: "f".repeat(64),
    ...overrides,
  };
}

function summary(overrides: Partial<AcquisitionSummary> = {}): AcquisitionSummary {
  return {
    total: 4,
    active: 1,
    succeeded: 2,
    failed: 1,
    byState: { succeeded: 2, failed: 1, pulling: 1 },
    byFailure: { transfer: 1 },
    enabled: true,
    ...overrides,
  };
}

function plan(overrides: Partial<ChangePlan> = {}): ChangePlan {
  return {
    planId: planID,
    containerId: "container-a",
    containerName: "web",
    currentImage: "nginx:1.27.0",
    proposedImage: "nginx:1.27.1",
    proposedDigest: testDigest,
    updateType: "patch",
    snapshotAvailable: true,
    restoreReadiness: "ready",
    driftOpen: 0,
    policyOpen: 0,
    registryStatus: "ok",
    risk: {
      riskScore: 5,
      riskBand: "veryLow",
      recommendation: "proceed",
      summary: "Nothing argues against this change.",
      factors: [],
    },
    planVersion: 1,
    plannerVersion: "1",
    inputDigest: "c".repeat(64),
    generatedAt: "2026-08-05T12:00:00Z",
    superseded: false,
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
    <MemoryRouter initialEntries={["/acquisitions"]}>
      <Routes>
        <Route path="/acquisitions" element={<Acquisitions />} />
      </Routes>
    </MemoryRouter>,
  );
}

function renderDetail() {
  return render(
    <MemoryRouter initialEntries={[`/acquisitions/${acquisitionID}`]}>
      <Routes>
        <Route path="/acquisitions/:id" element={<AcquisitionDetail />} />
      </Routes>
    </MemoryRouter>,
  );
}

// ------------------------------------------------------------------ list --

it("lists acquisitions with the digest that was fetched", async () => {
  mockApi([
    ["/acquisitions", { items: [acquisition()], pagination: pagination(1), summary: summary() }],
  ]);
  renderList();

  expect(await screen.findByText("nginx:1.27.1")).toBeInTheDocument();
  // The digest, not just the friendly name: it is what was actually fetched.
  expect(screen.getByText(new RegExp(testDigest))).toBeInTheDocument();
});

// The single most important thing this feature has to communicate.
it("says on every acquisition view that no container was updated", async () => {
  mockApi([
    ["/acquisitions", { items: [acquisition()], pagination: pagination(1), summary: summary() }],
  ]);
  renderList();

  const notice = await screen.findByRole("note");
  expect(notice).toHaveTextContent(/does not update, restart, or recreate any container/i);
});

// A green "Downloaded" badge is exactly what could be read as "applied", so its
// tooltip says otherwise in as many words.
it("does not let a successful download read as an applied update", async () => {
  mockApi([
    ["/acquisitions", { items: [acquisition()], pagination: pagination(1), summary: summary() }],
  ]);
  renderList();

  const badge = await screen.findByTitle(/NO CONTAINER HAS BEEN CHANGED/i);
  expect(badge).toHaveTextContent(/downloaded/i);
});

it("states plainly when acquisition is switched off", async () => {
  mockApi([
    [
      "/acquisitions",
      { items: [], pagination: pagination(0), summary: summary({ enabled: false }) },
    ],
  ]);
  renderList();

  expect(
    await screen.findByText(/Image acquisition is switched off in this deployment/i),
  ).toBeInTheDocument();
});

it("explains an empty history rather than showing a blank page", async () => {
  mockApi([
    ["/acquisitions", { items: [], pagination: pagination(0), summary: summary({ total: 0 }) }],
  ]);
  renderList();

  expect(
    await screen.findByText("No image downloads match these filters"),
  ).toBeInTheDocument();
  // And says that nothing happens on a schedule, which is the question an
  // empty list invites.
  expect(
    screen.getByText(/never\s+downloads an image on its own/i),
  ).toBeInTheDocument();
});

// ---------------------------------------------------------------- confirm --

// The confirmation is the control that matters. It must show the exact content,
// and it must say what will NOT happen.
it("confirms with the exact repository and digest before downloading", async () => {
  mockApi([["/acquisitions", acquisition(), 202]]);

  render(
    <MemoryRouter>
      <AcquireImageAction plan={plan()} />
    </MemoryRouter>,
  );

  await userEvent.click(screen.getByRole("button", { name: /acquire image/i }));

  const dialog = screen.getByRole("dialog");
  expect(within(dialog).getByText("nginx:1.27.1")).toBeInTheDocument();
  expect(within(dialog).getByText(testDigest)).toBeInTheDocument();
  expect(within(dialog).getByText(/This does not update web/i)).toBeInTheDocument();
  expect(
    within(dialog).getByText(/no container is stopped, restarted, or recreated/i),
  ).toBeInTheDocument();

  // Nothing has been requested yet: opening a confirmation is not acting.
  expect(writes).toHaveLength(0);
});

it("sends only the plan id, never a target", async () => {
  mockApi([["/acquisitions", acquisition(), 202]]);

  render(
    <MemoryRouter>
      <AcquireImageAction plan={plan()} />
    </MemoryRouter>,
  );

  await userEvent.click(screen.getByRole("button", { name: /acquire image/i }));
  await userEvent.click(screen.getByRole("button", { name: /download image/i }));

  await waitFor(() => expect(writes).toHaveLength(1));
  expect(writes[0]?.method).toBe("POST");

  const body = JSON.parse(writes[0]?.body ?? "{}") as Record<string, unknown>;
  expect(body.planId).toBe(planID);
  // The request carries no target of any kind. This is the property that keeps
  // the endpoint from being a general-purpose downloader.
  for (const forbidden of ["registry", "repository", "digest", "image", "tag", "platform"]) {
    expect(body).not.toHaveProperty(forbidden);
  }
});

it("can be dismissed without downloading anything", async () => {
  mockApi([["/acquisitions", acquisition(), 202]]);

  render(
    <MemoryRouter>
      <AcquireImageAction plan={plan()} />
    </MemoryRouter>,
  );

  await userEvent.click(screen.getByRole("button", { name: /acquire image/i }));
  await userEvent.click(screen.getByRole("button", { name: /^cancel$/i }));

  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(writes).toHaveLength(0);
});

// A plan with no resolved digest has nothing immutable to fetch, so the control
// explains itself rather than offering an action that would be refused.
it("offers no action for a plan with no digest", () => {
  render(
    <MemoryRouter>
      <AcquireImageAction plan={plan({ proposedDigest: undefined })} />
    </MemoryRouter>,
  );

  expect(screen.queryByRole("button", { name: /acquire image/i })).not.toBeInTheDocument();
  expect(screen.getByText(/no resolved digest/i)).toBeInTheDocument();
});

// A refusal is the safety model working, and the operator is told which check
// said no rather than being given a generic failure.
it("reports a refusal in HarborMaster's own words", async () => {
  mockApi([
    [
      "/acquisitions",
      {
        error: {
          code: "conflict",
          message:
            "the digest on offer has changed since the plan was written, so this is no longer the image that was approved",
        },
        refusal: "digestChanged",
      },
      409,
    ],
  ]);

  render(
    <MemoryRouter>
      <AcquireImageAction plan={plan()} />
    </MemoryRouter>,
  );

  await userEvent.click(screen.getByRole("button", { name: /acquire image/i }));
  await userEvent.click(screen.getByRole("button", { name: /download image/i }));

  expect(await screen.findByRole("alert")).toHaveTextContent(
    /no longer the image that was approved/i,
  );
});

// --------------------------------------------------------------- detail --

it("shows the pinned target and the audit trail", async () => {
  const events: AcquisitionEvent[] = [
    { state: "queued", detail: "requested by an operator", at: "2026-08-05T12:00:00Z" },
    { state: "pulling", detail: "Downloading", at: "2026-08-05T12:00:05Z" },
    { state: "succeeded", detail: "verified", at: "2026-08-05T12:01:00Z" },
  ];
  mockApi([
    [
      `/acquisitions/${acquisitionID}`,
      { acquisition: acquisition({ acquiredImageId: "sha256:image1", sizeBytes: 1048576 }), events },
    ],
  ]);
  renderDetail();

  const requested = await screen.findByLabelText("What was requested");
  expect(within(requested).getByText(testDigest)).toBeInTheDocument();
  expect(
    within(requested).getByText(`docker.io/library/nginx@${testDigest}`),
  ).toBeInTheDocument();

  const arrived = screen.getByLabelText("What arrived");
  expect(within(arrived).getByText("sha256:image1")).toBeInTheDocument();
  expect(within(arrived).getByText("1 MiB")).toBeInTheDocument();

  const trail = screen.getByLabelText("What happened");
  expect(within(trail).getByText("requested by an operator")).toBeInTheDocument();
  expect(within(trail).getByText("verified")).toBeInTheDocument();
});

// A digest mismatch is a finding, not an inconvenience: the host holds content
// nobody approved.
it("presents a digest mismatch as a finding with the evidence", async () => {
  mockApi([
    [
      `/acquisitions/${acquisitionID}`,
      {
        acquisition: acquisition({
          state: "failed",
          failure: "digestMismatch",
          message: "the acquired image is not the one that was approved",
          acquiredImageId: "sha256:unexpected",
          acquiredDigest: otherDigest,
        }),
        events: [],
      },
    ],
  ]);
  renderDetail();

  expect(
    await screen.findByText(/The image that arrived is not the one that was approved/i),
  ).toBeInTheDocument();
  expect(screen.getByText("sha256:unexpected")).toBeInTheDocument();
  // Still says no container was changed, which is what makes this recoverable.
  expect(screen.getByText(/No container was changed\./i)).toBeInTheDocument();
  // And does not invite a retry.
  expect(screen.queryByText(/looks transient/i)).not.toBeInTheDocument();
});

it("suggests a retry only for a transient failure", async () => {
  mockApi([
    [
      `/acquisitions/${acquisitionID}`,
      {
        acquisition: acquisition({
          state: "failed",
          failure: "transfer",
          message: "the transfer did not complete",
        }),
        events: [],
      },
    ],
  ]);
  renderDetail();

  expect(await screen.findByText(/looks transient/i)).toBeInTheDocument();
  expect(screen.getByText(/never retries on its own/i)).toBeInTheDocument();
});

// --------------------------------------------------------------- cancel --

it("offers cancellation while a transfer is running", async () => {
  mockApi([
    [
      `/acquisitions/${acquisitionID}/cancel`,
      acquisition({ state: "cancelled" }),
    ],
    [
      `/acquisitions/${acquisitionID}`,
      {
        acquisition: acquisition({ state: "pulling", progress: "Downloading", layers: 3 }),
        events: [],
      },
    ],
  ]);
  renderDetail();

  const button = await screen.findByRole("button", { name: /cancel download/i });
  await userEvent.click(button);

  await waitFor(() => expect(writes).toHaveLength(1));
  expect(writes[0]?.url).toContain(`/acquisitions/${acquisitionID}/cancel`);
  expect(writes[0]?.method).toBe("POST");
});

// Verifying is deliberately not cancellable: the bytes are already on the host.
it("does not offer cancellation once verification has started", async () => {
  mockApi([
    [
      `/acquisitions/${acquisitionID}`,
      { acquisition: acquisition({ state: "verifying" }), events: [] },
    ],
  ]);
  renderDetail();

  await screen.findByLabelText("Status");
  expect(screen.queryByRole("button", { name: /cancel download/i })).not.toBeInTheDocument();
});

it("shows live progress while pulling", async () => {
  mockApi([
    [
      `/acquisitions/${acquisitionID}`,
      {
        acquisition: acquisition({
          state: "pulling",
          progress: "Downloading",
          layers: 4,
          bytesTransferred: 5242880,
        }),
        events: [],
      },
    ],
  ]);
  renderDetail();

  expect(await screen.findByText(/Downloading · 4 layers/)).toBeInTheDocument();
  // The byte count is labelled as an estimate, because layers already present
  // are never counted. Matched on the paragraph's whole text: JSX splits the
  // formatted number and the literal into separate nodes.
  expect(
    screen.getByText(
      (_, element) =>
        element?.tagName === "P" &&
        (element.textContent ?? "").includes("5 MiB transferred (an estimate"),
    ),
  ).toBeInTheDocument();
});

// -------------------------------------------------------------- rendering --

// Progress text is relayed by the daemon from a registry. React escapes it;
// this pins that it is never treated as markup.
it("renders hostile daemon and registry text as text", async () => {
  const { container } = render(
    <MemoryRouter initialEntries={[`/acquisitions/${acquisitionID}`]}>
      <Routes>
        <Route path="/acquisitions/:id" element={<AcquisitionDetail />} />
      </Routes>
    </MemoryRouter>,
  );
  container.remove();

  mockApi([
    [
      `/acquisitions/${acquisitionID}`,
      {
        acquisition: acquisition({
          state: "pulling",
          progress: "<img src=x onerror=alert(1)>",
          containerName: "<script>alert(1)</script>",
        }),
        events: [
          { state: "pulling", detail: "<script>alert(2)</script>", at: "2026-08-05T12:00:00Z" },
        ],
      },
    ],
  ]);

  const { container: live } = renderDetail();

  expect(await screen.findByText("<img src=x onerror=alert(1)>")).toBeInTheDocument();
  expect(live.querySelector("img")).toBeNull();
  expect(live.querySelector("script")).toBeNull();
});
