import type {
  DriftCategory,
  DriftChangeKind,
  DriftRecord,
  DriftSeverity,
  DriftStatus,
} from "../api/driftTypes";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

/**
 * Drift badges.
 *
 * Built on the shared StatusBadge, so drift looks like the rest of the app and
 * inherits the rule that colour is never the only signal: every badge carries
 * a label, and the tone only reinforces it.
 */

const severityTones: Record<DriftSeverity, BadgeTone> = {
  critical: "danger",
  high: "danger",
  medium: "warn",
  low: "neutral",
};

/**
 * Severity, with its meaning in the tooltip.
 *
 * critical and high share the danger tone deliberately: both mean "look at
 * this now", and inventing a fifth colour to separate them would dilute the
 * one signal that matters.
 */
export function SeverityBadge({ severity }: { severity: DriftSeverity }) {
  const meaning: Record<DriftSeverity, string> = {
    critical: "A containment boundary was lost, or the running image changed",
    high: "The attack surface widened, or a safety net was removed",
    medium: "Behaviour changed in a way that could matter",
    low: "Bookkeeping: labels, timings, metadata",
  };

  return (
    <StatusBadge
      tone={severityTones[severity]}
      label={severity}
      title={meaning[severity]}
    />
  );
}

const statusTones: Record<DriftStatus, BadgeTone> = {
  active: "danger",
  resolved: "ok",
  acknowledged: "warn",
  ignored: "neutral",
  expected: "neutral",
};

/** Lifecycle status, with the engine/operator split explained in the tooltip. */
export function DriftStatusBadge({ status }: { status: DriftStatus }) {
  const meaning: Record<DriftStatus, string> = {
    active: "Present and unreviewed",
    resolved: "A later evaluation no longer saw it. Set by the engine, never by hand",
    acknowledged: "Seen and left in place",
    ignored: "Deliberately not reported on",
    expected: "Intended — the baseline is what is stale",
  };

  return (
    <StatusBadge tone={statusTones[status]} label={status} title={meaning[status]} />
  );
}

/** How the field differs. */
export function ChangeKindBadge({ kind }: { kind: DriftChangeKind }) {
  const tones: Record<DriftChangeKind, BadgeTone> = {
    added: "warn",
    removed: "warn",
    modified: "warn",
    // Not a change: a comparison that could not be made.
    unverifiable: "neutral",
  };
  const meaning: Record<DriftChangeKind, string> = {
    added: "Present now, absent in the baseline",
    removed: "Present in the baseline, absent now",
    modified: "Present in both, with a different value",
    unverifiable: "The two values could not be compared, so this field is unverified",
  };

  return <StatusBadge tone={tones[kind]} label={kind} title={meaning[kind]} />;
}

/** The configuration area that moved. */
export function CategoryBadge({ category }: { category: DriftCategory }) {
  return <StatusBadge tone="neutral" label={category} />;
}

/**
 * Renders a drift record's values, or explains their absence.
 *
 * A secret-backed field has no values to show — the API never sends them — so
 * this says what changed without pretending to know what to. Rendering an
 * empty string there would read as "the value is now blank", which is a
 * different and wrong statement.
 */
export function DriftValues({ record }: { record: DriftRecord }) {
  if (record.sensitive) {
    return (
      <p className="text-xs text-content-muted">
        <span className="font-medium text-warn">Value withheld.</span>{" "}
        This field is secret-backed: HarborMaster compares a keyed digest and
        never stores or displays the value itself.
      </p>
    );
  }

  if (record.kind === "unverifiable") {
    return (
      <p className="text-xs text-content-muted">
        The two values could not be compared, so this field is unverified.
      </p>
    );
  }

  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
      <dt className="text-content-muted">Baseline</dt>
      <dd className="font-mono break-all text-content">
        {record.previousValue === undefined || record.previousValue === ""
          ? "—"
          : record.previousValue}
      </dd>
      <dt className="text-content-muted">Current</dt>
      <dd className="font-mono break-all text-content">
        {record.currentValue === undefined || record.currentValue === ""
          ? "—"
          : record.currentValue}
      </dd>
    </dl>
  );
}
