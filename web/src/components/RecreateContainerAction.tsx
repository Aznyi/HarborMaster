import { useId, useState } from "react";

import type { Acquisition } from "../api/acquisitionTypes";
import { pinnedReference } from "../api/acquisitionTypes";
import { ApiError } from "../api/client";
import { recreateForAcquisition } from "../hooks/useExecutions";

/**
 * The Recreate Container action, with its confirmation.
 *
 * # This is the one control in HarborMaster that stops something running
 *
 * So it is deliberately a two-step control with a typed confirmation. The first
 * click opens a dialog that states, unambiguously:
 *
 *   - the container WILL BE STOPPED AND REPLACED;
 *   - the image is already on this host and was already verified;
 *   - rollback is NOT automatic.
 *
 * The operator then types the container's name. That is not theatre: a
 * single-click control for an operation that takes a service down is the wrong
 * design, and a name that has to be typed is one an operator has to have read.
 *
 * # Why the digest is shown rather than the tag
 *
 * A tag is a name and can move. Asking an operator to approve "nginx:1.27.1"
 * asks them to approve a label; asking them to approve a digest asks them to
 * approve content, which is the thing HarborMaster can actually guarantee is on
 * this host.
 */
export function RecreateContainerAction({
  acquisition,
  onRequested,
}: {
  /** The SUCCEEDED acquisition to apply. The only thing the request carries. */
  acquisition: Acquisition;
  /** Called once a request has been accepted, so the caller can refresh. */
  onRequested?: () => void;
}) {
  const [confirming, setConfirming] = useState(false);
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [accepted, setAccepted] = useState(false);
  const dialogId = useId();

  // Only a succeeded acquisition names an image that is known to be on this
  // host. Saying so is more useful than offering a control that cannot work.
  if (acquisition.state !== "succeeded") {
    return (
      <p className="text-xs text-content-muted">
        This image has not been downloaded and verified, so there is nothing to
        recreate the container onto.
      </p>
    );
  }

  const containerName = acquisition.containerName || acquisition.containerId;
  const confirmed = typed.trim() === containerName;

  const recreate = async () => {
    setError(null);
    setBusy(true);
    try {
      // The acquisition id is the whole request. There is no container, image,
      // or option to supply, and the server revalidates every prerequisite
      // before it stops anything.
      await recreateForAcquisition(acquisition.acquisitionId);
      setAccepted(true);
      setConfirming(false);
      onRequested?.();
    } catch (caught) {
      setError(
        caught instanceof ApiError
          ? caught.message
          : "The recreation could not be requested.",
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
        Recreation requested. It runs in the background — watch the execution
        record for what happens to {containerName}.
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
          title="Stops this container and replaces it with one built from its own configuration on the downloaded image. Rollback is NOT automatic."
        >
          Recreate container
        </button>
      ) : (
        <div
          role="dialog"
          aria-modal="false"
          aria-labelledby={`${dialogId}-title`}
          className="space-y-3 rounded-xl border border-danger/40 bg-danger-soft p-4"
        >
          <h4 id={`${dialogId}-title`} className="text-sm font-semibold text-danger">
            Stop and replace {containerName}?
          </h4>

          {/* The three facts, as a list, because each one is a separate thing
              an operator must have understood. */}
          <ul className="list-disc space-y-1.5 pl-5 text-xs text-content">
            <li>
              <strong>{containerName} will be stopped and recreated.</strong> It
              will be unavailable while the replacement starts and is checked.
            </li>
            <li>
              <strong>The image is already on this host.</strong> It was
              downloaded and its digest verified by an earlier acquisition —
              nothing is fetched now.
            </li>
            <li>
              <strong>Rollback is NOT automatic.</strong> If the replacement
              fails its checks, HarborMaster stops, leaves both containers in
              place, and gives you the manual steps. It will not undo anything
              by itself.
            </li>
          </ul>

          {/* The exact content, not the name. */}
          <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
            <dt className="text-content-muted">Image</dt>
            <dd className="font-mono break-all text-content">
              {acquisition.target.reference || acquisition.target.repository}
            </dd>
            <dt className="text-content-muted">Digest</dt>
            <dd className="font-mono break-all text-content">
              {pinnedReference(acquisition.target)}
            </dd>
          </dl>

          <p className="text-xs text-content-muted">
            HarborMaster rechecks the plan, the acquisition, the container, the
            snapshot, the policy evaluation, and the local image immediately
            before it stops anything, and refuses if anything has changed. The
            original is kept until the replacement passes health, image,
            configuration, and network checks.
          </p>

          <label className="block space-y-1 text-xs text-content">
            <span>
              Type <span className="font-mono font-semibold">{containerName}</span> to
              confirm
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
              onClick={recreate}
              disabled={busy || !confirmed}
              className="rounded-md border border-danger/50 bg-surface px-3 py-1.5 text-sm font-medium text-danger disabled:opacity-50"
            >
              {busy ? "Requesting…" : `Stop and recreate ${containerName}`}
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
