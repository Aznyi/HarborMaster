import { useCallback, useMemo, useState } from "react";
import { Link } from "react-router";

import type {
  AutomationDecision,
  AutomationRun,
  AutomationStatus,
} from "../api/automationTypes";
import {
  AUTOMATION_TRIGGER_LABELS,
  isAutomationRunActive,
} from "../api/automationTypes";
import { DecisionReason } from "../components/DependencyStatus";
import {
  AutomationRunStateBadge,
  AutomationVerdictBadge,
  AutomationWarningNotice,
  SelfUpdateNotice,
} from "../components/AutomationBadges";
import { PageIntro } from "../components/PageIntro";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import {
  useAutomationRuns,
  useAutomationStatus,
  useAutomationUpcoming,
  useRunAutomationPass,
} from "../hooks/useAutomation";
import { useSession } from "../hooks/useSession";

/**
 * The update engine.
 *
 * # What this page has to make unmissable
 *
 * Three things, in this order.
 *
 *  1. **Whether the engine can act at all.** A deployment with automation off
 *     and a page full of policies is a normal state, and one where nothing will
 *     happen. Saying so first is the difference between an operator who knows
 *     that and one who finds out in a month.
 *  2. **When it will next act.** "Waiting for 02:00" is the answer to the
 *     question an operator opens this page with, and it comes from the server's
 *     own timezone calculation rather than from a second one in the browser.
 *  3. **What it would do right now.** The preview is a READ â€” it writes no run
 *     and reaches no service â€” so it can sit on the page an operator refreshes.
 *
 * # Dry run is offered before Run
 *
 * Deliberately, and in that order on screen. The safe control is the one the
 * eye lands on first.
 */
export function Automation() {
  const status = useAutomationStatus({ poll: true });
  const runs = useAutomationRuns({ page: 1, pageSize: 10 });
  const upcoming = useAutomationUpcoming();

  const engine = status.data?.status;

  return (
    <div className="space-y-6">
      <PageIntro
        title="Automation"
        description={
          "The update engine: what it decided, what it would do next, and " +
          "when it may next act. Every change it makes goes through the same " +
          "checks an operator's own update would."
        }
      />

      <AutomationWarningNotice enabled={Boolean(engine?.enabled)} />

      <SelfUpdateNotice self={engine?.self} />

      <StatusPanel state={status} />

      <PassControls
        enabled={Boolean(engine?.enabled)}
        running={Boolean(engine?.running)}
        onRan={() => {
          status.refresh();
          runs.refresh();
          upcoming.refresh();
        }}
      />

      <UpcomingPanel state={upcoming} />

      <RecentRuns state={runs} />
    </div>
  );
}

// ---------------------------------------------------------------- status --

function StatusPanel({ state }: { state: ReturnType<typeof useAutomationStatus> }) {
  if (state.status === "loading") return <LoadingState label="Loading automation" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) return <ErrorState error={state.error} onRetry={state.refresh} />;
  if (!state.data) return <LoadingState label="Loading automation" />;

  const engine = state.data.status;

  return (
    <section className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
      <Card
        label="Policies"
        value={`${engine.enabledPolicies ?? 0} of ${engine.policies ?? 0}`}
        hint="in force"
      />
      <Card
        label="Next pass"
        value={engine.running ? "Running now" : formatMoment(engine.nextRunAt)}
        hint={engine.lastRunAt ? `last ${formatMoment(engine.lastRunAt)}` : "never run"}
      />
      <Card
        label="Maintenance window"
        value={engine.windowOpen ? "Open" : "Closed"}
        hint={windowHint(engine)}
      />
      <Card
        label="Blocked"
        value={`${engine.pausedContainers ?? 0} paused`}
        hint={
          (engine.awaitingApproval ?? 0) > 0
            ? `${engine.awaitingApproval} waiting for approval`
            : "nothing waiting for approval"
        }
      />
      {/* The approvals are an ACTION, not a statistic. Without this the only
          route to them was opening an archived pass and finding the row. */}
      {(engine.awaitingApproval ?? 0) > 0 ? (
        <Link
          to="/automation/approvals"
          className="rounded-lg border border-warn/40 bg-warn-soft p-4 sm:col-span-2 lg:col-span-4"
        >
          <p className="text-sm font-medium text-content">
            {engine.awaitingApproval} update
            {engine.awaitingApproval === 1 ? "" : "s"} waiting for you to approve
          </p>
          <p className="mt-1 text-xs text-content-muted">
            Review and release them &rarr;
          </p>
        </Link>
      ) : null}
    </section>
  );
}

/** The window hint, from the SERVER's calculation rather than a second one. */
function windowHint(engine: AutomationStatus): string {
  if (engine.windowOpen) return "a policy admits work now";
  if (engine.nextWindowOpensAt) {
    return `next opens ${formatMoment(engine.nextWindowOpensAt)}`;
  }
  return "no policy has an open window";
}

function Card({
  label,
  value,
  hint,
}: {
  label: string;
  value: string;
  hint: string;
}) {
  return (
    <div className="rounded-xl border border-border-subtle bg-surface-raised px-4 py-3">
      <p className="text-xs uppercase tracking-wide text-content-muted">{label}</p>
      <p className="mt-1 text-lg font-semibold">{value}</p>
      <p className="mt-0.5 text-xs text-content-muted">{hint}</p>
    </div>
  );
}

// -------------------------------------------------------------- controls --

function PassControls({
  enabled,
  running,
  onRan,
}: {
  enabled: boolean;
  running: boolean;
  onRan: () => void;
}) {
  const session = useSession();
  const runPass = useRunAutomationPass();

  const [busy, setBusy] = useState<"dryRun" | "run" | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const [failure, setFailure] = useState<string | null>(null);

  const mayRun = Boolean(session.user?.permissions.includes("automation:run"));

  const start = useCallback(
    async (dryRun: boolean) => {
      setBusy(dryRun ? "dryRun" : "run");
      setMessage(null);
      setFailure(null);
      try {
        const result = await runPass(dryRun);
        const run = result.run;
        setMessage(
          dryRun
            ? `Dry run considered ${run.considered ?? 0} containers; ` +
              `${run.eligible ?? 0} would be updated. Nothing was changed.`
            : `Pass considered ${run.considered ?? 0} containers and submitted ` +
              `${run.submitted ?? 0} updates.`,
        );
        onRan();
      } catch (error) {
        setFailure(error instanceof Error ? error.message : "The pass could not be run");
      } finally {
        setBusy(null);
      }
    },
    [onRan, runPass],
  );

  if (!mayRun) return null;

  return (
    <section className="space-y-3 rounded-xl border border-border-subtle bg-surface-raised px-4 py-3">
      <div>
        <h3 className="text-sm font-semibold">Run a pass now</h3>
        <p className="mt-1 max-w-prose text-sm text-content-muted">
          A pass evaluates every container against the policies in force. A dry
          run decides everything and changes nothing; it is recorded, so you can
          read afterwards exactly what would have happened and in what order.
        </p>
      </div>

      <div className="flex flex-wrap gap-2">
        {/* The safe control first, deliberately. */}
        <button
          type="button"
          className="rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium disabled:opacity-50"
          disabled={!enabled || running || busy !== null}
          onClick={() => void start(true)}
        >
          {busy === "dryRun" ? "Runningâ€¦" : "Dry run"}
        </button>
        <button
          type="button"
          className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-1.5 text-sm font-medium text-warn disabled:opacity-50"
          disabled={!enabled || running || busy !== null}
          onClick={() => void start(false)}
        >
          {busy === "run" ? "Runningâ€¦" : "Run pass"}
        </button>
      </div>

      {!enabled ? (
        <p className="text-sm text-content-muted">
          The engine is switched off, so a pass cannot be run.
        </p>
      ) : null}
      {running ? (
        <p className="text-sm text-content-muted">
          A pass is already running. Only one may run at a time.
        </p>
      ) : null}
      {message ? (
        <p role="status" className="text-sm text-content-muted">
          {message}
        </p>
      ) : null}
      {failure ? (
        <p role="alert" className="text-sm text-danger">
          {failure}
        </p>
      ) : null}
    </section>
  );
}

// -------------------------------------------------------------- upcoming --

function UpcomingPanel({ state }: { state: ReturnType<typeof useAutomationUpcoming> }) {
  const [showAll, setShowAll] = useState(false);

  const decisions = state.data?.items ?? [];
  const visible = useMemo(
    () => (showAll ? decisions : decisions.filter((d) => d.verdict !== "skip")),
    [decisions, showAll],
  );

  if (state.status === "loading") return <LoadingState label="Evaluating" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) return <ErrorState error={state.error} onRetry={state.refresh} />;

  return (
    <section className="space-y-3">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div>
          <h3 className="text-sm font-semibold">What the next pass would do</h3>
          <p className="mt-1 max-w-prose text-sm text-content-muted">
            Evaluated now, and written nowhere. This is a read: it records no
            pass and asks no service for anything.
          </p>
        </div>
        <label className="flex items-center gap-2 text-sm text-content-muted">
          <input
            type="checkbox"
            checked={showAll}
            onChange={(event) => setShowAll(event.target.checked)}
          />
          Show skipped containers
        </label>
      </div>

      {visible.length === 0 ? (
        <EmptyState
          title="Nothing would be updated"
          description={
            decisions.length === 0
              ? "No containers were considered. Check that the inventory has run."
              : "Every container was skipped. Turn on â€œShow skipped containersâ€ to see why."
          }
        />
      ) : (
        <DecisionTable decisions={visible} />
      )}
    </section>
  );
}

/** The decision table, shared by the preview and the run detail. */
export function DecisionTable({ decisions }: { decisions: AutomationDecision[] }) {
  return (
    <div
        className="overflow-x-auto rounded-xl border border-border-subtle"
        tabIndex={0}
      >
      <table className="min-w-full text-sm">
        <thead className="bg-surface-sunken text-left text-xs uppercase tracking-wide text-content-muted">
          <tr>
            <th className="px-3 py-2">Container</th>
            <th className="px-3 py-2">Verdict</th>
            <th className="px-3 py-2">Reason</th>
            <th className="px-3 py-2">Change</th>
            <th className="px-3 py-2">Policy</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border-subtle">
          {decisions.map((decision) => (
            <tr key={`${decision.runId}-${decision.position}-${decision.containerName}`}>
              <td className="px-3 py-2 font-medium">{decision.containerName}</td>
              <td className="px-3 py-2">
                <AutomationVerdictBadge verdict={decision.verdict} />
              </td>
              <td className="px-3 py-2">
                <DecisionReason decision={decision} />
              </td>
              <td className="px-3 py-2 text-content-muted">
                {decision.proposedImage ? (
                  <span title={decision.proposedDigest}>
                    {decision.currentImage} â†’ {decision.proposedImage}
                  </span>
                ) : (
                  "â€”"
                )}
              </td>
              <td className="px-3 py-2 text-content-muted">
                {decision.policyName || "â€”"}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ------------------------------------------------------------------ runs --

function RecentRuns({ state }: { state: ReturnType<typeof useAutomationRuns> }) {
  if (state.status === "loading") return null;
  if (state.error) return null;

  const runs = state.data?.items ?? [];
  if (runs.length === 0) {
    return (
      <EmptyState
        title="No passes yet"
        description="Nothing has run. A scheduled pass records a row whether or not it changes anything."
      />
    );
  }

  return (
    <section className="space-y-3">
      <h3 className="text-sm font-semibold">Recent passes</h3>
      <div
        className="overflow-x-auto rounded-xl border border-border-subtle"
        tabIndex={0}
      >
        <table className="min-w-full text-sm">
          <thead className="bg-surface-sunken text-left text-xs uppercase tracking-wide text-content-muted">
            <tr>
              <th className="px-3 py-2">Started</th>
              <th className="px-3 py-2">Trigger</th>
              <th className="px-3 py-2">Outcome</th>
              <th className="px-3 py-2">Considered</th>
              <th className="px-3 py-2">Submitted</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-border-subtle">
            {runs.map((run) => (
              <tr key={run.runId}>
                <td className="px-3 py-2">
                  <Link className="font-medium underline" to={`/automation/runs/${run.runId}`}>
                    {formatMoment(run.startedAt)}
                  </Link>
                </td>
                <td className="px-3 py-2 text-content-muted">
                  {AUTOMATION_TRIGGER_LABELS[run.trigger] ?? run.trigger}
                  {run.dryRun ? " (dry run)" : ""}
                </td>
                <td className="px-3 py-2">
                  <AutomationRunStateBadge state={run.state} />
                </td>
                <td className="px-3 py-2 text-content-muted">{run.considered ?? 0}</td>
                <td className="px-3 py-2 text-content-muted">{describeSubmitted(run)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

/** A dry run's zero is not the same fact as a real pass's zero. */
function describeSubmitted(run: AutomationRun): string {
  if (run.dryRun) return `${run.eligible ?? 0} would have`;
  if (isAutomationRunActive(run)) return "in progress";
  return String(run.submitted ?? 0);
}

/** Renders an instant in the viewer's locale, or a dash. */
export function formatMoment(value: string | undefined): string {
  if (!value) return "â€”";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return "â€”";
  return parsed.toLocaleString();
}

