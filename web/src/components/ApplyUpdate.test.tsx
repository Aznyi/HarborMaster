import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { RecreateContainerAction } from "./RecreateContainerAction";
import { TestSessionProvider, testSession, testUser } from "../test/session";
import type { Acquisition } from "../api/acquisitionTypes";

/**
 * "Apply update" is a label. "Recreate the container" is the operation.
 *
 * # What this defends
 *
 * The normal workflow now names the control for what the operator came to do.
 * The risk in that change is obvious and worth pinning: a friendlier word on a
 * button that stops a running service, with the explanation quietly softened to
 * match. That must not happen, so these tests assert the opposite properties --
 * the confirmation still says stopped, still says recreated, still says rollback
 * is not automatic, and still demands the container's name be typed.
 *
 * The advanced acquisition page keeps "Recreate container", because a page
 * about the download record is a place where the technical name is the accurate
 * one.
 */

const originalFetch = globalThis.fetch;

function acquisition(overrides: Partial<Acquisition> = {}): Acquisition {
  return {
    acquisitionId: "acq-1",
    planId: "plan-1",
    containerId: "c1",
    containerName: "vaultwarden",
    state: "succeeded",
    requestedAt: "2026-08-27T10:00:00Z",
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

function renderAction(props: { label?: string } = {}) {
  return render(
    <TestSessionProvider session={testSession({ user: testUser("administrator") })}>
      <MemoryRouter>
        <RecreateContainerAction acquisition={acquisition()} {...props} />
      </MemoryRouter>
    </TestSessionProvider>,
  );
}

beforeEach(() => {
  globalThis.fetch = vi.fn(async () =>
    new Response(JSON.stringify({ items: [], total: 0 }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    }),
  ) as typeof fetch;
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

// ------------------------------------------------------------- the label --

it("says what the operator came to do when the caller asks it to", () => {
  renderAction({ label: "Apply update" });

  expect(screen.getByRole("button", { name: "Apply update" })).toBeInTheDocument();
  expect(
    screen.queryByRole("button", { name: /recreate container/i }),
  ).not.toBeInTheDocument();
});

it("keeps the technical name where the technical name is the accurate one", () => {
  // No label passed: the acquisition record's own page gets the default.
  renderAction();
  expect(
    screen.getByRole("button", { name: "Recreate container" }),
  ).toBeInTheDocument();
});

it("tells the truth on hover regardless of what the button says", () => {
  // The friendlier word must not be the only thing an operator sees before
  // they commit to anything.
  renderAction({ label: "Apply update" });
  expect(screen.getByRole("button", { name: "Apply update" })).toHaveAttribute(
    "title",
    expect.stringMatching(/stops this container and replaces it/i),
  );
});

// ------------------------------------------------------ the confirmation --

it("still explains that the container is stopped and recreated", async () => {
  const user = userEvent.setup();
  renderAction({ label: "Apply update" });

  await user.click(screen.getByRole("button", { name: "Apply update" }));

  const dialog = await screen.findByRole("dialog");
  expect(
    within(dialog).getByRole("heading", { name: /stop and replace vaultwarden/i }),
  ).toBeInTheDocument();
  expect(dialog.textContent).toMatch(/will be stopped and recreated/i);
  expect(dialog.textContent).toMatch(/rollback is NOT automatic/i);
  expect(dialog.textContent).toMatch(/the image is already on this host/i);
  // The exact content, not the name.
  expect(dialog.textContent).toMatch(/sha256:abc123def456/);
});

it("still requires the container's name to be typed", async () => {
  const user = userEvent.setup();
  renderAction({ label: "Apply update" });

  await user.click(screen.getByRole("button", { name: "Apply update" }));
  const dialog = await screen.findByRole("dialog");

  const commit = within(dialog).getByRole("button", {
    name: /stop and recreate vaultwarden/i,
  });
  expect(commit).toBeDisabled();

  await user.type(within(dialog).getByRole("textbox"), "vaultwarden");
  await waitFor(() => expect(commit).toBeEnabled());
});

// ------------------------------------------------- the underlying request --

it("sends the same request whatever the button was called", async () => {
  const user = userEvent.setup();
  const calls = vi.mocked(globalThis.fetch);
  renderAction({ label: "Apply update" });

  await user.click(screen.getByRole("button", { name: "Apply update" }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByRole("textbox"), "vaultwarden");
  await user.click(
    within(dialog).getByRole("button", { name: /stop and recreate vaultwarden/i }),
  );

  await waitFor(() => {
    const posted = calls.mock.calls.find(
      ([, init]) => (init as RequestInit | undefined)?.method === "POST",
    );
    expect(posted).toBeTruthy();
    // The acquisition id is the whole request. No container, image, or option
    // is supplied by the caller, and the label changed none of that.
    expect(String(posted?.[0])).toContain("/executions");
    expect(String((posted?.[1] as RequestInit).body)).toContain("acq-1");
  });
});

it("refuses to offer anything for an acquisition that did not succeed", () => {
  render(
    <TestSessionProvider session={testSession({ user: testUser("administrator") })}>
      <MemoryRouter>
        <RecreateContainerAction
          acquisition={acquisition({ state: "failed" })}
          label="Apply update"
        />
      </MemoryRouter>
    </TestSessionProvider>,
  );

  expect(screen.queryByRole("button", { name: "Apply update" })).not.toBeInTheDocument();
  expect(
    screen.getByText(/has not been downloaded and verified/i),
  ).toBeInTheDocument();
});
