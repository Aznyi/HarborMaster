-- 0031_plan_approvals: record that a person reviewed one immutable change plan.
--
-- # What this stores, and what it deliberately does not
--
-- Phase 17.7 closes a gap the earlier stages exposed: a plan whose
-- recommendation is `manualReview` could be acquired and never executed, and
-- nothing in HarborMaster could record that the required review had happened.
-- The refusal said so in as many words -- "the review has not happened" -- and
-- offered no way for it to.
--
-- An approval is a SECOND FACT placed next to the planner's, never a change to
-- it. Nothing in this migration touches change_plans. The plan keeps its risk
-- score, its factors and its recommendation exactly as the planner wrote them,
-- forever. Two sentences stay separately answerable:
--
--     this exact proposed update requires human review
--     an authorised human reviewed this exact update and approved it
--
-- Collapsing them -- by lowering a score, resolving a factor, or rewriting a
-- recommendation -- would mean the assessment an operator reviewed was not the
-- assessment that authorised the change.
--
-- # Why plan_id is the binding
--
-- change_plans is insert-only and deduplicated on (container_id, input_digest),
-- so a plan_id names ONE immutable assessment permanently. Everything else the
-- approval needs -- the container, the image, the digests, the recommendation --
-- is derivable from it, so none of it is duplicated here as authority.
--
-- Binding to a container instead would let a judgement about one proposed change
-- authorise a different one later, which is the whole failure this table exists
-- to prevent.
--
-- # Why the two digest columns exist anyway
--
-- They are COMPARISON TRIPWIRES, never authority, and never supplied by a
-- caller. They are copied from the plan when the approval is written and
-- compared against the plan again at execution time.
--
-- "A plan row is never updated" is currently upheld by the absence of an update
-- method rather than by a constraint. Digest substitution is the one attack this
-- feature would otherwise enable, so the cheap runtime check earns its two
-- columns. Nothing SELECTs them as a source of truth.
--
-- # Why ON DELETE CASCADE
--
-- An approval is live authorisation state, not history. Retention prunes plans;
-- an approval whose plan is gone authorises nothing and must not survive as a
-- row that looks like it does. The permanent record of who approved what lives
-- in the audit log, which has its own, longer retention.
--
-- This is the opposite choice from `executions`, which deliberately avoids
-- foreign keys because it IS the permanent record. Both are right for what they
-- hold.
--
-- # States
--
-- Two, and only two. `superseded` is derivable -- a plan that is no longer the
-- container's current plan is superseded whether or not a row says so -- and
-- `consumed` is derivable from whether an execution of the plan has set
-- mutated_at. Storing either would duplicate a fact that changes without this
-- table being told.

CREATE TABLE plan_approvals (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    -- The authority. UNIQUE on the parent, so SQLite accepts it as a foreign
    -- key target.
    plan_id TEXT NOT NULL
        REFERENCES change_plans (plan_id) ON DELETE CASCADE,

    state TEXT NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'revoked')),

    -- Comparison tripwires. See the header: never authority.
    approved_input_digest    TEXT NOT NULL DEFAULT '',
    approved_proposed_digest TEXT NOT NULL DEFAULT '',

    -- WHO. An approval with nobody attached is unrepresentable: the question
    -- this table exists to answer is "who said yes to this".
    approved_by_user_id  TEXT NOT NULL DEFAULT '',
    approved_by_username TEXT NOT NULL,
    approved_at          TEXT NOT NULL,

    revoked_by_user_id  TEXT NOT NULL DEFAULT '',
    revoked_by_username TEXT NOT NULL DEFAULT '',
    revoked_at          TEXT,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    CHECK (approved_by_username <> ''),
    -- Revoked by nobody, or at no time, is unrepresentable.
    CHECK (revoked_at IS NULL OR revoked_by_username <> ''),
    CHECK ((state = 'revoked') = (revoked_at IS NOT NULL))
);

-- ONE effective approval per plan.
--
-- A partial unique index rather than a process lock, the same idiom as
-- idx_automation_pause_active and idx_execution_active_container: two
-- concurrent approvals settle in the database, and exactly one becomes
-- authority. A revoked row frees the slot, so a plan can be approved again
-- after a withdrawal and the earlier decision stays in the history.
CREATE UNIQUE INDEX idx_plan_approval_active
    ON plan_approvals (plan_id)
    WHERE state = 'active';

CREATE INDEX idx_plan_approval_plan ON plan_approvals (plan_id, id DESC);

-- ------------------------------------------------------------------------
-- The executions refusal vocabulary.
--
-- `approvalMissing` is added rather than reusing `recommendation`. The two say
-- different things: `recommendation` means the verdict can never be acted on
-- (`notRecommended`, `unknown`), and this means the plan IS approvable and has
-- not been approved. One has a remedy the operator can carry out; the other does
-- not, and pointing them at the wrong one is the mistake 0030's header records.
--
-- SQLite cannot alter a CHECK in place. Same procedure as 0014, 0017, 0021,
-- 0026, 0027, 0028, 0029 and 0030: copy, drop, rename, recreate the indexes.
--
-- The definition below was taken mechanically from 0030 -- the LIVE definition
-- of this table -- rather than retyped, with one value added to the refusal
-- CHECK. Nothing between 0030 and here altered `executions`.
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
                           'dependentsNotRebindable',
                           -- Added by 0030.
                           'snapshotChanged',
                           -- Added by 0031. See the header.
                           'approvalMissing')),
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
