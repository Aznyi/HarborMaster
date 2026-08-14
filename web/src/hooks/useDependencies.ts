import { useCallback } from "react";

import {
  createDependency,
  deleteDependency,
  getContainerDependencies,
  getDependencyGraph,
  listDependencies,
  listDependencyOperations,
} from "../api/client";
import type {
  ContainerDependencies,
  DependencyCreateRequest,
  DependencyGraph,
  DependencyListing,
  DependencyOperationListing,
  WorkloadDependency,
} from "../api/dependencyTypes";
import { useApiResource, type ResourceState } from "./useApiResource";

/**
 * Workload dependency resources.
 *
 * Three reads and two writes. The writes touch HarborMaster's own ordering
 * table and nothing else — there is deliberately no hook here that updates,
 * recreates, or reattaches a container, because the API exposes no such route.
 *
 * A relationship can only ever make HarborMaster wait or refuse. Recording one
 * is conservative; REMOVING one takes a constraint away, which is why both sit
 * behind an administrator-only permission and why the delete flow confirms.
 */

/** Every relationship in force, with the shares that could not be resolved. */
export function useDependencies(): ResourceState<DependencyListing> {
  return useApiResource(
    useCallback((options) => listDependencies(options), []),
  );
}

/**
 * One container's relationships, in both directions.
 *
 * Skipped entirely when no container is selected, so the detail view does not
 * issue a request before it knows what it is asking about.
 */
export function useContainerDependencies(
  containerId: string | undefined,
): ResourceState<ContainerDependencies> {
  return useApiResource(
    useCallback(
      (options) =>
        containerId
          ? getContainerDependencies(containerId, options)
          : Promise.resolve(undefined as unknown as ContainerDependencies),
      [containerId],
    ),
  );
}

/** The deterministic update order. A read-only projection. */
export function useDependencyGraph(): ResourceState<DependencyGraph> {
  return useApiResource(
    useCallback((options) => getDependencyGraph(options), []),
  );
}

/**
 * The coordinated provider updates, and where each reattachment got to.
 *
 * Polled while anything is still moving, because a reattachment is the phase
 * during which a dependent is attached to a namespace that no longer exists --
 * an operator watching this wants it to change under them.
 */
export function useDependencyOperations(
  options: { poll?: boolean } = {},
): ResourceState<DependencyOperationListing> {
  return useApiResource(
    useCallback((request) => listDependencyOperations(request), []),
    options.poll ? { pollIntervalMs: 5000 } : {},
  );
}

/**
 * The two writes.
 *
 * Returned as plain functions rather than wrapped in state, matching the other
 * admin editors: the caller owns the pending/error rendering because only it
 * knows which control the operator pressed.
 */
export function useDependencyAdmin() {
  const create = useCallback(
    (body: DependencyCreateRequest): Promise<WorkloadDependency> =>
      createDependency(body),
    [],
  );
  const remove = useCallback(
    (dependencyId: string): Promise<void> => deleteDependency(dependencyId),
    [],
  );
  return { create, remove };
}
