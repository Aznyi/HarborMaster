-- 0006_policy: the policy engine.
--
-- HarborMaster remains read-only with respect to Docker. A policy is a rule
-- HarborMaster checks a container's configuration AGAINST; it is never applied,
-- enforced, or pushed to the daemon. Nothing in this migration grants any
-- ability to create, change, remove, or restore a container, and there is no
-- remediation column because there is no remediation.
--
-- Timestamps are RFC3339Nano UTC text throughout, matching 0001 through 0005.

-- ----------------------------------------------------- policy definitions --

-- One row per administrator-defined policy.
--
-- # Why the rules are one JSON column rather than a fourth table
--
-- A policy's rules are only ever read as a whole policy: the engine loads every
-- active definition once per sweep and applies each one in full. No query
-- filters, sorts, or joins on an individual rule. A relational rules table
-- would therefore buy nothing at read time and cost a join and a second write
-- path per edit. The rules are still STRONGLY TYPED -- see internal/domain --
-- and the JSON is produced and consumed only by that type, never interpreted.
--
-- rule_count is denormalised so the list endpoint can show a policy's size
-- without decoding every document.
--
-- # Why there is no DELETE
--
-- policy_violations references this table, and the violation history must
-- survive a policy being withdrawn: an auditor asking "what were we failing in
-- March" must not be answered differently because someone tidied up in April.
-- DELETE /policies therefore ARCHIVES -- it sets archived = 1, which removes
-- the policy from evaluation and lets the next pass resolve its open
-- violations. The foreign key below is declared ON DELETE RESTRICT so that a
-- hand-written DELETE against the database cannot orphan that history either.
CREATE TABLE policy_definitions (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    -- The IMMUTABLE public identifier. Generated server-side from the system
    -- entropy source, never accepted from a caller, and never changed by an
    -- update: a violation references it, so a mutable id would orphan history.
    -- UNIQUE both because it identifies the policy and because the foreign key
    -- from policy_violations requires it.
    policy_id TEXT NOT NULL UNIQUE,

    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    severity    TEXT NOT NULL,

    enabled     INTEGER NOT NULL DEFAULT 1,
    archived    INTEGER NOT NULL DEFAULT 0,
    archived_at TEXT,

    -- The rule list, as the canonical JSON encoding of []domain.PolicyRule.
    -- Validated against the closed rule catalogue before it is written; the
    -- length bound is a backstop against a row that grows without one.
    rules_json TEXT    NOT NULL,
    rule_count INTEGER NOT NULL DEFAULT 0,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    CHECK (severity IN ('critical', 'high', 'medium', 'low')),
    CHECK (enabled IN (0, 1)),
    CHECK (archived IN (0, 1)),
    CHECK (name <> ''),
    CHECK (rule_count >= 0),
    CHECK (LENGTH(rules_json) <= 65536)
);

-- Serves the engine's "load every active policy" query, which is the one that
-- runs once per sweep, and the list endpoint's default ordering.
CREATE INDEX idx_policy_active ON policy_definitions (archived, enabled, id);
CREATE INDEX idx_policy_name   ON policy_definitions (name, id);

-- ------------------------------------------------------ policy violations --

-- One row per RULE that a container fails.
--
-- # Identity
--
-- (container_id, policy_id, rule_type) identifies a violation. Deliberately not
-- the offending values: a container whose forbidden label keeps changing is one
-- long-lived record rather than a new record per evaluation, which is what
-- makes an operator's acknowledgement survive the next pass and what bounds
-- table growth under a refresh storm.
--
-- The rule TYPE rather than a rule index, because an index moves when an
-- administrator reorders a policy's rules, and history must not be re-keyed by
-- an edit that changed nothing about the world. A policy therefore carries at
-- most one rule of each type, which validation enforces.
--
-- # Secrets
--
-- observed and expected are rendered by the rule evaluator from
-- domain.PolicyTarget, a structure that carries environment variable NAMES and
-- has no field capable of holding a value. A secret cannot reach these columns
-- because there is no path by which one could be read in the first place. The
-- length bounds below are growth bounds, not the secret control.
CREATE TABLE policy_violations (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    -- ON DELETE RESTRICT: violations outlive a policy's withdrawal, so the
    -- database refuses a delete that would orphan them. Withdrawal is an
    -- archive, not a delete.
    policy_id TEXT NOT NULL REFERENCES policy_definitions (policy_id) ON DELETE RESTRICT,
    -- Denormalised so a list response needs no join and stays readable after a
    -- policy is archived or renamed.
    policy_name TEXT NOT NULL DEFAULT '',

    container_id   TEXT NOT NULL,
    container_name TEXT NOT NULL DEFAULT '',

    -- From the closed rule catalogue in internal/domain. The CHECK is the
    -- schema-level backstop for a vocabulary the API already validates.
    rule_type TEXT NOT NULL,
    severity  TEXT NOT NULL,

    -- first seen, most recently seen, and when the container became compliant.
    detected_at  TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    resolved_at  TEXT,
    inventory_generation INTEGER NOT NULL DEFAULT 0,

    -- What the container has, what the rule wanted, and why that fails.
    -- Engine-rendered prose and identifiers; never caller free text, never an
    -- error string, never a secret.
    observed TEXT NOT NULL DEFAULT '',
    expected TEXT NOT NULL DEFAULT '',
    reason   TEXT NOT NULL DEFAULT '',

    status TEXT NOT NULL DEFAULT 'active',
    -- Operator annotation from a status change. Length-bounded, UTF-8
    -- validated, and control-character free before it reaches this column.
    note              TEXT NOT NULL DEFAULT '',
    status_changed_at TEXT,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    CHECK (severity IN ('critical', 'high', 'medium', 'low')),
    CHECK (status IN ('active', 'resolved', 'acknowledged', 'exempted')),
    CHECK (rule_type IN ('privilegedForbidden', 'readOnlyRootFilesystemRequired',
                         'imageAllowlist', 'imageDenylist',
                         'requiredLabels', 'forbiddenLabels',
                         'requiredEnv', 'forbiddenEnv',
                         'requiredCapabilities', 'forbiddenCapabilities',
                         'memoryLimitRequired', 'cpuLimitRequired',
                         'restartPolicyAllowlist', 'networkAllowlist',
                         'userNotRoot', 'healthCheckRequired')),
    CHECK (LENGTH(observed) <= 1024),
    CHECK (LENGTH(expected) <= 1024),
    CHECK (LENGTH(reason) <= 1024)
);

-- The identity index. UNIQUE, so re-evaluating a container cannot create a
-- second row for the same rule: the engine upserts against this key, which is
-- what deduplicates a repeated failure and bounds table growth.
CREATE UNIQUE INDEX idx_policy_violation_identity
    ON policy_violations (container_id, policy_id, rule_type);

-- The list endpoint's default ordering and its filters. Each index leads with
-- the filtered column and ends in id DESC, so a filtered page is served by one
-- index rather than a filter followed by a sort.
CREATE INDEX idx_policy_violation_status    ON policy_violations (status, detected_at DESC, id DESC);
CREATE INDEX idx_policy_violation_severity  ON policy_violations (severity, detected_at DESC, id DESC);
CREATE INDEX idx_policy_violation_container ON policy_violations (container_id, status, id DESC);
CREATE INDEX idx_policy_violation_policy    ON policy_violations (policy_id, status, id DESC);
CREATE INDEX idx_policy_violation_rule      ON policy_violations (rule_type, status, id DESC);
-- Serves the summary's "distinct containers with an open violation" without a
-- scan, and the retention pass's cutoff.
CREATE INDEX idx_policy_violation_open      ON policy_violations (status, container_id);
CREATE INDEX idx_policy_violation_resolved  ON policy_violations (status, resolved_at);

-- ----------------------------------------------------- policy evaluations --

-- One row per container, holding the most recent compliance pass.
--
-- Separate from the violations because a pass that found NOTHING is still a
-- fact worth storing. Without this table, "no violation rows for this
-- container" is ambiguous between "checked, and it complies with every policy"
-- and "never checked" -- and telling an operator their estate is compliant when
-- most of it was never examined is the worst thing a compliance dashboard can
-- do.
--
-- One row per container rather than a history: the current state is what the
-- dashboard reports, and an unbounded audit trail of passes would grow with
-- every refresh. The violations themselves carry the history that matters,
-- through detected_at and resolved_at.
CREATE TABLE policy_evaluations (
    container_id   TEXT PRIMARY KEY,
    host_id        TEXT NOT NULL DEFAULT 'local',
    container_name TEXT NOT NULL DEFAULT '',

    evaluated_at         TEXT    NOT NULL,
    inventory_generation INTEGER NOT NULL DEFAULT 0,

    policies_evaluated INTEGER NOT NULL DEFAULT 0,
    rules_evaluated    INTEGER NOT NULL DEFAULT 0,
    violation_count    INTEGER NOT NULL DEFAULT 0,

    -- compliant = 1 only when the pass was COMPLETE and found nothing. An
    -- incomplete pass is never recorded as compliant: it did not establish
    -- compliance, it stopped looking. The CHECK makes that unrepresentable
    -- rather than merely conventional.
    compliant INTEGER NOT NULL DEFAULT 0,
    complete  INTEGER NOT NULL DEFAULT 1,
    reason    TEXT    NOT NULL DEFAULT '',

    CHECK (compliant IN (0, 1)),
    CHECK (complete IN (0, 1)),
    CHECK (compliant = 0 OR (complete = 1 AND violation_count = 0))
);

CREATE INDEX idx_policy_evaluations_time      ON policy_evaluations (evaluated_at DESC);
CREATE INDEX idx_policy_evaluations_compliant ON policy_evaluations (compliant, complete);
