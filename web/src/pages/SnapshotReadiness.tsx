import { Link, useParams } from "react-router";

import type { ReadinessCheck, ReadinessReport } from "../api/snapshotTypes";
import { PageIntro } from "../components/PageIntro";
import { ReadinessBadge } from "../components/SnapshotBadges";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { useSnapshotReadiness } from "../hooks/useSnapshots";

/** Operator-facing names for each check. */
const CHECK_LABELS: Record<string, string> = {
  daemon_reachable: "Docker daemon reachable",
  inventory_fresh: "Inventory freshness",
  image_available: "Image available locally",
  image_digest_known: "Image digest recorded",
  named_volumes_present: "Named volumes present",
  mount_sources: "Mount sources",
  networks_present: "Networks present",
  restart_policy_valid: "Restart policy valid",
  compose_metadata_complete: "Compose metadata complete",
  secrets_available: "Secrets available",
  config_consistent: "Configuration internally consistent",
  runtime_features: "Runtime features",
};

/**
 * Restore-readiness for one snapshot.
 *
 * The banner is not decoration. Everything on this page describes whether a
 * restore WOULD succeed, and HarborMaster cannot perform one — an operator who
 * skims a page full of green ticks should still come away knowing that.
 */
export function SnapshotReadinessPage() {
  const params = useParams();
  const id = Number(params.id);
  const resource = useSnapshotReadiness(id);

  if (!Number.isInteger(id) || id < 1) {
    return <EmptyState title="Unknown snapshot" description="That snapshot id is not valid." />;
  }
  if (resource.status === "loading") return <LoadingState label="Evaluating readiness" />;
  if (resource.status === "disconnected") {
    return <DisconnectedState onRetry={resource.refresh} />;
  }
  if (resource.error) {
    return <ErrorState error={resource.error} onRetry={resource.refresh} />;
  }

  const report = resource.data;
  if (!report) return null;

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Snapshot completeness"
        description="Whether restoring this snapshot would succeed, evaluated against the current host."
      />

      <p
        role="note"
        className="rounded-lg border border-border-subtle bg-surface-sunken px-4 py-3 text-sm"
      >
        <strong className="font-semibold">This report is informational.</strong>{" "}
        HarborMaster is read-only: it cannot restore, recreate, or modify a
        container. These checks exist so the gaps are visible before a future
        release can act on them.
      </p>

      <div className="flex flex-wrap items-center gap-3">
        <ReadinessBadge status={report.status} />
        <Link to={`/snapshots/${id}`} className="text-sm text-accent hover:underline">
          Back to snapshot
        </Link>
      </div>

      <Provenance report={report} />

      <ul className="flex flex-col gap-2">
        {report.checks.map((check) => (
          <CheckRow key={check.id} check={check} />
        ))}
      </ul>
    </div>
  );
}

/**
 * How current the data behind this verdict is.
 *
 * Most checks read HarborMaster's cached inventory rather than the daemon, so a
 * verdict is only as trustworthy as that reading is recent. Showing the age is
 * what stops a stale "ready" being mistaken for a live one.
 */
function Provenance({ report }: { report: ReadinessReport }) {
  const age = report.inventoryAgeSeconds ?? 0;
  const ageLabel =
    age < 60 ? `${age}s` : age < 3600 ? `${Math.round(age / 60)}m` : `${Math.round(age / 3600)}h`;

  return (
    <div
      className={`rounded-lg border px-4 py-3 text-sm ${
        report.inventoryStale
          ? "border-warn/40 bg-warn-soft"
          : "border-border-subtle bg-surface-raised"
      }`}
    >
      <dl className="grid grid-cols-2 gap-x-6 gap-y-2 sm:grid-cols-4">
        <div>
          <dt className="text-xs uppercase tracking-wide text-content-muted">
            Inventory age
          </dt>
          <dd className="mt-0.5 font-medium tabular-nums">{ageLabel}</dd>
        </div>
        <div>
          <dt className="text-xs uppercase tracking-wide text-content-muted">
            Generation
          </dt>
          <dd className="mt-0.5 font-medium tabular-nums">
            {report.inventoryGeneration ?? 0}
          </dd>
        </div>
        <div>
          <dt className="text-xs uppercase tracking-wide text-content-muted">
            Inventory completed
          </dt>
          <dd className="mt-0.5">
            {report.inventoryCompletedAt
              ? new Date(report.inventoryCompletedAt).toLocaleString()
              : "never"}
          </dd>
        </div>
        <div>
          <dt className="text-xs uppercase tracking-wide text-content-muted">
            Daemon checked
          </dt>
          <dd className="mt-0.5">
            {report.daemonCheckedAt
              ? new Date(report.daemonCheckedAt).toLocaleTimeString()
              : "—"}
          </dd>
        </div>
      </dl>

      {report.inventoryStale ? (
        <p className="mt-3 text-xs">
          The inventory is older than the configured freshness threshold, so this
          verdict is capped at <strong>warning</strong>. Refresh the inventory before
          relying on it.
        </p>
      ) : null}
    </div>
  );
}

function CheckRow({ check }: { check: ReadinessCheck }) {
  return (
    <li className="flex flex-col gap-1 rounded-lg border border-border-subtle bg-surface-raised px-4 py-3 sm:flex-row sm:items-start sm:justify-between sm:gap-4">
      <div className="min-w-0">
        <p className="text-sm font-medium">{CHECK_LABELS[check.id] ?? check.id}</p>
        {check.detail ? (
          <p className="mt-0.5 text-sm text-content-muted">{check.detail}</p>
        ) : null}
      </div>
      <ReadinessBadge status={check.status} />
    </li>
  );
}
