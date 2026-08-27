import type {
  AutomationMode,
  UpdatePolicyRequest,
  UpdateStrategy,
} from "./automationTypes";
import type { Recommendation } from "./planTypes";

/**
 * Automation presets: the outcome an operator wants, compiled to a policy.
 *
 * # What a preset is, and what it is emphatically not
 *
 * A preset is a **configuration shortcut**. It writes a few fields of an
 * ordinary `UpdatePolicyRequest` and then gets out of the way. After
 * compilation there is nothing left to distinguish a policy that came from a
 * preset from one an operator typed by hand, and the backend is never told
 * which it was.
 *
 * That is the load-bearing property of this file:
 *
 *     A preset may simplify CONFIGURATION.
 *     It may never simplify AUTHORISATION.
 *
 * Every compiled request goes through the same handler, the same
 * `Normalise`, the same `Validate`, and the same runtime gates as a hand-built
 * one. There is no preset field in the API, no preset column in the database,
 * and no branch anywhere in the backend that reads one. If a preset could
 * authorise something a Custom policy could not, the preset would be a second
 * policy model wearing a friendly label.
 *
 * # Why the values here are measurements rather than opinions
 *
 * Each mapping below names the exact fields the current policy model needs to
 * express the outcome, and every value was read off the existing
 * implementation rather than chosen for how it sounds. The two that matter:
 *
 *  - `minimumRecommendation: "proceedWithCaution"` on every preset, including
 *    Observe. This was measured, and the measurement overturned the obvious
 *    answer.
 *
 *    `proceed` is the stricter of the two automatable verdicts and it is what
 *    the create handler defaults to, so it looks like the right floor for a
 *    safety-first preset. It is not, because the risk model reaches the
 *    `medium` band on CAUTION factors alone -- none of which say the change is
 *    unsafe:
 *
 *        republished digest        12   caution
 *        tag is mutable            12   caution   ("latest", "stable", ...)
 *        published < 48 hours ago   8   caution
 *                                  --
 *                                  32   medium -> proceedWithCaution
 *
 *    That row is `image:latest`, freshly republished: the canonical Watchtower
 *    workload, and precisely what Follow current tag exists to serve. At the
 *    `proceed` floor the policy skips it with reason `recommendation`, so the
 *    flagship preset does nothing at all -- and, because the freshness factor
 *    ages out after 48 hours, it would start working later with no explanation.
 *    A mutable tag on an aged image scores 24, one point under the boundary.
 *
 *    Every acting preset permits `digest`, so all three met this. `minor` on
 *    more containers than the blast-radius threshold (15 + 8 + 6 = 29) met it
 *    too.
 *
 *    `proceedWithCaution` widens the floor by exactly one verdict, and that
 *    verdict is the one meaning "probably fine, something is worth reading".
 *    Every WARNING-severity factor still produces `manualReview`, which no
 *    floor may automate: a major version, open drift, a failed readiness check,
 *    a missing snapshot, a platform mismatch, and an unresolved policy
 *    violation are all still held for a person.
 *
 *    Observe carries the same floor deliberately. `Mutates()` is false for it,
 *    but the floor is read at step 8 of `DecideAutomation` and the mode at step
 *    11 -- so a stricter floor would make the observe preview disagree with the
 *    preset it exists to preview.
 *
 *    Pinned end-to-end in `internal/service/automation_preset_floor_test.go`,
 *    which asserts against the real risk model rather than a copied number.
 *
 *  - `strategy: "digestOnly"` for Follow current tag. `UpdateStrategy.Permits`
 *    admits exactly `digest` and `rebind` under that ceiling and refuses
 *    `patch`, `minor`, `major`, `prerelease`, and `unknown`. That is precisely
 *    "stay on the configured tag, move when its content moves", with no new
 *    semantics invented here.
 *
 * # Field ownership
 *
 * A preset owns the fields that decide WHETHER and HOW FAR an update may go,
 * plus the recovery guarantee it promises:
 *
 *     strategy · mode · minimumRecommendation · failure.autoRollback
 *
 * Everything else belongs to the operator and survives a preset change:
 *
 *     name · description · scope · selector · window · priority
 *     failure.pauseAfterFailures · enabled
 *
 * The split is not arbitrary. The four owned fields are the ones that answer
 * "may this container be changed, and into what"; the rest answer "which
 * containers, when, and in what order", which are the operator's decisions and
 * are not what choosing an outcome should overwrite.
 */

/** The outcome vocabulary, in the order the editor offers it. */
export type AutomationPreset =
  | "observe"
  | "safeAutomatic"
  | "followCurrentTag"
  | "automaticMinor"
  | "custom";

/** Every preset, for iteration and for the radio group. */
export const AUTOMATION_PRESETS: AutomationPreset[] = [
  "observe",
  "safeAutomatic",
  "followCurrentTag",
  "automaticMinor",
  "custom",
];

/** The operator-facing name. No implementation vocabulary. */
export const PRESET_LABELS: Record<AutomationPreset, string> = {
  observe: "Observe only",
  safeAutomatic: "Keep containers safely updated",
  followCurrentTag: "Follow current tag",
  automaticMinor: "Allow minor version updates",
  custom: "Custom",
};

/** One sentence, describing the outcome rather than the fields. */
export const PRESET_DESCRIPTIONS: Record<AutomationPreset, string> = {
  observe: "See what HarborMaster would do, without changing any container.",
  safeAutomatic:
    "Let HarborMaster apply patch updates and republished tags on its own.",
  followCurrentTag:
    "Stay on each configured tag, and move only when that tag's digest changes.",
  automaticMinor:
    "Allow patch and minor version changes, but never a major version.",
  custom: "Configure every policy control yourself.",
};

/**
 * The fields a preset owns.
 *
 * Exported because the detector, the switcher, and the tests must all agree on
 * the list, and three copies of it would be three chances to disagree.
 */
export interface PresetSemantics {
  strategy: UpdateStrategy;
  mode: AutomationMode;
  minimumRecommendation: Recommendation;
  autoRollback: boolean;
}

/**
 * The table. One row per preset, and the only place these values exist.
 *
 * `custom` is deliberately absent: it is the ABSENCE of a preset, not a
 * mapping. Asking for its semantics is a question with no answer, and the type
 * system says so.
 */
const PRESET_SEMANTICS: Record<
  Exclude<AutomationPreset, "custom">,
  PresetSemantics
> = {
  /**
   * Observe only.
   *
   * `mode: observe` is the whole of it -- `AutomationMode.Mutates()` is true
   * for `automatic` alone, so nothing here can change a host whatever the
   * other fields say.
   *
   * The strategy is still `patch` rather than the narrowest value, and that is
   * deliberate: an observe policy's job is to REPORT what would happen, and
   * reporting under the same ceiling that "Keep containers safely updated"
   * would apply makes it a preview of that preset rather than of nothing.
   *
   * The recommendation floor matches the acting presets for the same reason,
   * and it is not inert: `DecideAutomation` reads the floor at step 8 and the
   * mode at step 11, so a stricter floor here would silently drop containers
   * out of the preview that an acting preset would have updated.
   *
   * `autoRollback` is true for consistency with the acting presets; that one
   * IS inert in this mode, because there is nothing to roll back.
   */
  observe: {
    strategy: "patch",
    mode: "observe",
    minimumRecommendation: "proceedWithCaution",
    autoRollback: true,
  },

  /**
   * Keep containers safely updated.
   *
   * A `patch` ceiling, which permits republished digests, namespace rebinds,
   * and patch version movement, and refuses minor and major. Narrower than
   * "minor" on purpose: where the brief allowed a choice, the narrower reading
   * wins, and an operator who wants minor movement has a preset that says so.
   *
   * The caution floor, for the measured reason in the file header: this
   * ceiling permits `digest`, and a republished mutable tag reaches the
   * `medium` band on caution factors alone.
   */
  safeAutomatic: {
    strategy: "patch",
    mode: "automatic",
    minimumRecommendation: "proceedWithCaution",
    autoRollback: true,
  },

  /**
   * Follow current tag. The Watchtower-parity preset.
   *
   * `digestOnly` is exactly "the configured reference did not change, its
   * content did", plus the namespace rebind a recreation can require. It
   * cannot move `nginx:1.27.4` to `nginx:1.28.0`: `Permits` refuses every
   * version classification.
   */
  followCurrentTag: {
    strategy: "digestOnly",
    mode: "automatic",
    minimumRecommendation: "proceedWithCaution",
    autoRollback: true,
  },

  /**
   * Allow minor version updates.
   *
   * The `minor` ceiling permits digest, rebind, patch, and minor, and refuses
   * major. Pre-releases are refused by `Permits` under every strategy, so this
   * preset does not make one eligible and no extra field is needed to prevent
   * it.
   */
  automaticMinor: {
    strategy: "minor",
    mode: "automatic",
    minimumRecommendation: "proceedWithCaution",
    autoRollback: true,
  },
};

/**
 * The outcome a new policy opens on, and the fields it opens with.
 *
 * Exported as a pair, and read by the editor for BOTH, because the alternative
 * is what this replaced: the editor selected a preset radio and then
 * initialised its fields from separate literals that had drifted away from it.
 * The form showed "Observe only" and would have sent a `digestOnly` ceiling,
 * so a policy saved without a single edit came back labelled Custom.
 *
 * Non-null by construction -- indexing the table directly rather than going
 * through `presetSemantics`, whose signature admits Custom and therefore null.
 */
export const NEW_POLICY_PRESET = "observe" as const;

/** The fields `NEW_POLICY_PRESET` opens with. See above. */
export const NEW_POLICY_SEMANTICS: PresetSemantics =
  PRESET_SEMANTICS[NEW_POLICY_PRESET];

/** The semantics a preset writes, or null for Custom. */
export function presetSemantics(
  preset: AutomationPreset,
): PresetSemantics | null {
  if (preset === "custom") return null;
  return PRESET_SEMANTICS[preset];
}

/**
 * Compile a preset onto an existing request.
 *
 * # Why this takes a base request rather than building one from nothing
 *
 * Because switching presets must not discard the operator's targeting. The
 * base carries name, scope, selector, exclusions, window, priority, and the
 * pause setting; this replaces only the four owned fields. That is the
 * ownership table, expressed as the one function that applies it.
 *
 * `custom` returns the base untouched: choosing Custom reveals the controls,
 * it does not reset them. That is what makes "Follow current tag -> Custom"
 * show a digest-only automatic policy rather than an empty form.
 */
export function compilePreset(
  preset: AutomationPreset,
  base: UpdatePolicyRequest,
): UpdatePolicyRequest {
  const semantics = presetSemantics(preset);
  if (!semantics) return base;

  return {
    ...base,
    strategy: semantics.strategy,
    mode: semantics.mode,
    minimumRecommendation: semantics.minimumRecommendation,
    failure: {
      ...base.failure,
      autoRollback: semantics.autoRollback,
    },
  };
}

/**
 * Which preset a request EXACTLY matches, or Custom.
 *
 * # Exact, and only over owned fields
 *
 * Two rules pull in opposite directions and both matter. A policy whose safety
 * configuration differs from every preset must read as Custom, or the label
 * would tell an operator something untrue about what their policy does. But a
 * policy that merely targets different containers, or runs in a different
 * window, must still read as its preset -- those are outside preset ownership,
 * and flipping to Custom because somebody added an exclusion would make the
 * label useless.
 *
 * So the comparison is over exactly the four owned fields, and it is equality
 * rather than compatibility. `automatic` + `digestOnly` with a different
 * recommendation floor is NOT Follow current tag; it is a policy that permits
 * more than Follow current tag permits, and it is labelled Custom.
 *
 * An absent `minimumRecommendation` is treated as `proceed`, matching what the
 * create handler stores when a caller omits it. Without that, every policy
 * written by the pre-preset editor -- which never sent the field -- would read
 * as Custom despite being stored with exactly the preset's value.
 */
export function detectPreset(request: UpdatePolicyRequest): AutomationPreset {
  const actual: PresetSemantics = {
    strategy: request.strategy ?? "digestOnly",
    mode: request.mode ?? "observe",
    // The server's create default. See above.
    minimumRecommendation: request.minimumRecommendation ?? "proceed",
    // The editor's long-standing default, and what the server stores when the
    // failure block is sent without it.
    autoRollback: request.failure?.autoRollback ?? true,
  };

  for (const preset of AUTOMATION_PRESETS) {
    const semantics = presetSemantics(preset);
    if (!semantics) continue;
    if (
      semantics.strategy === actual.strategy &&
      semantics.mode === actual.mode &&
      semantics.minimumRecommendation === actual.minimumRecommendation &&
      semantics.autoRollback === actual.autoRollback
    ) {
      return preset;
    }
  }
  return "custom";
}

/**
 * The plain-English consequence of a preset, for the before-save summary.
 *
 * Deliberately about what MAY happen. A policy governs eligibility, not
 * outcomes: a container is updated when every gate passes, and no sentence
 * here may promise that they will.
 *
 * These complement `summariseUpdatePolicy`, which describes the request's
 * fields. This describes the OUTCOME the operator picked, which is the thing
 * they were actually choosing.
 */
export function presetConsequence(preset: AutomationPreset): string[] {
  switch (preset) {
    case "observe":
      return [
        "HarborMaster will evaluate these containers and record what it would do.",
        "It will not download images or replace any container.",
      ];
    case "safeAutomatic":
      return [
        "HarborMaster may replace a container when a patch version is published, " +
          "or when its current tag is republished under the same name.",
        "It will not move to a new minor or major version.",
      ];
    case "followCurrentTag":
      return [
        "HarborMaster may replace a container when its configured image tag " +
          "resolves to a different digest.",
        "It will not move to another version tag: a container on 1.27.4 stays " +
          "on 1.27.4.",
      ];
    case "automaticMinor":
      return [
        "HarborMaster may move a container through patch and minor version " +
          "updates, and through a republished tag.",
        "Major version updates are still held for a person.",
      ];
    case "custom":
      return [];
  }
}

/**
 * What the recommendation floor means, in words.
 *
 * The field is never hidden -- Custom exposes it directly -- but an operator
 * choosing an outcome should not have to know the vocabulary to understand the
 * consequence.
 */
export function describeRecommendationFloor(
  floor: Recommendation | undefined,
): string {
  switch (floor ?? "proceed") {
    case "proceed":
      return (
        "HarborMaster still assesses each individual update, and acts only on " +
        "the ones it rates as straightforward."
      );
    case "proceedWithCaution":
      return (
        "HarborMaster still assesses each individual update, and will also act " +
        "on ones it has flagged for caution."
      );
    default:
      // Validation refuses these on a policy, so this is unreachable through
      // the editor. Rendered honestly rather than as a reassurance.
      return "This policy names a review threshold HarborMaster cannot automate.";
  }
}
