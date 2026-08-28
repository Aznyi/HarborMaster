import { formatMoment } from "../api/presentation";
import { useCallback, useMemo, useState } from "react";
import { Link } from "react-router";

import type { DockerEvent, DockerEventQuery } from "../api/eventTypes";
import {
  MAX_LIVE_EVENTS,
  useDockerEventPage,
  useEventEngine,
  useEventFilterOptions,
  useEventStream,
  type EventStreamFactory,
} from "../hooks/useDockerEvents";
import { PageIntro } from "../components/PageIntro";
import { Pagination } from "../components/Pagination";
import { ScrollArea } from "../components/DetailSection";
import {
  ConnectionStateBadge,
  EventResultBadge,
  EventTypeBadge,
  StreamStatusBadge,
} from "../components/EventBadges";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";

const PAGE_SIZE = 25;

/**
 * The Docker event log.
 *
 * Two views over the same data, and the distinction matters:
 *
 *   - LIVE shows what has arrived over SSE since the page opened. It is bounded
 *     at MAX_LIVE_EVENTS rows and is not a complete record.
 *   - HISTORY is the server-side paginated, filtered query. It is the complete
 *     record of what HarborMaster observed.
 *
 * Neither is an authoritative record of the HOST. Docker drops events while
 * nothing is listening and replays nothing across a daemon restart, so this is
 * an observation log. Current state lives in the Containers and Images views,
 * which read the inventory.
 */
export function Events({ streamFactory }: { streamFactory?: EventStreamFactory } = {}) {
  const engine = useEventEngine();
  const filters = useEventFilterOptions();

  const [query, setQuery] = useState<DockerEventQuery>({
    page: 1,
    pageSize: PAGE_SIZE,
  });
  const [live, setLive] = useState(true);
  const [paused, setPaused] = useState(false);

  const engineEnabled = engine.data?.enabled ?? false;

  const stream = useEventStream({
    enabled: live && engineEnabled,
    paused,
    ...(streamFactory ? { factory: streamFactory } : {}),
  });

  // Any filter change resets to page 1: staying on page 7 of a narrower result
  // set would show an empty table.
  const update = useCallback((patch: Partial<DockerEventQuery>) => {
    setQuery((current) => ({ ...current, ...patch, page: 1 }));
  }, []);

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Events"
        description="What the Docker daemon reported, in the order HarborMaster observed it. Events are hints that something may have changed; the inventory is rebuilt by inspecting Docker, never from an event's contents."
      />

      <EnginePanel engine={engine} stream={stream} />

      {engineEnabled ? (
        <LivePanel
          stream={stream}
          live={live}
          paused={paused}
          onToggleLive={() => setLive((current) => !current)}
          onTogglePause={() => setPaused((current) => !current)}
        />
      ) : null}

      <FilterBar
        query={query}
        onChange={update}
        actions={filters.data?.actions ?? []}
        projects={filters.data?.projects ?? []}
        types={filters.data?.types ?? []}
        results={filters.data?.results ?? []}
      />

      <HistoryTable
        query={query}
        onPageChange={(page) => setQuery((current) => ({ ...current, page }))}
      />
    </div>
  );
}

/** The server-side engine status: connection, reconnects, queue, retention. */
function EnginePanel({
  engine,
  stream,
}: {
  engine: ReturnType<typeof useEventEngine>;
  stream: ReturnType<typeof useEventStream>;
}) {
  if (engine.status === "loading") {
    return <LoadingState label="Loading event engine status" />;
  }
  if (engine.status === "disconnected") {
    return <DisconnectedState onRetry={engine.refresh} />;
  }
  if (engine.error) {
    return <ErrorState error={engine.error} onRetry={engine.refresh} />;
  }
  if (!engine.data) {
    return <LoadingState label="Loading event engine status" />;
  }

  const status = engine.data;

  if (!status.enabled) {
    return (
      <section
        aria-labelledby="engine-heading"
        className="rounded-xl border border-border-subtle bg-surface-raised p-5"
      >
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h2 id="engine-heading" className="text-lg font-semibold">
              Event engine
            </h2>
            <p className="mt-1 text-sm text-content-muted">
              The event engine is switched off by configuration. HarborMaster is
              keeping the inventory current by periodic reconciliation instead,
              which is a supported mode &mdash; not a fault. Set{" "}
              <code className="font-mono text-xs">HARBORMASTER_EVENTS_ENABLED=true</code>{" "}
              to receive live events.
            </p>
          </div>
          <ConnectionStateBadge state={status.state} />
        </div>
      </section>
    );
  }

  const disconnected = status.state !== "connected";

  return (
    <section
      aria-labelledby="engine-heading"
      className={`rounded-xl border p-5 ${
        disconnected || status.overflowPending
          ? "border-warn/40 bg-warn-soft"
          : "border-border-subtle bg-surface-raised"
      }`}
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 id="engine-heading" className="text-lg font-semibold">
            Event engine
          </h2>
          <p className="mt-1 text-sm text-content-muted">
            {describeEngine(status.state, status.overflowPending)}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <ConnectionStateBadge state={status.state} />
          <StreamStatusBadge status={stream.status} />
        </div>
      </div>

      <dl className="mt-4 grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">
        <Metric label="Last Docker event" value={formatTimestamp(status.lastEventAt)} />
        <Metric label="Last reconciliation" value={formatTimestamp(status.lastReconciliationAt)} />
        <Metric label="Reconnects" value={String(status.counters.reconnectCount)} />
        <Metric
          label="Queue"
          value={`${status.queueDepth} / ${status.queueCapacity}`}
        />
        <Metric label="Received" value={String(status.counters.eventsReceived)} />
        <Metric label="Stored" value={String(status.storedEvents)} />
        <Metric label="Deduplicated" value={String(status.counters.eventsDeduplicated)} />
        <Metric
          label="Dropped"
          value={String(status.counters.eventsDropped)}
          tone={status.counters.eventsDropped > 0 ? "warn" : "neutral"}
        />
      </dl>

      {status.lastError ? (
        <p className="mt-4 rounded-lg border border-border-subtle bg-surface-sunken px-3 py-2 text-sm">
          Last error: {status.lastError}
        </p>
      ) : null}

      {status.overflowPending ? (
        <p role="alert" className="mt-4 rounded-lg border border-warn/40 px-3 py-2 text-sm">
          The event queue overflowed, so some events were not recorded. A full
          reconciliation is running to restore the inventory. Event history has a
          gap; current state does not.
        </p>
      ) : null}
    </section>
  );
}

function describeEngine(state: string, overflow: boolean): string {
  if (overflow) {
    return "Events were dropped and a reconciliation is in progress.";
  }
  switch (state) {
    case "connected":
      return "Subscribed to the Docker event stream. Events trigger targeted inventory refreshes.";
    case "reconnecting":
      return "The event stream dropped. HarborMaster is reconnecting; periodic reconciliation keeps the inventory correct meanwhile.";
    case "connecting":
      return "Opening the Docker event stream.";
    default:
      return "The event engine is not running.";
  }
}

/** The bounded live list fed by SSE. */
function LivePanel({
  stream,
  live,
  paused,
  onToggleLive,
  onTogglePause,
}: {
  stream: ReturnType<typeof useEventStream>;
  live: boolean;
  paused: boolean;
  onToggleLive: () => void;
  onTogglePause: () => void;
}) {
  return (
    <section
      aria-labelledby="live-heading"
      className="rounded-xl border border-border-subtle bg-surface-raised"
    >
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border-subtle p-4">
        <div>
          <h2 id="live-heading" className="text-lg font-semibold">
            Live
          </h2>
          <p className="mt-1 text-sm text-content-muted">
            Events since this page opened, newest first. Capped at {MAX_LIVE_EVENTS}{" "}
            rows &mdash; the full record is in the history below.
          </p>
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <StreamStatusBadge status={stream.status} />
          <button
            type="button"
            onClick={onTogglePause}
            disabled={!live}
            aria-pressed={paused}
            className="rounded-lg border border-border-subtle px-3 py-1.5 text-sm font-medium transition-colors hover:bg-surface-sunken disabled:cursor-not-allowed disabled:opacity-40"
          >
            {paused ? "Resume" : "Pause"}
          </button>
          <button
            type="button"
            onClick={onToggleLive}
            aria-pressed={live}
            className="rounded-lg border border-border-subtle px-3 py-1.5 text-sm font-medium transition-colors hover:bg-surface-sunken"
          >
            {live ? "Disconnect" : "Connect"}
          </button>
        </div>
      </div>

      {stream.status === "reconnecting" ? (
        <p
          role="status"
          aria-live="polite"
          aria-label="Live stream status"
          className="border-b border-border-subtle bg-warn-soft px-4 py-2 text-sm"
        >
          The live connection dropped. The browser is retrying; the history below
          is unaffected.
        </p>
      ) : null}

      {paused ? (
        <p className="border-b border-border-subtle bg-surface-sunken px-4 py-2 text-sm text-content-muted">
          Paused. New events are still being received and recorded; this list is
          not moving.
        </p>
      ) : null}

      {stream.skipped > 0 ? (
        <p className="border-b border-border-subtle bg-warn-soft px-4 py-2 text-sm">
          {stream.skipped} event{stream.skipped === 1 ? " was" : "s were"} skipped
          when the stream reconnected. They are in the history below.
        </p>
      ) : null}

      {stream.events.length === 0 ? (
        <p className="px-4 py-8 text-center text-sm text-content-muted">
          {live
            ? "Waiting for events. Nothing has happened on the Docker host since this page opened."
            : "Live updates are off. Use Connect to subscribe."}
        </p>
      ) : (
        <EventTable events={stream.events} caption="Live events" />
      )}
    </section>
  );
}

function FilterBar({
  query,
  onChange,
  actions,
  projects,
  types,
  results,
}: {
  query: DockerEventQuery;
  onChange: (patch: Partial<DockerEventQuery>) => void;
  actions: string[];
  projects: string[];
  types: string[];
  results: string[];
}) {
  return (
    <section
      aria-label="Filters"
      className="grid gap-3 rounded-xl border border-border-subtle bg-surface-raised p-4 sm:grid-cols-2 lg:grid-cols-4"
    >
      <label className="flex flex-col gap-1 text-sm">
        <span className="text-content-muted">Search</span>
        <input
          type="search"
          value={query.search ?? ""}
          onChange={(event) => onChange({ search: event.target.value })}
          placeholder="Resource name or ID"
          className="rounded-lg border border-border-subtle bg-surface px-3 py-2"
        />
      </label>

      <label className="flex flex-col gap-1 text-sm">
        <span className="text-content-muted">Type</span>
        <select
          value={query.type?.[0] ?? ""}
          onChange={(event) =>
            onChange({ type: event.target.value ? [event.target.value] : [] })
          }
          className="rounded-lg border border-border-subtle bg-surface px-3 py-2"
        >
          <option value="">All types</option>
          {types.map((type) => (
            <option key={type} value={type}>
              {type}
            </option>
          ))}
        </select>
      </label>

      <label className="flex flex-col gap-1 text-sm">
        <span className="text-content-muted">Action</span>
        <select
          value={query.action?.[0] ?? ""}
          onChange={(event) =>
            onChange({ action: event.target.value ? [event.target.value] : [] })
          }
          className="rounded-lg border border-border-subtle bg-surface px-3 py-2"
        >
          <option value="">All actions</option>
          {actions.map((action) => (
            <option key={action} value={action}>
              {action}
            </option>
          ))}
        </select>
      </label>

      <label className="flex flex-col gap-1 text-sm">
        <span className="text-content-muted">Compose project</span>
        <select
          value={query.project ?? ""}
          onChange={(event) => onChange({ project: event.target.value })}
          className="rounded-lg border border-border-subtle bg-surface px-3 py-2"
        >
          <option value="">All projects</option>
          {projects.map((project) => (
            <option key={project} value={project}>
              {project}
            </option>
          ))}
        </select>
      </label>

      <label className="flex flex-col gap-1 text-sm lg:col-span-2">
        <span className="text-content-muted">Processing status</span>
        <select
          value={query.result?.[0] ?? ""}
          onChange={(event) =>
            onChange({ result: event.target.value ? [event.target.value] : [] })
          }
          className="rounded-lg border border-border-subtle bg-surface px-3 py-2"
        >
          <option value="">Any status</option>
          {results.map((result) => (
            <option key={result} value={result}>
              {result}
            </option>
          ))}
        </select>
      </label>
    </section>
  );
}

/** The server-side paginated history. */
function HistoryTable({
  query,
  onPageChange,
}: {
  query: DockerEventQuery;
  onPageChange: (page: number) => void;
}) {
  const page = useDockerEventPage(query);
  const rows = useMemo(() => page.data?.items ?? [], [page.data]);

  if (page.status === "loading") {
    return <LoadingState label="Loading event history" />;
  }
  if (page.status === "disconnected") {
    return <DisconnectedState onRetry={page.refresh} />;
  }
  if (page.error) {
    return <ErrorState error={page.error} onRetry={page.refresh} />;
  }
  if (!page.data) {
    return <LoadingState label="Loading event history" />;
  }

  if (rows.length === 0) {
    const filtered =
      Boolean(query.search) ||
      Boolean(query.project) ||
      (query.type?.length ?? 0) > 0 ||
      (query.action?.length ?? 0) > 0 ||
      (query.result?.length ?? 0) > 0;

    return (
      <EmptyState
        title={filtered ? "No events match these filters" : "No events recorded yet"}
        description={
          filtered
            ? "Try widening or clearing the filters above."
            : "HarborMaster has not observed any Docker events. Event history begins when the engine first connects; nothing that happened before then was seen."
        }
      />
    );
  }

  return (
    <section
      aria-labelledby="history-heading"
      className="rounded-xl border border-border-subtle bg-surface-raised"
    >
      <div className="border-b border-border-subtle p-4">
        <h2 id="history-heading" className="text-lg font-semibold">
          History
        </h2>
        <p className="mt-1 text-sm text-content-muted">
          Everything HarborMaster recorded, filtered and paged by the server.
        </p>
      </div>

      <EventTable events={rows} caption="Event history" />

      <Pagination
        pagination={page.data.pagination}
        onPageChange={onPageChange}
        busy={page.refreshing}
      />
    </section>
  );
}

/** The shared event table, used by both the live and history views. */
function EventTable({ events, caption }: { events: DockerEvent[]; caption: string }) {
  return (
    <ScrollArea>
      <table className="w-full min-w-[52rem] text-left text-sm">
        <caption className="sr-only">{caption}</caption>
        <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
          <tr>
            <th scope="col" className="px-4 py-3 font-medium">
              Docker time
            </th>
            <th scope="col" className="px-4 py-3 font-medium">
              Observed
            </th>
            <th scope="col" className="px-4 py-3 font-medium">
              Type
            </th>
            <th scope="col" className="px-4 py-3 font-medium">
              Action
            </th>
            <th scope="col" className="px-4 py-3 font-medium">
              Resource
            </th>
            <th scope="col" className="px-4 py-3 font-medium">
              Compose
            </th>
            <th scope="col" className="px-4 py-3 font-medium">
              Status
            </th>
          </tr>
        </thead>
        <tbody>
          {events.map((event) => (
            <EventRow key={event.sequence} event={event} />
          ))}
        </tbody>
      </table>
    </ScrollArea>
  );
}

function EventRow({ event }: { event: DockerEvent }) {
  return (
    <tr className="border-b border-border-subtle last:border-0 hover:bg-surface-sunken">
      {/* Two timestamps, always both. They diverge by the stream latency
          normally and by a great deal after a reconnect, which is exactly the
          gap an operator needs to see. */}
      <td className="px-4 py-3 text-xs text-content-muted" title={event.dockerTime}>
        {formatTimestamp(event.dockerTime)}
      </td>
      <td className="px-4 py-3 text-xs text-content-muted" title={event.observedAt}>
        {formatTimestamp(event.observedAt)}
      </td>
      <td className="px-4 py-3">
        <EventTypeBadge type={event.type} />
      </td>
      <td className="px-4 py-3 font-mono text-xs">{event.action || "â€”"}</td>
      <td className="px-4 py-3">
        <ResourceCell event={event} />
      </td>
      <td className="px-4 py-3 text-xs">
        {event.composeProject ? (
          <>
            <span className="block">{event.composeProject}</span>
            <span className="text-content-muted">{event.composeService}</span>
          </>
        ) : (
          <span className="text-content-muted">â€”</span>
        )}
      </td>
      <td className="px-4 py-3">
        <EventResultBadge result={event.result} />
        {event.error ? (
          <p className="mt-1 max-w-xs text-xs text-content-muted">{event.error}</p>
        ) : null}
      </td>
    </tr>
  );
}

/**
 * The resource an event concerns, linked where HarborMaster can resolve it.
 *
 * A container link works even after a destroy: the row is retained and marked
 * absent, which is the useful answer rather than a dead link.
 */
function ResourceCell({ event }: { event: DockerEvent }) {
  const label = event.actorName || event.actorId || "â€”";

  if (event.type === "container" && event.actorId) {
    return (
      <Link
        to={`/containers/${event.actorId}`}
        className="font-medium text-accent hover:underline"
      >
        {label}
      </Link>
    );
  }
  if (event.type === "image" && event.actorId) {
    return (
      <Link
        to={`/images`}
        className="font-medium text-accent hover:underline"
        title={event.actorId}
      >
        {label}
      </Link>
    );
  }
  return <span className="break-all">{label}</span>;
}

function Metric({
  label,
  value,
  tone = "neutral",
}: {
  label: string;
  value: string;
  tone?: "neutral" | "warn";
}) {
  return (
    <div>
      <dt className="text-content-muted">{label}</dt>
      <dd className={tone === "warn" ? "mt-0.5 font-medium text-warn" : "mt-0.5"}>
        {value}
      </dd>
    </div>
  );
}

/** The shared absolute-time format. See api/presentation.ts. */
const formatTimestamp = formatMoment;
