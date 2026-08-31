import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import axe from "axe-core";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Acquisition } from "../api/acquisitionTypes";
import type { ContainerDependencies } from "../api/dependencyTypes";
import { RecreateContainerAction } from "./RecreateContainerAction";
import {
  ProviderUpdateRefusal,
  ProviderUpdateWarning,
} from "./ProviderUpdateWarning";
import {
  HttpFailure,
  namespaceDependency,
  operatorDependency,
  stubApi,
  type ApiStubOptions,
} from "../test/fixtures";

/**
 * The manual provider update: what an operator is told before they confirm.
 *
 * # The fact Docker will not give them
 *
 * `network_mode: container:gluetun` binds a container to gluetun's CURRENT
 * identity. Replace gluetun and the dependents are attached to a namespace that
 * no longer exists — Docker reports nothing, the containers keep running, and
 * the network silently stops. Somebody pressing "stop and recreate gluetun"
 * deserves to know three other containers are about to be recreated too.
 *
 * # And the claim that must never be made
 *
 * A configured ordering is NOT a rebind cascade. `api depends on postgres` is
 * an operator's assertion about their application; updating postgres by hand
 * does not recreate api. The tests below assert both halves: the hard
 * dependents are listed as containers that will be recreated, and the
 * configured ones are listed as ordering context that explicitly will not be.
 */

afterEach(() => {
  vi.unstubAllGlobals();
});

function view(overrides: Partial<ContainerDependencies> = {}): ContainerDependencies {
  return {
    container: "gluetun",
    dependsOn: [],
    dependedOnBy: [],
    state: "dependencySatisfied",
    detail: "every container this one depends on is verified or stable",
    ...overrides,
  };
}

function renderIn(node: React.ReactNode) {
  return render(<MemoryRouter>{node}</MemoryRouter>);
}

async function noSeriousViolations(container: HTMLElement) {
  const results = await axe.run(container);
  return results.violations
    .filter((v) => v.impact === "serious" || v.impact === "critical")
    .map((v) => v.id);
}

// ------------------------------------------------------------- the notice --

describe("the provider update warning", () => {
  it("says nothing for a container nothing depends on", () => {
    const { container } = renderIn(
      <ProviderUpdateWarning container="gluetun" dependencies={view()} />,
    );
    // A provider with no dependents keeps the confirmation flow it always had.
    expect(container.textContent).toBe("");
  });

  it("names every hard dependent exactly once", () => {
    renderIn(
      <ProviderUpdateWarning
        container="gluetun"
        dependencies={view({
          dependedOnBy: [
            namespaceDependency({ dependent: "sonarr" }),
            namespaceDependency({ dependent: "radarr" }),
            namespaceDependency({ dependent: "qbittorrent" }),
          ],
        })}
      />,
    );

    expect(
      screen.getByRole("heading", { name: /Updating gluetun affects 3 other containers/ }),
    ).toBeInTheDocument();
    for (const name of ["sonarr", "radarr", "qbittorrent"]) {
      expect(screen.getAllByText(name)).toHaveLength(1);
    }
  });

  it("counts a container bound by two namespaces once", () => {
    renderIn(
      <ProviderUpdateWarning
        container="gluetun"
        dependencies={view({
          dependedOnBy: [
            namespaceDependency({ dependent: "sonarr" }),
            namespaceDependency({
              dependent: "sonarr",
              source: "dockerIPCNamespace",
            }),
          ],
        })}
      />,
    );

    // One container to recreate, listed once. Listing it twice would make an
    // operator believe more of their estate is affected than is.
    expect(
      screen.getByRole("heading", { name: /affects 1 other container/ }),
    ).toBeInTheDocument();
    expect(screen.getAllByText("sonarr")).toHaveLength(1);
  });

  it("states the four things this does not do", () => {
    renderIn(
      <ProviderUpdateWarning
        container="gluetun"
        dependencies={view({ dependedOnBy: [namespaceDependency()] })}
      />,
    );

    expect(screen.getByText(/No image version changes/i)).toBeInTheDocument();
    expect(
      screen.getByText(/digest that container is already running/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Every recreation takes the normal pipeline/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/A configured ordering never causes a recreation/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Nothing else on this host is affected/i),
    ).toBeInTheDocument();
  });

  it("shows a configured ordering as context, never as a recreation", () => {
    renderIn(
      <ProviderUpdateWarning
        container="postgres"
        dependencies={view({
          container: "postgres",
          dependedOnBy: [operatorDependency({ dependent: "api", dependency: "postgres" })],
        })}
      />,
    );

    // NOT "affects N other containers": nothing is being recreated.
    expect(screen.queryByText(/affects \d+ other/i)).toBeNull();
    const context = screen.getByRole("region", {
      name: "Containers configured to wait for postgres",
    });
    expect(within(context).getByText("api")).toBeInTheDocument();
    expect(
      screen.getByText(/will not be recreated and is not changed by this update/i),
    ).toBeInTheDocument();
  });

  it("keeps the two kinds apart when a provider has both", () => {
    renderIn(
      <ProviderUpdateWarning
        container="gluetun"
        dependencies={view({
          dependedOnBy: [
            namespaceDependency({ dependent: "sonarr" }),
            operatorDependency({ dependent: "reporting", dependency: "gluetun" }),
          ],
        })}
      />,
    );

    // Only the namespace dependent is counted as affected.
    expect(
      screen.getByRole("heading", { name: /affects 1 other container/ }),
    ).toBeInTheDocument();
    const context = screen.getByRole("region", {
      name: "Containers configured to wait for gluetun",
    });
    expect(within(context).getByText("reporting")).toBeInTheDocument();
    expect(within(context).queryByText("sonarr")).toBeNull();
  });

  it("reports an unavailable answer as unavailable, not as none", () => {
    renderIn(<ProviderUpdateWarning container="gluetun" dependencies={undefined} unavailable />);

    expect(
      screen.getByText(/could not establish what depends on gluetun/i),
    ).toBeInTheDocument();
    // THE distinction. It must not read as "nothing depends on this".
    expect(
      screen.getByText(/nothing here should be read as a statement that no container depends/i),
    ).toBeInTheDocument();
  });

  it("has no serious accessibility violations", async () => {
    const { container } = renderIn(
      <ProviderUpdateWarning
        container="gluetun"
        dependencies={view({
          dependedOnBy: [
            namespaceDependency({ dependent: "sonarr" }),
            operatorDependency({ dependent: "reporting", dependency: "gluetun" }),
          ],
        })}
      />,
    );
    expect(await noSeriousViolations(container)).toEqual([]);
  });
});

// ------------------------------------------------------------- refusals --

describe("a refused provider update", () => {
  it("renders HarborMaster's own sentence and says nothing changed", async () => {
    const { container } = renderIn(
      <ProviderUpdateRefusal message="a container that shares this one's network namespace could not be established as safely recreatable" />,
    );

    expect(screen.getByRole("alert")).toHaveTextContent(
      /could not be established as safely recreatable/,
    );
    expect(screen.getByText("Update blocked")).toBeInTheDocument();
    expect(
      screen.getByText(/refuses before it stops anything, so the container is exactly as it was/i),
    ).toBeInTheDocument();
    expect(await noSeriousViolations(container)).toEqual([]);
  });

  it("renders a hostile message as text rather than as markup", () => {
    renderIn(
      <ProviderUpdateRefusal message={'<img src=x onerror="alert(1)">'} />,
    );
    const alert = screen.getByRole("alert");
    expect(alert.querySelector("img")).toBeNull();
    expect(alert.textContent).toContain("<img src=x");
  });
});

// ------------------------------------------------------- the whole flow --

function acquisition(overrides: Partial<Acquisition> = {}): Acquisition {
  return {
    acquisitionId: "acq_0123456789abcdef0123",
    containerId: "c-gluetun",
    currentContainerId: "c-gluetun",
    containerName: "gluetun",
    state: "succeeded",
    target: {
      reference: "ghcr.io/qdm12/gluetun:v3",
      repository: "ghcr.io/qdm12/gluetun",
      digest: "sha256:aaaa",
    },
    requestedAt: "2026-08-13T09:00:00Z",
    ...overrides,
  } as Acquisition;
}

function renderAction(options: ApiStubOptions = {}) {
  stubApi(options);
  return render(
    <MemoryRouter>
      <RecreateContainerAction acquisition={acquisition()} />
    </MemoryRouter>,
  );
}

describe("the recreation confirmation", () => {
  it("lists the dependants before the operator confirms", async () => {
    const user = userEvent.setup();
    renderAction({
      dependencies: view({
        dependedOnBy: [
          namespaceDependency({ dependent: "sonarr" }),
          namespaceDependency({ dependent: "radarr" }),
        ],
      }),
    });

    await user.click(screen.getByRole("button", { name: "Recreate container" }));

    expect(
      await screen.findByRole("heading", { name: /affects 2 other containers/ }),
    ).toBeInTheDocument();
    // And the existing confirmation is intact: a typed name is still required.
    expect(
      screen.getByRole("button", { name: /Stop and recreate gluetun/ }),
    ).toBeDisabled();
  });

  it("keeps the plain confirmation for a provider nothing depends on", async () => {
    const user = userEvent.setup();
    renderAction({ dependencies: view() });

    await user.click(screen.getByRole("button", { name: "Recreate container" }));

    await screen.findByText(/will be stopped and recreated/i);
    expect(screen.queryByText(/affects \d+ other/i)).toBeNull();
  });

  it("says so when the dependency read failed", async () => {
    const user = userEvent.setup();
    renderAction({
      dependencies: new HttpFailure(503, "unavailable", "dependency tracking is not available"),
    });

    await user.click(screen.getByRole("button", { name: "Recreate container" }));

    expect(
      await screen.findByText(/could not establish what depends on gluetun/i),
    ).toBeInTheDocument();
  });

  it("renders a preflight refusal in HarborMaster's own words", async () => {
    const user = userEvent.setup();
    renderAction({
      dependencies: view({ dependedOnBy: [namespaceDependency()] }),
      execution: new HttpFailure(
        409,
        "conflict",
        "a container sharing this one's network namespace could not be established as safely recreatable",
      ),
    });

    await user.click(screen.getByRole("button", { name: "Recreate container" }));
    await user.type(
      screen.getByRole("textbox"),
      "gluetun",
    );
    await user.click(
      screen.getByRole("button", { name: /Stop and recreate gluetun/ }),
    );

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(
        /could not be established as safely recreatable/,
      ),
    );
    expect(screen.getByText("Update blocked")).toBeInTheDocument();
  });

  it("has no serious accessibility violations with the dialog open", async () => {
    const user = userEvent.setup();
    const { container } = renderAction({
      dependencies: view({
        dependedOnBy: [namespaceDependency({ dependent: "sonarr" })],
      }),
    });

    await user.click(screen.getByRole("button", { name: "Recreate container" }));
    await screen.findByRole("heading", { name: /affects 1 other container/ });

    expect(await noSeriousViolations(container)).toEqual([]);
  });
});
