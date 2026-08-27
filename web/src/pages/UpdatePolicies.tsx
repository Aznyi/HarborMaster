import { useCallback, useId, useMemo, useState } from "react";

import type {
  AutomationMode,
  ScopeChoice,
  UpdatePolicy,
  UpdatePolicyRequest,
  UpdateSelector,
  UpdateStrategy,
} from "../api/automationTypes";
import type { Recommendation } from "../api/planTypes";
import {
  AUTOMATION_PRESETS,
  PRESET_DESCRIPTIONS,
  PRESET_LABELS,
  compilePreset,
  describeRecommendationFloor,
  detectPreset,
  presetConsequence,
  type AutomationPreset,
} from "../api/automationPresets";
import {
  AUTOMATION_MODE_ORDER,
  SCOPE_CHOICE_DESCRIPTIONS,
  SCOPE_CHOICE_LABELS,
  SCOPE_CHOICE_ORDER,
  UPDATE_STRATEGY_DESCRIPTIONS,
  UPDATE_STRATEGY_LABELS,
  UPDATE_STRATEGY_ORDER,
  choiceOfPolicy,
  scopeOfChoice,
} from "../api/automationTypes";
import {
  describeCeiling,
  describeSchedule,
  describeScope,
  summariseUpdatePolicy,
} from "../api/updatePolicySummary";
import {
  AutomationModeBadge,
  AutomationWarningNotice,
  UpdateStrategyBadge,
} from "../components/AutomationBadges";
import { ContainerPicker } from "../components/ContainerPicker";
import { PageIntro } from "../components/PageIntro";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import {
  useArchiveUpdatePolicy,
  useAutomationStatus,
  useCreateUpdatePolicy,
  useUpdatePolicies,
  useUpdateUpdatePolicy,
} from "../hooks/useAutomation";
import { useSession } from "../hooks/useSession";

/**
 * Update policies: the rules that let HarborMaster change containers
 * unattended.
 *
 * # Deliberately not the Policies page
 *
 * A compliance policy REPORTS. This one ACTS. Sharing a page would invite an
 * operator to read one as the other, and the consequence of that confusion is
 * somebody believing a rule is checking their estate when it is changing it.
 *
 * # The editor is organised around questions, not around the stored object
 *
 * The form used to mirror the shape of `UpdatePolicy`: a selector section, a
 * limits section, a failure-handling section. That is the right shape for a
 * database row and the wrong one for a person, who is answering five questions:
 *
 *	1. What should HarborMaster manage?
 *	2. How far may updates go?
 *	3. How should updates happen?
 *	4. When may updates happen?
 *	5. What happens if an update fails?
 *
 * Everything that does not answer one of those is behind Advanced settings —
 * with one rule about what may go there: **nothing whose omission changes how
 * much the host can be changed.** Priority and the manual selector fields
 * qualify. The mode, the ceiling, the window, and the failure plan never can.
 */
export function UpdatePolicies() {
  const [includeArchived, setIncludeArchived] = useState(false);
  const [editing, setEditing] = useState<UpdatePolicy | null>(null);
  const [creating, setCreating] = useState(false);
  const [warnings, setWarnings] = useState<string[]>([]);

  const status = useAutomationStatus();
  const policies = useUpdatePolicies({ page: 1, pageSize: 50, includeArchived });

  const session = useSession();
  const mayManage = Boolean(
    session.user?.permissions.includes("automation:manage"),
  );

  const refresh = useCallback(() => {
    policies.refresh();
    status.refresh();
  }, [policies, status]);

  return (
    <div className="space-y-6">
      <PageIntro
        title="Update policies"
        description={
          "Which containers HarborMaster may update on its own, how far, and " +
          "when. A policy names containers and a ceiling; what image a matched " +
          "container moves to is the planner's decision, not the policy's."
        }
      />

      <AutomationWarningNotice enabled={Boolean(status.data?.status.enabled)} />

      {warnings.length > 0 ? (
        <div
          role="status"
          className="space-y-1 rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-sm text-warn"
        >
          <p className="font-medium">Saved. Worth knowing:</p>
          <ul className="list-disc space-y-1 pl-5">
            {warnings.map((warning) => (
              <li key={warning}>{warning}</li>
            ))}
          </ul>
        </div>
      ) : null}

      <div className="flex flex-wrap items-center justify-between gap-2">
        <label className="flex items-center gap-2 text-sm text-content-muted">
          <input
            type="checkbox"
            className="h-6 w-6 shrink-0 rounded border-border-subtle"
            checked={includeArchived}
            onChange={(event) => setIncludeArchived(event.target.checked)}
          />
          Show withdrawn policies
        </label>

        {mayManage ? (
          <button
            type="button"
            className="rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium"
            onClick={() => {
              setCreating(true);
              setEditing(null);
              setWarnings([]);
            }}
          >
            New policy
          </button>
        ) : null}
      </div>

      {creating || editing ? (
        <PolicyEditor
          key={editing?.policyId ?? "new"}
          policy={editing}
          engineEnabled={Boolean(status.data?.status.enabled)}
          onCancel={() => {
            setCreating(false);
            setEditing(null);
          }}
          onSaved={(saved) => {
            setCreating(false);
            setEditing(null);
            setWarnings(saved);
            refresh();
          }}
        />
      ) : null}

      <PolicyList
        state={policies}
        mayManage={mayManage}
        onEdit={(policy) => {
          setEditing(policy);
          setCreating(false);
          setWarnings([]);
        }}
        onChanged={refresh}
      />
    </div>
  );
}

// ------------------------------------------------------------------ list --

function PolicyList({
  state,
  mayManage,
  onEdit,
  onChanged,
}: {
  state: ReturnType<typeof useUpdatePolicies>;
  mayManage: boolean;
  onEdit: (policy: UpdatePolicy) => void;
  onChanged: () => void;
}) {
  if (state.status === "loading") return <LoadingState label="Loading policies" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) return <ErrorState error={state.error} onRetry={state.refresh} />;

  const items = state.data?.items ?? [];
  if (items.length === 0) {
    return (
      <EmptyState
        title="No update policies"
        description={
          "Nothing will be updated automatically. Start with a policy in " +
          "Observe mode: it evaluates everything and changes nothing."
        }
      />
    );
  }

  return (
    <div className="space-y-3">
      {items.map((policy) => (
        <PolicyCard
          key={policy.policyId}
          policy={policy}
          mayManage={mayManage}
          onEdit={() => onEdit(policy)}
          onChanged={onChanged}
        />
      ))}
    </div>
  );
}

/**
 * One policy, readable without opening the editor.
 *
 * The five answers an operator is looking for — what it covers, what it may
 * do, how far, when, and what happens on failure — plus the summary sentence,
 * which is generated by the same function the editor previews with.
 */
function PolicyCard({
  policy,
  mayManage,
  onEdit,
  onChanged,
}: {
  policy: UpdatePolicy;
  mayManage: boolean;
  onEdit: () => void;
  onChanged: () => void;
}) {
  const archive = useArchiveUpdatePolicy();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);

  const withdraw = useCallback(async () => {
    setBusy(true);
    try {
      await archive(policy.policyId);
      setConfirming(false);
      onChanged();
    } finally {
      setBusy(false);
    }
  }, [archive, onChanged, policy.policyId]);

  const broad = policy.scope === "allEligible";

  return (
    <article className="space-y-3 rounded-xl border border-border-subtle bg-surface-raised px-4 py-3">
      <header className="flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0">
          <h3 className="text-sm font-semibold break-words">
            {policy.name}
            {policy.archived ? (
              <span className="ml-2 text-xs font-normal text-content-muted">
                (withdrawn)
              </span>
            ) : null}
            {!policy.enabled && !policy.archived ? (
              <span className="ml-2 text-xs font-normal text-content-muted">
                (switched off)
              </span>
            ) : null}
          </h3>
          {policy.description ? (
            <p className="mt-1 max-w-prose text-sm text-content-muted">
              {policy.description}
            </p>
          ) : null}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {broad ? (
            <span className="rounded-full border border-border-subtle px-2 py-0.5 text-xs">
              All eligible
            </span>
          ) : null}
          <AutomationModeBadge mode={policy.mode} />
          <UpdateStrategyBadge strategy={policy.strategy} />
        </div>
      </header>

      <p className="max-w-prose text-sm">{summariseUpdatePolicy(policy)}</p>

      <dl className="grid gap-2 text-sm sm:grid-cols-2">
        <Detail label="Scope" value={capitalise(describeScope(policy))} />
        <Detail label="Allowed updates" value={capitalise(describeCeiling(policy.strategy))} />
        <Detail label="Window" value={capitalise(describeSchedule(policy.window))} />
        <Detail label="On failure" value={describeCardFailure(policy)} />
        {(policy.selector?.exclude ?? []).length > 0 ? (
          <Detail
            label="Never touched"
            value={(policy.selector.exclude ?? []).join(", ")}
          />
        ) : null}
      </dl>

      {mayManage && !policy.archived ? (
        <div className="flex flex-wrap gap-2">
          <button
            type="button"
            className="min-h-9 rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs font-medium"
            onClick={onEdit}
          >
            Edit
          </button>
          {confirming ? (
            <>
              <button
                type="button"
                className="min-h-9 rounded-lg border border-danger/40 bg-danger-soft px-2.5 py-1 text-xs font-medium text-danger disabled:opacity-50"
                disabled={busy}
                onClick={() => void withdraw()}
              >
                {busy ? "Withdrawing…" : "Yes, withdraw it"}
              </button>
              <button
                type="button"
                className="min-h-9 rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs"
                disabled={busy}
                onClick={() => setConfirming(false)}
              >
                Cancel
              </button>
            </>
          ) : (
            <button
              type="button"
              className="min-h-9 rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs"
              onClick={() => setConfirming(true)}
            >
              Withdraw
            </button>
          )}
        </div>
      ) : null}
    </article>
  );
}

function Detail({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs uppercase tracking-wide text-content-muted">{label}</dt>
      <dd className="mt-0.5 break-words">{value}</dd>
    </div>
  );
}

/** The failure line on a card, in the same words the editor uses. */
function describeCardFailure(policy: UpdatePolicy): string {
  if (policy.mode !== "automatic" && policy.mode !== "approvalRequired") {
    return "Nothing to fail; this policy changes nothing";
  }
  const pauseAfter = policy.failure?.pauseAfterFailures ?? 0;
  const rollback = policy.failure?.autoRollback
    ? "Roll back and pause"
    : "Stop and require review";
  return pauseAfter > 0
    ? `${rollback}, pause after ${pauseAfter}`
    : `${rollback}, never pause on repeats`;
}

function capitalise(value: string): string {
  if (!value) return value;
  return value.charAt(0).toUpperCase() + value.slice(1);
}

// ---------------------------------------------------------------- editor --

/**
 * The policy editor.
 *
 * # One request object, built once
 *
 * `buildRequest` is the only place form state becomes a policy. The summary
 * renders that object and the save sends it, so the sentence an operator reads
 * before saving is a rendering of the actual request rather than a second
 * opinion about it. See api/updatePolicySummary.ts.
 */
function PolicyEditor({
  policy,
  engineEnabled,
  onCancel,
  onSaved,
}: {
  policy: UpdatePolicy | null;
  engineEnabled: boolean;
  onCancel: () => void;
  onSaved: (warnings: string[]) => void;
}) {
  const create = useCreateUpdatePolicy();
  const update = useUpdateUpdatePolicy();

  const [name, setName] = useState(policy?.name ?? "");
  const [description, setDescription] = useState(policy?.description ?? "");

  // 1. What should HarborMaster manage?
  const [choice, setChoice] = useState<ScopeChoice>(
    policy ? choiceOfPolicy(policy) : "containers",
  );
  const [containers, setContainers] = useState<string[]>(
    policy?.selector.include ?? [],
  );
  const [images, setImages] = useState((policy?.selector.images ?? []).join(", "));
  const [labels, setLabels] = useState(
    Object.entries(policy?.selector.labels ?? {})
      .map(([key, value]) => (value ? `${key}=${value}` : key))
      .join(", "),
  );
  const [exclude, setExclude] = useState(
    (policy?.selector.exclude ?? []).join(", "),
  );

  // 2. How far may updates go?
  const [strategy, setStrategy] = useState<UpdateStrategy>(
    policy?.strategy ?? "digestOnly",
  );

  // 3. How should updates happen?
  const [mode, setMode] = useState<AutomationMode>(policy?.mode ?? "observe");

  // The recommendation floor.
  //
  // Previously the editor never sent this and the create handler stored
  // `proceed`. It is state now because a preset writes it and Custom exposes
  // it, and sending it explicitly makes the stored policy match what the
  // summary said rather than what the server chose.
  const [floor, setFloor] = useState<Recommendation>(
    policy?.minimumRecommendation ?? "proceed",
  );

  // 4. When may updates happen?
  const [alwaysOpen, setAlwaysOpen] = useState(
    policy?.window.alwaysOpen ?? true,
  );
  const [timezone, setTimezone] = useState(policy?.window.timezone ?? "UTC");
  const [start, setStart] = useState(policy?.window.start ?? "02:00");
  const [end, setEnd] = useState(policy?.window.end ?? "04:00");

  // 5. What happens if an update fails?
  const [autoRollback, setAutoRollback] = useState(
    policy?.failure?.autoRollback ?? true,
  );
  const [pauseEnabled, setPauseEnabled] = useState(
    (policy?.failure?.pauseAfterFailures ?? 2) > 0,
  );
  const [pauseAfter, setPauseAfter] = useState(
    String(policy?.failure?.pauseAfterFailures || 2),
  );

  // Advanced.
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const [priority, setPriority] = useState(String(policy?.priority ?? 0));

  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);

  /**
   * The request, and the only interpretation of the form there is.
   *
   * Note what each choice contributes and, more importantly, what it does NOT:
   * a selector clause belonging to a choice the operator is not on is never
   * sent. Sending one would be storing a rule they cannot see, and under
   * `allEligible` the server refuses the combination outright.
   */
  const request: UpdatePolicyRequest = useMemo(() => {
    const selector: UpdateSelector = {};
    if (choice === "containers") {
      selector.include = containers;
    }
    if (choice === "images") {
      selector.images = splitList(images);
    }
    if (choice === "advanced") {
      selector.include = containers;
      selector.images = splitList(images);
      selector.labels = parseLabels(labels);
    }
    // Exclusions apply in every scope, including the broad one.
    const excluded = splitList(exclude);
    if (excluded.length > 0) selector.exclude = excluded;

    return {
      name: name.trim(),
      description: description.trim(),
      priority: Number(priority) || 0,
      scope: scopeOfChoice(choice),
      selector,
      strategy,
      mode,
      minimumRecommendation: floor,
      window: alwaysOpen
        ? { alwaysOpen: true }
        : { alwaysOpen: false, timezone: timezone.trim(), start, end },
      failure: {
        autoRollback,
        pauseAfterFailures: pauseEnabled ? Number(pauseAfter) || 0 : 0,
      },
    };
  }, [
    alwaysOpen,
    autoRollback,
    choice,
    containers,
    description,
    end,
    exclude,
    floor,
    images,
    labels,
    mode,
    name,
    pauseAfter,
    pauseEnabled,
    priority,
    start,
    strategy,
    timezone,
  ]);

  /**
   * Which preset the operator is on.
   *
   * # Why this is explicit state and not purely derived
   *
   * The obvious implementation is `detectPreset(request)`, recomputed on every
   * keystroke. It is wrong, and the failing test that found it is worth
   * recording: with detection as the only source, **Custom is unreachable**.
   * Choosing Custom writes no fields -- that is its whole point -- so the next
   * render detects the preset the fields still match and snaps the selection
   * straight back. An operator could never open the individual controls.
   *
   * So the choice is remembered, and detection is used for the one thing it is
   * genuinely for: deciding what to select when the form OPENS. An existing
   * policy that exactly matches a preset opens on that preset; anything else
   * opens on Custom.
   *
   * Within a session the explicit choice then wins. Editing a ceiling inside
   * Custom until it happens to equal Follow current tag does not yank the
   * operator out of Custom, which would move the controls out from under them
   * mid-edit.
   */
  const [preset, setPreset] = useState<AutomationPreset>(() =>
    policy
      ? detectPreset({
          strategy: policy.strategy,
          mode: policy.mode,
          minimumRecommendation: policy.minimumRecommendation,
          failure: policy.failure,
        })
      : "observe",
  );

  /**
   * Choosing an outcome writes the fields that outcome owns.
   *
   * Everything else -- name, scope, selector, exclusions, window, priority,
   * pause setting -- is untouched, so switching presets never discards the
   * targeting an operator has already done. Custom writes nothing at all: it
   * reveals the controls holding whatever the last preset produced, which is
   * what makes "Follow current tag -> Custom" show a digest-only automatic
   * policy rather than an empty form.
   */
  const choosePreset = useCallback(
    (next: AutomationPreset) => {
      setPreset(next);
      const compiled = compilePreset(next, request);
      setStrategy(compiled.strategy as UpdateStrategy);
      setMode(compiled.mode as AutomationMode);
      setFloor(compiled.minimumRecommendation as Recommendation);
      setAutoRollback(compiled.failure?.autoRollback ?? true);
    },
    [request],
  );

  /**
   * Editing an owned field by hand re-derives which preset the form is on.
   *
   * This is the other half of the explicit-state decision above. Detection runs
   * HERE -- on an actual edit -- rather than on every render, so an operator who
   * clicked Custom and changed nothing stays in Custom, while one who widened
   * the ceiling under Follow current tag is moved to Custom because that is now
   * what their policy is.
   *
   * The three wrappers exist because each has to re-derive against the value
   * that is about to be set rather than the one currently in state, which React
   * has not applied yet.
   */
  const rederive = useCallback(
    (overrides: Partial<UpdatePolicyRequest>) => {
      setPreset(detectPreset({ ...request, ...overrides }));
    },
    [request],
  );

  const editStrategy = useCallback(
    (next: UpdateStrategy) => {
      setStrategy(next);
      rederive({ strategy: next });
    },
    [rederive],
  );

  const editMode = useCallback(
    (next: AutomationMode) => {
      setMode(next);
      rederive({ mode: next });
    },
    [rederive],
  );

  const editFloor = useCallback(
    (next: Recommendation) => {
      setFloor(next);
      rederive({ minimumRecommendation: next });
    },
    [rederive],
  );

  const editAutoRollback = useCallback(
    (next: boolean) => {
      setAutoRollback(next);
      rederive({ failure: { ...request.failure, autoRollback: next } });
    },
    [rederive, request.failure],
  );

  const save = useCallback(async () => {
    setBusy(true);
    setFailure(null);
    try {
      const result = policy
        ? await update(policy.policyId, request)
        : await create(request);
      onSaved(result.warnings ?? []);
    } catch (error) {
      setFailure(
        error instanceof Error ? error.message : "The policy could not be saved",
      );
    } finally {
      setBusy(false);
    }
  }, [create, onSaved, policy, request, update]);

  return (
    <section className="space-y-5 rounded-xl border border-border-subtle bg-surface-raised px-4 py-4">
      <h3 className="text-sm font-semibold">
        {policy ? `Edit ${policy.name}` : "New update policy"}
      </h3>

      <Field label="Name">
        <input
          className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
          value={name}
          onChange={(event) => setName(event.target.value)}
          maxLength={120}
          required
        />
      </Field>

      <Field label="Description" hint="Optional.">
        <input
          className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
          value={description}
          onChange={(event) => setDescription(event.target.value)}
          maxLength={1000}
        />
      </Field>

      <PresetSection
        preset={preset}
        onPreset={choosePreset}
        engineEnabled={engineEnabled}
      />

      <ScopeSection
        choice={choice}
        onChoice={setChoice}
        containers={containers}
        onContainers={setContainers}
        images={images}
        onImages={setImages}
        labels={labels}
        onLabels={setLabels}
        exclude={exclude}
        onExclude={setExclude}
      />

      {/* The individual controls, ALWAYS visible.
        *
        * It was tempting to reveal these only in Custom, so a preset would read
        * as one clean choice. That is wrong here, and the rule it breaks is
        * written at the top of this file: nothing whose omission changes how
        * much the host can be changed may go behind a disclosure. The ceiling
        * and the mode are exactly that, and a preset naming them in friendly
        * words is not a substitute for showing an operator the setting they are
        * about to save.
        *
        * So a preset FILLS these in and they stay on screen. Changing one by
        * hand re-derives which preset the form is on, which is how an operator
        * ends up in Custom without having to know Custom exists.
        */}
      <CeilingSection strategy={strategy} onStrategy={editStrategy} />

      <ModeSection mode={mode} onMode={editMode} />

      <FloorSection floor={floor} onFloor={editFloor} />

      <WindowSection
        alwaysOpen={alwaysOpen}
        onAlwaysOpen={setAlwaysOpen}
        timezone={timezone}
        onTimezone={setTimezone}
        start={start}
        onStart={setStart}
        end={end}
        onEnd={setEnd}
      />

      <FailureSection
        mode={mode}
        autoRollback={autoRollback}
        onAutoRollback={editAutoRollback}
        pauseEnabled={pauseEnabled}
        onPauseEnabled={setPauseEnabled}
        pauseAfter={pauseAfter}
        onPauseAfter={setPauseAfter}
      />

      {/* Advanced: only settings whose omission cannot change host safety. */}
      <details
        className="rounded-lg border border-border-subtle px-3 py-2"
        open={advancedOpen}
        onToggle={(event) => setAdvancedOpen(event.currentTarget.open)}
      >
        <summary className="min-h-9 cursor-pointer py-1 text-sm font-medium">
          Advanced settings
        </summary>
        <div className="space-y-3 pt-3">
          <Field
            label="Priority"
            hint={
              "Only matters when two policies select the same container. The " +
              "higher number wins. At the same number a policy that names the " +
              "container beats one that covers all eligible containers, so a " +
              "catch-all does not take over the rules you wrote."
            }
          >
            <input
              className="min-h-11 w-32 rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
              value={priority}
              onChange={(event) => setPriority(event.target.value)}
              inputMode="numeric"
            />
          </Field>
        </div>
      </details>

      {/* The summary: the request that is about to be sent, in words. */}
      <div className="rounded-lg border border-accent/40 bg-accent-soft px-3 py-2">
        <h4 className="text-xs uppercase tracking-wide text-content-muted">
          What this policy does
        </h4>
        <p className="mt-1 max-w-prose text-sm" data-testid="policy-summary">
          {summariseUpdatePolicy(request)}
        </p>

        {/* The outcome, in the operator's terms rather than the schema's.
          *
          * Rendered from the SAME request object that is about to be sent, via
          * the preset it currently matches -- so there is no second model of
          * what the policy means for this to drift away from.
          */}
        {presetConsequence(preset).length > 0 ? (
          <ul
            className="mt-2 max-w-prose list-disc space-y-1 pl-5 text-sm"
            data-testid="preset-consequence"
          >
            {presetConsequence(preset).map((sentence) => (
              <li key={sentence}>{sentence}</li>
            ))}
          </ul>
        ) : null}

        {/* The recommendation floor, never hidden. */}
        <p className="mt-2 max-w-prose text-xs text-content-muted">
          {describeRecommendationFloor(request.minimumRecommendation)}
        </p>

        {mode === "automatic" && !engineEnabled ? (
          <p
            className="mt-2 max-w-prose text-xs font-medium text-warning"
            data-testid="engine-disabled-warning"
          >
            This policy will not run automatically until the automation engine
            is enabled for this HarborMaster installation. It can be saved now,
            and it will be evaluated and recorded as soon as the engine is
            switched on.
          </p>
        ) : null}
      </div>

      {failure ? (
        <p role="alert" className="text-sm text-danger">
          {failure}
        </p>
      ) : null}

      <div className="flex flex-wrap gap-2">
        <button
          type="button"
          className="min-h-11 rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium disabled:opacity-50"
          disabled={busy}
          onClick={() => void save()}
        >
          {busy ? "Saving…" : "Save policy"}
        </button>
        <button
          type="button"
          className="min-h-11 rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm"
          disabled={busy}
          onClick={onCancel}
        >
          Cancel
        </button>
      </div>
    </section>
  );
}

// ------------------------------------------------------- 0. what outcome --

/**
 * The outcome question, asked before anything else.
 *
 * A radio group in a real `fieldset` with a real `legend`, so the whole set is
 * announced as one choice rather than as five unrelated checkboxes, and so
 * arrow keys move between options the way a person expects a radio group to.
 *
 * The selected state is carried by the radio itself -- which is what a screen
 * reader and a keyboard user act on -- and the ring and background are
 * decoration on top of it. Nothing here is distinguished by colour alone.
 */
function PresetSection({
  preset,
  onPreset,
  engineEnabled,
}: {
  preset: AutomationPreset;
  onPreset: (preset: AutomationPreset) => void;
  engineEnabled: boolean;
}) {
  const group = useId();

  return (
    <fieldset className="space-y-2">
      <legend className="text-sm font-medium">
        What do you want HarborMaster to do?
      </legend>

      <div className="space-y-2">
        {AUTOMATION_PRESETS.map((option) => {
          const id = `${group}-${option}`;
          const describedBy = `${id}-description`;
          const selected = preset === option;

          return (
            <label
              key={option}
              htmlFor={id}
              className={
                "flex min-h-11 cursor-pointer gap-3 rounded-lg border px-3 py-2 " +
                (selected
                  ? "border-accent bg-accent-soft ring-1 ring-accent"
                  : "border-border-subtle bg-surface")
              }
            >
              <input
                type="radio"
                id={id}
                name={group}
                className="mt-1 size-4 shrink-0"
                value={option}
                checked={selected}
                aria-describedby={describedBy}
                onChange={() => onPreset(option)}
              />
              <span className="min-w-0">
                <span className="block text-sm font-medium">
                  {PRESET_LABELS[option]}
                  {/* Not colour-only: the selected option says so in words. */}
                  {selected ? (
                    <span className="ml-2 text-xs font-normal text-content-muted">
                      (selected)
                    </span>
                  ) : null}
                </span>
                <span
                  id={describedBy}
                  className="mt-0.5 block text-xs text-content-muted"
                >
                  {PRESET_DESCRIPTIONS[option]}
                </span>
              </span>
            </label>
          );
        })}
      </div>

      {/* Watchtower migration help, offered where the decision is made rather
        * than in documentation somebody would have to already know to look for.
        */}
      <details className="rounded-lg border border-border-subtle px-3 py-2">
        <summary className="min-h-9 cursor-pointer py-1 text-sm font-medium">
          Coming from Watchtower?
        </summary>
        <div className="space-y-2 pt-2 text-sm text-content-muted">
          <p className="max-w-prose">
            Choose <strong>Follow current tag</strong>. It is the closest match
            to what Watchtower does by default: each container stays on the tag
            you configured, and HarborMaster recreates it when that tag starts
            pointing at a different image.
          </p>
          <p className="max-w-prose">
            HarborMaster does more around that than Watchtower does, and it is
            worth knowing before you rely on it. Before replacing a container it
            captures a configuration snapshot, verifies the image it pulled is
            the image it planned, checks whether anything shares the container&apos;s
            network namespace, compares the replacement&apos;s configuration against
            the original, and proves the new container is healthy — rolling back
            if it is not.
          </p>
          <p className="max-w-prose">
            The practical difference is that HarborMaster will sometimes refuse
            an update Watchtower would have applied. When it does, the
            Automation page names the check that refused it.
          </p>
        </div>
      </details>

      {!engineEnabled ? (
        <p className="max-w-prose text-xs text-content-muted">
          The automation engine is switched off for this installation, so no
          policy runs automatically yet. Policies can still be written and
          evaluated in Observe mode. Enabling it is a deployment setting
          (<code>HARBORMASTER_AUTOMATION_ENABLED</code>) and takes effect on
          restart.
        </p>
      ) : null}
    </fieldset>
  );
}

// ------------------------------------------------------ 1. what to manage --

function ScopeSection({
  choice,
  onChoice,
  containers,
  onContainers,
  images,
  onImages,
  labels,
  onLabels,
  exclude,
  onExclude,
}: {
  choice: ScopeChoice;
  onChoice: (choice: ScopeChoice) => void;
  containers: string[];
  onContainers: (names: string[]) => void;
  images: string;
  onImages: (value: string) => void;
  labels: string;
  onLabels: (value: string) => void;
  exclude: string;
  onExclude: (value: string) => void;
}) {
  const group = useId();
  const excludeHint = useId();

  return (
    <fieldset className="space-y-3 rounded-lg border border-border-subtle px-3 py-3">
      <legend className="px-1 text-xs font-semibold uppercase tracking-wide">
        What should HarborMaster manage?
      </legend>

      <div className="space-y-2">
        {SCOPE_CHOICE_ORDER.map((option) => (
          <label
            key={option}
            className="flex min-h-11 items-start gap-2 text-sm"
          >
            <input
              type="radio"
              name={group}
              className="mt-0.5 h-5 w-5 shrink-0"
              checked={choice === option}
              onChange={() => onChoice(option)}
            />
            <span>
              <span className="font-medium">{SCOPE_CHOICE_LABELS[option]}</span>
              <span className="block text-content-muted">
                {SCOPE_CHOICE_DESCRIPTIONS[option]}
              </span>
            </span>
          </label>
        ))}
      </div>

      {choice === "allEligible" ? (
        <p className="rounded-lg border border-border-subtle px-3 py-2 text-sm text-content-muted">
          HarborMaster will never enrol its own container, the parked and
          quarantined containers it keeps as evidence from an earlier update, or
          anything carrying <code>io.harbormaster.enabled=false</code>. Choosing
          this widens what the policy looks at and changes nothing else: every
          container still passes the same checks.
        </p>
      ) : null}

      {choice === "containers" || choice === "advanced" ? (
        <div className="space-y-1">
          <span className="text-xs uppercase tracking-wide text-content-muted">
            Containers
          </span>
          <ContainerPicker selected={containers} onChange={onContainers} />
        </div>
      ) : null}

      {choice === "images" || choice === "advanced" ? (
        <Field
          label="Image patterns"
          hint="Glob, not regex. Comma separated. A bare * is refused — use All eligible containers instead."
        >
          <input
            className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
            value={images}
            onChange={(event) => onImages(event.target.value)}
            placeholder="ghcr.io/acme/*"
          />
        </Field>
      ) : null}

      {choice === "advanced" ? (
        <Field
          label="Labels"
          hint="Comma separated. key=value, or a bare key to match any value. Every listed label must be present."
        >
          <input
            className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
            value={labels}
            onChange={(event) => onLabels(event.target.value)}
            placeholder="tier=front, managed"
          />
        </Field>
      ) : null}

      {/*
        Exclusions are shown for EVERY choice, including the broad one, and are
        visually separated because their precedence is the thing most worth
        getting across: they are checked first and nothing overrides them.
      */}
      <div className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2">
        <label className="block space-y-1">
          <span className="text-xs font-semibold uppercase tracking-wide text-warn">
            Never touch these
          </span>
          <input
            className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
            value={exclude}
            onChange={(event) => onExclude(event.target.value)}
            placeholder="database, redis"
            aria-describedby={excludeHint}
          />
        </label>
        <p id={excludeHint} className="mt-1 text-xs text-warn">
          Comma separated container names. Exclusions are checked first and win
          over everything above — including All eligible containers.
        </p>
      </div>
    </fieldset>
  );
}

// -------------------------------------------------------- 2. how far --

function CeilingSection({
  strategy,
  onStrategy,
}: {
  strategy: UpdateStrategy;
  onStrategy: (strategy: UpdateStrategy) => void;
}) {
  const group = useId();
  const hint = useId();

  return (
    <fieldset className="space-y-3 rounded-lg border border-border-subtle px-3 py-3">
      <legend className="px-1 text-xs font-semibold uppercase tracking-wide">
        How far may updates go?
      </legend>

      <p id={hint} className="text-sm text-content-muted">
        This is a CEILING, not an instruction. “{UPDATE_STRATEGY_LABELS.minor}”
        means HarborMaster may apply same-tag, patch, or minor updates when the
        rest of the policy and every safety check permit them — never that it
        will force one.
      </p>

      <div className="space-y-2">
        {UPDATE_STRATEGY_ORDER.map((option) => (
          <label key={option} className="flex min-h-11 items-start gap-2 text-sm">
            <input
              type="radio"
              name={group}
              className="mt-0.5 h-5 w-5 shrink-0"
              checked={strategy === option}
              onChange={() => onStrategy(option)}
              aria-describedby={hint}
            />
            <span>
              <span className="font-medium">{UPDATE_STRATEGY_LABELS[option]}</span>
              <span className="block text-content-muted">
                {UPDATE_STRATEGY_DESCRIPTIONS[option]}
              </span>
            </span>
          </label>
        ))}
      </div>
    </fieldset>
  );
}

// ------------------------------------------------------------- 3. how --

/**
 * The mode, and the warning that belongs to the mode CURRENTLY CHOSEN.
 *
 * The page used to carry one permanent notice about what automatic mode can do.
 * An operator configuring an observe policy read a danger warning that did not
 * apply to them, which is how a warning stops being read at all. This one
 * changes with the choice, and only the automatic branch is styled as a danger.
 */
function ModeSection({
  mode,
  onMode,
}: {
  mode: AutomationMode;
  onMode: (mode: AutomationMode) => void;
}) {
  const group = useId();

  return (
    <fieldset className="space-y-3 rounded-lg border border-border-subtle px-3 py-3">
      <legend className="px-1 text-xs font-semibold uppercase tracking-wide">
        How should updates happen?
      </legend>

      <div className="space-y-2">
        {AUTOMATION_MODE_ORDER.map((option) => (
          <label key={option} className="flex min-h-11 items-start gap-2 text-sm">
            <input
              type="radio"
              name={group}
              className="mt-0.5 h-5 w-5 shrink-0"
              checked={mode === option}
              onChange={() => onMode(option)}
            />
            <span>
              <span className="font-medium">{MODE_HEADINGS[option]}</span>
              <span className="block text-content-muted">
                {MODE_CONSEQUENCES[option]}
              </span>
            </span>
          </label>
        ))}
      </div>

      {mode === "automatic" ? (
        <p
          role="status"
          className="rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm font-medium text-danger"
        >
          HarborMaster may stop and replace matching containers unattended,
          inside this policy&apos;s allowed window. It will never update its own
          container.
        </p>
      ) : (
        <p className="rounded-lg border border-border-subtle px-3 py-2 text-sm text-content-muted">
          {MODE_CONSEQUENCES[mode]}
        </p>
      )}
    </fieldset>
  );
}

/** The heading for each mode. Short enough to scan as a list of four. */
const MODE_HEADINGS: Record<AutomationMode, string> = {
  observe: "Observe only",
  dryRun: "Dry run",
  approvalRequired: "Require approval",
  automatic: "Automatic",
};

/** What each mode DOES, phrased as a consequence rather than a definition. */
const MODE_CONSEQUENCES: Record<AutomationMode, string> = {
  observe:
    "HarborMaster evaluates matching containers and changes nothing.",
  dryRun:
    "HarborMaster evaluates matching containers, records the order it would act in, and changes nothing.",
  approvalRequired:
    "HarborMaster prepares eligible updates but waits for a person before changing the container.",
  automatic:
    "HarborMaster may stop and replace matching containers unattended inside the policy's allowed window.",
};

// ---------------------------------------------- 3b. how sure must it be --

/**
 * The recommendation floor.
 *
 * Custom only, because it is the one field where the vocabulary genuinely
 * matters and there is no honest way to hide it: the planner rates every
 * individual update, and this says how good that rating has to be. Presets set
 * it to the stricter value and the summary states the consequence in words, so
 * an operator who never opens Custom is told what it means without being asked
 * to choose it.
 *
 * Only two values are offered. Validation refuses the rest by name -- a verdict
 * that asks for human review cannot be the threshold for acting without one.
 */
function FloorSection({
  floor,
  onFloor,
}: {
  floor: Recommendation;
  onFloor: (floor: Recommendation) => void;
}) {
  const group = useId();

  const options: { value: Recommendation; heading: string; detail: string }[] = [
    {
      value: "proceed",
      heading: "Only straightforward updates",
      detail:
        "HarborMaster acts only on updates it rates as straightforward. The stricter setting.",
    },
    {
      value: "proceedWithCaution",
      heading: "Also updates flagged for caution",
      detail:
        "HarborMaster also acts on updates it has flagged for caution — an unfamiliar publisher, " +
        "an imperfect snapshot, or unresolved drift.",
    },
  ];

  return (
    <fieldset className="space-y-2">
      <legend className="text-sm font-medium">
        How sure must HarborMaster be before it acts?
      </legend>

      {options.map((option) => {
        const id = `${group}-${option.value}`;
        const describedBy = `${id}-description`;
        const selected = floor === option.value;

        return (
          <label
            key={option.value}
            htmlFor={id}
            className={
              "flex min-h-11 cursor-pointer gap-3 rounded-lg border px-3 py-2 " +
              (selected
                ? "border-accent bg-accent-soft ring-1 ring-accent"
                : "border-border-subtle bg-surface")
            }
          >
            <input
              type="radio"
              id={id}
              name={group}
              className="mt-1 size-4 shrink-0"
              value={option.value}
              checked={selected}
              aria-describedby={describedBy}
              onChange={() => onFloor(option.value)}
            />
            <span className="min-w-0">
              <span className="block text-sm font-medium">
                {option.heading}
                {selected ? (
                  <span className="ml-2 text-xs font-normal text-content-muted">
                    (selected)
                  </span>
                ) : null}
              </span>
              <span
                id={describedBy}
                className="mt-0.5 block text-xs text-content-muted"
              >
                {option.detail}
              </span>
            </span>
          </label>
        );
      })}
    </fieldset>
  );
}

// ------------------------------------------------------------ 4. when --

function WindowSection({
  alwaysOpen,
  onAlwaysOpen,
  timezone,
  onTimezone,
  start,
  onStart,
  end,
  onEnd,
}: {
  alwaysOpen: boolean;
  onAlwaysOpen: (value: boolean) => void;
  timezone: string;
  onTimezone: (value: string) => void;
  start: string;
  onStart: (value: string) => void;
  end: string;
  onEnd: (value: string) => void;
}) {
  const group = useId();

  return (
    <fieldset className="space-y-3 rounded-lg border border-border-subtle px-3 py-3">
      <legend className="px-1 text-xs font-semibold uppercase tracking-wide">
        When may updates happen?
      </legend>

      <div className="space-y-2">
        <label className="flex min-h-11 items-start gap-2 text-sm">
          <input
            type="radio"
            name={group}
            className="mt-0.5 h-5 w-5 shrink-0"
            checked={alwaysOpen}
            onChange={() => onAlwaysOpen(true)}
          />
          <span>
            <span className="font-medium">Anytime</span>
            <span className="block text-content-muted">
              Whenever a pass runs, at any hour.
            </span>
          </span>
        </label>
        <label className="flex min-h-11 items-start gap-2 text-sm">
          <input
            type="radio"
            name={group}
            className="mt-0.5 h-5 w-5 shrink-0"
            checked={!alwaysOpen}
            onChange={() => onAlwaysOpen(false)}
          />
          <span>
            <span className="font-medium">Maintenance window</span>
            <span className="block text-content-muted">
              Only inside the hours below.
            </span>
          </span>
        </label>
      </div>

      {!alwaysOpen ? (
        <div className="grid gap-3 sm:grid-cols-3">
          <Field label="Timezone" hint="An IANA name. Comparisons happen in it, so daylight saving is handled.">
            <input
              className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
              value={timezone}
              onChange={(event) => onTimezone(event.target.value)}
              placeholder="Europe/London"
            />
          </Field>
          <Field label="From">
            <input
              type="time"
              className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
              value={start}
              onChange={(event) => onStart(event.target.value)}
            />
          </Field>
          <Field label="Until" hint="Earlier than “from” means it crosses midnight.">
            <input
              type="time"
              className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
              value={end}
              onChange={(event) => onEnd(event.target.value)}
            />
          </Field>
        </div>
      ) : null}
    </fieldset>
  );
}

// --------------------------------------------------------- 5. on failure --

/**
 * What happens when an update fails.
 *
 * Rendered as a decision rather than as fields, and only claims what the engine
 * actually does: an automatic rollback restores the previous container AND
 * pauses that workload afterwards, whatever the failure count says, because the
 * change was wrong and the host moved twice.
 */
function FailureSection({
  mode,
  autoRollback,
  onAutoRollback,
  pauseEnabled,
  onPauseEnabled,
  pauseAfter,
  onPauseAfter,
}: {
  mode: AutomationMode;
  autoRollback: boolean;
  onAutoRollback: (value: boolean) => void;
  pauseEnabled: boolean;
  onPauseEnabled: (value: boolean) => void;
  pauseAfter: string;
  onPauseAfter: (value: string) => void;
}) {
  const group = useId();
  const acts = mode === "automatic" || mode === "approvalRequired";

  return (
    <fieldset className="space-y-3 rounded-lg border border-border-subtle px-3 py-3">
      <legend className="px-1 text-xs font-semibold uppercase tracking-wide">
        What happens if an update fails?
      </legend>

      {!acts ? (
        <p className="text-sm text-content-muted">
          This policy changes nothing, so nothing can fail. These settings are
          saved and take effect if you move it to Require approval or Automatic.
        </p>
      ) : null}

      <p className="text-sm font-medium">If verification fails</p>
      <div className="space-y-2">
        <label className="flex min-h-11 items-start gap-2 text-sm">
          <input
            type="radio"
            name={group}
            className="mt-0.5 h-5 w-5 shrink-0"
            checked={!autoRollback}
            onChange={() => onAutoRollback(false)}
          />
          <span>
            <span className="font-medium">Stop and require operator review</span>
            <span className="block text-content-muted">
              The replacement is left in place and HarborMaster stops. It writes
              a recovery plan naming exactly what is on the host.
            </span>
          </span>
        </label>
        <label className="flex min-h-11 items-start gap-2 text-sm">
          <input
            type="radio"
            name={group}
            className="mt-0.5 h-5 w-5 shrink-0"
            checked={autoRollback}
            onChange={() => onAutoRollback(true)}
          />
          <span>
            <span className="font-medium">Roll back automatically</span>
            <span className="block text-content-muted">
              HarborMaster restores the container it replaced, then pauses
              automation for that workload until a person clears it. A rollback
              always pauses: the change was wrong and the host moved twice.
            </span>
          </span>
        </label>
      </div>

      <p className="text-sm font-medium">After repeated failures</p>
      <label className="flex min-h-11 flex-wrap items-center gap-2 text-sm">
        <input
          type="checkbox"
          className="h-6 w-6 shrink-0 rounded border-border-subtle"
          checked={pauseEnabled}
          onChange={(event) => onPauseEnabled(event.target.checked)}
        />
        <span>Pause automation after</span>
        <input
          className="min-h-11 w-20 rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm disabled:opacity-50"
          value={pauseAfter}
          onChange={(event) => onPauseAfter(event.target.value)}
          inputMode="numeric"
          disabled={!pauseEnabled}
          aria-label="Failures before automation pauses"
        />
        <span>failures</span>
      </label>
      {!pauseEnabled ? (
        <p className="text-sm text-warn">
          With this off, a container that fails every pass is retried every
          pass. Not recommended.
        </p>
      ) : null}
    </fieldset>
  );
}

// ----------------------------------------------------------------- bits --

function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <label className="block space-y-1">
      <span className="text-xs uppercase tracking-wide text-content-muted">
        {label}
      </span>
      {children}
      {hint ? <span className="block text-xs text-content-muted">{hint}</span> : null}
    </label>
  );
}

/** Splits a comma-separated field, dropping the empties. */
function splitList(value: string): string[] {
  return value
    .split(",")
    .map((entry) => entry.trim())
    .filter((entry) => entry.length > 0);
}

/**
 * Reads `key=value, key` pairs.
 *
 * A bare key means "present with any value", matching the selector's own rule.
 * Only the FIRST `=` splits, so a value containing one survives.
 */
function parseLabels(value: string): Record<string, string> {
  const labels: Record<string, string> = {};
  for (const entry of splitList(value)) {
    const separator = entry.indexOf("=");
    if (separator < 0) {
      labels[entry] = "";
      continue;
    }
    const key = entry.slice(0, separator).trim();
    if (key) labels[key] = entry.slice(separator + 1).trim();
  }
  return labels;
}
