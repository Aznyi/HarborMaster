import { formatMoment } from "../api/presentation";
import { Link } from "react-router";

import type { AutomationStatus } from "../api/automationTypes";
import type { ChangePlanSummary } from "../api/planTypes";
import type { InventoryStatus } from "../api/inventoryTypes";
import { buildActivityFeed, describeChange, type ActivityEntry } from "../api/activityFeed";
import type { Acquisition } from "../api/acquisitionTypes";
import type { Execution } from "../api/executionTypes";
import type { Rollback } from "../api/rollbackTypes";
import { StatusBadge } from "./StatusBadge";

/**
 * The dashboard's answer at a glance, and its recent history.
 *
 * # What these are for
 *
 * Four questions -- are my containers up, are there updates, is automation
 * running, does anything need me -- answered from data the page already reads,
 * each card leading into the workspace that owns the subject.
 *
 * # What they deliberately do not do
 *
 * They decide nothing. Every count is a field a server response carried, and
 * the activity preview reuses Phase 4's joiner rather than correlating the
 * three lifecycle records a second time. A dashboard that reimplemented any of
 * that would be a second opinion that drifts from the page it summarises.
 */

/** One card. `to` is where the subject is actually managed. */
interface SummaryCard {
  label: string;
  value: string;
  hint: string;
  to: string;
  tone?: "ok" | "warn" | "danger" | "neutral";
}

export function DashboardSummary({
  inventory,
  plans,
  automation,
  attentionCount,
}: {
  inventory?: InventoryStatus | null;
  plans?: ChangePlanSummary | null;
  automation?: AutomationStatus | null;
  /** Items the shared attention model rated as work, not context. */
  attentionCount: number;
}) {
  const counts = inventory?.counts;
  const running = counts?.running ?? 0;
  const unhealthy = counts?.unhealthy ?? 0;

  const cards: SummaryCard[] = [
    {
      label: "Containers",
      value: String(running),
      hint: unhealthy > 0 ? `${unhealthy} unhealthy` : "running",
      to: "/containers",
      tone: unhealthy > 0 ? "danger" : "ok",
    },
    {
      label: "Updates",
      value: String(plans?.actionable ?? 0),
      hint:
        (plans?.needsReview ?? 0) > 0
          ? `${plans?.needsReview} need review`
          : "ready to act on",
      // Deep-linked to the tab that holds them, using Phase 2's query value.
      to: (plans?.needsReview ?? 0) > 0 ? "/updates?tab=review" : "/updates",
      tone: (plans?.needsReview ?? 0) > 0 ? "warn" : "neutral",
    },
    {
      label: "Automation",
      value: automationLabel(automation),
      hint:
        (automation?.pausedContainers ?? 0) > 0
          ? `${automation?.pausedContainers} paused`
          : "update engine",
      to: "/automation",
      tone: automationTone(automation),
    },
    {
      label: "Needs attention",
      // The attention model groups by ISSUE, not by container: "3 containers
      // are unhealthy" is one item. A bare "2" beside a panel listing six
      // containers reads as a container count and contradicts it, so the unit
      // is stated rather than left to be guessed.
      value:
        attentionCount === 1 ? "1 issue" : `${attentionCount} issues`,
      hint:
        attentionCount > 0
          ? "kinds of issue, not containers"
          : "nothing right now",
      to: "/activity",
      tone: attentionCount > 0 ? "danger" : "ok",
    },
  ];

  // A list of links, not a definition list: a <dl> may only contain dt, dd and
  // div, so wrapping each card in an <a> left every dt and dd without a valid
  // parent. axe reported it as definition-list + dlitem, and it is the correct
  // reading -- these are navigation targets that happen to show a number.
  return (
    <ul
      className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4"
      data-testid="dashboard-summary"
    >
      {cards.map((card) => (
        <li key={card.label} className="min-w-0">
          <Link
            to={card.to}
            className="flex h-full flex-col rounded-xl border border-border-subtle bg-surface-raised px-4 py-3 transition-colors hover:border-accent"
          >
            <span className="text-xs uppercase tracking-wide text-content-muted">
              {card.label}
            </span>
            <span className="mt-1 text-2xl font-semibold">{card.value}</span>
            <span className="text-xs text-content-muted">{card.hint}</span>
          </Link>
        </li>
      ))}
    </ul>
  );
}

/**
 * The engine in one word.
 *
 * "On" and "watching only" are different answers with opposite remedies -- one
 * is a deployment setting, the other a policy mode -- and Phase 3 makes the
 * same distinction on the page this card links to.
 */
function automationLabel(automation?: AutomationStatus | null): string {
  if (!automation) return "—";
  if (!automation.enabled) return "Off";
  return (automation.actingPolicies ?? 0) > 0 ? "On" : "Watching";
}

function automationTone(
  automation?: AutomationStatus | null,
): "ok" | "warn" | "danger" | "neutral" {
  if (!automation) return "neutral";
  if (!automation.enabled) return "warn";
  return (automation.actingPolicies ?? 0) > 0 ? "ok" : "warn";
}

/**
 * The subsystem state the header does NOT carry.
 *
 * Phase 1's header control already answers "is the backend reachable" and "is
 * Docker reachable", on every page and always visible. Repeating those two here
 * put the same sentence on screen twice, which is how a dashboard starts
 * reading as a report.
 *
 * The subsystem badges are not repeated either: the estate disclosure below
 * already carries them with their version and detail strings. What is left is
 * the pair nothing else on the page answers -- whether the update engine is
 * running, and when HarborMaster last actually looked at the host.
 */
export function SystemStrip({
  automation,
  lastCheck,
}: {
  automation?: AutomationStatus | null;
  /** When the inventory refresh last finished. */
  lastCheck?: string;
}) {
  const rows: { label: string; node: React.ReactNode }[] = [
    {
      label: "Update engine",
      node: automation ? (
        <StatusBadge
          tone={automation.enabled ? "ok" : "warn"}
          label={
            automation.running
              ? "Running a pass"
              : automation.enabled
                ? "Running"
                : "Off"
          }
        />
      ) : (
        <span className="text-sm text-content-muted">unknown</span>
      ),
    },
    {
      label: "Last checked",
      node: (
        <span className="text-sm text-content-muted">
          {formatMoment(lastCheck)}
        </span>
      ),
    },
  ];

  return (
    <section
      aria-labelledby="dashboard-system-heading"
      data-testid="dashboard-system"
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <h2 id="dashboard-system-heading" className="text-base font-semibold">
        System
      </h2>
      <dl className="mt-3 grid gap-3 sm:grid-cols-2">
        {rows.map((row) => (
          <div key={row.label} className="min-w-0">
            <dt className="text-xs uppercase tracking-wide text-content-muted">
              {row.label}
            </dt>
            <dd className="mt-1">{row.node}</dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

/**
 * The last few things HarborMaster did.
 *
 * Built with Phase 4's `buildActivityFeed`, on the same bounded reads the page
 * already makes for its attention model. A preview, not a history: five rows
 * and a way to the workspace that owns the subject.
 */
export function RecentActivity({
  acquisitions,
  executions,
  rollbacks,
  limit = 5,
}: {
  acquisitions: readonly Acquisition[];
  executions: readonly Execution[];
  rollbacks: readonly Rollback[];
  limit?: number;
}) {
  const entries = buildActivityFeed(acquisitions, executions, rollbacks).slice(0, limit);

  return (
    <section
      aria-labelledby="dashboard-activity-heading"
      data-testid="dashboard-activity"
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 id="dashboard-activity-heading" className="text-base font-semibold">
          Recent activity
        </h2>
        <Link
          to="/activity"
          className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
        >
          View all activity
        </Link>
      </div>

      {entries.length === 0 ? (
        <p className="mt-3 text-sm text-content-muted">
          HarborMaster has not changed anything yet.
        </p>
      ) : (
        <ol className="mt-3 flex flex-col gap-2">
          {entries.map((entry) => (
            <ActivityLine key={entry.key} entry={entry} />
          ))}
        </ol>
      )}
    </section>
  );
}

function ActivityLine({ entry }: { entry: ActivityEntry }) {
  const change = describeChange(entry);

  return (
    <li className="flex flex-wrap items-baseline gap-x-3 gap-y-1 text-sm">
      <span className="font-medium">{entry.containerName}</span>
      <StatusBadge tone={entry.status.tone} label={entry.status.label} />
      {change ? (
        <span className="min-w-0 break-all font-mono text-xs text-content-muted">
          {change}
        </span>
      ) : null}
      {entry.at ? (
        <time dateTime={entry.at} className="ml-auto text-xs text-content-muted">
          {formatMoment(entry.at)}
        </time>
      ) : null}
    </li>
  );
}
