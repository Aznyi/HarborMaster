import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SessionProvider } from "../hooks/useSession";
import { MemoryRouter, Route, Routes } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError } from "../api/client";
import { App } from "../App";
import { ContainerDetailPage } from "./ContainerDetail";
import { Containers } from "./Containers";
import { Images } from "./Images";
import {
  containerDetail,
  containerPage,
  containerSummary,
  inventoryStatus,
  lastContainerQuery,
  stubApi,
  type RecordedRequest,
} from "../test/fixtures";

function renderApp(path = "/") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <SessionProvider>
        <App />
      </SessionProvider>
    </MemoryRouter>,
  );
}

function renderContainers() {
  return render(
    <MemoryRouter initialEntries={["/containers"]}>
      <Routes>
        <Route path="/containers" element={<Containers />} />
        <Route path="/containers/:id" element={<ContainerDetailPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

function renderDetail(id = "abcdef0123456789") {
  return render(
    <MemoryRouter initialEntries={[`/containers/${id}`]}>
      <Routes>
        <Route path="/containers/:id" element={<ContainerDetailPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

/**
 * Opens the dashboard's collapsed technical section.
 *
 * The inventory generation, checksum and manual refresh moved below a
 * disclosure: they are how HarborMaster works, not what an operator needs on
 * the first screen. They are all still there, which is what these tests keep
 * true.
 */
async function openTechnicalDetails() {
  const user = userEvent.setup();
  await user.click(await screen.findByText(/technical details/i));
}

beforeEach(() => {
  vi.unstubAllGlobals();
});

// ------------------------------------------------------------- dashboard --

describe("Dashboard", () => {
  it("shows a loading state before the inventory arrives", async () => {
    stubApi();
    renderApp();

    // The identity resolves before the shell mounts, so the first status
    // region belongs to the session check rather than to the inventory.
    await waitFor(() =>
      expect(screen.getByRole("status", { name: /loading inventory/i })).toBeInTheDocument(),
    );
  });

  it("renders live inventory metrics", async () => {
    stubApi();
    renderApp();

    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /your containers/i })).toBeInTheDocument(),
    );

    // The four an operator scans for. "Total" was dropped: it is the sum of
    // the others and told nobody anything they could act on.
    const containers = screen.getByRole("region", { name: /your containers/i });
    expect(within(containers).getByText("Running").nextSibling).toHaveTextContent("2");
    expect(within(containers).getByText("Stopped").nextSibling).toHaveTextContent("1");
    expect(within(containers).getByText("Unhealthy").nextSibling).toHaveTextContent("1");

    // The catalog is telemetry and moved below the disclosure. Still there.
    await openTechnicalDetails();
    const catalog = screen.getByRole("region", { name: /catalog/i });
    expect(within(catalog).getByText("Images").nextSibling).toHaveTextContent("2");
    expect(within(catalog).getByText("Networks").nextSibling).toHaveTextContent("1");
    expect(within(catalog).getByText("Volumes").nextSibling).toHaveTextContent("1");
  });

  it("shows a disconnected state when the backend is unreachable", async () => {
    stubApi({ inventory: new TypeError("Failed to fetch") });
    renderApp();

    await waitFor(() =>
      expect(screen.getByText(/cannot reach the harbormaster backend/i)).toBeInTheDocument(),
    );
  });

  it("shows an error state when the backend rejects the request", async () => {
    stubApi({ inventory: new ApiError("internal_error", "internal error", 500) });
    renderApp();

    // A rejected promise from the stub surfaces as a connectivity failure;
    // either way the operator sees an actionable state rather than a blank page.
    await waitFor(() => expect(screen.getByRole("alert")).toBeInTheDocument());
  });

  it("reports Docker as disconnected without failing the page", async () => {
    stubApi({
      inventory: inventoryStatus({
        docker: { status: "down", detail: "docker engine unreachable" },
      }),
    });
    renderApp();

    await waitFor(() => expect(screen.getByText(/docker engine unreachable/i)).toBeInTheDocument());
    // The rest of the dashboard still renders.
    expect(screen.getByRole("region", { name: /containers/i })).toBeInTheDocument();
  });

  it("renders inventory warnings", async () => {
    stubApi({
      inventory: inventoryStatus({
        warnings: [
          {
            generation: 4,
            containerId: "c1",
            containerName: "web",
            code: "container_vanished",
            message: "container was removed while the inventory was being collected",
            occurredAt: "2026-08-03T09:00:00Z",
          },
        ],
        counts: { ...inventoryStatus().counts, warnings: 1 },
      }),
    });
    renderApp();

    await waitFor(() => expect(screen.getByText(/container_vanished/)).toBeInTheDocument());
    expect(screen.getByText(/was removed while the inventory/i)).toBeInTheDocument();
  });

  it("says so when the inventory engine is disabled", async () => {
    stubApi({ inventory: inventoryStatus({ enabled: false }) });
    renderApp();

    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /your containers/i })).toBeInTheDocument(),
    );
    await openTechnicalDetails();
    expect(
      screen.getByText(/inventory engine is disabled by configuration/i),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /refresh now/i })).toBeDisabled();
  });
});

// -------------------------------------------------------- manual refresh --

describe("manual refresh", () => {
  it("posts to the refresh endpoint and reports that it started", async () => {
    const requests = stubApi();
    const user = userEvent.setup();
    renderApp();

    await openTechnicalDetails();
    await waitFor(() => expect(screen.getByRole("button", { name: /refresh now/i })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: /refresh now/i }));

    await waitFor(() => {
      const refresh = requests.find((request) => request.url.includes("/inventory/refresh"));
      expect(refresh?.method).toBe("POST");
    });
    await waitFor(() => expect(screen.getByRole("status", { name: /refresh status/i })).toHaveTextContent(/refresh started/i));
  });

  it("disables the button while a refresh is running", async () => {
    stubApi({ inventory: inventoryStatus({ inProgress: true, state: "running" }) });
    renderApp();

    await waitFor(() =>
      expect(screen.getByRole("button", { name: /refreshing/i })).toBeDisabled(),
    );
  });

  it("reports a conflict when a refresh is already running", async () => {
    stubApi({
      refresh: {
        status: 409,
        body: {
          error: { code: "conflict", message: "an inventory refresh is already in progress" },
          active: { inProgress: true, startedAt: "2026-08-03T09:30:00Z" },
        },
      },
    });
    const user = userEvent.setup();
    renderApp();

    await openTechnicalDetails();
    await waitFor(() => expect(screen.getByRole("button", { name: /refresh now/i })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: /refresh now/i }));

    await waitFor(() =>
      expect(screen.getByRole("status", { name: /refresh status/i })).toHaveTextContent(/already running/i),
    );
  });

  it("reports that Docker was unreachable rather than claiming success", async () => {
    stubApi({
      refresh: {
        status: 503,
        body: {
          error: { code: "service_unavailable", message: "container runtime is unreachable" },
        },
      },
    });
    const user = userEvent.setup();
    renderApp();

    await openTechnicalDetails();
    await waitFor(() => expect(screen.getByRole("button", { name: /refresh now/i })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: /refresh now/i }));

    await waitFor(() =>
      expect(screen.getByRole("status", { name: /refresh status/i })).toHaveTextContent(/docker is unreachable/i),
    );
  });

  it("reloads metrics after triggering a refresh", async () => {
    const requests = stubApi();
    const user = userEvent.setup();
    renderApp();

    await openTechnicalDetails();
    await waitFor(() => expect(screen.getByRole("button", { name: /refresh now/i })).toBeEnabled());
    const before = requests.filter((request) => request.url.endsWith("/inventory")).length;

    await user.click(screen.getByRole("button", { name: /refresh now/i }));

    await waitFor(() => {
      const after = requests.filter((request) => request.url.endsWith("/inventory")).length;
      expect(after).toBeGreaterThan(before);
    });
  });
});

// ------------------------------------------------------------ containers --

describe("Containers page", () => {
  it("renders a row per container", async () => {
    stubApi({
      containers: containerPage([
        containerSummary({ id: "c1", name: "web" }),
        containerSummary({ id: "c2", name: "db", state: "exited", health: "none" }),
      ]),
    });
    renderContainers();

    await waitFor(() => expect(screen.getByRole("table")).toBeInTheDocument());
    expect(screen.getByRole("link", { name: "web" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "db" })).toBeInTheDocument();

    // Scoped to the table: the state names also appear as filter options.
    const table = screen.getByRole("table");
    expect(within(table).getByText("running")).toBeInTheDocument();
    expect(within(table).getByText("exited")).toBeInTheDocument();
  });

  it("shows an empty state when nothing is recorded", async () => {
    stubApi({ containers: containerPage([], 0) });
    renderContainers();

    await waitFor(() => expect(screen.getByText(/no containers found/i)).toBeInTheDocument());
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("distinguishes an empty filter result from an empty inventory", async () => {
    stubApi({ containers: containerPage([], 0) });
    const user = userEvent.setup();
    renderContainers();

    await waitFor(() => expect(screen.getByLabelText(/search/i)).toBeInTheDocument());
    await user.type(screen.getByLabelText(/search/i), "nomatch");

    await waitFor(() =>
      expect(screen.getByText(/no containers match these filters/i)).toBeInTheDocument(),
    );
  });

  it("shows a disconnected state when the backend is unreachable", async () => {
    stubApi({ containers: new TypeError("Failed to fetch") });
    renderContainers();

    await waitFor(() =>
      expect(screen.getByText(/cannot reach the harbormaster backend/i)).toBeInTheDocument(),
    );
  });

  // The filters must reach the server; nothing is filtered in the browser.
  it("sends the search term to the server", async () => {
    const requests = stubApi();
    const user = userEvent.setup();
    renderContainers();

    await waitFor(() => expect(screen.getByLabelText(/search/i)).toBeInTheDocument());
    await user.type(screen.getByLabelText(/search/i), "web");

    await waitFor(() => expect(lastContainerQuery(requests).get("search")).toBe("web"));
  });

  it("sends the state filter to the server", async () => {
    const requests = stubApi();
    const user = userEvent.setup();
    renderContainers();

    await waitFor(() => expect(screen.getByLabelText(/^state$/i)).toBeInTheDocument());
    await user.selectOptions(screen.getByLabelText(/^state$/i), "exited");

    await waitFor(() => expect(lastContainerQuery(requests).get("state")).toBe("exited"));
  });

  it("sends the health filter to the server", async () => {
    const requests = stubApi();
    const user = userEvent.setup();
    renderContainers();

    await waitFor(() => expect(screen.getByLabelText(/^health$/i)).toBeInTheDocument());
    await user.selectOptions(screen.getByLabelText(/^health$/i), "unhealthy");

    await waitFor(() => expect(lastContainerQuery(requests).get("health")).toBe("unhealthy"));
  });

  it("populates the Compose project filter from the API", async () => {
    const requests = stubApi();
    const user = userEvent.setup();
    renderContainers();

    await waitFor(() =>
      expect(screen.getByRole("option", { name: "shop" })).toBeInTheDocument(),
    );
    await user.selectOptions(screen.getByLabelText(/compose project/i), "shop");

    await waitFor(() => expect(lastContainerQuery(requests).get("project")).toBe("shop"));
  });

  it("sends the sort field and toggles direction", async () => {
    const requests = stubApi();
    const user = userEvent.setup();
    renderContainers();

    // "Sort by State", not "State": the column header's control now says what
    // it DOES, because a screen reader announcing a bare column name gives no
    // hint that activating it reorders the table.
    const sortByState = () => screen.getByRole("button", { name: /^sort by state$/i });

    await waitFor(() => expect(sortByState()).toBeInTheDocument());

    await user.click(sortByState());
    await waitFor(() => {
      const query = lastContainerQuery(requests);
      expect(query.get("sort")).toBe("state");
      expect(query.get("direction")).toBe("asc");
    });

    await user.click(sortByState());
    await waitFor(() => expect(lastContainerQuery(requests).get("direction")).toBe("desc"));
  });

  it("requests the next page from the server", async () => {
    const requests = stubApi({
      containers: {
        items: [containerSummary()],
        pagination: {
          page: 1, pageSize: 25, totalItems: 60, totalPages: 3,
          hasNext: true, hasPrevious: false,
        },
      },
    });
    const user = userEvent.setup();
    renderContainers();

    await waitFor(() => expect(screen.getByRole("button", { name: /next/i })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: /next/i }));

    await waitFor(() => expect(lastContainerQuery(requests).get("page")).toBe("2"));
  });

  it("resets to page 1 when a filter changes", async () => {
    // The stub reports page 1, so "Next" asks for page 2. The component
    // increments from the server-reported page rather than from local state,
    // which is what keeps it correct when the server clamps a page number.
    const requests = stubApi({
      containers: {
        items: [containerSummary()],
        pagination: {
          page: 1, pageSize: 25, totalItems: 60, totalPages: 3,
          hasNext: true, hasPrevious: false,
        },
      },
    });
    const user = userEvent.setup();
    renderContainers();

    await waitFor(() => expect(screen.getByRole("button", { name: /next/i })).toBeInTheDocument());
    await user.click(screen.getByRole("button", { name: /next/i }));
    await waitFor(() => expect(lastContainerQuery(requests).get("page")).toBe("2"));

    await user.type(screen.getByLabelText(/search/i), "x");
    await waitFor(() => {
      const query = lastContainerQuery(requests);
      expect(query.get("search")).toBe("x");
      expect(query.get("page")).toBe("1");
    });
  });

  it("marks containers that are no longer present", async () => {
    stubApi({ containers: containerPage([containerSummary({ present: false })]) });
    renderContainers();

    await waitFor(() => expect(screen.getByText(/no longer present/i)).toBeInTheDocument());
  });
});

// -------------------------------------------------------- container detail --

describe("Container detail", () => {
  it("renders the overview by default", async () => {
    stubApi();
    renderDetail();

    await waitFor(() => expect(screen.getByRole("heading", { name: "web" })).toBeInTheDocument());

    // The overview now leads with what HarborMaster makes of the container
    // rather than with twenty-eight Docker fields. Those are still here, one
    // disclosure down, which the next assertion holds.
    expect(
      screen.getByRole("region", { name: /what harbormaster makes of this container/i }),
    ).toBeInTheDocument();
    expect(screen.getByRole("region", { name: /^image$/i })).toBeInTheDocument();
    expect(
      screen.getByRole("region", { name: /what harbormaster has done here/i }),
    ).toBeInTheDocument();
  });

  it("keeps every Docker-level field, one disclosure down", async () => {
    // The rework moved the low-level state; it removed none of it. An operator
    // who came here for the exit code or the restart policy still finds them.
    const user = userEvent.setup();
    stubApi();
    renderDetail();

    await waitFor(() => expect(screen.getByRole("heading", { name: "web" })).toBeInTheDocument());
    await user.click(screen.getByText(/docker state/i));

    // Scoped to the disclosure: "Running" is also the container's state badge
    // above, and the claim under test is about what the disclosure contains.
    const advanced = screen.getByText(/docker state/i).closest("details");
    if (!advanced) throw new Error("the Docker state disclosure is missing");

    for (const label of [
      // Fields whose value is always present. A `Field` renders nothing at
      // all when its value is undefined, which is itself deliberate: an empty
      // label with no value tells nobody anything.
      "Container ID", "Short ID", "Host", "Image ID", "Created",
      "Restart count", "Restart policy", "Running", "Paused", "Dead",
      "OOM killed", "Inventory generation",
    ]) {
      expect(within(advanced).getByText(label)).toBeInTheDocument();
    }
  });

  it("offers a tab for every documented section", async () => {
    stubApi();
    renderDetail();

    await waitFor(() => expect(screen.getByRole("tablist")).toBeInTheDocument());
    for (const tab of [
      "Overview", "Configuration", "Environment", "Mounts", "Networks",
      "Ports", "Resources", "Security", "Labels", "Compose", "Raw inspection",
    ]) {
      expect(screen.getByRole("tab", { name: tab })).toBeInTheDocument();
    }
  });

  // The central UI guarantee of the masking design.
  it("masks sensitive environment values and offers no way to reveal them", async () => {
    stubApi();
    const user = userEvent.setup();
    renderDetail();

    await waitFor(() => expect(screen.getByRole("tab", { name: "Environment" })).toBeInTheDocument());
    await user.click(screen.getByRole("tab", { name: "Environment" }));

    await waitFor(() => expect(screen.getByText("DB_PASSWORD")).toBeInTheDocument());
    expect(screen.getByText("********")).toBeInTheDocument();
    expect(screen.getByText("masked")).toBeInTheDocument();

    // Non-sensitive values are shown normally.
    expect(screen.getByText("8080")).toBeInTheDocument();

    // No control exists to unmask, by design.
    for (const pattern of [/reveal/i, /show secret/i, /unmask/i, /show value/i]) {
      expect(screen.queryByRole("button", { name: pattern })).not.toBeInTheDocument();
      expect(screen.queryByRole("checkbox", { name: pattern })).not.toBeInTheDocument();
    }
  });

  it("renders mounts, networks, and ports", async () => {
    stubApi();
    const user = userEvent.setup();
    renderDetail();

    await waitFor(() => expect(screen.getByRole("tab", { name: "Mounts" })).toBeInTheDocument());

    await user.click(screen.getByRole("tab", { name: "Mounts" }));
    await waitFor(() => expect(screen.getByText("/data")).toBeInTheDocument());
    expect(screen.getByText("shop_data")).toBeInTheDocument();

    await user.click(screen.getByRole("tab", { name: "Networks" }));
    await waitFor(() => expect(screen.getByText("frontend")).toBeInTheDocument());
    expect(screen.getByText("172.20.0.2")).toBeInTheDocument();

    await user.click(screen.getByRole("tab", { name: "Ports" }));
    await waitFor(() => expect(screen.getByText("127.0.0.1:8080")).toBeInTheDocument());
    // An exposed but unbound port is labelled, not shown as reachable.
    expect(screen.getByText("not published")).toBeInTheDocument();
  });

  it("renders security posture", async () => {
    stubApi();
    const user = userEvent.setup();
    renderDetail();

    await waitFor(() => expect(screen.getByRole("tab", { name: "Security" })).toBeInTheDocument());
    await user.click(screen.getByRole("tab", { name: "Security" }));

    await waitFor(() => expect(screen.getByText("Privileged")).toBeInTheDocument());
    expect(screen.getByText("Read-only root filesystem")).toBeInTheDocument();
    expect(screen.getByText("ALL")).toBeInTheDocument();
  });

  // The raw payload is fetched only when its tab is opened, so a normal page
  // load never carries it.
  it("loads the raw inspection payload lazily and labels it redacted", async () => {
    const requests = stubApi();
    const user = userEvent.setup();
    renderDetail();

    await waitFor(() => expect(screen.getByRole("tab", { name: "Raw inspection" })).toBeInTheDocument());
    expect(requests.some((request) => request.url.includes("/raw"))).toBe(false);

    await user.click(screen.getByRole("tab", { name: "Raw inspection" }));

    await waitFor(() => expect(requests.some((request) => request.url.includes("/raw"))).toBe(true));
    await waitFor(() =>
      expect(screen.getByText(/cannot be used to recreate the container exactly/i)).toBeInTheDocument(),
    );
    expect(screen.getByLabelText(/redacted raw inspection payload/i)).toBeInTheDocument();
  });

  it("shows a not-found state for a missing container", async () => {
    stubApi({ detail: new ApiError("not_found", "container not found", 404) });
    renderDetail("missing");

    await waitFor(() => expect(screen.getByRole("alert")).toBeInTheDocument());
  });

  it("surfaces per-container warnings", async () => {
    stubApi({
      detail: containerDetail({
        warnings: [
          {
            generation: 4,
            code: "inspect_failed",
            message: "container could not be inspected; recorded from summary data only",
            occurredAt: "2026-08-03T09:00:00Z",
          },
        ],
      }),
    });
    renderDetail();

    await waitFor(() => expect(screen.getByText(/inventory warnings/i)).toBeInTheDocument());
    expect(screen.getByText(/recorded from summary data only/i)).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------- images --

describe("Images page", () => {
  it("renders image metadata and reference counts", async () => {
    stubApi();
    render(
      <MemoryRouter initialEntries={["/images"]}>
        <Routes>
          <Route path="/images" element={<Images />} />
        </Routes>
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByRole("table")).toBeInTheDocument());
    expect(screen.getByText("nginx:1.27")).toBeInTheDocument();
    expect(screen.getByText("img1")).toBeInTheDocument();
    expect(screen.getByText("178.3 MiB")).toBeInTheDocument();
    expect(screen.getByText("linux/amd64")).toBeInTheDocument();
    expect(screen.getByText("2")).toBeInTheDocument();
  });

  it("shows an empty state when no images are recorded", async () => {
    stubApi({ images: { items: [], pagination: { page: 1, pageSize: 25, totalItems: 0, totalPages: 0, hasNext: false, hasPrevious: false } } });
    render(
      <MemoryRouter initialEntries={["/images"]}>
        <Routes>
          <Route path="/images" element={<Images />} />
        </Routes>
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText(/no images recorded/i)).toBeInTheDocument());
  });
});

// ----------------------------------------------------------- navigation --

describe("navigation", () => {
  it("includes Images in the primary navigation", async () => {
    stubApi();
    renderApp();

    const nav = await waitFor(() =>
      screen.getByRole("navigation", { name: /primary/i }),
    );
    expect(within(nav).getByRole("link", { name: "Images" })).toBeInTheDocument();

    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /your containers/i })).toBeInTheDocument(),
    );
  });

  it("navigates from a container row to its detail page", async () => {
    stubApi();
    const user = userEvent.setup();
    renderContainers();

    await waitFor(() => expect(screen.getByRole("link", { name: "web" })).toBeInTheDocument());
    await user.click(screen.getByRole("link", { name: "web" }));

    await waitFor(() => expect(screen.getByRole("tablist")).toBeInTheDocument());
  });
});

// Keeps the unused-import checker honest about the recorded-request type.
export type { RecordedRequest };
