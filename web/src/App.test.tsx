import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";

import { SessionProvider } from "./hooks/useSession";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { ADVANCED_NAV, PRIMARY_NAV } from "./components/AppShell";
import type { HealthReport } from "./api/types";
import { stubApi as stubAllEndpoints } from "./test/fixtures";

const healthyReport: HealthReport = {
  status: "healthy",
  database: { status: "up", latencyMs: 1 },
  docker: { status: "up", latencyMs: 4, version: "1.51" },
  // The default posture: everything that only reads is on, and every capability
  // that can change a host is off.
  features: {
    inventory: true,
    events: true,
    snapshots: true,
    drift: true,
    policy: true,
    planner: true,
    imageIntel: true,
    acquisition: false,
    execution: false,
    rollback: false,
    imageCleanup: false,
    automation: false,
    notifications: false,
    notificationsAllowPrivate: false,
  },
  checkedAt: "2026-08-03T09:20:11.482Z",
  uptimeSeconds: 120,
};

const degradedReport: HealthReport = {
  status: "degraded",
  database: { status: "up", latencyMs: 1 },
  docker: { status: "down", detail: "docker engine unreachable", latencyMs: 10000 },
  checkedAt: "2026-08-03T09:20:11.482Z",
  uptimeSeconds: 120,
};


/**
 * Routes fetch calls to canned responses per endpoint.
 *
 * Delegates to the shared fixture so this suite and the inventory suite cannot
 * drift apart on what the API returns. `health` is the only knob these shell
 * tests need; when it is an error the inventory is failed too, because a
 * backend that cannot answer /health cannot answer /inventory either.
 */
function stubApi(health: HealthReport | Error) {
  stubAllEndpoints(
    health instanceof Error
      ? { health, inventory: health }
      : { health },
  );
}

function renderApp(initialPath = "/") {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <SessionProvider>
        <App />
      </SessionProvider>
    </MemoryRouter>,
  );
}

/**
 * Waits for the first health response to land.
 *
 * Tests that assert on the pre-fetch render still have to settle afterwards,
 * or the resolving promise updates state outside act() and React warns.
 */
async function settle() {
  await waitFor(() =>
    expect(screen.queryByText(/checking/i)).not.toBeInTheDocument(),
  );
}

/**
 * Waits for the shell itself.
 *
 * The identity is resolved before anything is rendered, so a test that asserts
 * on the navigation has to wait for the session to land first. That ordering is
 * the point: an unauthenticated visitor never sees the shell at all.
 */
async function shell() {
  return waitFor(() => screen.getByRole("navigation", { name: /primary/i }));
}

describe("App shell", () => {
  beforeEach(() => {
    vi.unstubAllGlobals();
  });

  it("renders the primary destinations, and only those", async () => {
    stubApi(healthyReport);
    renderApp();

    const nav = await shell();
    for (const item of PRIMARY_NAV) {
      expect(within(nav).getByRole("link", { name: item.label })).toBeInTheDocument();
    }

    // The specialised tools are the point of the change: they exist, they are
    // routable, and the default sidebar does not put them in front of somebody
    // who has not asked for them.
    for (const item of ADVANCED_NAV) {
      expect(
        within(nav).queryByRole("link", { name: item.label }),
      ).not.toBeInTheDocument();
    }
    await settle();
  });

  it("shows a loading state before the first health response", async () => {
    stubApi(healthyReport);
    renderApp();

    // The session resolves first, then the dashboard's own data. Both render a
    // status region, so the assertion is that SOMETHING said "loading" before
    // any data arrived rather than which of the two it was.
    expect(screen.getByRole("status", { name: /(checking|loading)/i })).toBeInTheDocument();
    await settle();
  });

  it("reports backend and Docker connectivity when both are up", async () => {
    stubApi(healthyReport);
    renderApp();

    await waitFor(() =>
      expect(screen.getByText(/backend: connected/i)).toBeInTheDocument(),
    );
    expect(screen.getByText(/docker: connected/i)).toBeInTheDocument();
  });

  // Docker down is a degraded state, not an outage: the backend indicator must
  // stay connected so the operator can tell the two apart.
  it("distinguishes a disconnected Docker socket from a disconnected backend", async () => {
    stubApi(degradedReport);
    renderApp();

    await waitFor(() =>
      expect(screen.getByText(/docker: disconnected/i)).toBeInTheDocument(),
    );
    expect(screen.getByText(/backend: connected/i)).toBeInTheDocument();
  });

  it("shows a disconnected state when the backend is unreachable", async () => {
    stubApi(new TypeError("Failed to fetch"));
    renderApp();

    await waitFor(() =>
      expect(screen.getByText(/backend: unreachable/i)).toBeInTheDocument(),
    );
    expect(
      screen.getByText(/cannot reach the harbormaster backend/i),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /try again/i })).toBeInTheDocument();
  });

  it("retries when the operator asks", async () => {
    stubApi(new TypeError("Failed to fetch"));
    const user = userEvent.setup();
    renderApp();

    await waitFor(() =>
      expect(screen.getByRole("button", { name: /try again/i })).toBeInTheDocument(),
    );
    const callsBefore = vi.mocked(fetch).mock.calls.length;

    await user.click(screen.getByRole("button", { name: /try again/i }));

    await waitFor(() =>
      expect(vi.mocked(fetch).mock.calls.length).toBeGreaterThan(callsBefore),
    );
  });

  it("renders real health data on the dashboard", async () => {
    stubApi(healthyReport);
    renderApp();

    // Scoped to the page: the header's status detail reports the same Engine
    // version, and this assertion is about the dashboard rendering it.
    const main = await screen.findByRole("main");
    await waitFor(() => expect(within(main).getByText("Docker")).toBeInTheDocument());
    expect(within(main).getByText("Database")).toBeInTheDocument();
    // The Engine API version comes from the response, not from a constant.
    expect(within(main).getByText(/API 1\.51/)).toBeInTheDocument();
  });

  it("navigates between sections", async () => {
    stubApi(healthyReport);
    const user = userEvent.setup();
    renderApp();

    const nav = await shell();
    await user.click(within(nav).getByRole("link", { name: "Updates" }));

    await waitFor(() =>
      expect(
        screen.getByRole("heading", { name: "Updates", level: 2 }),
      ).toBeInTheDocument(),
    );
    await settle();
  });

  // Placeholder rows in an operations tool are indistinguishable from real data
  // at a glance, so a page with no data must show an empty state rather than
  // invent one.
  //
  // Snapshots gained a real endpoint in Phase 3; what it must NOT do is render
  // a table when the backend returned nothing.
  it("shows no placeholder data when the snapshot list is empty", async () => {
    stubApi(healthyReport);
    renderApp("/snapshots");

    await waitFor(() =>
      expect(screen.getByText(/No snapshots yet/i)).toBeInTheDocument(),
    );
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    expect(screen.queryByRole("row")).not.toBeInTheDocument();
    await settle();
  });

  // The settings page says what the process CAN DO, and never what it is
  // configured with.
  //
  // The distinction matters: almost everything HarborMaster can do to a host is
  // off by default, so an operator looking at an empty page cannot otherwise
  // tell "switched off" from "not working". The values that produced those
  // states stay out, because the same mechanism carries credentials.
  it("states which capabilities exist without showing any configured value", async () => {
    stubApi(healthyReport);
    renderApp("/settings");

    await waitFor(() =>
      expect(screen.getByText("Recreate containers")).toBeInTheDocument(),
    );
    expect(screen.getByText("Unattended updates")).toBeInTheDocument();
    expect(screen.getAllByText(/Notifications/).length).toBeGreaterThan(0);

    // No configured value appears: not a socket path, not a database path, not
    // an address.
    expect(screen.queryByText(/unix:\/\//)).not.toBeInTheDocument();
    expect(screen.queryByText(/\/var\/lib\/harbormaster/)).not.toBeInTheDocument();
  });

  it("exposes a mobile navigation toggle", async () => {
    stubApi(healthyReport);
    const user = userEvent.setup();
    renderApp();

    await shell();
    const toggle = screen.getByRole("button", { name: /menu/i });
    expect(toggle).toHaveAttribute("aria-expanded", "false");

    await user.click(toggle);

    expect(toggle).toHaveAttribute("aria-expanded", "true");
    await settle();
  });

  // Batch B2 replaced the wildcard redirect with a real Not Found page.
  //
  // The redirect was the friendliest possible failure and it hid real defects:
  // a link to a route that never existed landed on the dashboard, so nothing
  // looked wrong. Batch A found three such links. What is asserted here is that
  // the failure is now VISIBLE -- and that the dashboard specifically is not
  // what an unknown address renders.
  it("renders Not Found for an unknown route rather than the dashboard", async () => {
    stubApi(healthyReport);
    renderApp("/does-not-exist");

    await waitFor(() =>
      expect(
        screen.getByRole("heading", { name: /page not found/i }),
      ).toBeInTheDocument(),
    );

    // The dashboard leads with the attention list, so this is what "silently
    // landed on the dashboard" would look like.
    expect(
      screen.queryByRole("heading", { name: /your containers/i }),
    ).not.toBeInTheDocument();
  });

  // The address bar is the point. An operator has to be able to see WHICH path
  // failed, a screenshot has to stay truthful, and a bookmark to a dead link
  // must not quietly become a dashboard bookmark.
  it("leaves the unknown address in the URL", async () => {
    stubApi(healthyReport);
    renderApp("/some/mistyped/path");

    const marker = await screen.findByTestId("not-found-path");
    expect(marker).toHaveAttribute("data-path", "/some/mistyped/path");
  });

  // A missing page is still a page of the signed-in application: same shell,
  // same six destinations, same header controls.
  it("keeps the normal application shell on Not Found", async () => {
    stubApi(healthyReport);
    renderApp("/does-not-exist");

    await screen.findByRole("heading", { name: /page not found/i });

    const sidebar = await screen.findByRole("navigation", { name: /primary/i });
    for (const label of [
      "Dashboard",
      "Containers",
      "Updates",
      "Automation",
      "Activity",
      "Settings",
    ]) {
      expect(within(sidebar).getByRole("link", { name: label })).toBeInTheDocument();
    }

    expect(
      screen.getByRole("link", { name: /go to dashboard/i }),
    ).toHaveAttribute("href", "/");
  });
});
