import { describe, expect, it } from "vitest";

import type { DependencySummary } from "../api/dependencyTypes";
import { buildAttention, type AttentionInputs } from "./attentionModel";

/**
 * The dependency items on the dashboard.
 *
 * # What these defend
 *
 *  1. **Nothing is asserted from an absent read.** A dependency graph that
 *     could not be built is HarborMaster refusing to order the estate.
 *     Inferring "no loops" from a read that never arrived would invent exactly
 *     the reassurance the subsystem exists to withhold.
 *  2. **Transient waiting never appears.** It clears without anybody, and there
 *     is no clock in this function to distinguish "waiting" from "waiting too
 *     long" honestly. Reporting it would teach an operator that this list
 *     contains things which do not need them.
 *  3. **One incident is one item.** A coordinated update that ended
 *     part-finished is already reported by its failed reattachment.
 */

function summary(overrides: Partial<DependencySummary> = {}): DependencySummary {
  return {
    cycles: 0,
    unresolved: 0,
    rebindsFailed: 0,
    rebindsPending: 0,
    ...overrides,
  };
}

function build(dependencies: AttentionInputs["dependencies"]) {
  return buildAttention({ dependencies });
}

const dependencyIds = (items: { id: string }[]) =>
  items.map((item) => item.id).filter((id) => id.startsWith("dependency-"));

describe("dependency attention", () => {
  it("says nothing when the dependency read never arrived", () => {
    expect(dependencyIds(build(undefined))).toEqual([]);
    expect(dependencyIds(build(null))).toEqual([]);
  });

  it("says nothing about a healthy estate", () => {
    expect(dependencyIds(build(summary()))).toEqual([]);
  });

  it("never reports work in flight", () => {
    // A reattachment in progress is the system working. Only the SETTLED
    // failure is somebody's problem.
    expect(dependencyIds(build(summary({ rebindsPending: 4 })))).toEqual([]);
  });

  it("reports a failed reattachment as an action, linking to the page that explains it", () => {
    const items = build(summary({ rebindsFailed: 2 }));
    const item = items.find((one) => one.id === "dependency-rebind-failed");

    expect(item).toBeDefined();
    expect(item?.level).toBe("action");
    expect(item?.title).toMatch(/2 containers could not be reattached/);
    expect(item?.detail).toMatch(/does not retry a reattachment by itself/i);
    expect(item?.detail).toMatch(/No image version was changed/i);
    expect(item?.to).toBe("/dependencies");
    expect(item?.count).toBe(2);
  });

  it("reports a loop as an action that nothing clears on its own", () => {
    const item = build(summary({ cycles: 1 })).find(
      (one) => one.id === "dependency-cycle",
    );

    expect(item?.level).toBe("action");
    expect(item?.title).toMatch(/1 dependency loop needs attention/);
    expect(item?.detail).toMatch(/Nothing will break the loop on its own/i);
    expect(item?.to).toBe("/dependencies");
  });

  it("reports an unresolvable namespace as a refusal, not an absence", () => {
    const item = build(summary({ unresolved: 3 })).find(
      (one) => one.id === "dependency-unresolved",
    );

    expect(item?.level).toBe("action");
    expect(item?.detail).toMatch(
      /a refusal, not a finding that they have no dependencies/i,
    );
    expect(item?.to).toBe("/dependencies");
  });

  it("gives every dependency item a destination", () => {
    // §17: no dead-end attention cards.
    const items = build(
      summary({ cycles: 1, unresolved: 1, rebindsFailed: 1 }),
    ).filter((item) => item.id.startsWith("dependency-"));

    expect(items).toHaveLength(3);
    for (const item of items) {
      expect(item.to, item.id).toBeTruthy();
      expect(item.to).toMatch(/^\//);
    }
  });

  it("reports each condition once", () => {
    const items = build(summary({ cycles: 2, unresolved: 2, rebindsFailed: 2 }));
    expect(dependencyIds(items).sort()).toEqual([
      "dependency-cycle",
      "dependency-rebind-failed",
      "dependency-unresolved",
    ]);
  });

  it("says nothing about waiting, in any wording", () => {
    const items = build(
      summary({ cycles: 1, unresolved: 1, rebindsFailed: 1, rebindsPending: 5 }),
    );
    for (const item of items) {
      expect(`${item.title} ${item.detail}`).not.toMatch(/waiting/i);
    }
  });
});
