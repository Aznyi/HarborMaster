/**
 * Types mirroring HarborMaster's snapshot API.
 *
 * Hand-maintained against `api/openapi.yaml`, which is the contract. These are
 * HarborMaster's own shapes: the API exposes no Docker SDK type, so none
 * appears here either.
 *
 * A snapshot records what a container's configuration WAS. HarborMaster cannot
 * restore one, and there is no client function here that could -- the API has
 * no such endpoint.
 *
 * ## Secrets
 *
 * A sensitive entry carries no `value` and no digest, and the types say so:
 * `SnapshotEnvEntry.value` is optional and there is deliberately no `digest`
 * field to read. The masking guarantee is a property of the payload, enforced
 * server-side, and the type makes it visible in the editor rather than
 * something a component has to remember.
 */

/** What caused a capture. `scheduled` and `pre_update` are reserved for later phases. */
export type SnapshotTrigger = "manual" | "api" | "scheduled" | "pre_update";

/**
 * A restore-readiness verdict.
 *
 * `unknown` means no evaluation has run yet. `unverifiable` appears on
 * individual checks only: HarborMaster could not establish the answer, and it
 * caps the overall verdict at `warning` rather than being rounded to a pass.
 */
export type ReadinessStatus =
  | "unknown"
  | "ready"
  | "warning"
  | "not_ready"
  | "unverifiable";

/** Identifier of one restore-readiness check. */
export type ReadinessCheckId =
  | "daemon_reachable"
  | "inventory_fresh"
  | "image_available"
  | "image_digest_known"
  | "named_volumes_present"
  | "mount_sources"
  | "networks_present"
  | "restart_policy_valid"
  | "compose_metadata_complete"
  | "secrets_available"
  | "config_consistent"
  | "runtime_features";

/** How a captured value is treated. */
export type Sensitivity = "normal" | "sensitive";

/** A configuration section in a diff. A closed vocabulary. */
export type DiffGroupName =
  | "environment"
  | "labels"
  | "ports"
  | "networks"
  | "mounts"
  | "resources"
  | "security"
  | "compose"
  | "metadata";

/**
 * How one entry differs.
 *
 * `unverifiable` means the two values cannot be compared -- two secret digests
 * produced under different HMAC keys. Rendering that as "modified" would tell
 * an operator every secret changed after a key rotation.
 */
export type ChangeKind =
  | "added"
  | "removed"
  | "modified"
  | "unchanged"
  | "unverifiable";

/**
 * One captured environment variable or log-driver option.
 *
 * Note what is NOT here: no `digest`, and `value` is optional. A sensitive
 * entry exposes only its name, that it is set, and how long it is.
 */
export interface SnapshotEnvEntry {
  position: number;
  key: string;
  classification: Sensitivity;
  /** False means the variable was unset, which differs from being empty. */
  present: boolean;
  /** Absent for a sensitive entry. */
  value?: string;
  length?: number;
}

export interface SnapshotMount {
  destination: string;
  type: "bind" | "volume" | "tmpfs" | "npipe" | "cluster" | "unknown";
  source?: string;
  readOnly?: boolean;
  volumeName?: string;
  driver?: string;
}

export interface SnapshotNetwork {
  networkName: string;
  aliases?: string[];
}

/**
 * The canonical configuration document.
 *
 * Loosely typed on purpose: the UI renders it section by section and does not
 * reimplement the server's model. Volatile fields are absent from it entirely.
 */
export interface SnapshotSpec {
  specVersion: number;
  identity?: { containerId?: string; containerName?: string };
  image?: {
    reference?: string;
    repository?: string;
    tag?: string;
    digest?: string;
    imageId?: string;
    repoDigests?: string[];
    architecture?: string;
    os?: string;
  };
  [section: string]: unknown;
}

/** An immutable configuration checkpoint. */
export interface Snapshot {
  id: number;
  hostId?: string;
  containerId: string;
  containerName: string;

  imageReference?: string;
  imageDigest?: string;
  imageId?: string;

  specVersion: number;
  /** Omitted from list responses; present on detail. */
  spec?: SnapshotSpec;
  checksum: string;

  harbormasterVersion?: string;
  dockerApiVersion?: string;
  dockerEngineVersion?: string;

  trigger: SnapshotTrigger;
  reason?: string;

  inventoryGeneration?: number;
  eventSequence?: number;

  warnings?: { code: string; message: string; occurredAt: string }[];
  warningCount?: number;

  readinessStatus: ReadinessStatus;
  readinessEvaluatedAt?: string;

  /** Digests under different key IDs are not comparable. */
  digestKeyId?: string;
  createdAt: string;

  /** Capture responses only: the configuration was unchanged, nothing created. */
  deduplicated?: boolean;
}

export interface SnapshotDetail extends Snapshot {
  environment: SnapshotEnvEntry[];
  mounts: SnapshotMount[];
  networks: SnapshotNetwork[];
}

export interface ReadinessCheck {
  id: ReadinessCheckId;
  status: ReadinessStatus;
  detail?: string;
}

/**
 * A complete restore-readiness evaluation.
 *
 * The freshness fields matter: most checks answer from HarborMaster's cached
 * inventory, so a verdict is only as trustworthy as that inventory is recent.
 */
export interface ReadinessReport {
  snapshotId: number;
  status: ReadinessStatus;
  checks: ReadinessCheck[];
  evaluatedAt: string;

  daemonCheckedAt?: string;
  inventoryGeneration?: number;
  inventoryCompletedAt?: string;
  inventoryAgeSeconds?: number;
  inventoryStale?: boolean;

  readyCount?: number;
  warningCount?: number;
  notReadyCount?: number;
  unverifiableCount?: number;
}

export interface DiffEntry {
  key: string;
  kind: ChangeKind;
  /** Always absent for a sensitive entry. */
  old?: string;
  /** Always absent for a sensitive entry. */
  new?: string;
  sensitive?: boolean;
  note?: string;
}

export interface DiffGroup {
  name: DiffGroupName;
  entries: DiffEntry[];
  added?: number;
  removed?: number;
  modified?: number;
  unchanged?: number;
  /** The group stopped short. Never silent. */
  truncated?: boolean;
  returned?: number;
  total?: number;
}

export interface SnapshotDiff {
  fromSnapshotId: number;
  /** Zero when the target was live configuration. */
  toSnapshotId?: number;
  againstCurrent?: boolean;
  groups: DiffGroup[];
  /** Never true on a truncated diff. */
  identical: boolean;

  addedCount?: number;
  removedCount?: number;
  modifiedCount?: number;
  changedCount?: number;
  unchangedCount?: number;

  truncated?: boolean;
  truncationReason?: string;
}

/** Query parameters for the snapshot list. */
export interface SnapshotQuery {
  page?: number;
  pageSize?: number;
  containerId?: string;
  trigger?: SnapshotTrigger[];
  readiness?: ReadinessStatus[];
  checksum?: string;
  since?: string;
  until?: string;
  sort?: "createdAt" | "id" | "container" | "readiness" | "trigger";
  direction?: "asc" | "desc";
}

/** Body of a capture request. */
export interface CreateSnapshotRequest {
  containerId: string;
  reason?: string;
}
