import { useEffect, useRef, useState } from "react";
import { Link } from "react-router";

import {
  readinessGroupLabel,
  readinessHeadline,
  type AutomationReadinessRequest,
  type AutomationReadinessResponse,
} from "../api/automationReadiness";
import { previewAutomationReadiness } from "../api/client";

/**
 * "Based on current assessment: N containers currently eligible."
 *
 * # Why this is a panel and not a number
 *
 * A count on its own invites the wrong question. An operator who reads "3
 * eligible" out of forty containers needs to know what happened to the other
 * thirty-seven, and the answer is not a diagnostic dump -- it is a handful of
 * grouped reasons, each of them HarborMaster's own sentence.
 *
 * # Four states, and they are distinguishable
 *
 * Loading, unavailable, an estate with nothing in it, and an error are four
 * different things and must never collapse into "0 eligible". A failed request
 * rendered as zero would tell an operator their policy governs nothing, which
 * is the most misleading thing this component could do.
 *
 * # Why the live region is polite and debounced
 *
 * The panel re-queries as the operator edits the policy. An assertive region
 * would interrupt a screen-reader user on every keystroke; a polite one
 * announces when they pause. The debounce is what stops the announcement -- and
 * the request -- firing per character.
 */

/** How long the form must be still before a preview is requested. */
const DEBOUNCE_MS = 400;

export function ReadinessPanel({
  request,
  enabled = true,
}: {
  /**
   * The policy to measure. Rebuilt by the editor on every edit; the panel
   * re-queries when its serialised form changes rather than on identity, so a
   * re-render that changes nothing costs nothing.
   */
  request: AutomationReadinessRequest;
  /** False while the form cannot yet describe a policy. */
  enabled?: boolean;
}) {
  const [state, setState] = useState<
    | { status: "idle" }
    | { status: "loading" }
    | { status: "ready"; data: AutomationReadinessResponse }
    | { status: "error" }
  >({ status: "idle" });

  // The serialised request, so the effect depends on the VALUE rather than on
  // an object identity the editor recreates every render.
  const key = JSON.stringify(request);
  // Guards against a slow response for an old request overwriting a newer one.
  const latest = useRef(0);

  useEffect(() => {
    if (!enabled) {
      setState({ status: "idle" });
      return;
    }

    const generation = ++latest.current;
    const timer = setTimeout(() => {
      setState({ status: "loading" });
      previewAutomationReadiness(JSON.parse(key) as AutomationReadinessRequest)
        .then((data) => {
          if (latest.current !== generation) return;
          // A 2xx whose body is not a readiness answer is not an answer. Shown
          // as "could not check" rather than rendered, because a missing count
          // must never become a zero one.
          if (!data?.readiness || !Array.isArray(data.readiness.groups)) {
            setState({ status: "error" });
            return;
          }
          setState({ status: "ready", data });
        })
        .catch(() => {
          if (latest.current !== generation) return;
          // NOT zero. A failed request is not an empty estate.
          setState({ status: "error" });
        });
    }, DEBOUNCE_MS);

    return () => clearTimeout(timer);
  }, [key, enabled]);

  if (!enabled || state.status === "idle") return null;

  return (
    <section
      aria-label="Readiness"
      className="space-y-2 rounded-lg border border-border-subtle bg-surface px-3 py-3"
    >
      <h3 className="text-sm font-medium">Based on current assessment</h3>

      {/*
        One polite live region for the whole answer. Editing the policy replaces
        its contents, and a screen reader announces the new reading once the
        operator stops typing rather than on every keystroke.
      */}
      <div aria-live="polite" aria-atomic="true" className="space-y-2">
        {state.status === "loading" ? (
          <p className="text-sm text-content-muted">Checking the estate…</p>
        ) : null}

        {state.status === "error" ? (
          <p className="text-sm text-warn">
            HarborMaster could not check the estate just now, so this policy has
            not been measured. This says nothing about how many containers it
            would govern.
          </p>
        ) : null}

        {state.status === "ready" ? (
          <ReadinessAnswer data={state.data} />
        ) : null}
      </div>
    </section>
  );
}

function ReadinessAnswer({ data }: { data: AutomationReadinessResponse }) {
  const { readiness, engineEnabled } = data;

  return (
    <>
      <p className="text-sm font-medium" data-testid="readiness-headline">
        {readinessHeadline(readiness, engineEnabled)}
      </p>

      {readiness.truncated ? (
        <p className="text-sm text-warn">
          This host has more containers than one pass may consider, so these
          counts describe part of the estate rather than all of it.
        </p>
      ) : null}

      {readiness.groups.length > 0 ? (
        <>
          {/*
            Deliberately NOT "{governed} governed, the rest:". The groups are an
            explanation of what stands in the way, not a partition of `governed`
            -- a container refused before a policy is even selected (paused,
            opted out, HarborMaster itself) is explained here without being
            counted as governed by anything. Claiming the numbers add up would
            be a claim that is false.
          */}
          <p className="text-xs text-content-muted">
            {readiness.governed} governed by this policy. What is standing in the
            way:
          </p>
          <ul className="space-y-1" data-testid="readiness-groups">
            {readiness.groups.map((group) => (
              <li key={group.reason} className="flex min-h-11 items-start gap-2 text-sm">
                {/*
                  The count is text, not a coloured chip: the distinction
                  between groups must not be carried by colour alone.
                */}
                <span className="w-8 shrink-0 tabular-nums font-medium">
                  {group.count}
                </span>
                <span className="min-w-0">
                  <span className="block">{readinessGroupLabel(group)}</span>
                  <span className="block text-xs text-content-muted">
                    {group.explanation}
                  </span>
                  {/*
                    One group is actionable, and only one: a pause is the state
                    that requires a person and clears no other way. Readiness
                    itself stays READ-ONLY -- this is a link to where the resume
                    lives, never the resume.
                  */}
                  {group.reason === "automationPaused" ? (
                    <Link
                      to="/automation/paused"
                      className="mt-0.5 inline-block text-xs text-accent hover:underline"
                    >
                      {group.count === 1
                        ? "Review paused container"
                        : "Review paused containers"}
                    </Link>
                  ) : null}
                </span>
              </li>
            ))}
          </ul>
        </>
      ) : null}

      <p className="text-xs text-content-muted">
        A reading of the estate as HarborMaster currently assesses it, not a
        prediction. Every safety check runs again before any container is
        changed.
      </p>
    </>
  );
}
