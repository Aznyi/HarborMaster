import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router";

import { ApiError, refreshInventory } from "../api/client";
import type { InventoryStatus } from "../api/inventoryTypes";
import type { HealthReport } from "../api/types";
import type { ResourceState } from "../hooks/useApiResource";
import { useInventory } from "../hooks/useContainers";
import { useEventEngine } from "../hooks/useDockerEvents";
import { useVersion } from "../hooks/useHealth";
import { ConnectionStateBadge } from "../components/EventBadges";
import {
  DisconnectedState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { SnapshotSummary } from "../components/SnapshotSummary";
import { StatusBadge, componentTone } from "../components/StatusBadge";

/** Outcome of a manual refresh, shown until the next attempt. */
type RefreshFeedback =
  | { kind: "idle" }
  | { kind: "started"; message: string }
  | { kind: "conflict"; message: string }
  | { kind: "failed"; message: string }
  | { kind: "completed"; message: string };

/**
 * The dashboard renders only data the API returned. There is no sample
 * content: an operations view showing invented numbers is worse than one
 * showing nothing.
 */
export function Dashboard({ health }: { health: ResourceState<HealthReport> }) {
  const inventory = useInventory();

  if (inventory.status === "loading") {
    return <LoadingState label="Loading inventory" />;
  }
  if (inventory.status === "disconnected") {
    return <DisconnectedState onRetry={inventory.refresh} />;
  }
  if (inventory.error) {
    return <ErrorState error={inventory.error} onRetry={inventory.refresh} />;
  }
  if (!inventory.data) {
    return <LoadingState label="Loading inventory" />;
  }

  return (
    <div className="flex flex-col gap-6">
      <InventoryHeader inventory={inventory} />
      <ConnectionCards status={inventory.data} health={health} />
      <EventEnginePanel />
      <SnapshotSummary />
      <ContainerMetrics status={inventory.data} />
      <CatalogMetrics status={inventory.data} />
      <WarningsPanel status={inventory.data} />
    </div>
  );
}

/**
 * Event-engine status.
 *
 * The one thing this panel must communicate clearly: whether the inventory is
 * being kept current by live events or has fallen back to periodic polling. An
 * operator seeing a stale container state needs to know which, because the two
 * have very different expected latencies.
 */
function EventEnginePanel() {
  const engine = useEventEngine();

  if (!engine.data) {
    return (
      <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <h2 className="text-lg font-semibold">Docker events</h2>
        <p className="mt-2 text-sm text-content-muted">
          Event engine status is unavailable.
        </p>
      </section>
    );
  }

  const status = engine.data;

  if (!status.enabled) {
    return (
      <section
        aria-labelledby="events-heading"
        className="rounded-xl border border-border-subtle bg-surface-raised p-5"
      >
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h2 id="events-heading" className="text-lg font-semibold">
              Docker events
            </h2>
            <p className="mt-1 text-sm text-content-muted">
              Disabled by configuration. The inventory refreshes on its own
              schedule, which is a supported mode.
            </p>
          </div>
          <ConnectionStateBadge state={status.state} />
        </div>
      </section>
    );
  }

  const disconnected = status.state !== "connected";
  const degraded = disconnected || status.overflowPending;

  return (
    <section
      aria-labelledby="events-heading"
      className={`rounded-xl border p-5 ${
        degraded ? "border-warn/40 bg-warn-soft" : "border-border-subtle bg-surface-raised"
      }`}
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 id="events-heading" className="text-lg font-semibold">
            Docker events
          </h2>
          <p className="mt-1 text-sm text-content-muted">
            Live synchronisation with the Docker daemon
          </p>
        </div>
        <div className="flex items-center gap-3">
          <ConnectionStateBadge state={status.state} />
          <Link to="/events" className="text-sm font-medium text-accent hover:underline">
            View all
          </Link>
        </div>
      </div>

      <dl className="mt-4 grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">
        <Metric label="Last event" value={formatTimestamp(status.lastEventAt)} />
        <Metric
          label="Last reconciliation"
          value={formatTimestamp(status.lastReconciliationAt)}
        />
        <Metric label="Reconnects" value={String(status.counters.reconnectCount)} />
        <Metric label="Queue" value={`${status.queueDepth} / ${status.queueCapacity}`} />
        <Metric label="Recorded events" value={String(status.storedEvents)} />
        <Metric label="Received" value={String(status.counters.eventsReceived)} />
        <Metric label="Dropped" value={String(status.counters.eventsDropped)} />
        <Metric
          label="Targeted refreshes"
          value={String(status.counters.targetedRefreshes)}
        />
      </dl>

      {disconnected ? (
        <p role="alert" className="mt-4 rounded-lg border border-warn/40 px-3 py-2 text-sm">
          The Docker event stream is disconnected, so HarborMaster is relying on
          periodic reconciliation. Container state may lag by up to one
          reconciliation interval until the stream is back.
        </p>
      ) : null}

      {status.overflowPending ? (
        <p role="alert" className="mt-4 rounded-lg border border-warn/40 px-3 py-2 text-sm">
          The event queue overflowed. A full reconciliation is running to restore
          the inventory.
        </p>
      ) : null}
    </section>
  );
}

/** Refresh state, timings, and the manual refresh control. */
function InventoryHeader({ inventory }: { inventory: ResourceState<InventoryStatus> }) {
  const status = inventory.data!;
  const [feedback, setFeedback] = useState<RefreshFeedback>({ kind: "idle" });
  const [submitting, setSubmitting] = useState(false);

  // Tracks the generation at the moment a refresh was requested, so completion
  // can be detected: the generation advances only once a refresh persists.
  const awaitingGeneration = useRef<number | null>(null);
  const { refresh: reload } = inventory;

  useEffect(() => {
    if (awaitingGeneration.current === null) return;
    if (status.inProgress) return;

    if (status.generation > awaitingGeneration.current) {
      awaitingGeneration.current = null;
      setFeedback({
        kind: "completed",
        message: `Inventory updated. Generation ${status.generation}.`,
      });
      return;
    }
    // The refresh finished without advancing the generation, which means it
    // failed. The server records why.
    if (status.state === "failed") {
      awaitingGeneration.current = null;
      setFeedback({
        kind: "failed",
        message: status.lastAttempt?.error
          ? `Refresh failed: ${status.lastAttempt.error}`
          : "Refresh failed.",
      });
    }
  }, [status.generation, status.inProgress, status.state, status.lastAttempt]);

  const onRefresh = useCallback(async () => {
    setSubmitting(true);
    awaitingGeneration.current = status.generation;

    try {
      const accepted = await refreshInventory();
      setFeedback({ kind: "started", message: accepted.message });
      // Pull the new status immediately so the UI shows "running" without
      // waiting for the next poll tick.
      reload();
    } catch (caught) {
      awaitingGeneration.current = null;
      const error = caught as ApiError;
      if (error.code === "conflict") {
        setFeedback({
          kind: "conflict",
          message: "A refresh is already running. Waiting for it to finish.",
        });
        reload();
      } else if (error.code === "service_unavailable") {
        setFeedback({
          kind: "failed",
          message: "Docker is unreachable, so the refresh was not started.",
        });
      } else {
        setFeedback({ kind: "failed", message: error.message });
      }
    } finally {
      setSubmitting(false);
    }
  }, [reload, status.generation]);

  const busy = submitting || status.inProgress;

  return (
    <section
      aria-labelledby="inventory-heading"
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h2 id="inventory-heading" className="text-lg font-semibold">
            Inventory
          </h2>
          <p className="mt-1 text-sm text-content-muted">
            {describeInventory(status)}
          </p>
        </div>

        <div className="flex flex-col items-end gap-2">
          <StatusBadge tone={refreshTone(status)} label={busy ? "refreshing" : status.state} />
          <button
            type="button"
            onClick={onRefresh}
            disabled={busy || !status.enabled}
            aria-busy={busy}
            className="rounded-lg border border-border-subtle bg-surface-raised px-4 py-2 text-sm font-medium transition-colors hover:bg-surface-sunken disabled:cursor-not-allowed disabled:opacity-50"
          >
            {busy ? "Refreshingâ€¦" : "Refresh inventory"}
          </button>
        </div>
      </div>

      <dl className="mt-4 grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">
        <Metric label="Generation" value={status.generation === 0 ? "none yet" : String(status.generation)} />
        <Metric label="Last success" value={formatTimestamp(status.lastSuccess?.finishedAt)} />
        <Metric label="Duration" value={formatDuration(status.lastSuccess?.durationMs)} />
        <Metric label="Checksum" value={status.checksum ? status.checksum.slice(0, 12) : "â€”"} mono />
      </dl>

      {feedback.kind !== "idle" ? (
        <p
          role="status"
          aria-live="polite"
          // Named, because the shell's connectivity indicator is also a status
          // region: without a label the two are indistinguishable to a screen
          // reader moving between them.
          aria-label="Refresh status"
          className={`mt-4 rounded-lg border px-3 py-2 text-sm ${feedbackClasses(feedback.kind)}`}
        >
          {feedback.message}
        </p>
      ) : null}

      {!status.enabled ? (
        <p className="mt-4 rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-sm">
          The inventory engine is disabled by configuration. The figures below
          describe the last inventory that was stored.
        </p>
      ) : null}
    </section>
  );
}

function ConnectionCards({
  status,
  health,
}: {
  status: InventoryStatus;
  health: ResourceState<HealthReport>;
}) {
  // Defensive: the API always sends `docker` (it is required by the schema),
  // but this reads from the network, and a malformed payload should degrade
  // one card rather than blank the whole dashboard with a render crash.
  const docker = status.docker ?? { status: "down" as const, detail: "status unavailable" };

  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <div className="flex items-start justify-between gap-3">
          <div>
            <h3 className="font-semibold">Docker Engine</h3>
            <p className="mt-1 text-sm text-content-muted">
              Read-only connection to the Docker socket
            </p>
          </div>
          <StatusBadge tone={componentTone(docker.status)} label={docker.status} />
        </div>
        <dl className="mt-4 grid grid-cols-2 gap-3 text-sm">
          <Metric label="API version" value={docker.version ?? "â€”"} />
          <Metric label="Runtime" value={status.runtime ?? "docker"} />
          {docker.detail ? (
            <div className="col-span-2">
              <dt className="text-content-muted">Detail</dt>
              <dd className="mt-0.5">{docker.detail}</dd>
            </div>
          ) : null}
        </dl>
      </section>

      <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <div className="flex items-start justify-between gap-3">
          <div>
            <h3 className="font-semibold">Database</h3>
            <p className="mt-1 text-sm text-content-muted">
              SQLite store for the inventory
            </p>
          </div>
          {health.data ? (
            <StatusBadge
              tone={componentTone(health.data.database.status)}
              label={health.data.database.status}
            />
          ) : (
            <StatusBadge tone="neutral" label="unknown" />
          )}
        </div>
        <BuildRow />
      </section>
    </div>
  );
}

function BuildRow() {
  const build = useVersion();

  if (!build.data) {
    return (
      <p className="mt-4 text-sm text-content-muted">Build information is unavailable.</p>
    );
  }
  return (
    <dl className="mt-4 grid grid-cols-2 gap-3 text-sm">
      <Metric label="Version" value={build.data.version} />
      <Metric label="Platform" value={build.data.platform} />
    </dl>
  );
}

/** Zeroed counts, so a malformed payload renders zeros rather than crashing. */
const EMPTY_COUNTS: InventoryStatus["counts"] = {
  containers: 0,
  absent: 0,
  running: 0,
  stopped: 0,
  paused: 0,
  restarting: 0,
  healthy: 0,
  unhealthy: 0,
  images: 0,
  networks: 0,
  volumes: 0,
  warnings: 0,
  byState: {},
};

function ContainerMetrics({ status }: { status: InventoryStatus }) {
  const counts = status.counts ?? EMPTY_COUNTS;

  return (
    <section
      aria-labelledby="containers-heading"
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 id="containers-heading" className="text-lg font-semibold">
          Containers
        </h2>
        <Link to="/containers" className="text-sm font-medium text-accent hover:underline">
          View all
        </Link>
      </div>

      <div className="mt-4 grid gap-3 sm:grid-cols-3 lg:grid-cols-5">
        <Tile label="Total" value={counts.containers} />
        <Tile label="Running" value={counts.running} tone="ok" />
        <Tile label="Stopped" value={counts.stopped} />
        <Tile label="Paused" value={counts.paused} tone={counts.paused > 0 ? "warn" : "neutral"} />
        <Tile
          label="Unhealthy"
          value={counts.unhealthy}
          tone={counts.unhealthy > 0 ? "danger" : "neutral"}
        />
      </div>

      {counts.absent > 0 ? (
        <p className="mt-3 text-sm text-content-muted">
          {counts.absent} container{counts.absent === 1 ? "" : "s"} seen previously
          {counts.absent === 1 ? " is" : " are"} no longer present. Their records are
          retained.
        </p>
      ) : null}
    </section>
  );
}

function CatalogMetrics({ status }: { status: InventoryStatus }) {
  const counts = status.counts ?? EMPTY_COUNTS;

  return (
    <section
      aria-labelledby="catalog-heading"
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <h2 id="catalog-heading" className="text-lg font-semibold">
        Catalog
      </h2>
      <div className="mt-4 grid gap-3 sm:grid-cols-3">
        <Tile label="Images" value={counts.images} />
        <Tile label="Networks" value={counts.networks} />
        <Tile label="Volumes" value={counts.volumes} />
      </div>
    </section>
  );
}

function WarningsPanel({ status }: { status: InventoryStatus }) {
  const warnings = status.warnings ?? [];

  if (warnings.length === 0) {
    return (
      <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <h2 className="text-lg font-semibold">Warnings</h2>
        <p className="mt-2 text-sm text-content-muted">
          The last refresh completed without warnings.
        </p>
      </section>
    );
  }

  return (
    <section
      aria-labelledby="warnings-heading"
      className="rounded-xl border border-warn/40 bg-warn-soft p-5"
    >
      <h2 id="warnings-heading" className="text-lg font-semibold">
        Warnings ({warnings.length})
      </h2>
      <p className="mt-1 text-sm text-content-muted">
        Non-fatal problems from the last refresh. A vanished container is
        expected churn, not a fault.
      </p>
      <ul className="mt-4 flex flex-col gap-2 text-sm">
        {warnings.slice(0, 10).map((warning, index) => (
          <li
            key={warning.id ?? `${warning.code}-${index}`}
            className="rounded-lg border border-border-subtle bg-surface-raised px-3 py-2"
          >
            <span className="font-mono text-xs text-content-muted">{warning.code}</span>
            <p className="mt-0.5">
              {warning.containerName ? (
                <strong className="font-medium">{warning.containerName}: </strong>
              ) : null}
              {warning.message}
            </p>
          </li>
        ))}
      </ul>
    </section>
  );
}

function Tile({
  label,
  value,
  tone = "neutral",
}: {
  label: string;
  value: number;
  tone?: "neutral" | "ok" | "warn" | "danger";
}) {
  const toneClasses = {
    neutral: "border-border-subtle",
    ok: "border-ok/40",
    warn: "border-warn/40",
    danger: "border-danger/40",
  }[tone];

  return (
    <div className={`rounded-lg border ${toneClasses} bg-surface-sunken px-4 py-3`}>
      <p className="text-xs text-content-muted">{label}</p>
      <p className="mt-1 text-2xl font-semibold tabular-nums">{value}</p>
    </div>
  );
}

function Metric({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return (
    <div>
      <dt className="text-content-muted">{label}</dt>
      <dd className={mono ? "mt-0.5 font-mono text-xs" : "mt-0.5"}>{value}</dd>
    </div>
  );
}

function describeInventory(status: InventoryStatus): string {
  if (status.inProgress) return "A refresh is running.";
  if (status.generation === 0) return "No inventory has been collected yet.";
  if (status.state === "failed") {
    return "The last refresh failed. The figures below describe the previous inventory.";
  }
  return `Read-only inventory of the local Docker host, last refreshed ${formatTimestamp(
    status.lastSuccess?.finishedAt,
  )}.`;
}

function refreshTone(status: InventoryStatus): "ok" | "warn" | "danger" | "neutral" {
  if (status.inProgress) return "warn";
  switch (status.state) {
    case "succeeded":
      return "ok";
    case "failed":
      return "danger";
    default:
      return "neutral";
  }
}

function feedbackClasses(kind: RefreshFeedback["kind"]): string {
  switch (kind) {
    case "completed":
      return "border-ok/40 bg-ok-soft";
    case "failed":
      return "border-danger/40 bg-danger-soft";
    case "conflict":
      return "border-warn/40 bg-warn-soft";
    default:
      return "border-border-subtle bg-surface-sunken";
  }
}

function formatTimestamp(iso: string | undefined): string {
  if (!iso) return "never";
  const parsed = new Date(iso);
  return Number.isNaN(parsed.getTime()) ? iso : parsed.toLocaleString();
}

function formatDuration(ms: number | undefined): string {
  if (ms === undefined) return "â€”";
  if (ms < 1000) return `${ms} ms`;
  return `${(ms / 1000).toFixed(1)} s`;
}
