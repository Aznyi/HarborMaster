import { useCallback, useId, useMemo, useState } from "react";
import { Link } from "react-router";

import { ApiError } from "../api/client";
import type {
  DependencyProblem,
  WorkloadDependency,
} from "../api/dependencyTypes";
import {
  dependencyKindLabel,
  dependencyOriginLabel,
  discoveryRefusalLabel,
  namespaceExplanation,
} from "../api/dependencyTypes";
import {
  DependencyDeleteConfirm,
  DependencyEditor,
} from "../components/DependencyEditor";
import { DependencyOrderPreview } from "../components/DependencyOrderPreview";
import { PageIntro } from "../components/PageIntro";
import {
  DisconnectedState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { DependencyOperationList } from "../components/DependencyOperations";
import {
  useDependencies,
  useDependencyAdmin,
  useDependencyGraph,
  useDependencyOperations,
} from "../hooks/useDependencies";
import { useSession } from "../hooks/useSession";

/**
 * Update dependencies: what has to be stable before something else changes.
 *
 * # What this page is for
 *
 * Six questions, in the order an operator asks them. What relationships exist,
 * which did HarborMaster find for itself, which did somebody configure, which
 * can I change, is the estate orderable at all, and what order would be used.
 * Every section below answers one of those.
 *
 * It is deliberately not a graph explorer. Nobody arrives here wanting to
 * inspect a directed acyclic graph; they arrive because a container did not
 * update and they want to know what it is waiting for.
 *
 * # Dependencies constrain ORDER, never eligibility
 *
 * Nothing on this page can cause an update. A relationship makes HarborMaster
 * wait or refuse, and that is the whole of its power — which is also why
 * REMOVING one is administrator-only while reading is not. The page says so
 * where an operator might otherwise assume the opposite: in the create preview,
 * in the delete confirmation, and in the order preview's own description.
 */
export function Dependencies() {
  const listing = useDependencies();
  const graph = useDependencyGraph();
  const operations = useDependencyOperations();
  const { create, remove } = useDependencyAdmin();
  const session = useSession();

  const mayManage = session.can("dependency:manage");

  const [filter, setFilter] = useState<DependencyFilter>("all");
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | undefined>(undefined);
  const [saving, setSaving] = useState(false);
  const [removingEdge, setRemovingEdge] = useState<WorkloadDependency | null>(
    null,
  );
  const [removeError, setRemoveError] = useState<string | undefined>(undefined);
  const [notice, setNotice] = useState<string | null>(null);

  const refresh = useCallback(() => {
    listing.refresh();
    graph.refresh();
  }, [listing, graph]);

  const onCreate = useCallback(
    async (dependent: string, dependency: string) => {
      setSaving(true);
      setCreateError(undefined);
      try {
        await create({ dependent, dependency });
        setCreating(false);
        setNotice(`HarborMaster will now update ${dependency} before ${dependent}.`);
        refresh();
      } catch (error) {
        // The server's own refusal, verbatim. It names WHICH check said no —
        // a self-edge, a duplicate, an absent container, a loop — and replacing
        // that with "could not save" would throw away the only thing that tells
        // an operator what to do differently.
        setCreateError(refusalText(error, "The relationship could not be recorded."));
      } finally {
        setSaving(false);
      }
    },
    [create, refresh],
  );

  const onRemove = useCallback(async () => {
    if (!removingEdge?.dependencyId) return;
    setSaving(true);
    setRemoveError(undefined);
    try {
      await remove(removingEdge.dependencyId);
      setNotice(
        `${removingEdge.dependent} will no longer wait for ${removingEdge.dependency}.`,
      );
      setRemovingEdge(null);
      refresh();
    } catch (error) {
      setRemoveError(refusalText(error, "The relationship could not be removed."));
    } finally {
      setSaving(false);
    }
  }, [remove, refresh, removingEdge]);

  const edges = useMemo(() => listing.data?.items ?? [], [listing.data]);
  const problems = useMemo(() => listing.data?.problems ?? [], [listing.data]);
  const shown = useMemo(() => filterEdges(edges, filter), [edges, filter]);

  const detected = edges.filter((edge) => edge.source !== "operator").length;
  const configured = edges.length - detected;
  const cycles = graph.data?.cycles?.length ?? 0;

  // The listing is what the page is ABOUT, so its failure is the page's failure.
  // The graph is a second read and is allowed to fail on its own — the preview
  // says so rather than taking the relationship list down with it.
  if (listing.status === "loading") {
    return <LoadingState label="Loading dependencies" />;
  }
  if (listing.status === "disconnected") {
    return <DisconnectedState onRetry={refresh} />;
  }
  if (listing.error) {
    return <ErrorState error={listing.error} onRetry={refresh} />;
  }

  return (
    <div className="space-y-6">
      <PageIntro
        title="Update dependencies"
        description={
          "Which containers must be stable before others can be changed. " +
          "HarborMaster detects the relationships Docker enforces and an " +
          "administrator can record ordering their application needs. A " +
          "relationship only ever delays or blocks an update — it never causes one."
        }
      />

      <Summary
        total={edges.length}
        detected={detected}
        configured={configured}
        cycles={cycles}
        problems={problems.length}
      />

      {notice ? (
        <p
          role="status"
          className="rounded-lg border border-accent/40 bg-accent-soft px-3 py-2 text-sm"
        >
          {notice}
        </p>
      ) : null}

      {/* Unresolved namespace shares. Rendered ABOVE the list and never folded
          into it, because a container listed here is blocked whatever the list
          below shows — "HarborMaster could not establish this" is a different
          answer from "there is nothing here". */}
      {problems.length > 0 ? <ProblemReport problems={problems} /> : null}

      <section aria-labelledby="relationships-heading" className="space-y-3">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <h2 id="relationships-heading" className="text-base font-semibold">
            Relationships
          </h2>
          {mayManage ? (
            <button
              type="button"
              className="min-h-11 rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
              onClick={() => {
                setCreating((open) => !open);
                setCreateError(undefined);
                setNotice(null);
              }}
              aria-expanded={creating}
            >
              {creating ? "Cancel" : "Record an ordering"}
            </button>
          ) : null}
        </div>

        {creating ? (
          <div className="rounded-xl border border-border-subtle bg-surface-raised px-4 py-4">
            <h3 className="text-sm font-semibold">Record an ordering</h3>
            <p className="mt-1 mb-4 max-w-prose text-sm text-content-muted">
              Tell HarborMaster that one container needs another updated and
              verified first. This is for ordering your application needs;
              anything Docker enforces is detected already.
            </p>
            <DependencyEditor
              onCreate={(dependent, dependency) => {
                void onCreate(dependent, dependency);
              }}
              busy={saving}
              error={createError}
            />
          </div>
        ) : null}

        {removingEdge ? (
          <>
            <DependencyDeleteConfirm
              edge={removingEdge}
              busy={saving}
              onCancel={() => {
                setRemovingEdge(null);
                setRemoveError(undefined);
              }}
              onConfirm={() => void onRemove()}
            />
            {removeError ? (
              <p role="alert" className="text-sm text-danger">
                {removeError}
              </p>
            ) : null}
          </>
        ) : null}

        {edges.length > 0 ? (
          <FilterControl value={filter} onChange={setFilter} counts={{
            all: edges.length,
            detected,
            configured,
          }} />
        ) : null}

        <RelationshipList
          edges={shown}
          total={edges.length}
          problems={problems.length}
          filter={filter}
          mayManage={mayManage}
          onRemove={(edge) => {
            setRemovingEdge(edge);
            setRemoveError(undefined);
            setNotice(null);
          }}
        />
      </section>

      <OperationsSection state={operations} />

      <OrderPreviewSection
        state={graph}
        mayManage={mayManage}
        onRemove={(edge) => {
          setRemovingEdge(edge);
          setRemoveError(undefined);
          setNotice(null);
        }}
      />
    </div>
  );
}

// ---------------------------------------------------------------- summary --

/**
 * The counts, at the top.
 *
 * Deliberately five numbers and no more. Node and edge totals, graph build
 * times, and index sizes are subsystem telemetry: true, and useless to somebody
 * deciding whether they need to do something.
 */
function Summary({
  total,
  detected,
  configured,
  cycles,
  problems,
}: {
  total: number;
  detected: number;
  configured: number;
  cycles: number;
  problems: number;
}) {
  return (
    <section
      aria-label="Dependency summary"
      className="flex flex-wrap gap-x-6 gap-y-2 rounded-xl border border-border-subtle bg-surface-raised px-4 py-3 text-sm"
    >
      <p>
        <span className="font-semibold">{total}</span>{" "}
        {total === 1 ? "dependency relationship" : "dependency relationships"}
      </p>
      <p className="text-content-muted">
        <span className="font-semibold text-content">{detected}</span> detected
        by HarborMaster
      </p>
      <p className="text-content-muted">
        <span className="font-semibold text-content">{configured}</span>{" "}
        configured by you
      </p>
      {cycles > 0 ? (
        <p className="font-medium text-danger">
          {cycles} {cycles === 1 ? "loop needs" : "loops need"} attention
        </p>
      ) : null}
      {problems > 0 ? (
        <p className="font-medium text-warn">
          {problems}{" "}
          {problems === 1
            ? "relationship could not be established"
            : "relationships could not be established"}
        </p>
      ) : null}
    </section>
  );
}

// ----------------------------------------------------------------- filter --

type DependencyFilter = "all" | "detected" | "configured";

function filterEdges(
  edges: WorkloadDependency[],
  filter: DependencyFilter,
): WorkloadDependency[] {
  if (filter === "detected") {
    return edges.filter((edge) => edge.source !== "operator");
  }
  if (filter === "configured") {
    return edges.filter((edge) => edge.source === "operator");
  }
  return edges;
}

/**
 * All / Detected / Configured.
 *
 * A radio group rather than three buttons: the choices are exclusive, and a
 * radio group is what a screen reader announces as "one of three" and what
 * arrow keys already navigate.
 */
function FilterControl({
  value,
  onChange,
  counts,
}: {
  value: DependencyFilter;
  onChange: (value: DependencyFilter) => void;
  counts: { all: number; detected: number; configured: number };
}) {
  const name = useId();
  const options: { value: DependencyFilter; label: string; count: number }[] = [
    { value: "all", label: "All", count: counts.all },
    { value: "detected", label: "Detected by HarborMaster", count: counts.detected },
    { value: "configured", label: "Configured by you", count: counts.configured },
  ];

  return (
    <fieldset className="flex flex-wrap items-center gap-x-4 gap-y-1">
      <legend className="sr-only">Show which relationships</legend>
      {options.map((option) => (
        <label
          key={option.value}
          className="flex min-h-11 items-center gap-2 text-sm"
        >
          <input
            type="radio"
            name={name}
            className="h-5 w-5 shrink-0"
            checked={value === option.value}
            onChange={() => onChange(option.value)}
          />
          <span>
            {option.label}{" "}
            <span className="text-content-muted">({option.count})</span>
          </span>
        </label>
      ))}
    </fieldset>
  );
}

// ------------------------------------------------------------------- list --

/**
 * The relationships, as cards rather than as table rows.
 *
 * A five-column table does not survive a 390px viewport without either
 * truncating a container name or putting the remove button behind a horizontal
 * scroll. Both are worse than a card: a name an operator cannot read in full is
 * one they cannot check they are looking at the right container, and an action
 * they have to scroll sideways to find is one they will not find.
 */
function RelationshipList({
  edges,
  total,
  problems,
  filter,
  mayManage,
  onRemove,
}: {
  edges: WorkloadDependency[];
  total: number;
  problems: number;
  filter: DependencyFilter;
  mayManage: boolean;
  onRemove: (edge: WorkloadDependency) => void;
}) {
  if (total === 0) {
    // The empty state is only honest when discovery actually SUCCEEDED. With
    // unresolved shares outstanding, "no dependencies found" would be the page
    // clearing containers on the strength of a question nobody could answer.
    if (problems > 0) {
      return (
        <p className="text-sm text-content-muted">
          HarborMaster recorded no relationships it could establish. It could not
          establish the ones reported above, so this is not a statement that
          these workloads are independent.
        </p>
      );
    }
    return (
      <div className="rounded-xl border border-border-subtle bg-surface-raised px-4 py-6 text-center">
        <p className="text-sm font-medium">No dependencies found</p>
        <p className="mt-1 text-sm text-content-muted">
          HarborMaster can evaluate these workloads independently. Nothing has to
          be updated before anything else.
        </p>
      </div>
    );
  }

  if (edges.length === 0) {
    return (
      <p className="text-sm text-content-muted">
        {filter === "detected"
          ? "HarborMaster has detected no relationships from the container configuration."
          : "No orderings have been configured."}
      </p>
    );
  }

  return (
    <ul className="space-y-2" aria-label="Dependency relationships">
      {edges.map((edge) => (
        <li
          key={`${edge.dependent}->${edge.dependency}:${edge.source}`}
          className="rounded-xl border border-border-subtle bg-surface-raised px-4 py-3"
        >
          <div className="flex flex-wrap items-start justify-between gap-2">
            <div className="min-w-0">
              <p className="break-all text-sm font-medium">
                <ContainerLink name={edge.dependent} /> depends on{" "}
                <ContainerLink name={edge.dependency} />
              </p>
              {/* Kind and origin as separate words. The distinction between
                  what Docker enforces and what a person asserted decides
                  whether there is anything here to remove, so it must survive
                  a greyscale screen and a screen reader. */}
              <p className="mt-1 flex flex-wrap gap-x-2 gap-y-1 text-xs text-content-muted">
                <span>{dependencyKindLabel(edge.source)}</span>
                <span aria-hidden="true">·</span>
                <span>{dependencyOriginLabel(edge.source)}</span>
              </p>
            </div>

            {edge.deletable && mayManage ? (
              <button
                type="button"
                className="min-h-9 shrink-0 rounded-lg border border-danger/40 px-2.5 py-1 text-xs font-medium text-danger"
                onClick={() => onRemove(edge)}
                /*
                 * The full name on the control itself rather than a visible
                 * word plus an sr-only tail. Several identical "Remove" buttons
                 * on one page are indistinguishable in a screen reader's list
                 * of controls, and the visible word is the first word of this
                 * label, which is what WCAG's label-in-name rule asks for.
                 */
                aria-label={`Remove the ordering between ${edge.dependent} and ${edge.dependency}`}
              >
                Remove
              </button>
            ) : null}
          </div>

          <p className="mt-2 max-w-prose text-sm text-content-muted">
            {namespaceExplanation(edge.source, edge.dependent, edge.dependency) ??
              `${edge.dependent} waits for ${edge.dependency} to finish updating.`}
          </p>

          {!edge.deletable ? (
            <p className="mt-1 text-xs text-content-muted">
              HarborMaster reads this from the container configuration on every
              refresh, so it cannot be removed here.
            </p>
          ) : null}
        </li>
      ))}
    </ul>
  );
}

/** A container name that goes somewhere useful. */
function ContainerLink({ name }: { name: string }) {
  return (
    <Link
      to={`/containers?search=${encodeURIComponent(name)}`}
      className="break-all text-accent underline"
    >
      {name}
    </Link>
  );
}

// --------------------------------------------------------------- problems --

/** Namespace shares HarborMaster could not resolve. */
function ProblemReport({ problems }: { problems: DependencyProblem[] }) {
  return (
    // The live region is the SECTION. Putting role="status" on the list items
    // would replace their implicit listitem role and leave the <ul> with
    // children that are not list items — a real defect, not only an axe finding:
    // a screen reader stops announcing the list and how long it is.
    <section
      role="status"
      aria-labelledby="dependency-problems-heading"
      className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-3"
    >
      <h2 id="dependency-problems-heading" className="text-sm font-semibold text-warn">
        Dependency information unavailable
      </h2>
      <p className="mt-1 max-w-prose text-sm">
        These containers declare a shared namespace HarborMaster could not
        resolve. It will not update them until it can establish what they depend
        on — this is not a statement that they have no dependencies.
      </p>
      <ul className="mt-2 space-y-2">
        {problems.map((problem, index) => (
          <li
            key={`${problem.container}:${problem.refusal}:${index}`}
            className="text-sm"
          >
            <span className="break-all font-medium">{problem.container}</span>
            <span className="block text-content-muted">
              {discoveryRefusalLabel(problem.refusal)}
            </span>
          </li>
        ))}
      </ul>
    </section>
  );
}

// ----------------------------------------------------------------- preview --

/**
 * What HarborMaster did when it replaced a shared-namespace container.
 *
 * Its own read and its own failure state, like the order preview: an operator
 * looking for a half-finished reattachment must not lose the relationship list
 * because a different query failed.
 */
function OperationsSection({
  state,
}: {
  state: ReturnType<typeof useDependencyOperations>;
}) {
  const items = state.data?.items ?? [];
  // Nothing to show and nothing that went wrong: the section stays away rather
  // than adding an empty box to every healthy estate.
  if (state.status === "ready" && items.length === 0) return null;

  return (
    <section aria-labelledby="dependency-operations-heading" className="space-y-3">
      <div>
        <h2 id="dependency-operations-heading" className="text-base font-semibold">
          Coordinated updates
        </h2>
        <p className="mt-1 max-w-prose text-sm text-content-muted">
          When HarborMaster replaces a container others share a namespace with,
          it must recreate those containers on the image they are already
          running so they can attach to the replacement. No version moves.
        </p>
      </div>

      {state.status === "loading" ? (
        <LoadingState label="Loading coordinated updates" />
      ) : state.error || state.status === "disconnected" ? (
        <p
          role="status"
          className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-sm"
        >
          HarborMaster could not read the coordinated updates. This is not a
          statement that there are none outstanding.
        </p>
      ) : (
        <DependencyOperationList summaries={items} limit={state.data?.limit} />
      )}
    </section>
  );
}

/**
 * The order preview, with its own failure state.
 *
 * The graph is a second read. When it fails the relationship list above is still
 * correct and still worth showing, so this degrades on its own — and says
 * UNAVAILABLE rather than showing an empty order, which would read as "nothing
 * constrains anything".
 */
function OrderPreviewSection({
  state,
  mayManage,
  onRemove,
}: {
  state: ReturnType<typeof useDependencyGraph>;
  mayManage: boolean;
  onRemove: (edge: WorkloadDependency) => void;
}) {
  if (state.status === "loading") {
    return <LoadingState label="Working out the update order" />;
  }
  if (state.status === "disconnected" || state.error || !state.data) {
    return (
      <section aria-labelledby="dependency-order-unavailable" className="space-y-2">
        <h2 id="dependency-order-unavailable" className="text-base font-semibold">
          Dependency order
        </h2>
        <p
          role="status"
          className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-sm"
        >
          Dependency information unavailable. HarborMaster could not work out an
          update order for this host, so it will not reorder or update anything
          that depends on something else until it can.
        </p>
      </section>
    );
  }

  return (
    <DependencyOrderPreview
      graph={state.data}
      mayManage={mayManage}
      onRemove={onRemove}
    />
  );
}

/**
 * The server's own words for a refusal.
 *
 * A refused relationship comes back as a 409 whose message names WHICH check
 * said no. That sentence is HarborMaster's, already operator-facing, and is the
 * only part of the response that tells somebody what to do differently — so it
 * is rendered as text, never interpolated as markup, and never replaced with a
 * generic failure.
 */
function refusalText(error: unknown, fallback: string): string {
  if (error instanceof ApiError && error.message) return error.message;
  if (error instanceof Error && error.message) return error.message;
  return fallback;
}
