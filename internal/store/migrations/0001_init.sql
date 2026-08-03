-- 0001_init: snapshot and event foundation.
--
-- HarborMaster is read-only with respect to Docker: container inventory is read
-- live from the Engine and never cached here. Only HarborMaster's own records
-- -- configuration snapshots and the audit log -- are persisted.

CREATE TABLE snapshots (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    container_id   TEXT    NOT NULL,
    container_name TEXT    NOT NULL,
    source         TEXT    NOT NULL,
    image          TEXT    NOT NULL DEFAULT '',
    image_id       TEXT    NOT NULL DEFAULT '',
    -- Normalised container configuration, JSON encoded.
    spec           BLOB    NOT NULL,
    -- Hex-encoded SHA-256 of spec, for drift detection without re-parsing.
    checksum       TEXT    NOT NULL,
    note           TEXT    NOT NULL DEFAULT '',
    created_at     TEXT    NOT NULL,

    CHECK (source IN ('manual', 'scheduled', 'pre_update')),
    CHECK (length(checksum) = 64)
);

CREATE INDEX idx_snapshots_container_created
    ON snapshots (container_id, created_at DESC);

-- One snapshot per container per distinct configuration keeps the history
-- meaningful instead of filling up with identical captures.
CREATE UNIQUE INDEX idx_snapshots_container_checksum
    ON snapshots (container_id, checksum);

CREATE TABLE events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    type         TEXT    NOT NULL,
    severity     TEXT    NOT NULL,
    container_id TEXT    NOT NULL DEFAULT '',
    message      TEXT    NOT NULL,
    -- Optional structured context, JSON encoded. Never holds credentials or
    -- environment-variable values.
    details      BLOB,
    occurred_at  TEXT    NOT NULL,

    CHECK (severity IN ('info', 'warning', 'error'))
);

CREATE INDEX idx_events_occurred_at ON events (occurred_at DESC);
CREATE INDEX idx_events_container ON events (container_id, occurred_at DESC);
