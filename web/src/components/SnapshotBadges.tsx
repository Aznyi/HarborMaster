import type {
  ChangeKind,
  ReadinessStatus,
  SnapshotTrigger,
} from "../api/snapshotTypes";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

/**
 * Badges for snapshot state.
 *
 * Colour is never the only signal -- every badge spells out its meaning in the
 * label -- so tone is emphasis, not information.
 */

/**
 * Maps a readiness verdict onto a badge tone.
 *
 * `unverifiable` is deliberately `warn` rather than `neutral`: HarborMaster
 * could not establish the answer, and rendering that as unremarkable would let
 * an operator read "we did not check" as "nothing to worry about".
 */
export function readinessTone(status: ReadinessStatus): BadgeTone {
  switch (status) {
    case "ready":
      return "ok";
    case "warning":
    case "unverifiable":
      return "warn";
    case "not_ready":
      return "danger";
    case "unknown":
    default:
      return "neutral";
  }
}

/** Human-readable readiness label. */
export function readinessLabel(status: ReadinessStatus): string {
  switch (status) {
    case "ready":
      return "Ready";
    case "warning":
      return "Warning";
    case "not_ready":
      return "Not ready";
    case "unverifiable":
      return "Unverifiable";
    case "unknown":
    default:
      return "Not evaluated";
  }
}

export function ReadinessBadge({ status }: { status: ReadinessStatus }) {
  return <StatusBadge tone={readinessTone(status)} label={readinessLabel(status)} />;
}

/** Human-readable trigger label. */
export function triggerLabel(trigger: SnapshotTrigger): string {
  switch (trigger) {
    case "manual":
      return "Manual";
    case "api":
      return "API";
    case "scheduled":
      return "Scheduled";
    case "pre_update":
      return "Pre-update";
    default:
      return trigger;
  }
}

export function TriggerBadge({ trigger }: { trigger: SnapshotTrigger }) {
  return <StatusBadge tone="neutral" label={triggerLabel(trigger)} />;
}

/** Maps a diff change onto a badge tone. */
export function changeTone(kind: ChangeKind): BadgeTone {
  switch (kind) {
    case "added":
      return "ok";
    case "removed":
      return "danger";
    case "modified":
      return "warn";
    case "unverifiable":
      return "warn";
    case "unchanged":
    default:
      return "neutral";
  }
}

export function changeLabel(kind: ChangeKind): string {
  switch (kind) {
    case "added":
      return "Added";
    case "removed":
      return "Removed";
    case "modified":
      return "Modified";
    case "unverifiable":
      return "Unverifiable";
    case "unchanged":
    default:
      return "Unchanged";
  }
}

export function ChangeBadge({ kind }: { kind: ChangeKind }) {
  return <StatusBadge tone={changeTone(kind)} label={changeLabel(kind)} />;
}

/**
 * The marker shown wherever a sensitive value would otherwise appear.
 *
 * There is no "reveal" affordance and there must never be one: the value is not
 * withheld from the UI, it is absent from the payload and from the database.
 * HarborMaster cannot show it because HarborMaster does not have it.
 */
export function SensitiveMarker({ length }: { length?: number }) {
  return (
    <span className="inline-flex items-center gap-1.5 text-content-muted">
      <span aria-hidden="true" className="font-mono">
        ********
      </span>
      <span className="text-xs">
        {length ? `not stored (${length} bytes)` : "not stored"}
      </span>
    </span>
  );
}
