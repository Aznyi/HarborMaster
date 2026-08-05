import type {
  PolicyDefinition,
  PolicyRuleType,
  PolicySeverity,
  PolicyViolation,
  PolicyViolationStatus,
} from "../api/policyTypes";
import { RULE_LABELS } from "../api/policyTypes";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

/**
 * Policy badges.
 *
 * Built on the shared StatusBadge, so compliance looks like the rest of the app
 * and inherits the rule that colour is never the only signal: every badge
 * carries a label, and the tone only reinforces it.
 */

const severityTones: Record<PolicySeverity, BadgeTone> = {
  critical: "danger",
  high: "danger",
  medium: "warn",
  low: "neutral",
};

/**
 * Severity, with its meaning in the tooltip.
 *
 * critical and high share the danger tone deliberately: both mean "look at this
 * now", and inventing a fifth colour to separate them would dilute the one
 * signal that matters.
 */
export function PolicySeverityBadge({ severity }: { severity: PolicySeverity }) {
  const meaning: Record<PolicySeverity, string> = {
    critical: "Breaks a rule the organisation treats as non-negotiable",
    high: "A significant gap in the required posture",
    medium: "Worth fixing, but not urgent",
    low: "Bookkeeping: conventions and hygiene",
  };

  return (
    <StatusBadge
      tone={severityTones[severity]}
      label={severity}
      title={meaning[severity]}
    />
  );
}

const statusTones: Record<PolicyViolationStatus, BadgeTone> = {
  active: "danger",
  resolved: "ok",
  acknowledged: "warn",
  exempted: "neutral",
};

/**
 * Lifecycle status.
 *
 * The tooltips say the thing that is easiest to get wrong: neither operator
 * status stops the checking. An acknowledged violation is re-evaluated on every
 * pass and resolves by itself once the container complies.
 */
export function PolicyStatusBadge({ status }: { status: PolicyViolationStatus }) {
  const meaning: Record<PolicyViolationStatus, string> = {
    active: "The rule fails and nobody has reviewed it",
    resolved:
      "A later pass found the container compliant. Set by the engine, never by hand",
    acknowledged:
      "Seen and accepted for now. Still re-checked on every pass, and resolves by itself once the container complies",
    exempted:
      "The risk is accepted for this container. Still re-checked on every pass",
  };

  return (
    <StatusBadge tone={statusTones[status]} label={status} title={meaning[status]} />
  );
}

/** The rule that failed, rendered by its human label rather than its identifier. */
export function RuleBadge({ rule }: { rule: PolicyRuleType }) {
  return <StatusBadge tone="neutral" label={RULE_LABELS[rule] ?? rule} />;
}

/** Whether a policy is in force, and why not when it is not. */
export function PolicyStateBadge({ policy }: { policy: PolicyDefinition }) {
  if (policy.archived) {
    return (
      <StatusBadge
        tone="neutral"
        label="withdrawn"
        title="Archived. It is no longer evaluated, and the violations it found are kept as history"
      />
    );
  }
  if (!policy.enabled) {
    return (
      <StatusBadge
        tone="warn"
        label="disabled"
        title="Kept but not evaluated. Its open violations were resolved when it was disabled"
      />
    );
  }
  return (
    <StatusBadge tone="ok" label="enabled" title="Evaluated against every container" />
  );
}

/**
 * Renders what a container has against what the rule wanted.
 *
 * `observed` is engine-rendered text: for an environment rule it is the
 * offending variable NAMES, never a value. There is nothing to withhold here
 * because there is nothing the server could have sent.
 */
export function PolicyViolationValues({ violation }: { violation: PolicyViolation }) {
  if (!violation.observed && !violation.expected) return null;

  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
      <dt className="text-content-muted">Observed</dt>
      <dd className="font-mono break-all text-content">
        {violation.observed || "—"}
      </dd>
      <dt className="text-content-muted">Required</dt>
      <dd className="font-mono break-all text-content">
        {violation.expected || "—"}
      </dd>
    </dl>
  );
}
