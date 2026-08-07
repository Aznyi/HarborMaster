-- 0021_notification_audit_target: let the audit log name a notification
-- destination and a notification rule.
--
-- # This is the third time
--
-- 0014 fixed it for rollback. 0017 fixed it for automation. Both times the
-- shape was identical: a subsystem shipped with audit ACTIONS in the Go
-- vocabulary and no matching word in the database's CHECK, so every attempt to
-- record one was refused, the recorder logged the failure and carried on -- by
-- design, an audit write must never fail the operation it describes -- and the
-- result was a set of administrator actions with no audit trail and nothing in
-- the running interface saying so.
--
-- It happened again in Phase 12, and it was found by a live smoke test against
-- a real process rather than by any unit test: the API tests use a stub audit
-- recorder, which has no CHECK constraint to violate. The lesson recorded here
-- is that the Go vocabulary and this CHECK are ONE vocabulary in two places,
-- and adding to either without the other produces a silent hole.
--
-- `TestEveryAuditTargetTypeIsAcceptedByTheSchema` now writes one row per
-- domain.AuditTargetTypes entry against a real database, so a fourth time
-- fails the build instead of shipping.
--
-- # Why a table rebuild
--
-- SQLite cannot alter a CHECK constraint in place. Same procedure as 0014 and
-- 0017: copy, drop, rename, recreate the indexes. Nothing references
-- audit_events with a foreign key, so the rebuild cannot orphan a row. Every
-- existing row is carried across unchanged, including its id.

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

    -- 'notificationDestination' and 'notificationRule' are the only additions.
    --
    -- They are separate words rather than one 'notification', because
    -- archiving a destination and archiving a rule have different consequences
    -- -- one destroys a credential, the other does not -- and a reader of the
    -- audit log must not have to guess which happened.
    target_type TEXT NOT NULL DEFAULT ''
        CHECK (target_type IN ('', 'user', 'session', 'container', 'snapshot',
                               'drift', 'policy', 'violation', 'plan',
                               'acquisition', 'execution', 'rollback',
                               'updatePolicy', 'automation',
                               'notificationDestination', 'notificationRule',
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

-- Recreated with the same names and definitions as 0011, 0014, and 0017, so an
-- upgraded database and a fresh one are indistinguishable afterwards.
CREATE INDEX idx_audit_occurred ON audit_events (occurred_at DESC, id DESC);
CREATE INDEX idx_audit_action   ON audit_events (action, id DESC);
CREATE INDEX idx_audit_actor    ON audit_events (actor_user_id, id DESC)
    WHERE actor_user_id <> '';
CREATE INDEX idx_audit_outcome  ON audit_events (outcome, id DESC)
    WHERE outcome <> 'succeeded';
