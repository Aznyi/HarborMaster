-- 0025_dependency_operations: one coordinated, dependency-safe provider update.
--
-- # Why these two tables exist
--
-- Invariant B. A provider reaching `succeeded` means ONE CONTAINER was replaced.
-- At that instant every container sharing its namespace is attached to a
-- namespace that no longer exists -- running, with no network, logging nothing.
-- Verified against Docker 29.6.2.
--
-- So "the provider succeeded" and "the operation succeeded" are different facts.
-- These tables are the second one, and it does not clear until every mandatory
-- rebind has itself reached VERIFIED success.
--
-- # Why persisted rather than held in memory
--
-- A provider update plus three rebinds is minutes of work across four separate
-- executions, and the process can be killed at any point. An in-memory
-- coordinator would forget what remained, and the safe guess -- redo everything
-- -- is a second unattended mutation of containers that may already be correct.
--
-- Progress is therefore derivable from rows alone. Nothing about reconstructing
-- an operation depends on this process having been alive for any of it.
--
-- # What is NOT stored
--
-- No secret, no environment value, no image reference from a caller, no digest
-- from a caller, and no daemon text. Every reason column is a HarborMaster
-- closed vocabulary constrained by CHECK, so a row cannot carry a sentence
-- somebody else wrote.
--
-- # This migration invents nothing
--
-- Both tables start empty. No operation is derived for a historical execution:
-- an update performed before this existed was performed under the old rules, and
-- manufacturing a record claiming otherwise would be a lie about what happened.
-- Existing update behaviour is unchanged until new dependency evidence appears.

-- ------------------------------------------------------------ operations --

CREATE TABLE dependency_operations (
    operation_id          TEXT PRIMARY KEY,

    -- The container whose namespace is shared, by stable NAME. Not an id: an id
    -- changes on every recreation, and this record has to survive exactly that.
    provider_name         TEXT NOT NULL,

    -- The records the provider's own update runs under. Both are
    -- HarborMaster-generated identifiers, never caller input.
    provider_plan_id      TEXT NOT NULL DEFAULT '',
    provider_execution_id TEXT NOT NULL DEFAULT '',

    -- internal/domain/dependency_operation.go holds the vocabulary. The CHECK
    -- is what stops a state this build does not understand being written by a
    -- future version and read as something benign by this one.
    state                 TEXT NOT NULL
                          CHECK (state IN ('queued', 'providerRunning', 'providerVerified',
                                           'rebindPending', 'rebindRunning',
                                           'succeeded', 'failed', 'blocked', 'interrupted')),

    -- Closed vocabulary, never a daemon string.
    failure               TEXT NOT NULL DEFAULT ''
                          CHECK (failure IN ('', 'providerFailed', 'rebindFailed',
                                             'dependentNotRebindable', 'evidenceUnavailable')),

    -- Attribution, in the same two fields every other asynchronous record uses.
    -- Never a role, a session, or an address: see domain.Requester.
    requested_by_user     TEXT NOT NULL DEFAULT '',
    requested_by_name     TEXT NOT NULL DEFAULT '',

    created_at            TEXT NOT NULL,
    updated_at            TEXT NOT NULL,
    completed_at          TEXT,

    CHECK (length(provider_name) > 0),
    -- A terminal state has an ending; a non-terminal one does not. Stops a row
    -- claiming to be finished with no timestamp, and one claiming to be running
    -- with one.
    CHECK ((state IN ('succeeded', 'failed', 'blocked')) = (completed_at IS NOT NULL))
);

-- The recovery query: every operation a restart has to reconstruct.
CREATE INDEX idx_dependency_operations_open
    ON dependency_operations (state, created_at);

-- "What happened to this provider" and "is one already running for it".
CREATE INDEX idx_dependency_operations_provider
    ON dependency_operations (provider_name, created_at DESC);

-- One operation per provider at a time. A second coordinated update of the same
-- container while the first is mid-flight would have two components deciding
-- what its dependents are attached to.
CREATE UNIQUE INDEX idx_dependency_operations_active
    ON dependency_operations (provider_name)
 WHERE state NOT IN ('succeeded', 'failed', 'blocked');

-- --------------------------------------------------------------- members --

-- One row per MANDATORY rebind.
--
-- Written BEFORE the provider is stopped. That ordering is the safety property:
-- a crash between writing these and stopping the provider leaves a complete
-- record of what was supposed to happen, while the reverse ordering would leave
-- a stopped provider and no record of what depended on it.
CREATE TABLE dependency_operation_members (
    operation_id         TEXT NOT NULL
                         REFERENCES dependency_operations (operation_id) ON DELETE CASCADE,

    -- Both endpoints by stable NAME, for the same reason the operation is.
    dependent_name       TEXT NOT NULL,
    provider_name        TEXT NOT NULL,

    -- Always a HARD source. An operator relationship constrains ORDER and can
    -- never require a namespace rebind, so the CHECK excludes it: a row claiming
    -- an operator-defined rebind would be asserting a runtime requirement the
    -- daemon does not enforce.
    source               TEXT NOT NULL
                         CHECK (source IN ('dockerNetworkNamespace',
                                           'dockerIPCNamespace',
                                           'dockerPIDNamespace')),

    -- The provider id the dependent NAMED when the operation was created -- the
    -- one about to go stale -- and the replacement's id once established.
    -- Evidence, never identity: nothing is decided from either.
    expected_provider_id TEXT NOT NULL DEFAULT '',
    target_provider_id   TEXT NOT NULL DEFAULT '',

    -- The records the rebind runs under. Their PRESENCE means work was
    -- requested. Only `state` says what happened -- see the note on
    -- domain.DependencyMemberState about why an id is not a completion.
    plan_id              TEXT NOT NULL DEFAULT '',
    acquisition_id       TEXT NOT NULL DEFAULT '',
    execution_id         TEXT NOT NULL DEFAULT '',

    state                TEXT NOT NULL
                         CHECK (state IN ('pending', 'planCreated', 'acquired', 'executing',
                                          'verified', 'blocked', 'failed', 'interrupted')),

    -- Closed vocabulary from internal/domain/dependency_rebind.go.
    refusal              TEXT NOT NULL DEFAULT ''
                         CHECK (refusal IN ('', 'noEvidence', 'dependentNotPresent',
                                            'namespaceObservationStale', 'providerMismatch',
                                            'notRecreatable', 'harborMasterContainer',
                                            'preservedContainer', 'optedOut',
                                            'runningReferenceUnestablished',
                                            'runningDigestUnestablished')),

    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL,

    -- A container cannot be its own namespace provider.
    CHECK (dependent_name <> provider_name),
    CHECK (length(dependent_name) > 0),
    CHECK (length(provider_name) > 0),
    -- A blocked member has a reason; a member in any other state does not carry
    -- one. Stops a verified row quietly holding a refusal nobody reads.
    CHECK ((state = 'blocked') = (refusal <> '')),

    PRIMARY KEY (operation_id, dependent_name)
);

-- The recovery read: every member of an operation, in a deterministic order.
CREATE INDEX idx_dependency_members_operation
    ON dependency_operation_members (operation_id, dependent_name);

-- "Is this container part of an outstanding rebind" -- asked per container by
-- the container detail read, so it is indexed rather than scanned.
CREATE INDEX idx_dependency_members_dependent
    ON dependency_operation_members (dependent_name, state);

-- Reconstructing a member from the execution it names, which is what restart
-- recovery does instead of trusting the member's own cached state.
CREATE INDEX idx_dependency_members_execution
    ON dependency_operation_members (execution_id)
 WHERE execution_id <> '';
