import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type {
  ImageIntel,
  ImageIntelSummary,
  ImageUpdateEvent,
  RegistryHealth,
} from "../api/imageTypes";
import { ImageDetailPage } from "./ImageDetail";
import { ImageUpdates } from "./ImageUpdates";

/**
 * Image update UI tests.
 *
 * Three properties matter most and are asserted repeatedly:
 *
 *   - "No updates" and "not checked yet" must READ DIFFERENTLY. Showing an
 *     unchecked estate as up to date is the worst thing this feature could do.
 *   - "Undetermined" must not look like good news. It means HarborMaster could
 *     not tell, which is the opposite of "up to date".
 *   - The refresh control must describe what it does: it schedules a METADATA
 *     read. There must be no control anywhere that pulls or applies anything.
 */

const originalFetch = globalThis.fetch;

let requests: string[] = [];
let writes: { url: string; method: string }[] = [];

/** The encoded image id, as the client actually sends it. */
const encodedImageID = encodeURIComponent("sha256:image1");

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

function pagination(totalItems: number) {
  return {
    page: 1,
    pageSize: 25,
    totalItems,
    totalPages: Math.max(1, Math.ceil(totalItems / 25)),
    hasNext: false,
    hasPrevious: false,
  };
}

function intel(overrides: Partial<ImageIntel> = {}): ImageIntel {
  return {
    id: 1,
    reference: "docker.io/library/nginx:1.25",
    familiar: "nginx:1.25",
    registryKind: "dockerhub",
    registry: "docker.io",
    namespace: "library",
    repository: "library/nginx",
    tag: "1.25",
    localDigest: "sha256:aaa",
    remoteDigest: "sha256:bbb",
    pinned: false,
    imageId: "sha256:image1",
    containerCount: 3,
    updateType: "minor",
    latestTag: "1.26",
    updateReason: "a newer tag is published in the same series",
    checkStatus: "ok",
    firstSeenAt: "2026-08-01T09:00:00Z",
    lastCheckedAt: "2026-08-05T12:00:00Z",
    failureCount: 0,
    ...overrides,
  };
}

function summary(overrides: Partial<ImageIntelSummary> = {}): ImageIntelSummary {
  return {
    images: 10,
    containers: 20,
    updatesAvailable: 3,
    containersAffected: 5,
    byUpdateType: { minor: 2, major: 1 },
    byCheckStatus: { ok: 8, pending: 2 },
    byRegistry: { "docker.io": 7, "ghcr.io": 3 },
    checked: 8,
    pending: 2,
    unsupported: 0,
    lastCheckedAt: "2026-08-05T12:00:00Z",
    ...overrides,
  };
}

function health(overrides: Partial<RegistryHealth> = {}): RegistryHealth {
  return {
    host: "docker.io",
    registryKind: "dockerhub",
    images: 7,
    consecutiveFailures: 0,
    rateLimited: false,
    ...overrides,
  };
}

/**
 * Installs a fetch double routing by path, matched IN THE ORDER GIVEN.
 *
 * Explicit ordering rather than a longest-fragment heuristic: the history URL
 * contains the detail URL as a prefix, so a heuristic cannot tell them apart and
 * would silently answer the wrong one.
 */
function mockApi(routes: [string, unknown][]) {
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString();
    const method = init?.method ?? "GET";
    requests.push(url);
    if (method !== "GET") writes.push({ url, method });

    for (const [fragment, body] of routes) {
      if (url.includes(fragment)) return jsonResponse(body);
    }
    return new Response("{}", { status: 404 });
  }) as typeof fetch;
}

/** The updates dashboard's one route. */
function updatesRoutes(
  items: ImageIntel[],
  overrides: Partial<ImageIntelSummary> = {},
): [string, unknown][] {
  return [
    [
      "/images/updates",
      { items, pagination: pagination(items.length), summary: summary(overrides) },
    ],
  ];
}

/** The detail page's two routes, history FIRST because it is the longer path. */
function detailRoutes(
  intelRecords: ImageIntel[],
  events: ImageUpdateEvent[] = [],
  image: Record<string, unknown> = {},
): [string, unknown][] {
  return [
    [
      `/images/${encodedImageID}/history`,
      {
        imageId: "sha256:image1",
        references: intelRecords.map((record) => record.reference),
        items: events,
        pagination: pagination(events.length),
      },
    ],
    [
      `/images/${encodedImageID}`,
      {
        image: {
          id: "sha256:image1",
          shortId: "image1",
          repoTags: [],
          repoDigests: [],
          size: 0,
          ...image,
        },
        containerCount: 3,
        intel: intelRecords,
      },
    ],
  ];
}

beforeEach(() => {
  requests = [];
  writes = [];
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

function renderUpdates() {
  return render(
    <MemoryRouter initialEntries={["/images/updates"]}>
      <Routes>
        <Route path="/images/updates" element={<ImageUpdates />} />
      </Routes>
    </MemoryRouter>,
  );
}

function renderDetail() {
  return render(
    <MemoryRouter initialEntries={["/images/sha256:image1"]}>
      <Routes>
        <Route path="/images/:id" element={<ImageDetailPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

// ------------------------------------------------------------- dashboard --

describe("Image updates dashboard", () => {
  it("renders the summary cards and an image with an update", async () => {
    mockApi(updatesRoutes([intel()]));
    renderUpdates();

    expect(await screen.findByText("nginx:1.25")).toBeInTheDocument();
    expect(screen.getByText("Updates available")).toBeInTheDocument();
    expect(screen.getByText(/5 containers affected/)).toBeInTheDocument();
    // The newer tag is named, because that is the actionable part.
    expect(screen.getByText(/Newer tag available/)).toBeInTheDocument();
    expect(screen.getByText("1.26")).toBeInTheDocument();
  });

  // THE DISTINCTION THE FEATURE TURNS ON.
  it("says how many images have not been checked", async () => {
    mockApi(updatesRoutes([], { images: 10, checked: 2, pending: 8 }));
    renderUpdates();

    expect(
      await screen.findByText(/8 of 10 tracked images have not been checked/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/not yet a complete picture of the estate/),
    ).toBeInTheDocument();
  });

  it("does not warn about coverage when everything has been checked", async () => {
    mockApi(updatesRoutes([], { images: 10, checked: 10, pending: 0 }));
    renderUpdates();
    await screen.findByText(/No images match these filters/);

    expect(screen.queryByText(/have not been checked/)).toBeNull();
  });

  // "Undetermined" must not read as good news, and must explain itself.
  it("distinguishes undetermined from up to date", async () => {
    mockApi(
      updatesRoutes([
        intel({
          updateType: "unknown",
          latestTag: undefined,
          updateReason:
            "the tag listing exceeded its budget, so a newer tag may exist",
        }),
      ]),
    );
    renderUpdates();

    // Scoped to the LIST: "Up to date" is also a filter <option>, and the
    // assertion is about what the row says rather than what the dropdown
    // offers.
    const row = (await screen.findByText("nginx:1.25")).closest("li");
    expect(row).not.toBeNull();
    expect(within(row as HTMLElement).getByText("Undetermined")).toBeInTheDocument();
    expect(within(row as HTMLElement).queryByText("Up to date")).toBeNull();

    expect(
      screen.getByText(/tag listing exceeded its budget/),
    ).toBeInTheDocument();
  });

  it("defaults to showing only images with updates", async () => {
    mockApi(updatesRoutes([]));
    renderUpdates();
    await screen.findByText(/No images match these filters/);

    const listRequest = requests.find((url) => url.includes("/images/updates"));
    expect(listRequest).toContain("updatesOnly=true");
  });

  // The refresh must describe what it does: it reads metadata, it does not pull.
  it("requests a metadata refresh without claiming to pull anything", async () => {
    mockApi([
      ["/images/refresh", { requested: true, engine: { enabled: true } }],
      ...updatesRoutes([]),
    ]);
    renderUpdates();

    const button = await screen.findByRole("button", { name: "Check for updates" });
    expect(button).toHaveAttribute("title", expect.stringContaining("Reads only"));
    expect(button).toHaveAttribute("title", expect.stringContaining("queued"));

    await userEvent.click(button);

    await waitFor(() =>
      expect(writes.some((w) => w.url.includes("/images/refresh"))).toBe(true),
    );
    expect(writes.find((w) => w.url.includes("/images/refresh"))?.method).toBe("POST");
  });

  // There must be no control that pulls, updates, or removes an image.
  it("offers no mutation control", async () => {
    mockApi(updatesRoutes([intel()]));
    renderUpdates();
    await screen.findByText("nginx:1.25");

    for (const label of [/pull/i, /apply/i, /upgrade/i, /delete/i, /prune/i]) {
      expect(screen.queryByRole("button", { name: label })).toBeNull();
    }
  });
});

// ------------------------------------------------------- registry status --

describe("Registry status", () => {
  it("shows each registry and how many images it serves", async () => {
    mockApi(
      updatesRoutes([], {
        registries: [
          health(),
          health({ host: "ghcr.io", registryKind: "ghcr", images: 3 }),
        ],
      }),
    );
    renderUpdates();

    const status = await screen.findByRole("region", { name: "Registry status" });
    expect(within(status).getByText("docker.io")).toBeInTheDocument();
    expect(within(status).getByText("ghcr.io")).toBeInTheDocument();
    expect(within(status).getAllByText("reachable")).toHaveLength(2);
  });

  // Attribution: "updates are stale because the registry is rate-limiting us"
  // is a very different situation from "there are no updates".
  it("attributes staleness to a rate-limited registry", async () => {
    mockApi(
      updatesRoutes([], {
        registries: [health({ rateLimited: true, consecutiveFailures: 3 })],
      }),
    );
    renderUpdates();

    expect(await screen.findByText("rate limited")).toBeInTheDocument();
    expect(
      screen.getByText(/may be out of date\. Previously discovered updates are retained/),
    ).toBeInTheDocument();
  });

  it("distinguishes an outage from a rate limit", async () => {
    mockApi(
      updatesRoutes([], {
        registries: [health({ consecutiveFailures: 5, rateLimited: false })],
      }),
    );
    renderUpdates();

    expect(await screen.findByText("unreachable")).toBeInTheDocument();
    expect(screen.queryByText("rate limited")).toBeNull();
  });
});

// ---------------------------------------------------------- image detail --

describe("Image detail", () => {
  it("shows the local image beside what the registry reports", async () => {
    mockApi(
      detailRoutes([intel()], [], {
        repoTags: ["nginx:1.25"],
        os: "linux",
        architecture: "amd64",
      }),
    );
    renderDetail();

    // Both are section headings. "Registry" also appears as a field label
    // inside the digest comparison, so the headings are matched by role.
    expect(await screen.findByRole("heading", { name: "Local" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Registry" })).toBeInTheDocument();
    expect(screen.getByText("linux/amd64")).toBeInTheDocument();
    expect(screen.getAllByText(/nginx:1\.25/).length).toBeGreaterThan(0);
  });

  // An image with no intelligence is not an image with no updates.
  it("says nothing has been asked rather than implying no updates", async () => {
    mockApi(detailRoutes([]));
    renderDetail();

    expect(await screen.findByText("No registry information")).toBeInTheDocument();
    expect(screen.getByText(/not the same as having no updates/)).toBeInTheDocument();
  });

  it("renders the history timeline", async () => {
    const event: ImageUpdateEvent = {
      id: 1,
      reference: "docker.io/library/nginx:1.25",
      observedAt: "2026-08-05T12:00:00Z",
      kind: "digestChanged",
      previousDigest: "sha256:old",
      currentDigest: "sha256:new",
      checkStatus: "ok",
      detail: "the registry now serves a different digest",
    };

    mockApi(detailRoutes([intel()], [event]));
    renderDetail();

    // Written as a statement about the world, not as engine vocabulary.
    expect(
      await screen.findByText("The publisher republished this tag"),
    ).toBeInTheDocument();
    expect(screen.getByText("sha256:old")).toBeInTheDocument();
    expect(screen.getByText("sha256:new")).toBeInTheDocument();
  });

  it("explains an empty history rather than leaving it blank", async () => {
    mockApi(detailRoutes([intel()]));
    renderDetail();

    expect(await screen.findByText("Nothing has changed yet")).toBeInTheDocument();
    expect(
      screen.getByText(/a check that found everything unchanged writes nothing/),
    ).toBeInTheDocument();
  });

  // A publisher-supplied URL is third-party content. Rendering it as a link
  // would hand a registry a way to place one in HarborMaster's UI.
  it("renders a publisher source as text rather than a link", async () => {
    mockApi(detailRoutes([intel({ source: "https://evil.example.com/payload" })]));
    renderDetail();

    const source = await screen.findByText("https://evil.example.com/payload");
    expect(source.closest("a")).toBeNull();
    // Asserted with a DOM selector rather than a name matcher.
    //
    // The obvious spellings -- an unanchored regex, or `includes` on the
    // accessible name -- are both the shape static analysis flags as a
    // URL-validation mistake, because in production code they WOULD be one.
    // Here the intent is the opposite of validation: prove no anchor anywhere
    // points at this host. An attribute selector says exactly that.
    expect(document.querySelector('a[href*="evil.example.com"]')).toBeNull();
  });
});
