import type { ResourceState } from "../hooks/useApiResource";
import type { HealthReport } from "../api/types";
import { deriveIndicators } from "./ConnectivityIndicator";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

/**
 * One health control in the header, with the detail one interaction away.
 *
 * # What changed and what did not
 *
 * The header used to carry two permanent badges -- "Backend: connected" and
 * "Docker: connected" -- which is two thirds of the header spent telling an
 * operator that nothing is wrong. This collapses them to a single summary.
 *
 * Both underlying states remain AVAILABLE: they are listed inside, together
 * with the same explanatory titles they always had, and the summary reports the
 * worst of the two so a problem is never hidden behind a click. When something
 * is wrong the summary says so on its face.
 *
 * # Why <details> rather than a popover component
 *
 * It is disclosure, which is what the element is for: keyboard operable,
 * announced as expandable, and closable with no state to manage. A custom
 * popover would mean focus handling and an outside-click listener to arrive at
 * the same behaviour.
 */
export function SystemStatus({ health }: { health: ResourceState<HealthReport> }) {
  const { backend, docker } = deriveIndicators(health);

  // The worst of the two. A degraded Docker socket must not read as healthy
  // just because the API answered.
  const tone: BadgeTone =
    backend.tone === "danger" || docker.tone === "danger"
      ? "danger"
      : backend.tone === "warn" || docker.tone === "warn"
        ? "warn"
        : backend.tone === "neutral" || docker.tone === "neutral"
          ? "neutral"
          : "ok";

  const summary =
    tone === "ok"
      ? "All systems connected"
      : tone === "neutral"
        ? "Checking systems"
        : tone === "warn"
          ? "Degraded"
          : "Not connected";

  return (
    <details className="relative" data-testid="system-status">
      <summary
        className="flex min-h-11 cursor-pointer list-none items-center gap-2 rounded-lg px-2 py-1"
        aria-label={`System status: ${summary}`}
      >
        <StatusBadge tone={tone} label={summary} />
      </summary>

      <div className="absolute right-0 z-30 mt-1 w-64 rounded-lg border border-border-subtle bg-surface-raised p-3 shadow-lg">
        <dl className="flex flex-col gap-2 text-xs">
          <div className="flex flex-col gap-1">
            <dt className="font-medium">
              <StatusBadge tone={backend.tone} label={backend.label} />
            </dt>
            <dd className="text-content-muted">{backend.title}</dd>
          </div>
          <div className="flex flex-col gap-1">
            <dt className="font-medium">
              <StatusBadge tone={docker.tone} label={docker.label} />
            </dt>
            <dd className="text-content-muted">{docker.title}</dd>
          </div>
        </dl>
      </div>
    </details>
  );
}
