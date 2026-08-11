import { useId, useState } from "react";

import { ApiError } from "../api/client";
import type { RollbackEligibility } from "../api/rollbackTypes";
import { ROLLBACK_REFUSAL_LABELS } from "../api/rollbackTypes";
import { rollBackExecution } from "../hooks/useRollbacks";

/**
 * The Roll Back action, with its confirmation.
 *
 * # This is the control that undoes the other control that stops things
 *
 * So it is a two-step control with a typed confirmation, exactly like
 * RecreateContainerAction. The first click opens a dialog that states,
 * unambiguously:
 *
 *   - the replacement WILL BE STOPPED and the original started in its place;
 *   - there is DOWNTIME between the two;
 *   - nothing is removed — the replacement is kept as evidence.
 *
 * The operator then types the container's name. That is not theatre: a
 * single-click control for an operation that takes a service down is the wrong
 * design, and a name that has to be typed is one an operator has to have read.
 *
 * # Why both container ids are shown
 *
 * A rollback is a swap between two specific containers, and the whole safety
 * argument rests on them being the two the record names. Showing the ids lets
 * an operator check that for themselves against `docker ps` before approving —
 * which is the only check HarborMaster cannot make on their behalf.
 *
 * # Hiding this control is not authorization
 *
 * `eligibility` is advice from the server so the page can explain itself, and
 * it is computed from the server's RECORDS alone — it issues no Docker call,
 * because the endpoint carrying it is polled. So an eligible verdict means
 * "nothing in the record rules this out", and the request can still be refused
 * once the server checks the live host. That refusal arrives as an error here
 * and is shown as one; it is not a failure of this component.
 *
 * A caller who skipped this component and posted anyway gets the same answer,
 * just later.
 */
export function RollbackContainerAction({
  executionId,
  eligibility,
  onRequested,
}: {
  /** The recreation to undo. The only thing the request carries. */
  executionId: string;
  /**
   * The server's advisory verdict. Absent means rollback is not configured on
   * this installation, which is rendered as "not available" rather than as a
   * refusal — an operator must not go looking for a check that never ran.
   */
  eligibility?: RollbackEligibility;
  /** Called once a request has been accepted, so the caller can refresh. */
  onRequested?: () => void;
}) {
  const [confirming, setConfirming] = useState(false);
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [accepted, setAccepted] = useState(false);
  const dialogId = useId();

  if (!eligibility) {
    return (
      <p className="text-xs text-content-muted">
        Manual rollback is not enabled on this installation, so nothing was
        checked. Enable it in the server configuration if you need it.
      </p>
    );
  }

  const containerName = eligibility.containerName ?? "";

  if (!eligibility.eligible) {
    return (
      <p
        role="status"
        className="rounded-lg border border-border-subtle bg-surface-sunken px-3 py-2 text-sm text-content-muted"
      >
        This recreation cannot be rolled back.{" "}
        {eligibility.reason ||
          (eligibility.refusal
            ? ROLLBACK_REFUSAL_LABELS[eligibility.refusal]
            : "HarborMaster refused the request.")}
      </p>
    );
  }

  const confirmed = typed.trim() === containerName;

  const rollBack = async () => {
    setError(null);
    setBusy(true);
    try {
      // The execution id is the whole request. There is no container, name, or
      // option to supply, and the server revalidates both container identities
      // against the live host before it stops anything.
      await rollBackExecution(executionId);
      setAccepted(true);
      setConfirming(false);
      onRequested?.();
    } catch (caught) {
      setError(
        caught instanceof ApiError
          ? caught.message
          : "The rollback could not be requested.",
      );
    } finally {
      setBusy(false);
    }
  };

  if (accepted) {
    return (
      <p
        role="status"
        className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-sm text-content"
      >
        Rollback requested. It runs in the background — watch the rollback record
        for what happens to {containerName}.
      </p>
    );
  }

  return (
    <div className="space-y-3">
      {error && (
        <p
          role="alert"
          className="rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger"
        >
          {error}
        </p>
      )}

      {!confirming ? (
        <button
          type="button"
          onClick={() => setConfirming(true)}
          className="rounded-md border border-warn/50 bg-surface-raised px-3 py-1.5 text-sm font-medium text-content"
          // Says what it does before the dialog opens, so the answer is
          // available to someone who only hovers.
          title="Stops the replacement container and starts the original this recreation preserved. The container is unavailable while this runs."
        >
          Roll back this recreation
        </button>
      ) : (
        <div
          role="dialog"
          aria-modal="false"
          aria-labelledby={`${dialogId}-title`}
          className="space-y-3 rounded-xl border border-danger/40 bg-danger-soft p-4"
        >
          <h4 id={`${dialogId}-title`} className="text-sm font-semibold text-danger">
            Roll {containerName} back?
          </h4>

          {/* The three facts, as a list, because each one is a separate thing
              an operator must have understood. */}
          <ul className="list-disc space-y-1.5 pl-5 text-xs text-content">
            <li>
              <strong>{containerName} will be unavailable while this runs.</strong>{" "}
              The replacement is stopped first, then the original is renamed back
              and started. There is a gap between the two, and the original has to
              start and pass its checks before service resumes.
            </li>
            <li>
              <strong>You are starting this one yourself.</strong> HarborMaster
              rolls back on its own only for an unattended update whose policy
              asks for it, and that pauses the container afterwards. Nothing rolls
              back on a schedule, and only one recreation is ever rolled back at a
              time.
            </li>
            <li>
              <strong>Nothing is removed.</strong> The replacement is stopped and
              renamed aside and stays on this host, so you can still diagnose why
              the recreation was backed out.
            </li>
          </ul>

          {/* The two specific containers, by id. This is the check
              HarborMaster cannot make on the operator's behalf. */}
          <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
            <dt className="text-content-muted">Will be stopped</dt>
            <dd className="font-mono break-all text-content">
              {containerName} ({(eligibility.replacementId ?? "").slice(0, 12)})
              {eligibility.replacementImage ? ` — ${eligibility.replacementImage}` : ""}
            </dd>

            <dt className="text-content-muted">Will be started</dt>
            <dd className="font-mono break-all text-content">
              {eligibility.parkedName} ({(eligibility.originalId ?? "").slice(0, 12)})
              {eligibility.originalImage ? ` — ${eligibility.originalImage}` : ""}
            </dd>
          </dl>

          <p className="text-xs text-content-muted">
            HarborMaster rechecks both container identities, their images, the
            production name, and this host's inventory immediately before it stops
            anything, and refuses if anything has changed. If the original does
            not come back correctly, HarborMaster stops, leaves both containers in
            place, and records the manual steps — it will not guess which
            container should serve traffic.
          </p>

          <label className="block space-y-1 text-xs text-content">
            <span>
              Type <span className="font-mono font-semibold">{containerName}</span>{" "}
              to confirm
            </span>
            <input
              type="text"
              value={typed}
              onChange={(event) => setTyped(event.target.value)}
              autoComplete="off"
              spellCheck={false}
              className="w-full rounded-md border border-border-subtle bg-surface px-2 py-1.5 font-mono text-sm text-content"
            />
          </label>

          <div className="flex flex-wrap gap-2">
            <button
              type="button"
              onClick={rollBack}
              disabled={busy || !confirmed}
              className="rounded-md border border-danger/50 bg-surface px-3 py-1.5 text-sm font-medium text-danger disabled:opacity-50"
            >
              {busy ? "Requesting…" : `Roll ${containerName} back`}
            </button>
            <button
              type="button"
              onClick={() => {
                setConfirming(false);
                setTyped("");
              }}
              disabled={busy}
              className="rounded-md border border-border-subtle bg-surface px-3 py-1.5 text-sm text-content-muted disabled:opacity-60"
            >
              Cancel
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
