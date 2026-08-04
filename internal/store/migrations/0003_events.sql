-- 0003_events: Docker event history and event-engine state.
--
-- This is an OBSERVATIONAL log, not an authoritative record of the host.
--
-- The Docker daemon makes no durability promise about its event stream: events
-- emitted while nothing is listening are gone, nothing survives a daemon
-- restart, and ordering holds only within one connection. So rows here answer
-- "what did HarborMaster observe, and when" -- never "what happened on the
-- host". Current state lives in the tables from 0002, which are rebuilt by
-- inspection, and a periodic full reconciliation is what repairs them after
-- events are missed.
--
-- Distinct from the `events` table in 0001, which is HarborMaster's own audit
-- log of its own actions. These two are not the same concept and must not be
-- merged: one records what HarborMaster did, the other what Docker reported.
--
-- Timestamps are RFC3339Nano UTC text throughout, matching 0001 and 0002.

-- ------------------------------------------- widen the refresh trigger --

-- 0002 constrained inventory_refreshes.trigger to startup/periodic/manual.
-- Phase 2.5 adds 'reconcile': a full sweep the event engine escalated to, after
-- a reconnect, a queue overflow, or an event it could not map onto a resource.
-- It is a separate value from 'periodic' on purpose -- one means "all is well",
-- the other means "events were missed" -- and collapsing them would hide the
-- distinction an operator most wants.
--
-- SQLite cannot alter a CHECK constraint in place, so the table is rebuilt.
-- Nothing references inventory_refreshes, so the drop-and-rename is safe; its
-- own foreign key to hosts is satisfied by the copied rows.

CREATE TABLE inventory_refreshes_new (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id    TEXT    NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    generation INTEGER NOT NULL,
    trigger    TEXT    NOT NULL,
    state      TEXT    NOT NULL,

    started_at  TEXT    NOT NULL,
    finished_at TEXT,
    duration_ms INTEGER NOT NULL DEFAULT 0,

    containers_listed    INTEGER NOT NULL DEFAULT 0,
    containers_inspected INTEGER NOT NULL DEFAULT 0,
    containers_failed    INTEGER NOT NULL DEFAULT 0,
    images_inspected     INTEGER NOT NULL DEFAULT 0,
    networks_listed      INTEGER NOT NULL DEFAULT 0,
    volumes_listed       INTEGER NOT NULL DEFAULT 0,
    warning_count        INTEGER NOT NULL DEFAULT 0,

    checksum TEXT NOT NULL DEFAULT '',
    error    TEXT NOT NULL DEFAULT '',

    CHECK (state IN ('running', 'succeeded', 'failed')),
    CHECK (trigger IN ('startup', 'periodic', 'manual', 'reconcile'))
);

INSERT INTO inventory_refreshes_new
    (id, host_id, generation, trigger, state, started_at, finished_at,
     duration_ms, containers_listed, containers_inspected, containers_failed,
     images_inspected, networks_listed, volumes_listed, warning_count,
     checksum, error)
SELECT
    id, host_id, generation, trigger, state, started_at, finished_at,
    duration_ms, containers_listed, containers_inspected, containers_failed,
    images_inspected, networks_listed, volumes_listed, warning_count,
    checksum, error
FROM inventory_refreshes;

DROP TABLE inventory_refreshes;

ALTER TABLE inventory_refreshes_new RENAME TO inventory_refreshes;

-- Recreated verbatim from 0002; the rebuild dropped them with the old table.
CREATE INDEX idx_refreshes_started ON inventory_refreshes (host_id, started_at DESC);
CREATE INDEX idx_refreshes_succeeded ON inventory_refreshes (state, generation DESC);

-- --------------------------------------------------------- docker events --

CREATE TABLE docker_events (
    -- The monotonic local sequence. It is the observation order, the SSE event
    -- ID, and the deterministic tiebreak for every list query. AUTOINCREMENT
    -- rather than plain rowid so a deleted row's number is never reused: an
    -- SSE client resuming from Last-Event-ID must never be handed a different
    -- event under a sequence it has already seen.
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,

    -- The deterministic deduplication identity. UNIQUE makes the database the
    -- last line of defence behind the in-memory window: a duplicate that
    -- arrives after the window expired, or after a restart, is still rejected
    -- rather than stored twice.
    fingerprint TEXT NOT NULL UNIQUE,

    host_id    TEXT NOT NULL,
    event_type TEXT NOT NULL,
    -- Free text, not a CHECK constraint. Daemons differ by version in which
    -- actions they emit; constraining the vocabulary here would turn an
    -- unfamiliar daemon into a stream of failed inserts.
    action     TEXT NOT NULL DEFAULT '',

    actor_id      TEXT NOT NULL DEFAULT '',
    resource_name TEXT NOT NULL DEFAULT '',
    scope         TEXT NOT NULL DEFAULT 'local',

    compose_project TEXT NOT NULL DEFAULT '',
    compose_service TEXT NOT NULL DEFAULT '',

    -- Both timestamps are kept. docker_time is when the daemon says it
    -- happened; observed_at is when HarborMaster read it. They diverge by the
    -- stream latency normally and by a great deal after a reconnect, which is
    -- exactly the gap an operator needs to see.
    docker_time      TEXT,
    docker_time_nano INTEGER NOT NULL DEFAULT 0,
    observed_at      TEXT NOT NULL,

    -- Actor attributes as JSON, AFTER redaction. Values whose key matches a
    -- sensitive-name pattern hold the mask, not the secret. Nothing
    -- unredacted may be written to this column: it is served by an
    -- unauthenticated REST endpoint and an unauthenticated SSE stream.
    attributes TEXT NOT NULL DEFAULT '{}',

    -- What HarborMaster did with the event, and what synchronization it asked
    -- for. Constrained, unlike `action`, because this vocabulary is
    -- HarborMaster's own and a value outside it would be a bug.
    result       TEXT NOT NULL DEFAULT 'processed',
    refresh_type TEXT NOT NULL DEFAULT 'none',
    -- Sanitised processing failure. Never a raw Docker error: those can name
    -- the socket path.
    error        TEXT NOT NULL DEFAULT '',
    -- The engine's connection state when this event was processed, so a gap in
    -- the history can be read alongside the reconnect that caused it.
    connection_state TEXT NOT NULL DEFAULT '',

    created_at TEXT NOT NULL,

    CHECK (result IN ('processed', 'deduplicated', 'ignored', 'warning', 'failed')),
    CHECK (refresh_type IN ('none', 'container', 'container_absent', 'image',
                            'image_catalog', 'networks', 'volumes', 'full')),
    CHECK (event_type IN ('container', 'image', 'network', 'volume', 'daemon', 'other'))
);

-- The list endpoint's default ordering and its filters. Each index leads with
-- the filtered column and ends in `sequence DESC`, so a filtered page is served
-- by one index rather than a filter followed by a sort.
CREATE INDEX idx_docker_events_host_seq ON docker_events (host_id, sequence DESC);
CREATE INDEX idx_docker_events_type ON docker_events (event_type, sequence DESC);
CREATE INDEX idx_docker_events_action ON docker_events (action, sequence DESC);
CREATE INDEX idx_docker_events_actor ON docker_events (actor_id, sequence DESC);
CREATE INDEX idx_docker_events_docker_time ON docker_events (docker_time DESC, sequence DESC);
-- Retention prunes by observed_at, and the time-range filter uses it too.
CREATE INDEX idx_docker_events_observed ON docker_events (observed_at, sequence);
CREATE INDEX idx_docker_events_project ON docker_events (compose_project, sequence DESC);
CREATE INDEX idx_docker_events_service ON docker_events (compose_service, sequence DESC);
CREATE INDEX idx_docker_events_result ON docker_events (result, sequence DESC);

-- --------------------------------------------------- event engine state --

-- One row per host, holding only what is worth surviving a restart: when the
-- stream was last up, and when the last full reconciliation ran. Live counters
-- stay in memory. Persisting them per event would put a write on the hot path
-- to report a number nobody reads more than once a minute.
CREATE TABLE event_engine_state (
    host_id TEXT PRIMARY KEY,

    last_connected_at    TEXT,
    last_disconnected_at TEXT,
    last_event_at        TEXT,
    last_reconciled_at   TEXT,

    -- Cumulative across restarts, so "has this been flapping" survives one.
    reconnect_count INTEGER NOT NULL DEFAULT 0,
    -- Sanitised. Never a raw Docker error.
    last_error      TEXT NOT NULL DEFAULT '',

    updated_at TEXT NOT NULL
);
