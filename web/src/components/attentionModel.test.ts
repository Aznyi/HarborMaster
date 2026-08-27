import { describe, expect, it } from "vitest";

import {
  atLevel,
  buildAttention,
  evidenceComplete,
  type AttentionInputs,
} from "./attentionModel";

/**
 * The dashboard's operator-attention model.
 *
 * The rules worth defending are all about EVIDENCE, because the failure mode
 * of a dashboard is not being wrong -- it is being reassuring:
 *
 *   - a summary that did not load produces no item, in either direction
 *   - an unexamined estate is reported as unexamined, never as healthy
 *   - a degraded dependency is always an item, whatever else is true
 *   - an update HarborMaster cannot judge is never counted as available
 *
 * Every input below is the shape an existing endpoint already returns.
 */

/** A fully loaded, entirely uneventful estate. */
function settled(): AttentionInputs {
  return {
    health: {
      status: "healthy",
      docker: { status: "up" },
      database: { status: "up" },
    } as AttentionInputs["health"],
    inventory: {
      state: "succeeded",
      counts: { unhealthy: 0, paused: 0 },
    } as unknown as AttentionInputs["inventory"],
    events: { enabled: true, state: "connected" } as AttentionInputs["events"],
    automation: {
      enabled: true,
      awaitingApproval: 0,
      pausedContainers: 0,
      policies: 2,
      enabledPolicies: 2,
    } as AttentionInputs["automation"],
    plans: {
      containers: 12,
      actionable: 0,
      needsReview: 0,
      blocked: 0,
      undetermined: 0,
    } as unknown as AttentionInputs["plans"],
    executions: { needsAttention: 0, failed: 0 } as unknown as AttentionInputs["executions"],
    rollbacks: { needsAttention: 0, failed: 0 } as unknown as AttentionInputs["rollbacks"],
    policy: {
      open: 0,
      policiesTotal: 3,
      bySeverity: {},
      containersEvaluated: 12,
    } as unknown as AttentionInputs["policy"],
    drift: {
      open: 0,
      containersWithDrift: 0,
      containersEvaluated: 12,
    } as unknown as AttentionInputs["drift"],
    canReadAutomation: true,
  };
}

function ids(inputs: AttentionInputs): string[] {
  return buildAttention(inputs).map((item) => item.id);
}

// ---------------------------------------------------------- evidence gaps --

describe("absent evidence", () => {
  it("produces nothing at all when nothing loaded", () => {
    // Not "everything is fine" and not "everything is broken". A model that
    // invented either from an empty object would be inventing certainty.
    expect(buildAttention({})).toEqual([]);
  });

  it("cannot claim an all-clear over summaries it never received", () => {
    expect(evidenceComplete({})).toBe(false);
    expect(evidenceComplete(settled())).toBe(true);

    const partial = settled();
    partial.policy = null;
    expect(evidenceComplete(partial)).toBe(false);
  });

  it("says an unexamined estate is unexamined rather than healthy", () => {
    const fresh = settled();
    fresh.policy = {
      open: 0,
      policiesTotal: 0,
      bySeverity: {},
      containersEvaluated: 0,
    } as unknown as AttentionInputs["policy"];
    fresh.drift = {
      open: 0,
      containersWithDrift: 0,
      containersEvaluated: 0,
    } as unknown as AttentionInputs["drift"];
    fresh.plans = {
      containers: 0,
      actionable: 0,
      needsReview: 0,
      blocked: 0,
      undetermined: 0,
    } as unknown as AttentionInputs["plans"];

    const items = buildAttention(fresh);
    expect(atLevel(items, "action")).toEqual([]);
    expect(ids(fresh)).toEqual(
      expect.arrayContaining(["no-policies", "no-drift-baseline", "no-plans"]),
    );
  });
});

// ------------------------------------------------------------ HarborMaster --

describe("HarborMaster's own dependencies", () => {
  it("raises an unreachable Docker above everything", () => {
    const broken = settled();
    broken.health = {
      status: "unhealthy",
      docker: { status: "down" },
      database: { status: "up" },
    } as AttentionInputs["health"];

    const items = buildAttention(broken);
    expect(items[0]?.id).toBe("docker-down");
    expect(items[0]?.level).toBe("action");
  });

  it("raises an unhealthy database", () => {
    const broken = settled();
    broken.health = {
      status: "degraded",
      docker: { status: "up" },
      database: { status: "down" },
    } as AttentionInputs["health"];

    expect(ids(broken)).toContain("database-down");
  });

  it("says a failed refresh makes everything below it possibly stale", () => {
    const stale = settled();
    stale.inventory = {
      state: "failed",
      counts: { unhealthy: 0, paused: 0 },
    } as unknown as AttentionInputs["inventory"];

    expect(ids(stale)).toContain("refresh-failed");
  });

  it("does not treat a deliberately disabled event engine as a fault", () => {
    const configured = settled();
    configured.events = {
      enabled: false,
      state: "disabled",
    } as AttentionInputs["events"];

    expect(ids(configured)).not.toContain("events-disconnected");
  });
});

// --------------------------------------------------------------- the work --

describe("what needs a person", () => {
  it("reports a failure that left containers on the host", () => {
    const messy = settled();
    messy.executions = { needsAttention: 2 } as unknown as AttentionInputs["executions"];

    const items = buildAttention(messy);
    const item = items.find((entry) => entry.id === "executions-need-attention");
    expect(item?.level).toBe("action");
    expect(item?.count).toBe(2);
    expect(item?.to).toBe("/executions");
  });

  it("reports an incomplete rollback separately from a failed update", () => {
    const messy = settled();
    messy.rollbacks = { needsAttention: 1 } as unknown as AttentionInputs["rollbacks"];

    expect(ids(messy)).toContain("rollbacks-need-attention");
    expect(ids(messy)).not.toContain("executions-need-attention");
  });

  it("points a held approval at the queue it can be released from", () => {
    const waiting = settled();
    waiting.automation = {
      enabled: true,
      awaitingApproval: 3,
      pausedContainers: 0,
    } as AttentionInputs["automation"];

    const item = buildAttention(waiting).find((entry) => entry.id === "awaiting-approval");
    expect(item?.to).toBe("/automation/approvals");
    expect(item?.count).toBe(3);
  });

  it("says nothing about automation to an account that cannot read it", () => {
    const hidden = settled();
    hidden.canReadAutomation = false;
    hidden.automation = {
      enabled: false,
      awaitingApproval: 5,
      pausedContainers: 2,
    } as AttentionInputs["automation"];

    const items = ids(hidden);
    expect(items).not.toContain("awaiting-approval");
    expect(items).not.toContain("automation-paused");
    expect(items).not.toContain("automation-off");
  });

  it("counts unhealthy containers and links to them filtered", () => {
    const sick = settled();
    sick.inventory = {
      state: "succeeded",
      counts: { unhealthy: 4, paused: 0 },
    } as unknown as AttentionInputs["inventory"];

    const item = buildAttention(sick).find((entry) => entry.id === "unhealthy");
    expect(item?.level).toBe("action");
    expect(item?.to).toBe("/containers?health=unhealthy");
  });
});

// ------------------------------------------------------------- judgement --

describe("what HarborMaster will and will not claim", () => {
  it("never counts an unjudgeable update as an available one", () => {
    const murky = settled();
    murky.plans = {
      containers: 5,
      actionable: 0,
      needsReview: 0,
      blocked: 0,
      undetermined: 3,
    } as unknown as AttentionInputs["plans"];

    const items = buildAttention(murky);
    expect(items.find((entry) => entry.id === "plans-actionable")).toBeUndefined();

    const undetermined = items.find((entry) => entry.id === "plans-undetermined");
    expect(undetermined?.count).toBe(3);
    // Reported as information, not as work: nobody can act on a non-answer.
    expect(undetermined?.level).toBe("info");
    expect(undetermined?.title).toMatch(/cannot judge/i);
  });

  it("separates the updates asking for a person from the ones that are not", () => {
    const mixed = settled();
    mixed.plans = {
      containers: 9,
      actionable: 4,
      needsReview: 2,
      blocked: 0,
      undetermined: 0,
    } as unknown as AttentionInputs["plans"];

    const items = buildAttention(mixed);
    expect(items.find((entry) => entry.id === "plans-need-review")?.count).toBe(2);
    expect(items.find((entry) => entry.id === "plans-actionable")?.count).toBe(4);
  });

  it("raises a critical policy finding louder than a routine one", () => {
    const critical = settled();
    critical.policy = {
      open: 5,
      policiesTotal: 2,
      bySeverity: { critical: 1, high: 1, low: 3 },
      containersEvaluated: 10,
    } as unknown as AttentionInputs["policy"];

    const item = buildAttention(critical).find((entry) => entry.id === "policy-critical");
    expect(item?.level).toBe("action");
    expect(item?.count).toBe(2);

    const routine = settled();
    routine.policy = {
      open: 3,
      policiesTotal: 2,
      bySeverity: { low: 3 },
      containersEvaluated: 10,
    } as unknown as AttentionInputs["policy"];

    const lesser = buildAttention(routine).find((entry) => entry.id === "policy-open");
    expect(lesser?.level).toBe("watch");
  });

  it("describes drift as an observation, not as work HarborMaster will undo", () => {
    const drifted = settled();
    drifted.drift = {
      open: 7,
      containersWithDrift: 2,
      containersEvaluated: 10,
    } as unknown as AttentionInputs["drift"];

    const item = buildAttention(drifted).find((entry) => entry.id === "drift-open");
    expect(item?.level).toBe("watch");
    expect(item?.detail).toMatch(/nothing here changes a container back/i);
  });
});

// ------------------------------------------------------------- ordering --

describe("ordering", () => {
  it("puts everything needing a person before everything that does not", () => {
    const busy = settled();
    busy.inventory = {
      state: "succeeded",
      counts: { unhealthy: 1, paused: 0 },
    } as unknown as AttentionInputs["inventory"];
    busy.plans = {
      containers: 4,
      actionable: 2,
      needsReview: 0,
      blocked: 0,
      undetermined: 1,
    } as unknown as AttentionInputs["plans"];

    const levels = buildAttention(busy).map((item) => item.level);
    const firstWatch = levels.indexOf("watch");
    const lastAction = levels.lastIndexOf("action");
    expect(lastAction).toBeLessThan(firstWatch === -1 ? levels.length : firstWatch);
  });

  it("gives every item somewhere to go", () => {
    const busy = settled();
    busy.inventory = {
      state: "failed",
      counts: { unhealthy: 2, paused: 1 },
    } as unknown as AttentionInputs["inventory"];
    busy.executions = { needsAttention: 1 } as unknown as AttentionInputs["executions"];
    busy.automation = {
      enabled: true,
      awaitingApproval: 1,
      pausedContainers: 1,
    } as AttentionInputs["automation"];

    for (const item of buildAttention(busy)) {
      expect(item.to).toMatch(/^\//);
      expect(item.title.length).toBeGreaterThan(0);
      expect(item.detail.length).toBeGreaterThan(0);
    }
  });

  it("gives every item a distinct identity", () => {
    const busy = settled();
    busy.inventory = {
      state: "failed",
      counts: { unhealthy: 2, paused: 1 },
    } as unknown as AttentionInputs["inventory"];

    const seen = ids(busy);
    expect(new Set(seen).size).toBe(seen.length);
  });
});

// ------------------------------------------------------------ onboarding --

/** A settled estate that has been assessed and can read automation. */
function onboarded(): AttentionInputs {
  return {
    ...settled(),
    canReadAutomation: true,
    planner: { lastRunAt: "2026-08-06T02:00:00Z" } as AttentionInputs["planner"],
    health: {
      status: "healthy",
      docker: { status: "up" },
      database: { status: "up" },
      features: { automation: true, planner: true },
    } as unknown as AttentionInputs["health"],
  };
}

it("raises nothing about onboarding on a configured installation", () => {
  const items = buildAttention(onboarded());
  expect(items.map((item) => item.id)).not.toContain("automation-not-configured");
  expect(items.map((item) => item.id)).not.toContain("automation-engine-disabled");
});

it("says when the engine is running and nothing tells it what to do", () => {
  const inputs = onboarded();
  inputs.automation = {
    ...inputs.automation,
    policies: 0,
    enabledPolicies: 0,
    actingPolicies: 0,
  } as AttentionInputs["automation"];

  const item = buildAttention(inputs).find(
    (candidate) => candidate.id === "automation-not-configured",
  );
  expect(item).toBeTruthy();
  // Not an error: an installation with no policy is a normal, safe one.
  expect(item!.level).toBe("info");
  expect(item!.detail).toMatch(/will not change any of them/i);
});

it("says when an automatic policy cannot run because the engine is off", () => {
  const inputs = onboarded();
  inputs.health = {
    ...inputs.health,
    features: { automation: false, planner: true },
  } as unknown as AttentionInputs["health"];
  inputs.automation = {
    ...inputs.automation,
    policies: 1,
    actingPolicies: 1,
  } as AttentionInputs["automation"];

  const item = buildAttention(inputs).find(
    (candidate) => candidate.id === "automation-engine-disabled",
  );
  expect(item).toBeTruthy();
  // The sentence that matters: saving the policy did not enable anything.
  expect(item!.detail).toMatch(/does not enable it/i);
  expect(item!.count).toBe(1);
});

it("stays silent about onboarding until the estate has been assessed", () => {
  const inputs = onboarded();
  inputs.planner = null;
  inputs.automation = {
    ...inputs.automation,
    policies: 0,
    actingPolicies: 0,
  } as AttentionInputs["automation"];

  // An estate nobody has assessed has no opinion about policies yet, and this
  // window is normally seconds long. Raising an item here would ask somebody to
  // fix something that is not broken.
  expect(
    buildAttention(inputs).map((item) => item.id),
  ).not.toContain("automation-not-configured");
});

it("raises no onboarding item for a deliberate read-only installation", () => {
  const inputs = onboarded();
  inputs.health = {
    ...inputs.health,
    features: { automation: false, planner: true },
  } as unknown as AttentionInputs["health"];
  inputs.automation = {
    ...inputs.automation,
    policies: 0,
    actingPolicies: 0,
  } as AttentionInputs["automation"];

  // Engine off AND no policies is somebody running HarborMaster as a reporter
  // on purpose. Nothing is misconfigured, so nothing is raised.
  const ids = buildAttention(inputs).map((item) => item.id);
  expect(ids).not.toContain("automation-not-configured");
  expect(ids).not.toContain("automation-engine-disabled");
});
