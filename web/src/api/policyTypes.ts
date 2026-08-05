import type { Pagination } from "./inventoryTypes";

/**
 * Policy engine types.
 *
 * A policy is an administrator-defined rule that a container's configuration is
 * CHECKED AGAINST. HarborMaster never applies, enforces, or pushes one to
 * Docker, so there is no enforcement type here and no client function that
 * could invoke one.
 *
 * # Policy is not drift
 *
 * Drift answers "did this container change from its baseline?". A policy
 * answers "does this container comply with an organisational rule?". A
 * container can drift into a still-compliant configuration, and one that has
 * never moved can be non-compliant from the day it was created because the rule
 * arrived later. The two are separate models on purpose.
 *
 * # Secrets
 *
 * Environment-variable rules match NAMES. A violation's `observed` renders the
 * offending variable NAMES and never a value — the server has no field capable
 * of holding one by the time a rule runs.
 */

/** How much a violation matters. */
export type PolicySeverity = "critical" | "high" | "medium" | "low";

/**
 * The lifecycle state of a violation.
 *
 * `active` and `resolved` are engine-owned facts. The other two are operator
 * intent, and are the only ones a PATCH may set. Neither of them suppresses
 * re-evaluation: an acknowledged violation is re-checked on every pass and
 * resolves automatically once the container complies.
 */
export type PolicyViolationStatus =
  | "active"
  | "resolved"
  | "acknowledged"
  | "exempted";

/** The statuses an operator may set. Mirrors the API's allowlist. */
export const POLICY_OPERATOR_STATUSES = [
  "acknowledged",
  "exempted",
] as const satisfies readonly PolicyViolationStatus[];

export type PolicyOperatorStatus = (typeof POLICY_OPERATOR_STATUSES)[number];

/** The closed vocabulary of rule types. */
export type PolicyRuleType =
  | "privilegedForbidden"
  | "readOnlyRootFilesystemRequired"
  | "imageAllowlist"
  | "imageDenylist"
  | "requiredLabels"
  | "forbiddenLabels"
  | "requiredEnv"
  | "forbiddenEnv"
  | "requiredCapabilities"
  | "forbiddenCapabilities"
  | "memoryLimitRequired"
  | "cpuLimitRequired"
  | "restartPolicyAllowlist"
  | "networkAllowlist"
  | "userNotRoot"
  | "healthCheckRequired";

/** What a rule's `values` mean, so the editor can label the input correctly. */
export type PolicyValueKind =
  | "none"
  | "imagePattern"
  | "labelKeyPattern"
  | "envNamePattern"
  | "capability"
  | "restartPolicy"
  | "networkName";

/** One typed check. `values` is meaningful only for rules that take them. */
export interface PolicyRule {
  type: PolicyRuleType;
  /** Empty inherits the policy's default severity. */
  severity?: PolicySeverity;
  values?: string[];
}

/** One administrator-defined policy. */
export interface PolicyDefinition {
  /** Immutable and server-generated. Never sent on a create or update. */
  policyId: string;
  name: string;
  description?: string;
  severity: PolicySeverity;

  enabled: boolean;
  /**
   * Set by DELETE, which ARCHIVES rather than destroys: a violation references
   * its policy and the history has to survive the rule being withdrawn.
   */
  archived: boolean;
  archivedAt?: string;

  rules: PolicyRule[];
  createdAt: string;
  updatedAt: string;
}

/**
 * The create and update body.
 *
 * Every field is optional because a PATCH is partial and an omitted field is
 * left alone. A create must supply `name`, `severity` and `rules`; the server
 * rejects one that does not.
 *
 * There is deliberately no `policyId`: the identifier is server-generated and
 * immutable, and the server rejects a body carrying one.
 */
export interface PolicyRequest {
  name?: string;
  description?: string;
  severity?: PolicySeverity;
  enabled?: boolean;
  rules?: PolicyRule[];
}

/** One rule type as the editor renders it. */
export interface PolicyRuleSpec {
  type: PolicyRuleType;
  label: string;
  /**
   * What the rule checks, including where HarborMaster's view is narrower than
   * the daemon's. Rendered in the editor so an operator is not choosing blind.
   */
  description: string;
  valueKind: PolicyValueKind;
  requiresValues: boolean;
}

/** The definition bounds the server enforces. */
export interface PolicyLimits {
  maxRules: number;
  maxValuesPerRule: number;
  maxNameBytes: number;
  maxDescriptionBytes: number;
}

/**
 * The catalogue the editor is built from.
 *
 * Fetched rather than hardcoded: a second copy in the frontend would eventually
 * offer a rule the backend rejects.
 */
export interface PolicyRuleCatalogue {
  rules: PolicyRuleSpec[];
  severities: PolicySeverity[];
  restartPolicyNames: string[];
  limits: PolicyLimits;
}

/** One rule that a container fails. */
export interface PolicyViolation {
  id: number;
  policyId: string;
  policyName: string;
  containerId: string;
  containerName: string;

  ruleType: PolicyRuleType;
  severity: PolicySeverity;

  /** When the violation was FIRST seen; it does not move on re-evaluation. */
  detectedAt: string;
  lastSeenAt: string;
  resolvedAt?: string;
  inventoryGeneration: number;

  /** What the container has. NEVER an environment variable value. */
  observed?: string;
  /** What the rule required. */
  expected?: string;
  reason?: string;

  status: PolicyViolationStatus;
  note?: string;
  statusChangedAt?: string;
}

/** One container's most recent compliance pass. */
export interface PolicyEvaluation {
  containerId: string;
  containerName: string;
  evaluatedAt: string;
  inventoryGeneration: number;
  policiesEvaluated: number;
  rulesEvaluated: number;
  violationCount: number;
  /** True only when the pass was COMPLETE and found nothing. */
  compliant: boolean;
  /** False when the pass could not apply every policy. */
  complete: boolean;
  reason?: string;
}

/** The compliance aggregate. */
export interface PolicySummary {
  policies: number;
  policiesTotal: number;

  total: number;
  open: number;

  bySeverity: Partial<Record<PolicySeverity, number>>;
  byStatus: Partial<Record<PolicyViolationStatus, number>>;
  byRule: Partial<Record<PolicyRuleType, number>>;

  /**
   * Containers a pass has been ATTEMPTED for. Without it, "6 containers are
   * compliant" implies the rest were checked and failing.
   */
  containersEvaluated: number;
  containersCompliant: number;
  containersNonCompliant: number;

  lastEvaluatedAt?: string;
  /** At least one pass could not apply every policy. */
  incomplete: boolean;
}

/** One container's violations, with the pass that produced them. */
export interface ContainerPolicy {
  containerId: string;
  violations: PolicyViolation[];
  pagination: Pagination;
  /** Absent when the container has never been evaluated. */
  evaluation?: PolicyEvaluation;
}

/** The engine's queue state, echoed by the evaluate endpoint. */
export interface PolicyEngineStatus {
  enabled: boolean;
  policyCount: number;
  pendingEvaluations: number;
  sweepPending: boolean;
  overflowed: boolean;
}

/** The 202 body from POST /policy/evaluate. */
export interface PolicyEvaluateAccepted {
  requested: boolean;
  engine: PolicyEngineStatus;
}

/** The policy list query. */
export interface PolicyQuery {
  page?: number;
  pageSize?: number;
  search?: string;
  enabled?: boolean;
  includeArchived?: boolean;
  sort?: string;
  order?: "asc" | "desc";
}

/** The violation list query. Every value is a vocabulary the API validates. */
export interface PolicyViolationQuery {
  page?: number;
  pageSize?: number;
  containerId?: string;
  policyId?: string;
  rule?: PolicyRuleType[];
  severity?: PolicySeverity[];
  status?: PolicyViolationStatus[];
  openOnly?: boolean;
  sort?: string;
  order?: "asc" | "desc";
}

/** Severities in report order, most severe first. */
export const POLICY_SEVERITY_ORDER = [
  "critical",
  "high",
  "medium",
  "low",
] as const satisfies readonly PolicySeverity[];

/**
 * A human label for each rule type, for the places a full catalogue lookup is
 * not available — a violation row rendered before the catalogue loads.
 *
 * The catalogue from the server is authoritative where both exist; this is a
 * fallback so a rule type never renders as a bare identifier.
 */
export const RULE_LABELS: Record<PolicyRuleType, string> = {
  privilegedForbidden: "Privileged mode forbidden",
  readOnlyRootFilesystemRequired: "Read-only root filesystem required",
  imageAllowlist: "Image allowlist",
  imageDenylist: "Image denylist",
  requiredLabels: "Required labels",
  forbiddenLabels: "Forbidden labels",
  requiredEnv: "Required environment variables",
  forbiddenEnv: "Forbidden environment variables",
  requiredCapabilities: "Required capabilities",
  forbiddenCapabilities: "Forbidden capabilities",
  memoryLimitRequired: "Memory limit required",
  cpuLimitRequired: "CPU limit required",
  restartPolicyAllowlist: "Restart policy allowlist",
  networkAllowlist: "Network allowlist",
  userNotRoot: "User must not be root",
  healthCheckRequired: "Health check required",
};
