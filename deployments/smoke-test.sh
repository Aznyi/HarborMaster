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

# ------------------------------------------------------- Git Bash / MSYS2 --
#
# Under Git Bash on Windows, MSYS2 rewrites anything that looks like a Unix
# path before it reaches a native .exe. `docker exec ... /usr/local/bin/...`
# arrives as `C:/Program Files/Git/usr/local/bin/...`, and every in-container
# path assertion fails for a reason that has nothing to do with the image.
#
# Disabling the conversion for this script is the whole fix. Harmless
# everywhere else: the variables mean nothing to a non-MSYS shell.
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

IMAGE="${IMAGE:-harbormaster:smoke}"
BUILD="${BUILD:-0}"
PORT="${PORT:-18080}"
CONTAINER="${CONTAINER:-harbormaster-smoke}"
VOLUME="${VOLUME:-harbormaster-smoke-data}"
HELPER_IMAGE="${HELPER_IMAGE:-busybox:1.37}"
READY_TIMEOUT="${READY_TIMEOUT:-60}"

# The account the smoke test claims the installation with. Disposable: the
# container and its volume are destroyed on exit.
SMOKE_USER="${SMOKE_USER:-smoketest}"
SMOKE_PASSWORD="${SMOKE_PASSWORD:-Smoke-Test-Passphrase-42}"

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

# status_of <curl args...> -- prints an HTTP status, or "CURL_<n>" on failure.
#
# # Why the body is discarded in the shell rather than with `-o /dev/null`
#
# Path conversion is disabled for this script (see the top), so under Git Bash
# a native curl receives the literal string `/dev/null` -- which is not a path
# on Windows. curl still performs the request and still reports the status, but
# exits 23 (write error). In a bare assignment that aborts the whole run under
# `set -e`, which is how this script used to stop halfway with no summary.
#
# Appending the status to the body and taking the last line needs no special
# file at all, so it behaves the same on every platform.
#
# It also never returns non-zero. A harness must REPORT a transport failure as
# a failed assertion, not become one.
status_of() {
  local out rc=0
  out="$(curl -s -w '\n%{http_code}' "$@" 2>/dev/null)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    printf 'CURL_%s' "$rc"
    return 0
  fi
  printf '%s' "${out##*$'\n'}"
}

# body_of <curl args...> -- prints a response body, empty on transport failure.
body_of() {
  curl -s "$@" 2>/dev/null || true
}

# http_status <method> <path> -- unauthenticated.
http_status() {
  status_of -X "$1" "${BASE_URL}${2}"
}

# http_body <path> -- unauthenticated.
http_body() {
  body_of "${BASE_URL}${1}"
}

# ------------------------------------------------------------ auth helpers --
#
# Every route but four needs a session, and every WRITE additionally needs the
# CSRF token derived from it. These wrap that so an assertion reads as what it
# is testing rather than as curl plumbing.
#
# The cookie jar lives in a temporary directory that the cleanup trap removes.

COOKIE_DIR="$(mktemp -d)"
COOKIE_JAR="${COOKIE_DIR}/cookies.txt"

# Under Git Bash, curl is usually a NATIVE Windows binary while the path above
# is an MSYS one. Path conversion is disabled for this script (see the top), so
# the two disagree and curl cannot write its jar -- it exits 23 and, under
# `set -e`, takes the whole run with it.
#
# cygpath is the translation MSYS itself uses. Absent everywhere else, where
# the path is already correct.
if command -v cygpath >/dev/null 2>&1; then
  COOKIE_JAR="$(cygpath -w "$COOKIE_JAR")"
fi

CSRF=""

# auth_post <path> <body> -- unauthenticated POST, used for bootstrap and login.
auth_post() {
  body_of -c "$COOKIE_JAR" -b "$COOKIE_JAR" \
    -H 'Content-Type: application/json' \
    -X POST -d "$2" "${BASE_URL}${1}"
}

# auth_post_status <path> <body>
auth_post_status() {
  status_of -c "$COOKIE_JAR" -b "$COOKIE_JAR" \
    -H 'Content-Type: application/json' \
    -X POST -d "$2" "${BASE_URL}${1}"
}

# authed_status <method> <path> [body] -- session cookie plus CSRF on writes.
authed_status() {
  local method="$1" path="$2" body="${3:-}"
  if [ "$method" = "GET" ]; then
    status_of -b "$COOKIE_JAR" -X GET "${BASE_URL}${path}"
  else
    status_of -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
      -H 'Content-Type: application/json' \
      -H "X-HarborMaster-CSRF: ${CSRF}" \
      -X "$method" ${body:+-d "$body"} "${BASE_URL}${path}"
  fi
}

# authed_body <path>
authed_body() {
  body_of -b "$COOKIE_JAR" "${BASE_URL}${1}"
}

# nocsrf_status <method> <path> <body> -- a cookie with no CSRF header at all.
nocsrf_status() {
  status_of -b "$COOKIE_JAR" \
    -H 'Content-Type: application/json' \
    -X "$1" -d "$3" "${BASE_URL}${2}"
}

# badcsrf_status <method> <path> <body> -- a cookie with the WRONG CSRF header.
badcsrf_status() {
  status_of -b "$COOKIE_JAR" \
    -H 'Content-Type: application/json' \
    -H 'X-HarborMaster-CSRF: 0000000000000000000000000000000000000000000000000000000000000000' \
    -X "$1" -d "$3" "${BASE_URL}${2}"
}

json_field() {
  # json_field <json> <jq filter> <grep fallback pattern>
  if command -v jq >/dev/null 2>&1; then
    printf '%s' "$1" | jq -r "$2" 2>/dev/null || printf 'JQ_ERROR'
  else
    printf '%s' "$1" | grep -o "$3" || printf 'NOT_FOUND'
  fi
}

# Reads the running process's UID via `docker top`.
#
# Invoked with NO ps arguments on purpose. The daemon runs `ps` on the host and
# then requires a PID column so it can map rows back to the container's
# processes; passing something like `-o user` yields a lone USER column and the
# request fails with "Couldn't find PID field in ps output". The daemon's
# default (`-ef`) always includes PID.
#
# The UID column is located by header name, because ps layout varies by host.
docker_top_uid() {
  local output
  output="$(docker top "$1" 2>/dev/null)" || return 1

  printf '%s\n' "$output" | awk '
    NR == 1 {
      for (i = 1; i <= NF; i++) {
        if ($i == "UID" || $i == "USER") { col = i }
      }
      if (col == 0) { exit 1 }
      next
    }
    { print $col; exit }
  '
}

cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker volume rm -f "$VOLUME" >/dev/null 2>&1 || true
  # The jar holds a live session token until the volume above is gone.
  rm -rf "$COOKIE_DIR" >/dev/null 2>&1 || true
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

# Identity is asserted from outside the container. The runtime image is
# distroless: there is no shell, no `id`, and no `ps` in it to run, which is
# the point -- a compromised process has nothing on disk to execute.
#
# `docker inspect` reports the configured identity, which is intent.
container_user="$(docker inspect -f '{{.Config.User}}' "$CONTAINER")"
check "container is configured to run as 65532:65532" "65532:65532" "$container_user"

# An empty User means the image's default applies, which for most bases is
# root. Treated as a failure rather than a pass-by-omission.
case "$container_user" in
"" | 0 | 0:* | root | root:*)
  fail "container would run as root (configured user: '$container_user')"
  ;;
*)
  pass "configured user is neither root nor unset"
  ;;
esac

# ...and this is what the kernel actually granted, which is what matters.
# /proc/<pid>/status is world-readable, so the host can read the effective UID
# and GID of the container's process without entering the container. It exists
# only when the daemon shares the host kernel, so a VM-backed Docker (Docker
# Desktop) falls through to `docker top`.
container_pid="$(docker inspect -f '{{.State.Pid}}' "$CONTAINER")"
case "$container_pid" in
'' | *[!0-9]*) container_pid=0 ;;
esac

if [ "$container_pid" -gt 0 ] && [ -r "/proc/${container_pid}/status" ]; then
  # The Uid: and Gid: lines are "<real> <effective> <saved> <filesystem>".
  effective_uid="$(awk '/^Uid:/ {print $3; exit}' "/proc/${container_pid}/status")"
  effective_gid="$(awk '/^Gid:/ {print $3; exit}' "/proc/${container_pid}/status")"
  check "process runs with effective UID 65532" "65532" "$effective_uid"
  check "process runs with effective GID 65532" "65532" "$effective_gid"
elif runtime_uid="$(docker_top_uid "$CONTAINER")" && [ -n "$runtime_uid" ]; then
  check "process runs as UID 65532 (via docker top)" "65532" "$runtime_uid"
else
  # Not skipped quietly: an unverifiable security assertion is a failed one.
  fail "could not determine the running process's UID by any available method"
fi

info "The anonymous surface"

# Exactly four routes answer without a session. Everything else must be 401,
# and this section is what would fail if a fifth ever appeared.

check "GET /api/v1/version returns 200" "200" "$(http_status GET /api/v1/version)"
version_body="$(http_body /api/v1/version)"
check_contains "version payload carries a version field" '"version"' "$version_body"
check_contains "version payload carries a platform field" '"platform"' "$version_body"

check "GET /api/v1/health returns 200" "200" "$(http_status GET /api/v1/health)"
health_body="$(http_body /api/v1/health)"
health_status="$(json_field "$health_body" '.status' '"status":"[a-z]*"')"
check_contains "health reports degraded without Docker" "degraded" "$health_status"

# The anonymous health body is REDUCED to the overall status. Component detail
# -- the database, the Docker connection, versions -- is for an authenticated
# caller, because it describes the host.
if printf '%s' "$health_body" | grep -q '"database"'; then
  fail "the anonymous health response discloses component detail"
else
  pass "the anonymous health response is reduced to the overall status"
fi

check "GET /api/v1/auth/bootstrap returns 200" "200" "$(http_status GET /api/v1/auth/bootstrap)"
check_contains "bootstrap status says whether an administrator exists" \
  '"completed"' "$(http_body /api/v1/auth/bootstrap)"

for guarded in /api/v1/containers /api/v1/events /api/v1/event-engine \
  /api/v1/snapshots /api/v1/images /api/v1/plans /api/v1/audit; do
  check "anonymous GET $guarded returns 401" "401" "$(http_status GET "$guarded")"
done

info "Bootstrap and sign in"

# The one-time token is printed to the log at startup and never stored in
# recoverable form, so reading it from the log is the only way in -- which is
# itself the property under test.
# Anchored to the banner rather than "any 32-character run in the log": request
# ids, digests, and timestamps are all long alphanumeric runs, and matching one
# of those instead produced a confusing cascade of authentication failures.
bootstrap_token="$(docker logs "$CONTAINER" 2>&1 |
  awk '/HarborMaster bootstrap token/ {found = 1; next}
       found && $1 ~ /^[A-Za-z0-9_-]{24,}$/ {print $1; exit}' || true)"

if [ -n "$bootstrap_token" ]; then
  pass "a one-time bootstrap token was issued at startup"
else
  fail "no bootstrap token was found in the startup log"
fi

bootstrap_body="$(auth_post /api/v1/auth/bootstrap \
  "{\"token\":\"${bootstrap_token}\",\"username\":\"${SMOKE_USER}\",\"password\":\"${SMOKE_PASSWORD}\"}")"
check_contains "bootstrap creates the first administrator" '"administrator"' "$bootstrap_body"
check_contains "bootstrap returns a CSRF token" '"csrfToken"' "$bootstrap_body"

CSRF="$(json_field "$bootstrap_body" '.csrfToken' '[0-9a-f]\{64\}')"
if [ -n "$CSRF" ] && [ "$CSRF" != "NOT_FOUND" ] && [ "$CSRF" != "JQ_ERROR" ]; then
  pass "a CSRF token was issued with the session"
else
  fail "no CSRF token was issued with the session"
fi

# The token is single use: the installation is claimed and cannot be claimed
# again, whatever the caller presents.
replay_status="$(auth_post_status /api/v1/auth/bootstrap \
  "{\"token\":\"${bootstrap_token}\",\"username\":\"intruder\",\"password\":\"${SMOKE_PASSWORD}\"}")"
if [ "$replay_status" = "200" ]; then
  fail "the bootstrap token was accepted a second time"
else
  pass "the bootstrap token cannot be replayed (status $replay_status)"
fi

check "sign in with the wrong password returns 401" "401" \
  "$(auth_post_status /api/v1/auth/login \
    "{\"username\":\"${SMOKE_USER}\",\"password\":\"not-the-password\"}")"

info "Authenticated reads"

check "GET /api/v1/containers returns 200 with a session" "200" "$(authed_status GET /api/v1/containers)"
check_contains "container list returns a paginated envelope" '"pagination"' \
  "$(authed_body /api/v1/containers)"

check "GET /api/v1/health returns component detail to a session" "200" \
  "$(authed_status GET /api/v1/health)"
check_contains "the authenticated health response names the database" '"database"' \
  "$(authed_body /api/v1/health)"

# The event engine must report itself honestly with no Docker socket:
# connecting is impossible, so anything but a live stream is the correct answer.
check "GET /api/v1/event-engine returns 200" "200" "$(authed_status GET /api/v1/event-engine)"
engine_body="$(authed_body /api/v1/event-engine)"
check_contains "event-engine payload carries a connection state" '"state"' "$engine_body"
check_contains "event-engine payload carries counters" '"counters"' "$engine_body"
if printf '%s' "$engine_body" | grep -q '"state":"connected"'; then
  fail "the event engine reports connected without a Docker socket"
else
  pass "the event engine does not claim a live stream without Docker"
fi

check "GET /api/v1/events returns 200" "200" "$(authed_status GET /api/v1/events)"
check_contains "event list returns a paginated envelope" '"pagination"' "$(authed_body /api/v1/events)"

# Read-only: no event endpoint may accept a write, and no prune endpoint exists.
check "POST /api/v1/events returns 405" "405" "$(authed_status POST /api/v1/events)"
check "DELETE /api/v1/events/1 returns 405" "405" "$(authed_status DELETE /api/v1/events/1)"
check "POST /api/v1/events/stream returns 405" "405" "$(authed_status POST /api/v1/events/stream)"

info "CSRF"

# A session cookie alone is not authority to write. The CSRF token is derived
# from the session token and must be presented in a header a cross-site caller
# cannot set.
check "a cookie-authenticated write with no CSRF token returns 403" "403" \
  "$(nocsrf_status POST /api/v1/inventory/refresh '{}')"
check "a write with a wrong CSRF token returns 403" "403" \
  "$(badcsrf_status POST /api/v1/inventory/refresh '{}')"

info "Docker mutation features are absent by default"

# This container was started with no acquisition, execution, or rollback
# settings, so all three must report themselves off. When they are off the
# capability is never wired at all, which is what these 503s represent.
for guarded in /api/v1/acquisitions /api/v1/executions /api/v1/rollbacks; do
  status="$(authed_status GET "$guarded")"
  case "$status" in
  503) pass "$guarded reports the feature unavailable (503)" ;;
  200)
    # Executions and acquisitions answer 200 with summary.enabled=false so
    # stored history stays readable. Either is correct; claiming enabled is not.
    if printf '%s' "$(authed_body "$guarded")" | grep -q '"enabled":true'; then
      fail "$guarded reports itself enabled with no capability configured"
    else
      pass "$guarded reports the feature switched off"
    fi
    ;;
  *) fail "$guarded returned $status, want 503 or a disabled 200" ;;
  esac
done

check "POST /api/v1/executions is refused without the capability" "503" \
  "$(authed_status POST /api/v1/executions '{"acquisitionId":"acq_0011223344556677889a"}')"
check "POST /api/v1/rollbacks is refused without the capability" "503" \
  "$(authed_status POST /api/v1/rollbacks '{"executionId":"exec_00112233445566778899"}')"

info "Sign out"

check "POST /api/v1/auth/logout returns 204" "204" "$(authed_status POST /api/v1/auth/logout)"
check "the session no longer reads a protected route" "401" \
  "$(authed_status GET /api/v1/containers)"

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
