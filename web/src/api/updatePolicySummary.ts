import type {
  AutomationMode,
  MaintenanceWindow,
  UpdateFailureHandling,
  UpdatePolicy,
  UpdatePolicyRequest,
  UpdateScope,
  UpdateSelector,
  UpdateStrategy,
} from "./automationTypes";

/**
 * A policy, in a sentence.
 *
 * # Why this file exists at all
 *
 * An update policy has a scope, a selector, a ceiling, a mode, a window, and a
 * failure plan. Read as six controls it is a form. Read as one sentence it is a
 * decision — and the decision is the thing an operator is actually making, so
 * they should be able to read it back before they save.
 *
 * # The anti-drift rule, and how it is enforced
 *
 * **This renders the request that is about to be sent, not the form that
 * produced it.** The editor builds one `UpdatePolicyRequest`, hands the same
 * object to this function and to the API client, and shows the result. There is
 * no second model of what the policy means, so there is nothing for the summary
 * to drift away from: a summary that was wrong would be a request that was
 * wrong, and the operator would be reading the actual defect rather than a
 * description of something else.
 *
 * The same function renders a STORED policy on the cards, because `UpdatePolicy`
 * is `UpdatePolicyRequest` with the optional fields filled in. One
 * interpretation, two places, and the card cannot disagree with the editor.
 *
 * # It describes, it does not reassure
 *
 * Every clause is derived from a field. Nothing here says "safely", "securely",
 * or "HarborMaster will make sure" — the summary's job is to state what the
 * settings mean, including when what they mean is alarming.
 */

/** The fields a summary is derived from. Both request and stored policy fit. */
export interface SummarisablePolicy {
  scope?: UpdateScope;
  selector?: UpdateSelector;
  strategy?: UpdateStrategy;
  mode?: AutomationMode;
  window?: MaintenanceWindow;
  failure?: UpdateFailureHandling;
  enabled?: boolean;
  archived?: boolean;
}

/** Compile-time proof that both shapes are summarisable. */
const _requestIsSummarisable: (r: UpdatePolicyRequest) => SummarisablePolicy = (
  r,
) => r;
const _policyIsSummarisable: (p: UpdatePolicy) => SummarisablePolicy = (p) => p;
void _requestIsSummarisable;
void _policyIsSummarisable;

/**
 * The verb, which is the whole meaning of the mode.
 *
 * `observe` and `dryRun` are "will look at". `approvalRequired` is "will
 * prepare". Only `automatic` is "may update", and it is the only one written
 * in a way that admits the host changes.
 */
function verbFor(mode: AutomationMode | undefined): string {
  switch (mode) {
    case "automatic":
      return "may automatically update";
    case "approvalRequired":
      return "will prepare updates for";
    case "dryRun":
      return "will evaluate, and record the order it would act on,";
    default:
      return "will observe";
  }
}

/** What the policy is pointed at, in words. */
export function describeScope(policy: SummarisablePolicy): string {
  const selector = policy.selector ?? {};

  if (policy.scope === "allEligible") {
    return "all eligible containers";
  }

  const parts: string[] = [];
  const include = selector.include ?? [];
  const images = selector.images ?? [];
  const labels = Object.entries(selector.labels ?? {});

  if (include.length > 0) {
    parts.push(joinList(include));
  }
  if (images.length > 0) {
    parts.push(`containers running ${joinList(images)}`);
  }
  if (labels.length > 0) {
    parts.push(
      `containers labelled ${joinList(
        labels.map(([key, value]) => (value ? `${key}=${value}` : key)),
      )}`,
    );
  }

  if (parts.length === 0) return "no containers";
  return parts.join(", and ");
}

/**
 * The exclusion clause, or nothing when there is none.
 *
 * Closed with its own comma. The clause is interposed between the scope and the
 * ceiling, and without the trailing comma the two run together — "all eligible
 * containers, except database through minor versions" reads as an exclusion OF
 * minor versions rather than of a container. A summary that can be misread
 * about what is excluded is worse than no summary.
 */
function describeExclusions(selector: UpdateSelector | undefined): string {
  const exclude = selector?.exclude ?? [];
  if (exclude.length === 0) return "";
  return `, except ${joinList(exclude)},`;
}

/** The ceiling, phrased as a ceiling. */
export function describeCeiling(strategy: UpdateStrategy | undefined): string {
  switch (strategy) {
    case "major":
      return "up to and including major versions";
    case "minor":
      return "through minor versions";
    case "patch":
      return "through patch versions";
    default:
      return "only when the same tag is republished";
  }
}

/** When it may act. */
export function describeSchedule(window: MaintenanceWindow | undefined): string {
  if (!window || window.alwaysOpen !== false) return "at any time";

  const start = window.start ?? "";
  const end = window.end ?? "";
  if (!start || !end) return "at any time";

  const zone = window.timezone ? ` ${window.timezone}` : "";
  const days = describeWeekdays(window.weekdays);
  return `between ${start} and ${end}${zone}${days}`;
}

const WEEKDAY_NAMES = [
  "Sunday",
  "Monday",
  "Tuesday",
  "Wednesday",
  "Thursday",
  "Friday",
  "Saturday",
];

function describeWeekdays(weekdays: number[] | undefined): string {
  if (!weekdays || weekdays.length === 0 || weekdays.length === 7) return "";
  const named: string[] = [];
  for (const day of weekdays) {
    const name = WEEKDAY_NAMES[day];
    // A value outside 0..6 is not a weekday this build can name. Dropped
    // rather than rendered as "undefined": the summary must never show an
    // operator a word HarborMaster did not choose.
    if (name) named.push(name);
  }
  if (named.length === 0) return "";
  return ` on ${joinList(named)}`;
}

/**
 * What happens when an update fails.
 *
 * Only rendered for the modes that can produce a failure. A policy in observe
 * mode has no failure behaviour to describe, and describing one would imply it
 * does something it cannot.
 */
export function describeFailureHandling(
  policy: SummarisablePolicy,
): string {
  if (policy.mode !== "automatic" && policy.mode !== "approvalRequired") {
    return "";
  }

  const failure = policy.failure ?? {};
  const clauses: string[] = [];

  if (failure.autoRollback) {
    // The pause is stated because the engine always applies it after a
    // rollback, whatever pauseAfterFailures says. A summary that promised only
    // the rollback would be describing half of what happens.
    clauses.push(
      "Failed verification rolls the container back and pauses it for review",
    );
  } else {
    clauses.push(
      "A failed update is left in place for a person to resolve, and the container is not rolled back",
    );
  }

  const pauseAfter = failure.pauseAfterFailures ?? 0;
  if (pauseAfter > 0) {
    clauses.push(
      `automation pauses after ${pauseAfter} ${
        pauseAfter === 1 ? "failure" : "failures"
      }`,
    );
  } else {
    clauses.push(
      "automation is never paused by repeated failures, so a container that fails every pass is retried every pass",
    );
  }

  return `${clauses.join(", and ")}.`;
}

/**
 * The whole policy, in one or two sentences.
 *
 * Sentence one is always what it covers, how far, and when. Sentence two is
 * failure handling when the mode can produce a failure, and the "nothing will
 * change" reassurance when it cannot — which is the one thing an operator
 * setting up their first policy most needs to read.
 */
export function summariseUpdatePolicy(policy: SummarisablePolicy): string {
  const scope = describeScope(policy);
  const exclusions = describeExclusions(policy.selector);
  const verb = verbFor(policy.mode);
  const ceiling = describeCeiling(policy.strategy);
  const schedule = describeSchedule(policy.window);

  const sentences: string[] = [];

  if (policy.mode === "automatic") {
    sentences.push(
      `HarborMaster ${verb} ${scope}${exclusions} ${ceiling}, ${schedule}.`,
    );
  } else if (policy.mode === "approvalRequired") {
    sentences.push(
      `HarborMaster ${verb} ${scope}${exclusions} ${ceiling}, ${schedule}, and waits for a person before changing any container.`,
    );
  } else {
    // The non-acting modes put the ceiling in its own sentence, because
    // "would be considered" is the whole point: nothing is applied, and the
    // ceiling describes a hypothetical rather than a permission in force.
    const trimmed = exclusions.replace(/,$/, "");
    sentences.push(
      `HarborMaster ${verb} ${scope}${trimmed} ${schedule}. Updates ${ceiling} would be considered.`,
    );
  }

  const failure = describeFailureHandling(policy);
  if (failure) {
    sentences.push(failure);
  } else {
    sentences.push(
      policy.mode === "dryRun"
        ? "No container will be changed while this policy remains in Dry run mode."
        : "No container will be changed while this policy remains in Observe mode.",
    );
  }

  // A policy that is switched off or withdrawn does nothing at all, and that
  // outranks every sentence above. Said last so it is the thing a reader
  // finishes on.
  if (policy.archived) {
    sentences.push("This policy is withdrawn and is not evaluated.");
  } else if (policy.enabled === false) {
    sentences.push("This policy is switched off and is not evaluated.");
  }

  return sentences.join(" ");
}

/** Joins a list the way a person writes one. */
function joinList(values: string[]): string {
  if (values.length === 0) return "";
  if (values.length === 1) return values[0] ?? "";
  if (values.length === 2) return `${values[0]} and ${values[1]}`;
  return `${values.slice(0, -1).join(", ")}, and ${values[values.length - 1]}`;
}
