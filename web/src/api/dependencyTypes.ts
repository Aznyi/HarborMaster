/**
 * Workload dependency types.
 *
 * # What a dependency is, and what it is not
 *
 * A dependency says "this container must be stable before that one changes". It
 * is an ORDERING constraint and, for the Docker-derived kinds, a statement that
 * the runtime itself ties the two containers together.
 *
 * It is never a way to make a container eligible for an update. HarborMaster's
 * update policies decide WHICH containers may be updated; dependencies decide
 * WHEN, and can only ever delay or block. Nothing in this file describes a
 * capability, and there is no client function here that changes a container.
 *
 * # Two origins, and the distinction matters to an operator
 *
 * DETECTED relationships are read from Docker's own configuration. HarborMaster
 * derives them on every read and they cannot be created or deleted through the
 * API — there is no stored row to remove. A dependent sharing a provider's
 * network namespace genuinely cannot run without it.
 *
 * CONFIGURED relationships are ordering an operator asserted about their own
 * application. They constrain order and nothing else.
 */

/** Where HarborMaster learned a relationship. */
export type DependencySource =
  | "dockerNetworkNamespace"
  | "dockerIPCNamespace"
  | "dockerPIDNamespace"
  | "operator";

/**
 * One container's dependency verdict.
 *
 * `dependencySatisfied` is the only state that lets work proceed, and it is
 * reached two ways: the dependency needed nothing and is running, or it needed
 * work and that work reached VERIFIED success. A submitted request satisfies
 * nothing.
 */
export type DependencyState =
  | "dependencySatisfied"
  | "dependencyWaiting"
  | "dependencyBlocked"
  | "dependencyCycle"
  | "dependencyMissing"
  | "dependencyIneligible";

/** Why a declared namespace share produced no relationship. */
export type DiscoveryRefusal =
  | "namespacesUnobserved"
  | "referenceNotParseable"
  | "referencedContainerUnknown"
  | "referencedContainerUnnamed";

/** Where one mandatory reattachment has got to. */
export type DependencyMemberState =
  | "pending"
  | "planCreated"
  | "acquired"
  | "executing"
  | "verified"
  | "blocked"
  | "failed"
  | "interrupted";

/**
 * What HarborMaster observed. Evidence, never identity.
 *
 * A container id changes on every recreation, which is the event this whole
 * subsystem exists to survive — so relationships are keyed on NAMES and these
 * ids exist only so a relationship can explain itself in an advanced view.
 */
export interface DependencyEvidence {
  dependentContainerId?: string;
  dependencyContainerId?: string;
  observedAt?: string;
}

/** One directed relationship: the dependent needs the dependency first. */
export interface WorkloadDependency {
  /** Present for CONFIGURED relationships only; detected ones have no row. */
  dependencyId?: string;
  dependent: string;
  dependency: string;
  source: DependencySource;
  /** The relationship kind, already in operator-facing words. */
  kind: string;
  /** Who established it, already in operator-facing words. */
  origin: string;
  /** True when the Docker runtime itself requires the relationship. */
  hard: boolean;
  /** HarborMaster's own sentence explaining the relationship. */
  why: string;
  /** False for every detected relationship. */
  deletable: boolean;
  evidence?: DependencyEvidence;
  createdBy?: { userId?: string; username?: string };
}

/**
 * A namespace share HarborMaster could not resolve.
 *
 * Read TOGETHER with the relationship list. A container appearing here is
 * BLOCKED whatever the edges say about it — "HarborMaster cannot establish what
 * this depends on" is not the same answer as "this depends on nothing", and
 * showing the second when the first is true is the one mistake this UI must not
 * make.
 */
export interface DependencyProblem {
  container: string;
  source?: DependencySource;
  referencedId?: string;
  refusal: DiscoveryRefusal;
}

/** One mandatory reattachment within a coordinated provider update. */
export interface DependencyMember {
  operationId: string;
  dependent: string;
  provider: string;
  source: DependencySource;
  expectedProviderId?: string;
  targetProviderId?: string;
  planId?: string;
  acquisitionId?: string;
  executionId?: string;
  state: DependencyMemberState;
  refusal?: string;
  createdAt?: string;
  updatedAt?: string;
}

/** Where one coordinated provider update has got to. */
export type DependencyOperationState =
  | "queued"
  | "providerRunning"
  | "providerVerified"
  | "rebindPending"
  | "rebindRunning"
  | "succeeded"
  | "failed"
  | "blocked"
  | "interrupted";

/** Why a coordinated provider update ended without succeeding. */
export type DependencyOperationFailure =
  | "providerFailed"
  | "rebindFailed"
  | "dependentNotRebindable"
  | "evidenceUnavailable";

/** One coordinated provider update. */
export interface DependencyOperation {
  operationId: string;
  provider: string;
  providerPlanId?: string;
  providerExecutionId?: string;
  state: DependencyOperationState;
  failure?: DependencyOperationFailure;
  requestedBy?: { userId?: string; username?: string };
  createdAt: string;
  updatedAt: string;
  completedAt?: string;
  members?: DependencyMember[];
}

/**
 * One operation with the answers derived from the execution records.
 *
 * `complete` is never read from a stored flag: a flag is a claim about the
 * past, an execution record is the evidence.
 */
export interface DependencyOperationSummary {
  operation: DependencyOperation;
  providerVerified: boolean;
  complete: boolean;
  needsAttention: boolean;
}

export interface DependencyOperationListing {
  items: DependencyOperationSummary[];
  total: number;
  /** The cap the listing was bounded by. Stated so a caller can tell a quiet
   * host from a truncated answer. */
  limit: number;
}

/**
 * The estate's dependency picture as counts.
 *
 * Only what nothing resolves by itself. There is deliberately no "waiting"
 * count: waiting is the system working, it clears without anybody, and
 * reporting it would teach an operator that this list contains things which do
 * not need them.
 */
export interface DependencySummary {
  cycles: number;
  unresolved: number;
  rebindsFailed: number;
  /** Reported for completeness. Work in flight is not attention. */
  rebindsPending: number;
}

/** Every relationship in force. */
export interface DependencyListing {
  items: WorkloadDependency[];
  total: number;
  problems?: DependencyProblem[];
  unresolved?: string[];
  summary?: DependencySummary;
}

/** One container's relationships, in both directions. */
export interface ContainerDependencies {
  container: string;
  /** What this container needs stable first. */
  dependsOn: WorkloadDependency[];
  /** What needs THIS container stable first. */
  dependedOnBy: WorkloadDependency[];
  /** Position in the update order. Absent means it cannot be ordered. */
  stage?: number | null;
  state: DependencyState;
  detail: string;
  problems?: DependencyProblem[];
  outstandingRebinds?: DependencyMember[];
}

/** The deterministic update order for the estate. */
export interface DependencyGraph {
  /** Stage 0 may be updated first; each later stage depends on earlier ones. */
  stages: string[][];
  /** Loops that must be broken, each a closed path. */
  cycles?: string[][];
  /** Each loop rendered as "a -> b -> c -> a". */
  cycleDescriptions?: string[];
  /** Edge endpoints the estate does not contain. */
  unresolved?: string[];
  /** Containers that could not be ordered, and why. */
  blocked?: Record<string, string>;
  edges: WorkloadDependency[];
}

/** The whole of what a caller may say when recording an ordering. */
export interface DependencyCreateRequest {
  dependent: string;
  dependency: string;
}

/**
 * The operator-facing name for each relationship kind.
 *
 * Rendered from the source rather than trusting the server's `kind` string, so
 * the UI has a total mapping even if a future server sends a source this build
 * does not know.
 */
export function dependencyKindLabel(source: DependencySource): string {
  switch (source) {
    case "dockerNetworkNamespace":
      return "Network namespace";
    case "dockerIPCNamespace":
      return "IPC namespace";
    case "dockerPIDNamespace":
      return "PID namespace";
    case "operator":
      return "Application ordering";
    default:
      return "Unrecognised relationship";
  }
}

/** Who established the relationship, in operator-facing words. */
export function dependencyOriginLabel(source: DependencySource): string {
  switch (source) {
    case "dockerNetworkNamespace":
    case "dockerIPCNamespace":
    case "dockerPIDNamespace":
      return "Detected by HarborMaster";
    case "operator":
      return "Configured by you";
    default:
      return "Unrecognised";
  }
}

/**
 * The short explanation of a hard namespace relationship.
 *
 * Written for an operator who has never heard of a network namespace. It says
 * what will happen and why, and stops short of Docker internals — the longer
 * version belongs in the Wiki.
 */
export function namespaceExplanation(
  source: DependencySource,
  dependent: string,
  provider: string,
): string | undefined {
  if (source === "operator") return undefined;
  const kind = dependencyKindLabel(source).toLowerCase();
  return (
    `${dependent} shares ${provider}'s ${kind}. If ${provider} is recreated, ` +
    `Docker gives the replacement a new identity, so ${dependent} must be ` +
    `recreated on the image it is already running to attach to it.`
  );
}

/**
 * Plain-English meaning of an ordering, shown before it is saved.
 *
 * Deliberately says "will wait" rather than "will not be updated": waiting is
 * what actually happens, and it resolves itself.
 */
export function describeOrdering(dependent: string, dependency: string): string {
  return (
    `Update ${dependency} before ${dependent}. ` +
    `If ${dependency} cannot be safely updated or verified, ${dependent} will wait.`
  );
}

/** Operator-facing wording for a dependency verdict. */
export function dependencyStateLabel(state: DependencyState): string {
  switch (state) {
    case "dependencySatisfied":
      return "Dependencies satisfied";
    case "dependencyWaiting":
      return "Waiting for dependency";
    case "dependencyBlocked":
      return "Blocked by dependency";
    case "dependencyCycle":
      return "No safe update order";
    case "dependencyMissing":
      return "Dependency unavailable";
    case "dependencyIneligible":
      return "Dependency not permitted to update";
    default:
      return "Dependency state unknown";
  }
}

/**
 * Why a namespace share could not be resolved, in operator-facing words.
 *
 * `namespacesUnobserved` deliberately does NOT read as "no dependencies". A
 * container HarborMaster has not looked at is not a container with nothing to
 * wait for.
 */
export function discoveryRefusalLabel(refusal: DiscoveryRefusal): string {
  switch (refusal) {
    case "namespacesUnobserved":
      return "HarborMaster has not yet read this container's namespace configuration.";
    case "referenceNotParseable":
      return "This container shares another container's namespace, but the reference cannot be resolved.";
    case "referencedContainerUnknown":
      return "This container is attached to a namespace whose container is no longer present. It needs reattaching.";
    case "referencedContainerUnnamed":
      return "This container shares the namespace of a container HarborMaster cannot identify.";
    default:
      return "HarborMaster could not establish what this container depends on.";
  }
}

/**
 * Why one container waits for another, in one short sentence.
 *
 * Written for the ORDER PREVIEW, where the reader is scanning a list rather
 * than studying one relationship, so it is deliberately shorter than
 * `namespaceExplanation` — it answers "why is this in stage 3" and stops.
 */
export function describeWait(edge: WorkloadDependency): string {
  if (edge.source === "operator") {
    return (
      `${edge.dependent} waits for ${edge.dependency} because you configured ` +
      `that ordering.`
    );
  }
  return (
    `${edge.dependent} waits for ${edge.dependency} because it shares ` +
    `${edge.dependency}'s ${dependencyKindLabel(edge.source).toLowerCase()}.`
  );
}

/**
 * The containers that appear in at least one reported loop.
 *
 * A cycle arrives as a CLOSED path — `["api","worker","postgres","api"]` — so
 * the last element repeats the first and is dropped. Membership is read from
 * this and from nothing else: the backend marks containers BEHIND a cycle with
 * the same state, and telling an operator to remove a relationship that is not
 * in the loop would send them to change the wrong thing.
 */
export function cycleMembers(graph: DependencyGraph): Set<string> {
  const members = new Set<string>();
  for (const cycle of graph.cycles ?? []) {
    // The closed path repeats its first element; a Set makes the duplicate
    // harmless, so this needs no special case for a one-container self-loop.
    for (const name of cycle) members.add(name);
  }
  return members;
}

/** How a container that has no stage came to have none. */
export interface BlockedContainer {
  container: string;
  /** HarborMaster's own sentence for the state, from the server. */
  detail: string;
  /** True when this container is itself one of the containers in a loop. */
  inCycle: boolean;
  /** True when it is not in a loop but its dependency chain reaches one. */
  behindCycle: boolean;
}

/**
 * Sorts the un-orderable containers into the three things that can be wrong.
 *
 * # Why "in the loop" and "behind the loop" must not share a sentence
 *
 * The backend gives both the `dependencyCycle` state, and correctly: neither can
 * be ordered and both need a person. But the ACTION differs. A container in the
 * loop is part of what has to be broken; a container behind it is a bystander
 * that recovers on its own the moment the loop is gone. Telling an operator to
 * remove a relationship on a bystander sends them to edit something that is not
 * the problem.
 *
 * Reachability is computed once over the REVERSE edges — from the loop outward
 * to everything that depends on it — so this is O(V + E) over a graph the
 * backend has already bounded at 2,000 nodes and 8,000 edges, not a walk per
 * blocked container.
 */
export function classifyBlocked(graph: DependencyGraph): BlockedContainer[] {
  const blocked = graph.blocked ?? {};
  const names = Object.keys(blocked).sort();
  if (names.length === 0) return [];

  const inCycle = cycleMembers(graph);

  // Everything that can reach a loop by following its own dependencies. Walked
  // outward from the loop over the reverse edges, so each container is visited
  // once whatever its distance from the loop.
  const dependentsOf = new Map<string, string[]>();
  for (const edge of graph.edges) {
    const existing = dependentsOf.get(edge.dependency);
    if (existing) existing.push(edge.dependent);
    else dependentsOf.set(edge.dependency, [edge.dependent]);
  }

  const reaches = new Set<string>(inCycle);
  const queue = [...inCycle];
  while (queue.length > 0) {
    const name = queue.shift() as string;
    for (const dependent of dependentsOf.get(name) ?? []) {
      if (reaches.has(dependent)) continue;
      reaches.add(dependent);
      queue.push(dependent);
    }
  }

  return names.map((container) => ({
    container,
    detail: blocked[container] ?? "",
    inCycle: inCycle.has(container),
    behindCycle: !inCycle.has(container) && reaches.has(container),
  }));
}

/**
 * Operator-facing wording for a coordinated update's state.
 *
 * `providerVerified` deliberately does not read as an ending. At that moment
 * every dependent is attached to a namespace that no longer exists, which is
 * the most fragile point in the whole operation.
 */
export function operationStateLabel(state: DependencyOperationState): string {
  switch (state) {
    case "queued":
      return "Queued";
    case "providerRunning":
      return "Replacing the provider";
    case "providerVerified":
      return "Provider verified, reattachments outstanding";
    case "rebindPending":
      return "Reattachments outstanding";
    case "rebindRunning":
      return "Reattaching";
    case "succeeded":
      return "Complete";
    case "failed":
      return "Did not complete";
    case "blocked":
      return "Could not proceed safely";
    case "interrupted":
      return "Interrupted by a restart";
    default:
      return "State not established";
  }
}

/** Why a coordinated update ended without succeeding, in operator words. */
export function operationFailureLabel(
  failure: DependencyOperationFailure,
): string {
  switch (failure) {
    case "providerFailed":
      return "The provider's own update did not succeed, so nothing was reattached and nothing needed to be.";
    case "rebindFailed":
      return "At least one container could not be reattached to the provider's replacement.";
    case "dependentNotRebindable":
      return "A container stopped being safely recreatable after the operation began.";
    case "evidenceUnavailable":
      return "HarborMaster could not re-establish which containers had to be reattached.";
    default:
      return "HarborMaster did not establish why this did not complete.";
  }
}

/** Operator-facing wording for a reattachment's progress. */
export function memberStateLabel(state: DependencyMemberState): string {
  switch (state) {
    case "pending":
      return "Waiting to be reattached";
    case "planCreated":
      return "Reattachment planned";
    case "acquired":
      return "Image verified";
    case "executing":
      return "Reattaching";
    case "verified":
      return "Reattached";
    case "blocked":
      return "Cannot be reattached";
    case "failed":
      return "Reattachment failed";
    case "interrupted":
      return "Interrupted by a restart";
    default:
      return "Unknown";
  }
}
