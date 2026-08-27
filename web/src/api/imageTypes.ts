import type { ImageUsage, Pagination } from "./inventoryTypes";

/**
 * Image intelligence types.
 *
 * HarborMaster reads registries to discover whether a newer image exists. It
 * never pulls, pushes, deletes, or prunes anything, so there is no mutation
 * type here and no client function that could invoke one — an update is
 * reported, and applying it is an operator's job with their own tooling.
 *
 * # There is no destination in this API
 *
 * Nothing here carries a registry host, a URL, or a scheme. The refresh call
 * takes no argument at all. Registry destinations come only from image
 * references the inventory already holds, which is what keeps the SSRF surface
 * closed — and a type with nowhere to put a host is a stronger guarantee than a
 * check that rejects one.
 */

/** Which provider serves a registry. */
export type RegistryKind = "dockerhub" | "ghcr" | "oci" | "unknown";

/**
 * What kind of update is available.
 *
 * `unknown` is not a small update — it means HarborMaster could NOT determine
 * one, which is a different statement from `none` and must read differently.
 */
export type UpdateType =
  | "none"
  | "digest"
  | "patch"
  | "minor"
  | "major"
  | "prerelease"
  // Not a registry finding: a recreation onto the digest the container is
  // already running, so it can be reattached to a replaced network namespace.
  | "rebind"
  | "unknown";

/**
 * The outcome of the most recent registry lookup.
 *
 * `unsupported` means the reference can never be looked up — a local registry
 * or an address literal. It is deliberately distinct from `failed` so a
 * dashboard does not show a healthy estate as broken.
 */
export type CheckStatus =
  | "pending"
  | "ok"
  | "failed"
  | "rateLimited"
  | "unauthorized"
  | "notFound"
  | "unsupported";

/** The OS and architecture a manifest targets. */
export interface Platform {
  os?: string;
  architecture?: string;
  variant?: string;
}

/** Everything known about one image reference. */
export interface ImageIntel {
  id: number;
  /** The canonical form, and the identity of the record. */
  reference: string;
  /** The short form an operator recognises, e.g. "nginx:1.25". */
  familiar: string;

  registryKind: RegistryKind;
  registry: string;
  namespace?: string;
  repository: string;
  tag?: string;

  /** What the local daemon reports, and what the registry currently serves. */
  localDigest?: string;
  remoteDigest?: string;
  /** The reference names a digest, so its tag cannot move. */
  pinned: boolean;

  platform?: Platform;

  imageId?: string;
  containerCount: number;

  updateType: UpdateType;
  /** The newer tag, when one was found by version comparison. */
  latestTag?: string;
  updateReason?: string;

  checkStatus: CheckStatus;
  /** HarborMaster's own words. Never a registry-supplied string. */
  statusDetail?: string;

  firstSeenAt: string;
  lastCheckedAt?: string;
  lastSuccessAt?: string;
  nextCheckAt?: string;
  failureCount: number;

  publishedAt?: string;
  vendor?: string;
  source?: string;
  labels?: Record<string, string>;
}

/** What changed, and when. */
export type ImageEventKind =
  | "discovered"
  | "digestChanged"
  | "updateFound"
  | "updateCleared"
  | "checkFailed"
  | "checkRecovered";

/** One observed change for a reference. */
export interface ImageUpdateEvent {
  id: number;
  reference: string;
  observedAt: string;
  kind: ImageEventKind;

  previousDigest?: string;
  currentDigest?: string;
  previousUpdateType?: UpdateType;
  currentUpdateType?: UpdateType;
  latestTag?: string;

  checkStatus: CheckStatus;
  detail?: string;
}

/** One registry host's recent behaviour. */
export interface RegistryHealth {
  host: string;
  registryKind: RegistryKind;
  /** How many references this host serves. */
  images: number;

  lastSuccessAt?: string;
  lastFailureAt?: string;
  consecutiveFailures: number;
  /** When the host may be contacted again. */
  availableAt?: string;

  lastDetail?: string;
  /**
   * The most recent failure was a rate limit, which an operator should read
   * very differently from an outage.
   */
  rateLimited: boolean;
}

/** The dashboard aggregate. */
export interface ImageIntelSummary {
  images: number;
  containers: number;

  updatesAvailable: number;
  /** The number an operator actually plans around. */
  containersAffected: number;

  byUpdateType: Partial<Record<UpdateType, number>>;
  byCheckStatus: Partial<Record<CheckStatus, number>>;
  byRegistry: Record<string, number>;

  /**
   * References looked up at least once. The gap between this and `images` is
   * the coverage a dashboard must not hide: an estate where nothing has been
   * checked is not an estate with no updates.
   */
  checked: number;
  pending: number;
  /** References that will never be looked up, so a gap can be explained. */
  unsupported: number;

  lastCheckedAt?: string;
  registries?: RegistryHealth[];
}

/** The collection engine's state. */
export interface ImageIntelEngineStatus {
  enabled: boolean;
  dueNow: number;
  running: boolean;
  sweepPending: boolean;
  lastSweepAt?: string;
  lastChecked: number;
  lastSkipped: number;
  lastFailed: number;
}

/** The updates listing, with the estate aggregate. */
export interface ImageUpdatesResponse {
  items: ImageIntel[];
  pagination: Pagination;
  summary: ImageIntelSummary;
}

/**
 * One local image with its registry intelligence.
 *
 * Extends ImageUsage rather than replacing it: the pre-existing `image` and
 * `containerCount` fields are unchanged and `intel` is additive.
 */
export interface ImageDetail extends ImageUsage {
  intel: ImageIntel[];
}

/** One image's observed changes. */
export interface ImageHistoryResponse {
  imageId: string;
  references: string[];
  items: ImageUpdateEvent[];
  pagination: Pagination;
}

/** The 202 body from POST /images/refresh. */
export interface ImageRefreshAccepted {
  requested: boolean;
  engine: ImageIntelEngineStatus;
}

/**
 * The updates list query.
 *
 * Note what is absent: no host, no URL, no scheme. `registry` filters the
 * stored column and cannot introduce a destination.
 */
export interface ImageUpdateQuery {
  page?: number;
  pageSize?: number;
  update?: UpdateType[];
  status?: CheckStatus[];
  registry?: string[];
  updatesOnly?: boolean;
  inUseOnly?: boolean;
  search?: string;
  sort?: string;
  order?: "asc" | "desc";
}

/** Update types in report order: most to least urgent, non-answers last. */
export const UPDATE_TYPE_ORDER = [
  "major",
  "minor",
  "patch",
  "prerelease",
  "digest",
  "rebind",
  "unknown",
  "none",
] as const satisfies readonly UpdateType[];

/** Human labels, so a type never renders as a bare identifier. */
export const UPDATE_TYPE_LABELS: Record<UpdateType, string> = {
  major: "Major",
  minor: "Minor",
  patch: "Patch",
  prerelease: "Pre-release",
  digest: "Digest moved",
  rebind: "Reattachment",
  unknown: "Undetermined",
  none: "Up to date",
};

/** What each type means, for the tooltip that keeps colour from being the only signal. */
export const UPDATE_TYPE_MEANINGS: Record<UpdateType, string> = {
  major: "A new major version is published. Expect breaking changes.",
  minor: "A new minor version is published.",
  patch: "A new patch version is published.",
  prerelease:
    "The only newer tag in this series is a pre-release, not a stable release.",
  digest:
    "The tag has not changed, but the publisher has republished it — the same tag now points at different content.",
  rebind:
    "HarborMaster would recreate this container on the image it is already running, so it can be reattached to a network namespace whose provider was replaced. No version changes.",
  unknown:
    "HarborMaster could not determine whether an update exists. This is not the same as being up to date.",
  none: "The tag resolves to the image already in use, and no newer tag is published in this series.",
};

/** Check statuses in report order. */
export const CHECK_STATUS_ORDER = [
  "ok",
  "pending",
  "failed",
  "rateLimited",
  "unauthorized",
  "notFound",
  "unsupported",
] as const satisfies readonly CheckStatus[];

export const CHECK_STATUS_LABELS: Record<CheckStatus, string> = {
  ok: "Checked",
  pending: "Not yet checked",
  failed: "Check failed",
  rateLimited: "Rate limited",
  unauthorized: "Private",
  notFound: "Not published",
  unsupported: "Not checkable",
};

export const CHECK_STATUS_MEANINGS: Record<CheckStatus, string> = {
  ok: "The most recent lookup succeeded.",
  pending: "This reference has not been looked up yet.",
  failed: "The registry could not be reached. The previous answer is retained.",
  rateLimited:
    "The registry asked HarborMaster to slow down. Checks resume on its schedule.",
  unauthorized:
    "The repository is private. HarborMaster holds no registry credentials by design.",
  notFound:
    "The registry has no such repository or tag — often a locally built image that was never published.",
  unsupported:
    "The reference names no public registry, so it can never be looked up.",
};
