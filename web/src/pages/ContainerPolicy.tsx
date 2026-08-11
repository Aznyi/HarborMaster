import { useState } from "react";
import { Link, useParams } from "react-router";

import { ApiError } from "../api/client";
import type {
  PolicyEvaluation,
  PolicyOperatorStatus,
  PolicyViolation,
} from "../api/policyTypes";
import { POLICY_OPERATOR_STATUSES, RULE_LABELS } from "../api/policyTypes";
import { DetailSection } from "../components/DetailSection";
import { PageIntro } from "../components/PageIntro";
import {
  PolicySeverityBadge,
  PolicyStatusBadge,
  PolicyViolationValues,
} from "../components/PolicyBadges";
import { PolicyTimeline } from "../components/PolicyTimeline";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { setViolationStatus, useContainerPolicy } from "../hooks/usePolicies";

/**
 * One container's compliance.
 *
 * The evaluation banner is the important part of this page: it distinguishes
 * "this container complies with every enabled policy" from "this container was
 * never checked". Rendering an empty list as a clean bill of health for a
 * container no pass has reached would be the worst thing this page could do.
 */
export function ContainerPolicy() {
  const { id = "" } = useParams();
  const [showResolved, setShowResolved] = useState(false);
  const state = useContainerPolicy(id, { openOnly: !showResolved, pageSize: 200 });

  if (state.status === "loading") {
    return <LoadingState label="Loading container compliance" />;
  }
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }

  // Tolerate a malformed payload rather than throwing.
  const violations: PolicyViolation[] = state.data?.violations ?? [];
  const evaluation = state.data?.evaluation;

  return (
    <div className="space-y-6">
      <PageIntro
        title="Container compliance"
        description={
          "Which policies this container fails. Reporting only — nothing here " +
          "changes the container."
        }
      />

      <p className="text-sm text-content-muted">
        <Link
          to={`/containers/${id}`}
          className="underline underline-offset-2 hover:text-content"
        >
          Back to the container
        </Link>
      </p>

      <EvaluationBanner evaluation={evaluation} />

      <label className="flex items-center gap-2 text-sm text-content">
        <input
          type="checkbox"
          className="h-6 w-6 shrink-0 rounded border-border-subtle"
          checked={showResolved}
          onChange={(event) => setShowResolved(event.target.checked)}
        />
        Include resolved
      </label>

      {violations.length === 0 ? (
        <EmptyState
          title={
            evaluation
              ? "Compliant with every enabled policy"
              : "This container has not been evaluated"
          }
          description={
            evaluation
              ? "Every rule this container was checked against passed."
              : "Compliance is evaluated in the background after each inventory refresh. An estate with no policies defined is never evaluated at all."
          }
        />
      ) : (
        <>
          <DetailSection title="Timeline">
            <PolicyTimeline violations={violations} />
          </DetailSection>

          <DetailSection title="Violations">
            <ul className="space-y-2">
              {violations.map((violation) => (
                <ViolationCard
                  key={violation.id}
                  violation={violation}
                  onChanged={state.refresh}
                />
              ))}
            </ul>
          </DetailSection>
        </>
      )}
    </div>
  );
}

/**
 * The evaluation banner.
 *
 * Three distinct states, deliberately not collapsed into two: never evaluated,
 * evaluated but incomplete, and evaluated fully. Only the last one licenses
 * "this container complies".
 */
function EvaluationBanner({ evaluation }: { evaluation?: PolicyEvaluation }) {
  if (!evaluation) {
    return (
      <p
        role="status"
        className="rounded-lg border border-border-subtle bg-surface-sunken px-3 py-2 text-sm text-content-muted"
      >
        This container has never been evaluated against any policy, so an empty
        list here means <strong>not checked</strong> rather than{" "}
        <strong>compliant</strong>.
      </p>
    );
  }

  if (!evaluation.complete) {
    return (
      <p
        role="status"
        className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-sm text-warn"
      >
        The most recent pass could not apply every policy
        {evaluation.reason ? `: ${evaluation.reason}` : "."} Anything listed
        below is real, but the list may be incomplete.
      </p>
    );
  }

  return (
    <p className="text-xs text-content-muted">
      Last evaluated{" "}
      <time dateTime={evaluation.evaluatedAt}>
        {new Date(evaluation.evaluatedAt).toLocaleString()}
      </time>{" "}
      against {evaluation.policiesEvaluated}{" "}
      {evaluation.policiesEvaluated === 1 ? "policy" : "policies"} (
      {evaluation.rulesEvaluated}{" "}
      {evaluation.rulesEvaluated === 1 ? "rule" : "rules"}).
    </p>
  );
}

/** One violation, with the operator's status controls. */
function ViolationCard({
  violation,
  onChanged,
}: {
  violation: PolicyViolation;
  onChanged: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function apply(status: PolicyOperatorStatus) {
    setBusy(true);
    setError(null);
    try {
      await setViolationStatus(violation.id, status);
      onChanged();
    } catch (cause) {
      setError(
        cause instanceof ApiError ? cause.message : "The status could not be changed",
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <li className="rounded-lg border border-border-subtle bg-surface p-4">
      <div className="flex flex-wrap items-center gap-2">
        <PolicySeverityBadge severity={violation.severity} />
        <PolicyStatusBadge status={violation.status} />
      </div>

      <p className="mt-2 text-sm font-medium text-content">
        {RULE_LABELS[violation.ruleType] ?? violation.ruleType}
      </p>
      <p className="text-xs text-content-muted">{violation.policyName}</p>

      {violation.reason && (
        <p className="mt-1 text-xs text-content-muted">{violation.reason}</p>
      )}

      <div className="mt-3">
        <PolicyViolationValues violation={violation} />
      </div>

      {violation.note && (
        <p className="mt-2 text-xs text-content-muted">
          <span className="font-medium">Note:</span> {violation.note}
        </p>
      )}

      {/*
        Only the two OPERATOR statuses are offered. There is no "resolve" button
        and there must not be: resolution is something the world does, observed
        by the engine, not something a person asserts. Neither of these stops
        the checking — the rule is still applied on every pass.
      */}
      {violation.status !== "resolved" && (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          {POLICY_OPERATOR_STATUSES.map((status) => (
            <button
              key={status}
              type="button"
              disabled={busy || violation.status === status}
              onClick={() => void apply(status)}
              className="rounded-md border border-border-subtle px-2.5 py-1 text-xs text-content transition hover:bg-surface-sunken disabled:cursor-not-allowed disabled:opacity-50"
              title="Records that you have seen this. The rule is still checked on every pass."
            >
              {status}
            </button>
          ))}
        </div>
      )}

      {error && (
        <p role="alert" className="mt-2 text-xs text-danger">
          {error}
        </p>
      )}
    </li>
  );
}
