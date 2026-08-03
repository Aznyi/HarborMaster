import type { ResourceState } from "../hooks/useApiResource";
import type { HealthReport } from "../api/types";
import { useVersion } from "../hooks/useHealth";
import { PageIntro } from "../components/PageIntro";
import { LoadingState } from "../components/States";
import { StatusBadge, componentTone } from "../components/StatusBadge";

/**
 * Settings is read-only.
 *
 * Configuration is supplied through environment variables and the API
 * deliberately does not echo their values back, so this page reports observed
 * state and points at the variable names instead.
 */
export function Settings({ health }: { health: ResourceState<HealthReport> }) {
  const build = useVersion();

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Settings"
        description="HarborMaster is configured entirely through environment variables. Values are never displayed here or written to the log, because the same mechanism will eventually carry credentials."
      />

      <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <h3 className="font-semibold">Observed state</h3>
        <dl className="mt-4 flex flex-col gap-3 text-sm">
          <Row label="Docker Engine">
            {health.data ? (
              <StatusBadge
                tone={componentTone(health.data.docker.status)}
                label={health.data.docker.status}
              />
            ) : (
              <span className="text-content-muted">unknown</span>
            )}
          </Row>
          <Row label="Database">
            {health.data ? (
              <StatusBadge
                tone={componentTone(health.data.database.status)}
                label={health.data.database.status}
              />
            ) : (
              <span className="text-content-muted">unknown</span>
            )}
          </Row>
        </dl>
      </section>

      <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <h3 className="font-semibold">Build</h3>
        {build.status === "loading" ? (
          <div className="mt-4">
            <LoadingState label="Loading build information" />
          </div>
        ) : build.data ? (
          <dl className="mt-4 grid gap-3 text-sm sm:grid-cols-2">
            <Row label="Version">{build.data.version}</Row>
            <Row label="Commit">
              <span className="font-mono text-xs">{build.data.commit}</span>
            </Row>
            <Row label="Built">{build.data.buildDate}</Row>
            <Row label="Platform">{build.data.platform}</Row>
          </dl>
        ) : (
          <p className="mt-4 text-sm text-content-muted">
            Build information is unavailable.
          </p>
        )}
      </section>

      <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <h3 className="font-semibold">Configuration reference</h3>
        <p className="mt-2 text-sm text-content-muted">
          Set these in the environment or in <code>.env</code>. See{" "}
          <code>.env.example</code> for defaults and full descriptions.
        </p>
        <ul className="mt-4 flex flex-col gap-1 font-mono text-xs text-content-muted">
          {CONFIG_VARIABLES.map((name) => (
            <li key={name}>{name}</li>
          ))}
        </ul>
      </section>
    </div>
  );
}

/**
 * Variable names only. Their values are never fetched or rendered.
 */
const CONFIG_VARIABLES = [
  "HARBORMASTER_HTTP_ADDR",
  "HARBORMASTER_MAX_REQUEST_BYTES",
  "HARBORMASTER_READ_HEADER_TIMEOUT",
  "HARBORMASTER_READ_TIMEOUT",
  "HARBORMASTER_WRITE_TIMEOUT",
  "HARBORMASTER_IDLE_TIMEOUT",
  "HARBORMASTER_SHUTDOWN_TIMEOUT",
  "HARBORMASTER_DOCKER_HOST",
  "HARBORMASTER_DOCKER_TIMEOUT",
  "HARBORMASTER_DB_PATH",
  "HARBORMASTER_LOG_LEVEL",
  "HARBORMASTER_LOG_FORMAT",
] as const;

function Row({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex items-center justify-between gap-4 sm:block">
      <dt className="text-content-muted">{label}</dt>
      <dd className="sm:mt-0.5">{children}</dd>
    </div>
  );
}
