# HarborMaster Threat Model

**Status:** current as of Phase 10 (manual rollback)
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

**As of Phase 10, HarborMaster issues three kinds of write to that socket**, and
all three are off by default:

1. **Pulling an approved, digest-pinned image** (Phase 8). One method, on its
   own interface, reachable from one service. Adds to the local image store,
   removes nothing, touches no container.
2. **Recreating ONE container** (Phase 9). Five methods — create, start, stop,
   rename, remove — on their own interface, reachable from one different
   service.
3. **Rolling ONE recreation back** (Phase 10). Four methods — stop, rename
   aside, rename back, start — on their own interface, reachable from one third
   service. It creates nothing and **removes nothing**, and its two rename
   operations are one-way: the aside rename requires a rollback marker in the
   new name and the restore rename refuses any HarborMaster-derived marker.

None of them changes consequence 1. The process was already root-equivalent by
virtue of holding the socket at all, and the defence was never "HarborMaster
only reads". What they change is the set of things a bug in HarborMaster's own
logic could cause, so that set is bounded deliberately and narrowly:

- Both capabilities are `nil` unless the deployment opts in, so a default
  HarborMaster holds no write access at all — absent, not merely unused.
- Neither interface can be reached by any package outside its owning service,
  enforced by source-level architecture tests that name `internal/api` as
  excluded.
- The container capability cannot exec, attach, copy, commit, kill, pause,
  update, or touch an image, a volume, or a network. It cannot force a removal
  and has no field for removing volumes.
- Every mutation targets a full 64-character container id read from the daemon
  moments earlier, so nothing can be aimed by name.

**The honest statement of what Phase 9 costs.** A bug in HarborMaster can now
stop a running container. The mitigation is not that it cannot happen — it is
that the original container is preserved through every failure path, that the
outcome is recorded before the original is removed, and that a person is handed
the exact steps to restore service. See R27 to R31.

**What Phase 10 adds to that cost.** A second path that can stop a running
container — the rollback — and it stops the container that is currently serving.
It is bounded the same way and in two further respects: it acts only on an
arrangement HarborMaster itself created and recorded, and it cannot destroy the
container it displaces. See R42 to R45.

**What Phase 11 adds to that cost, and what it does not.** Phase 11 introduces
NO fourth capability. The automation engine holds no Docker interface at all: it
submits the same three request types an operator's HTTP request produces, to the
same three services, each of which re-runs its full preflight against the live
host at the moment it acts. Eight architecture tests hold that.

What it changes is **who asks, and when**. Until Phase 11 every mutation
required a person at a keyboard; from Phase 11 a stored policy can cause one on
a timer, inside a maintenance window, with nobody watching. That is a real
change in exposure and it is stated as one:

- A mistake in a policy's SELECTOR now reaches containers nobody intended,
  repeatedly, rather than once.
- A bad image published upstream now reaches the host without a human between
  the publication and the pull.
- The blast radius of a compromised administrator session grows: writing one
  policy is a standing, unattended grant of `execution:create` over every
  container that policy selects.

Off by default, refused at startup unless recreation AND rollback are also on,
bounded to one concurrent update and ten per pass by default, and every new
policy starts in a mode that changes nothing. See R46 to R52.

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
| **TB1** | Network → HTTP API | **Authenticated by default and by construction.** Server-side sessions in HttpOnly cookies, role-based authorization decided in one place, CSRF on every write, loopback bind, security headers, strict input validation, rate limiting, body/pagination limits |
| **TB1a** | Anonymous network → the four public routes | `GET /health` (reduced body), `GET /version`, `POST /auth/login`, `GET /auth/bootstrap`. Nothing else answers without a session; an architecture test fails the build if a fifth appears |
| **TB2** | HarborMaster → Docker socket | Seven read-only methods; architecture test; SDK confined to `internal/docker`; all errors sanitised |
| **TB3** | Process → SQLite | Parameterised queries only; sort allowlists; no plaintext secrets; `0750` directory |
| **TB4** | CI → registry | SHA-pinned actions, least-privilege tokens, OIDC keyless attestation, SBOM, provenance |

**TB1 was the weakest boundary in the system until Phase 9.5, and it is the one
that phase exists to close.** It is now an authenticated boundary: every route
except four requires a server-side session, and every route that changes
something additionally requires a permission its role must hold and a CSRF token
its session derives.

Two things about it are worth stating precisely.

**There is no setting that turns it off.** HarborMaster fronts a root-equivalent
socket, and an "auth off for convenience" switch is a switch that ends up on in
production. Authentication is not a feature flag; it is the shape of the
routing.

**Default deny is a property of the type system, not of diligence.** Every route
declares an access policy, the zero value of that policy is invalid rather than
"public", and a route registered without one is refused at runtime and fails
`TestEveryRouteDeclaresAnAccessPolicy` at build time. "I forgot" and "I meant
public" have to look different, and with a zero value that means public they do
not.

**What TB1 still does not do** is authenticate the operator to anything other
than HarborMaster, terminate TLS, or federate with an identity provider. A
deployment beyond loopback still wants a TLS-terminating proxy — see R9 — and
`__Host-` cookie prefixing only engages over HTTPS.

---

## 5. Attack surface

### Network-reachable without a session

Four routes, and each has a stated reason. This list is the whole of
HarborMaster's unauthenticated surface, and
`TestThePublicSurfaceIsExactlyTheDocumentedRoutes` fails the build if a fifth
appears.

| Surface | Methods | Why it must be public | What it discloses |
| --- | --- | --- | --- |
| `GET /api/v1/health` | GET | A container runtime's HEALTHCHECK cannot hold a session | **One field**: the overall status. The full report — database path, journal mode, Docker API version, how long the daemon has been unreachable — is returned only to an authenticated caller |
| `GET /api/v1/version` | GET | Deployment identification by tooling that holds no credential | A version, a commit, a build date. Non-sensitive by construction |
| `POST /api/v1/auth/login` | POST | It is how a session is obtained | One 401 for every credential failure, with matching timing. It is not an account directory |
| `GET /api/v1/auth/bootstrap` | GET | A client must choose between the sign-in page and the bootstrap page before it holds anything | One boolean: whether an administrator exists |

`POST /api/v1/auth/bootstrap` is a fifth route with a policy of its own: it
answers only while the installation is unclaimed, needs the one-time token
printed at startup, and answers `404` once an administrator exists.

Everything else answers `401` without a session and `403` when the account's
role does not hold the route's permission. The SPA shell and its static assets
are served without a session because they contain no data — the bundle's first
act is to call an authenticated endpoint, so an unauthenticated visitor gets a
sign-in page and nothing else.

### Network-reachable with a session

| Surface | Methods | Notes |
| --- | --- | --- |
| 55 REST endpoints under `/api/v1` | Mostly GET | Full list in `api/openapi.yaml`; a test fails the build if the router and the document disagree |
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
| `POST /api/v1/acquisitions` | POST | Downloads an approved, digest-pinned image into the local image store. Changes no container. Off by default |
| `POST /api/v1/acquisitions/{id}/cancel` | POST | Stops a download. Changes nothing on the host |
| 2 acquisition read endpoints | GET | The history and one record with its audit trail |
| `POST /api/v1/executions` | POST | **The only endpoint that changes something RUNNING.** Stops one container and replaces it with one built from its own configuration and an already-verified local image. Body carries an acquisition id and an optional idempotency key; unknown fields rejected. Off by default |
| `POST /api/v1/executions/{id}/cancel` | POST | Stops a recreation that has not yet changed anything. Refused past the mutation point |
| 2 execution read endpoints | GET | The history and one record with its verification results, recovery plan, and audit trail |
| `GET /api/v1/events/stream` | GET | Long-lived SSE connection |
| SPA shell and static assets | GET | Served from an embedded FS |

**Policy administration is an ADMINISTRATOR permission, not an operator one**,
and the reason is worth stating: a policy is what blocks an acquisition or a
recreation, so an operator able to edit one could remove the gate standing in
their way. Creating, editing, and withdrawing a rule needs `policy:manage`;
annotating a violation and requesting a pass need only `policy:annotate` and
`policy:evaluate`, which an operator holds.

The write surface itself is unchanged: it acts on HarborMaster's own rows, it
cannot change a container, it cannot reach the Docker socket, it cannot destroy
violation history, and it cannot supply anything that is interpreted as code.
The bounds on policy size, rule count, value count, and pattern shape are what
stop it being used to make evaluation expensive; the per-process rate limit and
the asynchronous evaluate endpoint are what stop it being used to occupy the
process.

**The acquisition surface was the significant change in Phase 8**, and it is
worth being explicit about what an OPERATOR can do with it. It is no longer
reachable anonymously: `acquisition:create` is an operator-or-above permission,
and every request is recorded against the account that made it.

They can cause HarborMaster to download an image to the host — but only an image
that a current change plan recommends, for a container that exists, from a
registry the inventory already references, at a digest the registry is currently
serving. They cannot supply a target: the request body carries a plan id and
nothing else, and unknown fields are rejected. They cannot cause a container to
change through this endpoint: the acquisition service does not hold the
container mutation capability.

The realistic abuses are therefore resource abuse rather than compromise:
repeatedly requesting downloads to consume disk or uplink. That is bounded by
global and per-registry concurrency, a pull timeout, the write rate limiter, the
duplicate-work index, and the fact that a plan must independently recommend each
image. An attacker who can reach the port can also **cancel a legitimate
download**, which is a nuisance rather than a compromise: nothing is left in a
partial state that HarborMaster reports as acquired.

The disk-consumption risk is real and is recorded as R25. It is now an
ATTRIBUTABLE abuse rather than an anonymous one: every acquisition and every
recreation carries the account, session, request id, and source address that
caused it, which is the difference between an incident you can investigate and
one you can only observe.

**The recreation surface is the significant change in Phase 9**, and it is the
most consequential thing in this document after R1 itself.

Under R1, an anonymous caller who can reach the port and finds a deployment with
`EXECUTION_ENABLED=true` **can take a container down**. Not permanently, not
silently, and not arbitrarily — but down.

What bounds it:

- **They cannot choose the container or the image.** The request carries an
  acquisition id. That acquisition must have SUCCEEDED, must be fresh, and must
  not have been used before; its plan must still be current and still recommend
  the change; the container must still exist and still be on the assessed image;
  the inventory, the policy evaluation, and the registry evidence must all be
  fresh; a usable snapshot must exist; and the image must still be present
  locally carrying the approved digest. Every one of those is re-checked
  immediately before anything is stopped.
- **They cannot manufacture an opportunity.** An acquisition only exists because
  an operator (or, under R1, an attacker) already went through the acquisition
  path, which has its own full preflight. Single use means each one buys at most
  one recreation.
- **One at a time.** `EXECUTION_MAX_CONCURRENT` defaults to 1 and is capped at
  4, and a partial unique index allows one active recreation per container.
- **The container comes back.** The original is stopped and PARKED rather than
  removed, and is removed only after every proof passes and the success is
  durably recorded. Every failure path leaves it on the host, and the record
  carries the commands to restore it.

What is NOT bounded, and is stated rather than mitigated: the container is
unavailable while the replacement starts and is verified, which is up to
`EXECUTION_STARTUP_TIMEOUT` (five minutes by default). Recorded as R27.

The design decision worth defending explicitly is the absence of AUTOMATIC
rollback. An automatic undo would run at exactly the moment HarborMaster has
demonstrated that its model of the host is wrong, and it would run unattended.
Preserving both containers and handing a person precise instructions is slower
and less impressive, and it cannot make a bad situation worse.

**Manual rollback (Phase 10) does not weaken that.** It is the same decision
with the person put back in: a rollback happens only when an operator asks for
it, on one recorded recreation at a time, after HarborMaster has re-verified
both container identities against the live host. It derives every identity from
its own record of that recreation — the request body has an execution id and
nothing else — and it removes nothing, so the failed replacement remains
available as evidence. When a rollback itself fails after changing the host it
behaves exactly as a failed recreation does: it stops, preserves both
containers, records the checkpoint, and writes manual steps. It never guesses
which container should serve traffic. The new risks are recorded as R42 to R45.

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
| Registry manifest and tag-listing endpoints | **Outbound HTTPS** | Phase 6. Anonymous GETs only. Destinations derive solely from image references the inventory holds. On by default |
| Notification destinations | **Outbound HTTPS and SMTP** | Phase 12. Destinations an ADMINISTRATOR configured. Off by default |

There are **two** outbound egresses, and the difference between them is the
whole security story.

**Image intelligence** derives its destination from an image reference the
inventory already holds. No caller parameter becomes a network destination, and
`POST /images/refresh` takes no target of any kind. The registry is treated as
hostile input throughout — bounded before reading, decoded into a fixed shape,
and never echoed into a log, an error, or a column.

**Notifications** send to a URL a human typed, which is a strictly larger risk
and is why the subsystem is off unless a deployment asks. Three controls carry
it:

1. **Authorization.** Creating or editing a destination needs
   `notification:manage`, an administrator permission. An operator able to
   create one could exfiltrate every container name and update event this host
   produces to a server they control.
2. **A two-stage address guard.** The URL is validated when it is stored —
   HTTPS only, a hostname rather than an IP literal, no userinfo, bounded
   length — and the RESOLVED ADDRESS is checked again at dial time through the
   dialer's control hook. The second check is what makes DNS rebinding
   ineffective, and it is the one that decides. Redirects are refused rather
   than followed, no proxy is consulted, and the response body is bounded and
   discarded. Non-public addresses are refused unless
   `NOTIFICATIONS_ALLOW_PRIVATE_DESTINATIONS` is set, and link-local,
   multicast, CGNAT, benchmarking ranges, and `169.254.169.254` are refused
   even then.
3. **Nothing sensitive can be in the payload.** A notification's type has
   nowhere to put an environment value, a registry credential, a session token,
   or a raw Docker error. Every sentence is written in one file, and two
   architecture tests hold that: one fails the build if any other file
   constructs a notification, the other if that file interpolates a format verb
   or an error's text.

The destination credential — a webhook URL's path, an SMTP password — is a
separate TYPE in a separate table, reachable through exactly one repository
method and returned by no endpoint. An architecture test fails the build if the
type appears anywhere it could travel outward.

Turning `IMAGE_INTEL_ENABLED` and `NOTIFICATIONS_ENABLED` off removes both
surfaces entirely: no outbound request is made at all, which is the
configuration an air-gapped or egress-restricted deployment wants. Both are
independent, and image intelligence is the one that is on by default.

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
| **S**poofing | Guessing or stuffing a credential | Argon2id verification, exponential per-account backoff, per-address throttle, one indistinguishable 401 for every failure with matching timing via a decoy hash, minimum password length with a refusal list | Low |
| **S**poofing | Stealing a session token | HttpOnly + SameSite cookie, `__Host-` prefix over HTTPS, keyed digest at rest so a database copy yields nothing usable, idle and absolute expiry, revocation on password change, role change, and disablement | Low |
| **S**poofing | Forging the source address in the audit log | Forwarding headers ignored entirely unless a trusted-proxy CIDR is configured AND the peer is inside it; the chain is walked right to left and stops at the first untrusted hop | Low |
| **S**poofing | Racing the first administrator on a fresh installation | Claiming needs the one-time token printed at startup, so it requires log or filesystem access rather than being first to the port; the token is re-minted on every restart, stored as a keyed digest, and compared in constant time; both success and every rejection are audited | Low |
| **T**ampering | Malicious JSON to a POST | Strict decoding, unknown fields rejected, single object enforced, UTF-8 and control-character validation, size limits | Low |
| **T**ampering | Cross-origin write from a malicious page | A per-session CSRF token in a custom header (which a cross-origin form cannot set without a preflight that fails), `SameSite` on the cookie, `Content-Type: application/json` required, `Sec-Fetch-Site`/`Origin` checks, rate limiting. Only login and bootstrap are CSRF-exempt, because they have no session to derive a token from | Low |
| **T**ampering | Privilege escalation by editing one's own account | The account endpoints refuse an administrator modifying their own role or status, and refuse removing the last active administrator; both checks run inside the transaction that performs the change | Low |
| **R**epudiation | Forged log correlation | Client `X-Request-ID` ignored; server generates 12 random bytes | Low |
| **R**epudiation | Denying an action | Every authentication attempt, every authorization denial, and every state-changing request is recorded with the account, session, request id, and source address. Records are append-only: no endpoint edits or deletes one, and security records are retained far longer than operational ones | Low |
| **R**epudiation | Denying a HOST CHANGE | The two host-changing operations record their OUTCOME as well as their request, attributed to the requesting account, which is stored on the record so a worker finishing the job minutes later can still name who asked. A request and a completion are separate rows because they are separate facts | Low |
| **I**nfo disclosure | Secret in an API response | Digest fields unserialisable; sensitive values never stored | Low |
| **I**nfo disclosure | Stack trace or path in an error | All errors mapped to stable codes and generic messages; tested | Low |
| **I**nfo disclosure | Reconnaissance of the host estate | A session is required for every estate endpoint; the public health body is reduced to one field | Low |
| **I**nfo disclosure | A long-lived stream outliving its session | The event stream re-checks the session AND the `event:read` permission on every heartbeat, and ends with a `closed` frame the moment either stops holding. Fails closed: a lookup that errors ends the stream | Low |
| **I**nfo disclosure | Enumerating which accounts exist | One 401 for every credential failure, with the timing matched by hashing against a decoy; the per-address throttle is applied before the username is looked up so query timing is not a side channel either | Low |
| **I**nfo disclosure | A password or token in a log, a response, or the audit table | No column exists that could hold one; the API's account projection carries no credential field; `TestCredentialMaterialIsConfinedToStoreAndService` fails the build if the verifier type reaches the HTTP layer, and `TestNoAuditRowEverContainsASecret` sweeps a real recorded log for every secret that was in scope | Low |
| **D**enial of service | Unbounded pagination | `pageSize` capped at 200, rejected not clamped | Low |
| **D**enial of service | Expensive diffs | 4 concurrent max, 5s timeout, 1000-entry cap, 429 not queued | Low |
| **D**enial of service | Snapshot flooding | Rate limit, `(container_id, checksum)` dedup index, per-container capture lock | Low |
| **D**enial of service | SSE connection exhaustion | Subscriber cap with `Retry-After`; bounded per-subscriber queues | Low |
| **D**enial of service | Refresh flooding against the socket | Rate limit, single-flight refresh lock | Low |
| **D**enial of service | Locking a legitimate operator out by guessing at their username | Exponential backoff to a bounded ceiling rather than a hard lockout; a lockout would turn an authentication control into a denial-of-service tool | Low |
| **D**enial of service | Making the server hash unbounded input from an anonymous endpoint | Password length bounded before hashing and refused with the same 401 as a wrong password; Argon2id parameters bounds-checked at construction and again per credential, so a corrupt row cannot request a gigabyte | Low |
| **D**enial of service | Unbounded session or audit growth | Per-account session cap with oldest-first eviction in the same transaction as the insert; a bounded sweeper expires and prunes; audit retention runs on two cutoffs with a bounded batch | Low |
| **D**enial of service | Recreation flooding to take containers down | Off by default; one active recreation per container (database index) and `EXECUTION_MAX_CONCURRENT` at 1 by default, 4 maximum; every recreation consumes a single-use acquisition that itself needed a recommending plan; write rate limiter | **Medium if enabled — see R28** |
| **E**levation | Reaching Docker through the API | The API layer holds NO mutation capability. Architecture tests fail the build if `internal/api` names the image acquirer or the container mutator, so a handler cannot bypass the preflight even by importing the adapter | Low |

### TB2 — HarborMaster to Docker socket

| Threat | Vector | Mitigation | Residual |
| --- | --- | --- | --- |
| **T**ampering | A future method mutates Docker | `TestRuntimeExposesNoMutationMethods` and `TestRuntimeSurfaceIsTheExpectedReadOnlySet` fail the build on the read-only interface; `TestTheMutationSurfaceIsExactlyOneMethod` and `TestTheMutationInterfaceCannotTouchAContainer` fail it on the write interface | Low |
| **T**ampering | The pull capability is used from somewhere that skips the preflight | `TestTheMutationCapabilityIsNotReferencedOutsideItsOwners` fails the build if any package outside the acquisition service names it | Low |
| **T**ampering | An attacker aims the pull at content of their choosing | The API accepts a plan id, not a target. The digest comes from the plan and the registry record, both server-side; unknown request fields are rejected | Low |
| **T**ampering | A tag moves between approval and transfer | Pulls are digest-pinned with no tag-producing branch, and the digest is re-checked immediately before the transfer | Low |
| **T**ampering | The daemon or registry serves different content than requested | The image is re-inspected read-only after the pull and its digest and platform compared; a mismatch fails closed and is never retried | Low |
| **D**enial of service | Repeated pulls exhaust disk or saturate the uplink | Global and per-registry concurrency limits, a pull timeout, a request deadline, and a duplicate-work index. Requests are manual and rate limited | Medium |
| **D**enial of service | A hostile registry floods the progress stream | Bounded in three independent places: the adapter truncates and rate-limits, the service caps its writes, the repository caps stored rows | Low |
| **T**ampering | The container mutation capability is used from somewhere that skips the preflight | `TestTheRecreationCapabilityIsNotReferencedOutsideItsOwners` fails the build if any package outside the execution service names `ContainerMutator`, `ConfigCapturer`, `CapturedConfig`, or any of the five method names | Low |
| **T**ampering | The container mutation surface grows quietly | `TestTheContainerMutationSurfaceIsExactlyFiveMethods` pins the count and the names; `TestTheContainerMutatorCannotReachAnythingElse` refuses exec, attach, copy, commit, image, volume, and network verbs on it | Low |
| **T**ampering | A mutation is aimed at a container other than the one that was checked | Every request carries a full 64-character container id, validated at the adapter. Nothing can be aimed by name, so no name-resolution window exists | Low |
| **T**ampering | A recreation silently weakens the replacement's security posture | The replacement is re-inspected read-only and compared field by field against the original, including privileged, readonly rootfs, no-new-privileges, capabilities, security options, sysctls, devices, and namespaces. Any divergence fails closed and the original is not removed | Low |
| **T**ampering | A crash leaves the host in a state HarborMaster misreports | A checkpoint is written after every mutation and before the next. Recovery reads checkpoints and issues no Docker call. An unconfirmed stop is reported as unconfirmed, not as "nothing changed" | Low |
| **I**nfo disclosure | The create payload carries real secrets out of the adapter | `CapturedConfig` holds them in unexported fields; `LogValue`, `String`, and `MarshalJSON` are redacted; an architecture test pins the exported surface; round-trip tests put a known secret through `fmt` (including `%#v`), `slog`, and `encoding/json` | Low |
| **D**enial of service | A recreation holds a container down indefinitely | `EXECUTION_STARTUP_TIMEOUT` bounds the wait, the whole mutating half runs under a derived budget, and the poll interval is bounded below so the wait cannot busy-loop the socket | Low |
| **E**levation | A recreation is used to run something the plan never approved | The container is created from the digest-pinned reference the acquisition verified. Config, host config, and networking come from the container's OWN inspection and are never assembled from caller input; there is no request field for a command, mount, capability, or privilege flag | Low |
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
| R1 | ~~**No authentication.**~~ **RESOLVED in Phase 9.5.** Every route except four requires a server-side session; there is no setting that disables it | — | — | Retained here rather than deleted, because R1 is referenced throughout this document's history and because "this was once true" is what a reader of an older deployment needs to know |
| R2 | **Docker socket access is root-equivalent.** A compromise of the process is close to a host compromise | **High** | Inherent to the product's purpose | Read-only adapter, distroless non-root image, dropped capabilities, `no-new-privileges`, resource ceilings |
| R3 | **HMAC key beside the database.** An attacker with the data volume gets both | **Medium** | Convenient default for standalone use | Supply the key via `..._KEY_FILE` and a Docker secret |
| R4 | **Base images pinned by tag, not digest** | **Medium** | Digest pinning needs registry lookups and regular rotation | Dependabot `docker` ecosystem is configured to keep them current |
| R5 | ~~**No RBAC.**~~ **RESOLVED in Phase 9.5.** Three roles with fixed permission sets; every route declares the permission it needs and a route without a policy fails the build | — | — | — |
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
| R15 | **An administrator can withdraw or disable a policy**, which stops it being evaluated and resolves its open violations | **Low** | Somebody has to be able to change the rules, and the alternative — rules nobody can withdraw — makes a wrong rule permanent | Requires `policy:manage`, which an operator deliberately does not hold, so the person a policy blocks cannot remove it; the definition and every violation it found are retained; the change is recorded in the audit log against the account that made it |
| R16 | **A policy is only as good as the configuration HarborMaster can see.** Capability rules evaluate declared `capAdd` and cannot see the daemon's default set; `networkAllowlist` evaluates attached networks and cannot distinguish `container:<id>` namespace sharing from having no network | **Medium** | The alternative is hardcoding a claim about a daemon HarborMaster has not asked, which would be confidently wrong rather than honestly narrow | Stated in each rule's catalogue description, in the violation's reason, and in the editor; the network rule fails closed on an empty attachment set |
| R17 | **Compliance reflects the last inventory refresh, not the live host.** A container changed between refreshes is reported against stale configuration until the next pass | **Low** | Evaluating against the daemon on demand would put a Docker call behind an unauthenticated endpoint | A pass runs after every successful refresh and after a targeted refresh commits; the inventory generation is recorded on every violation |
| R25 | **An authenticated operator can cause image downloads** and consume disk space and uplink bandwidth | **Low** | Downloading an approved image is the feature. Requiring a second approval for each would make the capability unusable without making it safer | Off by default; needs `acquisition:create`, which a viewer does not hold; every acquisition needs a plan that independently recommends it; global and per-registry concurrency limits, a pull timeout, a write rate limiter, and a duplicate-work index; every request is attributed in the audit log |
| R26 | **A pulled image consumes disk that HarborMaster does not reclaim.** There is no delete or prune capability, so acquired images accumulate until an operator removes them | **Low** | Adding image deletion would be a second, larger mutation capability -- one that can destroy something rather than add to it -- and is a worse trade than accumulation | Concurrency limits bound the rate; `docker image prune` is an operator's tool; every acquisition is recorded, so what was downloaded is always attributable |
| R27 | **A digest mismatch means unapproved content is on the host.** Verification catches it and records it, but the layers are already in the local store | **Low** | The bytes arrive before anything can inspect them; catching it afterwards is the only point at which it CAN be caught | Never reported as acquired, never retried automatically, logged at error level with the evidence; no container is changed, so nothing runs it |
| R28 | **An operator can take a container down.** With `EXECUTION_ENABLED=true`, an account holding `execution:create` can spend a succeeded acquisition on a recreation and interrupt the service for as long as the startup verification takes | **Medium if enabled** | Recreating a container is the feature, and the account that can do it is the account an administrator decided should. What remains is that an operator's mistake, or a compromised operator session, reaches a running service | Off by default, and refused at startup unless acquisition is also on; needs `execution:create`, which a viewer does not hold; each recreation consumes a single-use acquisition that itself needed a recommending plan; one active recreation per container; `EXECUTION_MAX_CONCURRENT` defaults to 1; the original is preserved through every failure; the request is logged at WARN and recorded against the account, session, and address that made it |
| R29 | **A container is unavailable while its replacement is verified.** Up to `EXECUTION_STARTUP_TIMEOUT` — five minutes by default — plus the stop and create | **Medium** | Removing the wait would mean removing the verification, which is the only thing that establishes the replacement works. A recreation that reported success on an unproved container would be worse than a slow one | Tune `EXECUTION_STARTUP_TIMEOUT` down for fast-starting services; a container with a health check gets a verdict as soon as the daemon has one rather than waiting out the clock; an explicit `unhealthy` fails immediately |
| R30 | **A failed recreation leaves two containers on the host and does not restore service by itself** | **Medium** | Deliberate. An automatic rollback is another unattended mutation performed at exactly the moment HarborMaster has demonstrated its model of the host is wrong | The original is never removed before the replacement is fully proved; the failed replacement is stopped and renamed off the production name; the record carries a `recovery` plan naming both containers by name and id with the exact commands; the summary counts these separately as `needsAttention`, and retention never prunes them |
| R31 | **A recreation interrupted between a mutation and its checkpoint leaves HarborMaster uncertain what it did** | **Low** | Certainty here would need a two-phase commit against the Docker daemon, which the Engine API does not offer | The window is one Docker call wide; the pipeline stops rather than acting again on an uncertain record; recovery reports the uncertainty as uncertainty and tells the operator which single question to answer, rather than guessing in either direction |
| R32 | **Configuration preservation can only reproduce what the daemon reports.** A container created with tooling that keeps state outside the container's own inspection — an external orchestrator's bookkeeping, for instance — is reproduced faithfully as Docker sees it and not as that tooling sees it | **Medium** | HarborMaster reads one source of truth, and inventing a second would mean guessing | The projection is compared field by field and any divergence fails closed with the original preserved; Compose-managed containers keep their Compose labels, so `compose up` continues to recognise the replacement; anonymous volumes are carried forward explicitly rather than recreated empty |
| R22 | **A risk score is a judgement, not a measurement.** The weights are chosen by people and can be wrong for a given estate; a plan that reads "proceed" is not a guarantee the change is safe | **Medium** | Any risk model has this property, and the alternative — no assessment — leaves an operator with the same decision and less information. Making the weights configurable would make plans irreproducible between deployments, which costs more than it gains | Every factor names the rule that produced it and its contribution, so a verdict is auditable rather than opaque; the planner version is recorded on every plan and a rule change forces regeneration; nothing acts on a plan automatically |
| R23 | **A plan is only as fresh as its inputs.** It rests on the last inventory refresh, the last registry lookup, and the last drift and policy passes, so a world that moved since any of those is assessed against stale evidence | **Low** | Reading live state would put a Docker call and a registry call behind an unauthenticated endpoint, which is a larger risk than staleness | A pass runs after every successful inventory refresh; each plan records when it was generated and the registry status it rested on; the UI reports a non-OK registry status as `cannot advise` rather than as a verdict |
| R24 | **The freshness rule can act on a stale clock.** The evaluation time is excluded from the fingerprint deliberately, so an image crossing the 48-hour freshness boundary does not by itself produce a new plan | **Low** | Including a clock would make every fingerprint unique and defeat duplicate suppression entirely, which is what keeps the table from growing on every refresh | Documented at the fingerprint; the next genuine input change regenerates the plan, and the factor is worth 8 points of 100 |
| R33 | **A stolen session is usable until it expires or is revoked.** HarborMaster has no device binding, no re-authentication step for a privileged action, and no second factor | **Medium** | A second factor is a substantially larger feature — enrolment, recovery codes, a lost-device path — and shipping half of one is worse than shipping none. Device binding is unreliable behind proxies and NAT | HttpOnly and SameSite cookies with `__Host-` prefixing over HTTPS, a CSRF token a script cannot read, idle and absolute expiry, a per-account session cap, and immediate revocation on password change, role change, or disablement; an operator can see and end their own sessions, and an administrator can reset a password to end all of them |
| R34 | **The installation key is a single point of failure for every session.** Session and bootstrap digests are keyed with the same installation key the snapshots use; replacing it signs everyone out | **Low** | A second key would be a second thing to back up, a second thing to lose, and a second way for a restore to half-work. Being signed out is the correct consequence of a key that no longer exists — honouring a digest that cannot be verified would mean not verifying it | Domain-separated purposes so a snapshot digest and a session digest are not interchangeable; the key file's permissions are checked at startup and by the console commands; documented in the reliability runbook |
| R35 | **Anyone who can read the server's log or its data directory can claim an unclaimed installation.** The bootstrap token is printed to standard output | **Low** | The alternative is a default account, which is how appliances end up on the internet with admin/admin. Filesystem or log access is a stronger bar than being first to the port, and somebody with it can already edit the database | The token is one-time, expires, is re-minted on every restart, is stored only as a keyed digest, and is compared in constant time; every rejected attempt is audited; the window closes permanently the moment an administrator exists |
| R36 | **A console operator can reset any password without knowing it.** `harbormaster admin reset-password` takes filesystem access as its authority | **Low** | Every authentication system needs an answer to "the only administrator left", and the alternatives — a default password, a permanent recovery account, a support endpoint — are backdoors that are always present. This one is only available to somebody who could already rewrite the users table by hand | Never reachable over HTTP, and an architecture test fails the build if `internal/api` so much as names the type; refuses to run against a world-readable database or key file without `--force`; the destructive session revocation is confirmed; every console operation is audited as coming from the local console rather than from an account |
| R37 | **A browser XSS would be able to act as the signed-in operator.** The session cookie is HttpOnly, so a script cannot read the token — but it can make requests that carry it, and it can read the CSRF token | **Low** | This is inherent to cookie-based sessions in a browser. Any scheme in which JavaScript can make an authenticated request is a scheme in which injected JavaScript can too | React escapes by default and the app contains no `dangerouslySetInnerHTML`; a strict Content-Security-Policy with no inline script; every operator-supplied string reaches the DOM as a text node; the CSRF token is held in a module variable rather than web storage, so it does not survive the page |
| R38 | **HarborMaster has no password-reset-by-email, no account recovery, and no SSO.** An operator who forgets their password needs another administrator or a console | **Low** | Deliberate scope. Email introduces an outbound channel and a delivery dependency; SSO introduces a second identity system and a much larger attack surface. Neither belongs in a single-host tool before the basics are solid | Any administrator can reset any other account's password; the console can reset any account including the last administrator |
| R39 | **An authenticated account can grow the audit table by provoking denials.** Every 403 is recorded, and a read is not rate limited, so a low-privilege account can add rows as fast as it can make requests | **Low** | Every alternative is worse. Dropping repeated denials would blind the log to exactly the pattern it exists to show -- somebody looking for a way in. Capping total rows would let an attacker push out older security records, which is evidence destruction. Rate-limiting reads would let a viewer exhaust an operator's budget | Rows are small and bounded in every field; retention prunes on two cutoffs; the actor is recorded on every row, so the flooding is attributable and the account can be disabled. Reaching a meaningful size needs millions of authenticated requests, which is a louder problem than the table |
| R40 | **A stream can deliver up to one heartbeat's worth of events after its session ends.** The re-authorization runs on the heartbeat, not on every frame | **Low** | Checking on every frame would put an indexed lookup on the hot path of a burst, and the window is the heartbeat interval -- 30 seconds by default -- of already-redacted event metadata to a client that held a valid session moments earlier | Lower `HARBORMASTER_EVENTS_STREAM_HEARTBEAT` to narrow it; the frames carry no secret, because redaction happens before storage |
| R41 | **On a plain-HTTP loopback deployment the session cookie is not marked Secure.** A browser refuses to send a Secure cookie over `http://127.0.0.1` on older versions, so marking it would break sign-in for the default standalone deployment | **Low** | The traffic never leaves the machine, so there is no network to intercept it on. The case that mattered -- plain HTTP from ANYWHERE ELSE -- was closed by this audit: a request whose peer is not loopback now yields a Secure cookie regardless of configuration, which makes an exposed plain-HTTP deployment fail loudly at sign-in instead of quietly shipping the token in the clear | Terminate TLS, or set `HARBORMASTER_COOKIE_SECURE=true` behind a proxy; the corresponding CodeQL alert is recorded in the release process with the same reasoning rather than suppressed |
| R42 | **An operator can take a container down by rolling one back.** With `ROLLBACK_ENABLED=true`, an account holding `rollback:create` can stop the container that is currently serving and start the one it replaced | **Medium if enabled** | Undoing a bad recreation is the feature, and the account that can do it is the account an administrator decided should. What remains is that an operator's mistake, or a compromised operator session, reaches a running service | Off by default, and refused at startup unless recreation is also on; needs `rollback:create`, which a viewer does not hold; the request names an EXECUTION and cannot name a container, so it can only undo an arrangement HarborMaster itself created and recorded; one active rollback per container and one successful rollback per recreation, both by unique index; `ROLLBACK_MAX_CONCURRENT` defaults to 1; the UI requires the container's name to be typed; the request and the outcome are both audited against the account that asked |
| R43 | **A container is unavailable while a rollback runs, and the gap is real.** The replacement is stopped BEFORE the original is started, and the original must then start and pass its checks — up to `ROLLBACK_STARTUP_TIMEOUT`, five minutes by default | **Medium** | There is no overlap to remove: two containers cannot hold one name, and starting the original first would mean a name collision at exactly the wrong moment. Removing the wait would mean removing the verification | Stated in the confirmation dialogue, the API description, and this document rather than mitigated away; tune `ROLLBACK_STARTUP_TIMEOUT` down for fast-starting services; a container with a health check gets a verdict as soon as the daemon has one |
| R44 | **A failed rollback leaves two containers on the host and does not restore service by itself** | **Medium** | The same decision as R30, and for the same reason: HarborMaster has just demonstrated that its model of the host is wrong, and correcting it automatically at that moment is the unattended mutation the whole design avoids. It never guesses which container should serve traffic | Nothing is ever removed, so both containers remain; the checkpoint records exactly which mutations completed; the record carries a `recovery` plan naming both containers by name and id with the exact commands; failures that changed the host are counted separately as `needsAttention` and retention never prunes them |
| R45 | **A rollback restores a container that may itself be the reason the recreation happened.** HarborMaster verifies that the original is the container the record names and comes back configured as it was; it cannot know whether running it is a good idea | **Low** | Judging that would mean re-running the change-planning assessment in reverse, on evidence gathered for a different question. The operator asking for the rollback is the one who knows why | The confirmation shows both container ids and both image references so the decision is made against content rather than a label; the image the original returns to is displayed before the request is sent; the whole history is retained, so a rollback of a rollback is a decision an operator makes with the same information |
| R46 | **A policy selector can match more containers than its author intended.** A glob like `ghcr.io/acme/*` reaches every current AND future container from that namespace, and automation acts on all of them | **Medium if enabled** | Selecting by pattern is the feature; requiring every container to be named would make the capability unusable on any estate large enough to want it | An empty selector governs NOTHING and is refused outright; a bare `*` image pattern is refused by name; `exclude` is checked first and cannot be overridden by any other clause or by any label; the policy editor renders what the selector means in words before it is saved; `GET /automation/upcoming` reports exactly which containers the next pass would touch, writing nothing; every new policy defaults to `observe`, which changes nothing; `automation:manage` is an administrator permission |
| R47 | **A hostile or compromised upstream publisher reaches the host with no human in between.** A republished tag carrying malicious content is pulled and run on the next pass inside the window | **Medium if enabled** | This is what unattended updating IS. A publisher you have chosen to track is a publisher you trust to that extent, and the alternative is not automating | Every acquisition is digest-pinned and the digest is COMPUTED from what arrived rather than believed; the change must carry a planner recommendation the policy permits; the strategy ceiling bounds how far a version may move and permits `unknown` and `prerelease` under no setting; `AUTOMATION_REQUIRE_APPROVAL_FOR_MAJOR` holds major versions for a person by default; the maintenance window bounds when; the recreation's health, image, preservation, and network proofs must all pass or the container is rolled back and paused |
| R48 | **Automation can take a container down at 02:00 with nobody watching.** A recreation that fails verification interrupts the service until somebody notices | **Medium if enabled** | Recreation was already able to do this (R28, R29); what changes is that no person is present when it happens | Off by default and refused without `ROLLBACK_ENABLED`, so a failed unattended update can always be undone; automatic rollback is on by default in the policy editor; a rollback ALWAYS pauses the container afterwards, whatever the counters say, so one bad image cannot become a repeated outage; `AUTOMATION_MAX_CONCURRENT` defaults to 1 and `AUTOMATION_MAX_PER_RUN` to 10, so a bad night takes one container down at a time and at most ten in total; the maintenance window is when an operator chose to be able to respond |
| R49 | **A container's owner can opt it out of automation with a label, and anyone who can run `docker run` can set a label** | **Low** | This is the direction that makes automation safer, and refusing it would mean a container that must not be touched has no way to say so | A label may ONLY narrow: `enabled=true` cannot enrol a container no policy selected, and `strategy` may only tighten the ceiling, never widen it. There is no label for the MODE. An unrecognised or unreadable `io.harbormaster.update.*` key is reported rather than silently ignored, so a misspelled safety label is visible |
| R50 | **A maintenance window depends on a timezone database and on the host clock.** A wrong clock, or a zone this build cannot resolve, changes when automation acts | **Low** | Every scheduler has this exposure, and the alternative — UTC only — makes the setting unusable for the operators who most want it | The IANA database is EMBEDDED in the binary, so it does not depend on the runtime image; an unresolvable zone fails CLOSED and refuses every update under that policy, reported as `windowUnresolvable`; comparisons are made in the window's own zone with DST applied, so a spring-forward gap opens nothing and an autumn-back repeat opens twice, both correctly; the dashboard reports the next opening from the server's own calculation rather than a second one in the browser |
| R51 | **Automation history is the fastest-growing table HarborMaster has.** Every pass records one row per container it considered, including the ones it declined | **Low** | Recording only the containers that changed would make "why was this NOT updated last night" unanswerable, which is the question the whole record exists for | Every field is bounded and every row is small; one pass's decisions are capped at 5,000 and the truncation is reported on the run rather than silent; `AUTOMATION_RETENTION_AGE` prunes runs and their decisions cascade; one pass's estate read is capped at 2,000 containers with the truncation reported; the policy load is capped at 200 |
| R52 | **An approval releases a decision that was made earlier.** Between the pass and the approval, the world can move | **Low** | The value of approval mode is that a person looks; making the decision instantaneous would remove the looking | The engine re-reads the container's CURRENT change plan before submitting and refuses with 409 if it no longer matches the one the decision named; a paused container is not approvable; the acquisition preflight re-verifies the plan, the digest, and the registry evidence a third time immediately before pulling; the approval is audited against the approver, and the acquisition and recreation are attributed to them rather than to the scheduler |
| R53 | **An administrator can point one policy at the whole estate in one click.** `scope: allEligible` selects every container HarborMaster may consider, so a single mis-set mode reaches every workload on the host rather than the handful a selector named | **Medium if enabled** | This is the feature. "Keep my containers updated" is the thing operators come to an updater for, and the previous answer — type a glob and hope it means what you think — was BROADER in practice and less visible: a pattern's reach is discovered by reading it, a scope's is declared. Refusing to offer it does not stop people wanting it; it makes them express it badly | Breadth is a FIELD with a two-value vocabulary and a database CHECK, never a string a selector could contain; it is never a default and cannot be reached by leaving anything blank; `observe` remains the default mode, so the first broad policy an operator writes changes nothing; the editor renders the whole policy as an English sentence generated from the request it is about to send; a broad policy in `automatic` mode earns an explicit warning naming what it can do; `exclude` is checked before the scope and wins; every container it selects still passes all fourteen downstream checks unchanged, which two architecture tests hold |
| R54 | **"All eligible" is a judgement about which containers HarborMaster may consider, and it can be wrong in the narrow direction.** A workload it declines to enrol — because its name could not survive a recreation, or because it looks like a container an earlier update parked — is one an operator may have expected to be covered | **Low** | The alternative polarity is far worse. A screening that failed OPEN would enrol the containers a broad policy must never touch, including the wreckage of a failed update; failing closed costs a missed update, which the operator can always fix by naming the container explicitly | The eligibility facts are POSITIVE and the zero value selects nothing, so an unscreened target is declined rather than swept in; the decision records `notEligible` with HarborMaster's own sentence saying which fact decided it, so the exclusion is readable rather than mysterious; an explicit `include` still reaches any container, because an operator naming one is an operator pointing at it |
| R55 | **A container that appears between two passes is enrolled by a broad policy without anybody deciding to enrol it.** A new workload on the host is governed by the next pass | **Medium if enabled** | Inherent to any standing rule that covers a set rather than a list, and the same is already true of an image glob. Requiring re-approval per container would make the scope pointless | The new container still needs a change plan the planner produced, a recommendation the policy permits, an open window, and every preflight; `AUTOMATION_MAX_PER_RUN` bounds how many containers one pass may start; `io.harbormaster.enabled=false` on the container opts it out before any policy is consulted, which is a control the container's OWNER holds; the pass records a decision for every container it considered, so what was newly enrolled is visible in the run |
| R14 | **A wedged background task can be abandoned at shutdown** once the grace period elapses | **Low** | An unbounded wait is not recoverable; an abandoned SQLite transaction is rolled back by the database | Logged at error level with what was abandoned; raise `SHUTDOWN_TIMEOUT` |

---

## 9. Assumptions

If any of these is false, this model does not hold.

1. The operator does not expose HarborMaster to an untrusted network without
   TLS. Authentication is now HarborMaster's own, but the session cookie is only
   as safe as the transport carrying it, and the `__Host-` prefix engages only
   over HTTPS.
2. The host's Docker daemon is not already compromised.
3. The data volume is not readable by untrusted users on the host.
4. The HMAC key is backed up and is not stored in the same backup as the
   database, if key hygiene matters to the deployment. It now also keys every
   session digest: losing it signs everyone out, and replacing it does the same.
5. The bootstrap token is treated as a credential. Anyone who can read the
   startup log of an unclaimed installation can claim it.
6. An administrator's account is protected at least as well as the host. There
   is no second factor, so a compromised administrator password plus network
   reachability is a compromised installation.
5. Operators verify image attestations before deploying.
6. GitHub's OIDC and attestation infrastructure is trustworthy.

---

## 10. Future work

Roughly in the order it should be done.

1. **A second authentication factor** — closes the largest part of R33. TOTP is
   the smallest useful shape; it needs enrolment, recovery codes, and a
   lost-device path, which is why it is a phase rather than an afternoon.
2. **Re-authentication for privileged actions** — requiring the password again
   for `execution:create` would bound what a stolen session reaches, at the cost
   of friction on the action operators perform under time pressure. Worth
   measuring before adopting.
3. **Secure secret injection** — the prerequisite for restore. Restoring a
   container with secrets requires values HarborMaster deliberately does not
   hold.
4. **Digest-pinned base images** — closes R4.
5. **HMAC key rotation tooling** — the metadata exists; the tooling does not.
6. **Audit log of read access** — who looked at what. The identity now exists;
   what is missing is a decision about volume, because recording every GET would
   make the table grow far faster than the security records that matter most.
7. **Optional host validation provider** — the interface exists and answers
   `unverifiable`; a real implementation must be opt-in and carefully bounded.

---

## 11. Review triggers

Revisit this document when any of the following happens:

- HarborMaster gains **any** ability to modify a Docker resource.
- The authentication or authorization model changes: a new role, a new
  permission, a new public route, or any change to how a session is issued,
  stored, or revoked.
- A second identity source is introduced (SSO, LDAP, a proxy-asserted header).
- A new network listener, protocol, or endpoint appears.
- A new LONG-LIVED response is added. A stream authorized once at connect is a
  standing grant, and every such surface needs its own re-authorization.
- A new operation changes the Docker host. It needs an OUTCOME audit record
  attributed to a requester, not only a request record.
- The secret handling design changes.
- A new trust boundary is introduced (a second host, an agent, a plugin).
- A dependency with access to the socket changes major version.
- A diagnostic, backup, or storage-inspection capability is proposed for the
  **HTTP surface**. Today they are commands requiring shell access, and moving
  one to a route changes which actor can perform reconnaissance.
- A new command writes a file outside the data directory, or writes one that
  carries database contents.
