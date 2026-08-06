-- Attribution for the two operations that change the Docker host.
--
-- # What was missing
--
-- Phase 9.5 attributed every state-changing REQUEST: an audit row records that
-- an account asked for an image to be downloaded or a container to be replaced.
-- What it did not record is the OUTCOME. A recreation that stopped a container
-- and replaced it produced no security-audit row at all, so the log could not
-- answer the question an administrator actually asks after an incident:
--
--     who caused this container to be replaced, and did it work?
--
-- The request row alone cannot answer it. A request can be refused by the
-- second preflight, cancelled before the first mutation, expire in the queue,
-- or fail partway and leave two containers behind. "Requested" and "happened"
-- are different facts.
--
-- # Why the actor is stored on the record rather than joined
--
-- The outcome is recorded by a WORKER, minutes after the request, on a
-- goroutine that has no HTTP request and no session. For it to attribute the
-- outcome it must read the actor from somewhere, and the record of the work is
-- the only thing it has.
--
-- Storing the actor also makes the acquisition and execution histories
-- self-describing: an operator reading the recreations page sees who asked for
-- each one without a join against an audit table that may since have been
-- pruned.
--
-- # What is NOT stored
--
-- No session token, no role, no address. The user id and the username at the
-- time of the request, and nothing else.
--
-- The USERNAME is denormalised deliberately. An account can be renamed only by
-- being recreated -- there is no rename endpoint, for exactly this reason --
-- but an account can be deleted from a future version, and a history that read
-- "requested by (unknown)" after the fact would be a history that lost the
-- answer. The id is stored beside it so a live account can still be linked.
--
-- Both columns default to the empty string, so every row that predates this
-- migration reads as "requested before HarborMaster recorded requesters"
-- rather than being attributed to nobody in particular. The API renders that
-- as absent rather than inventing an actor.

ALTER TABLE acquisitions ADD COLUMN requested_by_user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE acquisitions ADD COLUMN requested_by_username TEXT NOT NULL DEFAULT '';

ALTER TABLE executions ADD COLUMN requested_by_user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE executions ADD COLUMN requested_by_username TEXT NOT NULL DEFAULT '';

-- Answering "what has this account caused on this host" without scanning.
--
-- Partial, so the index covers only rows that carry an actor: every row from
-- before this migration has the empty string, and indexing thousands of them
-- under one key would make the index worse than useless.
CREATE INDEX idx_acquisition_requester
    ON acquisitions (requested_by_user_id, id DESC)
    WHERE requested_by_user_id <> '';

CREATE INDEX idx_execution_requester
    ON executions (requested_by_user_id, id DESC)
    WHERE requested_by_user_id <> '';
