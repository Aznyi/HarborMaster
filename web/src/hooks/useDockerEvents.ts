import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import {
  eventStreamUrl,
  getEventEngine,
  getEventFilterOptions,
  listDockerEvents,
} from "../api/client";
import type {
  DockerEvent,
  DockerEventQuery,
  EventEngineStatus,
  EventFilterOptions,
  StreamStatus,
} from "../api/eventTypes";
import type { ListResponse } from "../api/inventoryTypes";
import { useApiResource, type ResourceState } from "./useApiResource";

/**
 * How often the event-engine status is re-read.
 *
 * Faster than the inventory poll: connection state is the thing an operator
 * watches during a daemon restart, and a ten-second lag there is the difference
 * between "it reconnected" and "is it stuck?".
 */
export const EVENT_ENGINE_POLL_INTERVAL_MS = 5_000;

/**
 * The ceiling on live rows held in the browser.
 *
 * A busy host emits events faster than anyone reads them, and an unbounded list
 * would grow until the tab dies. Older rows fall off the end; the paginated
 * history endpoint is where the full record lives.
 */
export const MAX_LIVE_EVENTS = 250;

/** Polls GET /api/v1/event-engine. */
export function useEventEngine(
  pollIntervalMs = EVENT_ENGINE_POLL_INTERVAL_MS,
): ResourceState<EventEngineStatus> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getEventEngine({ signal }),
    [],
  );
  return useApiResource<EventEngineStatus>(fetcher, { pollIntervalMs });
}

/**
 * Fetches a page of event history.
 *
 * The query is serialised into the fetcher's identity, so changing a filter
 * refetches from the server. Nothing is filtered or paged in the browser.
 */
export function useDockerEventPage(
  query: DockerEventQuery,
): ResourceState<ListResponse<DockerEvent>> {
  const key = useMemo(() => JSON.stringify(query), [query]);

  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) =>
      listDockerEvents(JSON.parse(key) as DockerEventQuery, { signal }),
    [key],
  );
  return useApiResource<ListResponse<DockerEvent>>(fetcher, { key });
}

/** Fetches the filter vocabularies event history actually contains. */
export function useEventFilterOptions(): ResourceState<EventFilterOptions> {
  const fetcher = useCallback(
    ({ signal }: { signal: AbortSignal }) => getEventFilterOptions({ signal }),
    [],
  );
  return useApiResource<EventFilterOptions>(fetcher);
}

/**
 * The minimum of EventSource this hook uses.
 *
 * Declared rather than taken from lib.dom so a test can supply a fake without
 * implementing the whole interface, and so the hook states exactly what it
 * depends on.
 */
export interface EventStreamLike {
  addEventListener(type: string, listener: (event: MessageEvent) => void): void;
  close(): void;
  onerror: ((event: Event) => void) | null;
  onopen: ((event: Event) => void) | null;
}

export type EventStreamFactory = (url: string) => EventStreamLike;

export interface UseEventStreamOptions {
  /** Opens the stream when true. Set false for a disabled engine. */
  enabled: boolean;
  /**
   * Stops appending to the live list while true. The connection stays OPEN --
   * closing it on pause would mean reconnecting and replaying on resume, and
   * the operator asked to stop the list moving, not to stop listening.
   */
  paused?: boolean;
  /** The live-row ceiling. Defaults to MAX_LIVE_EVENTS. */
  maxEvents?: number;
  /**
   * Builds the underlying stream. Defaults to the browser's EventSource.
   * Injected so tests never depend on a real network connection.
   */
  factory?: EventStreamFactory;
}

export interface EventStreamState {
  status: StreamStatus;
  /** Newest first, capped at maxEvents. */
  events: DockerEvent[];
  /** The highest sequence received, which is the reconnect resume point. */
  lastEventId: number;
  /** How many events the server said a replay had to skip. */
  skipped: number;
  /** Clears the live list without touching the connection. */
  clear: () => void;
}

/**
 * Subscribes to the live event stream.
 *
 * Behaviour worth knowing:
 *
 *   - The live list is capped. See MAX_LIVE_EVENTS.
 *   - Pausing stops the list moving but keeps the connection, so resuming does
 *     not trigger a reconnect-and-replay.
 *   - Reconnection is the browser's own: EventSource retries on its own timer,
 *     using the `retry:` hint the server sends, and resends Last-Event-ID. This
 *     hook does not implement a second reconnect loop on top.
 */
export function useEventStream({
  enabled,
  paused = false,
  maxEvents = MAX_LIVE_EVENTS,
  factory,
}: UseEventStreamOptions): EventStreamState {
  const [status, setStatus] = useState<StreamStatus>("idle");
  const [events, setEvents] = useState<DockerEvent[]>([]);
  const [lastEventId, setLastEventId] = useState(0);
  const [skipped, setSkipped] = useState(0);

  // Held in a ref so toggling pause does not tear down and rebuild the
  // connection: the effect below must not depend on it.
  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  // Likewise: the resume point changes on every event, and restarting the
  // stream each time would defeat the purpose.
  const lastEventIdRef = useRef(0);

  const clear = useCallback(() => setEvents([]), []);

  useEffect(() => {
    if (!enabled) {
      setStatus("unavailable");
      return;
    }

    const create: EventStreamFactory =
      factory ??
      ((url) => new EventSource(url) as unknown as EventStreamLike);

    setStatus("connecting");

    let source: EventStreamLike;
    try {
      source = create(eventStreamUrl(lastEventIdRef.current));
    } catch {
      // No EventSource in this environment, or the URL was rejected. The page
      // still works from the paginated history.
      setStatus("unavailable");
      return;
    }

    let closed = false;

    source.onopen = () => {
      if (!closed) setStatus("open");
    };

    source.onerror = () => {
      // EventSource retries on its own, so this is "reconnecting", not "dead".
      // Reporting it as an error would have the UI claim a failure the browser
      // is already recovering from.
      if (!closed) setStatus("reconnecting");
    };

    source.addEventListener("ready", (message: MessageEvent) => {
      if (closed) return;
      setStatus("open");
      try {
        const payload = JSON.parse(message.data as string) as { lastEventId?: number };
        if (typeof payload.lastEventId === "number" && payload.lastEventId > 0) {
          lastEventIdRef.current = payload.lastEventId;
          setLastEventId(payload.lastEventId);
        }
      } catch {
        // A frame that will not parse is dropped rather than crashing the view.
      }
    });

    source.addEventListener("replay-truncated", (message: MessageEvent) => {
      if (closed) return;
      try {
        const payload = JSON.parse(message.data as string) as { skipped?: number };
        if (typeof payload.skipped === "number") {
          setSkipped((current) => current + payload.skipped!);
        }
      } catch {
        // Ignored, as above.
      }
    });

    source.addEventListener("docker-event", (message: MessageEvent) => {
      if (closed) return;

      let event: DockerEvent;
      try {
        event = JSON.parse(message.data as string) as DockerEvent;
      } catch {
        return;
      }

      // The resume point advances even while paused, so resuming does not
      // replay what arrived in the meantime.
      if (event.sequence > lastEventIdRef.current) {
        lastEventIdRef.current = event.sequence;
        setLastEventId(event.sequence);
      }

      if (pausedRef.current) return;

      setEvents((current) => {
        // A reconnect can replay an event already held. Dropping it keeps the
        // list free of duplicates without a second dedup layer.
        if (current.some((existing) => existing.sequence === event.sequence)) {
          return current;
        }
        return [event, ...current].slice(0, maxEvents);
      });
    });

    return () => {
      closed = true;
      source.close();
      setStatus("idle");
    };
    // `paused` is intentionally absent: it is read through a ref so toggling it
    // never reopens the connection.
  }, [enabled, maxEvents, factory]);

  return { status, events, lastEventId, skipped, clear };
}
