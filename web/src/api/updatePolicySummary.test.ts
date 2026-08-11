import { describe, expect, it } from "vitest";

import type {
  AutomationMode,
  UpdatePolicyRequest,
  UpdateScope,
  UpdateStrategy,
} from "./automationTypes";
import {
  AUTOMATION_MODE_ORDER,
  UPDATE_STRATEGY_ORDER,
} from "./automationTypes";
import {
  describeCeiling,
  describeSchedule,
  describeScope,
  summariseUpdatePolicy,
} from "./updatePolicySummary";

/**
 * The policy summary.
 *
 * # What these tests are actually protecting
 *
 * The summary is the sentence an operator reads before authorising unattended
 * changes to their host. Its failure mode is not "looks wrong" — it is
 * "describes a safer policy than the one being saved". So the matrix below
 * covers every combination the form can produce, and the assertions are about
 * MEANING: does an automatic policy admit it may update, does an observe policy
 * promise nothing will change, does a broad policy say it is broad.
 */

/** The request a form with no input produces, as a starting point. */
function request(overrides: Partial<UpdatePolicyRequest> = {}): UpdatePolicyRequest {
  return {
    name: "test",
    scope: "selector",
    selector: { include: ["web"] },
    strategy: "patch",
    mode: "observe",
    window: { alwaysOpen: true },
    failure: { autoRollback: true, pauseAfterFailures: 2 },
    ...overrides,
  };
}

// ------------------------------------------------------------- the matrix --

const WINDOWS = [
  { name: "anytime", window: { alwaysOpen: true } },
  {
    name: "a maintenance window",
    window: {
      alwaysOpen: false,
      timezone: "America/Chicago",
      start: "02:00",
      end: "04:00",
    },
  },
] as const;

const FAILURES = [
  { name: "rollback and pause", failure: { autoRollback: true, pauseAfterFailures: 2 } },
  { name: "manual and pause", failure: { autoRollback: false, pauseAfterFailures: 2 } },
  { name: "rollback and never pause", failure: { autoRollback: true, pauseAfterFailures: 0 } },
  { name: "manual and never pause", failure: { autoRollback: false, pauseAfterFailures: 0 } },
] as const;

const SCOPES: { name: string; scope: UpdateScope; selector: UpdatePolicyRequest["selector"] }[] = [
  { name: "named containers", scope: "selector", selector: { include: ["web", "api"] } },
  { name: "image patterns", scope: "selector", selector: { images: ["ghcr.io/acme/*"] } },
  { name: "labels", scope: "selector", selector: { labels: { tier: "front" } } },
  { name: "all eligible", scope: "allEligible", selector: {} },
  {
    name: "all eligible with exclusions",
    scope: "allEligible",
    selector: { exclude: ["database"] },
  },
];

describe("summariseUpdatePolicy over every combination", () => {
  for (const scope of SCOPES) {
    for (const mode of AUTOMATION_MODE_ORDER) {
      for (const strategy of UPDATE_STRATEGY_ORDER) {
        for (const window of WINDOWS) {
          for (const failure of FAILURES) {
            it(`${scope.name} / ${mode} / ${strategy} / ${window.name} / ${failure.name}`, () => {
              const summary = summariseUpdatePolicy(
                request({
                  scope: scope.scope,
                  selector: scope.selector,
                  mode,
                  strategy,
                  window: { ...window.window },
                  failure: { ...failure.failure },
                }),
              );

              // It is always a sentence about HarborMaster, and never empty.
              expect(summary.startsWith("HarborMaster ")).toBe(true);
              expect(summary.length).toBeGreaterThan(40);

              // No unrendered value ever reaches an operator.
              expect(summary).not.toContain("undefined");
              expect(summary).not.toContain("[object");
              expect(summary).not.toContain("NaN");

              // The two claims that must never be made by the wrong mode.
              if (mode === "automatic") {
                expect(summary).toContain("may automatically update");
                expect(summary).not.toContain("No container will be changed");
              } else {
                expect(summary).not.toContain("may automatically update");
              }
              if (mode === "observe" || mode === "dryRun") {
                expect(summary).toContain("No container will be changed");
              }

              // Breadth is always stated when it is broad.
              if (scope.scope === "allEligible") {
                expect(summary).toContain("all eligible containers");
              } else {
                expect(summary).not.toContain("all eligible containers");
              }

              // An exclusion is never silently dropped.
              for (const excluded of scope.selector?.exclude ?? []) {
                expect(summary).toContain(excluded);
              }
            });
          }
        }
      }
    }
  }
});

// ------------------------------------------------------- the worked examples --

describe("the sentences an operator actually reads", () => {
  it("describes the recommended first policy", () => {
    const summary = summariseUpdatePolicy(
      request({
        scope: "allEligible",
        selector: {},
        mode: "observe",
        strategy: "minor",
        window: { alwaysOpen: true },
      }),
    );

    expect(summary).toBe(
      "HarborMaster will observe all eligible containers at any time. " +
        "Updates through minor versions would be considered. " +
        "No container will be changed while this policy remains in Observe mode.",
    );
  });

  it("describes an unattended windowed policy", () => {
    const summary = summariseUpdatePolicy(
      request({
        scope: "selector",
        selector: { include: ["web", "api"] },
        mode: "automatic",
        strategy: "minor",
        window: {
          alwaysOpen: false,
          timezone: "America/Chicago",
          start: "02:00",
          end: "04:00",
        },
        failure: { autoRollback: true, pauseAfterFailures: 2 },
      }),
    );

    expect(summary).toBe(
      "HarborMaster may automatically update web and api through minor versions, " +
        "between 02:00 and 04:00 America/Chicago. " +
        "Failed verification rolls the container back and pauses it for review, " +
        "and automation pauses after 2 failures.",
    );
  });

  it("says a policy governs nothing when the selector is empty", () => {
    const summary = summariseUpdatePolicy(request({ selector: {} }));
    expect(summary).toContain("no containers");
  });

  it("names the exclusions on a broad policy", () => {
    const summary = summariseUpdatePolicy(
      request({
        scope: "allEligible",
        selector: { exclude: ["database", "redis"] },
        mode: "automatic",
        strategy: "minor",
      }),
    );
    // The clause is closed with its own comma. Without it, "except database and
    // redis through minor versions" reads as excluding the VERSIONS.
    expect(summary).toContain(
      "all eligible containers, except database and redis, through minor versions",
    );
  });

  it("does not leave a dangling comma in a non-acting mode", () => {
    const summary = summariseUpdatePolicy(
      request({ scope: "allEligible", selector: { exclude: ["database"] }, mode: "observe" }),
    );
    expect(summary).toContain("all eligible containers, except database at any time");
    expect(summary).not.toContain(",,");
    expect(summary).not.toContain(", .");
  });

  it("does not promise a rollback the policy did not ask for", () => {
    const summary = summariseUpdatePolicy(
      request({ mode: "automatic", failure: { autoRollback: false, pauseAfterFailures: 2 } }),
    );
    expect(summary).toContain("left in place for a person to resolve");
    expect(summary).not.toContain("rolls the container back");
  });

  it("warns when repeated failures never pause", () => {
    const summary = summariseUpdatePolicy(
      request({ mode: "automatic", failure: { autoRollback: true, pauseAfterFailures: 0 } }),
    );
    expect(summary).toContain("retried every pass");
  });

  it("says a switched-off policy is not evaluated", () => {
    const summary = summariseUpdatePolicy(
      request({ mode: "automatic", enabled: false }),
    );
    expect(summary).toContain("switched off");
  });

  it("says a withdrawn policy is not evaluated", () => {
    const summary = summariseUpdatePolicy(
      request({ mode: "automatic", archived: true } as UpdatePolicyRequest & {
        archived: boolean;
      }),
    );
    expect(summary).toContain("withdrawn");
  });
});

// ------------------------------------------------------------- the pieces --

describe("describeScope", () => {
  it("names the broad scope without inventing a selector", () => {
    expect(describeScope({ scope: "allEligible", selector: {} })).toBe(
      "all eligible containers",
    );
  });

  it("reads an empty selector as nothing, not as everything", () => {
    expect(describeScope({ scope: "selector", selector: {} })).toBe("no containers");
  });

  it("joins several clauses", () => {
    expect(
      describeScope({
        scope: "selector",
        selector: { include: ["web"], images: ["nginx:*"] },
      }),
    ).toBe("web, and containers running nginx:*");
  });

  it("renders a bare label key as a presence test", () => {
    expect(
      describeScope({ scope: "selector", selector: { labels: { managed: "" } } }),
    ).toBe("containers labelled managed");
  });
});

describe("describeCeiling", () => {
  const expected: Record<UpdateStrategy, string> = {
    digestOnly: "only when the same tag is republished",
    patch: "through patch versions",
    minor: "through minor versions",
    major: "up to and including major versions",
  };

  for (const strategy of UPDATE_STRATEGY_ORDER) {
    it(`phrases ${strategy} as a ceiling`, () => {
      expect(describeCeiling(strategy)).toBe(expected[strategy]);
    });
  }
});

describe("describeSchedule", () => {
  it("reads a missing window as anytime", () => {
    expect(describeSchedule(undefined)).toBe("at any time");
  });

  it("reads an incomplete window as anytime rather than inventing hours", () => {
    expect(describeSchedule({ alwaysOpen: false, timezone: "UTC" })).toBe(
      "at any time",
    );
  });

  it("names the weekdays when a window has them", () => {
    expect(
      describeSchedule({
        alwaysOpen: false,
        timezone: "UTC",
        start: "02:00",
        end: "04:00",
        weekdays: [0, 6],
      }),
    ).toBe("between 02:00 and 04:00 UTC on Sunday and Saturday");
  });

  it("omits the weekday clause when every day is listed", () => {
    expect(
      describeSchedule({
        alwaysOpen: false,
        start: "02:00",
        end: "04:00",
        weekdays: [0, 1, 2, 3, 4, 5, 6],
      }),
    ).toBe("between 02:00 and 04:00");
  });

  it("drops a weekday value it cannot name", () => {
    expect(
      describeSchedule({
        alwaysOpen: false,
        start: "02:00",
        end: "04:00",
        weekdays: [9],
      }),
    ).toBe("between 02:00 and 04:00");
  });
});

// The mode is the field that decides whether the host changes, so a mode this
// build does not know must never render as one that acts.
describe("an unknown mode", () => {
  it("reads as the safe verb", () => {
    const summary = summariseUpdatePolicy(
      request({ mode: "somethingNew" as AutomationMode }),
    );
    expect(summary).toContain("will observe");
    expect(summary).not.toContain("may automatically update");
  });
});
