-- 0034_image_audit_target: let the audit log name a local image.
--
-- # The fifth time, and the test caught it again
--
-- 0014 fixed this for rollback, 0017 for automation, 0021 for notifications,
-- 0026 for dependencies. Each of the first three shipped with audit actions in
-- the Go vocabulary and no matching word in this CHECK, so every attempt to
-- record one was refused, the recorder logged the failure and carried on -- by
-- design, an audit write must never fail the operation it describes -- and the
-- result was privileged actions with no audit trail and nothing in the running
-- interface saying so.
--
-- 0021 added `TestEveryAuditTargetTypeIsAcceptedByTheSchema`, which writes one
-- row per domain.AuditTargetTypes entry against a real database. It caught
-- `dependency` in Phase 16 and it caught `image` here, the moment the constant
-- was added to the Go list -- before any of it ran against a host. The Go
-- vocabulary and this CHECK are ONE vocabulary in two places, and the build
-- refuses to let them diverge.
--
-- # Why `image` is its own word
--
-- Not folded into `container`, and not into `acquisition`. The subject of the
-- record is the ARTEFACT: `image.removed` says a local image was destroyed, and
-- the target id is its image id rather than any container's. A reader asking
-- "what happened to that image" must not have to infer it from a row about
-- something else.
--
-- It matters more here than it did for the other four. An image removal is the
-- one host change HarborMaster cannot undo -- a removed image comes back only
-- from a registry, and only if that registry still serves the same content --
-- and it happens on a timer with nobody watching. This record is the ONLY
-- account of why the image is gone.
--
-- # Why a table rebuild
--
-- SQLite cannot alter a CHECK constraint in place. Same procedure as 0014,
-- 0017, 0021, and 0026: copy, drop, rename, recreate the indexes. Nothing
-- references audit_events with a foreign key, so the rebuild cannot orphan a
-- row. Every existing row is carried across unchanged, including its id -- the
-- audit log is append-only evidence, and a migration that renumbered it would
-- be rewriting history to add a word.

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

    -- 'image' is the only addition.
    target_type TEXT NOT NULL DEFAULT ''
        CHECK (target_type IN ('', 'user', 'session', 'container', 'snapshot',
                               'drift', 'policy', 'violation', 'plan',
                               'acquisition', 'execution', 'rollback',
                               'updatePolicy', 'automation',
                               'notificationDestination', 'notificationRule',
                               'dependency', 'image',
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

-- Recreated with the same names and definitions as 0011, 0014, 0017, 0021, and
-- 0026, so an upgraded database and a fresh one are indistinguishable
-- afterwards.
CREATE INDEX idx_audit_occurred ON audit_events (occurred_at DESC, id DESC);
CREATE INDEX idx_audit_action   ON audit_events (action, id DESC);
CREATE INDEX idx_audit_actor    ON audit_events (actor_user_id, id DESC)
    WHERE actor_user_id <> '';
CREATE INDEX idx_audit_outcome  ON audit_events (outcome, id DESC)
    WHERE outcome <> 'succeeded';
