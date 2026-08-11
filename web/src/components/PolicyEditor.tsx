import { useMemo, useState } from "react";

import { ApiError } from "../api/client";
import type {
  PolicyDefinition,
  PolicyRequest,
  PolicyRule,
  PolicyRuleCatalogue,
  PolicyRuleSpec,
  PolicyRuleType,
  PolicySeverity,
} from "../api/policyTypes";
import { POLICY_SEVERITY_ORDER } from "../api/policyTypes";

/**
 * The policy editor.
 *
 * # It is built from the server's catalogue
 *
 * Every rule the editor offers comes from `GET /policy-rules`, which is the
 * same catalogue the validator checks against. A hardcoded list here would
 * eventually offer a rule the backend rejects, and the operator would find out
 * only after filling in the form.
 *
 * # It does not pretend to validate
 *
 * The obvious client-side checks are here so the common mistake is caught
 * before a round trip, but the SERVER is authoritative and its message is what
 * gets shown on rejection. Duplicating the full rule set in the browser would
 * create a second definition of "valid" that drifts from the first.
 *
 * # There is no enforcement control
 *
 * Saving a policy makes HarborMaster CHECK configuration against it. Nothing in
 * this form changes a container, and there is no endpoint that could.
 */

export interface PolicyEditorProps {
  catalogue: PolicyRuleCatalogue;
  /** The policy being edited, or undefined when creating a new one. */
  policy?: PolicyDefinition;
  onSubmit: (body: PolicyRequest) => Promise<void>;
  onCancel: () => void;
}

export function PolicyEditor({
  catalogue,
  policy,
  onSubmit,
  onCancel,
}: PolicyEditorProps) {
  const [name, setName] = useState(policy?.name ?? "");
  const [description, setDescription] = useState(policy?.description ?? "");
  const [severity, setSeverity] = useState<PolicySeverity>(
    policy?.severity ?? "high",
  );
  const [enabled, setEnabled] = useState(policy?.enabled ?? true);
  const [rules, setRules] = useState<PolicyRule[]>(policy?.rules ?? []);

  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const specs = useMemo(() => {
    const index = new Map<PolicyRuleType, PolicyRuleSpec>();
    for (const spec of catalogue.rules) index.set(spec.type, spec);
    return index;
  }, [catalogue.rules]);

  // A policy carries at most one rule of each type: a violation is identified
  // by (container, policy, rule type), so two rules of one type would compete
  // for the same history row. The picker therefore hides what is already used.
  const available = catalogue.rules.filter(
    (spec) => !rules.some((rule) => rule.type === spec.type),
  );

  const addRule = (type: PolicyRuleType) => {
    const spec = specs.get(type);
    if (!spec) return;
    setRules((current) => [
      ...current,
      spec.requiresValues ? { type, values: [] } : { type },
    ]);
  };

  const updateRule = (index: number, next: PolicyRule) => {
    setRules((current) => current.map((rule, at) => (at === index ? next : rule)));
  };

  const removeRule = (index: number) => {
    setRules((current) => current.filter((_, at) => at !== index));
  };

  const handleSubmit = async (event: React.FormEvent) => {
    event.preventDefault();
    setError(null);

    // The two checks worth making locally, because both are certain rejections
    // and both are easy to make by accident.
    if (name.trim() === "") {
      setError("A policy needs a name.");
      return;
    }
    if (rules.length === 0) {
      setError(
        "A policy needs at least one rule. One with no rules can never pass or fail.",
      );
      return;
    }

    setSaving(true);
    try {
      await onSubmit({
        name: name.trim(),
        description: description.trim(),
        severity,
        enabled,
        // Empty values are dropped here as well as on the server, so what is
        // sent is what the operator can see in the form.
        rules: rules.map((rule) => ({
          type: rule.type,
          ...(rule.severity ? { severity: rule.severity } : {}),
          ...(rule.values
            ? { values: rule.values.map((value) => value.trim()).filter(Boolean) }
            : {}),
        })),
      });
    } catch (caught) {
      // The SERVER's message, verbatim. It names the field and the constraint
      // and never echoes the submitted value, so it is safe to render as text.
      setError(
        caught instanceof ApiError
          ? caught.message
          : "The policy could not be saved.",
      );
    } finally {
      setSaving(false);
    }
  };

  return (
    <form
      onSubmit={handleSubmit}
      className="space-y-5 rounded-lg border border-border-subtle bg-surface p-5"
      aria-label={policy ? "Edit policy" : "New policy"}
    >
      <div className="grid gap-4 sm:grid-cols-2">
        <label className="flex flex-col gap-1 text-xs text-content-muted">
          Name
          <input
            type="text"
            required
            maxLength={catalogue.limits.maxNameBytes}
            className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
            value={name}
            onChange={(event) => setName(event.target.value)}
          />
        </label>

        <label className="flex flex-col gap-1 text-xs text-content-muted">
          Default severity
          <select
            className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
            value={severity}
            onChange={(event) => setSeverity(event.target.value as PolicySeverity)}
          >
            {POLICY_SEVERITY_ORDER.map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </select>
        </label>
      </div>

      <label className="flex flex-col gap-1 text-xs text-content-muted">
        Description
        <textarea
          rows={2}
          maxLength={catalogue.limits.maxDescriptionBytes}
          className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
          value={description}
          onChange={(event) => setDescription(event.target.value)}
        />
      </label>

      <label className="flex items-center gap-2 text-sm text-content">
        <input
          type="checkbox"
          className="h-6 w-6 shrink-0 rounded border-border-subtle"
          checked={enabled}
          onChange={(event) => setEnabled(event.target.checked)}
        />
        Evaluate this policy
      </label>

      <section className="space-y-3">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <h3 className="text-sm font-semibold text-content">
            Rules
            <span className="ml-2 font-normal text-content-muted">
              {rules.length} of {catalogue.limits.maxRules}
            </span>
          </h3>

          <label className="flex items-center gap-2 text-xs text-content-muted">
            Add a rule
            <select
              className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
              value=""
              disabled={available.length === 0 || rules.length >= catalogue.limits.maxRules}
              onChange={(event) => {
                if (event.target.value) addRule(event.target.value as PolicyRuleType);
              }}
            >
              <option value="">Choose…</option>
              {available.map((spec) => (
                <option key={spec.type} value={spec.type}>
                  {spec.label}
                </option>
              ))}
            </select>
          </label>
        </div>

        {rules.length === 0 ? (
          <p className="text-sm text-content-muted">
            No rules yet. A policy with no rules can never pass or fail, so the
            server refuses one.
          </p>
        ) : (
          <ul className="space-y-3">
            {rules.map((rule, index) => (
              <RuleRow
                key={rule.type}
                rule={rule}
                spec={specs.get(rule.type)}
                catalogue={catalogue}
                onChange={(next) => updateRule(index, next)}
                onRemove={() => removeRule(index)}
              />
            ))}
          </ul>
        )}
      </section>

      {error && (
        <p
          role="alert"
          className="rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger"
        >
          {error}
        </p>
      )}

      <div className="flex flex-wrap items-center gap-3">
        <button
          type="submit"
          disabled={saving}
          className="rounded-md border border-border-subtle bg-surface-raised px-3 py-1.5 text-sm font-medium text-content disabled:opacity-60"
        >
          {saving ? "Saving…" : policy ? "Save changes" : "Create policy"}
        </button>
        <button
          type="button"
          onClick={onCancel}
          className="rounded-md px-3 py-1.5 text-sm text-content-muted hover:text-content"
        >
          Cancel
        </button>
      </div>
    </form>
  );
}

/** One rule and its parameters. */
function RuleRow({
  rule,
  spec,
  catalogue,
  onChange,
  onRemove,
}: {
  rule: PolicyRule;
  spec: PolicyRuleSpec | undefined;
  catalogue: PolicyRuleCatalogue;
  onChange: (rule: PolicyRule) => void;
  onRemove: () => void;
}) {
  // A rule type the catalogue does not describe means the server is newer than
  // this bundle. Rendered honestly rather than hidden, so an operator editing
  // the policy does not silently drop it on save.
  const label = spec?.label ?? rule.type;

  return (
    <li className="rounded-md border border-border-subtle p-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <p className="text-sm font-medium text-content">{label}</p>
          {spec ? (
            <p className="max-w-prose text-xs text-content-muted">
              {spec.description}
            </p>
          ) : (
            <p className="max-w-prose text-xs text-warn">
              This rule type is not in the catalogue this page was built from.
              Its parameters are shown as text and are saved unchanged.
            </p>
          )}
        </div>

        <div className="flex items-center gap-2">
          <label className="flex items-center gap-1 text-xs text-content-muted">
            Severity
            <select
              className="rounded-md border border-border-subtle bg-surface px-2 py-1 text-xs text-content"
              value={rule.severity ?? ""}
              onChange={(event) =>
                onChange({
                  ...rule,
                  ...(event.target.value
                    ? { severity: event.target.value as PolicySeverity }
                    : { severity: undefined }),
                })
              }
            >
              <option value="">inherit</option>
              {POLICY_SEVERITY_ORDER.map((value) => (
                <option key={value} value={value}>
                  {value}
                </option>
              ))}
            </select>
          </label>

          <button
            type="button"
            onClick={onRemove}
            className="rounded-md px-2 py-1 text-xs text-content-muted hover:text-danger"
            aria-label={`Remove ${label}`}
          >
            Remove
          </button>
        </div>
      </div>

      {spec?.requiresValues && (
        <RuleValues rule={rule} spec={spec} catalogue={catalogue} onChange={onChange} />
      )}
    </li>
  );
}

/**
 * A rule's parameter list.
 *
 * Entered one per line, which avoids inventing a separator that a value could
 * itself contain — a label key or an image pattern can hold a comma, and a
 * split on one would quietly produce two wrong rules.
 */
function RuleValues({
  rule,
  spec,
  catalogue,
  onChange,
}: {
  rule: PolicyRule;
  spec: PolicyRuleSpec;
  catalogue: PolicyRuleCatalogue;
  onChange: (rule: PolicyRule) => void;
}) {
  const values = rule.values ?? [];

  // The restart-policy rule takes names from a closed vocabulary, so it gets
  // checkboxes rather than free text: an operator cannot mistype "unless
  // stopped" into a rule that then silently matches nothing.
  if (spec.valueKind === "restartPolicy") {
    return (
      <fieldset className="mt-3">
        <legend className="text-xs text-content-muted">Allowed policies</legend>
        <div className="mt-1 flex flex-wrap gap-3">
          {catalogue.restartPolicyNames.map((policyName) => (
            <label key={policyName} className="flex items-center gap-1.5 text-sm text-content">
              <input
                type="checkbox"
                className="h-6 w-6 shrink-0 rounded border-border-subtle"
                checked={values.includes(policyName)}
                onChange={(event) =>
                  onChange({
                    ...rule,
                    values: event.target.checked
                      ? [...values, policyName]
                      : values.filter((value) => value !== policyName),
                  })
                }
              />
              {policyName}
            </label>
          ))}
        </div>
      </fieldset>
    );
  }

  return (
    <label className="mt-3 flex flex-col gap-1 text-xs text-content-muted">
      {valueLabel(spec)}
      <textarea
        rows={3}
        className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 font-mono text-sm text-content"
        value={values.join("\n")}
        placeholder={valuePlaceholder(spec)}
        onChange={(event) =>
          onChange({
            ...rule,
            // Split only; trimming and deduplication happen on submit and again
            // on the server, so what is typed stays visible while typing.
            values: event.target.value.split("\n"),
          })
        }
      />
      <span>
        One per line, at most {catalogue.limits.maxValuesPerRule}.{" "}
        {patternHelp(spec)}
      </span>
    </label>
  );
}

function valueLabel(spec: PolicyRuleSpec): string {
  switch (spec.valueKind) {
    case "imagePattern":
      return "Image patterns";
    case "labelKeyPattern":
      return "Label key patterns";
    case "envNamePattern":
      return "Environment variable name patterns";
    case "capability":
      return "Capabilities";
    case "networkName":
      return "Network names";
    default:
      return "Values";
  }
}

function valuePlaceholder(spec: PolicyRuleSpec): string {
  switch (spec.valueKind) {
    case "imagePattern":
      return "registry.example.com/*";
    case "labelKeyPattern":
      return "com.example.owner";
    case "envNamePattern":
      return "AWS_*";
    case "capability":
      return "SYS_ADMIN";
    case "networkName":
      return "app_backend";
    default:
      return "";
  }
}

/**
 * Says what the value means, including the part that surprises people: env
 * rules match NAMES, and capabilities and networks are matched exactly.
 */
function patternHelp(spec: PolicyRuleSpec): string {
  switch (spec.valueKind) {
    case "imagePattern":
    case "labelKeyPattern":
      return "* matches any run of characters and ? matches one.";
    case "envNamePattern":
      return "* matches any run of characters and ? matches one. Names only — values are never read.";
    case "capability":
      return "Matched exactly. CAP_ prefixes and case are normalised; wildcards are not accepted.";
    case "networkName":
      return "Matched exactly. Host mode appears as “host” and isolation as “none”.";
    default:
      return "";
  }
}
