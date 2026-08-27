import type { ResourceState } from "../hooks/useApiResource";
import type { Features, HealthReport } from "../api/types";
import { useVersion } from "../hooks/useHealth";
import { PageIntro } from "../components/PageIntro";
import { LoadingState } from "../components/States";
import { StatusBadge, componentTone } from "../components/StatusBadge";

/**
 * Settings is read-only, and says which capabilities this deployment has.
 *
 * # Why the feature list is the important part of this page
 *
 * Almost everything HarborMaster can do to a host is OFF by default. That is
 * the right default and it produces one recurring confusion: an operator looks
 * at an empty Acquisitions page, or a Notifications page with no history, and
 * cannot tell "switched off" from "not working". Those lead somewhere very
 * different — one is a compose file to edit, the other is a bug to report.
 *
 * So this page states, plainly, what exists in this process. The values that
 * produced those states are never shown: configuration arrives through
 * environment variables and the API deliberately does not echo them back,
 * because the same mechanism carries credentials.
 */
export function Settings({ health }: { health: ResourceState<HealthReport> }) {
  const build = useVersion();
  const features = health.data?.features;

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Settings"
        description="HarborMaster is configured entirely through environment variables. Values are never displayed here or written to the log, because the same mechanism carries credentials. What this page shows is what the running process can actually do."
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
          {health.data?.events ? (
            <Row label="Docker event stream">
              <StatusBadge
                tone={componentTone(health.data.events.status)}
                label={health.data.events.status}
              />
            </Row>
          ) : null}
        </dl>
      </section>

      <FeatureSections features={features} />

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
        <h3 className="font-semibold">Where to change any of this</h3>
        <p className="mt-2 text-sm text-content-muted">
          Every setting lives in the environment. The supported deployment
          forwards them through{" "}
          <code className="font-mono text-xs">deployments/compose.yaml</code>;{" "}
          <code className="font-mono text-xs">.env.example</code> documents all
          of them, including what happens when each is wrong. Restart the
          container for a change to take effect.
        </p>
        <ul className="mt-4 list-disc space-y-1 pl-5 text-sm text-content-muted">
          <li>
            <strong className="text-content">Backups and recovery.</strong>{" "}
            HarborMaster does not back itself up on a schedule.{" "}
            <code className="font-mono text-xs">harbormaster backup</code>{" "}
            writes a consistent copy; see{" "}
            <code className="font-mono text-xs">
              docs/engineering/reliability.md
            </code>
            .
          </li>
          <li>
            <strong className="text-content">TLS.</strong> HarborMaster does not
            terminate it. Keep the port on loopback, or put a reverse proxy in
            front and set{" "}
            <code className="font-mono text-xs">
              HARBORMASTER_TRUSTED_PROXIES
            </code>{" "}
            and{" "}
            <code className="font-mono text-xs">
              HARBORMASTER_COOKIE_SECURE
            </code>
            .
          </li>
          <li>
            <strong className="text-content">Updating HarborMaster.</strong> It
            refuses to update itself, and that is not configurable. Run{" "}
            <code className="font-mono text-xs">
              docker compose pull &amp;&amp; docker compose up -d
            </code>{" "}
            from outside the container.
          </li>
        </ul>
      </section>
    </div>
  );
}

/** The capabilities this process has, grouped by what they can do to a host. */
function FeatureSections({ features }: { features?: Features }) {
  if (!features) {
    return (
      <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <h3 className="font-semibold">Features</h3>
        <p className="mt-2 text-sm text-content-muted">
          This deployment did not report which features are enabled.
        </p>
      </section>
    );
  }

  return (
    <>
      <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <h3 className="font-semibold">Observation</h3>
        <p className="mt-2 text-sm text-content-muted">
          These read the host and HarborMaster&rsquo;s own records. None of them
          can change a container.
        </p>
        <dl className="mt-4 grid gap-3 text-sm sm:grid-cols-2">
          <Feature label="Inventory" on={features.inventory} />
          <Feature label="Docker events" on={features.events} />
          <Feature label="Configuration snapshots" on={features.snapshots} />
          <Feature label="Drift detection" on={features.drift} />
          <Feature label="Compliance policy" on={features.policy} />
          <Feature label="Change plans" on={features.planner} />
          <Feature
            label="Image intelligence"
            on={features.imageIntel}
            note="Outbound: anonymous HTTPS to the registries your images come from."
          />
        </dl>
      </section>

      <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <h3 className="font-semibold">What this deployment may do to the host</h3>
        <p className="mt-2 text-sm text-content-muted">
          Each is a separate capability, off unless it was asked for. When one is
          off the interface is never wired, so the ability is{" "}
          <strong className="text-content">absent</strong> rather than merely
          unused — no request can turn it back on.
        </p>
        <dl className="mt-4 grid gap-3 text-sm sm:grid-cols-2">
          <Feature
            label="Download images"
            on={features.acquisition}
            note="Pulls an approved, digest-pinned image. Touches no container."
          />
          <Feature
            label="Recreate containers"
            on={features.execution}
            note="STOPS A RUNNING CONTAINER and replaces it with one built from its own recorded configuration."
            dangerous
          />
          <Feature
            label="Roll back"
            on={features.rollback}
            note="Stops the replacement and starts the original. There is a gap between the two."
            dangerous
          />
          <Feature
            label="Unattended updates"
            on={features.automation}
            note="Changes containers on a timer, with nobody watching. What it may touch is entirely the business of update policies."
            dangerous
          />
        </dl>

        {/* The two read-only engines an operator asks about when the automation
          * page says nothing is happening.
          *
          * Neither touches the host, so they sit below the four that do and
          * carry no danger styling. They are here because "assessment is
          * switched off" and "nothing needs updating" look identical from the
          * automation page, and this is where the difference is settled.
          */}
        <h4 className="mt-5 font-semibold">What it needs before it may act</h4>
        <dl className="mt-3 grid gap-3 text-sm sm:grid-cols-2">
          <Feature
            label="Assess updates"
            on={features.planner}
            note="Works out whether a newer image exists and how large the change would be. Without it there is nothing for a policy to act on, and no container is ever reported as eligible."
          />
          <Feature
            label="Record configuration"
            on={features.snapshots}
            note="Captures what a container looked like before it is changed. An update with no baseline to compare against is refused rather than performed."
          />
        </dl>
      </section>

      <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <h3 className="font-semibold">Notifications</h3>
        <dl className="mt-4 grid gap-3 text-sm sm:grid-cols-2">
          <Feature
            label="Delivery"
            on={features.notifications}
            note="HarborMaster's second outbound egress. Destinations and rules stay editable when this is off; nothing is sent."
          />
          <Feature
            label="Private destinations allowed"
            on={features.notificationsAllowPrivate}
            note="Permits a destination on a loopback, private, or unique-local address. Link-local, multicast, and the cloud metadata endpoint are refused whatever this says."
            dangerous={features.notificationsAllowPrivate}
          />
        </dl>
      </section>
    </>
  );
}

/**
 * One capability, and what it means.
 *
 * `dangerous` marks the ones that change a host, and it only warns when the
 * capability is ON: an alarming colour beside "off" would make the alarming one
 * ordinary.
 */
function Feature({
  label,
  on,
  note,
  dangerous = false,
}: {
  label: string;
  on: boolean;
  note?: string;
  dangerous?: boolean;
}) {
  return (
    <div>
      <dt className="flex items-center gap-2">
        <StatusBadge
          tone={on ? (dangerous ? "warn" : "ok") : "neutral"}
          label={on ? "on" : "off"}
        />
        <span className="font-medium">{label}</span>
      </dt>
      {note ? (
        <dd className="mt-1 text-xs text-content-muted">{note}</dd>
      ) : null}
    </div>
  );
}

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
