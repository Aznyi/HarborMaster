-- 0029_acquisition_refusal_vocabulary: record the self-update refusal.
--
-- # The seventh occurrence of one defect
--
-- `domain.AcquisitionRefusalSelfUpdate` exists in the Go vocabulary and was
-- REJECTED by this column. It is the refusal HarborMaster raises when a pull is
-- requested for the container HarborMaster is itself running in.
--
-- The pull was still refused -- that decision is made in Go, in the acquisition
-- preflight, which is the second of the four independent layers CLAUDE.md
-- invariant 12 describes. But writing the record failed, so the acquisition was
-- stored with no reason attached, and self-update protection is precisely the
-- refusal that looks from outside like HarborMaster declining for no reason.
--
-- 0028 fixed the identical drift on `executions.refusal`, where the same value
-- was missing alongside two others. The acquisition column was not audited at
-- the time, so it kept the defect for one more release.
--
-- Seven occurrences now: 0014, 0017, 0021 and 0026 for audit target types; 0027
-- for plan update types, found live against a real daemon; 0028 for execution
-- refusals; this one. Found by TestEveryAcquisitionVocabularyIsAcceptedByThe
-- Schema on its first run -- a test written specifically to walk the vocabularies
-- that did not yet have one. After it, every closed vocabulary in this schema is
-- walked by a test against a real database.
--
-- # Why a table rebuild
--
-- SQLite cannot alter a CHECK in place: copy, drop, rename, recreate the
-- indexes.
--
-- The definition below is the LIVE schema, read back from a migrated database
-- rather than transcribed from 0009 by hand. Two columns were added by a later
-- ALTER and one index does not appear in 0009 at all, so a hand copy would have
-- silently dropped all three.
--
-- Every row keeps its id. An acquisition id is named by execution records and by
-- the follower's dependency members, so renumbering would break the evidence
-- chain those rows depend on.

CREATE TABLE acquisitions_rebuilt (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    -- The IMMUTABLE public identifier, generated server-side from the system
    -- entropy source and never accepted from a caller.
    acquisition_id TEXT NOT NULL UNIQUE,

    -- The plan that approved this image, and the container it was assessed for.
    --
    -- plan_id is a plain column rather than a foreign key: retention may prune
    -- a superseded plan long before its acquisition record ages out, and an
    -- audit record that vanished when its plan did would defeat the purpose of
    -- keeping one.
    plan_id        TEXT NOT NULL,
    container_id   TEXT NOT NULL,
    container_name TEXT NOT NULL DEFAULT '',

    -- The immutable target. Components rather than a reference string: the
    -- reference sent to the daemon is assembled from these, and there is no
    -- column an operator could fill with an arbitrary pull argument.
    --
    -- target_digest is NOT NULL and constrained non-empty. A target without a
    -- digest is a target whose content can change after approval, and the whole
    -- safety model rests on that being impossible to express.
    target_registry   TEXT NOT NULL,
    target_repository TEXT NOT NULL,
    target_digest     TEXT NOT NULL CHECK (length(target_digest) > 0),
    -- The familiar form, for display only. Never used to perform a pull.
    target_reference  TEXT NOT NULL DEFAULT '',
    target_os         TEXT NOT NULL DEFAULT '',
    target_arch       TEXT NOT NULL DEFAULT '',
    target_variant    TEXT NOT NULL DEFAULT '',

    state TEXT NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'validating', 'pulling', 'verifying',
                         'succeeded', 'failed', 'cancelled', 'expired')),

    -- Why it did not succeed. A closed vocabulary: the classification decides
    -- whether a retry could ever help, and drives what an operator is told.
    failure TEXT NOT NULL DEFAULT ''
        CHECK (failure IN ('', 'preflight', 'dockerUnavailable', 'registry',
                           'transfer', 'timeout', 'digestMismatch',
                           'platformMismatch', 'verification', 'internal')),
    -- Which preflight check refused, when one did. A refusal is the safety
    -- model working, so it is recorded specifically rather than as a generic
    -- failure.
    --
    -- `selfUpdate` added here. See the header: it was produced by the code and
    -- refused by this constraint.
    refusal TEXT NOT NULL DEFAULT ''
        CHECK (refusal IN ('', 'planMissing', 'planSuperseded', 'planStale',
                           'recommendation', 'digestUnavailable', 'digestChanged',
                           'platformUnavailable', 'restoreReadiness',
                           'policyViolation', 'registryStale', 'duplicate',
                           'dockerUnavailable', 'limit', 'targetRefused',
                           'disabled', 'containerMissing', 'selfUpdate')),
    -- HarborMaster's own sentence about the outcome, bounded. Built from the
    -- vocabulary above; never a daemon or registry string.
    message TEXT NOT NULL DEFAULT '',

    -- What VERIFICATION found on the host, as opposed to what was expected.
    -- On a mismatch these two columns are the evidence, which is why they are
    -- recorded even when they disagree with the target.
    acquired_image_id TEXT NOT NULL DEFAULT '',
    acquired_digest   TEXT NOT NULL DEFAULT '',
    acquired_os       TEXT NOT NULL DEFAULT '',
    acquired_arch     TEXT NOT NULL DEFAULT '',
    acquired_variant  TEXT NOT NULL DEFAULT '',
    -- The local image size from inspection. Authoritative, unlike the
    -- transfer's own progress counters.
    size_bytes INTEGER NOT NULL DEFAULT 0,

    -- Transfer summary. Estimates for display: layers already present locally
    -- are never counted by the daemon's progress stream.
    layers            INTEGER NOT NULL DEFAULT 0,
    bytes_transferred INTEGER NOT NULL DEFAULT 0,
    -- The most recent bounded status line, e.g. "Downloading". Sanitised at the
    -- Docker adapter boundary before it ever reaches this column.
    progress TEXT NOT NULL DEFAULT '',

    requested_at TEXT NOT NULL,
    started_at   TEXT,
    completed_at TEXT,
    -- When a still-queued request is abandoned. An expired request was never
    -- validated and never pulled, and running it later would be acting on an
    -- approval whose evidence has aged.
    expires_at TEXT NOT NULL,

    -- The caller's idempotency key, when supplied. Empty means none, and empty
    -- is excluded from the uniqueness index below so unkeyed requests do not
    -- collide with each other.
    request_key TEXT NOT NULL DEFAULT '',

    -- The plan's input fingerprint as approved.
    plan_digest TEXT NOT NULL DEFAULT '',

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    -- Added by 0012. Two fields only, like every other asynchronous record.
    requested_by_user_id  TEXT NOT NULL DEFAULT '',
    requested_by_username TEXT NOT NULL DEFAULT ''
);

INSERT INTO acquisitions_rebuilt
    (id, host_id, acquisition_id, plan_id, container_id, container_name, target_registry, target_repository, target_digest, target_reference, target_os, target_arch, target_variant, state, failure, refusal, message, acquired_image_id, acquired_digest, acquired_os, acquired_arch, acquired_variant, size_bytes, layers, bytes_transferred, progress, requested_at, started_at, completed_at, expires_at, request_key, plan_digest, created_at, updated_at, requested_by_user_id, requested_by_username)
SELECT
    id, host_id, acquisition_id, plan_id, container_id, container_name, target_registry, target_repository, target_digest, target_reference, target_os, target_arch, target_variant, state, failure, refusal, message, acquired_image_id, acquired_digest, acquired_os, acquired_arch, acquired_variant, size_bytes, layers, bytes_transferred, progress, requested_at, started_at, completed_at, expires_at, request_key, plan_digest, created_at, updated_at, requested_by_user_id, requested_by_username
FROM acquisitions;

DROP TABLE acquisitions;

ALTER TABLE acquisitions_rebuilt RENAME TO acquisitions;

-- Recreated exactly as the live schema had them, so an upgraded database and a
-- fresh one are indistinguishable afterwards.
CREATE UNIQUE INDEX idx_acquisition_active_target
    ON acquisitions (container_id, target_digest)
    WHERE state IN ('queued', 'validating', 'pulling', 'verifying');
CREATE INDEX idx_acquisition_completed  ON acquisitions (completed_at)
    WHERE completed_at IS NOT NULL;
CREATE INDEX idx_acquisition_container  ON acquisitions (container_id, id DESC);
CREATE INDEX idx_acquisition_plan       ON acquisitions (plan_id, id DESC);
CREATE UNIQUE INDEX idx_acquisition_request_key
    ON acquisitions (request_key)
    WHERE request_key <> '';
CREATE INDEX idx_acquisition_requested  ON acquisitions (requested_at DESC);
CREATE INDEX idx_acquisition_requester
    ON acquisitions (requested_by_user_id, id DESC)
    WHERE requested_by_user_id <> '';
CREATE INDEX idx_acquisition_state      ON acquisitions (state, id DESC);
