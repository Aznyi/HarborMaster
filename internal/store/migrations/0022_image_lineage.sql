-- 0022_image_lineage: what a managed container FOLLOWS, apart from what it RUNS.
--
-- # The defect this closes
--
-- A recreation is digest-pinned on purpose: the replacement is created from
-- `repo@sha256:...`, the exact artefact that was planned, acquired, approved and
-- verified, never from the mutable tag it came from. That is correct and it must
-- not change.
--
-- On its own it was also terminal. The next inventory pass saw only
-- `repo@sha256:...`; image intelligence answered, accurately, that a digest
-- cannot move; the planner had nothing to propose; and the container left
-- automation for good. Every container received exactly one automated update
-- and then silently stopped being managed.
--
-- The two references were the same string, so nothing distinguished them:
--
--   EXECUTION REFERENCE  repo@sha256:...  what runs. Immutable. Unchanged.
--   TRACKING REFERENCE   repo:tag         what HarborMaster watches. New here.
--
-- This table is the tracking half, and it is AUTHORITATIVE. Containers also
-- carry `io.harbormaster.image.tracking` so lineage can be recovered from Docker
-- alone, but a label is written by whoever created the container and is treated
-- as evidence, never as authority.
--
-- # Why the key is a name
--
-- A recreation replaces the container and its id with it, so an id-keyed row
-- would stop describing the container at the exact moment it did its job. Update
-- policies, pauses and approvals already key by name for the same reason.
-- container_id is kept alongside as EVIDENCE: it is how reconciliation notices
-- that something replaced the container without HarborMaster's involvement.
--
-- # Why running_digest lives here rather than being read from the inventory
--
-- The inventory says what is running. This column says what HarborMaster
-- APPROVED and believes is running, and the two disagreeing is the signal that
-- somebody changed the host by hand. Collapsing them would make external change
-- undetectable, and §7 of this phase turns on being able to tell them apart.
--
-- It advances only on a verified recreation or a completed rollback. Never on a
-- pull: acquiring an image changes the image store, not the workload.
--
-- Timestamps are RFC3339Nano UTC text throughout, matching 0001 through 0021.

CREATE TABLE image_lineage (
    -- The container this describes, by name. One row per managed container.
    container_name TEXT PRIMARY KEY,

    -- The container observed when this row was last written. Evidence for
    -- reconciliation, never identity; empty when nothing has been observed yet.
    container_id TEXT NOT NULL DEFAULT '',

    -- tracked   a tracking reference is known and trusted
    -- untracked the workload runs a digest attributable to no trusted tag
    --
    -- 'untracked' is a deliberate, recorded answer rather than an absent row:
    -- "we looked and there is nothing to follow" is a different fact from "we
    -- have not looked", and an operator has to be able to see which one holds.
    state TEXT NOT NULL CHECK (state IN ('tracked', 'untracked')),

    -- observed    the container declared a tag and HarborMaster wrote it down
    -- recreation  HarborMaster performed the recreation and carried it across
    -- migration   recovered from HarborMaster's own historical records
    --
    -- Recorded because the origins are not equally strong, and a reconciliation
    -- choosing between two claims should see which one HarborMaster watched.
    -- Note there is no 'label' origin: a label can corroborate a record, and
    -- can never be the reason one exists.
    origin TEXT NOT NULL CHECK (origin IN ('observed', 'recreation', 'migration')),

    -- The canonical mutable reference update discovery resolves, and the short
    -- form an operator recognises. Both empty when untracked.
    tracking_reference TEXT NOT NULL DEFAULT '',
    tracking_familiar  TEXT NOT NULL DEFAULT '',

    -- The canonical repository path of the tracking reference, held separately
    -- so a cross-repository substitution can be refused without re-parsing and
    -- so a mismatch is visible in the row itself.
    repository TEXT NOT NULL DEFAULT '',

    -- The digest HarborMaster approved and believes is executing.
    running_digest TEXT NOT NULL DEFAULT '',

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    -- A tracked row without something to track would be a row that silently
    -- does nothing. Refused by the schema rather than by remembering to check.
    CHECK (state <> 'tracked' OR (tracking_reference <> '' AND repository <> ''))
);

-- Update discovery seeds the registry check set from the distinct tracking
-- references of every tracked row, on every inventory refresh.
CREATE INDEX idx_image_lineage_tracking
    ON image_lineage (tracking_reference)
    WHERE state = 'tracked';

-- Reconciliation looks a container up by the id it last observed.
CREATE INDEX idx_image_lineage_container_id
    ON image_lineage (container_id)
    WHERE container_id <> '';
