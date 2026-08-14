-- 0024_workload_dependencies: what must be stable before something else changes.
--
-- # What this adds
--
-- Two things, and only one of them is a table.
--
--   1. Four columns on `containers`, holding the three shareable namespace
--      declarations Docker records plus a flag saying whether they have been
--      READ. This is a projection of configuration HarborMaster already
--      inspects; it stores no new kind of fact.
--   2. `workload_dependencies`, holding OPERATOR-defined ordering only.
--
-- # Why discovered relationships have no table
--
-- A relationship Docker itself establishes -- `network_mode: container:<other>`
-- and its IPC and PID equivalents -- is DERIVED from the inventory on every
-- read, never stored.
--
-- Storing it would create a second copy of a fact that changes on every
-- recreation, and therefore a second thing that can be wrong about the host.
-- It would need its own invalidation, its own staleness gate, and its own
-- reconciliation after every execution and every rollback. The inventory row
-- already IS the evidence, it is already refreshed on a schedule, and its
-- freshness is already a refusal condition for a recreation. Deriving costs one
-- indexed query and removes the entire class of stale-dependency defects.
--
-- Operator relationships are different in kind: they are assertions about
-- APPLICATION behaviour that nothing on the host records, so there is nowhere
-- else for them to live.
--
-- # Why the namespace columns rather than reading container_config
--
-- The normalized configuration is one JSON document per container. Building the
-- estate's graph from it would mean decoding up to two thousand documents on
-- every evaluation to read three short strings. Four narrow columns make the
-- whole graph one indexed read.
--
-- # namespaces_observed is what makes this migration SAFE
--
-- Every existing row gets 0.
--
-- An empty `network_mode` and an UNREAD `network_mode` are opposite facts. The
-- first says "this container shares no namespace"; the second says
-- "HarborMaster has not looked". If they were collapsed, then for one refresh
-- interval after an upgrade every container on the host would read as
-- independent -- and a container that looks independent may be updated in any
-- order, including before the provider whose namespace it is actually sharing.
--
-- That is the same failure direction as silently truncating a graph: it can
-- only ever make a container appear SAFER than it is. So the flag is a POSITIVE
-- fact with a zero value of false, a container carrying 0 is refused rather than
-- cleared, and the first inventory refresh after upgrade sets it for every
-- present container.
--
-- # What this does NOT grant
--
-- No capability, and no widening of any policy. A relationship recorded here
-- can only ever cause HarborMaster to WAIT or to REFUSE. There is no value of
-- any column below that enrols a container into an update policy, that reaches
-- a Docker socket, or that makes an update happen which would not otherwise
-- have happened.

-- ------------------------------------------------ namespace projection --

-- The three shareable namespace declarations, exactly as the daemon reported
-- them. Stored raw rather than parsed: parsing is the domain's job, and a
-- column holding a half-interpreted value is a column two readers can disagree
-- about.
--
-- Values look like 'bridge', 'host', 'none', a network name, or
-- 'container:<64 hex>' for a namespace shared with another container. Only the
-- last of those produces a relationship.
ALTER TABLE containers ADD COLUMN network_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE containers ADD COLUMN ipc_mode     TEXT NOT NULL DEFAULT '';
ALTER TABLE containers ADD COLUMN pid_mode     TEXT NOT NULL DEFAULT '';

-- The fail-closed flag. See the note above: 0 means "not read", which blocks,
-- and is deliberately NOT the same as an empty mode, which clears.
ALTER TABLE containers
    ADD COLUMN namespaces_observed INTEGER NOT NULL DEFAULT 0
    CHECK (namespaces_observed IN (0, 1));

-- Serves the one query the graph is built from: every present container's
-- namespace facts, in id order. Covering, so the graph never touches the table
-- itself.
CREATE INDEX idx_containers_namespaces
    ON containers (present, id, network_mode, ipc_mode, pid_mode, namespaces_observed);

-- ---------------------------------------------- operator relationships --

-- One row per operator-asserted ordering constraint.
--
-- Keyed on NAMES, not ids. A container id changes on every recreation, which is
-- the event this subsystem exists to survive, so a relationship pinned to one
-- would stop describing the workload the moment either end was updated. This is
-- the same choice update policy selectors make, and for the same reason.
--
-- There is deliberately no foreign key to `containers`. A relationship must
-- survive its endpoints being recreated -- which deletes and reinserts rows
-- under new ids -- and it must also survive an endpoint being removed, because
-- an operator relationship whose target has vanished has to BLOCK the dependent
-- rather than silently disappear and clear it.
CREATE TABLE workload_dependencies (
    dependency_id   TEXT PRIMARY KEY,

    -- dependent_name NEEDS dependency_name. Execution order is the reverse of
    -- the arrow: the dependency is recreated first.
    dependent_name  TEXT NOT NULL,
    dependency_name TEXT NOT NULL,

    -- Only 'operator' is storable. The three namespace sources are derived and
    -- may not be written, so an insert claiming one is refused by the database
    -- as well as by the domain -- two independent refusals, because a row
    -- asserting a runtime requirement the daemon does not enforce would make
    -- HarborMaster wait on, or refuse, an update for a reason that is not true.
    source          TEXT NOT NULL DEFAULT 'operator'
                    CHECK (source = 'operator'),

    created_at      TEXT NOT NULL,
    -- Attribution, in the same two fields every other asynchronous record uses.
    -- Never a role, a session, or an address: see domain.Requester.
    created_by_user TEXT NOT NULL DEFAULT '',
    created_by_name TEXT NOT NULL DEFAULT '',

    -- A container cannot depend on itself. Enforced here as well as in the
    -- domain validator, because a self-edge is the smallest possible cycle and
    -- the cheapest place to make it impossible is the schema.
    CHECK (dependent_name <> dependency_name),
    CHECK (length(dependent_name) > 0),
    CHECK (length(dependency_name) > 0),

    -- One relationship per ordered pair. A second insert of the same pair is a
    -- duplicate, not a second constraint.
    UNIQUE (dependent_name, dependency_name)
);

-- The two directions the API and the UI ask for: "what does this container
-- depend on" and "what depends on this container". Both are point lookups.
CREATE INDEX idx_workload_dependencies_dependent
    ON workload_dependencies (dependent_name, dependency_name);

CREATE INDEX idx_workload_dependencies_dependency
    ON workload_dependencies (dependency_name, dependent_name);
