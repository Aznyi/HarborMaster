import { useCallback } from "react";

import {
  approveAutomationDecision,
  listAutomationApprovals,
  archiveUpdatePolicy,
  createUpdatePolicy,
  getAutomationRun,
  getAutomationStatus,
  getContainerBehaviorSummary,
  getSimpleUpdates,
  getAutomationUpcoming,
  listAutomationPauses,
  listAutomationRuns,
  listUpdatePolicies,
  pauseAutomation,
  resumeAutomation,
  runAutomationPass,
  updateUpdatePolicy,
} from "../api/client";
import type { ContainerBehaviorSummary } from "../api/inventoryTypes";
import type {
  SimpleUpdatesState,
  AutomationDecision,
  AutomationApprovalListResponse,
  AutomationPauseListResponse,
  AutomationRunDetailResponse,
  AutomationRunListResponse,
  AutomationRunQuery,
  AutomationStatusResponse,
  AutomationUpcomingResponse,
  PausedContainer,
  UpdatePolicyListResponse,
  UpdatePolicyQuery,
  UpdatePolicyRequest,
  UpdatePolicyResult,
} from "../api/automationTypes";
import { useApiResource, type ResourceState } from "./useApiResource";

/**
 * Automation resources.
 *
 * # What can be reached from here, and what cannot
 *
 * Five reads and six writes. Every write either administers a RULE or issues an
 * instruction that names one of HarborMaster's own records:
 *
 *   - `runPass` takes a boolean.
 *   - `approve` takes a run id and a container name that must match a decision
 *     the server already held.
 *   - `pause` and `resume` take a container name the inventory already knows.
 *
 * There is deliberately no hook that updates a named container, that names an
 * image, or that forces a decision the engine declined. A container an operator
 * wants updated now goes through the change plan and the recreate action, which
 * are their own, separately-permissioned paths.
 *
 * # Polling
 *
 * The status is polled while a pass is running and left alone otherwise. A pass
 * is minutes apart by default, so a settled estate costs nothing to leave open
 * on screen.
 */

/**
 * How often the engine status is re-read while a pass is running.
 *
 * Slower than a recreation's poll: a decision pass is bounded in seconds and
 * the interesting part is the counters settling, not a container being down.
 */
const ACTIVE_POLL_MS = 3000;

/** The engine's current state, with the history aggregate. */
export function useAutomationStatus(
  options: { poll?: boolean } = {},
): ResourceState<AutomationStatusResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getAutomationStatus({ signal }),
    [],
  );

  return useApiResource<AutomationStatusResponse>(fetcher, {
    key: "automation-status",
    ...(options.poll ? { pollIntervalMs: ACTIVE_POLL_MS } : {}),
  });
}

/**
 * What the next pass would do, without doing any of it.
 *
 * A read. It writes no run, no decisions, and reaches no service — which is the
 * difference between this and a dry run, and the reason it is safe to leave on
 * a page an operator refreshes.
 */
export function useAutomationUpcoming(): ResourceState<AutomationUpcomingResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getAutomationUpcoming({ signal }),
    [],
  );

  return useApiResource<AutomationUpcomingResponse>(fetcher, {
    key: "automation-upcoming",
  });
}

/** The pass history, filtered and paged server-side. */
export function useAutomationRuns(
  query: AutomationRunQuery,
): ResourceState<AutomationRunListResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => listAutomationRuns(query, { signal }),
    // The query object is rebuilt each render, so the key below is what
    // actually drives refetching.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(query)],
  );

  return useApiResource<AutomationRunListResponse>(fetcher, {
    key: JSON.stringify(query),
  });
}

/** One pass with every decision it made, in the order it made them. */
export function useAutomationRun(
  runId: string,
): ResourceState<AutomationRunDetailResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getAutomationRun(runId, { signal }),
    [runId],
  );

  return useApiResource<AutomationRunDetailResponse>(fetcher, { key: runId });
}

/**
 * The decisions waiting for a person.
 *
 * Its own query rather than a filter over the run history: an operator asking
 * "what needs me" should not have to know which pass produced the answer, and
 * the previous route to it was reading an archived pass table row by row.
 */
export function useAutomationApprovals(
  page = 1,
  pageSize = 25,
  container?: string,
): ResourceState<AutomationApprovalListResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) =>
      listAutomationApprovals({ signal, page, pageSize, container }),
    [page, pageSize, container],
  );

  return useApiResource<AutomationApprovalListResponse>(fetcher, {
    key: `approvals-${page}-${pageSize}-${container ?? ""}`,
  });
}

/** The containers automation will not touch. */
export function useAutomationPauses(
  all = false,
): ResourceState<AutomationPauseListResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => listAutomationPauses({ signal, all }),
    [all],
  );

  return useApiResource<AutomationPauseListResponse>(fetcher, {
    key: `pauses-${all}`,
  });
}

/** The automation rules. */
/**
 * The automatic-updates switch.
 *
 * A plain read. Not polled: the switch changes only when somebody flips it, and
 * this page already polls the engine's status for the things that move on their
 * own.
 */
export function useSimpleUpdates(): ResourceState<SimpleUpdatesState> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getSimpleUpdates({ signal }),
    [],
  );
  return useApiResource<SimpleUpdatesState>(fetcher, { key: "simple-updates" });
}

/**
 * Which containers carry a saved update behaviour (C2.2).
 *
 * ONE request, whatever the number of preferences. The workspace shows the SET
 * of containers an operator has given an explicit behaviour; asking what each
 * would actually do is a per-container engine evaluation, and belongs on the
 * container's own page where exactly one is asked for.
 */
export function useContainerBehaviorSummary(): ResourceState<ContainerBehaviorSummary> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getContainerBehaviorSummary({ signal }),
    [],
  );
  return useApiResource<ContainerBehaviorSummary>(fetcher, {
    key: "container-behavior-summary",
  });
}

export function useUpdatePolicies(
  query: UpdatePolicyQuery,
): ResourceState<UpdatePolicyListResponse> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => listUpdatePolicies(query, { signal }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(query)],
  );

  return useApiResource<UpdatePolicyListResponse>(fetcher, {
    key: JSON.stringify(query),
  });
}

// ----------------------------------------------------------------- writes --

/**
 * Runs a decision pass now.
 *
 * Resolves once the pass has DECIDED, not once the work it submitted finishes.
 * The caller refreshes afterwards rather than expecting the outcome inline.
 */
export function useRunAutomationPass(): (
  dryRun: boolean,
) => Promise<AutomationRunDetailResponse> {
  return useCallback((dryRun: boolean) => runAutomationPass(dryRun), []);
}

/**
 * Releases a decision a policy held for a person.
 *
 * **This causes a container to be stopped and replaced.** The two arguments
 * SELECT a decision the server already made; there is no way to describe a
 * different one.
 */
export function useApproveAutomationDecision(): (
  runId: string,
  containerName: string,
) => Promise<AutomationDecision> {
  return useCallback(
    (runId: string, containerName: string) =>
      approveAutomationDecision(runId, containerName),
    [],
  );
}

/** Stops automation for one container. Changes nothing on the host. */
export function usePauseAutomation(): (
  containerName: string,
  reason: string,
) => Promise<PausedContainer> {
  return useCallback(
    (containerName: string, reason: string) =>
      pauseAutomation(containerName, reason),
    [],
  );
}

/** Clears a pause, recording who cleared it. */
export function useResumeAutomation(): (containerName: string) => Promise<void> {
  return useCallback((containerName: string) => resumeAutomation(containerName), []);
}

/** Creates an automation rule. Needs `automation:manage`. */
export function useCreateUpdatePolicy(): (
  body: UpdatePolicyRequest,
) => Promise<UpdatePolicyResult> {
  return useCallback((body: UpdatePolicyRequest) => createUpdatePolicy(body), []);
}

/** Edits an automation rule. Needs `automation:manage`. */
export function useUpdateUpdatePolicy(): (
  policyId: string,
  body: UpdatePolicyRequest,
) => Promise<UpdatePolicyResult> {
  return useCallback(
    (policyId: string, body: UpdatePolicyRequest) =>
      updateUpdatePolicy(policyId, body),
    [],
  );
}

/**
 * Withdraws an automation rule.
 *
 * Archives rather than deletes: the record of what the rule caused outlives it.
 */
export function useArchiveUpdatePolicy(): (policyId: string) => Promise<void> {
  return useCallback((policyId: string) => archiveUpdatePolicy(policyId), []);
}
