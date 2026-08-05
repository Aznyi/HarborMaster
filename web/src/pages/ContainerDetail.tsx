import { useCallback, useMemo, useState } from "react";
import { Link, useParams } from "react-router";

import { getContainerRaw } from "../api/client";
import type {
  ContainerDetail as ContainerDetailData,
  EnvVar,
  RawInspection,
} from "../api/inventoryTypes";
import { useApiResource } from "../hooks/useApiResource";
import { useContainerDetail } from "../hooks/useContainers";
import { useDockerEventPage } from "../hooks/useDockerEvents";
import { useSnapshots } from "../hooks/useSnapshots";
import { useContainerPolicy } from "../hooks/usePolicies";
import {
  PolicySeverityBadge,
  PolicyStatusBadge,
} from "../components/PolicyBadges";
import { RULE_LABELS } from "../api/policyTypes";
import { EventResultBadge } from "../components/EventBadges";
import { ReadinessBadge, TriggerBadge } from "../components/SnapshotBadges";
import {
  BoolField,
  CodeBlock,
  DetailSection,
  Field,
  FieldList,
  ScrollArea,
} from "../components/DetailSection";
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

const TABS = [
  "Overview",
  "Configuration",
  "Environment",
  "Mounts",
  "Networks",
  "Ports",
  "Resources",
  "Security",
  "Labels",
  "Compose",
  "Snapshots",
  "Policy",
  "Events",
  "Raw inspection",
] as const;

type Tab = (typeof TABS)[number];

/**
 * How many recent events the detail view shows.
 *
 * Small on purpose: this is a supporting panel on a page about current state,
 * and an unbounded event query here would compete with the primary Events page
 * for no benefit.
 */
const RECENT_EVENT_LIMIT = 20;

export function ContainerDetailPage() {
  const { id = "" } = useParams();
  const detail = useContainerDetail(id);
  const [tab, setTab] = useState<Tab>("Overview");

  if (detail.status === "loading") {
    return <LoadingState label="Loading container" />;
  }
  if (detail.status === "disconnected") {
    return <DisconnectedState onRetry={detail.refresh} />;
  }
  if (detail.error) {
    return <ErrorState error={detail.error} onRetry={detail.refresh} />;
  }
  if (!detail.data) {
    return <LoadingState label="Loading container" />;
  }

  const container = detail.data;

  return (
    <div className="flex flex-col gap-6">
      <header>
        <Link to="/containers" className="text-sm text-accent hover:underline">
          &larr; All containers
        </Link>
        <div className="mt-2 flex flex-wrap items-center gap-3">
          <h2 className="text-xl font-semibold tracking-tight">
            {container.overview.name || container.overview.shortId}
          </h2>
          <ContainerStateBadge state={container.overview.state} />
          <ContainerHealthBadge health={container.overview.health} />
        </div>
        <p className="mt-1 font-mono text-xs text-content-muted">{container.overview.id}</p>
        {!container.overview.present ? (
          <p className="mt-2 rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-sm">
            This container was not seen in the most recent refresh. The record is
            retained from generation {container.overview.generation}.
          </p>
        ) : null}
      </header>

      {container.warnings.length > 0 ? <WarningList detail={container} /> : null}

      <div role="tablist" aria-label="Container sections" className="flex flex-wrap gap-1">
        {TABS.map((name) => (
          <button
            key={name}
            role="tab"
            type="button"
            aria-selected={tab === name}
            onClick={() => setTab(name)}
            className={`rounded-lg px-3 py-1.5 text-sm font-medium transition-colors ${
              tab === name
                ? "bg-accent-soft text-accent"
                : "text-content-muted hover:bg-surface-sunken hover:text-content"
            }`}
          >
            {name}
          </button>
        ))}
      </div>

      <div role="tabpanel" aria-label={tab}>
        {tab === "Overview" && <OverviewTab detail={container} />}
        {tab === "Configuration" && <ConfigurationTab detail={container} />}
        {tab === "Environment" && <EnvironmentTab detail={container} />}
        {tab === "Mounts" && <MountsTab detail={container} />}
        {tab === "Networks" && <NetworksTab detail={container} />}
        {tab === "Ports" && <PortsTab detail={container} />}
        {tab === "Resources" && <ResourcesTab detail={container} />}
        {tab === "Security" && <SecurityTab detail={container} />}
        {tab === "Labels" && <LabelsTab detail={container} />}
        {tab === "Compose" && <ComposeTab detail={container} />}
        {tab === "Snapshots" && <SnapshotsTab id={container.overview.id} />}
        {tab === "Policy" && <PolicyTab id={container.overview.id} />}
        {tab === "Events" && <EventsTab id={container.overview.id} />}
        {tab === "Raw inspection" && <RawTab id={container.overview.id} />}
      </div>
    </div>
  );
}

/**
 * The container's recent Docker events.
 *
 * Bounded to one small page and served by the existing events API filtered on
 * actor ID -- no new endpoint, and no unbounded query on a detail page. It is
 * an observation log, not a state record: what the container IS now is on the
 * other tabs, which read the inventory.
 */
/** How many snapshots the container detail lists. */
const RECENT_SNAPSHOT_LIMIT = 10;

/**
 * This container's snapshot history.
 *
 * Read-only, like every other tab. There is no capture button here and no
 * restore control: capture is a deliberate operator action taken from the
 * snapshots API, and restore does not exist.
 */
function SnapshotsTab({ id }: { id: string }) {
  const query = useMemo(
    () => ({ containerId: id, pageSize: RECENT_SNAPSHOT_LIMIT, page: 1 }),
    [id],
  );
  const page = useSnapshots(query);

  if (page.status === "loading") {
    return <LoadingState label="Loading snapshots" />;
  }
  if (page.status === "disconnected") {
    return <DisconnectedState onRetry={page.refresh} />;
  }
  if (page.error) {
    return <ErrorState error={page.error} onRetry={page.refresh} />;
  }

  const snapshots = page.data?.items ?? [];

  return (
    <DetailSection
      title="Configuration snapshots"
      description={
        `The ${RECENT_SNAPSHOT_LIMIT} most recent captures of this container's ` +
        "configuration, newest first. Snapshots are immutable records; HarborMaster " +
        "cannot restore one."
      }
    >
      {snapshots.length === 0 ? (
        <p className="text-sm text-content-muted">
          No snapshots have been captured for this container.
        </p>
      ) : (
        <ul className="flex flex-col gap-2">
          {snapshots.map((snapshot) => (
            <li
              key={snapshot.id}
              className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-border-subtle bg-surface-sunken px-3 py-2 text-sm"
            >
              <span className="flex flex-wrap items-center gap-2">
                <Link
                  to={`/snapshots/${snapshot.id}`}
                  className="font-medium text-accent hover:underline"
                >
                  <time dateTime={snapshot.createdAt}>
                    {new Date(snapshot.createdAt).toLocaleString()}
                  </time>
                </Link>
                <TriggerBadge trigger={snapshot.trigger} />
                <ReadinessBadge status={snapshot.readinessStatus} />
              </span>
              <Link
                to={`/snapshots/${snapshot.id}`}
                className="text-xs text-accent hover:underline"
              >
                Compare with current
              </Link>
            </li>
          ))}
        </ul>
      )}
    </DetailSection>
  );
}

function EventsTab({ id }: { id: string }) {
  const query = useMemo(
    () => ({ actorId: id, pageSize: RECENT_EVENT_LIMIT, page: 1 }),
    [id],
  );
  const page = useDockerEventPage(query);

  if (page.status === "loading") {
    return <LoadingState label="Loading events" />;
  }
  if (page.status === "disconnected") {
    return <DisconnectedState onRetry={page.refresh} />;
  }
  if (page.error) {
    return <ErrorState error={page.error} onRetry={page.refresh} />;
  }

  const events = page.data?.items ?? [];

  return (
    <DetailSection
      title="Recent events"
      description={
        `The most recent ${RECENT_EVENT_LIMIT} Docker events naming this container, ` +
        "newest first. HarborMaster records what the daemon reported; it does not " +
        "reconstruct state from these."
      }
    >
      <p className="mb-3 text-sm text-content-muted">
        See{" "}
        <Link to="/events" className="text-accent hover:underline">
          Events
        </Link>{" "}
        for the full log and its filters.
      </p>

      {events.length === 0 ? (
        <p className="text-sm text-content-muted">
          No events have been recorded for this container. Event history begins
          when the engine first connects; anything earlier was not observed.
        </p>
      ) : (
        <ScrollArea>
          <table className="w-full min-w-[36rem] text-left text-sm">
            <caption className="sr-only">Recent events for this container</caption>
            <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
              <tr>
                <th scope="col" className="px-3 py-2 font-medium">
                  Docker time
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  Observed
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  Action
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  Status
                </th>
              </tr>
            </thead>
            <tbody>
              {events.map((event) => (
                <tr key={event.sequence} className="border-b border-border-subtle last:border-0">
                  <td className="px-3 py-2 text-xs text-content-muted" title={event.dockerTime}>
                    {formatEventTime(event.dockerTime)}
                  </td>
                  <td className="px-3 py-2 text-xs text-content-muted" title={event.observedAt}>
                    {formatEventTime(event.observedAt)}
                  </td>
                  <td className="px-3 py-2 font-mono text-xs">{event.action || "â€”"}</td>
                  <td className="px-3 py-2">
                    <EventResultBadge result={event.result} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </ScrollArea>
      )}
    </DetailSection>
  );
}

function formatEventTime(iso: string | undefined): string {
  if (!iso) return "â€”";
  const parsed = new Date(iso);
  return Number.isNaN(parsed.getTime()) ? iso : parsed.toLocaleString();
}

function WarningList({ detail }: { detail: ContainerDetailData }) {
  return (
    <section className="rounded-xl border border-warn/40 bg-warn-soft p-4">
      <h3 className="font-semibold">Inventory warnings</h3>
      <ul className="mt-2 flex flex-col gap-1 text-sm">
        {detail.warnings.map((warning, index) => (
          <li key={warning.id ?? index}>
            <span className="font-mono text-xs text-content-muted">{warning.code}</span>{" "}
            {warning.message}
          </li>
        ))}
      </ul>
    </section>
  );
}

function OverviewTab({ detail }: { detail: ContainerDetailData }) {
  const { overview, state } = detail;

  return (
    <div className="flex flex-col gap-4">
      <DetailSection title="Identity">
        <FieldList>
          <Field label="Name" value={overview.name} />
          <Field label="Short ID" value={overview.shortId} mono />
          <Field label="Container ID" value={overview.id} mono span />
          <Field label="Host" value={overview.hostId} />
          <Field label="Image" value={overview.image.raw} mono />
          <Field label="Image ID" value={overview.imageId} mono />
          <Field label="Created" value={formatTimestamp(overview.createdAt)} />
          <Field label="Started" value={formatTimestamp(overview.startedAt)} />
          <Field label="Finished" value={formatTimestamp(overview.finishedAt)} />
        </FieldList>
      </DetailSection>

      <DetailSection title="State">
        <FieldList>
          <Field label="State" value={state.state} />
          <Field label="Raw state" value={state.rawState} />
          <Field label="Status" value={state.status} />
          <Field label="Health" value={state.health} />
          <Field
            label="Exit code"
            value={state.exitCode === undefined ? undefined : String(state.exitCode)}
          />
          <Field label="Restart count" value={String(state.restartCount)} />
          <Field label="Restart policy" value={overview.restartPolicy.name} />
          <Field label="Error" value={state.error} span />
          <BoolField label="Running" value={state.running} />
          <BoolField label="Paused" value={state.paused} />
          <BoolField label="Restarting" value={state.restarting} />
          <BoolField label="Dead" value={state.dead} />
          <BoolField label="OOM killed" value={state.oomKilled} />
          <Field
            label="Health failing streak"
            value={state.healthFailingStreak ? String(state.healthFailingStreak) : undefined}
          />
        </FieldList>
      </DetailSection>

      {detail.image ? (
        <DetailSection title="Image">
          <FieldList>
            <Field label="ID" value={detail.image.id} mono span />
            <Field label="Repository tags" value={detail.image.repoTags.join(", ")} span />
            <Field label="Repository digests" value={detail.image.repoDigests.join(", ")} mono span />
            <Field label="Created" value={formatTimestamp(detail.image.createdAt)} />
            <Field label="Size" value={formatBytes(detail.image.size)} />
            <Field label="Architecture" value={detail.image.architecture} />
            <Field label="OS" value={detail.image.os} />
          </FieldList>
        </DetailSection>
      ) : null}
    </div>
  );
}

function ConfigurationTab({ detail }: { detail: ContainerDetailData }) {
  const { process, healthCheck, logging } = detail;

  return (
    <div className="flex flex-col gap-4">
      <DetailSection title="Process">
        <FieldList>
          <Field label="Hostname" value={process.hostname} />
          <Field label="Domain name" value={process.domainname} />
          <Field label="User" value={process.user || "image default"} />
          <Field label="Working directory" value={process.workingDir} mono />
          <Field label="Entrypoint" value={process.entrypoint?.join(" ")} mono span />
          <Field label="Command" value={process.command?.join(" ")} mono span />
          <Field label="Stop signal" value={process.stopSignal} />
          <Field
            label="Stop timeout"
            value={
              process.stopTimeoutSeconds === undefined
                ? undefined
                : `${process.stopTimeoutSeconds}s`
            }
          />
          <BoolField label="TTY" value={process.tty} />
          <BoolField label="Stdin open" value={process.stdinOpen} />
        </FieldList>
      </DetailSection>

      <DetailSection
        title="Health check"
        description={
          healthCheck
            ? undefined
            : "This container declares no healthcheck."
        }
      >
        {healthCheck ? (
          <FieldList>
            <Field label="Test" value={healthCheck.test?.join(" ")} mono span />
            <Field label="Interval" value={formatMs(healthCheck.intervalMs)} />
            <Field label="Timeout" value={formatMs(healthCheck.timeoutMs)} />
            <Field label="Start period" value={formatMs(healthCheck.startPeriodMs)} />
            <Field label="Start interval" value={formatMs(healthCheck.startIntervalMs)} />
            <Field
              label="Retries"
              value={healthCheck.retries ? String(healthCheck.retries) : undefined}
            />
            <BoolField label="Disabled" value={healthCheck.disabled} />
          </FieldList>
        ) : null}
      </DetailSection>

      <DetailSection
        title="Logging"
        description="Option values are masked with the same rules as the environment: log drivers routinely carry endpoint credentials."
      >
        <FieldList>
          <Field label="Driver" value={logging.driver} />
        </FieldList>
        {logging.options && logging.options.length > 0 ? (
          <div className="mt-4">
            <EnvTable vars={logging.options} caption="Logging options" />
          </div>
        ) : null}
      </DetailSection>
    </div>
  );
}

function EnvironmentTab({ detail }: { detail: ContainerDetailData }) {
  return (
    <DetailSection
      title="Environment"
      description="Values whose variable name matches a configured pattern are masked. HarborMaster provides no way to reveal them: with no authentication in place, such a control would be an unauthenticated secret-disclosure feature."
    >
      {detail.environment.length === 0 ? (
        <p className="text-sm text-content-muted">
          No environment variables are recorded for this container.
        </p>
      ) : (
        <EnvTable vars={detail.environment} caption="Environment variables" />
      )}
    </DetailSection>
  );
}

function EnvTable({ vars, caption }: { vars: EnvVar[]; caption: string }) {
  return (
    <ScrollArea>
      <table className="w-full min-w-[32rem] text-left text-sm">
        <caption className="sr-only">{caption}</caption>
        <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
          <tr>
            <th scope="col" className="px-3 py-2 font-medium">
              Name
            </th>
            <th scope="col" className="px-3 py-2 font-medium">
              Value
            </th>
          </tr>
        </thead>
        <tbody>
          {vars.map((variable) => (
            <tr key={variable.name} className="border-b border-border-subtle last:border-0">
              <td className="px-3 py-2 font-mono text-xs">{variable.name}</td>
              <td className="px-3 py-2 font-mono text-xs">
                {variable.sensitivity === "sensitive" ? (
                  <span className="inline-flex items-center gap-2">
                    <span aria-label="masked value">{variable.value}</span>
                    <span className="rounded border border-warn/40 bg-warn-soft px-1.5 py-0.5 text-[10px] font-medium not-italic">
                      masked
                    </span>
                  </span>
                ) : (
                  <span className="break-all">{variable.value}</span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </ScrollArea>
  );
}

function MountsTab({ detail }: { detail: ContainerDetailData }) {
  return (
    <DetailSection
      title="Mounts"
      description="HarborMaster records where a mount points, never what it contains."
    >
      {detail.mounts.length === 0 ? (
        <p className="text-sm text-content-muted">This container has no mounts.</p>
      ) : (
        <ScrollArea>
          <table className="w-full min-w-[40rem] text-left text-sm">
            <caption className="sr-only">Mounts</caption>
            <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
              <tr>
                <th scope="col" className="px-3 py-2 font-medium">Destination</th>
                <th scope="col" className="px-3 py-2 font-medium">Type</th>
                <th scope="col" className="px-3 py-2 font-medium">Source</th>
                <th scope="col" className="px-3 py-2 font-medium">Access</th>
              </tr>
            </thead>
            <tbody>
              {detail.mounts.map((mount) => (
                <tr key={mount.destination} className="border-b border-border-subtle last:border-0">
                  <td className="px-3 py-2 font-mono text-xs">{mount.destination}</td>
                  <td className="px-3 py-2">{mount.type}</td>
                  <td className="px-3 py-2 font-mono text-xs break-all">
                    {mount.volumeName || mount.source || mount.tmpfsOptions || "â€”"}
                  </td>
                  <td className="px-3 py-2">{mount.readOnly ? "read-only" : "read-write"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </ScrollArea>
      )}
    </DetailSection>
  );
}

function NetworksTab({ detail }: { detail: ContainerDetailData }) {
  return (
    <DetailSection title="Networks">
      {detail.networks.length === 0 ? (
        <p className="text-sm text-content-muted">This container is not attached to a network.</p>
      ) : (
        <div className="flex flex-col gap-4">
          {detail.networks.map((network) => (
            <div
              key={network.networkName}
              className="rounded-lg border border-border-subtle bg-surface-sunken p-4"
            >
              <h4 className="font-medium">{network.networkName}</h4>
              <FieldList>
                <Field label="IPv4" value={network.ipv4Address} mono />
                <Field label="IPv6" value={network.ipv6Address} mono />
                <Field label="Gateway" value={network.gateway} mono />
                <Field label="MAC address" value={network.macAddress} mono />
                <Field label="Aliases" value={network.aliases?.join(", ")} />
                <Field label="Network ID" value={network.networkId} mono />
              </FieldList>
            </div>
          ))}
        </div>
      )}
    </DetailSection>
  );
}

function PortsTab({ detail }: { detail: ContainerDetailData }) {
  return (
    <DetailSection
      title="Ports"
      description="Only published ports are reachable from the host. An exposed port with no binding is declared by the image but not bound."
    >
      {detail.ports.length === 0 ? (
        <p className="text-sm text-content-muted">This container exposes no ports.</p>
      ) : (
        <ScrollArea>
          <table className="w-full min-w-[32rem] text-left text-sm">
            <caption className="sr-only">Ports</caption>
            <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
              <tr>
                <th scope="col" className="px-3 py-2 font-medium">Container port</th>
                <th scope="col" className="px-3 py-2 font-medium">Protocol</th>
                <th scope="col" className="px-3 py-2 font-medium">Host binding</th>
              </tr>
            </thead>
            <tbody>
              {detail.ports.map((port, index) => (
                <tr
                  key={`${port.containerPort}-${port.protocol}-${index}`}
                  className="border-b border-border-subtle last:border-0"
                >
                  <td className="px-3 py-2 font-mono text-xs">{port.containerPort}</td>
                  <td className="px-3 py-2">{port.protocol}</td>
                  <td className="px-3 py-2 font-mono text-xs">
                    {port.published
                      ? `${port.hostIp ?? "0.0.0.0"}:${port.hostPort}`
                      : "not published"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </ScrollArea>
      )}
    </DetailSection>
  );
}

function ResourcesTab({ detail }: { detail: ContainerDetailData }) {
  const { resources } = detail;

  return (
    <DetailSection
      title="Resources"
      description="An absent value means the limit is not set, which is different from being set to zero."
    >
      <FieldList>
        <Field label="CPU shares" value={nonZero(resources.cpuShares)} />
        <Field label="CPU quota" value={nonZero(resources.cpuQuota)} />
        <Field label="CPU period" value={nonZero(resources.cpuPeriod)} />
        <Field
          label="NanoCPUs"
          value={resources.nanoCpus ? `${(resources.nanoCpus / 1e9).toFixed(2)} CPUs` : undefined}
        />
        <Field label="Cpuset CPUs" value={resources.cpusetCpus} />
        <Field label="Cpuset memory nodes" value={resources.cpusetMems} />
        <Field label="Memory limit" value={formatBytesOrUndefined(resources.memoryBytes)} />
        <Field label="Memory reservation" value={formatBytesOrUndefined(resources.memoryReservationBytes)} />
        <Field label="Memory swap" value={formatBytesOrUndefined(resources.memorySwapBytes)} />
        <Field label="Memory swappiness" value={optionalNumber(resources.memorySwappiness)} />
        <Field label="Kernel memory" value={formatBytesOrUndefined(resources.kernelMemoryBytes)} />
        <Field label="PIDs limit" value={optionalNumber(resources.pidsLimit)} />
        <Field label="Blkio weight" value={nonZero(resources.blkioWeight)} />
        <Field label="Shared memory" value={formatBytesOrUndefined(resources.shmSizeBytes)} />
        <Field label="OOM score adjust" value={nonZero(resources.oomScoreAdj)} />
        <Field
          label="OOM kill disabled"
          value={resources.oomKillDisable === undefined ? undefined : String(resources.oomKillDisable)}
        />
      </FieldList>

      {resources.ulimits && resources.ulimits.length > 0 ? (
        <div className="mt-4">
          <h4 className="text-sm font-medium">Ulimits</h4>
          <ul className="mt-2 flex flex-col gap-1 font-mono text-xs">
            {resources.ulimits.map((ulimit) => (
              <li key={ulimit.name}>
                {ulimit.name}: soft {ulimit.soft}, hard {ulimit.hard}
              </li>
            ))}
          </ul>
        </div>
      ) : null}
    </DetailSection>
  );
}

function SecurityTab({ detail }: { detail: ContainerDetailData }) {
  const { security } = detail;

  return (
    <DetailSection
      title="Security"
      description="The posture an operator checks before and after a change."
    >
      <FieldList>
        <BoolField label="Privileged" value={security.privileged} />
        <BoolField label="Read-only root filesystem" value={security.readonlyRootfs} />
        <BoolField label="No new privileges" value={security.noNewPrivileges} />
        <Field label="AppArmor profile" value={security.apparmorProfile} />
        <Field label="SELinux label" value={security.selinuxLabel} mono />
        <Field label="Seccomp profile" value={security.seccompProfile} />
        <Field label="Capabilities added" value={security.capAdd?.join(", ")} />
        <Field label="Capabilities dropped" value={security.capDrop?.join(", ")} />
        <Field label="Security options" value={security.securityOpt?.join(", ")} mono span />
        <Field label="IPC mode" value={security.ipcMode} />
        <Field label="PID mode" value={security.pidMode} />
        <Field label="UTS mode" value={security.utsMode} />
        <Field label="User namespace" value={security.usernsMode} />
        <Field label="Cgroup namespace" value={security.cgroupnsMode} />
        <Field label="Supplementary groups" value={security.groupAdd?.join(", ")} />
        <Field label="Device cgroup rules" value={security.deviceCgroupRules?.join(", ")} mono span />
      </FieldList>

      {security.devices && security.devices.length > 0 ? (
        <div className="mt-4">
          <h4 className="text-sm font-medium">Devices</h4>
          <ul className="mt-2 flex flex-col gap-1 font-mono text-xs">
            {security.devices.map((device) => (
              <li key={device.pathInContainer}>
                {device.pathOnHost} &rarr; {device.pathInContainer} ({device.cgroupPermissions})
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {security.sysctls && Object.keys(security.sysctls).length > 0 ? (
        <div className="mt-4">
          <h4 className="text-sm font-medium">Sysctls</h4>
          <ul className="mt-2 flex flex-col gap-1 font-mono text-xs">
            {Object.entries(security.sysctls).map(([key, value]) => (
              <li key={key}>
                {key} = {value}
              </li>
            ))}
          </ul>
        </div>
      ) : null}
    </DetailSection>
  );
}

function LabelsTab({ detail }: { detail: ContainerDetailData }) {
  return (
    <DetailSection title="Labels">
      {detail.labels.length === 0 ? (
        <p className="text-sm text-content-muted">This container has no labels.</p>
      ) : (
        <ScrollArea>
          <table className="w-full min-w-[32rem] text-left text-sm">
            <caption className="sr-only">Labels</caption>
            <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
              <tr>
                <th scope="col" className="px-3 py-2 font-medium">Key</th>
                <th scope="col" className="px-3 py-2 font-medium">Value</th>
                <th scope="col" className="px-3 py-2 font-medium">Source</th>
              </tr>
            </thead>
            <tbody>
              {detail.labels.map((label) => (
                <tr key={label.key} className="border-b border-border-subtle last:border-0">
                  <td className="px-3 py-2 font-mono text-xs break-all">{label.key}</td>
                  <td className="px-3 py-2 font-mono text-xs break-all">{label.value}</td>
                  <td className="px-3 py-2 text-xs text-content-muted">{label.source}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </ScrollArea>
      )}
    </DetailSection>
  );
}

function ComposeTab({ detail }: { detail: ContainerDetailData }) {
  const { compose, harbormaster } = detail;

  return (
    <div className="flex flex-col gap-4">
      <DetailSection title="Docker Compose">
        {compose.managed ? (
          <FieldList>
            <Field label="Project" value={compose.project} />
            <Field label="Service" value={compose.service} />
            <Field
              label="Container number"
              value={compose.containerNumber ? String(compose.containerNumber) : undefined}
            />
            <Field label="Compose version" value={compose.version} />
            <Field label="Working directory" value={compose.workingDir} mono span />
            <Field label="Config files" value={compose.configFiles} mono span />
            <BoolField label="One-off (compose run)" value={compose.oneOff} />
          </FieldList>
        ) : (
          <p className="text-sm text-content-muted">
            This container is not managed by Docker Compose.
          </p>
        )}
      </DetailSection>

      <DetailSection
        title="HarborMaster metadata"
        description="Labels under io.harbormaster.*"
      >
        {harbormaster.enabled === undefined &&
        (!harbormaster.labels || Object.keys(harbormaster.labels).length === 0) ? (
          <p className="text-sm text-content-muted">
            This container carries no HarborMaster labels.
          </p>
        ) : (
          <FieldList>
            <Field
              label="Enabled"
              value={harbormaster.enabled === undefined ? undefined : String(harbormaster.enabled)}
            />
            {Object.entries(harbormaster.labels ?? {}).map(([key, value]) => (
              <Field key={key} label={key} value={value} />
            ))}
          </FieldList>
        )}
      </DetailSection>
    </div>
  );
}

/**
 * The raw inspection payload, fetched only when this tab is opened.
 *
 * The payload is redacted server-side; the UI never receives the real values
 * and so has nothing to reveal.
 */
function RawTab({ id }: { id: string }) {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getContainerRaw(id, { signal }),
    [id],
  );
  const raw = useApiResource<RawInspection>(fetcher);

  if (raw.status === "loading") {
    return <LoadingState label="Loading raw inspection data" />;
  }
  if (raw.status === "disconnected") {
    return <DisconnectedState onRetry={raw.refresh} />;
  }
  if (raw.error) {
    return <ErrorState error={raw.error} onRetry={raw.refresh} />;
  }
  if (!raw.data) {
    return <LoadingState label="Loading raw inspection data" />;
  }

  return (
    <DetailSection title="Raw inspection" description={raw.data.notice}>
      <CodeBlock
        label="Redacted raw inspection payload"
        content={JSON.stringify(raw.data.inspection, null, 2)}
      />
    </DetailSection>
  );
}

function formatTimestamp(iso: string | undefined): string | undefined {
  if (!iso) return undefined;
  const parsed = new Date(iso);
  return Number.isNaN(parsed.getTime()) ? iso : parsed.toLocaleString();
}

function formatMs(ms: number | undefined): string | undefined {
  if (!ms) return undefined;
  return ms >= 1000 ? `${(ms / 1000).toFixed(1)}s` : `${ms}ms`;
}

function formatBytes(bytes: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(unit === 0 ? 0 : 1)} ${units[unit]}`;
}

function formatBytesOrUndefined(bytes: number | undefined): string | undefined {
  if (!bytes) return undefined;
  return formatBytes(bytes);
}

function nonZero(value: number | undefined): string | undefined {
  return value ? String(value) : undefined;
}

function optionalNumber(value: number | undefined): string | undefined {
  return value === undefined ? undefined : String(value);
}

/** How many violations the container detail lists before deferring to the page. */
const RECENT_VIOLATION_LIMIT = 10;

/**
 * This container's compliance.
 *
 * Read-only, like every other tab. There is no remediation control and no
 * enforce button: a policy is a rule configuration is CHECKED AGAINST, and
 * HarborMaster has no capability to change a container to satisfy one.
 *
 * The banner is the point of the tab. "No violations" and "never evaluated"
 * look identical in a list and mean opposite things, so the evaluation is
 * reported explicitly rather than inferred from an empty result.
 */
function PolicyTab({ id }: { id: string }) {
  const query = useMemo(
    () => ({ pageSize: RECENT_VIOLATION_LIMIT, page: 1, openOnly: true }),
    [],
  );
  const state = useContainerPolicy(id, query);

  if (state.status === "loading") {
    return <LoadingState label="Loading compliance" />;
  }
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }

  const violations = state.data?.violations ?? [];
  const evaluation = state.data?.evaluation;

  return (
    <div className="space-y-4">
      {!evaluation ? (
        <p
          role="status"
          className="rounded-lg border border-border-subtle bg-surface-sunken px-3 py-2 text-sm text-content-muted"
        >
          This container has never been evaluated against any policy, so an
          empty list here means <strong>not checked</strong> rather than{" "}
          <strong>compliant</strong>.
        </p>
      ) : (
        <p className="text-xs text-content-muted">
          Last evaluated{" "}
          <time dateTime={evaluation.evaluatedAt}>
            {new Date(evaluation.evaluatedAt).toLocaleString()}
          </time>{" "}
          against {evaluation.policiesEvaluated}{" "}
          {evaluation.policiesEvaluated === 1 ? "policy" : "policies"}
          {evaluation.complete
            ? "."
            : " — the pass could not apply every policy, so this list may be incomplete."}
        </p>
      )}

      {violations.length === 0 ? (
        <EmptyState
          title={
            evaluation
              ? "Compliant with every enabled policy"
              : "Not evaluated"
          }
          description={
            evaluation
              ? "Every rule this container was checked against passed."
              : "Compliance is evaluated in the background after each inventory refresh."
          }
        />
      ) : (
        <ul className="space-y-2">
          {violations.map((violation) => (
            <li
              key={violation.id}
              className="rounded-lg border border-border-subtle bg-surface p-3"
            >
              <div className="flex flex-wrap items-center gap-2">
                <PolicySeverityBadge severity={violation.severity} />
                <PolicyStatusBadge status={violation.status} />
                <span className="text-sm text-content">
                  {RULE_LABELS[violation.ruleType] ?? violation.ruleType}
                </span>
              </div>
              <p className="mt-1 text-xs text-content-muted">
                {violation.policyName}
                {violation.reason ? ` — ${violation.reason}` : ""}
              </p>
            </li>
          ))}
        </ul>
      )}

      <p className="text-sm">
        <Link
          to={`/policy/container/${id}`}
          className="text-accent hover:underline"
        >
          Full compliance history
        </Link>
      </p>
    </div>
  );
}
