-- 0014_rollback_audit_target: let the audit log name a rollback.
--
-- # What was wrong
--
-- `audit_events.target_type` carries a CHECK against a closed vocabulary, which
-- is the right shape: a free-text target column is one that eventually holds a
-- daemon string. But the vocabulary was fixed in 0011, before manual rollback
-- existed, and it does not contain 'rollback'.
--
-- So every attempt to audit a rollback -- the request, and more importantly the
-- OUTCOME, which is the row recording that containers on this host were moved
-- -- was refused by the constraint. The recorder logs that failure and carries
-- on, by design: an audit write must never fail the operation it describes.
-- The result was a host-changing capability with no security-audit trail, and
-- nothing in the running system saying so.
--
-- This migration widens the vocabulary and nothing else.
--
-- # Why a table rebuild
--
-- SQLite cannot alter a CHECK constraint in place. The table is copied into one
-- with the corrected constraint, the original is dropped, and the copy takes
-- its name. Nothing references audit_events with a foreign key -- deliberately,
-- see 0011 -- so the rebuild cannot orphan a row in another table.
--
-- Every existing row is carried across unchanged, including its id. An audit
-- history that lost or renumbered rows during an upgrade would be one that
-- could not be relied on afterwards.

CREATE TABLE audit_events_rebuilt (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    event_id TEXT NOT NULL UNIQUE,

    action  TEXT NOT NULL CHECK (length(action) > 0),
    outcome TEXT NOT NULL
        CHECK (outcome IN ('succeeded', 'failed', 'denied')),

    actor_user_id    TEXT NOT NULL DEFAULT '',
    actor_username   TEXT NOT NULL DEFAULT '',
    actor_role       TEXT NOT NULL DEFAULT ''
        CHECK (actor_role IN ('', 'viewer', 'operator', 'administrator')),
    actor_session_id TEXT NOT NULL DEFAULT '',

    -- 'rollback' is the only addition.
    target_type TEXT NOT NULL DEFAULT ''
        CHECK (target_type IN ('', 'user', 'session', 'container', 'snapshot',
                               'drift', 'policy', 'violation', 'plan',
                               'acquisition', 'execution', 'rollback',
                               'inventory', 'system')),
    target_id   TEXT NOT NULL DEFAULT '',
    target_name TEXT NOT NULL DEFAULT '',

    request_id  TEXT NOT NULL DEFAULT '',
    client_addr TEXT NOT NULL DEFAULT '',

    reason TEXT NOT NULL DEFAULT '',

    occurred_at TEXT NOT NULL
);

INSERT INTO audit_events_rebuilt
    (id, event_id, action, outcome,
     actor_user_id, actor_username, actor_role, actor_session_id,
     target_type, target_id, target_name,
     request_id, client_addr, reason, occurred_at)
SELECT
     id, event_id, action, outcome,
     actor_user_id, actor_username, actor_role, actor_session_id,
     target_type, target_id, target_name,
     request_id, client_addr, reason, occurred_at
FROM audit_events;

DROP TABLE audit_events;

ALTER TABLE audit_events_rebuilt RENAME TO audit_events;

-- Dropping the table dropped its indexes. Recreated with the same names and
-- the same definitions as 0011, so an upgraded database and a fresh one are
-- indistinguishable afterwards.
CREATE INDEX idx_audit_occurred ON audit_events (occurred_at DESC, id DESC);
CREATE INDEX idx_audit_action   ON audit_events (action, id DESC);
CREATE INDEX idx_audit_actor    ON audit_events (actor_user_id, id DESC)
    WHERE actor_user_id <> '';
CREATE INDEX idx_audit_outcome  ON audit_events (outcome, id DESC)
    WHERE outcome <> 'succeeded';
