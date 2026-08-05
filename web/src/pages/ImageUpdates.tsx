import { useMemo, useState } from "react";
import { Link } from "react-router";

import { ApiError } from "../api/client";
import type {
  ImageIntel,
  ImageIntelSummary,
  RegistryHealth,
  UpdateType,
} from "../api/imageTypes";
import { UPDATE_TYPE_LABELS, UPDATE_TYPE_ORDER } from "../api/imageTypes";
import {
  CheckStatusBadge,
  DigestComparison,
  RegistryBadge,
  RegistryHealthBadge,
  UpdateBadge,
} from "../components/ImageBadges";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { requestImageRefresh, useImageUpdates } from "../hooks/useImageUpdates";

const PAGE_SIZE = 25;

/**
 * The image updates dashboard.
 *
 * Everything here is an OBSERVATION: which images have a newer version
 * published. Nothing on this page pulls, deletes, or applies anything — there is
 * no such control, because HarborMaster has no such capability and the API
 * exposes no such endpoint. The only write is a request to re-check.
 */
export function ImageUpdates() {
  const [page, setPage] = useState(1);
  const [update, setUpdate] = useState<UpdateType | "">("");
  const [registry, setRegistry] = useState("");
  const [updatesOnly, setUpdatesOnly] = useState(true);
  const [actionError, setActionError] = useState<string | null>(null);
  const [refreshing, setRefreshing] = useState(false);

  const query = useMemo(
    () => ({
      page,
      pageSize: PAGE_SIZE,
      updatesOnly,
      ...(update ? { update: [update] } : {}),
      ...(registry ? { registry: [registry] } : {}),
    }),
    [page, update, registry, updatesOnly],
  );

  const updates = useImageUpdates(query);

  const refresh = async () => {
    setActionError(null);
    setRefreshing(true);
    try {
      await requestImageRefresh();
      // The server answered 202 — the pass is scheduled, not finished — so the
      // view is refreshed rather than assumed to be current.
      updates.refresh();
    } catch (caught) {
      setActionError(
        caught instanceof ApiError
          ? caught.message
          : "The metadata refresh could not be requested.",
      );
    } finally {
      setRefreshing(false);
    }
  };

  const summary = updates.data?.summary;

  return (
    <div className="space-y-6">
      <PageIntro
        title="Image updates"
        description={
          "Which running images have a newer version published. HarborMaster " +
          "reads registries to find out; it never pulls, changes, or removes an " +
          "image."
        }
      />

      <SummaryCards
        state={updates}
        summary={summary}
        onRefresh={refresh}
        refreshing={refreshing}
      />

      {actionError && (
        <p
          role="alert"
          className="rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger"
        >
          {actionError}
        </p>
      )}

      {summary?.registries && summary.registries.length > 0 && (
        <RegistryStatus registries={summary.registries} />
      )}

      <section className="space-y-4">
        <Filters
          update={update}
          registry={registry}
          registries={summary?.byRegistry ?? {}}
          updatesOnly={updatesOnly}
          onUpdate={(value) => {
            setUpdate(value);
            setPage(1);
          }}
          onRegistry={(value) => {
            setRegistry(value);
            setPage(1);
          }}
          onUpdatesOnly={(value) => {
            setUpdatesOnly(value);
            setPage(1);
          }}
        />

        <UpdateList state={updates} onPage={setPage} />
      </section>
    </div>
  );
}

/** The summary cards. */
function SummaryCards({
  state,
  summary,
  onRefresh,
  refreshing,
}: {
  state: ReturnType<typeof useImageUpdates>;
  summary: ImageIntelSummary | undefined;
  onRefresh: () => void;
  refreshing: boolean;
}) {
  if (state.status === "loading") return <LoadingState label="Loading image updates" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }
  // Tolerate a null or malformed payload rather than throwing: a view that
  // crashes on unexpected input turns a backend hiccup into a blank screen.
  if (!summary) return <LoadingState label="Loading image updates" />;

  const unchecked = summary.images - summary.checked;

  return (
    <section aria-label="Update summary" className="space-y-3">
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Card
          label="Updates available"
          value={summary.updatesAvailable}
          hint={`${summary.containersAffected} containers affected`}
          tone={summary.updatesAvailable > 0 ? "warn" : "neutral"}
        />
        <Card
          label="Major"
          value={summary.byUpdateType.major ?? 0}
          hint="Expect breaking changes"
          tone={(summary.byUpdateType.major ?? 0) > 0 ? "danger" : "neutral"}
        />
        <Card
          label="Images tracked"
          value={summary.images}
          // Coverage sits beside the update count deliberately: an update count
          // without "how many were actually checked" invites reading a stale
          // estate as a healthy one.
          hint={`${summary.checked} checked`}
        />
        <Card
          label="Awaiting a check"
          value={summary.pending}
          hint={
            summary.unsupported > 0
              ? `${summary.unsupported} cannot be checked at all`
              : "Checked in the background"
          }
          tone={summary.pending > 0 ? "warn" : "neutral"}
        />
      </div>

      {unchecked > 0 && (
        <p
          role="status"
          className="rounded-lg border border-border-subtle bg-surface px-3 py-2 text-xs text-content-muted"
        >
          {unchecked} of {summary.images} tracked images have not been checked
          against a registry yet, so this list is not yet a complete picture of
          the estate.
        </p>
      )}

      <div className="flex flex-wrap items-center gap-3">
        <UpdateBreakdown summary={summary} />

        <button
          type="button"
          onClick={onRefresh}
          disabled={refreshing}
          className="ml-auto rounded-md border border-border-subtle bg-surface-raised px-3 py-1.5 text-sm font-medium text-content disabled:opacity-60"
          // Says what it does. It schedules metadata collection; it does not
          // pull anything, and promising a finished result would be a lie about
          // a pass that can take a while.
          title="Schedules a metadata collection pass against the registries. Reads only — nothing is pulled or changed. Returns as soon as it is queued."
        >
          {refreshing ? "Requesting…" : "Check for updates"}
        </button>
      </div>

      {summary.lastCheckedAt && (
        <p className="text-xs text-content-muted">
          Last checked{" "}
          <time dateTime={summary.lastCheckedAt}>
            {new Date(summary.lastCheckedAt).toLocaleString()}
          </time>
        </p>
      )}
    </section>
  );
}

function Card({
  label,
  value,
  hint,
  tone = "neutral",
}: {
  label: string;
  value: number | string;
  hint: string;
  tone?: "neutral" | "warn" | "danger";
}) {
  const toneClasses = {
    neutral: "border-border-subtle",
    warn: "border-warn/40",
    danger: "border-danger/40",
  }[tone];

  return (
    <div className={`rounded-lg border ${toneClasses} bg-surface p-4`}>
      <p className="text-xs font-medium uppercase tracking-wide text-content-muted">
        {label}
      </p>
      <p className="mt-1 text-2xl font-semibold text-content">{value}</p>
      <p className="mt-1 text-xs text-content-muted">{hint}</p>
    </div>
  );
}

/** A compact distribution of update types. */
function UpdateBreakdown({ summary }: { summary: ImageIntelSummary }) {
  const actionable = UPDATE_TYPE_ORDER.filter(
    (type) => type !== "none" && (summary.byUpdateType[type] ?? 0) > 0,
  );
  if (actionable.length === 0) return null;

  return (
    <div className="flex flex-wrap items-center gap-2" aria-label="Updates by type">
      {actionable.map((type) => (
        <span key={type} className="inline-flex items-center gap-1.5">
          <UpdateBadge update={type} />
          <span className="text-xs text-content-muted">
            {summary.byUpdateType[type] ?? 0}
          </span>
        </span>
      ))}
    </div>
  );
}

/**
 * Registry health.
 *
 * Shown so an operator can attribute staleness: "updates are old because Docker
 * Hub is rate-limiting us" is a very different situation from "there are no
 * updates", and a dashboard that could not tell them apart would be misleading.
 */
function RegistryStatus({ registries }: { registries: RegistryHealth[] }) {
  const unhealthy = registries.filter(
    (entry) => entry.consecutiveFailures > 0 || entry.rateLimited,
  );

  return (
    <section aria-label="Registry status" className="space-y-2">
      <h3 className="text-sm font-semibold text-content">Registries</h3>

      <ul className="flex flex-wrap gap-2">
        {registries.map((entry) => (
          <li
            key={entry.host}
            className="flex items-center gap-2 rounded-lg border border-border-subtle bg-surface px-3 py-2"
          >
            <span className="text-sm text-content">{entry.host}</span>
            <RegistryHealthBadge health={entry} />
            <span className="text-xs text-content-muted">
              {entry.images} {entry.images === 1 ? "image" : "images"}
            </span>
          </li>
        ))}
      </ul>

      {unhealthy.length > 0 && (
        <p
          role="status"
          className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-xs text-warn"
        >
          {unhealthy.length === 1
            ? `${unhealthy[0]?.host} is not answering, so update information for its images may be out of date. Previously discovered updates are retained.`
            : `${unhealthy.length} registries are not answering, so update information for their images may be out of date.`}
        </p>
      )}
    </section>
  );
}

function Filters({
  update,
  registry,
  registries,
  updatesOnly,
  onUpdate,
  onRegistry,
  onUpdatesOnly,
}: {
  update: UpdateType | "";
  registry: string;
  registries: Record<string, number>;
  updatesOnly: boolean;
  onUpdate: (value: UpdateType | "") => void;
  onRegistry: (value: string) => void;
  onUpdatesOnly: (value: boolean) => void;
}) {
  return (
    <div className="flex flex-wrap items-end gap-3">
      <label className="flex flex-col gap-1 text-xs text-content-muted">
        Update type
        <select
          className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
          value={update}
          onChange={(event) => onUpdate(event.target.value as UpdateType | "")}
        >
          <option value="">All types</option>
          {UPDATE_TYPE_ORDER.map((value) => (
            <option key={value} value={value}>
              {UPDATE_TYPE_LABELS[value]}
            </option>
          ))}
        </select>
      </label>

      <label className="flex flex-col gap-1 text-xs text-content-muted">
        Registry
        <select
          className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
          value={registry}
          onChange={(event) => onRegistry(event.target.value)}
        >
          <option value="">All registries</option>
          {Object.keys(registries)
            .sort()
            .map((host) => (
              <option key={host} value={host}>
                {host}
              </option>
            ))}
        </select>
      </label>

      <label className="flex items-center gap-2 pb-1.5 text-sm text-content">
        <input
          type="checkbox"
          className="size-4 rounded border-border-subtle"
          checked={updatesOnly}
          onChange={(event) => onUpdatesOnly(event.target.checked)}
        />
        Only images with updates
      </label>
    </div>
  );
}

function UpdateList({
  state,
  onPage,
}: {
  state: ReturnType<typeof useImageUpdates>;
  onPage: (page: number) => void;
}) {
  if (state.status === "loading") return <LoadingState label="Loading images" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }

  const data = state.data;
  const items = data?.items ?? [];

  if (items.length === 0) {
    return (
      <EmptyState
        title="No images match these filters"
        description={
          "An image with no update is running the newest published version of " +
          "its tag. An image that has not been checked yet is a different thing " +
          "entirely — the summary above says how many of those there are."
        }
      />
    );
  }

  return (
    <div className="space-y-3">
      <ul className="space-y-2">
        {items.map((intel) => (
          <ImageRow key={intel.id} intel={intel} />
        ))}
      </ul>
      {data?.pagination && (
        <Pagination
          pagination={data.pagination}
          onPageChange={onPage}
          busy={state.refreshing}
        />
      )}
    </div>
  );
}

/** One tracked image reference. */
function ImageRow({ intel }: { intel: ImageIntel }) {
  return (
    <li className="rounded-lg border border-border-subtle bg-surface p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <div className="flex flex-wrap items-center gap-2">
            <UpdateBadge update={intel.updateType} />
            <RegistryBadge registry={intel.registry} />
            {intel.checkStatus !== "ok" && (
              <CheckStatusBadge status={intel.checkStatus} />
            )}
            {intel.pinned && (
              <span
                className="rounded border border-border-subtle px-1.5 py-0.5 text-xs text-content-muted"
                title="The reference names a digest, so its tag cannot move."
              >
                pinned
              </span>
            )}
          </div>

          <p className="font-mono text-sm text-content break-all">
            {intel.imageId ? (
              <Link
                to={`/images/${intel.imageId}`}
                className="underline underline-offset-2 hover:text-content"
              >
                {intel.familiar}
              </Link>
            ) : (
              intel.familiar
            )}
          </p>

          {intel.latestTag && (
            <p className="text-xs text-content">
              Newer tag available:{" "}
              <span className="font-mono">{intel.latestTag}</span>
            </p>
          )}

          <p className="text-xs text-content-muted">
            {intel.containerCount}{" "}
            {intel.containerCount === 1 ? "container" : "containers"}
            {intel.lastCheckedAt && (
              <>
                {" · checked "}
                <time dateTime={intel.lastCheckedAt}>
                  {new Date(intel.lastCheckedAt).toLocaleString()}
                </time>
              </>
            )}
          </p>

          {intel.updateReason && (
            <p className="text-xs text-content-muted">{intel.updateReason}</p>
          )}
          {intel.statusDetail && (
            <p className="text-xs text-warn">{intel.statusDetail}</p>
          )}
        </div>

        <div className="min-w-0 max-w-md shrink-0">
          <DigestComparison intel={intel} />
        </div>
      </div>
    </li>
  );
}
