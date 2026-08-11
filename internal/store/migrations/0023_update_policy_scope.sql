-- 0023_update_policy_scope: what an update policy is POINTED AT.
--
-- # What this adds
--
-- One column on update_policies. Its two values are the closed vocabulary in
-- internal/domain/update_scope.go:
--
--   'selector'    -- the policy governs the containers its selector names.
--   'allEligible' -- the policy governs every container HarborMaster may
--                    legitimately consider, minus its own exclusions.
--
-- # Why the breadth of a policy needed its own column
--
-- Before this, "keep everything updated" could only be written by putting
-- something into a selector field, and every way of doing that was wrong: the
-- pattern `*` is refused by validation, an empty selector means the OPPOSITE
-- (it governs nothing), and a magic name in `include` collides with real
-- container names. All three would have made the breadth of a rule a property
-- of a string, discovered by parsing, rather than a property an operator chose.
--
-- A column also makes the estate-wide question answerable in SQL --
-- "which rules can reach the whole host" is one indexed read, not a scan that
-- decodes and interprets every selector document.
--
-- # This migration cannot broaden an existing policy
--
-- DEFAULT 'selector' applies to every existing row, and 'selector' evaluates
-- through exactly the code path those rows already used: UpdateSelector.Matches
-- against the clauses they already carry. An existing narrow rule stays narrow,
-- an existing empty one keeps governing nothing, and an archived one is not
-- read at all. There is no value of any other column that can make a row come
-- out of this migration as 'allEligible'.
--
-- The domain applies the same default a second time, in Normalise, so a policy
-- decoded from a row written by any build lands on the narrow reading rather
-- than on the zero value of the type -- which is not a scope, and which
-- UpdatePolicy.Governs refuses.
--
-- # What it does NOT grant
--
-- No new capability. A policy in either scope reaches the host only by being
-- selected in a decision pass, and every container it selects still passes the
-- pause, the container's opt-out labels, the strategy ceiling, the planner's
-- recommendation, the maintenance window, the mode, the run budgets, the
-- acquisition preflight, the digest verification, the recreation preflight, the
-- preservation comparison, and the health proof. Broad SELECTION is not broad
-- AUTHORISATION.

ALTER TABLE update_policies
    ADD COLUMN scope TEXT NOT NULL DEFAULT 'selector'
    CHECK (scope IN ('selector', 'allEligible'));

-- Serves two questions that are worth answering without decoding a selector:
-- the scheduler's "load every active policy" (already covered by
-- idx_update_policy_active, which this does not replace), and an operator's
-- "which rules in force can reach the whole estate". The second is the one that
-- matters at review time, and it is the reason the column is indexed at all.
CREATE INDEX idx_update_policy_scope
    ON update_policies (scope, archived, enabled, priority DESC);
