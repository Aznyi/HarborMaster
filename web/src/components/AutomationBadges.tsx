import type {
  AutomationMode,
  AutomationReason,
  AutomationRunState,
  AutomationVerdict,
  PauseReason,
  UpdateStrategy,
} from "../api/automationTypes";
import {
  AUTOMATION_MODE_LABELS,
  AUTOMATION_REASON_LABELS,
  AUTOMATION_RUN_STATE_LABELS,
  AUTOMATION_VERDICT_LABELS,
  PAUSE_REASON_LABELS,
  UPDATE_STRATEGY_DESCRIPTIONS,
  UPDATE_STRATEGY_LABELS,
} from "../api/automationTypes";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

/**
 * The automation vocabulary, rendered.
 *
 * # Colour carries the risk, and the label carries the same information
 *
 * The one thing this file must get right is that a policy which CHANGES THE
 * HOST never looks like one that merely watches it. `automatic` is the only
 * mode that acts, and it is the only one rendered in a warning tone — every
 * other mode is neutral, because "observe" reporting an alarming colour would
 * make the alarming one ordinary.
 *
 * Nothing here is colour-only. Every badge's text says what the colour says, so
 * an operator who cannot distinguish the tones reads the same page.
 */

/**
 * How far a policy may act.
 *
 * `automatic` warns. The other three change nothing and say so plainly.
 */
export function AutomationModeBadge({ mode }: { mode: AutomationMode }) {
  const tone: BadgeTone = mode === "automatic" ? "warn" : "neutral";
  const label = AUTOMATION_MODE_LABELS[mode] ?? mode;
  const title =
    mode === "automatic"
      ? "This policy stops and replaces containers without asking."
      : "This policy changes nothing on its own.";

  return <StatusBadge tone={tone} label={label} title={title} />;
}

/**
 * How far an update may go.
 *
 * `major` warns, because a major version is where publishers put breaking
 * changes. The rest are neutral: the ceiling is a setting, not an alarm.
 */
export function UpdateStrategyBadge({ strategy }: { strategy: UpdateStrategy }) {
  const tone: BadgeTone = strategy === "major" ? "warn" : "neutral";
  return (
    <StatusBadge
      tone={tone}
      label={UPDATE_STRATEGY_LABELS[strategy] ?? strategy}
      title={UPDATE_STRATEGY_DESCRIPTIONS[strategy] ?? strategy}
    />
  );
}

/** Where one pass got to. */
export function AutomationRunStateBadge({ state }: { state: AutomationRunState }) {
  const tones: Record<AutomationRunState, BadgeTone> = {
    running: "warn",
    completed: "ok",
    failed: "danger",
    // A restart cut it short. A bookkeeping gap, never a host in an unknown
    // state — a pass submits work to services that checkpoint their own.
    interrupted: "warn",
  };
  return (
    <StatusBadge
      tone={tones[state] ?? "neutral"}
      label={AUTOMATION_RUN_STATE_LABELS[state] ?? state}
    />
  );
}

/** What the engine concluded for one container. */
export function AutomationVerdictBadge({
  verdict,
}: {
  verdict: AutomationVerdict;
}) {
  const tones: Record<AutomationVerdict, BadgeTone> = {
    // The only verdict that changed anything.
    update: "warn",
    wouldUpdate: "neutral",
    awaitingApproval: "warn",
    skip: "neutral",
  };
  return (
    <StatusBadge
      tone={tones[verdict] ?? "neutral"}
      label={AUTOMATION_VERDICT_LABELS[verdict] ?? verdict}
    />
  );
}

/**
 * Why a container was, or was not, updated.
 *
 * Rendered as plain text rather than a pill: on a list of two hundred
 * containers the reason is the column an operator reads, and two hundred pills
 * is a page nobody can scan.
 */
export function AutomationReasonText({
  reason,
  detail,
}: {
  reason: AutomationReason;
  detail?: string;
}) {
  const label = AUTOMATION_REASON_LABELS[reason] ?? reason;
  return (
    <span className="text-sm text-content-muted" title={detail || label}>
      {label}
    </span>
  );
}

/**
 * Why automation stopped for a container.
 *
 * A rollback pause is the loudest: the change was wrong AND the host was moved
 * twice to discover it.
 */
export function PauseReasonBadge({ reason }: { reason: PauseReason }) {
  const tones: Record<PauseReason, BadgeTone> = {
    repeatedFailure: "danger",
    automaticRollback: "danger",
    operator: "neutral",
  };
  return (
    <StatusBadge
      tone={tones[reason] ?? "neutral"}
      label={PAUSE_REASON_LABELS[reason] ?? reason}
    />
  );
}

/**
 * The notice every automation page carries when the engine can act.
 *
 * Stated before any control is offered, and stated in terms of what HAPPENS
 * rather than of what is configured: an operator reading "automation is
 * enabled" learns less than one reading "containers will be stopped and
 * replaced without asking".
 */
export function AutomationWarningNotice({ enabled }: { enabled: boolean }) {
  if (!enabled) {
    return (
      <p
        role="status"
        className="rounded-lg border border-border-subtle bg-surface px-3 py-2 text-sm text-content-muted"
      >
        The update engine is switched off in this deployment. Policies can still
        be written and reviewed, and nothing will act on them until it is turned
        on.
      </p>
    );
  }

  return (
    <p
      role="status"
      className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-sm text-warn"
    >
      The update engine is on. A policy in <strong>Automatic</strong> mode will
      stop and replace matching containers without asking, inside its
      maintenance window. Every other mode changes nothing.
    </p>
  );
}
