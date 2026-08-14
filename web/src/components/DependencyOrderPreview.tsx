import { useMemo } from "react";

import type { DependencyGraph, WorkloadDependency } from "../api/dependencyTypes";
import {
  classifyBlocked,
  dependencyKindLabel,
  dependencyOriginLabel,
  describeWait,
} from "../api/dependencyTypes";

/**
 * The order HarborMaster would update in, and the loops that stop it.
 *
 * # This is an ORDER, not a work list
 *
 * A container appears here because something constrains when it may change, not
 * because it has an update waiting. Reading it as "the containers HarborMaster
 * will update now" would be reading a schedule into a diagram, so the heading,
 * the description, and the empty state all say ordering and never say now. There
 * is deliberately no update availability on this view: the data behind it is the
 * dependency graph, which knows nothing about images.
 *
 * # Two ways of being un-orderable, and they need different actions
 *
 * A container inside a loop is part of what must be broken. A container BEHIND a
 * loop is a bystander that recovers by itself once the loop is gone. The backend
 * gives both the same state — correctly, since neither can be ordered — so the
 * separation happens here, in `classifyBlocked`, and each gets its own sentence.
 */
export function DependencyOrderPreview({
  graph,
  mayManage = false,
  onRemove,
}: {
  graph: DependencyGraph;
  /** True for an administrator, who may remove a configured relationship. */
  mayManage?: boolean;
  onRemove?: (edge: WorkloadDependency) => void;
}) {
  const blocked = useMemo(() => classifyBlocked(graph), [graph]);
  const behind = blocked.filter((entry) => entry.behindCycle);
  const otherwiseBlocked = blocked.filter(
    (entry) => !entry.inCycle && !entry.behindCycle,
  );

  const cycles = graph.cycles ?? [];
  const stages = graph.stages ?? [];

  // Every relationship a container waits on, so a stage entry can say why it is
  // not in stage 1. Built once rather than filtered per container.
  const waits = useMemo(() => {
    const index = new Map<string, WorkloadDependency[]>();
    for (const edge of graph.edges) {
      const existing = index.get(edge.dependent);
      if (existing) existing.push(edge);
      else index.set(edge.dependent, [edge]);
    }
    return index;
  }, [graph.edges]);

  return (
    <section aria-labelledby="dependency-order-heading" className="space-y-4">
      <div>
        <h2 id="dependency-order-heading" className="text-base font-semibold">
          Dependency order
        </h2>
        <p className="mt-1 max-w-prose text-sm text-content-muted">
          The order these relationships imply. A container listed here is not
          necessarily due an update — this is when it <em>could</em> change
          relative to the others, not what HarborMaster is about to do.
        </p>
      </div>

      {cycles.length > 0 ? (
        <CycleReport
          cycles={cycles}
          edges={graph.edges}
          behind={behind}
          mayManage={mayManage}
          onRemove={onRemove}
        />
      ) : null}

      {otherwiseBlocked.length > 0 ? (
        <section
          aria-label="Containers that cannot be ordered"
          className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-3"
        >
          <h3 className="text-sm font-semibold text-warn">
            HarborMaster cannot place these in the order
          </h3>
          <ul className="mt-2 space-y-1 text-sm">
            {otherwiseBlocked.map((entry) => (
              <li key={entry.container} className="break-words">
                <span className="font-medium">{entry.container}</span>
                {entry.detail ? <> — {entry.detail}</> : null}
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      {stages.length === 0 ? (
        <p className="text-sm text-content-muted">
          {cycles.length > 0 || otherwiseBlocked.length > 0
            ? "No container can be placed in an order until the problems above are resolved."
            : "No ordering constraints exist, so HarborMaster can evaluate every workload independently."}
        </p>
      ) : (
        <ol className="space-y-3" aria-label="Update order by stage">
          {stages.map((stage, index) => (
            <li
              key={`stage-${index}`}
              className="rounded-lg border border-border-subtle bg-surface px-3 py-3"
            >
              <h3 className="text-sm font-semibold">
                {/* One-based for the reader. The API is zero-based and stays
                    that way; only the label counts from one. */}
                Stage {index + 1}
                <span className="ml-2 text-xs font-normal text-content-muted">
                  {index === 0
                    ? "nothing has to happen first"
                    : "after every earlier stage is verified"}
                </span>
              </h3>
              <ul className="mt-2 space-y-2">
                {stage.map((name) => (
                  <li key={name} className="text-sm">
                    <span className="break-all font-medium">{name}</span>
                    {(waits.get(name) ?? []).length > 0 ? (
                      <ul className="mt-1 space-y-0.5">
                        {(waits.get(name) ?? []).map((edge) => (
                          <li
                            key={`${edge.dependency}:${edge.source}`}
                            className="break-words text-xs text-content-muted"
                          >
                            {describeWait(edge)}
                          </li>
                        ))}
                      </ul>
                    ) : null}
                  </li>
                ))}
              </ul>
            </li>
          ))}
        </ol>
      )}
    </section>
  );
}

/**
 * The loops, and what an operator can actually do about each one.
 *
 * # Whether it is removable is the whole point
 *
 * A loop made entirely of detected namespace relationships is not something
 * HarborMaster can fix — there is no stored row to delete, and the Docker
 * configuration on the host has to change. A loop containing a configured
 * ordering has an obvious lever, and an administrator is shown it. Saying
 * "dependency cycle" and stopping would leave both operators with the same
 * non-answer.
 */
function CycleReport({
  cycles,
  edges,
  behind,
  mayManage,
  onRemove,
}: {
  cycles: string[][];
  edges: WorkloadDependency[];
  behind: { container: string }[];
  mayManage: boolean;
  onRemove?: (edge: WorkloadDependency) => void;
}) {
  return (
    // The alert wraps the whole report rather than each loop: the loops are read
    // together, and one live region per loop would announce the same problem
    // several times over.
    <section
      role="alert"
      aria-labelledby="dependency-cycle-heading"
      className="space-y-4 rounded-lg border border-danger/40 bg-danger-soft px-3 py-3"
    >
      <div>
        <h3 id="dependency-cycle-heading" className="text-sm font-semibold text-danger">
          HarborMaster cannot determine a safe update order for these workloads
        </h3>
        <p className="mt-1 max-w-prose text-sm">
          These containers depend on each other in a loop, so there is no order
          in which each one&apos;s dependencies are ready first. HarborMaster
          will not update any of them until the loop is broken.
        </p>
      </div>

      {cycles.map((cycle, index) => (
        <OneCycle
          key={`cycle-${index}`}
          cycle={cycle}
          index={index}
          total={cycles.length}
          edges={edges}
          mayManage={mayManage}
          onRemove={onRemove}
        />
      ))}

      {behind.length > 0 ? (
        <section aria-label="Containers blocked behind the loop">
          <h4 className="text-sm font-semibold">Also held up by this</h4>
          <ul className="mt-1 space-y-1 text-sm">
            {behind.map((entry) => (
              <li key={entry.container} className="break-words">
                {entry.container} cannot update because its dependency chain
                reaches the loop above. Breaking the loop releases it; there is
                nothing to change on {entry.container} itself.
              </li>
            ))}
          </ul>
        </section>
      ) : null}
    </section>
  );
}

/** One loop: the path round it, then the relationships that form it. */
function OneCycle({
  cycle,
  index,
  total,
  edges,
  mayManage,
  onRemove,
}: {
  cycle: string[];
  index: number;
  total: number;
  edges: WorkloadDependency[];
  mayManage: boolean;
  onRemove?: (edge: WorkloadDependency) => void;
}) {
  // The relationships that make up this loop, in the order it runs. A pair can
  // be related by more than one source at once — a namespace share AND a
  // configured ordering — and every one of them is a link that has to go, so all
  // matches are listed rather than the first.
  const links = useMemo(() => {
    const out: WorkloadDependency[] = [];
    for (let step = 0; step < cycle.length - 1; step += 1) {
      const dependent = cycle[step];
      const dependency = cycle[step + 1];
      for (const edge of edges) {
        if (edge.dependent === dependent && edge.dependency === dependency) {
          out.push(edge);
        }
      }
    }
    return out;
  }, [cycle, edges]);

  const removable = links.filter((edge) => edge.deletable);
  const path = cycle.slice(0, -1);

  return (
    <div className="rounded-lg border border-border-subtle bg-surface px-3 py-3">
      <h4 className="text-sm font-semibold">
        {total > 1 ? `Loop ${index + 1}` : "The loop"}
      </h4>

      {/* The path, as a list rather than as arrows. The arrow is decorative and
          hidden; every step also says "depends on" in words, so the shape
          survives a screen reader and a stylesheet that did not load. */}
      <ol className="mt-2 space-y-1 text-sm" aria-label="The path round the loop">
        {path.map((name, step) => {
          const next = path[(step + 1) % path.length];
          return (
            <li key={name} className="break-words">
              <span className="font-medium">{name}</span>
              <span aria-hidden="true" className="mx-1 text-content-muted">
                ↓
              </span>
              <span className="text-content-muted">
                depends on {next}
                {step === path.length - 1 ? ", which closes the loop" : ""}
              </span>
            </li>
          );
        })}
      </ol>

      <h5 className="mt-3 text-xs font-semibold uppercase tracking-wide text-content-muted">
        The relationships in this loop
      </h5>
      <ul className="mt-1 space-y-2">
        {links.map((edge) => (
          <li
            key={`${edge.dependent}->${edge.dependency}:${edge.source}`}
            className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm"
          >
            <span className="break-all">
              {edge.dependent} depends on {edge.dependency}
            </span>
            {/* Kind and origin as words. The difference between what Docker
                enforces and what a person asserted decides whether there is
                anything to remove, so it must not be carried by colour. */}
            <span className="text-xs text-content-muted">
              {dependencyKindLabel(edge.source)} ·{" "}
              {dependencyOriginLabel(edge.source)}
            </span>
            {edge.deletable && mayManage && onRemove ? (
              <button
                type="button"
                className="min-h-9 rounded-lg border border-danger/40 px-2.5 py-1 text-xs font-medium text-danger"
                onClick={() => onRemove(edge)}
                aria-label={`Remove this relationship between ${edge.dependent} and ${edge.dependency}`}
              >
                Remove this relationship
              </button>
            ) : null}
          </li>
        ))}
      </ul>

      {removable.length === 0 ? (
        <p className="mt-2 max-w-prose text-sm">
          Every relationship in this loop is one Docker itself enforces.
          HarborMaster derives them from the container configuration on every
          read and cannot remove them — the configuration on the host has to
          change, then HarborMaster will pick that up on its next refresh.
        </p>
      ) : (
        <p className="mt-2 max-w-prose text-sm">
          {removable.length === 1
            ? "One relationship in this loop was configured by an administrator and can be removed."
            : `${removable.length} relationships in this loop were configured by an administrator and can be removed.`}{" "}
          Removing one breaks the loop and lets HarborMaster order these
          containers again. It changes no container.
        </p>
      )}
    </div>
  );
}
