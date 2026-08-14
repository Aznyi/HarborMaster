import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

import {
  AUTOMATION_REASON_DETAILS,
  AUTOMATION_REASON_LABELS,
  automationReasonIsDependency,
  automationReasonIsFailure,
  type AutomationReason,
} from "./automationTypes";
import {
  dependencyKindLabel,
  dependencyOriginLabel,
  dependencyStateLabel,
  discoveryRefusalLabel,
  memberStateLabel,
  type DependencyMemberState,
  type DependencySource,
  type DependencyState,
  type DiscoveryRefusal,
} from "./dependencyTypes";
import {
  NOTIFICATION_EVENT_GROUPS,
  NOTIFICATION_EVENT_LABELS,
} from "./notificationTypes";
import { ATTENTION_LABELS, ATTENTION_MEANINGS } from "../components/AttentionBadges";
import type { AttentionState } from "./inventoryTypes";

/**
 * The closed vocabularies, pinned against the published schema.
 *
 * # The defect this exists to stop happening again
 *
 * Every vocabulary below is rendered by mapping each value to a sentence, and
 * every one of those maps falls back to the RAW VALUE when it does not know a
 * key. That is not hypothetical. Phase 16 added seven automation reasons in Go
 * and left both the schema and this frontend behind, so an operator whose
 * container was held by a dependency read `dependencyWaiting` in the Automation
 * page — the exact failure the closed-vocabulary design exists to prevent.
 *
 * TypeScript cannot catch it: the union and the map agree with each other and
 * both are wrong together. The server is the authority, `api/openapi.yaml` is
 * the published statement of what the server sends, and this reads that file.
 *
 * The Go side pins itself against the same file in
 * internal/api/vocabulary_openapi_test.go, so the schema is the single place
 * the two agree rather than a third copy to keep in step.
 */

const SCHEMA = resolve(__dirname, "..", "..", "..", "api", "openapi.yaml");

/**
 * Extracts one schema's `enum:` list.
 *
 * Text rather than a YAML parser: the enums are flat scalar lists under a known
 * key, and a parser dependency for one test is a supply-chain addition. The
 * extraction is guarded by its own non-vacuity check below.
 */
function enumValues(schema: string): string[] {
  const lines = readFileSync(SCHEMA, "utf8").split(/\r?\n/);
  const values: string[] = [];

  let inSchema = false;
  let inEnum = false;
  for (const line of lines) {
    if (line.startsWith(`    ${schema}:`)) {
      inSchema = true;
      continue;
    }
    if (!inSchema) continue;

    // A new top-level schema ends this one.
    if (/^ {4}\S.*:\s*$/.test(line)) break;

    const trimmed = line.trim();
    if (trimmed.startsWith("enum:")) {
      const rest = trimmed.slice("enum:".length).trim();
      if (rest.startsWith("[")) {
        for (const value of rest.replace(/[[\]]/g, "").split(",")) {
          if (value.trim()) values.push(value.trim());
        }
        break;
      }
      inEnum = true;
      continue;
    }
    if (inEnum) {
      if (!trimmed.startsWith("- ")) break;
      values.push(trimmed.slice(2).trim());
    }
  }
  return values;
}

/**
 * Asserts a frontend map covers exactly one published vocabulary.
 *
 * Both directions matter. A missing key renders the raw enum to an operator; an
 * extra key is a value no server sends, which means dead wording nobody will
 * ever see and nobody will ever notice is wrong.
 */
function pinnedTo(schema: string, keys: string[]) {
  const published = enumValues(schema);

  // Without this, a typo in the schema name compares two empty sets and the
  // check passes having verified nothing.
  expect(
    published.length,
    `no enum values were extracted for ${schema}; this check would pass vacuously`,
  ).toBeGreaterThan(0);

  expect([...keys].sort()).toEqual([...published].sort());
}

describe("the published vocabularies", () => {
  it("covers every automation reason with a label", () => {
    pinnedTo("AutomationReason", Object.keys(AUTOMATION_REASON_LABELS));
  });

  it("covers every automation reason with a sentence", () => {
    pinnedTo("AutomationReason", Object.keys(AUTOMATION_REASON_DETAILS));
  });

  it("covers every notification event with a label", () => {
    // The record is keyed on `string`, so TypeScript cannot catch a missing
    // entry the way it does for the reason and attention maps. This is the only
    // thing that will: an event added to the server and forgotten here renders
    // as "dependency.rebindFailed" in a rule editor.
    pinnedTo("NotificationEvent", Object.keys(NOTIFICATION_EVENT_LABELS));
  });

  it("offers every notifiable event in the rule editor", () => {
    // `test` is raised only by an operator proving a destination works, so it
    // is deliberately not a rule an operator can subscribe to.
    const grouped = NOTIFICATION_EVENT_GROUPS.flatMap((group) => group.events);
    const expected = enumValues("NotificationEvent").filter(
      (value) => value !== "test",
    );
    expect([...grouped].sort()).toEqual([...expected].sort());
  });

  it("covers every attention state with a label and a meaning", () => {
    pinnedTo("AttentionState", Object.keys(ATTENTION_LABELS));
    pinnedTo("AttentionState", Object.keys(ATTENTION_MEANINGS));
  });

  // The four below are rendered through functions rather than records, so the
  // test drives the function with every published value and asserts it produced
  // words rather than the value it was given.
  it("renders every dependency state as words", () => {
    for (const value of enumValues("DependencyState")) {
      const label = dependencyStateLabel(value as DependencyState);
      expect(label, `dependencyStateLabel(${value})`).not.toBe(value);
      expect(label).not.toMatch(/unknown|unrecognised/i);
    }
  });

  it("renders every dependency source as a kind and an origin", () => {
    for (const value of enumValues("DependencySource")) {
      const kind = dependencyKindLabel(value as DependencySource);
      const origin = dependencyOriginLabel(value as DependencySource);
      expect(kind, `dependencyKindLabel(${value})`).not.toBe(value);
      expect(kind).not.toMatch(/unrecognised/i);
      expect(origin, `dependencyOriginLabel(${value})`).not.toBe(value);
      expect(origin).not.toMatch(/unrecognised/i);
    }
  });

  it("renders every reattachment state as words", () => {
    for (const value of enumValues("DependencyMemberState")) {
      const label = memberStateLabel(value as DependencyMemberState);
      expect(label, `memberStateLabel(${value})`).not.toBe(value);
      expect(label).not.toBe("Unknown");
    }
  });

  it("renders every discovery refusal as a sentence", () => {
    for (const value of enumValues("DiscoveryRefusal")) {
      const label = discoveryRefusalLabel(value as DiscoveryRefusal);
      expect(label, `discoveryRefusalLabel(${value})`).not.toBe(value);
      // A refusal must never read as an absence of dependencies.
      expect(label).not.toMatch(/no dependencies/i);
    }
  });
});

describe("what the vocabulary says about itself", () => {
  it("never renders a raw enum for any published value", () => {
    // The single assertion the brief asks for: walk everything that can reach
    // the UI and fail if any of it would render as an identifier.
    const rendered: string[] = [
      ...enumValues("AutomationReason").map(
        (value) => AUTOMATION_REASON_LABELS[value as AutomationReason],
      ),
      ...enumValues("AutomationReason").map(
        (value) => AUTOMATION_REASON_DETAILS[value as AutomationReason],
      ),
      ...enumValues("AttentionState").map(
        (value) => ATTENTION_LABELS[value as AttentionState],
      ),
      ...enumValues("DependencyState").map((value) =>
        dependencyStateLabel(value as DependencyState),
      ),
      ...enumValues("DependencyMemberState").map((value) =>
        memberStateLabel(value as DependencyMemberState),
      ),
    ];

    expect(rendered.length).toBeGreaterThan(40);
    for (const text of rendered) {
      expect(text).toBeTruthy();
      // A camelCase identifier that reached a label is the defect.
      expect(text, `"${text}" looks like a raw enum value`).not.toMatch(
        /^[a-z]+[A-Z][a-zA-Z]*$/,
      );
    }
  });

  it("treats waiting as a normal condition rather than a failure", () => {
    // THE distinction this stage turns on. Waiting is the system working.
    expect(automationReasonIsFailure("dependencyWaiting")).toBe(false);
    expect(automationReasonIsFailure("runLimit")).toBe(false);
    expect(automationReasonIsFailure("approvalRequired")).toBe(false);

    expect(automationReasonIsFailure("dependencyBlocked")).toBe(true);
    expect(automationReasonIsFailure("dependencyMissing")).toBe(true);
    expect(automationReasonIsFailure("dependentsNotRebindable")).toBe(true);
  });

  it("recognises every dependency reason as a dependency reason", () => {
    for (const value of enumValues("AutomationReason")) {
      const isDependency = automationReasonIsDependency(value as AutomationReason);
      const looksLikeOne =
        value.startsWith("dependency") || value === "dependentsNotRebindable";
      expect(isDependency, `automationReasonIsDependency(${value})`).toBe(
        looksLikeOne,
      );
    }
  });

  it("says the run limit is not a dependency block", () => {
    // §2: a container deferred by the per-pass budget is not blocked by
    // anything, and conflating the two would send an operator looking for a
    // dependency problem that does not exist.
    expect(AUTOMATION_REASON_DETAILS.runLimit).toMatch(/next in line/i);
    expect(AUTOMATION_REASON_DETAILS.runLimit).not.toMatch(/dependenc/i);
    expect(automationReasonIsDependency("runLimit")).toBe(false);
  });
});
