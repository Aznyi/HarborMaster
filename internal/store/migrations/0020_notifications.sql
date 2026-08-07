-- 0020_notifications: destinations, routing rules, and the delivery record.
--
-- # What this migration adds, and what it must never hold
--
-- HarborMaster's second outbound egress. The first, image intelligence, sends
-- anonymous GETs to hosts derived from image references. This one sends data to
-- a URL an administrator typed, which makes the credential handling here the
-- most consequential part of the schema.
--
-- Two things follow, and the table shapes below exist to enforce them.
--
-- ## The URL is a credential, and lives apart from everything that is read
--
-- For Slack, Discord, and Teams the incoming-webhook URL IS the bearer token:
-- anybody holding it can post to that channel forever. So it is NOT a column on
-- the row every list query selects. It lives in notification_secrets, keyed one
-- to one, and is read only by the code that is about to send.
--
-- What a list query gets is `endpoint`: a scheme and a host, and nothing else.
-- `https://hooks.slack.com` identifies a destination without handing over the
-- ability to post to it.
--
-- ## A delivery record holds what was SENT, never what came back
--
-- The payload is stored, because "what did HarborMaster actually say" is a
-- question an operator asks -- and by construction it contains nothing sensitive:
-- a title and body HarborMaster wrote, an event from a closed vocabulary, and
-- bounded label/value pairs.
--
-- The RESPONSE is not stored. A response body is third-party text that would
-- travel into a page an operator reads, and a transport error message embeds the
-- URL. What is stored instead is the status code and one of HarborMaster's own
-- sentences from a fixed set.
--
-- Timestamps are RFC3339Nano UTC text throughout, matching 0001 through 0019.

-- ------------------------------------------------------- destinations --

-- One row per place notifications are sent.
--
-- # Why there is no DELETE
--
-- notification_deliveries references this table, and the record of what was
-- sent must survive the destination being withdrawn -- an operator asking "did
-- anybody get told about that outage in March" must not get a different answer
-- because somebody tidied up in April. DELETE archives.
CREATE TABLE notification_destinations (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    -- The IMMUTABLE public identifier, generated server-side from the system
    -- entropy source. Rules reference it, so a mutable id would orphan routing.
    destination_id TEXT NOT NULL UNIQUE,

    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    channel     TEXT NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,

    -- The SAFE rendering of where this goes: scheme and host for a webhook,
    -- host and port for a relay. NEVER the full URL. Every read path selects
    -- this column and no read path selects the secret.
    endpoint TEXT NOT NULL DEFAULT '',

    -- The one piece of operator text that reaches a delivered message.
    -- Bounded, sanitised, and inserted as TEXT by every channel encoder.
    title_prefix TEXT NOT NULL DEFAULT '',

    -- Email recipients, as a JSON array of addresses. Each was net/mail parsed
    -- and refused for control characters and line breaks before it was written,
    -- which is what makes SMTP header injection unrepresentable rather than
    -- merely checked for.
    email_to_json TEXT NOT NULL DEFAULT '[]',
    email_from    TEXT NOT NULL DEFAULT '',

    -- Health, denormalised so a list can show "this destination is not working"
    -- without joining the delivery history.
    last_result          TEXT NOT NULL DEFAULT '',
    last_attempt_at      TEXT,
    -- HarborMaster's own sentence about the last failure. Never the transport's
    -- error text, which embeds hostnames and occasionally the URL.
    last_error           TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,

    archived    INTEGER NOT NULL DEFAULT 0,
    archived_at TEXT,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    CHECK (name <> ''),
    CHECK (enabled IN (0, 1)),
    CHECK (archived IN (0, 1)),
    CHECK (channel IN ('webhook', 'discord', 'slack', 'teams', 'email')),
    CHECK (last_result IN ('', 'pending', 'succeeded', 'failed',
                           'retrying', 'suppressed', 'dropped')),
    CHECK (consecutive_failures >= 0),
    CHECK (LENGTH(name)          <= 120),
    CHECK (LENGTH(description)   <= 500),
    CHECK (LENGTH(endpoint)      <= 300),
    CHECK (LENGTH(title_prefix)  <= 60),
    CHECK (LENGTH(email_to_json) <= 8192),
    CHECK (LENGTH(email_from)    <= 320),
    CHECK (LENGTH(last_error)    <= 500),
    -- A webhook destination has no recipients and an email destination has no
    -- endpoint scheme. Not enforceable in full here -- the endpoint is a
    -- rendering -- but the recipient rule is, and it is the one that matters:
    -- a webhook row carrying addresses would mean the validation was bypassed.
    CHECK (channel = 'email' OR email_to_json IN ('[]', ''))
);

CREATE INDEX idx_notification_destination_active
    ON notification_destinations (archived, enabled, name);
CREATE INDEX idx_notification_destination_name
    ON notification_destinations (name, id);
-- Serves the "which destinations are failing" count the dashboard shows.
CREATE INDEX idx_notification_destination_failing
    ON notification_destinations (last_result, id)
    WHERE archived = 0;

-- --------------------------------------------------------- the secrets --

-- The credential half of a destination, in its own table.
--
-- # Why a separate table rather than a column
--
-- Because a column is loaded by `SELECT *` and by any query somebody adds
-- later. A separate table is loaded only by a query that names it, and the
-- repository exposes exactly one method that does -- called by the sender,
-- immediately before it sends.
--
-- It is stored in the CLEAR, and that is worth being explicit about. A webhook
-- URL has to be usable, so it cannot be a keyed digest the way a password or a
-- session token is. What protects it is the same thing that protects the
-- database as a whole: file permissions, a non-root container, and a volume the
-- operator controls. HarborMaster's contribution is that it never leaves: no
-- API response, no log line, no error message, and no delivery record.
CREATE TABLE notification_secrets (
    -- ON DELETE CASCADE: a destination that is genuinely deleted by hand takes
    -- its credential with it. Archiving does not, because an archived
    -- destination may be un-archived, and losing the URL would make that a
    -- re-entry rather than a restore.
    destination_id TEXT PRIMARY KEY
        REFERENCES notification_destinations (destination_id) ON DELETE CASCADE,

    -- The full webhook URL, including whatever token its path or query carries.
    webhook_url TEXT NOT NULL DEFAULT '',

    -- SMTP authentication. The username is not a secret and lives here anyway,
    -- so the pair is loaded and handled as one thing.
    smtp_username TEXT NOT NULL DEFAULT '',
    smtp_password TEXT NOT NULL DEFAULT '',

    updated_at TEXT NOT NULL,

    CHECK (LENGTH(webhook_url)   <= 2048),
    CHECK (LENGTH(smtp_username) <= 320),
    CHECK (LENGTH(smtp_password) <= 512)
);

-- ------------------------------------------------------------- rules --

-- Which events reach which destinations.
--
-- Deliberately not a compliance policy and not an update policy. Three
-- subsystems now have something called a policy and they do entirely different
-- things: one REPORTS, one ACTS, this one ROUTES. Sharing a table would mean an
-- edit to one could change the behaviour of another.
CREATE TABLE notification_rules (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    rule_id TEXT NOT NULL UNIQUE,

    name    TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,

    -- Events as a JSON array. EMPTY MEANS EVERY EVENT -- unlike an update
    -- policy's selector, where empty means nothing, because the cost of being
    -- wrong here is an extra message rather than an unintended container
    -- change.
    events_json TEXT NOT NULL DEFAULT '[]',

    minimum_severity TEXT NOT NULL DEFAULT 'info',

    -- Destination ids as a JSON array. Validated against the destinations table
    -- by the service; not a foreign key, because a rule naming several
    -- destinations cannot express that relationally without a join table whose
    -- only purpose would be referential integrity on a list that is always read
    -- whole.
    destinations_json TEXT NOT NULL DEFAULT '[]',

    -- Suppresses a repeat of the same dedup key within the window. Zero
    -- disables suppression, which is honest rather than convenient.
    cooldown_seconds INTEGER NOT NULL DEFAULT 0,

    archived    INTEGER NOT NULL DEFAULT 0,
    archived_at TEXT,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    CHECK (name <> ''),
    CHECK (enabled IN (0, 1)),
    CHECK (archived IN (0, 1)),
    CHECK (minimum_severity IN ('info', 'warning', 'critical')),
    CHECK (cooldown_seconds >= 0 AND cooldown_seconds <= 86400),
    CHECK (LENGTH(name)              <= 120),
    CHECK (LENGTH(events_json)       <= 4096),
    CHECK (LENGTH(destinations_json) <= 2048)
);

CREATE INDEX idx_notification_rule_active
    ON notification_rules (archived, enabled, id);
CREATE INDEX idx_notification_rule_name
    ON notification_rules (name, id);

-- ------------------------------------------------------- deliveries --

-- One row per notification going to one destination.
--
-- # What is here, and what is deliberately not
--
-- HERE: the payload HarborMaster sent, the result, the attempt count, the HTTP
-- status, and HarborMaster's own sentence about a failure.
--
-- NOT HERE: the destination URL, the SMTP password, the response body, and the
-- transport's error text. The first two are credentials; the third and fourth
-- are third-party text that would travel into a page an operator reads.
CREATE TABLE notification_deliveries (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id TEXT    NOT NULL DEFAULT 'local',

    delivery_id TEXT NOT NULL UNIQUE,

    -- ON DELETE RESTRICT: a delivery outlives its destination's withdrawal.
    destination_id TEXT NOT NULL
        REFERENCES notification_destinations (destination_id) ON DELETE RESTRICT,
    -- Denormalised so the history stays readable after a destination is
    -- archived or renamed.
    destination_name TEXT NOT NULL DEFAULT '',
    channel          TEXT NOT NULL,

    -- The rule that routed it. Nullable: a test delivery is not routed by one.
    rule_id   TEXT,
    rule_name TEXT NOT NULL DEFAULT '',

    -- The payload. Every field is HarborMaster's own: an event from a closed
    -- vocabulary, a severity, and text HarborMaster wrote.
    event          TEXT NOT NULL,
    severity       TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    body           TEXT NOT NULL DEFAULT '',
    container_name TEXT NOT NULL DEFAULT '',

    result      TEXT    NOT NULL DEFAULT 'pending',
    attempts    INTEGER NOT NULL DEFAULT 0,
    -- The HTTP status a webhook returned. Zero for email and for a failure that
    -- never reached a response.
    status_code INTEGER NOT NULL DEFAULT 0,
    -- HarborMaster's own sentence, from a fixed vocabulary.
    error       TEXT    NOT NULL DEFAULT '',

    -- What suppression compared on.
    dedup_key TEXT NOT NULL DEFAULT '',

    queued_at       TEXT    NOT NULL,
    completed_at    TEXT,
    next_attempt_at TEXT,
    duration_ms     INTEGER NOT NULL DEFAULT 0,

    CHECK (result IN ('pending', 'succeeded', 'failed',
                      'retrying', 'suppressed', 'dropped')),
    CHECK (channel IN ('webhook', 'discord', 'slack', 'teams', 'email')),
    CHECK (severity IN ('info', 'warning', 'critical')),
    CHECK (attempts >= 0),
    CHECK (status_code >= 0 AND status_code < 1000),
    CHECK (duration_ms >= 0),
    CHECK (LENGTH(title)          <= 200),
    CHECK (LENGTH(body)           <= 2000),
    CHECK (LENGTH(error)          <= 500),
    CHECK (LENGTH(dedup_key)      <= 200),
    CHECK (LENGTH(container_name) <= 255),
    -- A delivery that has not been attempted cannot have succeeded, and one
    -- that succeeded must have been attempted. The ordering, made
    -- unrepresentable in reverse rather than merely conventional.
    CHECK (result <> 'succeeded' OR attempts > 0)
);

CREATE INDEX idx_notification_delivery_time
    ON notification_deliveries (queued_at DESC, id DESC);
CREATE INDEX idx_notification_delivery_destination
    ON notification_deliveries (destination_id, queued_at DESC, id DESC);
CREATE INDEX idx_notification_delivery_result
    ON notification_deliveries (result, queued_at DESC, id DESC);
CREATE INDEX idx_notification_delivery_container
    ON notification_deliveries (container_name, queued_at DESC, id DESC);

-- Serves the retry sweep: which deliveries are due for another attempt.
-- Partial, so the index holds only the rows that could ever qualify -- the
-- overwhelming majority of deliveries are terminal and never enter it.
CREATE INDEX idx_notification_delivery_retry
    ON notification_deliveries (next_attempt_at, id)
    WHERE result = 'retrying';

-- ------------------------------------------------------- deduplication --

-- The last time each (rule, dedup key) pair was delivered.
--
-- # Why a table rather than a query over the deliveries
--
-- Because the question the suppression check asks is "when did this last go
-- out", asked once per notification per rule, and answering it by scanning a
-- growing history would put the cost of deduplication on the size of the log.
-- One row per pair, updated in place, is a point lookup forever.
--
-- Rows are pruned with the deliveries they describe.
CREATE TABLE notification_dedup (
    rule_id   TEXT NOT NULL,
    dedup_key TEXT NOT NULL,

    last_sent_at TEXT NOT NULL,

    PRIMARY KEY (rule_id, dedup_key),
    CHECK (LENGTH(dedup_key) <= 200)
);

CREATE INDEX idx_notification_dedup_age ON notification_dedup (last_sent_at);
