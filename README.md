# HarborMaster

Safety-first container lifecycle manager.

HarborMaster watches the containers you already run and gives you the things
you need before you change one: an accurate inventory, a configuration snapshot
you can compare against, a health verdict you can trust, and a record of what
happened. The eventual goal is image updates with validation and rollback. The
order matters — the observation and recovery machinery is built first, so that
by the time HarborMaster can change a container it can already undo it.

> **Read-only today.** HarborMaster currently inventories Docker connectivity
> and provides the snapshot store. It cannot pull images, create, recreate,
> update, restart, or remove containers, and it has no command-execution path.
> See [Status](#status) for what is and is not here yet.

## Contents

- [Quick start](#quick-start)
- [Deploying on Linux](#deploying-on-linux)
- [Container image](#container-image)
- [Architecture](#architecture)
- [Configuration](#configuration)
- [API](#api)
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

## Architecture

Layers depend inward. Nothing in `domain` imports an adapter, and services
depend on interfaces rather than concrete clients, so every layer is testable
without a Docker daemon or a database file.

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
| `HARBORMASTER_HEALTHCHECK_TIMEOUT` | `3s` | Bound on `harbormaster healthcheck` |
| `HARBORMASTER_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `HARBORMASTER_LOG_FORMAT` | `json` | `json` or `text` |

The container image overrides two of these: it binds `0.0.0.0:8080` and stores
the database at `/var/lib/harbormaster/harbormaster.db`, which is where the
named volume mounts.

The health check derives its URL from `HARBORMASTER_HTTP_ADDR` rather than
taking one of its own, so it cannot drift out of sync with the port the server
actually binds. A wildcard bind is probed over loopback.

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
Docker adapter is faked through the `docker.Pinger` interface, so no test
requires a running daemon.

Frontend tests run under Vitest with Testing Library, asserting on roles and
accessible names rather than class names.

## Security

The Docker socket is a privileged interface: anything able to reach it can
control every container on the host, which in most configurations is equivalent
to root. HarborMaster is built accordingly.

- **Read-only by construction.** `internal/docker` exposes only `Ping`. There
  is no code path in this build that mutates a container, and no shell or
  command execution anywhere in the codebase.
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

### Not yet present

HarborMaster has **no authentication or authorization**. Do not expose it
beyond loopback without putting an authenticating reverse proxy in front of it.

If you find a security problem, please open a private security advisory rather
than a public issue.

## Status

### What works today

- HTTP server with health and version endpoints, graceful shutdown, and
  structured logging
- Docker Engine reachability reporting, degrading rather than failing when the
  socket is absent
- SQLite storage with embedded migrations, and append-only snapshot and event
  repositories
- Responsive web interface, embedded in the binary
- Hardened container image on distroless, with a native `harbormaster
  healthcheck` command
- Multi-platform publishing to GHCR with build provenance and an SBOM
- Hardened Compose and `docker run` deployments, with container smoke tests

### What is not built yet

- Container inventory endpoints — the domain model exists, but nothing
  populates it
- Snapshot capture and comparison — storage and repositories are in place, but
  no service writes a snapshot
- Endpoints for listing snapshots and events
- Image update checks, image pulls, and container recreation
- Health validation with automatic rollback, and deployment history
- Notifications
- Authentication and authorization

The order is deliberate. The observation and recovery machinery comes first, so
that by the time HarborMaster can change a container it can already undo it.

## License

[MIT](LICENSE).
