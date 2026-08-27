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
    expect(request.minimumRecommendation).toBe("proceedWithCaution");
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
    expect(request.minimumRecommendation).toBe("proceedWithCaution");
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

  it("never compiles a recommendation floor automation cannot act on", () => {
    // The real invariant, which survived the floor being measured.
    //
    // `Validate` admits exactly two floors: `proceed` and `proceedWithCaution`.
    // `manualReview`, `notRecommended` and `unknown` are refused, because the
    // first two mean a person has to look and the third means the model argued
    // against it. A preset that wrote one of those would produce a policy the
    // API rejects, or -- worse, if validation ever loosened -- one that claimed
    // to automate what nobody may automate.
    //
    // Which of the two admitted values each preset uses is a measurement, and
    // it lives in the floor contract test rather than here:
    // internal/service/automation_preset_floor_test.go
    for (const preset of AUTOMATION_PRESETS) {
      const semantics = presetSemantics(preset);
      if (!semantics) continue;
      expect(["proceed", "proceedWithCaution"]).toContain(
        semantics.minimumRecommendation,
      );
    }
  });

  it("gives every preset the same recommendation floor", () => {
    // Observe included. An observe policy previews the acting presets, and
    // `DecideAutomation` reads the floor at step 8 and the mode at step 11 --
    // so a preview with a different floor is a preview of a policy nobody has.
    const floors = AUTOMATION_PRESETS.map(presetSemantics)
      .filter((semantics) => semantics !== null)
      .map((semantics) => semantics.minimumRecommendation);

    expect(new Set(floors).size).toBe(1);
    expect(floors[0]).toBe("proceedWithCaution");
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
    // The default still resolves the way the server resolves it: a request
    // without the field is stored as `proceed`, so that is what the detector
    // must read.
    //
    // What that now means has changed. `proceed` is no longer any preset's
    // floor, so a policy written by the pre-preset editor reports Custom --
    // and that is the correct answer rather than a regression. Such a policy
    // really does behave differently from Follow current tag: it refuses a
    // republished mutable tag, which is the workload the preset exists for.
    // Labelling it with the preset's name would tell the operator it does
    // something it does not do, which is the one thing this detector must
    // never do.
    //
    // The remedy is in the operator's hands and takes one click: re-selecting
    // the preset recompiles the floor. Nothing about the stored policy changes
    // until they save.
    const legacy: UpdatePolicyRequest = {
      strategy: "digestOnly",
      mode: "automatic",
      failure: { autoRollback: true },
    };
    expect(detectPreset(legacy)).toBe("custom");

    // The same policy carrying the field explicitly reads the same way, which
    // is what proves the omission is being resolved rather than special-cased.
    expect(detectPreset({ ...legacy, minimumRecommendation: "proceed" })).toBe(
      "custom",
    );
  });

  describe("near misses, one field at a time", () => {
    const cases: { name: string; change: Partial<UpdatePolicyRequest> }[] = [
      {
        name: "a stricter recommendation floor",
        change: { minimumRecommendation: "proceed" },
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

    it("a digest-only automatic policy with a strict floor is Custom", () => {
      // The brief's worked example, in the direction the measured floor puts
      // it. This policy permits strictly LESS than Follow current tag does --
      // it refuses every update that lands in the medium band on caution
      // factors alone -- so labelling it Follow current tag would tell the
      // operator something untrue in the other direction.
      //
      // Detection is equality over the owned fields, not compatibility, which
      // is what makes both directions come out as Custom without a rule for
      // each.
      expect(
        detectPreset({
          strategy: "digestOnly",
          mode: "automatic",
          minimumRecommendation: "proceed",
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
