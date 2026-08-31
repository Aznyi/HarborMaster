import type { SimpleUpdatesState } from "./automationTypes";
import { UPDATE_STRATEGY_LABELS } from "./automationTypes";

/**
 * The automatic-updates switch, in words an operator can act on.
 *
 * # Why this is a pure module and not markup
 *
 * Everything below is a claim about what HarborMaster will and will not do. A
 * claim that is wrong is worse than no claim at all: an operator who believes
 * "review-required changes still wait for me" and is wrong finds out when a
 * container they wanted to look at first has already been replaced.
 *
 * So the sentences are derived from the SERVER'S stored policy wherever the
 * policy decides them, they are tested, and they are kept out of the component
 * so a render cannot quietly change one.
 *
 * # The one distinction this file exists to keep
 *
 * Rollback is automatic HERE and not in the manual flow. `Failure.autoRollback`
 * on the managed policy is real — `AutomationService.policyPermitsRollback`
 * re-reads it after a failed recreation and calls the existing rollback service,
 * failing closed if the policy cannot be read. The manual "Apply update" dialog
 * says rollback is NOT automatic, and that is also true, because a person
 * applying an update by hand gets no automatic rollback.
 *
 * Both sentences are correct and they are about different paths. Neither may be
 * copied onto the other.
 */

/** What the switch can be, as one value the UI branches on once. */
export type SimpleUpdatesStatus =
  /** The deployment cannot run automation at all. The switch is unavailable. */
  | "engineOff"
  /** The engine is available and the switch has never been turned on. */
  | "off"
  /** The switch was on and has been turned off. */
  | "turnedOff"
  /** Automatic updates are running. */
  | "on";

export function simpleUpdatesStatus(state: SimpleUpdatesState): SimpleUpdatesStatus {
  if (!state.engineEnabled) return "engineOff";
  if (state.enabled) return "on";
  return state.configured ? "turnedOff" : "off";
}

/** The headline, matched to the status. */
export const SIMPLE_UPDATES_HEADLINE: Record<SimpleUpdatesStatus, string> = {
  engineOff: "Automatic updates are unavailable",
  off: "Automatic updates are off",
  turnedOff: "Automatic updates are off",
  on: "Automatic updates are on",
};

/**
 * What HarborMaster WILL do, once the switch is on.
 *
 * Derived from the stored policy where the policy decides it. The strategy
 * sentence names the actual ceiling rather than a word chosen for how it reads,
 * so a future change to the compiled policy shows up here rather than being
 * quietly contradicted by it.
 */
export function simpleUpdatesWill(state: SimpleUpdatesState): string[] {
  const strategy = state.policy?.strategy;
  const ceiling = strategy
    ? (UPDATE_STRATEGY_LABELS[strategy] ?? strategy)
    : "the configured ceiling";

  return [
    "check eligible containers against the registries their images come from",
    `apply changes up to ${ceiling.toLowerCase()}, and no further`,
    "download the approved image and verify its digest before anything is replaced",
    "recreate the container from the configuration HarborMaster recorded for it",
    "verify the replacement's health, image and preserved configuration",
    // Accurate for THIS path, and only this path. See the module comment.
    "put the original back automatically if the replacement fails its checks",
  ];
}

/**
 * What HarborMaster will NOT do.
 *
 * Every line is a gate that exists in the engine today. None of them is created
 * or relaxed by the switch: the managed policy is an ordinary policy, and these
 * apply to every policy.
 */
export function simpleUpdatesWillNot(): string[] {
  return [
    "apply a change the planner flagged for review — those still wait for you",
    "touch a container carrying io.harbormaster.update.enabled=false",
    "touch a container whose automation you have paused",
    "update itself; HarborMaster refuses to replace its own container",
    "update before a container's dependencies are ready",
    "act outside a maintenance window you set on a policy of your own",
    "override a policy you wrote — yours takes precedence for the containers it names",
  ];
}

/**
 * The sentence for a deployment whose engine is off.
 *
 * Names the variable the SERVER reported rather than one hardcoded here, so the
 * instruction cannot drift from what the deployment actually reads.
 */
export function engineOffExplanation(state: SimpleUpdatesState): string {
  return (
    `The automation engine is disabled for this HarborMaster deployment, so ` +
    `automatic updates cannot be turned on from here. Set ${state.engineVariable}=true ` +
    `in the deployment's environment and restart HarborMaster.`
  );
}

/**
 * Whether existing policies mean the switch will not reach everything.
 *
 * Not a warning about danger — a narrower rule winning is the designed
 * behaviour. It is here because "I turned it on and that container still is not
 * updating" is otherwise a mystery.
 */
export function overrideNotice(state: SimpleUpdatesState): string | null {
  const overrides = state.overriddenBy ?? [];
  if (overrides.length === 0) return null;
  const names = overrides.map((o) => o.name).join(", ");
  return (
    `${overrides.length} policy you wrote ${overrides.length === 1 ? "takes" : "take"} ` +
    `precedence over automatic updates for the containers ${overrides.length === 1 ? "it names" : "they name"}: ` +
    `${names}. Automatic updates cover everything else.`
  );
}
