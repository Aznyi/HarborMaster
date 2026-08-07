-- 0017_automation_audit_target: let the audit log name an update policy and an
-- automation run.
--
-- # Why this is a separate migration from 0016
--
-- 0014 fixed exactly this class of bug for rollback: a host-changing capability
-- shipped with an audit vocabulary that had no word for its target, so every
-- attempt to record it was refused by the CHECK, the recorder logged the
-- failure and carried on -- by design, an audit write must never fail the
-- operation it describes -- and the result was a capability with no security
-- audit trail and nothing in the running system saying so.
--
-- Phase 11 adds two new audit targets. They are widened here, in their own
-- migration with their own name, so the change is visible in the migration list
-- rather than buried in a 300-line schema addition.
--
-- # Why a table rebuild
--
-- SQLite cannot alter a CHECK constraint in place. Same procedure as 0014:
-- copy, drop, rename, recreate the indexes. Nothing references audit_events
-- with a foreign key, so the rebuild cannot orphan a row. Every existing row is
-- carried across unchanged, including its id.

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

    -- 'updatePolicy' and 'automation' are the only additions.
    target_type TEXT NOT NULL DEFAULT ''
        CHECK (target_type IN ('', 'user', 'session', 'container', 'snapshot',
                               'drift', 'policy', 'violation', 'plan',
                               'acquisition', 'execution', 'rollback',
                               'updatePolicy', 'automation',
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

-- Recreated with the same names and definitions as 0011 and 0014, so an
-- upgraded database and a fresh one are indistinguishable afterwards.
CREATE INDEX idx_audit_occurred ON audit_events (occurred_at DESC, id DESC);
CREATE INDEX idx_audit_action   ON audit_events (action, id DESC);
CREATE INDEX idx_audit_actor    ON audit_events (actor_user_id, id DESC)
    WHERE actor_user_id <> '';
CREATE INDEX idx_audit_outcome  ON audit_events (outcome, id DESC)
    WHERE outcome <> 'succeeded';
