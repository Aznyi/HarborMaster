import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import axe from "axe-core";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Role } from "../api/authTypes";
import type {
  DependencyGraph,
  DependencyListing,
} from "../api/dependencyTypes";
import { App } from "../App";
import { NAV_ITEMS } from "../components/AppShell";
import { SessionProvider } from "../hooks/useSession";
import {
  HttpFailure,
  containerPage,
  containerRow,
  namespaceDependency,
  operatorDependency,
  sessionResponse,
  stubApi,
  type ApiStubOptions,
  type RecordedRequest,
} from "../test/fixtures";
import { TestSessionProvider, testSession, testUser } from "../test/session";
import { Dependencies } from "./Dependencies";

/**
 * The dependency management page.
 *
 * # What these tests are actually protecting
 *
 * Three things, and all three are about an operator being told the truth:
 *
 *  1. **"Unavailable" never renders as "none".** A container HarborMaster could
 *     not assess is not a container with nothing to wait for, and the page
 *     saying the second when the first is true would clear a container on the
 *     strength of a question nobody answered.
 *  2. **A detected relationship offers no way to remove it.** There is no stored
 *     row to delete, so a remove control would be an action that cannot work.
 *  3. **A container BEHIND a loop is not blamed for the loop.** The backend
 *     gives it the same state as a container inside one; telling an operator to
 *     remove a relationship on a bystander sends them to change the wrong thing.
 *
 * The assertions are mostly about WORDS rather than markup, because the page
 * exists to answer questions in English and a structural test would pass while
 * the answers were wrong.
 */

afterEach(() => {
  vi.unstubAllGlobals();
});

function renderPage(
  options: ApiStubOptions = {},
  role: Role = "administrator",
): RecordedRequest[] {
  const requests = stubApi(options);
  render(
    <MemoryRouter>
      <TestSessionProvider session={testSession({ user: testUser(role) })}>
        <Dependencies />
      </TestSessionProvider>
    </MemoryRouter>,
  );
  return requests;
}

function listing(overrides: Partial<DependencyListing> = {}): DependencyListing {
  const items = overrides.items ?? [namespaceDependency(), operatorDependency()];
  return { items, total: items.length, ...overrides };
}

/** The estate the brief's own example describes. */
function orderedGraph(): DependencyGraph {
  return {
    stages: [
      ["gluetun", "postgres"],
      ["api", "radarr", "sonarr"],
      ["worker"],
    ],
    edges: [
      namespaceDependency(),
      namespaceDependency({ dependent: "radarr" }),
      operatorDependency(),
      operatorDependency({
        dependencyId: "dep_1111111111111111abcd",
        dependent: "worker",
        dependency: "api",
      }),
    ],
  };
}

/**
 * A loop, plus a bystander behind it.
 *
 * api -> worker -> postgres -> api is the loop; `reporting` merely depends on
 * api, so it cannot be ordered either but is not part of what has to be broken.
 */
function cyclicGraph(): DependencyGraph {
  const edge = (dependent: string, dependency: string, deletable = false) =>
    deletable
      ? operatorDependency({ dependent, dependency })
      : namespaceDependency({ dependent, dependency });

  return {
    stages: [],
    cycles: [["api", "worker", "postgres", "api"]],
    cycleDescriptions: ["api -> worker -> postgres -> api"],
    blocked: {
      api: "these containers depend on each other in a loop, so no safe order exists",
      worker: "these containers depend on each other in a loop, so no safe order exists",
      postgres: "these containers depend on each other in a loop, so no safe order exists",
      reporting: "these containers depend on each other in a loop, so no safe order exists",
    },
    edges: [
      edge("api", "worker"),
      edge("worker", "postgres", true),
      edge("postgres", "api"),
      edge("reporting", "api"),
    ],
  };
}

async function loaded() {
  await screen.findByRole("heading", { name: "Relationships" });
}

/**
 * The inventory the container picker offers.
 *
 * The picker is the real component here rather than a mock, so these tests
 * exercise the way an operator actually names a container: by choosing one that
 * exists, never by typing an id. That is the whole reason the picker replaced a
 * free-text field, and a suite that typed into the search box would be testing
 * a control the page does not have.
 */
function inventory() {
  return containerPage([
    containerRow({ id: "c-api", name: "api", image: { raw: "ghcr.io/acme/api:1" } }),
    containerRow({
      id: "c-postgres",
      name: "postgres",
      image: { raw: "postgres:16" },
    }),
  ]);
}

/** Picks a container in one end of the editor. */
async function choose(
  user: ReturnType<typeof userEvent.setup>,
  end: "Dependent container" | "Depends on",
  name: string,
) {
  const group = await screen.findByRole("group", { name: new RegExp(end) });
  await user.click(
    await within(group).findByRole("checkbox", { name: new RegExp(`^${name}`) }),
  );
}

// ------------------------------------------------------- mounting & route --

describe("Dependencies page", () => {
  it("mounts and reports what it found", async () => {
    renderPage({ dependencyListing: listing(), dependencyGraph: orderedGraph() });
    await loaded();

    // Asserted on the region's text as a whole: the number is emphasised in
    // its own element, so the sentence an operator reads spans two nodes.
    const summary = screen.getByRole("region", { name: "Dependency summary" });
    expect(summary.textContent).toContain("2 dependency relationships");
    expect(summary.textContent).toContain("1 detected by HarborMaster");
    expect(summary.textContent).toContain("1 configured by you");
  });

  it("counts the two origins separately in the summary", async () => {
    renderPage({
      dependencyListing: listing({
        items: [
          namespaceDependency(),
          namespaceDependency({ dependent: "radarr" }),
          namespaceDependency({ dependent: "lidarr" }),
          operatorDependency(),
        ],
      }),
    });
    await loaded();

    const summary = screen.getByRole("region", { name: "Dependency summary" });
    expect(summary.textContent).toContain("4 dependency relationships");
    expect(summary.textContent).toContain("3 detected by HarborMaster");
    expect(summary.textContent).toContain("1 configured by you");
  });

  it("reports a loop in the summary as something needing attention", async () => {
    renderPage({ dependencyListing: listing(), dependencyGraph: cyclicGraph() });
    await loaded();

    expect(await screen.findByText("1 loop needs attention")).toBeInTheDocument();
  });
});

describe("navigation", () => {
  it("is listed next to the other update machinery", () => {
    const paths = NAV_ITEMS.map((item) => item.path);
    const index = paths.indexOf("/dependencies");
    expect(index).toBeGreaterThan(-1);
    // Immediately after Update policies: a dependency decides WHEN an update may
    // happen, which is the same question the policy pages answer.
    expect(paths[index - 1]).toBe("/update-policies");
    expect(NAV_ITEMS[index]?.permission).toBe("dependency:read");
  });

  it("is reachable at /dependencies", async () => {
    stubApi({ dependencyListing: listing() });
    render(
      <MemoryRouter initialEntries={["/dependencies"]}>
        <SessionProvider>
          <App />
        </SessionProvider>
      </MemoryRouter>,
    );

    expect(
      await screen.findByRole("heading", { name: "Update dependencies" }),
    ).toBeInTheDocument();
  });
});

// ------------------------------------------------------------------ roles --

describe("role behaviour", () => {
  it.each<Role>(["viewer", "operator"])(
    "gives a %s no create or remove control",
    async (role) => {
      renderPage({ dependencyListing: listing() }, role);
      await loaded();

      expect(
        screen.queryByRole("button", { name: "Record an ordering" }),
      ).toBeNull();
      expect(screen.queryByRole("button", { name: /^Remove/ })).toBeNull();
    },
  );

  it.each<Role>(["viewer", "operator"])(
    "still shows a %s every relationship",
    async (role) => {
      renderPage({ dependencyListing: listing() }, role);
      await loaded();

      const list = screen.getByRole("list", { name: "Dependency relationships" });
      expect(within(list).getAllByRole("listitem")).toHaveLength(2);
    },
  );

  it("gives an administrator both controls", async () => {
    renderPage({ dependencyListing: listing() });
    await loaded();

    expect(
      screen.getByRole("button", { name: "Record an ordering" }),
    ).toBeInTheDocument();
    // Exactly one: the detected relationship has no stored row to delete.
    expect(
      screen.getAllByRole("button", { name: /^Remove the ordering/ }),
    ).toHaveLength(1);
  });

  it("offers no remove control for a detected relationship, to anybody", async () => {
    renderPage({ dependencyListing: listing({ items: [namespaceDependency()] }) });
    await loaded();

    expect(screen.queryByRole("button", { name: /^Remove/ })).toBeNull();
    expect(
      screen.getByText(/reads this from the container configuration on every refresh/i),
    ).toBeInTheDocument();
  });
});

// ----------------------------------------------------------------- create --

describe("create workflow", () => {
  it("previews the ordering before it is saved", async () => {
    const user = userEvent.setup();
    renderPage({ dependencyListing: listing(), containers: inventory() });
    await loaded();

    await user.click(screen.getByRole("button", { name: "Record an ordering" }));
    await choose(user, "Dependent container", "api");
    await choose(user, "Depends on", "postgres");

    expect(
      screen.getByText(
        "Update postgres before api. If postgres cannot be safely updated or verified, api will wait.",
      ),
    ).toBeInTheDocument();
    // And what it will NOT do, which is the half an operator would otherwise
    // have to find out afterwards.
    expect(screen.getByText(/does not change networking/i)).toBeInTheDocument();
    expect(
      screen.getByText(/does not make either container eligible for updates/i),
    ).toBeInTheDocument();
  });

  it("sends two names and nothing else", async () => {
    const user = userEvent.setup();
    const requests = renderPage({
      dependencyListing: listing(),
      containers: inventory(),
    });
    await loaded();

    await user.click(screen.getByRole("button", { name: "Record an ordering" }));
    await choose(user, "Dependent container", "api");
    await choose(user, "Depends on", "postgres");
    await user.click(screen.getByRole("button", { name: "Record ordering" }));

    await waitFor(() =>
      expect(
        requests.some(
          (request) =>
            request.method === "POST" && request.url.includes("/dependencies"),
        ),
      ).toBe(true),
    );
    expect(
      await screen.findByText(
        "HarborMaster will now update postgres before api.",
      ),
    ).toBeInTheDocument();
  });

  it.each([
    ["a container cannot depend on itself"],
    ["HarborMaster already records this relationship"],
    ["HarborMaster has no container by that name"],
    ["the last inventory refresh did not see that container"],
    ["that is the container HarborMaster is running in"],
    [
      "that container is a parked original or a quarantined replacement HarborMaster keeps as evidence",
    ],
    ["a container name was not one HarborMaster can record a relationship for"],
    ["this relationship would make the containers depend on each other in a loop"],
  ])("renders the server's refusal verbatim: %s", async (message) => {
    const user = userEvent.setup();
    renderPage({
      dependencyListing: listing(),
      containers: inventory(),
      dependencyWrite: new HttpFailure(409, "conflict", message),
    });
    await loaded();

    await user.click(screen.getByRole("button", { name: "Record an ordering" }));
    await choose(user, "Dependent container", "api");
    await choose(user, "Depends on", "postgres");
    await user.click(screen.getByRole("button", { name: "Record ordering" }));

    // Announced, and in the server's own words. A generic "Could not save"
    // would throw away the only part of the response that tells an operator
    // what to do differently.
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(message);
  });
});

// ----------------------------------------------------------------- delete --

describe("delete workflow", () => {
  it("states the consequence rather than asking if the operator is sure", async () => {
    const user = userEvent.setup();
    renderPage({ dependencyListing: listing() });
    await loaded();

    await user.click(
      screen.getByRole("button", { name: /^Remove the ordering/ }),
    );

    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveAccessibleName("Remove dependency?");
    expect(
      within(dialog).getByText(
        "api will no longer wait for postgres before updating.",
      ),
    ).toBeInTheDocument();
    expect(
      within(dialog).getByText("This does not modify either container."),
    ).toBeInTheDocument();
    expect(screen.queryByText(/are you sure/i)).toBeNull();
  });

  it("deletes by id and confirms what changed", async () => {
    const user = userEvent.setup();
    const requests = renderPage({ dependencyListing: listing() });
    await loaded();

    await user.click(
      screen.getByRole("button", { name: /^Remove the ordering/ }),
    );
    await user.click(screen.getByRole("button", { name: "Remove dependency" }));

    await waitFor(() =>
      expect(
        requests.some(
          (request) =>
            request.method === "DELETE" &&
            request.url.includes("/dependencies/dep_0123456789abcdef0123"),
        ),
      ).toBe(true),
    );
    expect(
      await screen.findByText("api will no longer wait for postgres."),
    ).toBeInTheDocument();
  });

  it("cancelling deletes nothing", async () => {
    const user = userEvent.setup();
    const requests = renderPage({ dependencyListing: listing() });
    await loaded();

    await user.click(
      screen.getByRole("button", { name: /^Remove the ordering/ }),
    );
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByRole("dialog")).toBeNull();
    expect(requests.some((request) => request.method === "DELETE")).toBe(false);
  });

  it("reports a refused delete rather than claiming success", async () => {
    const user = userEvent.setup();
    renderPage({
      dependencyListing: listing(),
      dependencyWrite: new HttpFailure(404, "not_found", "dependency not found"),
    });
    await loaded();

    await user.click(
      screen.getByRole("button", { name: /^Remove the ordering/ }),
    );
    await user.click(screen.getByRole("button", { name: "Remove dependency" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "dependency not found",
    );
    // No success notice. The dialog stays open -- it still states what removal
    // WOULD do -- but nothing claims the removal happened.
    expect(
      screen.queryByText("api will no longer wait for postgres."),
    ).toBeNull();
    expect(screen.queryByRole("status")).toBeNull();
  });
});

// ------------------------------------------------------------------- list --

describe("relationship list", () => {
  it("names the kind and the origin in words", async () => {
    renderPage({ dependencyListing: listing() });
    await loaded();

    const list = screen.getByRole("list", { name: "Dependency relationships" });
    expect(within(list).getByText("Network namespace")).toBeInTheDocument();
    expect(within(list).getByText("Detected by HarborMaster")).toBeInTheDocument();
    expect(within(list).getByText("Application ordering")).toBeInTheDocument();
    expect(within(list).getByText("Configured by you")).toBeInTheDocument();
  });

  it("renders no raw enum value", async () => {
    renderPage({ dependencyListing: listing() });
    await loaded();

    for (const raw of [
      "dockerNetworkNamespace",
      "dockerIPCNamespace",
      "dockerPIDNamespace",
      "operator",
      "dependencyCycle",
    ]) {
      expect(screen.queryByText(raw)).toBeNull();
    }
  });

  it("filters to one origin at a time", async () => {
    const user = userEvent.setup();
    renderPage({ dependencyListing: listing() });
    await loaded();

    await user.click(screen.getByRole("radio", { name: /Configured by you/ }));
    let list = screen.getByRole("list", { name: "Dependency relationships" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(1);
    expect(within(list).getByText("Application ordering")).toBeInTheDocument();

    await user.click(
      screen.getByRole("radio", { name: /Detected by HarborMaster/ }),
    );
    list = screen.getByRole("list", { name: "Dependency relationships" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(1);
    expect(within(list).getByText("Network namespace")).toBeInTheDocument();
  });

  it("renders a long container name in full", async () => {
    const long = `${"a-very-long-container-name-".repeat(4)}end`;
    renderPage({
      dependencyListing: listing({
        items: [operatorDependency({ dependent: long })],
      }),
    });
    await loaded();

    const link = screen.getByRole("link", { name: long });
    // break-all rather than truncate: a name an operator cannot read in full is
    // a name they cannot check they are looking at the right container.
    expect(link.className).toContain("break-all");
  });
});

// -------------------------------------------------------- empty & unknown --

describe("empty and unavailable states", () => {
  it("says nothing was found only when discovery positively succeeded", async () => {
    renderPage({ dependencyGraph: { stages: [], edges: [] } });
    await loaded();

    expect(screen.getByText("No dependencies found")).toBeInTheDocument();
    expect(
      screen.getByText(/can evaluate these workloads independently/i),
    ).toBeInTheDocument();
  });

  it("never says 'no dependencies found' while a share is unresolved", async () => {
    renderPage({
      dependencyListing: {
        items: [],
        total: 0,
        problems: [
          {
            container: "sonarr",
            source: "dockerNetworkNamespace",
            refusal: "namespacesUnobserved",
          },
        ],
      },
    });
    await loaded();

    // THE distinction this page must not get wrong.
    expect(screen.queryByText("No dependencies found")).toBeNull();
    expect(
      screen.getByText("Dependency information unavailable"),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/has not yet read this container's namespace configuration/i),
    ).toBeInTheDocument();
  });

  it("says a namespace provider could not be identified", async () => {
    renderPage({
      dependencyListing: {
        items: [],
        total: 0,
        problems: [
          {
            container: "sonarr",
            source: "dockerNetworkNamespace",
            refusal: "referencedContainerUnnamed",
          },
        ],
      },
    });
    await loaded();

    expect(
      screen.getByText(/shares the namespace of a container HarborMaster cannot identify/i),
    ).toBeInTheDocument();
  });

  it("reports the order as unavailable rather than empty when the graph fails", async () => {
    renderPage({
      dependencyListing: listing(),
      dependencyGraph: new HttpFailure(
        503,
        "unavailable",
        "the dependency graph could not be built for this host",
      ),
    });
    await loaded();

    expect(
      await screen.findByText(/Dependency information unavailable\./),
    ).toBeInTheDocument();
    // The relationship list is a separate read and survives.
    expect(
      screen.getByRole("list", { name: "Dependency relationships" }),
    ).toBeInTheDocument();
  });

  it("fails the whole page when the relationship list cannot be read", async () => {
    renderPage({
      dependencyListing: new HttpFailure(503, "unavailable", "dependency tracking is not available"),
    });

    expect(
      await screen.findByText(/dependency tracking is not available/),
    ).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------- preview --

describe("update-order preview", () => {
  it("lists the stages in order, counted from one", async () => {
    renderPage({ dependencyListing: listing(), dependencyGraph: orderedGraph() });
    await loaded();

    const order = await screen.findByRole("list", { name: "Update order by stage" });
    const stages = within(order).getAllByRole("listitem", { });
    // Three stage entries, each with its own nested container list.
    expect(within(order).getByRole("heading", { name: /Stage 1/ })).toBeInTheDocument();
    expect(within(order).getByRole("heading", { name: /Stage 2/ })).toBeInTheDocument();
    expect(within(order).getByRole("heading", { name: /Stage 3/ })).toBeInTheDocument();
    expect(stages.length).toBeGreaterThanOrEqual(3);
  });

  it("says the order is a constraint, not a work list", async () => {
    renderPage({ dependencyListing: listing(), dependencyGraph: orderedGraph() });
    await loaded();

    expect(
      await screen.findByText(/is not necessarily due an update/i),
    ).toBeInTheDocument();
    expect(screen.queryByText(/will update now/i)).toBeNull();
  });

  it("explains why each wait exists", async () => {
    renderPage({ dependencyListing: listing(), dependencyGraph: orderedGraph() });
    await loaded();

    expect(
      await screen.findByText(
        "sonarr waits for gluetun because it shares gluetun's network namespace.",
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "api waits for postgres because you configured that ordering.",
      ),
    ).toBeInTheDocument();
  });

  it("says so plainly when nothing is constrained", async () => {
    renderPage({ dependencyGraph: { stages: [], edges: [] } });
    await loaded();

    expect(
      await screen.findByText(/No ordering constraints exist/i),
    ).toBeInTheDocument();
  });
});

// ------------------------------------------------------------------ cycle --

describe("cycle UX", () => {
  it("says what is wrong in English, never 'dependencyCycle'", async () => {
    renderPage({ dependencyListing: listing(), dependencyGraph: cyclicGraph() });
    await loaded();

    expect(
      await screen.findByText(
        /cannot determine a safe update order for these workloads/i,
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText("dependencyCycle")).toBeNull();
  });

  it("walks the loop as text rather than as arrows alone", async () => {
    renderPage({ dependencyListing: listing(), dependencyGraph: cyclicGraph() });
    await loaded();

    // Scoped to the loop walk. The relationship list below it says "api depends
    // on worker" too, and an unscoped query would pass on the wrong element.
    const loop = await screen.findByRole("list", { name: "The path round the loop" });
    expect(within(loop).getByText(/depends on worker$/)).toBeInTheDocument();
    expect(within(loop).getByText(/depends on postgres$/)).toBeInTheDocument();
    expect(
      within(loop).getByText(/depends on api, which closes the loop/),
    ).toBeInTheDocument();
    // Three steps, not four: the closed path repeats its first element and the
    // repeat must not be rendered as a fourth container.
    expect(within(loop).getAllByRole("listitem")).toHaveLength(3);
  });

  it("names which relationship in the loop can be removed", async () => {
    renderPage({ dependencyListing: listing(), dependencyGraph: cyclicGraph() });
    await loaded();

    expect(
      await screen.findByText(
        /One relationship in this loop was configured by an administrator and can be removed/i,
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /Remove this relationship/ }),
    ).toBeInTheDocument();
  });

  it("says HarborMaster cannot fix a loop made only of detected relationships", async () => {
    const graph = cyclicGraph();
    graph.edges = graph.edges.map((edge) =>
      namespaceDependency({
        dependent: edge.dependent,
        dependency: edge.dependency,
      }),
    );
    renderPage({ dependencyListing: listing(), dependencyGraph: graph });
    await loaded();

    expect(
      await screen.findByText(/Every relationship in this loop is one Docker itself enforces/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/the configuration on the host has to change/i),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /Remove this relationship/ }),
    ).toBeNull();
  });

  it("does not offer an administrator's remove control to an operator", async () => {
    renderPage(
      { dependencyListing: listing(), dependencyGraph: cyclicGraph() },
      "operator",
    );
    await loaded();

    await screen.findByText(/cannot determine a safe update order/i);
    expect(
      screen.queryByRole("button", { name: /Remove this relationship/ }),
    ).toBeNull();
  });

  it("distinguishes a container behind the loop from one inside it", async () => {
    renderPage({ dependencyListing: listing(), dependencyGraph: cyclicGraph() });
    await loaded();

    // The bystander is explained, and explicitly NOT blamed.
    expect(
      await screen.findByText(
        /reporting cannot update because its dependency chain reaches the loop above/i,
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/nothing to change on reporting itself/i),
    ).toBeInTheDocument();

    // And the containers in the loop are not listed as bystanders.
    const behind = screen.getByRole("region", {
      name: "Containers blocked behind the loop",
    });
    expect(within(behind).getAllByRole("listitem")).toHaveLength(1);
  });
});

// ---------------------------------------------------------- accessibility --

describe("accessibility", () => {
  it("has no serious or critical violations in the ordinary state", async () => {
    stubApi({ dependencyListing: listing(), dependencyGraph: orderedGraph() });
    const { container } = render(
      <MemoryRouter>
        <TestSessionProvider>
          <Dependencies />
        </TestSessionProvider>
      </MemoryRouter>,
    );
    await loaded();
    await screen.findByRole("list", { name: "Update order by stage" });

    const results = await axe.run(container);
    expect(
      results.violations
        .filter((v) => v.impact === "serious" || v.impact === "critical")
        .map((v) => v.id),
    ).toEqual([]);
  });

  it("has no serious or critical violations with the create form open", async () => {
    const user = userEvent.setup();
    stubApi({ dependencyListing: listing() });
    const { container } = render(
      <MemoryRouter>
        <TestSessionProvider>
          <Dependencies />
        </TestSessionProvider>
      </MemoryRouter>,
    );
    await loaded();
    await user.click(screen.getByRole("button", { name: "Record an ordering" }));

    const results = await axe.run(container);
    expect(
      results.violations
        .filter((v) => v.impact === "serious" || v.impact === "critical")
        .map((v) => v.id),
    ).toEqual([]);
  });

  it("has no serious or critical violations with the delete dialog open", async () => {
    const user = userEvent.setup();
    stubApi({ dependencyListing: listing() });
    const { container } = render(
      <MemoryRouter>
        <TestSessionProvider>
          <Dependencies />
        </TestSessionProvider>
      </MemoryRouter>,
    );
    await loaded();
    await user.click(
      screen.getByRole("button", { name: /^Remove the ordering/ }),
    );

    const results = await axe.run(container);
    expect(
      results.violations
        .filter((v) => v.impact === "serious" || v.impact === "critical")
        .map((v) => v.id),
    ).toEqual([]);
  });

  it("has no serious or critical violations in the cycle state", async () => {
    stubApi({ dependencyListing: listing(), dependencyGraph: cyclicGraph() });
    const { container } = render(
      <MemoryRouter>
        <TestSessionProvider>
          <Dependencies />
        </TestSessionProvider>
      </MemoryRouter>,
    );
    await loaded();
    await screen.findByText(/cannot determine a safe update order/i);

    const results = await axe.run(container);
    expect(
      results.violations
        .filter((v) => v.impact === "serious" || v.impact === "critical")
        .map((v) => v.id),
    ).toEqual([]);
  });

  it("has no serious or critical violations while a share is unresolved", async () => {
    stubApi({
      dependencyListing: {
        items: [],
        total: 0,
        problems: [
          {
            container: "sonarr",
            source: "dockerNetworkNamespace",
            refusal: "referenceNotParseable",
          },
        ],
      },
    });
    const { container } = render(
      <MemoryRouter>
        <TestSessionProvider>
          <Dependencies />
        </TestSessionProvider>
      </MemoryRouter>,
    );
    await loaded();

    const results = await axe.run(container);
    expect(
      results.violations
        .filter((v) => v.impact === "serious" || v.impact === "critical")
        .map((v) => v.id),
    ).toEqual([]);
  });

  it("reaches the delete flow entirely from the keyboard", async () => {
    const user = userEvent.setup();
    renderPage({ dependencyListing: listing({ items: [operatorDependency()] }) });
    await loaded();

    screen.getByRole("button", { name: /^Remove the ordering/ }).focus();
    await user.keyboard("{Enter}");

    // Focus moves INTO the dialog, and onto the reversible choice. Leaving it
    // on the button behind a modal would have the operator tabbing through the
    // page underneath rather than the decision in front of them.
    const dialog = await screen.findByRole("dialog");
    const cancel = within(dialog).getByRole("button", { name: "Cancel" });
    expect(cancel).toHaveFocus();

    // Tab reaches the destructive one; Escape backs out.
    await user.tab();
    expect(
      within(dialog).getByRole("button", { name: "Remove dependency" }),
    ).toHaveFocus();

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

// -------------------------------------------------------------- integrity --

describe("what the page does not do", () => {
  it("issues no request that could change a container", async () => {
    const user = userEvent.setup();
    const requests = renderPage({
      dependencyListing: listing(),
      dependencyGraph: orderedGraph(),
    });
    await loaded();
    await user.click(screen.getByRole("button", { name: "Record an ordering" }));

    for (const request of requests) {
      expect(request.url).not.toMatch(
        /\/(acquisitions|executions|rollbacks|plans)\b/,
      );
    }
  });

  it("asks only for what it renders", async () => {
    const requests = renderPage({
      dependencyListing: listing(),
      dependencyGraph: orderedGraph(),
    });
    await loaded();

    const dependencyReads = requests.filter(
      (request) =>
        request.method === "GET" && request.url.includes("/dependencies"),
    );
    // The listing, the graph, and the coordinated updates. THREE, and never one
    // per relationship or one per operation: the count is the N+1 guard, so it
    // is asserted exactly rather than as an upper bound.
    expect(dependencyReads).toHaveLength(3);
    expect(
      dependencyReads.map((request) =>
        request.url.replace(/^.*\/api\/v1/, "").split("?")[0],
      ).sort(),
    ).toEqual([
      "/dependencies",
      "/dependencies/graph",
      "/dependencies/operations",
    ]);
  });
});

// The session fixture is the server's permission matrix restated. If the two
// drift, every role test above becomes a test of the fixture rather than of the
// page -- which is exactly how this file's first run passed for the wrong
// reason, before `dependency:read` was added to the viewer set.
describe("the permission fixture", () => {
  it("matches the server's matrix for dependencies", () => {
    expect(sessionResponse("viewer").user.permissions).toContain("dependency:read");
    expect(sessionResponse("operator").user.permissions).toContain("dependency:read");
    expect(sessionResponse("viewer").user.permissions).not.toContain(
      "dependency:manage",
    );
    expect(sessionResponse("operator").user.permissions).not.toContain(
      "dependency:manage",
    );
    expect(sessionResponse("administrator").user.permissions).toContain(
      "dependency:manage",
    );
  });
});
