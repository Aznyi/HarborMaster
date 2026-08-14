import { render, screen, within } from "@testing-library/react";
import axe from "axe-core";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import type {
  ContainerDependencies,
  WorkloadDependency,
} from "../api/dependencyTypes";
import { DependencySection } from "./DependencySection";

/**
 * The dependency section, from an operator's point of view.
 *
 * The assertions are deliberately about WORDS rather than markup: this section
 * exists to answer questions in English, and a test that only checked structure
 * would pass while the answers were wrong.
 */

function edge(overrides: Partial<WorkloadDependency> = {}): WorkloadDependency {
  return {
    dependent: "sonarr",
    dependency: "gluetun",
    source: "dockerNetworkNamespace",
    kind: "Network namespace",
    origin: "Detected by HarborMaster",
    hard: true,
    why: "the dependent's Docker configuration shares this container's network namespace",
    deletable: false,
    ...overrides,
  };
}

function view(overrides: Partial<ContainerDependencies> = {}): ContainerDependencies {
  return {
    container: "sonarr",
    dependsOn: [],
    dependedOnBy: [],
    state: "dependencySatisfied",
    detail: "every container this one depends on is verified or stable",
    ...overrides,
  };
}

function renderSection(props: Parameters<typeof DependencySection>[0]) {
  return render(
    <MemoryRouter>
      <DependencySection {...props} />
    </MemoryRouter>,
  );
}

describe("DependencySection", () => {
  it("names the relationship kind and who established it", () => {
    renderSection({ dependencies: view({ dependsOn: [edge()] }) });

    const list = screen.getByRole("region", {
      name: "This container depends on",
    });
    expect(within(list).getByRole("link", { name: "gluetun" })).toBeInTheDocument();
    expect(within(list).getByText("Network namespace")).toBeInTheDocument();
    expect(within(list).getByText("Detected by HarborMaster")).toBeInTheDocument();
  });

  it("distinguishes a configured ordering from a detected one", () => {
    renderSection({
      dependencies: view({
        dependsOn: [
          edge(),
          edge({
            dependency: "postgres",
            source: "operator",
            kind: "Application ordering",
            origin: "Configured by you",
            hard: false,
            deletable: true,
          }),
        ],
      }),
    });

    expect(screen.getByText("Application ordering")).toBeInTheDocument();
    expect(screen.getByText("Configured by you")).toBeInTheDocument();
    expect(screen.getByText("Detected by HarborMaster")).toBeInTheDocument();
  });

  it("explains a namespace relationship without Docker jargon", () => {
    renderSection({ dependencies: view({ dependsOn: [edge()] }) });

    // The sentence an operator needs: what happens, and what HarborMaster will
    // do about it.
    expect(
      screen.getByText(/sonarr shares gluetun's network namespace/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/recreated on the image it is already running/i),
    ).toBeInTheDocument();
  });

  it("writes the explanation dependent-first in both directions", () => {
    renderSection({
      dependencies: view({
        container: "gluetun",
        dependedOnBy: [edge()],
      }),
    });

    // Listing what depends on gluetun still explains it as "sonarr shares
    // gluetun's ...", never the other way round.
    expect(
      screen.getByText(/sonarr shares gluetun's network namespace/i),
    ).toBeInTheDocument();
  });

  it("explains a configured ordering as a wait rather than a namespace", () => {
    renderSection({
      dependencies: view({
        dependsOn: [
          edge({
            dependency: "postgres",
            source: "operator",
            hard: false,
          }),
        ],
      }),
    });

    expect(
      screen.getByText("sonarr waits for postgres to finish updating."),
    ).toBeInTheDocument();
    // An ordering must NOT claim a namespace is involved.
    expect(screen.queryByText(/namespace/i)).not.toBeInTheDocument();
  });

  it("says nothing was detected rather than implying safety was proven", () => {
    renderSection({ dependencies: view() });

    expect(
      screen.getByText(/has not detected or been configured with any dependencies/i),
    ).toBeInTheDocument();
  });

  it("reports unavailable rather than none when the service could not answer", () => {
    renderSection({ dependencies: undefined, unavailable: true });

    expect(
      screen.getByText(/Dependency information unavailable/i),
    ).toBeInTheDocument();
    // THE distinction. "None" would tell an operator it is safe to proceed on
    // the strength of a question nobody asked.
    expect(screen.queryByText(/has not detected or been configured/i)).toBeNull();
  });

  it("reports an unresolved namespace share as unavailable, not as absent", () => {
    renderSection({
      dependencies: view({
        problems: [
          {
            container: "sonarr",
            source: "dockerNetworkNamespace",
            refusal: "referencedContainerUnknown",
          },
        ],
      }),
    });

    expect(
      screen.getByText(/attached to a namespace whose container is no longer present/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/will not update this container until it can establish/i),
    ).toBeInTheDocument();
  });

  it("explains a pending reattachment as a same-image recreation", () => {
    renderSection({
      dependencies: view({
        outstandingRebinds: [
          {
            operationId: "depop_0123456789abcdef0123",
            dependent: "sonarr",
            provider: "gluetun",
            source: "dockerNetworkNamespace",
            state: "pending",
          },
        ],
      }),
    });

    expect(screen.getByText(/Its image is not being changed/i)).toBeInTheDocument();
    expect(screen.getByText("Waiting to be reattached")).toBeInTheDocument();
  });

  it("renders a long container name without truncating it", () => {
    const long = "a-very-long-container-name-".repeat(4) + "end";
    renderSection({
      dependencies: view({ dependsOn: [edge({ dependency: long })] }),
    });

    const link = screen.getByRole("link", { name: long });
    // break-all rather than truncate: a name an operator cannot read in full is
    // a name they cannot verify they are looking at the right container.
    expect(link.className).toContain("break-all");
  });

  it("has no serious or critical accessibility violations", async () => {
    const { container } = renderSection({
      dependencies: view({
        dependsOn: [edge()],
        dependedOnBy: [edge({ dependent: "radarr" })],
        problems: [
          {
            container: "sonarr",
            source: "dockerIPCNamespace",
            refusal: "namespacesUnobserved",
          },
        ],
      }),
    });

    const results = await axe.run(container);
    const serious = results.violations.filter(
      (violation) => violation.impact === "serious" || violation.impact === "critical",
    );
    expect(serious.map((violation) => violation.id)).toEqual([]);
  });
});
