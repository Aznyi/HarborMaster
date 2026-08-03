#!/usr/bin/env bash
#
# Container smoke tests for the HarborMaster image.
#
# Runs the image the way production runs it -- non-root, read-only root
# filesystem, all capabilities dropped, no new privileges -- and asserts the
# application behaves. The hardening is part of what is under test: if a check
# fails, fix the image, do not relax the flags.
#
# The container is started WITHOUT a Docker socket on purpose. That exercises
# the degraded path: HarborMaster must serve, report `degraded`, and pass its
# own health check rather than crash-looping when Docker is unreachable.
#
# Usage:
#   deployments/smoke-test.sh                    # tests $IMAGE (default harbormaster:smoke)
#   IMAGE=ghcr.io/aznyi/harbormaster:edge deployments/smoke-test.sh
#   BUILD=1 deployments/smoke-test.sh            # build the image first
#
# Requires: docker, curl. Uses jq when present and falls back to grep when not.

set -euo pipefail

IMAGE="${IMAGE:-harbormaster:smoke}"
BUILD="${BUILD:-0}"
PORT="${PORT:-18080}"
CONTAINER="${CONTAINER:-harbormaster-smoke}"
VOLUME="${VOLUME:-harbormaster-smoke-data}"
HELPER_IMAGE="${HELPER_IMAGE:-busybox:1.37}"
READY_TIMEOUT="${READY_TIMEOUT:-60}"

BASE_URL="http://127.0.0.1:${PORT}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

passed=0
failed=0

# ---------------------------------------------------------------- helpers --

info() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

pass() {
  passed=$((passed + 1))
  printf '  \033[32mPASS\033[0m %s\n' "$*"
}

fail() {
  failed=$((failed + 1))
  printf '  \033[31mFAIL\033[0m %s\n' "$*"
}

# check <description> <expected> <actual>
check() {
  local description="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    pass "$description"
  else
    fail "$description (expected '$expected', got '$actual')"
  fi
}

# check_contains <description> <needle> <haystack>
check_contains() {
  local description="$1" needle="$2" haystack="$3"
  case "$haystack" in
  *"$needle"*) pass "$description" ;;
  *) fail "$description (expected output to contain '$needle')" ;;
  esac
}

# http_status <method> <path>
http_status() {
  curl -s -o /dev/null -w '%{http_code}' -X "$1" "${BASE_URL}${2}"
}

# http_body <path>
http_body() {
  curl -s "${BASE_URL}${1}"
}

json_field() {
  # json_field <json> <jq filter> <grep fallback pattern>
  if command -v jq >/dev/null 2>&1; then
    printf '%s' "$1" | jq -r "$2" 2>/dev/null || printf 'JQ_ERROR'
  else
    printf '%s' "$1" | grep -o "$3" || printf 'NOT_FOUND'
  fi
}

cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker volume rm -f "$VOLUME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Starts the container with the production hardening profile.
start_container() {
  docker run -d \
    --name "$CONTAINER" \
    --read-only \
    --tmpfs /tmp:rw,noexec,nosuid,size=16m \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    --user 65532:65532 \
    -p "127.0.0.1:${PORT}:8080" \
    -v "${VOLUME}:/var/lib/harbormaster" \
    "$IMAGE" >/dev/null
}

wait_for_ready() {
  local deadline=$((SECONDS + READY_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if curl -fsS "${BASE_URL}/api/v1/health" >/dev/null 2>&1; then
      return 0
    fi
    if [ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null)" != "true" ]; then
      echo "container exited early; logs follow:" >&2
      docker logs "$CONTAINER" >&2 || true
      return 1
    fi
    sleep 1
  done
  echo "timed out waiting for the API; logs follow:" >&2
  docker logs "$CONTAINER" >&2 || true
  return 1
}

# ------------------------------------------------------------------ build --

if [ "$BUILD" = "1" ]; then
  info "Building $IMAGE"
  docker build -t "$IMAGE" "$REPO_ROOT"
fi

info "Image builds and is present"
if docker image inspect "$IMAGE" >/dev/null 2>&1; then
  pass "image $IMAGE is available"
else
  fail "image $IMAGE is not available"
  echo
  echo "Smoke tests cannot continue without the image."
  exit 1
fi

# ------------------------------------------------- static image assertions --

info "Image configuration"

image_user="$(docker image inspect -f '{{.Config.User}}' "$IMAGE")"
check "image declares a non-root user" "65532:65532" "$image_user"

image_entrypoint="$(docker image inspect -f '{{json .Config.Entrypoint}}' "$IMAGE")"
check_contains "entrypoint is exec form" '["/usr/local/bin/harbormaster"' "$image_entrypoint"

image_healthcheck="$(docker image inspect -f '{{json .Config.Healthcheck.Test}}' "$IMAGE")"
check_contains "image declares a native health check" 'harbormaster' "$image_healthcheck"
check_contains "health check is exec form (CMD, not CMD-SHELL)" '"CMD"' "$image_healthcheck"

for label in \
  org.opencontainers.image.title \
  org.opencontainers.image.description \
  org.opencontainers.image.source \
  org.opencontainers.image.revision \
  org.opencontainers.image.version \
  org.opencontainers.image.created \
  org.opencontainers.image.licenses; do
  value="$(docker image inspect -f "{{index .Config.Labels \"${label}\"}}" "$IMAGE")"
  if [ -n "$value" ] && [ "$value" != "<no value>" ]; then
    pass "label $label is set"
  else
    fail "label $label is missing"
  fi
done

image_size="$(docker image inspect -f '{{.Size}}' "$IMAGE")"
printf '  \033[36mINFO\033[0m image size: %s bytes (%s MiB)\n' \
  "$image_size" "$((image_size / 1024 / 1024))"

# ------------------------------------------------------------ run and probe --

info "Starting container without a Docker socket (degraded path)"
start_container
if ! wait_for_ready; then
  fail "container did not become ready"
  echo
  echo "Smoke tests cannot continue."
  exit 1
fi
pass "application started and serves without Docker access"

info "Runtime identity"
runtime_user="$(docker top "$CONTAINER" -o user 2>/dev/null | tail -n +2 | head -n 1 | tr -d '[:space:]')"
if [ "$runtime_user" = "0" ] || [ "$runtime_user" = "root" ]; then
  fail "process runs as root (user column: '$runtime_user')"
else
  pass "process runs as non-root (user column: '$runtime_user')"
fi

info "API endpoints"

check "GET /api/v1/version returns 200" "200" "$(http_status GET /api/v1/version)"
version_body="$(http_body /api/v1/version)"
check_contains "version payload carries a version field" '"version"' "$version_body"
check_contains "version payload carries a platform field" '"platform"' "$version_body"

check "GET /api/v1/health returns 200" "200" "$(http_status GET /api/v1/health)"
health_body="$(http_body /api/v1/health)"
health_status="$(json_field "$health_body" '.status' '"status":"[a-z]*"')"
check_contains "health reports degraded without Docker" "degraded" "$health_status"
check_contains "health reports the database as up" '"status":"up"' "$health_body"

info "Frontend serving"

check "GET / returns 200" "200" "$(http_status GET /)"
check_contains "GET / serves the frontend shell" "HarborMaster" "$(http_body /)"
check "GET /containers returns 200 (SPA fallback)" "200" "$(http_status GET /containers)"
check_contains "SPA fallback serves the shell, not a 404 page" "HarborMaster" "$(http_body /containers)"

info "API error handling"

check "unknown API route returns 404" "404" "$(http_status GET /api/v1/does-not-exist)"
not_found_body="$(http_body /api/v1/does-not-exist)"
not_found_code="$(json_field "$not_found_body" '.error.code' '"code":"[a-z_]*"')"
check_contains "unknown API route returns JSON, not HTML" "not_found" "$not_found_code"

check "POST to a read-only endpoint returns 405" "405" "$(http_status POST /api/v1/health)"
check "PUT to a read-only endpoint returns 405" "405" "$(http_status PUT /api/v1/version)"

info "Native health-check command"

# No shell involved: docker exec runs the binary directly, which is the whole
# point of shipping the check as a subcommand.
if docker exec "$CONTAINER" /usr/local/bin/harbormaster healthcheck; then
  pass "harbormaster healthcheck exits 0 while degraded"
else
  fail "harbormaster healthcheck exited nonzero while degraded"
fi

# The runtime's own view of the same check.
health_deadline=$((SECONDS + 90))
docker_health="starting"
while [ "$SECONDS" -lt "$health_deadline" ]; do
  docker_health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$CONTAINER")"
  [ "$docker_health" = "starting" ] || break
  sleep 2
done
check "docker reports the container healthy" "healthy" "$docker_health"

info "Read-only root filesystem"

readonly_rootfs="$(docker inspect -f '{{.HostConfig.ReadonlyRootfs}}' "$CONTAINER")"
check "container runs with a read-only root filesystem" "true" "$readonly_rootfs"

# Behavioural, not just declarative: with a read-only rootfs and the database on
# a volume, nothing should have been written to the container layer. Volume and
# tmpfs mount points may appear as directory entries, which is expected.
container_diff="$(docker diff "$CONTAINER" | grep -v '^C /var/lib/harbormaster$' | grep -v '^C /tmp$' || true)"
if [ -z "$container_diff" ]; then
  pass "no writes to the container filesystem outside declared mounts"
else
  fail "unexpected writes to the container filesystem:"
  printf '%s\n' "$container_diff" | sed 's/^/         /'
fi

info "Database persistence across container recreation"

db_before="$(docker run --rm -v "${VOLUME}:/data" "$HELPER_IMAGE" \
  sh -c 'stat -c "%i %n" /data/harbormaster.db 2>/dev/null || echo MISSING')"
if [ "$db_before" = "MISSING" ]; then
  fail "database was not created in the named volume"
else
  pass "database created in the named volume ($db_before)"
fi

docker rm -f "$CONTAINER" >/dev/null
start_container
if wait_for_ready; then
  pass "recreated container starts against the existing volume"
else
  fail "recreated container did not become ready"
fi

db_after="$(docker run --rm -v "${VOLUME}:/data" "$HELPER_IMAGE" \
  sh -c 'stat -c "%i %n" /data/harbormaster.db 2>/dev/null || echo MISSING')"
check "database file survives container recreation" "$db_before" "$db_after"

# ---------------------------------------------------------------- summary --

info "Summary"
printf '  %d passed, %d failed\n\n' "$passed" "$failed"

if [ "$failed" -ne 0 ]; then
  echo "Smoke tests failed. Do not weaken the security flags above to make them pass."
  exit 1
fi

echo "All container smoke tests passed."
