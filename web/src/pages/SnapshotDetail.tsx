import { useState } from "react";
import { Link, useParams } from "react-router";

import type { SnapshotEnvEntry } from "../api/snapshotTypes";
import { DetailSection, Field, FieldList } from "../components/DetailSection";
import { PageIntro } from "../components/PageIntro";
import {
  ReadinessBadge,
  SensitiveMarker,
  TriggerBadge,
} from "../components/SnapshotBadges";
import { SnapshotDiffView } from "../components/SnapshotDiff";
import {
  DisconnectedState,
  ErrorState,
  LoadingState,
  EmptyState,
} from "../components/States";
import { useSnapshot, useSnapshotDiff } from "../hooks/useSnapshots";

type Tab = "configuration" | "environment" | "diff";

/**
 * One snapshot's detail.
 *
 * There is no restore control here, and there is no code path that could add
 * one: the API exposes no restore endpoint. The diff tab compares this capture
 * against the container's current configuration, which is the closest thing
 * Phase 3 offers to "what would change".
 */
export function SnapshotDetailPage() {
  const params = useParams();
  const id = Number(params.id);
  const [tab, setTab] = useState<Tab>("configuration");

  const resource = useSnapshot(id);

  if (!Number.isInteger(id) || id < 1) {
    return (
      <EmptyState
        title="Unknown snapshot"
        description="That snapshot id is not valid."
      />
    );
  }
  if (resource.status === "loading") return <LoadingState label="Loading snapshot" />;
  if (resource.status === "disconnected") {
    return <DisconnectedState onRetry={resource.refresh} />;
  }
  if (resource.error) {
    return <ErrorState error={resource.error} onRetry={resource.refresh} />;
  }

  const snapshot = resource.data;
  if (!snapshot) {
    return <EmptyState title="Snapshot not found" description="It may have been pruned by the retention policy." />;
  }

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title={snapshot.containerName || snapshot.containerId}
        description="An immutable record of this container's configuration at a moment in time. HarborMaster cannot restore it; this is evidence, not a control."
      />

      <div className="flex flex-wrap items-center gap-2">
        <TriggerBadge trigger={snapshot.trigger} />
        <ReadinessBadge status={snapshot.readinessStatus} />
        <Link
          to={`/snapshots/${snapshot.id}/readiness`}
          className="text-sm text-accent hover:underline"
        >
          View restore readiness
        </Link>
      </div>

      <DetailSection title="Capture">
        <FieldList>
          <Field label="Captured" value={new Date(snapshot.createdAt).toLocaleString()} />
          <Field label="Container ID" value={snapshot.containerId} mono />
          <Field label="Image" value={snapshot.imageReference || "—"} />
          <Field label="Image digest" value={snapshot.imageDigest || "not recorded"} mono />
          <Field label="Checksum" value={snapshot.checksum} mono />
          <Field label="Reason" value={snapshot.reason || "—"} />
          <Field label="HarborMaster" value={snapshot.harbormasterVersion || "—"} />
          <Field label="Docker API" value={snapshot.dockerApiVersion || "—"} />
          <Field
            label="Inventory generation"
            value={String(snapshot.inventoryGeneration ?? 0)}
          />
        </FieldList>
      </DetailSection>

      <nav className="flex gap-1 border-b border-border-subtle" aria-label="Snapshot sections">
        {(["configuration", "environment", "diff"] as Tab[]).map((value) => (
          <button
            key={value}
            type="button"
            onClick={() => setTab(value)}
            aria-current={tab === value ? "page" : undefined}
            className={`rounded-t-lg px-4 py-2 text-sm capitalize ${
              tab === value
                ? "border-b-2 border-accent font-medium text-content"
                : "text-content-muted hover:text-content"
            }`}
          >
            {value}
          </button>
        ))}
      </nav>

      {tab === "configuration" ? <ConfigurationTab snapshot={snapshot} /> : null}
      {tab === "environment" ? <EnvironmentTab entries={snapshot.environment} /> : null}
      {tab === "diff" ? <DiffTab id={snapshot.id} /> : null}
    </div>
  );
}

function ConfigurationTab({
  snapshot,
}: {
  snapshot: ReturnType<typeof useSnapshot>["data"] & object;
}) {
  return (
    <div className="flex flex-col gap-4">
      <DetailSection title="Mounts">
        {snapshot.mounts.length === 0 ? (
          <p className="text-sm text-content-muted">No mounts.</p>
        ) : (
          <ul className="flex flex-col gap-2 text-sm">
            {snapshot.mounts.map((mount) => (
              <li key={mount.destination} className="font-mono text-xs">
                {mount.type} {mount.volumeName || mount.source || ""} →{" "}
                {mount.destination}
                {mount.readOnly ? " (ro)" : ""}
              </li>
            ))}
          </ul>
        )}
      </DetailSection>

      <DetailSection title="Networks">
        {snapshot.networks.length === 0 ? (
          <p className="text-sm text-content-muted">No network attachments.</p>
        ) : (
          <ul className="flex flex-col gap-2 text-sm">
            {snapshot.networks.map((network) => (
              <li key={network.networkName} className="font-mono text-xs">
                {network.networkName}
                {network.aliases?.length ? ` (${network.aliases.join(", ")})` : ""}
              </li>
            ))}
          </ul>
        )}
      </DetailSection>
    </div>
  );
}

/**
 * The captured environment.
 *
 * A sensitive entry has no value in the payload at all, so there is nothing to
 * hide behind a toggle and no "reveal" affordance to build. HarborMaster does
 * not have the value.
 */
function EnvironmentTab({ entries }: { entries: SnapshotEnvEntry[] }) {
  if (entries.length === 0) {
    return <p className="text-sm text-content-muted">No environment recorded.</p>;
  }

  const sensitiveCount = entries.filter((e) => e.classification === "sensitive").length;

  return (
    <div className="flex flex-col gap-3">
      {sensitiveCount > 0 ? (
        <p className="rounded-lg border border-border-subtle bg-surface-sunken px-4 py-3 text-sm text-content-muted">
          {sensitiveCount} value{sensitiveCount === 1 ? " is" : "s are"} recorded by
          keyed digest only. HarborMaster never stores secret values, so restoring this
          container would require supplying them from elsewhere.
        </p>
      ) : null}

      <div className="overflow-hidden rounded-xl border border-border-subtle bg-surface-raised">
        <table className="w-full text-left text-sm">
          <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
            <tr>
              <th scope="col" className="px-4 py-2">Name</th>
              <th scope="col" className="px-4 py-2">Value</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((entry) => (
              <tr key={entry.key} className="border-b border-border-subtle last:border-0">
                <td className="px-4 py-2 font-mono text-xs">{entry.key}</td>
                <td className="px-4 py-2">
                  {entry.classification === "sensitive" ? (
                    <SensitiveMarker length={entry.length} />
                  ) : (
                    <span className="break-all font-mono text-xs">
                      {entry.value ?? "—"}
                    </span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

/** Compares this snapshot against the container's current configuration. */
function DiffTab({ id }: { id: number }) {
  const resource = useSnapshotDiff(id, "current");

  if (resource.status === "loading") return <LoadingState label="Comparing" />;
  if (resource.status === "disconnected") {
    return <DisconnectedState onRetry={resource.refresh} />;
  }
  if (resource.error) {
    return <ErrorState error={resource.error} onRetry={resource.refresh} />;
  }
  if (!resource.data) return null;

  return (
    <div className="flex flex-col gap-3">
      <p className="text-sm text-content-muted">
        Comparing this snapshot against the container's current configuration.
        Nothing is written by this comparison.
      </p>
      <SnapshotDiffView diff={resource.data} />
    </div>
  );
}
