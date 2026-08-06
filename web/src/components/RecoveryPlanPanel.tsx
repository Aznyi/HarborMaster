import type { RecoveryPlan } from "../api/executionTypes";

/**
 * The manual recovery plan.
 *
 * # Nothing here is a button
 *
 * Every step is TEXT. There is no control that runs one, no endpoint behind
 * one, and no server capability that could execute it. HarborMaster does not
 * roll back: an automatic undo is another unattended mutation, performed at
 * exactly the moment HarborMaster has demonstrated that its model of the host
 * is wrong.
 *
 * So the panel's whole job is to be READABLE and COPYABLE. The commands are
 * rendered in a monospace block an operator can select, and the destructive
 * ones are called out so nobody runs one by reflex while skimming.
 */
export function RecoveryPlanPanel({ plan }: { plan: RecoveryPlan }) {
  const tone = {
    urgent: {
      border: "border-danger/40",
      background: "bg-danger-soft",
      heading: "text-danger",
      label: "Action needed now",
    },
    attention: {
      border: "border-warn/40",
      background: "bg-warn-soft",
      heading: "text-content",
      label: "Needs tidying up",
    },
    informational: {
      border: "border-border-subtle",
      background: "bg-surface-sunken",
      heading: "text-content",
      label: "Nothing to do",
    },
  }[plan.urgency];

  return (
    <section
      aria-label="Recovery steps"
      className={`space-y-3 rounded-xl border ${tone.border} ${tone.background} p-4`}
    >
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 className={`text-sm font-semibold ${tone.heading}`}>
          What was left on this host
        </h3>
        <span className="text-xs font-medium uppercase tracking-wide text-content-muted">
          {tone.label}
        </span>
      </div>

      <p className="text-sm text-content">{plan.situation}</p>

      {plan.serviceInterrupted && (
        <p role="alert" className="text-sm font-medium text-danger">
          This container is not currently serving.
        </p>
      )}

      {plan.steps && plan.steps.length > 0 && (
        <ol className="space-y-3">
          {plan.steps.map((step) => (
            <li key={step.order} className="space-y-1">
              <p className="text-sm text-content">
                <span className="mr-2 font-semibold text-content-muted">
                  {step.order}.
                </span>
                {step.description}
                {step.destructive && (
                  <span className="ml-2 rounded border border-danger/40 px-1.5 py-0.5 text-xs font-medium text-danger">
                    removes something
                  </span>
                )}
              </p>
              {step.command && (
                <pre className="overflow-x-auto rounded-md border border-border-subtle bg-surface px-3 py-2 font-mono text-xs text-content">
                  <code>{step.command}</code>
                </pre>
              )}
            </li>
          ))}
        </ol>
      )}

      <p className="text-xs text-content-muted">
        HarborMaster does not run these for you. It stopped rather than acting
        again on a host whose state it could no longer be sure of, and these are
        the steps it would have needed to be sure about.
      </p>
    </section>
  );
}
