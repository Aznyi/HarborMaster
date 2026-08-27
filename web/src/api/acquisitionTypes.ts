import type { Pagination } from "./inventoryTypes";
import type { Platform } from "./imageTypes";

/**
 * Image acquisition types.
 *
 * # What an acquisition is, and is not
 *
 * An operator looked at a change plan, decided the proposed image was worth
 * having on the host, and asked HarborMaster to download it. Layers arrive in
 * the daemon's local image store.
 *
 * **It is not an update. No container is touched.** A container keeps running
 * the image it was created from; an acquired image is another entry in the
 * store beside it. This is the single most important thing the UI has to
 * convey, and the reason there is no "applied" state anywhere in this file:
 * there is nothing to apply.
 *
 * # There is no target in the request
 *
 * `AcquisitionRequest` carries a PLAN ID and an optional idempotency key.
 * There is no registry, repository, digest, tag, or pull option a caller can
 * supply — a type with nowhere to put a target is a stronger guarantee than a
 * check that rejects one.
 */

/**
 * Where one acquisition has got to.
 *
 * The first four are active; the last four are terminal and nothing moves out
 * of them.
 */
export type AcquisitionState =
  | "queued"
  | "validating"
  | "pulling"
  | "verifying"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "expired";

/**
 * Why an acquisition did not succeed.
 *
 * `digestMismatch` is the serious one: the host now holds content that was
 * never approved. It is never presented as retryable.
 */
export type AcquisitionFailure =
  | "preflight"
  | "dockerUnavailable"
  | "registry"
  | "transfer"
  | "timeout"
  | "digestMismatch"
  | "platformMismatch"
  | "verification"
  | "internal";

/** Which preflight check refused. A refusal means the safety model worked. */
export type AcquisitionRefusal =
  | "planMissing"
  | "planSuperseded"
  | "planStale"
  | "recommendation"
  | "digestUnavailable"
  | "digestChanged"
  | "platformUnavailable"
  | "restoreReadiness"
  | "policyViolation"
  | "registryStale"
  | "duplicate"
  | "dockerUnavailable"
  | "limit"
  | "targetRefused"
  | "disabled"
  | "containerMissing"
  // In the Go vocabulary and the published schema before it was here: the
  // refusal that stops HarborMaster downloading an image for its own container.
  | "selfUpdate";

/** The immutable image an acquisition names. Derived entirely from a plan. */
export interface AcquisitionTarget {
  registry: string;
  repository: string;
  /** The manifest digest. Always present on a legal target. */
  digest: string;
  /** The familiar form, for display only. Never used to perform a pull. */
  reference: string;
  platform?: Platform;
}

/** One immutable record of an attempt to acquire an image. */
export interface Acquisition {
  acquisitionId: string;
  planId: string;
  containerId: string;
  containerName: string;

  target: AcquisitionTarget;
  state: AcquisitionState;

  failure?: AcquisitionFailure;
  /** HarborMaster's own sentence. Never a daemon or registry string. */
  message?: string;
  refusal?: AcquisitionRefusal;

  /** What verification found on the host, as opposed to what was expected. */
  acquiredImageId?: string;
  acquiredDigest?: string;
  acquiredPlatform?: Platform;
  sizeBytes?: number;

  layers?: number;
  bytesTransferred?: number;
  /** The most recent bounded status line, e.g. "Downloading". */
  progress?: string;

  requestedAt: string;
  startedAt?: string;
  completedAt?: string;
  expiresAt: string;

  requestKey?: string;
  planDigest: string;
}

/** One bounded entry in an acquisition's audit trail. */
export interface AcquisitionEvent {
  state: AcquisitionState;
  detail?: string;
  bytesTransferred?: number;
  layers?: number;
  at: string;
}

/** The dashboard aggregate. */
export interface AcquisitionSummary {
  total: number;
  active: number;
  succeeded: number;
  failed: number;
  byState: Partial<Record<AcquisitionState, number>>;
  byFailure: Partial<Record<AcquisitionFailure, number>>;
  lastCompletedAt?: string;
  /**
   * Whether acquisition is switched on at all, so an empty list is not read as
   * "nothing has ever been acquired".
   */
  enabled: boolean;
}

/** GET /acquisitions */
export interface AcquisitionListResponse {
  items: Acquisition[];
  pagination: Pagination;
  summary: AcquisitionSummary;
}

/** GET /acquisitions/{id} */
export interface AcquisitionDetailResponse {
  acquisition: Acquisition;
  events: AcquisitionEvent[];
}

/**
 * POST /acquisitions
 *
 * Two fields, and neither names an image.
 */
export interface AcquisitionRequest {
  planId: string;
  requestKey?: string;
}

/** The listing filters. Every value is a closed vocabulary. */
export interface AcquisitionQuery {
  page?: number;
  pageSize?: number;
  state?: AcquisitionState[];
  failure?: AcquisitionFailure[];
  activeOnly?: boolean;
  sort?: "requestedAt" | "completedAt" | "state" | "container" | "id";
  order?: "asc" | "desc";
}

/** States in lifecycle order, which is the order controls offer them in. */
export const ACQUISITION_STATE_ORDER: readonly AcquisitionState[] = [
  "queued",
  "validating",
  "pulling",
  "verifying",
  "succeeded",
  "failed",
  "cancelled",
  "expired",
] as const;

/** Human labels. The wire values are identifiers, not prose. */
export const ACQUISITION_STATE_LABELS: Record<AcquisitionState, string> = {
  queued: "Queued",
  validating: "Checking",
  pulling: "Downloading",
  verifying: "Verifying",
  succeeded: "Downloaded",
  failed: "Failed",
  cancelled: "Cancelled",
  expired: "Expired",
};

/**
 * What each state means.
 *
 * "Downloaded" says explicitly that no container changed, because that is the
 * assumption an operator is most likely to make on seeing a success.
 */
export const ACQUISITION_STATE_MEANING: Record<AcquisitionState, string> = {
  queued: "Accepted and waiting for a slot. Nothing has been checked yet",
  validating: "Rechecking the plan and its supporting evidence before downloading",
  pulling: "The daemon is transferring layers",
  verifying: "Confirming that the image which arrived is the one that was approved",
  succeeded:
    "The image is on this host and its digest was confirmed. NO CONTAINER HAS BEEN CHANGED",
  failed: "The acquisition stopped. Nothing was applied",
  cancelled: "Stopped by an operator. Nothing was applied",
  expired: "The request waited past its deadline and was abandoned without starting",
};

/** Whether an acquisition is still in progress. */
export function isActive(state: AcquisitionState): boolean {
  return (
    state === "queued" ||
    state === "validating" ||
    state === "pulling" ||
    state === "verifying"
  );
}

/**
 * Whether an operator may still stop this acquisition.
 *
 * Verifying is deliberately excluded: the bytes are already on the host, and
 * stopping the confirmation would leave an unverified image with no record
 * saying so.
 */
export function isCancellable(state: AcquisitionState): boolean {
  return state === "queued" || state === "validating" || state === "pulling";
}

export const ACQUISITION_FAILURE_LABELS: Record<AcquisitionFailure, string> = {
  preflight: "Refused by safety checks",
  dockerUnavailable: "Docker unavailable",
  registry: "Registry refused",
  transfer: "Transfer failed",
  timeout: "Timed out",
  digestMismatch: "Wrong image",
  platformMismatch: "Wrong platform",
  verification: "Could not verify",
  internal: "Internal failure",
};

/**
 * Whether another attempt could plausibly succeed.
 *
 * Advisory only — HarborMaster never retries by itself, because an automatic
 * retry would be an automatic pull. A digest or platform mismatch is
 * deliberately not retryable: repeating a pull that produced the wrong content
 * is how a transient substitution becomes a persistent one.
 */
export function isRetryable(failure: AcquisitionFailure | undefined): boolean {
  return (
    failure === "dockerUnavailable" ||
    failure === "transfer" ||
    failure === "timeout"
  );
}

/** Renders the digest-pinned reference an operator should see in a confirmation. */
export function pinnedReference(target: AcquisitionTarget): string {
  if (!target.registry || !target.repository || !target.digest) return "";
  return `${target.registry}/${target.repository}@${target.digest}`;
}

/**
 * Renders a byte count for display.
 *
 * One decimal place below 10 units, where it carries information ("1.5 MiB"),
 * and none where it does not: "1.0 MiB" is noise, and "1 MiB" reads as the size
 * it is.
 */
export function formatBytes(bytes: number | undefined): string {
  if (!bytes || bytes <= 0) return "—";

  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }

  const rendered =
    unit === 0 || value >= 10 || Number.isInteger(value)
      ? String(Math.round(value))
      : value.toFixed(1);

  return `${rendered} ${units[unit]}`;
}
