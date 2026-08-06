import { useId, useState } from "react";

import { ApiError } from "../api/client";
import type { ChangePlan } from "../api/planTypes";
import { acquireForPlan } from "../hooks/useAcquisitions";

/**
 * The Acquire Image action, with its confirmation.
 *
 * # This is the one button in HarborMaster that changes the host
 *
 * So it is deliberately a two-step control. The first click opens a
 * confirmation that states, unambiguously:
 *
 *   - the exact repository and DIGEST that will be downloaded, not a tag;
 *   - that the container will NOT be updated by this.
 *
 * The second is the one that acts. A single-click download would be a
 * reasonable design for a read-only tool and is the wrong one here.
 *
 * # Why the digest is shown rather than the tag
 *
 * A tag is a name and can move. Asking an operator to approve "nginx:1.27.1"
 * asks them to approve a label; asking them to approve a digest asks them to
 * approve content, which is the thing HarborMaster can actually guarantee it
 * fetched.
 */
export function AcquireImageAction({
  plan,
  onRequested,
}: {
  plan: ChangePlan;
  /** Called once a request has been accepted, so the caller can refresh. */
  onRequested?: () => void;
}) {
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [accepted, setAccepted] = useState(false);
  const dialogId = useId();

  // Without a proposed digest there is nothing immutable to fetch, and the
  // server would refuse. Saying so here is more useful than offering a control
  // that cannot work.
  if (!plan.proposedDigest) {
    return (
      <p className="text-xs text-content-muted">
        This plan names no resolved digest, so there is no immutable image to
        acquire.
      </p>
    );
  }

  const acquire = async () => {
    setError(null);
    setBusy(true);
    try {
      // The plan id is the whole request. There is no target to supply, and the
      // server revalidates the plan before downloading anything.
      await acquireForPlan(plan.planId);
      setAccepted(true);
      setConfirming(false);
      onRequested?.();
    } catch (caught) {
      setError(
        caught instanceof ApiError
          ? caught.message
          : "The image could not be requested.",
      );
    } finally {
      setBusy(false);
    }
  };

  if (accepted) {
    return (
      <p
        role="status"
        className="rounded-lg border border-ok/40 bg-ok-soft px-3 py-2 text-sm text-ok"
      >
        Download requested. It runs in the background — no container has been
        changed.
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
          className="rounded-md border border-border-subtle bg-surface-raised px-3 py-1.5 text-sm font-medium text-content"
          // Says what it does and what it does not do, so the answer is
          // available before the dialog opens.
          title="Downloads this image to this host. It does NOT update the container."
        >
          Acquire image
        </button>
      ) : (
        <div
          role="dialog"
          aria-modal="false"
          aria-labelledby={`${dialogId}-title`}
          className="space-y-3 rounded-xl border border-warn/40 bg-warn-soft p-4"
        >
          <h4 id={`${dialogId}-title`} className="text-sm font-semibold text-content">
            Download this image to this host?
          </h4>

          {/* The exact content, not the name. */}
          <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
            <dt className="text-content-muted">Repository</dt>
            <dd className="font-mono break-all text-content">{plan.proposedImage}</dd>
            <dt className="text-content-muted">Digest</dt>
            <dd className="font-mono break-all text-content">{plan.proposedDigest}</dd>
          </dl>

          <p className="text-xs text-content">
            <strong>
              This does not update {plan.containerName || "the container"}.
            </strong>{" "}
            The image is downloaded to this host and nothing else happens —
            no container is stopped, restarted, or recreated. Applying it is
            something you do with your own tooling.
          </p>

          <p className="text-xs text-content-muted">
            HarborMaster rechecks the plan and its evidence immediately before
            downloading, and refuses if anything has changed since it was
            written.
          </p>

          <div className="flex flex-wrap gap-2">
            <button
              type="button"
              onClick={acquire}
              disabled={busy}
              className="rounded-md border border-warn/50 bg-surface px-3 py-1.5 text-sm font-medium text-content disabled:opacity-60"
            >
              {busy ? "Requesting…" : "Download image"}
            </button>
            <button
              type="button"
              onClick={() => setConfirming(false)}
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
