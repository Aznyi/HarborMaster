# Reliability and Recovery

What HarborMaster does when storage, the daemon, or the process itself goes
wrong — and what an operator does about it.

This is the runbook. The reasoning behind each control lives in the code it
governs; this document is what you read at 3am.

---

## 1. The commands

| Command | What it does | Exit codes |
| --- | --- | --- |
| `harbormaster diagnose` | Inspects the database **read-only** and prints findings | `0` clean, `1` findings, `2` nothing could be established |
| `harbormaster backup <path>` | Writes a consistent copy and **verifies it** | `0` written and verified, `2` refused or failed |
| `harbormaster healthcheck` | Probes the live HTTP endpoint | `0` healthy/degraded, `1` unhealthy, `2` unreachable, `3` malformed |
| `harbormaster admin bootstrap` | Creates the first administrator on an unclaimed installation | `0` created, `1` refused or failed, `2` usage |
| `harbormaster admin reset-password` | Replaces an account's password, reactivates it, and ends its sessions | `0` done, `1` refused or failed, `2` usage |

**None of these is reachable over HTTP, and none ever will be.** `diagnose` and
`backup` report filesystem paths, free space, schema history, and daemon
reachability — what an operator needs and what an attacker wants. The `admin`
commands go further: they claim an installation without the one-time token and
set a password without knowing the old one. Both are correct for somebody
holding the database file and catastrophic over a network.

Requiring shell access is the control, and it is enforced structurally rather
than by convention. `internal/arch/arch_test.go` fails the build if
`internal/api` imports the diagnostics package or so much as names
`service.LocalAdmin`, the type the `admin` commands are built on. A handler
cannot call what its dependency does not declare.

`diagnose` opens no Docker connection. A diagnostic that talks to a privileged
socket to answer a question about a file would be adding a capability for the
sake of a report.

---

## 1a. Account recovery

### The situation this exists for

The only administrator has forgotten their password, or left, or their account
was disabled by mistake. Every authentication system needs an answer, and the
answers people reach for otherwise are worse: a default password, a permanent
recovery account, or a support endpoint. All three are backdoors that are always
present. A command is a backdoor available only to somebody who could already
edit the users table by hand — which is to say, not a backdoor at all.

### Claiming a fresh installation

A new installation has no accounts and no default password. It prints a
one-time token at startup:

```
  ==========================================================
   HarborMaster bootstrap token (valid until 2026-08-06T13:04:11Z)

     kUu3v8Qm1sFo2Zt9dWl4rXcB

   Use it once to create the first administrator. Restarting
   HarborMaster issues a new token and invalidates this one.
  ==========================================================
```

Open the web interface and use it. If the token was lost, restart HarborMaster
and a new one is printed; the old one stops working.

Without the web interface, from the host:

```
harbormaster admin bootstrap --username admin
```

No token is needed here, because filesystem access to the database is a stronger
proof than any token. It is still refused once an administrator exists.

### Finding out which accounts exist

```
harbormaster admin list-users
```

```
USERNAME  ROLE            STATUS    PASSWORD
hm-admin  administrator   active    set
watcher   viewer          disabled  must change at next login
```

Recovery needs a username, and before this there was no way to discover one.
During release validation the only method available was copying the SQLite
database off the host and reading it — carrying every verifier and every session
digest out of the installation to learn a string. That is a far larger thing to
hand somebody than the four columns above.

Those four columns are the whole contract: the name `reset-password` takes, what
the account may do, whether it may authenticate, and whether a previous recovery
left a password change outstanding. Nothing else — no verifier, no session
digest, no key material, no password timestamp. The summary type has four fields
and two architecture tests hold it there: one pins the field set, the other
fails the build if the HTTP layer so much as names it.

Console only, like every other command here. An installation's account list is
the first thing an unauthenticated scrape would want, and the authenticated case
is already served by the user-administration endpoints under their own
authorization.

### Recovering an account

```
harbormaster admin reset-password --username hm-admin
```

It prompts for the password twice, with echo off, and asks for confirmation
first — because the operation ends every session that account holds.

What it does:

- Replaces the password.
- **Reactivates a disabled or locked account.** The reset exists because
  somebody is locked out; leaving them locked out would make it useless. It says
  so in its output rather than doing it silently.
- **Revokes every session**, including one the operator may be holding in a
  browser. A reset whose old sessions survive has recovered nothing from an
  attacker who holds one.
- **Requires the password to change at next sign-in**, so a password typed into
  a terminal is temporary.

What it deliberately does not do: change the role. A password reset is not a
privilege grant, and a command that could quietly make an account an
administrator would be the most attractive thing on the host.

For an unattended run, `--generate` produces a strong password and prints it
once. Without that flag no password is ever printed, and there is no way to
supply one as an argument or an environment variable — both are visible in the
process list and in shell history.

### The permission check the commands make

Both refuse to run when the database or the HMAC key file is readable beyond its
owner:

```
admin: refusing to continue: harbormaster.db (0644) readable beyond the owner
    this installation holds password verifiers and session key material
    fix with: chmod 600 <file>, or pass --force to proceed anyway
```

Recovering an account into a directory anybody on the host can read is
recovering it for them too. `--force` exists because an operator locked out of a
mis-permissioned installation still needs a way in; making them type it is the
point.

On Windows the check is skipped and says so, because Go synthesises a mode from
the read-only attribute alone — a "0600, looks fine" from a synthesised value
would be a security check that lies, which is worse than none.

You should not see that refusal on a database this build created. It is what a
database from an OLDER HarborMaster looks like: see below.

### Database file permissions

HarborMaster holds its database at **0600 — owner only** and restricts it at
every start, before anything reads from it.

This corrects a real exposure. SQLite creates a database subject to the process
umask, which on an ordinary host yields **0644**: readable by every account on
the machine, holding every Argon2id verifier, every live session's keyed digest,
the bootstrap token's digest, and the security audit log. Release validation
found exactly that on a running deployment.

| File | Mode | Set by |
| --- | --- | --- |
| `harbormaster.db` | `0600` | Restricted at every open |
| `harbormaster.db-wal`, `-shm`, `-journal` | `0600` | Derived by SQLite from the database, and tightened directly if an older build left one behind |
| Backups | `0600` | `harbormaster backup`, which fails rather than leaving a readable copy |
| `snapshot-hmac.key` | `0600` | Created owner-only; an existing wider one is reported, not changed |
| The data directory | `0750` | Created if absent; never changed |

Two deliberate asymmetries:

- **The directory is not tightened.** It may be a mount point, or a directory
  the operator shares with a backup agent. Its mode is reported by
  `harbormaster diagnose` rather than changed underneath them.
- **An existing key file is reported, not tightened.** HarborMaster creates it
  `0600`, so a wider one is something a person did on purpose, and undoing an
  operator's explicit decision is different from correcting HarborMaster's own
  umask-derived default. The database gets tightened because the 0644 was
  HarborMaster's doing.

**Upgrading tightens an existing database automatically.** You will see it once:

```
WARN the database was readable beyond its owner and has been restricted
     mode=0600 filesChanged=3
     effect=any copy taken while it was readable is still exposed; rotate credentials if this host is shared
```

Read that last clause literally. Tightening the file does nothing about a copy
somebody already took. On a host where other accounts existed while the database
was `0644`, treat the verifiers as disclosed: reset passwords, which revokes
every session those accounts hold.

**If the mode cannot be established, HarborMaster does not start.** Serving
password verifiers out of a file whose exposure is unknown is the condition this
exists to end, so an unrestrictable database is a refusal rather than a warning.
A database reached through a symbolic link is refused for the same reason:
`chmod` follows symlinks, so honouring one would let whoever planted it choose
which file HarborMaster changes.

On Windows nothing is changed and `Enforced` is reported false — the mode bits
Go reports there are synthesised, and the real answer lives in an ACL
HarborMaster neither reads nor writes.

### Every console operation is audited

As coming from the local console, not from an account. No user performed it, and
attributing it to the account that was modified would be a lie the audit log
then repeats forever. "An administrator's password was replaced from the
console" is exactly the event a compromised host would produce, so it is written
down.

### What replacing the installation key costs

The key that signs snapshot digests also keys every session digest and the
bootstrap token digest.

**Replacing it signs everyone out.** That is the correct behaviour: a session
digest computed under a key that no longer exists cannot be verified, and
honouring it anyway would mean not verifying it. Nobody is locked out
permanently — accounts and passwords are unaffected — but every browser has to
sign in again.

Back the key up. See section 2.

---

## 2. Backups

### Taking one

```sh
harbormaster backup /backups/harbormaster-$(date -u +%Y%m%dT%H%M%SZ).db
```

Safe while the server is running. The command:

1. Refuses if the destination exists — one bad backup must not destroy the good
   one.
2. Refuses the live database and its `-wal`, `-shm`, and `-journal` sidecars.
3. Writes with `VACUUM INTO`, which runs inside a read transaction and produces
   a consistent, defragmented copy without an exclusive lock.
4. Sets mode `0600`. The file describes container configuration, mounts, and
   secret digests.
5. **Verifies the copy** — full `integrity_check`, `foreign_key_check`, schema
   history compared against this build, and row counts for every table that
   carries history.

A backup that fails verification is **left on disk deliberately** and reported
as failed. It is evidence; deleting it would destroy what you need to work out
what went wrong.

### Do not use `cp`

With write-ahead logging the committed state is split between the database file
and its `-wal` sidecar. A file copy taken while HarborMaster is running can
capture a database missing its most recent commits, or one that is internally
inconsistent. It will usually *seem* to work — which is the worst property a
backup procedure can have.

Copying files is safe only after a clean shutdown, which checkpoints the log.

### Restoring

There is no restore command, and that is deliberate: a restore overwrites the
operator's data, and HarborMaster does not perform destructive operations.

```sh
# 1. Stop HarborMaster.
docker compose -f deployments/compose.yaml down

# 2. Verify the backup BEFORE putting it in place, by diagnosing it where it
#    lies. diagnose opens read-only and migrates nothing, so this does not
#    alter the backup.
HARBORMASTER_DB_PATH=/backups/harbormaster-20260805T120000Z.db harbormaster diagnose

# 3. Move the damaged database aside. Do not delete it.
mv data/harbormaster.db data/harbormaster.db.damaged
rm -f data/harbormaster.db-wal data/harbormaster.db-shm

# 4. Put the backup in place and start.
cp /backups/harbormaster-20260805T120000Z.db data/harbormaster.db
chmod 0600 data/harbormaster.db
docker compose -f deployments/compose.yaml up -d
```

Step 3 moves rather than deletes. A damaged database is often still partially
readable, and it is the only copy of whatever happened between the backup and
the failure.

---

## 3. What HarborMaster checks at startup

In this order, each gating the next:

1. **Data directory** created with mode `0750`.
2. **Connection**, then **journal mode**. WAL is requested in the connection
   string; SQLite falls back to a rollback journal silently on a filesystem
   that cannot support WAL's shared-memory index. HarborMaster asks which one
   it actually got and warns — or refuses, if
   `HARBORMASTER_DB_REQUIRE_WAL=true`.
3. **Integrity**, *before* migrating. `quick_check` by default.
4. **Schema history**, then pending migrations.

### The integrity check

| Mode | What runs | Cost |
| --- | --- | --- |
| `off` | nothing | none |
| `quick` (default) | `PRAGMA quick_check` | proportional to size, no index verification |
| `full` | `PRAGMA integrity_check` + `PRAGMA foreign_key_check` | every page, every index |

**Detected damage fails closed.** HarborMaster refuses to start rather than
write more history over a malformed image — which would shorten the window in
which a backup still predates the damage.

**An incomplete check does not.** A check that timed out establishes nothing,
and turning a slow disk into an outage is worse than a late diagnosis. It is
logged, recorded in the open report, and startup continues. Bound it with
`HARBORMASTER_DB_INTEGRITY_TIMEOUT`.

Setting the mode to `off` does **not** make a damaged database openable: SQLite
refuses a malformed image itself, on the first statement. The setting governs
whether HarborMaster looks for damage proactively, not whether the engine's own
refusal is honoured.

### Schema history validation

Three states are refused, and each one means *the schema is not what this binary
believes*:

| Refusal | Cause | Remedy |
| --- | --- | --- |
| `ErrSchemaAhead` | The database records a migration this build does not contain | Run the newer HarborMaster, or restore a backup taken before the upgrade |
| `ErrMigrationChanged` | An applied migration's file has since been edited | Never edit an applied migration; add a new one |
| `ErrMigrationGap` | A migration is recorded while an earlier one is not | The bookkeeping table was hand-edited |

**In all three cases the database is not modified.** The error says so, because
the destructive instinct here — delete the database and let it rebuild — throws
away the operator's history.

**Limitation.** Migration checksums are recorded from this version onward.
Migrations applied by an earlier build carry `NULL` and are backfilled on the
first start, which means an edit made *before* the upgrade cannot be detected.
Nothing can detect it; the evidence does not exist. Edits from that point
forward are caught.

---

## 4. Crash recovery

A HarborMaster killed mid-write leaves committed transactions in the
write-ahead log. The next start replays them automatically. **No committed data
is lost, and no operator action is required.**

| What was in flight | What survives |
| --- | --- |
| An inventory refresh | The **previous** inventory, whole. A refresh commits in one transaction at a new generation; a partial one rolls back. |
| A snapshot capture | Nothing partial. The snapshot and its child rows are one transaction. |
| An event batch | Whole batches only. The unique index on the fingerprint rejects a duplicate on replay. |
| A migration | The last **complete** migration. The interrupted one is retried on the next start. |
| Event engine state | Timestamps and the reconnect count, so "has this been flapping" survives a restart. |
| An image acquisition | Nothing is resumed. An unverified transfer is failed, because the image on the host now may not be the one that pull produced. |
| **A container recreation** | The **checkpoint**, which says what was done to the host. Nothing is resumed and nothing is undone. See below. |
| **A manual rollback** | The same: its own **checkpoint**. Nothing is resumed and nothing is undone. See below. |

### A container recreation interrupted by a restart

This is the only crash that can leave something on the host rather than only in
the database, so it is worth reading before it happens.

**HarborMaster makes no Docker call during recovery.** It reads each interrupted
row's checkpoint, records the outcome as `interrupted`, and attaches a recovery
plan. Resuming would mean continuing a mutation sequence whose last step nobody
watched; undoing would mean mutating on the strength of the same uncertainty.

| Checkpoint | What is on the host | What the plan says |
| --- | --- | --- |
| *(empty, `mutatedAt` unset)* | Nothing was changed | Nothing to do |
| *(empty, `mutatedAt` set)* | **Unknown.** A stop was issued and never confirmed | Check whether the container is running; start it if not |
| `originalStopped` | The original is stopped under its own name | `docker start <name>` |
| `originalParked` | The original is stopped and renamed aside | Rename it back, then start it |
| `replacementCreated` / `replacementStarted` | Both containers exist; neither is serving | Read the replacement's logs, then rename the original back and start it |
| `replacementVerified` | The replacement is running and proved; the original is still parked | Confirm, then remove the parked original |
| `originalRemoved` | The recreation completed | Nothing to do |

The record is never pruned while it is in this state, and the list page counts
it under **Needs attention**. Find them with:

```sh
curl -s localhost:8080/api/v1/executions?needsAttention=true | jq
```

A container HarborMaster parked is identifiable on the host by its name:

```sh
docker ps -a --filter 'name=.hm-old-'        # originals waiting to be settled
docker ps -a --filter 'name=.hm-failed-'     # replacements kept for diagnosis
docker ps -a --filter 'name=.hm-rolledback-' # replacements a rollback displaced
```

### A rollback interrupted by a restart

The same discipline, the same reasoning, and the same "no Docker call during
recovery" rule. The table is different because the mutation sequence is.

| Checkpoint | What is on the host | What the plan says |
| --- | --- | --- |
| *(empty, `mutatedAt` unset)* | Nothing was changed | Nothing to do |
| *(empty, `mutatedAt` set)* | **Unknown.** A stop was issued and never confirmed | Check which container is running; the replacement may be stopped or may still be serving |
| `replacementStopped` | The replacement is stopped and still holds the production name; the original is still parked | Nothing is serving. Either start the replacement again, or rename it aside and rename the original back |
| `replacementParked` | The replacement is stopped and renamed aside; the production name is free; the original is still parked | Rename the original back and start it |
| `originalRestored` | The original holds its own name and is not running | `docker start <name>` |
| `originalStarted` | The original is running under its own name and was never proved | Confirm it is serving correctly. Service is up; only the proof is missing |
| `originalVerified` | The rollback completed | Nothing to do |

Find them with:

```sh
curl -s localhost:8080/api/v1/rollbacks?needsAttention=true | jq
```

A rollback record in that state is never pruned either.

`diagnose` reports `rows still running` if any refresh row is left in the
`running` state. It should always be zero: a refresh is recorded only when it
completes. A nonzero count means that invariant has regressed, not that data is
lost.

### Event stream recovery

Docker replays nothing across a daemon restart and keeps only a bounded ring of
recent events. On reconnect **and on restart**, HarborMaster asks the daemon to
resume from just after the last event it saw — clamped to **one hour**. An
unclamped window would turn a month-long outage into a request for the daemon's
entire ring, making the recovery path the load spike.

Every connection also requests a **full reconciliation**, because the resume is
best effort and a sweep is the only thing that can be trusted.

---

## 5. Shutdown

On `SIGINT` or `SIGTERM`, in order:

1. The HTTP server drains. In-flight requests and open SSE streams end first.
2. Background services stop: the inventory loop, the event engine, snapshot
   retention, image acquisition, container recreation, manual rollback.
3. The Docker client and the database close. The database **must** close last,
   or a final event flush would write to a closed handle.

Every wait is **bounded** by `HARBORMASTER_SHUTDOWN_TIMEOUT`.

Work already in flight gets a **grace period** rather than being cancelled
outright — a reconciliation mid-transaction should commit, not roll back for no
reason — and is then cancelled. Both halves matter: an unbounded wait means a
sweep of a thousand containers against a daemon that has stopped answering
holds the process open long past the point the runtime gives up and sends
`SIGKILL`, which is a worse ending because it happens at an arbitrary instant.

If the bound is reached, HarborMaster logs what it is abandoning at error level
and closes the database anyway. An abandoned SQLite writer's transaction is
rolled back by the database; an unbounded hang is not recoverable.

`Close` checkpoints the write-ahead log (`wal_checkpoint(TRUNCATE)`), so a clean
stop leaves no log to replay and a subsequent file copy is complete.

### Shutting down mid-recreation or mid-rollback

These are the two background tasks that can be holding a container down when the
signal arrives, so they share a discipline:

- The pipeline checks for shutdown **at every step boundary** and stops there,
  with its checkpoint current. In the common case no grace is used at all.
- A Docker call already in flight gets a **10-second grace** — under the default
  shutdown budget, deliberately, so this feature cannot be the reason a shutdown
  overruns. That is enough for the call to return and its checkpoint to land.
- The verification wait watches the shutdown signal separately from the mutation
  budget. It is entirely reads, so abandoning it changes nothing.

The result is that a recreation or a rollback interrupted by a shutdown lands on
a recorded checkpoint, and the next start settles it from the tables above.
Either is therefore recoverable by an operator following a plan, rather than a
container in a state nobody wrote down.

---

## 6. Failure conditions and what they look like

HarborMaster classifies SQLite result codes rather than message text, so each
condition reports the remedy it actually implies.

| Condition | How it presents | Remedy |
| --- | --- | --- |
| **Corrupt** | Refuses to start; `diagnose` reports critical | Restore a verified backup. A corrupt SQLite file cannot be repaired in place. |
| **Disk full** | Writes fail; the inventory keeps serving what is stored | Free space or grow the volume, then restart |
| **Read-only filesystem** | Writes fail; `diagnose` reports the data directory as not writable | Fix the mount options or the file mode |
| **Busy** | Absorbed by `HARBORMASTER_DB_BUSY_TIMEOUT`; a failure means another writer is stuck | Stop the other writer, or raise the timeout |
| **Permission** | Cannot open the database | Fix ownership of the data directory |
| **Docker unavailable** | Health reports **degraded**, not unhealthy. The API keeps serving the stored inventory; the event engine reconnects with bounded backoff | None, usually — it recovers by itself |
| **A notification destination is broken** | Its card on the Notifications page says so, and the page header counts how many are failing. Nothing else degrades | Fix or replace the destination. A revoked webhook URL cannot be repaired; create a new destination |
| **Notifications are being dropped** | Delivery records with result `dropped`, and a warning in the log naming a running total | Something raised notifications faster than they could be delivered. Raise `NOTIFICATIONS_QUEUE_SIZE` or `NOTIFICATIONS_WORKERS`, or narrow the rules |
| **HarborMaster is on an old image and never appears in a plan** | Not a failure. The Automation page names the container it excludes | Update from outside: `docker compose pull && docker compose up -d`. See [Upgrading](upgrading.md) |

Docker being unreachable is **degraded, never unhealthy**. Escalating it would
put HarborMaster into a restart loop every time the daemon restarts.

**A notification failure never escalates either**, and never affects the thing
it was reporting on. A rollback whose duration depended on somebody else's
webhook server would be a worse rollback, so delivery is asynchronous, a full
queue drops rather than blocks, and a delivery that cannot be recorded is not
attempted. The cost of that trade is that notifications can be LOST — which is
why every drop is recorded and counted rather than silently absorbed.

---

## 7. Reading a diagnosis

```
harbormaster diagnose
```

Findings are ranked, most severe first, and each carries a remedy.

| Finding | Means |
| --- | --- |
| `the data directory is not writable` | SQLite cannot write the database, its log, **or its shared-memory index** — the last of which it needs even to *read* a WAL database |
| `the journal mode is "delete" rather than WAL` | The filesystem cannot support WAL. Readers block on the writer; the crash profile differs from the documented one |
| `a write-ahead log is present with no active connection` | Normal after an unclean stop. The next start replays it. Not corruption |
| `the write-ahead log is <large>` | A reader is holding a transaction open and preventing checkpoints. Restarting checkpoints on close |
| `N of M pages are free space left by pruning` | Retention has deleted a lot. `backup` writes a compacted copy; the live file is not reclaimed automatically |
| `the database records N migration(s) this build does not contain` | A newer HarborMaster wrote this database. **Do not delete it** |
| `the database could not be opened read-only because its write-ahead log needs replaying` | A crashed process left a hot log, and replaying it is a write. Start HarborMaster once, then re-run. **Not corruption** |

That last one is a real limitation of the read-only design: after a crash,
`diagnose` can report everything the filesystem knows but cannot read inside the
database until something replays the log.

---

## 8. Settings

| Variable | Default | Effect |
| --- | --- | --- |
| `HARBORMASTER_DB_BUSY_TIMEOUT` | `5s` | Wait for another **process's** write lock. Range 100ms–5m |
| `HARBORMASTER_DB_INTEGRITY_CHECK` | `quick` | `off`, `quick`, or `full` at startup |
| `HARBORMASTER_DB_INTEGRITY_TIMEOUT` | `30s` | Bound on that check. Past it: incomplete, not damaged |
| `HARBORMASTER_DB_REQUIRE_WAL` | `false` | Refuse to start when WAL could not be enabled |
| `HARBORMASTER_SHUTDOWN_TIMEOUT` | `15s` | One budget for the HTTP drain **and** the background drain |

An unrecognised `DB_INTEGRITY_CHECK` is a startup error, not a silent fallback
to `off`. A typo must not disable a check.

---

## 9. Verifying a release

```sh
go test ./...                         # includes failure injection and recovery
go test -race ./...                   # needs cgo
go test -run FuzzOpen -fuzz FuzzOpen -fuzztime 60s ./internal/store/
```

The store suite induces **real** failures rather than mocking them:

| Condition | How it is induced |
| --- | --- |
| Disk full | `PRAGMA max_page_count`, which returns the same `SQLITE_FULL` a full volume does |
| Corruption | Overwriting content pages while leaving the header intact — what a bad sector produces |
| Not a database | Arbitrary bytes at the path |
| Busy | A second connection holding `BEGIN IMMEDIATE` with no busy timeout |
| Read-only | A `mode=ro` connection |
| Unclean stop | A handle abandoned without `Close`, leaving a hot write-ahead log |
| Interrupted migration | Applying a prefix of the set, then opening normally |
| Unwritable path | A file where a directory needs to be |
| Docker unavailable / hanging | `docker.Fake`, and a runtime whose calls block until cancelled |

The fuzz target asserts the property that matters for damaged input: whatever
bytes are at the path, `Open` either succeeds with a **usable** database or
returns an error. It never panics, never hangs, and never leaks a handle on the
failure path.
