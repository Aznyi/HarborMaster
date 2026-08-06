import type {
  Acquisition,
  AcquisitionFailure,
  AcquisitionState,
} from "../api/acquisitionTypes";
import {
  ACQUISITION_FAILURE_LABELS,
  ACQUISITION_STATE_LABELS,
  ACQUISITION_STATE_MEANING,
  formatBytes,
  pinnedReference,
} from "../api/acquisitionTypes";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

/**
 * Image acquisition badges.
 *
 * Built on the shared StatusBadge, so acquisition looks like the rest of the
 * app and inherits the rule that colour is never the only signal.
 *
 * The rendering decision that matters most: a SUCCEEDED acquisition must not
 * read as "the container was updated". It says "Downloaded", and its tooltip
 * says in as many words that no container has been changed — because that is
 * the assumption an operator is most likely to make on seeing a green badge.
 */

const stateTones: Record<AcquisitionState, BadgeTone> = {
  queued: "neutral",
  validating: "neutral",
  pulling: "warn",
  verifying: "warn",
  succeeded: "ok",
  failed: "danger",
  cancelled: "neutral",
  expired: "neutral",
};

/** Where an acquisition has got to, with what that means in the tooltip. */
export function AcquisitionStateBadge({ state }: { state: AcquisitionState }) {
  return (
    <StatusBadge
      tone={stateTones[state]}
      label={ACQUISITION_STATE_LABELS[state]}
      title={ACQUISITION_STATE_MEANING[state]}
    />
  );
}

/** Why an acquisition did not succeed. */
export function AcquisitionFailureBadge({ failure }: { failure: AcquisitionFailure }) {
  const meaning: Record<AcquisitionFailure, string> = {
    preflight:
      "The safety checks refused this download. The world changed between the plan and the request",
    dockerUnavailable: "The Docker daemon did not answer. Nothing was downloaded",
    registry: "The registry did not serve this image. HarborMaster holds no registry credentials",
    transfer: "The transfer started and did not finish. Nothing was applied",
    timeout: "The download exceeded its time budget and was stopped",
    digestMismatch:
      "The image that arrived is NOT the one that was approved. Worth a look before trying again",
    platformMismatch: "The image that arrived does not target this host's platform",
    verification:
      "The image could not be inspected, so nothing about it was confirmed. Not the same as a mismatch",
    internal: "HarborMaster could not complete the acquisition",
  };

  // A mismatch is danger; everything else is a warning. The distinction is
  // real: a failed transfer is an inconvenience, while an unapproved image on
  // the host is a finding.
  const tone: BadgeTone =
    failure === "digestMismatch" || failure === "platformMismatch" ? "danger" : "warn";

  return (
    <StatusBadge
      tone={tone}
      label={ACQUISITION_FAILURE_LABELS[failure]}
      title={meaning[failure]}
    />
  );
}

/**
 * The banner every acquisition view carries.
 *
 * Stated once, plainly, and never conditionally: downloading an image does not
 * change a container. An operator who reads nothing else on the page should
 * still leave knowing that.
 */
export function NoContainerChangedNotice() {
  return (
    <p
      role="note"
      className="rounded-lg border border-border-subtle bg-surface-sunken px-3 py-2 text-xs text-content-muted"
    >
      Acquiring an image <strong className="text-content">downloads it to this host</strong>.
      It does not update, restart, or recreate any container — containers keep
      running the image they were created from. Applying an image is something
      you do with your own tooling.
    </p>
  );
}

/**
 * The transfer's progress, when there is any.
 *
 * The byte count is labelled as an estimate: the daemon reports per-layer
 * progress and never counts layers already present locally, so a "complete"
 * transfer routinely reports fewer bytes than the image's size.
 */
export function AcquisitionProgress({ acquisition }: { acquisition: Acquisition }) {
  const transferred = acquisition.bytesTransferred ?? 0;
  if (acquisition.state !== "pulling" && transferred === 0) return null;

  return (
    <div className="space-y-1">
      <p className="text-sm text-content">
        {acquisition.progress || "Downloading"}
        {acquisition.layers ? ` · ${acquisition.layers} layers` : ""}
      </p>
      {transferred > 0 && (
        <p className="text-xs text-content-muted">
          {formatBytes(transferred)} transferred (an estimate; layers already on
          this host are not counted)
        </p>
      )}
    </div>
  );
}

/**
 * What was asked for, spelled out unambiguously.
 *
 * The digest-pinned form, deliberately. A confirmation that said "nginx:1.27.1"
 * would be asking someone to approve a NAME; this asks them to approve
 * CONTENT, which is the thing that cannot change afterwards.
 */
export function AcquisitionTargetDetail({ acquisition }: { acquisition: Acquisition }) {
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
      <dt className="text-content-muted">Registry</dt>
      <dd className="font-mono break-all text-content">{acquisition.target.registry}</dd>

      <dt className="text-content-muted">Repository</dt>
      <dd className="font-mono break-all text-content">{acquisition.target.repository}</dd>

      <dt className="text-content-muted">Digest</dt>
      <dd className="font-mono break-all text-content">{acquisition.target.digest}</dd>

      {acquisition.target.platform && (
        <>
          <dt className="text-content-muted">Platform</dt>
          <dd className="text-content">
            {acquisition.target.platform.os}/{acquisition.target.platform.architecture}
            {acquisition.target.platform.variant
              ? `/${acquisition.target.platform.variant}`
              : ""}
          </dd>
        </>
      )}

      <dt className="text-content-muted">Pinned reference</dt>
      <dd className="font-mono break-all text-content">
        {pinnedReference(acquisition.target)}
      </dd>
    </dl>
  );
}

/**
 * What verification actually found, when it disagreed with what was approved.
 *
 * Rendered only on a mismatch, and rendered as evidence rather than as an
 * error: an operator investigating one needs to know what arrived.
 */
export function AcquisitionMismatchEvidence({ acquisition }: { acquisition: Acquisition }) {
  const mismatch =
    acquisition.failure === "digestMismatch" || acquisition.failure === "platformMismatch";
  if (!mismatch) return null;

  return (
    <div className="space-y-2 rounded-lg border border-danger/40 bg-danger-soft p-3">
      <p className="text-sm font-medium text-danger">
        The image that arrived is not the one that was approved.
      </p>
      <p className="text-xs text-content-muted">
        No container was changed. The image is in this host's local store and
        should be examined before anything is done with it.
      </p>
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
        <dt className="text-content-muted">Approved digest</dt>
        <dd className="font-mono break-all text-content">{acquisition.target.digest}</dd>

        <dt className="text-content-muted">Image that arrived</dt>
        <dd className="font-mono break-all text-content">
          {acquisition.acquiredImageId || "not recorded"}
        </dd>

        {acquisition.acquiredPlatform && (
          <>
            <dt className="text-content-muted">Its platform</dt>
            <dd className="text-content">
              {acquisition.acquiredPlatform.os}/{acquisition.acquiredPlatform.architecture}
            </dd>
          </>
        )}
      </dl>
    </div>
  );
}
