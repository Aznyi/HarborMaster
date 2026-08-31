import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { expect, it, vi } from "vitest";

import { SimpleUpdatesPanel } from "./SimpleUpdatesPanel";
import {
  simpleUpdatesStatus,
  simpleUpdatesWill,
  simpleUpdatesWillNot,
} from "../api/simpleUpdates";
import type { SimpleUpdatesState, UpdatePolicy } from "../api/automationTypes";

/**
 * The automatic-updates switch (C1).
 *
 * # What these tests defend
 *
 * The switch tells an operator that HarborMaster will stop and replace running
 * containers without asking again. Every claim it makes has to be true, and the
 * ones that would be dangerous to get wrong are asserted individually:
 *
 *   - a deployment that cannot run automation is not offered a toggle;
 *   - a viewer sees the state and no controls;
 *   - turning it on is confirmed BEFORE anything is called;
 *   - the confirmation does not bury what it means;
 *   - turning it off promises to keep user policies, and says so.
 */

const managedPolicy: UpdatePolicy = {
  policyId: "upd_ffffffffffffffffffff",
  name: "Automatic updates",
  enabled: true,
  priority: 0,
  scope: "allEligible",
  selector: {},
  strategy: "patch",
  minimumRecommendation: "proceedWithCaution",
  mode: "automatic",
  window: { alwaysOpen: true },
  limits: {},
  failure: { autoRollback: true, pauseAfterFailures: 2, pauseWindowHours: 24, cooldownHours: 0, maxRetries: 1 },
  archived: false,
  createdAt: "2026-09-01T12:00:00Z",
  updatedAt: "2026-09-01T12:00:00Z",
} as UpdatePolicy;

function state(overrides: Partial<SimpleUpdatesState> = {}): SimpleUpdatesState {
  return {
    enabled: false,
    configured: false,
    engineEnabled: true,
    engineVariable: "HARBORMASTER_AUTOMATION_ENABLED",
    ...overrides,
  };
}

const on = () =>
  state({ enabled: true, configured: true, policy: managedPolicy, warnings: ["this policy may update every eligible container on this host without asking"] });

function renderPanel(props: Partial<Parameters<typeof SimpleUpdatesPanel>[0]> = {}) {
  const onEnable = vi.fn();
  const onDisable = vi.fn();
  render(
    <MemoryRouter>
      <SimpleUpdatesPanel
        state={state()}
        mayManage
        busy={false}
        error={null}
        onEnable={onEnable}
        onDisable={onDisable}
        {...props}
      />
    </MemoryRouter>,
  );
  return { onEnable, onDisable };
}

// ------------------------------------------------------------- the states --

it("offers one control on a fresh installation", () => {
  renderPanel();
  expect(screen.getByRole("heading", { name: /automatic updates are off/i })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /turn on automatic updates/i })).toBeInTheDocument();
  // The advanced surface stays reachable, but it is not where a new operator
  // is sent for the common case.
  expect(screen.getByRole("link", { name: /manage update policies/i })).toHaveAttribute(
    "href",
    "/update-policies",
  );
});

it("does not offer a toggle when the deployment cannot run automation", () => {
  // A control that cannot work is a control that lies. The panel names the
  // variable the SERVER reported instead.
  renderPanel({ state: state({ engineEnabled: false }) });

  expect(screen.getByRole("heading", { name: /unavailable/i })).toBeInTheDocument();
  expect(
    screen.queryByRole("button", { name: /turn on automatic updates/i }),
  ).not.toBeInTheDocument();
  expect(screen.getByText(/HARBORMASTER_AUTOMATION_ENABLED=true/)).toBeInTheDocument();
  expect(screen.getByText(/restart HarborMaster/i)).toBeInTheDocument();
});

it("distinguishes the engine being off from the switch being off", () => {
  expect(simpleUpdatesStatus(state({ engineEnabled: false }))).toBe("engineOff");
  expect(simpleUpdatesStatus(state())).toBe("off");
  expect(simpleUpdatesStatus(state({ configured: true }))).toBe("turnedOff");
  expect(simpleUpdatesStatus(on())).toBe("on");
});

it("shows a viewer the state and no controls", () => {
  renderPanel({ state: on(), mayManage: false });

  expect(screen.getByRole("heading", { name: /automatic updates are on/i })).toBeInTheDocument();
  expect(
    screen.queryByRole("button", { name: /turn off automatic updates/i }),
  ).not.toBeInTheDocument();
  expect(screen.getByText(/needs the automation management permission/i)).toBeInTheDocument();
});

// --------------------------------------------------------- turning it on --

it("confirms before calling anything", async () => {
  const user = userEvent.setup();
  const { onEnable } = renderPanel();

  await user.click(screen.getByRole("button", { name: /turn on automatic updates/i }));

  // The dialog is open and NOTHING has been called yet.
  const dialog = screen.getByRole("dialog", { name: /turn on automatic updates\?/i });
  expect(onEnable).not.toHaveBeenCalled();

  // It says what it means, without burying it.
  expect(within(dialog).getByText(/stop and recreate eligible containers/i)).toBeInTheDocument();
  expect(within(dialog).getByText(/without\s+asking again/i)).toBeInTheDocument();
  // And it is honest that confirming changes nothing immediately.
  expect(within(dialog).getByText(/next scheduled pass is what acts/i)).toBeInTheDocument();

  await user.click(within(dialog).getByRole("button", { name: /turn on automatic updates/i }));
  expect(onEnable).toHaveBeenCalledTimes(1);
});

it("can be cancelled without calling anything", async () => {
  const user = userEvent.setup();
  const { onEnable } = renderPanel();

  await user.click(screen.getByRole("button", { name: /turn on automatic updates/i }));
  await user.click(screen.getByRole("button", { name: /cancel/i }));

  expect(onEnable).not.toHaveBeenCalled();
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("shows the server's own warnings rather than copy written here", async () => {
  const user = userEvent.setup();
  renderPanel({
    state: state({ warnings: ["a warning only the server could have produced"] }),
  });

  await user.click(screen.getByRole("button", { name: /turn on automatic updates/i }));
  expect(
    screen.getByText(/a warning only the server could have produced/),
  ).toBeInTheDocument();
});

// -------------------------------------------------------- turning it off --

it("promises to keep user policies, and says so before acting", async () => {
  const user = userEvent.setup();
  const { onDisable } = renderPanel({ state: on() });

  await user.click(screen.getByRole("button", { name: /turn off automatic updates/i }));

  const dialog = screen.getByRole("dialog", { name: /turn off automatic updates\?/i });
  expect(onDisable).not.toHaveBeenCalled();
  expect(within(dialog).getByText(/policies you wrote are kept/i)).toBeInTheDocument();
  expect(within(dialog).getByText(/history, approvals and paused containers are kept/i)).toBeInTheDocument();
  expect(within(dialog).getByText(/no container is stopped, started or recreated/i)).toBeInTheDocument();

  await user.click(within(dialog).getByRole("button", { name: /turn off automatic updates/i }));
  expect(onDisable).toHaveBeenCalledTimes(1);
});

// ---------------------------------------------------- effective behaviour --

it("describes the effective behaviour from the stored policy", () => {
  renderPanel({ state: on() });

  // The ceiling comes from the policy the server stored, not from a constant
  // here, so a change to the compiled policy shows up rather than being
  // contradicted.
  expect(screen.getByText(/up to a patch version, and no further/i)).toBeInTheDocument();
  expect(screen.getByText(/verify the replacement's health/i)).toBeInTheDocument();
});

it("states the gates it does not bypass", () => {
  renderPanel({ state: on() });

  for (const claim of [
    /flagged for review/i,
    /io\.harbormaster\.update\.enabled=false/,
    /paused/i,
    /refuses to replace its own container/i,
    /dependencies are ready/i,
    /maintenance window/i,
    /takes precedence/i,
  ]) {
    expect(screen.getByText(claim)).toBeInTheDocument();
  }
});

it("keeps automatic rollback a claim about AUTOMATION only", () => {
  // The manual Apply-update dialog says rollback is NOT automatic, and that is
  // also true. This list is about the automated path, where the managed
  // policy's failure.autoRollback is real. Neither sentence may be copied onto
  // the other, so this pins which list the claim lives in.
  const will = simpleUpdatesWill(on());
  const willNot = simpleUpdatesWillNot();

  expect(will.some((line) => /put the original back automatically/i.test(line))).toBe(true);
  expect(willNot.some((line) => /rollback/i.test(line))).toBe(false);
});

it("reports the policies that outrank it", () => {
  renderPanel({
    state: state({
      enabled: true,
      configured: true,
      policy: managedPolicy,
      overriddenBy: [
        { policyId: "upd_abc", name: "my careful rule", scope: "selector", priority: 0, mode: "observe" },
      ],
    }),
  });

  expect(screen.getByText(/my careful rule/)).toBeInTheDocument();
  expect(screen.getByText(/takes precedence over automatic updates/i)).toBeInTheDocument();
});
