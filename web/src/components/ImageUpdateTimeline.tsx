import type { ImageEventKind, ImageUpdateEvent } from "../api/imageTypes";
import { UPDATE_TYPE_LABELS } from "../api/imageTypes";

/**
 * An image update timeline.
 *
 * Ordered by when each change was OBSERVED, newest first. Every entry is a
 * change that actually happened — a pass that found everything unchanged writes
 * nothing — so this reads as "what moved, and when" rather than "when we last
 * looked".
 */
export function ImageUpdateTimeline({ events }: { events: ImageUpdateEvent[] }) {
  const ordered = [...events].sort(
    (a, b) => new Date(b.observedAt).getTime() - new Date(a.observedAt).getTime(),
  );

  if (ordered.length === 0) return null;

  return (
    <ol className="relative space-y-4 border-l border-border-subtle pl-5">
      {ordered.map((event) => (
        <li key={event.id} className="relative">
          <span
            aria-hidden="true"
            className={`absolute -left-[1.4rem] top-1.5 size-2.5 rounded-full ring-2 ring-surface ${dotFor(event.kind)}`}
          />

          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm text-content">{titleFor(event)}</span>
            <time
              dateTime={event.observedAt}
              className="text-xs text-content-muted"
            >
              {new Date(event.observedAt).toLocaleString()}
            </time>
          </div>

          {event.detail && (
            <p className="mt-1 text-xs text-content-muted">{event.detail}</p>
          )}

          {(event.previousDigest || event.currentDigest) && (
            <dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-3 text-xs">
              {event.previousDigest && (
                <>
                  <dt className="text-content-muted">Was</dt>
                  <dd className="font-mono break-all text-content-muted">
                    {event.previousDigest}
                  </dd>
                </>
              )}
              {event.currentDigest && (
                <>
                  <dt className="text-content-muted">Now</dt>
                  <dd className="font-mono break-all text-content">
                    {event.currentDigest}
                  </dd>
                </>
              )}
            </dl>
          )}
        </li>
      ))}
    </ol>
  );
}

/**
 * A human sentence for each event kind.
 *
 * Written as statements about the world rather than as engine vocabulary: an
 * operator reading a timeline wants to know what happened, not which branch of
 * the classifier ran.
 */
function titleFor(event: ImageUpdateEvent): string {
  switch (event.kind) {
    case "discovered":
      return "First resolved against the registry";
    case "digestChanged":
      return "The publisher republished this tag";
    case "updateFound":
      return event.latestTag
        ? `Update available: ${event.latestTag}`
        : `Update available: ${UPDATE_TYPE_LABELS[event.currentUpdateType ?? "unknown"]}`;
    case "updateCleared":
      return "The reported update is no longer available";
    case "checkFailed":
      return "The registry stopped answering";
    case "checkRecovered":
      return "The registry is answering again";
    default:
      return event.kind;
  }
}

function dotFor(kind: ImageEventKind): string {
  switch (kind) {
    case "updateFound":
    case "digestChanged":
      return "bg-warn";
    case "checkFailed":
      return "bg-danger";
    case "updateCleared":
    case "checkRecovered":
      return "bg-ok";
    default:
      return "bg-content-muted";
  }
}
