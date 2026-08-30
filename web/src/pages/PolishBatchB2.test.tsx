import fs from "node:fs";
import path from "node:path";

import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { beforeEach, expect, it, vi } from "vitest";

import { App } from "../App";
import {
  ADVANCED_GROUPS,
  ADVANCED_NAV,
  PRIMARY_NAV,
  visibleNavGroups,
} from "../components/AppShell";
import { SessionProvider } from "../hooks/useSession";
import { setAdvancedTools } from "../hooks/useAdvancedTools";
import { stubApi } from "../test/fixtures";
import { testUser } from "../test/session";
import type { PublicUser } from "../api/authTypes";

/**
 * Polish Batch B2 — presentation only.
 *
 * Four areas, none of which changes what HarborMaster DOES:
 *
 *   1. the seventeen advanced destinations are grouped by subject;
 *   2. Settings can be navigated by section without becoming several routes;
 *   3. an unknown address renders a truthful Not Found instead of the dashboard;
 *   4. two more summary surfaces use the shared compact image formatter.
 *
 * What each group of tests is really defending is that the polish did not
 * quietly become something else: grouping is not disclosure, section links are
 * not routes, a friendly 404 is not a redirect, and compaction is not the loss
 * of a value somebody needs to read.
 */

function renderApp(initialPath = "/") {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <SessionProvider>
        <App />
      </SessionProvider>
    </MemoryRouter>,
  );
}

const sidebar = () =>
  waitFor(() => screen.getByRole("navigation", { name: /primary/i }));

beforeEach(() => {
  vi.unstubAllGlobals();
  // The preference is module state shared across tests in this file, so it is
  // reset through its own setter rather than by reaching into storage.
  setAdvancedTools(false);
  stubApi();
});

// ------------------------------------------------------ 1. advanced nav --

it("shows only the six primary destinations while Advanced is off", async () => {
  renderApp("/");
  const nav = await sidebar();

  expect(within(nav).getAllByRole("link")).toHaveLength(PRIMARY_NAV.length);
  for (const group of ADVANCED_GROUPS) {
    expect(
      within(nav).queryByRole("group", { name: group.heading }),
    ).not.toBeInTheDocument();
  }
});

it("groups the advanced destinations by subject once the preference is on", async () => {
  renderApp("/");
  const nav = await sidebar();
  setAdvancedTools(true);

  for (const group of ADVANCED_GROUPS) {
    const rendered = await within(nav).findByRole("group", { name: group.heading });
    // Every entry appears under ITS OWN group, not merely somewhere.
    for (const item of group.items) {
      expect(
        within(rendered).getByRole("link", { name: item.label }),
      ).toBeInTheDocument();
    }
    expect(within(rendered).getAllByRole("link")).toHaveLength(group.items.length);
  }
});

it("keeps every advanced destination and every URL", () => {
  // Grouping is presentation. Membership, order and targets are untouched, and
  // the flat list is derived from the groups so the two cannot drift.
  expect(ADVANCED_NAV).toHaveLength(17);
  expect(ADVANCED_GROUPS.flatMap((group) => group.items)).toEqual([...ADVANCED_NAV]);
  expect(ADVANCED_NAV.map((item) => item.path)).toEqual([
    "/images",
    "/images/updates",
    "/snapshots",
    "/drift",
    "/plans",
    "/acquisitions",
    "/executions",
    "/rollbacks",
    "/update-policies",
    "/dependencies",
    "/automation/paused",
    "/compliance",
    "/policies",
    "/notifications",
    "/events",
    "/users",
    "/audit",
  ]);
});

it("headings are headings, not navigation", async () => {
  renderApp("/");
  const nav = await sidebar();
  setAdvancedTools(true);
  // Scoped to the Advanced region. "Automation" is both a group heading here
  // and a PRIMARY destination, and the primary one is a real link.
  const region = await within(nav).findByRole("group", { name: /advanced/i });

  for (const group of ADVANCED_GROUPS) {
    // No route, so no link and no button: grouping costs no keystrokes and adds
    // no click on the way to an advanced page.
    expect(
      within(region).queryByRole("link", { name: group.heading }),
    ).not.toBeInTheDocument();
    expect(
      within(region).queryByRole("button", { name: group.heading }),
    ).not.toBeInTheDocument();

    const heading = within(region).getByRole("heading", { name: group.heading });
    expect(heading).not.toHaveAttribute("tabindex");
    expect(heading).not.toHaveAttribute("href");
  }
});

// ------------------------------------------------- permissions per child --

/** An account holding exactly the named permissions. */
function accountWith(...permissions: string[]): PublicUser {
  return {
    ...testUser("administrator"),
    permissions: permissions as PublicUser["permissions"],
  };
}

it("derives group visibility from the children that survive the filter", () => {
  // The example from the brief: no accounts, no audit, but Docker events is
  // readable — so the group appears, holding Docker events alone.
  const groups = visibleNavGroups(accountWith("event:read"));

  expect(groups.map((group) => group.heading)).toEqual([
    "Administration & security",
  ]);
  expect(groups[0]!.items.map((item) => item.label)).toEqual(["Docker events"]);
});

it("renders no empty group heading", () => {
  // An account with one permission from one group must not be shown the other
  // three headings announcing subjects it cannot reach.
  const groups = visibleNavGroups(accountWith("snapshot:read"));

  expect(groups).toHaveLength(1);
  expect(groups[0]!.heading).toBe("Container diagnostics");
  expect(groups[0]!.items.map((item) => item.label)).toEqual(["Restore points"]);
  for (const group of groups) {
    expect(group.items.length).toBeGreaterThan(0);
  }
});

it("shows nothing advanced to an account with none of the permissions", () => {
  expect(visibleNavGroups(accountWith())).toEqual([]);
  expect(visibleNavGroups(null)).toEqual([]);
});

it("still highlights the active route inside a group", async () => {
  renderApp("/snapshots");
  const nav = await sidebar();
  setAdvancedTools(true);

  const link = await within(nav).findByRole("link", { name: "Restore points" });
  await waitFor(() => expect(link).toHaveClass("text-accent"));
});

it("serves the same grouped navigation to the mobile drawer", async () => {
  // One Sidebar renders both: the drawer is the same markup revealed by a CSS
  // transform, so grouping cannot be present on desktop and missing on a phone.
  const user = userEvent.setup();
  renderApp("/");
  const nav = await sidebar();
  setAdvancedTools(true);
  await within(nav).findByRole("group", { name: "Container diagnostics" });

  await user.click(screen.getByRole("button", { name: /menu/i }));

  const drawer = screen.getByRole("navigation", { name: /primary/i });
  for (const group of ADVANCED_GROUPS) {
    expect(
      within(drawer).getByRole("group", { name: group.heading }),
    ).toBeInTheDocument();
  }
});

// ------------------------------------------------------- 2. settings nav --

it("offers in-page section navigation on Settings", async () => {
  renderApp("/settings");

  const nav = await screen.findByRole("navigation", { name: /settings sections/i });
  expect(within(nav).getAllByRole("link").length).toBeGreaterThan(0);
});

it("points every section link at a heading that exists on the page", async () => {
  renderApp("/settings");
  await screen.findByRole("navigation", { name: /settings sections/i });

  const nav = screen.getByRole("navigation", { name: /settings sections/i });
  for (const link of within(nav).getAllByRole("link")) {
    const id = link.getAttribute("href")!.replace(/^#/, "");

    // The target exists, is unique, and is a heading rather than a wrapper.
    const targets = document.querySelectorAll(`#${id}`);
    expect(targets).toHaveLength(1);
    expect(targets[0]!.tagName).toBe("H3");
    // Focusable, so following the link moves the keyboard and not only the
    // viewport.
    expect(targets[0]).toHaveAttribute("tabindex", "-1");
  }
});

it("resolves a direct link with a hash to its section", async () => {
  renderApp("/settings#settings-security");

  await waitFor(() => {
    const target = document.getElementById("settings-security");
    expect(target).not.toBeNull();
    expect(document.activeElement).toBe(target);
  });
});

it("stays one route", async () => {
  // The section links are anchors on the page HarborMaster is already showing.
  // If any of them were a route, the link guard in PolishBatchA would have to
  // resolve it — and Settings would have become six destinations.
  renderApp("/settings");
  await screen.findByRole("navigation", { name: /settings sections/i });

  const nav = screen.getByRole("navigation", { name: /settings sections/i });
  for (const link of within(nav).getAllByRole("link")) {
    expect(link.getAttribute("href")!.startsWith("#")).toBe(true);
  }
});

it("does not pin the section navigation under the header", async () => {
  // A second sticky bar costs vertical space on every scroll of every visit, on
  // the viewport that can least afford it, to save one scroll to the top. The
  // application header is already sticky; this sits in the normal flow.
  renderApp("/settings");
  const nav = await screen.findByRole("navigation", { name: /settings sections/i });

  expect(nav.className).not.toMatch(/\bsticky\b/);
  expect(nav.className).not.toMatch(/\bfixed\b/);
});

it("keeps the Advanced Tools toggle working and persisted", async () => {
  const user = userEvent.setup();
  renderApp("/settings");

  const toggle = await screen.findByRole("checkbox", { name: /show advanced tools/i });
  expect(toggle).not.toBeChecked();

  await user.click(toggle);

  expect(toggle).toBeChecked();
  const nav = await sidebar();
  await within(nav).findByRole("group", { name: "Container diagnostics" });
});

// -------------------------------------------------------- 3. not found ----

it("renders Not Found for an unknown path", async () => {
  renderApp("/nope/not/a/page");

  await screen.findByRole("heading", { name: /page not found/i });
  expect(
    screen.queryByRole("heading", { name: /your containers/i }),
  ).not.toBeInTheDocument();
});

it("keeps the unknown address rather than rewriting it", async () => {
  renderApp("/nope/not/a/page");

  const marker = await screen.findByTestId("not-found-path");
  expect(marker).toHaveAttribute("data-path", "/nope/not/a/page");
});

it("offers a way back to the dashboard", async () => {
  const user = userEvent.setup();
  renderApp("/nope");
  await screen.findByRole("heading", { name: /page not found/i });

  await user.click(screen.getByRole("link", { name: /go to dashboard/i }));

  await waitFor(() =>
    expect(
      screen.getByRole("heading", { name: /your containers/i }),
    ).toBeInTheDocument(),
  );
});

it("does not echo the unknown path into the page text", async () => {
  // The address bar already shows it. Printing it again would add nothing
  // except a place where a crafted URL becomes page content.
  renderApp("/nope/<script>alert(1)</script>");
  await screen.findByRole("heading", { name: /page not found/i });

  const main = screen.getByRole("main");
  expect(main.textContent).not.toContain("alert(1)");
  expect(main.textContent).not.toContain("<script>");
});

it("keeps every known route resolving", async () => {
  // The wildcard changed; nothing before it did. Every advanced destination
  // must still render its own page rather than falling through to Not Found.
  for (const item of [...PRIMARY_NAV, ...ADVANCED_NAV]) {
    const view = render(
      <MemoryRouter initialEntries={[item.path]}>
        <SessionProvider>
          <App />
        </SessionProvider>
      </MemoryRouter>,
    );

    await waitFor(() =>
      expect(view.getByRole("navigation", { name: /primary/i })).toBeInTheDocument(),
    );
    expect(
      view.queryByRole("heading", { name: /page not found/i }),
    ).not.toBeInTheDocument();

    view.unmount();
  }
});

// ------------------------------------------------- 4. image references ----

/**
 * The audit, as tests.
 *
 * Batch A settled the rule and the helper. B2 extends it to two more SUMMARY
 * surfaces and, just as importantly, leaves the technical ones alone. The
 * classification is the interesting part, so both halves are pinned: a surface
 * that should be compact, and the surfaces that must not be.
 */

const DIGEST = "sha256:9c1f2e3a4b5c6d7e8f90112233445566778899aabbccddeeff00112233445566";

it("compacts the digest on the container overview and keeps the whole value", async () => {
  const { ContainerOverview } = await import("../components/ContainerOverview");
  const detail = {
    overview: {
      id: "abcdef0123456789",
      name: "vaultwarden",
      image: {
        raw: `docker.io/vaultwarden/server:1.32.0@${DIGEST}`,
        repository: "docker.io/vaultwarden/server",
        tag: "1.32.0",
        digest: DIGEST,
      },
      state: "running",
      health: "healthy",
    },
    attention: undefined,
  } as never;

  render(
    <MemoryRouter>
      <ContainerOverview detail={detail} />
    </MemoryRouter>,
  );

  const panel = screen
    .getByRole("heading", { name: "Image" })
    .closest("section") as HTMLElement;
  const rendered = within(panel).getByTitle(
    `docker.io/vaultwarden/server:1.32.0@${DIGEST}`,
  );

  // The TAG survives whole -- Batch A's guarantee, and the half an operator
  // actually reads.
  expect(rendered.textContent).toContain("docker.io/vaultwarden/server:1.32.0");
  // Only the digest hex is shortened.
  expect(rendered.textContent).toContain("sha256:9c1f2e3a4b5c…");
  expect(rendered.textContent).not.toContain(DIGEST);
  // And the complete value is still on the element, so nothing is lost.
  expect(rendered).toHaveAttribute("title", `docker.io/vaultwarden/server:1.32.0@${DIGEST}`);
});

it("leaves a reference with no digest exactly as it is", async () => {
  const { ContainerOverview } = await import("../components/ContainerOverview");
  const detail = {
    overview: {
      id: "abcdef0123456789",
      name: "web",
      image: { raw: "nginx:1.27.0", repository: "nginx", tag: "1.27.0" },
      state: "running",
      health: "healthy",
    },
    attention: undefined,
  } as never;

  render(
    <MemoryRouter>
      <ContainerOverview detail={detail} />
    </MemoryRouter>,
  );

  const panel = screen
    .getByRole("heading", { name: "Image" })
    .closest("section") as HTMLElement;
  expect(within(panel).getByText("nginx:1.27.0")).toBeInTheDocument();
  // Nothing was abbreviated, so nothing needs a title to recover.
  expect(within(panel).getByText("nginx:1.27.0")).not.toHaveAttribute("title");
});

it("keeps the technical and safety-relevant surfaces on the full reference", () => {
  const read = (relative: string) =>
    fs.readFileSync(path.join(__dirname, "..", relative), "utf8");

  // The container page's Image SECTION is raw inspection: the record, field by
  // field, and the place the overview's title points somebody who needs it.
  expect(read("pages/ContainerDetail.tsx")).toContain(
    'value={detail.overview.image.raw}',
  );

  // The Images page is the image inventory -- repository tags and repo digests
  // as the daemon reports them. Forensic output, left whole.
  const images = read("pages/Images.tsx");
  expect(images).toContain("usage.image.repoDigests");
  expect(images).not.toContain("formatImageReference");

  // The Apply update confirmation names the digest ON PURPOSE: approving a tag
  // is approving a label, and this is where the exact value matters most.
  const recreate = read("components/RecreateContainerAction.tsx");
  expect(recreate).not.toContain("formatImageReference");
  expect(recreate).toMatch(/acquisition\.target\.(reference|digest)/);
});
