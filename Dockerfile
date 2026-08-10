# syntax=docker/dockerfile:1

# HarborMaster container image.
#
# Three stages: build the frontend, build the static Go binary with that bundle
# embedded, then copy the single binary onto a distroless base. The runtime
# stage has no shell, no package manager, and no interpreter, so a compromised
# HarborMaster process has nothing on disk to execute. That is also why the
# health check is a subcommand of the binary rather than a curl invocation.
#
# Build:
#   docker build -t harbormaster:dev .
#
# Base images are pinned to explicit patch versions. Pinning by digest is
# stronger still and is the intended follow-up; it needs a registry lookup that
# the pinned tags below stand in for.

ARG NODE_IMAGE=node:22.23.2-alpine3.24
ARG GO_IMAGE=golang:1.26.5-alpine3.24
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian13:nonroot

# ---------------------------------------------------------------- frontend --
FROM ${NODE_IMAGE} AS web

WORKDIR /src/web

# Dependencies first: this layer is cached until the lockfile changes.
# `npm ci` installs exactly what package-lock.json pins.
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY web/ ./

# Type-checks and bundles into /src/web/dist, which the Go stage embeds.
RUN npm run build

# ----------------------------------------------------------------- backend --
FROM ${GO_IMAGE} AS build

WORKDIR /src

# Modules before sources, for the same caching reason.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY web/embed.go ./web/

# The frontend built above, not whatever the host had lying around. The SQLite
# migrations under internal/store/migrations are embedded by the same build.
COPY --from=web /src/web/dist/ ./web/dist/

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# CGO off: the SQLite driver is pure Go, so the result is a static binary that
# needs no libc in the runtime stage.
#
# -trimpath strips host filesystem paths from the binary; -s -w drop the symbol
# table and DWARF debug info, which shrinks the image and removes detail that
# would otherwise help someone probing a running process.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath \
    -ldflags "-s -w \
      -X github.com/Aznyi/HarborMaster/internal/version.version=${VERSION} \
      -X github.com/Aznyi/HarborMaster/internal/version.commit=${COMMIT} \
      -X github.com/Aznyi/HarborMaster/internal/version.buildDate=${BUILD_DATE}" \
    -o /out/harbormaster ./cmd/harbormaster

# Stage the data directory here, with the runtime user's ownership already
# applied. The distroless base has no shell, so `mkdir` and `chown` cannot run
# there — and the directory must exist in the image, because Docker seeds a new
# named volume from the image content at the mount point, ownership included.
# Without this, Docker would create the mount point as root and the
# unprivileged process could not write its database.
RUN install -d -o 65532 -g 65532 -m 0750 /out/state/var/lib/harbormaster

# ----------------------------------------------------------------- runtime --
FROM ${RUNTIME_IMAGE}

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# The source label is what connects the published package to this repository in
# the GitHub Packages UI.
LABEL org.opencontainers.image.title="HarborMaster" \
      org.opencontainers.image.description="Safety-first container lifecycle manager: Docker inventory, configuration snapshots, drift and compliance, image intelligence, change planning, and verified container updates with rollback. Every capability that changes a host is off by default." \
      org.opencontainers.image.source="https://github.com/Aznyi/HarborMaster" \
      org.opencontainers.image.url="https://github.com/Aznyi/HarborMaster" \
      org.opencontainers.image.documentation="https://github.com/Aznyi/HarborMaster/blob/main/README.md" \
      org.opencontainers.image.vendor="HarborMaster contributors" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.base.name="gcr.io/distroless/static-debian13:nonroot"

COPY --from=build /out/harbormaster /usr/local/bin/harbormaster

# The staged directory, ownership intact. Trailing slashes on both sides so the
# contents land at the destination path rather than inside a nested directory.
COPY --from=build --chown=65532:65532 /out/state/var/lib/harbormaster/ /var/lib/harbormaster/

# 65532:65532 is distroless's `nonroot` user. HarborMaster never needs root: it
# reads the Docker socket and writes one SQLite file. Numeric form so that
# `--read-only` and user-namespace remapping behave predictably, and so
# orchestrators can assert the UID without resolving a name.
USER 65532:65532

WORKDIR /var/lib/harbormaster

# Declared so the database survives container replacement even when the
# operator forgets `-v`. Mount a named volume here in production.
VOLUME ["/var/lib/harbormaster"]

# Inside a container the network namespace is the isolation boundary, so
# binding to all interfaces is correct here even though the bare-binary default
# is loopback.
#
# Publish the port to 127.0.0.1 on the host unless a TLS-terminating proxy sits
# in front of it. HarborMaster authenticates every request and there is no
# setting that disables that, but it speaks plain HTTP: the session cookie
# needs a trusted network path or a proxy that provides one.
ENV HARBORMASTER_HTTP_ADDR=0.0.0.0:8080 \
    HARBORMASTER_DB_PATH=/var/lib/harbormaster/harbormaster.db \
    HARBORMASTER_DOCKER_HOST=unix:///var/run/docker.sock \
    HARBORMASTER_LOG_FORMAT=json \
    HARBORMASTER_HEALTHCHECK_TIMEOUT=3s

EXPOSE 8080

# Exec form, invoking the binary directly: there is no shell in this image to
# interpret a string-form command. `healthcheck` exits 0 for healthy and for
# degraded, so an unreachable Docker socket does not cause a restart loop.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/harbormaster", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/harbormaster"]
CMD ["serve"]
