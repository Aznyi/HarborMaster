-- 0028_execution_refusal_vocabulary: record every refusal the code can produce.
--
-- # This drift was already shipping
--
-- Three refusals exist in the Go vocabulary and were REJECTED by this column:
--
--     selfUpdate                 the container HarborMaster is running in
--     namespaceProviderMissing   a shared namespace whose provider is gone
--     dependentsNotRebindable    invariant A
--
-- Those are among the most safety-critical refusals in the product. The
-- recreation was still REFUSED -- that decision is made in Go, not in SQL --
-- but the record write failed, so an operator saw a refused execution with no
-- reason attached and no way to tell which check had said no. Self-update
-- protection and invariant A are exactly the two an operator most needs
-- explained.
--
-- It is the sixth occurrence of this defect's shape: 0014, 0017, 0021, 0026 and
-- 0027 each fixed it in a different table. `TestEveryExecutionRefusalIsAccepted
-- ByTheSchema` now writes one row per domain.ExecutionRefusals entry against a
-- real database, which is the guard that was missing here -- the same one 0021
-- wrote for audit target types.
--
-- # Why a table rebuild
--
-- SQLite cannot alter a CHECK in place. Same procedure as the five migrations
-- above: copy, drop, rename, recreate the indexes.
--
-- The definition below is the LIVE schema, read back from a migrated database
-- rather than transcribed from 0010 by hand. This table has forty-eight columns
-- and two of them were added by a later ALTER, so a hand copy would have
-- silently dropped them.
--
-- Every row keeps its id. An execution id appears in parked and quarantined
-- CONTAINER NAMES on the host, so renumbering would break the link between a
-- record and the evidence it left behind.

CREATE TABLE executions_rebuilt (
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
                           'secretUnavailable', 'nameUnavailable',
                           -- Added by 0028. Present in the Go vocabulary since
                           -- the self-update and dependency phases; refused by
                           -- this column until now.
                           'selfUpdate', 'namespaceProviderMissing',
                           'dependentsNotRebindable')),
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
, requested_by_user_id TEXT NOT NULL DEFAULT '', requested_by_username TEXT NOT NULL DEFAULT '');

INSERT INTO executions_rebuilt
    (id, host_id, execution_id, acquisition_id, plan_id, snapshot_id, container_id, container_name, old_image, old_image_id, old_image_digest, target_registry, target_repository, target_digest, target_reference, target_image_id, target_os, target_arch, target_variant, state, checkpoint, failure, refusal, message, replacement_id, parked_name, quarantine_name, original_removed, verify_health, verify_image, verify_preservation, verify_network, health_state, health_checked, stability_seconds, preservation_report, recovery_plan, requested_at, started_at, mutated_at, completed_at, expires_at, request_key, plan_digest, created_at, updated_at, requested_by_user_id, requested_by_username)
SELECT
    id, host_id, execution_id, acquisition_id, plan_id, snapshot_id, container_id, container_name, old_image, old_image_id, old_image_digest, target_registry, target_repository, target_digest, target_reference, target_image_id, target_os, target_arch, target_variant, state, checkpoint, failure, refusal, message, replacement_id, parked_name, quarantine_name, original_removed, verify_health, verify_image, verify_preservation, verify_network, health_state, health_checked, stability_seconds, preservation_report, recovery_plan, requested_at, started_at, mutated_at, completed_at, expires_at, request_key, plan_digest, created_at, updated_at, requested_by_user_id, requested_by_username
FROM executions;

DROP TABLE executions;

ALTER TABLE executions_rebuilt RENAME TO executions;

-- Recreated exactly as the live schema had them, so an upgraded database and
-- a fresh one are indistinguishable afterwards.
CREATE UNIQUE INDEX idx_execution_active_container
    ON executions (container_id)
    WHERE state IN ('queued', 'validating', 'capturing', 'creating',
                    'starting', 'verifying');
CREATE INDEX idx_execution_attention  ON executions (state, checkpoint)
    WHERE state = 'failed' AND checkpoint <> '';
CREATE INDEX idx_execution_completed  ON executions (completed_at)
    WHERE completed_at IS NOT NULL;
CREATE INDEX idx_execution_container  ON executions (container_id, id DESC);
CREATE INDEX idx_execution_plan       ON executions (plan_id, id DESC);
CREATE UNIQUE INDEX idx_execution_request_key
    ON executions (request_key)
    WHERE request_key <> '';
CREATE INDEX idx_execution_requested  ON executions (requested_at DESC);
CREATE INDEX idx_execution_requester
    ON executions (requested_by_user_id, id DESC)
    WHERE requested_by_user_id <> '';
CREATE INDEX idx_execution_state      ON executions (state, id DESC);
