import { describe, expect, it } from "vitest";

import {
  AUTOMATION_PRESETS,
  PRESET_DESCRIPTIONS,
  PRESET_LABELS,
  compilePreset,
  detectPreset,
  presetConsequence,
  presetSemantics,
  type AutomationPreset,
} from "./automationPresets";
import type { UpdatePolicyRequest } from "./automationTypes";
import type { UpdateType } from "./imageTypes";

/**
 * Preset compiler and detector.
 *
 * Every assertion here is about FIELD VALUES rather than rendered output. A
 * preset is a mapping onto a policy request, and the only way it can be wrong
 * is by producing the wrong field -- which a snapshot of some markup would
 * happily record as correct.
 */

/** A base request carrying only operator-owned fields. */
function base(overrides: Partial<UpdatePolicyRequest> = {}): UpdatePolicyRequest {
  return {
    name: "keep things current",
    description: "",
    priority: 0,
    scope: "allEligible",
    selector: { exclude: ["database"] },
    window: { alwaysOpen: true },
    failure: { pauseAfterFailures: 2 },
    ...overrides,
  };
}

describe("the preset compiler", () => {
  it("compiles Follow current tag to digest-only automatic", () => {
    const request = compilePreset("followCurrentTag", base());

    expect(request.strategy).toBe("digestOnly");
    expect(request.mode).toBe("automatic");
    expect(request.minimumRecommendation).toBe("proceed");
    expect(request.failure?.autoRollback).toBe(true);
  });

  it("compiles Observe only to a mode that cannot mutate", () => {
    const request = compilePreset("observe", base());

    expect(request.mode).toBe("observe");
    // The ceiling is still meaningful: observe previews what the safe
    // automatic preset would do, rather than previewing nothing.
    expect(request.strategy).toBe("patch");
  });

  it("compiles Keep containers safely updated to a patch ceiling", () => {
    const request = compilePreset("safeAutomatic", base());

    expect(request.strategy).toBe("patch");
    expect(request.mode).toBe("automatic");
    expect(request.minimumRecommendation).toBe("proceed");
    expect(request.failure?.autoRollback).toBe(true);
  });

  it("compiles Allow minor version updates to a minor ceiling", () => {
    const request = compilePreset("automaticMinor", base());

    expect(request.strategy).toBe("minor");
    expect(request.mode).toBe("automatic");
  });

  it("never compiles a major-version ceiling", () => {
    // No preset may make a major version update eligible. An operator who
    // wants that has to say so in Custom, in as many words.
    for (const preset of AUTOMATION_PRESETS) {
      const semantics = presetSemantics(preset);
      if (!semantics) continue;
      expect(semantics.strategy).not.toBe("major");
    }
  });

  it("never compiles a recommendation floor below proceed", () => {
    // `proceed` is the stricter of the two automatable verdicts. A preset
    // choosing the looser one would be widening what automation may act on,
    // which is not a thing a configuration shortcut may do.
    for (const preset of AUTOMATION_PRESETS) {
      const semantics = presetSemantics(preset);
      if (!semantics) continue;
      expect(semantics.minimumRecommendation).toBe("proceed");
    }
  });

  it("leaves Custom entirely alone", () => {
    const original = base({
      strategy: "major",
      mode: "automatic",
      minimumRecommendation: "proceedWithCaution",
    });
    expect(compilePreset("custom", original)).toEqual(original);
    expect(presetSemantics("custom")).toBeNull();
  });
});

describe("operator-owned fields survive compilation", () => {
  const owned: { name: string; value: Partial<UpdatePolicyRequest> }[] = [
    { name: "the policy name", value: { name: "my rule" } },
    { name: "the description", value: { description: "why this exists" } },
    { name: "the scope", value: { scope: "selector" } },
    {
      name: "selected containers",
      value: { selector: { include: ["web", "api"] } },
    },
    {
      name: "image patterns",
      value: { selector: { images: ["ghcr.io/acme/*"] } },
    },
    { name: "exclusions", value: { selector: { exclude: ["database"] } } },
    {
      name: "the maintenance window",
      value: {
        window: {
          alwaysOpen: false,
          timezone: "Europe/London",
          start: "02:00",
          end: "04:00",
        },
      },
    },
    { name: "priority", value: { priority: 40 } },
    {
      name: "the pause-after-failures setting",
      value: { failure: { pauseAfterFailures: 5 } },
    },
  ];

  for (const field of owned) {
    it(`preserves ${field.name} through every preset`, () => {
      for (const preset of AUTOMATION_PRESETS) {
        const start = base(field.value);
        const compiled = compilePreset(preset, start);

        for (const key of Object.keys(field.value) as (keyof UpdatePolicyRequest)[]) {
          if (key === "failure") {
            // The failure block is SHARED: the preset owns autoRollback and
            // the operator owns the rest, so it is merged rather than replaced.
            expect(compiled.failure?.pauseAfterFailures).toBe(
              start.failure?.pauseAfterFailures,
            );
            continue;
          }
          expect(compiled[key]).toEqual(start[key]);
        }
      }
    });
  }

  it("switching between presets changes only the owned fields", () => {
    const configured = base({
      name: "estate",
      priority: 25,
      scope: "selector",
      selector: { include: ["web"], exclude: ["database"] },
      window: { alwaysOpen: false, timezone: "UTC", start: "01:00", end: "03:00" },
      failure: { pauseAfterFailures: 4 },
    });

    const follow = compilePreset("followCurrentTag", configured);
    const minor = compilePreset("automaticMinor", follow);

    // Owned: changed.
    expect(minor.strategy).toBe("minor");
    // Everything else: identical, field by field.
    expect(minor.name).toBe("estate");
    expect(minor.priority).toBe(25);
    expect(minor.scope).toBe("selector");
    expect(minor.selector).toEqual({ include: ["web"], exclude: ["database"] });
    expect(minor.window).toEqual(configured.window);
    expect(minor.failure?.pauseAfterFailures).toBe(4);
  });
});

describe("exact preset detection", () => {
  it("recognises each preset's own output", () => {
    for (const preset of AUTOMATION_PRESETS) {
      if (preset === "custom") continue;
      expect(detectPreset(compilePreset(preset, base()))).toBe(preset);
    }
  });

  it("ignores operator targeting when detecting", () => {
    const wide = compilePreset("followCurrentTag", base({ scope: "allEligible" }));
    const narrow = compilePreset(
      "followCurrentTag",
      base({ scope: "selector", selector: { include: ["web"] } }),
    );
    const windowed = compilePreset(
      "followCurrentTag",
      base({
        window: { alwaysOpen: false, timezone: "UTC", start: "02:00", end: "04:00" },
      }),
    );

    expect(detectPreset(wide)).toBe("followCurrentTag");
    expect(detectPreset(narrow)).toBe("followCurrentTag");
    // §20: a maintenance window is not a preset-owned semantic field.
    expect(detectPreset(windowed)).toBe("followCurrentTag");
  });

  it("treats an omitted recommendation as proceed, matching the server", () => {
    // Every policy written by the pre-preset editor omitted this field, and
    // the create handler stored `proceed`. Reading those back as Custom would
    // mislabel every existing policy on the estate.
    const legacy: UpdatePolicyRequest = {
      strategy: "digestOnly",
      mode: "automatic",
      failure: { autoRollback: true },
    };
    expect(detectPreset(legacy)).toBe("followCurrentTag");
  });

  describe("near misses, one field at a time", () => {
    const cases: { name: string; change: Partial<UpdatePolicyRequest> }[] = [
      {
        name: "a looser recommendation floor",
        change: { minimumRecommendation: "proceedWithCaution" },
      },
      { name: "a wider strategy", change: { strategy: "minor" } },
      { name: "a different mode", change: { mode: "approvalRequired" } },
      {
        name: "automatic rollback switched off",
        change: { failure: { autoRollback: false, pauseAfterFailures: 2 } },
      },
    ];

    for (const testCase of cases) {
      it(`reports Custom for ${testCase.name}`, () => {
        const compiled = compilePreset("followCurrentTag", base());
        const altered = { ...compiled, ...testCase.change };

        expect(detectPreset(altered)).not.toBe("followCurrentTag");
      });
    }

    it("a digest-only automatic policy with a caution floor is Custom", () => {
      // The brief's worked example. It permits strictly more than Follow
      // current tag does, so labelling it Follow current tag would tell the
      // operator something untrue.
      expect(
        detectPreset({
          strategy: "digestOnly",
          mode: "automatic",
          minimumRecommendation: "proceedWithCaution",
          failure: { autoRollback: true },
        }),
      ).toBe("custom");
    });

    it("a major-version policy is always Custom", () => {
      expect(
        detectPreset({
          strategy: "major",
          mode: "automatic",
          minimumRecommendation: "proceed",
          failure: { autoRollback: true },
        }),
      ).toBe("custom");
    });
  });

  it("round-trips: compile then detect then compile is stable", () => {
    for (const preset of AUTOMATION_PRESETS) {
      const once = compilePreset(preset, base());
      const detected = detectPreset(once);
      const twice = compilePreset(detected, once);
      expect(twice).toEqual(once);
    }
  });
});

describe("what each preset permits", () => {
  /**
   * The strategy ceiling, mirrored from `UpdateStrategy.Permits` in Go.
   *
   * This is a DUPLICATE of a backend rule, which normally would be a defect.
   * It is here because the preset descriptions make promises about what will
   * and will not happen -- "a container on 1.27.4 stays on 1.27.4" -- and a
   * table that drifted from the backend would make the UI lie. The Go side is
   * pinned by TestDigestOnlyStillPermitsExactlyWhatItDid; this pins that the
   * PRESET chooses a ceiling whose promises match its words.
   */
  const permits: Record<string, UpdateType[]> = {
    digestOnly: ["digest", "rebind"],
    patch: ["digest", "rebind", "patch"],
    minor: ["digest", "rebind", "patch", "minor"],
    major: ["digest", "rebind", "patch", "minor", "major"],
  };

  const everyType: UpdateType[] = [
    "none",
    "digest",
    "patch",
    "minor",
    "major",
    "prerelease",
    "rebind",
    "unknown",
  ];

  it("Follow current tag permits only digest and rebind", () => {
    const strategy = presetSemantics("followCurrentTag")!.strategy;
    const allowed = permits[strategy] ?? [];

    expect(allowed).toEqual(["digest", "rebind"]);
    for (const type of everyType) {
      const permitted = allowed.includes(type);
      if (["patch", "minor", "major", "prerelease", "unknown", "none"].includes(type)) {
        expect(permitted, `${type} must be refused`).toBe(false);
      }
    }
  });

  it("Allow minor version updates stops below major", () => {
    const allowed = permits[presetSemantics("automaticMinor")!.strategy] ?? [];
    expect(allowed).toContain("minor");
    expect(allowed).not.toContain("major");
    expect(allowed).not.toContain("prerelease");
  });

  it("no preset permits a prerelease or an unknown update", () => {
    for (const preset of AUTOMATION_PRESETS) {
      const semantics = presetSemantics(preset);
      if (!semantics) continue;
      const allowed = permits[semantics.strategy] ?? [];
      expect(allowed, `${preset} must refuse prereleases`).not.toContain("prerelease");
      expect(allowed, `${preset} must refuse unknown updates`).not.toContain("unknown");
    }
  });
});

describe("operator-facing language", () => {
  it("gives every preset a label, a description and a consequence", () => {
    for (const preset of AUTOMATION_PRESETS) {
      expect(PRESET_LABELS[preset]).toBeTruthy();
      expect(PRESET_DESCRIPTIONS[preset]).toBeTruthy();
      if (preset !== "custom") {
        expect(presetConsequence(preset).length).toBeGreaterThan(0);
      }
    }
  });

  it("uses no implementation vocabulary", () => {
    const forbidden = [
      "digestOnly",
      "StrategyDigestOnly",
      "proceedWithCaution",
      "minimumRecommendation",
      "allEligible",
      "UpdateType",
      "autoRollback",
    ];

    const text = AUTOMATION_PRESETS.flatMap((preset) => [
      PRESET_LABELS[preset],
      PRESET_DESCRIPTIONS[preset],
      ...presetConsequence(preset),
    ]).join(" ");

    for (const term of forbidden) {
      expect(text, `operator text must not contain ${term}`).not.toContain(term);
    }
  });

  it("promises eligibility rather than outcomes", () => {
    // "will update" is a promise a policy cannot keep: every gate still runs.
    for (const preset of AUTOMATION_PRESETS) {
      for (const sentence of presetConsequence(preset)) {
        expect(sentence).not.toMatch(/will (be )?updated?\b/i);
        expect(sentence).not.toMatch(/these containers will/i);
      }
    }
  });

  it("says plainly that Follow current tag does not change version", () => {
    const text = presetConsequence("followCurrentTag").join(" ");
    expect(text).toMatch(/digest/i);
    expect(text).toMatch(/1\.27\.4/);
  });
});

// Compile-time proof that a compiled preset IS a policy request, with no
// extra field. If a `preset` key were ever added, this would still typecheck
// -- so the runtime assertion below is what actually guards it.
const _compiles: (p: AutomationPreset, b: UpdatePolicyRequest) => UpdatePolicyRequest =
  compilePreset;
void _compiles;

describe("no preset identity leaks into the request", () => {
  it("adds no field the policy API does not define", () => {
    const allowed = new Set<keyof UpdatePolicyRequest>([
      "name",
      "description",
      "enabled",
      "priority",
      "scope",
      "selector",
      "strategy",
      "minimumRecommendation",
      "mode",
      "window",
      "limits",
      "failure",
    ]);

    for (const preset of AUTOMATION_PRESETS) {
      const compiled = compilePreset(preset, base());
      for (const key of Object.keys(compiled)) {
        expect(
          allowed.has(key as keyof UpdatePolicyRequest),
          `compiled request carries an unexpected field: ${key}`,
        ).toBe(true);
      }
      expect(compiled).not.toHaveProperty("preset");
    }
  });
});
