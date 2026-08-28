import { describe, expect, it } from "vitest";

import {
  assessmentOf,
  availableTabCount,
  buildUpdateRows,
  filterRows,
  isUndetermined,
  summarise,
  type UpdateRowModel,
} from "./updateWorkspace";
import type { ChangePlan } from "./planTypes";
import type { CheckStatus } from "./imageTypes";
import type { Recommendation } from "./planTypes";

/**
 * Three kinds of "no verdict", separated because the server already separates
 * them.
 *
 * # The defect this exists to prevent coming back
 *
 * Every row HarborMaster could not judge was labelled "Cannot advise" and
 * counted in one number. On a real homelab that number is dominated by images
 * that can never be looked up at all -- something built locally, something from
 * a registry with no public endpoint -- and by images nothing has got round to
 * checking yet. Neither needs anybody. Mixing them with the rows that DID get
 * looked at and DID come back inconclusive made the one number that deserved
 * attention indistinguishable from the two that did not, so operators learned
 * to ignore all three.
 *
 * # Where the distinction comes from
 *
 * `plan.registryStatus`, a required field with a closed server-side vocabulary
 * (`internal/domain/imageintel.go`), set from the outcome of the most recent
 * registry lookup. Not from the image string, not from the tag, not from a
 * heuristic. The two values used here are the two the server documents as
 * definite:
 *
 *   - `unsupported` -- "the reference names no public registry, so it can never
 *     be looked up";
 *   - `pending` -- "this reference has not been looked up yet".
 *
 * Everything else -- ok, failed, rateLimited, unauthorized, notFound -- means a
 * lookup happened and settled nothing, which is one answer with one remedy.
 *
 * # Why reading it cannot override a real verdict
 *
 * `assessRegistryQuality` in `internal/domain/plan_risk.go` contributes an
 * unknown-SEVERITY factor for both values, and `recommend` turns any
 * unknown-severity factor into `RecommendUnknown`. A plan whose registry status
 * is `unsupported` or `pending` therefore cannot carry `proceed`,
 * `manualReview` or `notRecommended` in the first place -- and the mapping here
 * is applied only on the paths that already have no verdict, so even if that
 * ever changed, a real recommendation would still win.
 */

function plan(overrides: Partial<ChangePlan> = {}): ChangePlan {
  return {
    planId: "p1",
    containerId: "c1",
    containerName: "web",
    currentImage: "nginx:1.27",
    proposedImage: "nginx:1.27.1",
    currentDigest: "sha256:aaa",
    proposedDigest: "sha256:bbb",
    updateType: "patch",
    snapshotAvailable: true,
    restoreReadiness: "ready",
    driftOpen: 0,
    policyOpen: 0,
    registryStatus: "ok",
    risk: {
      riskScore: 10,
      riskBand: "low",
      recommendation: "proceed",
      summary: "Looks routine.",
      factors: [],
    },
    planVersion: 1,
    plannerVersion: "1",
    inputDigest: "d",
    generatedAt: "2026-08-27T00:00:00Z",
    superseded: false,
    ...overrides,
  } as ChangePlan;
}

/** A plan the planner declined to judge, for the given lookup outcome. */
function undecided(registryStatus: CheckStatus): ChangePlan {
  return plan({
    registryStatus,
    risk: {
      riskScore: 40,
      riskBand: "medium",
      recommendation: "unknown" as Recommendation,
      summary: "",
      factors: [],
    },
  });
}

// ----------------------------------------------------- the three verdicts --

it("calls an unlookupable reference not tracked, not undetermined", () => {
  const assessment = assessmentOf(undecided("unsupported"));

  expect(assessment.kind).toBe("untracked");
  expect(assessment.label).toBe("Not tracked");
  // The reason is stated, because "nothing will ever be found" is the useful
  // half and a bare grey badge does not carry it.
  expect(assessment.summary).toMatch(/no update will ever be found/i);
});

it("calls an unchecked reference not checked yet", () => {
  const assessment = assessmentOf(undecided("pending"));

  expect(assessment.kind).toBe("unchecked");
  expect(assessment.label).toBe("Not checked yet");
  expect(assessment.summary).toMatch(/has not been looked up yet/i);
});

describe("a lookup that settled nothing", () => {
  // Each of these means HarborMaster went and asked. The remedy is the same for
  // all of them -- look, or wait and look again -- so they are one category.
  it.each<CheckStatus>(["ok", "failed", "rateLimited", "unauthorized", "notFound"])(
    "%s is cannot-determine",
    (status) => {
      const assessment = assessmentOf(undecided(status));
      expect(assessment.kind).toBe("unknown");
      expect(assessment.label).toBe("Cannot determine");
    },
  );
});

it("does not use cannot-determine for a state the model knows is untracked", () => {
  // The specific confusion this batch was asked to remove.
  const assessment = assessmentOf(undecided("unsupported"));
  expect(assessment.label).not.toMatch(/cannot determine/i);
  expect(assessment.kind).not.toBe("unknown");
});

it("never lets the registry status override a verdict the planner reached", () => {
  // A plan that proposes a target AND carries a recommendation is judged on the
  // recommendation, whatever the lookup outcome was.
  for (const status of ["unsupported", "pending"] as CheckStatus[]) {
    expect(assessmentOf(plan({ registryStatus: status })).kind).toBe("ready");
    expect(
      assessmentOf(
        plan({
          registryStatus: status,
          risk: { ...plan().risk, recommendation: "manualReview" },
        }),
      ).kind,
    ).toBe("review");
  }
});

it("reads the reason even when the plan proposed no target at all", () => {
  const noTarget = plan({
    proposedImage: "",
    proposedDigest: undefined,
    registryStatus: "unsupported",
    risk: { ...plan().risk, recommendation: "unknown", summary: "" },
  });
  expect(assessmentOf(noTarget).kind).toBe("untracked");
});

it("treats all three as offering nothing to apply", () => {
  expect(isUndetermined("untracked")).toBe(true);
  expect(isUndetermined("unchecked")).toBe(true);
  expect(isUndetermined("unknown")).toBe(true);
  expect(isUndetermined("ready")).toBe(false);
  expect(isUndetermined("review")).toBe(false);
  expect(isUndetermined("against")).toBe(false);
});

// -------------------------------------------------------- counts and tabs --

function rowsFor(plans: ChangePlan[]): UpdateRowModel[] {
  return buildUpdateRows(plans, [], []);
}

const oneOfEach = () => [
  plan({ planId: "a", containerId: "a", containerName: "a" }),
  plan({
    planId: "b",
    containerId: "b",
    containerName: "b",
    risk: { ...plan().risk, recommendation: "manualReview" },
  }),
  plan({
    planId: "c",
    containerId: "c",
    containerName: "c",
    risk: { ...plan().risk, recommendation: "notRecommended" },
  }),
  { ...undecided("unsupported"), planId: "d", containerId: "d", containerName: "d" },
  { ...undecided("pending"), planId: "e", containerId: "e", containerName: "e" },
  { ...undecided("failed"), planId: "f", containerId: "f", containerName: "f" },
];

it("partitions every row into exactly one bucket", () => {
  const summary = summarise(rowsFor(oneOfEach()));

  expect(summary).toMatchObject({
    ready: 1,
    needsReview: 1,
    notRecommended: 1,
    untracked: 1,
    unchecked: 1,
    undetermined: 1,
  });

  // The invariant: the six states sum to the row count, so the cards read as a
  // partition and no container is counted twice.
  const partition =
    summary.ready +
    summary.needsReview +
    summary.notRecommended +
    summary.untracked +
    summary.unchecked +
    summary.undetermined;
  expect(partition).toBe(6);
});

it("keeps every no-verdict row out of 'available'", () => {
  const summary = summarise(rowsFor(oneOfEach()));
  // Available is the three that propose a move. A gap in evidence is not one.
  expect(summary.available).toBe(3);
});

it("makes the Available tab's badge match the rows that tab shows", () => {
  // The defect: the badge counted `ready` while the tab also listed
  // `notRecommended`, so a tab labelled 1 opened onto 2 rows.
  const rows = rowsFor(oneOfEach());
  const summary = summarise(rows);
  expect(availableTabCount(summary)).toBe(filterRows(rows, "available").length);
});

it("makes the review tab's badge match its rows", () => {
  const rows = rowsFor(oneOfEach());
  expect(summarise(rows).needsReview).toBe(filterRows(rows, "review").length);
});

it("shows every row on the All tab, including the ones with no verdict", () => {
  const rows = rowsFor(oneOfEach());
  expect(filterRows(rows, "all")).toHaveLength(6);
});

it("counts nothing on an empty estate", () => {
  const summary = summarise([]);
  expect(summary).toMatchObject({
    available: 0,
    ready: 0,
    needsReview: 0,
    notRecommended: 0,
    untracked: 0,
    unchecked: 0,
    undetermined: 0,
  });
  expect(filterRows([], "all")).toHaveLength(0);
  expect(availableTabCount(summary)).toBe(0);
});
