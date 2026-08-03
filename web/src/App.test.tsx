import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { NAV_ITEMS } from "./components/AppShell";
import type { HealthReport, VersionInfo } from "./api/types";

const healthyReport: HealthReport = {
  status: "healthy",
  database: { status: "up", latencyMs: 1 },
  docker: { status: "up", latencyMs: 4, version: "1.51" },
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

const buildInfo: VersionInfo = {
  version: "v0.1.0",
  commit: "9f2c1ab",
  buildDate: "2026-08-03T08:00:00Z",
  goVersion: "go1.26.5",
  platform: "linux/amd64",
};

/** Routes fetch calls to canned responses per endpoint. */
function stubApi(health: HealthReport | Error, version: VersionInfo = buildInfo) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/health")) {
        if (health instanceof Error) return Promise.reject(health);
        return Promise.resolve(
          new Response(JSON.stringify(health), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          }),
        );
      }
      return Promise.resolve(
        new Response(JSON.stringify(version), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    }),
  );
}

function renderApp(initialPath = "/") {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <App />
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

describe("App shell", () => {
  beforeEach(() => {
    vi.unstubAllGlobals();
  });

  it("renders navigation for every section", async () => {
    stubApi(healthyReport);
    renderApp();

    const nav = screen.getByRole("navigation", { name: /primary/i });
    for (const item of NAV_ITEMS) {
      expect(within(nav).getByRole("link", { name: item.label })).toBeInTheDocument();
    }
    await settle();
  });

  it("shows a loading state before the first health response", async () => {
    stubApi(healthyReport);
    renderApp();

    expect(screen.getByRole("status", { name: /loading/i })).toBeInTheDocument();
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

    await waitFor(() => expect(screen.getByText("Docker Engine")).toBeInTheDocument());
    expect(screen.getByText("Database")).toBeInTheDocument();
    // The Engine API version comes from the response, not from a constant.
    expect(screen.getByText("1.51")).toBeInTheDocument();
  });

  it("navigates between sections", async () => {
    stubApi(healthyReport);
    const user = userEvent.setup();
    renderApp();

    const nav = screen.getByRole("navigation", { name: /primary/i });
    await user.click(within(nav).getByRole("link", { name: "Snapshots" }));

    expect(
      screen.getByRole("heading", { name: "Snapshots", level: 2 }),
    ).toBeInTheDocument();
    await settle();
  });

  // Placeholder rows in an operations tool are indistinguishable from real
  // data at a glance, so pages without an endpoint must show nothing.
  it("shows no placeholder data on pages whose endpoints do not exist yet", async () => {
    stubApi(healthyReport);
    renderApp("/containers");

    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    expect(screen.queryByRole("row")).not.toBeInTheDocument();
    expect(screen.getByText(/not available yet/i)).toBeInTheDocument();
    await settle();
  });

  it("lists configuration variable names without their values", async () => {
    stubApi(healthyReport);
    renderApp("/settings");

    await waitFor(() =>
      expect(screen.getByText("HARBORMASTER_DOCKER_HOST")).toBeInTheDocument(),
    );
    // The API never returns config values, so none can appear here.
    expect(screen.queryByText(/unix:\/\//)).not.toBeInTheDocument();
  });

  it("exposes a mobile navigation toggle", async () => {
    stubApi(healthyReport);
    const user = userEvent.setup();
    renderApp();

    const toggle = screen.getByRole("button", { name: /menu/i });
    expect(toggle).toHaveAttribute("aria-expanded", "false");

    await user.click(toggle);

    expect(toggle).toHaveAttribute("aria-expanded", "true");
    await settle();
  });

  it("redirects unknown routes to the dashboard", async () => {
    stubApi(healthyReport);
    renderApp("/does-not-exist");

    await waitFor(() =>
      expect(
        screen.getByRole("heading", { name: /system status/i }),
      ).toBeInTheDocument(),
    );
  });
});
