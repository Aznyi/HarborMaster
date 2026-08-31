import { Link } from "react-router";

import type {
  AttentionState,
  ContainerAttention,
  PreservedKind,
} from "../api/inventoryTypes";
import type { CheckStatus } from "../api/imageTypes";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

/**
 * How HarborMaster's container verdicts are said out loud.
 *
 * # Why the wording is here and not in the table
 *
 * These ten strings are the whole of what a Docker administrator learns from a
 * container list. Keeping them in one file means the same verdict reads the
 * same way on the list, on the container's own page, and on the dashboard --
 * and it means the distinctions the server works hard to preserve cannot be
 * flattened by a component that only needed a short label.
 *
 * The four that must never be conflated. The Updates workspace derives the
 * same four from the plan's registry status and says them the same way, so an
 * operator moving between the two surfaces is not shown two vocabularies for
 * one set of facts:
 *
 *   - "Not tracked"      -- no reference can be followed, so there will never
 *                           be anything to tell.
 *   - "Not checked"      -- HarborMaster has not looked yet.
 *   - "Up to date"       -- HarborMaster looked and found nothing to do.
 *   - "Cannot determine" -- HarborMaster looked and cannot judge what it
 *                           found.
 *
 * All four would be defensible as a grey badge saying nothing. None of them
 * means the same thing to somebody deciding whether to act.
 */
export const ATTENTION_LABELS: Record<AttentionState, string> = {
  preserved: "Kept by HarborMaster",
  unhealthy: "Unhealthy",
  dependencyFailed: "Rebind failed",
  approvalRequired: "Approval required",
  paused: "Automation paused",
  dependencyCycle: "Dependency cycle",
  dependencyUnresolved: "Dependency unavailable",
  needsReview: "Needs review",
  dependencyBlocked: "Dependency blocked",
  cannotAdvise: "Cannot determine",
  updateAvailable: "Update available",
  notTracked: "Not tracked",
  notComparable: "Not comparable",
  notChecked: "Not checked",
  upToDate: "Up to date",
};

/**
 * One sentence per verdict, shown on hover and used verbatim on the container
 * page. Written for somebody who has never read HarborMaster's documentation.
 */
export const ATTENTION_MEANINGS: Record<AttentionState, string> = {
  preserved:
    "HarborMaster stopped and renamed this container to keep it as evidence. " +
    "It is not a running workload, and nothing will remove it for you.",
  unhealthy: "The container's own healthcheck is failing.",
  dependencyFailed:
    "HarborMaster could not reattach this container to the replacement of a " +
    "container whose namespace it shares. It does not retry a reattachment " +
    "by itself, so this stands until somebody looks.",
  dependencyCycle:
    "This container is in, or behind, a loop of dependencies. No safe update " +
    "order exists, and no pass will invent one — the loop has to be broken.",
  dependencyUnresolved:
    "HarborMaster could not establish what this container depends on, so it " +
    "will not update it. This is a refusal, not a finding of no dependencies.",
  dependencyBlocked:
    "Something this container depends on could not be updated safely, or is " +
    "not permitted to be. Often clears on the next pass.",
  approvalRequired:
    "An update policy has decided on a change and is holding it until you " +
    "release it.",
  paused:
    "Automation stopped trying for this container after a failure. It will " +
    "not be updated automatically until somebody resumes it.",
  needsReview:
    "A newer image exists and the assessment asks for a person to look " +
    "before it is applied.",
  cannotAdvise:
    "A newer image may exist and HarborMaster cannot judge it. This is the " +
    "absence of a verdict, not a mild one.",
  updateAvailable:
    "A newer image exists and nothing in the assessment argues against it.",
  notTracked:
    "HarborMaster cannot attribute the running image to any tag, so it will " +
    "never find an update for this container. An empty update column here " +
    "means it cannot tell you, not that there is nothing to tell.",
  notComparable:
    "This image has no registry to compare against, so HarborMaster will " +
    "never find an update for it. Unlike “cannot determine” this " +
    "does not resolve on a later pass — the fix is to run the container " +
    "on an image from a registry.",
  notChecked:
    "HarborMaster has not assessed this container yet. That is not the same " +
    "as finding it up to date.",
  upToDate: "HarborMaster compared the running image against the tag it " +
    "follows and found nothing newer.",
};

/**
 * Tones.
 *
 * `notChecked`, `notTracked` and `cannotAdvise` are all NEUTRAL rather than
 * green. A gap in evidence is not good news, and colouring one as though it
 * were is precisely how "we never looked" comes to read as "everything is
 * fine".
 */
const ATTENTION_TONES: Record<AttentionState, BadgeTone> = {
  preserved: "neutral",
  unhealthy: "danger",
  // A container possibly detached from its provider's namespace. The only
  // dependency state that gets the danger tone -- the other three are real but
  // none of them means something is broken right now.
  dependencyFailed: "danger",
  approvalRequired: "warn",
  paused: "danger",
  dependencyCycle: "warn",
  dependencyUnresolved: "warn",
  // Not a failure. A dependency block is HarborMaster declining to proceed,
  // and frequently the next pass clears it -- rendering it in danger would put
  // an ordinary consequence beside a failed healthcheck.
  dependencyBlocked: "neutral",
  needsReview: "warn",
  cannotAdvise: "neutral",
  updateAvailable: "warn",
  notTracked: "neutral",
  notComparable: "neutral",
  notChecked: "neutral",
  upToDate: "ok",
};

/**
 * The attention block for a row, or an honest stand-in.
 *
 * This reads from the network. The server always sends the field, but a
 * malformed or older payload must degrade to the answer that claims the least
 * -- "not checked" -- rather than take the containers page down or, worse,
 * render a page of unknown containers as up to date.
 */
export function rowAttention(
  attention: ContainerAttention | undefined,
): ContainerAttention {
  return (
    attention ?? {
      state: "notChecked",
      trackingKnown: false,
      awaitingApproval: false,
      automationPaused: false,
      openViolations: 0,
      openDrift: 0,
    }
  );
}

/**
 * Whether a verdict is asking something of a person. Mirrors the server.
 *
 * The three dependency states nothing resolves by itself are here.
 * `dependencyBlocked` is NOT: it is frequently the ordinary consequence of a
 * dependency that will be updated on the next pass, and listing it as work
 * would make the list noisy enough to stop being read.
 */
export function needsOperator(state: AttentionState): boolean {
  return (
    state === "unhealthy" ||
    state === "approvalRequired" ||
    state === "paused" ||
    state === "needsReview" ||
    state === "dependencyFailed" ||
    state === "dependencyCycle" ||
    state === "dependencyUnresolved"
  );
}

/**
 * The headline badge for one container.
 *
 * An unrecognised state renders its own raw value rather than falling back to
 * anything: a verdict this build has never heard of must be visible as
 * something new, not silently painted as "up to date".
 */
export function AttentionBadge({
  state,
  /**
   * The state of the most recent registry lookup, when a comparison has
   * settled. Optional: every existing caller renders exactly what it did
   * before, and only a caller that has the fact can qualify the verdict.
   */
  checkStatus,
}: {
  state: AttentionState;
  checkStatus?: CheckStatus;
}) {
  /*
   * "Up to date" after a re-check that did not answer.
   *
   * HarborMaster preserves the earlier successful comparison rather than
   * discarding it — it is still the best knowledge available — so the verdict
   * is correct. What it must not become is a claim about RIGHT NOW: the latest
   * lookup failed, and the honest statement is that this was true when it was
   * last confirmed.
   *
   * The badge stays green because nothing is wrong with the container; only
   * the sentence changes.
   */
  const unconfirmed =
    state === "upToDate" && checkStatus !== undefined && checkStatus !== "ok";

  return (
    <StatusBadge
      tone={ATTENTION_TONES[state] ?? "neutral"}
      label={ATTENTION_LABELS[state] ?? state}
      title={
        unconfirmed
          ? "HarborMaster compared the running image against the tag it " +
            "follows and found nothing newer. The most recent check did not " +
            "answer, so this was true when it was last confirmed rather than " +
            "a statement about right now."
          : (ATTENTION_MEANINGS[state] ?? state)
      }
    />
  );
}

const PRESERVED_LABELS: Record<PreservedKind, string> = {
  original: "Previous version, kept",
  failed: "Failed update, kept as evidence",
  rolledBack: "Rolled back, kept as evidence",
  suspected: "Looks like one of ours",
};

const PRESERVED_MEANINGS: Record<PreservedKind, string> = {
  original:
    "The container an update replaced. HarborMaster stops and renames the " +
    "original rather than removing it, until the replacement has proved " +
    "itself. Seeing one after a finished update means the removal step did " +
    "not complete.",
  failed:
    "A replacement that failed its own verification. Kept deliberately: it " +
    "is the evidence explaining why the new image did not work.",
  rolledBack:
    "A replacement a rollback moved aside. It may have been perfectly " +
    "healthy — a rollback is not a verdict on it.",
  suspected:
    "The name matches the shape HarborMaster uses, but HarborMaster has no " +
    "record of creating it. It is listed normally and nothing acts on this.",
};

/** Why a preserved container exists, in one line. */
export function PreservedNote({
  kind,
  workload,
}: {
  kind: PreservedKind;
  workload?: string;
}) {
  return (
    <span className="text-xs text-content-muted">
      {PRESERVED_LABELS[kind] ?? kind}
      {workload ? (
        <>
          {" for "}
          <span className="font-medium">{workload}</span>
        </>
      ) : null}
    </span>
  );
}

export function preservedMeaning(kind: PreservedKind): string {
  return PRESERVED_MEANINGS[kind] ?? "";
}

/**
 * The small markers that sit beside the headline.
 *
 * Deliberately few. A row that carries eight badges communicates nothing, so
 * only the facts an operator would act on differently appear here, and only
 * when they are NOT already the headline -- an "Approval required" row does
 * not also need an "approval" marker.
 */
export function AttentionMarkers({
  attention,
  containerId,
}: {
  attention: ContainerAttention;
  containerId: string;
}) {
  const markers: { key: string; label: string; title: string; to: string }[] = [];

  if (attention.awaitingApproval && attention.state !== "approvalRequired") {
    markers.push({
      key: "approval",
      label: "approval waiting",
      title: "An update policy is holding a change for you.",
      to: "/automation/approvals",
    });
  }
  if (attention.automationPaused && attention.state !== "paused") {
    markers.push({
      key: "paused",
      label: "automation paused",
      title: "Automation stopped trying for this container.",
      to: "/automation/paused",
    });
  }
  if (attention.openViolations > 0) {
    markers.push({
      key: "violations",
      label: `${attention.openViolations} policy ${
        attention.openViolations === 1 ? "finding" : "findings"
      }`,
      title:
        attention.highestSeverity
          ? `The worst open finding is ${attention.highestSeverity}.`
          : "Open policy findings.",
      to: `/policy/container/${containerId}`,
    });
  }
  if (attention.openDrift > 0) {
    markers.push({
      key: "drift",
      label: `${attention.openDrift} drifted ${
        attention.openDrift === 1 ? "setting" : "settings"
      }`,
      title: "The container has moved from its baseline snapshot.",
      to: `/drift/container/${containerId}`,
    });
  }

  if (markers.length === 0) return null;

  return (
    <ul className="mt-1 flex flex-wrap gap-x-3 gap-y-1">
      {markers.map((marker) => (
        <li key={marker.key}>
          <Link
            to={marker.to}
            title={marker.title}
            className="inline-flex min-h-6 items-center text-xs text-accent hover:underline"
          >
            {marker.label}
          </Link>
        </li>
      ))}
    </ul>
  );
}

/**
 * What HarborMaster would change, in the words of the person deciding.
 *
 * The honest non-answers are spelled out rather than left as an empty cell. An
 * empty cell in an "Update" column reads as "no update", which is exactly the
 * claim HarborMaster cannot make about a container it never assessed.
 */
export function UpdateCell({ attention }: { attention: ContainerAttention }) {
  if (attention.state === "preserved") {
    return <span className="text-xs text-content-muted">not applicable</span>;
  }
  if (attention.state === "notTracked") {
    return (
      <span className="text-xs text-content-muted">
        no tag is followed, so no update can be found
      </span>
    );
  }
  if (attention.state === "notChecked") {
    return <span className="text-xs text-content-muted">not assessed yet</span>;
  }
  if (attention.state === "cannotAdvise") {
    return (
      <span className="text-xs text-content-muted">
        {attention.proposedImage
          ? `${attention.proposedImage} — HarborMaster cannot judge this change`
          : "a newer image may exist; HarborMaster cannot judge it"}
      </span>
    );
  }
  if (!attention.proposedImage) {
    return <span className="text-xs text-content-muted">nothing newer</span>;
  }
  return (
    <span className="flex flex-col gap-0.5">
      <span className="break-all font-mono text-xs">{attention.proposedImage}</span>
      {attention.updateType ? (
        <span className="text-xs text-content-muted">{attention.updateType} change</span>
      ) : null}
    </span>
  );
}
