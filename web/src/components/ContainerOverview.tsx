import { formatMoment } from "../api/presentation";
import { Link } from "react-router";

import type {
  AttentionState,
  ContainerAttention,
  ContainerDetail,
  ImageRef,
} from "../api/inventoryTypes";
import { UPDATE_TYPE_LABELS } from "../api/imageTypes";
import { formatImageReference } from "../api/presentation";
import { ATTENTION_LABELS, ATTENTION_MEANINGS } from "./AttentionBadges";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

/**
 * One container, answered from one request.
 *
 * # Why this costs nothing extra
 *
 * `ContainerDetail.attention` is the projection HarborMaster already puts on
 * both the list row and the detail payload, and it carries the update verdict,
 * the automation state, the open-drift count and the last update and rollback
 * outcomes. Every question this section answers was therefore already on screen
 * as a record id somewhere -- what was missing was somebody assembling it.
 *
 * So the overview adds ZERO requests. Anything needing a second read stays on
 * the tab that already fetches it, lazily, exactly as before.
 *
 * # What it does not do
 *
 * It reaches no verdicts. The recommendation is the planner's, the automation
 * state is the engine's, and the drift count is the drift subsystem's. Where
 * acting means a workflow, it links to the workspace that owns it rather than
 * growing a second copy of the control.
 */
export function ContainerOverview({ detail }: { detail: ContainerDetail }) {
  const attention = detail.attention;
  const id = detail.overview.id;

  return (
    <div className="flex flex-col gap-4" data-testid="container-overview">
      <div className="grid gap-4 lg:grid-cols-2 xl:grid-cols-3">
        <Panel title="Image">
          {/*
            The SUMMARY form. A container HarborMaster has updated runs an
            immutable digest, so this panel was 71 characters of hex in a card
            three of which fit across a desktop -- and the one part an operator
            reads, the repository and tag, was the part pushed off the line.

            Only the digest is abbreviated, the whole value is on the element's
            title, and the Details tab's Image section carries it complete and
            unabbreviated a click away. Nothing here is the only copy.
          */}
          <ImageReference image={detail.overview.image} />
          {attention?.tracking ? (
            <p className="mt-1 text-xs text-content-muted">
              Follows <span className="font-mono">{attention.tracking}</span>
            </p>
          ) : null}
        </Panel>

        <UpdatePanel attention={attention} />

        <AutomationPanel attention={attention} />

        <ConfigurationPanel attention={attention} containerId={id} />

        <RecentPanel attention={attention} />

        <OrderPanel attention={attention} />
      </div>
    </div>
  );
}

function Panel({
  title,
  children,
  action,
}: {
  title: string;
  children: React.ReactNode;
  action?: React.ReactNode;
}) {
  return (
    <section className="flex min-w-0 flex-col rounded-xl border border-border-subtle bg-surface-raised p-4">
      <h3 className="text-xs uppercase tracking-wide text-content-muted">{title}</h3>
      <div className="mt-2 min-w-0 flex-1">{children}</div>
      {action ? <div className="mt-3">{action}</div> : null}
    </section>
  );
}

/**
 * One image reference, compact, with the complete value one hover away.
 *
 * Uses the shared `formatImageReference` rather than a second rule: Batch A
 * settled that only the digest hex is shortened, that a tag is never shortened,
 * that a reference carrying both keeps both, and that anything it cannot parse
 * is passed through whole.
 */
function ImageReference({ image }: { image?: ImageRef }) {
  if (!image) {
    return <p className="break-all font-mono text-sm">&mdash;</p>;
  }

  const reference = formatImageReference(image);
  return (
    <p
      className="break-all font-mono text-sm"
      title={reference.abbreviated ? reference.full : undefined}
    >
      {reference.display}
    </p>
  );
}

/**
 * The four verdicts that mean there is nothing here to act on.
 *
 * They are NOT interchangeable, and this panel used to render all of them --
 * plus the case where no assessment exists at all -- as "Up to date". That is
 * the exact confusion `internal/domain/attention.go` forbids:
 *
 *	Absent evidence produces AttentionNotChecked, never AttentionUpToDate.
 *	[...] one says HarborMaster looked and found nothing to do, the other
 *	says HarborMaster has not looked.
 *
 * `ContainerAttention.updateType` is documented as "absent when no assessment
 * exists, never defaulted to none", so treating an absent one as "none" turned
 * a container HarborMaster had never assessed into a reassurance.
 */
const NO_UPDATE_STATES = new Set<AttentionState>([
  "upToDate",
  "notChecked",
  "notTracked",
  "cannotAdvise",
]);

/**
 * The update verdict, in the same words Phase 2 uses.
 *
 * Deliberately not a second recommendation engine: the recommendation is read
 * off the projection, and acting on it happens in the Updates workspace. WHICH
 * kind of "nothing to do" applies is likewise read off `state` -- the verdict
 * the server already computed -- rather than re-derived here from the update
 * type. Two surfaces deriving the same four answers separately is how they come
 * to disagree, and the Updates workspace derives them from the plan's registry
 * status and says them in these same words.
 */
function UpdatePanel({ attention }: { attention?: ContainerAttention }) {
  const updateType = attention?.updateType;

  // `unknown` belongs here too: it means a newer image MAY exist and its size
  // could not be determined, which is the absence of a verdict rather than an
  // available update. Rendering it as "Update available" would show one
  // container two vocabularies across two pages.
  if (!attention || !updateType || updateType === "none" || updateType === "unknown") {
    // Degrade to the answer that claims the least, exactly as `rowAttention`
    // does for a payload it does not recognise.
    const verdict: AttentionState =
      attention && NO_UPDATE_STATES.has(attention.state) ? attention.state : "notChecked";
    return (
      <Panel title="Update">
        <p className="text-sm">{ATTENTION_LABELS[verdict]}</p>
        <p className="mt-1 text-xs text-content-muted">{ATTENTION_MEANINGS[verdict]}</p>
      </Panel>
    );
  }

  const review = attention.recommendation === "manualReview";
  const tone: BadgeTone =
    attention.recommendation === "proceed"
      ? "ok"
      : review
        ? "warn"
        : attention.recommendation === "notRecommended"
          ? "danger"
          : "neutral";

  return (
    <Panel
      title="Update"
      action={
        <Link
          to={review ? "/updates?tab=review" : "/updates"}
          className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
        >
          {review ? "Review update" : "View in Updates"}
        </Link>
      }
    >
      <div className="flex flex-wrap items-center gap-2">
        <StatusBadge tone={tone} label={review ? "Review required" : "Update available"} />
      </div>
      {attention.proposedImage ? (
        <p className="mt-2 break-all font-mono text-xs text-content-muted">
          → {attention.proposedImage}
        </p>
      ) : null}
      <p className="mt-1 text-xs text-content-muted">
        {UPDATE_TYPE_LABELS[updateType]}
      </p>
    </Panel>
  );
}

/** Why this container is or is not updated automatically. */
function AutomationPanel({ attention }: { attention?: ContainerAttention }) {
  const state = automationState(attention);

  return (
    <Panel
      title="Automation"
      action={
        <Link
          to="/automation"
          className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
        >
          Manage automation
        </Link>
      }
    >
      <StatusBadge tone={state.tone} label={state.label} />
      <p className="mt-2 text-xs text-content-muted">{state.detail}</p>
    </Panel>
  );
}

function automationState(attention?: ContainerAttention): {
  label: string;
  tone: BadgeTone;
  detail: string;
} {
  if (attention?.automationPaused) {
    return {
      label: "Paused",
      tone: "danger",
      detail: "Automatic updates are held for this container after failures.",
    };
  }
  if (attention?.awaitingApproval) {
    return {
      label: "Needs approval",
      tone: "warn",
      detail: "A decision is waiting for a person to release it.",
    };
  }
  if (attention?.dependencyKnown && attention.dependencyState === "dependencyWaiting") {
    return {
      label: "Waiting on a dependency",
      tone: "warn",
      detail: attention.dependencyBlockedBy
        ? `Held until ${attention.dependencyBlockedBy} has been updated and verified.`
        : "Held until something it depends on has been updated.",
    };
  }
  return {
    label: "Managed by policy",
    tone: "neutral",
    detail:
      "Whether this container updates on its own is decided by the update policies.",
  };
}

/**
 * Drift, as a question an operator actually has.
 *
 * "Configuration changed" is what `openDrift` means. The word drift is kept for
 * the detail view and the specialised page, where it is the accurate term.
 */
function ConfigurationPanel({
  attention,
  containerId,
}: {
  attention?: ContainerAttention;
  containerId: string;
}) {
  const changes = attention?.openDrift ?? 0;

  return (
    <Panel
      title="Configuration"
      action={
        changes > 0 ? (
          <Link
            to={`/drift/container/${encodeURIComponent(containerId)}`}
            className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
          >
            Review changes
          </Link>
        ) : undefined
      }
    >
      {changes > 0 ? (
        <>
          <StatusBadge tone="warn" label="Changed" />
          <p className="mt-2 text-xs text-content-muted">
            {changes} {changes === 1 ? "change" : "changes"} since the recorded
            configuration.
          </p>
        </>
      ) : (
        <>
          <StatusBadge tone="ok" label="Unchanged" />
          <p className="mt-2 text-xs text-content-muted">
            No configuration changes detected since the recorded baseline.
          </p>
        </>
      )}
    </Panel>
  );
}

/**
 * The last thing that happened, from the projection.
 *
 * The full per-container lifecycle history is NOT available: the acquisition,
 * execution and rollback lists cannot be filtered by container in the current
 * API. This reports the outcomes the projection carries and sends the operator
 * to the Activity workspace rather than implying a history it cannot assemble.
 */
function RecentPanel({ attention }: { attention?: ContainerAttention }) {
  const update = attention?.lastUpdate;
  const rollback = attention?.lastRollback;

  return (
    <Panel
      title="Recent activity"
      action={
        <Link
          to="/activity"
          className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
        >
          View activity
        </Link>
      }
    >
      {!update && !rollback ? (
        <p className="text-sm text-content-muted">
          HarborMaster has not changed this container.
        </p>
      ) : (
        <ul className="flex flex-col gap-2 text-sm">
          {update ? (
            <li>
              <StatusBadge
                tone={
                  update.state === "succeeded"
                    ? "ok"
                    : update.needsAttention
                      ? "danger"
                      : "neutral"
                }
                label={update.state === "succeeded" ? "Updated" : "Update " + update.state}
              />
              {update.at ? (
                <span className="ml-2 text-xs text-content-muted">
                  {formatMoment(update.at)}
                </span>
              ) : null}
            </li>
          ) : null}
          {rollback ? (
            <li>
              <StatusBadge
                tone={rollback.state === "succeeded" ? "ok" : "danger"}
                label={
                  rollback.state === "succeeded"
                    ? "Recovered"
                    : "Rollback " + rollback.state
                }
              />
              {rollback.at ? (
                <span className="ml-2 text-xs text-content-muted">
                  {formatMoment(rollback.at)}
                </span>
              ) : null}
            </li>
          ) : null}
        </ul>
      )}
    </Panel>
  );
}

/**
 * Update order, quiet when there is none.
 *
 * A container with no declared dependency gets one line. `dependencyKnown`
 * false asserts nothing -- a deployment without dependency tracking must not
 * read as "independent, confirmed".
 */
function OrderPanel({ attention }: { attention?: ContainerAttention }) {
  const blockedBy = attention?.dependencyBlockedBy;

  return (
    <Panel
      title="Update order"
      action={
        blockedBy ? (
          <Link
            to="/dependencies"
            className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
          >
            Manage dependencies
          </Link>
        ) : undefined
      }
    >
      {attention?.dependencyKnown === false ? (
        <p className="text-sm text-content-muted">Not established.</p>
      ) : blockedBy ? (
        <p className="text-sm">
          Updated after <span className="font-medium">{blockedBy}</span>.
        </p>
      ) : (
        <p className="text-sm text-content-muted">Independent.</p>
      )}
    </Panel>
  );
}
