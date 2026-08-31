import { useState } from "react";
import { Link } from "react-router";

import type { SimpleUpdatesState } from "../api/automationTypes";
import {
  SIMPLE_UPDATES_HEADLINE,
  engineOffExplanation,
  overrideNotice,
  simpleUpdatesStatus,
  simpleUpdatesWill,
  simpleUpdatesWillNot,
} from "../api/simpleUpdates";
import { StatusBadge } from "./StatusBadge";

/**
 * The automatic-updates switch (C1).
 *
 * # What this is
 *
 * The one control a homelab operator needs: "keep my containers updated". It
 * stands for a single ordinary update policy, written through the same service
 * and the same validation a hand-authored one goes through. There is no second
 * engine behind it and no second authorisation path.
 *
 * # Why it explains itself at such length
 *
 * Because it is a switch that lets HarborMaster stop and replace running
 * containers without being asked. An operator turning that on is entitled to
 * read exactly what it will and will not do BEFORE it changes anything, and to
 * find those sentences again afterwards. The lists come from `api/simpleUpdates`
 * so they are derived, tested, and cannot drift in a render.
 *
 * # It renders no verdict of its own
 *
 * The state, the effective policy and the warnings are all read off the server.
 * This component chooses which sentence to show; it never decides whether a
 * container may be updated.
 */
export function SimpleUpdatesPanel({
  state,
  mayManage,
  busy,
  error,
  onEnable,
  onDisable,
}: {
  state: SimpleUpdatesState;
  /** `automation:manage`. A viewer sees the state and no controls. */
  mayManage: boolean;
  busy: boolean;
  error: string | null;
  onEnable: () => void;
  onDisable: () => void;
}) {
  const [confirming, setConfirming] = useState<"on" | "off" | null>(null);
  const status = simpleUpdatesStatus(state);
  const notice = overrideNotice(state);

  return (
    <section
      aria-labelledby="simple-updates-heading"
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 id="simple-updates-heading" className="font-semibold">
            {SIMPLE_UPDATES_HEADLINE[status]}
          </h2>
          <p className="mt-1 max-w-prose text-sm text-content-muted">
            {status === "on"
              ? "HarborMaster keeps eligible containers updated on its own, using the same checks it applies when you update one by hand."
              : "One setting. HarborMaster checks eligible containers for updates and applies the safe ones for you."}
          </p>
        </div>
        <StatusBadge
          tone={status === "on" ? "ok" : "neutral"}
          label={status === "on" ? "On" : status === "engineOff" ? "Unavailable" : "Off"}
        />
      </div>

      {/*
        The engine being off is a DEPLOYMENT fact, not a setting. Rendering a
        toggle here would be a control that cannot work, so the panel says what
        to change and where instead.
      */}
      {status === "engineOff" ? (
        <p className="mt-4 max-w-prose rounded-lg border border-border-subtle bg-surface p-3 text-sm text-content-muted">
          {engineOffExplanation(state)}
        </p>
      ) : null}

      {notice ? (
        <p className="mt-4 max-w-prose rounded-lg border border-border-subtle bg-surface p-3 text-sm text-content-muted">
          {notice}
        </p>
      ) : null}

      {error ? (
        <p role="alert" className="mt-4 text-sm text-danger">
          {error}
        </p>
      ) : null}

      {/* The effective behaviour, always readable -- not only at the moment of
          turning it on. An operator who wants to check what they agreed to
          should not have to toggle something to find out. */}
      {status === "on" ? (
        <BehaviourLists state={state} />
      ) : null}

      {status !== "engineOff" && mayManage ? (
        <div className="mt-5 flex flex-wrap gap-3">
          {status === "on" ? (
            <button
              type="button"
              onClick={() => setConfirming("off")}
              disabled={busy}
              className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle px-4 py-2 text-sm font-medium disabled:opacity-60"
            >
              Turn off automatic updates
            </button>
          ) : (
            <button
              type="button"
              onClick={() => setConfirming("on")}
              disabled={busy}
              className="inline-flex min-h-11 items-center rounded-lg bg-accent px-4 py-2 text-sm font-medium text-surface disabled:opacity-60"
            >
              Turn on automatic updates
            </button>
          )}
          <Link
            to="/update-policies"
            className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle px-4 py-2 text-sm font-medium"
          >
            Manage update policies
          </Link>
        </div>
      ) : null}

      {status !== "engineOff" && !mayManage ? (
        <p className="mt-4 text-xs text-content-muted">
          Changing this needs the automation management permission.
        </p>
      ) : null}

      {confirming === "on" ? (
        <ConfirmTurnOn
          state={state}
          busy={busy}
          onCancel={() => setConfirming(null)}
          onConfirm={() => {
            setConfirming(null);
            onEnable();
          }}
        />
      ) : null}

      {confirming === "off" ? (
        <ConfirmTurnOff
          busy={busy}
          onCancel={() => setConfirming(null)}
          onConfirm={() => {
            setConfirming(null);
            onDisable();
          }}
        />
      ) : null}
    </section>
  );
}

/** What HarborMaster will and will not do, side by side. */
function BehaviourLists({ state }: { state: SimpleUpdatesState }) {
  return (
    <div className="mt-4 grid gap-4 sm:grid-cols-2">
      <div>
        <h3 className="text-xs font-semibold uppercase tracking-wide text-content-muted">
          HarborMaster will
        </h3>
        <ul className="mt-2 space-y-1 text-sm">
          {simpleUpdatesWill(state).map((line) => (
            <li key={line} className="flex gap-2">
              <span aria-hidden="true" className="text-ok">
                &#10003;
              </span>
              <span>{line}</span>
            </li>
          ))}
        </ul>
      </div>
      <div>
        <h3 className="text-xs font-semibold uppercase tracking-wide text-content-muted">
          HarborMaster will not
        </h3>
        <ul className="mt-2 space-y-1 text-sm">
          {simpleUpdatesWillNot().map((line) => (
            <li key={line} className="flex gap-2">
              <span aria-hidden="true" className="text-content-muted">
                &minus;
              </span>
              <span>{line}</span>
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}

/**
 * The confirmation for turning it on.
 *
 * Says what will happen BEFORE anything changes, and does not bury the part
 * that matters: HarborMaster will stop and replace running containers without
 * asking again. The server's own warnings about the compiled policy are shown
 * verbatim, because they are generated by the same code that warns about a
 * hand-written policy and are the most honest description available.
 */
function ConfirmTurnOn({
  state,
  busy,
  onCancel,
  onConfirm,
}: {
  state: SimpleUpdatesState;
  busy: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  return (
    <div
      role="dialog"
      aria-modal="false"
      aria-labelledby="simple-updates-confirm-on"
      className="mt-5 rounded-lg border border-border-subtle bg-surface p-4"
    >
      <h3 id="simple-updates-confirm-on" className="font-semibold">
        Turn on automatic updates?
      </h3>
      <p className="mt-2 max-w-prose text-sm">
        HarborMaster will stop and recreate eligible containers on its own, without
        asking again each time. It will not do anything the moment you confirm:
        the next scheduled pass is what acts.
      </p>

      <BehaviourLists state={state} />

      {state.warnings && state.warnings.length > 0 ? (
        <>
          <h4 className="mt-4 text-xs font-semibold uppercase tracking-wide text-content-muted">
            Worth knowing
          </h4>
          <ul className="mt-2 space-y-1 text-sm text-content-muted">
            {state.warnings.map((warning) => (
              <li key={warning}>{warning}</li>
            ))}
          </ul>
        </>
      ) : null}

      <div className="mt-4 flex flex-wrap gap-3">
        <button
          type="button"
          onClick={onConfirm}
          disabled={busy}
          className="inline-flex min-h-11 items-center rounded-lg bg-accent px-4 py-2 text-sm font-medium text-surface disabled:opacity-60"
        >
          Turn on automatic updates
        </button>
        <button
          type="button"
          onClick={onCancel}
          className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle px-4 py-2 text-sm font-medium"
        >
          Cancel
        </button>
      </div>
    </div>
  );
}

/**
 * The confirmation for turning it off.
 *
 * States what is NOT affected, because that is the thing an operator is
 * actually uncertain about: whether turning the switch off will cost them the
 * policies, history or pauses they have built up. It will not.
 */
function ConfirmTurnOff({
  busy,
  onCancel,
  onConfirm,
}: {
  busy: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  return (
    <div
      role="dialog"
      aria-modal="false"
      aria-labelledby="simple-updates-confirm-off"
      className="mt-5 rounded-lg border border-border-subtle bg-surface p-4"
    >
      <h3 id="simple-updates-confirm-off" className="font-semibold">
        Turn off automatic updates?
      </h3>
      <p className="mt-2 max-w-prose text-sm">
        HarborMaster stops updating containers by itself. Nothing on the host
        changes, and nothing already done is undone.
      </p>
      <ul className="mt-3 space-y-1 text-sm text-content-muted">
        <li>Update policies you wrote are kept, exactly as they are.</li>
        <li>Update history, approvals and paused containers are kept.</li>
        <li>No container is stopped, started or recreated by this.</li>
        <li>You can turn it back on at any time.</li>
      </ul>
      <div className="mt-4 flex flex-wrap gap-3">
        <button
          type="button"
          onClick={onConfirm}
          disabled={busy}
          className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle px-4 py-2 text-sm font-medium disabled:opacity-60"
        >
          Turn off automatic updates
        </button>
        <button
          type="button"
          onClick={onCancel}
          className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle px-4 py-2 text-sm font-medium"
        >
          Cancel
        </button>
      </div>
    </div>
  );
}
