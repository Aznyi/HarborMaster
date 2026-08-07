# HarborMaster

Safety-first container lifecycle manager.

HarborMaster watches the containers you already run and gives you the things
you need before you change one: an accurate inventory, a configuration snapshot
you can compare against, a health verdict you can trust, and a record of what
happened. The eventual goal is image updates with validation and rollback. The
order matters — the observation and recovery machinery is built first, so that
by the time HarborMaster can change a container it can already undo it.

> **Observation, plus one narrow write.** HarborMaster inventories the
> containers, images, networks, and volumes on a Docker host, reports their
> normalized configuration, and subscribes to the Docker event stream to keep
> that inventory current. Watching events does not change anything: an event
> only ever causes HarborMaster to re-read the host.
>
> It has exactly three abilities to change the host, all **off by default**:
>
> 1. **Downloading an approved, digest-pinned image** into the local image
>    store, verified afterwards. It touches no container. See
>    [Safe image acquisition](#safe-image-acquisition).
> 2. **Recreating ONE container** on an image it already downloaded and
>    verified, preserving that container's configuration. The original is kept
>    until the replacement is proved, and there is **no automatic rollback**.
>    See [Manual container recreation](#manual-container-recreation).
> 3. **Rolling ONE recreation back**, when a person asks. It stops the
>    replacement, starts the original that recreation preserved, and proves it.
>    It **removes nothing** — the replacement is kept as evidence. See
>    [Manual rollback](#manual-rollback).
>
> Those three are the whole of it. **Who asks for them** is now two things
> rather than one: a person, or an update policy on a schedule — see
> [Automatic updates](#automatic-updates), also off by default, and refused
> unless both recreation and rollback are on. Automation adds no fourth ability:
> it submits the same three requests to the same three services, each of which
> re-runs its full preflight against the live host.
>
> Everything else remains absent: no fleet management, no restore from a
> snapshot, no image deletion or pruning, and no command-execution path of any
> kind. See [Status](#status) for what is and is not here yet.
>
> **Every route requires a session**, and which routes an account can reach
> depends on its role. There is no setting that turns that off. A fresh
> installation has no accounts and no default password: it prints a one-time
> token at startup and you claim it. See [Accounts and access](#accounts-and-access).

## Contents

- [Quick start](#quick-start)
- [Deploying on Linux](#deploying-on-linux)
- [Accounts and access](#accounts-and-access)
- [Container image](#container-image)
- [Inventory](#inventory)
- [Docker events](#docker-events)
- [Configuration drift](#configuration-drift)
- [Policy engine](#policy-engine)
- [Image intelligence](#image-intelligence)
- [Change planning](#change-planning)
- [Safe image acquisition](#safe-image-acquisition)
- [Manual container recreation](#manual-container-recreation)
- [Manual rollback](#manual-rollback)
- [Automatic updates](#automatic-updates)
- [Reliability and recovery](#reliability-and-recovery)
- [Architecture](#architecture)
- [Configuration](#configuration)
- [API](#api)
- [Web interface](#web-interface)
- [Development](#development)
- [Security](#security)
- [Status](#status)
- [License](#license)

## Quick start

### Docker Compose

```sh
cp .env.example .env
# Set HARBORMASTER_DOCKER_GID to the host's docker group so the unprivileged
# container can read the socket:
#   stat -c '%g' /var/run/docker.sock
docker compose -f deployments/compose.yaml up -d
```

Open <http://127.0.0.1:8080>.

To build from this checkout instead of pulling the published image, layer the
build override on top:

```sh
docker compose -f deployments/compose.yaml -f deployments/compose.build.yaml up --build
```

### From source

Requires Go 1.25+ and Node 20+.

```sh
make build   # compiles the frontend, then embeds it in ./bin/harbormaster
./bin/harbormaster
```

The server binds to `127.0.0.1:8080` by default and will start whether or not
the Docker socket is reachable — an absent socket is reported as a degraded
state, not a startup failure.

### Frontend development

Run the backend and the Vite dev server side by side. Vite proxies `/api` to
the backend, so the frontend uses the same relative URLs it will use in
production.

```sh
make run      # terminal 1: backend on 127.0.0.1:8080
make web-dev  # terminal 2: Vite on 127.0.0.1:5173
```

## Deploying on Linux

> ### Read this before you deploy
>
> **Access to the Docker socket is equivalent to root on the host.** A process
> that can talk to `/var/run/docker.sock` can start a container that mounts the
> host filesystem, and from there it owns the machine. Anyone who can reach
> HarborMaster's port is one application bug away from that.
>
> Mounting the socket `:ro` makes the *socket file* read-only. It does **not**
> make the Docker API read-only — the API is a request/response protocol over
> that socket, and a read-only bind mount still permits the writes a request
> needs. What keeps this deployment read-only is the application: HarborMaster
> only calls the Engine's ping endpoint. The `:ro` flag is defence in depth,
> not a boundary.
>
> **HarborMaster authenticates every route, but it does not terminate TLS.**
> Publish it to `127.0.0.1` and reach it over SSH or a VPN, or put a
> TLS-terminating reverse proxy in front of it — a session cookie is only as
> safe as the transport carrying it, and the `__Host-` cookie prefix engages
> only over HTTPS. Every command below binds to loopback for exactly this
> reason. When you do put a proxy in front, set `HARBORMASTER_COOKIE_SECURE=true`
> and `HARBORMASTER_TRUSTED_PROXIES`; see
> [Accounts and access](#accounts-and-access).

### 1. Find the Docker socket's group ID

The container runs as UID 65532, which needs the socket's group to read it.
Read the numeric GID off the socket itself rather than guessing:

```sh
DOCKER_GID="$(stat -c '%g' /var/run/docker.sock)"
echo "$DOCKER_GID"
```

If `stat` is unavailable or uses BSD syntax (macOS, some minimal images), any of
these produce the same number:

```sh
getent group docker | cut -d: -f3     # if the group is named "docker"
ls -n /var/run/docker.sock | awk '{print $4}'
stat -f '%g' /var/run/docker.sock     # BSD stat
```

Pass the number directly if you already know it — `--group-add 999`, for
example. Do not assume 999: it varies by distribution and by how Docker was
installed.

### 2. Create the volume and pull the image

```sh
docker volume create harbormaster-data
docker pull ghcr.io/aznyi/harbormaster:latest
```

### 3. Start it

```sh
docker run -d \
  --name harbormaster \
  --restart unless-stopped \
  --user 65532:65532 \
  --group-add "$DOCKER_GID" \
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid,size=16m \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  -p 127.0.0.1:8080:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v harbormaster-data:/var/lib/harbormaster \
  ghcr.io/aznyi/harbormaster:latest
```

What each hardening flag buys you:

| Flag | Effect |
| --- | --- |
| `--user 65532:65532` | Runs as the image's non-root user. Also the default; stated explicitly so it survives a base-image change. |
| `--group-add "$DOCKER_GID"` | The only privilege granted: permission to read the Docker socket. |
| `--read-only` | The root filesystem cannot be written, so nothing can drop a binary into the container. |
| `--tmpfs /tmp:...,size=16m` | The one writable scratch area, capped, `noexec` and `nosuid` so nothing placed there can run. |
| `--cap-drop ALL` | Removes every Linux capability. HarborMaster needs none. |
| `--security-opt no-new-privileges:true` | Blocks privilege escalation through setuid binaries. |
| `-p 127.0.0.1:8080:8080` | Publishes 8080 on loopback only. `-p 8080:8080` would publish on **every** interface — do not use it without TLS in front. |
| `-v harbormaster-data:/var/lib/harbormaster` | Keeps the SQLite database outside the container, so upgrades preserve history. |

### 4. Operate it

```sh
# Follow the logs
docker logs -f harbormaster

# Health, from the outside
curl -s http://127.0.0.1:8080/api/v1/health

# Health, as the container runtime sees it
docker inspect --format '{{.State.Health.Status}}' harbormaster

# Run the built-in check by hand (no shell or curl inside the image)
docker exec harbormaster /usr/local/bin/harbormaster healthcheck

# Inspect the database and report reliability findings (read-only, no Docker)
docker exec harbormaster /usr/local/bin/harbormaster diagnose

# Take a consistent, verified backup while the server is running
docker exec harbormaster /usr/local/bin/harbormaster backup \
  /var/lib/harbormaster/backup-$(date -u +%Y%m%dT%H%M%SZ).db

# Build metadata
curl -s http://127.0.0.1:8080/api/v1/version

# Stop, start, remove
docker stop harbormaster
docker start harbormaster
docker rm -f harbormaster
```

Removing the container leaves the named volume alone. To delete the data too:

```sh
docker volume rm harbormaster-data
```

A `degraded` health status means HarborMaster is running but cannot reach the
Docker socket — usually a wrong `--group-add` GID. The container stays healthy
from the runtime's point of view, because a missing socket is a condition to
report, not a reason to restart the process.

### 5. Upgrade, preserving data

The named volume is what carries the database across the replacement:

```sh
docker pull ghcr.io/aznyi/harbormaster:latest
docker stop harbormaster
docker rm harbormaster
# Re-run the `docker run` command from step 3 unchanged.
```

The new container mounts the same `harbormaster-data` volume, so snapshots and
event history survive. Roll back by re-running with the previous digest.

### Pinning a digest

`latest` is a moving tag. It is not immutable: the same name points at different
images over time, and pulling it twice can give you two different builds. For
anything you need to reproduce, pin the digest:

```sh
# Record the digest you are running
docker inspect --format '{{index .RepoDigests 0}}' ghcr.io/aznyi/harbormaster:latest

# Deploy that exact image
docker run -d --name harbormaster \
  ... same flags as above ... \
  ghcr.io/aznyi/harbormaster@sha256:<digest>
```

A digest reference always resolves to the same bytes. Each published build's
digest appears in the container workflow's job summary, and the image carries a
signed provenance attestation you can verify:

```sh
gh attestation verify oci://ghcr.io/aznyi/harbormaster:latest -R Aznyi/HarborMaster
```

## Accounts and access

Every route in HarborMaster requires a session except four, and there is no
setting that turns that off. HarborMaster fronts a root-equivalent Docker socket
and can replace a running container; an "auth off for convenience" switch is a
switch that ends up on in production.

### The first sign-in

A fresh installation has no accounts and no default password. It prints a
one-time bootstrap token at startup:

```
  ==========================================================
   HarborMaster bootstrap token (valid until 2026-08-06T13:04:11Z)

     kUu3v8Qm1sFo2Zt9dWl4rXcB

   Use it once to create the first administrator. Restarting
   HarborMaster issues a new token and invalidates this one.
  ==========================================================
```

Open the web interface, paste the token, and choose a username and password.
That is the whole of it.

**Why a token rather than a default account.** Without one, claiming a brand-new
installation is a race won by whoever reaches the port first, which on an
exposed port is not you. The token moves the requirement from "be first" to "can
read the server's log", which is the same bar the rest of the deployment already
assumes. A default account, meanwhile, is how appliances end up on the internet
with `admin`/`admin`.

Lost the token? Restart HarborMaster — a new one is printed and the old one
stops working. Or claim it from the host, where no token is needed because
filesystem access to the database is a stronger proof:

```sh
docker exec -it harbormaster /harbormaster admin bootstrap --username admin
```

Once an administrator exists, the bootstrap endpoint answers `404`. For that
installation it genuinely no longer exists.

### Roles

Three, and each holds a fixed set of permissions.

| Role | Can | Cannot |
| --- | --- | --- |
| **viewer** | Read everything: inventory, events, snapshots, drift, compliance, plans, acquisitions, recreations | Change anything at all |
| **operator** | Everything a viewer can, plus: refresh the inventory, capture snapshots, annotate drift and violations, request a compliance pass, refresh registry metadata, generate plans, **acquire images**, and **recreate containers** | Edit or withdraw a policy; manage accounts; read the security audit log |
| **administrator** | Everything | — |

**Policy administration is deliberately not an operator permission.** A policy
is what blocks an acquisition or a recreation, so an operator able to edit one
could remove the gate standing in their way.

`GET /api/v1/roles` returns the catalogue with each role's exact permission
list, and the web interface builds its role picker from it — so the picker and
the authorization middleware read the same source of truth.

### What a session is

An opaque token in an `HttpOnly`, `SameSite` cookie. It appears in no response
body, no URL, and no log line, and no script on the page can read it. The
database stores only a keyed digest, so a copy of the database yields the
ability to verify a token somebody already holds and nothing else.

Sessions end on sign-out, on a password change, on a role change, on
disablement, when they go idle, at a hard ceiling regardless of use, and when
the per-account cap supersedes the oldest. The account behind a session is
re-read on **every** request, which is what makes a demotion or a disablement
take effect immediately rather than at the next sign-in.

The live event stream is the one response that outlives a single request, so it
re-checks the session and the permission on **every heartbeat** and closes as
soon as either stops holding. Revoking a session stops the flow of estate data
within one heartbeat, not at the end of the session's seven-day ceiling.

Every state-changing request additionally carries a CSRF token in the
`X-HarborMaster-CSRF` header. It is derived from the session token rather than
stored, so it rotates with the session and there is nothing at rest to steal.

### Passwords

Argon2id, at the OWASP baseline of 64 MiB, 3 iterations, 4 lanes — tunable, and
stored alongside each hash so raising the cost re-hashes people as they sign in
rather than invalidating every password at once.

The policy is length and a refusal list, not a character-class mixture:
twelve characters minimum, not one of the commonly-chosen values, not containing
the username, and not a single repeated character or a simple run. Composition
rules push people toward `Password1!`, which is predictable and no stronger than
a longer passphrase.

**Every sign-in failure looks and takes the same.** An unknown username, a wrong
password, and a disabled account all answer `401` with the same message, and the
timing matches because an unknown username is verified against a decoy
credential. Repeated failures apply an exponential backoff to a bounded ceiling,
not a lockout — a lockout lets anyone who knows a username deny that account
service.

### Managing accounts

An administrator creates accounts under **Accounts** in the web interface.
HarborMaster generates the password and shows it **once**; it is stored only as
a verifier and cannot be retrieved again. The account must replace it at first
sign-in.

There is deliberately **no delete**. Disabling preserves the history an audit
record depends on, and an account that never existed is a different fact from
one that was turned off. Disabling ends every session on the account
immediately.

Two refusals are built in and are not configurable:

- An administrator cannot change their own role or disable themselves. The one
  legitimate case — stepping down — is better done by another administrator.
- The last active administrator cannot be demoted or disabled. The check runs
  inside the transaction that performs the change, so two concurrent demotions
  cannot both succeed.

### Recovering an account

From the host, when nobody can sign in:

```sh
docker exec -it harbormaster /harbormaster admin reset-password --username admin
```

It prompts twice with echo off, confirms first (because it ends every session
that account holds), reactivates a disabled account, and requires the password
to change at next sign-in. It cannot change a role: a password reset is not a
privilege grant.

It refuses to run against a database or key file readable beyond its owner
unless you pass `--force` — recovering an account into a directory anybody on
the host can read is recovering it for them too.

**These commands are never reachable over HTTP.** They claim an installation
without the token and set a password without knowing the old one; both are
correct for somebody holding the database file and catastrophic over a network.
An architecture test fails the build if the API package so much as names the
type they are built on.

### The security audit log

Under **Security audit**, for administrators. Every authentication attempt,
every authorization denial, and every state-changing request, with the account,
role, session, request id, and source address that caused it.

A record is who, what, to what, from where, and whether it worked. **It is not a
request log**: no body, no header, no environment value, and no credential
appears in any record, and no column exists that could hold one. Records are
append-only — no endpoint edits or deletes one.

**The two operations that change the Docker host record their outcome as well as
their request.** A request can be refused by the second preflight, cancelled
before anything moves, expire in the queue, or fail partway — so
`execution.requested` and `execution.completed` are separate rows, and the
failure row says whether the host was left changed. That last part is what
decides whether somebody has to go and look.

The completions are what the page counts as host changes and what appears in the
log at `WARN`. A request that came to nothing is not a host change. Security records are retained
far longer than operational ones, because an inventory refresh from six months
ago is noise while a failed sign-in from six months ago is the first entry in a
story.

**The source address is the transport peer**, not a header. `X-Forwarded-For`
and `Forwarded` are ignored entirely unless you configure
`HARBORMASTER_TRUSTED_PROXIES` and the request arrives from inside one of those
ranges — a forwarding header is attacker-controlled text, and believing it
unconditionally would let anyone spoof the source in the audit log.

### Behind a reverse proxy

Set two things:

```sh
HARBORMASTER_COOKIE_SECURE=true          # the browser's connection is HTTPS
HARBORMASTER_TRUSTED_PROXIES=10.0.0.0/8  # only the ranges your proxy uses
```

`COOKIE_SECURE` matters when the proxy terminates TLS and forwards over plain
HTTP: the browser's connection was HTTPS and the cookie must be marked to match.
Setting it on a genuinely plain-HTTP deployment makes the browser discard the
cookie and nobody can sign in — a loud failure rather than a silent weakening.

Over HTTPS the cookie is also named `__Host-harbormaster_session`. That prefix
is a browser-enforced guarantee rather than a convention: it stops a sibling
subdomain overwriting the session cookie, which is the standard session-fixation
route.

## Container image

Published to `ghcr.io/aznyi/harbormaster` for `linux/amd64` and `linux/arm64`.

| Event | Tags published |
| --- | --- |
| Pull request | none — built and smoke-tested only |
| Push to `main` | `edge`, `sha-<full commit sha>` |
| Release `v0.1.0` | `0.1.0`, `0.1`, `0`, `latest` |
| Manual dispatch | none, unless `push_image` is set, and then only `dispatch-<sha>` |

`latest` moves on releases and nowhere else, so merging to `main` can never
replace the tag a production host is pulling.

The runtime layer is `gcr.io/distroless/static-debian13:nonroot`: no shell, no
package manager, no interpreter, and no curl or wget. The binary is static
(`CGO_ENABLED=0`), built with `-trimpath` and `-s -w`. Because there is nothing
in the image to execute, the container health check is a subcommand of the
binary itself:

```sh
harbormaster healthcheck
```

It probes the local health endpoint and exits `0` for `healthy` **and** for
`degraded`, `1` for `unhealthy`, `2` when the endpoint is unreachable, and `3`
when the response is not a health report. Degraded exits zero deliberately: an
unreachable Docker socket must not put the container into a restart loop.

Build and test the image locally:

```sh
make docker-build   # build harbormaster:dev
make docker-smoke   # build and run deployments/smoke-test.sh against it
```

## Inventory

HarborMaster reads the local Docker host and stores a normalized picture of it.

### The observation guarantee

`internal/docker` is the only package that talks to the Docker Engine.

The interface every service receives, `docker.Runtime`, exposes seven
operations: `Ping`, `ListContainers`, `InspectContainer`, `InspectImage`,
`ListNetworks`, `ListVolumes`, and `StreamEvents`. Every one is an observation,
and its exact method set is pinned by a test. There is no accessor that hands
the underlying SDK client to another package.

Two capabilities sit outside it, each on its own interface, each held by exactly
one service, and each `nil` unless the deployment opts in:

| Interface | Methods | Held by |
| --- | --- | --- |
| `docker.ImageAcquirer` | 1: pull a digest-pinned image | the acquisition service |
| `docker.ContainerMutator` | 5: create, start, stop, rename, remove | the execution service |

Capability is granted by what a constructor is handed. The inventory, drift,
policy, planning, and image-intelligence services all receive `Runtime` and
therefore cannot change anything, and architecture tests fail the build if any
package outside a capability's owner so much as names it — `internal/api`
included.

The inventory itself is unchanged: `POST /api/v1/inventory/refresh` re-reads the
host and replaces HarborMaster's own records, and changes nothing on the Docker
host.

### How a refresh works

1. Ping the Engine. A failure here fails the refresh; nothing else is attempted.
2. List every container, **including stopped ones**.
3. Inspect each container with bounded concurrency (`INVENTORY_WORKERS`).
4. Resolve each distinct image exactly once — a host with 200 containers on 5
   images performs 5 image inspections, not 200.
5. List networks and volumes for metadata.
6. Compute the inventory checksum.
7. Persist everything in a single transaction.

Failures are graded rather than uniform. Only a failure to *list* containers
fails a refresh. A container that vanishes between being listed and inspected
is recorded from summary data with a `container_vanished` warning — routine
churn on a busy host, not a fault. An image removed while a container still
uses it produces `image_unavailable`. One unreadable container never costs you
the other nine hundred.

If a refresh fails, the previously persisted inventory stays intact and keeps
being served. The generation number advances only when a refresh completes
**and** commits, so a client can always tell whether what it holds is current.

### Refresh triggers

| Trigger | When |
| --- | --- |
| `startup` | Once at boot, unless `INVENTORY_REFRESH_ON_STARTUP=false` |
| `periodic` | Every `INVENTORY_REFRESH_INTERVAL`; set to `0` to disable |
| `manual` | The Dashboard's **Refresh inventory** button, or `POST /api/v1/inventory/refresh` |

Overlapping refreshes are **refused, not queued**. A manual refresh while one
is running returns `409` with the active refresh's start time. Queuing would
mean three clicks became three sequential sweeps of a privileged socket.

Manual refresh is **asynchronous** and returns `202`. A sweep of a thousand
containers cannot be something an HTTP client waits on. Poll
`GET /api/v1/inventory` and watch `inProgress` and `generation`.

### Environment masking

Environment variables and log-driver options whose **name** matches a
configured pattern are masked as `********` before they leave the Docker
adapter. The defaults are `PASSWORD`, `PASSWD`, `SECRET`, `TOKEN`, `API_KEY`,
`APIKEY`, `PRIVATE_KEY`, `CREDENTIAL`, and `AUTH`.

Matching is on the name, never the value. Scanning values for things that look
like secrets produces false positives on any long random string and false
negatives on short passwords. The bias is towards over-masking: `AUTHOR`
matches `AUTH` and gets masked, because a masked non-secret is an annoyance
while a leaked secret is not recoverable.

**Raw values are never written to disk.** The persisted configuration holds the
masked form. There is no API parameter, header, or endpoint that reveals a real
value, and no UI control to unmask one. Such a switch would be a
secret-disclosure API gated on nothing but a session, and a session is a much
lower bar than the values behind it deserve.

Raw values do exist in memory during a refresh, where they contribute a
SHA-256 hash to the inventory checksum. That is what lets a rotated password
change the checksum without the checksum containing the password.

### Inventory checksum

A deterministic SHA-256 over the whole inventory. Two refreshes of an unchanged
host produce the same checksum, so a client can tell "nothing changed" from
"changed" without diffing.

**Included:** container identity, image reference and ID, normalized state,
health, exit code, restart count and policy, Compose and HarborMaster metadata,
ports, labels, mounts, network attachments, process configuration, healthcheck
configuration, resources, security posture, logging configuration, and the
environment (sensitive values as hashes). Then the sets of image IDs, network
IDs, and volume names.

**Excluded:** refresh timestamps, duration, generation, and trigger; container
created/started/finished and first/last-seen timestamps; the volatile status
text ("Up 3 minutes"); and healthcheck result timings. These describe the
observation, not the thing observed.

Collections whose order carries no meaning — ports, labels, capabilities,
network aliases — are sorted before hashing. Environment order is preserved,
because it is meaningful to some programs.

This is **not** the configuration-snapshot checksum from migration `0001`, and
the two must not be conflated: a snapshot checksum fingerprints one container's
captured configuration for rollback; this fingerprints the whole current
inventory for change detection.

### What is stored

Migration `0002_inventory.sql` adds `hosts`, `inventory_refreshes`, `images`,
`networks`, `volumes`, `containers`, `container_config`, `container_labels`,
`container_networks`, `container_mounts`, and `inventory_warnings`.

Containers that disappear are marked absent rather than deleted, so warnings
and observed lifetimes survive the container being removed. They are purged
after `INVENTORY_ABSENT_RETENTION`.

The redacted raw inspection payload is stored separately from the normalized
fields and served only from `/api/v1/containers/{id}/raw`. It is genuinely
useful for troubleshooting — it carries fields HarborMaster has not normalized
yet — but it is **not** a faithful copy: redacted values are gone, not
recoverable, so it cannot recreate a container exactly.

## Docker events

HarborMaster subscribes to the Docker daemon's event stream so the inventory
tracks the host in near real time instead of waiting for the next sweep.

### Events are hints, not authoritative state

This is the rule the whole subsystem is built around, and it is worth stating
plainly because it is easy to assume the opposite:

> **A Docker event says a resource MAY have changed. It never says what the
> resource now is, and HarborMaster does not believe it if it did.**

Every event triggers a **re-inspection** through the same pipeline a full
refresh uses — the same normalization, the same masking, the same transaction
rules. Nothing parses an event payload into container state. So there is exactly
one path by which a container becomes rows in the database, regardless of what
triggered the write.

The reason is not purity. Docker promises far less about its event stream than
it appears to:

- Events emitted while nothing is listening are **gone**. There is no durable log.
- Nothing is replayed across a **daemon restart**.
- Ordering holds only **within one connection**.
- **Duplicates** are permitted.
- Some daemon versions emit only whole-second timestamps, and some omit actions
  others send.

An inventory reconstructed from that stream would drift and never notice. One
rebuilt by inspection cannot.

### What the engine does

```
Docker event stream
   │  (subscribe; reconnect with bounded backoff + jitter)
   ▼
normalize + redact          ← internal/docker, the only place that sees SDK types
   │
   ▼
deduplicate (fingerprint)   ← bounded in-memory window + a UNIQUE column
   │
   ▼
persist in batches          ← docker_events, monotonic local sequence
   │
   ├──► SSE subscribers     ← live UI, redacted, non-blocking
   │
   ▼
classify → debounce/coalesce
   │
   ├──► targeted refresh    ← re-inspect ONE container/image/network/volume
   └──► full reconciliation ← the Phase 2 refresh pipeline, unchanged
```

### Reconnection

A dropped stream is expected — a daemon restart, a socket hiccup, an upgrade.
The reader reconnects with **bounded exponential backoff and jitter**: the delay
starts at `EVENTS_RECONNECT_INITIAL_DELAY`, multiplies by
`EVENTS_RECONNECT_MULTIPLIER` after each consecutive failure, is capped at
`EVENTS_RECONNECT_MAX_DELAY`, and is spread over 50–100% of its nominal value so
several HarborMasters that lost the same daemon do not reconnect in lockstep.

A connection that stays up for the maximum-delay duration **resets** the
backoff, so a daemon that flaps hourly does not stay parked at the ceiling.

Shutdown never waits out a backoff: the delay is a timer inside a `select`, so
cancellation returns immediately rather than sixty seconds later.

**Every reconnect also triggers a full reconciliation.** Whatever happened while
disconnected produced no event HarborMaster saw, and the daemon's own replay is
bounded and unreliable, so a sweep is the only honest way to be sure.

A daemon that is down at startup is not a startup failure. HarborMaster serves
the last inventory it stored, reports `degraded`, and keeps retrying.

### Reconciliation

Periodic full reconciliation stays on **even when events flow perfectly**, for
all the reasons above.

**Exactly one component owns the periodic full sweep.** While the event engine
runs it owns reconciliation at `EVENTS_RECONCILE_INTERVAL` (15m by default), and
`INVENTORY_REFRESH_INTERVAL` is suppressed. Turn the engine off and the
inventory's own ticker takes over again, unchanged — an existing Phase 2
configuration keeps working exactly as it did. Two independent full-refresh
timers would double the load on a privileged socket and make "when was the last
sweep" ambiguous.

Reconciliation is also requested on demand when:

- the event stream reconnects,
- the daemon reports a configuration reload,
- an event cannot be mapped onto a resource,
- a targeted refresh fails,
- the processing queue overflows.

A reconciliation is recorded with the `reconcile` trigger, distinct from
`periodic`, so an operator can tell a routine sweep from one the engine
escalated to — the difference between "all is well" and "events were missed".

### Deduplication and debouncing

Each event carries a deterministic **fingerprint** derived from the host, type,
action, actor, scope, and the daemon's nanosecond timestamp. A duplicate
delivery inside `EVENTS_DEDUP_WINDOW` is counted and discarded. A genuinely
repeated action at a different instant has a different fingerprint and is **not**
suppressed — suppressing a real second `start` would lose a state transition.
The window is bounded in memory and expires; a `UNIQUE` constraint on the column
catches anything that outlives it, including across a restart.

One lifecycle transition emits a burst — `docker restart` produces kill, die,
stop, start, and usually a health_status within a second or two. A debounce
window (`EVENTS_REFRESH_DEBOUNCE`) coalesces them into **one** refresh per
resource. There is one worker and one timer for the whole scheduler: no
goroutine per event, no timer per event, and the pending set is capped.

When the queue or the pending set is full, HarborMaster records a warning,
increments the drop counter, and requests a full reconciliation. It never blocks
the stream reader — a blocked reader stalls the daemon's event dispatch and
fails silently.

### Retention

Event history is observational and would otherwise grow without limit. It is
pruned by age (`EVENTS_RETENTION_AGE`) and by row count
(`EVENTS_RETENTION_COUNT`), in bounded batches so a large backlog does not hold
one long write lock. Pruning touches **only** the event table — it can never
remove a current inventory record.

There is deliberately **no destructive API endpoint** for pruning in this phase.
An endpoint that deletes history would be exactly the wrong
thing to add first.

### Live updates (Server-Sent Events)

`GET /api/v1/events/stream` is a `text/event-stream` endpoint carrying events as
they are persisted. SSE rather than WebSockets: the traffic is one-way, SSE is
plain HTTP so it inherits the server's timeouts, security headers, and access
log unchanged, and the browser reconnects on its own.

- Frames are named: `docker-event`, `ready`, `replay-truncated`, plus comment
  heartbeats.
- Each event's SSE `id:` is its local sequence, which the browser echoes back as
  `Last-Event-ID` on reconnect.
- Replay from `Last-Event-ID` reads from SQLite and is **capped**. A client that
  fell further behind gets a `replay-truncated` frame saying how many were
  skipped, and should reload the paginated history rather than expect the stream
  to catch it up.
- Concurrent subscribers are capped; over the limit the endpoint answers `503`
  with `Retry-After`.
- A subscriber that stops reading has events dropped **for it alone**. It never
  blocks event processing or back-pressures the Docker stream.

The endpoint carries only
already-redacted event data and never a raw Docker payload.

### Redaction

Actor attributes are a resource's labels plus daemon-supplied fields — arbitrary
operator-supplied key/value pairs that can and do carry credentials. Any value
whose **key** matches a sensitive-name pattern (`PASSWORD`, `SECRET`, `TOKEN`,
`API_KEY`, `AUTH`, …) is replaced with `********`.

Redaction happens in the Docker adapter, **before** an event is logged,
persisted, returned by the API, or written to the SSE stream. There is no
unredacted copy anywhere, and no switch to reveal one. Docker registry
authentication is never stored.

Non-sensitive structural metadata is preserved, because inventory correlation
depends on it.

### Live UI

The Events page shows two views over the same data, and the distinction matters:

- **Live** — what has arrived over SSE since the page opened. Bounded at 250
  rows; not a complete record.
- **History** — the server-side paginated, filtered query. The complete record
  of what HarborMaster observed.

Both show **two timestamps per event**: the Docker time and HarborMaster's
observed time. They differ by the stream latency normally and by a great deal
after a reconnect, and that gap is exactly what an operator needs to see.

Pausing the live view stops the list moving but keeps the connection open, so
resuming does not trigger a reconnect and replay.

### Events tables

Migration `0003_events.sql` adds `docker_events` and `event_engine_state`, and
widens the `inventory_refreshes` trigger constraint to accept `reconcile`.

`docker_events` is distinct from the `events` table in `0001`, and the two must
not be conflated: that one is HarborMaster's audit log of its **own** actions,
this one records what the **Docker daemon** reported.

## Configuration drift

HarborMaster compares each container's **current** configuration against its
**baseline snapshot** and records every field that differs.

**Drift is an observation.** HarborMaster reports that something moved and has
no capability to move it back — no remediation, no rollback, no apply, and no
route behind any of this that reaches the Docker socket.

### It reuses the diff engine rather than reimplementing it

Detection runs the same comparison as `GET /snapshots/{id}/diff?against=current`
and then **classifies** the result. Nothing in the drift engine compares
anything itself. A second comparison implementation would eventually disagree
with the first, and the first is the one whose determinism the snapshot
checksum depends on.

The baseline is the container's most recent snapshot — already the one
retention never prunes, described as the restore baseline. Drift needs no
baseline *selection* mechanism as a result, which also means no caller-supplied
snapshot id in the evaluation path.

### Severity follows the blast radius, not the field

Ranking answers one question: *how much worse is the host now?* **Direction
decides it.**

| | |
|---|---|
| **Critical** | A lost containment boundary — privileged enabled, read-only rootfs disabled, a capability added, a security option removed, a host namespace shared. Also an image **digest** change: the container is running code nobody recorded. |
| **High** | A wider attack surface or a removed safety net — a new image reference, health check removed, bind mount added, port published, a mount that lost `ro`. |
| **Medium** | Changed behaviour that could matter — environment, restart policy, a removed memory limit, network membership. |
| **Low** | Bookkeeping — labels, CPU limits, health-check timings, Compose metadata. |

`privileged` moving false→true is critical; true→false is the operator fixing
something and ranks low. A capability **added** is critical; one dropped is not.
Ranking the field rather than the movement would fill a dashboard with critical
alerts for improvements, and a dashboard that cries wolf gets ignored — which
costs more than having no dashboard.

### Secrets

A secret-backed field reports **that** it changed, never what to. The diff
engine withholds the values, the record has nowhere to put them, the repository
blanks them again, and a `CHECK` constraint refuses a row that carries one.
Four layers, because a leaked credential cannot be un-leaked.

Digests produced under **different HMAC keys** report `unverifiable` rather than
`modified`. Saying "modified" after a key rotation would tell an operator every
secret changed at once — a false alarm indistinguishable from a breach.

### Two distinctions the UI depends on

**"No drift" is not "never checked."** Every evaluation is recorded, including
one that found nothing, so the dashboard can say *"12 of 40 containers
evaluated"* rather than implying the other 28 are clean. A container with no
baseline is recorded as **incomplete with a reason**, not as clean.

**An incomplete evaluation resolves nothing.** A comparison that hit its size
budget never established that the fields it did not reach still match, so
resolving on that basis would silently clear real drift.

### Status: who owns which value

`active` and `resolved` are **engine-owned** facts about whether the difference
still exists. `acknowledged`, `ignored`, and `expected` are **operator-owned**
intent, and are the only three `PATCH /api/v1/drift/{id}` accepts.

An operator therefore cannot mark something resolved. Resolution is something
the world does, not something a person asserts, and allowing the assertion
would turn the drift list into a to-do list that lies. An operator's status
*survives* re-evaluation; only a resolved record that reappears returns to
active.

### Event integration

An event schedules an inventory refresh; the refresh commits; **the commit**
schedules a drift evaluation. Inventory reconciliation stays authoritative and
drift is always computed from committed data.

The evaluation queue is coalesced per container, hard-capped, and escalates to
a full sweep on overflow — the same discipline as the event engine's refresh
scheduler, for the same reason. Drift also gets its **own** diff-engine
instance, so a background sweep cannot exhaust the concurrency slots the
comparison endpoint depends on.

## Policy engine

HarborMaster checks every container's configuration against
**administrator-defined policies** and records each rule it fails.

**A policy is a question, not an instruction.** HarborMaster asks whether a
container complies; it has no capability to change one so that it does. There is
no enforce, apply, or remediate endpoint, and an API test asserts the absence
directly rather than trusting that nobody added one.

### Policy is not drift

Drift asks *did this container change from its baseline?* A policy asks *does
this container comply with an organisational rule?* They are independent, and
both answers are worth having:

- A container can **drift into a still-compliant configuration** — somebody
  bumped an image tag, and the new tag is still on the allowlist.
- A container that has **never moved can be non-compliant** from the day it was
  created, because the rule arrived afterwards.

Nothing in the policy engine reads a snapshot, and nothing in the drift engine
reads a policy.

### Strongly typed rules, not a language

A policy is a list of rules from a **closed catalogue of sixteen types**. There
is no expression language, no template, no script, and no user-supplied code
path of any kind. Every rule's semantics are fixed at compile time in
`internal/domain/policy_rules.go`; an administrator supplies a rule type and a
bounded parameter list, and nothing they write is ever interpreted.

That shape is the point. A policy is administrator-supplied input to an
network-reachable API, and an interpreter would make that input executable.

| Rule | What it checks |
| --- | --- |
| `privilegedForbidden` | `privileged == false` |
| `readOnlyRootFilesystemRequired` | `readOnlyRootFilesystem == true` |
| `imageAllowlist` / `imageDenylist` | The image reference against glob patterns |
| `requiredLabels` / `forbiddenLabels` | Label **keys** against glob patterns |
| `requiredEnv` / `forbiddenEnv` | Environment variable **names** — never values |
| `requiredCapabilities` / `forbiddenCapabilities` | Declared `capAdd` |
| `memoryLimitRequired` / `cpuLimitRequired` | A real cap, not a reservation or a weight |
| `restartPolicyAllowlist` | The restart policy against a closed vocabulary |
| `networkAllowlist` | Every attached network |
| `userNotRoot` | A declared non-root user |
| `healthCheckRequired` | A configured, enabled health check |

Two of those are narrower than their names suggest, and the editor says so
rather than implying otherwise:

- **Capabilities** are evaluated against the **declared** configuration.
  HarborMaster cannot see the daemon's default capability set, so a capability
  granted by default is invisible to it. Hardcoding Docker's default list would
  be a claim about a daemon HarborMaster has not asked.
- **`networkAllowlist`** evaluates the container's **attached networks**, which
  is how Docker surfaces the network mode: host mode appears as `host`,
  isolation as `none`. A container attached to no network of its own shares
  another container's namespace, which never satisfies an allowlist — the
  fail-closed direction.

### Severity follows the rule, not the container

A rule may override its policy's default severity; an omitted override
inherits. `privileged` is critical because the containment boundary is gone,
whoever set it.

### Patterns cannot be made expensive

Glob patterns accept `*` and `?` and nothing else — no character classes, no
alternation, and no regular expressions anywhere. `MatchGlob` is **iterative**
and remembers exactly one backtrack point, so it never recurses and has no
exponential path: that is the failure mode the phrase "regex DoS" names. Its
worst case is bounded by the pattern and subject length caps, and a pattern that
exceeds either is **refused at write time** rather than trimmed at match time,
so an expensive pattern never reaches the database.

### Environment rules evaluate names only

The evaluation input is a `PolicyTarget`, projected from the container detail.
It carries variable **names** and **has no field capable of holding a value**.
"Policies never read a secret" is therefore a property of the type rather than a
discipline every future rule author has to remember, and a test asserts it over
every string the projected target holds.

### "Compliant" is not "never checked"

Every pass is recorded, including one that found nothing. Without that,
*no violations for this container* is ambiguous between "it complies with every
rule" and "no pass has ever reached it" — and telling an operator their estate
is compliant when most of it was never examined is the worst thing a compliance
dashboard can do. The container view and the summary both report the
distinction.

**An incomplete pass resolves nothing.** A pass that could not apply every
policy has not established that the rules it skipped now pass; it established
that it stopped applying them. Resolving on that basis would silently clear real
non-compliance.

### Acknowledgement does not suppress

`active` and `resolved` are **engine-owned** facts about whether the rule still
fails. `acknowledged` and `exempted` are **operator-owned** statements of
intent, and are the only two `PATCH /api/v1/policy-violations/{id}` accepts.

Neither stops the checking. An acknowledged violation is re-evaluated on every
pass, keeps its last-seen timestamp current, and **resolves automatically** the
moment the container complies. An acknowledgement that stopped the checking
would turn the compliance report into a list of things somebody once clicked.

### Withdrawing a policy keeps its history

`DELETE /api/v1/policies/{id}` **archives**. A violation references its policy,
and the history has to survive the rule being withdrawn: an auditor asking what
the estate was failing last quarter must not get a different answer because
somebody tidied up this quarter.

So the definition is retained and marked archived, it stops being evaluated, and
its open violations resolve in the same transaction — the truthful record of a
rule that no longer applies. The schema enforces the same thing through
`ON DELETE RESTRICT`, so a hand-written `DELETE` against the database cannot
orphan the history either. There is no endpoint that permanently removes a
policy.

### Evaluation is batched and bounded

A pass runs after **every successful inventory refresh**, after a targeted
refresh of one container commits, on a periodic sweep, and on request through
`POST /api/v1/policy/evaluate`.

The sweep loads the active policy set **once** and applies it to every
container, so a thousand containers cost one policy query rather than a
thousand. Containers are processed sequentially and in pages: parallelism would
queue at the single database writer anyway while multiplying peak memory.

`POST /policy/evaluate` is **asynchronous** and answers 202. A synchronous
evaluation of a large estate would let a caller hold a request
open for minutes and occupy the single writer at will; the request is coalesced
through the same queue the scheduled passes use, so calling it in a loop
produces one pass rather than a backlog.

## Image intelligence

HarborMaster checks each image the inventory references against its registry and
reports whether a newer version is published.

**It reads. It never fetches.** There is no pull, no push, no delete, no prune,
and no apply — an update is reported, and acting on it is your job with your own
tooling. An API test asserts the absence of any mutation route directly.

### This is the only place HarborMaster talks to the internet

Every other part of HarborMaster reads a local Docker socket and a local SQLite
file. This one makes anonymous HTTPS GETs to registry manifest and tag-listing
endpoints, which makes SSRF the dominant risk of the feature. The defences are
layered, and each is independent:

1. **Destinations come only from image references.** A host reaches the client
   only through `domain.NormalizeImageRef`, which refuses IP literals, ports,
   `localhost`, single-label names, userinfo, and anything that is not purely a
   hostname. No config value, API parameter, or database column supplies one, and
   `POST /images/refresh` takes no target at all.
2. **The resolved address is checked at dial time.** The dialler inspects the
   actual IP the socket is about to use and refuses loopback, private,
   link-local, unique-local, carrier-grade NAT, multicast, and reserved ranges.
   Because it runs on the socket address rather than on the name, **DNS
   rebinding cannot get past it**.
3. **Redirects are refused outright.** A redirect is a registry-controlled URL,
   which is exactly the input this must not accept. Manifest and tag endpoints do
   not redirect; blob endpoints do, and HarborMaster never fetches a blob.
4. **No proxy.** Proxy environment variables are ignored, so the destination is
   always the registry itself — a proxy would necessarily be an internal address,
   which defence 2 exists to refuse.
5. **HTTPS only, verified**, with a TLS 1.2 floor and no setting that disables
   any of it.

An architecture test confines `net/http` and `crypto/tls` to the packages that
may have them, so a second HTTP client built elsewhere — which would have none of
these defences — fails the build.

### It holds no credentials

Every lookup is anonymous. HarborMaster reads no Docker config, no keychain, and
no credential helper, and accepts none through configuration. A private
repository reports `unauthorized`: an honest statement that the answer is
unavailable, rather than a reason to start handling your registry passwords. The
bearer tokens it negotiates for *public* repositories are pull-scoped, held in
memory for minutes, and never written to the database or a log.

### A tag is not a version

Most tags are not semantic versions, and treating them as though they were
produces confident nonsense. So parsing is conservative and **refuses more than
it accepts**:

- **Channel tags carry no version.** `latest`, `stable`, `main`, `edge` and the
  rest are never version-compared. Without that rule, a repository that also
  publishes `2.0` would report "a major update is available" for every container
  tracking `latest` — which is backwards, since `latest` is usually already the
  newest thing.
- **Comparability is narrow.** Two tags compare only within the same family: the
  same `v` prefix, the same number of components, and the same variant suffix.
  `1.25` and `1.25.3` do not compare — `1.25` is a floating tag that already
  points at the newest patch, so offering `1.25.3` as its update would be advice
  to pin something you deliberately left floating. `1.25-alpine` and `1.26` do
  not compare either; they are different images.
- **A calendar tag is not a major version.** `20240115` → `20240201` is reported
  as an update of *undetermined* size, because calling it "major" would assert a
  breaking change nobody claimed.

When no version comparison is possible, the fallback is always available and
always true: **the digest**. A tag that resolves to different content than the
one you are running has been republished, whatever it is called.

| Verdict | Meaning |
| --- | --- |
| `none` | The tag resolves to the image in use, and no newer tag exists in its series |
| `digest` | The same tag now points at different content — the publisher republished it |
| `patch` / `minor` / `major` | A newer tag exists in the same series |
| `prerelease` | The only newer tag is a release candidate, not a release |
| `unknown` | **HarborMaster could not determine whether an update exists** |

`unknown` is not a small update — it is the absence of an answer, and the UI
colours it accordingly. A tag listing that hit its page budget reports `unknown`
rather than `none`, because a listing that stopped early has not established that
no newer tag exists.

### "No updates" is not "not checked"

Every reference records whether it has ever been looked up. Without that,
*no updates for this image* is ambiguous between "it is current" and "nothing has
asked" — and telling an operator their estate is up to date when most of it was
never examined is the worst thing this feature could do. The dashboard states
coverage beside the update count, and a reference that can never be checked (a
local registry, an address literal) is tracked as `unsupported` so the gap is
explained rather than invisible.

**A failed lookup never overwrites a good answer.** If a registry is unreachable,
the previous digest and verdict remain the best knowledge available; blanking
them would turn "we could not ask" into "no update is available", which is a
different and false claim.

### Registry health is tracked per host

Rate limits and outages are properties of an endpoint, not of an image. Keeping
health per host means a rate-limited Docker Hub backs off every Docker Hub
reference at once — politer to the registry and faster to recover — and it is what
lets the UI say *"updates are stale because this registry is rate-limiting us"*
instead of showing a hundred individually failed images with no explanation.

A registry's own `Retry-After` is honoured, bounded at six hours so a hostile or
misconfigured value delays a check rather than cancelling it.

### Registry support is a small interface

Docker Hub, GHCR, and any registry speaking the OCI distribution API. The
protocol is identical everywhere; what differs is host defaulting, repository
shape, tag-listing page size, and whether listing exists at all — so that, and
only that, is what the `Provider` interface carries. Adding a registry is adding
a provider and registering it; nothing else in the codebase changes.

### Work is bounded everywhere

Concurrent requests, references per pass, tag pages, retries, response sizes,
manifest entries, annotation count and length, and the token cache all have caps.
The peer on the other end is a third party HarborMaster must stay welcome at, and
a client that hammers a public registry gets everyone sharing its egress address
rate-limited.

`POST /images/refresh` is **asynchronous** and answers 202. A synchronous pass
would let a caller hold a request open across hundreds of
registry lookups and generate outbound traffic on demand; the request is
coalesced, so calling it in a loop produces one pass rather than a backlog.

## Change planning

HarborMaster assesses each proposed image change and says how risky it looks,
why, and what to do about it.

**Nothing applies a plan.** HarborMaster cannot pull an image, recreate a
container, or roll one back, and planning adds no such capability — the planner
reads six tables it already populated and writes one row. There is no apply,
execute, approve, or schedule endpoint, and the route-coverage test would have
to change for one to appear.

### What a plan combines

For each container: the image it runs, the update its registry offers, its
baseline snapshot, that snapshot's restore readiness, its open drift, and its
policy compliance. Every input comes from data HarborMaster has already
persisted, so **a plan is reproducible from the database alone** — the planner
makes no network request and touches no Docker socket.

### The risk model is deterministic, and that is the point

A fixed slice of compile-time typed rules, evaluated in order, over integer
scores. No clock, no randomness, no I/O, and no map iteration — Go randomises
map order, so a factor list built from one would differ between runs of the same
input. The evaluation time reaches the rules as an explicit field, which is what
lets a test pin every time-dependent rule.

The same inputs produce the same score, the same band, the same recommendation,
and the same explanation in the same order, byte for byte, on any machine.
**Nothing is learned, probabilistic, or statistical.** Each number is a
judgement written down by a person and reviewable as such, and every factor
names the rule that produced it — so a score is traceable to a specific, named
piece of code rather than to an opaque total.

Points and severity are deliberately separate. Points decide the **band**;
severity decides the **recommendation**. Collapsing them would lose the case
that matters most: a critical policy violation must argue against a change even
when everything else about it is unremarkable, and a low total must not launder
it into "proceed".

### A gap in evidence is never reported as safe

When HarborMaster could not establish something — a registry that did not
answer, a digest it could not resolve, a change it could not classify — the
recommendation is `unknown`, which is deliberately **not** `proceed`. This is
the single most misleading thing the feature could do, so it is pinned from both
sides in the model, the API, and the UI: the count is reported beside the rest of
the summary rather than folded into "ready to act on", and the badge's tooltip
says what it is not.

The same care applies to an *absent* plan. A container with no plan has **no
change proposed for it** — not a change that was assessed and found safe.

### Plans are immutable

There is no PATCH and no DELETE, and the stored row carries no status, no queue
position, and no assignee. A changed world produces a **new** plan and the old
one remains as the record of what was believed when a decision was made. That is
what makes the reasoning timeline worth having, and it is enforced by the schema
rather than by convention.

### An unchanged estate costs nothing to re-plan

Every assessment is fingerprinted over exactly the values it read — a SHA-256
over a sorted list of named fields, including the planner version, so bumping the
rule set invalidates every stored fingerprint and forces a clean regeneration. A
pass over an unchanged estate **writes nothing at all**, which is what makes it
safe to run one after every inventory refresh.

The application-level check avoids the write; a unique index on
`(container_id, input_digest)` is what makes the guarantee hold when two passes
race, and a conflict there is treated as "unchanged" rather than as an error.

### No N+1

Containers are processed in batches, and each batch costs a fixed number of
grouped queries whatever its size. Ten thousand containers in batches of five
hundred is about a hundred queries rather than sixty thousand, and a test counts
them rather than describing them.

`POST /plans/generate` is **asynchronous** and answers 202, because a synchronous
pass over a large estate would hold a request open for minutes.
It is coalesced, so calling it in a loop produces one pass rather than a backlog.

## Safe image acquisition

HarborMaster can download an approved image to your host. It is off by default.

### Acquiring an image does not update a container

Worth stating first, because it is the thing most easily assumed. Acquiring an
image puts layers in the daemon's local image store. **No container is stopped,
started, recreated, or reconfigured by an acquisition** — a container keeps
running the image it was created from, and an acquired image sits in the store
beside it.

Applying one is a separate, separately-enabled capability held by a different
service: see [Manual container recreation](#manual-container-recreation). There
is still no endpoint, setting, or button that deletes or prunes an image.

### The image mutation surface is one method

`docker.Runtime` — which every other service receives — remains read-only, with
its exact method set pinned by a test. The pull capability lives on its own
interface, `docker.ImageAcquirer`, with **exactly one method**: pull a
digest-pinned image. Three architecture tests keep it there:

- the interface has exactly one method, named `PullByDigest`;
- no method on it may be named for a container operation;
- no package outside the acquisition service may even reference it — so a
  handler cannot pull directly and bypass the checks below.

Capability is granted by what a constructor is handed. A service that only needs
to observe never receives the acquirer and therefore cannot pull.

### Every acquisition is digest-pinned

A tag can move between the moment a change is approved and the moment it is
fetched. So the target is always `registry/repository@sha256:…`, assembled
inside the adapter from validated components — there is no branch that produces
a tag, and a plan with no resolved digest cannot be acquired at all.

The request itself carries **no target**: an operator supplies a plan id, and
the registry, repository, and digest are derived from that plan. Unknown JSON
fields are rejected rather than ignored, so a target cannot be smuggled in.

### The plan is revalidated immediately before the pull

The gap between "a plan said this was reasonable" and "we are downloading it" is
a time-of-check/time-of-use window. The preflight runs twice — once when the
operator asks, so the answer is immediate, and again inside the worker, which is
the run that matters. It refuses when:

the plan is missing, superseded, or does not recommend the change · the
recommendation is `unknown` · there is no proposed digest, or it has changed
since approval · the image does not publish this platform · the registry
evidence is missing, failed, or older than the freshness window · the container
is gone · a critical policy violation is open · there is no usable snapshot ·
Docker is unavailable · another acquisition for the same image is running · a
concurrency limit would be exceeded.

A refusal is reported with the **specific check** that said no, because "the
digest moved" and "the daemon is down" call for different things from an
operator.

### The transfer is not the proof

A completed pull means the daemon accepted the request and reported no error. It
does not establish what is in the local store: the daemon resolves the reference
itself, and a registry can serve different content than expected.

So after every transfer the image is **re-inspected through the read-only path**
and its digest and platform compared against what was approved. Only that
comparison can conclude "succeeded", and it fails closed — a check that could
not be performed is its own classification, distinct from a mismatch.

A digest mismatch is logged at error level and is **never retried
automatically**: repeating a pull that produced the wrong content is how a
transient substitution becomes a persistent one.

### Bounded everywhere, and never automatic

Global and per-registry concurrency, a pull timeout, a request deadline after
which a queued request expires unstarted, and three independent bounds on how
much of the daemon's progress stream is persisted. The per-registry limit is the
one that matters to a third party: anonymous rate limits are shared by egress
address.

**Nothing happens on a schedule.** Every acquisition is requested by a person.
The worker's periodic ticks only expire stale requests and prune old records; a
HarborMaster left running with nobody asking it for anything downloads nothing,
forever.

### Records are audit records

At most one acquisition per (container, digest) can be active — a partial unique
index, so the guarantee holds when two requests race rather than depending on a
check winning. A completed record is never rewritten by a later attempt, and
retention never removes the most recent one per container.

No column can hold a credential, a raw daemon error, or a registry response
body. The operator-facing message is built from a fixed HarborMaster vocabulary
keyed by the failure classification.

### After a restart

An acquisition left in `pulling` is a claim about a process that no longer
exists. Those rows are **failed honestly rather than resumed**: the transfer was
never verified, and an unverified image must never be recorded as acquired.
Re-verifying instead would mean asserting that the image on the host now is the
one that particular pull produced, which is exactly the assumption verification
exists to avoid making.

## Manual container recreation

HarborMaster can replace one container with a new one built from its own
configuration, running an image it already downloaded and verified.

**This is the largest thing HarborMaster does, and the only thing that changes
something running.** It is off by default, and refuses to start unless image
acquisition is enabled too.

### What it does, in order

```
stop the original  →  rename it aside  →  create the replacement  →  start it
                                                                        ↓
       remove the original  ←  record the success  ←  prove all four checks
```

The original is **parked, not removed**: stopped and renamed
`<name>.hm-old-<executionId>`. It is removed only after every verification
passes *and* the success has been written durably. Any failure before that point
leaves it exactly where it is, which is what makes an unsuccessful recreation
recoverable by hand rather than an outage with no way back.

### The four proofs

All four must pass. A check that was never reached reads `unknown`, and an
`unknown` is never treated as a pass.

| Proof | What it establishes |
| --- | --- |
| **Health** or **stability** | The replacement works. A container with a health check gets a real verdict; one without must stay running for a configured window, which is recorded as the weaker evidence it is |
| **Image** | It is running the digest that was approved, not a tag that happens to point there |
| **Configuration** | Its configuration matches the original's, field by field — including capabilities, security options, read-only rootfs, namespaces, limits, mounts, and ports |
| **Network** | It is attached to every network the original was on, with the same aliases |

### There is no automatic rollback

Deliberately. When a recreation fails after the first mutation, HarborMaster:

- stops the replacement and renames it `<name>.hm-failed-<executionId>`, so a
  container that failed its checks is not left serving under the production
  name;
- **removes neither container** — both are evidence;
- records a manual recovery plan naming both by name and id, with the exact
  commands to restore service.

An automatic undo would be another unattended mutation, performed at exactly the
moment HarborMaster has demonstrated that its model of the host is wrong. The
UI, the API, and the record all say this in as many words, and the confirmation
dialog says it before you act.

Undoing a recreation *afterwards*, when a person decides to, is a separate
feature with its own permission and its own record. See
[Manual rollback](#manual-rollback).

## Manual rollback

**Returning ONE container to the state ONE recreation replaced.** Off by
default, and refused at startup unless recreation is on too.

A recreation stopped a container, parked it under a derived name, and started a
replacement in its place. A rollback undoes exactly that arrangement: it stops
the replacement, renames it aside, renames the preserved original back, starts
it, and proves it.

> **This causes downtime, and the README says so before the feature does
> anything else.** The replacement is stopped *before* the original is started,
> and the original then has to start and pass its checks. There is no overlap:
> two containers cannot hold one name.

### What the request carries

An **execution id**, and optionally an idempotency key. That is the whole body.

There is no field for a container id, a container name, an image, a digest, a
snapshot, or any Docker parameter — not rejected, *absent*. Both container
identities, the production name, and the image identity are read from
HarborMaster's own record of that recreation and re-verified against the live
host before anything moves.

### What it checks, twice

The full preflight runs synchronously before the request is accepted, so a
refusal is immediate rather than arriving as a failed background job. It runs
**again** inside the worker, immediately before the first mutation, because
minutes may have passed and somebody may have been moving containers by hand.
Only the second verdict decides whether anything is touched.

Both runs ask the same questions: the execution exists and has settled; its
checkpoint left an arrangement that can be undone; the original was not removed;
no successful rollback of it exists; nothing else is in flight for this
container; the daemon answers; the inventory is fresh; both containers are still
present under the names and images the record gives; the production name is free
or held by the replacement; and the original's configuration can be projected so
the result can be proved. A refusal names the specific check that said no.

### What happens to the containers

| Step | What is true afterwards |
| --- | --- |
| Stop the replacement | The replacement is stopped and still holds the production name |
| Rename it aside | It is `<name>.hm-rolledback-<rollbackId>`, and the name is free |
| Rename the original back | The original holds its own name and is not running |
| Start the original | It is running and not yet proved |
| Prove it | Health or stability, image identity, configuration preservation, network attachment — all four must pass |

A checkpoint is written after each of those and before the next. If one cannot
be written, the pipeline **stops** — it never repeats a mutation whose record is
uncertain.

### Nothing is removed

The rollback capability has four methods — stop, rename aside, rename back,
start — and no remove. The replacement stays on the host as the evidence of why
the recreation was backed out, and you delete it yourself when you are done with
it.

### One per recreation, ever

Enforced by a unique index rather than by a check that hopes to win a race. A
*refused* rollback does not consume that chance: the point of a refusal is that
nothing happened.

### When a rollback fails

Exactly as a failed recreation does. It stops, preserves both containers,
records the checkpoint that says what was actually done, and writes a manual
recovery plan of fixed-vocabulary steps. **It never guesses which container
should serve traffic**, and it does not try to put things back — it has just
demonstrated that its model of the host is wrong.

### Turning it on

```bash
HARBORMASTER_EXECUTION_ENABLED=true
HARBORMASTER_ROLLBACK_ENABLED=true
```

When `ROLLBACK_ENABLED` is false the capability is **absent**, not merely
unused: no rollbacker is wired and the process holds no ability to perform one.
Requesting a rollback needs `rollback:create`, which an operator and an
administrator hold and a viewer does not. Reading the history needs
`rollback:read`, which every role holds — an incident is not a reason to hide
the record of it.

### Configuration is reproduced, and secrets are not re-derived

The container's live configuration is read into a value the execution service
**cannot inspect, log, or serialise** — its environment, log-driver options, and
SDK structures live in unexported fields inside `internal/docker`, and it is
handed straight back to the daemon. What the service and the API can see is a
value-free projection: environment *names*, mount destinations, network names,
capability lists. A sensitive value contributes a keyed digest and never a
value, under the same installation key the snapshots use.

Anonymous volumes are carried forward **explicitly**, naming the volume that
already exists. Recreating naively would give the replacement a brand new empty
volume and orphan the original's data, which is data loss dressed up as an
update.

### The mutation surface is five methods

`docker.ContainerMutator`: create, start, stop, rename, remove. Four
architecture tests keep it there — the exact set, a verb check that refuses
exec/attach/copy/image/volume/network operations, a pin on the exported surface
of the captured configuration, and a source-level rule that no package outside
the execution service may name any of it.

`RemoveContainer` cannot force and cannot remove volumes. There is no field for
either: a container's data is not HarborMaster's to delete, and forcing would
discard the evidence that the container was already stopped.

### Every prerequisite is re-checked immediately before anything is stopped

An acquisition that succeeded, is fresh, and has not been used before; a plan
that is current and still recommends the change and whose fingerprint still
matches; a container that exists and is still on the assessed image and in a
usable state; a fresh inventory; a usable snapshot; a fresh, complete policy
evaluation with no critical violation; fresh registry evidence; and the image
still present locally carrying the approved digest for this platform.

The whole set runs when you ask, so a refusal is immediate, and **again**
immediately before the first mutation.

### One use per acquisition

A succeeded acquisition can be executed exactly once, enforced by a unique
index. There is no override parameter. A second recreation of the same container
needs a fresh plan — assessed against the world as it is now — and a fresh
acquisition to prove the image is still there.

### Requesting one

```sh
curl -X POST localhost:8080/api/v1/executions \
  -H 'Content-Type: application/json' \
  -d '{"acquisitionId":"acq_…"}'
```

That is the whole request. There is no container, image, digest, command,
mount, capability, timeout, or force parameter, and unknown fields are rejected
rather than ignored.

In the UI the control lives on a succeeded acquisition's page, behind a
confirmation that states the three facts that matter — the container will be
stopped and recreated, the image is already here, rollback is not automatic —
and requires you to type the container's name.

### After a restart

A recreation interrupted mid-flight is settled from its **checkpoint**, which
records what was actually done to the host rather than what HarborMaster was
doing. Recovery issues **no Docker call at all**: it records the outcome and
attaches the recovery plan for that exact situation. See
[`docs/engineering/reliability.md`](docs/engineering/reliability.md) for the
table of checkpoints and what each one means for the host.

## Automatic updates

**This is the only thing HarborMaster does without being asked.** Everything
else on this page either reports, or acts when you press a button.

Off by default. Turning it on does not start updating anything: what may be
updated is entirely the business of **update policies**, and every new policy
starts in a mode that changes nothing.

```bash
HARBORMASTER_AUTOMATION_ENABLED=true
```

Refused at startup unless recreation **and** rollback are also enabled.
Automation submits recreations, and an unattended update that fails
verification must be able to put the container back.

### It is a caller, not a new capability

The engine holds no Docker interface. For each container a policy selects, it
submits exactly the two requests your own update would — acquire the planned
image, then recreate the container with it — to exactly the same services, each
of which re-runs its **full preflight against the live host** at the moment it
acts.

So automation cannot skip the planner, the digest verification, the
configuration-preservation comparison, the health proof, or the checkpoint
discipline. It does not implement any of them; it asks the components that do.
Eight architecture tests fail the build if that stops being true.

It also cannot be aimed. A policy names **which containers** and **how far**.
What image a matched container moves to is decided by the planner from registry
evidence and re-verified immediately before the pull. There is no field
anywhere in this feature — not in the API, not in the environment, not in a
label — that carries an image, a tag, a digest, or a registry.

### An update policy

```
Name          Nightly patches
Selects       containers named web, api  — never database
Ceiling       up to patch
Mode          automatic
Window        02:00–04:00 Europe/London, Saturday and Sunday
On failure    roll back automatically, then pause
```

**Mode** is the setting that matters most, and there are four:

| Mode | What it does |
| --- | --- |
| `observe` | Evaluates everything, records the decision, **changes nothing**. The correct first setting on a real host. |
| `dryRun` | Observe, plus the order things would happen in. |
| `approvalRequired` | Decides automatically and waits for a person to release each update. |
| `automatic` | Downloads and recreates without asking. |

**Ceiling** bounds how far a version may move: `digestOnly` (a republished tag
and nothing else), `patch`, `minor`, `major`. No ceiling permits an update
HarborMaster could not size — an `unknown` or a pre-release is never applied,
whatever the setting says.

`AUTOMATION_REQUIRE_APPROVAL_FOR_MAJOR` is **on by default** and holds major
versions for a person whatever the policy says. A major version is where
publishers put breaking changes, and the setting that lets one reach your host
at 02:00 should have to be turned off in two places.

Writing a policy needs `automation:manage`, which only an **administrator**
holds. A policy is a standing, unattended grant of `execution:create` over every
container its selector reaches.

### Maintenance windows

The window carries an IANA timezone and every comparison is made in it. A window
that crosses midnight is handled as two spans, and the weekday that governs the
morning half is the one the window **started** on. DST is correct in both
directions: a spring-forward gap opens nothing and an autumn-back repeat opens
twice.

**A window whose timezone this host cannot resolve is CLOSED, not open.** A
mistyped zone never silently authorises updates at any hour. (The IANA database
is embedded in the binary, so the distroless runtime image does not need one.)

### Container labels

A container can narrow what automation does to it, without a policy edit:

```
io.harbormaster.update.enabled=false       never update this container
io.harbormaster.update.pause=true          same, temporarily
io.harbormaster.update.strategy=patch      a narrower ceiling than the policy's
io.harbormaster.update.window=02:00-04:00  different times, same timezone
io.harbormaster.update.rollback=false      do not roll this one back
```

**A label may only ever make automation safer.** `enabled=true` does *not*
enrol a container no policy selected, and `strategy` may only narrow. Anyone
who can run `docker run` can set a label; if that were enough to opt a container
into unattended updates, it would also be enough to widen how far they go. There
is no label for the mode.

A misspelled `io.harbormaster.update.*` key is reported rather than ignored.

### What happens when one fails

An unattended update that fails verification is **rolled back automatically**,
when the policy permits — the same rollback an operator would have asked for,
through the same service, with the same four proofs.

Then the container is **paused**, and a rollback pause never expires on its own.
The change was wrong and the host was moved twice to discover that; retrying
that on a timer is how one bad image becomes a repeated outage. A person clears
it, and clearing it is recorded against their account.

Repeated failures pause a container too, at the policy's threshold, counted
within the policy's window.

### Every pass is recorded, including the ones that did nothing

The hardest question to ask an automation system is *why did you not update that
container*, and it is unanswerable unless the reasoning was written down at the
time. So every pass records one row per container it considered, each with a
reason from a closed vocabulary: `windowClosed`, `strategyCeiling`,
`recommendation`, `automationPaused`, `noPlan`, `labelDisabled`, and so on.

```bash
# What would the next pass do? A read — it records nothing and pulls nothing.
curl -s -b cookies.txt localhost:8080/api/v1/automation/upcoming | jq

# Decide everything, change nothing, and keep the record.
curl -s -b cookies.txt -H "X-HarborMaster-CSRF: $CSRF" \
  -X POST -H 'Content-Type: application/json' \
  -d '{"dryRun":true}' localhost:8080/api/v1/automation/run | jq

# Which containers is automation refusing to touch, and why?
curl -s -b cookies.txt localhost:8080/api/v1/automation/paused | jq
```

### Bounds

| Setting | Default | What it bounds |
| --- | --- | --- |
| `AUTOMATION_INTERVAL` | `15m` | How often a pass runs. A closed window costs a query. |
| `AUTOMATION_MAX_CONCURRENT` | `1` | Simultaneous updates, engine-wide. A policy cannot raise it. |
| `AUTOMATION_MAX_PER_RUN` | `10` | Updates one pass may start. |
| `AUTOMATION_PASS_TIMEOUT` | `10m` | One decision pass. It does not wait for a pull. |
| `AUTOMATION_FOLLOW_INTERVAL` | `30s` | How often submitted work is advanced. |
| `AUTOMATION_RETENTION_AGE` | `90d` | How long the decision history is kept. |

### After a restart

The follower holds no state. Every tick it re-reads recent decisions from the
database and takes the one next step each is owed, so a restart between the pull
and the recreation resumes rather than abandons. A pass a restart cut short is
recorded as `interrupted`: it submits work to services that checkpoint their
own, so an interrupted pass is a bookkeeping gap and never a host in an unknown
state.

## Reliability and recovery

The full runbook is [`docs/engineering/reliability.md`](docs/engineering/reliability.md).
This is the shape of it.

### Two operator commands

```sh
harbormaster diagnose            # inspect the database, print findings
harbormaster backup <path>       # consistent copy, then verify it
```

Both are **commands, not endpoints, and will stay that way while the API is
reachable over HTTP**. They report filesystem paths, free space, schema history,
and when the daemon was last reachable — what an operator needs and what an
attacker wants. Requiring shell access is the control, and an architecture test
fails the build if `internal/api` ever imports the diagnostics package.

`diagnose` opens the database **read-only** and migrates nothing, so it is safe
against a live server, a stopped one, or a backup. It opens no Docker
connection: a diagnostic that talks to a privileged socket to answer a question
about a file would be adding a capability for the sake of a report.

### Do not `cp` the database

With write-ahead logging the committed state is split between
`harbormaster.db` and its `-wal` sidecar. A file copy taken while HarborMaster
is running can capture a database missing its most recent commits, or one that
is internally inconsistent. It will usually *seem* to work — the worst property
a backup procedure can have.

`harbormaster backup` uses SQLite's `VACUUM INTO`, which runs inside a read
transaction and produces a consistent, defragmented copy without an exclusive
lock. It then **verifies the copy**: a full integrity check, a foreign key
check, the schema history compared against the running build, and row counts
for every table that carries history. An unverified backup is a belief, not a
control.

It refuses to overwrite an existing file and refuses the live database and its
sidecars. A backup that fails verification is left on disk deliberately — it is
evidence — and reported as failed.

### What happens at startup

Journal mode is **confirmed**, not assumed: SQLite falls back from WAL silently
on a filesystem that cannot support it, and the durability profile then differs
from the documented one with no signal unless something asks.

The database is then **validated before it is migrated**. Detected damage
refuses startup, because writing more history over a malformed image shortens
the window in which a backup still predates the damage. A check that could not
*complete* does not refuse: it establishes nothing, and turning a slow disk into
an outage is worse than a late diagnosis.

Schema history is validated too. Three states are refused, and each means *the
schema is not what this binary believes*: a migration this build does not
contain (a newer HarborMaster wrote the database), an applied migration whose
file has since changed, and a gap in the sequence. In all three cases the
database is **not modified**, and the error says so — the destructive instinct
here is to delete it and let it rebuild, which throws away the history.

### Crash recovery

A HarborMaster killed mid-write leaves committed transactions in the
write-ahead log; the next start replays them. No committed data is lost and no
operator action is required.

An interrupted inventory refresh leaves the **previous** inventory whole,
because a refresh commits in one transaction at a new generation. An
interrupted migration leaves the last complete one, and the next start finishes
the job. The event engine restores its timestamps and reconnect count, and
resumes the Docker stream from just after the last event it saw — **clamped to
one hour**, because an unclamped window turns a long outage into a request for
the daemon's whole event ring, making recovery the load spike.

### Bounded shutdown

Every wait is bounded by `HARBORMASTER_SHUTDOWN_TIMEOUT`, and it is **one
budget** for the HTTP drain and the background drain together — the
orchestrator counts down a single deadline for the whole process.

Work already in flight gets a grace period rather than being cancelled
outright, so a reconciliation mid-transaction commits instead of rolling back
for no reason; it is then cancelled. Both halves matter. An unbounded wait means
a sweep of a thousand containers against a daemon that has stopped answering
holds the process open long past the point the runtime gives up and sends
`SIGKILL` — a worse ending, because it lands at an arbitrary instant.

### Failure conditions

SQLite result codes are classified rather than message text, so each condition
reports the remedy it actually implies: corruption means restore, a full disk
means free space, a read-only mount means fix the mount, busy means another
writer holds the lock. Corruption is the only one treated as fatal — everything
else describes a condition that can clear.

Docker being unreachable is **degraded, never unhealthy**. Escalating it would
put HarborMaster into a restart loop every time the daemon restarts.

## Architecture

Layers depend inward. Nothing in `domain` imports an adapter, and services
depend on interfaces rather than concrete clients, so every layer is testable
without a Docker daemon or a database file.

Docker SDK types never leave `internal/docker`. Domain models, services,
repositories, API handlers, OpenAPI schemas, and the frontend all speak
HarborMaster's own types, which is what would let a second runtime adapter be
added without touching the service or API layers.

That rule is enforced, not just documented. `internal/arch` parses every Go
file's imports and fails the build if the SDK appears outside the adapter, and
it reflects over the runtime interface to fail if a mutation method is added.

The SDK is the Moby client split out of the engine at Docker v29:
`github.com/moby/moby/client` and `github.com/moby/moby/api`. The retired root
module `github.com/docker/docker` is not used, and neither is the engine
monolith `github.com/moby/moby/v2`, which is not meant to be consumed as an
application library. See [docs/security-triage.md](docs/security-triage.md) for
why the old module could not simply be upgraded.

```
cmd/harbormaster      Composition root: config, wiring, signals, shutdown
  └── internal/api        REST handlers, middleware, SSE, SPA static serving
        └── internal/service   Application logic (health, inventory, events)
              ├── internal/docker  Docker Engine SDK adapter (read-only)
              └── internal/store   SQLite persistence, embedded migrations
                    └── internal/domain  Models shared by every layer
internal/config       Environment configuration and validation
internal/logging      Structured logging (log/slog)
internal/version      Build metadata injected via -ldflags
web                   React + TypeScript + Tailwind SPA, embedded via go:embed
api                   OpenAPI 3.1 specification
deployments           Docker Compose stack
```

Two boundaries carry most of the safety burden:

- **`internal/docker`** is the only package that talks to the Engine, and every
  operation it exposes is an observation: ping, list, inspect, and subscribe to
  events. There is no method that creates, starts, stops, removes, pulls, or
  executes anything, and no accessor that hands out the SDK client. Gaining
  write access to Docker requires editing this package, which makes that change
  reviewable in a diff.

  The event subscription hands out HarborMaster's own event records, never the
  SDK's `events.Message` and never a raw channel, so nothing downstream can come
  to depend on the SDK's shape.
- **`internal/api`** is the only package that renders errors to a client. It
  logs the real error and returns a stable code with a generic message, so
  stack traces and socket paths never leave the process.

### Concurrency and shutdown

The event engine introduces long-lived goroutines, so ownership is explicit.
`EventService.Run` is the sole owner of exactly three children — the stream
reader, the event processor, and the refresh/reconcile/prune worker — and
returns only once all three have exited. Every child selects on the context. The
processing queue has one writer, which is also its only closer, so there is no
send-on-closed race and no double close.

Shutdown order is deliberate, because the last step depends on the first:

1. The HTTP server drains. In-flight requests and open SSE streams end.
2. Background services stop. The inventory loop and the event engine exit, and
   the engine flushes anything already read using a context that survives
   cancellation for a bounded grace period, so those events are not lost and
   the flush cannot outlive the shutdown deadline.
3. The Docker client and the database close — the database **after** background
   persistence has stopped, or a final flush would write to a closed handle.
   `Close` checkpoints the write-ahead log, so a clean stop leaves nothing to
   replay.

Every one of those waits is **bounded**. `GraceContext` is the primitive: work
detached from cancellation survives a signal just long enough to commit, and is
then cancelled. Detached-but-unbounded is the failure mode it replaced — a
fifteen-minute reconciliation bound meant `SIGTERM` during a large sweep held
the process open until the runtime killed it.

One subtlety worth knowing if you touch the adapter: the event stream uses a
**second SDK client built without a request timeout**. The SDK's `WithTimeout`
sets `http.Client.Timeout`, which bounds the whole exchange including reading
the body — and the event stream is a body that never ends. Sharing the timed
client would tear the stream down every `DOCKER_TIMEOUT` seconds and present as
a daemon that will not stay up. Every other call keeps its timeout.

### Frontend

A single-page app with a responsive shell, served from the Go binary. Health is
polled once at the shell level and passed down, so every view agrees on
connectivity and the app makes one request per interval rather than one per
component.

Pages whose endpoints do not exist yet render an explicit empty state. They
show no sample rows: placeholder data in an operations tool is
indistinguishable from real data at a glance, which is a dangerous thing to put
next to a container list.

## Configuration

Every setting comes from the environment. See [`.env.example`](.env.example)
for the full list with defaults.

| Variable | Default | Purpose |
| --- | --- | --- |
| `HARBORMASTER_HTTP_ADDR` | `127.0.0.1:8080` | Listen address |
| `HARBORMASTER_MAX_REQUEST_BYTES` | `1048576` | Request body limit |
| `HARBORMASTER_READ_HEADER_TIMEOUT` | `5s` | Header read deadline |
| `HARBORMASTER_READ_TIMEOUT` | `15s` | Request read deadline |
| `HARBORMASTER_WRITE_TIMEOUT` | `30s` | Response write deadline |
| `HARBORMASTER_IDLE_TIMEOUT` | `60s` | Keep-alive deadline |
| `HARBORMASTER_SHUTDOWN_TIMEOUT` | `15s` | Drain period on SIGINT/SIGTERM |
| `HARBORMASTER_DOCKER_HOST` | `unix:///var/run/docker.sock` | Engine endpoint |
| `HARBORMASTER_DOCKER_TIMEOUT` | `10s` | Per-call Engine timeout |
| `HARBORMASTER_DB_PATH` | `./data/harbormaster.db` | SQLite database file |
| `HARBORMASTER_DB_BUSY_TIMEOUT` | `5s` | Wait for another **process's** write lock |
| `HARBORMASTER_DB_INTEGRITY_CHECK` | `quick` | Startup validation: `off`, `quick`, or `full` |
| `HARBORMASTER_DB_INTEGRITY_TIMEOUT` | `30s` | Bound on that check; past it the result is *incomplete*, not *damaged* |
| `HARBORMASTER_DB_REQUIRE_WAL` | `false` | Refuse to start when write-ahead logging could not be enabled |
| `HARBORMASTER_DRIFT_ENABLED` | `true` | Turn drift detection on or off |
| `HARBORMASTER_DRIFT_EVALUATE_ON_EVENTS` | `true` | Evaluate after an event-driven refresh **commits** |
| `HARBORMASTER_DRIFT_EVALUATION_DEBOUNCE` | `5s` | Coalesce a lifecycle burst into one evaluation |
| `HARBORMASTER_DRIFT_SWEEP_INTERVAL` | `30m` | Periodic full sweep; `0` disables it |
| `HARBORMASTER_DRIFT_MAX_RECORDS_PER_CONTAINER` | `500` | Past it an evaluation is *incomplete*, never silently truncated |
| `HARBORMASTER_DRIFT_RETENTION_AGE` | `720h` | How long resolved records are kept; open records are never pruned |
| `HARBORMASTER_POLICY_ENABLED` | `true` | Turn the policy engine on or off |
| `HARBORMASTER_POLICY_SWEEP_INTERVAL` | `15m` | Periodic full compliance pass; `0` disables it |
| `HARBORMASTER_POLICY_MAX_POLICIES` | `200` | Active policies one pass may load |
| `HARBORMASTER_POLICY_MAX_VIOLATIONS_PER_CONTAINER` | `500` | Past it a pass is *incomplete*, never silently truncated |
| `HARBORMASTER_POLICY_MAX_RULES_PER_POLICY` | `32` | Bounds what one policy can cost to evaluate |
| `HARBORMASTER_POLICY_MAX_VALUES_PER_RULE` | `32` | Bounds the pattern matches one rule performs |
| `HARBORMASTER_POLICY_RETENTION_AGE` | `2160h` | How long resolved violations are kept; open ones are never pruned |
| `HARBORMASTER_POLICY_WRITE_RATE_LIMIT` | `60`/min | Policy writes get their own budget; a write is one small transaction, not a Docker sweep |
| `HARBORMASTER_IMAGE_INTEL_ENABLED` | `true` | Image update discovery. **`false` makes no outbound request at all** |
| `HARBORMASTER_IMAGE_INTEL_REFRESH_INTERVAL` | `6h` | How long a successful answer stays fresh |
| `HARBORMASTER_IMAGE_INTEL_COLLECT_INTERVAL` | `5m` | How often the due set is drained; `0` collects only on request |
| `HARBORMASTER_IMAGE_INTEL_MAX_CONCURRENT_REQUESTS` | `4` | Simultaneous registry requests, process-wide |
| `HARBORMASTER_IMAGE_INTEL_MAX_REFERENCES_PER_PASS` | `50` | Bounds one batch, spreading a large estate over time |
| `HARBORMASTER_IMAGE_INTEL_MAX_TAG_PAGES` | `5` | Past it a listing is *incomplete*, never silently "up to date" |
| `HARBORMASTER_IMAGE_INTEL_FAILURE_BACKOFF` | `15m` | Doubling, capped, jittered; a registry's `Retry-After` wins |
| `HARBORMASTER_INVENTORY_ENABLED` | `true` | Turn the inventory engine on or off |
| `HARBORMASTER_INVENTORY_REFRESH_ON_STARTUP` | `true` | Collect once at boot |
| `HARBORMASTER_INVENTORY_REFRESH_INTERVAL` | `60s` | Periodic refresh; `0` disables it |
| `HARBORMASTER_INVENTORY_WORKERS` | `8` | Concurrent inspections (1–64) |
| `HARBORMASTER_INVENTORY_ABSENT_RETENTION` | `168h` | How long absent containers are kept |
| `HARBORMASTER_INVENTORY_MASK_PATTERNS` | see below | Names treated as secret-bearing |
| `HARBORMASTER_EVENTS_ENABLED` | `true` | Turn the Docker event engine on or off |
| `HARBORMASTER_EVENTS_RECONNECT_INITIAL_DELAY` | `1s` | First backoff delay after a dropped stream |
| `HARBORMASTER_EVENTS_RECONNECT_MAX_DELAY` | `60s` | Backoff ceiling |
| `HARBORMASTER_EVENTS_RECONNECT_MULTIPLIER` | `2.0` | Backoff growth factor (≥ 1) |
| `HARBORMASTER_EVENTS_BUFFER_SIZE` | `1024` | Processing queue depth |
| `HARBORMASTER_EVENTS_BATCH_SIZE` | `64` | Events persisted per transaction |
| `HARBORMASTER_EVENTS_BATCH_FLUSH_INTERVAL` | `500ms` | How long a partial batch waits |
| `HARBORMASTER_EVENTS_DEDUP_WINDOW` | `10s` | Duplicate-fingerprint memory |
| `HARBORMASTER_EVENTS_REFRESH_DEBOUNCE` | `750ms` | Coalescing window per resource |
| `HARBORMASTER_EVENTS_RECONCILE_INTERVAL` | `15m` | Periodic full sweep while the engine runs |
| `HARBORMASTER_EVENTS_RETENTION_AGE` | `168h` | Event age limit; `0` disables |
| `HARBORMASTER_EVENTS_RETENTION_COUNT` | `50000` | Event row limit; `0` disables |
| `HARBORMASTER_EVENTS_PRUNE_INTERVAL` | `1h` | How often retention runs |
| `HARBORMASTER_EVENTS_STREAM_MAX_SUBSCRIBERS` | `16` | Concurrent SSE clients |
| `HARBORMASTER_EVENTS_STREAM_BUFFER_SIZE` | `128` | Per-subscriber queue |
| `HARBORMASTER_EVENTS_STREAM_REPLAY_LIMIT` | `200` | Cap on `Last-Event-ID` replay |
| `HARBORMASTER_EVENTS_STREAM_HEARTBEAT` | `20s` | SSE keep-alive comment interval |
| `HARBORMASTER_HEALTHCHECK_TIMEOUT` | `3s` | Bound on `harbormaster healthcheck` |
| `HARBORMASTER_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `HARBORMASTER_LOG_FORMAT` | `json` | `json` or `text` |

The container image overrides two of these: it binds `0.0.0.0:8080` and stores
the database at `/var/lib/harbormaster/harbormaster.db`, which is where the
named volume mounts.

The health check derives its URL from `HARBORMASTER_HTTP_ADDR` rather than
taking one of its own, so it cannot drift out of sync with the port the server
actually binds. A wildcard bind is probed over loopback.

`HARBORMASTER_INVENTORY_MASK_PATTERNS` defaults to
`PASSWORD,PASSWD,SECRET,TOKEN,API_KEY,APIKEY,PRIVATE_KEY,CREDENTIAL,AUTH`.
Setting it **replaces** that list rather than extending it, so to add a pattern
repeat the defaults and append yours.

Every value is validated at startup and the process refuses to start on a bad
one. The event-engine settings are validated **even when the engine is
disabled**: a configuration error that only surfaces the day someone flips
`EVENTS_ENABLED` to true is a worse failure than one caught at boot.

**Only one component runs the periodic full sweep.** With the event engine
enabled, `EVENTS_RECONCILE_INTERVAL` governs it and
`INVENTORY_REFRESH_INTERVAL` is suppressed; with the engine disabled, the
inventory's own interval takes over unchanged. Startup and manual refreshes are
unaffected either way. The startup log states which component owns it.

The default bind address is loopback so that running the bare binary never
exposes the API to the network by accident. The container image overrides it to
`0.0.0.0:8080`, where the network namespace is the isolation boundary and the
Compose file publishes the port to `127.0.0.1` on the host.

Configuration values are never written to the log. The same mechanism will
eventually carry registry credentials, so the redaction is built in from the
start rather than retrofitted.

## API

The contract is [`api/openapi.yaml`](api/openapi.yaml) (OpenAPI 3.1).

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/api/v1/health` | Database and Docker reachability |
| `GET` | `/api/v1/version` | Build metadata |
| `GET` | `/api/v1/inventory` | Inventory and refresh status |
| `POST` | `/api/v1/inventory/refresh` | Trigger a refresh (async, `202`) |
| `GET` | `/api/v1/inventory/filters` | Filter vocabularies actually present |
| `GET` | `/api/v1/containers` | Paged, filtered, sorted container list |
| `GET` | `/api/v1/containers/{id}` | One container's normalized detail |
| `GET` | `/api/v1/containers/{id}/raw` | Redacted raw inspection payload |
| `GET` | `/api/v1/images` | Images with reference counts |
| `GET` | `/api/v1/images/{id}` | One image |
| `GET` | `/api/v1/networks` | Networks |
| `GET` | `/api/v1/volumes` | Volumes |
| `GET` | `/api/v1/events` | Paged, filtered Docker event history |
| `GET` | `/api/v1/events/{id}` | One event by local sequence number |
| `GET` | `/api/v1/events/stream` | Live events over Server-Sent Events |
| `GET` | `/api/v1/event-engine` | Event-engine connection state and counters |
| `GET` | `/api/v1/event-filters` | Event filter vocabularies actually present |

Every path except the refresh returns `405` for any write method. There is no
event mutation endpoint of any kind, including pruning.

### Examples

```sh
# Inventory status
curl -s http://127.0.0.1:8080/api/v1/inventory | jq '{state, generation, counts}'

# Trigger a refresh; returns 202 immediately
curl -s -X POST http://127.0.0.1:8080/api/v1/inventory/refresh

# Unhealthy containers in one Compose project, newest first
curl -s 'http://127.0.0.1:8080/api/v1/containers?health=unhealthy&project=shop&sort=created&direction=desc'

# Containers carrying a label
curl -s 'http://127.0.0.1:8080/api/v1/containers?labelKey=tier&labelValue=frontend'

# One container by short ID
curl -s http://127.0.0.1:8080/api/v1/containers/abcdef012345 | jq '.security'

# Environment, showing that secrets are masked
curl -s http://127.0.0.1:8080/api/v1/containers/abcdef012345 | jq '.environment'

# Event-engine state: connected? how far behind? how many reconnects?
curl -s http://127.0.0.1:8080/api/v1/event-engine | jq '{state, lastEventAt, queueDepth, counters}'

# Everything that happened to one Compose project in the last hour
curl -s "http://127.0.0.1:8080/api/v1/events?project=shop&since=$(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ)"

# Events HarborMaster could not map onto a resource
curl -s 'http://127.0.0.1:8080/api/v1/events?result=warning' | jq '.items[] | {action, error}'

# Follow live events (Ctrl-C to stop)
curl -N http://127.0.0.1:8080/api/v1/events/stream
```

List responses carry an `items` array and a `pagination` object:

```json
{
  "items": [ ... ],
  "pagination": {
    "page": 2, "pageSize": 25, "totalItems": 57,
    "totalPages": 3, "hasNext": true, "hasPrevious": true
  }
}
```

Page size is capped at 200 and out-of-range values are **rejected with `400`**
rather than clamped: silently serving page 1 to a client that asked for page −3
hides a bug in the caller.

Sort fields are validated against a fixed allowlist. Nothing from a query string
is ever interpolated into SQL.

Status codes worth knowing: `400` invalid query parameter, `404` container not
found, `409` ambiguous ID prefix **or** refresh already running, `503` Docker
unreachable, inventory or event engine disabled, **or** the SSE subscriber limit
reached (with `Retry-After`).

`GET /api/v1/health` always returns `200` while the server is running. The
endpoint answering at all is the liveness signal; the body carries readiness:

```json
{
  "status": "degraded",
  "database": { "status": "up", "latencyMs": 1 },
  "docker": { "status": "down", "detail": "docker engine unreachable" },
  "checkedAt": "2026-08-03T09:20:11.482Z",
  "uptimeSeconds": 3600
}
```

`status` is `healthy` when everything is up, `degraded` when Docker is
unreachable **or the Docker event stream is disconnected** but HarborMaster is
still serving, and `unhealthy` when the database is unreachable. Clients must
branch on `status` rather than on the HTTP code, so they can distinguish
"HarborMaster is down" (no response at all) from "Docker is down" (`degraded`).

The event engine contributes an optional `events` component and never escalates
past `degraded`:

| Condition | Verdict |
| --- | --- |
| Event stream connected | not degraded |
| Event stream disconnected | `degraded` — periodic reconciliation still runs |
| Queue overflow pending reconciliation | `degraded` until the sweep completes |
| Event engine disabled by configuration | **not** degraded — a supported mode |
| Docker unreachable | `degraded`, unchanged from Phase 2 |
| Database unreachable | `unhealthy` |

A transient reconnect must never fail the container health check, or a daemon
restart would put HarborMaster into a restart loop. The native health check
continues to treat `degraded` as exit 0.

Errors use a stable envelope. Branch on `error.code`, not on the prose:

```json
{
  "error": { "code": "not_found", "message": "endpoint not found" },
  "requestId": "3f8a1c9e7b2d4a6f0c1e5b93"
}
```

`requestId` correlates the response with the server log, which holds the
detail the API deliberately withholds.

## Web interface

Six sections, served from the binary.

**Dashboard** — Docker and database connectivity, refresh state, generation,
last successful refresh and its duration, container counts (total, running,
stopped, paused, unhealthy), catalog counts (images, networks, volumes), and
the warnings from the last refresh. The **Refresh inventory** button disables
itself while a refresh runs, reports whether the refresh started, conflicted,
or was refused because Docker is unreachable, and reloads the metrics when the
generation advances.

The **Docker events** panel reports the event-engine connection state, the last
Docker event, the last reconciliation, the reconnect count, queue usage, and the
recorded-event count. When the stream is disconnected it says so explicitly and
explains the consequence: HarborMaster is relying on periodic reconciliation, so
container state may lag by up to one reconciliation interval.

**Containers** — a sortable table with server-side pagination, search, and
filters for state, health, Compose project, and image. Filters and sorting are
sent to the API; the browser never holds more than one page, which is why no
virtualisation is needed. Rows link to the detail view.

**Container detail** — tabs for Overview, Configuration, Environment, Mounts,
Networks, Ports, Resources, Security, Labels, Compose, Events, and Raw
inspection. Sensitive environment values render as `********` with a "masked"
marker and there is no control to reveal them. The raw payload is fetched only
when its tab is opened, and is redacted server-side. The Events tab shows a
bounded list of recent Docker events naming this container, served by the same
events API filtered on actor ID.

**Images** — repository tags, digests, creation time, size, platform, and how
many containers reference each image.

**Events** — the event-engine status panel, a bounded live view fed by
Server-Sent Events, and the server-side paginated history with filters for type,
action, Compose project, processing status, and free-text search.

The live and history views are deliberately distinct: live shows what has
arrived since the page opened and is capped at 250 rows, while history is the
complete record. Every row shows **both** the Docker timestamp and
HarborMaster's observed timestamp, because the gap between them is what tells
you the stream fell behind. Pausing stops the live list moving without closing
the connection, so resuming does not trigger a reconnect and replay. Status
badges distinguish processed, deduplicated, ignored, warning, and failed.

**Snapshots** remains an explicit empty state: its endpoints do not exist yet,
and placeholder rows in an operations tool are indistinguishable from real data
at a glance.

Every view has explicit loading, empty, disconnected, and error states, and
none of them displays invented data.

## Development

```sh
make help          # list every target
make test          # Go tests
make web-test      # frontend tests
make fmt vet       # format and vet Go sources
make ci            # everything CI runs
make docker-build  # build the container image
make docker-smoke  # build the image and run the container smoke tests
make compose-config # validate both Compose files
```

### Testing

Go tests use a real SQLite file in `t.TempDir()` rather than a mock, so
migrations, constraints, and the text time encoding are all exercised. The
Docker adapter is faked through the `docker.Runtime` interface, so no test
requires a running daemon — which matters because Docker is frequently absent
from CI and developer machines.

Normalization is tested in-package against real Docker SDK structs, since that
is the boundary being converted. Everything above the adapter is tested against
`docker.Fake`, which models the awkward cases deliberately: a container that
vanishes mid-refresh, an image that cannot be resolved, a daemon that is down.

A synthetic 1,000-container inventory test exercises persistence and paged
queries at the stated design target.

Storage failures are **induced rather than mocked**, because a mock of
`SQLITE_FULL` proves only that the mock works. A page limit produces a real
full-database error, a second connection holding `BEGIN IMMEDIATE` produces a
real lock conflict, overwriting content pages while leaving the header intact
produces the corruption a bad sector produces, and a handle abandoned without
`Close` leaves the hot write-ahead log a killed process leaves. Every
classification test asserts the specific verdict, and the remedy sweep carries
a positive control proving it can find what it looks for.

`FuzzOpenArbitraryDatabaseFile` feeds arbitrary bytes to the open path. The
property is not "bad input is rejected" but the stronger one: whatever is at
the path, `Open` either succeeds with a **usable, migrated** database or
returns an error — never a panic, never a hang, never a leaked handle on the
failure path.

Frontend tests run under Vitest with Testing Library, asserting on roles and
accessible names rather than class names. Filter, sort, and pagination tests
assert on the **request URL the UI produced**, which is how they verify the
work happens on the server rather than in the browser.

The event engine's tests never sleep to wait for a state change. Timing
behaviour — backoff growth, the cap, debouncing — is driven through injected
clock and jitter functions, so the *sequence* of delays is asserted rather than
the elapsed time. Everything else polls a condition and fails fast. A test that
sleeps for a fixed duration is slow, flaky, or both, and it hides the very
concurrency defect it was meant to catch.

The SSE hook takes its stream factory as an option, so frontend tests drive a
controllable fake rather than depending on jsdom having an `EventSource`.

`go test -race ./...` is a required gate and runs in CI on every push. The
race detector needs cgo and a C toolchain, so it may not run on a developer
machine without one; CI enforces it regardless.

A test in `internal/api` reads `api/openapi.yaml` and asserts the documented
paths and the routed paths are exactly the same set, in both directions, so the
spec cannot drift from the router silently.

## Security

The Docker socket is a privileged interface: anything able to reach it can
control every container on the host, which in most configurations is equivalent
to root. HarborMaster is built accordingly.

- **Read-only by construction.** `internal/docker` exposes only observations:
  `Ping`, `ListContainers`, `InspectContainer`, `InspectImage`, `ListNetworks`,
  `ListVolumes`, and `StreamEvents`. Subscribing to events changes nothing on
  the host. No code path in this build mutates a container, and there is no
  shell or command execution anywhere in the codebase. Docker SDK types never
  leave that package, and the event subscription hands out HarborMaster's own
  records rather than the SDK's or a raw channel.
- **Secrets are masked at the adapter boundary.** Environment values,
  log-driver options, and Docker **event actor attributes** matching the
  configured name patterns are replaced before they leave `internal/docker`, so
  no downstream call site can leak one by forgetting to mask. Raw values are
  never persisted, never logged, never in an API response or an SSE frame, and
  there is no parameter or UI control that reveals them. Docker registry
  authentication is never stored.
- **The live event stream is bounded and non-blocking.** Concurrent SSE
  subscribers are capped, each has a bounded buffer, and a subscriber that stops
  reading has events dropped for it alone rather than stalling event processing
  or holding memory. Replay is capped. No raw Docker payload reaches the wire.
- **Loopback by default.** The bare binary binds to `127.0.0.1`, and Compose
  publishes to `127.0.0.1` on the host. The server logs a warning if you bind
  it wider.
- **The socket is mounted read-only** in the Compose stack, and the container
  drops every capability, runs with `no-new-privileges`, a read-only root
  filesystem, and as UID 65532 rather than root. Note that `:ro` restricts the
  socket *file*, not the Docker *API* — see
  [Deploying on Linux](#deploying-on-linux) for why that distinction matters.
- **The runtime image has nothing to execute.** No shell, no package manager,
  no curl or wget. The container health check is a subcommand of the
  application binary rather than a shelled-out HTTP client.
- **Secrets are never logged.** Environment-variable values, registry
  credentials, and the `Authorization`, `Cookie`, `Proxy-Authorization`, and
  `X-Registry-Auth` headers are excluded from log records. The access log
  records method, path, status, duration, and request ID — nothing that can
  carry a token.
- **No stack traces cross the API boundary.** Panics are recovered, logged with
  their trace, and answered with a generic `500`. Docker and database errors
  are sanitised before they reach a response, since they embed socket paths and
  daemon internals.
- **Explicit limits everywhere.** Request bodies, header size, and all four
  server timeouts are bounded, so a slow or hostile client cannot hold
  connections open indefinitely.

### Read this before deploying

Six statements, all of them true today:

1. **HarborMaster changes a host only when a person asks, and only in three
   narrow ways** — pulling an approved digest-pinned image, recreating one
   container, and rolling one recreation back. All three are **off by default**,
   and when off the capability is absent rather than merely unused. Nothing here
   happens on a timer, on a schedule, or across a fleet, and there is no
   restart, no exec, and no image or volume deletion at all. Subscribing to the
   Docker event stream changes none of that: reading events is an observation,
   and an event only ever causes HarborMaster to *re-read* the host.
2. **Docker socket access remains highly privileged regardless.** The socket
   HarborMaster holds could do anything, whether or not HarborMaster uses it
   that way. Mounting it `:ro` restricts the socket *file*, not the Docker
   *API*. Treat access to this service as equivalent to root on the host.
3. **Authentication is implemented and cannot be disabled**, but HarborMaster
   does not terminate TLS. Four routes answer without a session — `GET /health`
   (reduced to one field for an anonymous caller), `GET /version`,
   `POST /auth/login`, and `GET /auth/bootstrap` — and nothing else does. What
   remains your job is the transport: put TLS in front of it, or keep it on
   loopback. There is also no second factor, so an administrator's password plus
   network reachability is a compromised installation.
4. **Event history is observational and incomplete by nature.** It records what
   HarborMaster observed, not what happened. Events emitted while the engine was
   disconnected, or before it first connected, were never seen and are not in
   the history. Full reconciliation restores **current-state** accuracy after
   missed events; it does not backfill the log.
5. **Event payloads are redacted**, and there is no unredacted copy anywhere.
   Variable and label *names* are still disclosed.
6. **Keep it on loopback**, or behind a trusted reverse proxy with access
   controls. Every deployment example in this README binds to `127.0.0.1` for
   this reason. If you front it with a proxy, note that the SSE endpoint needs
   response buffering disabled — HarborMaster sends `X-Accel-Buffering: no`,
   which nginx honours.

On masking specifically: environment values are masked in the API and the UI,
and raw values are not persisted. That is a meaningful reduction in exposure,
not a complete one — variable *names* are still disclosed, and names alone often
reveal which services and providers a host talks to. Configuration persistence
warrants continued security review as HarborMaster grows. Recreation does need
real values, and it handles them without ever holding one it can read: the live
configuration is captured into a value carrying unexported fields and handed
straight back to the daemon, and what the service and the API see is a
value-free projection in which a sensitive value contributes a keyed digest.

If you find a security problem, please open a private security advisory rather
than a public issue.

## Status

### What works today

- **Read-only Docker inventory**: every container in every state, normalized
  image metadata, runtime configuration, ports, mounts, networks, resources,
  security posture, logging, and Compose provenance
- **Refresh lifecycle**: at startup, on an interval, or on demand; bounded
  concurrency, per-refresh image caching, resilient per-container failure
  handling, and atomic persistence
- **Inventory checksum and generation** for change detection
- **Environment masking** by configurable name pattern, applied at the adapter
  boundary and never reversible through the API or UI
- **Docker event engine**: subscribes to the daemon's event stream, reconnects
  with bounded exponential backoff and jitter, deduplicates by deterministic
  fingerprint, debounces and coalesces refresh work, and escalates to full
  reconciliation whenever events may have been missed. Events are hints; every
  inventory write still comes from a fresh Docker inspection.
- **Event history** with server-side filtering, pagination, and configurable
  retention by age and count
- **Live updates over Server-Sent Events**, with `Last-Event-ID` replay,
  heartbeats, a subscriber cap, and bounded per-subscriber buffers
- **REST API**: inventory status and refresh, containers with server-side
  filtering, sorting and pagination, container detail, redacted raw inspection,
  images, networks, volumes, event history, event-engine status, and the live
  event stream
- **Configuration snapshots**: immutable captures of a container's
  configuration, deduplicated by checksum, with restore-readiness validation
  and configuration comparison. Readiness answers "could this be restored";
  HarborMaster does not restore.
- **Configuration drift**: the differences between a container's baseline
  snapshot and its configuration now, reusing the same comparison engine, with
  operator status transitions that never suppress re-evaluation
- **Policy engine**: administrator-defined rules that every container's
  configuration is checked against, as typed structs from a closed catalogue.
  Reporting only; nothing is enforced or remediated.
- **Image intelligence**: anonymous HTTPS lookups against registry manifest and
  tag-listing endpoints to discover whether a newer image is published.
  HarborMaster's only outbound egress, behind layered SSRF defences.
- **Change planning**: deterministic risk assessment of each proposed image
  change, combining every source above. Analysis only; nothing executes a plan.
- **Safe image acquisition**: downloading an approved, digest-pinned image into
  the local image store. Off by default, requested by a person, revalidated
  immediately before the transfer, and verified afterwards. It does not update
  containers.
- **Manual container recreation**: replacing one container with a new one built
  from its own configuration, on an already-verified local image. Off by
  default, requested by a person, revalidated immediately before the first
  mutation, checkpointed after every step, and proved four ways before the
  original is removed. There is no automatic rollback: a failure preserves both
  containers and records the manual steps.
- **Manual rollback**: undoing one recreation, when a person asks. Off by
  default and refused unless recreation is on. It stops the replacement, renames
  it aside, renames the preserved original back, starts it, and proves it four
  ways. It **removes nothing**, acts on one execution at a time, and derives
  every identity from HarborMaster's own record of that recreation.
- **Automatic updates**: administrator-defined update policies that let
  HarborMaster keep containers current unattended, inside a maintenance window.
  Off by default and refused unless recreation and rollback are both on. The
  engine holds no Docker capability: it submits the same requests a person would
  to the same three services, each of which re-runs its full preflight. A failed
  update is rolled back and the container is paused until somebody looks. Every
  pass records why each container was, or was not, updated.
- **Web interface**: Dashboard, Containers with detail, Images, Updates,
  Snapshots, Drift, Policies, Compliance, Change plans, Acquisitions,
  Recreations, Rollbacks, Automation, Update policies, Paused containers, and a
  live Events page
- HTTP server with health and version endpoints, graceful shutdown, and
  structured logging
- SQLite storage with embedded migrations
- Hardened distroless container image with a native `harbormaster healthcheck`
  command, multi-platform GHCR publishing with provenance and an SBOM, and
  hardened Compose and `docker run` deployments

### What is not built yet

- **Fleet management.** One host. The schema is keyed by host and exactly one
  host row exists; automation acts on the containers of that host and no other.
- **Rollback to an arbitrary image, configuration, or snapshot.** A rollback
  undoes one recorded recreation and derives every identity from that record.
- **Retrying a recreation.** HarborMaster stops at the first failure, and
  automation counts that failure rather than trying again immediately. A retry
  is a new plan and a new acquisition.
- **Choosing what automation updates to.** A policy names containers and a
  ceiling. There is no setting, API field, or label anywhere in the feature that
  carries an image, a tag, a digest, or a registry — the target comes from the
  planner and is re-verified before the pull.
- **Automation without a person having enabled three things.** Automatic updates
  are refused at startup unless recreation and rollback are both on, and no
  policy acts until an administrator moves it out of `observe` mode.
- Standalone container start, stop, restart, and removal. The five recreation
  lifecycle methods and the four rollback methods exist only inside their own
  pipelines and are not reachable from the API.
- Restore from a snapshot. Readiness validation answers whether it *could*
  work; nothing performs it.
- Image deletion and pruning. Acquisition adds to the local store, recreation
  removes only the container it replaced, rollback removes nothing at all, and
  none of them can remove an image or a volume.
- Notifications
- A second authentication factor, single sign-on, LDAP, OIDC, public
  registration, and password reset by email. Accounts are local, and recovery is
  another administrator or the console.
- An audit record of READ access. Writes and authorization decisions are
  recorded; who looked at what is not.
- Multi-host management — the schema is keyed by host, but exactly one host row
  exists
- Distributed event processing. Everything is in-process bounded queues and
  SQLite; there is no message broker, and no WebSocket endpoint.

### Known limitations of the event engine

- **Event history is not a complete record of the host.** Docker drops events
  while nothing is listening and replays nothing across a daemon restart, so a
  prolonged outage leaves a permanent gap in the log. Full reconciliation
  restores current-state accuracy; it does not backfill history.
- **Exact global ordering is not guaranteed by Docker.** The `sequence` field
  records the order HarborMaster observed events in, which is the only order it
  can honestly claim.
- **Image `delete` and `prune` events do not resolve to a single image**, so
  they defer to the next reconciliation rather than attempting a narrower pass
  that could not be correct.
- **Networks and volumes refresh as a set**, not one at a time: the read-only
  adapter lists them in a single call and has no per-resource inspect, so a
  "targeted" single-resource read would do the same work while widening the
  adapter surface for no gain.
- **A container event with no actor ID** cannot be targeted and escalates to a
  full reconciliation.

The order is deliberate. The observation and recovery machinery comes first, so
that by the time HarborMaster can change a container it can already undo it. The
first write capability — acquiring an image — was added only once snapshots,
drift, policy, registry intelligence, and risk assessment existed to decide
whether it should happen at all.

## License

[MIT](LICENSE).
