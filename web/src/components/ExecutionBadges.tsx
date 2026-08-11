import type {
  Execution,
  ExecutionCheckpoint,
  ExecutionFailure,
  ExecutionState,
  VerificationResult,
} from "../api/executionTypes";
import {
  EXECUTION_CHECKPOINT_LABELS,
  EXECUTION_FAILURE_LABELS,
  EXECUTION_STATE_LABELS,
  EXECUTION_STATE_MEANING,
  hostChanged,
  needsAttention,
  pinnedReference,
} from "../api/executionTypes";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

/**
 * Container recreation badges.
 *
 * Built on the shared StatusBadge, so recreation looks like the rest of the app
 * and inherits the rule that colour is never the only signal.
 *
 * Two rendering decisions carry most of the weight here:
 *
 *  1. A verification result of `unknown` is never styled like a pass. A proof
 *     that was not reached established nothing, and a grey tick would be the
 *     single most misleading thing this UI could draw.
 *  2. A FAILED recreation that changed the host is styled as danger and says
 *     so, because the operator has containers to settle. One that changed
 *     nothing is styled as a warning, because they have nothing to do.
 */

const stateTones: Record<ExecutionState, BadgeTone> = {
  queued: "neutral",
  validating: "neutral",
  capturing: "neutral",
  // The mutating states are warn rather than neutral: something is being
  // changed, and the colour should say so before the label is read.
  creating: "warn",
  starting: "warn",
  verifying: "warn",
  succeeded: "ok",
  failed: "danger",
  cancelled: "neutral",
  expired: "neutral",
};

/** Where a recreation has got to, with what that means in the tooltip. */
export function ExecutionStateBadge({ state }: { state: ExecutionState }) {
  return (
    <StatusBadge
      tone={stateTones[state]}
      label={EXECUTION_STATE_LABELS[state]}
      title={EXECUTION_STATE_MEANING[state]}
    />
  );
}

/**
 * What is TRUE OF THE HOST.
 *
 * Shown alongside the state rather than instead of it, because the two answer
 * different questions and an operator reading a failure needs the second.
 */
export function ExecutionCheckpointBadge({
  checkpoint,
  mutated,
}: {
  checkpoint: ExecutionCheckpoint | undefined;
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
          "HarborMaster asked for the container to be stopped and was interrupted " +
          "before it could confirm the result. Check whether the container is running"
        }
      />
    );
  }

  const tones: Record<ExecutionCheckpoint, BadgeTone> = {
    "": "neutral",
    originalStopped: "danger",
    originalParked: "danger",
    replacementCreated: "danger",
    replacementStarted: "danger",
    replacementVerified: "warn",
    replacementQuarantined: "danger",
    originalRemoved: "ok",
  };

  const meaning: Record<ExecutionCheckpoint, string> = {
    "": "Nothing on this host was changed",
    originalStopped: "The original container was stopped and still carries its own name",
    originalParked: "The original container was stopped and renamed aside",
    replacementCreated: "The replacement container was created and not started",
    replacementStarted: "The replacement container was started and not yet proved",
    replacementVerified: "The replacement passed every verification",
    replacementQuarantined:
      "The replacement was stopped and renamed aside so you can diagnose it",
    originalRemoved: "The original was removed and the recreation is complete",
  };

  return (
    <StatusBadge
      tone={tones[value]}
      label={EXECUTION_CHECKPOINT_LABELS[value]}
      title={meaning[value]}
    />
  );
}

/** Why a recreation did not succeed. */
export function ExecutionFailureBadge({ failure }: { failure: ExecutionFailure }) {
  const meaning: Record<ExecutionFailure, string> = {
    preflight:
      "The safety checks refused this recreation. Nothing on this host was changed",
    capture:
      "The container's configuration could not be read, so there was nothing to reproduce. Nothing was changed",
    stop: "The original container could not be stopped",
    rename:
      "A container could not be renamed, so the names on this host may not be the ones you expect",
    create:
      "The replacement could not be created. The original is stopped and preserved",
    start: "The replacement was created but would not start",
    healthTimeout: "The replacement did not become healthy within its time budget",
    unhealthy: "The replacement reported unhealthy",
    notStable: "The replacement did not stay running long enough to be considered stable",
    imageMismatch: "The running replacement is not on the image that was approved",
    preservation:
      "The replacement's configuration does not match the original's. Check the differences below",
    network: "The replacement is not attached to every network the original was on",
    secretUnavailable:
      "A value this container needs could not be reproduced, so nothing was changed",
    dockerUnavailable: "The Docker daemon stopped answering partway through",
    timeout: "The recreation exceeded its time budget",
    interrupted:
      "HarborMaster restarted while this recreation was in progress, so its outcome was never confirmed",
    persistence:
      "HarborMaster could not record what it had just done, so it stopped rather than act again on an uncertain record",
    internal: "HarborMaster could not complete the recreation",
  };

  // A failure that left containers behind is danger; one that changed nothing
  // is a warning. The distinction is what tells an operator whether to act now.
  const untouched =
    failure === "preflight" || failure === "capture" || failure === "secretUnavailable";

  return (
    <StatusBadge
      tone={untouched ? "warn" : "danger"}
      label={EXECUTION_FAILURE_LABELS[failure]}
      title={meaning[failure]}
    />
  );
}

/** One proof's verdict. */
export function VerificationBadge({
  label,
  result,
  detail,
}: {
  label: string;
  result: VerificationResult;
  detail?: string;
}) {
  const tones: Record<VerificationResult, BadgeTone> = {
    // NOT ok. An unknown established nothing, and the parked original is
    // removed only when every proof reads passed.
    unknown: "neutral",
    passed: "ok",
    failed: "danger",
  };
  const suffix: Record<VerificationResult, string> = {
    unknown: "not checked",
    passed: "passed",
    failed: "failed",
  };

  return (
    <StatusBadge
      tone={tones[result]}
      label={`${label}: ${suffix[result]}`}
      title={
        detail ??
        (result === "unknown"
          ? "This check was never reached, so nothing about it was established"
          : `The ${label.toLowerCase()} check ${suffix[result]}`)
      }
    />
  );
}

/**
 * The banner every recreation view carries.
 *
 * Stated once, plainly, and never conditionally. An operator who reads nothing
 * else on the page should still leave knowing that this feature stops
 * containers and does not roll back.
 */
export function RecreationWarningNotice() {
  return (
    <p
      role="note"
      className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-xs text-content"
    >
      Recreating a container{" "}
      <strong>stops it and replaces it with a new one</strong> built from its own
      configuration and an image already downloaded to this host. The original is
      kept until the replacement passes every check, and then removed.{" "}
      <strong>A recreation you start here is not rolled back automatically</strong>{" "}
      — if the replacement fails, both containers are left in place and
      HarborMaster records the manual steps to restore service. Only an
      unattended update run by an update policy can roll itself back, and only
      when that policy asks for it.
    </p>
  );
}

/**
 * The one-line summary of what a record means for the host right now.
 *
 * Rendered above everything else on a failed record, because "what do I have to
 * do about this" is the only question an operator opens that page with.
 */
export function ExecutionHostState({ execution }: { execution: Execution }) {
  if (execution.state === "succeeded") {
    return (
      <p
        role="status"
        className="rounded-lg border border-ok/40 bg-ok-soft px-3 py-2 text-sm text-ok"
      >
        {execution.containerName} is running the new image and passed every check.
        {execution.originalRemoved
          ? " The original has been removed."
          : " The original could not be removed and is still on this host under its parked name."}
      </p>
    );
  }

  if (!needsAttention(execution)) {
    if (execution.state === "failed" || execution.state === "cancelled") {
      return (
        <p
          role="status"
          className="rounded-lg border border-border-subtle bg-surface-sunken px-3 py-2 text-sm text-content-muted"
        >
          Nothing on this host was changed. {execution.containerName} is exactly
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
      This recreation changed this host and did not finish.{" "}
      {execution.recovery?.serviceInterrupted
        ? `${execution.containerName} is NOT running.`
        : `${execution.containerName} needs checking.`}{" "}
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
export function ExecutionContainerIdentities({ execution }: { execution: Execution }) {
  if (!hostChanged(execution)) return null;

  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
      <dt className="text-content-muted">Original container</dt>
      <dd className="font-mono break-all text-content">
        {execution.parkedName || execution.containerName} ({execution.containerId.slice(0, 12)})
      </dd>

      {execution.replacementId && (
        <>
          <dt className="text-content-muted">Replacement container</dt>
          <dd className="font-mono break-all text-content">
            {execution.quarantineName || execution.containerName} (
            {execution.replacementId.slice(0, 12)})
          </dd>
        </>
      )}
    </dl>
  );
}

/**
 * What the container is being moved onto, spelled out unambiguously.
 *
 * The digest-pinned form, deliberately. A confirmation that said "nginx:1.27.1"
 * would be asking someone to approve a NAME; this asks them to approve CONTENT,
 * which is the thing that cannot change afterwards.
 */
export function ExecutionTargetDetail({ execution }: { execution: Execution }) {
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
      <dt className="text-content-muted">From</dt>
      <dd className="font-mono break-all text-content">{execution.oldImage}</dd>

      <dt className="text-content-muted">To</dt>
      <dd className="font-mono break-all text-content">
        {execution.target.reference || execution.target.repository}
      </dd>

      <dt className="text-content-muted">Digest</dt>
      <dd className="font-mono break-all text-content">{execution.target.digest}</dd>

      {execution.target.platform && (
        <>
          <dt className="text-content-muted">Platform</dt>
          <dd className="text-content">
            {execution.target.platform.os}/{execution.target.platform.architecture}
            {execution.target.platform.variant
              ? `/${execution.target.platform.variant}`
              : ""}
          </dd>
        </>
      )}

      <dt className="text-content-muted">Pinned reference</dt>
      <dd className="font-mono break-all text-content">
        {pinnedReference(execution.target)}
      </dd>
    </dl>
  );
}
