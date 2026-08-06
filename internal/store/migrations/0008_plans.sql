-- 0008_plans: change planning and risk analysis.
--
-- HarborMaster remains read-only with respect to Docker. A plan is an
-- ASSESSMENT of a proposed image change, built from data HarborMaster already
-- holds. Nothing executes it, and nothing in this migration grants any ability
-- to pull an image, recreate a container, or roll one back -- there is no state
-- column, no queue, and no target, because a plan is analysis rather than an
-- instruction.
--
-- Timestamps are RFC3339Nano UTC text throughout, matching 0001 through 0007.

-- ------------------------------------------------------------ change plans --

-- One row per generated assessment.
--
-- # Plans are immutable
--
-- A plan records what was known AT ONE MOMENT. It is never updated in place: a
-- changed world produces a NEW row, and the old one remains as the record of
-- what was believed when a decision was made. That is what makes a plan usable
-- as evidence afterwards.
--
-- There is deliberately no `status`, `applied_at`, or `superseded` column.
-- Adding one would make plans mutable and would be the first step toward
-- treating them as work items, which they are not. "Superseded" is derived at
-- read time from the existence of a newer plan for the same container.
--
-- # What is NOT duplicated here
--
-- A plan references its baseline snapshot by id and copies nothing from it --
-- no document, no checksum, no spec. Drift and policy are stored as the SUMMARY
-- the assessment acted on, not as copies of the records; those live in
-- drift_records and policy_violations and have one home each. The registry's
-- own findings live in image_intel.
--
-- What IS stored is the summary the risk model actually read, because a plan
-- must remain readable and explicable after the underlying records have moved
-- on. Re-deriving it at read time would mean an old plan silently reporting
-- today's drift counts beside yesterday's verdict.
CREATE TABLE change_plans (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    -- The IMMUTABLE public identifier, generated server-side from the system
    -- entropy source and never accepted from a caller.
    plan_id TEXT NOT NULL UNIQUE,

    container_id   TEXT NOT NULL,
    container_name TEXT NOT NULL DEFAULT '',

    -- The change being assessed. Familiar references for display, digests for
    -- the comparison the model actually made.
    current_image   TEXT NOT NULL DEFAULT '',
    proposed_image  TEXT NOT NULL DEFAULT '',
    current_digest  TEXT NOT NULL DEFAULT '',
    proposed_digest TEXT NOT NULL DEFAULT '',
    update_type     TEXT NOT NULL DEFAULT 'none',

    -- The baseline the container would refer back to. Referenced by id, with
    -- ON DELETE SET NULL rather than CASCADE: a plan whose snapshot has since
    -- been pruned is still a truthful record of what was known, and deleting it
    -- would destroy the evidence rather than the stale pointer.
    snapshot_id        INTEGER REFERENCES snapshots (id) ON DELETE SET NULL,
    snapshot_available INTEGER NOT NULL DEFAULT 0,
    restore_readiness  TEXT    NOT NULL DEFAULT 'unknown',

    -- The drift and policy SUMMARIES the assessment read, frozen with the plan.
    drift_open          INTEGER NOT NULL DEFAULT 0,
    drift_max_severity  TEXT    NOT NULL DEFAULT '',
    policy_open         INTEGER NOT NULL DEFAULT 0,
    policy_max_severity TEXT    NOT NULL DEFAULT '',

    -- How much the registry data behind this plan can be relied on.
    registry_status TEXT NOT NULL DEFAULT 'pending',
    -- HarborMaster's own words for a non-OK status, from a fixed set. Never a
    -- registry-supplied string.
    registry_detail       TEXT NOT NULL DEFAULT '',
    proposed_published_at TEXT,

    risk_score     INTEGER NOT NULL DEFAULT 0,
    risk_band      TEXT    NOT NULL DEFAULT 'veryLow',
    recommendation TEXT    NOT NULL DEFAULT 'unknown',
    -- One sentence naming the dominant reason.
    summary TEXT NOT NULL DEFAULT '',
    -- The contributing factors, as the canonical JSON encoding of
    -- []domain.RiskFactor, in fixed rule order. Read only as a whole
    -- explanation, never filtered or joined on, so a relational table would buy
    -- nothing and cost a join per plan.
    factors TEXT NOT NULL DEFAULT '[]',

    plan_version    INTEGER NOT NULL DEFAULT 1,
    planner_version TEXT    NOT NULL DEFAULT '',

    -- The fingerprint of everything the assessment read. Two passes over an
    -- unchanged world produce the same digest, and the second writes nothing --
    -- which is what keeps this table from growing on every pass.
    input_digest TEXT NOT NULL,

    generated_at TEXT NOT NULL,

    CHECK (update_type IN ('none', 'digest', 'patch', 'minor', 'major',
                           'prerelease', 'unknown')),
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

-- The duplicate-suppression index.
--
-- UNIQUE over (container, fingerprint): a second pass over an unchanged world
-- cannot insert a second row for the same container, whatever races with it.
-- The application checks first to avoid the write; this constraint is what makes
-- the guarantee hold under concurrency rather than merely usually.
CREATE UNIQUE INDEX idx_plan_fingerprint ON change_plans (container_id, input_digest);

-- The newest plan per container, which is what every read path wants. Leading
-- with container_id and descending by id makes "the current plan" an index seek
-- rather than a group-and-sort.
CREATE INDEX idx_plan_container ON change_plans (container_id, id DESC);

-- The list endpoint's filters and default ordering.
CREATE INDEX idx_plan_generated      ON change_plans (generated_at DESC, id DESC);
CREATE INDEX idx_plan_band           ON change_plans (risk_band, id DESC);
CREATE INDEX idx_plan_recommendation ON change_plans (recommendation, id DESC);
CREATE INDEX idx_plan_update         ON change_plans (update_type, id DESC);
CREATE INDEX idx_plan_snapshot       ON change_plans (snapshot_id);
