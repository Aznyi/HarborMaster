-- 0010_executions: manual container recreation.
--
-- This migration accompanies HarborMaster's first CONTAINER mutation, and the
-- schema is shaped around the limits rather than around the capability.
--
--   HarborMaster can replace ONE container, ONCE, on an image an operator
--   already downloaded and HarborMaster already verified, when a current plan
--   recommends it. It stops the original, parks it under a derived name,
--   creates a replacement, starts it, proves it, and only then removes what it
--   replaced.
--
-- # What is deliberately NOT here
--
-- There is no `rollback` column, no `retry_count`, no `schedule`, and no
-- `batch_id`. Not omitted for later -- absent because the operations do not
-- exist. HarborMaster does not undo its own work, does not retry a recreation,
-- does not run one on a timer, and cannot act on more than one container.
--
-- # The checkpoint is the load-bearing column
--
-- `state` says what HarborMaster was DOING. `checkpoint` says what is TRUE of
-- the host, and it is written after each mutation completes and before the next
-- is attempted. After a crash only the second question matters: a process that
-- died in `creating` may have created nothing, or may have stopped the
-- original, renamed it, and created the replacement.
--
-- Recovery therefore reads `checkpoint`, never `state`, and never repeats a
-- mutation whose record was uncertain.
--
-- # Single use
--
-- `acquisition_id` is UNIQUE across the whole table -- not partial, not scoped
-- to active rows. One execution per acquisition, ever. A second recreation of
-- the same container needs a fresh plan, assessed against the world as it is
-- now, and a fresh acquisition to prove the image is still present.
--
-- The cost is honest and worth stating: an execution that was cancelled or
-- refused before it changed anything still consumes its acquisition, and the
-- operator has to acquire again. Acquiring an image that is already local is
-- cheap; acting on a stale approval is not.
--
-- Timestamps are RFC3339Nano UTC text throughout, matching 0001 through 0009.

-- -------------------------------------------------------------- executions --

CREATE TABLE executions (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    -- The IMMUTABLE public identifier, generated server-side from the system
    -- entropy source. It appears in URLs AND in container names on the host, so
    -- it is random rather than sequential: a sequential one would leak how many
    -- recreations have happened and would make a parked container's name
    -- guessable.
    execution_id TEXT NOT NULL UNIQUE,

    -- The evidence chain. Plain columns rather than foreign keys: retention may
    -- prune an acquisition or a plan long before its execution record ages out,
    -- and an audit record that vanished when its evidence did would defeat the
    -- purpose of keeping one.
    --
    -- acquisition_id is UNIQUE. See the header: single use, no override.
    acquisition_id TEXT NOT NULL UNIQUE,
    plan_id        TEXT NOT NULL,
    snapshot_id    INTEGER NOT NULL DEFAULT 0,

    -- The ORIGINAL container, and the name the replacement takes over.
    container_id   TEXT NOT NULL,
    container_name TEXT NOT NULL,

    -- What it was running before.
    old_image        TEXT NOT NULL DEFAULT '',
    old_image_id     TEXT NOT NULL DEFAULT '',
    old_image_digest TEXT NOT NULL DEFAULT '',

    -- The immutable target. Components rather than a reference string: the
    -- reference sent to the daemon is assembled from these, and there is no
    -- column an operator could fill with an arbitrary create argument.
    --
    -- target_digest is NOT NULL and constrained non-empty. A container created
    -- from anything but a digest is a container whose content can change after
    -- approval.
    target_registry   TEXT NOT NULL,
    target_repository TEXT NOT NULL,
    target_digest     TEXT NOT NULL CHECK (length(target_digest) > 0),
    target_reference  TEXT NOT NULL DEFAULT '',
    target_image_id   TEXT NOT NULL DEFAULT '',
    target_os         TEXT NOT NULL DEFAULT '',
    target_arch       TEXT NOT NULL DEFAULT '',
    target_variant    TEXT NOT NULL DEFAULT '',

    state TEXT NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'validating', 'capturing', 'creating',
                         'starting', 'verifying', 'succeeded', 'failed',
                         'cancelled', 'expired')),

    -- What is TRUE of the host. See the header.
    checkpoint TEXT NOT NULL DEFAULT ''
        CHECK (checkpoint IN ('', 'originalStopped', 'originalParked',
                              'replacementCreated', 'replacementStarted',
                              'replacementVerified', 'replacementQuarantined',
                              'originalRemoved')),

    failure TEXT NOT NULL DEFAULT ''
        CHECK (failure IN ('', 'preflight', 'capture', 'stop', 'rename',
                           'create', 'start', 'healthTimeout', 'unhealthy',
                           'notStable', 'imageMismatch', 'preservation',
                           'network', 'secretUnavailable', 'dockerUnavailable',
                           'timeout', 'interrupted', 'persistence', 'internal')),

    refusal TEXT NOT NULL DEFAULT ''
        CHECK (refusal IN ('', 'disabled', 'acquisitionMissing',
                           'acquisitionNotSucceeded', 'acquisitionStale',
                           'acquisitionConsumed', 'planMissing',
                           'planSuperseded', 'planChanged', 'recommendation',
                           'containerMissing', 'containerChanged',
                           'containerState', 'inventoryStale',
                           'snapshotMissing', 'restoreReadiness',
                           'policyViolation', 'policyStale', 'registryStale',
                           'imageMissing', 'digestMismatch', 'platformMismatch',
                           'conflict', 'limit', 'dockerUnavailable',
                           'secretUnavailable', 'nameUnavailable')),

    -- HarborMaster's own sentence about the outcome, bounded and built from the
    -- vocabularies above. NEVER a daemon string: an Engine error can embed the
    -- socket path and the container's command line, and this column is rendered
    -- in a browser.
    message TEXT NOT NULL DEFAULT '',

    -- What exists on the host afterwards. These three are how an operator finds
    -- things when a recreation has failed, so they are recorded as soon as each
    -- becomes true rather than at the end.
    replacement_id   TEXT NOT NULL DEFAULT '',
    parked_name      TEXT NOT NULL DEFAULT '',
    quarantine_name  TEXT NOT NULL DEFAULT '',
    original_removed INTEGER NOT NULL DEFAULT 0 CHECK (original_removed IN (0, 1)),

    -- The four proofs. 'unknown' means the proof was never reached, which is
    -- deliberately distinct from having failed: a check that could not be
    -- PERFORMED establishes nothing, and the success path requires all four to
    -- read 'passed'.
    verify_health TEXT NOT NULL DEFAULT 'unknown'
        CHECK (verify_health IN ('unknown', 'passed', 'failed')),
    verify_image TEXT NOT NULL DEFAULT 'unknown'
        CHECK (verify_image IN ('unknown', 'passed', 'failed')),
    verify_preservation TEXT NOT NULL DEFAULT 'unknown'
        CHECK (verify_preservation IN ('unknown', 'passed', 'failed')),
    verify_network TEXT NOT NULL DEFAULT 'unknown'
        CHECK (verify_network IN ('unknown', 'passed', 'failed')),

    -- How health was established. health_checked distinguishes a container that
    -- declares a health check from one proved by staying running.
    health_state      TEXT    NOT NULL DEFAULT ''
        CHECK (health_state IN ('', 'none', 'starting', 'healthy', 'unhealthy')),
    health_checked    INTEGER NOT NULL DEFAULT 0 CHECK (health_checked IN (0, 1)),
    stability_seconds INTEGER NOT NULL DEFAULT 0,

    -- The field-by-field preservation comparison, as JSON. Bounded by the
    -- domain's own difference cap before it is written; a wholesale mismatch
    -- produces one difference per field and only the first few are actionable.
    preservation_report TEXT NOT NULL DEFAULT '',

    -- The manual recovery plan, as JSON. TEXT that HarborMaster RENDERS and
    -- never executes: there is no endpoint that runs one and no capability
    -- behind one.
    recovery_plan TEXT NOT NULL DEFAULT '',

    requested_at TEXT NOT NULL,
    started_at   TEXT,
    -- When the host was FIRST changed. NULL means it never was, which is the
    -- single most useful fact on a failed record.
    mutated_at   TEXT,
    completed_at TEXT,
    expires_at   TEXT NOT NULL,

    request_key TEXT NOT NULL DEFAULT '',
    plan_digest TEXT NOT NULL DEFAULT '',

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- At most ONE active recreation per container.
--
-- A partial unique index over the active states makes this a database
-- invariant rather than a check the service performs and hopes to win. Two
-- concurrent requests race, one inserts, the other is refused -- and a race is
-- the normal case for a button an operator can double-click.
--
-- Two simultaneous recreations of one container would be catastrophic rather
-- than merely wasteful: both would stop it, both would try to take its name,
-- and the host would be left in a state neither of them recorded.
CREATE UNIQUE INDEX idx_execution_active_container
    ON executions (container_id)
    WHERE state IN ('queued', 'validating', 'capturing', 'creating',
                    'starting', 'verifying');

-- Idempotency. A caller that retries with the same key gets the existing
-- record rather than a second recreation. Partial, so the empty key -- meaning
-- "no key supplied" -- does not make every unkeyed request collide.
CREATE UNIQUE INDEX idx_execution_request_key
    ON executions (request_key)
    WHERE request_key <> '';

CREATE INDEX idx_execution_state      ON executions (state, id DESC);
CREATE INDEX idx_execution_container  ON executions (container_id, id DESC);
CREATE INDEX idx_execution_plan       ON executions (plan_id, id DESC);
CREATE INDEX idx_execution_requested  ON executions (requested_at DESC);
CREATE INDEX idx_execution_completed  ON executions (completed_at)
    WHERE completed_at IS NOT NULL;
-- Failures that left containers behind are what an operator opens this page
-- for, so they are indexed rather than scanned.
CREATE INDEX idx_execution_attention  ON executions (state, checkpoint)
    WHERE state = 'failed' AND checkpoint <> '';

-- -------------------------------------------------------- execution events --

-- The bounded audit trail for one recreation.
--
-- Records STATE TRANSITIONS and CHECKPOINTS: every entry is written by
-- HarborMaster about its own action, from a fixed vocabulary. Nothing external
-- influences how many rows one execution can produce, unlike the acquisition
-- trail, which mirrors a registry-driven progress stream.
CREATE TABLE execution_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    execution_id TEXT NOT NULL
        REFERENCES executions (execution_id) ON DELETE CASCADE,

    state TEXT NOT NULL
        CHECK (state IN ('queued', 'validating', 'capturing', 'creating',
                         'starting', 'verifying', 'succeeded', 'failed',
                         'cancelled', 'expired')),

    checkpoint TEXT NOT NULL DEFAULT ''
        CHECK (checkpoint IN ('', 'originalStopped', 'originalParked',
                              'replacementCreated', 'replacementStarted',
                              'replacementVerified', 'replacementQuarantined',
                              'originalRemoved')),

    -- HarborMaster's own words, bounded and sanitised.
    detail TEXT NOT NULL DEFAULT '',

    at TEXT NOT NULL
);

CREATE INDEX idx_execution_event_parent ON execution_events (execution_id, id);
