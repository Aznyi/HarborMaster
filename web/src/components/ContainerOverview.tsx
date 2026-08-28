import { formatMoment } from "../api/presentation";
import { Link } from "react-router";

import type { ContainerAttention, ContainerDetail } from "../api/inventoryTypes";
import { UPDATE_TYPE_LABELS } from "../api/imageTypes";
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
          <p className="break-all font-mono text-sm">
            {detail.overview.image?.raw ?? "—"}
          </p>
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
 * The update verdict, in the same words Phase 2 uses.
 *
 * Deliberately not a second recommendation engine: the recommendation is read
 * off the projection, and acting on it happens in the Updates workspace.
 */
function UpdatePanel({ attention }: { attention?: ContainerAttention }) {
  if (!attention || !attention.updateType || attention.updateType === "none") {
    return (
      <Panel title="Update">
        <p className="text-sm">Up to date</p>
        <p className="mt-1 text-xs text-content-muted">
          Nothing newer has been established for what this container follows.
        </p>
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
        {UPDATE_TYPE_LABELS[attention.updateType]}
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
