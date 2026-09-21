-- harbormaster:foreign_keys=off
--
-- 0036_execution_restore: record what became of restoring a failed update, and
-- accept the restart-policy failure.
--
-- # Two changes, one rebuild
--
--  1. `failure` gains 'restartPolicy'. When the parked original's restart
--     policy cannot be set to "no", the recreation stops before creating a
--     replacement -- a parked container that could start by itself after a
--     daemon restart, beside a replacement holding its name and its ports, is
--     the race the suspension exists to prevent. That outcome needs its own
--     word: an operator reading "internal failure" would not know the
--     original could resurrect, or that it can be restored without a
--     replacement to move.
--
--  2. Three columns record the AUTOMATIC RESTORE of a failed update. A manual
--     update that fails after the mutation point now asks the rollback service
--     to put the original back, and the update's own record must say what came
--     of that: requested, restored, failed, refused, or unavailable. An
--     operator reading a failed update needs one answer -- is the service
--     back? -- and the rollback row alone does not give it to them.
--
-- # Why a table rebuild
--
-- SQLite cannot alter a CHECK in place. Same procedure as 0028: copy, drop,
-- rename, recreate the indexes. The definition below is the LIVE schema read
-- back from a migrated database, including the two columns 0012 added and the
-- one 0035 added.
--
-- # Why foreign keys are OFF for this file
--
-- With foreign keys on, DROP TABLE performs an implicit DELETE FROM, and ON
-- DELETE CASCADE fires on it: `execution_events` references this table and
-- every event row would be deleted by the rebuild. The migration runner reads
-- the directive on the first line, runs this file on a connection with foreign
-- keys off, checks for orphans before committing, and turns them back on.
-- Every row keeps its id.

CREATE TABLE executions_rebuilt (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',
    execution_id TEXT NOT NULL UNIQUE,
    acquisition_id TEXT NOT NULL UNIQUE,
    plan_id        TEXT NOT NULL,
    snapshot_id    INTEGER NOT NULL DEFAULT 0,
    container_id   TEXT NOT NULL,
    container_name TEXT NOT NULL,
    old_image        TEXT NOT NULL DEFAULT '',
    old_image_id     TEXT NOT NULL DEFAULT '',
    old_image_digest TEXT NOT NULL DEFAULT '',
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
                           'timeout', 'interrupted', 'persistence', 'internal',
                           -- Added by 0036. See the header.
                           'restartPolicy')),
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
                           'selfUpdate', 'namespaceProviderMissing',
                           'dependentsNotRebindable',
                           'snapshotChanged',
                           'approvalMissing')),
    message TEXT NOT NULL DEFAULT '',
    replacement_id   TEXT NOT NULL DEFAULT '',
    parked_name      TEXT NOT NULL DEFAULT '',
    quarantine_name  TEXT NOT NULL DEFAULT '',
    original_removed INTEGER NOT NULL DEFAULT 0 CHECK (original_removed IN (0, 1)),
    verify_health TEXT NOT NULL DEFAULT 'unknown'
        CHECK (verify_health IN ('unknown', 'passed', 'failed')),
    verify_image TEXT NOT NULL DEFAULT 'unknown'
        CHECK (verify_image IN ('unknown', 'passed', 'failed')),
    verify_preservation TEXT NOT NULL DEFAULT 'unknown'
        CHECK (verify_preservation IN ('unknown', 'passed', 'failed')),
    verify_network TEXT NOT NULL DEFAULT 'unknown'
        CHECK (verify_network IN ('unknown', 'passed', 'failed')),
    health_state      TEXT    NOT NULL DEFAULT ''
        CHECK (health_state IN ('', 'none', 'starting', 'healthy', 'unhealthy')),
    health_checked    INTEGER NOT NULL DEFAULT 0 CHECK (health_checked IN (0, 1)),
    stability_seconds INTEGER NOT NULL DEFAULT 0,
    preservation_report TEXT NOT NULL DEFAULT '',
    recovery_plan TEXT NOT NULL DEFAULT '',
    requested_at TEXT NOT NULL,
    started_at   TEXT,
    mutated_at   TEXT,
    completed_at TEXT,
    expires_at   TEXT NOT NULL,
    request_key TEXT NOT NULL DEFAULT '',
    plan_digest TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    requested_by_user_id  TEXT NOT NULL DEFAULT '',
    requested_by_username TEXT NOT NULL DEFAULT '',
    original_restart_policy TEXT NOT NULL DEFAULT '',
    -- What became of restoring the original after a failure that changed the
    -- host. Empty when no restore applies. See domain.ExecutionRestore.
    restore_state TEXT NOT NULL DEFAULT ''
        CHECK (restore_state IN ('', 'requested', 'restored', 'failed',
                                 'refused', 'unavailable')),
    restore_rollback_id TEXT NOT NULL DEFAULT '',
    -- HarborMaster's own sentence about a refusal or a failure. Never a daemon
    -- string.
    restore_detail TEXT NOT NULL DEFAULT ''
);

INSERT INTO executions_rebuilt
    (id, host_id, execution_id, acquisition_id, plan_id, snapshot_id, container_id, container_name, old_image, old_image_id, old_image_digest, target_registry, target_repository, target_digest, target_reference, target_image_id, target_os, target_arch, target_variant, state, checkpoint, failure, refusal, message, replacement_id, parked_name, quarantine_name, original_removed, verify_health, verify_image, verify_preservation, verify_network, health_state, health_checked, stability_seconds, preservation_report, recovery_plan, requested_at, started_at, mutated_at, completed_at, expires_at, request_key, plan_digest, created_at, updated_at, requested_by_user_id, requested_by_username, original_restart_policy)
SELECT
    id, host_id, execution_id, acquisition_id, plan_id, snapshot_id, container_id, container_name, old_image, old_image_id, old_image_digest, target_registry, target_repository, target_digest, target_reference, target_image_id, target_os, target_arch, target_variant, state, checkpoint, failure, refusal, message, replacement_id, parked_name, quarantine_name, original_removed, verify_health, verify_image, verify_preservation, verify_network, health_state, health_checked, stability_seconds, preservation_report, recovery_plan, requested_at, started_at, mutated_at, completed_at, expires_at, request_key, plan_digest, created_at, updated_at, requested_by_user_id, requested_by_username, original_restart_policy
FROM executions;

DROP TABLE executions;

ALTER TABLE executions_rebuilt RENAME TO executions;

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
-- The sweep that advances pending restores reads exactly this.
CREATE INDEX idx_execution_restore_pending ON executions (restore_state, completed_at)
    WHERE restore_state = 'requested';
