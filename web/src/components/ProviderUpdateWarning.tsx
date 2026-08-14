import type { ContainerDependencies, WorkloadDependency } from "../api/dependencyTypes";
import { dependencyKindLabel } from "../api/dependencyTypes";

/**
 * What updating a container will do to the containers that share its namespace.
 *
 * # The one thing an operator cannot see from Docker
 *
 * `network_mode: container:gluetun` binds a container to gluetun's CURRENT
 * container identity. Replace gluetun and the dependents are attached to a
 * namespace that no longer exists — and Docker reports nothing, the containers
 * keep running, and the network silently stops working.
 *
 * So before somebody confirms an update to a container in that position, this
 * says which containers are affected and what HarborMaster will do about them.
 * It is not a scare notice: the reattachment is the SAFE behaviour, and the
 * text says so.
 *
 * # What it is careful NOT to claim
 *
 * A configured ordering is not a rebind cascade. `api depends on postgres` is an
 * operator's assertion about their application; it constrains WHEN things
 * happen and creates no runtime binding at all. Updating postgres by hand does
 * not recreate api, and this component never says it does — configured
 * relationships are shown as ordering CONTEXT under their own heading, never in
 * the list of containers that will be recreated.
 */
export function ProviderUpdateWarning({
  container,
  dependencies,
  unavailable,
}: {
  container: string;
  /** The provider's own relationships, both directions. */
  dependencies: ContainerDependencies | undefined;
  /** True when HarborMaster could not answer at all. */
  unavailable?: boolean;
}) {
  if (unavailable) {
    return (
      <p
        role="status"
        className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-sm"
      >
        <span className="font-medium">
          HarborMaster could not establish what depends on {container}.
        </span>{" "}
        It refuses a recreation it cannot assess, so this is likely to be
        declined when you confirm it — and if it is not, nothing here should be
        read as a statement that no container depends on this one.
      </p>
    );
  }

  if (!dependencies) return null;

  // The containers Docker itself ties to this one. These are the only ones a
  // recreation can require, and the list is deduplicated by name because a pair
  // can share more than one namespace at once.
  const hard = dedupeByDependent(
    (dependencies.dependedOnBy ?? []).filter((edge) => edge.hard),
  );
  // Configured orderings. CONTEXT, never cascade.
  const ordering = (dependencies.dependedOnBy ?? []).filter(
    (edge) => !edge.hard,
  );

  if (hard.length === 0 && ordering.length === 0) return null;

  return (
    <section
      aria-label={`What depends on ${container}`}
      className="space-y-3 rounded-lg border border-warn/40 bg-warn-soft px-3 py-3 text-sm"
    >
      {hard.length > 0 ? (
        <>
          <h4 className="font-semibold">
            Updating {container} affects {hard.length} other{" "}
            {hard.length === 1 ? "container" : "containers"}
          </h4>

          <ul className="space-y-1">
            {hard.map((edge) => (
              <li key={`${edge.dependent}:${edge.source}`} className="break-all">
                <span className="font-medium">{edge.dependent}</span>{" "}
                <span className="text-content-muted">
                  {dependencyKindLabel(edge.source)}
                </span>
              </li>
            ))}
          </ul>

          <p className="max-w-prose">
            Docker ties {hard.length === 1 ? "this container" : "these containers"}{" "}
            to {container}&apos;s current container identity. If {container} is
            replaced, HarborMaster must safely recreate{" "}
            {hard.length === 1 ? "it" : "them"} so{" "}
            {hard.length === 1 ? "it" : "they"} can attach to the replacement.
          </p>

          {/* Each of these corresponds to something a reasonable operator
              might otherwise assume, and each of them is false. */}
          <ul className="list-disc space-y-1 pl-5 text-content-muted">
            <li>
              <strong>No image version changes.</strong> Each recreation uses
              the digest that container is already running.
            </li>
            <li>
              <strong>Every recreation takes the normal pipeline.</strong> The
              same preflight, verification, and refusal rules as an update you
              start yourself.
            </li>
            <li>
              <strong>Only containers Docker binds are touched.</strong> A
              configured ordering never causes a recreation.
            </li>
            <li>
              <strong>Nothing else on this host is affected.</strong>
            </li>
          </ul>
        </>
      ) : null}

      {ordering.length > 0 ? (
        <section aria-label={`Containers configured to wait for ${container}`}>
          <h4 className="font-semibold">
            Configured to wait for {container}
          </h4>
          <ul className="mt-1 space-y-1">
            {ordering.map((edge) => (
              <li key={`${edge.dependent}:ordering`} className="break-all">
                <span className="font-medium">{edge.dependent}</span>{" "}
                <span className="text-content-muted">Application ordering</span>
              </li>
            ))}
          </ul>
          <p className="mt-1 max-w-prose text-content-muted">
            Ordering only.{" "}
            {ordering.length === 1 ? "This container" : "These containers"} will
            not be recreated and{" "}
            {ordering.length === 1 ? "is" : "are"} not changed by this update —
            an ordering affects when an AUTOMATIC update happens, not what a
            manual one does.
          </p>
        </section>
      ) : null}
    </section>
  );
}

/**
 * One entry per dependent container.
 *
 * A pair can be related by a network namespace AND an IPC namespace at once,
 * which is one container to recreate and would otherwise be listed twice —
 * making an operator believe more of their estate is affected than is.
 */
function dedupeByDependent(edges: WorkloadDependency[]): WorkloadDependency[] {
  const seen = new Set<string>();
  const out: WorkloadDependency[] = [];
  for (const edge of edges) {
    if (seen.has(edge.dependent)) continue;
    seen.add(edge.dependent);
    out.push(edge);
  }
  return out;
}

/**
 * A refusal from the recreation preflight, in HarborMaster's own words.
 *
 * # Why the server's sentence is used verbatim
 *
 * Every refusal the execution service produces comes from a closed vocabulary
 * with a fixed explanation — no daemon text, no registry text, no interpolated
 * input. That sentence names WHICH check said no, which is the only part of the
 * response that tells an operator what to do differently. Replacing it with
 * "the update was refused" throws that away, and inventing a friendlier one
 * risks describing a check that did not run.
 */
export function ProviderUpdateRefusal({ message }: { message: string }) {
  return (
    <div
      role="alert"
      className="space-y-1 rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm"
    >
      <p className="font-semibold text-danger">Update blocked</p>
      {/* Rendered as text. The message is HarborMaster's own sentence, and it
          is still put through React's escaping rather than trusted. */}
      <p className="break-words">{message}</p>
      <p className="text-content-muted">
        Nothing was changed. HarborMaster refuses before it stops anything, so
        the container is exactly as it was.
      </p>
    </div>
  );
}
