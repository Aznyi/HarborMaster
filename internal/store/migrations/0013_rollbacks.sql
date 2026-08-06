-- 0013_rollbacks: manual rollback.
--
-- A recreation stopped a container, parked it under a derived name, and started
-- a replacement in its place. A rollback stops the replacement, parks it,
-- renames the preserved original back, starts it, and proves it.
--
-- # What is deliberately NOT here
--
-- No `automatic`, no `trigger`, no `schedule`, no `retry_count`, no `batch_id`,
-- and no `snapshot_id`. Not omitted for later -- absent because the operations
-- do not exist. A rollback is asked for by a person, acts on one execution's
-- two containers, and cannot rebuild a container from a snapshot. That last one
-- would be a RESTORE, which needs evidence a rollback does not have and is not
-- this phase.
--
-- There is also no column that could hold a container id, a name, or an image
-- supplied by a caller. Every identity here is copied from the execution record
-- at request time and re-verified against the live host before anything moves.
--
-- # The checkpoint is the load-bearing column
--
-- `state` says what HarborMaster was DOING. `checkpoint` says what is TRUE of
-- the host, and it is written after each mutation completes and before the next
-- is attempted. After a crash only the second question matters: a process that
-- died in `stoppingReplacement` may have stopped nothing, or may have stopped
-- the replacement without recording it.
--
-- Recovery reads `checkpoint`, never `state`, and never repeats a mutation
-- whose record was uncertain.
--
-- # Single successful use
--
-- `idx_rollback_execution_succeeded` is a PARTIAL unique index over succeeded
-- rows only. One successful rollback per execution, ever -- a second would be
-- acting on an arrangement the first already undid.
--
-- Deliberately partial, unlike the executions table's total unique constraint
-- on `acquisition_id`. A rollback that was refused before it touched anything
-- teaches the operator what to fix, and consuming the only chance to roll back
-- on a refusal would be perverse: the whole point of the refusal is that the
-- rollback did not happen.
--
-- # No foreign key to executions
--
-- Plain columns, matching 0010's reasoning. Retention may prune an execution
-- long before its rollback record ages out, and an audit record that vanished
-- when its evidence did would defeat the purpose of keeping one.
--
-- Timestamps are RFC3339Nano UTC text throughout, matching 0001 through 0012.

-- --------------------------------------------------------------- rollbacks --

CREATE TABLE rollbacks (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    -- The IMMUTABLE public identifier, generated server-side from the system
    -- entropy source. It appears in URLs AND in container names on the host, so
    -- it is random rather than sequential: a sequential one would leak how many
    -- rollbacks have happened and would make a parked container's name
    -- guessable.
    rollback_id TEXT NOT NULL UNIQUE,

    -- The recreation being undone. The ONLY value a caller supplies, and it is
    -- validated by shape before it reaches this column.
    execution_id TEXT NOT NULL,

    -- The production name both containers contend for. Read from the execution
    -- record, which read it from the daemon.
    container_name TEXT NOT NULL,

    -- The two container identities, full 64-character ids. Copied from the
    -- execution record and re-verified against the live host at preflight;
    -- every mutation targets one of these, never a name.
    original_id    TEXT NOT NULL,
    parked_name    TEXT NOT NULL,
    replacement_id TEXT NOT NULL,

    -- Where this rollback moved the replacement to. Empty until that rename
    -- happens, which is what makes the empty string meaningful rather than
    -- merely unset.
    replacement_parked_name TEXT NOT NULL DEFAULT '',

    -- What the execution recorded the original as running. Compared against the
    -- live container at preflight AND after the restore: a rollback that put
    -- back a container running something else would be restoring the wrong
    -- thing.
    original_image    TEXT NOT NULL DEFAULT '',
    original_image_id TEXT NOT NULL DEFAULT '',
    -- What the recreation moved the container onto, recorded so an operator
    -- reading this row sees what is being backed out of.
    replacement_image TEXT NOT NULL DEFAULT '',

    state TEXT NOT NULL
        CHECK (state IN ('queued', 'validating', 'stoppingReplacement',
                         'restoringName', 'startingOriginal', 'verifyingOriginal',
                         'succeeded', 'failed', 'cancelled', 'expired')),

    -- The last mutation known to have COMPLETED and to have been recorded.
    checkpoint TEXT NOT NULL DEFAULT ''
        CHECK (checkpoint IN ('', 'replacementStopped', 'replacementParked',
                              'originalRestored', 'originalStarted',
                              'originalVerified')),

    failure TEXT NOT NULL DEFAULT ''
        CHECK (failure IN ('', 'preflight', 'stop', 'rename', 'start',
                           'healthTimeout', 'unhealthy', 'notStable',
                           'imageMismatch', 'preservation', 'network',
                           'dockerUnavailable', 'timeout',
                           'interrupted', 'persistence', 'internal')),

    refusal TEXT NOT NULL DEFAULT ''
        CHECK (refusal IN ('', 'disabled', 'executionMissing', 'executionActive',
                           'nothingToRollBack', 'originalRemoved',
                           'checkpointUncertain', 'alreadyRolledBack',
                           'conflict', 'limit',
                           'originalMissing', 'originalIdentity',
                           'replacementMissing', 'replacementIdentity',
                           'nameUnavailable',
                           'inventoryStale', 'dockerUnavailable', 'unverifiable')),

    -- HarborMaster's own sentence about the outcome. NEVER a daemon string: an
    -- Engine error can embed the socket path, a command line, and internal
    -- state, and this column reaches an API response and a browser.
    message TEXT NOT NULL DEFAULT '',

    -- Verification results, each a tri-state. 'unknown' means the proof was
    -- never reached, which is deliberately distinct from having failed.
    verify_health       TEXT NOT NULL DEFAULT 'unknown'
        CHECK (verify_health IN ('unknown', 'passed', 'failed')),
    verify_image        TEXT NOT NULL DEFAULT 'unknown'
        CHECK (verify_image IN ('unknown', 'passed', 'failed')),
    verify_preservation TEXT NOT NULL DEFAULT 'unknown'
        CHECK (verify_preservation IN ('unknown', 'passed', 'failed')),
    verify_network      TEXT NOT NULL DEFAULT 'unknown'
        CHECK (verify_network IN ('unknown', 'passed', 'failed')),

    health_state      TEXT    NOT NULL DEFAULT '',
    health_checked    INTEGER NOT NULL DEFAULT 0 CHECK (health_checked IN (0, 1)),
    stability_seconds INTEGER NOT NULL DEFAULT 0 CHECK (stability_seconds >= 0),

    -- The field-by-field preservation comparison, as JSON. Bounded before it is
    -- written: it holds RENDERED configuration, and a wholesale mismatch would
    -- otherwise produce one difference per field.
    --
    -- Every sensitive value in it is a keyed digest, never a value. See
    -- BuildPreservationSummary.
    preservation_report TEXT NOT NULL DEFAULT '',

    -- The manual recovery plan, as JSON. Fixed-vocabulary sentences and
    -- commands built from HarborMaster's own identifiers. Never executed.
    recovery_plan TEXT NOT NULL DEFAULT '',

    requested_at TEXT NOT NULL,
    started_at   TEXT,
    -- When the host was FIRST changed. NULL means it never was, which is the
    -- single most useful fact on a failed record.
    mutated_at   TEXT,
    completed_at TEXT,
    expires_at   TEXT NOT NULL,

    -- The idempotency key, when a caller supplied one. Bounded and printable;
    -- see the executions table for the same reasoning.
    request_key TEXT NOT NULL DEFAULT '',

    -- Who asked. Carried on the record because the OUTCOME is audited by a
    -- worker minutes later with no request and no session -- see 0012.
    requested_by_user_id  TEXT NOT NULL DEFAULT '',
    requested_by_username TEXT NOT NULL DEFAULT '',

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- The listing: newest first, which is the only order this page is read in.
CREATE INDEX idx_rollback_recent ON rollbacks (id DESC);

-- "What happened to this recreation" and "what happened to this container".
CREATE INDEX idx_rollback_execution ON rollbacks (execution_id, id DESC);
CREATE INDEX idx_rollback_container ON rollbacks (container_name, id DESC);

-- ONE ACTIVE ROLLBACK PER CONTAINER, enforced by the database.
--
-- The in-process counters in the service close the window between reading the
-- queue and claiming a row, but they cannot survive a restart and do not exist
-- across two processes. This index is what makes the property hold regardless:
-- two rollbacks moving the same container's names would each rename what the
-- other just renamed, and neither would record what actually happened.
CREATE UNIQUE INDEX idx_rollback_active_container
    ON rollbacks (container_name)
    WHERE state IN ('queued', 'validating', 'stoppingReplacement',
                    'restoringName', 'startingOriginal', 'verifyingOriginal');

-- ONE SUCCESSFUL ROLLBACK PER EXECUTION, ever.
--
-- Partial over succeeded rows: see the header for why a refused rollback does
-- not consume the chance.
CREATE UNIQUE INDEX idx_rollback_execution_succeeded
    ON rollbacks (execution_id)
    WHERE state = 'succeeded';

-- Idempotency. Partial, so the empty key -- which most requests carry -- does
-- not collide with itself.
CREATE UNIQUE INDEX idx_rollback_request_key
    ON rollbacks (request_key)
    WHERE request_key <> '';

-- Answering "what has this account caused on this host" without scanning.
CREATE INDEX idx_rollback_requester
    ON rollbacks (requested_by_user_id, id DESC)
    WHERE requested_by_user_id <> '';

-- ---------------------------------------------------------- rollback events --

-- The bounded audit trail for one rollback.
--
-- Records STATE TRANSITIONS and CHECKPOINTS: every entry is written by
-- HarborMaster about its own action, from a fixed vocabulary. Nothing external
-- influences how many rows one rollback can produce.
CREATE TABLE rollback_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    rollback_id TEXT NOT NULL
        REFERENCES rollbacks (rollback_id) ON DELETE CASCADE,

    state TEXT NOT NULL
        CHECK (state IN ('queued', 'validating', 'stoppingReplacement',
                         'restoringName', 'startingOriginal', 'verifyingOriginal',
                         'succeeded', 'failed', 'cancelled', 'expired')),

    checkpoint TEXT NOT NULL DEFAULT ''
        CHECK (checkpoint IN ('', 'replacementStopped', 'replacementParked',
                              'originalRestored', 'originalStarted',
                              'originalVerified')),

    -- HarborMaster's own words, bounded and sanitised.
    detail TEXT NOT NULL DEFAULT '',

    at TEXT NOT NULL
);

CREATE INDEX idx_rollback_event_parent ON rollback_events (rollback_id, id);
