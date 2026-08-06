import type { RecoveryPlan, VerificationResult } from "./executionTypes";
import type { Pagination } from "./inventoryTypes";

/**
 * Manual rollback types.
 *
 * # What a rollback is
 *
 * A recreation stopped a container, parked it under a derived name, and started
 * a replacement in its place. A rollback undoes exactly that: it stops the
 * replacement, parks it, renames the preserved original back, starts it, and
 * proves it.
 *
 * **This is the second of two things HarborMaster does that change something
 * RUNNING**, and it is the one an operator reaches for when the first went
 * wrong. The UI's job is to make three things unmissable.
 *
 * # The three facts the UI must never get wrong
 *
 *  1. **A rollback causes downtime.** The replacement is stopped before the
 *     original is started. There is a gap, and the page says so before the
 *     control is offered.
 *  2. `checkpoint` says what is TRUE OF THE HOST. `state` says what
 *     HarborMaster was doing. On a failed record only the first matters.
 *  3. **Nothing is removed.** The replacement is stopped and parked and stays
 *     on the host as the evidence of why the recreation was backed out. There
 *     is no delete control and no server capability behind one.
 *
 * # There is no target in the request
 *
 * `RollbackRequest` carries an EXECUTION ID and an optional idempotency key.
 * There is no container, name, image, or Docker option a caller can supply — a
 * type with nowhere to put one is a stronger guarantee than a check that
 * rejects one.
 */

/**
 * Where one rollback has got to.
 *
 * The first two change nothing on the host and are freely cancellable.
 * `stoppingReplacement` is the MUTATION POINT: from there the replacement has
 * been stopped and the rollback must reach a recorded conclusion.
 */
export type RollbackState =
  | "queued"
  | "validating"
  | "stoppingReplacement"
  | "restoringName"
  | "startingOriginal"
  | "verifyingOriginal"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "expired";

/**
 * What is TRUE OF THE HOST.
 *
 * The field to read first on any failed or interrupted record. An empty value
 * with `mutatedAt` unset means nothing was changed; an empty value with
 * `mutatedAt` set means a stop was issued and never confirmed.
 */
export type RollbackCheckpoint =
  | ""
  | "replacementStopped"
  | "replacementParked"
  | "originalRestored"
  | "originalStarted"
  | "originalVerified";

/**
 * Why a rollback did not succeed.
 *
 * `preflight` happens before the mutation point and leaves the host untouched.
 * Everything else may have left containers where an operator has to settle
 * them — which the CHECKPOINT, not this field, is what actually says.
 */
export type RollbackFailure =
  | "preflight"
  | "stop"
  | "rename"
  | "start"
  | "healthTimeout"
  | "unhealthy"
  | "notStable"
  | "imageMismatch"
  | "preservation"
  | "network"
  | "dockerUnavailable"
  | "timeout"
  | "interrupted"
  | "persistence"
  | "internal";

/** Which preflight check refused. A refusal means the safety model worked. */
export type RollbackRefusal =
  | "disabled"
  | "executionMissing"
  | "executionActive"
  | "nothingToRollBack"
  | "originalRemoved"
  | "checkpointUncertain"
  | "alreadyRolledBack"
  | "conflict"
  | "limit"
  | "originalMissing"
  | "originalIdentity"
  | "replacementMissing"
  | "replacementIdentity"
  | "nameUnavailable"
  | "inventoryStale"
  | "dockerUnavailable"
  | "unverifiable";

/** What each proof concluded. An `unknown` is NEVER a pass. */
export interface RollbackVerification {
  health: VerificationResult;
  healthState?: "" | "none" | "starting" | "healthy" | "unhealthy";
  /** False means health reflects the STABILITY window, which is weaker. */
  healthChecked: boolean;
  stabilitySeconds?: number;
  image: VerificationResult;
  preservation: VerificationResult;
  preservationReport?: import("./executionTypes").PreservationReport;
  network: VerificationResult;
}

/** One immutable record of one manual rollback. */
export interface Rollback {
  rollbackId: string;
  /** The recreation being undone. The only thing a caller supplied. */
  executionId: string;

  /** The production name both containers contend for. */
  containerName: string;

  /** The preserved original, and the name the recreation parked it under. */
  originalId: string;
  parkedName: string;
  /** The container the recreation created. */
  replacementId: string;
  /** Where the rollback moved the replacement to. Empty until it happens. */
  replacementParkedName?: string;

  originalImage?: string;
  originalImageId?: string;
  replacementImage?: string;

  state: RollbackState;
  checkpoint?: RollbackCheckpoint;

  failure?: RollbackFailure;
  refusal?: RollbackRefusal;
  /** HarborMaster's own sentence. Never a daemon string. */
  message?: string;

  verification: RollbackVerification;
  recovery?: RecoveryPlan;

  requestedAt: string;
  startedAt?: string;
  /** When the host was FIRST changed. Absent means it never was. */
  mutatedAt?: string;
  completedAt?: string;
  expiresAt: string;

  requestKey?: string;
  requestedBy?: { userId?: string; username?: string };
}

/** One bounded entry in a rollback's audit trail. */
export interface RollbackEvent {
  state: RollbackState;
  checkpoint?: RollbackCheckpoint;
  detail?: string;
  at: string;
}

/** The estate aggregate. */
export interface RollbackSummary {
  total: number;
  active: number;
  succeeded: number;
  failed: number;
  /** Failures that left containers on this host. The number that matters. */
  needsAttention: number;
  /**
   * Whether rollback is switched on at all, so an empty list is not read as
   * "nothing has ever been rolled back".
   */
  enabled: boolean;
}

/**
 * Whether one recreation LOOKS like one that can be undone, and if not, why.
 *
 * Advice for the UI, not permission — and computed from the server's own
 * records alone. It issues no Docker call, because the endpoint that carries it
 * is polled and a read that drove the Docker socket would be a denial-of-service
 * amplifier.
 *
 * So `eligible: true` means "nothing in the record rules this out". The server
 * asks the rest of the questions, against the live host, when the rollback is
 * requested and again immediately before it acts. **A confirmed request can
 * still be refused**, and the UI must present the refusal rather than treat it
 * as a surprise.
 */
export interface RollbackEligibility {
  eligible: boolean;
  refusal?: RollbackRefusal;
  /** The refusal in operator-facing words. */
  reason?: string;

  /** The identities a confirmation must show. All from the server's record. */
  containerName?: string;
  originalId?: string;
  parkedName?: string;
  replacementId?: string;
  replacementImage?: string;
  originalImage?: string;
}

/** GET /rollbacks */
export interface RollbackListResponse {
  items: Rollback[];
  pagination: Pagination;
  summary: RollbackSummary;
}

/** GET /rollbacks/{id} */
export interface RollbackDetailResponse {
  rollback: Rollback;
  events: RollbackEvent[];
}

/**
 * POST /rollbacks
 *
 * Two fields, and neither names a container.
 */
export interface RollbackRequest {
  executionId: string;
  requestKey?: string;
}

/** The listing filters. Every value is a closed vocabulary. */
export interface RollbackQuery {
  page?: number;
  pageSize?: number;
  executionId?: string;
  state?: RollbackState[];
  failure?: RollbackFailure[];
  activeOnly?: boolean;
  needsAttention?: boolean;
}

/** States in lifecycle order, which is the order controls offer them in. */
export const ROLLBACK_STATE_ORDER: readonly RollbackState[] = [
  "queued",
  "validating",
  "stoppingReplacement",
  "restoringName",
  "startingOriginal",
  "verifyingOriginal",
  "succeeded",
  "failed",
  "cancelled",
  "expired",
] as const;

/** Human labels. The wire values are identifiers, not prose. */
export const ROLLBACK_STATE_LABELS: Record<RollbackState, string> = {
  queued: "Queued",
  validating: "Checking",
  stoppingReplacement: "Stopping replacement",
  restoringName: "Restoring name",
  startingOriginal: "Starting original",
  verifyingOriginal: "Verifying",
  succeeded: "Rolled back",
  failed: "Failed",
  cancelled: "Cancelled",
  expired: "Expired",
};

/**
 * What each state means.
 *
 * The two pre-mutation states say plainly that nothing has changed, because
 * that is the fact an operator most needs while deciding whether to cancel.
 */
export const ROLLBACK_STATE_MEANING: Record<RollbackState, string> = {
  queued: "Accepted and waiting for a slot. Nothing on this host has changed",
  validating:
    "Rechecking both container identities against the live host. Nothing on this host has changed",
  stoppingReplacement:
    "The replacement is being stopped. From here the rollback cannot be cancelled",
  restoringName:
    "The replacement has been moved aside so the original can take its name back",
  startingOriginal: "The original carries its own name again and is being started",
  verifyingOriginal:
    "Proving the restored original: health, image, configuration, networks",
  succeeded:
    "The original is running under its own name and passed every check. The replacement is stopped and kept as evidence",
  failed: "The rollback stopped. Check what was left on this host",
  cancelled: "Stopped by an operator before anything on this host was changed",
  expired: "The request waited past its deadline and was abandoned without starting",
};

export const ROLLBACK_CHECKPOINT_LABELS: Record<RollbackCheckpoint, string> = {
  "": "Nothing changed",
  replacementStopped: "Replacement stopped",
  replacementParked: "Replacement parked",
  originalRestored: "Original renamed back",
  originalStarted: "Original started",
  originalVerified: "Original verified",
};

export const ROLLBACK_FAILURE_LABELS: Record<RollbackFailure, string> = {
  preflight: "Refused by safety checks",
  stop: "Could not stop the replacement",
  rename: "Could not rename",
  start: "Original would not start",
  healthTimeout: "Never became healthy",
  unhealthy: "Reported unhealthy",
  notStable: "Did not stay running",
  imageMismatch: "Wrong image",
  preservation: "Configuration not preserved",
  network: "Networks not preserved",
  dockerUnavailable: "Docker unavailable",
  timeout: "Timed out",
  interrupted: "Interrupted by a restart",
  persistence: "Could not record what was done",
  internal: "Internal failure",
};

/**
 * Why a rollback was refused, in operator-facing words.
 *
 * The server sends the same sentences; these exist so a UI can render a refusal
 * it received as a bare code, and so the vocabulary is exhaustive at compile
 * time.
 */
export const ROLLBACK_REFUSAL_LABELS: Record<RollbackRefusal, string> = {
  disabled: "Manual rollback is not enabled on this installation",
  executionMissing: "That recreation is not recorded here",
  executionActive: "That recreation is still running",
  nothingToRollBack:
    "That recreation did not get far enough to leave anything to undo",
  originalRemoved: "The original container has already been removed",
  checkpointUncertain:
    "That recreation issued a change that was never confirmed, so HarborMaster cannot say where the containers are",
  alreadyRolledBack: "That recreation has already been rolled back",
  conflict: "Another rollback or recreation is already running for this container",
  limit: "Too many rollbacks are already running",
  originalMissing: "The preserved original is no longer on this host",
  originalIdentity:
    "The preserved original is not the container the record describes",
  replacementMissing: "The replacement container is no longer on this host",
  replacementIdentity:
    "The replacement is not the container the record describes",
  nameUnavailable: "Another container holds the name the original needs back",
  inventoryStale: "HarborMaster's picture of this host is too old to act on",
  dockerUnavailable: "Docker is not reachable",
  unverifiable:
    "The configuration comparison cannot be made, so the rollback could not be proved",
};

/** Whether a rollback is still in progress. */
export function isRollbackActive(state: RollbackState): boolean {
  return (
    state === "queued" ||
    state === "validating" ||
    state === "stoppingReplacement" ||
    state === "restoringName" ||
    state === "startingOriginal" ||
    state === "verifyingOriginal"
  );
}

/**
 * Whether an operator may still stop this rollback.
 *
 * Only BEFORE the mutation point. A rollback that has stopped the replacement
 * must reach a recorded conclusion, so the UI must not offer a control that
 * would answer 409.
 */
export function isRollbackCancellable(state: RollbackState): boolean {
  return state === "queued" || state === "validating";
}

/** Whether this state implies the host has been changed. */
export function isRollbackMutating(state: RollbackState): boolean {
  return (
    state === "stoppingReplacement" ||
    state === "restoringName" ||
    state === "startingOriginal" ||
    state === "verifyingOriginal"
  );
}

/**
 * Whether the host was changed at all.
 *
 * `mutatedAt` is included deliberately: a record whose checkpoint is empty but
 * whose mutation timestamp is set is one where a stop was issued and never
 * confirmed, and treating that as "untouched" would be a confident and false
 * statement about a container that may be down.
 */
export function rollbackHostChanged(rollback: Rollback): boolean {
  const checkpoint = rollback.checkpoint ?? "";
  return checkpoint !== "" || Boolean(rollback.mutatedAt);
}

/**
 * Whether this record left containers on the host for an operator to settle.
 *
 * Driven by the CHECKPOINT rather than the failure, because the checkpoint is
 * what says whether anything was actually changed.
 */
export function rollbackNeedsAttention(rollback: Rollback): boolean {
  if (rollback.state !== "failed") return false;
  return rollbackHostChanged(rollback);
}

/** Whether every proof passed. An unknown is not a pass. */
export function rollbackVerificationPassed(
  verification: RollbackVerification,
): boolean {
  return (
    verification.health === "passed" &&
    verification.image === "passed" &&
    verification.preservation === "passed" &&
    verification.network === "passed"
  );
}
