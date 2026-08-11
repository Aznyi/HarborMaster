import { useMemo, useState } from "react";
import { Link } from "react-router";

import type {
  Acquisition,
  AcquisitionState,
  AcquisitionSummary,
} from "../api/acquisitionTypes";
import {
  ACQUISITION_STATE_LABELS,
  ACQUISITION_STATE_ORDER,
  isActive,
} from "../api/acquisitionTypes";
import {
  AcquisitionFailureBadge,
  AcquisitionProgress,
  AcquisitionStateBadge,
  NoContainerChangedNotice,
} from "../components/AcquisitionBadges";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { useAcquisitions } from "../hooks/useAcquisitions";

const PAGE_SIZE = 25;

/**
 * The image acquisition history.
 *
 * Every row is a record of a DOWNLOAD. Nothing on this page updated a
 * container, and nothing on it can — there is no apply, rollback, or restart
 * control, because HarborMaster has no such capability.
 *
 * The list polls only while something is actually moving, so a settled estate
 * costs nothing to leave open.
 */
export function Acquisitions() {
  const [page, setPage] = useState(1);
  const [state, setState] = useState<AcquisitionState | "">("");
  const [activeOnly, setActiveOnly] = useState(false);

  const query = useMemo(
    () => ({
      page,
      pageSize: PAGE_SIZE,
      ...(state ? { state: [state] } : {}),
      ...(activeOnly ? { activeOnly: true } : {}),
    }),
    [page, state, activeOnly],
  );

  // Polling is decided from what the last response contained, so an idle page
  // settles into no traffic at all.
  const acquisitions = useAcquisitions(query, { pollWhileActive: true });
  const summary = acquisitions.data?.summary;

  return (
    <div className="space-y-6">
      <PageIntro
        title="Image downloads"
        description={
          "Images HarborMaster has downloaded to this host, and what happened " +
          "to each attempt. Downloading an image never changes a container."
        }
      />

      <NoContainerChangedNotice />

      <SummaryCards state={acquisitions} summary={summary} />

      <section className="space-y-4">
        <Filters
          state={state}
          activeOnly={activeOnly}
          onState={(value) => {
            setState(value);
            setPage(1);
          }}
          onActiveOnly={(value) => {
            setActiveOnly(value);
            setPage(1);
          }}
        />

        <AcquisitionList state={acquisitions} onPage={setPage} />
      </section>
    </div>
  );
}

function SummaryCards({
  state,
  summary,
}: {
  state: ReturnType<typeof useAcquisitions>;
  summary: AcquisitionSummary | undefined;
}) {
  if (state.status === "loading") return <LoadingState label="Loading acquisitions" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }
  // Tolerate a null or malformed payload rather than throwing.
  if (!summary) return <LoadingState label="Loading acquisitions" />;

  // A deployment that has not opted in is stated explicitly, so an empty list
  // is not read as "nothing has ever been downloaded".
  if (!summary.enabled) {
    return (
      <p
        role="status"
        className="rounded-lg border border-border-subtle bg-surface px-3 py-2 text-sm text-content-muted"
      >
        Image acquisition is switched off in this deployment. HarborMaster
        cannot download anything, and records already stored remain readable.
      </p>
    );
  }

  return (
    <section aria-label="Acquisition summary" className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
      <Card label="In progress" value={summary.active} hint="Downloads running now" />
      <Card
        label="Downloaded"
        value={summary.succeeded}
        hint="Verified and present on this host"
      />
      <Card
        label="Failed"
        value={summary.failed}
        hint="Nothing was applied"
        tone={summary.failed > 0 ? "warn" : "neutral"}
      />
      <Card label="Total" value={summary.total} hint="Every attempt recorded" />
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
  value: number;
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

function Filters({
  state,
  activeOnly,
  onState,
  onActiveOnly,
}: {
  state: AcquisitionState | "";
  activeOnly: boolean;
  onState: (value: AcquisitionState | "") => void;
  onActiveOnly: (value: boolean) => void;
}) {
  return (
    <div className="flex flex-wrap items-end gap-3">
      <label className="flex flex-col gap-1 text-xs text-content-muted">
        State
        <select
          className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
          value={state}
          onChange={(event) => onState(event.target.value as AcquisitionState | "")}
        >
          <option value="">All states</option>
          {ACQUISITION_STATE_ORDER.map((value) => (
            <option key={value} value={value}>
              {ACQUISITION_STATE_LABELS[value]}
            </option>
          ))}
        </select>
      </label>

      <label className="flex items-center gap-2 pb-1.5 text-sm text-content">
        <input
          type="checkbox"
          className="h-6 w-6 shrink-0 rounded border-border-subtle"
          checked={activeOnly}
          onChange={(event) => onActiveOnly(event.target.checked)}
        />
        Only work in progress
      </label>
    </div>
  );
}

function AcquisitionList({
  state,
  onPage,
}: {
  state: ReturnType<typeof useAcquisitions>;
  onPage: (page: number) => void;
}) {
  if (state.status === "loading") return <LoadingState label="Loading acquisitions" />;
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
        title="No image downloads match these filters"
        description={
          "An acquisition is requested from a change plan that recommends the " +
          "change. Nothing here happens on a schedule — HarborMaster never " +
          "downloads an image on its own."
        }
      />
    );
  }

  return (
    <div className="space-y-3">
      <ul className="space-y-2">
        {items.map((acquisition) => (
          <AcquisitionRow key={acquisition.acquisitionId} acquisition={acquisition} />
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

/** One acquisition. */
function AcquisitionRow({ acquisition }: { acquisition: Acquisition }) {
  return (
    <li className="rounded-lg border border-border-subtle bg-surface p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-2">
          <div className="flex flex-wrap items-center gap-2">
            <AcquisitionStateBadge state={acquisition.state} />
            {acquisition.failure && (
              <AcquisitionFailureBadge failure={acquisition.failure} />
            )}
          </div>

          <Link
            to={`/acquisitions/${encodeURIComponent(acquisition.acquisitionId)}`}
            className="flex min-h-6 items-center text-sm font-medium text-content hover:underline"
          >
            {acquisition.target.reference || acquisition.target.repository}
          </Link>

          {/* The digest, always. The reference above is the friendly name; this
              is what was actually fetched. */}
          <p className="font-mono text-xs break-all text-content-muted">
            {acquisition.target.registry}/{acquisition.target.repository}@
            {acquisition.target.digest}
          </p>

          <p className="text-xs text-content-muted">
            for{" "}
            <Link
              to={`/containers/${encodeURIComponent(acquisition.containerId)}`}
              className="underline underline-offset-2"
            >
              {acquisition.containerName || acquisition.containerId}
            </Link>
          </p>

          {isActive(acquisition.state) && (
            <AcquisitionProgress acquisition={acquisition} />
          )}

          {acquisition.message && (
            <p className="text-sm text-content-muted">{acquisition.message}</p>
          )}
        </div>

        <time
          dateTime={acquisition.requestedAt}
          className="shrink-0 text-xs text-content-muted"
        >
          {new Date(acquisition.requestedAt).toLocaleString()}
        </time>
      </div>
    </li>
  );
}
