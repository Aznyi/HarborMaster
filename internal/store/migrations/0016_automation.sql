-- 0016_automation: update policies and the automation engine's records.
--
-- # What this migration does and does not grant
--
-- This is the first migration whose rows can cause the host to change without
-- a person asking. It grants no new capability to do so. The automation engine
-- holds no Docker interface: it submits an AcquisitionRequest, an
-- ExecutionRequest, and a RollbackRequest to the same three services a human
-- operator's HTTP request reaches, and each of those re-runs its own preflight
-- against the live host before acting. Everything below is bookkeeping for
-- decisions made by code that already existed.
--
-- Deliberately separate from policy_definitions (0006). A compliance policy
-- REPORTS; an update policy ACTS. Sharing a table would mean one UPDATE could
-- turn a reporting rule into a mutation rule.
--
-- Timestamps are RFC3339Nano UTC text throughout, matching 0001 through 0015.

-- --------------------------------------------------------- update policies --

-- One row per administrator-defined automation rule.
--
-- # Why the selector, limits, and failure handling are JSON
--
-- Same reasoning as policy_definitions.rules_json: a policy is only ever read
-- as a whole policy. The scheduler loads every enabled row once per pass and
-- applies each in full. No query filters, sorts, or joins on an individual
-- selector clause. Each document is produced and consumed exclusively by a
-- strongly typed struct in internal/domain and is validated before it is
-- written; the LENGTH checks are growth backstops, not the validation.
--
-- # Why there is no DELETE
--
-- automation_decisions and automation_pauses reference this table, and the
-- record of what automation did must survive the policy being withdrawn. An
-- auditor asking "what changed our estate in March" must not get a different
-- answer because someone tidied up in April. DELETE /update-policies ARCHIVES.
-- The foreign keys below are ON DELETE RESTRICT so a hand-written DELETE
-- cannot orphan that history either.
CREATE TABLE update_policies (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    -- The IMMUTABLE public identifier. Generated server-side from the system
    -- entropy source, never accepted from a caller, and never changed by an
    -- update: decisions and pauses reference it.
    policy_id TEXT NOT NULL UNIQUE,

    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',

    enabled  INTEGER NOT NULL DEFAULT 1,
    archived INTEGER NOT NULL DEFAULT 0,
    archived_at TEXT,

    -- Higher wins when several policies match one container. Bounded so a
    -- caller cannot make ordering depend on integer overflow.
    priority INTEGER NOT NULL DEFAULT 0,

    -- The ceiling on how far an update may go, and how far the engine may act.
    -- Both CHECKed against the closed vocabularies in internal/domain: a typo
    -- that reached this column would otherwise be a policy that silently
    -- permitted more, or less, than an operator read on the screen.
    strategy TEXT NOT NULL,
    mode     TEXT NOT NULL,

    -- The planner verdict a change must carry. Constrained to the two that can
    -- be automated at all: 'unknown' and 'manualReview' mean a person has to
    -- look, and 'notRecommended' means the planner argued against the change.
    minimum_recommendation TEXT NOT NULL DEFAULT 'proceed',

    -- domain.UpdateSelector, domain.MaintenanceWindow, domain.UpdateLimits,
    -- and domain.UpdateFailureHandling as canonical JSON.
    selector_json TEXT NOT NULL DEFAULT '{}',
    window_json   TEXT NOT NULL DEFAULT '{}',
    limits_json   TEXT NOT NULL DEFAULT '{}',
    failure_json  TEXT NOT NULL DEFAULT '{}',

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    CHECK (name <> ''),
    CHECK (enabled IN (0, 1)),
    CHECK (archived IN (0, 1)),
    CHECK (priority >= 0 AND priority <= 1000),
    CHECK (strategy IN ('digestOnly', 'patch', 'minor', 'major')),
    CHECK (mode IN ('observe', 'dryRun', 'approvalRequired', 'automatic')),
    CHECK (minimum_recommendation IN ('proceed', 'proceedWithCaution')),
    CHECK (LENGTH(selector_json) <= 32768),
    CHECK (LENGTH(window_json)   <= 4096),
    CHECK (LENGTH(limits_json)   <= 4096),
    CHECK (LENGTH(failure_json)  <= 4096),
    CHECK (LENGTH(description)   <= 4096)
);

-- Serves the scheduler's "load every enabled policy" query, which runs once per
-- pass, in the order the selection rule needs: priority descending, then id.
CREATE INDEX idx_update_policy_active ON update_policies (archived, enabled, priority DESC, policy_id);
CREATE INDEX idx_update_policy_name   ON update_policies (name, id);

-- ---------------------------------------------------------- scheduler runs --

-- One row per scheduler pass, including the passes that did nothing.
--
-- A pass that decided to change nothing is the answer to "why did automation
-- not update that container last night", and it is unavailable unless the pass
-- itself was recorded. Every pass writes a row before it examines anything.
CREATE TABLE automation_runs (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT    NOT NULL UNIQUE,

    host_id TEXT NOT NULL DEFAULT 'local',

    trigger_source TEXT NOT NULL,
    state          TEXT NOT NULL DEFAULT 'running',

    -- A pass that could never have acted, whatever the policies said. Recorded
    -- on the run so history is readable without joining every decision back to
    -- the policy mode at the time it ran.
    dry_run INTEGER NOT NULL DEFAULT 0,

    considered INTEGER NOT NULL DEFAULT 0,
    eligible   INTEGER NOT NULL DEFAULT 0,
    submitted  INTEGER NOT NULL DEFAULT 0,
    skipped    INTEGER NOT NULL DEFAULT 0,
    failed     INTEGER NOT NULL DEFAULT 0,

    -- Who asked for a manual pass, as domain.Requester's two fields and no
    -- more. Empty for a scheduled pass, which nobody asked for. Never a
    -- password, a token, a role, a session, or a request body.
    requested_by_user_id  TEXT NOT NULL DEFAULT '',
    requested_by_username TEXT NOT NULL DEFAULT '',

    -- HarborMaster's own sentence. Never a daemon or registry string.
    message TEXT NOT NULL DEFAULT '',

    started_at   TEXT    NOT NULL,
    completed_at TEXT,
    duration_ms  INTEGER NOT NULL DEFAULT 0,

    CHECK (trigger_source IN ('schedule', 'manual', 'dryRun', 'startup')),
    CHECK (state IN ('running', 'completed', 'failed', 'interrupted')),
    CHECK (dry_run IN (0, 1)),
    CHECK (considered >= 0 AND eligible >= 0 AND submitted >= 0
           AND skipped >= 0 AND failed >= 0),
    CHECK (duration_ms >= 0),
    CHECK (LENGTH(message) <= 1024)
);

CREATE INDEX idx_automation_run_time  ON automation_runs (started_at DESC, id DESC);
CREATE INDEX idx_automation_run_state ON automation_runs (state, started_at DESC);
-- Serves the recovery sweep that marks an interrupted pass on startup, and the
-- retention pass's cutoff.
CREATE INDEX idx_automation_run_open  ON automation_runs (state, id);

-- ----------------------------------------------------- per-container decisions --

-- One row per container examined in one pass.
--
-- Every identity column here is copied from a record HarborMaster wrote itself.
-- There is no column on this table whose value a caller supplies: the container
-- id and name come from the inventory, the image and digest from the plan, and
-- the acquisition, execution, and rollback ids from the services that created
-- them.
CREATE TABLE automation_decisions (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT    NOT NULL REFERENCES automation_runs (run_id) ON DELETE CASCADE,

    host_id TEXT NOT NULL DEFAULT 'local',

    container_id   TEXT NOT NULL DEFAULT '',
    container_name TEXT NOT NULL DEFAULT '',

    -- ON DELETE RESTRICT: a decision outlives its policy's withdrawal.
    policy_id   TEXT REFERENCES update_policies (policy_id) ON DELETE RESTRICT,
    policy_name TEXT NOT NULL DEFAULT '',

    -- From the closed vocabularies in internal/domain. Not CHECKed against a
    -- literal list of reasons: the reason vocabulary grows as the engine learns
    -- to decline for new evidence, and a schema-level list would turn adding a
    -- reason into a migration. The verdict IS checked, because it is what a UI
    -- branches on and what an operator filters by.
    verdict TEXT NOT NULL,
    reason  TEXT NOT NULL,
    detail  TEXT NOT NULL DEFAULT '',

    plan_id         TEXT NOT NULL DEFAULT '',
    current_image   TEXT NOT NULL DEFAULT '',
    proposed_image  TEXT NOT NULL DEFAULT '',
    proposed_digest TEXT NOT NULL DEFAULT '',
    update_type     TEXT NOT NULL DEFAULT '',
    recommendation  TEXT NOT NULL DEFAULT '',

    -- The records the engine created when it acted. Empty in observe and dry
    -- run, which is the whole difference between those modes and automatic.
    acquisition_id TEXT NOT NULL DEFAULT '',
    execution_id   TEXT NOT NULL DEFAULT '',
    rollback_id    TEXT NOT NULL DEFAULT '',

    -- This decision's place in the pass's execution order, so a dry run renders
    -- "what would happen, in what sequence" rather than an unordered set.
    position INTEGER NOT NULL DEFAULT 0,

    decided_at TEXT NOT NULL,

    CHECK (verdict IN ('update', 'wouldUpdate', 'awaitingApproval', 'skip')),
    CHECK (position >= 0),
    CHECK (LENGTH(reason) <= 64),
    CHECK (LENGTH(detail) <= 1024),
    -- A decision that names no acquisition cannot name an execution, and one
    -- that names no execution cannot name a rollback. The ordering of the
    -- pipeline, made unrepresentable in reverse rather than merely conventional.
    CHECK (acquisition_id <> '' OR execution_id = ''),
    CHECK (execution_id   <> '' OR rollback_id  = ''),
    -- Observe and dry run cannot have acted. Enforced here as well as in the
    -- engine, so a future code path that forgot the mode check would be
    -- refused by the database rather than silently recorded.
    CHECK (verdict <> 'wouldUpdate' OR acquisition_id = '')
);

CREATE INDEX idx_automation_decision_run       ON automation_decisions (run_id, position, id);
CREATE INDEX idx_automation_decision_container ON automation_decisions (container_name, decided_at DESC, id DESC);
CREATE INDEX idx_automation_decision_verdict   ON automation_decisions (verdict, decided_at DESC, id DESC);
CREATE INDEX idx_automation_decision_policy    ON automation_decisions (policy_id, decided_at DESC, id DESC);
-- Serves the "decisions waiting for a person" count on the dashboard.
CREATE INDEX idx_automation_decision_approval  ON automation_decisions (verdict, id DESC);

-- --------------------------------------------------------------- pausing --

-- One row per container automation will not touch.
--
-- # Keyed on the NAME, not the id
--
-- A container's id changes every time it is recreated, and recreation is
-- exactly what automation does. A pause keyed on the id would be cleared by the
-- very action that went wrong. The id is kept for the audit trail only.
--
-- # Why a pause defaults to never expiring
--
-- resume_after NULL means only an acknowledgement clears it. Repeated failure
-- is evidence that something is wrong which a timer will not fix, and an
-- automation system that resumes by itself after a bad image is how one bad
-- image becomes an outage loop. A cooldown is available and is opt-in.
CREATE TABLE automation_pauses (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    container_name TEXT NOT NULL,
    container_id   TEXT NOT NULL DEFAULT '',

    reason TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',

    failures INTEGER NOT NULL DEFAULT 0,

    policy_id    TEXT REFERENCES update_policies (policy_id) ON DELETE RESTRICT,
    rollback_id  TEXT NOT NULL DEFAULT '',
    execution_id TEXT NOT NULL DEFAULT '',

    paused_at    TEXT NOT NULL,
    resume_after TEXT,

    acknowledged_at       TEXT,
    acknowledged_user_id  TEXT NOT NULL DEFAULT '',
    acknowledged_username TEXT NOT NULL DEFAULT '',

    CHECK (container_name <> ''),
    CHECK (reason IN ('repeatedFailure', 'automaticRollback', 'operator')),
    CHECK (failures >= 0),
    CHECK (LENGTH(detail) <= 1024),
    -- An acknowledgement records who made it. A row that claims to be
    -- acknowledged by nobody is unrepresentable.
    CHECK (acknowledged_at IS NULL OR acknowledged_username <> '')
);

-- At most one ACTIVE pause per container. A partial unique index rather than a
-- plain one: the history of previous pauses is kept, and only the unresolved
-- one is unique, so re-pausing a container that was acknowledged last month is
-- a new row rather than an update that erased the earlier fact.
CREATE UNIQUE INDEX idx_automation_pause_active
    ON automation_pauses (container_name)
    WHERE acknowledged_at IS NULL;

CREATE INDEX idx_automation_pause_time     ON automation_pauses (paused_at DESC, id DESC);
CREATE INDEX idx_automation_pause_resume   ON automation_pauses (acknowledged_at, resume_after);
CREATE INDEX idx_automation_pause_policy   ON automation_pauses (policy_id, id DESC);

-- ---------------------------------------------- per-container failure counts --

-- The rolling failure count a pause decision is made from.
--
-- Separate from the decisions table because the question "how many times has
-- this container failed in the last N hours" must be answerable in one indexed
-- read on every pass, for every container, without scanning a growing history.
-- One row per container, updated in place.
CREATE TABLE automation_failures (
    container_name TEXT PRIMARY KEY,
    host_id        TEXT NOT NULL DEFAULT 'local',

    consecutive INTEGER NOT NULL DEFAULT 0,
    -- The count within the policy's pause window. Reset when the window has
    -- elapsed, which is why window_started_at is recorded alongside it.
    windowed          INTEGER NOT NULL DEFAULT 0,
    window_started_at TEXT,

    last_failure_at TEXT,
    last_success_at TEXT,
    -- HarborMaster's own sentence about the most recent failure.
    last_detail TEXT NOT NULL DEFAULT '',

    CHECK (consecutive >= 0),
    CHECK (windowed >= 0),
    CHECK (LENGTH(last_detail) <= 1024)
);

CREATE INDEX idx_automation_failure_recent ON automation_failures (last_failure_at DESC);
