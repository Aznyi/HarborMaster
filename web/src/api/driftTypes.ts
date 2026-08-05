import type { Pagination } from "./inventoryTypes";

/**
 * Configuration drift types.
 *
 * Drift is an OBSERVATION: the differences between a container's baseline
 * snapshot and its configuration now. HarborMaster reports that something
 * moved and has no capability to move it back, so there is no remediation
 * type here and no client function that could apply one.
 *
 * # Secrets
 *
 * `previousValue` and `currentValue` are optional in this type ON PURPOSE.
 * A secret-backed field carries `sensitive: true` and neither value — the API
 * never sends them, so the type cannot promise them. A component that wants to
 * show a value has to handle their absence, which is what stops a UI rendering
 * `undefined` where a password would have been.
 */

/** The part of the configuration that moved. */
export type DriftCategory =
  | "security"
  | "image"
  | "mounts"
  | "ports"
  | "networks"
  | "process"
  | "health"
  | "environment"
  | "resources"
  | "restart"
  | "logging"
  | "labels"
  | "compose"
  | "metadata";

/** How much worse the host is now. */
export type DriftSeverity = "critical" | "high" | "medium" | "low";

/**
 * The lifecycle state of a record.
 *
 * `active` and `resolved` are engine-owned facts. The other three are operator
 * intent, and are the only ones a PATCH may set.
 */
export type DriftStatus =
  | "active"
  | "resolved"
  | "acknowledged"
  | "ignored"
  | "expected";

/** The statuses an operator may set. Mirrors the API's allowlist. */
export const OPERATOR_STATUSES = [
  "acknowledged",
  "ignored",
  "expected",
] as const satisfies readonly DriftStatus[];

export type OperatorStatus = (typeof OPERATOR_STATUSES)[number];

/** How a field differs. */
export type DriftChangeKind = "added" | "removed" | "modified" | "unverifiable";

/** One field that differs from the baseline. */
export interface DriftRecord {
  id: number;
  containerId: string;
  containerName: string;
  snapshotId: number;

  /** When the difference was FIRST seen; it does not move on re-evaluation. */
  detectedAt: string;
  lastSeenAt: string;
  resolvedAt?: string;
  inventoryGeneration: number;

  category: DriftCategory;
  field: string;
  kind: DriftChangeKind;
  severity: DriftSeverity;

  /** Absent when `sensitive` is true. */
  previousValue?: string;
  /** Absent when `sensitive` is true. */
  currentValue?: string;
  /** The field is secret-backed; only the fact of the change is reported. */
  sensitive?: boolean;

  status: DriftStatus;
  reason?: string;
  note?: string;
  statusChangedAt?: string;
}

/** One container's most recent comparison attempt. */
export interface DriftEvaluation {
  containerId: string;
  containerName: string;
  snapshotId: number;
  evaluatedAt: string;
  inventoryGeneration: number;
  driftCount: number;
  /**
   * False when the comparison could not examine everything. `driftCount` is
   * then a floor rather than a total.
   */
  complete: boolean;
  reason?: string;
}

/** The dashboard aggregate. */
export interface DriftSummary {
  total: number;
  open: number;
  bySeverity: Partial<Record<DriftSeverity, number>>;
  byStatus: Partial<Record<DriftStatus, number>>;
  byCategory: Partial<Record<DriftCategory, number>>;
  containersWithDrift: number;
  /**
   * Containers a comparison has been ATTEMPTED for. Without it, "12 containers
   * have drift" implies the rest were checked and clean.
   */
  containersEvaluated: number;
  lastEvaluatedAt?: string;
  /** At least one evaluation could not examine everything. */
  incomplete: boolean;
}

/** One container's drift, with the evaluation that produced it. */
export interface ContainerDrift {
  containerId: string;
  records: DriftRecord[];
  pagination: Pagination;
  /** Absent when the container has never been evaluated. */
  evaluation?: DriftEvaluation;
}

/** The drift list query. Every value is a closed vocabulary the API validates. */
export interface DriftQuery {
  page?: number;
  pageSize?: number;
  containerId?: string;
  category?: DriftCategory[];
  severity?: DriftSeverity[];
  status?: DriftStatus[];
  openOnly?: boolean;
  sort?: string;
  order?: "asc" | "desc";
}

/** Severities in report order, most severe first. */
export const SEVERITY_ORDER = [
  "critical",
  "high",
  "medium",
  "low",
] as const satisfies readonly DriftSeverity[];

/** Categories in report order: roughly most to least security-relevant. */
export const CATEGORY_ORDER = [
  "security",
  "image",
  "mounts",
  "ports",
  "networks",
  "process",
  "health",
  "environment",
  "resources",
  "restart",
  "logging",
  "labels",
  "compose",
  "metadata",
] as const satisfies readonly DriftCategory[];
