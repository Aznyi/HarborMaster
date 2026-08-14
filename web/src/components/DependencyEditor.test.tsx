import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import axe from "axe-core";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { WorkloadDependency } from "../api/dependencyTypes";
import { DependencyDeleteConfirm, DependencyEditor } from "./DependencyEditor";

/**
 * The ordering editor and the removal confirmation.
 *
 * The editor's job is to make an operator's assertion legible before they
 * commit to it, so the tests are mostly about what it SAYS. The confirmation's
 * job is to state a consequence, so the same applies.
 */

// ContainerPicker fetches the inventory. Replaced with a plain control that
// keeps the contract -- selected names in, names out -- so these tests exercise
// the editor rather than the picker, which has its own coverage.
//
// # The mock must not be MORE accessible than the real control
//
// It used to name its input from `describedBy` alone, which produced the tidy
// accessible name "Dependent container" -- a name the real picker never
// produced, because its search box is also labelled "Search the inventory". The
// tests passed and the page they were meant to cover could not find the field.
// This mock now reproduces the real name exactly, so a query that works here
// works against the real component too.
vi.mock("./ContainerPicker", () => ({
  ContainerPicker: ({
    selected,
    onChange,
    describedBy,
  }: {
    selected: string[];
    onChange: (names: string[]) => void;
    describedBy?: string;
  }) => (
    <>
      <input
        aria-labelledby={describedBy ? `${describedBy} ${describedBy}-search` : undefined}
        value={selected[0] ?? ""}
        onChange={(event) => onChange(event.target.value ? [event.target.value] : [])}
      />
      {/* Derived from describedBy, which is a useId: two pickers on one form
          must not both mint the same id, or the aria-labelledby references
          collide and axe reports it as critical. */}
      <span id={describedBy ? `${describedBy}-search` : undefined}>
        Search the inventory
      </span>
    </>
  ),
}));

// Typed explicitly: `npm run build` typechecks test files, and an untyped
// vi.fn() does not satisfy the prop signature.
const makeOnCreate = () => vi.fn<(dependent: string, dependency: string) => void>();

function edge(overrides: Partial<WorkloadDependency> = {}): WorkloadDependency {
  return {
    dependencyId: "dep_0123456789abcdef0123",
    dependent: "api",
    dependency: "postgres",
    source: "operator",
    kind: "Application ordering",
    origin: "Configured by you",
    hard: false,
    why: "an operator recorded that the dependent needs this container",
    deletable: true,
    ...overrides,
  };
}

describe("DependencyEditor", () => {
  let onCreate: ReturnType<typeof makeOnCreate>;

  beforeEach(() => {
    onCreate = makeOnCreate();
  });

  it("asks for both ends before it will say anything", () => {
    render(<DependencyEditor onCreate={onCreate} />);

    expect(
      screen.getByText(/Choose both containers to see what this ordering will do/i),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Record ordering" })).toBeDisabled();
  });

  it("previews the ordering in plain English before saving", async () => {
    const user = userEvent.setup();
    render(<DependencyEditor onCreate={onCreate} />);

    await user.type(screen.getByLabelText(/Dependent container/), "api");
    await user.type(screen.getByLabelText(/Depends on/), "postgres");

    expect(
      screen.getByText(
        "Update postgres before api. If postgres cannot be safely updated or verified, api will wait.",
      ),
    ).toBeInTheDocument();
  });

  it("says what an ordering will NOT do", async () => {
    const user = userEvent.setup();
    render(<DependencyEditor onCreate={onCreate} />);

    await user.type(screen.getByLabelText(/Dependent container/), "api");
    await user.type(screen.getByLabelText(/Depends on/), "postgres");

    // Each of these corresponds to something a reasonable operator might
    // otherwise assume.
    expect(screen.getByText(/does not change networking/i)).toBeInTheDocument();
    expect(
      screen.getByText(/does not create a Docker relationship/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/does not make either container eligible for updates/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/If an update policy excludes postgres/i),
    ).toBeInTheDocument();
  });

  it("refuses a self-dependency in the form", async () => {
    const user = userEvent.setup();
    render(<DependencyEditor onCreate={onCreate} />);

    await user.type(screen.getByLabelText(/Dependent container/), "api");
    await user.type(screen.getByLabelText(/Depends on/), "api");

    expect(screen.getByRole("alert")).toHaveTextContent(
      /A container cannot depend on itself/i,
    );
    expect(screen.getByRole("button", { name: "Record ordering" })).toBeDisabled();
    expect(onCreate).not.toHaveBeenCalled();
  });

  it("submits the two names once both are chosen", async () => {
    const user = userEvent.setup();
    render(<DependencyEditor onCreate={onCreate} />);

    await user.type(screen.getByLabelText(/Dependent container/), "api");
    await user.type(screen.getByLabelText(/Depends on/), "postgres");
    await user.click(screen.getByRole("button", { name: "Record ordering" }));

    expect(onCreate).toHaveBeenCalledWith("api", "postgres");
  });

  it("renders a server refusal as an alert", () => {
    render(
      <DependencyEditor
        onCreate={onCreate}
        error="this relationship would make the containers depend on each other in a loop"
      />,
    );

    expect(screen.getByRole("alert")).toHaveTextContent(/depend on each other in a loop/i);
  });

  /*
   * A layout guard jsdom cannot express, pinned structurally.
   *
   * A fieldset carries `min-width: min-content` from the user agent, so it
   * refuses to be narrower than its widest unwrappable child. The container
   * picker truncates a selected name, truncation needs `white-space: nowrap`,
   * and the two together made a long container name push the page 353px past a
   * 390px viewport. Real Chromium found it during responsive acceptance.
   *
   * jsdom has no layout, so no rendering test here can catch a regression. The
   * class is asserted instead: it is the fix, and losing it is the regression.
   */
  it("keeps both ends narrowable, so a long name cannot widen the page", () => {
    const { container } = render(<DependencyEditor onCreate={onCreate} />);

    const fieldsets = container.querySelectorAll("fieldset");
    expect(fieldsets).toHaveLength(2);
    for (const fieldset of fieldsets) {
      expect(fieldset.className).toContain("min-w-0");
    }
  });

  it("has no serious or critical accessibility violations", async () => {
    const { container } = render(
      <DependencyEditor onCreate={onCreate} error="a refusal" />,
    );

    const results = await axe.run(container);
    const serious = results.violations.filter(
      (violation) => violation.impact === "serious" || violation.impact === "critical",
    );
    expect(serious.map((violation) => violation.id)).toEqual([]);
  });
});

describe("DependencyDeleteConfirm", () => {
  it("states the consequence rather than asking if the operator is sure", () => {
    render(
      <DependencyDeleteConfirm edge={edge()} onCancel={vi.fn()} onConfirm={vi.fn()} />,
    );

    expect(
      screen.getByText("api will no longer wait for postgres before updating."),
    ).toBeInTheDocument();
    expect(
      screen.getByText("This does not modify either container."),
    ).toBeInTheDocument();
  });

  it("is a labelled dialog", () => {
    render(
      <DependencyDeleteConfirm edge={edge()} onCancel={vi.fn()} onConfirm={vi.fn()} />,
    );

    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveAccessibleName("Remove dependency?");
    expect(dialog).toHaveAttribute("aria-modal", "true");
  });

  it("cancels and confirms through the keyboard", async () => {
    const user = userEvent.setup();
    const onCancel = vi.fn();
    const onConfirm = vi.fn();
    render(
      <DependencyDeleteConfirm
        edge={edge()}
        onCancel={onCancel}
        onConfirm={onConfirm}
      />,
    );

    // Focus is already inside the dialog, on Cancel: the dialog moves it there
    // on mount, so a keyboard operator is not left on the control behind it.
    expect(screen.getByRole("button", { name: "Cancel" })).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(onCancel).toHaveBeenCalled();

    await user.tab();
    await user.keyboard("{Enter}");
    expect(onConfirm).toHaveBeenCalled();
  });

  it("cancels on Escape", async () => {
    const user = userEvent.setup();
    const onCancel = vi.fn();
    render(
      <DependencyDeleteConfirm
        edge={edge()}
        onCancel={onCancel}
        onConfirm={vi.fn()}
      />,
    );

    await user.keyboard("{Escape}");
    expect(onCancel).toHaveBeenCalled();
  });

  it("has no serious or critical accessibility violations", async () => {
    const { container } = render(
      <DependencyDeleteConfirm edge={edge()} onCancel={vi.fn()} onConfirm={vi.fn()} />,
    );

    const results = await axe.run(container);
    const serious = results.violations.filter(
      (violation) => violation.impact === "serious" || violation.impact === "critical",
    );
    expect(serious.map((violation) => violation.id)).toEqual([]);
  });
});
