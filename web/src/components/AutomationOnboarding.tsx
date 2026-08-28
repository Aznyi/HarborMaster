import { Link } from "react-router";

import {
  FIRST_RUN_HEADINGS,
  REQUIRED_FOR_AUTOMATION,
  describeFirstRun,
  firstRunExplanation,
  type FirstRunFacts,
  type FirstRunState,
} from "../api/firstRun";
import { PRESET_DESCRIPTIONS, PRESET_LABELS } from "../api/automationPresets";
import type { Features } from "../api/types";

/**
 * What HarborMaster is doing about updates, and what to do next.
 *
 * # The three questions it answers
 *
 *     What state is HarborMaster in?
 *     What should I do next?
 *     What happens if I do nothing?
 *
 * # Why the states are not collapsed
 *
 * A fresh installation has several ordinary states that all look like "nothing
 * is happening" from outside: the estate has not been assessed yet, the estate
 * was assessed and nothing needs doing, planning is switched off, the engine is
 * off, no policy exists, a policy only watches. Rendering any of them as
 * "0 eligible" or "automation off" sends an operator to change the wrong thing.
 *
 * Every count here comes from a server response. This component chooses which
 * sentence to show; it never decides whether a container may be updated.
 */
export function AutomationOnboarding({
  facts,
  features,
  requiredCapabilities,
  mayManagePolicies,
}: {
  facts: FirstRunFacts;
  /** From /health. Null when the capability report could not be read. */
  features: Features | null;
  /**
   * The capabilities an acting policy needs, decided by the SERVER.
   *
   * The set is what the process must START with once automation is on, which
   * is not the same as what any one policy uses -- HarborMaster refuses to boot
   * with automation enabled and rollback disabled, so a list that omitted
   * rollback would be instructions that stop it coming back. The rule lives in
   * Go; this component only looks up flags it already has.
   */
  requiredCapabilities: readonly string[];
  mayManagePolicies: boolean;
}) {
  const state = describeFirstRun(facts);

  return (
    <section
      aria-labelledby="automation-onboarding-heading"
      data-testid="automation-onboarding"
      data-state={state}
      className="space-y-3 rounded-xl border border-border-subtle bg-surface-raised px-4 py-4"
    >
      <h2 id="automation-onboarding-heading" className="text-base font-semibold">
        {FIRST_RUN_HEADINGS[state]}
      </h2>
      <p className="max-w-prose text-sm text-content">
        {firstRunExplanation(state, facts)}
      </p>

      <ProgressDetail state={state} facts={facts} />

      {state === "engineDisabled" || state === "assessmentUnavailable" ? (
        <CapabilityChecklist
          features={features}
          required={
            requiredCapabilities.length > 0
              ? requiredCapabilities
              : REQUIRED_FOR_AUTOMATION
          }
        />
      ) : null}

      {state === "noPolicy" ? <PresetInvitation may={mayManagePolicies} /> : null}

      <Actions state={state} facts={facts} may={mayManagePolicies} />
    </section>
  );
}

/**
 * The extra sentence some states earn.
 *
 * Only where it changes what an operator would do. A running or queued
 * assessment tells them the wait is progressing rather than stuck.
 */
function ProgressDetail({
  state,
  facts,
}: {
  state: FirstRunState;
  facts: FirstRunFacts;
}) {
  if (state === "needsAttention") {
    return (
      <ul className="space-y-1 text-sm" data-testid="onboarding-attention">
        {facts.pausedContainers > 0 ? (
          <li>
            {facts.pausedContainers === 1
              ? "1 container has automatic updates paused after failures."
              : `${facts.pausedContainers} containers have automatic updates paused after failures.`}
          </li>
        ) : null}
        {facts.manualReviews > 0 ? (
          <li>
            {facts.manualReviews === 1
              ? "1 update requires manual review before it can be applied."
              : `${facts.manualReviews} updates require manual review before they can be applied.`}
          </li>
        ) : null}
      </ul>
    );
  }
  return null;
}

/**
 * Which capabilities this deployment has, against what its policies need.
 *
 * State is carried in TEXT ("Enabled" / "Disabled") as well as in the mark, so
 * the distinction never depends on seeing a colour or a glyph.
 */
function CapabilityChecklist({
  features,
  required,
}: {
  features: Features | null;
  required: readonly string[];
}) {
  if (!features) {
    return (
      <p className="text-sm text-content-muted">
        HarborMaster could not read this deployment&rsquo;s capability settings,
        so it cannot say which are missing.
      </p>
    );
  }

  const known: Record<string, boolean> = {
    acquisition: features.acquisition,
    execution: features.execution,
    automation: features.automation,
    rollback: features.rollback,
    planner: features.planner,
  };
  const labels: Record<string, string> = {
    acquisition: "Image acquisition",
    execution: "Container recreation",
    automation: "Automation engine",
    rollback: "Automatic rollback",
    planner: "Update assessment",
  };

  return (
    <>
      <ul className="space-y-1 text-sm" data-testid="capability-checklist">
        {required.map((name) => {
          const on = known[name] ?? false;
          return (
            <li key={name} className="flex min-h-11 items-start gap-2">
              <span aria-hidden="true" className="w-4 shrink-0">
                {on ? "✓" : "✕"}
              </span>
              <span className="min-w-0">
                <span className="block">{labels[name] ?? name}</span>
                <span className="block text-xs text-content-muted">
                  {on ? "Enabled" : "Disabled"}
                </span>
              </span>
            </li>
          );
        })}
      </ul>
      <ConfigurationHelp missing={required.filter((name) => !known[name])} />
    </>
  );
}

/**
 * The exact settings, and nothing that could be mistaken for a control.
 *
 * These are startup environment variables. HarborMaster reads them once at
 * boot, so there is no button here and there cannot be one: a page that offered
 * to write them would be offering to write a file it does not own and to
 * restart a process it is running inside.
 */
function ConfigurationHelp({ missing }: { missing: string[] }) {
  if (missing.length === 0) return null;

  const lines = missing.map(
    (name) => `HARBORMASTER_${name.toUpperCase()}_ENABLED=true`,
  );

  return (
    <div className="space-y-1">
      <p className="text-sm text-content-muted">
        These are read from the environment when HarborMaster starts. Set them
        where this deployment keeps its configuration, then recreate the
        container for the change to take effect.
      </p>
      <pre
        // Labelled, so a screen reader announces what the block IS before
        // reading variable names out character by character.
        aria-label="Environment variables to enable the missing capabilities"
        tabIndex={0}
        className="overflow-x-auto rounded-lg border border-border-subtle bg-surface px-3 py-2 text-xs"
      >
        <code>{lines.join("\n")}</code>
      </pre>
    </div>
  );
}

/**
 * The Watchtower callout, shown where somebody is choosing for the first time.
 *
 * Names the preset from the canonical Stage 17.3 tables rather than restating
 * it, so the two cannot disagree about what it is called or what it does.
 */
function PresetInvitation({ may }: { may: boolean }) {
  return (
    <div className="space-y-2 rounded-lg border border-border-subtle bg-surface px-3 py-3">
      <p className="text-sm font-medium">Coming from Watchtower?</p>
      <p className="max-w-prose text-sm text-content-muted">
        Choose &ldquo;{PRESET_LABELS.followCurrentTag}&rdquo; if you want
        HarborMaster to stay on each container&rsquo;s configured image tag and
        recreate it when that tag points to a different digest.{" "}
        {PRESET_DESCRIPTIONS.followCurrentTag} HarborMaster also applies its
        normal snapshot, policy, dependency, verification and rollback safety
        checks, which Watchtower does not.
      </p>
      {!may ? (
        <p className="text-xs text-content-muted">
          Creating an update policy needs the automation management permission.
        </p>
      ) : null}
    </div>
  );
}

/** The one thing worth doing next, per state. */
function Actions({
  state,
  facts,
  may,
}: {
  state: FirstRunState;
  facts: FirstRunFacts;
  may: boolean;
}) {
  const link = (to: string, label: string) => (
    <Link
      key={to}
      to={to}
      className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
    >
      {label}
    </Link>
  );

  const actions: React.ReactNode[] = [];
  switch (state) {
    case "noPolicy":
      if (may) actions.push(link("/update-policies", "Create update policy"));
      break;
    case "observeOnly":
      actions.push(link("/update-policies", "Review policies"));
      break;
    case "needsAttention":
      if (facts.pausedContainers > 0) {
        actions.push(link("/automation/paused", "Review paused containers"));
      }
      if (facts.manualReviews > 0) {
        actions.push(link("/plans", "Review plans"));
      }
      break;
    case "nothingEligible":
    case "active":
      actions.push(link("/update-policies", "Review policies"));
      break;
    case "engineDisabled":
    case "assessmentUnavailable":
      actions.push(link("/settings", "View deployment settings"));
      break;
    default:
      // inventoryPending, assessmentPending and unknown are waiting on
      // HarborMaster. Offering an action would ask somebody to fix something
      // that is not broken.
      break;
  }

  if (actions.length === 0) return null;
  return <div className="flex flex-wrap gap-2">{actions}</div>;
}
