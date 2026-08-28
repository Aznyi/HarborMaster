import { render, screen, waitFor, within } from "@testing-library/react";
import fs from "node:fs";
import path from "node:path";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { ADVANCED_NAV, PRIMARY_NAV } from "../components/AppShell";
import { Containers } from "./Containers";
import { containerPage, containerRow } from "../test/fixtures";
import { TestSessionProvider, testSession, testUser } from "../test/session";

/**
 * The post-simplification polish batch.
 *
 * Six semantic problems that survived six phases of consolidation, because each
 * one was correct in the code and wrong in the reading. What is defended here is
 * the READING -- these are presentation properties, and every one of them can be
 * silently undone by an edit that looks like tidying.
 */

const originalFetch = globalThis.fetch;

const SHA =
  "sha256:2b8d1a4f3c9e7b6a5d4c3b2a1908f7e6d5c4b3a29180f7e6d5c4b3a2918f7e6d5";

function stub(rows: ReturnType<typeof containerPage>) {
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(typeof input === "string" ? input : (input as Request).url);
    const json = (body: unknown) =>
      new Response(JSON.stringify(body), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });

    if (url.includes("/inventory/filters")) {
      return json({ images: [], states: [], projects: [] });
    }
    if (url.includes("/containers")) return json(rows);
    return json({
      items: [],
      pagination: {
        page: 1,
        pageSize: 25,
        totalItems: 0,
        totalPages: 1,
        hasNext: false,
        hasPrevious: false,
      },
    });
  }) as typeof fetch;
}

function renderContainers() {
  return render(
    <TestSessionProvider session={testSession({ user: testUser("administrator") })}>
      <MemoryRouter initialEntries={["/containers"]}>
        <Routes>
          <Route path="/containers" element={<Containers />} />
        </Routes>
      </MemoryRouter>
    </TestSessionProvider>,
  );
}

beforeEach(() => {
  stub(containerPage([containerRow()]));
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

// ------------------------------------------- 2. the column says what it is --

it("does not call HarborMaster's own verdict an attention column", async () => {
  /*
   * The column held "Up to date", "Not tracked" and "Not checked" -- the three
   * commonest values on a healthy host -- under a heading that said all three
   * needed a person. It taught operators that the column cries wolf, which is
   * the opposite of what an attention signal is for.
   */
  renderContainers();

  expect(
    await screen.findByRole("columnheader", { name: "HarborMaster" }),
  ).toBeInTheDocument();
  expect(
    screen.queryByRole("columnheader", { name: /needs attention/i }),
  ).not.toBeInTheDocument();
});

it("keeps State, Health and HarborMaster as three separate columns", async () => {
  // Docker's state, the container's own healthcheck, and our verdict are three
  // different facts. Merging any two of them into one ambiguous column would
  // lose the distinction that makes the table worth reading.
  renderContainers();
  await screen.findByRole("columnheader", { name: "HarborMaster" });

  // State and Health are sortable, so their header carries a control; the
  // assessment column is not. Distinctness is the property under test: three
  // headings, three subjects, none merged into the others.
  expect(screen.getByRole("button", { name: "Sort by State" })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Sort by Health" })).toBeInTheDocument();
  expect(
    screen.getByRole("columnheader", { name: "HarborMaster" }),
  ).toBeInTheDocument();
});

// ---------------------------------------------------- 3. the image is read --

it("shows a digest-pinned image compactly, with the whole value on the row", async () => {
  stub(
    containerPage([
      containerRow({
        name: "vaultwarden",
        image: {
          raw: `docker.io/vaultwarden/server@${SHA}`,
          repository: "docker.io/vaultwarden/server",
          digest: SHA,
        },
      }),
    ]),
  );
  renderContainers();

  const cell = await screen.findByText(
    "docker.io/vaultwarden/server@sha256:2b8d1a4f3c9e…",
  );
  // Nothing is lost: the complete reference is on the element, available
  // without leaving the row and without a click.
  expect(cell).toHaveAttribute("title", `docker.io/vaultwarden/server@${SHA}`);
});

it("leaves an ordinary tag alone and adds no tooltip it does not need", async () => {
  stub(
    containerPage([
      containerRow({
        name: "web",
        image: { raw: "nginx:1.27.1", repository: "nginx", tag: "1.27.1" },
      }),
    ]),
  );
  renderContainers();

  const cell = await screen.findByText("nginx:1.27.1");
  expect(cell).not.toHaveAttribute("title");
});

// ------------------------------------ the defect the audit turned up -------

/**
 * Every in-app destination resolves to a registered route.
 *
 * The Automation onboarding block offered "Create update policy" and "Review
 * policies" pointing at `/automation/policies`, which has never been a route.
 * The wildcard sends unknown paths to the dashboard, so all three links looked
 * like they worked: the operator pressed one, landed somewhere plausible, and
 * had no reason to think anything had gone wrong.
 *
 * A source scan rather than a click-through, because the property is about
 * every link in the application and no render exercises them all.
 */
it("points every internal link at a route that exists", () => {
  const src = path.join(__dirname, "..");
  const app = fs.readFileSync(path.join(src, "App.tsx"), "utf8");

  const routes = [...app.matchAll(/path="([^"]+)"/g)].map((m) => m[1] as string);
  expect(routes.length).toBeGreaterThan(20);

  /*
   * A target resolves if some registered route accepts it.
   *
   * Two shapes count. A complete path -- `/plans`, `/containers/abc` -- must
   * match a route end to end. A PREFIX ending in a slash counts too, because
   * several pages build a link by concatenating an id onto one:
   * `"/executions/" + execution.id`. Such a prefix is valid exactly when a
   * route continues it with a parameter.
   */
  const matches = (target: string) =>
    routes.some((route) => {
      if (route === "*") return false;

      // Escape the literal parts, then let each :param match one segment.
      const escaped = route
        .split(/:[^/]+/)
        .map((part) => part.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))
        .join("[^/]+");
      if (new RegExp("^" + escaped + "$").test(target)) return true;

      if (!target.endsWith("/") || !route.includes(":")) return false;
      return route.slice(0, route.indexOf(":")) === target;
    });

  const files: string[] = [];
  const walk = (dir: string) => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      // `api/` holds server endpoint paths and `test/` holds scaffolding.
      // Neither names a page, and both are full of absolute paths that are
      // correct and are not routes.
      if (entry.isDirectory()) {
        if (entry.name !== "api" && entry.name !== "test") {
          walk(path.join(dir, entry.name));
        }
        continue;
      }
      if (/\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name)) {
        files.push(path.join(dir, entry.name));
      }
    }
  };
  walk(src);
  expect(files.length).toBeGreaterThan(30);

  /*
   * Both spellings, because the defect was in the second.
   *
   *   <Link to="/plans">            -- a literal in the JSX
   *   link("/automation/policies")  -- a literal handed to a helper
   *
   * A scan that read only the first found nothing wrong with three dead links.
   */
  const broken: string[] = [];
  for (const file of files) {
    const body = fs.readFileSync(file, "utf8");

    const targets = new Set<string>();
    for (const jsx of body.matchAll(/to="(\/[^"?#]*)[^"]*"/g)) {
      targets.add(jsx[1] as string);
    }
    for (const literal of body.matchAll(/"(\/[a-z][A-Za-z0-9/_-]*)"/g)) {
      targets.add(literal[1] as string);
    }

    for (const target of targets) {
      if (!matches(target)) {
        broken.push(path.relative(src, file) + " -> " + target);
      }
    }
  }

  expect(broken).toEqual([]);
});

// -------------------------------------------------------- 20/21 regression --

it("leaves the six primary destinations and the advanced list untouched", () => {
  expect(PRIMARY_NAV.map((item) => item.label)).toEqual([
    "Dashboard",
    "Containers",
    "Updates",
    "Automation",
    "Activity",
    "Settings",
  ]);
  expect(ADVANCED_NAV).toHaveLength(17);
});

it("makes no mutation request merely by opening the container list", async () => {
  const calls = vi.mocked(globalThis.fetch);
  renderContainers();
  await waitFor(() => expect(calls.mock.calls.length).toBeGreaterThan(0));

  for (const [input, init] of calls.mock.calls) {
    const method =
      (init as RequestInit | undefined)?.method ??
      (typeof input === "object" && "method" in (input as Request)
        ? (input as Request).method
        : "GET");
    expect(method.toUpperCase()).toBe("GET");
  }
});

// ------------------------------------------------ the row still reads well --

it("still shows the verdict badge in the renamed column", async () => {
  stub(containerPage([containerRow({ name: "web" }, { state: "cannotAdvise" })]));
  renderContainers();

  const row = (await screen.findByText("web")).closest("tr") as HTMLElement;
  // Renaming the heading changed no row state.
  expect(within(row).getByText("Cannot determine")).toBeInTheDocument();
});
