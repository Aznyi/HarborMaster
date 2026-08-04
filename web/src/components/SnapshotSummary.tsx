import { Link } from "react-router";

import type { Snapshot } from "../api/snapshotTypes";
import { ReadinessBadge } from "./SnapshotBadges";
import { useSnapshots } from "../hooks/useSnapshots";

/** How many snapshots the dashboard samples to build its readiness picture. */
const SAMPLE_SIZE = 50;

/**
 * The dashboard's snapshot panel.
 *
 * Deliberately quiet when there is nothing to say: a dashboard tile showing
 * zeroes is noise, and an operator scanning for problems should not have to
 * filter it out.
 *
 * The readiness distribution is computed over a bounded sample rather than the
 * whole table. Saying so in the UI matters -- an unqualified "3 not ready"
 * derived from the most recent fifty would be a claim about the whole estate
 * that HarborMaster has not made.
 */
export function SnapshotSummary() {
  const resource = useSnapshots({ page: 1, pageSize: SAMPLE_SIZE });

  const data = resource.data;
  const items: Snapshot[] = data?.items ?? [];
  const total = data?.pagination?.totalItems ?? 0;

  if (resource.status === "loading" || resource.error || total === 0) {
    return null;
  }

  const counts = {
    ready: items.filter((s) => s.readinessStatus === "ready").length,
    warning: items.filter((s) => s.readinessStatus === "warning").length,
    not_ready: items.filter((s) => s.readinessStatus === "not_ready").length,
    unknown: items.filter((s) => s.readinessStatus === "unknown").length,
  };

  const newest = items[0];
  const sampled = items.length;

  return (
    <section
      aria-labelledby="snapshot-summary-heading"
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 id="snapshot-summary-heading" className="text-lg font-semibold">
          Configuration snapshots
        </h2>
        <Link to="/snapshots" className="text-sm text-accent hover:underline">
          View all
        </Link>
      </div>

      <p className="mt-2 text-sm text-content-muted">
        {total} snapshot{total === 1 ? "" : "s"} recorded
        {newest ? (
          <>
            {" "}
            · newest{" "}
            <time dateTime={newest.createdAt}>
              {new Date(newest.createdAt).toLocaleString()}
            </time>
          </>
        ) : null}
      </p>

      <dl className="mt-4 grid grid-cols-2 gap-3 text-sm sm:grid-cols-4">
        <Tile label="Ready" value={counts.ready} />
        <Tile label="Warning" value={counts.warning} />
        <Tile label="Not ready" value={counts.not_ready} />
        <Tile label="Not evaluated" value={counts.unknown} />
      </dl>

      {total > sampled ? (
        <p className="mt-3 text-xs text-content-muted">
          Readiness counts cover the {sampled} most recent snapshots, not all {total}.
        </p>
      ) : null}

      {newest ? (
        <p className="mt-3 flex flex-wrap items-center gap-2 text-sm">
          <span className="text-content-muted">Most recent:</span>
          <Link
            to={`/snapshots/${newest.id}`}
            className="font-medium text-accent hover:underline"
          >
            {newest.containerName || newest.containerId}
          </Link>
          <ReadinessBadge status={newest.readinessStatus} />
        </p>
      ) : null}
    </section>
  );
}

function Tile({ label, value }: { label: string; value: number }) {
  return (
    <div className="rounded-lg border border-border-subtle bg-surface-sunken px-3 py-2">
      <dt className="text-xs uppercase tracking-wide text-content-muted">{label}</dt>
      <dd className="mt-0.5 text-lg font-semibold tabular-nums">{value}</dd>
    </div>
  );
}
