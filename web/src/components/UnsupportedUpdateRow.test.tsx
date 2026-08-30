import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { expect, it } from "vitest";

import { UpdateAction } from "./UpdateAction";
import {
  buildUpdateRows,
  summarise,
  availableTabCount,
  type UpdateRowModel,
} from "../api/updateWorkspace";
import { TestSessionProvider, testSession, testUser } from "../test/session";
import type { ChangePlan } from "../api/planTypes";

/**
 * The "Not tracked" row, as the SERVER now actually sends it.
 *
 * # Why this exists alongside updateAssessment.test.ts
 *
 * That file proves the mapping is right for a plan shaped by hand. This one
 * proves the mapping is right for the plan the backend genuinely produces, and
 * that the state is REACHABLE at all.
 *
 * It was not. `internal/service/planner.go` looked the image intelligence
 * record up under the reference's CANONICAL form, which for a reference the
 * domain refuses is the empty string. The lookup always missed, so a container
 * running an unlookupable image received no plan and never appeared on this
 * page -- the presentation existed and nothing could ever reach it.
 *
 * The fixture below is therefore not invented. Every field is what the planner
 * writes for a container whose reference cannot be normalised:
 *
 *   - `registryStatus: "unsupported"` -- the record's own status;
 *   - `proposedImage` / `proposedDigest` EMPTY -- nothing may be derived from a
 *     string the domain refused to parse;
 *   - `updateType: "unknown"` -- NOT "none". Nothing was ever compared, and
 *     "none" is what the container list renders as "Up to date";
 *   - `recommendation: "unknown"` -- the unknown-severity registry factor
 *     forces it.
 */
function unsupportedPlan(overrides: Partial<ChangePlan> = {}): ChangePlan {
  return {
    planId: "plan-u",
    containerId: "c-u",
    containerName: "internal-app",
    currentImage: "registry.internal:5000/app:1.2.3",
    proposedImage: "",
    currentDigest: "",
    proposedDigest: "",
    updateType: "unknown",
    snapshotAvailable: false,
    restoreReadiness: "unknown",
    driftOpen: 0,
    policyOpen: 0,
    registryStatus: "unsupported",
    risk: {
      riskScore: 15,
      riskBand: "medium",
      recommendation: "unknown",
      summary:
        "this image reference cannot be looked up, so no registry evidence is available",
      factors: [],
    },
    planVersion: 1,
    plannerVersion: "1",
    inputDigest: "du",
    generatedAt: "2026-08-30T00:00:00Z",
    superseded: false,
    ...overrides,
  } as ChangePlan;
}

/** A perfectly ordinary update, for contrast in the counts. */
function readyPlan(): ChangePlan {
  return {
    ...unsupportedPlan(),
    planId: "plan-r",
    containerId: "c-r",
    containerName: "web",
    currentImage: "nginx:1.27.0",
    proposedImage: "nginx:1.27.1",
    proposedDigest: "sha256:bbb",
    updateType: "patch",
    registryStatus: "ok",
    risk: {
      riskScore: 10,
      riskBand: "low",
      recommendation: "proceed",
      summary: "Looks routine.",
      factors: [],
    },
  } as ChangePlan;
}

/** The single row a one-plan list must produce, or a failure saying so. */
function onlyRow(plan: ChangePlan): UpdateRowModel {
  const rows = buildUpdateRows([plan], [], []);
  if (rows.length !== 1 || !rows[0]) {
    throw new Error(`built ${rows.length} rows, want exactly 1`);
  }
  return rows[0];
}

it("shows the container rather than omitting it", () => {
  const row = onlyRow(unsupportedPlan());

  expect(row.plan.containerName).toBe("internal-app");
  expect(row.assessment.label).toBe("Not tracked");
  expect(row.assessment.kind).toBe("untracked");
});

it("counts it as untracked and not as anything actionable", () => {
  const rows = buildUpdateRows([unsupportedPlan(), readyPlan()], [], []);
  const summary = summarise(rows);

  expect(summary.untracked).toBe(1);
  expect(summary.ready).toBe(1);

  // It contributes to NOTHING an operator would act on.
  expect(summary.available).toBe(1);
  expect(availableTabCount(summary)).toBe(1);
  expect(summary.needsReview).toBe(0);
  expect(summary.notRecommended).toBe(0);

  // And it is not quietly swept into either of the other two no-verdict
  // buckets, which have different remedies.
  expect(summary.unchecked).toBe(0);
  expect(summary.undetermined).toBe(0);
});

it("offers no way to approve, download or apply it", () => {
  render(
    <TestSessionProvider session={testSession({ user: testUser("administrator") })}>
      <MemoryRouter>
        <UpdateAction row={onlyRow(unsupportedPlan())} onChanged={() => {}} />
      </MemoryRouter>
    </TestSessionProvider>,
  );

  // An administrator -- the account that CAN do all three -- is still offered
  // none of them, because there is nothing to move onto.
  expect(screen.getByText(/nothing to apply until this can be assessed/i))
    .toBeInTheDocument();
  for (const label of [/approve/i, /download/i, /apply update/i, /recreate/i]) {
    expect(screen.queryByRole("button", { name: label })).not.toBeInTheDocument();
  }
});

it("keeps Cannot determine for the outcomes that are not this one", () => {
  // The distinction the backend fix had to preserve: a lookup that HAPPENED and
  // settled nothing is a different state with a different remedy, and must not
  // have been collapsed into unsupported to make this row appear.
  const failed = onlyRow(
    unsupportedPlan({ planId: "plan-f", containerId: "c-f", registryStatus: "failed" }),
  );
  expect(failed.assessment.label).toBe("Cannot determine");

  const pending = onlyRow(
    unsupportedPlan({ planId: "plan-p", containerId: "c-p", registryStatus: "pending" }),
  );
  expect(pending.assessment.label).toBe("Not checked yet");
});
