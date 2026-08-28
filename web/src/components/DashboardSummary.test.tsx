import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { expect, it } from "vitest";

import type { Acquisition } from "../api/acquisitionTypes";
import type { AutomationStatus } from "../api/automationTypes";
import type { Execution } from "../api/executionTypes";
import type { InventoryStatus } from "../api/inventoryTypes";
import type { Rollback } from "../api/rollbackTypes";
import { DashboardSummary, RecentActivity, SystemStrip } from "./DashboardSummary";

/**
 * The dashboard's summary, system strip and activity preview (Phase 5).
 *
 * # What this defends
 *
 * The dashboard led with three attention lists and four diagnostic panels, and
 * read as a report. It now answers four questions and links into the workspaces
 * Phases 2-4 built.
 *
 * The properties that matter are about NOT reimplementing those workspaces:
 *
 *   - every number is a field a server response carried;
 *   - the activity preview uses Phase 4's joiner, so a recovered failure reads
 *     as recovered here too;
 *   - the cards lead to the page that owns the subject, deep-linked where the
 *     existing routing supports it.
 */

function inventory(counts: Partial<InventoryStatus["counts"]> = {}): InventoryStatus {
  return {
    counts: {
      containers: 10, absent: 0, running: 10, stopped: 0, paused: 0,
      restarting: 0, healthy: 10, unhealthy: 0, images: 4, networks: 2,
      ...counts,
    },
  } as unknown as InventoryStatus;
}

function automation(overrides: Partial<AutomationStatus> = {}): AutomationStatus {
  return {
    enabled: true, running: false, policies: 1, enabledPolicies: 1,
    actingPolicies: 1, pausedContainers: 0, awaitingApproval: 0,
    ...overrides,
  } as AutomationStatus;
}

const renderWith = (node: React.ReactNode) =>
  render(<MemoryRouter>{node}</MemoryRouter>);

function card(label: string): HTMLElement {
  return screen.getByText(label).closest("a") as HTMLElement;
}

// --------------------------------------------------------------- summary --

it("answers the four questions from server-supplied counts", () => {
  renderWith(
    <DashboardSummary
      inventory={inventory({ running: 10, unhealthy: 3 })}
      plans={{ actionable: 15, needsReview: 3 } as never}
      automation={automation()}
      attentionCount={2}
    />,
  );

  expect(within(card("Containers")).getByText("10")).toBeInTheDocument();
  expect(within(card("Containers")).getByText("3 unhealthy")).toBeInTheDocument();
  expect(within(card("Updates")).getByText("15")).toBeInTheDocument();
  expect(within(card("Updates")).getByText("3 need review")).toBeInTheDocument();
  expect(within(card("Automation")).getByText("On")).toBeInTheDocument();
  // The unit is part of the value: "2" alone reads as a container count.
  expect(within(card("Needs attention")).getByText("2 issues")).toBeInTheDocument();
});

it("deep-links review-required updates to the review tab", () => {
  renderWith(
    <DashboardSummary
      inventory={inventory()}
      plans={{ actionable: 4, needsReview: 3 } as never}
      automation={automation()}
      attentionCount={0}
    />,
  );
  expect(card("Updates")).toHaveAttribute("href", "/updates?tab=review");
});

it("links to Updates plainly when nothing needs review", () => {
  renderWith(
    <DashboardSummary
      inventory={inventory()}
      plans={{ actionable: 4, needsReview: 0 } as never}
      automation={automation()}
      attentionCount={0}
    />,
  );
  expect(card("Updates")).toHaveAttribute("href", "/updates");
});

it("distinguishes an engine that is off from policies that only watch", () => {
  // The same distinction Phase 3 makes on the page this card links to: two
  // situations that both change nothing, with opposite remedies.
  const { unmount } = renderWith(
    <DashboardSummary
      inventory={inventory()}
      plans={null}
      automation={automation({ enabled: false })}
      attentionCount={0}
    />,
  );
  expect(within(card("Automation")).getByText("Off")).toBeInTheDocument();
  unmount();

  renderWith(
    <DashboardSummary
      inventory={inventory()}
      plans={null}
      automation={automation({ actingPolicies: 0 })}
      attentionCount={0}
    />,
  );
  expect(within(card("Automation")).getByText("Watching")).toBeInTheDocument();
});

it("reports paused containers on the automation card", () => {
  renderWith(
    <DashboardSummary
      inventory={inventory()}
      plans={null}
      automation={automation({ pausedContainers: 1 })}
      attentionCount={1}
    />,
  );
  expect(within(card("Automation")).getByText("1 paused")).toBeInTheDocument();
  expect(card("Automation")).toHaveAttribute("href", "/automation");
});

it("says nothing needs you rather than leaving the card blank", () => {
  renderWith(
    <DashboardSummary
      inventory={inventory()}
      plans={null}
      automation={automation()}
      attentionCount={0}
    />,
  );
  expect(within(card("Needs attention")).getByText("nothing right now")).toBeInTheDocument();
});

// ---------------------------------------------------------------- system --

it("reports the engine and when HarborMaster last looked", () => {
  renderWith(
    <SystemStrip automation={automation({ running: true })} lastCheck="2026-08-01T10:00:00Z" />,
  );
  const strip = screen.getByTestId("dashboard-system");
  expect(within(strip).getByText("Running a pass")).toBeInTheDocument();
  expect(within(strip).getByText(/2026/)).toBeInTheDocument();
});

it("does not claim an engine state it was not told", () => {
  renderWith(<SystemStrip automation={null} />);
  const strip = screen.getByTestId("dashboard-system");
  expect(within(strip).getByText("unknown")).toBeInTheDocument();
});

// -------------------------------------------------------------- activity --

function acquisition(overrides: Record<string, unknown> = {}) {
  return {
    acquisitionId: "acq_1", planId: "plan_1", containerId: "c1",
    containerName: "redis", state: "succeeded",
    requestedAt: "2026-08-01T10:00:00Z", completedAt: "2026-08-01T10:01:00Z",
    expiresAt: "2026-09-01T00:00:00Z",
    target: { repository: "redis", reference: "redis:7.2.5" },
    ...overrides,
  } as unknown as Acquisition;
}

function execution(overrides: Record<string, unknown> = {}) {
  return {
    executionId: "exec_1", acquisitionId: "acq_1", planId: "plan_1",
    containerId: "c1", containerName: "redis", oldImage: "redis:7.2.4",
    target: { repository: "redis", reference: "redis:7.2.5" },
    state: "succeeded", originalRemoved: false, verification: {},
    requestedAt: "2026-08-01T10:02:00Z", completedAt: "2026-08-01T10:03:00Z",
    expiresAt: "2026-09-01T00:00:00Z",
    ...overrides,
  } as unknown as Execution;
}

function rollback(overrides: Record<string, unknown> = {}) {
  return {
    rollbackId: "rbk_1", executionId: "exec_1", containerName: "redis",
    originalId: "c1", parkedName: "redis.hm-old", replacementId: "c2",
    state: "succeeded", verification: {},
    requestedAt: "2026-08-01T10:04:00Z", completedAt: "2026-08-01T10:05:00Z",
    expiresAt: "2026-09-01T00:00:00Z",
    ...overrides,
  } as unknown as Rollback;
}

it("previews recent activity using the Activity model", () => {
  renderWith(
    <RecentActivity acquisitions={[acquisition()]} executions={[execution()]} rollbacks={[]} />,
  );
  const feed = screen.getByTestId("dashboard-activity");
  expect(within(feed).getByText("redis")).toBeInTheDocument();
  expect(within(feed).getByText("Updated")).toBeInTheDocument();
  expect(within(feed).getByText(/redis:7\.2\.4 → redis:7\.2\.5/)).toBeInTheDocument();
});

it("shows a recovered failure as recovered, exactly as Activity does", () => {
  renderWith(
    <RecentActivity
      acquisitions={[acquisition()]}
      executions={[execution({ state: "failed" })]}
      rollbacks={[rollback()]}
    />,
  );
  expect(within(screen.getByTestId("dashboard-activity")).getByText("Recovered")).toBeInTheDocument();
});

it("caps the preview and offers the full workspace", () => {
  const many = Array.from({ length: 9 }, (_, i) =>
    execution({
      executionId: `exec_${i}`,
      acquisitionId: `acq_${i}`,
      containerName: `c${i}`,
      requestedAt: `2026-08-0${(i % 9) + 1}T10:00:00Z`,
    }),
  );
  renderWith(<RecentActivity acquisitions={[]} executions={many} rollbacks={[]} />);

  const feed = screen.getByTestId("dashboard-activity");
  expect(within(feed).getAllByRole("listitem")).toHaveLength(5);
  expect(within(feed).getByRole("link", { name: /view all activity/i })).toHaveAttribute(
    "href",
    "/activity",
  );
});

it("says nothing has happened rather than showing an empty list", () => {
  renderWith(<RecentActivity acquisitions={[]} executions={[]} rollbacks={[]} />);
  expect(
    within(screen.getByTestId("dashboard-activity")).getByText(/has not changed anything yet/i),
  ).toBeInTheDocument();
});
