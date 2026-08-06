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

**Neither `diagnose` nor `backup` is reachable over HTTP, and neither ever will
be while the API is unauthenticated.** They report filesystem paths, free space,
schema history, and daemon reachability — what an operator needs and what an
attacker wants. Requiring shell access is the control.
`internal/arch/arch_test.go` fails the build if `internal/api` imports the
diagnostics package.

`diagnose` opens no Docker connection. A diagnostic that talks to a privileged
socket to answer a question about a file would be adding a capability for the
sake of a report.

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
docker ps -a --filter 'name=.hm-old-'    # originals waiting to be settled
docker ps -a --filter 'name=.hm-failed-' # replacements kept for diagnosis
```

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
   retention, image acquisition, container recreation.
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

### Shutting down mid-recreation

A container recreation is the one background task that can be holding a
container down when the signal arrives, so it gets its own discipline:

- The pipeline checks for shutdown **at every step boundary** and stops there,
  with its checkpoint current. In the common case no grace is used at all.
- A Docker call already in flight gets a **10-second grace** — under the default
  shutdown budget, deliberately, so this feature cannot be the reason a shutdown
  overruns. That is enough for the call to return and its checkpoint to land.
- The verification wait watches the shutdown signal separately from the mutation
  budget. It is entirely reads, so abandoning it changes nothing.

The result is that a recreation interrupted by a shutdown lands on a recorded
checkpoint, and the next start settles it from the table above. A shutdown mid-
recreation is therefore recoverable by an operator following a plan, rather than
a container in a state nobody wrote down.

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

Docker being unreachable is **degraded, never unhealthy**. Escalating it would
put HarborMaster into a restart loop every time the daemon restarts.

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
