-- 0018_automation_pending_index: the index the follower's one query needs.
--
-- # Why this is its own migration
--
-- 0016 shipped with the follower walking the last few scheduler PASSES to find
-- work it had started. That was wrong in two ways, and both were found by
-- driving the feature against a live Docker host rather than by reading it:
--
--   - An APPROVED decision is promoted after its pass has already finished, so
--     that run's `submitted` counter is zero and stays zero. A follower
--     filtering on "this pass submitted something" never saw it.
--   - A decision that takes longer to resolve than a few scheduler ticks falls
--     out of a window over runs entirely.
--
-- Both left an update with an acquired image and no recreation, and the
-- pipeline stopped without saying so.
--
-- The follower now asks the DECISIONS directly: which ones named an acquisition
-- and have not reached a rollback. That is a property of the work rather than
-- of which passes happen to be recent, and it is the query this index serves.
--
-- 0016 is left untouched. An applied migration is never edited -- a database
-- that already carries it must reach the same schema by moving forward.

-- Partial, so the index covers only the rows that could ever qualify: the
-- overwhelming majority of decisions are skips, which never enter it, and a
-- resolved update leaves it as soon as its rollback id is written.
CREATE INDEX idx_automation_decision_pending
    ON automation_decisions (verdict, id DESC)
    WHERE acquisition_id <> '' AND rollback_id = '';
