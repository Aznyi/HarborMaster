import { Link } from "react-router";

import { BEHAVIOR_LABELS } from "./ContainerUpdateBehavior";
import { StatusBadge } from "./StatusBadge";
import type { ContainerBehaviorSummary } from "../api/inventoryTypes";
import type { ResourceState } from "../hooks/useApiResource";

/**
 * Which containers have their own update behaviour (C2.2).
 *
 * # What this is
 *
 * The automation workspace answers "what is configured?" and, until now, could
 * not answer it. A policy is only half the configuration: an operator can give
 * one container its own behaviour on the container's page, and nothing on this
 * page said so. Somebody reading the policy list would conclude the policy
 * describes the estate, and for any container with an override it does not.
 *
 * # What it is NOT
 *
 * A second editor. The container's own page is where a behaviour is chosen, and
 * putting a control here would give the same state two owners — one of which
 * has no container in front of it to show the consequence. Every row here is a
 * link to that page.
 *
 * It is also not a claim about what will happen. A preference may only make
 * automation SAFER, so a policy can still hold a container listed here as
 * automatic. That is why the heading says "saved" and every count is described
 * as a choice rather than an outcome: answering the other question truthfully
 * costs one engine evaluation per container, which would turn opening this page
 * into evaluating the estate several times over.
 */
export function ContainerBehaviorOverrides({
  state,
}: {
  state: ResourceState<ContainerBehaviorSummary>;
}) {
  // A failed read establishes nothing. Claiming "no container has an override"
  // because the request failed would be the more dangerous of the two lies, so
  // the section says what it does not know.
  if (state.error) {
    return (
      <Section>
        <p className="text-sm text-content-muted">
          HarborMaster could not read which containers have their own update
          behaviour, so nothing is claimed about them here.
        </p>
      </Section>
    );
  }

  if (!state.data) {
    return (
      <Section>
        <p role="status" className="text-sm text-content-muted">
          Loading saved update behaviours…
        </p>
      </Section>
    );
  }

  const { items, counts, total, stale } = state.data;

  // The normal state for most installations, and not a finding. Said plainly
  // rather than shown as an empty table, which reads like something is missing.
  if (total === 0 && stale === 0) {
    return (
      <Section>
        <p className="text-sm text-content-muted">
          No container has its own update behaviour. Every container follows
          whichever update policy governs it.
        </p>
        <ViewContainers />
      </Section>
    );
  }

  // Present containers first, then the stale rows, each group by name. The
  // server already orders by name; this keeps the two groups apart without
  // re-sorting within either.
  const present = items.filter((item) => item.present);
  const orphaned = items.filter((item) => !item.present);

  return (
    <Section>
      <p className="text-sm text-content-muted">
        {total === 1
          ? "One container has its own update behaviour, set on its page."
          : `${total} containers have their own update behaviour, set on their pages.`}{" "}
        A container&rsquo;s own setting can only make HarborMaster more cautious
        than the policy governing it, never less.
      </p>

      <dl className="mt-3 flex flex-wrap gap-x-6 gap-y-2">
        {(Object.keys(BEHAVIOR_LABELS) as (keyof typeof BEHAVIOR_LABELS)[]).map(
          (behavior) => (
            <div key={behavior} className="min-w-0">
              <dt className="text-xs uppercase tracking-wide text-content-muted">
                {BEHAVIOR_LABELS[behavior]}
              </dt>
              {/* A real zero, because the server sends a key for every
                  behaviour. A gap would read as a question. */}
              <dd className="text-lg font-semibold tabular-nums">
                {counts[behavior] ?? 0}
              </dd>
            </div>
          ),
        )}
      </dl>

      {present.length > 0 ? (
        <ul className="mt-4 flex flex-col gap-1">
          {present.map((item) => (
            <li
              key={item.containerName}
              className="flex min-w-0 flex-wrap items-center justify-between gap-2 rounded-lg border border-border-subtle px-3 py-2"
            >
              {item.containerId ? (
                <Link
                  to={`/containers/${encodeURIComponent(item.containerId)}`}
                  className="min-w-0 truncate text-sm font-medium underline-offset-2 hover:underline"
                >
                  {item.containerName}
                </Link>
              ) : (
                <span className="min-w-0 truncate text-sm font-medium">
                  {item.containerName}
                </span>
              )}
              <StatusBadge tone="neutral" label={BEHAVIOR_LABELS[item.behavior]} />
            </li>
          ))}
        </ul>
      ) : null}

      {/*
        Saved behaviours whose container is not here.

        A preference is keyed by container NAME so it survives the recreation it
        authorises, which means one can outlive its container. HarborMaster keeps
        it — a name that comes back finds its setting waiting — and says so here
        rather than counting it among what is configured, which would overstate
        the estate, or hiding it, which would leave an operator unable to explain
        a name they no longer recognise.
      */}
      {stale > 0 ? (
        <div className="mt-4 rounded-lg border border-border-subtle bg-surface p-3">
          <p className="text-xs text-content-muted">
            {stale === 1
              ? "One saved behaviour names a container that is not on this host."
              : `${stale} saved behaviours name containers that are not on this host.`}{" "}
            They are kept, and take effect again if a container of that name
            returns. They are not counted above.
          </p>
          <ul className="mt-2 flex flex-wrap gap-2">
            {orphaned.map((item) => (
              <li
                key={item.containerName}
                className="min-w-0 truncate rounded-md border border-border-subtle px-2 py-1 text-xs"
              >
                {item.containerName} · {BEHAVIOR_LABELS[item.behavior]}
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      <ViewContainers />
    </Section>
  );
}

/** The way to the page where a behaviour is actually chosen. */
function ViewContainers() {
  return (
    <p className="mt-4 text-xs text-content-muted">
      A container&rsquo;s update behaviour is set on its own page.{" "}
      <Link to="/containers" className="underline underline-offset-2">
        View containers
      </Link>
    </p>
  );
}

function Section({ children }: { children: React.ReactNode }) {
  return (
    <section
      aria-labelledby="container-overrides-heading"
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <h2 id="container-overrides-heading" className="text-base font-semibold">
        Containers with their own setting
      </h2>
      <div className="mt-3">{children}</div>
    </section>
  );
}
