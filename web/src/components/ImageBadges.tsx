import type {
  CheckStatus,
  ImageIntel,
  RegistryHealth,
  UpdateType,
} from "../api/imageTypes";
import {
  CHECK_STATUS_LABELS,
  CHECK_STATUS_MEANINGS,
  UPDATE_TYPE_LABELS,
  UPDATE_TYPE_MEANINGS,
} from "../api/imageTypes";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

/**
 * Image intelligence badges.
 *
 * Built on the shared StatusBadge, so updates look like the rest of the app and
 * inherit the rule that colour is never the only signal: every badge carries a
 * label, and the tone only reinforces it.
 */

const updateTones: Record<UpdateType, BadgeTone> = {
  major: "danger",
  minor: "warn",
  patch: "warn",
  prerelease: "neutral",
  digest: "warn",
  // Deliberately NOT "ok". "Undetermined" is not good news, and colouring it
  // green would invite reading a gap in knowledge as a clean bill of health.
  unknown: "neutral",
  none: "ok",
};

/** What kind of update is available. */
export function UpdateBadge({ update }: { update: UpdateType }) {
  return (
    <StatusBadge
      tone={updateTones[update] ?? "neutral"}
      label={UPDATE_TYPE_LABELS[update] ?? update}
      title={UPDATE_TYPE_MEANINGS[update] ?? ""}
    />
  );
}

const statusTones: Record<CheckStatus, BadgeTone> = {
  ok: "ok",
  pending: "neutral",
  failed: "danger",
  rateLimited: "warn",
  unauthorized: "neutral",
  notFound: "neutral",
  unsupported: "neutral",
};

/**
 * The outcome of the most recent lookup.
 *
 * `ok` is rendered as a badge only when something else needs explaining — a
 * successful check is the expected state, and a green badge on every row is
 * noise rather than information.
 */
export function CheckStatusBadge({ status }: { status: CheckStatus }) {
  return (
    <StatusBadge
      tone={statusTones[status] ?? "neutral"}
      label={CHECK_STATUS_LABELS[status] ?? status}
      title={CHECK_STATUS_MEANINGS[status] ?? ""}
    />
  );
}

/** Which provider serves the registry. */
export function RegistryBadge({ registry }: { registry: string }) {
  return <StatusBadge tone="neutral" label={registry || "unknown"} />;
}

/**
 * A registry host's health.
 *
 * Rate limiting is called out separately from an outage, because an operator
 * reads them very differently: one resolves by waiting, the other needs looking
 * into.
 */
export function RegistryHealthBadge({ health }: { health: RegistryHealth }) {
  if (health.rateLimited) {
    return (
      <StatusBadge
        tone="warn"
        label="rate limited"
        title="The registry asked HarborMaster to slow down. Checks resume on its schedule; nothing is lost."
      />
    );
  }
  if (health.consecutiveFailures > 0) {
    return (
      <StatusBadge
        tone="danger"
        label="unreachable"
        title={`${health.consecutiveFailures} consecutive failures. Previously discovered updates are retained.`}
      />
    );
  }
  return (
    <StatusBadge
      tone="ok"
      label="reachable"
      title="The registry is answering."
    />
  );
}

/**
 * Renders the digest comparison, or explains its absence.
 *
 * A missing digest on either side is an ABSENCE OF EVIDENCE, not a difference.
 * Saying so is what stops a locally built image reading as out of date.
 */
export function DigestComparison({ intel }: { intel: ImageIntel }) {
  if (!intel.localDigest && !intel.remoteDigest) {
    return (
      <p className="text-xs text-content-muted">
        No digest is available on either side, so the running image and the
        registry cannot be compared.
      </p>
    );
  }

  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
      <dt className="text-content-muted">Running</dt>
      <dd className="font-mono break-all text-content">
        {intel.localDigest || "—"}
      </dd>
      <dt className="text-content-muted">Registry</dt>
      <dd className="font-mono break-all text-content">
        {intel.remoteDigest || "—"}
      </dd>
    </dl>
  );
}
