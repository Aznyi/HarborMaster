-- 0002_inventory: the read-only container inventory.
--
-- This schema holds the CURRENT inventory, not a history of configurations.
-- Those are separate concepts and will stay separate: the snapshots table from
-- 0001 is an immutable, point-in-time record captured on demand, while
-- everything here is overwritten by each refresh. Do not conflate them.
--
-- Timestamps are RFC3339Nano UTC text throughout, matching migration 0001.

-- ---------------------------------------------------------------- hosts --

-- Exactly one row exists (id 'local'). The table is here so that repositories,
-- foreign keys, and the API are already keyed by host: adding a second host
-- later becomes a data change rather than a migration.
CREATE TABLE hosts (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    runtime     TEXT NOT NULL,
    api_version TEXT NOT NULL DEFAULT '',
    os_type     TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- ------------------------------------------------------------- refreshes --

CREATE TABLE inventory_refreshes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id    TEXT    NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    -- Generation increments only when a refresh completes AND persists, so a
    -- client can tell whether the inventory it holds is current.
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

    -- Deterministic fingerprint of the resulting inventory. Empty for a failed
    -- refresh, which produces no inventory.
    checksum TEXT NOT NULL DEFAULT '',
    -- Sanitised failure reason. Never a raw runtime error.
    error    TEXT NOT NULL DEFAULT '',

    CHECK (state IN ('running', 'succeeded', 'failed')),
    CHECK (trigger IN ('startup', 'periodic', 'manual'))
);

CREATE INDEX idx_refreshes_started ON inventory_refreshes (host_id, started_at DESC);
-- Serves "what is the current generation": the newest succeeded refresh.
CREATE INDEX idx_refreshes_succeeded ON inventory_refreshes (state, generation DESC);

-- ---------------------------------------------------------------- images --

-- Keyed by content-addressable image ID, so re-observing the same image across
-- refreshes updates one row instead of creating duplicates.
CREATE TABLE images (
    id           TEXT PRIMARY KEY,
    short_id     TEXT    NOT NULL,
    repo_tags    TEXT    NOT NULL DEFAULT '[]',
    repo_digests TEXT    NOT NULL DEFAULT '[]',
    created_at   TEXT,
    architecture TEXT    NOT NULL DEFAULT '',
    os           TEXT    NOT NULL DEFAULT '',
    os_version   TEXT    NOT NULL DEFAULT '',
    variant      TEXT    NOT NULL DEFAULT '',
    size         INTEGER NOT NULL DEFAULT 0,
    author       TEXT    NOT NULL DEFAULT '',
    comment      TEXT    NOT NULL DEFAULT '',
    labels       TEXT    NOT NULL DEFAULT '{}',

    first_seen_at TEXT    NOT NULL,
    last_seen_at  TEXT    NOT NULL,
    generation    INTEGER NOT NULL
);

CREATE INDEX idx_images_generation ON images (generation);

-- -------------------------------------------------------------- networks --

CREATE TABLE networks (
    id         TEXT PRIMARY KEY,
    name       TEXT    NOT NULL,
    driver     TEXT    NOT NULL DEFAULT '',
    scope      TEXT    NOT NULL DEFAULT '',
    internal   INTEGER NOT NULL DEFAULT 0,
    attachable INTEGER NOT NULL DEFAULT 0,
    ipv6       INTEGER NOT NULL DEFAULT 0,
    created_at TEXT,
    labels     TEXT    NOT NULL DEFAULT '{}',
    subnets    TEXT    NOT NULL DEFAULT '[]',

    last_seen_at TEXT    NOT NULL,
    generation   INTEGER NOT NULL
);

CREATE INDEX idx_networks_name ON networks (name);

-- --------------------------------------------------------------- volumes --

-- Metadata only. HarborMaster never reads volume contents.
CREATE TABLE volumes (
    name       TEXT PRIMARY KEY,
    driver     TEXT NOT NULL DEFAULT '',
    scope      TEXT NOT NULL DEFAULT '',
    mountpoint TEXT NOT NULL DEFAULT '',
    created_at TEXT,
    labels     TEXT NOT NULL DEFAULT '{}',
    options    TEXT NOT NULL DEFAULT '{}',

    last_seen_at TEXT    NOT NULL,
    generation   INTEGER NOT NULL
);

-- ------------------------------------------------------------ containers --

CREATE TABLE containers (
    id       TEXT PRIMARY KEY,
    host_id  TEXT NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    short_id TEXT NOT NULL,
    name     TEXT NOT NULL,

    -- image_id is a SOFT reference to images.id, deliberately without a
    -- foreign key: a container can outlive its image (`docker rmi` on an image
    -- still in use), and a hard constraint would make that normal situation
    -- fail the whole refresh.
    image_id         TEXT NOT NULL DEFAULT '',
    image_ref        TEXT NOT NULL DEFAULT '',
    image_repository TEXT NOT NULL DEFAULT '',
    image_tag        TEXT NOT NULL DEFAULT '',
    image_digest     TEXT NOT NULL DEFAULT '',

    state     TEXT NOT NULL,
    raw_state TEXT NOT NULL DEFAULT '',
    status    TEXT NOT NULL DEFAULT '',
    health    TEXT NOT NULL DEFAULT 'none',

    created_at  TEXT NOT NULL,
    started_at  TEXT,
    finished_at TEXT,

    exit_code                INTEGER,
    restart_count            INTEGER NOT NULL DEFAULT 0,
    restart_policy_name      TEXT    NOT NULL DEFAULT '',
    restart_policy_max_retry INTEGER NOT NULL DEFAULT 0,

    compose_project          TEXT    NOT NULL DEFAULT '',
    compose_service          TEXT    NOT NULL DEFAULT '',
    compose_container_number INTEGER NOT NULL DEFAULT 0,
    compose_oneoff           INTEGER NOT NULL DEFAULT 0,

    -- NULL means the container carries no io.harbormaster.enabled label, which
    -- is distinct from the label being present and false.
    hm_enabled INTEGER,

    -- Ports are stored as JSON rather than in their own table: they are always
    -- rendered with the row and are never filtered on, so a join would cost a
    -- query without buying anything.
    ports TEXT NOT NULL DEFAULT '[]',

    -- present = 0 marks a container an earlier refresh saw and this one did
    -- not. Rows are retained rather than deleted so that history and warnings
    -- survive a container being removed out from under HarborMaster.
    present       INTEGER NOT NULL DEFAULT 1,
    first_seen_at TEXT    NOT NULL,
    last_seen_at  TEXT    NOT NULL,
    generation    INTEGER NOT NULL,
    warning_count INTEGER NOT NULL DEFAULT 0,

    CHECK (state IN ('created', 'running', 'paused', 'restarting',
                     'removing', 'exited', 'dead', 'unknown')),
    CHECK (health IN ('none', 'starting', 'healthy', 'unhealthy'))
);

-- Indexes chosen for the container list endpoint's filters and its default
-- ordering (name, then id).
CREATE INDEX idx_containers_name ON containers (host_id, name, id);
CREATE INDEX idx_containers_state ON containers (state, name);
CREATE INDEX idx_containers_health ON containers (health, name);
CREATE INDEX idx_containers_compose ON containers (compose_project, compose_service, name);
CREATE INDEX idx_containers_image ON containers (image_repository, name);
CREATE INDEX idx_containers_present ON containers (present, name);
CREATE INDEX idx_containers_generation ON containers (generation);
CREATE INDEX idx_containers_created ON containers (created_at DESC);

-- -------------------------------------- current normalized configuration --

-- One row per container holding the normalized configuration sections that the
-- list view never needs: process, environment, resources, security, logging,
-- healthcheck, and the redacted raw inspection payload.
--
-- Stored as JSON because these sections are read whole, for one container at a
-- time, and are never queried field-by-field. Splitting them into columns
-- would add joins to serve a request that always wants all of it.
--
-- IMPORTANT: config_json holds MASKED environment values. Raw values are never
-- written to disk.
CREATE TABLE container_config (
    container_id TEXT PRIMARY KEY REFERENCES containers (id) ON DELETE CASCADE,
    config_json  TEXT NOT NULL,
    -- Redacted raw inspection payload. Sensitive values have been removed, so
    -- this is NOT a faithful copy and cannot recreate a container exactly.
    raw_json     TEXT,
    updated_at   TEXT NOT NULL,
    generation   INTEGER NOT NULL
);

-- ------------------------------------------------------ container labels --

-- Relational, unlike the other configuration, because the API filters on label
-- key and on key/value.
CREATE TABLE container_labels (
    container_id TEXT NOT NULL REFERENCES containers (id) ON DELETE CASCADE,
    key          TEXT NOT NULL,
    value        TEXT NOT NULL DEFAULT '',
    source       TEXT NOT NULL DEFAULT 'user',

    PRIMARY KEY (container_id, key)
);

CREATE INDEX idx_container_labels_key ON container_labels (key, value);

-- ---------------------------------------------------- container networks --

CREATE TABLE container_networks (
    container_id TEXT NOT NULL REFERENCES containers (id) ON DELETE CASCADE,
    network_name TEXT NOT NULL,
    -- Soft reference to networks.id for the same reason as image_id: a network
    -- can be removed while an attachment record still describes it.
    network_id   TEXT NOT NULL DEFAULT '',
    aliases      TEXT NOT NULL DEFAULT '[]',
    ipv4_address TEXT NOT NULL DEFAULT '',
    ipv6_address TEXT NOT NULL DEFAULT '',
    gateway      TEXT NOT NULL DEFAULT '',
    mac_address  TEXT NOT NULL DEFAULT '',
    endpoint_id  TEXT NOT NULL DEFAULT '',
    links        TEXT NOT NULL DEFAULT '[]',

    PRIMARY KEY (container_id, network_name)
);

CREATE INDEX idx_container_networks_network ON container_networks (network_id);

-- ------------------------------------------------------ container mounts --

CREATE TABLE container_mounts (
    container_id   TEXT    NOT NULL REFERENCES containers (id) ON DELETE CASCADE,
    destination    TEXT    NOT NULL,
    type           TEXT    NOT NULL DEFAULT 'unknown',
    source         TEXT    NOT NULL DEFAULT '',
    read_only      INTEGER NOT NULL DEFAULT 0,
    propagation    TEXT    NOT NULL DEFAULT '',
    consistency    TEXT    NOT NULL DEFAULT '',
    volume_name    TEXT    NOT NULL DEFAULT '',
    driver         TEXT    NOT NULL DEFAULT '',
    driver_options TEXT    NOT NULL DEFAULT '{}',
    tmpfs_options  TEXT    NOT NULL DEFAULT '',

    PRIMARY KEY (container_id, destination)
);

CREATE INDEX idx_container_mounts_volume ON container_mounts (volume_name);

-- ---------------------------------------------------------- warnings --

CREATE TABLE inventory_warnings (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    generation     INTEGER NOT NULL,
    container_id   TEXT    NOT NULL DEFAULT '',
    container_name TEXT    NOT NULL DEFAULT '',
    code           TEXT    NOT NULL,
    -- Operator-facing prose. Never a raw runtime error, a socket path, or an
    -- environment value.
    message        TEXT    NOT NULL,
    occurred_at    TEXT    NOT NULL
);

CREATE INDEX idx_warnings_generation ON inventory_warnings (generation, id);
CREATE INDEX idx_warnings_container ON inventory_warnings (container_id, generation DESC);
