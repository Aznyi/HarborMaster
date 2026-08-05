import type { DriftRecord } from "../api/driftTypes";
import { SeverityBadge } from "./DriftBadges";

/**
 * A drift timeline.
 *
 * Ordered by when each difference was FIRST seen, newest first. `detectedAt`
 * does not move when a later evaluation sees the same difference again, so
 * this reads as "what changed, and when" rather than "when we last looked" —
 * which is the question an operator reconstructing an incident is asking.
 *
 * A resolved record keeps its place and gains an end point. Removing it would
 * erase exactly the history that makes the timeline worth having.
 */
export function DriftTimeline({ records }: { records: DriftRecord[] }) {
  const ordered = [...records].sort(
    (a, b) => new Date(b.detectedAt).getTime() - new Date(a.detectedAt).getTime(),
  );

  if (ordered.length === 0) return null;

  return (
    <ol className="relative space-y-4 border-l border-border-subtle pl-5">
      {ordered.map((record) => (
        <li key={record.id} className="relative">
          <span
            aria-hidden="true"
            className={`absolute -left-[1.4rem] top-1.5 size-2.5 rounded-full ring-2 ring-surface ${
              record.status === "resolved" ? "bg-ok" : dotForSeverity(record.severity)
            }`}
          />

          <div className="flex flex-wrap items-center gap-2">
            <SeverityBadge severity={record.severity} />
            <span className="font-mono text-sm text-content break-all">
              {record.field}
            </span>
          </div>

          <p className="mt-1 text-xs text-content-muted">
            <time dateTime={record.detectedAt}>
              {new Date(record.detectedAt).toLocaleString()}
            </time>
            {record.resolvedAt ? (
              <>
                {" → resolved "}
                <time dateTime={record.resolvedAt}>
                  {new Date(record.resolvedAt).toLocaleString()}
                </time>
              </>
            ) : (
              <>
                {" · still present, last confirmed "}
                <time dateTime={record.lastSeenAt}>
                  {new Date(record.lastSeenAt).toLocaleString()}
                </time>
              </>
            )}
          </p>

          {record.reason && (
            <p className="mt-1 text-xs text-content-muted">{record.reason}</p>
          )}
        </li>
      ))}
    </ol>
  );
}

function dotForSeverity(severity: DriftRecord["severity"]): string {
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
