import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router";

import { ApiError, refreshInventory } from "../api/client";
import type { InventoryStatus } from "../api/inventoryTypes";
import type { HealthReport } from "../api/types";
import type { ResourceState } from "../hooks/useApiResource";
import { useInventory } from "../hooks/useContainers";
import { useEventEngine } from "../hooks/useDockerEvents";
import { useExecutions } from "../hooks/useExecutions";
import { usePlans } from "../hooks/usePlans";
import { useRollbacks } from "../hooks/useRollbacks";
import { useVersion } from "../hooks/useHealth";
import { ConnectionStateBadge } from "../components/EventBadges";
import {
  DisconnectedState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { SnapshotSummary } from "../components/SnapshotSummary";
import { useAutomationStatus } from "../hooks/useAutomation";
import { useDriftSummary } from "../hooks/useDrift";
import { usePolicySummary } from "../hooks/usePolicies";
import { useSession } from "../hooks/useSession";
import { StatusBadge, componentTone } from "../components/StatusBadge";
import {
  atLevel,
  buildAttention,
  evidenceComplete,
  type AttentionItem,
  type AttentionInputs,
} from "../components/attentionModel";

/** Outcome of a manual refresh, shown until the next attempt. */
type RefreshFeedback =
  | { kind: "idle" }
  | { kind: "started"; message: string }
  | { kind: "conflict"; message: string }
  | { kind: "failed"; message: string }
  | { kind: "completed"; message: string };

/**
 * One row is enough to get a summary.
 *
 * The plan, recreation and rollback list endpoints return their aggregate
 * beside the page, so the dashboard asks for the smallest page that exists
 * rather than a dedicated endpoint. Three bounded reads, not three new
 * surfaces to authorise and document.
 */
const SUMMARY_ONLY = { page: 1, pageSize: 1 } as const;

/**
 * The dashboard.
 *
 * # What changed and why
 *
 * It used to be nine panels of subsystem telemetry in the order the subsystems
 * were built. The first screen carried the inventory generation, the inventory
 * checksum, the event queue depth, the reconnect count and the number of
 * volumes -- and an operator could read all of it without learning that a
 * container was unhealthy or that an update was waiting for their approval.
 *
 * It now answers, in order:
 *
 *   1. Does anything need me?          -- the attention list
 *   2. What is the state of my estate? -- containers, updates, automation
 *   3. Is HarborMaster itself working? -- Docker, database, events
 *   4. Everything else                 -- collapsed, and still all there
 *
 * # What is deliberately NOT hidden
 *
 * A degraded Docker connection, a failing database, a disconnected event
 * stream and a failed refresh all appear in the attention list at the top,
 * whatever else is collapsed below. The advanced section holds telemetry, not
 * problems.
 *
 * # Why the composition happens in the browser
 *
 * Every number here comes from an aggregate HarborMaster already computes and
 * already serves: they are `COUNT`s and `GROUP BY`s over indexed columns, one
 * query each, not per-container reads. Composing them server-side would mean a
 * new endpoint with its own authorisation surface, deciding centrally what a
 * viewer without `automation:read` may see -- which the per-panel hooks
 * already decide correctly, one permission at a time.
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
      <AttentionPanel health={health} inventory={inventory.data} />
      <EstatePanel inventory={inventory.data} />
      <SystemPanel status={inventory.data} health={health} />
      <SnapshotSummary />
      <AdvancedPanel inventory={inventory} />
    </div>
  );
}

// ------------------------------------------------------------ attention --

/**
 * The first thing on the page: what needs a person.
 *
 * Three groups, because they call for different responses. "Needs you" is work.
 * "Worth watching" is context. "Nothing established" is the honest report of
 * what HarborMaster has not looked at -- an estate with no policies and no
 * plans is unexamined, not healthy, and this is where that gets said.
 */
function AttentionPanel({
  health,
  inventory,
}: {
  health: ResourceState<HealthReport>;
  inventory: InventoryStatus;
}) {
  const session = useSession();
  const canReadAutomation = Boolean(
    session.user?.permissions.includes("automation:read"),
  );

  const events = useEventEngine();
  const automation = useAutomationStatus();
  const policy = usePolicySummary();
  const drift = useDriftSummary();
  const plans = usePlans(SUMMARY_ONLY);
  const executions = useExecutions(SUMMARY_ONLY);
  const rollbacks = useRollbacks(SUMMARY_ONLY);

  const inputs: AttentionInputs = useMemo(
    () => ({
      health: health.data,
      inventory,
      events: events.data,
      // A 503 from a subsystem that is switched off is not an error to report;
      // it is a feature that is not there, and the model reads its absence as
      // "no evidence" rather than as "nothing wrong".
      automation: automation.error ? null : (automation.data?.status ?? null),
      plans: plans.data?.summary ?? null,
      executions: executions.data?.summary ?? null,
      rollbacks: rollbacks.data?.summary ?? null,
      policy: policy.data,
      drift: drift.data,
      canReadAutomation,
    }),
    [
      health.data,
      inventory,
      events.data,
      automation.data,
      automation.error,
      plans.data,
      executions.data,
      rollbacks.data,
      policy.data,
      drift.data,
      canReadAutomation,
    ],
  );

  const items = useMemo(() => buildAttention(inputs), [inputs]);
  const complete = evidenceComplete(inputs);

  const action = atLevel(items, "action");
  const watch = atLevel(items, "watch");
  const info = atLevel(items, "info");

  return (
    <section
      aria-labelledby="attention-heading"
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <h2 id="attention-heading" className="text-lg font-semibold">
        {action.length > 0
          ? `${action.length} thing${action.length === 1 ? "" : "s"} need${
              action.length === 1 ? "s" : ""
            } you`
          : "Nothing needs you right now"}
      </h2>

      {action.length === 0 ? (
        <p className="mt-1 text-sm text-content-muted">
          {complete
            ? "HarborMaster has checked everything it can and found nothing " +
              "that requires a person."
            : "Some of what HarborMaster reports has not loaded, so this is " +
              "not a complete answer."}
        </p>
      ) : null}

      {action.length > 0 ? <ItemList items={action} /> : null}

      {watch.length > 0 ? (
        <>
          <h3 className="mt-5 text-sm font-semibold uppercase tracking-wide text-content-muted">
            Worth watching
          </h3>
          <ItemList items={watch} />
        </>
      ) : null}

      {info.length > 0 ? (
        <>
          <h3 className="mt-5 text-sm font-semibold uppercase tracking-wide text-content-muted">
            What HarborMaster has not established
          </h3>
          <ItemList items={info} />
        </>
      ) : null}
    </section>
  );
}

const LEVEL_BORDERS: Record<AttentionItem["level"], string> = {
  action: "border-danger/40 bg-danger-soft",
  watch: "border-warn/40 bg-warn-soft",
  info: "border-border-subtle bg-surface-sunken",
};

function ItemList({ items }: { items: AttentionItem[] }) {
  return (
    <ul className="mt-3 flex flex-col gap-2">
      {items.map((item) => (
        <li key={item.id}>
          {/*
            The whole card is the link. An operator reading "3 updates left
            containers behind" should not then have to find a separate control
            to go and look at them.
          */}
          <Link
            to={item.to}
            className={`flex min-h-11 flex-col rounded-lg border px-4 py-3 transition-colors hover:brightness-110 ${
              LEVEL_BORDERS[item.level]
            }`}
          >
            <span className="font-medium text-content">{item.title}</span>
            <span className="mt-0.5 text-sm text-content-muted">{item.detail}</span>
          </Link>
        </li>
      ))}
    </ul>
  );
}

// --------------------------------------------------------------- estate --

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

/**
 * The estate at a glance: what is running, and whether HarborMaster is
 * allowed to change any of it.
 *
 * The automation line is here rather than in its own panel because "is
 * automatic updating on" is a question about the ESTATE, and answering it
 * three panels below a container count made it look like a subsystem detail
 * rather than the standing permission it is.
 */
function EstatePanel({ inventory }: { inventory: InventoryStatus }) {
  const counts = inventory.counts ?? EMPTY_COUNTS;
  const session = useSession();
  const automation = useAutomationStatus();
  const canReadAutomation = session.user?.permissions.includes("automation:read");
  const engine = automation.error ? null : automation.data?.status;

  return (
    <section
      aria-labelledby="estate-heading"
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 id="estate-heading" className="text-lg font-semibold">
          Your containers
        </h2>
        <Link
          to="/containers"
          className="inline-flex min-h-11 items-center text-sm font-medium text-accent hover:underline"
        >
          View all
        </Link>
      </div>

      <div className="mt-4 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Tile label="Running" value={counts.running} tone="ok" />
        <Tile label="Stopped" value={counts.stopped} />
        <Tile
          label="Unhealthy"
          value={counts.unhealthy}
          tone={counts.unhealthy > 0 ? "danger" : "neutral"}
        />
        <Tile
          label="Paused"
          value={counts.paused}
          tone={counts.paused > 0 ? "warn" : "neutral"}
        />
      </div>

      {counts.absent > 0 ? (
        <p className="mt-3 text-sm text-content-muted">
          {counts.absent} container{counts.absent === 1 ? "" : "s"} seen previously
          {counts.absent === 1 ? " is" : " are"} no longer present.{" "}
          {counts.absent === 1 ? "Its record is" : "Their records are"} retained.
        </p>
      ) : null}

      {canReadAutomation && engine ? (
        <p className="mt-4 rounded-lg border border-border-subtle bg-surface-sunken px-3 py-2 text-sm">
          <span className="font-medium">
            {engine.enabled
              ? "Automatic updating is on."
              : "Automatic updating is off."}
          </span>{" "}
          <span className="text-content-muted">
            {engine.enabled
              ? `${engine.enabledPolicies ?? 0} of ${
                  engine.policies ?? 0
                } update policies are in force. A policy in Automatic mode will ` +
                "replace matching containers inside its maintenance window."
              : "Policies can be written and reviewed; nothing will act on them."}
          </span>{" "}
          <Link
            to="/automation"
            className="inline-flex min-h-6 items-center text-accent underline underline-offset-2"
          >
            Automatic updates
          </Link>
        </p>
      ) : null}
    </section>
  );
}

// --------------------------------------------------------------- system --

/**
 * Is HarborMaster itself working.
 *
 * Kept as its own panel and NOT collapsed. A degraded dependency also raises an
 * attention item above, and this is where an operator confirms the detail
 * without expanding anything.
 */
function SystemPanel({
  status,
  health,
}: {
  status: InventoryStatus;
  health: ResourceState<HealthReport>;
}) {
  const engine = useEventEngine();
  // Defensive: the API always sends `docker` (it is required by the schema),
  // but this reads from the network, and a malformed payload should degrade
  // one card rather than blank the whole dashboard with a render crash.
  const docker = status.docker ?? { status: "down" as const, detail: "status unavailable" };

  return (
    <section
      aria-labelledby="system-heading"
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <h2 id="system-heading" className="text-lg font-semibold">
        HarborMaster
      </h2>
      <p className="mt-1 text-sm text-content-muted">
        Everything above is read through these. If one is degraded, treat the
        rest of this page as possibly out of date.
      </p>

      <div className="mt-4 grid gap-3 sm:grid-cols-3">
        <div className="rounded-lg border border-border-subtle bg-surface-sunken p-4">
          <div className="flex items-start justify-between gap-2">
            <h3 className="font-medium">Docker</h3>
            <StatusBadge tone={componentTone(docker.status)} label={docker.status} />
          </div>
          <p className="mt-2 text-xs text-content-muted">
            Read-only connection to the Docker socket
            {docker.version ? ` — API ${docker.version}` : ""}
          </p>
          {docker.detail ? (
            <p className="mt-1 text-xs text-content-muted">{docker.detail}</p>
          ) : null}
        </div>

        <div className="rounded-lg border border-border-subtle bg-surface-sunken p-4">
          <div className="flex items-start justify-between gap-2">
            <h3 className="font-medium">Database</h3>
            {health.data ? (
              <StatusBadge
                tone={componentTone(health.data.database.status)}
                label={health.data.database.status}
              />
            ) : (
              <StatusBadge tone="neutral" label="unknown" />
            )}
          </div>
          <p className="mt-2 text-xs text-content-muted">
            Everything HarborMaster knows is stored here.
          </p>
        </div>

        <div className="rounded-lg border border-border-subtle bg-surface-sunken p-4">
          <div className="flex items-start justify-between gap-2">
            <h3 className="font-medium">Docker events</h3>
            {engine.data ? (
              <ConnectionStateBadge state={engine.data.state} />
            ) : (
              <StatusBadge tone="neutral" label="unknown" />
            )}
          </div>
          <p className="mt-2 text-xs text-content-muted">
            {engine.data
              ? engine.data.enabled
                ? engine.data.state === "connected"
                  ? "Container state is kept current by live events."
                  : "Falling back to periodic reconciliation; state may lag."
                : "Disabled by configuration. The inventory refreshes on a schedule."
              : "Event engine status is unavailable."}
          </p>
          <Link
            to="/events"
            className="mt-2 inline-flex min-h-6 items-center text-xs text-accent hover:underline"
          >
            Event log
          </Link>
        </div>
      </div>
    </section>
  );
}

// ------------------------------------------------------------- advanced --

/**
 * Everything an operator does not need on the first screen.
 *
 * Collapsed, not removed. Inventory generation, checksum, event counters, the
 * catalog counts and the refresh control all live here, and a `<details>`
 * element is used rather than a custom disclosure so it is keyboard-operable
 * and announced correctly without any code of ours.
 */
function AdvancedPanel({
  inventory,
}: {
  inventory: ResourceState<InventoryStatus>;
}) {
  const status = inventory.data!;
  const engine = useEventEngine();
  const build = useVersion();

  return (
    <details className="rounded-xl border border-border-subtle bg-surface-raised">
      <summary className="flex min-h-11 cursor-pointer items-center px-5 py-3 text-sm font-medium">
        Technical details
        <span className="ml-2 font-normal text-content-muted">
          inventory, event engine, catalog, build
        </span>
      </summary>

      <div className="flex flex-col gap-5 border-t border-border-subtle p-5">
        <InventorySection inventory={inventory} />

        {engine.data && engine.data.enabled ? (
          <section aria-labelledby="event-counters-heading">
            <h3 id="event-counters-heading" className="font-medium">
              Event engine counters
            </h3>
            <dl className="mt-2 grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">
              <Metric label="Last event" value={formatTimestamp(engine.data.lastEventAt)} />
              <Metric
                label="Last reconciliation"
                value={formatTimestamp(engine.data.lastReconciliationAt)}
              />
              <Metric label="Reconnects" value={String(engine.data.counters.reconnectCount)} />
              <Metric
                label="Queue"
                value={`${engine.data.queueDepth} / ${engine.data.queueCapacity}`}
              />
              <Metric label="Recorded events" value={String(engine.data.storedEvents)} />
              <Metric label="Received" value={String(engine.data.counters.eventsReceived)} />
              <Metric label="Dropped" value={String(engine.data.counters.eventsDropped)} />
              <Metric
                label="Targeted refreshes"
                value={String(engine.data.counters.targetedRefreshes)}
              />
            </dl>
            {engine.data.overflowPending ? (
              <p role="alert" className="mt-3 rounded-lg border border-warn/40 px-3 py-2 text-sm">
                The event queue overflowed. A full reconciliation is running to
                restore the inventory.
              </p>
            ) : null}
          </section>
        ) : null}

        <section aria-labelledby="catalog-heading">
          <h3 id="catalog-heading" className="font-medium">
            Catalog
          </h3>
          <div className="mt-2 grid gap-3 sm:grid-cols-3">
            <Tile label="Images" value={status.counts?.images ?? 0} />
            <Tile label="Networks" value={status.counts?.networks ?? 0} />
            <Tile label="Volumes" value={status.counts?.volumes ?? 0} />
          </div>
        </section>

        <section aria-labelledby="build-heading">
          <h3 id="build-heading" className="font-medium">
            Build
          </h3>
          {/*
            Version and platform only. The database's status is reported in the
            HarborMaster panel above, where it belongs -- repeating it here
            would give an operator two places to read one fact.
          */}
          <dl className="mt-2 grid gap-3 text-sm sm:grid-cols-2">
            <Metric label="Version" value={build.data?.version ?? "unavailable"} />
            <Metric label="Platform" value={build.data?.platform ?? "unavailable"} />
          </dl>
        </section>

        <WarningsSection status={status} />
      </div>
    </details>
  );
}

/** Refresh state, timings, and the manual refresh control. */
function InventorySection({ inventory }: { inventory: ResourceState<InventoryStatus> }) {
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
    <section aria-labelledby="inventory-heading">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h3 id="inventory-heading" className="font-medium">
            Inventory refresh
          </h3>
          <p className="mt-1 text-sm text-content-muted">{describeInventory(status)}</p>
        </div>

        <div className="flex flex-col items-end gap-2">
          <StatusBadge tone={refreshTone(status)} label={busy ? "refreshing" : status.state} />
          <button
            type="button"
            onClick={onRefresh}
            disabled={busy || !status.enabled}
            aria-busy={busy}
            className="min-h-11 rounded-lg border border-border-subtle bg-surface-raised px-4 py-2 text-sm font-medium transition-colors hover:bg-surface-sunken disabled:cursor-not-allowed disabled:opacity-50"
          >
            {busy ? "Refreshing…" : "Refresh now"}
          </button>
        </div>
      </div>

      <dl className="mt-3 grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">
        <Metric
          label="Generation"
          value={status.generation === 0 ? "none yet" : String(status.generation)}
        />
        <Metric label="Last success" value={formatTimestamp(status.lastSuccess?.finishedAt)} />
        <Metric label="Duration" value={formatDuration(status.lastSuccess?.durationMs)} />
        <Metric
          label="Checksum"
          value={status.checksum ? status.checksum.slice(0, 12) : "—"}
          mono
        />
      </dl>

      {feedback.kind !== "idle" ? (
        <p
          role="status"
          aria-live="polite"
          // Named, because the shell's connectivity indicator is also a status
          // region: without a label the two are indistinguishable to a screen
          // reader moving between them.
          aria-label="Refresh status"
          className={`mt-3 rounded-lg border px-3 py-2 text-sm ${feedbackClasses(feedback.kind)}`}
        >
          {feedback.message}
        </p>
      ) : null}

      {!status.enabled ? (
        <p className="mt-3 rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-sm">
          The inventory engine is disabled by configuration. The figures here
          describe the last inventory that was stored.
        </p>
      ) : null}
    </section>
  );
}

function WarningsSection({ status }: { status: InventoryStatus }) {
  const warnings = status.warnings ?? [];

  return (
    <section aria-labelledby="warnings-heading">
      <h3 id="warnings-heading" className="font-medium">
        Refresh warnings {warnings.length > 0 ? `(${warnings.length})` : ""}
      </h3>
      {warnings.length === 0 ? (
        <p className="mt-1 text-sm text-content-muted">
          The last refresh completed without warnings.
        </p>
      ) : (
        <>
          <p className="mt-1 text-sm text-content-muted">
            Non-fatal problems from the last refresh. A vanished container is
            expected churn, not a fault.
          </p>
          <ul className="mt-3 flex flex-col gap-2 text-sm">
            {warnings.slice(0, 10).map((warning, index) => (
              <li
                key={warning.id ?? `${warning.code}-${index}`}
                className="rounded-lg border border-border-subtle bg-surface-sunken px-3 py-2"
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
        </>
      )}
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
    return "The last refresh failed. The figures here describe the previous inventory.";
  }
  return `Last refreshed ${formatTimestamp(status.lastSuccess?.finishedAt)}.`;
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
  if (ms === undefined) return "—";
  if (ms < 1000) return `${ms} ms`;
  return `${(ms / 1000).toFixed(1)} s`;
}
