import type { Acquisition } from "./acquisitionTypes";
import type { Execution } from "./executionTypes";
import type { Rollback } from "./rollbackTypes";
import type { BadgeTone } from "../components/StatusBadge";

/**
 * The Activity feed's view model.
 *
 * # The question this answers
 *
 * "What happened to my containers?" -- not "which internal lifecycle record
 * would you like to inspect?". HarborMaster stores an attempted update as three
 * records in three tables, and an operator had to know that vocabulary to find
 * out whether their container came back up.
 *
 * # Why this is not `updateWorkspace.ts`
 *
 * Phase 2's joiner answers a different question -- "what can change now" -- from
 * the CURRENT plan of each container. It keys by container and deliberately
 * keeps one row per container. A history keys by ATTEMPT and keeps every one,
 * including several for the same container. Sharing the joiner would mean one
 * of the two questions being answered wrongly.
 *
 * # What the join is, and how reliable it is
 *
 *     acquisition ──acquisitionId──▶ execution ──executionId──▶ rollback(s)
 *
 * Every arrow is a stored foreign key, so the join is exact rather than
 * heuristic. Two things are NOT exact and are handled explicitly:
 *
 *   - an execution may have SEVERAL rollback attempts. The schema constrains
 *     only succeeded ones (`idx_rollback_execution_succeeded` is partial), so a
 *     failed rollback can be retried. The succeeded attempt wins; otherwise the
 *     most recent does;
 *   - the three lists paginate INDEPENDENTLY. A merged timeline is therefore
 *     only complete within the window actually loaded, and `feedHorizon` reports
 *     where that window ends so the page can say so rather than imply the
 *     history stops there.
 */

/** One attempted update, however far it got. */
export interface ActivityEntry {
  /** Stable across renders: the acquisition when there is one, else the record. */
  key: string;
  containerName: string;
  containerId?: string;

  acquisition?: Acquisition;
  execution?: Execution;
  /** The attempt that decided the outcome. */
  rollback?: Rollback;
  /** Every attempt, newest first, for the details disclosure. */
  rollbackAttempts: Rollback[];

  /** When this attempt last did something. Drives the ordering. */
  at: string;
  status: ActivityStatus;
}

export type ActivityStatusKind =
  | "succeeded"
  | "recovered"
  | "failed"
  | "rollbackFailed"
  | "inProgress"
  | "waiting"
  | "cancelled";

export interface ActivityStatus {
  kind: ActivityStatusKind;
  label: string;
  tone: BadgeTone;
  /** What happened, in one sentence, without naming a record type. */
  detail: string;
  /** True when a person still has something to do about it. */
  needsAttention: boolean;
}

const TERMINAL = new Set(["succeeded", "failed", "cancelled", "expired"]);

function isActive(state?: string): boolean {
  return state !== undefined && !TERMINAL.has(state);
}

/**
 * Reads the outcome off the records, in the order that decides it.
 *
 * Rollback outcome is read BEFORE execution outcome, deliberately. A failed
 * recreation that was rolled back successfully is a container that came back --
 * reporting it as an unresolved failure forever is how a history stops being
 * read.
 */
export function statusOf(
  acquisition?: Acquisition,
  execution?: Execution,
  rollback?: Rollback,
): ActivityStatus {
  if (rollback) {
    if (isActive(rollback.state)) {
      return {
        kind: "inProgress",
        label: "Rolling back",
        tone: "warn",
        detail: "The update failed and HarborMaster is restoring the original container.",
        needsAttention: false,
      };
    }
    if (rollback.state === "succeeded") {
      return {
        kind: "recovered",
        label: "Recovered",
        tone: "ok",
        detail: "The update failed and the original container was restored.",
        needsAttention: false,
      };
    }
    if (rollback.state === "failed") {
      return {
        kind: "rollbackFailed",
        label: "Rollback failed",
        tone: "danger",
        detail:
          rollback.message ??
          "The update failed and the attempt to restore the original did not succeed.",
        needsAttention: true,
      };
    }
  }

  if (execution) {
    if (isActive(execution.state)) {
      return {
        kind: "inProgress",
        label: "Updating",
        tone: "warn",
        detail: describeExecutionProgress(execution),
        needsAttention: false,
      };
    }
    if (execution.state === "succeeded") {
      return {
        kind: "succeeded",
        label: "Updated",
        tone: "ok",
        detail: "The container was recreated and verified.",
        needsAttention: false,
      };
    }
    if (execution.state === "failed") {
      return {
        kind: "failed",
        label: "Update failed",
        tone: "danger",
        detail: execution.message ?? "The container could not be recreated.",
        // No rollback record reached a conclusion, so somebody has to decide.
        needsAttention: true,
      };
    }
    return {
      kind: "cancelled",
      label: execution.state === "expired" ? "Expired" : "Cancelled",
      tone: "neutral",
      detail: "The update did not run to a conclusion.",
      needsAttention: false,
    };
  }

  if (acquisition) {
    if (isActive(acquisition.state)) {
      return {
        kind: "inProgress",
        label: "Downloading",
        tone: "warn",
        detail: "The image is being downloaded and verified.",
        needsAttention: false,
      };
    }
    if (acquisition.state === "succeeded") {
      return {
        kind: "waiting",
        label: "Ready to apply",
        tone: "warn",
        detail: "The image is downloaded and verified. The container has not been recreated.",
        needsAttention: false,
      };
    }
    if (acquisition.state === "failed") {
      return {
        kind: "failed",
        label: "Download failed",
        tone: "danger",
        detail: acquisition.message ?? "The image could not be downloaded.",
        needsAttention: false,
      };
    }
    return {
      kind: "cancelled",
      label: acquisition.state === "expired" ? "Expired" : "Cancelled",
      tone: "neutral",
      detail: "The download did not run to a conclusion.",
      needsAttention: false,
    };
  }

  return {
    kind: "inProgress",
    label: "In progress",
    tone: "neutral",
    detail: "",
    needsAttention: false,
  };
}

/**
 * The stage a running recreation reached.
 *
 * Named stages the server reported, never a percentage: the backend exposes no
 * progress figure, and inventing one would be the page claiming to know
 * something it does not.
 */
function describeExecutionProgress(execution: Execution): string {
  switch (execution.state) {
    case "capturing":
      return "Recording the container's configuration before changing it.";
    case "creating":
      return "Creating the replacement container.";
    case "starting":
      return "Starting the replacement container.";
    case "verifying":
      return "Checking that the replacement is healthy.";
    default:
      return "Preparing to recreate the container.";
  }
}

/** The images an attempt moved between, when the records say. */
export function describeChange(entry: ActivityEntry): string | undefined {
  const { execution, acquisition } = entry;
  if (execution?.oldImage && execution.target?.reference) {
    return `${execution.oldImage} → ${execution.target.reference}`;
  }
  if (acquisition?.target?.reference) return acquisition.target.reference;
  return undefined;
}

/** The moment an entry is filed under. */
function momentOf(
  acquisition?: Acquisition,
  execution?: Execution,
  rollback?: Rollback,
): string {
  return (
    rollback?.completedAt ??
    rollback?.requestedAt ??
    execution?.completedAt ??
    execution?.requestedAt ??
    acquisition?.completedAt ??
    acquisition?.requestedAt ??
    ""
  );
}

/**
 * Picks the rollback attempt that decided the outcome.
 *
 * A succeeded attempt is the answer whenever one exists -- the container came
 * back, and an earlier failed attempt does not change that. Otherwise the most
 * recent attempt is what an operator is looking at.
 */
function decidingRollback(attempts: Rollback[]): Rollback | undefined {
  if (attempts.length === 0) return undefined;
  return attempts.find((attempt) => attempt.state === "succeeded") ?? attempts[0];
}

/**
 * Joins three independently-paginated lists into one chronological feed.
 *
 * An execution whose acquisition is outside the loaded window still appears --
 * it carries its own container, images and outcome, and dropping it would make
 * the history quietly wrong. The same is true of a rollback whose execution is
 * out of window.
 */
export function buildActivityFeed(
  acquisitions: readonly Acquisition[],
  executions: readonly Execution[],
  rollbacks: readonly Rollback[],
): ActivityEntry[] {
  const rollbacksByExecution = new Map<string, Rollback[]>();
  for (const rollback of rollbacks) {
    const list = rollbacksByExecution.get(rollback.executionId) ?? [];
    list.push(rollback);
    rollbacksByExecution.set(rollback.executionId, list);
  }
  for (const list of rollbacksByExecution.values()) {
    // Newest first, so "most recent attempt" is list[0].
    list.sort((a, b) => (a.requestedAt < b.requestedAt ? 1 : -1));
  }

  const entries: ActivityEntry[] = [];
  const usedAcquisitions = new Set<string>();
  const acquisitionById = new Map(
    acquisitions.map((acquisition) => [acquisition.acquisitionId, acquisition]),
  );
  const seenExecutions = new Set<string>();

  for (const execution of executions) {
    seenExecutions.add(execution.executionId);
    const acquisition = acquisitionById.get(execution.acquisitionId);
    if (acquisition) usedAcquisitions.add(acquisition.acquisitionId);

    const attempts = rollbacksByExecution.get(execution.executionId) ?? [];
    const rollback = decidingRollback(attempts);

    entries.push({
      key: execution.executionId,
      containerName: execution.containerName,
      containerId: execution.containerId,
      acquisition,
      execution,
      rollback,
      rollbackAttempts: attempts,
      at: momentOf(acquisition, execution, rollback),
      status: statusOf(acquisition, execution, rollback),
    });
  }

  // Downloads that have not become a recreation: the "ready to apply" case, and
  // failed or in-flight downloads.
  for (const acquisition of acquisitions) {
    if (usedAcquisitions.has(acquisition.acquisitionId)) continue;
    entries.push({
      key: acquisition.acquisitionId,
      containerName: acquisition.containerName,
      containerId: acquisition.containerId,
      acquisition,
      rollbackAttempts: [],
      at: momentOf(acquisition, undefined, undefined),
      status: statusOf(acquisition, undefined, undefined),
    });
  }

  // A rollback whose execution fell outside the loaded page still happened.
  for (const [executionId, attempts] of rollbacksByExecution) {
    if (seenExecutions.has(executionId)) continue;
    const rollback = decidingRollback(attempts);
    if (!rollback) continue;
    entries.push({
      key: `rollback:${executionId}`,
      containerName: rollback.containerName,
      rollback,
      rollbackAttempts: attempts,
      at: momentOf(undefined, undefined, rollback),
      status: statusOf(undefined, undefined, rollback),
    });
  }

  return entries.sort((a, b) => (a.at < b.at ? 1 : a.at > b.at ? -1 : 0));
}

/** The four numbers the header reports. */
export interface ActivitySummary {
  total: number;
  needsAttention: number;
  recovered: number;
  inProgress: number;
}

export function summariseActivity(
  entries: readonly ActivityEntry[],
): ActivitySummary {
  let needsAttention = 0;
  let recovered = 0;
  let inProgress = 0;

  for (const entry of entries) {
    if (entry.status.needsAttention) needsAttention += 1;
    if (entry.status.kind === "recovered") recovered += 1;
    if (entry.status.kind === "inProgress") inProgress += 1;
  }

  return { total: entries.length, needsAttention, recovered, inProgress };
}

/** The filters the page offers, all applied to data already loaded. */
export type ActivityKind = "all" | "updates" | "downloads" | "failures" | "rollbacks";

export function filterActivity(
  entries: readonly ActivityEntry[],
  kind: ActivityKind,
  search: string,
): ActivityEntry[] {
  const needle = search.trim().toLowerCase();

  return entries.filter((entry) => {
    if (needle) {
      const haystack = [
        entry.containerName,
        entry.execution?.oldImage,
        entry.execution?.target?.reference,
        entry.acquisition?.target?.reference,
      ]
        .filter(Boolean)
        .join(" ")
        .toLowerCase();
      if (!haystack.includes(needle)) return false;
    }

    switch (kind) {
      case "all":
        return true;
      case "updates":
        return Boolean(entry.execution);
      case "downloads":
        return Boolean(entry.acquisition) && !entry.execution;
      case "failures":
        return (
          entry.status.kind === "failed" || entry.status.kind === "rollbackFailed"
        );
      case "rollbacks":
        return entry.rollbackAttempts.length > 0;
    }
  });
}

/**
 * How far back the loaded window reaches, so the page can say so.
 *
 * The three lists paginate independently, so a merged feed is complete only
 * back to the NEWEST of the three oldest loaded records: past that point one
 * list has run out and the timeline would be missing its entries without
 * saying so. Reporting the horizon is the honest alternative to pretending the
 * history ends where the first page does.
 */
export function feedHorizon(
  acquisitions: readonly Acquisition[],
  executions: readonly Execution[],
  rollbacks: readonly Rollback[],
): string | undefined {
  const oldest = [
    acquisitions.at(-1)?.requestedAt,
    executions.at(-1)?.requestedAt,
    rollbacks.at(-1)?.requestedAt,
  ].filter((value): value is string => Boolean(value));

  if (oldest.length < 2) return undefined;
  return oldest.reduce((newest, value) => (value > newest ? value : newest));
}
