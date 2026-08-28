import { formatMoment } from "../api/presentation";
import { Link } from "react-router";

import {
  AUTOMATION_MODE_LABELS,
  UPDATE_STRATEGY_LABELS,
  modeMutates,
  type AutomationStatus,
  type MaintenanceWindow,
  type UpdatePolicy,
} from "../api/automationTypes";
import type { AutomationStatusResponse } from "../api/automationTypes";
import type { DependencyListing } from "../api/dependencyTypes";
import type { ResourceState } from "../hooks/useApiResource";
import { DisconnectedState, ErrorState, LoadingState } from "./States";
import { StatusBadge } from "./StatusBadge";

/**
 * The Automation workspace's sections.
 *
 * # What this is for
 *
 * Answering, on one page, the questions a homelab operator actually has: is
 * automatic updating on, what will it touch, what will it not touch and why,
 * when does it run, and is anything waiting for me.
 *
 * Those answers used to be spread across the automation page, the update-policy
 * editor, the dependency page and the paused list -- four screens, each named
 * after a part of the engine rather than a question.
 *
 * # What it deliberately does not do
 *
 * It computes nothing about eligibility. Every count and every state here is a
 * field the server already sent on `AutomationStatus`, a policy record, a
 * dependency listing, or a pause record. Where the engine has an opinion, this
 * renders the engine's opinion.
 */

// ------------------------------------------------------------- the header --

/**
 * Is automatic updating on, and what is it doing.
 *
 * # Why "enabled" is two facts and not one
 *
 * The engine capability and a policy that may act are different things, and an
 * operator who confuses them fixes the wrong one. A deployment can have the
 * engine running with every policy in observe mode, which changes nothing and
 * is not a fault; and it can have acting policies with the engine switched off,
 * which also changes nothing and is a deployment setting. Both are reported.
 */
export function AutomationSummary({
  state,
  note,
  children,
}: {
  /**
   * One extra sentence, when the settled state has one worth reading.
   *
   * "3 containers are currently eligible" and "no containers are currently
   * eligible" are the two that survived the Phase-6-B condensation: they are
   * the only part of the onboarding block that was not already said by the
   * badge two lines above.
   */
  note?: string;
  /** The engine's safety warning, rendered inside the one status section. */
  children?: React.ReactNode;
  /**
   * The whole resource, not just the payload.
   *
   * A read that failed is not an engine that is off, and rendering "Off" for an
   * unreachable API would send an operator to change a deployment setting that
   * is already correct. This section owns those states because it is the one
   * that reports the engine.
   */
  state: ResourceState<AutomationStatusResponse>;
}) {
  if (state.status === "loading") return <LoadingState label="Loading automation" />;
  if (state.status === "disconnected") return <DisconnectedState onRetry={state.refresh} />;
  if (state.error) return <ErrorState error={state.error} onRetry={state.refresh} />;

  const engine = state.data?.status;
  const enabled = Boolean(engine?.enabled);
  const acting = engine?.actingPolicies ?? 0;

  const engineState = !enabled
    ? {
        tone: "danger" as const,
        label: "Off",
        detail: "The engine capability is disabled, so no policy can act however it is configured.",
      }
    : acting === 0
      ? {
          tone: "warn" as const,
          label: "Watching only",
          detail: "The engine is running, but no policy is set to change a container.",
        }
      : {
          tone: "ok" as const,
          label: "On",
          detail: `${acting} ${acting === 1 ? "policy" : "policies"} may update containers automatically.`,
        };

  const cards = [
    {
      label: "Policies in force",
      value: `${engine?.enabledPolicies ?? 0} of ${engine?.policies ?? 0}`,
      hint: acting > 0 ? `${acting} may change a container` : "None may change a container",
    },
    {
      label: "Need approval",
      value: engine?.awaitingApproval ?? 0,
      hint: "Waiting for a person",
    },
    {
      label: "Paused",
      value: engine?.pausedContainers ?? 0,
      hint: "Held after failures",
    },
    {
      label: "Next pass",
      value: engine?.nextRunAt ? new Date(engine.nextRunAt).toLocaleTimeString() : "—",
      hint: engine?.running ? "A pass is running now" : "Scheduled",
    },
  ];

  return (
    <section
      aria-labelledby="automation-state-heading"
      data-testid="automation-summary"
      className="flex flex-col gap-4 rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <div className="flex flex-wrap items-center gap-3">
        <h2 id="automation-state-heading" className="text-base font-semibold">
          Automatic updates
        </h2>
        <StatusBadge tone={engineState.tone} label={engineState.label} />
      </div>
      <p className="max-w-prose text-sm text-content-muted">{engineState.detail}</p>

      {note ? (
        <p className="max-w-prose text-sm text-content-muted">{note}</p>
      ) : null}

      {/* The warning lives with the state it qualifies. As a free-standing
          banner above this section it was the third consecutive restatement of
          "automatic updates are on"; here it is the second half of the first. */}
      {children}

      <dl className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        {cards.map((card) => (
          <div key={card.label} className="rounded-lg border border-border-subtle bg-surface px-4 py-3">
            <dt className="text-xs uppercase tracking-wide text-content-muted">
              {card.label}
            </dt>
            <dd className="mt-1 text-xl font-semibold">{card.value}</dd>
            <dd className="text-xs text-content-muted">{card.hint}</dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

// ---------------------------------------------------------------- window --

/** Renders a maintenance window as an operator reads a clock. */
export function describeWindow(window?: MaintenanceWindow): string {
  if (!window || window.alwaysOpen) return "Any time";
  if (!window.start || !window.end) return "Any time";

  const days =
    window.weekdays && window.weekdays.length > 0
      ? window.weekdays
          .map((day) => ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"][day] ?? String(day))
          .join(", ")
      : "every day";

  return `${window.start}–${window.end} ${window.timezone ?? "UTC"}, ${days}`;
}

/**
 * Whether updates may run right now, and when they next may.
 *
 * `windowOpen` and `nextWindowOpensAt` are the ENGINE's answer, computed across
 * every policy's window and timezone. Working it out here from the policy rows
 * would be a second scheduler that disagrees on daylight saving.
 */
export function MaintenanceWindowState({ engine }: { engine?: AutomationStatus }) {
  if (engine?.windowOpen === undefined) return null;

  return (
    <div className="rounded-lg border border-border-subtle bg-surface px-4 py-3">
      <p className="text-xs uppercase tracking-wide text-content-muted">
        Maintenance window
      </p>
      {engine.windowOpen ? (
        <p className="mt-1 text-sm">Open — updates may run now</p>
      ) : (
        <>
          <p className="mt-1 text-sm">Closed</p>
          <p className="text-xs text-content-muted">
            {engine.nextWindowOpensAt
              ? `Next opens ${formatMoment(engine.nextWindowOpensAt)}`
              : "No policy window is scheduled to open."}
          </p>
        </>
      )}
    </div>
  );
}

// ------------------------------------------------------- needs attention --

/**
 * The only section that earns its place by being conditional.
 *
 * Rendered when something is actually waiting. A permanent "nothing needs
 * attention" panel trains an operator to skip the area where the real thing
 * will eventually appear.
 *
 * Each waiting state links to the queue that can actually clear it. Automation
 * owns the REASON something is waiting; the workflow that answers it lives
 * where it already lives, and is not reimplemented here.
 */
export function AutomationAttention({
  engine,
  pausedCount,
  manualReviews,
}: {
  engine?: AutomationStatus;
  pausedCount: number;
  /**
   * Plans the planner asked a person to review.
   *
   * DISTINCT from `awaitingApproval`, and the distinction decides where the
   * operator is sent. `awaitingApproval` counts automation DECISIONS the engine
   * held, which are released from the approvals queue. A manual review is a
   * verdict on a PLAN, answered in the Updates workspace. Sending either to the
   * other queue offers a control that cannot clear it.
   */
  manualReviews: number;
}) {
  const approvals = engine?.awaitingApproval ?? 0;
  const windowClosed = engine?.windowOpen === false;

  if (approvals === 0 && pausedCount === 0 && manualReviews === 0 && !windowClosed) {
    return null;
  }

  return (
    <section
      aria-labelledby="automation-attention-heading"
      data-testid="automation-attention"
      className="flex flex-col gap-3 rounded-xl border border-warn/40 bg-warn-soft p-5"
    >
      <h2 id="automation-attention-heading" className="text-base font-semibold">
        Needs attention
      </h2>

      <ul className="flex flex-col gap-3 text-sm">
        {approvals > 0 ? (
          <li className="flex flex-wrap items-center gap-3">
            <span>
              {approvals} automation{" "}
              {approvals === 1 ? "decision is" : "decisions are"} held for approval
            </span>
            <Link
              to="/automation/approvals"
              className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
            >
              Review and release
            </Link>
          </li>
        ) : null}

        {manualReviews > 0 ? (
          <li className="flex flex-wrap items-center gap-3">
            <span>
              {manualReviews} {manualReviews === 1 ? "update needs" : "updates need"}{" "}
              a person to review the change
            </span>
            <Link
              to="/updates?tab=review"
              className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
            >
              Review in Updates
            </Link>
          </li>
        ) : null}

        {pausedCount > 0 ? (
          <li className="flex flex-wrap items-center gap-3">
            <span>
              {pausedCount}{" "}
              {pausedCount === 1 ? "container is" : "containers are"} paused after
              failures
            </span>
          </li>
        ) : null}

        {windowClosed ? (
          <li>
            The maintenance window is closed, so nothing will be updated until it
            opens
            {engine?.nextWindowOpensAt
              ? ` — next ${formatMoment(engine.nextWindowOpensAt)}`
              : ""}
            .
          </li>
        ) : null}
      </ul>
    </section>
  );
}

// ----------------------------------------------------------- the settings --

/**
 * What automatic updating is actually configured to do.
 *
 * # Why this is not called "the default policy"
 *
 * There is no such thing in this backend. Policies are a priority-ordered set
 * with no default flag, so naming one would be inventing a concept the server
 * does not have. What this does instead is honest about the two real cases: one
 * enabled policy IS the settings, and several are a set worth summarising.
 *
 * The full editor stays where it is. This is a readable summary with a way in,
 * not a second editor.
 */
export function AutomationSettings({
  policies,
  mayManage,
}: {
  policies: readonly UpdatePolicy[];
  mayManage: boolean;
}) {
  const active = policies.filter((policy) => policy.enabled && !policy.archived);
  const disabled = policies.filter((policy) => !policy.enabled && !policy.archived);

  return (
    <section
      aria-labelledby="automation-settings-heading"
      data-testid="automation-settings"
      className="flex flex-col gap-4 rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 id="automation-settings-heading" className="text-base font-semibold">
          Automatic update settings
        </h2>
        <Link
          to="/update-policies"
          className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
        >
          {mayManage ? "Edit settings" : "View settings"}
        </Link>
      </div>

      {active.length === 0 ? (
        <p className="max-w-prose text-sm text-content-muted">
          No policy is enabled, so HarborMaster will not update anything on its
          own. Nothing is wrong with the engine; it has not been told what to do.
        </p>
      ) : (
        <>
          <ul className="flex flex-col gap-3">
            {active.map((policy) => (
              <PolicySummary key={policy.policyId} policy={policy} />
            ))}
          </ul>
          {disabled.length > 0 ? (
            <p className="text-xs text-content-muted">
              {active.length} active, {disabled.length} disabled.
            </p>
          ) : null}
        </>
      )}
    </section>
  );
}

/** One policy, in the terms it was configured in. */
function PolicySummary({ policy }: { policy: UpdatePolicy }) {
  const rows: [string, string][] = [
    [
      "Applies to",
      policy.scope === "allEligible"
        ? "All eligible containers"
        : describeSelector(policy),
    ],
    ["Allowed updates", UPDATE_STRATEGY_LABELS[policy.strategy]],
    [
      "Mode",
      // The one translation this page makes, and it is a summary rather than a
      // rename: the backend enum is unchanged and its own label is shown too.
      `${modeMutates(policy.mode) ? "May change containers" : "Evaluates only, changes nothing"} — ${AUTOMATION_MODE_LABELS[policy.mode]}`,
    ],
    ["Schedule", describeWindow(policy.window)],
    ["If an update fails", describeFailure(policy)],
  ];

  return (
    <li className="rounded-lg border border-border-subtle bg-surface p-4">
      <p className="text-sm font-semibold">{policy.name}</p>
      <dl className="mt-2 grid gap-2 text-sm sm:grid-cols-2">
        {rows.map(([label, value]) => (
          <div key={label} className="min-w-0">
            <dt className="text-xs uppercase tracking-wide text-content-muted">
              {label}
            </dt>
            <dd className="break-words text-content">{value}</dd>
          </div>
        ))}
      </dl>
    </li>
  );
}

function describeSelector(policy: UpdatePolicy): string {
  const selector = policy.selector ?? {};
  const parts: string[] = [];
  if (selector.include?.length) parts.push(`${selector.include.length} named`);
  if (selector.images?.length) parts.push(`${selector.images.length} image patterns`);
  if (selector.labels && Object.keys(selector.labels).length > 0) {
    parts.push(`${Object.keys(selector.labels).length} labels`);
  }
  if (selector.exclude?.length) parts.push(`${selector.exclude.length} excluded`);
  return parts.length > 0 ? parts.join(", ") : "Nothing — the selector is empty";
}

function describeFailure(policy: UpdatePolicy): string {
  const failure = policy.failure;
  if (!failure) return "Default handling";
  const parts: string[] = [];
  parts.push(failure.autoRollback ? "Roll back automatically" : "Stop and wait for a person");
  if (failure.pauseAfterFailures && failure.pauseAfterFailures > 0) {
    parts.push(`pause after ${failure.pauseAfterFailures} failures`);
  }
  return parts.join(", ");
}

// -------------------------------------------------------------- ordering --

/**
 * Update order, and nothing when there is no order to describe.
 *
 * An estate with no declared dependencies has nothing to say here, and a stage
 * diagram listing every independent container as its own stage is noise
 * dressed as information.
 */
export function AutomationOrder({
  dependencies,
  mayManage,
}: {
  dependencies?: DependencyListing;
  mayManage: boolean;
}) {
  const total = dependencies?.total ?? 0;

  return (
    <section
      aria-labelledby="automation-order-heading"
      data-testid="automation-order"
      className="flex flex-col gap-3 rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 id="automation-order-heading" className="text-base font-semibold">
          Update order
        </h2>
        <Link
          to="/dependencies"
          className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
        >
          {total === 0
            ? mayManage
              ? "Configure dependencies"
              : "View dependencies"
            : mayManage
              ? "View or edit"
              : "View dependencies"}
        </Link>
      </div>

      {total === 0 ? (
        <p className="max-w-prose text-sm text-content-muted">
          No dependencies configured. Containers are evaluated independently.
        </p>
      ) : (
        <p className="max-w-prose text-sm text-content-muted">
          {total} dependency {total === 1 ? "relationship" : "relationships"}{" "}
          configured. A container is not updated until what it depends on has
          been updated and verified.
        </p>
      )}
    </section>
  );
}
