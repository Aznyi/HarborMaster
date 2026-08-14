import { useEffect, useId, useRef, useState } from "react";

import type { WorkloadDependency } from "../api/dependencyTypes";
import { describeOrdering } from "../api/dependencyTypes";
import { ContainerPicker } from "./ContainerPicker";

/**
 * Recording an application ordering.
 *
 * # What an operator is actually asserting
 *
 * "My application needs postgres up before api." That is a fact about the
 * software, invisible to Docker, and it is the only kind of relationship an
 * operator can create — the namespace ones are read off the daemon and cannot
 * be typed in.
 *
 * So the form says what it will do in a sentence, before the operator commits,
 * and says explicitly what it will NOT do. Every one of those disclaimers
 * corresponds to something a reasonable person might otherwise assume: that
 * this changes networking, that it will drag postgres into an update, that it
 * overrides the update policy. None of them is true, and finding out afterwards
 * would be a bad way to learn.
 */
export function DependencyEditor({
  onCreate,
  busy = false,
  error,
}: {
  onCreate: (dependent: string, dependency: string) => void;
  busy?: boolean;
  /** A refusal from the server, already in HarborMaster's own words. */
  error?: string;
}) {
  const [dependent, setDependent] = useState<string[]>([]);
  const [dependency, setDependency] = useState<string[]>([]);
  const dependentId = useId();
  const dependencyId = useId();
  const previewId = useId();

  // The picker is multi-select by design; an ordering has exactly one container
  // at each end, so the last choice wins.
  const dependentName = dependent.at(-1) ?? "";
  const dependencyName = dependency.at(-1) ?? "";

  const identical = dependentName !== "" && dependentName === dependencyName;
  const complete = dependentName !== "" && dependencyName !== "" && !identical;

  return (
    <form
      className="space-y-5"
      onSubmit={(event) => {
        event.preventDefault();
        if (complete && !busy) onCreate(dependentName, dependencyName);
      }}
    >
      {/*
        min-w-0 defeats the UA's `min-width: min-content` on a fieldset.
        Without it the fieldset refuses to be narrower than its widest
        unwrappable child -- and a selected-container chip uses white-space:
        nowrap to truncate, so a long name made the whole page 353px wider than
        a 390px viewport. Real Chromium found that; jsdom cannot, having no
        layout at all.
      */}
      <fieldset className="min-w-0 space-y-2">
        <legend className="text-sm font-medium" id={dependentId}>
          Dependent container
        </legend>
        <p className="text-sm text-content-muted">
          The container that must wait.
        </p>
        <ContainerPicker
          selected={dependentName ? [dependentName] : []}
          onChange={(names) => setDependent(names)}
          describedBy={dependentId}
        />
      </fieldset>

      {/*
        min-w-0 defeats the UA's `min-width: min-content` on a fieldset.
        Without it the fieldset refuses to be narrower than its widest
        unwrappable child -- and a selected-container chip uses white-space:
        nowrap to truncate, so a long name made the whole page 353px wider than
        a 390px viewport. Real Chromium found that; jsdom cannot, having no
        layout at all.
      */}
      <fieldset className="min-w-0 space-y-2">
        <legend className="text-sm font-medium" id={dependencyId}>
          Depends on
        </legend>
        <p className="text-sm text-content-muted">
          The container that must be updated and verified first.
        </p>
        <ContainerPicker
          selected={dependencyName ? [dependencyName] : []}
          onChange={(names) => setDependency(names)}
          describedBy={dependencyId}
        />
      </fieldset>

      {identical ? (
        <p role="alert" className="text-sm text-danger">
          A container cannot depend on itself. Choose two different containers.
        </p>
      ) : null}

      {/* The preview. Rendered as soon as both ends are chosen, because the
          point of it is to be read BEFORE the button is pressed. */}
      <section
        aria-label="What this will do"
        id={previewId}
        className="rounded-lg border border-border-subtle bg-surface p-4 text-sm"
      >
        <h3 className="font-medium">What this will do</h3>
        {complete ? (
          <>
            <p className="mt-2">{describeOrdering(dependentName, dependencyName)}</p>
            <p className="mt-3 text-content-muted">
              This is application ordering only. It does not change networking,
              does not create a Docker relationship, and does not make either
              container eligible for updates.
            </p>
            <p className="mt-2 text-content-muted">
              If an update policy excludes {dependencyName}, HarborMaster will
              not update it — {dependentName} will wait instead.
            </p>
          </>
        ) : (
          <p className="mt-2 text-content-muted">
            Choose both containers to see what this ordering will do.
          </p>
        )}
      </section>

      {error ? (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      ) : null}

      <button
        type="submit"
        disabled={!complete || busy}
        className="min-h-11 rounded-lg bg-accent px-4 py-2 text-sm font-medium text-on-accent disabled:opacity-50"
        aria-describedby={previewId}
      >
        {busy ? "Saving…" : "Record ordering"}
      </button>
    </form>
  );
}

/**
 * Removing an ordering.
 *
 * # Why this confirms
 *
 * Deleting is not the inverse of a harmless action. Creating an ordering can
 * only make HarborMaster wait; removing one takes a constraint away and lets an
 * update proceed that was being held back. The dialog therefore states the
 * CONSEQUENCE rather than asking "are you sure" — and states what it will not
 * do, since "remove dependency" could reasonably be read as touching the
 * containers themselves.
 */
export function DependencyDeleteConfirm({
  edge,
  onCancel,
  onConfirm,
  busy = false,
}: {
  edge: WorkloadDependency;
  onCancel: () => void;
  onConfirm: () => void;
  busy?: boolean;
}) {
  const titleId = useId();
  const bodyId = useId();
  const cancelRef = useRef<HTMLButtonElement>(null);

  /*
   * Focus moves into the dialog when it opens, and lands on CANCEL.
   *
   * Without this, an operator who opened the dialog from the keyboard is left
   * with focus on a Remove button that is now behind a modal, and tabbing walks
   * the page underneath rather than the choice in front of them.
   *
   * Cancel rather than Confirm because it is the reversible one: the first
   * thing a keyboard reaches should never be the destructive option.
   */
  useEffect(() => {
    cancelRef.current?.focus();
  }, []);

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby={titleId}
      aria-describedby={bodyId}
      // Escape cancels, which is what every dialog convention promises and what
      // somebody who opened this by accident will try first.
      onKeyDown={(event) => {
        if (event.key === "Escape") {
          event.stopPropagation();
          onCancel();
        }
      }}
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <h3 id={titleId} className="text-base font-semibold">
        Remove dependency?
      </h3>
      <div id={bodyId} className="mt-2 space-y-2 text-sm">
        <p className="break-words">
          {edge.dependent} will no longer wait for {edge.dependency} before
          updating.
        </p>
        <p className="text-content-muted">
          This does not modify either container.
        </p>
      </div>
      <div className="mt-4 flex flex-wrap gap-2">
        <button
          ref={cancelRef}
          type="button"
          onClick={onCancel}
          className="min-h-11 rounded-lg border border-border-subtle px-4 py-2 text-sm"
        >
          Cancel
        </button>
        <button
          type="button"
          onClick={onConfirm}
          disabled={busy}
          className="min-h-11 rounded-lg bg-danger px-4 py-2 text-sm font-medium text-on-danger disabled:opacity-50"
        >
          {busy ? "Removing…" : "Remove dependency"}
        </button>
      </div>
    </div>
  );
}
