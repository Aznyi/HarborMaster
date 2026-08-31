/**
 * Types mirroring HarborMaster's inventory API.
 *
 * Hand-maintained against `api/openapi.yaml`, which is the contract. These are
 * HarborMaster's own shapes -- no Docker SDK type is exposed by the API, so
 * none appears here either.
 */

import type { DependencyState } from "./dependencyTypes";
import type { HealthComponent } from "./types";
import type { UpdateType } from "./imageTypes";
import type { Recommendation } from "./planTypes";
import type { PolicySeverity } from "./policyTypes";
import type { CheckStatus } from "./imageTypes";

// ---------------------------------------------------------------- shared --

export interface Pagination {
  page: number;
  pageSize: number;
  totalItems: number;
  totalPages: number;
  hasNext: boolean;
  hasPrevious: boolean;
}

export interface ListResponse<T> {
  items: T[];
  pagination: Pagination;
}

// ------------------------------------------------------------- inventory --

export type RefreshState = "idle" | "running" | "succeeded" | "failed";
export type RefreshTrigger = "startup" | "periodic" | "manual";

export type WarningCode =
  | "container_vanished"
  | "inspect_failed"
  | "image_unavailable"
  | "incomplete_data";

export interface InventoryWarning {
  id?: number;
  generation: number;
  containerId?: string;
  containerName?: string;
  code: WarningCode;
  message: string;
  occurredAt: string;
}

export interface RefreshRecord {
  id?: number;
  generation: number;
  trigger: RefreshTrigger;
  state: RefreshState;
  startedAt: string;
  finishedAt?: string;
  durationMs: number;
  containersListed: number;
  containersInspected: number;
  containersFailed: number;
  imagesInspected: number;
  networksListed: number;
  volumesListed: number;
  warningCount: number;
  checksum?: string;
  error?: string;
}

export interface InventoryCounts {
  containers: number;
  absent: number;
  running: number;
  stopped: number;
  paused: number;
  restarting: number;
  healthy: number;
  unhealthy: number;
  images: number;
  networks: number;
  volumes: number;
  warnings: number;
  byState: Record<string, number>;
}

export interface InventoryStatus {
  enabled: boolean;
  runtime: string;
  docker: HealthComponent;
  state: RefreshState;
  inProgress: boolean;
  /** Advances only when a refresh completes AND persists. 0 means none yet. */
  generation: number;
  checksum?: string;
  lastAttempt?: RefreshRecord;
  lastSuccess?: RefreshRecord;
  counts: InventoryCounts;
  warnings: InventoryWarning[];
}

export interface RefreshAccepted {
  accepted: boolean;
  trigger: RefreshTrigger;
  startedAt: string;
  message: string;
}

export interface FilterOptions {
  states: string[];
  health: string[];
  projects: string[];
  images: string[];
  sortFields: string[];
}

// ------------------------------------------------------------ containers --

export type ContainerState =
  | "created"
  | "running"
  | "paused"
  | "restarting"
  | "removing"
  | "exited"
  | "dead"
  | "unknown";

export type HealthState = "none" | "starting" | "healthy" | "unhealthy";

export interface ImageRef {
  raw: string;
  repository?: string;
  tag?: string;
  digest?: string;
}

export interface RestartPolicy {
  name: string;
  maximumRetryCount?: number;
}

export interface Port {
  containerPort: number;
  protocol: string;
  hostIp?: string;
  hostPort?: number;
  /** Only published ports are reachable from the host. */
  published: boolean;
}

export interface ComposeMetadata {
  managed: boolean;
  project?: string;
  service?: string;
  containerNumber?: number;
  workingDir?: string;
  configFiles?: string;
  version?: string;
  oneOff: boolean;
}

export interface HarborMasterMetadata {
  /** Absent, true, and false are three distinct answers. */
  enabled?: boolean;
  labels?: Record<string, string>;
}

/**
 * The single headline verdict for one container.
 *
 * A closed vocabulary decided by the server. `notChecked` and `upToDate` are
 * different facts and must not be rendered alike: the first says HarborMaster
 * has not looked, the second that it looked and found nothing to do.
 */
export type AttentionState =
  | "preserved"
  | "unhealthy"
  | "dependencyFailed"
  | "approvalRequired"
  | "paused"
  | "dependencyCycle"
  | "dependencyUnresolved"
  | "needsReview"
  | "dependencyBlocked"
  | "cannotAdvise"
  | "updateAvailable"
  | "notTracked"
  | "notComparable"
  | "notChecked"
  | "upToDate";

/** Why HarborMaster is keeping a container that is not a workload. */
export type PreservedKind = "original" | "failed" | "rolledBack" | "suspected";

/**
 * The last thing HarborMaster did to a container.
 *
 * Absent when there has never been one. That must render as "HarborMaster has
 * not changed this container", never as a success.
 */
export interface ActionOutcome {
  id: string;
  state: string;
  /** The closed-vocabulary reason, empty unless it failed. */
  failure?: string;
  at?: string;
  /** A failure that left containers on the host for somebody to settle. */
  needsAttention: boolean;
}

/**
 * What HarborMaster knows about one container, for the list row.
 *
 * Computed server-side from a batched lookup over the whole page. Nothing here
 * is derived in the browser, so a row cannot disagree with the container's own
 * detail page about whether an update exists.
 */
/**
 * How one container should be updated (C2).
 *
 * Three values, each naming an automation mode the engine already has. None is
 * a new concept, and none can make a container MORE mutable: a preference
 * narrows the governing policy and can never widen it.
 *
 * There is deliberately no `excluded`. It would be `monitorOnly` under a second
 * name — and the version that stopped WATCHING would remove the container from
 * the Updates workspace, so an operator would no longer be told a critical
 * patch was waiting. Somebody who genuinely wants HarborMaster to ignore a
 * container sets `io.harbormaster.enabled=false` where the container is
 * defined, which outranks everything here.
 */
export type UpdateBehavior = "automatic" | "reviewFirst" | "monitorOnly";

/** One container's stored choice. */
export interface ContainerUpdatePreference {
  containerName: string;
  behavior: UpdateBehavior;
  /** Evidence of the container observed when this was written. Not the key. */
  containerId?: string;
  setByUsername?: string;
  createdAt?: string;
  updatedAt?: string;
}

/**
 * What was asked for, and what the engine will actually do.
 *
 * They genuinely differ, which is the whole reason both are sent. A container an
 * update policy holds for review stays held however the control is set, and
 * `effective` names the rule that decided. The server derives it from the
 * engine's own decision; nothing here re-implements policy selection.
 */
export interface ContainerUpdateBehavior {
  containerName: string;
  /** Absent when nobody has chosen: the container inherits. */
  requested?: ContainerUpdatePreference;
  effective: {
    /** False when the engine could not be consulted. Assume nothing. */
    known: boolean;
    verdict?: string;
    reason?: string;
    detail?: string;
    policyId?: string;
    policyName?: string;
  };
}

/**
 * Which containers carry a saved update behaviour.
 *
 * A summary of what was SAVED, not of what will happen. The two differ — a
 * preference may only make automation safer, so a policy can still hold a
 * container listed here as `automatic` — and the effective behaviour is
 * deliberately absent, because computing it per container turns one page load
 * into one estate evaluation per preference.
 *
 * Every field is always sent. `counts` carries a key for every behaviour,
 * including the ones nobody chose, so a client renders a real zero rather than
 * an absence.
 */
export interface ContainerBehaviorSummary {
  /** One entry per saved behaviour, by container name. Stale entries included. */
  items: ContainerBehaviorItem[];
  /** Present containers per behaviour. Every behaviour has a key. */
  counts: Record<UpdateBehavior, number>;
  /** Present containers with a saved behaviour. The sum of `counts`. */
  total: number;
  /** Saved behaviours whose container is not here. Kept, never deleted. */
  stale: number;
}

/** One saved behaviour, resolved against the live inventory. */
export interface ContainerBehaviorItem {
  containerName: string;
  behavior: UpdateBehavior;
  /** Whether a container of this name is currently present. */
  present: boolean;
  /** The CURRENT container's id, when one is present. Never the stored one. */
  containerId?: string;
}

export interface ContainerAttention {
  state: AttentionState;
  /** Absent when no assessment exists. Never defaulted to "none". */
  updateType?: UpdateType;
  recommendation?: Recommendation;
  proposedImage?: string;
  /**
   * The state of the most recent registry lookup for this container's image,
   * and when one last ANSWERED.
   *
   * Both absent unless a comparison has actually settled, so neither may be
   * read as a claim that one has. Together they separate two states the model
   * deliberately keeps apart:
   *
   *   checkStatus "ok"      the verdict was reconfirmed just now
   *   checkStatus not "ok"  a real EARLIER verdict the latest attempt could
   *                         not reconfirm — true as of `lastCheckedOkAt`, and
   *                         not a claim about this moment
   *
   * HarborMaster preserves the earlier verdict rather than discarding it, so
   * the honest rendering of the second is "up to date as of <time>".
   */
  checkStatus?: CheckStatus;
  lastCheckedOkAt?: string;
  /** The tag update discovery follows, when there is one. */
  tracking?: string;
  /** False when HarborMaster has not yet established what this follows. */
  trackingKnown: boolean;
  awaitingApproval: boolean;
  automationPaused: boolean;
  openViolations: number;
  highestSeverity?: PolicySeverity;
  openDrift: number;
  preserved?: PreservedKind;
  /** The workload a preserved container belongs to. */
  preservedFor?: string;
  lastUpdate?: ActionOutcome;
  lastRollback?: ActionOutcome;

  /**
   * Whether the dependency subsystem answered for this container.
   *
   * FALSE ASSERTS NOTHING. A deployment without dependency tracking, or one
   * whose graph could not be built, produces exactly the verdicts it produced
   * before dependencies existed — never a fleet of containers claiming their
   * dependencies are satisfied.
   */
  dependencyKnown?: boolean;
  /**
   * The dependency verdict. Carried even when it did not change `state`:
   * `dependencyWaiting` never does, and a detail page still wants to explain
   * the delay.
   */
  dependencyState?: DependencyState;
  /** The container responsible, when one is. A name from the inventory. */
  dependencyBlockedBy?: string;
  /** A mandatory reattachment settled without succeeding, and is not retried. */
  rebindFailed?: boolean;
  /** A mandatory reattachment is in flight. Work, not a condition. */
  rebindPending?: boolean;
  /** The container whose replacement is being attached to. */
  rebindProvider?: string;
}

export interface ContainerSummary {
  hostId: string;
  id: string;
  shortId: string;
  name: string;
  image: ImageRef;
  imageId?: string;
  state: ContainerState;
  status?: string;
  health: HealthState;
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
  exitCode?: number;
  restartCount: number;
  restartPolicy: RestartPolicy;
  compose: ComposeMetadata;
  harbormaster: HarborMasterMetadata;
  ports: Port[];
  /** False for a container an earlier refresh saw but the latest did not. */
  present: boolean;
  firstSeenAt: string;
  lastSeenAt: string;
  generation: number;
  warningCount: number;
}

/**
 * A container list row: the summary plus what HarborMaster knows about it.
 *
 * The server embeds the summary, so every field a row carried before is in the
 * same place and `attention` is the addition.
 */
export interface ContainerListRow extends ContainerSummary {
  attention: ContainerAttention;
}

export interface HealthLogEntry {
  start: string;
  end: string;
  exitCode: number;
}

export interface StateDetail {
  state: ContainerState;
  rawState?: string;
  status?: string;
  running: boolean;
  paused: boolean;
  restarting: boolean;
  dead: boolean;
  oomKilled: boolean;
  exitCode?: number;
  error?: string;
  restartCount: number;
  startedAt?: string;
  finishedAt?: string;
  health: HealthState;
  healthFailingStreak?: number;
  /** Timings and exit codes only; probe output is never carried. */
  healthLog?: HealthLogEntry[];
}

export interface Process {
  hostname?: string;
  domainname?: string;
  entrypoint?: string[];
  command?: string[];
  user?: string;
  workingDir?: string;
  stopSignal?: string;
  stopTimeoutSeconds?: number;
  tty: boolean;
  stdinOpen: boolean;
}

/**
 * One environment variable or log-driver option.
 *
 * There is no field carrying the real value of a sensitive entry, by design:
 * the API never sends one, so the UI has nothing to reveal even accidentally.
 */
export interface EnvVar {
  name: string;
  /** The real value when not sensitive; "********" when it is. */
  value: string;
  sensitivity: "normal" | "sensitive";
}

export interface Label {
  key: string;
  value: string;
  source: "user" | "compose" | "harbormaster";
}

export interface Mount {
  type: "bind" | "volume" | "tmpfs" | "npipe" | "cluster" | "unknown";
  source?: string;
  destination: string;
  readOnly: boolean;
  propagation?: string;
  consistency?: string;
  volumeName?: string;
  driver?: string;
  driverOptions?: Record<string, string>;
  tmpfsOptions?: string;
}

export interface NetworkAttachment {
  networkId?: string;
  networkName: string;
  driver?: string;
  aliases?: string[];
  ipv4Address?: string;
  ipv6Address?: string;
  gateway?: string;
  macAddress?: string;
  endpointId?: string;
  links?: string[];
}

export interface HealthCheck {
  test?: string[];
  intervalMs?: number;
  timeoutMs?: number;
  startPeriodMs?: number;
  startIntervalMs?: number;
  retries?: number;
  disabled: boolean;
}

export interface Ulimit {
  name: string;
  soft: number;
  hard: number;
}

export interface Resources {
  cpuShares?: number;
  cpuQuota?: number;
  cpuPeriod?: number;
  nanoCpus?: number;
  cpusetCpus?: string;
  cpusetMems?: string;
  memoryBytes?: number;
  memoryReservationBytes?: number;
  memorySwapBytes?: number;
  memorySwappiness?: number;
  kernelMemoryBytes?: number;
  pidsLimit?: number;
  blkioWeight?: number;
  shmSizeBytes?: number;
  oomScoreAdj?: number;
  oomKillDisable?: boolean;
  ulimits?: Ulimit[];
}

export interface Device {
  pathOnHost: string;
  pathInContainer: string;
  cgroupPermissions?: string;
}

export interface DeviceRequest {
  driver?: string;
  count?: number;
  deviceIds?: string[];
  capabilities?: string[][];
  options?: Record<string, string>;
}

export interface Security {
  privileged: boolean;
  readonlyRootfs: boolean;
  capAdd?: string[];
  capDrop?: string[];
  securityOpt?: string[];
  apparmorProfile?: string;
  selinuxLabel?: string;
  seccompProfile?: string;
  noNewPrivileges: boolean;
  devices?: Device[];
  deviceCgroupRules?: string[];
  deviceRequests?: DeviceRequest[];
  ipcMode?: string;
  pidMode?: string;
  utsMode?: string;
  usernsMode?: string;
  cgroupnsMode?: string;
  sysctls?: Record<string, string>;
  groupAdd?: string[];
}

export interface Logging {
  driver?: string;
  options?: EnvVar[];
}

export interface Image {
  id: string;
  shortId: string;
  repoTags: string[];
  repoDigests: string[];
  createdAt?: string;
  architecture?: string;
  os?: string;
  osVersion?: string;
  variant?: string;
  size: number;
  author?: string;
  comment?: string;
  labels?: Record<string, string>;
}

/**
 * What HarborMaster FOLLOWS for a container, as distinct from the immutable
 * digest the container RUNS.
 *
 * A container HarborMaster has updated is created from `repo@sha256:...`, so
 * showing only the running reference makes an actively automated workload read
 * as deliberately pinned — the opposite of the truth.
 *
 * Absent when HarborMaster holds no lineage, which is a real answer rather than
 * an empty one.
 */
export interface ImageLineage {
  containerName: string;
  containerId?: string;
  state: "tracked" | "untracked";
  origin: "observed" | "recreation" | "migration";
  trackingReference?: string;
  trackingFamiliar?: string;
  repository?: string;
  runningDigest?: string;
  createdAt: string;
  updatedAt: string;
}

export interface ContainerDetail {
  overview: ContainerSummary;
  /**
   * What HarborMaster knows about this container: the same projection its list
   * row carries, so the two can never disagree about whether an update exists.
   */
  attention?: ContainerAttention;
  state: StateDetail;
  image?: Image;
  /** What this container follows for updates. Absent when nothing is tracked. */
  imageLineage?: ImageLineage;
  /** The digest this container is actually running, resolved from the local image. */
  runningDigest?: string;
  process: Process;
  healthCheck?: HealthCheck;
  environment: EnvVar[];
  labels: Label[];
  ports: Port[];
  mounts: Mount[];
  networks: NetworkAttachment[];
  resources: Resources;
  security: Security;
  logging: Logging;
  compose: ComposeMetadata;
  harbormaster: HarborMasterMetadata;
  warnings: InventoryWarning[];
}

export interface RawInspection {
  containerId: string;
  redacted: true;
  notice: string;
  inspection: unknown;
}

export interface ImageUsage {
  image: Image;
  containerCount: number;
}

export interface NetworkSummary {
  id: string;
  name: string;
  driver?: string;
  scope?: string;
  internal: boolean;
  attachable: boolean;
  ipv6: boolean;
  createdAt?: string;
  labels?: Record<string, string>;
  subnets?: string[];
}

export interface VolumeSummary {
  name: string;
  driver?: string;
  scope?: string;
  mountpoint?: string;
  createdAt?: string;
  labels?: Record<string, string>;
  options?: Record<string, string>;
}

/** Query parameters accepted by the container list endpoint. */
export interface ContainerQuery {
  page?: number;
  pageSize?: number;
  search?: string;
  state?: string[];
  health?: string[];
  project?: string;
  service?: string;
  image?: string;
  restartPolicy?: string;
  labelKey?: string;
  labelValue?: string;
  harbormasterEnabled?: boolean;
  includeAbsent?: boolean;
  /**
   * Include the containers HarborMaster parked aside as evidence.
   *
   * Excluded by default. Note the polarity: the narrow list is the default and
   * this opts IN to the fuller one, so a caller that omits it sees workloads.
   */
  includePreserved?: boolean;
  sort?: string;
  direction?: "asc" | "desc";
}
