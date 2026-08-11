import { useMemo, useState } from "react";
import { Link } from "react-router";

import { ApiError } from "../api/client";
import type {
  PolicyOperatorStatus,
  PolicyRuleType,
  PolicySeverity,
  PolicySummary,
  PolicyViolation,
} from "../api/policyTypes";
import {
  POLICY_OPERATOR_STATUSES,
  POLICY_SEVERITY_ORDER,
  RULE_LABELS,
} from "../api/policyTypes";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import {
  PolicySeverityBadge,
  PolicyStatusBadge,
  PolicyViolationValues,
  RuleBadge,
} from "../components/PolicyBadges";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import {
  requestPolicyEvaluation,
  setViolationStatus,
  usePolicySummary,
  usePolicyViolations,
} from "../hooks/usePolicies";

const PAGE_SIZE = 25;

/**
 * The compliance dashboard.
 *
 * Everything here is an OBSERVATION: which containers fail which rules.
 * Nothing on this page changes a container — there is no remediation control,
 * because HarborMaster has no such capability and the API exposes no such
 * endpoint. The only writes are an operator's acknowledgement and a request to
 * re-run the checks.
 */
export function PolicyViolations() {
  const [page, setPage] = useState(1);
  const [severity, setSeverity] = useState<PolicySeverity | "">("");
  const [rule, setRule] = useState<PolicyRuleType | "">("");
  const [showResolved, setShowResolved] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [evaluating, setEvaluating] = useState(false);

  const query = useMemo(
    () => ({
      page,
      pageSize: PAGE_SIZE,
      openOnly: !showResolved,
      ...(severity ? { severity: [severity] } : {}),
      ...(rule ? { rule: [rule] } : {}),
    }),
    [page, severity, rule, showResolved],
  );

  const summary = usePolicySummary();
  const violations = usePolicyViolations(query);

  const evaluate = async () => {
    setActionError(null);
    setEvaluating(true);
    try {
      await requestPolicyEvaluation();
      // The server answered 202 — the pass is scheduled, not finished — so the
      // views are refreshed rather than assumed to be current.
      summary.refresh();
      violations.refresh();
    } catch (caught) {
      setActionError(
        caught instanceof ApiError
          ? caught.message
          : "The evaluation could not be requested.",
      );
    } finally {
      setEvaluating(false);
    }
  };

  const acknowledge = async (
    violation: PolicyViolation,
    status: PolicyOperatorStatus,
  ) => {
    setActionError(null);
    try {
      await setViolationStatus(violation.id, status);
      violations.refresh();
      summary.refresh();
    } catch (caught) {
      setActionError(
        caught instanceof ApiError
          ? caught.message
          : "The status could not be changed.",
      );
    }
  };

  return (
    <div className="space-y-6">
      <PageIntro
        title="Compliance"
        description={
          "Which containers fail which policies. HarborMaster reports " +
          "non-compliance; it cannot change a container to resolve it."
        }
      />

      <ComplianceCards state={summary} onEvaluate={evaluate} evaluating={evaluating} />

      {actionError && (
        <p
          role="alert"
          className="rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger"
        >
          {actionError}
        </p>
      )}

      <section className="space-y-4">
        <Filters
          severity={severity}
          rule={rule}
          showResolved={showResolved}
          onSeverity={(value) => {
            setSeverity(value);
            setPage(1);
          }}
          onRule={(value) => {
            setRule(value);
            setPage(1);
          }}
          onShowResolved={(value) => {
            setShowResolved(value);
            setPage(1);
          }}
        />

        <ViolationList
          state={violations}
          onPage={setPage}
          onAcknowledge={acknowledge}
        />
      </section>
    </div>
  );
}

/** The compliance summary cards. */
function ComplianceCards({
  state,
  onEvaluate,
  evaluating,
}: {
  state: ReturnType<typeof usePolicySummary>;
  onEvaluate: () => void;
  evaluating: boolean;
}) {
  if (state.status === "loading") {
    return <LoadingState label="Loading compliance summary" />;
  }
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }

  // Tolerate a null or malformed payload rather than throwing: a view that
  // crashes on unexpected input turns a backend hiccup into a blank screen.
  const summary = state.data;
  if (!summary) return <LoadingState label="Loading compliance summary" />;

  const rate = summary.containersEvaluated
    ? Math.round((summary.containersCompliant / summary.containersEvaluated) * 100)
    : 0;

  return (
    <section aria-label="Compliance summary" className="space-y-3">
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Card
          label="Compliant"
          value={summary.containersEvaluated ? `${rate}%` : "—"}
          // Stated over EVALUATED containers rather than the estate: a rate
          // whose denominator silently included containers nobody checked would
          // improve every time coverage got worse.
          hint={`${summary.containersCompliant} of ${summary.containersEvaluated} evaluated`}
          tone={summary.containersEvaluated && rate < 100 ? "warn" : "neutral"}
        />
        <Card
          label="Open violations"
          value={summary.open}
          hint={`${summary.total} recorded in total`}
          tone={summary.open > 0 ? "warn" : "neutral"}
        />
        <Card
          label="Critical"
          value={summary.bySeverity.critical ?? 0}
          hint="Breaks a non-negotiable rule"
          tone={(summary.bySeverity.critical ?? 0) > 0 ? "danger" : "neutral"}
        />
        <Card
          label="Policies in force"
          value={summary.policies}
          hint={`${summary.policiesTotal} defined in total`}
        />
      </div>

      {summary.policies === 0 && (
        <p
          role="status"
          className="rounded-lg border border-border-subtle bg-surface px-3 py-2 text-xs text-content-muted"
        >
          No policies are enabled, so nothing is being checked. An estate with no
          policies has not been found compliant — it has not been asked anything.{" "}
          <Link to="/policies" className="underline underline-offset-2">
            Define one
          </Link>
          .
        </p>
      )}

      {summary.incomplete && (
        <p
          role="status"
          className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-xs text-warn"
        >
          At least one container could not be checked against every policy, so
          these counts are a floor rather than a total.
        </p>
      )}

      <div className="flex flex-wrap items-center gap-3">
        <SeverityBreakdown summary={summary} />

        <button
          type="button"
          onClick={onEvaluate}
          disabled={evaluating}
          className="ml-auto rounded-md border border-border-subtle bg-surface-raised px-3 py-1.5 text-sm font-medium text-content disabled:opacity-60"
          // Says what it does: the server schedules a pass and answers
          // immediately. Promising a finished result would be a lie about a
          // sweep that can take a while on a large estate.
          title="Schedules a compliance pass over every container. Returns as soon as it is queued."
        >
          {evaluating ? "Requesting…" : "Re-evaluate now"}
        </button>
      </div>

      {summary.lastEvaluatedAt && (
        <p className="text-xs text-content-muted">
          Last evaluated{" "}
          <time dateTime={summary.lastEvaluatedAt}>
            {new Date(summary.lastEvaluatedAt).toLocaleString()}
          </time>
        </p>
      )}
    </section>
  );
}

function Card({
  label,
  value,
  hint,
  tone = "neutral",
}: {
  label: string;
  value: number | string;
  hint: string;
  tone?: "neutral" | "warn" | "danger";
}) {
  const toneClasses = {
    neutral: "border-border-subtle",
    warn: "border-warn/40",
    danger: "border-danger/40",
  }[tone];

  return (
    <div className={`rounded-lg border ${toneClasses} bg-surface p-4`}>
      <p className="text-xs font-medium uppercase tracking-wide text-content-muted">
        {label}
      </p>
      <p className="mt-1 text-2xl font-semibold text-content">{value}</p>
      <p className="mt-1 text-xs text-content-muted">{hint}</p>
    </div>
  );
}

/** A compact severity distribution, so the shape of the estate is visible. */
function SeverityBreakdown({ summary }: { summary: PolicySummary }) {
  const total = POLICY_SEVERITY_ORDER.reduce(
    (sum, severity) => sum + (summary.bySeverity[severity] ?? 0),
    0,
  );
  if (total === 0) return null;

  return (
    <div
      className="flex flex-wrap items-center gap-2"
      aria-label="Open violations by severity"
    >
      {POLICY_SEVERITY_ORDER.map((severity) => {
        const count = summary.bySeverity[severity] ?? 0;
        if (count === 0) return null;
        return (
          <span key={severity} className="inline-flex items-center gap-1.5">
            <PolicySeverityBadge severity={severity} />
            <span className="text-xs text-content-muted">{count}</span>
          </span>
        );
      })}
    </div>
  );
}

function Filters({
  severity,
  rule,
  showResolved,
  onSeverity,
  onRule,
  onShowResolved,
}: {
  severity: PolicySeverity | "";
  rule: PolicyRuleType | "";
  showResolved: boolean;
  onSeverity: (value: PolicySeverity | "") => void;
  onRule: (value: PolicyRuleType | "") => void;
  onShowResolved: (value: boolean) => void;
}) {
  return (
    <div className="flex flex-wrap items-end gap-3">
      <label className="flex flex-col gap-1 text-xs text-content-muted">
        Severity
        <select
          className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
          value={severity}
          onChange={(event) => onSeverity(event.target.value as PolicySeverity | "")}
        >
          <option value="">All severities</option>
          {POLICY_SEVERITY_ORDER.map((value) => (
            <option key={value} value={value}>
              {value}
            </option>
          ))}
        </select>
      </label>

      <label className="flex flex-col gap-1 text-xs text-content-muted">
        Rule
        <select
          className="rounded-md border border-border-subtle bg-surface px-2 py-1.5 text-sm text-content"
          value={rule}
          onChange={(event) => onRule(event.target.value as PolicyRuleType | "")}
        >
          <option value="">All rules</option>
          {(Object.keys(RULE_LABELS) as PolicyRuleType[]).map((value) => (
            <option key={value} value={value}>
              {RULE_LABELS[value]}
            </option>
          ))}
        </select>
      </label>

      <label className="flex items-center gap-2 pb-1.5 text-sm text-content">
        <input
          type="checkbox"
          className="h-6 w-6 shrink-0 rounded border-border-subtle"
          checked={showResolved}
          onChange={(event) => onShowResolved(event.target.checked)}
        />
        Include resolved
      </label>
    </div>
  );
}

function ViolationList({
  state,
  onPage,
  onAcknowledge,
}: {
  state: ReturnType<typeof usePolicyViolations>;
  onPage: (page: number) => void;
  onAcknowledge: (
    violation: PolicyViolation,
    status: PolicyOperatorStatus,
  ) => void;
}) {
  if (state.status === "loading") return <LoadingState label="Loading violations" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }

  const data = state.data;
  const items = data?.items ?? [];

  if (items.length === 0) {
    return (
      <EmptyState
        title="No violations match these filters"
        description={
          "A container with no violations complies with every enabled policy. " +
          "A container that has never been evaluated is not shown here at all — " +
          "check its detail page to tell the two apart."
        }
      />
    );
  }

  return (
    <div className="space-y-3">
      <ul className="space-y-2">
        {items.map((violation) => (
          <ViolationRow
            key={violation.id}
            violation={violation}
            onAcknowledge={onAcknowledge}
          />
        ))}
      </ul>
      {data?.pagination && (
        <Pagination
          pagination={data.pagination}
          onPageChange={onPage}
          busy={state.refreshing}
        />
      )}
    </div>
  );
}

/** One violation. */
function ViolationRow({
  violation,
  onAcknowledge,
}: {
  violation: PolicyViolation;
  onAcknowledge: (
    violation: PolicyViolation,
    status: PolicyOperatorStatus,
  ) => void;
}) {
  return (
    <li className="rounded-lg border border-border-subtle bg-surface p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <div className="flex flex-wrap items-center gap-2">
            <PolicySeverityBadge severity={violation.severity} />
            <RuleBadge rule={violation.ruleType} />
            <PolicyStatusBadge status={violation.status} />
          </div>

          <p className="text-sm text-content">{violation.policyName}</p>

          <p className="text-xs text-content-muted">
            <Link
              to={`/containers/${violation.containerId}`}
              className="underline underline-offset-2 hover:text-content"
            >
              {violation.containerName || violation.containerId.slice(0, 12)}
            </Link>
            {" · first seen "}
            <time dateTime={violation.detectedAt}>
              {new Date(violation.detectedAt).toLocaleString()}
            </time>
          </p>

          {violation.reason && (
            <p className="text-xs text-content-muted">{violation.reason}</p>
          )}

          {violation.note && (
            <p className="text-xs italic text-content-muted">
              Operator note: {violation.note}
            </p>
          )}
        </div>

        <div className="min-w-0 max-w-md shrink-0 space-y-2">
          <PolicyViolationValues violation={violation} />

          {violation.status !== "resolved" && (
            <div className="flex flex-wrap items-center gap-2">
              {POLICY_OPERATOR_STATUSES.filter(
                (status) => status !== violation.status,
              ).map((status) => (
                <button
                  key={status}
                  type="button"
                  onClick={() => onAcknowledge(violation, status)}
                  className="rounded-md border border-border-subtle px-2 py-1 text-xs text-content-muted hover:text-content"
                  // Says the part that surprises people: this does not stop the
                  // checking.
                  title="Records that you have seen this. The rule is still checked on every pass, and the violation resolves by itself once the container complies."
                >
                  Mark {status}
                </button>
              ))}
            </div>
          )}
        </div>
      </div>
    </li>
  );
}
