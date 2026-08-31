import { useState } from "react";

import type {
  ContainerUpdateBehavior as BehaviorState,
  UpdateBehavior,
} from "../api/inventoryTypes";
import { StatusBadge } from "./StatusBadge";

/**
 * How one container should be updated (C2).
 *
 * # What this control is
 *
 * The per-container half of the simple experience: open a container, choose how
 * it should be updated, never learn what a selector is.
 *
 * # What it cannot do
 *
 * It cannot make a container MORE mutable. A preference narrows the policy that
 * governs the container and can never widen it, so the worst any choice here
 * can do is stop something from happening. That is why the confirmation below
 * is asymmetric: moving toward automatic is a real increase in what
 * HarborMaster may do on its own and is confirmed; moving away from it is a
 * restriction and is not.
 *
 * # It renders no verdict of its own
 *
 * `requested` is the stored choice and `effective` is the ENGINE'S decision,
 * derived server-side from the same evaluation the scheduler runs. This
 * component shows both and never computes the second from the first — a second
 * policy evaluator in React would eventually disagree with the real one, and
 * the disagreement would be invisible until somebody's database restarted.
 */

/** The operator-facing name for each behaviour. */
export const BEHAVIOR_LABELS: Record<UpdateBehavior, string> = {
  automatic: "Automatic",
  reviewFirst: "Review first",
  monitorOnly: "Monitor only",
};

/** One sentence describing the outcome, not the field it sets. */
export const BEHAVIOR_DESCRIPTIONS: Record<UpdateBehavior, string> = {
  automatic:
    "HarborMaster may stop and recreate this container on its own when an eligible update is available.",
  reviewFirst:
    "HarborMaster prepares the update and waits for somebody to release it.",
  monitorOnly:
    "HarborMaster keeps checking for updates and reports them, and never changes this container by itself.",
};

/** How permissive each behaviour is. Moving UP this order is confirmed. */
const PERMISSIVENESS: Record<UpdateBehavior, number> = {
  monitorOnly: 0,
  reviewFirst: 1,
  automatic: 2,
};

export function ContainerUpdateBehaviorPanel({
  state,
  mayManage,
  busy,
  error,
  onChoose,
  onClear,
  /** True for HarborMaster's own container, which it will never update. */
  isSelf = false,
}: {
  state: BehaviorState | null;
  mayManage: boolean;
  busy: boolean;
  error: string | null;
  onChoose: (behavior: UpdateBehavior) => void;
  onClear: () => void;
  isSelf?: boolean;
}) {
  const [choosing, setChoosing] = useState(false);
  const [confirming, setConfirming] = useState<UpdateBehavior | null>(null);

  const requested = state?.requested?.behavior ?? null;
  const effective = state?.effective;

  // HarborMaster's own container is refused by the engine at four independent
  // layers. Offering a working selector for it would be a control that cannot
  // do what it says.
  if (isSelf) {
    return (
      <Panel>
        <p className="text-sm">Excluded</p>
        <p className="mt-1 text-xs text-content-muted">
          HarborMaster does not update its own container.
        </p>
      </Panel>
    );
  }

  return (
    <Panel>
      <div className="flex flex-wrap items-center gap-2">
        <p className="text-sm font-medium">
          {requested ? BEHAVIOR_LABELS[requested] : "Inherited"}
        </p>
        {requested ? null : (
          <StatusBadge tone="neutral" label="no override" />
        )}
      </div>
      <p className="mt-1 text-xs text-content-muted">
        {requested
          ? BEHAVIOR_DESCRIPTIONS[requested]
          : "This container follows whichever update policy governs it."}
      </p>

      {/*
        The EFFECTIVE behaviour, whenever it differs from what was asked for.
        This is the sentence that stops the page lying: a preference may only
        narrow, so a container an update policy holds for review stays held
        however this control is set.
      */}
      {effective?.known && effective.detail ? (
        <p className="mt-3 rounded-lg border border-border-subtle bg-surface p-3 text-xs text-content-muted">
          <span className="font-medium text-content">What happens now: </span>
          {effective.detail}
          {effective.policyName ? (
            <>
              {" "}
              <span className="text-content">
                (decided by the policy &ldquo;{effective.policyName}&rdquo;)
              </span>
            </>
          ) : null}
        </p>
      ) : null}

      {effective && !effective.known ? (
        <p className="mt-3 text-xs text-content-muted">
          HarborMaster could not work out what would happen to this container
          right now, so nothing is claimed about it.
        </p>
      ) : null}

      {error ? (
        <p role="alert" className="mt-3 text-sm text-danger">
          {error}
        </p>
      ) : null}

      {mayManage ? (
        <div className="mt-4 flex flex-wrap gap-2">
          <button
            type="button"
            onClick={() => setChoosing((open) => !open)}
            disabled={busy}
            aria-expanded={choosing}
            className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle px-3 py-1.5 text-sm font-medium disabled:opacity-60"
          >
            Change
          </button>
          {requested ? (
            <button
              type="button"
              onClick={onClear}
              disabled={busy}
              className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle px-3 py-1.5 text-sm font-medium disabled:opacity-60"
            >
              Use the policy default
            </button>
          ) : null}
        </div>
      ) : (
        <p className="mt-3 text-xs text-content-muted">
          Changing this needs the automation management permission.
        </p>
      )}

      {choosing && mayManage ? (
        <fieldset className="mt-4 rounded-lg border border-border-subtle p-3">
          <legend className="px-1 text-xs font-semibold uppercase tracking-wide text-content-muted">
            Update behaviour
          </legend>
          <div className="flex flex-col gap-2">
            {(Object.keys(BEHAVIOR_LABELS) as UpdateBehavior[]).map((option) => (
              <label key={option} className="flex min-h-11 items-start gap-2 text-sm">
                <input
                  type="radio"
                  name="container-update-behavior"
                  value={option}
                  checked={requested === option}
                  onChange={() => {
                    setChoosing(false);
                    // Confirm only when the choice INCREASES what HarborMaster
                    // may do without asking.
                    const increases =
                      PERMISSIVENESS[option] >
                      (requested ? PERMISSIVENESS[requested] : PERMISSIVENESS.monitorOnly);
                    if (increases) {
                      setConfirming(option);
                    } else {
                      onChoose(option);
                    }
                  }}
                  className="mt-1 size-4 shrink-0"
                />
                <span>
                  <span className="block font-medium">{BEHAVIOR_LABELS[option]}</span>
                  <span className="block text-xs text-content-muted">
                    {BEHAVIOR_DESCRIPTIONS[option]}
                  </span>
                </span>
              </label>
            ))}
          </div>
        </fieldset>
      ) : null}

      {confirming ? (
        <div
          role="dialog"
          aria-modal="false"
          aria-labelledby="behavior-confirm"
          className="mt-4 rounded-lg border border-border-subtle bg-surface p-3"
        >
          <h4 id="behavior-confirm" className="text-sm font-semibold">
            Let HarborMaster update this container automatically?
          </h4>
          <p className="mt-2 text-sm">
            {confirming === "automatic"
              ? "HarborMaster may stop and recreate this container without asking, when an eligible update is available. It verifies the replacement and puts the original back if the checks fail."
              : "HarborMaster will prepare updates for this container and hold them until somebody releases each one."}
          </p>
          <p className="mt-2 text-xs text-content-muted">
            An update policy, a container label, or a pause can still hold it
            back. Nothing changes on the host until the next automation pass.
          </p>
          <div className="mt-3 flex flex-wrap gap-2">
            <button
              type="button"
              onClick={() => {
                const chosen = confirming;
                setConfirming(null);
                onChoose(chosen);
              }}
              disabled={busy}
              className="inline-flex min-h-11 items-center rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-surface disabled:opacity-60"
            >
              {BEHAVIOR_LABELS[confirming]}
            </button>
            <button
              type="button"
              onClick={() => setConfirming(null)}
              className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle px-3 py-1.5 text-sm font-medium"
            >
              Cancel
            </button>
          </div>
        </div>
      ) : null}
    </Panel>
  );
}

function Panel({ children }: { children: React.ReactNode }) {
  return (
    <section className="flex min-w-0 flex-col rounded-xl border border-border-subtle bg-surface-raised p-4">
      <h3 className="text-xs uppercase tracking-wide text-content-muted">
        Automatic updates
      </h3>
      <div className="mt-2 min-w-0 flex-1">{children}</div>
    </section>
  );
}
