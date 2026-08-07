-- 0019_automation_decision_settled: a terminal marker for a followed decision.
--
-- # What was wrong
--
-- The follower asks "which decisions did automation start that have not
-- finished", and 0018 answered it with "named an acquisition and has no
-- rollback". That is correct for a decision that ends in a rollback and wrong
-- for every other ending.
--
-- A SUCCESSFUL update never gets a rollback id, so it matched the query
-- forever: the follower re-read the same finished execution on every tick,
-- re-recorded the success, and re-logged it. Nothing was done twice to the host
-- -- the recreation service refuses a spent acquisition, and the follower only
-- reads -- but the log filled with the same line every five seconds and the
-- backlog query never drained. Found by driving the feature against a live
-- Docker host.
--
-- # The fix
--
-- "Finished" is its own fact and is recorded as one. The follower writes
-- settled_at when a decision reaches ANY terminal outcome: the update
-- succeeded, the pull failed, the recreation preflight refused, or a rollback
-- was submitted. The query then means exactly what it says.
--
-- Nullable and written once. A decision that is still moving carries NULL,
-- which is also what every row written before this migration carries -- so an
-- upgrade re-examines outstanding work rather than silently abandoning it, and
-- settles it on the next tick.

ALTER TABLE automation_decisions ADD COLUMN settled_at TEXT;

-- Replaces the 0018 index. The predicate is now the whole question the follower
-- asks, so the index covers it exactly: a settled decision leaves the index and
-- never returns.
DROP INDEX IF EXISTS idx_automation_decision_pending;

CREATE INDEX idx_automation_decision_pending
    ON automation_decisions (verdict, id DESC)
    WHERE acquisition_id <> '' AND settled_at IS NULL;
