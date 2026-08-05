-- 0005_drift: configuration drift detection.
--
-- HarborMaster remains read-only with respect to Docker. Drift detection READS
-- the inventory, READS an immutable snapshot, compares them, and writes the
-- differences here. Nothing in this migration grants any ability to create,
-- change, remove, or restore a container, and there is no remediation column
-- because there is no remediation.
--
-- Timestamps are RFC3339Nano UTC text throughout, matching 0001 through 0004.

-- --------------------------------------------------------- drift records --

-- One row per FIELD that differs between a container's baseline snapshot and
-- its configuration at evaluation time.
--
-- # No duplicated snapshot data
--
-- A drift row references the baseline by id and stores nothing that the
-- snapshot already holds. It carries the two rendered values for the one field
-- that moved and nothing else: no document, no checksum, no image identity, no
-- copy of the spec. Duplicating any of that would create a second source of
-- truth for evidence that is supposed to be immutable, and the two copies
-- would eventually disagree.
--
-- # Identity
--
-- (container_id, snapshot_id, category, field) identifies a drift. Deliberately
-- NOT the values: a field whose value keeps moving is one long-lived record
-- rather than a new record per evaluation, which is what makes an operator's
-- "ignored" survive the next evaluation and what keeps an event storm from
-- growing the table.
CREATE TABLE drift_records (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id      TEXT    NOT NULL DEFAULT 'local',

    container_id   TEXT NOT NULL,
    -- Denormalised so a list query needs no join, and so a record stays
    -- readable after the container it describes is gone.
    container_name TEXT NOT NULL DEFAULT '',
    -- The baseline. ON DELETE CASCADE: a drift measured against a snapshot
    -- that retention has pruned is meaningless, because the thing it was
    -- measured against no longer exists to compare with.
    snapshot_id  INTEGER NOT NULL REFERENCES snapshots (id) ON DELETE CASCADE,

    -- first seen, most recently seen, and when it stopped being seen.
    detected_at  TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    resolved_at  TEXT,
    -- The inventory the comparison read, so a record ties back to the exact
    -- observation that produced it.
    inventory_generation INTEGER NOT NULL DEFAULT 0,

    category TEXT NOT NULL,
    -- Engine-generated: a key from the diff engine such as 'privileged' or
    -- 'image.digest'. Never caller text.
    field    TEXT NOT NULL,
    kind     TEXT NOT NULL,
    severity TEXT NOT NULL,

    -- The two rendered values for this one field.
    --
    -- BOTH ARE EMPTY WHEN sensitive = 1, enforced by the CHECK below. This is
    -- the fourth of four independent layers keeping a secret out of the
    -- database: the diff engine withholds the value, the domain model has
    -- nowhere to put it, the repository blanks it again, and this constraint
    -- refuses the row. Any one of them alone would be enough; all four exist
    -- because a leaked credential cannot be un-leaked.
    previous_value TEXT NOT NULL DEFAULT '',
    current_value  TEXT NOT NULL DEFAULT '',
    sensitive      INTEGER NOT NULL DEFAULT 0,

    status TEXT NOT NULL DEFAULT 'active',
    -- Engine prose from a fixed set of phrases, explaining the ranking. Never
    -- an error string, never a value, never caller input.
    reason TEXT NOT NULL DEFAULT '',

    -- Operator annotation from a status change. Length-bounded and UTF-8
    -- validated before it reaches this column.
    note              TEXT NOT NULL DEFAULT '',
    status_changed_at TEXT,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    CHECK (category IN ('image', 'process', 'environment', 'labels', 'ports',
                        'mounts', 'networks', 'restart', 'health', 'logging',
                        'resources', 'security', 'compose', 'metadata')),
    CHECK (severity IN ('critical', 'high', 'medium', 'low')),
    CHECK (status IN ('active', 'resolved', 'acknowledged', 'ignored', 'expected')),
    CHECK (kind IN ('added', 'removed', 'modified', 'unverifiable')),
    CHECK (sensitive IN (0, 1)),
    -- The constraint that makes a secret unstorable.
    CHECK (sensitive = 0 OR (previous_value = '' AND current_value = ''))
);

-- The identity index. UNIQUE, so re-evaluating a container cannot create a
-- second row for the same field: the engine upserts against this key, which is
-- what bounds table growth under an event storm.
CREATE UNIQUE INDEX idx_drift_identity
    ON drift_records (container_id, snapshot_id, category, field);

-- The list endpoint's default ordering and its filters. Each index leads with
-- the filtered column and ends in id DESC, so a filtered page is served by one
-- index rather than a filter followed by a sort.
CREATE INDEX idx_drift_status_detected ON drift_records (status, detected_at DESC, id DESC);
CREATE INDEX idx_drift_severity        ON drift_records (severity, detected_at DESC, id DESC);
CREATE INDEX idx_drift_container       ON drift_records (container_id, status, id DESC);
CREATE INDEX idx_drift_category        ON drift_records (category, status, id DESC);
CREATE INDEX idx_drift_snapshot        ON drift_records (snapshot_id);
-- Serves the summary's "distinct containers with open drift" without a scan.
CREATE INDEX idx_drift_open            ON drift_records (status, container_id);

-- ----------------------------------------------------- drift evaluations --

-- One row per container, holding the most recent comparison ATTEMPT.
--
-- Separate from the records because an evaluation that found nothing is still
-- a fact worth storing. Without this table, "no drift rows for this container"
-- is ambiguous between "checked, and it matches its baseline" and "never
-- checked" -- and telling an operator their estate is clean when most of it
-- was never examined is the worst thing a posture dashboard can do.
--
-- One row per container rather than a history: the current state is what the
-- dashboard reports, and an unbounded audit trail of evaluations would grow
-- with every event on a busy host. The drift records themselves carry the
-- history that matters, through detected_at and resolved_at.
CREATE TABLE drift_evaluations (
    container_id   TEXT PRIMARY KEY,
    host_id        TEXT NOT NULL DEFAULT 'local',
    container_name TEXT NOT NULL DEFAULT '',
    -- 0 when no baseline snapshot existed, in which case complete is 0 and
    -- reason says so.
    snapshot_id    INTEGER NOT NULL DEFAULT 0,

    evaluated_at         TEXT    NOT NULL,
    inventory_generation INTEGER NOT NULL DEFAULT 0,
    drift_count          INTEGER NOT NULL DEFAULT 0,

    -- complete = 0 means the comparison did not examine everything, so
    -- drift_count is a floor rather than a total. The summary surfaces this
    -- rather than averaging it away.
    complete INTEGER NOT NULL DEFAULT 1,
    reason   TEXT    NOT NULL DEFAULT '',

    CHECK (complete IN (0, 1))
);

CREATE INDEX idx_drift_evaluations_time ON drift_evaluations (evaluated_at DESC);
CREATE INDEX idx_drift_evaluations_complete ON drift_evaluations (complete);
