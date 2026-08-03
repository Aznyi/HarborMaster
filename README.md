# HarborMaster

Safety-first container lifecycle manager.

HarborMaster watches the containers you already run and gives you the things
you need before you change one: an accurate inventory, a configuration snapshot
you can compare against, a health verdict you can trust, and a record of what
happened. The eventual goal is image updates with validation and rollback. The
order matters — the observation and recovery machinery is built first, so that
by the time HarborMaster can change a container it can already undo it.

> **Read-only today.** HarborMaster inventories the containers, images,
> networks, and volumes on a Docker host and reports their normalized
> configuration. It cannot pull images, create, recreate, update, restart, or
> remove containers, and it has no command-execution path. See
> [Status](#status) for what is and is not here yet.

## Contents

- [Quick start](#quick-start)
- [Deploying on Linux](#deploying-on-linux)
- [Container image](#container-image)
- [Inventory](#inventory)
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
> **HarborMaster has no authentication yet. Do not expose it to the public
> internet.** Publish it to `127.0.0.1` and reach it over SSH or a VPN, or put
> an authenticating reverse proxy in front of it. Every command below binds to
> loopback for exactly this reason.

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
| `-p 127.0.0.1:8080:8080` | Publishes 8080 on loopback only. `-p 8080:8080` would publish on **every** interface — do not use it without authentication in front. |
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

### The read-only guarantee

`internal/docker` is the only package that talks to the Docker Engine, and it
exposes exactly five operations: `Ping`, `ListContainers`, `InspectContainer`,
`InspectImage`, `ListNetworks`, and `ListVolumes`. Every one is an observation.
There is no method that creates, starts, stops, removes, pulls, or executes
anything, and no accessor that hands the underlying SDK client to another
package. Gaining the ability to mutate Docker requires editing that package,
which makes it visible in a diff.

The API has one endpoint that accepts a write method,
`POST /api/v1/inventory/refresh`. It re-reads the host and replaces
HarborMaster's own records. It changes nothing on the Docker host.

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
value, and no UI control to unmask one — with no authentication in front of the
service, such a switch would be an unauthenticated secret-disclosure API.

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

## Architecture

Layers depend inward. Nothing in `domain` imports an adapter, and services
depend on interfaces rather than concrete clients, so every layer is testable
without a Docker daemon or a database file.

Docker SDK types never leave `internal/docker`. Domain models, services,
repositories, API handlers, OpenAPI schemas, and the frontend all speak
HarborMaster's own types, which is what would let a second runtime adapter be
added without touching the service or API layers.

```
cmd/harbormaster      Composition root: config, wiring, signals, shutdown
  └── internal/api        REST handlers, middleware, SPA static serving
        └── internal/service   Application logic (health checks)
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

- **`internal/docker`** is the only package that talks to the Engine, and it
  exposes exactly one operation, `Ping`. Gaining write access to Docker
  requires editing this package, which makes that change reviewable in a diff.
- **`internal/api`** is the only package that renders errors to a client. It
  logs the real error and returns a stable code with a generic message, so
  stack traces and socket paths never leave the process.

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
| `HARBORMASTER_INVENTORY_ENABLED` | `true` | Turn the inventory engine on or off |
| `HARBORMASTER_INVENTORY_REFRESH_ON_STARTUP` | `true` | Collect once at boot |
| `HARBORMASTER_INVENTORY_REFRESH_INTERVAL` | `60s` | Periodic refresh; `0` disables it |
| `HARBORMASTER_INVENTORY_WORKERS` | `8` | Concurrent inspections (1–64) |
| `HARBORMASTER_INVENTORY_ABSENT_RETENTION` | `168h` | How long absent containers are kept |
| `HARBORMASTER_INVENTORY_MASK_PATTERNS` | see below | Names treated as secret-bearing |
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
one.

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

Every path except the refresh returns `405` for any write method.

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
unreachable or inventory disabled.

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
unreachable but HarborMaster is still serving, and `unhealthy` when the
database is unreachable. Clients must branch on `status` rather than on the
HTTP code, so they can distinguish "HarborMaster is down" (no response at all)
from "Docker is down" (`degraded`).

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

Five sections, served from the binary.

**Dashboard** — Docker and database connectivity, refresh state, generation,
last successful refresh and its duration, container counts (total, running,
stopped, paused, unhealthy), catalog counts (images, networks, volumes), and
the warnings from the last refresh. The **Refresh inventory** button disables
itself while a refresh runs, reports whether the refresh started, conflicted,
or was refused because Docker is unreachable, and reloads the metrics when the
generation advances.

**Containers** — a sortable table with server-side pagination, search, and
filters for state, health, Compose project, and image. Filters and sorting are
sent to the API; the browser never holds more than one page, which is why no
virtualisation is needed. Rows link to the detail view.

**Container detail** — tabs for Overview, Configuration, Environment, Mounts,
Networks, Ports, Resources, Security, Labels, Compose, and Raw inspection.
Sensitive environment values render as `********` with a "masked" marker and
there is no control to reveal them. The raw payload is fetched only when its
tab is opened, and is redacted server-side.

**Images** — repository tags, digests, creation time, size, platform, and how
many containers reference each image.

**Snapshots** and **Events** remain explicit empty states: their endpoints do
not exist yet, and placeholder rows in an operations tool are indistinguishable
from real data at a glance.

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

Frontend tests run under Vitest with Testing Library, asserting on roles and
accessible names rather than class names. Filter, sort, and pagination tests
assert on the **request URL the UI produced**, which is how they verify the
work happens on the server rather than in the browser.

## Security

The Docker socket is a privileged interface: anything able to reach it can
control every container on the host, which in most configurations is equivalent
to root. HarborMaster is built accordingly.

- **Read-only by construction.** `internal/docker` exposes only observations:
  `Ping`, `ListContainers`, `InspectContainer`, `InspectImage`, `ListNetworks`,
  `ListVolumes`. No code path in this build mutates a container, and there is no
  shell or command execution anywhere in the codebase. Docker SDK types never
  leave that package.
- **Secrets are masked at the adapter boundary.** Environment values and
  log-driver options matching the configured name patterns are replaced before
  they leave `internal/docker`, so no downstream call site can leak one by
  forgetting to mask. Raw values are never persisted, never logged, never in an
  API response, and there is no parameter or UI control that reveals them.
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

Four statements, all of them true today:

1. **HarborMaster does not update containers.** It observes them. There is no
   image pull, no recreation, no restart, no rollback, and no exec.
2. **Docker socket access remains highly privileged despite HarborMaster's
   read-only behaviour.** The application only reads, but the socket it holds
   could do anything. Mounting it `:ro` restricts the socket *file*, not the
   Docker *API*. Treat access to this service as equivalent to root on the host.
3. **Authentication is not implemented.** Anyone who can reach the port can read
   your entire container inventory — image references, mounts, network layout,
   labels, and the *names* of every environment variable.
4. **Keep it on loopback**, or behind a trusted reverse proxy with access
   controls. Every deployment example in this README binds to `127.0.0.1` for
   this reason.

On masking specifically: environment values are masked in the API and the UI,
and raw values are not persisted. That is a meaningful reduction in exposure,
not a complete one — variable *names* are still disclosed, and names alone often
reveal which services and providers a host talks to. Configuration persistence
warrants continued security review as HarborMaster grows, particularly when a
future phase needs real values to recreate a container.

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
- **REST API**: inventory status and refresh, containers with server-side
  filtering, sorting and pagination, container detail, redacted raw inspection,
  images, networks, volumes
- **Web interface**: populated Dashboard, Containers table, container detail
  with eleven sections, and Images
- HTTP server with health and version endpoints, graceful shutdown, and
  structured logging
- SQLite storage with embedded migrations
- Hardened distroless container image with a native `harbormaster healthcheck`
  command, multi-platform GHCR publishing with provenance and an SBOM, and
  hardened Compose and `docker run` deployments

### What is not built yet

- Configuration snapshots and drift detection — the snapshot store exists from
  the earlier phase, but no service captures one. Snapshots and the current
  inventory are separate concepts and remain separate.
- Docker event streaming; the inventory is polled, not subscribed
- Image update checks, image pulls, and container recreation
- Container start, stop, restart, and removal
- Health validation with automatic rollback, and deployment history
- Notifications
- Authentication and authorization
- Multi-host management — the schema is keyed by host, but exactly one host row
  exists

The order is deliberate. The observation and recovery machinery comes first, so
that by the time HarborMaster can change a container it can already undo it.

## License

[MIT](LICENSE).
