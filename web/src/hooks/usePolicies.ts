import { useCallback } from "react";

import {
  archivePolicy,
  createPolicy,
  evaluatePolicies,
  getContainerPolicy,
  getPolicyRules,
  getPolicySummary,
  listPolicies,
  listPolicyViolations,
  updatePolicy,
  updateViolationStatus,
} from "../api/client";
import type { ListResponse } from "../api/inventoryTypes";
import type {
  ContainerPolicy,
  PolicyDefinition,
  PolicyOperatorStatus,
  PolicyQuery,
  PolicyRequest,
  PolicyRuleCatalogue,
  PolicySummary,
  PolicyViolation,
  PolicyViolationQuery,
} from "../api/policyTypes";
import { useApiResource, type ResourceState } from "./useApiResource";

/**
 * Policy resources.
 *
 * Read hooks plus the write helpers. Every write acts on HarborMaster's own
 * rows: a policy definition, a violation's status, or a request to re-run a
 * pass. There is deliberately no hook that enforces, remediates, or applies a
 * policy to Docker — HarborMaster has no such capability and the API exposes no
 * such endpoint.
 */

/** The policy list, filtered and paged server-side. */
export function usePolicies(
  query: PolicyQuery,
): ResourceState<ListResponse<PolicyDefinition>> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => listPolicies(query, { signal }),
    // The query object is rebuilt each render, so the key below is what
    // actually drives refetching.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(query)],
  );

  return useApiResource<ListResponse<PolicyDefinition>>(fetcher, {
    key: JSON.stringify(query),
  });
}

/**
 * The rule catalogue the editor is built from.
 *
 * Fetched once and never polled: the catalogue changes only when HarborMaster
 * itself changes.
 */
export function usePolicyRules(): ResourceState<PolicyRuleCatalogue> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getPolicyRules({ signal }),
    [],
  );
  return useApiResource<PolicyRuleCatalogue>(fetcher, { key: "policy-rules" });
}

/** The compliance aggregate. */
export function usePolicySummary(): ResourceState<PolicySummary> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getPolicySummary({ signal }),
    [],
  );
  return useApiResource<PolicySummary>(fetcher, { key: "policy-summary" });
}

/** The violation list, filtered and paged server-side. */
export function usePolicyViolations(
  query: PolicyViolationQuery,
): ResourceState<ListResponse<PolicyViolation>> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => listPolicyViolations(query, { signal }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(query)],
  );

  return useApiResource<ListResponse<PolicyViolation>>(fetcher, {
    key: JSON.stringify(query),
  });
}

/**
 * One container's violations, with the pass that produced them.
 *
 * The evaluation is what lets the UI distinguish "compliant" from "never
 * checked" — rendering an empty list as a clean bill of health for a container
 * no pass has ever reached would be the worst thing this view could do.
 */
export function useContainerPolicy(
  containerId: string,
  query: PolicyViolationQuery = {},
): ResourceState<ContainerPolicy> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) =>
      getContainerPolicy(containerId, query, { signal }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [containerId, JSON.stringify(query)],
  );

  return useApiResource<ContainerPolicy>(fetcher, {
    key: `${containerId}:${JSON.stringify(query)}`,
  });
}

/** Stores a new policy. */
export function savePolicy(body: PolicyRequest): Promise<PolicyDefinition> {
  return createPolicy(body);
}

/** Applies a partial update. Omitted fields are left alone. */
export function editPolicy(
  policyId: string,
  body: PolicyRequest,
): Promise<PolicyDefinition> {
  return updatePolicy(policyId, body);
}

/**
 * Withdraws a policy.
 *
 * Named for what it does. `DELETE /policies/{id}` ARCHIVES: the definition is
 * kept, its open violations resolve, and the history of what it caught remains.
 * Calling this `deletePolicy` would describe the verb rather than the effect.
 */
export function withdrawPolicy(policyId: string): Promise<void> {
  return archivePolicy(policyId);
}

/**
 * Moves a violation's status.
 *
 * Typed to PolicyOperatorStatus rather than PolicyViolationStatus: `active` and
 * `resolved` are engine-owned and the server rejects them, so a caller that
 * tries finds out at compile time rather than from a 400.
 */
export function setViolationStatus(
  id: number,
  status: PolicyOperatorStatus,
  note?: string,
): Promise<PolicyViolation> {
  return updateViolationStatus(id, status, note);
}

/**
 * Requests a compliance pass.
 *
 * Resolves once the request is ACCEPTED, not once the pass finishes: the server
 * schedules it and answers 202. Callers refresh the summary afterwards rather
 * than expecting the result inline.
 */
export function requestPolicyEvaluation(): Promise<unknown> {
  return evaluatePolicies();
}
