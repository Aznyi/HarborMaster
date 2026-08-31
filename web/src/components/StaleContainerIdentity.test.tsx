import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { RecreateContainerAction } from "./RecreateContainerAction";
import { TestSessionProvider, testSession, testUser } from "../test/session";
import type { Acquisition } from "../api/acquisitionTypes";

/**
 * Historical ids and current ids are different questions (C3A, Part B).
 *
 * # The defect
 *
 * An acquisition records the container it was requested for. HarborMaster
 * applies the update by RECREATING that container, and the replacement has a
 * different Docker id — so from the moment an update succeeds, the id on the
 * record names something that is no longer on the host.
 *
 * The recreate control read what depends on the container using that id. On
 * Updates and Activity, every row for an already-applied update therefore
 * issued `GET /dependencies/container/{old-id}`, got a 404, and filled the
 * console with errors about a container that was running perfectly well under
 * a new id.
 *
 * # The rule
 *
 * `containerId` is history and never moves. `currentContainerId` is resolved
 * server-side from the container NAME — HarborMaster's stable identity across a
 * recreation — in the same query that reads the acquisition, so it costs no
 * extra request. Anything asking the host a question NOW uses the second.
 *
 * Absence of `currentContainerId` means there is no container to ask about, and
 * the honest handling is to make NO request rather than one that 404s.
 */

const originalFetch = globalThis.fetch;
let requested: string[] = [];

function stubApi() {
  requested = [];
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    requested.push(url);
    if (url.includes("/dependencies/container/")) {
      return new Response(
        JSON.stringify({
          container: "vaultwarden",
          dependsOn: [],
          dependedOnBy: [],
          state: "satisfied",
        }),
        { status: 200, headers: { "content-type": "application/json" } },
      );
    }
    return new Response(JSON.stringify({ error: { code: "not_found" } }), {
      status: 404,
    });
  }) as typeof fetch;
}

/** An acquisition whose container has since been recreated under a new id. */
function recreated(overrides: Partial<Acquisition> = {}): Acquisition {
  return {
    acquisitionId: "acq-1",
    planId: "plan-1",
    // What happened: the update was requested for OLD.
    containerId: "container-old",
    containerName: "vaultwarden",
    // What is running now: the replacement.
    currentContainerId: "container-new",
    state: "succeeded",
    requestedAt: "2026-09-01T10:00:00Z",
    target: {
      reference: "docker.io/vaultwarden/server:1.32.0",
      registry: "docker.io",
      repository: "vaultwarden/server",
      tag: "1.32.0",
      digest: "sha256:abc123def456",
    },
    ...overrides,
  } as Acquisition;
}

function renderAction(acquisition: Acquisition) {
  return render(
    <TestSessionProvider session={testSession({ user: testUser("administrator") })}>
      <MemoryRouter>
        <RecreateContainerAction acquisition={acquisition} />
      </MemoryRouter>
    </TestSessionProvider>,
  );
}

const dependencyCalls = () =>
  requested.filter((url) => url.includes("/dependencies/container/"));

beforeEach(stubApi);
afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

// ------------------------------------------------ the stale-id regression --

it("asks about the CURRENT container, never the historical one", async () => {
  // THE REGRESSION GUARD. This is the request that used to 404 on every
  // Updates and Activity row after an update had been applied.
  renderAction(recreated());

  await waitFor(() => expect(dependencyCalls().length).toBeGreaterThan(0));

  for (const url of dependencyCalls()) {
    expect(url, "a dependency read used the historical id").not.toContain(
      "container-old",
    );
  }
  expect(dependencyCalls().some((url) => url.includes("container-new"))).toBe(
    true,
  );
});

it("makes no request at all when no current container exists", async () => {
  // The honest missing state. There is nothing on the host to ask about, so
  // the correct number of requests is zero — not one that 404s, and never a
  // fallback to the historical id.
  renderAction(recreated({ currentContainerId: undefined }));

  await screen.findByText(/no container named/i);

  expect(dependencyCalls()).toEqual([]);
  expect(requested.some((url) => url.includes("container-old"))).toBe(false);
});

it("says why there is nothing to recreate, and keeps the record", async () => {
  renderAction(recreated({ currentContainerId: undefined }));

  expect(await screen.findByText(/no container named/i)).toBeInTheDocument();
  expect(screen.getByText(/vaultwarden/)).toBeInTheDocument();
  expect(screen.getByText(/kept as evidence/i)).toBeInTheDocument();
  // And no control that cannot work.
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
});

it("follows the latest id across repeated recreations", async () => {
  // Set-and-forget means many updates over time. Each recreation replaces the
  // id again, and the control must track the newest one rather than any
  // intermediate generation.
  const { unmount } = renderAction(
    recreated({ currentContainerId: "container-v2" }),
  );
  await waitFor(() => expect(dependencyCalls().length).toBeGreaterThan(0));
  expect(dependencyCalls().at(-1)).toContain("container-v2");
  unmount();

  stubApi();
  renderAction(recreated({ currentContainerId: "container-v3" }));
  await waitFor(() => expect(dependencyCalls().length).toBeGreaterThan(0));
  expect(dependencyCalls().at(-1)).toContain("container-v3");
  // The original id never appears, in either generation.
  expect(requested.some((url) => url.includes("container-old"))).toBe(false);
});

it("reads dependencies once, not once per render pass", async () => {
  // No N+1: the control asks about one container, once. A row that re-read on
  // every render would turn a page of history into a burst of requests.
  renderAction(recreated());

  await waitFor(() => expect(dependencyCalls().length).toBeGreaterThan(0));
  const settled = dependencyCalls().length;
  await new Promise((resolve) => setTimeout(resolve, 50));

  expect(dependencyCalls().length).toBe(settled);
  expect(settled).toBeLessThanOrEqual(1);
});
