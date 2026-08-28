import fs from "node:fs";
import path from "node:path";
import { expect, it } from "vitest";

/**
 * One question, one request.
 *
 * # The defect this exists to prevent coming back
 *
 * The Phase 6 request audit measured the dashboard in a real browser and found
 * it opening `GET /api/v1/event-engine` three times and `GET /api/v1/automation`
 * twice on every load. Not a mistake anybody made in one place: the page has a
 * `useDashboardData()` that collects the shared reads, and three panels
 * underneath it had each quietly called the hook again for the same fact.
 *
 * Each of those is a POLLING hook. The duplication was not three extra requests
 * once -- it was three extra requests every five seconds, for as long as the tab
 * stayed open, all answering a question the page had already asked.
 *
 * The fix is that a panel is given what it needs. This guard is source text
 * rather than a render, because the property is about how many times the page
 * OPENS a subscription, and a render test would pass on a page that opened four
 * and displayed one.
 */

const DASHBOARD = fs.readFileSync(
  path.join(__dirname, "Dashboard.tsx"),
  "utf8",
);

/** Source with `//` line comments removed, so a guard cannot match its own prose. */
function code(source: string): string {
  return source
    .split("\n")
    .map((line) => line.replace(/\/\/.*$/, ""))
    .join("\n");
}

const POLLING_HOOKS = [
  "useEventEngine",
  "useAutomationStatus",
  "useInventory",
  "usePlans",
  "useExecutions",
  "useRollbacks",
  "useAcquisitions",
  "useDependencies",
  "usePolicySummary",
  "useDriftSummary",
] as const;

it.each(POLLING_HOOKS)("opens %s exactly once", (hook) => {
  const calls = code(DASHBOARD).match(new RegExp(`\\b${hook}\\(`, "g")) ?? [];
  expect(calls).toHaveLength(1);
});

it("keeps the shared reads together rather than spread through the panels", () => {
  // The single call site for each is inside the collector, so a new panel
  // asking for something already fetched is a visible edit to one function
  // rather than an invisible extra poll.
  const body = code(DASHBOARD);
  const opens = body.indexOf("function useDashboardData()");
  expect(opens).toBeGreaterThan(-1);
  const rest = body.slice(opens);
  const collector = rest.slice(0, rest.indexOf(String.fromCharCode(10) + "}"));
  for (const hook of ["useEventEngine", "useAutomationStatus"]) {
    expect(collector).toContain(`${hook}()`);
  }
});
