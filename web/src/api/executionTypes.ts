import type { Platform } from "./imageTypes";
import type { Pagination } from "./inventoryTypes";

/**
 * Container recreation types.
 *
 * # What a recreation is
 *
 * An operator looked at a change plan, downloaded the image it proposed, saw
 * that the download was verified, and asked HarborMaster to move the container
 * onto it. HarborMaster stops the original, parks it under a derived name,
 * creates a replacement from the original's own configuration, starts it,
 * proves it, and only then removes what it replaced.
 *
 * **This is the only thing HarborMaster does that changes something RUNNING.**
 * Every other feature reads, and image acquisition writes only to the image
 * store. The UI's job here is to make the difference unmissable.
 *
 * # The three facts the UI must never get wrong
 *
 *  1. `checkpoint` says what is TRUE OF THE HOST. `state` says what
 *     HarborMaster was doing. On a failed record only the first matters.
 *  2. A recreation an OPERATOR requested is never rolled back for them. A
 *     failure leaves both containers in place and records manual steps. Only
 *     an unattended update whose policy enables rollback undoes itself, and
 *     that path pauses the container afterwards — see the automation types.
 *  3. An acquisition is SINGLE USE. There is no retry control, because a
 *     second recreation needs a fresh plan and a fresh acquisition.
 *
 * # There is no target in the request
 *
 * `ExecutionRequest` carries an ACQUISITION ID and an optional idempotency
 * key. There is no container, image, digest, command, mount, capability, or
 * timeout a caller can supply — a type with nowhere to put one is a stronger
 * guarantee than a check that rejects one.
 */

/**
 * Where one recreation has got to.
 *
 * The first three change nothing on the host and are freely cancellable.
 * `creating` is the MUTATION POINT: from there the original has been stopped
 * and the recreation must reach a recorded conclusion.
 */
export type ExecutionState =
  | "queued"
  | "validating"
  | "capturing"
  | "creating"
  | "starting"
  | "verifying"
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
export type ExecutionCheckpoint =
  | ""
  | "originalStopped"
  | "originalParked"
  | "replacementCreated"
  | "replacementStarted"
  | "replacementVerified"
  | "replacementQuarantined"
  | "originalRemoved";

/**
 * Why a recreation did not succeed.
 *
 * `preflight`, `capture`, and `secretUnavailable` happen before the mutation
 * point and leave the host untouched. Everything else means containers were
 * left behind.
 */
export type ExecutionFailure =
  | "preflight"
  | "capture"
  | "stop"
  | "rename"
  | "create"
  | "start"
  | "healthTimeout"
  | "unhealthy"
  | "notStable"
  | "imageMismatch"
  | "preservation"
  | "network"
  | "secretUnavailable"
  | "dockerUnavailable"
  | "timeout"
  | "interrupted"
  | "persistence"
  | "internal";

/** Which preflight check refused. A refusal means the safety model worked. */
export type ExecutionRefusal =
  | "disabled"
  | "acquisitionMissing"
  | "acquisitionNotSucceeded"
  | "acquisitionStale"
  | "acquisitionConsumed"
  | "planMissing"
  | "planSuperseded"
  | "planChanged"
  | "recommendation"
  | "containerMissing"
  | "containerChanged"
  | "containerState"
  | "inventoryStale"
  | "snapshotMissing"
  | "restoreReadiness"
  | "snapshotChanged"
  | "policyViolation"
  | "policyStale"
  | "registryStale"
  | "imageMissing"
  | "digestMismatch"
  | "platformMismatch"
  | "conflict"
  | "limit"
  | "dockerUnavailable"
  | "secretUnavailable"
  | "nameUnavailable"
  // The three below were in the Go vocabulary and in the published schema long
  // before they were in this union, so a refused recreation naming one of them
  // was typed as an impossible value here. They are the self-update refusal and
  // the two namespace refusals -- the most safety-critical in the set.
  | "selfUpdate"
  | "namespaceProviderMissing"
  | "dependentsNotRebindable";

/**
 * The outcome of one proof.
 *
 * `unknown` means the proof was never reached. It is NEVER a pass, and the UI
 * must not render it as one.
 */
export type VerificationResult = "unknown" | "passed" | "failed";

/** The immutable image a recreation moves a container onto. */
export interface ExecutionTarget {
  registry: string;
  repository: string;
  /** The manifest digest. The container is created from this, never a tag. */
  digest: string;
  reference?: string;
  imageId?: string;
  platform?: Platform;
}

/** One configuration field that did not survive the recreation. */
export interface PreservationDifference {
  field: string;
  kind: "changed" | "missing" | "added";
  /** Bounded and sanitised. A sensitive value renders as a keyed digest. */
  expected?: string;
  actual?: string;
}

/** The field-by-field comparison. No value here is a secret. */
export interface PreservationReport {
  status: VerificationResult;
  checked: number;
  matched: number;
  differences?: PreservationDifference[];
  truncated?: boolean;
  expectedFingerprint?: string;
  actualFingerprint?: string;
  /** The comparison could not be performed. Never a pass. */
  unverifiable?: boolean;
  reason?: string;
}

/** What each proof concluded. */
export interface ExecutionVerification {
  health: VerificationResult;
  healthState?: "" | "none" | "starting" | "healthy" | "unhealthy";
  /** False means health reflects the STABILITY window, which is weaker. */
  healthChecked: boolean;
  stabilitySeconds?: number;
  image: VerificationResult;
  preservation: VerificationResult;
  preservationReport?: PreservationReport;
  network: VerificationResult;
}

/**
 * One instruction in a manual recovery plan.
 *
 * `command` is for an operator to READ. HarborMaster never executes it.
 */
export interface RecoveryStep {
  order: number;
  description: string;
  command?: string;
  destructive: boolean;
}

/** What was left on the host, and what to do about it. */
export interface RecoveryPlan {
  urgency: "informational" | "attention" | "urgent";
  situation: string;
  serviceInterrupted: boolean;
  steps?: RecoveryStep[];
}

/** One immutable record of one container recreation. */
export interface Execution {
  executionId: string;
  acquisitionId: string;
  planId: string;
  snapshotId?: number;

  /** The ORIGINAL container, and the name the replacement takes over. */
  containerId: string;
  containerName: string;

  oldImage: string;
  oldImageId?: string;
  oldImageDigest?: string;
  target: ExecutionTarget;

  state: ExecutionState;
  checkpoint?: ExecutionCheckpoint;

  failure?: ExecutionFailure;
  refusal?: ExecutionRefusal;
  /** HarborMaster's own sentence. Never a daemon string. */
  message?: string;

  replacementId?: string;
  /** `<name>.hm-old-<executionId>`. Where to find the original. */
  parkedName?: string;
  /** `<name>.hm-failed-<executionId>`. Where to find a failed replacement. */
  quarantineName?: string;
  originalRemoved: boolean;

  verification: ExecutionVerification;
  recovery?: RecoveryPlan;

  requestedAt: string;
  startedAt?: string;
  /** When the host was FIRST changed. Absent means it never was. */
  mutatedAt?: string;
  completedAt?: string;
  expiresAt: string;

  requestKey?: string;
  planDigest: string;
}

/** One bounded entry in a recreation's audit trail. */
export interface ExecutionEvent {
  state: ExecutionState;
  checkpoint?: ExecutionCheckpoint;
  detail?: string;
  at: string;
}

/** The dashboard aggregate. */
export interface ExecutionSummary {
  total: number;
  active: number;
  succeeded: number;
  failed: number;
  /** Failures that left containers on this host. The number that matters. */
  needsAttention: number;
  byState: Partial<Record<ExecutionState, number>>;
  byFailure: Partial<Record<ExecutionFailure, number>>;
  lastCompletedAt?: string;
  enabled: boolean;
}

/** GET /executions */
export interface ExecutionListResponse {
  items: Execution[];
  pagination: Pagination;
  summary: ExecutionSummary;
}

/** GET /executions/{id} */
export interface ExecutionDetailResponse {
  execution: Execution;
  events: ExecutionEvent[];
  /**
   * Whether this recreation can be undone, and if not, why.
   *
   * Answered here rather than on a separate endpoint so the page that offers
   * the control and the check that governs it cannot disagree. Advice only:
   * the server asks the same questions again, against the live host,
   * immediately before it acts.
   *
   * ABSENT means manual rollback is not configured, which the UI renders as
   * "not available" rather than as "refused" — an operator must not go looking
   * for a check that never ran.
   */
  rollback?: import("./rollbackTypes").RollbackEligibility;
}

/**
 * POST /executions
 *
 * Two fields, and neither names a container or an image.
 */
export interface ExecutionRequest {
  acquisitionId: string;
  requestKey?: string;
}

/** The listing filters. Every value is a closed vocabulary. */
export interface ExecutionQuery {
  page?: number;
  pageSize?: number;
  state?: ExecutionState[];
  failure?: ExecutionFailure[];
  activeOnly?: boolean;
  needsAttention?: boolean;
  sort?: "requestedAt" | "completedAt" | "state" | "container" | "id";
  order?: "asc" | "desc";
}

/** States in lifecycle order, which is the order controls offer them in. */
export const EXECUTION_STATE_ORDER: readonly ExecutionState[] = [
  "queued",
  "validating",
  "capturing",
  "creating",
  "starting",
  "verifying",
  "succeeded",
  "failed",
  "cancelled",
  "expired",
] as const;

/** Human labels. The wire values are identifiers, not prose. */
export const EXECUTION_STATE_LABELS: Record<ExecutionState, string> = {
  queued: "Queued",
  validating: "Checking",
  capturing: "Reading configuration",
  creating: "Replacing",
  starting: "Starting",
  verifying: "Verifying",
  succeeded: "Recreated",
  failed: "Failed",
  cancelled: "Cancelled",
  expired: "Expired",
};

/**
 * What each state means.
 *
 * The three pre-mutation states say plainly that nothing has changed, because
 * that is the fact an operator most needs while deciding whether to cancel.
 */
export const EXECUTION_STATE_MEANING: Record<ExecutionState, string> = {
  queued: "Accepted and waiting for a slot. Nothing on this host has changed",
  validating: "Rechecking every prerequisite. Nothing on this host has changed",
  capturing:
    "Reading the container's current configuration. Nothing on this host has changed",
  creating:
    "The original has been stopped and a replacement is being created. This cannot be cancelled",
  starting: "The replacement has been created and is starting",
  verifying:
    "Proving the replacement before removing the original: health, image, configuration, networks",
  succeeded:
    "The replacement is running the approved image, passed every check, and the original was removed",
  failed: "The recreation stopped. Check what was left on this host",
  cancelled: "Stopped by an operator before anything on this host was changed",
  expired: "The request waited past its deadline and was abandoned without starting",
};

export const EXECUTION_CHECKPOINT_LABELS: Record<ExecutionCheckpoint, string> = {
  "": "Nothing changed",
  originalStopped: "Original stopped",
  originalParked: "Original parked",
  replacementCreated: "Replacement created",
  replacementStarted: "Replacement started",
  replacementVerified: "Replacement verified",
  replacementQuarantined: "Replacement quarantined",
  originalRemoved: "Original removed",
};

export const EXECUTION_FAILURE_LABELS: Record<ExecutionFailure, string> = {
  preflight: "Refused by safety checks",
  capture: "Could not read configuration",
  stop: "Could not stop the original",
  rename: "Could not rename",
  create: "Could not create the replacement",
  start: "Replacement would not start",
  healthTimeout: "Never became healthy",
  unhealthy: "Reported unhealthy",
  notStable: "Did not stay running",
  imageMismatch: "Wrong image",
  preservation: "Configuration not preserved",
  network: "Networks not preserved",
  secretUnavailable: "A value could not be reproduced",
  dockerUnavailable: "Docker unavailable",
  timeout: "Timed out",
  interrupted: "Interrupted by a restart",
  persistence: "Could not record what was done",
  internal: "Internal failure",
};

/** Whether a recreation is still in progress. */
export function isActive(state: ExecutionState): boolean {
  return (
    state === "queued" ||
    state === "validating" ||
    state === "capturing" ||
    state === "creating" ||
    state === "starting" ||
    state === "verifying"
  );
}

/**
 * Whether an operator may still stop this recreation.
 *
 * Only BEFORE the mutation point. A recreation that has stopped the original
 * must reach a recorded conclusion, so the UI must not offer a control that
 * would answer 409.
 */
export function isCancellable(state: ExecutionState): boolean {
  return state === "queued" || state === "validating" || state === "capturing";
}

/** Whether this state implies the host has been changed. */
export function isMutating(state: ExecutionState): boolean {
  return state === "creating" || state === "starting" || state === "verifying";
}

/**
 * Whether this record left containers on the host for an operator to settle.
 *
 * Driven by the CHECKPOINT rather than the failure, because the checkpoint is
 * what says whether anything was actually changed.
 */
export function needsAttention(execution: Execution): boolean {
  if (execution.state !== "failed") return false;
  const checkpoint = execution.checkpoint ?? "";
  if (checkpoint !== "" && checkpoint !== "originalRemoved") return true;
  // The uncertain case: a stop was issued and never confirmed.
  return checkpoint === "" && Boolean(execution.mutatedAt);
}

/**
 * Whether the host was changed at all.
 *
 * `mutatedAt` is included deliberately: a record whose checkpoint is empty but
 * whose mutation timestamp is set is one where a stop was issued and never
 * confirmed, and treating that as "untouched" would be a confident and false
 * statement about a container that may be down.
 */
export function hostChanged(execution: Execution): boolean {
  const checkpoint = execution.checkpoint ?? "";
  return checkpoint !== "" || Boolean(execution.mutatedAt);
}

/** Whether every proof passed. An unknown is not a pass. */
export function verificationPassed(verification: ExecutionVerification): boolean {
  return (
    verification.health === "passed" &&
    verification.image === "passed" &&
    verification.preservation === "passed" &&
    verification.network === "passed"
  );
}

/** Renders the digest-pinned reference an operator should see before acting. */
export function pinnedReference(target: ExecutionTarget): string {
  if (!target.registry || !target.repository || !target.digest) return "";
  return `${target.registry}/${target.repository}@${target.digest}`;
}
