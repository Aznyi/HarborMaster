import fs from "node:fs";
import path from "node:path";
import { expect, it } from "vitest";

import { NO_VALUE, formatMoment, formatMomentOrNothing } from "./presentation";

/**
 * One instant, formatted one way.
 *
 * # What this defends
 *
 * By the end of Phase 5 the same timestamp could be rendered by six different
 * pieces of code: four private helpers with the same body, and a scattering of
 * inline `new Date(x).toLocaleString()`. They agreed, which is why it survived
 * five phases -- nothing ever broke to point at it. What differed was the edge:
 * some printed "Invalid Date" at the operator, some printed the raw ISO string,
 * some printed an em dash, and which one you got depended on the page.
 *
 * Phase 6 settled it. These tests pin the two contracts and then guard the
 * consolidation, because a shared formatter that half the pages ignore is worse
 * than no shared formatter: it reads as a rule while not being one.
 */

// ------------------------------------------------------------- behaviour --

it("renders a real instant in the viewer's own locale", () => {
  const value = "2026-08-27T14:05:00Z";
  expect(formatMoment(value)).toBe(new Date(value).toLocaleString());
});

it.each([undefined, null, ""])("renders %p as the no-value dash", (value) => {
  expect(formatMoment(value)).toBe(NO_VALUE);
});

it("never prints 'Invalid Date' at somebody", () => {
  // A malformed timestamp is a bug in what was sent, not something to shout
  // about in the middle of a table.
  expect(formatMoment("not a date")).toBe(NO_VALUE);
  expect(formatMoment("2026-13-45T99:99:99Z")).toBe(NO_VALUE);
});

it("distinguishes 'never sent' from 'sent and unreadable'", () => {
  // The variant for layouts that omit an empty row: absent hides the row,
  // present-but-unreadable keeps it and says so.
  expect(formatMomentOrNothing(undefined)).toBeUndefined();
  expect(formatMomentOrNothing("")).toBeUndefined();
  expect(formatMomentOrNothing("not a date")).toBe(NO_VALUE);
  expect(formatMomentOrNothing("2026-08-27T14:05:00Z")).toBe(
    new Date("2026-08-27T14:05:00Z").toLocaleString(),
  );
});

// ------------------------------------------------------------------ drift --

/** The six destinations the sidebar offers by default, and what they render. */
const NORMAL_DESTINATIONS = [
  "pages/Dashboard.tsx",
  "components/DashboardSummary.tsx",
  "pages/Containers.tsx",
  "pages/ContainerDetail.tsx",
  "components/ContainerOverview.tsx",
  "pages/Updates.tsx",
  "pages/Automation.tsx",
  "components/AutomationWorkspace.tsx",
  "pages/Activity.tsx",
  "pages/Settings.tsx",
  "components/SettingsSections.tsx",
] as const;

function source(relative: string): string {
  return fs
    .readFileSync(path.join(__dirname, "..", relative), "utf8")
    .split("\n")
    .map((line) => line.replace(/\/\/.*$/, ""))
    .join("\n");
}

it.each(NORMAL_DESTINATIONS)(
  "%s formats no timestamp of its own",
  (file) => {
    // `toLocaleString` on a Date is the one this exists to replace.
    expect(source(file)).not.toMatch(/new Date\([^)]*\)\.toLocaleString\(\)/);
  },
);

it.each(NORMAL_DESTINATIONS)("%s declares no private formatter", (file) => {
  expect(source(file)).not.toMatch(
    /function format(Moment|Timestamp|EventTime|Date|Time)\b/,
  );
});
