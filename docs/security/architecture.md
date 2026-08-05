# HarborMaster Security Architecture

How the codebase is arranged so that its security properties are structural
rather than remembered.

---

## 1. The layering rule

```
   ┌──────────────────────────────────────────────────────────┐
   │ web/            React SPA, embedded in the binary        │
   ├──────────────────────────────────────────────────────────┤
   │ internal/api    HTTP: routing, validation, serialisation │
   ├──────────────────────────────────────────────────────────┤
   │ internal/service   Business logic. No SQL, no HTTP        │
   ├──────────────────────────────────────────────────────────┤
   │ internal/store     SQLite. No HTTP, no Docker             │
   ├──────────────────────────────────────────────────────────┤
   │ internal/domain    Pure models. No I/O of any kind        │
   ├──────────────────────────────────────────────────────────┤
   │ internal/docker    THE ONLY package that imports the SDK  │
   └──────────────────────────────────────────────────────────┘
```

`internal/domain` is the lingua franca. Every layer speaks it, and nothing else
crosses a boundary.

**This is enforced, not documented.** `internal/arch/arch_test.go` parses every
Go file's import declarations with `go/parser` — not grep, which would match the
module paths written in comments — and fails the build if:

- the retired `github.com/docker/docker` module reappears anywhere;
- the Moby engine monolith is imported;
- the Moby SDK is imported from any package other than `internal/docker`.

## 2. The read-only guarantee

HarborMaster's central claim is that it cannot change Docker. Three mechanisms
uphold it, and all three would have to be defeated together.

**One narrow interface.** `docker.Runtime` has exactly seven methods:

```
Ping  ListContainers  InspectContainer  InspectImage
ListNetworks  ListVolumes  StreamEvents
```

**A verb check.** `TestRuntimeExposesNoMutationMethods` reflects over the
interface and fails if any method name begins with a mutating verb — `create`,
`start`, `stop`, `remove`, `exec`, `pull`, and thirty more. Prefix matching, not
substring, so `ListContainers` is not rejected for containing "tag".

**An exact-set check.** `TestRuntimeSurfaceIsTheExpectedReadOnlySet` pins the
method set literally. A mutation hidden behind an innocuous name still has to be
added to that list, in a diff a reviewer sees.

Gaining write access therefore requires editing the interface, editing two
tests, and explaining why in a pull request. That is the point.

## 3. Where secrets are stopped

Defence in depth, four independent layers. Any one of them alone would prevent a
plaintext secret reaching disk; all four exist because this is the one mistake
that cannot be undone after the fact.

| Layer | Mechanism | File |
| --- | --- | --- |
| 1. Classification | Name-based masking at the adapter boundary, before a value enters the application | `internal/docker/normalize.go`, `internal/domain/masking.go` |
| 2. Type system | `EnvVar.RawValue` and every digest field are `json:"-"` — accidental serialisation is impossible | `internal/domain/container_config.go`, `secret_digest.go` |
| 3. Capture | Sensitive values hashed once and dropped; the row carries no value | `internal/service/snapshot_spec.go` |
| 4. Persistence | The repository blanks a sensitive value again; a `CHECK` constraint rejects one | `internal/store/snapshot_repository.go`, `migrations/0004_snapshots.sql` |

**Fail-closed classification.** An entry whose classification is missing is
stored as *sensitive*, not as normal. If HarborMaster does not know whether a
value is a secret, the answer to "is this safe to store" is no. Being wrong in
that direction costs an operator a value they cannot see; being wrong the other
way leaks a credential.

## 4. Trust boundaries in code

| Boundary | Enforcement point |
| --- | --- |
| Untrusted HTTP input | `internal/api/query.go`, `snapshot_query.go`, `write_guard.go` |
| Untrusted Docker data | `internal/docker/normalize.go`, `redact.go` |
| SQL | Every query in `internal/store` — bound parameters, allowlisted identifiers |
| Response rendering | `internal/api/response.go` — stable codes, generic messages |
| Browser | `withSecurityHeaders` — CSP with no `unsafe-inline` |

### Input validation

Every enumerated value is checked against a closed domain vocabulary. Every sort
field is checked against a repository allowlist. **Nothing caller-controlled
ever becomes part of SQL text**; the allowlist maps a caller's string to a
compile-time constant column name.

Rejected rather than clamped: a `pageSize` of 10 000 is a `400`, not a silent 200.
Silently serving something other than what was asked hides a bug in the caller.

### Output sanitisation

Errors never cross the boundary verbatim. Handlers log the real error and return
a stable `ErrorCode` plus a short message. Docker errors additionally pass
through `docker.SanitizeError`, which maps every failure to one of three fixed
phrases so a socket path cannot appear in a response.

## 5. Resource discipline

An unauthenticated API must not be able to consume unbounded resources. Every
dimension is bounded, and the bound is configuration rather than a magic number.

| Dimension | Bound |
| --- | --- |
| Request body | `MAX_REQUEST_BYTES`, plus `MaxBytesReader` at the handler |
| Page size | 200, rejected above |
| Docker inspection concurrency | `INVENTORY_WORKERS`, max 64, semaphore-bounded |
| Event queue | `EVENTS_BUFFER_SIZE`; overflow drops and requests reconciliation rather than blocking the reader |
| SSE subscribers | `EVENTS_STREAM_MAX_SUBSCRIBERS`, refused with `Retry-After` |
| SSE replay | `EVENTS_STREAM_REPLAY_LIMIT`, truncation announced to the client |
| Diff concurrency | 4, refused with `429` — never queued |
| Diff wall time | 5s, `context.WithTimeout` |
| Diff output | 1 000 entries, 5 000 compared per group, 4 KiB per value |
| Write endpoints | Token bucket, per process |
| Snapshot growth | `(container_id, checksum)` unique index; retention by count and age |
| Database writer | `MaxOpenConns(1)`; prune in bounded batches |

**Refused, not queued.** A queue converts a load spike into unbounded memory and
latency. Every ceiling here returns `429` or `503` with `Retry-After`.

## 6. Concurrency

Every goroutine has a bounded lifetime tied to a context. Every `time.NewTimer`
has a `defer Stop()`. Every channel is created with an explicit capacity.

Shutdown order is deliberate and documented in `cmd/harbormaster/main.go`: the
HTTP server drains first, then background services, then the Docker client and
the database. The database must close *after* the background services or a final
event flush would write to a closed handle.

`go test -race` runs in CI on every push. It is not optional: the event engine
owns several long-lived goroutines and a bounded queue.

## 7. Frontend

- **No `dangerouslySetInnerHTML` anywhere.** React escapes by default; nothing
  opts out.
- **No URL built from backend data.** The only dynamic link targets are internal
  routes built from numeric IDs.
- **No secret can be rendered**, because none is sent. `SnapshotEnvEntry` has no
  `digest` field in its TypeScript type at all, and `value` is optional.
- **CSP forbids inline styles and scripts.** The build emits a linked stylesheet
  and a module script; nothing needs an exemption.
- **Errors render as typed `ApiError` values**, distinguishing "backend
  unreachable" from "backend rejected the request" — different remedies.

## 8. Container

| Property | Implementation |
| --- | --- |
| No shell, no package manager, no interpreter | `gcr.io/distroless/static-debian13:nonroot` |
| Non-root | `USER 65532:65532`, numeric |
| Static binary | `CGO_ENABLED=0`, pure-Go SQLite driver |
| No debug symbols or host paths | `-trimpath -ldflags "-s -w"` |
| Read-only root filesystem | `read_only: true` |
| No capabilities | `cap_drop: [ALL]` |
| No privilege escalation | `no-new-privileges:true` |
| Bounded resources | `mem_limit`, `pids_limit`, `cpus` |
| Health check without a shell | `harbormaster healthcheck` subcommand |
| Loopback by default | `HARBORMASTER_BIND=127.0.0.1` |

## 9. Supply chain

- Every GitHub Action pinned to a **commit SHA**, not a tag.
- Dependabot covers Go, npm, Actions, and Docker base images.
- Dependency review blocks High/Critical advisories and copyleft licences on PRs.
- CodeQL (`security-extended`) for Go and TypeScript.
- Trivy scans the filesystem, the config, and the built image.
- govulncheck — reachability-aware, so a finding is a real exposure.
- Gitleaks scans full history.
- Releases carry an SBOM, in-toto provenance, and a keyless OIDC attestation.

## 10. What is deliberately absent

Listing these so their absence reads as a decision rather than an oversight.

| Absent | Why |
| --- | --- |
| Authentication | Deferred to a later phase. **The largest residual risk.** |
| RBAC | Follows authentication |
| Any Docker mutation | The product's central guarantee |
| Restore / rollback / update | Later phases; snapshots prepare for them |
| Arbitrary command execution | Never. Not a feature, a category of vulnerability |
| Template or plugin execution | Same |
| User-controlled file access | Same |
| Host filesystem inspection | Interface exists, answers `unverifiable` |
| Expression languages in the API | An interpreter on an unauthenticated endpoint is a DoS and injection surface |
| TLS termination | A reverse proxy's job |
