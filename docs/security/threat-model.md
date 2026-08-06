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
| 40 REST endpoints under `/api/v1` | Mostly GET | Full list in `api/openapi.yaml`; a test fails the build if the router and the document disagree |
| `POST /api/v1/inventory/refresh` | POST | Drives a full Docker sweep |
| `POST /api/v1/snapshots` | POST | Writes to the database |
| `PATCH /api/v1/drift/{id}` | PATCH | Moves a drift record's status. Cannot reach Docker, a container, or a snapshot; cannot set `active` or `resolved` |
| 4 drift read endpoints | GET | Drift records, summary, and per-container view |
| `POST` / `PATCH` / `DELETE /api/v1/policies` | POST, PATCH, DELETE | **The first create/update/delete surface.** Acts on HarborMaster's own rows only. `DELETE` archives rather than destroys, and the schema refuses a delete that would orphan violation history |
| `PATCH /api/v1/policy-violations/{id}` | PATCH | Moves a violation's status; cannot set `active` or `resolved`, and does not suppress re-evaluation |
| `POST /api/v1/policy/evaluate` | POST | Schedules a compliance pass and answers 202. Coalesced through the same queue the scheduled passes use, so a loop produces one pass; rate limited on top |
| 5 policy read endpoints | GET | Definitions, the rule catalogue, violations, summary, per-container view |
| `POST /api/v1/plans/generate` | POST | Schedules a plan generation pass and answers 202. Generates HarborMaster's own analysis of HarborMaster's own database: pulls nothing, changes no container, schedules no change. Coalesced and rate limited |
| 3 plan read endpoints | GET | Plans with the estate summary, one plan, one container's planning view. No PATCH and no DELETE: plans are immutable |
| `GET /api/v1/events/stream` | GET | Long-lived SSE connection |
| SPA shell and static assets | GET | Served from an embedded FS |

**The policy write surface is the notable change in this phase**, so it is worth
being explicit about what an anonymous caller can do with it: create, edit, and
withdraw HarborMaster's own compliance rules, and ask for a compliance pass. It
cannot change a container, cannot reach the Docker socket, cannot destroy
violation history, and cannot supply anything that is interpreted as code. The
bounds on policy size, rule count, value count, and pattern shape are what stop
it being used to make evaluation expensive; the per-process rate limit and the
asynchronous evaluate endpoint are what stop it being used to occupy the
process. Under R1 (no authentication) an attacker who can reach the port can
also **disable a policy and hide non-compliance from the dashboard** — the
definition and its history survive, but the report stops being trustworthy.

**The change planning surface adds no capability**, which is the point worth
stating. `POST /plans/generate` is the third asynchronous "do a pass" endpoint,
and like the other two it takes no body and no target. What it schedules reads
six tables HarborMaster already populated and writes one; it makes no network
request and opens no Docker connection, so an anonymous caller who reaches it
gains nothing they could not already cause with an inventory refresh — and less,
since a pass over an unchanged estate writes no rows at all.

There is no plan write surface beyond that. A plan cannot be edited, deleted,
approved, or applied, because none of those routes exists and HarborMaster has
no capability behind them. Under R1 an attacker who can reach the port can read
every assessment, which tells them which containers are running outdated images
and which have unresolved policy violations — reconnaissance value, addressed by
the same binding and proxy guidance as the rest of the API rather than by
anything specific to this feature.

### Outbound (initiated by HarborMaster)

| Surface | Direction | Notes |
| --- | --- | --- |
| Registry manifest and tag-listing endpoints | **Outbound HTTPS** | Phase 6. Anonymous GETs only. **The first and only outbound egress in the product.** Destinations derive solely from image references the inventory holds |

This is the boundary Phase 6 added, and it deserves stating plainly: HarborMaster
now initiates connections to third parties it does not control, and parses their
responses. The registry is treated as hostile input throughout — bounded before
reading, decoded into a fixed shape, and never echoed into a log, an error, or a
column.

Turning `IMAGE_INTEL_ENABLED` off removes this surface entirely: no outbound
request is made at all, which is the configuration an air-gapped or
egress-restricted deployment wants.

### Not network-reachable

Docker event stream (outbound), SQLite file, HMAC key file, embedded migrations.

**The operator commands are deliberately here and not above.**
`harbormaster diagnose` and `harbormaster backup` report filesystem paths, free
space, journal mode, page counts, schema history, and daemon reachability, and
`backup` writes a complete copy of the database to an operator-chosen path.
Every one of those is what an operator needs during an incident and what an
attacker wants during reconnaissance.

Exposing them over HTTP would put host layout behind an endpoint that has no
authentication. Requiring shell access to the container or the host is the
control: that is a privilege an operator already has and an anonymous HTTP
request does not. `TestDiagnosticsAreNotReachableOverHTTP` fails the build if
`internal/api` imports the diagnostics package, so the rule survives a
well-meaning "show the diagnosis in the UI" change.

`diagnose` opens the database read-only and contacts no Docker daemon.
`backup` reads and writes files only. Neither adds a Docker capability.

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
| **I**nfo disclosure | Database world-readable | Directory created `0750`; `diagnose` reports a wider mode as a finding | Low |
| **I**nfo disclosure | A backup left world-readable | `harbormaster backup` creates `0600`, parent `0750` | Low |
| **I**nfo disclosure | Diagnostics exposed to the network | Command only, never a route; enforced by `TestDiagnosticsAreNotReachableOverHTTP` | Low |
| **I**nfo disclosure | A diagnosis printing row contents | Report carries only counts, closed-vocabulary states, sizes, and configured paths; output swept in test with a positive control | Low |
| **T**ampering | An old binary writes a schema it does not understand | Applied migrations validated against the embedded set; `ErrSchemaAhead` refuses the open without modifying the database | Low |
| **T**ampering | An applied migration edited after the fact | SHA-256 recorded per migration at apply time; a mismatch refuses the open | Low for edits made after this version; **undetectable** for edits predating it |
| **T**ampering | Backup path interpreted as SQL | `VACUUM INTO ?` takes a bound parameter; tested with a quoted filename | Low |
| **T**ampering | A backup silently overwriting the good one | Refuses an existing destination and the live database and its sidecars | Low |
| **I**ntegrity | Undetected corruption written over | `quick_check` at open, **before** migrating; damage refuses startup | Low |
| **I**ntegrity | A restored backup that cannot actually be restored | Every backup verified on write: full integrity check, foreign key check, schema history, row counts | Low |
| **I**ntegrity | Silent WAL fallback changing the crash profile | Journal mode read back and warned; `DB_REQUIRE_WAL` turns it into a refusal | Low |
| **D**enial of service | Unbounded growth | Retention by count and age, bounded batches, newest always kept | Low |
| **D**enial of service | Writer starvation | `MaxOpenConns(1)`, WAL, busy timeout, bounded prune batches | Medium under heavy write load |
| **D**enial of service | A startup integrity check that never finishes | `DB_INTEGRITY_TIMEOUT`; an incomplete check is reported, not treated as damage, and does not refuse startup | Low |
| **D**enial of service | Shutdown held open by detached background work | One `SHUTDOWN_TIMEOUT` budget; `GraceContext` bounds every detached task | Low |
| **D**enial of service | A long outage requesting the daemon's whole event ring on reconnect | Resume window clamped to one hour | Low |
| **I**nfo disclosure | A secret value reaching a drift record | Diff engine withholds it, the model has no field, the repository blanks it in and out, and a CHECK constraint refuses the row; whole-database sweep test with a positive control | Low |
| **I**nfo disclosure | A key rotation reported as "every secret changed" | Digests under different keys report `unverifiable`, never `modified` | Low |
| **T**ampering | An operator marking real drift resolved | `active` and `resolved` are engine-owned; PATCH validates against the operator vocabulary only | Low |
| **T**ampering | A caller reaching a field other than status through PATCH | Strict decode with unknown fields rejected; the body has exactly two fields | Low |
| **I**ntegrity | A truncated comparison silently clearing real drift | An incomplete evaluation resolves nothing and is surfaced in the summary | Low |
| **I**ntegrity | "Never evaluated" read as "no drift" | Evaluations recorded separately from records; the summary reports `containersEvaluated` | Low |
| **D**enial of service | An event storm growing the drift table | Identity is `(container, snapshot, category, field)`; repeats upsert rather than insert | Low |
| **D**enial of service | A drift sweep starving the unauthenticated diff endpoint | Drift owns a separate DiffEngine instance | Low |
| **D**enial of service | An unbounded `IN` clause from repeated filter parameters | 32 values per parameter, rejected above | Low |

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
| R10 | **Backups are unencrypted.** A backup is a complete copy of the database, and `harbormaster backup` writes it in the clear | **Medium** | Encrypting it would need key management HarborMaster does not have, and would produce an artifact no standard tool can read during an incident | `0600` mode; store backups on an encrypted volume; keep the HMAC key out of the same backup |
| R11 | **Migration edits predating this version are undetectable.** Checksums are recorded from this release onward and backfilled for older rows | **Low** | Nothing can retroactively prove what an already-applied file contained; the evidence does not exist | Every edit from this version forward is detected and refuses the open |
| R12 | **`diagnose` cannot read a database left with a hot write-ahead log by a crashed process**, because replaying it is a write and the diagnosis is read-only | **Low** | The read-only guarantee is worth more than reading in this one state; the filesystem-level findings still render | Start HarborMaster once to replay the log, then re-run; the condition is reported explicitly rather than as corruption |
| R13 | **An integrity check that times out establishes nothing**, and startup continues | **Low** | Refusing on an *incomplete* check would turn a slow disk into an outage; a false-positive refusal is worse than a late diagnosis | Raise `DB_INTEGRITY_TIMEOUT`, or run `harbormaster diagnose`, which uses the full check with no startup pressure |
| R18 | **HarborMaster initiates outbound connections to public registries.** An egress-restricted network may block them, and a network observer learns which images the estate runs | **Medium** | The feature cannot work without asking a registry, and the alternative — shipping a database of known images — would be worse and staler | `IMAGE_INTEL_ENABLED=false` removes the surface entirely; destinations are confined to hosts named by the inventory's own image references |
| R19 | **A compromised or hostile registry serves HarborMaster arbitrary bytes.** It can report a false digest, a false tag list, or misleading annotations | **Medium** | Any client of a registry has this exposure, and HarborMaster's is read-only: the worst outcome is a wrong badge on a dashboard, not a changed container | Responses bounded and parsed into a fixed shape; digests COMPUTED rather than believed; annotations allowlisted, bounded and control-character free; no registry string reaches a log, an error, or a column |
| R20 | **Update verdicts are advisory and can be wrong.** A publisher who reuses tags unconventionally, or a repository with more tags than the page budget, can produce a missed or mislabelled update | **Low** | Tag conventions are not standardised and cannot be inferred reliably; the parser is deliberately conservative and refuses more than it accepts | Missed updates are the chosen failure direction; a truncated listing reports `unknown` rather than `none`; the digest comparison is always available and always true |
| R21 | **Anonymous rate limits are shared by egress address.** A busy host can exhaust a public registry's anonymous quota for everything behind the same address | **Low** | Authenticating would mean holding registry credentials, which is a larger risk than the one it solves | Bounded concurrency and batch size, jittered scheduling, per-host backoff, and `Retry-After` honoured; the whole feature can be disabled |
| R15 | **Anyone who can reach the API can withdraw or disable a policy**, which stops it being evaluated and resolves its open violations | **Medium** | Follows from R1; the policy surface has no separate authentication because HarborMaster has none | The definition and every violation it found are retained, so the change is visible and reversible rather than destructive; loopback bind and an authenticating proxy |
| R16 | **A policy is only as good as the configuration HarborMaster can see.** Capability rules evaluate declared `capAdd` and cannot see the daemon's default set; `networkAllowlist` evaluates attached networks and cannot distinguish `container:<id>` namespace sharing from having no network | **Medium** | The alternative is hardcoding a claim about a daemon HarborMaster has not asked, which would be confidently wrong rather than honestly narrow | Stated in each rule's catalogue description, in the violation's reason, and in the editor; the network rule fails closed on an empty attachment set |
| R17 | **Compliance reflects the last inventory refresh, not the live host.** A container changed between refreshes is reported against stale configuration until the next pass | **Low** | Evaluating against the daemon on demand would put a Docker call behind an unauthenticated endpoint | A pass runs after every successful refresh and after a targeted refresh commits; the inventory generation is recorded on every violation |
| R22 | **A risk score is a judgement, not a measurement.** The weights are chosen by people and can be wrong for a given estate; a plan that reads "proceed" is not a guarantee the change is safe | **Medium** | Any risk model has this property, and the alternative — no assessment — leaves an operator with the same decision and less information. Making the weights configurable would make plans irreproducible between deployments, which costs more than it gains | Every factor names the rule that produced it and its contribution, so a verdict is auditable rather than opaque; the planner version is recorded on every plan and a rule change forces regeneration; nothing acts on a plan automatically |
| R23 | **A plan is only as fresh as its inputs.** It rests on the last inventory refresh, the last registry lookup, and the last drift and policy passes, so a world that moved since any of those is assessed against stale evidence | **Low** | Reading live state would put a Docker call and a registry call behind an unauthenticated endpoint, which is a larger risk than staleness | A pass runs after every successful inventory refresh; each plan records when it was generated and the registry status it rested on; the UI reports a non-OK registry status as `cannot advise` rather than as a verdict |
| R24 | **The freshness rule can act on a stale clock.** The evaluation time is excluded from the fingerprint deliberately, so an image crossing the 48-hour freshness boundary does not by itself produce a new plan | **Low** | Including a clock would make every fingerprint unique and defeat duplicate suppression entirely, which is what keeps the table from growing on every refresh | Documented at the fingerprint; the next genuine input change regenerates the plan, and the factor is worth 8 points of 100 |
| R14 | **A wedged background task can be abandoned at shutdown** once the grace period elapses | **Low** | An unbounded wait is not recoverable; an abandoned SQLite transaction is rolled back by the database | Logged at error level with what was abandoned; raise `SHUTDOWN_TIMEOUT` |

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
- A diagnostic, backup, or storage-inspection capability is proposed for the
  **HTTP surface**. Today they are commands requiring shell access, and moving
  one to a route changes which actor can perform reconnaissance.
- A new command writes a file outside the data directory, or writes one that
  carries database contents.
