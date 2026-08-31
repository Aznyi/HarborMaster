-- 0032_container_update_preferences: one container's chosen update behaviour.
--
-- # Why this is a table and not a Docker label
--
-- The obvious representation for "how should this container be updated" is a
-- label on the container, and HarborMaster already honours several. It cannot
-- be the representation for a SETTING, for one hard reason:
--
--     Docker has no operation that changes a label on an existing container.
--
-- Labels are part of the creation configuration. `docker update` cannot touch
-- them, and HarborMaster's own ContainerMutator has exactly five methods --
-- create, start, stop, rename, remove -- with no sixth that could. Storing this
-- preference in a label would mean STOPPING AND RECREATING A RUNNING CONTAINER
-- every time somebody changed a dropdown, which is precisely the thing an
-- operator is choosing between when they open this control.
--
-- A second reason confirms it: on a Compose-managed host the labels belong to
-- the Compose file, and the next `docker compose up` would silently discard
-- anything HarborMaster wrote there. HarborMaster does not fight an external
-- orchestrator for ownership of a file it does not own.
--
-- External labels remain authoritative as SAFETY inputs. This table never
-- overrides them; see the composition rule below.
--
-- # Why the key is the container NAME
--
-- Because a preference has to survive the thing it configures. A successful
-- update recreates the container, and the replacement has a different Docker
-- id -- so a preference keyed on the id would be discarded by the first update
-- it authorised, which is the worst possible moment to forget it.
--
-- The name is the identity HarborMaster already uses everywhere this problem
-- has been solved before: `automation_pauses.container_name` (with the id kept
-- only as evidence) and `image_lineage.container_name` as its PRIMARY KEY. This
-- table follows that established rule rather than inventing a third one.
--
-- `container_id` is recorded for the same reason those tables record it: as
-- evidence of what was observed when the row was written. Nothing resolves a
-- preference by it.
--
-- # What a preference may do
--
-- It may only make automation SAFER. `domain.Resolve` composes it onto the
-- governing policy the same way a label is composed -- narrowing the mode,
-- never widening it -- so a preference can lower `automatic` to
-- `approvalRequired` or `observe`, and can never raise a policy's ceiling.
--
-- That is what makes this a presentation of the existing engine rather than a
-- second one. An operator who selects "Automatic" on a container an update
-- policy holds for review does not get automatic; they get the policy's
-- behaviour and a sentence saying which rule decided it.

CREATE TABLE container_update_preferences (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    -- The stable identity. One preference per container, by name.
    container_name TEXT NOT NULL,

    -- Evidence of the container observed when this row was last written. Never
    -- used to resolve the preference: it changes on every recreation.
    container_id TEXT NOT NULL DEFAULT '',

    -- The chosen behaviour. CHECKed against the closed vocabulary in
    -- internal/domain, because a value that reached this column unchecked would
    -- be a container quietly automated differently from what an operator read
    -- on the screen.
    --
    -- `automatic` is the "no restriction" choice: it narrows nothing and leaves
    -- the governing policy in force. It is stored rather than deleted so that
    -- "an operator chose this" and "nobody has chosen" stay distinguishable.
    behavior TEXT NOT NULL,

    -- Who chose it, for the same reason every other state-changing record in
    -- this schema carries an actor.
    set_by_user_id  TEXT NOT NULL DEFAULT '',
    set_by_username TEXT NOT NULL DEFAULT '',

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    CHECK (container_name <> ''),
    CHECK (behavior IN ('automatic', 'reviewFirst', 'monitorOnly'))
);

-- One preference per container per host. The uniqueness is the whole storage
-- contract: setting a behaviour twice edits one row rather than accumulating
-- rows whose order would decide the answer.
CREATE UNIQUE INDEX idx_container_update_preferences_name
    ON container_update_preferences (host_id, container_name);
