import { useState } from "react";
import { Link, useParams } from "react-router";

import { ApiError } from "../api/client";
import type { Acquisition, AcquisitionEvent } from "../api/acquisitionTypes";
import {
  ACQUISITION_STATE_LABELS,
  formatBytes,
  isActive,
  isCancellable,
  isRetryable,
} from "../api/acquisitionTypes";
import {
  AcquisitionFailureBadge,
  AcquisitionMismatchEvidence,
  AcquisitionProgress,
  AcquisitionStateBadge,
  AcquisitionTargetDetail,
  NoContainerChangedNotice,
} from "../components/AcquisitionBadges";
import { DetailSection } from "../components/DetailSection";
import { PageIntro } from "../components/PageIntro";
import { RecreateContainerAction } from "../components/RecreateContainerAction";
import {
  DisconnectedState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { stopAcquisition, useAcquisition } from "../hooks/useAcquisitions";

/**
 * One acquisition, with live progress and its audit trail.
 *
 * Polls while the acquisition is active and stops once it settles, so a
 * finished record costs nothing to leave open.
 *
 * The page states plainly, in more than one place, that no container was
 * changed. That repetition is deliberate: a green "Downloaded" badge is exactly
 * the thing an operator might read as "the update was applied", and it was not.
 */
export function AcquisitionDetail() {
  const { id = "" } = useParams();
  const [cancelling, setCancelling] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  // The poll decision is made from the CURRENT state, so the page settles into
  // no traffic once the transfer finishes.
  const state = useAcquisition(id, { poll: true });
  const acquisition = state.data?.acquisition;
  const events = state.data?.events ?? [];

  const cancel = async () => {
    setActionError(null);
    setCancelling(true);
    try {
      await stopAcquisition(id);
      state.refresh();
    } catch (caught) {
      setActionError(
        caught instanceof ApiError
          ? caught.message
          : "The acquisition could not be cancelled.",
      );
    } finally {
      setCancelling(false);
    }
  };

  if (state.status === "loading") {
    return <LoadingState label="Loading the acquisition" />;
  }
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }
  if (!acquisition) {
    return <LoadingState label="Loading the acquisition" />;
  }

  return (
    <div className="space-y-6">
      <PageIntro
        title="Image download"
        description={
          "What HarborMaster downloaded, what it checked before doing so, and " +
          "how the transfer went. No container was changed by any of it."
        }
      />

      <p className="text-sm text-content-muted">
        <Link to="/acquisitions" className="underline underline-offset-2 hover:text-content">
          Back to acquisitions
        </Link>
      </p>

      <NoContainerChangedNotice />

      {actionError && (
        <p
          role="alert"
          className="rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger"
        >
          {actionError}
        </p>
      )}

      <DetailSection title="Status">
        <div className="space-y-3">
          <div className="flex flex-wrap items-center gap-2">
            <AcquisitionStateBadge state={acquisition.state} />
            {acquisition.failure && (
              <AcquisitionFailureBadge failure={acquisition.failure} />
            )}
          </div>

          {acquisition.message && (
            <p className="text-sm text-content">{acquisition.message}</p>
          )}

          {isActive(acquisition.state) && (
            <AcquisitionProgress acquisition={acquisition} />
          )}

          {acquisition.failure && isRetryable(acquisition.failure) && (
            <p className="text-xs text-content-muted">
              This failure looks transient. Requesting the image again from its
              change plan may succeed — HarborMaster never retries on its own.
            </p>
          )}

          {isCancellable(acquisition.state) && (
            <button
              type="button"
              onClick={cancel}
              disabled={cancelling}
              className="rounded-md border border-border-subtle bg-surface-raised px-3 py-1.5 text-sm font-medium text-content disabled:opacity-60"
              title="Stops the download. Nothing on this host is removed or changed."
            >
              {cancelling ? "Cancelling…" : "Cancel download"}
            </button>
          )}
        </div>
      </DetailSection>

      <AcquisitionMismatchEvidence acquisition={acquisition} />

      <DetailSection
        title="What was requested"
        description="The exact content, pinned by digest rather than named by tag."
      >
        <AcquisitionTargetDetail acquisition={acquisition} />
      </DetailSection>

      {acquisition.state === "succeeded" && (
        <DetailSection
          title="What arrived"
          description="Confirmed by re-inspecting the local image after the transfer."
        >
          <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
            <dt className="text-content-muted">Image ID</dt>
            <dd className="font-mono break-all text-content">
              {acquisition.acquiredImageId || "—"}
            </dd>
            <dt className="text-content-muted">Digest</dt>
            <dd className="font-mono break-all text-content">
              {acquisition.acquiredDigest || "—"}
            </dd>
            <dt className="text-content-muted">Size on disk</dt>
            <dd className="text-content">{formatBytes(acquisition.sizeBytes)}</dd>
          </dl>
        </DetailSection>
      )}

      {/* The one place a recreation can be started from.
          Deliberately here rather than on the plan page: a recreation needs an
          image that is already on this host and already verified, and this is
          the record that establishes both. */}
      {acquisition.state === "succeeded" && (
        <DetailSection
          title="Apply this image"
          description="Recreate the container on the image above. This stops it."
        >
          <RecreateContainerAction
            acquisition={acquisition}
            onRequested={state.refresh}
          />
        </DetailSection>
      )}

      <DetailSection
        title="Context"
        description="The plan that approved this image, and the container it was assessed for."
      >
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-2 text-xs">
          <dt className="text-content-muted">Container</dt>
          <dd className="text-content">
            <Link
              to={`/containers/${encodeURIComponent(acquisition.containerId)}`}
              className="underline underline-offset-2"
            >
              {acquisition.containerName || acquisition.containerId}
            </Link>
          </dd>

          <dt className="text-content-muted">Change plan</dt>
          <dd className="text-content">
            <Link
              to={`/plans/${encodeURIComponent(acquisition.planId)}`}
              className="underline underline-offset-2"
            >
              {acquisition.planId}
            </Link>
          </dd>

          <dt className="text-content-muted">Requested</dt>
          <dd className="text-content">
            <time dateTime={acquisition.requestedAt}>
              {new Date(acquisition.requestedAt).toLocaleString()}
            </time>
          </dd>

          {acquisition.completedAt && (
            <>
              <dt className="text-content-muted">Finished</dt>
              <dd className="text-content">
                <time dateTime={acquisition.completedAt}>
                  {new Date(acquisition.completedAt).toLocaleString()}
                </time>
              </dd>
            </>
          )}
        </dl>
      </DetailSection>

      {events.length > 0 && (
        <DetailSection
          title="What happened"
          description="Oldest first. This is the record of one operation, kept as evidence."
        >
          <AcquisitionTimeline events={events} />
        </DetailSection>
      )}
    </div>
  );
}

/**
 * The audit trail, oldest first.
 *
 * Unlike every other history in HarborMaster this reads forwards: it is the
 * narrative of a single operation rather than a log scanned for its newest
 * entry.
 */
function AcquisitionTimeline({ events }: { events: AcquisitionEvent[] }) {
  return (
    <ol className="relative space-y-3 border-l border-border-subtle pl-5">
      {events.map((event, index) => (
        <li key={`${event.at}-${index}`} className="relative">
          <span
            aria-hidden="true"
            className={`absolute -left-[1.4rem] top-1.5 size-2.5 rounded-full ring-2 ring-surface ${dotFor(
              event,
            )}`}
          />
          <div className="flex flex-wrap items-baseline gap-2">
            <span className="text-sm text-content">
              {ACQUISITION_STATE_LABELS[event.state] ?? event.state}
            </span>
            <time dateTime={event.at} className="text-xs text-content-muted">
              {new Date(event.at).toLocaleString()}
            </time>
          </div>
          {/* Rendered as text. The detail can carry a sanitised progress line
              the daemon relayed from a registry, so it is never treated as
              anything but a string. */}
          {event.detail && (
            <p className="mt-0.5 text-xs text-content-muted">{event.detail}</p>
          )}
        </li>
      ))}
    </ol>
  );
}

/** The timeline dot. Never the only signal: the state is written beside it. */
function dotFor(event: AcquisitionEvent): string {
  switch (event.state) {
    case "succeeded":
      return "bg-ok";
    case "failed":
      return "bg-danger";
    case "pulling":
    case "verifying":
      return "bg-warn";
    default:
      return "bg-content-muted";
  }
}

/** Rendered when an acquisition's transfer summary is worth showing inline. */
export function AcquisitionTransferSummary({ acquisition }: { acquisition: Acquisition }) {
  if (!acquisition.bytesTransferred && !acquisition.layers) return null;

  return (
    <p className="text-xs text-content-muted">
      {acquisition.layers ?? 0} layers · {formatBytes(acquisition.bytesTransferred)}{" "}
      transferred
    </p>
  );
}
