import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { beforeEach, expect, it, vi } from "vitest";

import { App } from "../App";
import { ADVANCED_NAV, AppShell, PRIMARY_NAV } from "./AppShell";
import { SessionProvider } from "../hooks/useSession";
import { setAdvancedTools } from "../hooks/useAdvancedTools";
import { stubApi } from "../test/fixtures";

/**
 * The Phase 1 information architecture.
 *
 * # What this file is defending
 *
 * The sidebar used to name twenty-two destinations, most of them a stage of
 * HarborMaster's update lifecycle rather than a thing an operator sets out to
 * do. The default is now six. Everything else moved behind a preference.
 *
 * The properties that have to hold:
 *
 *   - the default sidebar is the six, and nothing else;
 *   - the specialised pages are still ROUTABLE, whether or not they are listed.
 *     Hiding a link has never been the access control here, and a bookmark
 *     someone made last year must still work;
 *   - showing them is a preference and not a grant: the permission filter is
 *     the same filter, applied to the same list;
 *   - the section landings are signposts, not second implementations.
 */

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <SessionProvider>
        <App />
      </SessionProvider>
    </MemoryRouter>,
  );
}

/** A health resource that never resolves; the shell only needs the shape. */
const loadingHealth = {
  status: "loading",
  data: null,
  error: null,
  refresh: () => {},
  refreshing: false,
} as unknown as Parameters<typeof AppShell>[0]["health"];

const nav = () =>
  waitFor(() => screen.getByRole("navigation", { name: /primary/i }));

/**
 * Answers every endpoint with an empty collection.
 *
 * The shared fixture in `test/fixtures` models the endpoints each PAGE test
 * needs; this file mounts eighteen pages and cares about none of their data, so
 * it layers a catch-all underneath. Without it a page whose endpoint the shared
 * fixture does not model throws on `data.items`, and a routing test fails for a
 * reason that has nothing to do with routing.
 */
function stubEveryEndpoint() {
  const requests = stubApi();
  const inner = globalThis.fetch as unknown as (
    input: RequestInfo | URL,
    init?: RequestInit,
  ) => Promise<Response>;

  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const response = await inner(input, init);
      if (response.status !== 404) return response;

      // Not modelled by the shared fixture. An empty page is the right shape
      // for every list endpoint in this application.
      return new Response(
        JSON.stringify({
          items: [],
          // Several list pages render a severity roll-up above the table. An
          // empty one is the honest shape for "no rows".
          summary: { bySeverity: {}, byState: {}, total: 0 },
          pagination: {
            page: 1,
            pageSize: 25,
            totalItems: 0,
            totalPages: 0,
            hasNext: false,
            hasPrevious: false,
          },
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }),
  );
  return requests;
}

beforeEach(() => {
  vi.unstubAllGlobals();
  stubApi();
});

// ------------------------------------------------------------- the default --

it("lists six primary destinations and no lifecycle internals", async () => {
  renderAt("/");
  const sidebar = await nav();

  const links = within(sidebar)
    .getAllByRole("link")
    .map((link) => link.textContent?.trim());

  expect(links).toEqual([
    "Dashboard",
    "Containers",
    "Updates",
    "Automation",
    "Activity",
    "Settings",
  ]);
  expect(PRIMARY_NAV).toHaveLength(6);
});

it("does not list the advanced tools by default", async () => {
  renderAt("/");
  const sidebar = await nav();

  for (const item of ADVANCED_NAV) {
    expect(
      within(sidebar).queryByRole("link", { name: item.label }),
    ).not.toBeInTheDocument();
  }
  expect(
    within(sidebar).queryByRole("group", { name: /advanced/i }),
  ).not.toBeInTheDocument();
});

// ---------------------------------------------------------- advanced tools --

it("lists the advanced tools once the preference is on", async () => {
  renderAt("/");
  const sidebar = await nav();

  setAdvancedTools(true);

  const group = await within(sidebar).findByRole("group", { name: /advanced/i });
  for (const item of ADVANCED_NAV) {
    expect(
      within(group).getByRole("link", { name: item.label }),
    ).toBeInTheDocument();
  }
});

it("is a density preference, not a permission grant", async () => {
  // An administrator holds everything, so the list is complete. The property
  // under test is that the SAME filter runs either way -- Session.test.tsx
  // covers the viewer, who is offered none of the administrative entries even
  // with the section expanded.
  renderAt("/");
  const sidebar = await nav();
  setAdvancedTools(true);

  const group = await within(sidebar).findByRole("group", { name: /advanced/i });
  expect(within(group).getAllByRole("link")).toHaveLength(ADVANCED_NAV.length);
});

it("can be turned on from Settings", async () => {
  const user = userEvent.setup();
  renderAt("/settings");

  const sidebar = await nav();
  const toggle = await screen.findByRole("checkbox", {
    name: /show advanced tools/i,
  });
  expect(toggle).not.toBeChecked();

  await user.click(toggle);

  expect(
    await within(sidebar).findByRole("link", { name: "Snapshots" }),
  ).toBeInTheDocument();
});

// -------------------------------------------------------------- deep links --

it("names every specialised destination at its own URL", async () => {
  // The shell titles the page from the MATCHED path, so this proves each
  // advanced route is still a route the application knows -- without mounting
  // eighteen pages and their data, which is what their own test files do.
  for (const item of ADVANCED_NAV) {
    const { unmount } = render(
      <MemoryRouter initialEntries={[item.path]}>
        <SessionProvider>
          <AppShell health={loadingHealth}>
            <p>page</p>
          </AppShell>
        </SessionProvider>
      </MemoryRouter>,
    );
    expect(
      await screen.findByRole("heading", { name: item.label, level: 1 }),
    ).toBeInTheDocument();
    unmount();
  }
});

it("still renders specialised pages by URL, unlisted or not", async () => {
  // The compatibility guarantee, exercised end to end on a representative
  // sample: nothing was redirected or removed when it left the sidebar.
  //
  // A sample rather than all eighteen: each page has its own test file that
  // mounts it with the data it actually needs, and the test above already
  // proves every advanced path is a route this application resolves.
  stubEveryEndpoint();

  const cases: [string, string][] = [
    ["/snapshots", "Snapshots"],
    ["/rollbacks", "Rollbacks"],
    ["/images", "Images"],
    ["/dependencies", "Update dependencies"],
  ];

  for (const [path, title] of cases) {
    const { unmount } = renderAt(path);
    expect(
      await screen.findByRole("heading", { name: title, level: 1 }),
    ).toBeInTheDocument();
    // And it is the page, not the catch-all redirect to the dashboard.
    expect(
      screen.queryByRole("heading", { name: "Dashboard", level: 1 }),
    ).not.toBeInTheDocument();
    unmount();
  }
});

it("keeps the section highlighted on a nested route", async () => {
  renderAt("/containers");
  const sidebar = await nav();

  const containers = within(sidebar).getByRole("link", { name: "Containers" });
  await waitFor(() => expect(containers).toHaveAttribute("aria-current", "page"));

  // The dashboard is the one link with `end`, so a section route must not also
  // light it up.
  expect(within(sidebar).getByRole("link", { name: "Dashboard" })).not.toHaveAttribute(
    "aria-current",
  );
});

// --------------------------------------------------------- the landings --

it("routes /updates to the consolidated workspace, not a landing page", async () => {
  // Phase 2 replaced the transitional landing. The property Phase 1 cared
  // about -- that the sidebar entry leads somewhere useful -- now means the
  // workspace itself rather than a menu of five other pages.
  renderAt("/updates");

  const main = await screen.findByRole("main");
  expect(
    within(main).getByRole("heading", { name: "Updates", level: 2 }),
  ).toBeInTheDocument();
  expect(
    within(main).getByRole("tablist", { name: /update views/i }),
  ).toBeInTheDocument();

  // And it is not the old signpost: no card linking onwards to a separate
  // "Update reviews" application.
  expect(
    within(main).queryByRole("link", { name: /^Update reviews$/i }),
  ).not.toBeInTheDocument();
});

it("routes the activity landing the same way", async () => {
  renderAt("/activity");

  const main = await screen.findByRole("main");
  expect(
    within(main).getByRole("heading", { name: "Activity", level: 2 }),
  ).toBeInTheDocument();
  expect(within(main).getByRole("link", { name: /events/i })).toHaveAttribute(
    "href",
    "/events",
  );
});

// ------------------------------------------------------------------ header --

it("summarises system health and keeps both states reachable", async () => {
  renderAt("/");

  // The summary is on the face of it.
  const status = await screen.findByTestId("system-status");
  await waitFor(() =>
    expect(within(status).getByText(/all systems connected/i)).toBeInTheDocument(),
  );

  // And neither underlying state was removed.
  expect(within(status).getByText(/backend: connected/i)).toBeInTheDocument();
  expect(within(status).getByText(/docker: connected/i)).toBeInTheDocument();
});

it("puts the account and sign-out behind one control", async () => {
  renderAt("/");

  const menu = await screen.findByTestId("account-menu");
  expect(
    within(menu).getByRole("link", { name: /your account/i }),
  ).toHaveAttribute("href", "/account");
  expect(
    within(menu).getByRole("button", { name: /sign out/i }),
  ).toBeInTheDocument();
});
