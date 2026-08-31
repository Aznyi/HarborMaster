import { useId, useState } from "react";

import type { Acquisition } from "../api/acquisitionTypes";
import { pinnedReference } from "../api/acquisitionTypes";
import { ApiError } from "../api/client";
import { useContainerDependencies } from "../hooks/useDependencies";
import { recreateForAcquisition } from "../hooks/useExecutions";
import {
  ProviderUpdateRefusal,
  ProviderUpdateWarning,
} from "./ProviderUpdateWarning";

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
 * # Why the button can be called two things
 *
 * The OPERATION has one name and one meaning: HarborMaster stops the container
 * and recreates it from its own recorded configuration on the acquired image.
 * That is what the confirmation says, in those words, every time.
 *
 * The BUTTON is named for what the person pressing it came to do. In the
 * Updates and Activity workspaces they came to apply an update, and calling the
 * control "Recreate container" there asked them to translate. On the
 * acquisition record -- an advanced page about the download itself -- the
 * technical name is the accurate one and stays.
 *
 * Nothing else varies. Same component, same permission, same two-step typed
 * confirmation, same `recreateForAcquisition` call, same server preflight.
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
  label = "Recreate container",
}: {
  /** The SUCCEEDED acquisition to apply. The only thing the request carries. */
  acquisition: Acquisition;
  /** Called once a request has been accepted, so the caller can refresh. */
  onRequested?: () => void;
  /**
   * What the button says. Presentation only: it changes no behaviour, no
   * permission, and nothing about the confirmation the operator must read.
   */
  label?: string;
}) {
  /*
   * What depends on this container, read BEFORE the operator confirms.
   *
   * The one fact Docker will not tell them: a container bound to this one's
   * network namespace is bound to its current IDENTITY, and replacing this
   * container silently detaches it. HarborMaster reattaches it, but an operator
   * pressing a button that says "stop and recreate gluetun" deserves to know
   * three other containers are about to be recreated too.
   *
   * The read is scoped to this control and its failure is reported rather than
   * swallowed: "HarborMaster could not establish what depends on this" is a
   * different answer from "nothing does", and the preflight will refuse on the
   * same condition anyway.
   *
   * # Why this is currentContainerId and not containerId
   *
   * `acquisition.containerId` is the container the acquisition was requested
   * for, and it is history: an update RECREATES the container, so the moment
   * one has been applied that id names something no longer on the host. Asking
   * about it produced a 404 on every Activity and Updates row for a container
   * that was running perfectly well under a new id.
   *
   * `currentContainerId` is resolved server-side from the container NAME --
   * HarborMaster's stable identity across a recreation -- in the same query
   * that reads the acquisition, so this costs no extra request and cannot
   * become a per-row lookup.
   *
   * Absent means no container of that name is present. The hook skips the
   * request entirely for an undefined id, so the honest outcome is NO REQUEST
   * rather than one that 404s. Falling back to `containerId` here would
   * reintroduce exactly the defect.
   */
  const dependencies = useContainerDependencies(acquisition.currentContainerId);

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

  /*
   * No container of this name is on the host.
   *
   * Said plainly rather than left to the confirmation dialog. The recreation
   * preflight re-resolves the container itself and would refuse, but an
   * operator reading a history row deserves to know why the control is not
   * offered -- and the alternative renders a button that cannot work beside a
   * dependency panel that silently shows nothing, which reads as "nothing
   * depends on this" when the truth is "there is nothing to ask about".
   */
  if (!acquisition.currentContainerId) {
    return (
      <p className="text-xs text-content-muted">
        No container named{" "}
        <span className="font-mono">{acquisition.containerName}</span> is on
        this host, so there is nothing to recreate. This record is kept as
        evidence of what was acquired.
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
      {/* The refusal, in HarborMaster's own words. A preflight refusal names
          WHICH check said no, and that sentence is the only part of the answer
          that tells an operator what to do differently. */}
      {error && <ProviderUpdateRefusal message={error} />}

      {!confirming ? (
        <button
          type="button"
          onClick={() => setConfirming(true)}
          className="rounded-md border border-warn/50 bg-surface-raised px-3 py-1.5 text-sm font-medium text-content"
          // Says what it does before the dialog opens, so the answer is
          // available to someone who only hovers.
          title="Stops this container and replaces it with one built from its own configuration on the downloaded image. Rollback is NOT automatic."
        >
          {label}
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

          {/* What else this touches. Above the digest, because "three other
              containers are being recreated" changes the decision and the
              digest only confirms it. A provider with no hard dependents
              renders nothing here and keeps the flow it always had. */}
          <ProviderUpdateWarning
            container={containerName}
            dependencies={dependencies.data ?? undefined}
            unavailable={Boolean(dependencies.error)}
          />

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
