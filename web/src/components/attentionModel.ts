import type { AutomationStatus } from "../api/automationTypes";
import type { DependencySummary } from "../api/dependencyTypes";
import type { DriftSummary } from "../api/driftTypes";
import type { EventEngineStatus } from "../api/eventTypes";
import type { ExecutionSummary } from "../api/executionTypes";
import type { InventoryStatus } from "../api/inventoryTypes";
import type { ChangePlanSummary, PlannerStatus } from "../api/planTypes";
import type { PolicySummary } from "../api/policyTypes";
import type { RollbackSummary } from "../api/rollbackTypes";
import type { HealthReport } from "../api/types";

/**
 * The operator-attention model.
 *
 * # What problem this solves
 *
 * The dashboard used to be nine panels of subsystem telemetry in the order the
 * subsystems were built. An operator could read the inventory generation, the
 * inventory checksum, the event queue depth and the reconnect count before
 * reaching anything that said a container was unhealthy -- and there was no
 * single place that answered "does anything need me".
 *
 * This turns the summaries HarborMaster already computes into a short, ordered
 * list of things a person could act on, each one linking to the page that
 * explains it.
 *
 * # Why it is a pure function
 *
 * Because the interesting rules are the ones about EVIDENCE, and they are
 * worth testing exhaustively:
 *
 *   - Nothing is ever asserted from an absent input. A summary that failed to
 *     load produces no item at all, never a reassuring one.
 *   - "Nothing has been checked" is reported as its own item, not folded into
 *     "all clear". An estate with no policies and no plans is not compliant
 *     and not up to date; it is unexamined, and saying so is the difference
 *     between an operator configuring HarborMaster and an operator trusting a
 *     green screen.
 *   - A degraded dependency is always an item, whatever else is true.
 *
 * It reads no clock and performs no I/O. Every input is a payload the
 * dashboard already fetched.
 */

/** How loudly an item asks for the operator. */
export type AttentionLevel = "action" | "watch" | "info";

export interface AttentionItem {
  /** Stable identity, used as a React key and in tests. */
  id: string;
  level: AttentionLevel;
  /** The headline, written as a fact rather than a metric name. */
  title: string;
  /** One sentence saying what it means and what to do. */
  detail: string;
  /** Where the operator goes to act on it. */
  to: string;
  /** The number behind the item, when there is one worth showing. */
  count?: number;
}

/**
 * Everything the model reads.
 *
 * Every field is optional and `undefined` means NOT LOADED -- which is
 * different from loaded-and-empty, and must not produce an item in either
 * direction. A dashboard that inferred "no failures" from a summary that never
 * arrived would be inventing certainty.
 */
export interface AttentionInputs {
  health?: HealthReport | null;
  inventory?: InventoryStatus | null;
  events?: EventEngineStatus | null;
  automation?: AutomationStatus | null;
  plans?: ChangePlanSummary | null;
  /**
   * The planner's own state, from the same plan read the summary comes from.
   *
   * Needed because "no policy yet" is only worth an operator's attention once
   * the estate has actually been assessed. Raising it while HarborMaster is
   * still starting up asks somebody to fix something that is not broken.
   */
  planner?: PlannerStatus | null;
  executions?: ExecutionSummary | null;
  rollbacks?: RollbackSummary | null;
  policy?: PolicySummary | null;
  drift?: DriftSummary | null;
  /** False when the account cannot read automation at all. */
  canReadAutomation?: boolean;

  /**
   * The dependency picture, when the account can read it and it loaded.
   *
   * `undefined` means NOT LOADED and produces no item, like every other input.
   * That matters more here than elsewhere: a dependency graph that could not be
   * built is HarborMaster refusing to order the estate, and inferring "no loops"
   * from a read that never arrived would be inventing exactly the reassurance
   * the subsystem exists to withhold.
   */
  dependencies?: DependencySummary | null;
}

const LEVEL_ORDER: Record<AttentionLevel, number> = {
  action: 0,
  watch: 1,
  info: 2,
};

/**
 * Builds the ordered attention list.
 *
 * Order is by level and then by the order the rules appear below, which is
 * deliberate: within "needs you", a failed update that left containers on the
 * host outranks a routine approval, and a broken Docker connection outranks
 * both because nothing else HarborMaster reports can be trusted while it
 * holds.
 */
export function buildAttention(inputs: AttentionInputs): AttentionItem[] {
  const items: AttentionItem[] = [];
  const add = (item: AttentionItem) => items.push(item);

  // ---------------------------------------------------------- onboarding --
  //
  // Two states, and only two, are worth an operator's attention here. Both
  // clear themselves as soon as they are addressed, so neither becomes a
  // permanent fixture on a configured installation.
  //
  // Everything else a fresh install can be -- starting up, assessing, observing
  // deliberately, configured with nothing currently eligible -- is either
  // waiting on HarborMaster or working as asked. An item for any of those would
  // be telling somebody to fix something that is not broken, which is how an
  // attention list stops being read.
  if (inputs.health?.features && inputs.automation && inputs.canReadAutomation) {
    const assessed = Boolean(inputs.planner?.lastRunAt);
    const acting = inputs.automation.actingPolicies ?? 0;
    const policies = inputs.automation.policies ?? 0;

    if (!assessed) {
      // Nothing below is meaningful yet: an estate nobody has assessed has no
      // opinion about policies. Deliberately silent rather than informational,
      // because this window is normally seconds long.
    } else if (!inputs.health.features.automation && acting > 0) {
      // The genuinely confusing state: somebody wrote a policy that says
      // "automatic" on an installation where the engine cannot run it. Saving
      // the policy did not enable anything, and nothing else on the interface
      // says so.
      add({
        id: "automation-engine-disabled",
        level: "watch",
        title: "Automatic policies cannot run",
        detail:
          "An update policy is set to update containers automatically, but the " +
          "automation engine is disabled on this installation. Saving the " +
          "policy does not enable it.",
        to: "/automation",
        count: acting,
      });
    } else if (inputs.health.features.automation && policies === 0) {
      // The first-run item. Clears on the first policy of any kind, including
      // an observe-only one.
      add({
        id: "automation-not-configured",
        level: "info",
        title: "Automatic updates are not configured",
        detail:
          "The update engine is running and no policy tells it what to do. " +
          "HarborMaster is assessing containers and will not change any of them.",
        to: "/automation",
      });
    }
  }

  // ------------------------------------------------- HarborMaster itself --
  //
  // First, and never suppressed. Every other statement on this page is a
  // report about the host, and each one is only as good as the connection it
  // was read over.

  if (inputs.health && inputs.health.docker.status !== "up") {
    add({
      id: "docker-down",
      level: "action",
      title: "HarborMaster cannot reach Docker",
      detail:
        "Nothing on this page is current while this holds. Container state, " +
        "updates and compliance are all read through the Docker socket.",
      to: "/settings",
    });
  }
  if (inputs.health && inputs.health.database.status !== "up") {
    add({
      id: "database-down",
      level: "action",
      title: "The database is not healthy",
      detail:
        "HarborMaster stores everything it knows here. Check the reliability " +
        "runbook before restarting.",
      to: "/settings",
    });
  }
  if (inputs.inventory && inputs.inventory.state === "failed") {
    add({
      id: "refresh-failed",
      level: "watch",
      title: "The last inventory refresh failed",
      detail:
        "The figures below describe the previous successful refresh, so they " +
        "may be out of date.",
      to: "/events",
    });
  }
  if (inputs.events && inputs.events.enabled && inputs.events.state !== "connected") {
    add({
      id: "events-disconnected",
      level: "watch",
      title: "The Docker event stream is disconnected",
      detail:
        "HarborMaster has fallen back to periodic reconciliation, so container " +
        "state can lag by up to one interval.",
      to: "/events",
    });
  }

  // --------------------------------------------------------- the estate --

  if (inputs.inventory && (inputs.inventory.counts?.unhealthy ?? 0) > 0) {
    const count = inputs.inventory.counts.unhealthy;
    add({
      id: "unhealthy",
      level: "action",
      title: `${count} container${count === 1 ? "" : "s"} ${
        count === 1 ? "is" : "are"
      } unhealthy`,
      detail: "The container's own healthcheck is failing.",
      to: "/containers?health=unhealthy",
      count,
    });
  }

  // ------------------------------------------------------- what it did --
  //
  // A failure that left containers on the host is the single most urgent
  // thing HarborMaster can report about its own work: the operator has
  // something to clean up, and until they do the workload may be down.

  if (inputs.executions && inputs.executions.needsAttention > 0) {
    const count = inputs.executions.needsAttention;
    add({
      id: "executions-need-attention",
      level: "action",
      title: `${count} update${count === 1 ? "" : "s"} left containers behind`,
      detail:
        "An update failed after it had begun changing the host. Each record " +
        "carries the recovery steps.",
      to: "/executions",
      count,
    });
  }
  if (inputs.rollbacks && inputs.rollbacks.needsAttention > 0) {
    const count = inputs.rollbacks.needsAttention;
    add({
      id: "rollbacks-need-attention",
      level: "action",
      title: `${count} rollback${count === 1 ? "" : "s"} did not complete`,
      detail:
        "A rollback failed part-way. The original container may still be " +
        "parked under a renamed container.",
      to: "/rollbacks",
      count,
    });
  }

  // ------------------------------------------------------- automation --

  if (inputs.canReadAutomation !== false && inputs.automation) {
    const waiting = inputs.automation.awaitingApproval ?? 0;
    const paused = inputs.automation.pausedContainers ?? 0;

    if (waiting > 0) {
      add({
        id: "awaiting-approval",
        level: "action",
        title: `${waiting} update${waiting === 1 ? "" : "s"} waiting for your approval`,
        detail:
          "A policy has decided on a change and is holding it until somebody " +
          "releases it.",
        to: "/automation/approvals",
        count: waiting,
      });
    }
    if (paused > 0) {
      add({
        id: "automation-paused",
        level: "action",
        title: `Automation has given up on ${paused} container${paused === 1 ? "" : "s"}`,
        detail:
          "Repeated failures paused these containers. They will not be updated " +
          "automatically until somebody looks.",
        to: "/automation/paused",
        count: paused,
      });
    }
  }

  // ------------------------------------------------------ dependencies --
  //
  // Only what nothing resolves by itself. A failed reattachment is the loudest
  // because of what it means on the host -- a container possibly cut off from
  // the namespace it was sharing -- and HarborMaster never retries one.
  //
  // Waiting is deliberately absent. It is the system working, it clears without
  // anybody, and there is no clock in this function to distinguish "waiting"
  // from "waiting too long" honestly.

  if (inputs.dependencies) {
    const { cycles, unresolved, rebindsFailed } = inputs.dependencies;

    if (rebindsFailed > 0) {
      add({
        id: "dependency-rebind-failed",
        level: "action",
        title: `${rebindsFailed} container${rebindsFailed === 1 ? "" : "s"} could not be reattached`,
        detail:
          "HarborMaster replaced a container others shared a namespace with " +
          "and could not reattach all of them. It does not retry a " +
          "reattachment by itself. No image version was changed.",
        to: "/dependencies",
        count: rebindsFailed,
      });
    }
    // A coordinated update that ended part-finished is deliberately NOT its own
    // item. The two ways it can end are already reported: a failed reattachment
    // by the item above, and a failed provider update by
    // `executions-need-attention`. A third item would count one incident twice
    // on a list an operator reads as a count of incidents.

    if (cycles > 0) {
      add({
        id: "dependency-cycle",
        level: "action",
        title: `${cycles} dependency loop${cycles === 1 ? "" : "s"} need${
          cycles === 1 ? "s" : ""
        } attention`,
        detail:
          "These containers depend on each other in a loop, so no safe update " +
          "order exists. Nothing will break the loop on its own.",
        to: "/dependencies",
        count: cycles,
      });
    }
    if (unresolved > 0) {
      add({
        id: "dependency-unresolved",
        level: "action",
        title: `${unresolved} container${unresolved === 1 ? "" : "s"} share${
          unresolved === 1 ? "s" : ""
        } a namespace HarborMaster cannot resolve`,
        detail:
          "HarborMaster will not update these until it can establish what " +
          "they depend on. This is a refusal, not a finding that they have no " +
          "dependencies.",
        to: "/dependencies",
        count: unresolved,
      });
    }
  }

  // ------------------------------------------------------ what is open --

  if (inputs.policy) {
    const critical =
      (inputs.policy.bySeverity?.critical ?? 0) + (inputs.policy.bySeverity?.high ?? 0);
    if (critical > 0) {
      add({
        id: "policy-critical",
        level: "action",
        title: `${critical} critical or high policy finding${critical === 1 ? "" : "s"}`,
        detail: "Containers are running configuration a policy forbids.",
        to: "/compliance",
        count: critical,
      });
    } else if (inputs.policy.open > 0) {
      add({
        id: "policy-open",
        level: "watch",
        title: `${inputs.policy.open} open policy finding${inputs.policy.open === 1 ? "" : "s"}`,
        detail: "None of them is critical or high.",
        to: "/compliance",
        count: inputs.policy.open,
      });
    }
  }

  if (inputs.drift && inputs.drift.open > 0) {
    add({
      id: "drift-open",
      level: "watch",
      title: `${inputs.drift.containersWithDrift} container${
        inputs.drift.containersWithDrift === 1 ? "" : "s"
      } moved from ${inputs.drift.containersWithDrift === 1 ? "its" : "their"} baseline`,
      detail:
        "Something changed these containers outside HarborMaster. Drift is an " +
        "observation; nothing here changes a container back.",
      to: "/drift",
      count: inputs.drift.open,
    });
  }

  // ---------------------------------------------------------- updates --

  if (inputs.plans) {
    if (inputs.plans.needsReview > 0) {
      add({
        id: "plans-need-review",
        level: "watch",
        title: `${inputs.plans.needsReview} update${
          inputs.plans.needsReview === 1 ? "" : "s"
        } ask for a person`,
        detail: "HarborMaster assessed these and wants somebody to look first.",
        to: "/plans",
        count: inputs.plans.needsReview,
      });
    }
    if (inputs.plans.actionable > 0) {
      add({
        id: "plans-actionable",
        level: "watch",
        title: `${inputs.plans.actionable} update${
          inputs.plans.actionable === 1 ? "" : "s"
        } available`,
        detail: "Nothing in the assessment argues against these.",
        to: "/plans",
        count: inputs.plans.actionable,
      });
    }
    if (inputs.plans.undetermined > 0) {
      // Reported, never folded into "available". An update HarborMaster
      // cannot judge is not a safe one and it is not an absent one.
      add({
        id: "plans-undetermined",
        level: "info",
        title: `${inputs.plans.undetermined} update${
          inputs.plans.undetermined === 1 ? "" : "s"
        } HarborMaster cannot judge`,
        detail:
          "A calendar tag, an incomplete tag listing, or a comparison the " +
          "version parser refused to make. Reported rather than guessed.",
        to: "/plans",
        count: inputs.plans.undetermined,
      });
    }
  }

  // --------------------------------------------------- gaps in evidence --
  //
  // The items that stop an unexamined estate reading as a healthy one. Each
  // is `info`: nothing is wrong, but nothing has been established either.

  if (inputs.policy && inputs.policy.policiesTotal === 0) {
    add({
      id: "no-policies",
      level: "info",
      title: "No compliance policies are defined",
      detail:
        "Nothing has been checked, so an empty compliance page is not a clean " +
        "bill of health.",
      to: "/policies",
    });
  }
  if (inputs.drift && inputs.drift.containersEvaluated === 0) {
    add({
      id: "no-drift-baseline",
      level: "info",
      title: "No container has a configuration baseline yet",
      detail:
        "Drift is measured against a snapshot. Until one exists, nothing can " +
        "be found to have moved.",
      to: "/snapshots",
    });
  }
  if (inputs.plans && inputs.plans.containers === 0) {
    add({
      id: "no-plans",
      level: "info",
      title: "No container has been assessed for updates yet",
      detail:
        "An empty update list here means HarborMaster has not looked, not that " +
        "everything is current.",
      to: "/plans",
    });
  }
  if (
    inputs.canReadAutomation !== false &&
    inputs.automation &&
    !inputs.automation.enabled
  ) {
    add({
      id: "automation-off",
      level: "info",
      title: "Automatic updating is off",
      detail:
        "Policies can be written and reviewed, and nothing will act on them. " +
        "Updates happen when somebody starts one.",
      to: "/automation",
    });
  }

  return items.sort((a, b) => LEVEL_ORDER[a.level] - LEVEL_ORDER[b.level]);
}

/** The items at one level, in the order the model produced them. */
export function atLevel(
  items: AttentionItem[],
  level: AttentionLevel,
): AttentionItem[] {
  return items.filter((item) => item.level === level);
}

/**
 * Whether the model can say "nothing needs you" at all.
 *
 * Only when every input it would have read actually arrived. A dashboard
 * cannot report an all-clear over summaries it never received, and this is the
 * guard that stops it trying.
 */
export function evidenceComplete(inputs: AttentionInputs): boolean {
  const required = [
    inputs.health,
    inputs.inventory,
    inputs.plans,
    inputs.executions,
    inputs.policy,
    inputs.drift,
  ];
  return required.every((value) => Boolean(value));
}
