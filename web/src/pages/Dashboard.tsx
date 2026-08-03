import type { ResourceState } from "../hooks/useApiResource";
import type { HealthComponent, HealthReport } from "../api/types";
import { useVersion } from "../hooks/useHealth";
import {
  DisconnectedState,
  ErrorState,
  LoadingState,
} from "../components/States";
import {
  StatusBadge,
  componentTone,
  overallTone,
} from "../components/StatusBadge";

/**
 * The dashboard renders only data the API actually returned. There is no
 * sample content: an operations view showing invented numbers is worse than
 * one showing nothing.
 */
export function Dashboard({ health }: { health: ResourceState<HealthReport> }) {
  if (health.status === "loading") {
    return <LoadingState label="Loading system status" />;
  }
  if (health.status === "disconnected") {
    return <DisconnectedState onRetry={health.refresh} />;
  }
  if (health.error) {
    return <ErrorState error={health.error} onRetry={health.refresh} />;
  }
  if (!health.data) {
    return <LoadingState label="Loading system status" />;
  }

  const report = health.data;

  return (
    <div className="flex flex-col gap-6">
      <section
        aria-labelledby="system-status-heading"
        className="rounded-xl border border-border-subtle bg-surface-raised p-5"
      >
        <div className="flex flex-wrap items-center justify-between gap-3">
          <h2 id="system-status-heading" className="text-lg font-semibold">
            System status
          </h2>
          <StatusBadge tone={overallTone(report.status)} label={report.status} />
        </div>
        <p className="mt-2 text-sm text-content-muted">
          Checked {formatTimestamp(report.checkedAt)} &middot; up for{" "}
          {formatUptime(report.uptimeSeconds)}
          {health.refreshing ? " · refreshing" : ""}
        </p>
      </section>

      <div className="grid gap-4 sm:grid-cols-2">
        <ComponentCard
          name="Database"
          description="SQLite store for snapshots and events"
          component={report.database}
        />
        <ComponentCard
          name="Docker Engine"
          description="Read-only connection to the Docker socket"
          component={report.docker}
        />
      </div>

      <BuildCard />
    </div>
  );
}

function ComponentCard({
  name,
  description,
  component,
}: {
  name: string;
  description: string;
  component: HealthComponent;
}) {
  return (
    <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h3 className="font-semibold">{name}</h3>
          <p className="mt-1 text-sm text-content-muted">{description}</p>
        </div>
        <StatusBadge
          tone={componentTone(component.status)}
          label={component.status}
        />
      </div>

      <dl className="mt-4 grid grid-cols-2 gap-3 text-sm">
        {component.version ? (
          <Field label="API version" value={component.version} />
        ) : null}
        {typeof component.latencyMs === "number" ? (
          <Field label="Latency" value={`${component.latencyMs} ms`} />
        ) : null}
        {component.detail ? (
          <div className="col-span-2">
            <dt className="text-content-muted">Detail</dt>
            <dd className="mt-0.5">{component.detail}</dd>
          </div>
        ) : null}
      </dl>
    </section>
  );
}

function BuildCard() {
  const build = useVersion();

  if (build.status === "loading") {
    return <LoadingState label="Loading build information" />;
  }
  // The shell already reports connectivity, so a failure here is a quiet note
  // rather than a second alarming banner.
  if (!build.data) {
    return (
      <section className="rounded-xl border border-border-subtle bg-surface-raised p-5 text-sm text-content-muted">
        Build information is unavailable.
      </section>
    );
  }

  return (
    <section
      aria-labelledby="build-heading"
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <h2 id="build-heading" className="text-lg font-semibold">
        Build
      </h2>
      <dl className="mt-4 grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">
        <Field label="Version" value={build.data.version} />
        <Field label="Commit" value={build.data.commit} mono />
        <Field label="Go" value={build.data.goVersion} />
        <Field label="Platform" value={build.data.platform} />
      </dl>
    </section>
  );
}

function Field({
  label,
  value,
  mono = false,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div>
      <dt className="text-content-muted">{label}</dt>
      <dd className={mono ? "mt-0.5 font-mono text-xs" : "mt-0.5"}>{value}</dd>
    </div>
  );
}

function formatUptime(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ${minutes % 60}m`;
  return `${Math.floor(hours / 24)}d ${hours % 24}h`;
}

function formatTimestamp(iso: string): string {
  const parsed = new Date(iso);
  return Number.isNaN(parsed.getTime()) ? iso : parsed.toLocaleTimeString();
}
