import type {
  Rollback,
  RollbackCheckpoint,
  RollbackFailure,
  RollbackState,
} from "../api/rollbackTypes";
import {
  ROLLBACK_CHECKPOINT_LABELS,
  ROLLBACK_FAILURE_LABELS,
  ROLLBACK_STATE_LABELS,
  ROLLBACK_STATE_MEANING,
  rollbackHostChanged,
  rollbackNeedsAttention,
} from "../api/rollbackTypes";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

/**
 * Manual rollback badges.
 *
 * Built on the shared StatusBadge, so rollback looks like the rest of the app
 * and inherits the rule that colour is never the only signal.
 *
 * Three rendering decisions carry most of the weight:
 *
 *  1. A verification result of `unknown` is never styled like a pass. That is
 *     handled by the shared VerificationBadge, which this file deliberately
 *     reuses rather than reimplementing.
 *  2. A FAILED rollback that changed the host is styled as danger and says so.
 *     One that changed nothing is a warning, because the operator has nothing
 *     to do.
 *  3. `originalStarted` is NOT a service interruption. The container is running
 *     under its own name; what is missing is the proof that it is running
 *     correctly, and telling an operator their site is down when it is up would
 *     send them to the wrong emergency.
 */

const stateTones: Record<RollbackState, BadgeTone> = {
  queued: "neutral",
  validating: "neutral",
  // The mutating states are warn rather than neutral: something is being
  // changed, and the colour should say so before the label is read.
  stoppingReplacement: "warn",
  restoringName: "warn",
  startingOriginal: "warn",
  verifyingOriginal: "warn",
  succeeded: "ok",
  failed: "danger",
  cancelled: "neutral",
  expired: "neutral",
};

/** Where a rollback has got to, with what that means in the tooltip. */
export function RollbackStateBadge({ state }: { state: RollbackState }) {
  return (
    <StatusBadge
      tone={stateTones[state]}
      label={ROLLBACK_STATE_LABELS[state]}
      title={ROLLBACK_STATE_MEANING[state]}
    />
  );
}

/**
 * What is TRUE OF THE HOST.
 *
 * Shown alongside the state rather than instead of it, because the two answer
 * different questions and an operator reading a failure needs the second.
 */
export function RollbackCheckpointBadge({
  checkpoint,
  mutated,
}: {
  checkpoint: RollbackCheckpoint | undefined;
  /** Whether a mutation was issued, even if no checkpoint confirmed it. */
  mutated: boolean;
}) {
  const value = checkpoint ?? "";

  // The uncertain case: a stop was issued and never confirmed. It must not
  // render as "nothing changed".
  if (value === "" && mutated) {
    return (
      <StatusBadge
        tone="danger"
        label="State unconfirmed"
        title={
          "HarborMaster asked for the replacement to be stopped and was interrupted " +
          "before it could confirm the result. Check which container is running"
        }
      />
    );
  }

  const tones: Record<RollbackCheckpoint, BadgeTone> = {
    "": "neutral",
    replacementStopped: "danger",
    replacementParked: "danger",
    originalRestored: "danger",
    // The original is running under its own name. Service is back; only the
    // proof is missing.
    originalStarted: "warn",
    originalVerified: "ok",
  };

  const meaning: Record<RollbackCheckpoint, string> = {
    "": "Nothing on this host was changed",
    replacementStopped:
      "The replacement container was stopped and still carries the production name",
    replacementParked:
      "The replacement was stopped and renamed aside, so the production name is free",
    originalRestored:
      "The original carries its own name again and has not been started",
    originalStarted:
      "The original is running under its own name and has not yet been proved",
    originalVerified: "The original passed every verification",
  };

  return (
    <StatusBadge
      tone={tones[value]}
      label={ROLLBACK_CHECKPOINT_LABELS[value]}
      title={meaning[value]}
    />
  );
}

/** Why a rollback did not succeed. */
export function RollbackFailureBadge({ failure }: { failure: RollbackFailure }) {
  const meaning: Record<RollbackFailure, string> = {
    preflight:
      "The safety checks refused this rollback. Nothing on this host was changed",
    stop: "The replacement container could not be stopped",
    rename:
      "A container could not be renamed, so the names on this host may not be the ones you expect",
    start: "The original container would not start",
    healthTimeout: "The original did not become healthy within its time budget",
    unhealthy: "The original reported unhealthy",
    notStable:
      "The original did not stay running long enough to be considered stable",
    imageMismatch:
      "The running original is not on the image the recreation recorded it as running",
    preservation:
      "The original's configuration is not what it was before the rollback moved it. Check the differences below",
    network: "The original is not attached to every network it was on",
    dockerUnavailable: "The Docker daemon stopped answering partway through",
    timeout: "The rollback exceeded its time budget",
    interrupted:
      "HarborMaster restarted while this rollback was in progress, so its outcome was never confirmed",
    persistence:
      "HarborMaster could not record what it had just done, so it stopped rather than act again on an uncertain record",
    internal: "HarborMaster could not complete the rollback",
  };

  // A failure that left containers behind is danger; one that changed nothing
  // is a warning. The distinction is what tells an operator whether to act now.
  const untouched = failure === "preflight";

  return (
    <StatusBadge
      tone={untouched ? "warn" : "danger"}
      label={ROLLBACK_FAILURE_LABELS[failure]}
      title={meaning[failure]}
    />
  );
}

/**
 * The banner every rollback view carries.
 *
 * Stated once, plainly, and never conditionally. An operator who reads nothing
 * else on the page should still leave knowing that a rollback causes downtime
 * and that HarborMaster will not do it by itself.
 */
export function RollbackWarningNotice() {
  return (
    <p
      role="note"
      className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-xs text-content"
    >
      Rolling back <strong>stops the replacement container and starts the one it
      replaced</strong>. There is a gap between the two, so the container is
      unavailable while the rollback runs.{" "}
      <strong>A rollback is started either by a person or by the update policy
      that made the failed change</strong> — never on a schedule, and only ever
      one recreation at a time. A policy rolls back only when it is set to, and a
      rollback it starts pauses the container afterwards. The replacement is
      kept, stopped and renamed aside, as the evidence of why the recreation was
      backed out; nothing is removed.
    </p>
  );
}

/**
 * The one-line summary of what a record means for the host right now.
 *
 * Rendered above everything else on a failed record, because "what do I have to
 * do about this" is the only question an operator opens that page with.
 */
export function RollbackHostState({ rollback }: { rollback: Rollback }) {
  if (rollback.state === "succeeded") {
    return (
      <p
        role="status"
        className="rounded-lg border border-ok/40 bg-ok-soft px-3 py-2 text-sm text-ok"
      >
        {rollback.containerName} is running its original image again under its
        own name, and passed every check. The replacement is stopped and kept as
        evidence
        {rollback.replacementParkedName
          ? ` under ${rollback.replacementParkedName}.`
          : "."}
      </p>
    );
  }

  if (!rollbackNeedsAttention(rollback)) {
    if (
      rollback.state === "failed" ||
      rollback.state === "cancelled" ||
      rollback.state === "expired"
    ) {
      return (
        <p
          role="status"
          className="rounded-lg border border-border-subtle bg-surface-sunken px-3 py-2 text-sm text-content-muted"
        >
          Nothing on this host was changed. {rollback.containerName} is exactly
          as it was.
        </p>
      );
    }
    return null;
  }

  return (
    <p
      role="alert"
      className="rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger"
    >
      This rollback changed this host and did not finish.{" "}
      {rollback.recovery?.serviceInterrupted
        ? `${rollback.containerName} is NOT running.`
        : `${rollback.containerName} needs checking.`}{" "}
      The recovery steps below say exactly what was left and what to do.
    </p>
  );
}

/**
 * Where the containers are, by name and id.
 *
 * The most practically useful thing on a failed record: names can have been
 * reorganised halfway, and ids cannot.
 */
export function RollbackContainerIdentities({ rollback }: { rollback: Rollback }) {
  const changed = rollbackHostChanged(rollback);

  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
      <dt className="text-content-muted">Original container</dt>
      <dd className="font-mono break-all text-content">
        {changed && rollback.checkpoint && rollback.checkpoint !== "replacementStopped"
          ? rollback.containerName
          : rollback.parkedName}{" "}
        ({rollback.originalId.slice(0, 12)})
      </dd>

      <dt className="text-content-muted">Replacement container</dt>
      <dd className="font-mono break-all text-content">
        {rollback.replacementParkedName || rollback.containerName} (
        {rollback.replacementId.slice(0, 12)})
      </dd>

      <dt className="text-content-muted">Production name</dt>
      <dd className="font-mono break-all text-content">{rollback.containerName}</dd>
    </dl>
  );
}

/** What the container is being moved back onto. */
export function RollbackImageDetail({ rollback }: { rollback: Rollback }) {
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
      <dt className="text-content-muted">Backing out of</dt>
      <dd className="font-mono break-all text-content">
        {rollback.replacementImage || "—"}
      </dd>

      <dt className="text-content-muted">Returning to</dt>
      <dd className="font-mono break-all text-content">
        {rollback.originalImage || "—"}
      </dd>

      {rollback.originalImageId && (
        <>
          <dt className="text-content-muted">Original image id</dt>
          <dd className="font-mono break-all text-content">
            {rollback.originalImageId}
          </dd>
        </>
      )}
    </dl>
  );
}
