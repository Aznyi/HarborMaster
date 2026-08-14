import { Link } from "react-router";

import type { AutomationDecision } from "../api/automationTypes";
import {
  AUTOMATION_REASON_DETAILS,
  AUTOMATION_REASON_LABELS,
  automationReasonIsDependency,
  automationReasonIsFailure,
} from "../api/automationTypes";
import type {
  DependencyMember,
  DependencyMemberState,
} from "../api/dependencyTypes";
import { memberStateLabel } from "../api/dependencyTypes";
import type { ContainerAttention } from "../api/inventoryTypes";

/**
 * How dependency conditions are said out loud, in one place.
 *
 * # The distinction everything here is built around
 *
 * **Waiting is not failure.** A container waiting for its dependency is the
 * system working: the dependency will finish and this one proceeds, or the
 * dependency will fail and produce a different, louder condition. Nothing is
 * asked of anybody in between.
 *
 * So `dependencyWaiting` gets no failure colour, no alert role, and no wording
 * suggesting somebody should do something. It is stated as a fact and it links
 * to the container being waited on, because "which one" is the only question an
 * operator actually has about it.
 *
 * # A rebind is never an image update
 *
 * The recreation this subsystem performs moves NO VERSION. It rebuilds a
 * container on the digest it is already running, so that it can attach to the
 * replacement of a container whose namespace it shares. Every sentence below
 * that mentions a rebind says so, because "HarborMaster recreated my container"
 * and "HarborMaster updated my container" are very different claims and only
 * one of them is true.
 */

// ------------------------------------------------------- automation rows --

/**
 * Why one container was, or was not, updated.
 *
 * Replaces a bare label with the label PLUS the dependency context, because the
 * two together answer the question and the label alone does not: "Waiting for
 * dependency" leaves an operator asking which one, and the answer is a link.
 */
export function DecisionReason({ decision }: { decision: AutomationDecision }) {
  const label = AUTOMATION_REASON_LABELS[decision.reason] ?? decision.reason;
  const detail = AUTOMATION_REASON_DETAILS[decision.reason] ?? "";
  const failure = automationReasonIsFailure(decision.reason);

  return (
    <span className="flex flex-col gap-0.5">
      <span
        className={`text-sm ${failure ? "font-medium text-danger" : "text-content-muted"}`}
        title={decision.detail || detail}
      >
        {label}
      </span>

      {automationReasonIsDependency(decision.reason) ? (
        <DependencyDecisionNote decision={decision} />
      ) : null}
    </span>
  );
}

/**
 * The sentence under a dependency-related decision.
 *
 * Names the container responsible and links to it. The link is the point: an
 * operator reading "blocked by dependency" wants to open the thing that blocked
 * it, and making them search the container list for a name they just read is
 * the difference between an explanation and a label.
 */
function DependencyDecisionNote({ decision }: { decision: AutomationDecision }) {
  const blocker = decision.blockedBy;

  // The loop is the one dependency condition that is not ABOUT another
  // container in a way a link could usefully follow -- the answer is the whole
  // graph, so it goes to the dependency page.
  if (decision.reason === "dependencyCycle") {
    return (
      <span className="text-xs text-content-muted">
        No safe order exists for these containers.{" "}
        <Link to="/dependencies" className="text-accent underline">
          See the loop
        </Link>
      </span>
    );
  }

  if (decision.reason === "dependentsNotRebindable") {
    return (
      <span className="text-xs text-content-muted">
        Containers share this one&apos;s namespace and could not be established
        as safely reattachable, so it was not replaced.{" "}
        <Link to="/dependencies" className="text-accent underline">
          See what depends on it
        </Link>
      </span>
    );
  }

  if (!blocker) {
    return (
      <span className="text-xs text-content-muted">
        {AUTOMATION_REASON_DETAILS[decision.reason]}
      </span>
    );
  }

  const verb =
    decision.reason === "dependencyWaiting"
      ? "is waiting for"
      : decision.reason === "dependencyIneligible"
        ? "depends on"
        : "was not updated because of";

  return (
    <span className="break-words text-xs text-content-muted">
      {decision.containerName} {verb}{" "}
      <Link
        to={`/containers?search=${encodeURIComponent(blocker)}`}
        className="break-all text-accent underline"
      >
        {blocker}
      </Link>
      {decision.reason === "dependencyWaiting"
        ? ". Nothing is wrong and nothing is being asked of you."
        : decision.reason === "dependencyIneligible"
          ? ", which needs an update the rules in force do not permit."
          : ", which could not be updated safely."}
    </span>
  );
}

/**
 * "Approved" is authorisation, not a promise that it is happening now.
 *
 * An approved container whose dependency is still being updated has NOT been
 * bypassed: the gate runs again when the work is submitted, and it can still
 * refuse. Rendering the approval alone would tell an operator the update is
 * under way when it is queued behind something else.
 */
export function ApprovalDependencyNote({
  decision,
}: {
  decision: AutomationDecision;
}) {
  const waiting =
    decision.dependencyState === "dependencyWaiting" ||
    decision.dependencyState === "dependencyBlocked" ||
    decision.dependencyState === "dependencyIneligible" ||
    decision.dependencyState === "dependencyMissing" ||
    decision.dependencyState === "dependencyCycle";
  if (!waiting) return null;

  const blocker = decision.blockedBy;
  const clears = decision.dependencyState === "dependencyWaiting";

  return (
    <p className="mt-1 break-words text-xs text-content-muted">
      <span className="font-medium text-content">
        Approved — waiting for dependency.
      </span>{" "}
      {clears
        ? "Approving authorises the update; it does not start it ahead of what this container depends on."
        : "Approving authorises the update. HarborMaster still refuses to proceed while this holds."}
      {blocker ? (
        <>
          {" Waiting on "}
          <Link
            to={`/containers?search=${encodeURIComponent(blocker)}`}
            className="break-all text-accent underline"
          >
            {blocker}
          </Link>
          .
        </>
      ) : null}
    </p>
  );
}

// -------------------------------------------------------------- rebinds --

/**
 * What a reattachment IS, in the state it is in.
 *
 * One function so the four states cannot be worded differently on four pages,
 * and so every one of them says the same thing about the image: it does not
 * move.
 */
export function describeRebind(
  state: DependencyMemberState,
  dependent: string,
  provider: string,
): { title: string; detail: string } {
  switch (state) {
    case "pending":
    case "planCreated":
      return {
        title: "Rebind required",
        detail:
          `${dependent} must be safely recreated with the same image so it ` +
          `can attach to ${provider}'s replacement namespace. Its version is ` +
          `not changing.`,
      };
    case "acquired":
    case "executing":
      return {
        title: "Rebind in progress",
        detail:
          `HarborMaster is recreating ${dependent} on the image digest it is ` +
          `already running and attaching it to ${provider}'s verified ` +
          `replacement. No version movement is involved.`,
      };
    case "verified":
      return {
        title: "Rebind succeeded",
        detail:
          `${dependent} is now attached to ${provider}'s verified ` +
          `replacement, running the same image it was before.`,
      };
    case "blocked":
      return {
        title: "Rebind refused",
        detail:
          `HarborMaster could not establish that ${dependent} can be safely ` +
          `recreated, so it did not try. ${dependent} may still be attached ` +
          `to a namespace that no longer exists.`,
      };
    case "failed":
      return {
        title: "Rebind failed",
        detail:
          `HarborMaster could not safely reattach ${dependent} to ` +
          `${provider}'s replacement. It does not retry a reattachment by ` +
          `itself. No image version was changed at any point.`,
      };
    case "interrupted":
      return {
        title: "Rebind interrupted",
        detail:
          `A restart found the reattachment of ${dependent} part-way. ` +
          `HarborMaster re-derives what happened from its records rather ` +
          `than assuming.`,
      };
    default:
      return {
        title: "Reattachment state unknown",
        detail:
          `HarborMaster did not establish where the reattachment of ` +
          `${dependent} has got to.`,
      };
  }
}

/** True for the states that mean somebody has to look. */
export function rebindNeedsOperator(state: DependencyMemberState): boolean {
  return state === "failed" || state === "blocked";
}

/** One reattachment, explained. */
export function RebindNote({ member }: { member: DependencyMember }) {
  const { title, detail } = describeRebind(
    member.state,
    member.dependent,
    member.provider,
  );
  const urgent = rebindNeedsOperator(member.state);

  return (
    <div
      className={`rounded-lg border p-3 text-sm ${
        urgent
          ? "border-danger/40 bg-danger-soft"
          : "border-border-subtle bg-surface"
      }`}
    >
      <p className={`font-medium ${urgent ? "text-danger" : ""}`}>{title}</p>
      <p className="mt-1 break-words text-content-muted">{detail}</p>
      <p className="mt-1 text-xs text-content-muted">
        {/* The state in its own words as well as the sentence above, so a
            reader scanning several of these can compare them at a glance. */}
        {memberStateLabel(member.state)}
      </p>
    </div>
  );
}

// ---------------------------------------------- a container's own status --

/**
 * The current dependency activity for one container, or nothing.
 *
 * Renders only when there is something to say. A container whose dependencies
 * are satisfied gets no status line: "everything is fine" repeated beside every
 * relationship is noise that makes the one line that matters harder to find.
 */
export function CurrentDependencyStatus({
  attention,
  container,
}: {
  attention: ContainerAttention | undefined;
  container: string;
}) {
  if (!attention?.dependencyKnown && !attention?.rebindFailed && !attention?.rebindPending) {
    return null;
  }

  const provider = attention.rebindProvider ?? "";
  const blocker = attention.dependencyBlockedBy ?? "";

  let title = "";
  let detail = "";
  let urgent = false;

  if (attention.rebindFailed) {
    title = "Rebind failed";
    detail =
      `HarborMaster could not safely reattach ${container}` +
      (provider ? ` to ${provider}'s replacement` : "") +
      `. It did not retry the recreation automatically, and no image version ` +
      `was changed.`;
    urgent = true;
  } else if (attention.rebindPending) {
    title = "Rebind in progress";
    detail =
      `HarborMaster is recreating ${container} on the image digest it is ` +
      `already running` +
      (provider ? ` and attaching it to ${provider}'s replacement` : "") +
      `. Its version is not changing.`;
  } else {
    switch (attention.dependencyState) {
      case "dependencyWaiting":
        title = "Waiting for dependency";
        detail = blocker
          ? `Waiting for ${blocker} to finish updating and verify successfully.`
          : `Waiting for a container this one depends on to finish updating.`;
        break;
      case "dependencyBlocked":
        title = "Blocked by dependency";
        detail = blocker
          ? `${container} was not updated because ${blocker} could not be updated safely.`
          : `A container this one depends on could not be updated safely.`;
        urgent = true;
        break;
      case "dependencyIneligible":
        title = "Dependency not permitted to update";
        detail = `A container this one depends on needs an update the rules in force do not permit.`;
        break;
      case "dependencyCycle":
        title = "No safe update order";
        detail =
          `${container} is in, or behind, a loop of dependencies. ` +
          `HarborMaster will not update it until the loop is broken.`;
        urgent = true;
        break;
      case "dependencyMissing":
        title = "Dependency unavailable";
        detail =
          `HarborMaster could not establish what ${container} depends on, so ` +
          `it will not update it. This is a refusal, not a finding that there ` +
          `is nothing to wait for.`;
        urgent = true;
        break;
      default:
        // Satisfied, or a state this build does not know. Nothing worth
        // saying: see the note on this component.
        return null;
    }
  }

  return (
    <section
      aria-label="Current dependency status"
      className={`rounded-lg border px-3 py-2 text-sm ${
        urgent ? "border-warn/40 bg-warn-soft" : "border-border-subtle bg-surface"
      }`}
    >
      <h4 className="text-xs uppercase tracking-wide text-content-muted">
        Current status
      </h4>
      {/* The headline is a word, not a colour: an operator who cannot
          distinguish the warn tone reads the same page. */}
      <p className={`mt-0.5 font-medium ${urgent ? "text-warn" : ""}`}>{title}</p>
      <p className="mt-1 break-words text-content-muted">{detail}</p>
    </section>
  );
}
