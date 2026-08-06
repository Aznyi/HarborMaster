-- 0011_auth: authentication, authorization, and audit identity.
--
-- Until this migration HarborMaster had no notion of a user. Every endpoint
-- was reachable by anyone who could reach the port, which was the largest
-- residual risk in the threat model (R1) and the one that made every other
-- control conditional on network placement.
--
-- # What is stored, and what deliberately is not
--
-- NO PLAINTEXT PASSWORD, in any encoding, anywhere. A credential is an Argon2id
-- hash beside the parameters and salt it was produced with, so a future
-- parameter increase can re-hash on next login without invalidating existing
-- credentials.
--
-- NO RAW SESSION TOKEN. The token exists in one place -- the browser's cookie.
-- What is stored is a keyed digest under the installation key, so a stolen
-- database yields nothing that can authenticate, and a stolen database plus the
-- key yields only the ability to verify a token somebody already holds.
--
-- NO CSRF TOKEN, in any form. It is DERIVED from the raw session token on each
-- request (see internal/service/auth_session.go), which means the one
-- authentication secret a database thief might otherwise read directly does not
-- exist at rest.
--
-- # Immutability
--
-- audit_events is insert-only. There is no repository method that updates one,
-- no endpoint that edits one, and a test asserts the package contains no UPDATE
-- or DELETE against the table outside the bounded retention pass. An audit
-- trail that can be edited is not an audit trail.
--
-- Timestamps are RFC3339Nano UTC text throughout, matching 0001 through 0010.

-- ------------------------------------------------------------------ users --

-- One row per account.
--
-- There is no DELETE path. Disabling is reversible and preserves history; a
-- deleted user would leave audit rows naming an account nobody can look up,
-- which is the failure the audit log exists to prevent.
CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    -- The IMMUTABLE public identifier, generated server-side from the system
    -- entropy source. Used in URLs in place of the row id so an administration
    -- endpoint cannot be walked and the number of accounts is not disclosed.
    user_id TEXT NOT NULL UNIQUE,

    -- Stored already normalised: lowercase, trimmed, and restricted to an
    -- ASCII allowlist by the domain layer. UNIQUE on the normalised form, so
    -- case-insensitive uniqueness is a database invariant rather than a
    -- property of whichever collation a query happens to use.
    username TEXT NOT NULL UNIQUE
        CHECK (length(username) BETWEEN 3 AND 64),

    role TEXT NOT NULL
        CHECK (role IN ('viewer', 'operator', 'administrator')),

    status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'disabled')),

    -- Set on a credential an administrator or the bootstrap flow chose. The
    -- session is issued but every request except the password change is
    -- refused until it is cleared.
    must_change_password INTEGER NOT NULL DEFAULT 0
        CHECK (must_change_password IN (0, 1)),

    -- When the credential was last set.
    --
    -- Load-bearing rather than informational: a session issued before this
    -- moment is invalid, which is what makes a password change revoke every
    -- session even if the revocation UPDATE is lost to a crash.
    password_changed_at TEXT NOT NULL,

    last_login_at TEXT,

    -- The administrator who created the account. Empty for the bootstrap
    -- administrator, which by definition had nobody to create it.
    created_by_user_id TEXT NOT NULL DEFAULT '',

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Authentication looks a user up by username on every attempt, including
-- failed ones, so it must not be a scan.
CREATE INDEX idx_user_status ON users (status, username);

-- ------------------------------------------------------------ credentials --

-- The password verifier, in its own table.
--
-- Separate from `users` so that reading a user -- which every authorization
-- check does, on every request -- does not load the hash into memory at all.
-- The separation is what makes it safe to return a User from an API endpoint:
-- the struct has nowhere to put a credential because the query never selects
-- one.
--
-- ON DELETE CASCADE is present for correctness, not because anything deletes:
-- there is no user-deletion path.
CREATE TABLE user_credentials (
    user_id TEXT PRIMARY KEY
        REFERENCES users (user_id) ON DELETE CASCADE,

    -- The algorithm and the parameters it was produced with, stored ALONGSIDE
    -- the hash rather than read from configuration at verification time.
    --
    -- This is what makes a parameter increase safe. Raising the memory cost
    -- must not invalidate every existing credential; instead, verification uses
    -- the parameters the hash was made with, and a successful login whose
    -- parameters are below the current policy is transparently re-hashed.
    algorithm TEXT NOT NULL DEFAULT 'argon2id'
        CHECK (algorithm IN ('argon2id')),
    memory_kib   INTEGER NOT NULL CHECK (memory_kib > 0),
    iterations   INTEGER NOT NULL CHECK (iterations > 0),
    parallelism  INTEGER NOT NULL CHECK (parallelism > 0),

    -- Salt and hash, base64 (raw, unpadded). A unique random salt per
    -- credential, so two accounts choosing the same password produce different
    -- verifiers and a precomputed table is useless.
    salt TEXT NOT NULL CHECK (length(salt) > 0),
    hash TEXT NOT NULL CHECK (length(hash) > 0),

    -- Brute-force state, persisted rather than held in memory.
    --
    -- An in-memory counter resets when the process restarts, which hands an
    -- attacker a free reset. These survive, and drive an exponential backoff
    -- rather than a hard lockout: a hard lockout lets anyone who knows a
    -- username deny that account service by guessing at it.
    failed_attempts INTEGER NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
    locked_until    TEXT,
    last_failure_at TEXT,

    updated_at TEXT NOT NULL
);

-- --------------------------------------------------------------- sessions --

-- One row per issued session.
--
-- Server-side by design. A self-contained token -- a JWT, a signed cookie --
-- cannot be revoked without a denylist, and HarborMaster needs revocation on
-- logout, password change, disablement, and role change. A denylist is a
-- session table with worse failure modes, so this is the session table.
CREATE TABLE sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    -- The PUBLIC identifier. What an administrator sees in a session list and
    -- what a revocation request names. It is not the token and grants nothing.
    session_id TEXT NOT NULL UNIQUE,

    user_id TEXT NOT NULL
        REFERENCES users (user_id) ON DELETE CASCADE,

    -- Snapshots taken at issue, for display in a session list. Authorization
    -- NEVER reads them: the middleware re-reads the user on every request, so a
    -- role change takes effect immediately rather than at the next login.
    username TEXT NOT NULL,
    role     TEXT NOT NULL
        CHECK (role IN ('viewer', 'operator', 'administrator')),

    -- The keyed digest of the session token, under the installation key.
    --
    -- UNIQUE, which is both a correctness property and the lookup path: a
    -- request presents a token, the server digests it, and this index finds the
    -- row in one probe without ever holding the raw token at rest.
    token_digest TEXT NOT NULL UNIQUE CHECK (length(token_digest) > 0),

    created_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,

    -- Both expiries are enforced. Idle bounds an abandoned session; absolute
    -- bounds a stolen one that is being deliberately kept warm.
    idle_expires_at     TEXT NOT NULL,
    absolute_expires_at TEXT NOT NULL,

    revoked_at TEXT,
    revocation TEXT NOT NULL DEFAULT ''
        CHECK (revocation IN ('', 'loggedOut', 'passwordChanged', 'roleChanged',
                              'userDisabled', 'revokedByAdmin', 'superseded',
                              'expired')),

    -- Bounded, sanitised display fields so an operator can recognise their own
    -- sessions. Attacker-controlled text that reaches a browser, so both are
    -- truncated and stripped of control characters before they are written.
    user_agent  TEXT NOT NULL DEFAULT '',
    client_addr TEXT NOT NULL DEFAULT ''
);

-- Listing a user's live sessions, and enforcing the per-user cap.
CREATE INDEX idx_session_user ON sessions (user_id, id DESC);

-- The expiry sweep scans by absolute expiry, so it must not be a table scan on
-- an installation with a long session history.
CREATE INDEX idx_session_expiry ON sessions (absolute_expires_at)
    WHERE revoked_at IS NULL;

-- ----------------------------------------------------------- audit events --

-- Immutable security records.
--
-- # What can never be written here
--
-- There is no column for a password, a hash, a session token, a CSRF token, an
-- environment value, a registry credential, or a request body. `reason` is
-- built from a fixed HarborMaster vocabulary keyed by a classification, exactly
-- as the acquisition and execution messages are, so no attacker-influenced
-- string can reach a column an administrator renders.
--
-- # Actor fields are SNAPSHOTS
--
-- actor_username and actor_role record what was true when the action happened.
-- An account renamed or demoted afterwards must not rewrite the history of what
-- it did, which is precisely what a join against `users` would do.
--
-- actor_user_id is a plain column rather than a foreign key for the same
-- reason: the row must stand on its own, and an audit trail that could be
-- broken by a change to another table is not one.
CREATE TABLE audit_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    event_id TEXT NOT NULL UNIQUE,

    action  TEXT NOT NULL CHECK (length(action) > 0),
    outcome TEXT NOT NULL
        CHECK (outcome IN ('succeeded', 'failed', 'denied')),

    -- Empty for an unauthenticated attempt, which is exactly the case a failed
    -- login records.
    actor_user_id    TEXT NOT NULL DEFAULT '',
    actor_username   TEXT NOT NULL DEFAULT '',
    actor_role       TEXT NOT NULL DEFAULT ''
        CHECK (actor_role IN ('', 'viewer', 'operator', 'administrator')),
    actor_session_id TEXT NOT NULL DEFAULT '',

    target_type TEXT NOT NULL DEFAULT ''
        CHECK (target_type IN ('', 'user', 'session', 'container', 'snapshot',
                               'drift', 'policy', 'violation', 'plan',
                               'acquisition', 'execution', 'inventory',
                               'system')),
    -- A HarborMaster-generated or validated identifier. Never free text.
    target_id   TEXT NOT NULL DEFAULT '',
    target_name TEXT NOT NULL DEFAULT '',

    -- Correlates with the access log, so one investigation covers both.
    request_id  TEXT NOT NULL DEFAULT '',
    -- Normalised: port dropped, IPv6 canonicalised, unparseable becomes
    -- 'unknown'. See domain.NormaliseClientAddr.
    client_addr TEXT NOT NULL DEFAULT '',

    reason TEXT NOT NULL DEFAULT '',

    occurred_at TEXT NOT NULL
);

-- The audit page reads newest-first and filters by action, outcome, and actor.
CREATE INDEX idx_audit_occurred ON audit_events (occurred_at DESC, id DESC);
CREATE INDEX idx_audit_action   ON audit_events (action, id DESC);
CREATE INDEX idx_audit_actor    ON audit_events (actor_user_id, id DESC)
    WHERE actor_user_id <> '';
-- Failed and denied attempts are what an administrator opens the page to find.
CREATE INDEX idx_audit_outcome  ON audit_events (outcome, id DESC)
    WHERE outcome <> 'succeeded';

-- --------------------------------------------------------- bootstrap state --

-- A single row recording whether HarborMaster has an administrator yet.
--
-- # Why a table rather than "SELECT COUNT(*) FROM users"
--
-- Because the question is not "does a user exist" but "has this installation
-- been claimed". Those differ in exactly the case that matters: an installation
-- whose last administrator was disabled still has users, and must NOT fall back
-- into a bootstrap flow that would let anyone who can reach the port create a
-- new administrator. Once claimed, always claimed; recovery is the CLI.
--
-- # The bootstrap token
--
-- A brand-new installation prints a one-time token at startup and stores only
-- its keyed digest here. The web bootstrap flow requires it, so claiming an
-- installation needs access to the server's log or data directory rather than
-- merely being first to the port. The CLI needs no token: filesystem access to
-- the database is a stronger proof than the token is.
CREATE TABLE auth_state (
    -- Exactly one row, enforced by the CHECK. A second row would make "is this
    -- installation claimed" ambiguous.
    id INTEGER PRIMARY KEY CHECK (id = 1),

    -- NULL until the first administrator exists.
    bootstrap_completed_at TEXT,
    -- Which account claimed it, for the audit trail.
    bootstrap_user_id TEXT NOT NULL DEFAULT '',

    -- Keyed digest of the one-time token, never the token itself.
    bootstrap_token_digest     TEXT NOT NULL DEFAULT '',
    bootstrap_token_expires_at TEXT,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Seeded unclaimed. The timestamps are placeholders replaced on first write;
-- an empty table would make every read a two-case branch for no benefit.
INSERT INTO auth_state (id, created_at, updated_at)
VALUES (1, '1970-01-01T00:00:00Z', '1970-01-01T00:00:00Z');
