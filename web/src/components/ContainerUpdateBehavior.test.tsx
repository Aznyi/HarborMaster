import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";

import { ContainerUpdateBehaviorPanel } from "./ContainerUpdateBehavior";
import type { ContainerUpdateBehavior, UpdateBehavior } from "../api/inventoryTypes";

/**
 * How one container should be updated (C2), in the interface.
 *
 * # What these tests defend
 *
 * The control lets an operator say how HarborMaster may treat one container. The
 * ways that could go wrong are all about honesty rather than function:
 *
 *   - it must never claim the operator got a behaviour the engine will not give;
 *   - it must confirm before INCREASING what HarborMaster may do unasked, and
 *     not nag when the change is a restriction;
 *   - a viewer must see the state and no controls;
 *   - HarborMaster's own container must not be offered a control that cannot work.
 */

function state(overrides: Partial<ContainerUpdateBehavior> = {}): ContainerUpdateBehavior {
  return {
    containerName: "vaultwarden",
    effective: { known: false },
    ...overrides,
  };
}

function withRequested(behavior: UpdateBehavior): ContainerUpdateBehavior {
  return state({
    requested: { containerName: "vaultwarden", behavior },
    effective: { known: true, verdict: "update", reason: "eligible", detail: "every check passed" },
  });
}

function renderPanel(props: Partial<Parameters<typeof ContainerUpdateBehaviorPanel>[0]> = {}) {
  const onChoose = vi.fn();
  const onClear = vi.fn();
  render(
    <ContainerUpdateBehaviorPanel
      state={state()}
      mayManage
      busy={false}
      error={null}
      onChoose={onChoose}
      onClear={onClear}
      {...props}
    />,
  );
  return { onChoose, onClear };
}

// ------------------------------------------------------------- the states --

it("says a container with no choice is inheriting", () => {
  renderPanel();
  expect(screen.getByText(/inherited/i)).toBeInTheDocument();
  expect(screen.getByText(/follows whichever update policy governs it/i)).toBeInTheDocument();
  // Nothing to reset when nothing was chosen.
  expect(
    screen.queryByRole("button", { name: /use the policy default/i }),
  ).not.toBeInTheDocument();
});

it("offers the three supported behaviours and no fourth", async () => {
  const user = userEvent.setup();
  renderPanel();

  await user.click(screen.getByRole("button", { name: /change/i }));

  expect(screen.getByRole("radio", { name: /automatic/i })).toBeInTheDocument();
  expect(screen.getByRole("radio", { name: /review first/i })).toBeInTheDocument();
  expect(screen.getByRole("radio", { name: /monitor only/i })).toBeInTheDocument();
  expect(screen.getAllByRole("radio")).toHaveLength(3);
  // "Excluded" would be Monitor only under a second name.
  expect(screen.queryByRole("radio", { name: /excluded/i })).not.toBeInTheDocument();
});

it("shows a viewer the state and no controls", () => {
  renderPanel({ state: withRequested("reviewFirst"), mayManage: false });

  expect(screen.getByText("Review first")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /change/i })).not.toBeInTheDocument();
  expect(screen.getByText(/needs the automation management permission/i)).toBeInTheDocument();
});

it("does not offer a working control for HarborMaster's own container", () => {
  // The engine refuses a self-update at four independent layers. A selector
  // that cannot do what it says is worse than none.
  renderPanel({ isSelf: true });

  expect(screen.getByText("Excluded")).toBeInTheDocument();
  expect(screen.getByText(/does not update its own container/i)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /change/i })).not.toBeInTheDocument();
});

// ----------------------------------------------------- requested vs effective --

it("shows what will actually happen, and which policy decided", () => {
  // The sentence that stops the page lying. A preference may only narrow, so a
  // container an update policy holds for review stays held however this is set.
  renderPanel({
    state: state({
      requested: { containerName: "vaultwarden", behavior: "automatic" },
      effective: {
        known: true,
        verdict: "awaitingApproval",
        reason: "approvalRequired",
        detail: "the policy holds each change for a person",
        policyName: "Databases",
      },
    }),
  });

  expect(screen.getByText(/the policy holds each change for a person/i)).toBeInTheDocument();
  expect(screen.getByText(/decided by the policy/i)).toBeInTheDocument();
  expect(screen.getByText(/Databases/)).toBeInTheDocument();
});

it("claims nothing when the engine could not be consulted", () => {
  renderPanel({ state: state({ effective: { known: false } }) });
  expect(screen.getByText(/could not work out what would happen/i)).toBeInTheDocument();
});

// --------------------------------------------------------- confirmation --

it("confirms before increasing what HarborMaster may do unasked", async () => {
  const user = userEvent.setup();
  const { onChoose } = renderPanel({ state: withRequested("monitorOnly") });

  await user.click(screen.getByRole("button", { name: /change/i }));
  await user.click(screen.getByRole("radio", { name: /automatic/i }));

  const dialog = screen.getByRole("dialog");
  expect(onChoose).not.toHaveBeenCalled();
  expect(within(dialog).getByText(/stop and recreate this container without asking/i)).toBeInTheDocument();
  // And it is honest that nothing happens the instant they confirm.
  expect(within(dialog).getByText(/nothing changes on the host until the next automation pass/i)).toBeInTheDocument();

  await user.click(within(dialog).getByRole("button", { name: "Automatic" }));
  expect(onChoose).toHaveBeenCalledWith("automatic");
});

it("does not nag when the change is a restriction", async () => {
  const user = userEvent.setup();
  const { onChoose } = renderPanel({ state: withRequested("automatic") });

  await user.click(screen.getByRole("button", { name: /change/i }));
  await user.click(screen.getByRole("radio", { name: /monitor only/i }));

  // Choosing LESS automation needs no warning: it can only stop things.
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(onChoose).toHaveBeenCalledWith("monitorOnly");
});

it("can be cancelled without choosing anything", async () => {
  const user = userEvent.setup();
  const { onChoose } = renderPanel({ state: withRequested("monitorOnly") });

  await user.click(screen.getByRole("button", { name: /change/i }));
  await user.click(screen.getByRole("radio", { name: /automatic/i }));
  await user.click(screen.getByRole("button", { name: /cancel/i }));

  expect(onChoose).not.toHaveBeenCalled();
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("offers a way back to the policy default", async () => {
  const user = userEvent.setup();
  const { onClear } = renderPanel({ state: withRequested("monitorOnly") });

  await user.click(screen.getByRole("button", { name: /use the policy default/i }));
  expect(onClear).toHaveBeenCalledTimes(1);
});

it("reports a failure on the control that failed", () => {
  renderPanel({ error: "This container's update behaviour could not be changed." });
  expect(screen.getByRole("alert")).toHaveTextContent(/could not be changed/i);
});
