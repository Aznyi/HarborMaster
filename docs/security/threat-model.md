# HarborMaster Threat Model

**Status:** current as of the Phase 3 security audit
**Scope:** the whole repository — backend, frontend, container image, CI/CD
**Method:** asset-driven, with STRIDE applied per trust boundary

This document exists so that a reviewer can disagree with a specific claim
rather than with a general impression. Every mitigation names the code or
configuration that implements it.

---

## 1. The single most important fact

**HarborMaster reads a Docker socket, and access to a Docker socket is
equivalent to root on the host.**

Not "similar to root". Equivalent. Anything that can issue Docker API calls can
start a container with `--privileged`, bind-mount `/`, and write to the host
filesystem as root. This is a property of Docker's architecture, not of
HarborMaster.

Two consequences follow, and they shape everything below:

1. **Compromising the HarborMaster process is very close to compromising the
   host**, regardless of how little HarborMaster itself does. The defence is not
   "HarborMaster is read-only" — it is keeping the process unreachable by
   attackers in the first place, and keeping the blast radius small if it is
   reached.
2. **`-v /var/run/docker.sock:/var/run/docker.sock:ro` is not a security
   boundary.** The `:ro` makes the *socket file* read-only. It does not make the
   *Docker API* read-only: the writes an API request needs are still permitted.
   The deployment documentation says this in the same words.

---

## 2. Assets

Ordered by what an attacker would want most.

| # | Asset | Where it lives | Why it matters |
| --- | --- | --- | --- |
| A1 | **The Docker socket** | Host, bind-mounted into the container | Root-equivalent host control |
| A2 | **Snapshot HMAC key** | `<data-dir>/snapshot-hmac.key`, or a mounted secret | Turns stored digests into an offline oracle for guessing secrets |
| A3 | **Container environment values** | Docker daemon; in HarborMaster only transiently in memory | Database passwords, API tokens, registry credentials |
| A4 | **The SQLite database** | `/var/lib/harbormaster/harbormaster.db` | Full configuration history of every container on the host |
| A5 | **Inventory metadata** | Database and API | Maps the host: images, ports, mounts, networks — reconnaissance |
| A6 | **Release artifacts** | GHCR, GitHub releases | Compromise here reaches every operator |
| A7 | **CI credentials** | `GITHUB_TOKEN`, OIDC identity | Can publish a malicious image |
| A8 | **Availability** | The running process | An operator blind to their estate during an incident |

---

## 3. Actors

| Actor | Capability assumed | Motivation |
| --- | --- | --- |
| **Anonymous network attacker** | Can reach the HTTP port | Reconnaissance, then host control |
| **Malicious local user** | Shell on the host, not root | Privilege escalation |
| **Hostile container** | Runs on the same host, not privileged | Escape, lateral movement |
| **Malicious dependency** | Code executes in build or at runtime | Supply-chain persistence |
| **Compromised CI action** | Runs in the workflow | Publish a backdoored image |
| **Curious operator** | Legitimate UI access | Not an attacker; the reason secrets stay masked even from them |
| **Physical/backup access** | Has the database file | Offline analysis |

**Explicitly not defended against:** an actor who already has root on the host,
or who can already write to the Docker socket directly. Both already have
everything HarborMaster could protect.

---

## 4. Trust boundaries

```
                    ┌─────────────────────────────────────────────┐
                    │  HOST (root-equivalent zone)                │
                    │                                             │
   ┌────────┐  TB1  │   ┌──────────────┐   TB2   ┌────────────┐   │
   │Browser │───────┼──▶│ HarborMaster │────────▶│Docker socket│  │
   └────────┘       │   │  container   │  read   └────────────┘   │
                    │   └──────┬───────┘  only          │         │
   ┌────────┐  TB1  │          │ TB3                    ▼         │
   │API      │──────┼──▶       ▼                  ┌────────────┐  │
   │client   │      │   ┌──────────────┐          │Other       │  │
   └────────┘       │   │ SQLite (vol) │          │containers  │  │
                    │   └──────────────┘          └────────────┘  │
                    └─────────────────────────────────────────────┘
        TB4 ▲
   ┌────────┴────────┐
   │ GitHub Actions  │──▶ GHCR (signed, attested)
   └─────────────────┘
```

| Boundary | Between | Crossing controls |
| --- | --- | --- |
| **TB1** | Network → HTTP API | **No authentication.** Loopback bind by default, security headers, strict input validation, rate limiting, body/pagination limits |
| **TB2** | HarborMaster → Docker socket | Seven read-only methods; architecture test; SDK confined to `internal/docker`; all errors sanitised |
| **TB3** | Process → SQLite | Parameterised queries only; sort allowlists; no plaintext secrets; `0750` directory |
| **TB4** | CI → registry | SHA-pinned actions, least-privilege tokens, OIDC keyless attestation, SBOM, provenance |

**TB1 is the weakest boundary in the system, and it is weak by design in this
phase.** Authentication is deliberately out of scope until a later phase. The
compensating control is deployment guidance — loopback binding by default — and
that is a weaker control than authentication would be. This is the single
largest residual risk in the product.

---

## 5. Attack surface

### Network-reachable (unauthenticated)

| Surface | Methods | Notes |
| --- | --- | --- |
| 21 REST endpoints under `/api/v1` | GET, plus 2 POST | Full list in `api/openapi.yaml` |
| `POST /api/v1/inventory/refresh` | POST | Drives a full Docker sweep |
| `POST /api/v1/snapshots` | POST | Writes to the database |
| `GET /api/v1/events/stream` | GET | Long-lived SSE connection |
| SPA shell and static assets | GET | Served from an embedded FS |

### Not network-reachable

Docker event stream (outbound), SQLite file, HMAC key file, embedded migrations.

---

## 6. STRIDE per boundary

### TB1 — Network to HTTP API

| Threat | Vector | Mitigation | Residual |
| --- | --- | --- | --- |
| **S**poofing | No identity exists to spoof | N/A — no authentication in this phase | **Anyone reachable is fully trusted** |
| **T**ampering | Malicious JSON to a POST | Strict decoding, unknown fields rejected, single object enforced, UTF-8 and control-character validation, size limits | Low |
| **T**ampering | Cross-origin write from a malicious page | `Content-Type: application/json` required (forces preflight), `Sec-Fetch-Site`/`Origin` checks, rate limiting | Low |
| **R**epudiation | Forged log correlation | Client `X-Request-ID` ignored; server generates 12 random bytes | Low |
| **I**nfo disclosure | Secret in an API response | Digest fields unserialisable; sensitive values never stored | Low |
| **I**nfo disclosure | Stack trace or path in an error | All errors mapped to stable codes and generic messages; tested | Low |
| **I**nfo disclosure | Reconnaissance of the host estate | **None — this is the product's purpose** | **High if exposed** |
| **D**enial of service | Unbounded pagination | `pageSize` capped at 200, rejected not clamped | Low |
| **D**enial of service | Expensive diffs | 4 concurrent max, 5s timeout, 1000-entry cap, 429 not queued | Low |
| **D**enial of service | Snapshot flooding | Rate limit, `(container_id, checksum)` dedup index, per-container capture lock | Low |
| **D**enial of service | SSE connection exhaustion | Subscriber cap with `Retry-After`; bounded per-subscriber queues | Low |
| **D**enial of service | Refresh flooding against the socket | Rate limit, single-flight refresh lock | Low |
| **E**levation | Reaching Docker through the API | **No mutation capability exists anywhere in the codebase** | Low |

### TB2 — HarborMaster to Docker socket

| Threat | Vector | Mitigation | Residual |
| --- | --- | --- | --- |
| **T**ampering | A future method mutates Docker | `TestRuntimeExposesNoMutationMethods` and `TestRuntimeSurfaceIsTheExpectedReadOnlySet` fail the build | Low |
| **I**nfo disclosure | Socket path in an API error | `docker.SanitizeError` maps every failure to a fixed phrase | Low |
| **I**nfo disclosure | Raw daemon payload reaching a client | Only HarborMaster domain models are serialised; raw inspection is redacted before storage | Low |
| **D**enial of service | Unbounded inspection concurrency | Worker semaphore, bounded by `INVENTORY_WORKERS` (max 64) | Low |
| **D**enial of service | Event stream floods memory | Bounded queue; overflow drops and requests reconciliation rather than blocking the reader | Low |
| **E**levation | Compromised process uses the socket | **None. This is the core residual risk** | **Critical if the process is compromised** |

### TB3 — Process to SQLite

| Threat | Vector | Mitigation | Residual |
| --- | --- | --- | --- |
| **T**ampering | SQL injection via a filter | Every value bound; sort fields from a fixed allowlist; tested with injection payloads | Low |
| **T**ampering | Snapshot history rewritten | Append-only repository; one `UPDATE` touching two summary columns; source-level test enforces it | Low |
| **I**nfo disclosure | Stolen database yields secrets | Keyed HMAC digests, not plaintext; whole-database sweep test | Low, given key hygiene |
| **I**nfo disclosure | Database world-readable | Directory created `0750` | Low |
| **D**enial of service | Unbounded growth | Retention by count and age, bounded batches, newest always kept | Low |
| **D**enial of service | Writer starvation | `MaxOpenConns(1)`, WAL, busy timeout, bounded prune batches | Medium under heavy write load |

### TB4 — CI/CD to registry

| Threat | Vector | Mitigation | Residual |
| --- | --- | --- | --- |
| **T**ampering | Compromised action tag | Every action SHA-pinned; Dependabot updates them | Low |
| **T**ampering | Malicious dependency | Dependency review on PRs, govulncheck, Trivy, license denylist | Medium — a clean-but-malicious package still merges if reviewed carelessly |
| **S**poofing | Forged release artifact | Keyless OIDC attestation, SBOM, in-toto provenance | Low |
| **E**levation | Workflow privilege abuse | `contents: read` default, widened per job; no `pull_request_target`; no untrusted input interpolated into `run:` | Low |
| **I**nfo disclosure | Secret in build logs | No secrets in build args; gitleaks scans full history | Low |

---

## 7. Secret handling design

The rule: **HarborMaster never stores a secret value, in any encoding,
anywhere.**

What is stored per sensitive variable: the name, a classification, presence, byte
length, and `HMAC-SHA-256(installation key, value)` with the algorithm and a
non-reversible key ID.

**Why keyed rather than a plain hash.** A plain `SHA-256` of `hunter2` is
identical in every database on earth and appears in every rainbow table. A stolen
HarborMaster database full of plain hashes would be a wordlist attack waiting to
happen. Keyed digests mean a stolen database alone yields nothing.

**Threats this addresses and does not.**

| Attacker holds | Outcome |
| --- | --- |
| Database only | Cannot recover any secret. Can see which variables exist and their lengths. |
| Database + HMAC key | Can mount an offline dictionary attack against low-entropy values. This is why the key belongs in a Docker secret, not beside the database. |
| Live process memory | Can read values transiently present during capture. Not defended against. |

**Accepted trade-offs**, documented rather than hidden:

- **Value length is observable.** An operator preparing a restore needs to know a
  variable is set and roughly what it is. It does disclose password length.
- **Classification is name-based.** A credential under an innocuous name
  (`ENDPOINT`, `CONFIG`) is not detected. `HARBORMASTER_MASK_MODE=all-sensitive`
  is the mitigation for operators who need the guarantee.
- **Restore is impossible for containers with secrets.** This is a deliberate
  consequence, surfaced by the `secrets_available` readiness check rather than
  discovered during an incident.

---

## 8. Residual risks

Ordered by severity. These are accepted, not solved.

| # | Risk | Severity | Why accepted | Mitigation available today |
| --- | --- | --- | --- | --- |
| R1 | **No authentication.** Anyone who can reach the port gets the full inventory and can drive both POST endpoints | **High** | Out of scope until a later phase | Loopback bind by default; authenticating reverse proxy; network policy |
| R2 | **Docker socket access is root-equivalent.** A compromise of the process is close to a host compromise | **High** | Inherent to the product's purpose | Read-only adapter, distroless non-root image, dropped capabilities, `no-new-privileges`, resource ceilings |
| R3 | **HMAC key beside the database.** An attacker with the data volume gets both | **Medium** | Convenient default for standalone use | Supply the key via `..._KEY_FILE` and a Docker secret |
| R4 | **Base images pinned by tag, not digest** | **Medium** | Digest pinning needs registry lookups and regular rotation | Dependabot `docker` ecosystem is configured to keep them current |
| R5 | **No RBAC.** No notion of a user, so no least privilege among operators | **Medium** | Follows from R1 | Proxy-level access control |
| R6 | **Value length disclosure** | **Low** | Needed for restore readiness | None; documented |
| R7 | **Name-based secret classification** | **Low** | Value scanning is worse | `all-sensitive` mode |
| R8 | **SQLite single writer** | **Low** | Right choice for a single-host tool | Bounded batches; retention |
| R9 | **No TLS in-process** | **Low** | Terminating TLS is a proxy's job | Reverse proxy |

---

## 9. Assumptions

If any of these is false, this model does not hold.

1. The operator does not expose HarborMaster to an untrusted network without an
   authenticating proxy.
2. The host's Docker daemon is not already compromised.
3. The data volume is not readable by untrusted users on the host.
4. The HMAC key is backed up and is not stored in the same backup as the
   database, if key hygiene matters to the deployment.
5. Operators verify image attestations before deploying.
6. GitHub's OIDC and attestation infrastructure is trustworthy.

---

## 10. Future work

Roughly in the order it should be done.

1. **Authentication** — closes R1, the largest residual risk. Until it exists,
   every other network control is compensating for its absence.
2. **Authorization / RBAC** — closes R5; only meaningful after (1).
3. **Secure secret injection** — the prerequisite for restore. Restoring a
   container with secrets requires values HarborMaster deliberately does not
   hold.
4. **Digest-pinned base images** — closes R4.
5. **HMAC key rotation tooling** — the metadata exists; the tooling does not.
6. **Audit log of read access** — who looked at what, once identity exists.
7. **Optional host validation provider** — the interface exists and answers
   `unverifiable`; a real implementation must be opt-in and carefully bounded.

---

## 11. Review triggers

Revisit this document when any of the following happens:

- HarborMaster gains **any** ability to modify a Docker resource.
- Authentication or authorization is added.
- A new network listener, protocol, or endpoint appears.
- The secret handling design changes.
- A new trust boundary is introduced (a second host, an agent, a plugin).
- A dependency with access to the socket changes major version.
