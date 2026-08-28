import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { ADVANCED_NAV, PRIMARY_NAV } from "../components/AppShell";
import { Settings } from "./Settings";
import { TestSessionProvider, testSession, testUser } from "../test/session";
import type { HealthReport } from "../api/types";
import type { ResourceState } from "../hooks/useApiResource";

/**
 * Settings as a control centre, and the settled vocabulary (Phase 6).
 *
 * # What this defends
 *
 * Settings reported what the running process can do -- accurately -- and named
 * no route to the things people open Settings to find. It now signposts them,
 * and the properties that matter are about it staying a signpost:
 *
 *   - it links to the pages that own each subject; it does not reproduce them,
 *     because two places to change one thing is worse than one place to find it;
 *   - each link is filtered by the permission its destination already needs, so
 *     Settings never offers a page that would refuse the operator;
 *   - one subject keeps one heading.
 */

const originalFetch = globalThis.fetch;

function health(): ResourceState<HealthReport> {
  return {
    status: "ready",
    data: {
      status: "healthy",
      database: { status: "up" },
      docker: { status: "up" },
      events: { status: "up" },
      checkedAt: "2026-08-01T00:00:00Z",
      uptimeSeconds: 10,
      features: {
        inventory: true, events: true, snapshots: true, drift: true,
        policy: true, planner: true, imageIntel: true, acquisition: true,
        execution: true, rollback: true, automation: true,
        notifications: true, notificationsAllowPrivate: false,
      },
    },
    error: null,
    refresh: () => {},
    refreshing: false,
  } as unknown as ResourceState<HealthReport>;
}

function renderSettings(role: "viewer" | "operator" | "administrator" = "administrator") {
  return render(
    <TestSessionProvider session={testSession({ user: testUser(role) })}>
      <MemoryRouter>
        <Settings health={health()} />
      </MemoryRouter>
    </TestSessionProvider>,
  );
}

beforeEach(() => {
  globalThis.fetch = vi.fn(async () =>
    new Response(JSON.stringify({ version: "dev", commit: "unknown" }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    }),
  ) as typeof fetch;
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

// --------------------------------------------------------- control centre --

it("names where access is managed without reproducing it", async () => {
  renderSettings();

  const access = await screen.findByTestId("settings-access");
  expect(within(access).getByRole("link", { name: /manage accounts/i })).toHaveAttribute(
    "href",
    "/users",
  );
  expect(within(access).getByRole("link", { name: /your account/i })).toHaveAttribute(
    "href",
    "/account",
  );
  // A signpost, not an editor: no account form appears here.
  expect(within(access).queryByRole("textbox")).not.toBeInTheDocument();
});

it("names the security destinations", async () => {
  renderSettings();

  const security = await screen.findByTestId("settings-security");
  expect(within(security).getByRole("link", { name: /security audit/i })).toHaveAttribute(
    "href",
    "/audit",
  );
  expect(
    within(security).getByRole("link", { name: /compliance policies/i }),
  ).toHaveAttribute("href", "/policies");
  expect(
    within(security).getByRole("link", { name: /compliance findings/i }),
  ).toHaveAttribute("href", "/compliance");
});

it("keeps one heading per subject", async () => {
  renderSettings();
  await screen.findByTestId("settings-access");

  // Notifications reports its capability state and carries the way in, rather
  // than existing twice.
  expect(screen.getAllByRole("heading", { name: "Notifications" })).toHaveLength(1);
  expect(
    screen.getByRole("link", { name: /configure notifications/i }),
  ).toHaveAttribute("href", "/notifications");
});

it("offers a viewer only what their role can reach", async () => {
  renderSettings("viewer");
  await screen.findByTestId("settings-access");

  // A viewer holds no user:manage, so account administration is not offered.
  expect(
    screen.queryByRole("link", { name: /manage accounts/i }),
  ).not.toBeInTheDocument();
  // Their own account always is.
  expect(screen.getByRole("link", { name: /your account/i })).toBeInTheDocument();
});

it("still reports what the deployment may actually do", async () => {
  // The capability report is the reason this page existed and was not replaced.
  renderSettings();
  expect(await screen.findByText("Observed state")).toBeInTheDocument();
  expect(screen.getByText("What this deployment may do to the host")).toBeInTheDocument();
});

// ----------------------------------------------------------- terminology --

it("names the specialised tools in the settled vocabulary", () => {
  const labels = ADVANCED_NAV.map((item) => item.label);

  // Renamed to match what the container page has called them since Phase 5.
  expect(labels).toContain("Restore points");
  expect(labels).toContain("Configuration changes");
  expect(labels).toContain("Update order");

  // The record-oriented names those pages replaced are gone from the sidebar.
  expect(labels).not.toContain("Snapshots");
  expect(labels).not.toContain("Drift");
  expect(labels).not.toContain("Update dependencies");

  // Qualified where the bare word was ambiguous next to another entry.
  expect(labels).toContain("Compliance policies");
  expect(labels).toContain("Docker events");
});

it("keeps the routes those labels point at unchanged", () => {
  // Renaming a label is presentation. Renaming a URL breaks bookmarks and buys
  // nothing, so the paths are exactly what they were.
  const byLabel = Object.fromEntries(ADVANCED_NAV.map((i) => [i.label, i.path]));
  expect(byLabel["Restore points"]).toBe("/snapshots");
  expect(byLabel["Configuration changes"]).toBe("/drift");
  expect(byLabel["Update order"]).toBe("/dependencies");
});

it("leaves the six primary destinations untouched", () => {
  expect(PRIMARY_NAV.map((item) => item.label)).toEqual([
    "Dashboard",
    "Containers",
    "Updates",
    "Automation",
    "Activity",
    "Settings",
  ]);
});
