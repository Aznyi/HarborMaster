# Upgrading HarborMaster

## The short version

```sh
docker compose -f deployments/compose.yaml pull
docker compose -f deployments/compose.yaml up -d
docker compose -f deployments/compose.yaml logs harbormaster | head -40
```

Your data is a named volume. Accounts, snapshots, drift records, policies,
plans, acquisitions, recreations, rollbacks, automation history, and the audit
log all survive.

## HarborMaster does not update itself

**This is not a limitation to be worked around; it is enforced, and no setting
disables it.**

A self-update cannot complete. The process performing the recreation is inside
the container being recreated, so it is killed between stopping the old
container and verifying the new one. Nothing checks the replacement, nothing
records the outcome, and no rollback is possible because the thing that would
perform it is gone.

So the update runs from outside — `docker compose pull`, or `docker pull`
followed by `docker rm` and a fresh `docker run`. Both leave the data volume
alone.

HarborMaster will still tell you an update EXISTS: it appears in Updates and in
Change plans like any other container. What it will not do is act on it.
Acquisition and recreation refuse at four independent layers, and the
Automation page names the container it is excluding so the refusal is visible
rather than mysterious.

If HarborMaster cannot work out which container it is — a host PID namespace, a
custom hostname, and no label all at once — set
`HARBORMASTER_SELF_CONTAINER_ID` to the full 64-character id. A wrong value
makes HarborMaster decline to update some other container, which is a smaller
harm than the reverse.

## Before you upgrade

### Take a backup

```sh
docker compose -f deployments/compose.yaml exec harbormaster \
  harbormaster backup /var/lib/harbormaster/backup-$(date +%F).db
```

`backup` takes a consistent copy while the server is running and then verifies
it. It is a command rather than an endpoint, deliberately: it reports
filesystem paths and writes a complete copy of the database, and no HTTP
surface offers either.

There is **no scheduled backup**. Nothing in HarborMaster takes one on a timer.

### Read the release notes

Every release records what changed, what is new, and anything that needs an
operator to act. Migrations run automatically; a release that needs more than
that says so at the top.

## What happens on the first start after an upgrade

1. **The database is opened and validated.** A quick integrity check runs by
   default; `HARBORMASTER_DB_INTEGRITY_CHECK=full` is slower and more thorough.
2. **Migrations are applied in order, inside a transaction each.** Each is
   recorded with the SHA-256 of its contents.
3. **The recorded checksums are verified.** A migration whose file changed
   since it was applied refuses the open, because that means the schema in the
   database is not the schema the code expects.
4. **A gap in the history refuses the open** for the same reason.
5. **A database written by a NEWER build refuses to open.** Downgrading is not
   supported: the older code does not know about the newer columns, and running
   it would either fail confusingly or write rows the newer build cannot read.

Every one of these is a refusal rather than a repair. A database HarborMaster
cannot vouch for is one it will not write to.

## Downgrading

**Not supported.** Restore the backup you took before upgrading, and run the
older image against that.

## Upgrading a deployment that skipped several releases

Supported, and tested. `TestEverySchemaVersionUpgradesToCurrent` applies the
migrations up to each previous schema version in turn, writes data at that
version, and then upgrades the rest of the way — one case per version rather
than one case for the latest.

## After the upgrade

Check the log:

```sh
docker compose -f deployments/compose.yaml logs harbormaster | head -40
```

You are looking for four lines:

- `applied database migrations` — which ones ran.
- `database integrity verified` — the check passed.
- `docker api version negotiated` — the daemon is reachable and which API
  version was settled on.
- `identified HarborMaster's own container` — the self-update exclusion knows
  what it is excluding. Its absence is not an error; it means HarborMaster is
  running outside a container, or that every probe failed, and the refusal
  still holds at the other three layers.

Then open Settings. It reports which capabilities the running process has,
which is how you confirm an upgrade did not quietly change what your deployment
can do.

## Upgrading to v0.9.0-beta.1 specifically

Nothing to do beyond the steps above. Two things are worth knowing:

- **Notifications are new and off.** Nothing is sent until
  `HARBORMASTER_NOTIFICATIONS_ENABLED=true` and an administrator has created a
  destination and a rule. Setting the variable alone sends nothing.
- **Two new permissions exist**, `notification:read` and
  `notification:manage`. They are added to the existing roles automatically:
  every role can read the delivery history, and only an administrator can
  configure destinations. No account changes.
