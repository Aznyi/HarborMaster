import { useCallback, useState } from "react";

import type {
  AutomationMode,
  UpdatePolicy,
  UpdatePolicyRequest,
  UpdateStrategy,
} from "../api/automationTypes";
import {
  AUTOMATION_MODE_DESCRIPTIONS,
  AUTOMATION_MODE_LABELS,
  AUTOMATION_MODE_ORDER,
  UPDATE_STRATEGY_DESCRIPTIONS,
  UPDATE_STRATEGY_LABELS,
  UPDATE_STRATEGY_ORDER,
  describeWindow,
  selectorIsEmpty,
} from "../api/automationTypes";
import {
  AutomationModeBadge,
  AutomationWarningNotice,
  UpdateStrategyBadge,
} from "../components/AutomationBadges";
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
 * # The editor's job
 *
 * To make the two dangerous settings impossible to set by accident. The mode
 * defaults to Observe, the strategy to Digest only, and each option carries the
 * sentence that says what it actually does. The server's warnings are shown
 * after a save, because a warning nobody reads is a warning that did nothing.
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
          policy={editing}
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

  return (
    <article className="space-y-3 rounded-xl border border-border-subtle bg-surface-raised px-4 py-3">
      <header className="flex flex-wrap items-start justify-between gap-2">
        <div>
          <h3 className="text-sm font-semibold">
            {policy.name}
            {policy.archived ? (
              <span className="ml-2 text-xs font-normal text-content-muted">
                (withdrawn)
              </span>
            ) : null}
            {!policy.enabled && !policy.archived ? (
              <span className="ml-2 text-xs font-normal text-content-muted">
                (disabled)
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
          <AutomationModeBadge mode={policy.mode} />
          <UpdateStrategyBadge strategy={policy.strategy} />
        </div>
      </header>

      <dl className="grid gap-2 text-sm sm:grid-cols-2">
        <Detail label="Selects" value={describeSelector(policy)} />
        <Detail label="Window" value={describeWindow(policy.window)} />
        <Detail label="Priority" value={String(policy.priority)} />
        <Detail
          label="On failure"
          value={
            policy.failure?.autoRollback
              ? "Roll back automatically, then pause"
              : "Leave the replacement for a person"
          }
        />
      </dl>

      {mayManage && !policy.archived ? (
        <div className="flex flex-wrap gap-2">
          <button
            type="button"
            className="rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs font-medium"
            onClick={onEdit}
          >
            Edit
          </button>
          {confirming ? (
            <>
              <button
                type="button"
                className="rounded-lg border border-danger/40 bg-danger-soft px-2.5 py-1 text-xs font-medium text-danger disabled:opacity-50"
                disabled={busy}
                onClick={() => void withdraw()}
              >
                {busy ? "Withdrawing…" : "Yes, withdraw it"}
              </button>
              <button
                type="button"
                className="rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs"
                disabled={busy}
                onClick={() => setConfirming(false)}
              >
                Cancel
              </button>
            </>
          ) : (
            <button
              type="button"
              className="rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs"
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
    <div>
      <dt className="text-xs uppercase tracking-wide text-content-muted">{label}</dt>
      <dd className="mt-0.5">{value}</dd>
    </div>
  );
}

/** Renders a selector in words, and says plainly when it governs nothing. */
function describeSelector(policy: UpdatePolicy): string {
  const selector = policy.selector;
  if (selectorIsEmpty(selector)) return "nothing (the selector is empty)";

  const parts: string[] = [];
  if (selector.include?.length) parts.push(`named ${selector.include.join(", ")}`);
  if (selector.images?.length) parts.push(`images matching ${selector.images.join(", ")}`);
  const labels = Object.entries(selector.labels ?? {});
  if (labels.length) {
    parts.push(
      `labelled ${labels.map(([key, value]) => (value ? `${key}=${value}` : key)).join(", ")}`,
    );
  }
  let rendered = parts.join("; or ");
  if (selector.exclude?.length) {
    rendered += ` — never ${selector.exclude.join(", ")}`;
  }
  return rendered;
}

// ---------------------------------------------------------------- editor --

/**
 * The policy editor.
 *
 * Every control that decides how dangerous the policy is carries the sentence
 * that says what it does, next to the control rather than in a help page. The
 * safe values are the defaults.
 */
function PolicyEditor({
  policy,
  onCancel,
  onSaved,
}: {
  policy: UpdatePolicy | null;
  onCancel: () => void;
  onSaved: (warnings: string[]) => void;
}) {
  const create = useCreateUpdatePolicy();
  const update = useUpdateUpdatePolicy();

  const [name, setName] = useState(policy?.name ?? "");
  const [description, setDescription] = useState(policy?.description ?? "");
  const [include, setInclude] = useState((policy?.selector.include ?? []).join(", "));
  const [images, setImages] = useState((policy?.selector.images ?? []).join(", "));
  const [exclude, setExclude] = useState((policy?.selector.exclude ?? []).join(", "));
  const [strategy, setStrategy] = useState<UpdateStrategy>(
    policy?.strategy ?? "digestOnly",
  );
  const [mode, setMode] = useState<AutomationMode>(policy?.mode ?? "observe");
  const [priority, setPriority] = useState(String(policy?.priority ?? 0));

  const [alwaysOpen, setAlwaysOpen] = useState(policy?.window.alwaysOpen ?? false);
  const [timezone, setTimezone] = useState(policy?.window.timezone ?? "UTC");
  const [start, setStart] = useState(policy?.window.start ?? "02:00");
  const [end, setEnd] = useState(policy?.window.end ?? "04:00");

  const [autoRollback, setAutoRollback] = useState(
    policy?.failure?.autoRollback ?? true,
  );
  const [pauseAfter, setPauseAfter] = useState(
    String(policy?.failure?.pauseAfterFailures ?? 2),
  );

  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);

  const save = useCallback(async () => {
    setBusy(true);
    setFailure(null);

    const body: UpdatePolicyRequest = {
      name: name.trim(),
      description: description.trim(),
      priority: Number(priority) || 0,
      selector: {
        include: splitList(include),
        images: splitList(images),
        exclude: splitList(exclude),
      },
      strategy,
      mode,
      window: alwaysOpen
        ? { alwaysOpen: true }
        : { alwaysOpen: false, timezone: timezone.trim(), start, end },
      failure: {
        autoRollback,
        pauseAfterFailures: Number(pauseAfter) || 0,
      },
    };

    try {
      const result = policy
        ? await update(policy.policyId, body)
        : await create(body);
      onSaved(result.warnings ?? []);
    } catch (error) {
      setFailure(
        error instanceof Error ? error.message : "The policy could not be saved",
      );
    } finally {
      setBusy(false);
    }
  }, [
    alwaysOpen,
    autoRollback,
    create,
    description,
    end,
    exclude,
    images,
    include,
    mode,
    name,
    onSaved,
    pauseAfter,
    policy,
    priority,
    start,
    strategy,
    timezone,
    update,
  ]);

  return (
    <section className="space-y-4 rounded-xl border border-border-subtle bg-surface-raised px-4 py-4">
      <h3 className="text-sm font-semibold">
        {policy ? `Edit ${policy.name}` : "New update policy"}
      </h3>

      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Name">
          <input
            className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
            value={name}
            onChange={(event) => setName(event.target.value)}
            maxLength={120}
          />
        </Field>
        <Field
          label="Priority"
          hint="Only matters when two policies select the same container; the higher number wins. Leave it at 0 otherwise."
        >
          <input
            className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
            value={priority}
            onChange={(event) => setPriority(event.target.value)}
            inputMode="numeric"
          />
        </Field>
      </div>

      <Field label="Description">
        <input
          className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
          value={description}
          onChange={(event) => setDescription(event.target.value)}
          maxLength={1000}
        />
      </Field>

      <fieldset className="space-y-3 rounded-lg border border-border-subtle px-3 py-3">
        <legend className="px-1 text-xs uppercase tracking-wide text-content-muted">
          Which containers
        </legend>
        <p className="text-sm text-content-muted">
          A policy with nothing here governs nothing. Exclusions are checked
          first and cannot be overridden.
        </p>
        <Field label="Container names" hint="Comma separated.">
          <input
            className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
            value={include}
            onChange={(event) => setInclude(event.target.value)}
            placeholder="web, api"
          />
        </Field>
        <Field label="Image patterns" hint="Glob, not regex. A bare * is refused.">
          <input
            className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
            value={images}
            onChange={(event) => setImages(event.target.value)}
            placeholder="ghcr.io/acme/*"
          />
        </Field>
        <Field
          label="Exclude containers"
          hint="Matched by the patterns above, but never updated by this policy."
        >
          <input
            className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
            value={exclude}
            onChange={(event) => setExclude(event.target.value)}
            placeholder="database"
          />
        </Field>
      </fieldset>

      <fieldset className="space-y-3 rounded-lg border border-border-subtle px-3 py-3">
        <legend className="px-1 text-xs uppercase tracking-wide text-content-muted">
          How far, and how much
        </legend>

        <Field label="Mode" hint={AUTOMATION_MODE_DESCRIPTIONS[mode]}>
          <select
            className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
            value={mode}
            onChange={(event) => setMode(event.target.value as AutomationMode)}
          >
            {AUTOMATION_MODE_ORDER.map((option) => (
              <option key={option} value={option}>
                {AUTOMATION_MODE_LABELS[option]}
              </option>
            ))}
          </select>
        </Field>

        <Field
          label="Allowed updates"
          hint={
            "How far may a version move? " +
            UPDATE_STRATEGY_DESCRIPTIONS[strategy]
          }
        >
          <select
            className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
            value={strategy}
            onChange={(event) => setStrategy(event.target.value as UpdateStrategy)}
          >
            {UPDATE_STRATEGY_ORDER.map((option) => (
              <option key={option} value={option}>
                {UPDATE_STRATEGY_LABELS[option]}
              </option>
            ))}
          </select>
        </Field>
      </fieldset>

      <fieldset className="space-y-3 rounded-lg border border-border-subtle px-3 py-3">
        <legend className="px-1 text-xs uppercase tracking-wide text-content-muted">
          When
        </legend>

        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            className="h-6 w-6 shrink-0 rounded border-border-subtle"
            checked={alwaysOpen}
            onChange={(event) => setAlwaysOpen(event.target.checked)}
          />
          At any time, with no maintenance window
        </label>
        <p className="text-sm text-content-muted">
          {alwaysOpen
            ? "This policy may act whenever a pass runs. There is no window to set."
            : "Updates happen only inside the window below, in the timezone you give."}
        </p>

        {!alwaysOpen ? (
          <div className="grid gap-3 sm:grid-cols-3">
            <Field label="Timezone" hint="An IANA name. Comparisons happen in it.">
              <input
                className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
                value={timezone}
                onChange={(event) => setTimezone(event.target.value)}
                placeholder="Europe/London"
              />
            </Field>
            <Field label="From">
              <input
                type="time"
                className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
                value={start}
                onChange={(event) => setStart(event.target.value)}
              />
            </Field>
            <Field label="Until" hint="Earlier than “from” means it crosses midnight.">
              <input
                type="time"
                className="min-h-11 w-full rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
                value={end}
                onChange={(event) => setEnd(event.target.value)}
              />
            </Field>
          </div>
        ) : null}
      </fieldset>

      <fieldset className="space-y-3 rounded-lg border border-border-subtle px-3 py-3">
        <legend className="px-1 text-xs uppercase tracking-wide text-content-muted">
          Failure handling
        </legend>

        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            className="h-6 w-6 shrink-0 rounded border-border-subtle"
            checked={autoRollback}
            onChange={(event) => setAutoRollback(event.target.checked)}
          />
          Roll the container back automatically if the update fails verification
        </label>
        <p className="text-sm text-content-muted">
          A rollback always pauses the container afterwards. The change was
          wrong and the host was moved twice; a person needs to look.
        </p>

        <Field
          label="Pause after this many failures"
          hint="Zero never pauses, which is not recommended."
        >
          <input
            className="min-h-11 w-32 rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
            value={pauseAfter}
            onChange={(event) => setPauseAfter(event.target.value)}
            inputMode="numeric"
          />
        </Field>
      </fieldset>

      {failure ? (
        <p role="alert" className="text-sm text-danger">
          {failure}
        </p>
      ) : null}

      <div className="flex gap-2">
        <button
          type="button"
          className="rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium disabled:opacity-50"
          disabled={busy}
          onClick={() => void save()}
        >
          {busy ? "Saving…" : "Save policy"}
        </button>
        <button
          type="button"
          className="rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm"
          disabled={busy}
          onClick={onCancel}
        >
          Cancel
        </button>
      </div>
    </section>
  );
}

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
