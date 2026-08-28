import { formatImageReference } from "../api/presentation";
import { useCallback, useMemo, useState } from "react";
import { Link } from "react-router";

import type { ContainerListRow, ContainerQuery } from "../api/inventoryTypes";
import { useContainerPage, useFilterOptions } from "../hooks/useContainers";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import { ScrollArea } from "../components/DetailSection";
import {
  AttentionBadge,
  AttentionMarkers,
  PreservedNote,
  UpdateCell,
  needsOperator,
  rowAttention,
} from "../components/AttentionBadges";
import {
  ContainerHealthBadge,
  ContainerStateBadge,
} from "../components/InventoryBadges";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";

const PAGE_SIZE = 25;

/**
 * The columns, and which server-side sort each one drives.
 *
 * Two of the previous eight became row context: compose project moved into the
 * name cell and restart count under the state it explains. Neither is
 * something anybody scans a column for, and the space they were using now
 * carries the two things an operator was previously opening every row to find.
 *
 * "HarborMaster" and "Update" have no `sort` because the server does not order
 * on them. A header that looked sortable and silently was not would be worse
 * than one that plainly is not.
 *
 * # Why the assessment column is called "HarborMaster"
 *
 * It was called "Needs attention", and most of what it contains does not need
 * any: "Up to date", "Not tracked" and "Not checked" are the three commonest
 * values on a healthy host, and labelling them as attention taught operators
 * that the column cries wolf.
 *
 * The column is not one subject. It carries HarborMaster's update verdict, its
 * automation state, its dependency findings, and the containers it is keeping
 * as evidence -- so no word narrower than the product name covers it
 * honestly. "Assessment" would have fitted the update verdicts and mislabelled
 * the rest.
 *
 * It is also the only naming that cannot be confused with the two columns
 * beside it, which is the other requirement: "State" is Docker's, "Health" is
 * the container's own healthcheck, and this one is ours.
 */
const COLUMNS: { key: string; label: string; sort?: string }[] = [
  { key: "name", label: "Name", sort: "name" },
  { key: "state", label: "State", sort: "state" },
  { key: "health", label: "Health", sort: "health" },
  { key: "attention", label: "HarborMaster" },
  { key: "image", label: "Image", sort: "image" },
  { key: "update", label: "Update" },
  { key: "ports", label: "Ports" },
];

/**
 * Sort fields with no column of their own.
 *
 * Offered in the filter bar so nothing that was sortable before this page was
 * reorganised stopped being sortable. The select and the column headers write
 * the same state, so they can never disagree.
 */
const SORT_OPTIONS: { value: string; label: string }[] = [
  { value: "name", label: "Name" },
  { value: "state", label: "State" },
  { value: "health", label: "Health" },
  { value: "image", label: "Image" },
  { value: "project", label: "Compose project" },
  { value: "restartCount", label: "Restart count" },
  { value: "lastSeen", label: "Last seen" },
];

/**
 * The containers table.
 *
 * Filtering, sorting, and paging all happen on the server. The browser never
 * holds more than one page, which is what keeps a thousand-container host
 * usable without virtualisation.
 *
 * Each row now carries what HarborMaster KNOWS about the container -- whether
 * an update exists, whether an approval is held, whether automation gave up,
 * whether findings are open -- computed server-side in one batched lookup for
 * the whole page. Before this, telling a healthy workload from one holding an
 * unapproved major update meant opening every row.
 */
export function Containers() {
  const [query, setQuery] = useState<ContainerQuery>({
    page: 1,
    pageSize: PAGE_SIZE,
    sort: "name",
    direction: "asc",
  });

  const page = useContainerPage(query);
  const filters = useFilterOptions();

  // Any filter change resets to page 1: staying on page 7 of a narrower result
  // set would show an empty table.
  const update = useCallback((patch: Partial<ContainerQuery>) => {
    setQuery((current) => ({ ...current, ...patch, page: 1 }));
  }, []);

  const onSort = useCallback((field: string) => {
    setQuery((current) => ({
      ...current,
      sort: field,
      direction: current.sort === field && current.direction === "asc" ? "desc" : "asc",
      page: 1,
    }));
  }, []);

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Containers"
        description="Every container on this host, and what HarborMaster knows about each one. HarborMaster observes them; it never starts, stops, or removes a container on its own."
      />

      <FilterBar
        query={query}
        onChange={update}
        onSort={onSort}
        projects={filters.data?.projects ?? []}
        images={filters.data?.images ?? []}
      />

      <ContainerTable
        query={query}
        page={page}
        onSort={onSort}
        onPageChange={(next) => setQuery((current) => ({ ...current, page: next }))}
      />
    </div>
  );
}

function FilterBar({
  query,
  onChange,
  onSort,
  projects,
  images,
}: {
  query: ContainerQuery;
  onChange: (patch: Partial<ContainerQuery>) => void;
  onSort: (field: string) => void;
  projects: string[];
  images: string[];
}) {
  return (
    <section
      aria-label="Filters"
      className="grid gap-3 rounded-xl border border-border-subtle bg-surface-raised p-4 sm:grid-cols-2 lg:grid-cols-4"
    >
      <label className="flex flex-col gap-1 text-sm">
        <span className="text-content-muted">Search</span>
        <input
          type="search"
          value={query.search ?? ""}
          onChange={(event) => onChange({ search: event.target.value })}
          placeholder="Name, image, or ID"
          className="min-h-11 rounded-lg border border-border-subtle bg-surface px-3 py-2"
        />
      </label>

      <label className="flex flex-col gap-1 text-sm">
        <span className="text-content-muted">State</span>
        <select
          value={query.state?.[0] ?? ""}
          onChange={(event) =>
            onChange({ state: event.target.value ? [event.target.value] : [] })
          }
          className="min-h-11 rounded-lg border border-border-subtle bg-surface px-3 py-2"
        >
          <option value="">All states</option>
          {["running", "exited", "paused", "restarting", "created", "dead", "removing", "unknown"].map(
            (state) => (
              <option key={state} value={state}>
                {state}
              </option>
            ),
          )}
        </select>
      </label>

      <label className="flex flex-col gap-1 text-sm">
        <span className="text-content-muted">Health</span>
        <select
          value={query.health?.[0] ?? ""}
          onChange={(event) =>
            onChange({ health: event.target.value ? [event.target.value] : [] })
          }
          className="min-h-11 rounded-lg border border-border-subtle bg-surface px-3 py-2"
        >
          <option value="">Any health</option>
          {["healthy", "unhealthy", "starting", "none"].map((health) => (
            <option key={health} value={health}>
              {health}
            </option>
          ))}
        </select>
      </label>

      <label className="flex flex-col gap-1 text-sm">
        <span className="text-content-muted">Compose project</span>
        <select
          value={query.project ?? ""}
          onChange={(event) => onChange({ project: event.target.value })}
          className="min-h-11 rounded-lg border border-border-subtle bg-surface px-3 py-2"
        >
          <option value="">All projects</option>
          {projects.map((project) => (
            <option key={project} value={project}>
              {project}
            </option>
          ))}
        </select>
      </label>

      <label className="flex flex-col gap-1 text-sm lg:col-span-2">
        <span className="text-content-muted">Image</span>
        <select
          value={query.image ?? ""}
          onChange={(event) => onChange({ image: event.target.value })}
          className="min-h-11 rounded-lg border border-border-subtle bg-surface px-3 py-2"
        >
          <option value="">All images</option>
          {images.map((image) => (
            <option key={image} value={image}>
              {image}
            </option>
          ))}
        </select>
      </label>

      {/*
        The same sort state the column headers drive. It exists because two of
        the previous columns became row context, and because at phone width the
        headers are off to the side of a scrolling table where nobody will find
        them.
      */}
      <label className="flex flex-col gap-1 text-sm lg:col-span-2">
        <span className="text-content-muted">Sort by</span>
        <select
          value={query.sort ?? "name"}
          onChange={(event) => onSort(event.target.value)}
          className="min-h-11 rounded-lg border border-border-subtle bg-surface px-3 py-2"
        >
          {SORT_OPTIONS.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </select>
      </label>

      <label className="flex min-h-11 items-center gap-2 self-end text-sm lg:col-span-2">
        <input
          type="checkbox"
          checked={query.includeAbsent ?? false}
          onChange={(event) => onChange({ includeAbsent: event.target.checked })}
          className="h-6 w-6 shrink-0 rounded border-border-subtle"
        />
        <span>Include containers that are no longer present</span>
      </label>

      {/*
        The containers HarborMaster parked aside are not workloads: they are
        stopped on purpose and run a deliberately older image. They are out of
        the default view and one click back into it -- and nothing anywhere
        removes them.
      */}
      <label className="flex min-h-11 items-center gap-2 self-end text-sm lg:col-span-2">
        <input
          type="checkbox"
          checked={query.includePreserved ?? false}
          onChange={(event) => onChange({ includePreserved: event.target.checked })}
          className="h-6 w-6 shrink-0 rounded border-border-subtle"
        />
        <span>
          Show containers HarborMaster kept
          <span className="block text-xs text-content-muted">
            Originals held during an update, failed replacements, and rollback
            evidence. Hidden by default; never deleted.
          </span>
        </span>
      </label>
    </section>
  );
}

function ContainerTable({
  query,
  page,
  onSort,
  onPageChange,
}: {
  query: ContainerQuery;
  page: ReturnType<typeof useContainerPage>;
  onSort: (field: string) => void;
  onPageChange: (page: number) => void;
}) {
  const rows = useMemo(() => page.data?.items ?? [], [page.data]);

  if (page.status === "loading") {
    return <LoadingState label="Loading containers" />;
  }
  if (page.status === "disconnected") {
    return <DisconnectedState onRetry={page.refresh} />;
  }
  if (page.error) {
    return <ErrorState error={page.error} onRetry={page.refresh} />;
  }
  if (!page.data) {
    return <LoadingState label="Loading containers" />;
  }

  if (rows.length === 0) {
    const filtered =
      Boolean(query.search) ||
      Boolean(query.project) ||
      Boolean(query.image) ||
      (query.state?.length ?? 0) > 0 ||
      (query.health?.length ?? 0) > 0;

    return (
      <EmptyState
        title={filtered ? "No containers match these filters" : "No containers found"}
        description={
          filtered
            ? "Try widening or clearing the filters above."
            : "HarborMaster has not recorded any containers. Run a refresh from the Dashboard, and check that the Docker socket is reachable."
        }
      />
    );
  }

  const needing = rows.filter((row) =>
    needsOperator(rowAttention(row.attention).state),
  ).length;

  return (
    <div className="rounded-xl border border-border-subtle bg-surface-raised">
      {/*
        The count first, so the answer to "is anything wrong on this page" does
        not require reading every row. Silent when nothing is.
      */}
      {needing > 0 ? (
        <p
          role="status"
          className="border-b border-border-subtle px-4 py-3 text-sm"
        >
          <span className="font-medium">
            {needing} of {rows.length} container{rows.length === 1 ? "" : "s"} on
            this page need{needing === 1 ? "s" : ""} attention.
          </span>{" "}
          <span className="text-content-muted">
            Look at the HarborMaster column.
          </span>
        </p>
      ) : null}

      <ScrollArea>
        <table className="w-full min-w-[64rem] text-left text-sm">
          <caption className="sr-only">
            Containers, page {page.data.pagination.page} of {page.data.pagination.totalPages}
          </caption>
          <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
            <tr>
              {COLUMNS.map((column) => {
                const active = Boolean(column.sort) && query.sort === column.sort;
                const ascending = active && query.direction === "asc";
                return (
                  /*
                    `aria-sort` belongs on the COLUMN HEADER, not on the control
                    inside it. It is only defined for a row/column header, so a
                    button carrying it is an unsupported attribute - axe reports
                    it as a critical violation, and a screen reader gets no sort
                    state from the header it is actually reading.
                  */
                  <th
                    key={column.key}
                    scope="col"
                    aria-sort={
                      column.sort
                        ? active
                          ? ascending
                            ? "ascending"
                            : "descending"
                          : "none"
                        : undefined
                    }
                    className="px-4 py-3 font-medium"
                  >
                    {column.sort ? (
                      <button
                        type="button"
                        onClick={() => onSort(column.sort!)}
                        /*
                          The name says what the control DOES. The current state
                          is the header's job, above, so saying it here as well
                          would have a screen reader announce the sort twice.
                        */
                        aria-label={`Sort by ${column.label}`}
                        className="inline-flex min-h-6 items-center gap-1 hover:text-content"
                      >
                        {column.label}
                        {/*
                          Escapes rather than the literal triangle characters.
                          Those were once written into this file as UTF-8 and
                          read back as CP1252, which baked a corrupted sequence
                          into the source and showed it on every sortable
                          column. An escape cannot be corrupted by a re-encode.

                          Rendered only for the ACTIVE column: the previous
                          markup drew the active column's direction on every
                          header and merely hid it with opacity, so an inactive
                          column carried an arrow that contradicted its own
                          state.
                        */}
                        {active ? (
                          <span aria-hidden="true">
                            {ascending ? "▲" : "▼"}
                          </span>
                        ) : null}
                      </button>
                    ) : (
                      column.label
                    )}
                  </th>
                );
              })}
            </tr>
          </thead>
          <tbody>
            {rows.map((container) => (
              <ContainerRow key={container.id} container={container} />
            ))}
          </tbody>
        </table>
      </ScrollArea>

      <Pagination
        pagination={page.data.pagination}
        onPageChange={onPageChange}
        busy={page.refreshing}
      />
    </div>
  );
}

function ContainerRow({ container }: { container: ContainerListRow }) {
  const attention = rowAttention(container.attention);
  const image = formatImageReference(container.image);

  return (
    <tr className="border-b border-border-subtle last:border-0 align-top hover:bg-surface-sunken">
      <td className="px-4 py-3">
        {/*
          `min-h-11` and the block display give the name link a touch target
          rather than the 19px line box a bare inline link has. It is the most
          used control on the page.
        */}
        <Link
          to={`/containers/${container.id}`}
          className="inline-flex min-h-11 items-center font-medium text-accent hover:underline"
        >
          {container.name || container.shortId}
        </Link>
        <p className="font-mono text-xs text-content-muted">{container.shortId}</p>
        {container.compose.managed ? (
          <p className="text-xs text-content-muted">
            {container.compose.project}
            {container.compose.service ? ` / ${container.compose.service}` : ""}
          </p>
        ) : null}
        {!container.present ? (
          <p className="mt-1 text-xs text-warn">no longer present</p>
        ) : null}
      </td>

      <td className="px-4 py-3">
        <ContainerStateBadge state={container.state} />
        {/*
          Restart count sits under the state it explains rather than in a
          column of its own. A number nobody scans a column for, and one that
          means nothing without the state beside it.
        */}
        {container.restartCount > 0 ? (
          <p className="mt-1 text-xs text-content-muted">
            {container.restartCount} restart{container.restartCount === 1 ? "" : "s"}
          </p>
        ) : null}
      </td>

      <td className="px-4 py-3">
        <ContainerHealthBadge health={container.health} />
      </td>

      <td className="px-4 py-3">
        <AttentionBadge state={attention.state} />
        {attention.preserved ? (
          <p className="mt-1">
            <PreservedNote
              kind={attention.preserved}
              workload={attention.preservedFor}
            />
          </p>
        ) : null}
        <AttentionMarkers attention={attention} containerId={container.id} />
      </td>

      <td className="px-4 py-3">
        {/*
          The digest is abbreviated, the tag never is, and the complete
          reference is on the title so it is available without leaving the row.
          The container's own page carries it in full and unabbreviated.
        */}
        <span
          className="block break-all font-mono text-xs"
          title={image.abbreviated ? image.full : undefined}
        >
          {image.display}
        </span>
        {/*
          What the container FOLLOWS, under what it RUNS. A container
          HarborMaster has updated runs an immutable digest, so without this an
          actively automated workload reads as deliberately pinned -- exactly
          backwards.
        */}
        <span className="mt-0.5 block text-xs text-content-muted">
          {attention.trackingKnown
            ? attention.tracking
              ? `follows ${attention.tracking}`
              : "follows no tag"
            : "not yet established"}
        </span>
      </td>

      <td className="px-4 py-3">
        <UpdateCell attention={attention} />
      </td>

      <td className="px-4 py-3">
        <PortList ports={container.ports} />
      </td>
    </tr>
  );
}

function PortList({ ports }: { ports: ContainerListRow["ports"] }) {
  const published = ports.filter((port) => port.published);
  if (published.length === 0) {
    return <span className="text-xs text-content-muted">none published</span>;
  }
  return (
    <ul className="flex flex-col gap-0.5 font-mono text-xs">
      {published.slice(0, 3).map((port, index) => (
        <li key={`${port.containerPort}-${port.protocol}-${index}`}>
          {port.hostIp ? `${port.hostIp}:` : ""}
          {port.hostPort}&rarr;{port.containerPort}/{port.protocol}
        </li>
      ))}
      {published.length > 3 ? (
        <li className="text-content-muted">+{published.length - 3} more</li>
      ) : null}
    </ul>
  );
}
