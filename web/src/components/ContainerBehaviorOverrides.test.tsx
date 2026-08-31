import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { expect, it } from "vitest";

import { ContainerBehaviorOverrides } from "./ContainerBehaviorOverrides";
import { ApiError } from "../api/client";
import type { ContainerBehaviorSummary } from "../api/inventoryTypes";
import type { ResourceState } from "../hooks/useApiResource";

/**
 * Which containers have their own update behaviour, in the interface (C2.2).
 *
 * # What these tests defend
 *
 * The section exists because the automation workspace described a policy and
 * called it the configuration, while any container with its own behaviour is a
 * standing exception to that policy. The ways showing it could go wrong are all
 * about honesty:
 *
 *   - it must not become a second EDITOR for state the container's page owns;
 *   - it must not claim to say what will HAPPEN, which a saved choice does not;
 *   - a failed read must not render as "nothing is configured";
 *   - a preference that outlived its container must be visible as the inert
 *     row it is, and must not be counted among running containers.
 */

function summary(overrides: Partial<ContainerBehaviorSummary> = {}): ContainerBehaviorSummary {
  return {
    items: [],
    counts: { automatic: 0, reviewFirst: 0, monitorOnly: 0 },
    total: 0,
    stale: 0,
    ...overrides,
  };
}

function resource(
  data: ContainerBehaviorSummary | null,
  error: ApiError | null = null,
): ResourceState<ContainerBehaviorSummary> {
  return {
    status: error ? "error" : data ? "ready" : "loading",
    data,
    error,
    refreshing: false,
    refresh: () => {},
  };
}

function renderSection(state: ResourceState<ContainerBehaviorSummary>) {
  return render(
    <MemoryRouter>
      <ContainerBehaviorOverrides state={state} />
    </MemoryRouter>,
  );
}

const populated = summary({
  items: [
    { containerName: "grafana", behavior: "automatic", present: true, containerId: "grafana-id" },
    { containerName: "vaultwarden", behavior: "monitorOnly", present: true, containerId: "vw-id" },
  ],
  counts: { automatic: 1, reviewFirst: 0, monitorOnly: 1 },
  total: 2,
});

// ------------------------------------------------------------ the states --

it("says plainly when no container has its own setting", () => {
  // The normal state for most installations. Not a finding, and not an empty
  // table, which reads as though something failed to load.
  renderSection(resource(summary()));

  expect(screen.getByText(/no container has its own update behaviour/i)).toBeInTheDocument();
  expect(screen.getByText(/follows whichever update policy governs it/i)).toBeInTheDocument();
});

it("counts each behaviour, and shows a real zero for the ones nobody chose", () => {
  renderSection(resource(populated));

  const section = screen.getByRole("region", { name: /containers with their own setting/i });
  const figures = section.querySelector("dl");
  if (!figures) throw new Error("the section shows no figures");

  // Every behaviour has a figure. A gap would read as a question rather than
  // as the fact that nobody chose it.
  const terms = [...figures.querySelectorAll("dt")].map((dt) => dt.textContent);
  expect(terms).toEqual(["Automatic", "Review first", "Monitor only"]);

  const values = [...figures.querySelectorAll("dd")].map((dd) => dd.textContent);
  expect(values).toEqual(["1", "0", "1"]);
});

it("does not claim to say what will happen", () => {
  // A saved choice may only make automation SAFER, so a policy can still hold a
  // container listed here as automatic. The section must say so and must not
  // present the saved value as an outcome.
  renderSection(resource(populated));

  expect(
    screen.getByText(/can only make HarborMaster more cautious/i),
  ).toBeInTheDocument();
});

// ----------------------------------------------------------- read-only --

it("offers no control that changes a behaviour", () => {
  // The container's own page owns this state. A control here would give it two
  // owners, one of which has no container in front of it.
  renderSection(resource(populated));

  expect(screen.queryAllByRole("button")).toHaveLength(0);
  expect(screen.queryAllByRole("radio")).toHaveLength(0);
  expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
});

it("sends an operator to the container's own page to change one", () => {
  renderSection(resource(populated));

  const link = screen.getByRole("link", { name: "grafana" });
  expect(link).toHaveAttribute("href", "/containers/grafana-id");
  expect(screen.getByRole("link", { name: /view containers/i })).toHaveAttribute(
    "href",
    "/containers",
  );
});

it("links to the container's CURRENT id", () => {
  // An update recreates the container with a new id. The server re-resolves the
  // id from the name for exactly this reason; a link built from the stored id
  // would lead to a container that no longer exists.
  renderSection(
    resource(
      summary({
        items: [
          { containerName: "grafana", behavior: "automatic", present: true, containerId: "new-id" },
        ],
        counts: { automatic: 1, reviewFirst: 0, monitorOnly: 0 },
        total: 1,
      }),
    ),
  );

  expect(screen.getByRole("link", { name: "grafana" })).toHaveAttribute(
    "href",
    "/containers/new-id",
  );
});

// ---------------------------------------------------- orphaned preferences --

it("reports a saved behaviour whose container is gone, without counting it", () => {
  renderSection(
    resource(
      summary({
        items: [
          { containerName: "grafana", behavior: "automatic", present: true, containerId: "grafana-id" },
          { containerName: "old-thing", behavior: "reviewFirst", present: false },
        ],
        counts: { automatic: 1, reviewFirst: 0, monitorOnly: 0 },
        total: 1,
        stale: 1,
      }),
    ),
  );

  // Visible, and explained.
  expect(screen.getByText(/one saved behaviour names a container that is not on this host/i))
    .toBeInTheDocument();
  expect(screen.getByText(/they are kept/i)).toBeInTheDocument();
  expect(screen.getByText(/old-thing/)).toBeInTheDocument();

  // And not a link: there is no container to open.
  expect(screen.queryByRole("link", { name: /old-thing/ })).not.toBeInTheDocument();
  // Nor a control that would delete it. A read changes nothing.
  expect(screen.queryAllByRole("button")).toHaveLength(0);
});

// ---------------------------------------------------- loading and failure --

it("claims nothing while the summary is loading", () => {
  renderSection(resource(null));

  expect(screen.getByRole("status")).toBeInTheDocument();
  expect(screen.queryByText(/no container has its own update behaviour/i)).not.toBeInTheDocument();
});

it("does not report a failed read as an estate with no overrides", () => {
  // Fail closed. A read that could not be performed establishes nothing, and
  // "no container has an override" is the more dangerous of the two lies.
  renderSection(resource(null, new ApiError("internal_error", "the summary could not be read", 500)));

  expect(screen.getByText(/could not read which containers/i)).toBeInTheDocument();
  expect(screen.queryByText(/no container has its own update behaviour/i)).not.toBeInTheDocument();
});

it("renders the singular case as a sentence, not a count of one", () => {
  renderSection(
    resource(
      summary({
        items: [
          { containerName: "grafana", behavior: "automatic", present: true, containerId: "grafana-id" },
        ],
        counts: { automatic: 1, reviewFirst: 0, monitorOnly: 0 },
        total: 1,
      }),
    ),
  );

  expect(screen.getByText(/one container has its own update behaviour/i)).toBeInTheDocument();
});
