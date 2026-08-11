import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { Containers } from "./Containers";

/**
 * Sortable column semantics on the containers table.
 *
 * Three defects lived in this one block of markup, and each is pinned below.
 *
 *  1. `aria-sort` sat on the <button> inside the header rather than on the
 *     <th>. The attribute is only defined for a row or column header, so axe
 *     reported an unsupported-attribute violation and a screen reader reading
 *     the header got no sort state at all.
 *  2. The sort indicator was written into the source as a literal triangle,
 *     which at some point was saved as UTF-8 and read back as CP1252. The
 *     corrupted three-character sequence rendered on every sortable column.
 *  3. The indicator was drawn for EVERY column using the ACTIVE column's
 *     direction, and merely hidden with opacity. An inactive column therefore
 *     carried a hidden arrow that contradicted its own state, and any change
 *     to that opacity rule would have exposed it.
 */

const originalFetch = globalThis.fetch;

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

function container(name: string) {
  return {
    id: name.padEnd(64, "0"),
    shortId: name.slice(0, 12),
    name,
    image: { raw: "nginx:1.27.0", registry: "docker.io", repository: "library/nginx", tag: "1.27.0" },
    imageId: "sha256:" + "a".repeat(64),
    state: "running",
    status: "running",
    health: "healthy",
    present: true,
    restartCount: 0,
    restartPolicy: { name: "unless-stopped" },
    compose: {},
    harbormaster: {},
    ports: [],
    lastSeenAt: "2026-08-10T12:00:00Z",
    createdAt: "2026-08-10T11:00:00Z",
  };
}

beforeEach(() => {
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes("/containers")) {
      return jsonResponse({
        items: [container("web"), container("api")],
        pagination: { page: 1, pageSize: 25, totalItems: 2, totalPages: 1, hasNext: false, hasPrevious: false },
      });
    }
    return jsonResponse({ projects: [], images: [] });
  }) as typeof fetch;
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

function renderTable() {
  return render(
    <MemoryRouter>
      <Containers />
    </MemoryRouter>,
  );
}

/** The <th> for a column, found through its sort control. */
function headerFor(label: string): HTMLElement {
  const button = screen.getByRole("button", { name: `Sort by ${label}` });
  const th = button.closest("th");
  if (!th) throw new Error(`no <th> wraps the "${label}" sort control`);
  return th;
}

describe("sortable column headers", () => {
  it("puts aria-sort on the column header, never on the button", async () => {
    renderTable();
    await waitFor(() => expect(screen.getByRole("button", { name: "Sort by Name" })).toBeInTheDocument());

    const button = screen.getByRole("button", { name: "Sort by Name" });
    expect(
      button.hasAttribute("aria-sort"),
      "aria-sort is only defined for a row or column header; on a button it is " +
        "an unsupported attribute and conveys nothing",
    ).toBe(false);

    expect(headerFor("Name")).toHaveAttribute("aria-sort", "ascending");
  });

  it("reports every other sortable column as unsorted rather than guessing", async () => {
    renderTable();
    await waitFor(() => expect(screen.getByRole("button", { name: "Sort by State" })).toBeInTheDocument());

    for (const label of ["State", "Health", "Image"]) {
      expect(headerFor(label)).toHaveAttribute("aria-sort", "none");
    }
  });

  it("gives a column the server cannot order on no sort state at all", async () => {
    // "Needs attention" and "Update" are computed per row, not ordered by the
    // database. `aria-sort="none"` on them would announce a sortable column
    // that is not currently sorted, and there is no way to sort it -- so the
    // attribute is absent rather than dishonest, and the header carries no
    // control to press.
    renderTable();
    await waitFor(() => expect(screen.getByRole("button", { name: "Sort by Name" })).toBeInTheDocument());

    for (const label of ["Needs attention", "Update", "Ports"]) {
      const header = screen.getByRole("columnheader", { name: label });
      expect(header).not.toHaveAttribute("aria-sort");
      expect(within(header).queryByRole("button")).not.toBeInTheDocument();
    }
  });

  it("names the control by what it does", async () => {
    renderTable();
    await waitFor(() => expect(screen.getByRole("button", { name: "Sort by Name" })).toBeInTheDocument());

    // The accessible name describes the ACTION. The state lives on the header,
    // so a screen reader announces it once rather than twice.
    expect(screen.getByRole("button", { name: "Sort by Health" })).toBeInTheDocument();
  });

  it("moves the sort when a header is activated, by keyboard", async () => {
    const user = userEvent.setup();
    renderTable();
    await waitFor(() => expect(screen.getByRole("button", { name: "Sort by Name" })).toBeInTheDocument());

    const health = screen.getByRole("button", { name: "Sort by Health" });
    health.focus();
    expect(health).toHaveFocus();
    await user.keyboard("{Enter}");

    await waitFor(() => expect(headerFor("Health")).toHaveAttribute("aria-sort", "ascending"));
    expect(headerFor("Name")).toHaveAttribute("aria-sort", "none");
  });

  it("toggles direction on the active column", async () => {
    const user = userEvent.setup();
    renderTable();
    await waitFor(() => expect(screen.getByRole("button", { name: "Sort by Name" })).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: "Sort by Name" }));
    await waitFor(() => expect(headerFor("Name")).toHaveAttribute("aria-sort", "descending"));
  });
});

describe("the sort indicator", () => {
  it("renders the intended triangle, not a corrupted sequence", async () => {
    renderTable();
    await waitFor(() => expect(screen.getByRole("button", { name: "Sort by Name" })).toBeInTheDocument());

    const text = headerFor("Name").textContent ?? "";

    // U+25B2 BLACK UP-POINTING TRIANGLE, written in the source as an escape so
    // that re-encoding the file cannot corrupt it.
    expect(text).toContain("▲");

    // The exact bytes the defect produced: U+00E2 U+2013 U+00B2.
    expect(text).not.toContain("â");
    expect(text).not.toContain("–²");
  });

  it("draws an indicator only on the column actually sorted", async () => {
    renderTable();
    await waitFor(() => expect(screen.getByRole("button", { name: "Sort by Name" })).toBeInTheDocument());

    expect(headerFor("Name").textContent).toContain("▲");
    for (const label of ["State", "Health", "Image"]) {
      const text = headerFor(label).textContent ?? "";
      expect(
        text.includes("▲") || text.includes("▼"),
        `${label} is not the sorted column and must not carry a direction indicator`,
      ).toBe(false);
    }
  });

  it("flips the indicator with the direction", async () => {
    const user = userEvent.setup();
    renderTable();
    await waitFor(() => expect(screen.getByRole("button", { name: "Sort by Name" })).toBeInTheDocument());

    expect(headerFor("Name").textContent).toContain("▲");
    await user.click(screen.getByRole("button", { name: "Sort by Name" }));
    await waitFor(() => expect(headerFor("Name").textContent).toContain("▼"));
  });

  it("hides the indicator from assistive technology", async () => {
    renderTable();
    await waitFor(() => expect(screen.getByRole("button", { name: "Sort by Name" })).toBeInTheDocument());

    // The state is conveyed by aria-sort; the glyph is decoration and must not
    // be read out as a character name.
    const decoration = within(headerFor("Name")).getByText("▲");
    expect(decoration).toHaveAttribute("aria-hidden", "true");
  });
});
