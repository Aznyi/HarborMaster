import { useCallback, useMemo, useState } from "react";
import { Link } from "react-router";

import type { ContainerQuery, ContainerSummary } from "../api/inventoryTypes";
import { useContainerPage, useFilterOptions } from "../hooks/useContainers";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import { ScrollArea } from "../components/DetailSection";
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

/** Columns the API can sort on, paired with their headings. */
const SORTABLE_COLUMNS: { field: string; label: string }[] = [
  { field: "name", label: "Name" },
  { field: "state", label: "State" },
  { field: "health", label: "Health" },
  { field: "image", label: "Image" },
  { field: "project", label: "Compose" },
  { field: "restartCount", label: "Restarts" },
  { field: "lastSeen", label: "Last seen" },
];

/**
 * The containers table.
 *
 * Filtering, sorting, and paging all happen on the server. The browser never
 * holds more than one page, which is what keeps a thousand-container host
 * usable without virtualisation.
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
        description="A read-only inventory of the containers the Docker Engine reports. HarborMaster observes them; it never starts, stops, recreates, or removes one."
      />

      <FilterBar
        query={query}
        onChange={update}
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
  projects,
  images,
}: {
  query: ContainerQuery;
  onChange: (patch: Partial<ContainerQuery>) => void;
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
          className="rounded-lg border border-border-subtle bg-surface px-3 py-2"
        />
      </label>

      <label className="flex flex-col gap-1 text-sm">
        <span className="text-content-muted">State</span>
        <select
          value={query.state?.[0] ?? ""}
          onChange={(event) =>
            onChange({ state: event.target.value ? [event.target.value] : [] })
          }
          className="rounded-lg border border-border-subtle bg-surface px-3 py-2"
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
          className="rounded-lg border border-border-subtle bg-surface px-3 py-2"
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
          className="rounded-lg border border-border-subtle bg-surface px-3 py-2"
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
          className="rounded-lg border border-border-subtle bg-surface px-3 py-2"
        >
          <option value="">All images</option>
          {images.map((image) => (
            <option key={image} value={image}>
              {image}
            </option>
          ))}
        </select>
      </label>

      <label className="flex items-center gap-2 self-end text-sm lg:col-span-2">
        <input
          type="checkbox"
          checked={query.includeAbsent ?? false}
          onChange={(event) => onChange({ includeAbsent: event.target.checked })}
          className="size-4 rounded border-border-subtle"
        />
        <span>Include containers that are no longer present</span>
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

  return (
    <div className="rounded-xl border border-border-subtle bg-surface-raised">
      <ScrollArea>
        <table className="w-full min-w-[56rem] text-left text-sm">
          <caption className="sr-only">
            Containers, page {page.data.pagination.page} of {page.data.pagination.totalPages}
          </caption>
          <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
            <tr>
              {SORTABLE_COLUMNS.map((column) => {
                const active = query.sort === column.field;
                return (
                  <th key={column.field} scope="col" className="px-4 py-3 font-medium">
                    <button
                      type="button"
                      onClick={() => onSort(column.field)}
                      aria-sort={
                        active
                          ? query.direction === "asc"
                            ? "ascending"
                            : "descending"
                          : "none"
                      }
                      className="inline-flex items-center gap-1 hover:text-content"
                    >
                      {column.label}
                      <span aria-hidden="true" className={active ? "" : "opacity-0"}>
                        {query.direction === "asc" ? "â–²" : "â–¼"}
                      </span>
                    </button>
                  </th>
                );
              })}
              <th scope="col" className="px-4 py-3 font-medium">
                Ports
              </th>
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

function ContainerRow({ container }: { container: ContainerSummary }) {
  return (
    <tr className="border-b border-border-subtle last:border-0 hover:bg-surface-sunken">
      <td className="px-4 py-3">
        <Link
          to={`/containers/${container.id}`}
          className="font-medium text-accent hover:underline"
        >
          {container.name || container.shortId}
        </Link>
        <p className="font-mono text-xs text-content-muted">{container.shortId}</p>
        {!container.present ? (
          <p className="mt-1 text-xs text-warn">no longer present</p>
        ) : null}
      </td>
      <td className="px-4 py-3">
        <ContainerStateBadge state={container.state} />
      </td>
      <td className="px-4 py-3">
        <ContainerHealthBadge health={container.health} />
      </td>
      <td className="px-4 py-3">
        <span className="break-all">{container.image.repository ?? container.image.raw}</span>
        {container.image.tag ? (
          <span className="text-content-muted">:{container.image.tag}</span>
        ) : null}
      </td>
      <td className="px-4 py-3">
        {container.compose.managed ? (
          <>
            <span className="block">{container.compose.project}</span>
            <span className="text-xs text-content-muted">{container.compose.service}</span>
          </>
        ) : (
          <span className="text-xs text-content-muted">standalone</span>
        )}
      </td>
      <td className="px-4 py-3 tabular-nums">
        {container.restartCount}
        <span className="block text-xs text-content-muted">
          {container.restartPolicy.name}
        </span>
      </td>
      <td className="px-4 py-3 text-xs text-content-muted">
        {formatRelative(container.lastSeenAt)}
      </td>
      <td className="px-4 py-3">
        <PortList ports={container.ports} />
      </td>
    </tr>
  );
}

function PortList({ ports }: { ports: ContainerSummary["ports"] }) {
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

function formatRelative(iso: string): string {
  const parsed = new Date(iso);
  if (Number.isNaN(parsed.getTime())) return iso;

  const seconds = Math.round((Date.now() - parsed.getTime()) / 1000);
  if (seconds < 60) return "just now";
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return parsed.toLocaleDateString();
}
