-- 0007_image_intel: image intelligence and update discovery.
--
-- HarborMaster remains read-only with respect to Docker. This migration backs a
-- feature that READS registries over HTTPS and records what it learned. Nothing
-- here grants any ability to pull, push, delete, prune, tag, or recreate
-- anything, and there is no column that could hold an instruction to.
--
-- No registry credential is stored, anywhere, ever. Every lookup is anonymous;
-- bearer tokens are held in memory for minutes and are deliberately absent from
-- this schema. See internal/registry.
--
-- Timestamps are RFC3339Nano UTC text throughout, matching 0001 through 0006.

-- ------------------------------------------------------------ image intel --

-- One row per canonical image REFERENCE.
--
-- Per reference, not per container: a hundred containers running nginx:1.25 are
-- one registry lookup. That is the difference between a client a registry
-- tolerates and one it rate-limits, and it is why the identity of this table is
-- the reference rather than the container or the local image id.
--
-- # What is NOT duplicated here
--
-- The local image row in `images` already holds size, layers, local labels, and
-- the content-addressable id. None of that is copied. This table holds only
-- what the REGISTRY knows and what the scheduler needs, and links to the local
-- row by id when there is one.
--
-- image_id is deliberately NOT a foreign key. A container can reference an
-- image the daemon has since removed, and the intelligence for that reference
-- is still worth keeping; a foreign key would force a choice between deleting
-- the knowledge and refusing the inventory refresh.
CREATE TABLE image_intel (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    -- The canonical reference, e.g. "docker.io/library/nginx:1.25". The
    -- identity of the row, produced only by domain.NormalizeImageRef.
    reference TEXT NOT NULL UNIQUE,
    -- The short form an operator recognises, e.g. "nginx:1.25". Denormalised
    -- for display so a list needs no re-derivation.
    familiar  TEXT NOT NULL DEFAULT '',

    registry_kind TEXT NOT NULL DEFAULT 'unknown',
    -- The registry as it appears in the canonical reference. This column, and
    -- the reference it came from, are the ONLY origin of a network destination
    -- in HarborMaster.
    registry   TEXT NOT NULL DEFAULT '',
    namespace  TEXT NOT NULL DEFAULT '',
    repository TEXT NOT NULL DEFAULT '',
    tag        TEXT NOT NULL DEFAULT '',

    -- The digest the local daemon reports, and the one the registry currently
    -- serves. The pair is the whole of digest-based update detection.
    --
    -- remote_digest is COMPUTED from the manifest bytes rather than read from a
    -- registry header: see internal/registry.
    local_digest  TEXT NOT NULL DEFAULT '',
    remote_digest TEXT NOT NULL DEFAULT '',
    -- pinned = 1 when the reference names a digest, so its tag cannot move.
    pinned INTEGER NOT NULL DEFAULT 0,

    platform_os      TEXT NOT NULL DEFAULT '',
    platform_arch    TEXT NOT NULL DEFAULT '',
    platform_variant TEXT NOT NULL DEFAULT '',

    -- The local image row, when one exists. See above for why this is not a
    -- foreign key.
    image_id TEXT NOT NULL DEFAULT '',

    update_type   TEXT NOT NULL DEFAULT 'none',
    latest_tag    TEXT NOT NULL DEFAULT '',
    update_reason TEXT NOT NULL DEFAULT '',

    check_status TEXT NOT NULL DEFAULT 'pending',
    -- HarborMaster's own description of a non-OK status, from a fixed set of
    -- phrases. NEVER a registry-supplied string: a registry is a third party
    -- and its error text must not reach a column that a UI renders.
    status_detail TEXT NOT NULL DEFAULT '',

    first_seen_at   TEXT NOT NULL,
    last_checked_at TEXT,
    last_success_at TEXT,
    -- When the scheduler will consider this reference again. One column
    -- implements both the refresh interval and the failure backoff, which is
    -- what keeps "when is this due" a single indexed question.
    next_check_at TEXT,
    failure_count INTEGER NOT NULL DEFAULT 0,

    -- Provenance from OCI annotations, when the publisher set any. Bounded and
    -- sanitised before they reach these columns.
    published_at TEXT,
    vendor       TEXT NOT NULL DEFAULT '',
    source       TEXT NOT NULL DEFAULT '',
    labels       TEXT NOT NULL DEFAULT '{}',

    -- The conditional-request validator for this reference's manifest, so an
    -- unchanged image costs a 304 rather than a manifest transfer. Bounded and
    -- control-character free; validated again before it becomes a header.
    etag TEXT NOT NULL DEFAULT '',

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    CHECK (registry_kind IN ('dockerhub', 'ghcr', 'oci', 'unknown')),
    CHECK (update_type IN ('none', 'digest', 'patch', 'minor', 'major',
                           'prerelease', 'unknown')),
    CHECK (check_status IN ('pending', 'ok', 'failed', 'rateLimited',
                            'unauthorized', 'notFound', 'unsupported')),
    CHECK (pinned IN (0, 1)),
    CHECK (failure_count >= 0),
    CHECK (LENGTH(reference) <= 512),
    CHECK (LENGTH(status_detail) <= 512),
    CHECK (LENGTH(update_reason) <= 512),
    CHECK (LENGTH(etag) <= 256),
    CHECK (LENGTH(labels) <= 8192)
);

-- The scheduler's only question: which references are due now. Leading with
-- next_check_at makes "due" an index range scan rather than a table scan, which
-- is what keeps a ten-thousand-reference estate cheap to schedule.
CREATE INDEX idx_image_intel_due ON image_intel (next_check_at, id);
-- The list endpoint's filters and default ordering.
CREATE INDEX idx_image_intel_update   ON image_intel (update_type, reference);
CREATE INDEX idx_image_intel_status   ON image_intel (check_status, reference);
CREATE INDEX idx_image_intel_registry ON image_intel (registry, reference);
CREATE INDEX idx_image_intel_image    ON image_intel (image_id);

-- --------------------------------------------------------- update history --

-- One row per observed CHANGE, not per check.
--
-- A row per check would add an entry per reference per refresh interval and
-- record nothing: "we looked, and it was the same" is not history. Only an
-- actual movement is written -- a digest changed, an update appeared or was
-- taken, the check started or stopped failing -- which is what makes this table
-- readable and what bounds its growth to the rate the world actually changes.
CREATE TABLE image_update_history (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    -- The reference this event belongs to. ON DELETE CASCADE: an event about a
    -- reference no longer tracked has nothing to describe.
    reference TEXT NOT NULL REFERENCES image_intel (reference) ON DELETE CASCADE,

    observed_at TEXT NOT NULL,
    kind        TEXT NOT NULL,

    previous_digest TEXT NOT NULL DEFAULT '',
    current_digest  TEXT NOT NULL DEFAULT '',

    previous_update TEXT NOT NULL DEFAULT '',
    current_update  TEXT NOT NULL DEFAULT '',
    latest_tag      TEXT NOT NULL DEFAULT '',

    check_status TEXT NOT NULL DEFAULT 'ok',
    -- HarborMaster's own words, from a fixed set. Never registry text.
    detail TEXT NOT NULL DEFAULT '',

    CHECK (kind IN ('discovered', 'digestChanged', 'updateFound',
                    'updateCleared', 'checkFailed', 'checkRecovered')),
    CHECK (check_status IN ('pending', 'ok', 'failed', 'rateLimited',
                            'unauthorized', 'notFound', 'unsupported')),
    CHECK (LENGTH(detail) <= 512)
);

-- Serves the per-image history endpoint, newest first.
CREATE INDEX idx_image_history_reference ON image_update_history (reference, observed_at DESC, id DESC);
-- Serves the estate-wide timeline and the retention pass.
CREATE INDEX idx_image_history_time      ON image_update_history (observed_at DESC, id DESC);

-- -------------------------------------------------------- registry health --

-- One row per registry HOST.
--
-- Rate limits, outages, and backoff are properties of an endpoint, not of an
-- image. Keeping them here means a rate-limited Docker Hub delays every Docker
-- Hub reference at once rather than each discovering the limit separately --
-- which is both politer to the registry and faster to recover from.
--
-- It is also what lets the UI say "updates are stale because Docker Hub is
-- rate-limiting us" instead of showing a hundred individually failed images.
CREATE TABLE registry_hosts (
    host          TEXT PRIMARY KEY,
    registry_kind TEXT NOT NULL DEFAULT 'oci',

    last_success_at TEXT,
    last_failure_at TEXT,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    -- When the host may be contacted again. Set by HarborMaster's backoff and,
    -- when a registry sends one, by its own Retry-After.
    available_at TEXT,

    -- HarborMaster's own description of the most recent failure.
    last_detail  TEXT    NOT NULL DEFAULT '',
    rate_limited INTEGER NOT NULL DEFAULT 0,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    CHECK (registry_kind IN ('dockerhub', 'ghcr', 'oci', 'unknown')),
    CHECK (consecutive_failures >= 0),
    CHECK (rate_limited IN (0, 1)),
    CHECK (LENGTH(last_detail) <= 512)
);

CREATE INDEX idx_registry_hosts_available ON registry_hosts (available_at);
