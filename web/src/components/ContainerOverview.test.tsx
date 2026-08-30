import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { expect, it } from "vitest";

import type { ContainerDetail } from "../api/inventoryTypes";
import { ContainerOverview } from "./ContainerOverview";

/**
 * The container overview (Phase 5).
 *
 * # What this defends
 *
 * Understanding one container used to mean knowing which of fifteen tabs held
 * the answer, most named after a Docker config section or a HarborMaster record
 * type. This answers the operator's questions in one place -- and, critically,
 * from the ONE request the page already made: every value below is read off
 * `ContainerDetail.attention`, the projection the payload already carries.
 *
 * The properties that matter:
 *
 *   - it reaches no verdicts of its own. The recommendation, the automation
 *     state and the change count are read, never computed;
 *   - "not established" never renders as a reassuring answer;
 *   - it links to the workspace that owns each action rather than growing a
 *     second copy of the control.
 */

function detail(attention?: Partial<ContainerDetail["attention"]>): ContainerDetail {
  return {
    overview: {
      id: "abcdef0123456789",
      name: "hm13-cache",
      image: { raw: "redis:7.2.4", repository: "redis", tag: "7.2.4" },
      state: "running",
      health: "healthy",
    },
    attention: attention
      ? ({ state: "notChecked", trackingKnown: true, awaitingApproval: false,
           automationPaused: false, openViolations: 0, openDrift: 0,
           ...attention } as ContainerDetail["attention"])
      : undefined,
    state: {},
    process: {},
    environment: [],
    labels: [],
    ports: [],
    mounts: [],
    networks: [],
    resources: {},
    security: {},
    logging: {},
    compose: {},
    harbormaster: {},
    warnings: [],
  } as unknown as ContainerDetail;
}

function renderOverview(d: ContainerDetail) {
  return render(
    <MemoryRouter>
      <ContainerOverview detail={d} />
    </MemoryRouter>,
  );
}

function panel(title: string): HTMLElement {
  return screen.getByRole("heading", { name: title }).closest("section") as HTMLElement;
}

// --------------------------------------------------------------- basics --

it("shows what the container is running", () => {
  renderOverview(detail());
  expect(within(panel("Image")).getByText("redis:7.2.4")).toBeInTheDocument();
});

it("names what the container follows for updates", () => {
  renderOverview(detail({ tracking: "redis:7.2" }));
  expect(within(panel("Image")).getByText("redis:7.2")).toBeInTheDocument();
});

// --------------------------------------------------------------- update --

it("says up to date when HarborMaster compared and found nothing newer", () => {
  renderOverview(detail({ state: "upToDate", updateType: "none" }));
  expect(within(panel("Update")).getByText("Up to date")).toBeInTheDocument();
});

// The evidence-gap rule, on this panel.
//
// `internal/domain/attention.go`: "Absent evidence produces
// AttentionNotChecked, never AttentionUpToDate." The panel used to render
// "Up to date" for an ABSENT update type -- documented as "absent when no
// assessment exists" -- so a container HarborMaster had never looked at was
// reported as one it had looked at and cleared.
it("does not call an unassessed container up to date", () => {
  renderOverview(detail({ state: "notChecked" }));

  const update = panel("Update");
  expect(within(update).queryByText("Up to date")).not.toBeInTheDocument();
  expect(within(update).getByText("Not checked")).toBeInTheDocument();
});

// The three other no-verdict states keep their own words rather than
// collapsing into one another or into a reassurance.
it.each([
  ["notTracked", "Not tracked"],
  ["cannotAdvise", "Cannot determine"],
] as const)("renders %s as %s rather than up to date", (state, label) => {
  renderOverview(detail({ state, updateType: "unknown" }));

  const update = panel("Update");
  expect(within(update).getByText(label)).toBeInTheDocument();
  expect(within(update).queryByText("Up to date")).not.toBeInTheDocument();
  expect(within(update).queryByText("Update available")).not.toBeInTheDocument();
});

it("shows an available update and sends the operator to Updates", () => {
  renderOverview(
    detail({
      updateType: "patch",
      recommendation: "proceed",
      proposedImage: "redis:7.2.5",
    }),
  );

  const update = panel("Update");
  expect(within(update).getByText("Update available")).toBeInTheDocument();
  expect(within(update).getByText(/redis:7\.2\.5/)).toBeInTheDocument();
  expect(within(update).getByRole("link", { name: /view in updates/i })).toHaveAttribute(
    "href",
    "/updates",
  );
});

it("sends a review-required update to the review tab", () => {
  renderOverview(
    detail({ updateType: "major", recommendation: "manualReview", proposedImage: "redis:8.0.0" }),
  );

  const update = panel("Update");
  expect(within(update).getByText("Review required")).toBeInTheDocument();
  // Phase 2's own query value, so the link lands where the work is.
  expect(within(update).getByRole("link", { name: /review update/i })).toHaveAttribute(
    "href",
    "/updates?tab=review",
  );
});

// ----------------------------------------------------------- automation --

it("says when automation is paused for this container", () => {
  renderOverview(detail({ automationPaused: true }));
  const automation = panel("Automation");
  expect(within(automation).getByText("Paused")).toBeInTheDocument();
  expect(within(automation).getByText(/after failures/i)).toBeInTheDocument();
});

it("says when a decision is waiting for a person", () => {
  renderOverview(detail({ awaitingApproval: true }));
  expect(within(panel("Automation")).getByText("Needs approval")).toBeInTheDocument();
});

it("explains a container held by a dependency, and names it", () => {
  renderOverview(
    detail({
      dependencyKnown: true,
      dependencyState: "dependencyWaiting",
      dependencyBlockedBy: "database",
    }),
  );
  const automation = panel("Automation");
  expect(within(automation).getByText("Waiting on a dependency")).toBeInTheDocument();
  expect(within(automation).getByText(/database/)).toBeInTheDocument();
});

// -------------------------------------------------------- configuration --

it("reports configuration changes as changes, not as drift", () => {
  renderOverview(detail({ openDrift: 3 }));

  const config = panel("Configuration");
  expect(within(config).getByText("Changed")).toBeInTheDocument();
  expect(within(config).getByText(/3 changes since the recorded configuration/i)).toBeInTheDocument();
  expect(within(config).getByRole("link", { name: /review changes/i })).toHaveAttribute(
    "href",
    "/drift/container/abcdef0123456789",
  );
});

it("says plainly when nothing has changed", () => {
  renderOverview(detail({ openDrift: 0 }));
  const config = panel("Configuration");
  expect(within(config).getByText("Unchanged")).toBeInTheDocument();
  expect(within(config).queryByRole("link", { name: /review changes/i })).not.toBeInTheDocument();
});

// -------------------------------------------------------------- recent --

it("says nothing has happened when nothing has", () => {
  renderOverview(detail());
  expect(
    within(panel("Recent activity")).getByText(/has not changed this container/i),
  ).toBeInTheDocument();
});

it("renders a failure that was rolled back as recovered", () => {
  renderOverview(
    detail({
      lastUpdate: { id: "exec_1", state: "failed", needsAttention: false, at: "2026-08-01T10:00:00Z" },
      lastRollback: { id: "rbk_1", state: "succeeded", needsAttention: false, at: "2026-08-01T10:05:00Z" },
    }),
  );

  const recent = panel("Recent activity");
  expect(within(recent).getByText("Recovered")).toBeInTheDocument();
});

// ------------------------------------------------------------ ordering --

it("keeps update order to one word when the container is independent", () => {
  renderOverview(detail({ dependencyKnown: true }));
  const order = panel("Update order");
  expect(within(order).getByText("Independent.")).toBeInTheDocument();
  expect(within(order).queryByRole("link")).not.toBeInTheDocument();
});

it("names the container it waits for, and offers the dependency page", () => {
  renderOverview(detail({ dependencyKnown: true, dependencyBlockedBy: "database" }));
  const order = panel("Update order");
  expect(within(order).getByText(/database/)).toBeInTheDocument();
  expect(within(order).getByRole("link", { name: /manage dependencies/i })).toHaveAttribute(
    "href",
    "/dependencies",
  );
});

it("does not claim independence when dependencies were never established", () => {
  // `dependencyKnown: false` asserts nothing. Rendering it as "Independent"
  // would be the reassurance the subsystem exists to withhold.
  renderOverview(detail({ dependencyKnown: false }));
  const order = panel("Update order");
  expect(within(order).getByText("Not established.")).toBeInTheDocument();
  expect(within(order).queryByText("Independent.")).not.toBeInTheDocument();
});

// ------------------------------------------------------- missing input --

it("renders without a projection at all", () => {
  // A container HarborMaster has not assessed still has to render -- and must
  // say so, rather than claiming the assessment came back clean.
  renderOverview(detail());
  expect(within(panel("Update")).getByText("Not checked")).toBeInTheDocument();
  expect(within(panel("Update")).queryByText("Up to date")).not.toBeInTheDocument();
  expect(within(panel("Configuration")).getByText("Unchanged")).toBeInTheDocument();
});
