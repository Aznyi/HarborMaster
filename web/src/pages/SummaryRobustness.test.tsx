import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { Drift } from "./Drift";
import { PolicyViolations } from "./PolicyViolations";
import { stubApi } from "../test/fixtures";

/**
 * Summary robustness (C2.1).
 *
 * # The defect these tests exist for
 *
 * Two pages read `summary.bySeverity.critical`, and a third read
 * `destinations.data?.items.length`. Each guarded ONE level and dereferenced the
 * next. Against the real server that is correct -- the store `make()`s
 * `bySeverity` and never omits it, and every list envelope is
 * `required: [items, pagination]` -- so the production assumption was sound.
 *
 * What was not sound was the shared test stub, whose fallback answered EVERY
 * unmodelled endpoint with the `/version` payload. That object is truthy and
 * carries none of those fields, so a page handed it as a summary crashed on a
 * response the server could never send.
 *
 * The failures were INTERMITTENT because the bad render happened when a fetch
 * resolved after its test had already finished. Landing outside the test window
 * makes Vitest report an unhandled error rather than a failure, and which side
 * of that boundary it falls on is a timing race.
 *
 * # What is asserted here
 *
 * That every LEGITIMATE state renders -- complete, empty, loading, failed --
 * and that the fixture can no longer manufacture an illegitimate one.
 */

const originalFetch = globalThis.fetch;
let unhandled: unknown[] = [];

/** Fails the test if a render threw where nobody caught it. */
function captureUnhandled() {
  unhandled = [];
  const onError = (event: ErrorEvent) => {
    unhandled.push(event.error ?? event.message);
  };
  const onRejection = (event: PromiseRejectionEvent) => {
    unhandled.push(event.reason);
  };
  window.addEventListener("error", onError);
  window.addEventListener("unhandledrejection", onRejection);
  return () => {
    window.removeEventListener("error", onError);
    window.removeEventListener("unhandledrejection", onRejection);
  };
}

/** Routes by path fragment, most specific first, 404 otherwise. */
function routeApi(routes: Record<string, unknown>, status = 200) {
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    for (const [fragment, body] of Object.entries(routes)) {
      if (url.includes(fragment)) {
        return new Response(JSON.stringify(body), {
          status,
          headers: { "content-type": "application/json" },
        });
      }
    }
    return new Response(JSON.stringify({ error: { code: "not_found" } }), { status: 404 });
  }) as typeof fetch;
}

const emptyList = {
  items: [],
  pagination: { page: 1, pageSize: 25, totalItems: 0, totalPages: 1, hasNext: false, hasPrevious: false },
};

/** A drift summary with every field the server guarantees. */
const driftSummary = (bySeverity: Record<string, number>) => ({
  total: 0,
  open: 0,
  bySeverity,
  byStatus: {},
  byCategory: {},
  containersWithDrift: 0,
  containersEvaluated: 4,
  incomplete: false,
});

/** A compliance summary with every field the server guarantees. */
const policySummary = (bySeverity: Record<string, number>) => ({
  policies: 2,
  policiesTotal: 2,
  total: 0,
  open: 0,
  bySeverity,
  byStatus: {},
  byRule: {},
  containersEvaluated: 4,
  containersCompliant: 4,
});

const renderDrift = () =>
  render(
    <MemoryRouter initialEntries={["/drift"]}>
      <Routes>
        <Route path="/drift" element={<Drift />} />
      </Routes>
    </MemoryRouter>,
  );

const renderCompliance = () =>
  render(
    <MemoryRouter initialEntries={["/compliance"]}>
      <Routes>
        <Route path="/compliance" element={<PolicyViolations />} />
      </Routes>
    </MemoryRouter>,
  );

let release: () => void;
beforeEach(() => {
  release = captureUnhandled();
});
afterEach(() => {
  release();
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
  // Nothing may have thrown into the void during any of these renders.
  expect(unhandled).toEqual([]);
});

// ------------------------------------------------------- populated state --

it("renders drift with complete data", async () => {
  routeApi({ "/drift/summary": driftSummary({ critical: 2, high: 1 }), "/drift": emptyList });
  renderDrift();
  const region = await screen.findByRole("region", { name: /drift summary/i });
  expect(region).toHaveTextContent("Critical");
  expect(region.textContent).toContain("2");
});

it("renders compliance with complete data", async () => {
  routeApi({ "/policy-summary": policySummary({ critical: 3 }), "/policy-violations": emptyList });
  renderCompliance();
  await waitFor(() => expect(screen.getByText("Critical")).toBeInTheDocument());
  expect(document.body.textContent).toContain("3");
});

// ------------------------------------------- the legitimate empty summary --

it("renders drift when the severity map is legitimately empty", async () => {
  // `bySeverity: {}` is what the server sends for an estate with no findings.
  // It is a LOADED ZERO, not an absence, and must render as zero.
  routeApi({ "/drift/summary": driftSummary({}), "/drift": emptyList });
  renderDrift();

  const region = await screen.findByRole("region", { name: /drift summary/i });
  expect(region).toBeInTheDocument();
  // Zero findings, stated -- not invented, and not a crash.
  expect(screen.getAllByText("0").length).toBeGreaterThan(0);
});

it("renders compliance when the severity map is legitimately empty", async () => {
  routeApi({ "/policy-summary": policySummary({}), "/policy-violations": emptyList });
  renderCompliance();
  await waitFor(() => expect(screen.getAllByText("0").length).toBeGreaterThan(0));
});

// ---------------------------------------------------- loading and failure --

it("does not read the drift summary before it has loaded", async () => {
  // A request that never settles. The page must show its loading state and
  // touch nothing on the response.
  globalThis.fetch = vi.fn(() => new Promise(() => {})) as typeof fetch;
  renderDrift();
  expect(await screen.findAllByRole("status")).not.toHaveLength(0);
});

it("does not read the compliance summary before it has loaded", async () => {
  globalThis.fetch = vi.fn(() => new Promise(() => {})) as typeof fetch;
  renderCompliance();
  expect(await screen.findAllByRole("status")).not.toHaveLength(0);
});

it("renders drift when the summary request fails", async () => {
  // A failed request is NOT zero findings. The page must say so and must not
  // reach into a response it never got.
  routeApi({}, 500);
  renderDrift();
  // The page renders, and the distinction that matters holds: a failed request
  // claims no counts, so the summary region is absent rather than showing zero.
  await waitFor(() => expect(document.body.textContent).toBeTruthy());
  expect(screen.queryByRole("region", { name: /drift summary/i })).not.toBeInTheDocument();
});

it("renders compliance when the summary request fails", async () => {
  routeApi({}, 500);
  renderCompliance();
  await waitFor(() => expect(document.body.textContent).toBeTruthy());
});

// ------------------------------------------------- the fixture-honesty guard --

it("the shared stub never answers an unmodelled endpoint with a success", async () => {
  // THE REGRESSION GUARD.
  //
  // The stub used to answer every unmodelled path with the /version payload --
  // a 200 carrying a shape no other endpoint has. That is what let an
  // impossible response reach a page as though the server had sent it.
  //
  // An unmodelled endpoint must be a 404: loud, and impossible to mistake for
  // data.
  stubApi();

  for (const path of [
    "/api/v1/there-is-no-such-endpoint",
    "/api/v1/made/up/path",
    "/api/v1/widgets",
  ]) {
    const response = await fetch(path);
    expect(response.status).toBe(404);
  }

  // And the endpoints it DOES model answer with their own shape, never with
  // another endpoint's.
  const version = await (await fetch("/api/v1/version")).json();
  expect(version).toHaveProperty("version");
  expect(version).not.toHaveProperty("items");

  const snapshots = await (await fetch("/api/v1/snapshots")).json();
  expect(snapshots).toHaveProperty("items");
  expect(snapshots).toHaveProperty("pagination");
  expect(snapshots).not.toHaveProperty("version");
});

it("the shared stub answers a summary path with a summary, not a list", async () => {
  // THE ORDERING TRAP.
  //
  // `/drift/summary` also contains `/drift`, so a substring router with the
  // list branch first answers it with `{ items, pagination }` -- a list
  // envelope served as a summary, which is exactly the class of lie this batch
  // removed. This asserts the more specific path keeps its own shape.
  stubApi();

  for (const path of ["/api/v1/drift/summary", "/api/v1/policy-summary"]) {
    const body = await (await fetch(path)).json();
    expect(body, `${path} must answer with a summary`).toHaveProperty("bySeverity");
    expect(body, `${path} must not answer with a list envelope`).not.toHaveProperty("items");
    // And the severity map is an object, so `map.critical ?? 0` is safe.
    expect(typeof body.bySeverity).toBe("object");
    expect(body.bySeverity).not.toBeNull();
  }
});
