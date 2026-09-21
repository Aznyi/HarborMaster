-- harbormaster:foreign_keys=off
--
-- 0037_rollback_restart_policy_failure: accept the restart-policy failure on a
-- rollback.
--
-- # What was wrong
--
-- A rollback restores the original's restart policy after giving it back its
-- name and before starting it. When that write fails the original is not
-- started -- starting it as "no" would leave a workload that runs until its
-- next crash and then stays down -- and the rollback fails. That outcome was
-- being recorded as 'internal', which tells an operator nothing: the original
-- holds its name, is stopped, and needs one `docker update` before a start.
-- It gets its own word, exactly as the execution side does in 0036.
--
-- # Why a table rebuild, and why foreign keys are OFF
--
-- SQLite cannot alter a CHECK in place. `rollback_events` references this
-- table ON DELETE CASCADE, and with foreign keys on the DROP TABLE below would
-- delete every event row. The runner reads the directive on the first line,
-- runs this file with foreign keys off on its own connection, checks for
-- orphans before committing, and turns them back on. The definition is the
-- LIVE schema read back from a migrated database. Every row keeps its id.

CREATE TABLE rollbacks_rebuilt (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',
    rollback_id TEXT NOT NULL UNIQUE,
    execution_id TEXT NOT NULL,
    container_name TEXT NOT NULL,
    original_id    TEXT NOT NULL,
    parked_name    TEXT NOT NULL,
    replacement_id TEXT NOT NULL,
    replacement_parked_name TEXT NOT NULL DEFAULT '',
    original_image    TEXT NOT NULL DEFAULT '',
    original_image_id TEXT NOT NULL DEFAULT '',
    replacement_image TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL
        CHECK (state IN ('queued', 'validating', 'stoppingReplacement',
                         'restoringName', 'startingOriginal', 'verifyingOriginal',
                         'succeeded', 'failed', 'cancelled', 'expired')),
    checkpoint TEXT NOT NULL DEFAULT ''
        CHECK (checkpoint IN ('', 'replacementStopped', 'replacementParked',
                              'originalRestored', 'originalStarted',
                              'originalVerified')),
    failure TEXT NOT NULL DEFAULT ''
        CHECK (failure IN ('', 'preflight', 'stop', 'rename', 'start',
                           'healthTimeout', 'unhealthy', 'notStable',
                           'imageMismatch', 'preservation', 'network',
                           'dockerUnavailable', 'timeout',
                           'interrupted', 'persistence', 'internal',
                           -- Added by 0037. See the header.
                           'restartPolicy')),
    refusal TEXT NOT NULL DEFAULT ''
        CHECK (refusal IN ('', 'disabled', 'executionMissing', 'executionActive',
                           'nothingToRollBack', 'originalRemoved',
                           'checkpointUncertain', 'alreadyRolledBack',
                           'conflict', 'limit',
                           'originalMissing', 'originalIdentity',
                           'replacementMissing', 'replacementIdentity',
                           'nameUnavailable',
                           'inventoryStale', 'dockerUnavailable', 'unverifiable')),
    message TEXT NOT NULL DEFAULT '',
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
    preservation_report TEXT NOT NULL DEFAULT '',
    recovery_plan TEXT NOT NULL DEFAULT '',
    requested_at TEXT NOT NULL,
    started_at   TEXT,
    mutated_at   TEXT,
    completed_at TEXT,
    expires_at   TEXT NOT NULL,
    request_key TEXT NOT NULL DEFAULT '',
    requested_by_user_id  TEXT NOT NULL DEFAULT '',
    requested_by_username TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

INSERT INTO rollbacks_rebuilt
    (id, host_id, rollback_id, execution_id, container_name, original_id, parked_name, replacement_id, replacement_parked_name, original_image, original_image_id, replacement_image, state, checkpoint, failure, refusal, message, verify_health, verify_image, verify_preservation, verify_network, health_state, health_checked, stability_seconds, preservation_report, recovery_plan, requested_at, started_at, mutated_at, completed_at, expires_at, request_key, requested_by_user_id, requested_by_username, created_at, updated_at)
SELECT
    id, host_id, rollback_id, execution_id, container_name, original_id, parked_name, replacement_id, replacement_parked_name, original_image, original_image_id, replacement_image, state, checkpoint, failure, refusal, message, verify_health, verify_image, verify_preservation, verify_network, health_state, health_checked, stability_seconds, preservation_report, recovery_plan, requested_at, started_at, mutated_at, completed_at, expires_at, request_key, requested_by_user_id, requested_by_username, created_at, updated_at
FROM rollbacks;

DROP TABLE rollbacks;

ALTER TABLE rollbacks_rebuilt RENAME TO rollbacks;

CREATE INDEX idx_rollback_recent ON rollbacks (id DESC);
CREATE INDEX idx_rollback_execution ON rollbacks (execution_id, id DESC);
CREATE INDEX idx_rollback_container ON rollbacks (container_name, id DESC);
CREATE UNIQUE INDEX idx_rollback_active_container
    ON rollbacks (container_name)
    WHERE state IN ('queued', 'validating', 'stoppingReplacement',
                    'restoringName', 'startingOriginal', 'verifyingOriginal');
CREATE UNIQUE INDEX idx_rollback_execution_succeeded
    ON rollbacks (execution_id)
    WHERE state = 'succeeded';
CREATE UNIQUE INDEX idx_rollback_request_key
    ON rollbacks (request_key)
    WHERE request_key <> '';
CREATE INDEX idx_rollback_requester
    ON rollbacks (requested_by_user_id, id DESC)
    WHERE requested_by_user_id <> '';
