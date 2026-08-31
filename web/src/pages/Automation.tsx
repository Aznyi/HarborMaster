import { useCallback, useMemo, useState } from "react";
import { Link } from "react-router";
import { formatMoment } from "../api/presentation";

import type {
  AutomationDecision,
  AutomationRun,
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
import { AutomationOnboarding } from "../components/AutomationOnboarding";
import { ContainerBehaviorOverrides } from "../components/ContainerBehaviorOverrides";
import { SimpleUpdatesPanel } from "../components/SimpleUpdatesPanel";
import { enableSimpleUpdates, disableSimpleUpdates } from "../api/client";
import type { FirstRunFacts } from "../api/firstRun";
import { describeFirstRun, firstRunExplanation } from "../api/firstRun";
import type { HealthReport } from "../api/types";
import type { ResourceState } from "../hooks/useApiResource";
import { useInventory } from "../hooks/useContainers";
import { usePlans } from "../hooks/usePlans";
import { PageIntro } from "../components/PageIntro";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import {
  useAutomationPauses,
  useAutomationRuns,
  useAutomationStatus,
  useContainerBehaviorSummary,
  useSimpleUpdates,
  useAutomationUpcoming,
  useUpdatePolicies,
  useRunAutomationPass,
} from "../hooks/useAutomation";
import { useSession } from "../hooks/useSession";
import { useDependencies } from "../hooks/useDependencies";
import {
  AutomationAttention,
  AutomationOrder,
  AutomationSettings,
  AutomationSummary,
  MaintenanceWindowState,
} from "../components/AutomationWorkspace";
import { PauseCard } from "./AutomationPaused";

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
export function Automation({ health }: { health: ResourceState<HealthReport> }) {
  const status = useAutomationStatus({ poll: true });
  const runs = useAutomationRuns({ page: 1, pageSize: 10 });
  const upcoming = useAutomationUpcoming();

  // The three extra reads the onboarding panel needs, each requested once.
  //
  // None of them is per-container: `inventory` reports one generation number,
  // `plans` carries the planner's own status alongside the first page, and the
  // health report is the app-wide capability read every page already makes.
  const inventory = useInventory();
  // The workspace's own reads. All existing endpoints, each the same one its
  // specialised page already uses; none of them decides anything.
  const policies = useUpdatePolicies({ page: 1, pageSize: 50 });
  const pauses = useAutomationPauses();
  const dependencies = useDependencies();
  const plans = usePlans({ page: 1, pageSize: 1 });
  // ONE more request, whatever the number of overrides. The alternative --
  // listing preferences and then asking about each container -- is the request
  // explosion this section exists without.
  const containerBehaviors = useContainerBehaviorSummary();

  const session = useSession();
  const engine = status.data?.status;
  const mayManage = Boolean(session.user?.permissions.includes("automation:manage"));

  // The automatic-updates switch. Its own request state, because a failure to
  // flip it must be reported on the panel that owns it rather than as a page
  // error that says nothing about which control failed.
  const simple = useSimpleUpdates();
  const [switching, setSwitching] = useState(false);
  const [switchError, setSwitchError] = useState<string | null>(null);

  const flipSimpleUpdates = async (on: boolean) => {
    setSwitching(true);
    setSwitchError(null);
    try {
      await (on ? enableSimpleUpdates() : disableSimpleUpdates());
      // Re-read rather than assume. The server owns the resulting policy, and
      // the panel describes the EFFECTIVE behaviour from it.
      simple.refresh();
      status.refresh();
    } catch (error) {
      setSwitchError(
        error instanceof Error
          ? error.message
          : "The automatic updates setting could not be changed.",
      );
    } finally {
      setSwitching(false);
    }
  };

  // Every fact below comes from a server response. Nothing here decides
  // whether a container may be updated; see web/src/api/firstRun.ts.
  const facts: FirstRunFacts = {
    features: health.data?.features
      ? {
          planner: health.data.features.planner,
          automation: health.data.features.automation,
        }
      : null,
    // "0 means none yet" -- the repository's own words for this field. Not
    // inferred from a container count, which is legitimately zero on an empty
    // host.
    inventoryEstablished: (inventory.data?.generation ?? 0) > 0,
    assessed: Boolean(plans.data?.planner?.lastRunAt),
    policies: engine?.policies ?? 0,
    actingPolicies: engine?.actingPolicies ?? 0,
    pausedContainers: engine?.pausedContainers ?? 0,
    // Grouping decisions the SERVER produced, not re-deciding them.
    manualReviews: (upcoming.data?.items ?? []).filter(
      (decision) => decision.recommendation === "manualReview",
    ).length,
    eligible: upcoming.data?.eligible ?? 0,
    // A preview that did not answer is not a count of zero.
    readinessKnown: upcoming.status === "ready" && !upcoming.error,
  };

  /*
   * Which first-run states the status badge above already answers.
   *
   * `active` and `nothingEligible` differ from each other only in the
   * eligibility sentence, which is carried across as `settledNote`.
   * `needsAttention` adds nothing at all: AutomationAttention below names every
   * waiting thing AND links to the queue that clears it, which the onboarding
   * block did not.
   */
  const firstRun = describeFirstRun(facts);
  const settled =
    firstRun === "active" ||
    firstRun === "nothingEligible" ||
    firstRun === "needsAttention";
  const settledNote =
    firstRun === "active" || firstRun === "nothingEligible"
      ? firstRunExplanation(firstRun, facts)
      : undefined;

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

      {/*
        The engine's state, said once.

        Before this batch the page opened with three consecutive statements of
        the same fact: the onboarding heading ("Automatic updates are active"),
        the warning banner ("The update engine is on"), and then this section's
        own heading and badge. An operator read three paragraphs to learn one
        thing, and the actionable content began below the fold.

        Now the status section is first and carries the badge, the engine's own
        detail sentence, the one extra sentence a settled state earns, and the
        safety warning -- which is NOT shortened, because the thing it warns
        about has not changed.
      */}
      {/*
        The switch comes FIRST, because for most homelab operators it is the
        whole page: turn it on and stop reading. Everything below is the
        engine's own reporting, which matters once something has happened.
      */}
      {simple.data ? (
        <SimpleUpdatesPanel
          state={simple.data}
          mayManage={mayManage}
          busy={switching}
          error={switchError}
          onEnable={() => void flipSimpleUpdates(true)}
          onDisable={() => void flipSimpleUpdates(false)}
        />
      ) : null}

      <AutomationSummary state={status} note={settledNote}>
        <AutomationWarningNotice enabled={Boolean(engine?.enabled)} />
      </AutomationSummary>

      <SelfUpdateNotice self={engine?.self} />

      {/*
        Onboarding, only while it has an instruction to give.

        Its purpose is getting a new deployment to a working automation setup,
        and in the three SETTLED states it had nothing left to say that the
        badge above and the attention panel below do not say better. Every
        unsettled state -- starting up, assessing, engine off, no policy,
        observing, or unable to answer -- still renders it in full, including
        its capability checklist and its actions.
      */}
      {settled ? null : (
        <AutomationOnboarding
          facts={facts}
          features={health.data?.features ?? null}
          requiredCapabilities={engine?.requiredCapabilities ?? []}
          mayManagePolicies={mayManage}
        />
      )}

      {/* Only when something is genuinely waiting. */}
      <AutomationAttention
        engine={engine}
        pausedCount={pauses.data?.items?.length ?? 0}
        manualReviews={facts.manualReviews}
      />

      {/* What it is configured to do, in the terms it was configured in. */}
      <AutomationSettings
        policies={policies.data?.items ?? []}
        mayManage={mayManage}
      />

      {/*
        The other half of "what is configured". A policy describes the estate
        only until a container is given its own behaviour, and until now nothing
        on this page said which containers had been. READ-ONLY: every row links
        to the container's page, which stays the one place a behaviour is
        chosen.
      */}
      <ContainerBehaviorOverrides state={containerBehaviors} />

      {/* Held containers, using the paused page's own confirmed resume rather
          than a second implementation of it. */}
      <PausedSection state={pauses} />

      <AutomationOrder
        dependencies={dependencies.data ?? undefined}
        mayManage={Boolean(session.user?.permissions.includes("dependency:manage"))}
      />

      <section
        aria-labelledby="automation-pass-heading"
        className="flex flex-col gap-4 rounded-xl border border-border-subtle bg-surface-raised p-5"
      >
        <h2 id="automation-pass-heading" className="text-base font-semibold">
          Next automation pass
        </h2>

        <MaintenanceWindowState engine={engine} />

        <PassControls
          enabled={Boolean(engine?.enabled)}
          running={Boolean(engine?.running)}
          onRan={() => {
            status.refresh();
            runs.refresh();
            upcoming.refresh();
            pauses.refresh();
          }}
        />

        <UpcomingPanel state={upcoming} />
      </section>

      <details className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <summary className="cursor-pointer text-sm font-medium text-content-muted">
          Recent automation passes
        </summary>
        <div className="mt-4">
          <RecentRuns state={runs} />
        </div>
      </details>
    </div>
  );
}

// ---------------------------------------------------------------- status --

/**
 * Paused containers, where an operator looks for them.
 *
 * Rendered only when something is held: a permanent empty panel is a place
 * people learn to skip. The card is the paused page's own -- same
 * confirmation, same permission, same wording that resuming retries nothing --
 * so there is one resume flow rather than two.
 */
function PausedSection({
  state,
}: {
  state: ReturnType<typeof useAutomationPauses>;
}) {
  const items = state.data?.items ?? [];
  if (items.length === 0) return null;

  return (
    <section
      aria-labelledby="automation-paused-heading"
      data-testid="automation-paused"
      className="flex flex-col gap-3 rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 id="automation-paused-heading" className="text-base font-semibold">
          Paused containers
        </h2>
        <Link to="/automation/paused" className="text-sm text-accent hover:underline">
          Full paused list
        </Link>
      </div>
      <p className="max-w-prose text-sm text-content-muted">
        {items.length}{" "}
        {items.length === 1 ? "container needs" : "containers need"} attention. A
        pause does not clear itself.
      </p>
      <ul className="flex flex-col gap-3">
        {items.map((pause) => (
          <li key={pause.containerName}>
            <PauseCard pause={pause} onChanged={state.refresh} />
          </li>
        ))}
      </ul>
    </section>
  );
}

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
/**
 * Re-exported so pages importing it from here keep working.
 *
 * The definition moved to api/presentation.ts, where four
 * near-identical copies were collapsed into one.
 */
export { formatMoment };

