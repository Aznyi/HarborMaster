import type { PolicyViolation } from "../api/policyTypes";
import { RULE_LABELS } from "../api/policyTypes";
import { PolicySeverityBadge } from "./PolicyBadges";

/**
 * A policy violation timeline.
 *
 * Ordered by when each violation was FIRST detected, newest first.
 * `detectedAt` does not move when a later pass sees the same failure again, so
 * this reads as "when did we stop complying, and for how long" rather than
 * "when did we last look" — which is the question an auditor is asking.
 *
 * A resolved violation keeps its place and gains an end point. Removing it
 * would erase exactly the history that makes the timeline worth having, and it
 * is the history the archive-instead-of-delete design exists to protect.
 */
export function PolicyTimeline({ violations }: { violations: PolicyViolation[] }) {
  const ordered = [...violations].sort(
    (a, b) => new Date(b.detectedAt).getTime() - new Date(a.detectedAt).getTime(),
  );

  if (ordered.length === 0) return null;

  return (
    <ol className="relative space-y-4 border-l border-border-subtle pl-5">
      {ordered.map((violation) => (
        <li key={violation.id} className="relative">
          <span
            aria-hidden="true"
            className={`absolute -left-[1.4rem] top-1.5 size-2.5 rounded-full ring-2 ring-surface ${
              violation.status === "resolved"
                ? "bg-ok"
                : dotForSeverity(violation.severity)
            }`}
          />

          <div className="flex flex-wrap items-center gap-2">
            <PolicySeverityBadge severity={violation.severity} />
            <span className="text-sm text-content">
              {RULE_LABELS[violation.ruleType] ?? violation.ruleType}
            </span>
            <span className="text-xs text-content-muted">
              {violation.policyName}
            </span>
          </div>

          <p className="mt-1 text-xs text-content-muted">
            <time dateTime={violation.detectedAt}>
              {new Date(violation.detectedAt).toLocaleString()}
            </time>
            {violation.resolvedAt ? (
              <>
                {" → resolved "}
                <time dateTime={violation.resolvedAt}>
                  {new Date(violation.resolvedAt).toLocaleString()}
                </time>
              </>
            ) : (
              <>
                {" · still failing, last confirmed "}
                <time dateTime={violation.lastSeenAt}>
                  {new Date(violation.lastSeenAt).toLocaleString()}
                </time>
              </>
            )}
          </p>

          {violation.reason && (
            <p className="mt-1 text-xs text-content-muted">{violation.reason}</p>
          )}

          {violation.note && (
            <p className="mt-1 text-xs italic text-content-muted">
              Operator note: {violation.note}
            </p>
          )}
        </li>
      ))}
    </ol>
  );
}

function dotForSeverity(severity: PolicyViolation["severity"]): string {
  switch (severity) {
    case "critical":
    case "high":
      return "bg-danger";
    case "medium":
      return "bg-warn";
    default:
      return "bg-content-muted";
  }
}
