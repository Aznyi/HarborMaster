-- 0027_plan_rebind_update_type: let a change plan record a reattachment.
--
-- # This is the same defect as 0014, 0017, 0021 and 0026, in a different table
--
-- Each of those fixed an audit CHECK that had not learned a word the Go
-- vocabulary already used. This one is the same shape and worse in consequence.
--
-- Phase 16 added `domain.UpdateRebind` ("rebind"): a plan to recreate a
-- container on THE DIGEST IT IS ALREADY RUNNING, so it can attach to the
-- replacement of a container whose namespace it shares. The vocabulary, the
-- planner, the coordinator, the API and the interface all learned it. This
-- CHECK did not.
--
-- # What that cost, observed live
--
-- Stage 5 acceptance against a real daemon: HarborMaster updated a network
-- namespace provider, verified it, recorded the dependency operation, and then
-- could not insert the reattachment plan:
--
--     could not produce reattachment plans
--     insert plan: CHECK constraint failed: update_type IN (...)
--
-- The follower retried every ten seconds, forever. The dependent stayed
-- attached to a container id that no longer existed: still running, reporting
-- nothing wrong, with no network. That is precisely the silent breakage the
-- whole phase exists to prevent, and the phase caused it.
--
-- Nothing above the store caught it because every dependency test uses a fake
-- plan store. `TestEveryUpdateTypeIsAcceptedByTheSchema` now writes one plan
-- per `domain.UpdateTypes` entry against a real database, which is the guard
-- 0021 wrote for audit targets and which would have failed this on the first
-- run.
--
-- # Why image_intel is NOT widened
--
-- `image_intel` records what a REGISTRY reports about a tag. A rebind is not a
-- registry finding — no tag moved, no comparison was made, and image
-- intelligence never produces one. Widening that CHECK would be inviting a
-- value that column has no meaning for. Only `change_plans` gains the word.
--
-- # Why a table rebuild
--
-- SQLite cannot alter a CHECK in place. Same procedure as 0014, 0017, 0021 and
-- 0026: copy, drop, rename, recreate the indexes. `change_plans` is referenced
-- by no foreign key, and its own reference to `snapshots` is carried across
-- unchanged. Every row keeps its id: a plan id appears in acquisition and
-- execution records, and renumbering would break the trail between them.

CREATE TABLE change_plans_rebuilt (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    plan_id TEXT NOT NULL UNIQUE,

    container_id   TEXT NOT NULL,
    container_name TEXT NOT NULL DEFAULT '',

    current_image   TEXT NOT NULL DEFAULT '',
    proposed_image  TEXT NOT NULL DEFAULT '',
    current_digest  TEXT NOT NULL DEFAULT '',
    proposed_digest TEXT NOT NULL DEFAULT '',
    update_type     TEXT NOT NULL DEFAULT 'none',

    snapshot_id        INTEGER REFERENCES snapshots (id) ON DELETE SET NULL,
    snapshot_available INTEGER NOT NULL DEFAULT 0,
    restore_readiness  TEXT    NOT NULL DEFAULT 'unknown',

    drift_open          INTEGER NOT NULL DEFAULT 0,
    drift_max_severity  TEXT    NOT NULL DEFAULT '',
    policy_open         INTEGER NOT NULL DEFAULT 0,
    policy_max_severity TEXT    NOT NULL DEFAULT '',

    registry_status TEXT NOT NULL DEFAULT 'pending',
    registry_detail       TEXT NOT NULL DEFAULT '',
    proposed_published_at TEXT,

    risk_score     INTEGER NOT NULL DEFAULT 0,
    risk_band      TEXT    NOT NULL DEFAULT 'veryLow',
    recommendation TEXT    NOT NULL DEFAULT 'unknown',
    summary TEXT NOT NULL DEFAULT '',
    factors TEXT NOT NULL DEFAULT '[]',

    plan_version    INTEGER NOT NULL DEFAULT 1,
    planner_version TEXT    NOT NULL DEFAULT '',

    input_digest TEXT NOT NULL,

    generated_at TEXT NOT NULL,

    -- 'rebind' is the only addition.
    CHECK (update_type IN ('none', 'digest', 'patch', 'minor', 'major',
                           'prerelease', 'rebind', 'unknown')),
    CHECK (restore_readiness IN ('unknown', 'ready', 'warning', 'not_ready')),
    CHECK (registry_status IN ('pending', 'ok', 'failed', 'rateLimited',
                               'unauthorized', 'notFound', 'unsupported')),
    CHECK (risk_band IN ('veryLow', 'low', 'medium', 'high', 'critical')),
    CHECK (recommendation IN ('proceed', 'proceedWithCaution', 'manualReview',
                              'notRecommended', 'unknown')),
    CHECK (snapshot_available IN (0, 1)),
    CHECK (risk_score >= 0 AND risk_score <= 100),
    CHECK (drift_open >= 0 AND policy_open >= 0),
    CHECK (LENGTH(summary) <= 1024),
    CHECK (LENGTH(registry_detail) <= 512),
    CHECK (LENGTH(factors) <= 16384)
);

INSERT INTO change_plans_rebuilt
    (id, host_id, plan_id, container_id, container_name,
     current_image, proposed_image, current_digest, proposed_digest, update_type,
     snapshot_id, snapshot_available, restore_readiness,
     drift_open, drift_max_severity, policy_open, policy_max_severity,
     registry_status, registry_detail, proposed_published_at,
     risk_score, risk_band, recommendation, summary, factors,
     plan_version, planner_version, input_digest, generated_at)
SELECT
     id, host_id, plan_id, container_id, container_name,
     current_image, proposed_image, current_digest, proposed_digest, update_type,
     snapshot_id, snapshot_available, restore_readiness,
     drift_open, drift_max_severity, policy_open, policy_max_severity,
     registry_status, registry_detail, proposed_published_at,
     risk_score, risk_band, recommendation, summary, factors,
     plan_version, planner_version, input_digest, generated_at
FROM change_plans;

DROP TABLE change_plans;

ALTER TABLE change_plans_rebuilt RENAME TO change_plans;

-- Recreated with the same names and definitions as 0008, so an upgraded
-- database and a fresh one are indistinguishable afterwards.
CREATE UNIQUE INDEX idx_plan_fingerprint ON change_plans (container_id, input_digest);
CREATE INDEX idx_plan_container ON change_plans (container_id, id DESC);
