import { useMemo, useState } from "react";
import { Link } from "react-router";

import type {
  ReadinessStatus,
  Snapshot,
  SnapshotTrigger,
} from "../api/snapshotTypes";
import type { ListResponse } from "../api/inventoryTypes";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import { ReadinessBadge, TriggerBadge } from "../components/SnapshotBadges";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import type { ResourceState } from "../hooks/useApiResource";
import { useSnapshots } from "../hooks/useSnapshots";

const PAGE_SIZE = 25;

/** The filter vocabularies, matching the server's closed sets exactly. */
const TRIGGERS: SnapshotTrigger[] = ["manual", "api", "scheduled", "pre_update"];
const READINESS: ReadinessStatus[] = ["unknown", "ready", "warning", "not_ready"];

/**
 * Snapshot history.
 *
 * A snapshot records what a container's configuration WAS. HarborMaster cannot
 * restore one, and there is no control on this page that would: the API has no
 * such endpoint.
 */
export function Snapshots() {
  const [page, setPage] = useState(1);
  const [trigger, setTrigger] = useState<SnapshotTrigger | "">("");
  const [readiness, setReadiness] = useState<ReadinessStatus | "">("");

  const query = useMemo(
    () => ({
      page,
      pageSize: PAGE_SIZE,
      ...(trigger ? { trigger: [trigger] } : {}),
      ...(readiness ? { readiness: [readiness] } : {}),
    }),
    [page, trigger, readiness],
  );

  const resource = useSnapshots(query);

  const changeFilter = <T,>(set: (value: T) => void) => (value: T) => {
    set(value);
    // Any filter change invalidates the current page number.
    setPage(1);
  };

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Snapshots"
        description="Immutable point-in-time captures of container configuration. Snapshots are append-only records: HarborMaster reads configuration and never changes a container, and there is no restore."
      />

      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1 text-sm">
          <span className="text-content-muted">Trigger</span>
          <select
            className="rounded-lg border border-border-subtle bg-surface-raised px-3 py-2"
            value={trigger}
            onChange={(event) =>
              changeFilter(setTrigger)(event.target.value as SnapshotTrigger | "")
            }
          >
            <option value="">All triggers</option>
            {TRIGGERS.map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </select>
        </label>

        <label className="flex flex-col gap-1 text-sm">
          <span className="text-content-muted">Readiness</span>
          <select
            className="rounded-lg border border-border-subtle bg-surface-raised px-3 py-2"
            value={readiness}
            onChange={(event) =>
              changeFilter(setReadiness)(event.target.value as ReadinessStatus | "")
            }
          >
            <option value="">Any readiness</option>
            {READINESS.map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </select>
        </label>
      </div>

      <SnapshotResults resource={resource} onPageChange={setPage} />
    </div>
  );
}

function SnapshotResults({
  resource,
  onPageChange,
}: {
  resource: ResourceState<ListResponse<Snapshot>>;
  onPageChange: (page: number) => void;
}) {
  if (resource.status === "loading") return <LoadingState label="Loading snapshots" />;
  if (resource.status === "disconnected") {
    return <DisconnectedState onRetry={resource.refresh} />;
  }
  if (resource.error) {
    return <ErrorState error={resource.error} onRetry={resource.refresh} />;
  }

  // Optional chaining on items, not just on data: a truncated or unexpected
  // payload must render the empty state rather than crash the page. A view that
  // throws on malformed input turns a backend hiccup into a blank screen.
  const data = resource.data;
  if (!data?.items || data.items.length === 0) {
    return (
      <EmptyState
        title="No snapshots yet"
        description="Capture a snapshot from a container's detail page to record its configuration."
      />
    );
  }

  return (
    <div className="overflow-hidden rounded-xl border border-border-subtle bg-surface-raised">
      <div className="overflow-x-auto" tabIndex={0}>
        <table className="w-full text-left text-sm">
          <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
            <tr>
              <th scope="col" className="px-4 py-3">Container</th>
              <th scope="col" className="px-4 py-3">Captured</th>
              <th scope="col" className="px-4 py-3">Trigger</th>
              <th scope="col" className="px-4 py-3">Readiness</th>
              <th scope="col" className="px-4 py-3">Checksum</th>
            </tr>
          </thead>
          <tbody>
            {data.items.map((snapshot) => (
              <tr
                key={snapshot.id}
                className="border-b border-border-subtle last:border-0"
              >
                <td className="px-4 py-3">
                  <Link
                    to={`/snapshots/${snapshot.id}`}
                    className="font-medium text-accent hover:underline"
                  >
                    {snapshot.containerName || snapshot.containerId}
                  </Link>
                  {snapshot.reason ? (
                    <p className="mt-0.5 text-xs text-content-muted">{snapshot.reason}</p>
                  ) : null}
                </td>
                <td className="px-4 py-3 text-content-muted">
                  <time dateTime={snapshot.createdAt}>
                    {new Date(snapshot.createdAt).toLocaleString()}
                  </time>
                </td>
                <td className="px-4 py-3">
                  <TriggerBadge trigger={snapshot.trigger} />
                </td>
                <td className="px-4 py-3">
                  <ReadinessBadge status={snapshot.readinessStatus} />
                </td>
                <td className="px-4 py-3 font-mono text-xs text-content-muted">
                  {snapshot.checksum.slice(0, 12)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <Pagination
        pagination={data.pagination}
        onPageChange={onPageChange}
        busy={resource.refreshing}
      />
    </div>
  );
}
