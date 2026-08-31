import type { DependencyState } from "./dependencyTypes";
import type { ContainerState, Pagination } from "./inventoryTypes";
import type { UpdateType } from "./imageTypes";
import type { Recommendation } from "./planTypes";

/**
 * Automation types: update policies and the update engine.
 *
 * # What automation is, and what the UI must never get wrong about it
 *
 * Every other feature in HarborMaster reports, or acts when a person presses a
 * button. **This one changes the host on a timer.** The interface's job is to
 * make three things unmissable.
 *
 *  1. **Which mode a policy is in.** `observe` and `dryRun` change nothing.
 *     `approvalRequired` waits for a person. Only `automatic` acts. A UI that
 *     rendered those four the same would be the reason somebody believed their
 *     estate was being watched when it was being changed, or the reverse.
 *  2. **Why a container was NOT updated.** The hardest question an operator
 *     asks an automation system, and the reason every decision carries a
 *     closed-vocabulary `reason` rather than prose.
 *  3. **What a pause means.** HarborMaster stopped trying, on purpose, and in
 *     the rollback case it will not resume until a person says so.
 *
 * # There is no target in any request
 *
 * Read the request types below. Running a pass takes a boolean. Approving takes
 * a run id and a container name that must match a decision HarborMaster already
 * made. Pausing takes a container name the inventory already knows. **There is
 * no field anywhere in this file that carries an image, a tag, a digest, or a
 * registry** — a type with nowhere to put one is a stronger guarantee than a
 * check that rejects one.
 */

// ------------------------------------------------------------ vocabularies --

/**
 * How far an update may go. A CEILING, not a filter.
 *
 * `digestOnly` is the safest automation there is: the operator chose the tag,
 * and only its content moved. `major` is never a default — a major version is
 * where publishers put breaking changes.
 */
export type UpdateStrategy = "digestOnly" | "patch" | "minor" | "major";

export const UPDATE_STRATEGY_ORDER: UpdateStrategy[] = [
  "digestOnly",
  "patch",
  "minor",
  "major",
];

export const UPDATE_STRATEGY_LABELS: Record<UpdateStrategy, string> = {
  digestOnly: "Same tag only, when it is republished",
  patch: "Up to a patch version",
  minor: "Up to a minor version",
  major: "Up to a major version",
};

export const UPDATE_STRATEGY_DESCRIPTIONS: Record<UpdateStrategy, string> = {
  digestOnly:
    "Only a republished tag. The operator chose the tag; only its content moved.",
  patch: "A republished tag, or a patch version bump.",
  minor: "Everything up to and including a minor version bump.",
  major:
    "Everything, including a major version bump — where publishers put breaking changes.",
};

/** How far a policy is allowed to act. */
export type AutomationMode =
  | "observe"
  | "dryRun"
  | "approvalRequired"
  | "automatic";

export const AUTOMATION_MODE_ORDER: AutomationMode[] = [
  "observe",
  "dryRun",
  "approvalRequired",
  "automatic",
];

export const AUTOMATION_MODE_LABELS: Record<AutomationMode, string> = {
  observe: "Observe only",
  dryRun: "Dry run (decide, change nothing)",
  approvalRequired: "Require approval",
  automatic: "Automatic",
};

export const AUTOMATION_MODE_DESCRIPTIONS: Record<AutomationMode, string> = {
  observe:
    "Evaluates everything and changes nothing. The correct first setting on a real host.",
  dryRun: "Observe, plus the order things would happen in.",
  approvalRequired:
    "Decides automatically and waits for a person to release each update.",
  automatic: "Downloads and recreates without asking.",
};

/** Whether a mode can change the host at all. The single check the UI asks. */
export function modeMutates(mode: AutomationMode): boolean {
  return mode === "automatic";
}

/** Where one pass got to. */
export type AutomationRunState =
  | "running"
  | "completed"
  | "failed"
  | "interrupted";

export const AUTOMATION_RUN_STATE_ORDER: AutomationRunState[] = [
  "running",
  "completed",
  "failed",
  "interrupted",
];

export const AUTOMATION_RUN_STATE_LABELS: Record<AutomationRunState, string> = {
  running: "Running",
  completed: "Completed",
  failed: "Failed",
  interrupted: "Interrupted",
};

/** What started a pass. */
export type AutomationTrigger = "schedule" | "manual" | "dryRun" | "startup";

export const AUTOMATION_TRIGGER_LABELS: Record<AutomationTrigger, string> = {
  schedule: "Scheduled",
  manual: "Run by hand",
  dryRun: "Dry run",
  startup: "At startup",
};

/** What the engine concluded for one container. */
export type AutomationVerdict =
  | "update"
  | "wouldUpdate"
  | "awaitingApproval"
  | "skip";

export const AUTOMATION_VERDICT_LABELS: Record<AutomationVerdict, string> = {
  update: "Updated",
  wouldUpdate: "Would update",
  awaitingApproval: "Waiting for approval",
  skip: "Skipped",
};

/**
 * Why, from a closed vocabulary.
 *
 * Closed so this file can carry the operator-facing sentence for each, rather
 * than the server sending prose a client has to render blindly.
 */
export type AutomationReason =
  | "eligible"
  | "noPlan"
  | "noUpdate"
  | "notSelected"
  | "notEligible"
  | "noPolicy"
  | "policyDisabled"
  | "labelDisabled"
  | "labelPaused"
  | "observeMode"
  | "dryRunMode"
  | "approvalRequired"
  | "strategyCeiling"
  | "recommendation"
  | "windowClosed"
  | "windowUnresolvable"
  | "automationPaused"
  | "alreadyInFlight"
  | "concurrencyLimit"
  | "registryLimit"
  | "runLimit"
  | "refusedByService"
  | "error"
  | "selfUpdate"
  | "dependencyWaiting"
  | "dependencyBlocked"
  | "dependencyCycle"
  | "dependencyMissing"
  | "dependencyIneligible"
  | "dependentsNotRebindable";

export const AUTOMATION_REASON_LABELS: Record<AutomationReason, string> = {
  eligible: "Every check passed",
  noPlan: "No change plan",
  noUpdate: "Nothing to update",
  notSelected: "No policy selects it",
  notEligible: "Not eligible for a broad policy",
  noPolicy: "No policies defined",
  policyDisabled: "Policy is off",
  labelDisabled: "Opted out by label",
  labelPaused: "Paused by label",
  observeMode: "Observe mode",
  dryRunMode: "Dry run",
  approvalRequired: "Needs approval",
  strategyCeiling: "Larger than the policy permits",
  recommendation: "Planner wants a person",
  windowClosed: "Outside the maintenance window",
  windowUnresolvable: "Window could not be evaluated",
  automationPaused: "Automation is paused",
  alreadyInFlight: "Work already in flight",
  concurrencyLimit: "Concurrency limit",
  registryLimit: "Per-registry limit",
  runLimit: "Per-pass limit",
  refusedByService: "Refused by a preflight",
  error: "Could not be decided",
  selfUpdate: "HarborMaster's own container",
  // "Waiting", never "blocked" or "failed": the dependency will finish or fail
  // on its own and nothing is being asked of anybody meanwhile.
  dependencyWaiting: "Waiting for dependency",
  dependencyBlocked: "Blocked by dependency",
  dependencyCycle: "No safe update order",
  dependencyMissing: "Dependency unavailable",
  dependencyIneligible: "Dependency not permitted to update",
  dependentsNotRebindable: "Dependants could not be reattached",
};

/**
 * The longer sentence for each reason.
 *
 * # Why these are separate from the labels
 *
 * A label fits a table cell and a sentence answers the question. "Per-pass
 * limit" is enough to scan a hundred rows; it is not enough to tell an operator
 * that nothing is wrong and the container is first in line next time.
 *
 * The dependency ones carry the distinction the whole subsystem turns on:
 * WAITING is the system working, BLOCKED is HarborMaster declining, and the
 * last is the one case where a container's own update is refused to protect
 * something else.
 */
export const AUTOMATION_REASON_DETAILS: Record<AutomationReason, string> = {
  eligible: "Every check passed and the update was submitted.",
  noPlan: "No current change plan exists for this container.",
  noUpdate: "The plan proposes no change.",
  notSelected: "No update policy selects this container.",
  notEligible:
    "A broad policy looked at this container and did not enrol it. " +
    "HarborMaster never enrols its own container, the containers it keeps as " +
    "evidence, or anything opted out by label.",
  noPolicy: "No update policy is defined.",
  policyDisabled: "The governing policy is switched off.",
  labelDisabled: "The container carries a label opting it out.",
  labelPaused: "The container carries a label pausing it.",
  observeMode: "The policy is in observe mode, so nothing was changed.",
  dryRunMode: "The policy is in dry-run mode, so nothing was changed.",
  approvalRequired: "The policy holds each update until a person releases it.",
  strategyCeiling: "The change is larger than the policy's ceiling permits.",
  recommendation: "The planner's assessment asks for a person to look first.",
  windowClosed: "The maintenance window is closed.",
  windowUnresolvable:
    "The maintenance window could not be evaluated, so it is treated as closed.",
  automationPaused: "Automation is paused for this container.",
  alreadyInFlight: "HarborMaster is already working on this container.",
  concurrencyLimit: "The pass had already reached its concurrency limit.",
  registryLimit: "The pass had already reached its limit for that registry.",
  runLimit:
    "The pass had already submitted as many updates as it will in one run. " +
    "Nothing is wrong: this container is simply next in line.",
  refusedByService: "A preflight check refused the request.",
  error: "The decision could not be made.",
  selfUpdate:
    "This is the container HarborMaster is running in. It is never updated " +
    "from the inside — update it from outside with your compose file.",
  dependencyWaiting:
    "A container this one depends on is still being updated. Nothing is " +
    "wrong and nothing is being asked of you; this clears when that update " +
    "finishes and verifies.",
  dependencyBlocked:
    "A container this one depends on could not be updated safely, so " +
    "HarborMaster did not proceed with this one either.",
  dependencyCycle:
    "These containers depend on each other in a loop, so no safe order " +
    "exists. The loop has to be broken before either can be updated.",
  dependencyMissing:
    "HarborMaster could not establish what this container depends on, so it " +
    "will not update it.",
  dependencyIneligible:
    "A container this one depends on needs an update the rules in force do " +
    "not permit, so this one waits on something that will not happen.",
  dependentsNotRebindable:
    "Containers share this one's namespace and HarborMaster could not " +
    "establish that it can safely reattach all of them. Replacing this " +
    "container would break them silently, so it was not replaced.",
};

/**
 * Whether a reason describes a FAILURE.
 *
 * Used to keep waiting out of failure styling. Only three reasons here mean
 * something went wrong; everything else is a decision, a limit, or the system
 * working as designed — and a page that painted all twenty-nine the same way
 * would teach an operator to ignore the colour.
 */
export function automationReasonIsFailure(reason: AutomationReason): boolean {
  return (
    reason === "error" ||
    reason === "refusedByService" ||
    reason === "dependencyBlocked" ||
    reason === "dependencyMissing" ||
    reason === "dependentsNotRebindable"
  );
}

/** Whether a reason is about a dependency at all. */
export function automationReasonIsDependency(reason: AutomationReason): boolean {
  return reason.startsWith("dependency") || reason === "dependentsNotRebindable";
}

/** Why automation stopped for a container. */
export type PauseReason = "repeatedFailure" | "automaticRollback" | "operator";

export const PAUSE_REASON_LABELS: Record<PauseReason, string> = {
  repeatedFailure: "Repeated failures",
  automaticRollback: "Rolled back",
  operator: "Paused by an operator",
};

// ------------------------------------------------------------- the policy --

/**
 * What a policy is POINTED AT.
 *
 * Its own field with its own vocabulary, not a value hidden in the selector.
 * The three stringly-typed ways of saying "everything" are all still refused:
 * a bare `*` by name, an empty selector because it means the opposite, and a
 * magic name in `include` because a container could be called that.
 *
 * `selector` is the default and is what an absent `scope` means, so a stored
 * policy written before the field existed keeps exactly the breadth it had.
 *
 * **Broad selection is not broad authorisation.** `allEligible` widens what a
 * policy looks at and nothing else — every container it selects still passes
 * every check in the pipeline, and a policy in this scope in `observe` mode
 * changes nothing.
 */
export type UpdateScope = "selector" | "allEligible";

export const UPDATE_SCOPE_ORDER: UpdateScope[] = ["selector", "allEligible"];

/**
 * The four choices the editor offers.
 *
 * Three of them are the `selector` scope with a different SHAPE of selector,
 * which is a UI distinction rather than a domain one: an operator picking
 * containers from a list and an operator typing an image glob are both writing
 * a selector, and the server sees no difference. The fourth is the real scope.
 *
 * Kept apart from `UpdateScope` deliberately. Collapsing them would either put
 * a UI concept into the request body or force the form to re-derive which of
 * three selector shapes an operator meant, and the second is how a form starts
 * disagreeing with the policy it saved.
 */
export type ScopeChoice = "allEligible" | "containers" | "images" | "advanced";

export const SCOPE_CHOICE_ORDER: ScopeChoice[] = [
  "allEligible",
  "containers",
  "images",
  "advanced",
];

export const SCOPE_CHOICE_LABELS: Record<ScopeChoice, string> = {
  allEligible: "All eligible containers",
  containers: "Selected containers",
  images: "Matching images",
  advanced: "Advanced selection",
};

export const SCOPE_CHOICE_DESCRIPTIONS: Record<ScopeChoice, string> = {
  allEligible:
    "All discovered workloads that pass HarborMaster's safety rules. Never HarborMaster itself, never the containers it keeps as evidence from an earlier update, and never one you exclude.",
  containers: "Choose one or more containers from the inventory.",
  images: "Match containers by image pattern.",
  advanced:
    "Container names, image patterns and labels together, with exclusions.",
};

/** Which domain scope a UI choice submits. */
export function scopeOfChoice(choice: ScopeChoice): UpdateScope {
  return choice === "allEligible" ? "allEligible" : "selector";
}

/**
 * Which UI choice a stored policy came from.
 *
 * A stored policy carries a scope and a selector, not a choice, so the editor
 * infers one when it opens. Deliberately biased towards `advanced`: showing an
 * operator MORE of their policy than they need is a smaller harm than showing
 * them a simple control that silently discards a clause they wrote.
 */
export function choiceOfPolicy(policy: {
  scope?: UpdateScope;
  selector?: UpdateSelector;
}): ScopeChoice {
  if (policy.scope === "allEligible") return "allEligible";

  const selector = policy.selector ?? {};
  const hasLabels = Object.keys(selector.labels ?? {}).length > 0;
  const hasImages = (selector.images ?? []).length > 0;
  const hasInclude = (selector.include ?? []).length > 0;

  // More than one kind of clause, or any labels, is Advanced: no simple
  // control can round-trip it.
  if (hasLabels) return "advanced";
  if (hasImages && hasInclude) return "advanced";
  if (hasImages) return "images";
  return "containers";
}

/**
 * Which containers a policy governs.
 *
 * Exclusion is checked FIRST and cannot be overridden — in every scope. An
 * empty selector governs NOTHING under `selector`, which the editor states
 * rather than leaving to be discovered; under `allEligible` an inclusion clause
 * is refused, because the scope already said what the policy reaches.
 */
export interface UpdateSelector {
  labels?: Record<string, string>;
  images?: string[];
  include?: string[];
  exclude?: string[];
}

/** When automation may act. */
export interface MaintenanceWindow {
  alwaysOpen: boolean;
  timezone?: string;
  /** `time.Weekday` values: 0 is Sunday. Empty means every day. */
  weekdays?: number[];
  start?: string;
  end?: string;
}

export interface UpdateLimits {
  maxConcurrent?: number;
  maxPerRegistry?: number;
  maxPerRun?: number;
  acquisitionTimeoutSeconds?: number;
  recreateTimeoutSeconds?: number;
  healthTimeoutSeconds?: number;
}

export interface UpdateFailureHandling {
  autoRollback?: boolean;
  pauseAfterFailures?: number;
  pauseWindowHours?: number;
  cooldownHours?: number;
  maxRetries?: number;
}

/** One administrator-defined automation rule. */
export interface UpdatePolicy {
  policyId: string;
  name: string;
  description?: string;
  enabled: boolean;
  priority: number;
  /** Absent on a policy stored before the field existed; treat as `selector`. */
  scope?: UpdateScope;
  selector: UpdateSelector;
  strategy: UpdateStrategy;
  minimumRecommendation: Recommendation;
  mode: AutomationMode;
  window: MaintenanceWindow;
  limits?: UpdateLimits;
  failure?: UpdateFailureHandling;
  archived?: boolean;
  createdAt?: string;
  updatedAt?: string;
}

/**
 * Who asked for a pass, or cleared a pause.
 *
 * Two fields and no more, matching the server's projection: no role, no
 * session, no address. What remains is the smallest thing that answers "whose
 * action was this" after the request is gone.
 */
export interface Requester {
  userId?: string;
  username?: string;
}

/** A stored policy plus the warnings it earned. */
/**
 * The automatic-updates switch (C1).
 *
 * One setting standing for one ordinary `UpdatePolicy`. It is a configuration
 * facade, not a second engine: turning it on writes a policy through the same
 * service and the same validation a hand-written one goes through, and nothing
 * downstream can tell the difference.
 *
 * # `enabled` and `engineEnabled` are different questions
 *
 * `engineEnabled` says this DEPLOYMENT can run automation at all — it reflects
 * an environment variable and cannot be changed from the UI. `enabled` says the
 * switch is on. A control that conflates them offers a toggle that silently
 * does nothing, which is the failure this pair exists to prevent.
 */
export interface SimpleUpdatesState {
  /** The managed policy exists and is in force. */
  enabled: boolean;
  /**
   * The managed policy exists at all, in force or not. Distinguishes "never
   * turned on" from "turned off" — different sentences on screen.
   */
  configured: boolean;
  /**
   * The managed rule, when there is one. The workspace describes the EFFECTIVE
   * behaviour from these stored values rather than restating them from a
   * constant that could drift from the server.
   */
  policy?: UpdatePolicy;
  /**
   * The managed policy's own warnings, generated by the same code that warns
   * about a hand-written policy. This is the honest disclosure text for the
   * confirmation.
   */
  warnings?: string[];
  /**
   * Active policies that outrank the managed one. A narrower or
   * higher-priority rule winning is the designed behaviour; this is reported so
   * an operator is not left wondering why the switch seems inert.
   */
  overriddenBy?: SimpleUpdatesOverride[];
  /** Whether this deployment can run automation at all. */
  engineEnabled: boolean;
  /** The environment variable that enables the engine. */
  engineVariable: string;
}

/** One policy that takes precedence over the managed one. */
export interface SimpleUpdatesOverride {
  policyId: string;
  name: string;
  scope: UpdateScope;
  priority: number;
  mode: AutomationMode;
}

export interface UpdatePolicyResult {
  policy: UpdatePolicy;
  /** Legal but worth seeing. They never refuse the policy. */
  warnings?: string[];
}

export interface UpdatePolicyListResponse {
  items: UpdatePolicy[];
  pagination: Pagination;
}

/**
 * A create or edit body.
 *
 * Note what is absent: there is no image, digest, tag, or registry field. A
 * policy says WHICH containers and HOW FAR; what image a matched container
 * moves to is the planner's decision from registry evidence.
 */
export interface UpdatePolicyRequest {
  name?: string;
  description?: string;
  enabled?: boolean;
  priority?: number;
  scope?: UpdateScope;
  selector?: UpdateSelector;
  strategy?: UpdateStrategy;
  minimumRecommendation?: Recommendation;
  mode?: AutomationMode;
  window?: MaintenanceWindow;
  limits?: UpdateLimits;
  failure?: UpdateFailureHandling;
}

export interface UpdatePolicyQuery {
  page?: number;
  pageSize?: number;
  sort?: string;
  order?: "asc" | "desc";
  enabled?: boolean;
  includeArchived?: boolean;
  mode?: AutomationMode[];
  search?: string;
}

// ------------------------------------------------------------- the engine --

export interface AutomationRun {
  runId: string;
  trigger: AutomationTrigger;
  state: AutomationRunState;
  dryRun?: boolean;
  considered?: number;
  eligible?: number;
  submitted?: number;
  skipped?: number;
  failed?: number;
  requestedBy?: Requester;
  message?: string;
  startedAt: string;
  completedAt?: string;
  durationMs?: number;
}

/**
 * One container's outcome in one pass.
 *
 * Every identity here was copied from a record HarborMaster wrote itself.
 */
export interface AutomationDecision {
  runId: string;
  containerId?: string;
  containerName: string;
  policyId?: string;
  policyName?: string;
  verdict: AutomationVerdict;
  reason: AutomationReason;
  detail?: string;
  planId?: string;
  currentImage?: string;
  proposedImage?: string;
  proposedDigest?: string;
  updateType?: UpdateType;
  recommendation?: Recommendation;
  acquisitionId?: string;
  executionId?: string;
  rollbackId?: string;

  /** The lifecycle state the pass saw. "Present" is not "stable". */
  containerState?: ContainerState;
  /**
   * What this container's dependencies looked like when the pass ran.
   *
   * DISTINCT from `reason`. `reason` says what the engine concluded; this says
   * what the dependencies looked like, and a container can be skipped for an
   * entirely unrelated reason while its dependencies are perfectly fine.
   */
  dependencyState?: DependencyState;
  /** The container responsible, when one is. A name from the inventory. */
  blockedBy?: string;

  position: number;
  decidedAt: string;
}

/** One container automation will not touch. */
export interface PausedContainer {
  containerName: string;
  containerId?: string;
  reason: PauseReason;
  detail?: string;
  failures?: number;
  policyId?: string;
  rollbackId?: string;
  executionId?: string;
  pausedAt: string;
  /** Absent means only an acknowledgement clears it. */
  resumeAfter?: string;
  acknowledgedAt?: string;
  acknowledgedBy?: Requester;
}

export interface AutomationRunSummary {
  total?: number;
  completed?: number;
  failed?: number;
  submitted?: number;
}

export interface AutomationStatus {
  enabled: boolean;
  running: boolean;
  policies?: number;
  enabledPolicies?: number;
  /**
   * Enabled policies whose mode may actually change a container.
   *
   * Decided by the server: "policies exist" and "a policy may act" are
   * different facts, and working the second one out here would be
   * reimplementing AutomationMode.Mutates in the browser.
   */
  actingPolicies?: number;
  /**
   * The deployment capabilities an acting policy needs.
   *
   * The RULE is the server's -- rollback appears only when a policy asks for
   * automatic rollback. The client compares these names against capability
   * flags it already has, and decides nothing.
   */
  requiredCapabilities?: string[];
  pausedContainers?: number;
  awaitingApproval?: number;
  lastRunAt?: string;
  nextRunAt?: string;
  lastRunId?: string;
  lastOutcome?: string;
  windowOpen?: boolean;
  nextWindowOpensAt?: string;
  nextWindowPolicyId?: string;
  /** The container HarborMaster is running in, and refuses to update. */
  self?: SelfIdentity;
}

/**
 * The container HarborMaster believes it is running in.
 *
 * # HarborMaster cannot update itself, and this is not configurable
 *
 * There is no setting that permits it. Acquisition and recreation refuse at
 * four independent layers, and an architecture test fails the build on a
 * configuration flag that would turn any of them off. Update HarborMaster from
 * outside it: `docker compose pull && docker compose up -d`.
 *
 * Every field is optional, and an EMPTY field matches nothing. A partial
 * identification therefore never excludes the wrong container, and a wholly
 * empty identity — HarborMaster running outside a container — excludes nothing.
 */
export interface SelfIdentity {
  containerId?: string;
  containerName?: string;
  imageRef?: string;
  imageId?: string;
  source?: "configured" | "runtime" | "hostname" | "label" | "none";
  /** HarborMaster's own sentence about how it decided. */
  detail?: string;
}

export interface AutomationStatusResponse {
  status: AutomationStatus;
  history: AutomationRunSummary;
}

export interface AutomationRunListResponse {
  items: AutomationRun[];
  pagination: Pagination;
  summary: AutomationRunSummary;
}

export interface AutomationRunDetailResponse {
  run: AutomationRun;
  decisions: AutomationDecision[];
  pagination: Pagination;
}

export interface AutomationUpcomingResponse {
  items: AutomationDecision[];
  eligible: number;
}

export interface AutomationPauseListResponse {
  items: PausedContainer[];
  pagination: Pagination;
}

/**
 * The decisions an approval-required policy is holding.
 *
 * A pass records a decision for every container it considered and skips most of
 * them, so the ones that ask something of an operator were previously buried in
 * an archived pass table. This carries only the `awaitingApproval` verdict.
 */
export interface AutomationApprovalListResponse {
  items: AutomationDecision[];
  pagination: Pagination;
}

export interface AutomationRunQuery {
  page?: number;
  pageSize?: number;
  state?: AutomationRunState[];
  trigger?: AutomationTrigger[];
  acted?: boolean;
}

// ------------------------------------------------------------- rendering --

/** Whether a run is still moving, for the poll decision. */
export function isAutomationRunActive(run: AutomationRun): boolean {
  return run.state === "running";
}

/** Whether a pause still blocks automation at an instant. */
export function isPauseActive(pause: PausedContainer, at: Date): boolean {
  if (pause.acknowledgedAt) return false;
  if (!pause.resumeAfter) return true;
  return at.getTime() < new Date(pause.resumeAfter).getTime();
}

/**
 * Renders a maintenance window in operator-facing words.
 *
 * Built in the client from the same fields the server compares against, so the
 * two cannot disagree about what a window says. The server still owns whether
 * the window is OPEN — that is a timezone calculation, and duplicating it here
 * is how a UI comes to disagree with the engine twice a year.
 */
const WEEKDAY_NAMES = [
  "Sunday",
  "Monday",
  "Tuesday",
  "Wednesday",
  "Thursday",
  "Friday",
  "Saturday",
];

export function describeWindow(window: MaintenanceWindow): string {
  if (window.alwaysOpen) return "At any time";

  const zone = window.timezone?.trim() || "UTC";
  const days =
    window.weekdays && window.weekdays.length > 0
      ? window.weekdays
          .filter((day) => day >= 0 && day <= 6)
          .map((day) => WEEKDAY_NAMES[day])
          .join(", ")
      : "every day";

  const crossing = crossesMidnight(window) ? " (crossing midnight)" : "";
  return `${window.start ?? "??:??"}–${window.end ?? "??:??"}${crossing} ${zone}, ${days}`;
}

/** Whether the window wraps past midnight, for the label above. */
export function crossesMidnight(window: MaintenanceWindow): boolean {
  const start = minutesOfDay(window.start);
  const end = minutesOfDay(window.end);
  if (start === null || end === null) return false;
  return end < start;
}

function minutesOfDay(value: string | undefined): number | null {
  if (!value) return null;
  const parts = value.split(":");
  if (parts.length !== 2) return null;
  const hour = Number(parts[0]);
  const minute = Number(parts[1]);
  if (!Number.isInteger(hour) || hour < 0 || hour > 23) return null;
  if (!Number.isInteger(minute) || minute < 0 || minute > 59) return null;
  return hour * 60 + minute;
}

/** Whether a selector could match anything at all. */
export function selectorIsEmpty(selector: UpdateSelector): boolean {
  return (
    Object.keys(selector.labels ?? {}).length === 0 &&
    (selector.images ?? []).length === 0 &&
    (selector.include ?? []).length === 0
  );
}
