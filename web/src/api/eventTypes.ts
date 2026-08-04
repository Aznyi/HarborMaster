/**
 * Types mirroring HarborMaster's Docker event API.
 *
 * Hand-maintained against `api/openapi.yaml`, which is the contract. These are
 * HarborMaster's own shapes: the API exposes no Docker SDK type, so none
 * appears here either.
 *
 * A Docker event is a HINT that something on the host may have changed, not an
 * authoritative record of what is there. Docker inspection and HarborMaster's
 * persistence remain authoritative, which is why the containers and images
 * views read the inventory rather than replaying this history.
 */

/** Object kinds an event can describe. "other" covers anything unmodelled. */
export type DockerEventType =
  | "container"
  | "image"
  | "network"
  | "volume"
  | "daemon"
  | "other";

/**
 * What HarborMaster did with an event.
 *
 * - processed: recorded, and a refresh was requested where applicable
 * - deduplicated: an identical event was already seen (counted, not stored)
 * - ignored: recorded, no inventory consequence
 * - warning: recorded, but could not be mapped, so a reconciliation was asked for
 * - failed: processing raised an error; the event is still stored
 */
export type EventProcessingResult =
  | "processed"
  | "deduplicated"
  | "ignored"
  | "warning"
  | "failed";

/** The synchronization an event asked for. */
export type RefreshRequest =
  | "none"
  | "container"
  | "container_absent"
  | "image"
  | "image_catalog"
  | "networks"
  | "volumes"
  | "full";

/** The event stream's connection state. */
export type EventConnectionState =
  | "disabled"
  | "connecting"
  | "connected"
  | "reconnecting"
  | "stopped";

export interface DockerEvent {
  /**
   * HarborMaster's monotonic local ordering, assigned on persistence. It is the
   * SSE event ID and the deterministic tiebreak for every list query.
   *
   * Docker guarantees no global ordering across a reconnect, so this records the
   * order HarborMaster OBSERVED events in -- the only order it can claim.
   */
  sequence: number;
  fingerprint: string;
  hostId: string;
  type: DockerEventType;
  action: string;

  actorId?: string;
  actorName?: string;
  scope?: string;

  /**
   * Actor attributes AFTER redaction. A value whose key matched a sensitive
   * pattern carries "********", never the secret.
   */
  attributes: Record<string, string>;

  composeProject?: string;
  composeService?: string;
  harbormasterLabels?: Record<string, string>;

  /** When the daemon says it happened. */
  dockerTime: string;
  dockerTimeNano?: number;
  /**
   * When HarborMaster read it off the stream. It differs from dockerTime by the
   * stream latency, and by much more after a reconnect -- which is exactly why
   * both are shown.
   */
  observedAt: string;

  result: EventProcessingResult;
  refreshRequested: RefreshRequest;
  error?: string;
  connectionState?: EventConnectionState;
  createdAt: string;
}

/** Links to the resources an event concerns, where they can be resolved. */
export interface EventLinks {
  container?: string;
  image?: string;
}

export interface DockerEventDetail extends DockerEvent {
  links: EventLinks;
  /** Always true. Stated so a client is not left to assume it. */
  redacted: boolean;
}

export interface EventEngineCounters {
  eventsReceived: number;
  eventsPersisted: number;
  eventsDeduplicated: number;
  eventsDropped: number;
  targetedRefreshes: number;
  fullReconciliations: number;
  reconnectCount: number;
  refreshFailures: number;
  eventsPruned: number;
}

export interface EventRetentionPolicy {
  maxAgeSeconds: number;
  maxCount: number;
  intervalSeconds: number;
}

export interface EventEngineStatus {
  /**
   * Reflects configuration. A disabled engine is a supported mode in which
   * periodic reconciliation carries the inventory alone -- not a fault.
   */
  enabled: boolean;
  state: EventConnectionState;

  connectedSince?: string;
  lastConnectedAt?: string;
  lastDisconnectedAt?: string;
  lastEventAt?: string;
  lastReconciliationAt?: string;

  currentBackoffMs: number;

  queueDepth: number;
  queueCapacity: number;
  pendingRefreshes: number;
  /** True while a queue overflow has forced a reconciliation that has not finished. */
  overflowPending: boolean;

  subscribers: number;
  subscriberLimit: number;

  counters: EventEngineCounters;
  lastError?: string;
  retention: EventRetentionPolicy;
  storedEvents: number;
}

export interface EventFilterOptions {
  types: string[];
  actions: string[];
  results: string[];
  projects: string[];
  sortFields: string[];
}

/** Query parameters accepted by the event list endpoint. */
export interface DockerEventQuery {
  page?: number;
  pageSize?: number;
  type?: string[];
  action?: string[];
  result?: string[];
  actorId?: string;
  project?: string;
  service?: string;
  search?: string;
  /** RFC 3339 timestamps. */
  since?: string;
  until?: string;
  sort?: string;
  direction?: "asc" | "desc";
}

/** The opening SSE frame. */
export interface StreamReadyPayload {
  lastEventId: number;
  replayed: number;
  redacted: boolean;
  notice: string;
}

/** Sent when a Last-Event-ID replay was capped. */
export interface StreamTruncatedPayload {
  skipped: number;
  limit: number;
  notice: string;
}

/** How the browser's connection to the SSE endpoint is doing. */
export type StreamStatus =
  | "idle"
  | "connecting"
  | "open"
  | "reconnecting"
  | "unavailable";
