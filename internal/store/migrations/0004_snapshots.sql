-- 0004_snapshots: immutable configuration snapshots and restore readiness.
--
-- HarborMaster remains read-only with respect to Docker. Capture READS the
-- inventory and writes to these tables; nothing here grants any ability to
-- create, change, or remove a container.

-- --------------------------------------------------- legacy table rename --

-- The Phase 1 `snapshots` table is RENAMED, not dropped.
--
-- No shipped code path ever wrote to it: the repository was constructed in
-- store.go and exercised only by tests. But "no writer shipped" describes
-- HarborMaster's releases, not any particular operator's database -- a
-- hand-written row, an experimental branch, or a restored backup would all be
-- destroyed by a DROP, and unexpected data is worth more than a tidy schema.
--
-- The renamed table has no reader, no writer, and no repository method. It is
-- inert storage. A later, separately announced migration may remove it once
-- operators have had a release to notice and export anything it holds.
ALTER TABLE snapshots RENAME TO snapshots_legacy_phase1;

-- SQLite carries indexes across a rename under their original names, which
-- would collide with the new ones below.
DROP INDEX IF EXISTS idx_snapshots_container_created;
DROP INDEX IF EXISTS idx_snapshots_container_checksum;

-- ------------------------------------------------------------- snapshots --

-- One row per captured configuration.
--
-- IMMUTABLE after insert, with exactly two exceptions: readiness_status and
-- readiness_evaluated_at, which are a denormalised cache of the most recent
-- row in snapshot_restore_checks. Everything else -- the canonical document,
-- the checksum, image identity, trigger, and every metadata column -- is fixed
-- at capture time. A snapshot is evidence, and evidence that can be edited is
-- not evidence.
CREATE TABLE snapshots (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id               TEXT    NOT NULL DEFAULT 'local',
    container_id          TEXT    NOT NULL,
    container_name        TEXT    NOT NULL,

    -- Image identity. The reference as written AND the resolved content
    -- address: a tag can be repointed, an ID cannot.
    image_reference       TEXT    NOT NULL DEFAULT '',
    image_digest          TEXT    NOT NULL DEFAULT '',
    image_id              TEXT    NOT NULL DEFAULT '',

    -- The canonical document. Authoritative: every child row below is derived
    -- from it in the same transaction and is never an independent source of
    -- truth.
    --
    -- IMPORTANT: this holds NO plaintext secret. A sensitive value contributes
    -- only through the keyed digest recorded in snapshot_environment.
    spec_version          INTEGER NOT NULL,
    spec_json             BLOB    NOT NULL,

    -- SHA-256 over the document bytes plus the secret digests, hex encoded.
    checksum              TEXT    NOT NULL,

    -- Provenance: what produced this snapshot, and what the host looked like.
    harbormaster_version  TEXT    NOT NULL DEFAULT '',
    docker_api_version    TEXT    NOT NULL DEFAULT '',
    docker_engine_version TEXT    NOT NULL DEFAULT '',
    trigger               TEXT    NOT NULL,
    -- Operator-supplied prose. Length-bounded and UTF-8 validated before it
    -- reaches this column. Never a raw error or an environment value.
    reason                TEXT    NOT NULL DEFAULT '',

    inventory_generation  INTEGER NOT NULL DEFAULT 0,
    event_sequence        INTEGER NOT NULL DEFAULT 0,

    warning_count         INTEGER NOT NULL DEFAULT 0,
    warnings_json         BLOB,

    -- The two mutable columns, and the only two.
    readiness_status      TEXT    NOT NULL DEFAULT 'unknown',
    readiness_evaluated_at TEXT,

    -- Identifies the HMAC key the digests were produced under. Derived from the
    -- key by hashing, so it is safe to store: it is not the key and cannot
    -- produce it. Digests under different key IDs are not comparable.
    digest_key_id         TEXT    NOT NULL DEFAULT '',

    created_at            TEXT    NOT NULL,

    CHECK (length(checksum) = 64),
    CHECK (spec_version > 0),
    CHECK (trigger IN ('manual', 'api', 'scheduled', 'pre_update')),
    -- 'unknown' means no evaluation has run. It is the insert default and is
    -- never produced BY an evaluation.
    CHECK (readiness_status IN ('unknown', 'ready', 'warning', 'not_ready'))
);

-- History for one container, newest first: the container detail page's query.
CREATE INDEX idx_snapshots_container_created
    ON snapshots (container_id, created_at DESC);

-- The global list, newest first.
CREATE INDEX idx_snapshots_created
    ON snapshots (created_at DESC);

-- Filter by readiness or trigger, still newest first.
CREATE INDEX idx_snapshots_readiness
    ON snapshots (readiness_status, created_at DESC);
CREATE INDEX idx_snapshots_trigger
    ON snapshots (trigger, created_at DESC);

-- One snapshot per container per distinct configuration.
--
-- This is the durable bound on database growth from repeated capture requests,
-- and it is what makes Phase 3 CONFIGURATION HISTORY rather than time-series
-- evidence: re-capturing an unchanged configuration returns the existing row
-- instead of recording that an observation occurred. Compliance attestation
-- needs a separate observation model, not a weakening of this index.
CREATE UNIQUE INDEX idx_snapshots_container_checksum
    ON snapshots (container_id, checksum);

-- -------------------------------------------------- snapshot environment --

-- One row per environment variable, derived from spec_json.
--
-- Relational rather than left in the document because restore readiness has to
-- answer "which secrets would be missing" without decoding every document, and
-- because a diff compares digests key by key.
--
-- IMPORTANT: `value` is EMPTY for a sensitive variable. The plaintext of a
-- secret is never written to this or any other column, in any encoding. What
-- is recorded is a keyed HMAC digest, which answers exactly one question --
-- did this value change between two snapshots -- and no others.
CREATE TABLE snapshot_environment (
    snapshot_id      INTEGER NOT NULL REFERENCES snapshots (id) ON DELETE CASCADE,
    -- Environment ORDER is semantically meaningful to some programs, so it is
    -- preserved rather than normalised away.
    position         INTEGER NOT NULL,
    key              TEXT    NOT NULL,
    classification   TEXT    NOT NULL DEFAULT 'normal',
    -- Present distinguishes an unset variable from one set to the empty
    -- string. A future restore must not conflate the two.
    present          INTEGER NOT NULL DEFAULT 1,
    value            TEXT    NOT NULL DEFAULT '',
    value_length     INTEGER NOT NULL DEFAULT 0,
    digest           TEXT    NOT NULL DEFAULT '',
    digest_algorithm TEXT    NOT NULL DEFAULT '',
    digest_key_id    TEXT    NOT NULL DEFAULT '',

    PRIMARY KEY (snapshot_id, position),

    CHECK (classification IN ('normal', 'sensitive')),
    -- Belt and braces on the rule the application already enforces: a
    -- sensitive row may not carry a value.
    CHECK (classification <> 'sensitive' OR value = '')
);

CREATE INDEX idx_snapshot_environment_key
    ON snapshot_environment (snapshot_id, key);

-- Finding every snapshot that carries a given variable, for readiness.
CREATE INDEX idx_snapshot_environment_lookup
    ON snapshot_environment (key, classification);

-- ------------------------------------------------------ snapshot mounts --

-- Derived from spec_json. Relational because readiness validates named volumes
-- against the inventory, and doing that from documents would mean decoding
-- every snapshot to answer one question.
CREATE TABLE snapshot_mounts (
    snapshot_id INTEGER NOT NULL REFERENCES snapshots (id) ON DELETE CASCADE,
    destination TEXT    NOT NULL,
    type        TEXT    NOT NULL DEFAULT 'unknown',
    source      TEXT    NOT NULL DEFAULT '',
    read_only   INTEGER NOT NULL DEFAULT 0,
    volume_name TEXT    NOT NULL DEFAULT '',
    driver      TEXT    NOT NULL DEFAULT '',

    PRIMARY KEY (snapshot_id, destination)
);

CREATE INDEX idx_snapshot_mounts_volume
    ON snapshot_mounts (volume_name);

-- ---------------------------------------------------- snapshot networks --

CREATE TABLE snapshot_networks (
    snapshot_id  INTEGER NOT NULL REFERENCES snapshots (id) ON DELETE CASCADE,
    network_name TEXT    NOT NULL,
    aliases_json TEXT    NOT NULL DEFAULT '[]',

    PRIMARY KEY (snapshot_id, network_name)
);

CREATE INDEX idx_snapshot_networks_name
    ON snapshot_networks (network_name);

-- ----------------------------------------------- snapshot restore checks --

-- The historical record of readiness evaluations. APPEND ONLY.
--
-- These rows are what a past evaluation concluded, never the answer to a
-- current question: readiness is recomputed live from the inventory and a
-- daemon ping, because a stored verdict ages the moment it is written.
CREATE TABLE snapshot_restore_checks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    snapshot_id  INTEGER NOT NULL REFERENCES snapshots (id) ON DELETE CASCADE,
    evaluated_at TEXT    NOT NULL,
    check_id     TEXT    NOT NULL,
    status       TEXT    NOT NULL,
    -- Operator-facing prose. Never a raw daemon error, a secret, or a path the
    -- snapshot does not already record.
    detail       TEXT    NOT NULL DEFAULT '',

    -- 'unverifiable' is a first-class outcome: HarborMaster could not
    -- establish the answer, which caps the overall verdict at 'warning' rather
    -- than being rounded to a pass or a failure.
    CHECK (status IN ('ready', 'warning', 'not_ready', 'unverifiable'))
);

CREATE INDEX idx_snapshot_restore_checks_snapshot
    ON snapshot_restore_checks (snapshot_id, evaluated_at DESC);
