-- 0009_acquisitions: safe image acquisition.
--
-- This migration accompanies HarborMaster's FIRST Docker mutation. It is worth
-- being precise about what that mutation is, because the schema is shaped
-- around the limit rather than around the capability:
--
--   HarborMaster can download an approved, digest-pinned image into the
--   daemon's local image store. It cannot start, stop, recreate, or reconfigure
--   a container, and it cannot delete or prune an image.
--
-- **No container is touched by anything recorded here.** A container keeps
-- running the image it was created from; an acquired image is another entry in
-- the local store beside it. There is consequently no `applied`, `rolled_back`,
-- or `container_updated` column anywhere below -- not omitted for later, but
-- absent because the operation does not exist.
--
-- Timestamps are RFC3339Nano UTC text throughout, matching 0001 through 0008.

-- ------------------------------------------------------------ acquisitions --

-- One row per attempt to acquire one image.
--
-- # The record is an audit record
--
-- Identity, target, and approval are written once and never change. The
-- lifecycle fields advance forwards through the documented states and stop at a
-- terminal one. A completed acquisition is never rewritten by a later attempt:
-- a second request produces a second row, so the history of what was downloaded
-- and when survives.
--
-- # What is deliberately NOT stored
--
-- No registry credential, bearer token, or authorization header -- there is no
-- column for one, and HarborMaster holds none. No raw daemon error and no
-- registry response body: `message` is built from a fixed HarborMaster
-- vocabulary keyed by `failure`, so a socket path or an attacker-influenced
-- string cannot reach a column that a browser renders.
--
-- # The approval is pinned by fingerprint, not by timestamp
--
-- `plan_digest` is the change plan's input fingerprint AS APPROVED. Preflight
-- recomputes the plan's fingerprint from current data and compares. That is
-- what closes the time-of-check/time-of-use window between "a plan said this
-- was reasonable" and "we are about to download it" -- exactly, rather than by
-- guessing from how old the plan is.
CREATE TABLE acquisitions (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    -- The IMMUTABLE public identifier, generated server-side from the system
    -- entropy source and never accepted from a caller.
    acquisition_id TEXT NOT NULL UNIQUE,

    -- The plan that approved this image, and the container it was assessed for.
    --
    -- plan_id is a plain column rather than a foreign key: retention may prune
    -- a superseded plan long before its acquisition record ages out, and an
    -- audit record that vanished when its plan did would defeat the purpose of
    -- keeping one.
    plan_id        TEXT NOT NULL,
    container_id   TEXT NOT NULL,
    container_name TEXT NOT NULL DEFAULT '',

    -- The immutable target. Components rather than a reference string: the
    -- reference sent to the daemon is assembled from these, and there is no
    -- column an operator could fill with an arbitrary pull argument.
    --
    -- target_digest is NOT NULL and constrained non-empty. A target without a
    -- digest is a target whose content can change after approval, and the whole
    -- safety model rests on that being impossible to express.
    target_registry   TEXT NOT NULL,
    target_repository TEXT NOT NULL,
    target_digest     TEXT NOT NULL CHECK (length(target_digest) > 0),
    -- The familiar form, for display only. Never used to perform a pull.
    target_reference  TEXT NOT NULL DEFAULT '',
    target_os         TEXT NOT NULL DEFAULT '',
    target_arch       TEXT NOT NULL DEFAULT '',
    target_variant    TEXT NOT NULL DEFAULT '',

    state TEXT NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'validating', 'pulling', 'verifying',
                         'succeeded', 'failed', 'cancelled', 'expired')),

    -- Why it did not succeed. A closed vocabulary: the classification decides
    -- whether a retry could ever help, and drives what an operator is told.
    failure TEXT NOT NULL DEFAULT ''
        CHECK (failure IN ('', 'preflight', 'dockerUnavailable', 'registry',
                           'transfer', 'timeout', 'digestMismatch',
                           'platformMismatch', 'verification', 'internal')),
    -- Which preflight check refused, when one did. A refusal is the safety
    -- model working, so it is recorded specifically rather than as a generic
    -- failure.
    refusal TEXT NOT NULL DEFAULT ''
        CHECK (refusal IN ('', 'planMissing', 'planSuperseded', 'planStale',
                           'recommendation', 'digestUnavailable', 'digestChanged',
                           'platformUnavailable', 'restoreReadiness',
                           'policyViolation', 'registryStale', 'duplicate',
                           'dockerUnavailable', 'limit', 'targetRefused',
                           'disabled', 'containerMissing')),
    -- HarborMaster's own sentence about the outcome, bounded. Built from the
    -- vocabulary above; never a daemon or registry string.
    message TEXT NOT NULL DEFAULT '',

    -- What VERIFICATION found on the host, as opposed to what was expected.
    -- On a mismatch these two columns are the evidence, which is why they are
    -- recorded even when they disagree with the target.
    acquired_image_id TEXT NOT NULL DEFAULT '',
    acquired_digest   TEXT NOT NULL DEFAULT '',
    acquired_os       TEXT NOT NULL DEFAULT '',
    acquired_arch     TEXT NOT NULL DEFAULT '',
    acquired_variant  TEXT NOT NULL DEFAULT '',
    -- The local image size from inspection. Authoritative, unlike the
    -- transfer's own progress counters.
    size_bytes INTEGER NOT NULL DEFAULT 0,

    -- Transfer summary. Estimates for display: layers already present locally
    -- are never counted by the daemon's progress stream.
    layers            INTEGER NOT NULL DEFAULT 0,
    bytes_transferred INTEGER NOT NULL DEFAULT 0,
    -- The most recent bounded status line, e.g. "Downloading". Sanitised at the
    -- Docker adapter boundary before it ever reaches this column.
    progress TEXT NOT NULL DEFAULT '',

    requested_at TEXT NOT NULL,
    started_at   TEXT,
    completed_at TEXT,
    -- When a still-queued request is abandoned. An expired request was never
    -- validated and never pulled, and running it later would be acting on an
    -- approval whose evidence has aged.
    expires_at TEXT NOT NULL,

    -- The caller's idempotency key, when supplied. Empty means none, and empty
    -- is excluded from the uniqueness index below so unkeyed requests do not
    -- collide with each other.
    request_key TEXT NOT NULL DEFAULT '',

    -- The plan's input fingerprint as approved. See the header.
    plan_digest TEXT NOT NULL DEFAULT '',

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- The duplicate-work guarantee.
--
-- At most ONE active acquisition per (container, digest). A partial index over
-- the active states makes this a database invariant rather than a check the
-- service performs and hopes to win: two concurrent requests race, one inserts,
-- the other is refused by the constraint.
--
-- Terminal rows are excluded, so the same image may be acquired again after a
-- previous attempt finished -- which is exactly what a retry after a transient
-- failure needs.
CREATE UNIQUE INDEX idx_acquisition_active_target
    ON acquisitions (container_id, target_digest)
    WHERE state IN ('queued', 'validating', 'pulling', 'verifying');

-- Idempotency. A caller that retries a request with the same key gets the
-- existing record rather than a second pull.
--
-- Partial, so the empty key -- meaning "no key supplied" -- does not make every
-- unkeyed request collide with every other.
CREATE UNIQUE INDEX idx_acquisition_request_key
    ON acquisitions (request_key)
    WHERE request_key <> '';

CREATE INDEX idx_acquisition_state      ON acquisitions (state, id DESC);
CREATE INDEX idx_acquisition_container  ON acquisitions (container_id, id DESC);
CREATE INDEX idx_acquisition_plan       ON acquisitions (plan_id, id DESC);
CREATE INDEX idx_acquisition_requested  ON acquisitions (requested_at DESC);
-- Retention scans terminal rows by completion time.
CREATE INDEX idx_acquisition_completed  ON acquisitions (completed_at)
    WHERE completed_at IS NOT NULL;

-- ------------------------------------------------------ acquisition events --

-- The bounded audit trail for one acquisition.
--
-- Records STATE TRANSITIONS and periodic progress. It is deliberately NOT a
-- copy of the daemon's pull stream: that stream is unbounded in length and
-- carries registry-influenced text, and persisting it would turn one operator
-- action into an unbounded number of writes.
--
-- The adapter rate-limits progress before it is emitted, the service caps how
-- many rows one acquisition may write, and retention removes them with their
-- parent. Three independent bounds, because this is the one table an external
-- party can influence the size of.
CREATE TABLE acquisition_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    -- CASCADE here, unlike the plan reference above: an event has no meaning
    -- without its acquisition, and orphaned progress rows would be noise rather
    -- than history.
    acquisition_id TEXT NOT NULL
        REFERENCES acquisitions (acquisition_id) ON DELETE CASCADE,

    state TEXT NOT NULL
        CHECK (state IN ('queued', 'validating', 'pulling', 'verifying',
                         'succeeded', 'failed', 'cancelled', 'expired')),
    -- A short, sanitised note: HarborMaster's own words, or a progress status
    -- already bounded and stripped of control characters by the adapter.
    detail TEXT NOT NULL DEFAULT '',

    bytes_transferred INTEGER NOT NULL DEFAULT 0,
    layers            INTEGER NOT NULL DEFAULT 0,

    at TEXT NOT NULL
);

CREATE INDEX idx_acquisition_event_parent ON acquisition_events (acquisition_id, id);
