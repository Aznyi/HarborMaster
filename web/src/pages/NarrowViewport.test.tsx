import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { SessionProvider } from "../hooks/useSession";
import { ImageUpdates } from "./ImageUpdates";
import { Users } from "./Users";

/**
 * Narrow-viewport regression cover for the two pages that scrolled the page
 * sideways at 390px.
 *
 * # Why these assert on classes rather than on measured width
 *
 * The symptom is `document.scrollWidth > clientWidth`, and jsdom implements no
 * layout — every element measures zero — so the symptom itself is unobservable
 * here. It is asserted in the browser acceptance pass at 390x844, which is what
 * actually proves the pages do not scroll.
 *
 * What these tests can do is pin the MECHANISM, so the specific mistake cannot
 * come back unnoticed:
 *
 *   - `/images/updates` pinned a `max-w-md` block with an unconditional
 *     `shrink-0`, so it held 448px inside a 324px row and pushed the page out.
 *     The floor must not apply below `sm`.
 *   - `/users` put the role select — whose options are whole sentences — in a
 *     flex item that kept the default `min-width: auto`, so it refused to
 *     shrink below its widest option.
 *
 * Both are one-token mistakes that a later edit could reintroduce while every
 * behavioural test stayed green.
 */

const originalFetch = globalThis.fetch;

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

beforeEach(() => {
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes("/images/updates")) {
      return jsonResponse({
        items: [
          {
            id: 1,
            reference: "docker.io/library/nginx:1.27",
            familiar: "nginx:1.27",
            registryKind: "dockerhub",
            registry: "docker.io",
            repository: "library/nginx",
            tag: "1.27",
            localDigest: `sha256:${"a".repeat(64)}`,
            remoteDigest: `sha256:${"b".repeat(64)}`,
            updateType: "digest",
            checkStatus: "ok",
            containerCount: 1,
            pinned: false,
          },
        ],
        pagination: { page: 1, pageSize: 25, totalItems: 1, totalPages: 1 },
        summary: { total: 1, byUpdateType: {}, byCheckStatus: {} },
      });
    }
    if (url.includes("/auth/session")) {
      return jsonResponse({
        user: {
          userId: "usr_0011223344556677889a",
          username: "operator",
          role: "administrator",
          status: "active",
          permissions: ["user:manage"],
        },
        csrfToken: "t".repeat(64),
        expiresAt: "2099-01-01T00:00:00Z",
      });
    }
    if (url.includes("/users")) {
      return jsonResponse({
        items: [
          {
            userId: "usr_0011223344556677889a",
            username: "operator",
            role: "administrator",
            status: "active",
            createdAt: "2026-08-10T12:00:00Z",
          },
        ],
      });
    }
    return jsonResponse({ items: [], pagination: {}, summary: {} });
  }) as typeof fetch;
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

/** The nearest ancestor carrying any of `classes`. */
function ancestorWith(start: Element | null, classes: string[]): Element | null {
  let node: Element | null = start;
  while (node) {
    if (classes.some((c) => node!.classList.contains(c))) return node;
    node = node.parentElement;
  }
  return null;
}

describe("/images/updates at a narrow viewport", () => {
  it("does not pin the digest comparison at a width the row cannot give it", async () => {
    render(
      <MemoryRouter initialEntries={["/images/updates"]}>
        <Routes>
          <Route path="/images/updates" element={<ImageUpdates />} />
        </Routes>
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("nginx:1.27")).toBeInTheDocument());

    // The block wrapping the digest comparison: found by the max-width it
    // carries, which is the thing that made it wide.
    const pinned = document.querySelector(".sm\\:max-w-md, .max-w-md");
    expect(pinned).not.toBeNull();

    // An UNCONDITIONAL shrink-0 is the defect. A responsive one is the fix.
    expect(pinned!.classList.contains("shrink-0")).toBe(false);
    expect(pinned!.className).toContain("sm:shrink-0");
    expect(pinned!.classList.contains("min-w-0")).toBe(true);
  });
});

describe("/users at a narrow viewport", () => {
  it("lets the role select shrink below its widest option", async () => {
    render(
      <SessionProvider>
        <MemoryRouter initialEntries={["/users"]}>
          <Routes>
            <Route path="/users" element={<Users />} />
          </Routes>
        </MemoryRouter>
      </SessionProvider>,
    );

    const select = await screen.findByLabelText(/role/i);

    // The control fills its wrapper rather than sizing to its longest option.
    expect(select.className).toContain("w-full");

    // And the wrapper may shrink: without min-w-0 a flex item keeps
    // `min-width: auto` and refuses to go below its content.
    const wrapper = ancestorWith(select.parentElement, ["min-w-0"]);
    expect(
      wrapper,
      "the role select's flex wrapper needs min-w-0 or the page scrolls sideways",
    ).not.toBeNull();
  });

  it("keeps the accounts table inside a horizontal scroll wrapper", async () => {
    render(
      <SessionProvider>
        <MemoryRouter initialEntries={["/users"]}>
          <Routes>
            <Route path="/users" element={<Users />} />
          </Routes>
        </MemoryRouter>
      </SessionProvider>,
    );

    await waitFor(() => expect(screen.getByText("operator")).toBeInTheDocument());

    // The same containment /containers uses: a wide table scrolls inside its
    // own wrapper rather than scrolling the page.
    const table = document.querySelector("table");
    expect(table).not.toBeNull();
    expect(ancestorWith(table, ["overflow-x-auto"])).not.toBeNull();
  });
});
