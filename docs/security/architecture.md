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

## 2. The observation guarantee

HarborMaster's central claim used to be that it cannot change Docker. As of
Phase 9 that claim is no longer true as stated, so it is restated precisely
rather than quietly kept:

**Every service in HarborMaster observes Docker and cannot change it, except
two — and each of those holds exactly one narrow capability, granted by what its
constructor is handed.**

| Interface | Methods | Held by |
| --- | --- | --- |
| `docker.Runtime` | 7 reads | every service |
| `docker.ImageAcquirer` | 1 mutation: `PullByDigest` | the acquisition service |
| `docker.ConfigCapturer` | 1 read: `CaptureConfig` | the execution service |
| `docker.ContainerMutator` | 5 mutations: create, start, stop, rename, remove | the execution service |

Both capabilities are OFF by default and are `nil` unless the deployment opts
in, so a default HarborMaster holds no write access to its Docker host at all —
the capability is absent rather than merely unused.

`CaptureConfig` is a read and is deliberately NOT on `Runtime`: the value it
returns is the container-create payload, and every service receives `Runtime`.
Putting it there would hand a container's real environment to the drift engine,
the policy engine, and the planner, none of which has any use for it.

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

**The same discipline applies to the two write interfaces.** Each is pinned by
its own exact-set test, each by its own verb test, and each by a source-level
test that fails the build if any package outside its owning service so much as
names it. `internal/api` is absent from both allowlists: a handler that could
reach a mutation directly would bypass the preflight revalidation, which is the
whole safety model.

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

## 3a. Configuration drift

Drift compares a container's current configuration against its baseline
snapshot. It is an **observation**: there is no remediation path, no rollback,
and no call into `docker.Runtime` anywhere behind it.

| Property | Mechanism | File |
| --- | --- | --- |
| One comparison implementation | Drift runs `DiffEngine` and classifies its output; it compares nothing itself | `internal/service/drift.go` |
| No baseline selection surface | Baseline is the newest snapshot, resolved server-side; no caller-supplied id in the evaluation path | `internal/store/snapshot_repository.go` |
| Background cannot starve foreground | Drift owns a **separate** `DiffEngine` instance, so a sweep cannot exhaust the HTTP diff endpoint's slots | `NewDriftService` |
| An incomplete comparison resolves nothing | `resolveVanishedDrift` returns early when `Complete` is false | `internal/store/drift_repository.go` |
| Bounded under an event storm | Per-container coalescing, hard cap, overflow → sweep | `internal/service/drift_worker.go` |
| Table growth is bounded | `UNIQUE (container_id, snapshot_id, category, field)`; repeats upsert | `migrations/0005_drift.sql` |
| Engine/operator status split | `PATCH` validates against `OperatorDriftStatuses`, which excludes `active` and `resolved` | `internal/api/drift_handlers.go` |
| No snapshot data duplicated | A drift row references its baseline by id and copies nothing from it | `migrations/0005_drift.sql` |

**Secrets, four layers again.** The diff engine withholds a sensitive value;
`DriftRecord` never carries one; the repository blanks it a third time on the
way in *and* on the way out; and a `CHECK (sensitive = 0 OR (previous_value =
'' AND current_value = ''))` refuses the row. A whole-database sweep test with
a positive control proves no value reaches storage.

**Severity is direction-aware**, which is a security property rather than a
cosmetic one: ranking the field instead of the movement would rank an operator
*fixing* a container as critical, and a dashboard that cries wolf gets ignored.

## 3b. Policy engine

A policy is an administrator-defined rule that a container's configuration is
**checked against**. It is never applied, enforced, or pushed to the daemon:
there is no enforcement path, no remediation, and no call into
`docker.Runtime` anywhere behind it.

This is the first phase to add a create/update/delete surface, so what that
surface reaches matters. Every policy write acts on **HarborMaster's own rows**.

| Property | Mechanism | File |
| --- | --- | --- |
| No interpreter | Rules are a closed catalogue of 16 typed checks whose semantics are fixed at compile time; nothing an administrator writes is evaluated as code | `internal/domain/policy_rules.go` |
| Secrets unreachable by construction | Rules run against `PolicyTarget`, which carries env NAMES and has no field able to hold a value | `NewPolicyTarget` |
| No pattern can be made expensive | Iterative matcher with one backtrack point (no recursion, so no exponential path); pattern length, wildcard count and subject length all capped; over-budget patterns refused at write time | `internal/domain/policy_glob.go` |
| Definition cost is bounded | Rules per policy, values per rule, active policies per pass, and violations per container are all configured caps validated on write | `internal/config/config.go` |
| Identifiers are unguessable and immutable | `pol_` + 80 bits from `crypto/rand`; never accepted from a caller; `PolicyUpdate` has no field for it | `domain.NewPolicyID` |
| An incomplete pass resolves nothing | `resolveCompliantViolations` returns early when `Complete` is false | `internal/store/policy_repository.go` |
| Compliance cannot be asserted from ignorance | `CHECK (compliant = 0 OR (complete = 1 AND violation_count = 0))` | `migrations/0006_policy.sql` |
| Table growth is bounded | `UNIQUE (container_id, policy_id, rule_type)`; repeats upsert | `migrations/0006_policy.sql` |
| Engine/operator status split | `PATCH` validates against `OperatorPolicyStatuses`, which excludes `active` and `resolved` | `internal/api/policy_handlers.go` |
| History survives a withdrawal | `DELETE` archives; `ON DELETE RESTRICT` refuses a delete that would orphan violations | `migrations/0006_policy.sql` |
| No unauthenticated caller can drive a sweep synchronously | `POST /policy/evaluate` schedules and answers 202; coalesced by the shared evaluation queue and rate limited | `internal/api/policy_handlers.go` |
| No N+1 | The sweep loads the active set once and applies it to every container | `internal/service/policy.go` |

**Two rules are narrower than their names, and say so.** Capability rules
evaluate the *declared* `capAdd` — HarborMaster cannot see the daemon's default
capability set, and hardcoding Docker's default list would be a claim about a
daemon it has not asked. `networkAllowlist` evaluates *attached networks*,
which is how Docker surfaces the network mode; a container attached to no
network of its own shares another's namespace and fails an allowlist, which is
the fail-closed direction.

**Why no regular expressions.** Go's `regexp` is RE2 and has no catastrophic
backtracking, so ReDoS would not arise there either. Globs are still the better
choice: two metacharacters, no construct whose cost is non-obvious, and a
matcher short enough to reason about in full. Nothing in HarborMaster compiles a
caller-supplied regular expression.

## 3c. Image intelligence and outbound egress

Phase 6 gave HarborMaster its FIRST outbound network connection. Everything
before it read a local Docker socket and a local SQLite file; this reads a public
third party, which changes the shape of the threat model rather than adding to
it.

The capability is narrow: anonymous HTTPS GETs against registry manifest and
tag-listing endpoints. It cannot pull, push, delete, or prune, it holds no
credentials, and it has no dependency on `internal/docker` in either direction.

| Property | Mechanism | File |
| --- | --- | --- |
| Destinations come only from image references | `domain.NormalizeImageRef` refuses IP literals, ports, localhost, single-label names, userinfo, and anything that is not purely a hostname. No config value, API parameter, or column supplies a host | `internal/domain/registry.go` |
| The resolved ADDRESS is checked at dial time | `net.Dialer.Control` refuses loopback, private, link-local, unique-local, CGNAT, multicast and reserved ranges on the socket address, so DNS rebinding cannot get past it | `internal/registry/transport.go` |
| Redirects are refused outright | `CheckRedirect` always errors. A redirect is a registry-controlled URL; blob endpoints redirect and are never fetched | `refuseRedirect` |
| No proxy | `Transport.Proxy` is nil, so the destination is always the registry — a proxy is an internal address the dial guard exists to refuse | `newHTTPClient` |
| HTTPS only, verified | TLS 1.2 floor, no `InsecureSkipVerify`, no setting that disables either | `newHTTPClient` |
| The one registry-supplied URL is validated identically | The bearer-token realm clears `domain.ContactableRegistryHost`, must be https, may carry no userinfo, and has its query replaced with HarborMaster's own pull-only scope | `tokenURL` |
| A second HTTP client cannot appear unnoticed | An architecture test confines `net/http` and `crypto/tls` to four packages | `internal/arch` |
| Registry text never reaches a record | Every failure maps to a fixed HarborMaster phrase; response bodies are read only to be discarded | `registry.Classify` |
| Responses are bounded before they are read | Manifests, tag pages, and token responses have byte caps, and exceeding one is DETECTED rather than truncated | `internal/registry/client.go` |
| Digests are computed, not believed | The manifest digest is the SHA-256 of the bytes received, rather than the `Docker-Content-Digest` header | `Client.Manifest` |
| Publisher content is allowlisted | Annotations are limited to sixteen known keys, each bounded, UTF-8 validated, and refused if it carries a control character | `sanitiseAnnotation` |
| Credentials never persist | Anonymous pull-scoped tokens live in a bounded in-memory cache for minutes; the schema has no column for one | `internal/registry/client.go` |
| Work is bounded | Concurrency, batch size, tag pages, retries, and per-host backoff are all capped and configurable within limits | `internal/config` |

**Three conclusions the engine refuses to draw**, each because the alternative
would be confidently wrong:

- A **channel tag** (`latest`, `stable`, `main`) is never version-compared.
  Otherwise a repository that also publishes `2.0` would report a major update
  for every container tracking `latest`.
- A **truncated tag listing** yields `unknown`, not `none`. A listing that
  stopped at its budget has not established that no newer tag exists.
- A **failed lookup** overwrites nothing. "We could not ask" and "no update is
  available" are different claims, and the second is the dangerous one.

## 3d. Change planning

Phase 7 adds an ANALYSIS layer over data HarborMaster already holds. It is the
one feature that adds no new capability at all: no network call, no Docker call,
no new input source. The planner reads six existing tables and writes one.

| Property | Mechanism | File |
| --- | --- | --- |
| No Docker access, even indirectly | The planner's store interface holds six methods, none of which reaches Docker. Every input comes from persisted inventory | `internal/service/planner.go` |
| No network access | Nothing in the planning path constructs a request. The `net/http` architecture test still confines HTTP to four packages, and `internal/service` is not one of them | `internal/arch` |
| No mutation of a plan | `PlanStore` has insert, read, and prune. There is no update method, so immutability is structural rather than conventional | `internal/service/planner.go` |
| No mutable state on a plan | The `change_plans` table has no status, applied, approved, or executed column. A test asserts their absence, because adding one would be the first step toward treating a plan as a work item | `0008_plans.sql` |
| Determinism | Rules are a fixed slice of typed functions. No map is ever ranged over, scores are integers, and the clock arrives as an explicit field | `internal/domain/plan_risk.go` |
| Duplicate suppression is exact | SHA-256 over a sorted list of named fields, including the planner version. A unique index on `(container_id, input_digest)` makes it hold under concurrency | `plan_repository.go` |
| No N+1 | Input gathering is a batch operation in the repository: five grouped queries per batch whatever its size | `PlanRepository.GatherInputs` |
| Sort fields are allowlisted | The caller's string selects a compile-time column constant; rank expressions are built from literals only | `planSortFields` |
| No secret can reach a plan | `PlanInputs` has no field that could carry an environment value. The four-layer secret defence is upstream of every source the planner reads | `internal/domain/plan_risk.go` |
| Factor text is HarborMaster's own | Every `detail` string is a fixed phrase built by a rule. Never an error string, never registry text, never caller input | `internal/domain/plan_risk.go` |

**Three conclusions the model refuses to draw**, each because the alternative
would be confidently wrong:

- **Missing evidence is not safety.** A failed lookup, an unresolved digest, or
  an unclassifiable change forces `unknown` rather than reducing confidence in a
  verdict that still reads as "proceed".
- **A low total does not overrule a blocker.** A critical policy violation
  argues against a change whatever the score, because an organisation that
  marked a rule critical has already said the container should not be running as
  it is.
- **A failed registry lookup says nothing about platform support.** Silence is
  not an answer of "no", and reporting it as a mismatch would double-count a
  failure already scored.

## 3e. Safe image acquisition

Phase 8 gave HarborMaster its FIRST Docker mutation. This section states the
limit rather than the capability, because the limit is the design.

**HarborMaster can download an approved, digest-pinned image into the daemon's
local image store. It cannot change a container, and it cannot remove an image.**

| Property | Mechanism | File |
| --- | --- | --- |
| The image mutation surface is ONE method | `docker.ImageAcquirer` has exactly one method. Three architecture tests pin the count, refuse container verbs on it, and refuse any package outside the acquisition service from referencing it. This is still true after Phase 9: containers are changed through a DIFFERENT interface held by a DIFFERENT service, so a component able to pull is still unable to apply | `internal/arch` |
| The read-only surface is unchanged | `docker.Runtime` still has its seven observation methods, pinned by the pre-existing tests. Every other service receives that interface and therefore cannot pull | `internal/docker/inventory.go` |
| Capability is granted, not assumed | The acquirer is nil unless the deployment opted in, and is handed to exactly one service in `main` | `cmd/harbormaster/main.go` |
| No pull is expressible without a digest | `PullTarget.Reference()` has no branch that produces a tag, and `Validate` refuses an absent or malformed digest before the daemon is contacted | `internal/docker/acquire.go` |
| The target is not caller text | The request body carries a plan id; registry, repository, and digest are derived from the plan. Unknown JSON fields are rejected | `internal/api/acquisition_handlers.go` |
| The repository path is an allowlist | Refuses traversal, a second `@`, upper case, and anything outside the distribution character set, because the value is about to be parsed by someone else's reference parser | `validRepositoryPath` |
| The registry host clears the SSRF gate | Same `domain.ContactableRegistryHost` an image reference clears. The daemon performs the transfer, but a host HarborMaster would not contact is not one it should ask the daemon to contact | `PullTarget.Validate` |
| No credential can be supplied | `ImagePullOptions` is built inside the adapter with `RegistryAuth` and `PrivilegeFunc` unset, and no caller reaches it. A private repository fails as unauthorized | `Client.PullByDigest` |
| The pull is revalidated first | The full preflight runs again inside the worker, immediately before the transfer. This is the TOCTOU defence | `AcquisitionService.preflight` |
| The transfer is not the proof | The image is re-inspected through the read-only path; digest and platform are compared and a mismatch fails closed | `AcquisitionService.verify` |
| Progress is bounded three times | The adapter rate-limits and truncates, the service caps its writes, the repository caps stored rows | adapter / service / repository |
| Daemon errors never reach a response | Every failure maps to a fixed HarborMaster phrase keyed by classification; the Engine error is logged, never rendered | `classifyPullError` |
| Duplicate work is a database invariant | A partial unique index over the active states, so two racing requests cannot both start | `0009_acquisitions.sql` |
| Interrupted work is failed, not resumed | An unverified transfer must never be recorded as acquired | `RecoverInterrupted` |

**Four conclusions the feature refuses to draw**, each because the alternative
would be confidently wrong:

- **A successful pull is not a successful acquisition.** Only verification can
  conclude that, and a check that could not be performed establishes nothing.
- **Missing evidence is not approval.** A plan whose recommendation is `unknown`
  refuses alongside one that recommends against the change.
- **An old registry lookup does not establish a current digest.** Evidence past
  the freshness window refuses rather than being used.
- **A restart does not license resuming a transfer.** The image on the host now
  may not be the one that pull produced.

## 3f. Manual container recreation

Phase 9 gave HarborMaster its first CONTAINER mutation, and its largest
privilege. This section states the limits, because the limits are the design.

**HarborMaster can replace ONE container, ONCE, on an image an operator already
downloaded and HarborMaster already verified, when a current plan recommends
it.** It cannot roll back, cannot act on more than one container, cannot run on
a schedule, and cannot retry.

### The pipeline, and where the point of no return is

```
queued → validating → capturing │ creating → starting → verifying → succeeded
└──── changes nothing ─────────┘ └──── the host is being changed ────┘
        freely cancellable              cancellation refused
```

The transition into `creating` is the MUTATION POINT. Before it, an operator can
cancel and nothing has happened. After it, cancellation is refused and the
in-process cancel function is unregistered — a recreation that has stopped a
container must reach a RECORDED conclusion, because an abandoned one leaves a
host in a state nobody chose and nobody wrote down.

### The checkpoint is what survives a crash

`state` says what HarborMaster was doing. `checkpoint` says what is TRUE OF THE
HOST, and it is written after each Docker mutation succeeds and before the next
is attempted. After a crash only the second question matters.

**A checkpoint that cannot be written stops the pipeline.** It is the one write
in HarborMaster whose failure is itself a safety event: the host has been
changed and HarborMaster cannot prove it recorded the fact. It does NOT retry
the mutation. Repeating a stop, a rename, or a remove against a host whose
recorded state is uncertain is how a recoverable situation becomes an
unrecoverable one.

Restart recovery therefore reads checkpoints and issues **no Docker call at
all**. It settles each interrupted row from its own checkpoint and attaches a
manual recovery plan. Resuming would mean continuing a mutation sequence whose
last step nobody watched; undoing would mean mutating on the strength of the
same uncertainty.

### The properties

| Property | Mechanism | File |
| --- | --- | --- |
| The container mutation surface is FIVE methods | `docker.ContainerMutator` pins create, start, stop, rename, remove. Four architecture tests pin the count and names, refuse image/exec/volume/network verbs on it, pin the capture interface at one read, and refuse any package outside the execution service from naming any of it | `internal/arch` |
| No SDK option struct is reachable | Every method takes a HarborMaster-owned request. There is no field for a command, a mount, a device, a capability, or a force flag | `internal/docker/recreate.go` |
| Mutations target a FULL container id | Exactly 64 lowercase hex, validated at the adapter. Nothing can be aimed by name, so no window exists in which a name resolves to a container other than the one that was checked | `validContainerID` |
| Remove cannot force and cannot delete volumes | Both hardcoded false, and `RemoveRequest` has exactly one field. A container's data is not HarborMaster's to delete, and forcing would discard the caller's evidence that it was stopped | `Client.RemoveContainer` |
| The create payload never leaves `internal/docker` | `CapturedConfig` holds the environment, log options, and SDK structs in UNEXPORTED fields. The service holds the value and hands it back; it cannot read, log, or serialise it | `CapturedConfig` |
| The secret boundary is tested three ways | Unexported fields (the compiler); an architecture test pinning the exported field and method sets; round-trip tests putting a known secret through `fmt` (including `%#v`), `slog`, and `encoding/json` | `internal/arch`, `internal/docker/recreate_test.go` |
| The service sees a VALUE-FREE projection | `Summary()` renders environment NAMES, mount destinations, network names, and capability lists. A sensitive value contributes a keyed digest under the installation key the snapshots use | `domain.BuildPreservationSummary` |
| The original is parked, never removed early | Stopped and renamed `<name>.hm-old-<executionId>`. Removed only after all four proofs pass AND the success is durably recorded | `ExecutionService.succeed` |
| Four proofs, and `unknown` is not a pass | Health or stability, image digest, configuration preservation, network attachment. `Verification.Passed()` requires all four to read `passed` | `domain.ExecutionVerification` |
| Anonymous volumes are carried forward | A daemon-created volume is converted to an EXPLICIT mount naming the volume that already exists. Recreating naively would give the replacement an empty volume and orphan the data | `implicitVolumeMounts` |
| Daemon-assigned values are stripped | A hostname equal to the container's own short id, plus IP addresses, gateways, MAC addresses, and endpoint ids. Sending them back would pin the replacement to a sandbox that is about to be destroyed | `copyConfigForCreate`, `copyNetworksForCreate` |
| An acquisition is SINGLE USE | A full unique index on `acquisition_id`, not a partial one. One execution per acquisition, ever, with no override parameter | `0010_executions.sql` |
| One recreation per container | A partial unique index over the active states. Two would both stop it and both fight for its name | `0010_executions.sql` |
| Container names are DERIVED, never supplied | Built from a name read from the daemon and an id from the system entropy source, then re-validated against an allowlist. Refused in the PREFLIGHT if they cannot be produced, before anything is stopped | `internal/domain/execution_names.go` |
| Shutdown is bounded and recoverable | The mutation context carries a 10-second grace so a call in flight can finish and its checkpoint land; the pipeline also checks for shutdown at every step boundary, so in the common case it stops at the next one | `executionShutdownGrace`, `shuttingDown` |
| Recovery plans are text | Assembled from a fixed vocabulary and names HarborMaster generated or read from the daemon. Nothing executes one; there is no endpoint and no capability | `internal/domain/recovery.go` |

### Five conclusions the feature refuses to draw

- **A running replacement is not a successful recreation.** Only all four proofs
  together can conclude that, and a proof that was not reached establishes
  nothing.
- **A failure is not a reason to act again.** HarborMaster does not roll back on
  its own. It stops, quarantines the replacement, preserves both containers, and
  records what a person would do. Undoing the recreation afterwards is a
  separate, separately authorised operation a person asks for — see §3h.
- **A recorded state is not a known state after a failed write.** The pipeline
  stops rather than assume.
- **An empty checkpoint does not always mean "nothing changed".** With
  `mutated_at` set it means a stop was issued and never confirmed, and the
  recovery plan says exactly that rather than guessing in either direction.
- **A previous approval does not license a second application.** An acquisition
  is single use; another recreation needs a fresh plan assessed against the
  world as it is now.

## 3g. Manual rollback

Phase 10 gave HarborMaster a way to UNDO §3f, and nothing more. This section
states the limits, because the limits are the design.

**HarborMaster can return ONE container to the state ONE recreation replaced,
ONCE, when a person asks for it.** It cannot roll back automatically, on a
schedule, or across a fleet; cannot restore from a snapshot; cannot roll back to
an image or configuration a caller chooses; and cannot remove anything.

### Nothing a caller supplies reaches the host

The request body has two fields: an execution id and an optional idempotency
key. There is no type in the rollback path with somewhere to put a container id,
a name, an image, or a Docker option — which makes "roll back to an
attacker-selected target" structurally impossible rather than merely checked
for. Both container identities, the production name, and the image identity are
read from HarborMaster's own record of that recreation and re-verified against
the live host before anything moves.

### The pipeline, and where the point of no return is

```
queued → validating │ stoppingReplacement → restoringName → startingOriginal → verifyingOriginal → succeeded
└─ changes nothing ─┘ └──────────────── the host is being changed ─────────────────────────────┘
   freely cancellable                       cancellation refused
```

The transition into `stoppingReplacement` is the MUTATION POINT, and it works
exactly as §3f's does: before it an operator can cancel and nothing has
happened; after it cancellation is refused and the in-process cancel function is
unregistered, because a rollback that has stopped a container an operator
depends on must reach a RECORDED conclusion.

### Two preflights, and the second one is the one that decides

The full preflight runs synchronously inside the request, so an operator gets a
real refusal rather than a queued job that fails a moment later. It runs AGAIN
inside the worker, immediately before the first mutation, because minutes may
have passed and a person may have been moving containers by hand. **Only the
second verdict decides whether anything is touched.**

Both runs ask the same seventeen questions, and any one of them refuses:
the capability is enabled; the execution exists and has settled; its checkpoint
left an arrangement that can be undone; the original was not removed; no
successful rollback of it exists; no rollback or recreation of this container is
in flight; the concurrency limit is free; the daemon answers; the inventory is
fresh; the preserved original is present, under the parked name the record
gives, on the image the record gives; the replacement is present under a name
HarborMaster derived; the production name is free or held by the replacement;
the derived parked name fits; and the original's configuration can be projected
so the result can be proved.

### The checkpoint is what survives a crash

The same design as §3f, for the same reason. `state` says what HarborMaster was
doing; `checkpoint` says what is TRUE OF THE HOST, written after each Docker
mutation succeeds and before the next is attempted. A checkpoint that cannot be
written **stops the pipeline** — the mutation is never repeated, because
repeating a stop, a rename, or a start against a host whose recorded state is
uncertain is how a recoverable situation becomes an unrecoverable one.

Restart recovery reads checkpoints and issues **no Docker call at all**. It
settles each interrupted row from its own checkpoint and attaches a manual
recovery plan.

### The properties

| Property | Mechanism | File |
| --- | --- | --- |
| The rollback surface is FOUR methods | `docker.ContainerRollbacker` pins stop, park, restore, start. Architecture tests pin the count and names, refuse create/remove/exec/image verbs on it, require a full container id on every request, and refuse any package outside the rollback service from naming any of it | `internal/arch/rollback_arch_test.go` |
| **There is no remove method at all** | Deliberately narrower than the spec allowed. The failed replacement is the evidence of why the recreation was backed out, and a capability that could destroy it would eventually be used to. Pinned by `TestTheRollbackInterfaceCannotCreateOrDestroy` | `internal/docker/rollback.go` |
| The rollback service holds NO other mutation capability | It is handed `docker.Runtime` and `docker.ContainerRollbacker` and nothing else, so it cannot create, remove, or capture. Pinned by test | `internal/arch/rollback_arch_test.go` |
| Mutations target a FULL container id | Exactly 64 lowercase hex, validated at the adapter. A prefix would let a shorter id match a container the record does not name | `RollbackStopRequest.Validate` and siblings |
| The renames are ONE-WAY | Park REQUIRES the rollback marker in the new name; restore REFUSES any HarborMaster-derived marker. Nothing here can move a container into a name that says something untrue about how it got there | `containsRollbackMarker`, `containsRecreationMarker` |
| Names are DERIVED, never supplied | `<name>.hm-rolledback-<rollbackId>`, built from a name read from the daemon and an id from the system entropy source, then re-validated. Refused in the PREFLIGHT if it cannot be produced, before anything is stopped | `domain.RollbackParkedName` |
| Preservation is proved against a BEFORE picture | The projection is taken during validation, before anything moves, and compared against the restored container afterwards. What that proves is that the ROLLBACK did not change the container it restored | `rollbackDecision.Baseline` |
| No hasher means UNVERIFIABLE, which refuses | Without the installation key the preservation comparison cannot be made at all, so the rollback is refused rather than run unprovable | `assess`, `RollbackRefusalUnverifiable` |
| Four proofs, and `unknown` is not a pass | Health or stability, image identity, configuration preservation, network attachment. `RollbackVerification.Passed()` requires all four to read `passed` | `domain.RollbackVerification` |
| One successful rollback per recreation, ever | A partial unique index over succeeded rows, plus an in-transaction check on insert. A refused rollback does not consume the chance | `0013_rollbacks.sql`, `RollbackRepository.Create` |
| One rollback per container | A partial unique index over the active states. Two would each rename what the other just renamed | `0013_rollbacks.sql` |
| The preflight excludes ITSELF from the conflict check | The second preflight runs while the rollback is active. Without the exclusion every rollback would refuse for conflicting with itself | `ActiveCount(ctx, excluding)` |
| Retention never prunes a failure that left containers | The same rule the recreation records follow. Removing that row would leave an operator with two containers and nothing explaining them | `RollbackRepository.Prune` |
| Shutdown is bounded and recoverable | The mutation context carries a 10-second grace so a call in flight can finish and its checkpoint land; the pipeline also checks for shutdown at every step boundary | `rollbackShutdownGrace`, `shuttingDown` |
| Recovery plans are text | Assembled from a fixed vocabulary and names HarborMaster generated or read from the daemon. Nothing executes one | `internal/domain/rollback_recovery.go` |
| The outcome is audited from ONE choke point | A deferred call reads the FINAL state back from the store, so every terminal path is audited whether or not its author remembered to, and attributes it to the account recorded on the request | `RollbackService.auditOutcome` |
| The advisory answer reads NO Docker | `Eligible` runs the record half of the preflight and stops. The execution detail endpoint carries it and the UI polls that page; a read able to turn one request into a ping, two inspections, and a listing would be a denial-of-service amplifier pointed at the socket. Pinned by test | `assessRecords`, `TestEligibilityNeverTouchesTheDockerSocket` |

### Four conclusions the feature refuses to draw

- **A restored container is not a successful rollback.** Only all four proofs
  together can conclude that, and a proof that was not reached establishes
  nothing.
- **A failure is not a reason to put things back.** HarborMaster has just
  demonstrated that its model of the host is wrong; correcting it automatically
  at that moment is the unattended mutation this whole design exists to avoid.
  It never guesses which container should serve traffic.
- **An uncertain recreation is not rollable-back.** A recreation whose mutation
  was issued and never confirmed refuses with `checkpointUncertain`: HarborMaster
  does not know where the containers are, and a rollback would be a guess.
- **A check that could not be PERFORMED is not a pass.** An unreadable container
  listing refuses the rollback rather than assuming the production name is free.

## 3g-bis. Automated updates

Phase 11 broke the premise every earlier phase rested on: that nothing changes
the host unless a person asked. It broke it on purpose, because "a safer
replacement for Watchtower" is not a thing HarborMaster can be without it.

What it did NOT break is everything that premise was protecting, and this
section is the argument for why.

### The engine is a caller, not a capability

The automation service holds **no Docker interface**. There is no field on
`service.AutomationOptions` for a runtime, an acquirer, a mutator, a capturer,
or a rollbacker, and none of the automation source names one. Its entire ability
to affect the host is one interface with five methods:

```
AutomationPipeline
    RequestAcquisition(AcquisitionRequest{PlanID, RequestKey, RequestedBy})
    RequestExecution(ExecutionRequest{AcquisitionID, RequestKey, RequestedBy})
    RequestRollback(RollbackRequest{ExecutionID, RequestKey, RequestedBy})
    Acquisition(id)   // read
    Execution(id)     // read
```

Those are the same three request types an HTTP handler builds. Each is submitted
to the same service, which runs the same full preflight against the LIVE host at
the moment it acts. Automation therefore cannot skip the planner, the digest
verification, the platform check, the configuration-preservation comparison, the
health proof, or the checkpoint discipline — because it does not implement any
of them.

Eight tests in `internal/arch/automation_arch_test.go` hold this: the options
struct carries no capability, the source names none, the source performs no
Docker operation, the pipeline is exactly those methods, the evidence interface
has no write, no request type carries a caller-chosen target, the three request
shapes are unchanged, and no package outside the engine and the composition root
may name the pipeline.

### Two loops, and why the split matters

A **decision pass** asks "what should change" and submits acquisitions. A
**follower** asks "what happened to what I already asked for" and advances it:
acquisition succeeded, so request the recreation; recreation failed
verification, so submit the rollback.

They are separate because they have different deadlines, and because the second
must survive a restart. The follower holds NO in-memory state: every tick it
re-reads the decisions of the most recent passes from the database, looks up
what happened to the records they name, and takes the one next step each is
owed. Restarting HarborMaster mid-update loses nothing.

A pass that a restart cut short is marked `interrupted` at the next startup.
That is a bookkeeping gap and never a host in an unknown state — a pass submits
work to services that checkpoint their own.

### Ten checks, in an order that is the security design

`service.DecideAutomation` is a **pure function**: no clock of its own, no
database, no Docker. Every input arrives as a parameter and the result is a
decision record carrying a closed-vocabulary reason. That is what lets the most
consequential judgement HarborMaster makes be tested exhaustively without a
host, and what makes its answer for a given world identical on every pass.

The order is cheapest and most absolute first, so a container that must never be
touched is refused before anything expensive or fallible runs:

1. **Paused** — HarborMaster already decided not to. Checked before the policy
   lookup, so a policy edit cannot clear a pause.
2. **Label opt-out** — the container's owner already decided not to. Read before
   the policy is selected, so the label means the same thing whatever the policy
   set looks like.
3. **Policy selection** — nothing governs it.
4. **Policy disabled.**
5. **A current change plan exists** — the planner has assessed this container.
6. **The plan proposes a change**, and its proposed reference and digest were
   resolved together. The Phase 10.1 defect class is re-checked here as well as
   in the acquisition preflight.
7. **Strategy ceiling** — the change is no larger than the policy permits.
   `unknown` and `prerelease` are permitted by no strategy: an update
   HarborMaster could not size is exactly what a ceiling is for.
8. **Recommendation** — an ALLOWLIST comparison, not an ordering. Only `proceed`
   and `proceedWithCaution` can gate automation; the other three verdicts mean a
   person has to look.
9. **Maintenance window** — evaluated in the window's own IANA timezone, and
   **fails closed**: a zone this host cannot resolve authorises nothing.
10. **Nothing already in flight** for this container.

Only then is the mode consulted, and only `automatic` acts. An unrecognised mode
fails closed rather than falling through into the branch that changes the host.

### One clock reading per pass

A pass takes a single `now` and decides every container against it. Two
containers whose windows differ by a millisecond must not get different answers
because time moved between them.

### The window is the part that is harder than it looks

"Update between 02:00 and 04:00 on weekends" is three problems: whose 02:00,
which day when the window crosses midnight, and what happens on a DST boundary.
`domain.MaintenanceWindow` converts the instant into the window's zone FIRST and
compares minutes-of-day after, which makes a spring-forward gap and an
autumn-back repeat both come out right without either being special-cased. A
window that crosses midnight is two spans, and the weekday that governs the
morning half is the one the window STARTED on.

The runtime image is distroless and carries no system zoneinfo, so
`cmd/harbormaster` imports `time/tzdata`. Without it every named zone would fail
to load and every window would correctly-but-uselessly fail closed.

### Labels may only ever make automation safer

Precedence is `container label → policy → built-in default`, implemented in one
function, `domain.Resolve`. The asymmetry is deliberate and load-bearing:

- `enabled=false` and `pause=true` always win.
- `enabled=true` **cannot enrol** a container no policy selected. A label is set
  by whoever can run `docker run`; if that were enough to opt a container into
  unattended updates, then anyone able to start a container could decide
  HarborMaster should start changing it.
- `strategy` may only NARROW. A container cannot label its way from `patch` to
  `major`, and an unrecognised strategy ranks as the most permissive so it is
  never adopted.
- There is **no label for the mode**. Mode is the setting that decides whether
  the host may be touched at all, and it is a policy's alone.

Unknown and unreadable `io.harbormaster.update.*` keys are reported rather than
dropped: a misspelled safety label that silently does nothing is worse than one
that complains.

### Automatic rollback, and why it always pauses

When a recreation fails verification and the governing policy permits it, the
engine submits the rollback an operator would have submitted — the same
`RollbackRequest{ExecutionID}`, to the same service, which re-verifies both
container identities against the live host before anything moves. There is no
new rollback logic.

A rollback then **always** pauses the container, whatever the failure counters
say and whatever cooldown the policy configures. The change was wrong AND the
host was moved twice to discover that, and an engine that retries such a thing
on a timer is how one bad image becomes a repeated outage.

The rollback decision fails closed in three places: the capability must exist in
the deployment, the recreation must have reached a checkpoint that leaves an
arrangement to undo, and the governing policy must be re-readable and still
permit it. A policy withdrawn between the decision and the failure is not an
authorisation to act.

### Pauses are keyed on the NAME

A container's id changes every time it is recreated, and recreation is exactly
what automation does. A pause keyed on the id would be cleared by the very
action that went wrong. The id is kept for the audit trail only.

A pause with no cooldown is cleared only by an acknowledgement, which records
who made it and resets the container's failure counters — an operator who
investigated and fixed the problem must not be one failure away from the same
pause.

### Every pass is recorded, including the ones that did nothing

The hardest question an operator asks an automation system is "why did you not
update that container", and it is unanswerable unless the reasoning was recorded
at the moment it happened, from the evidence it happened on. So every pass
writes a run row BEFORE it examines anything, and every container it considered
gets a decision row with a closed-vocabulary reason. Both survive the containers
they describe, and both survive the policy being withdrawn.

Retention is therefore not optional: `AUTOMATION_RETENTION_AGE` prunes runs and
their decisions cascade.

### What the schema makes unrepresentable

- A decision naming an execution without an acquisition, or a rollback without
  an execution. The pipeline's ordering, refused in reverse.
- A `wouldUpdate` decision that names an acquisition. Observe and dry run
  cannot have acted, enforced by the database as well as by the engine.
- A second running pass. A single-run rule that holds across processes and
  across a restart.
- A second active pause for one container, by partial unique index.
- A pause acknowledged by nobody.
- A policy whose `minimum_recommendation` is a verdict that asks for human
  review.

### Five conclusions the engine refuses to draw

- **A container the planner has not assessed is not updatable.** No plan is a
  reason, not an error.
- **An update whose size is unknown is not a small update.** No strategy permits
  `unknown` or `prerelease`.
- **A window nobody can evaluate is not open.** An unresolvable timezone refuses.
- **A policy that cannot be re-read does not authorise a second mutation.** The
  rollback check fails closed.
- **A failure that left the host unchanged is not something to roll back.** A
  refused preflight and a failed pull are counted, not undone.

### Authorization is four permissions, not one

`automation:read` is held by every role: automation changes the host without
being asked, and a viewer who cannot see what it decided cannot answer the
question their role exists for.

`automation:run` and `automation:pause` are operator permissions. A manual pass
submits exactly the work the scheduled pass would have submitted a few minutes
later — it changes WHEN, not WHETHER. Pausing is a safety action anyone
operating the host should be able to take immediately, and resuming shares the
permission so the safe state is not the inconvenient one.

`automation:approve` is an operator permission and is marked **privileged**: the
approver releases a change that will stop and replace a running container.

`automation:manage` — writing update policies — is an ADMINISTRATOR permission,
for the reason `policy:manage` is. An update policy is a standing, unattended
grant of `execution:create` over every container a selector reaches, and an
operator able to write one would be granting it to themselves.

### Approval re-derives rather than trusts

A held decision may be minutes or hours old. Before submitting, the engine
re-reads the container's CURRENT change plan and refuses if it no longer matches
the one the decision named. Approving a proposal is approving THAT proposal, and
a registry that republished a tag in the meantime has made it a different one.
A paused container is not approvable either: clearing a pause is a separate,
deliberate act.

The approval endpoint takes a run id and a container NAME, and neither chooses a
target — together they SELECT one of HarborMaster's own held decisions. A name
matching no held decision approves nothing.

## 3h. Identity, authorization, and audit

Phase 9.5 closed the boundary every earlier phase was compensating for. Before
it, HarborMaster's answer to "who may stop a container" was "whoever can reach
the port". This section states how that changed, and what it is that makes the
change hold.

### The shape of the rule

**Every route declares an access policy, and the zero value of that policy is
invalid.**

That sentence is the whole design. `routeAccess` is a type whose zero value is
neither "public" nor "authenticated" but `accessInvalid`, so a route registered
without a policy is refused at runtime and fails
`TestEveryRouteDeclaresAnAccessPolicy` at build time. "I forgot" and "I meant
public" have to look different, and with a zero value that means public they do
not.

`internal/api/routes.go` lists every route exactly once with its policy, and
nothing else in the package registers a handler. It is deliberately long and
deliberately not abbreviated with loops: a reader auditing "who can stop a
container" should find the answer by searching for the permission, and a
grouping construct is exactly what hides one entry inside another's rule.

Four routes are public, each with a stated reason:

| Route | Reason |
| --- | --- |
| `GET /health` | A container runtime's HEALTHCHECK cannot hold a session. The body is REDUCED to the overall status for an anonymous caller; the full report names the database path, the journal mode, and the Docker API version |
| `GET /version` | Deployment identification by tooling that holds no credential |
| `POST /auth/login` | It is how a session is obtained |
| `GET /auth/bootstrap` | A client must choose between sign-in and bootstrap before it holds anything. Returns one boolean |

`POST /auth/bootstrap` carries a policy of its own: reachable only while the
installation is unclaimed, and `404` afterwards — for that installation the
endpoint genuinely no longer exists, and "forbidden" would confirm there is a
flow to race.

### Authorization happens in one place

Handlers never ask about roles. They receive an already-authorized request and
an identity, and the identity is there for AUDIT ATTRIBUTION rather than for a
second decision. `TestNoHandlerChecksARoleDirectly` fails the build if any
handler file names a role constant, and `TestUserHandlersDoNotCompareRoles`
narrows the one exemption — the account handlers may PARSE a role out of a
request body, but they may not branch on one.

The permission model itself lives in `internal/domain/permission.go`. Three
roles, fixed permission sets, and one property that matters more than the
contents: **an unrecognised role holds nothing at all.** A role read back from a
corrupt row, or written by a future migration this build does not know, grants
no permission rather than defaulting to the least privileged one — a corrupt row
must not become a silent grant.

Policy administration sits at administrator level deliberately. A policy is what
BLOCKS an acquisition or a recreation, so an operator able to edit one could
remove the gate standing in their way.

### Sessions

Opaque, server-side, and stored as a keyed digest.

- **The token never leaves the cookie.** It is minted from 32 bytes of system
  entropy, set `HttpOnly` and `SameSite`, and appears in no response body, no
  URL, and no log line. A script on the page cannot read it.
- **The database holds only `HMAC(installation key, purpose, token)`**, so a
  stolen database yields the ability to verify a token somebody already holds
  and nothing else.
- **`__Host-` prefixing over HTTPS.** The prefix is a browser-enforced
  guarantee, not a convention: a cookie carrying it must be Secure, must have
  `Path=/`, and must not set a Domain — which stops a sibling subdomain, or a
  network attacker who can spoof one over plain HTTP, from overwriting the
  session cookie.
- **Two expiries.** Idle bounds an abandoned session; absolute bounds a stolen
  one being kept deliberately warm. Both are enforced in the lookup query, so an
  expired row cannot be resurrected by a bug elsewhere.
- **The user is re-read on every request**, not trusted from the session's
  snapshot. That is what makes a role change, a disablement, and a password
  change take effect immediately rather than at the next sign-in.
- **A password change is checked twice.** The session rows are revoked
  explicitly, AND the lookup requires a session to be newer than the account's
  `password_changed_at`. The timestamp is the belt that survives a crash between
  the revocation and the write.

### CSRF, derived rather than stored

The CSRF token is `HMAC(installation key, csrf purpose, raw session token)`.

The server has the raw token on every request — it arrived in the cookie — so
the expected value is recomputed rather than looked up. Nothing is at rest for a
database thief to read, the token rotates with the session without a rotation
mechanism, and the comparison is constant-time.

It travels in a CUSTOM header. A cross-origin form or a "simple" fetch cannot
set one without triggering a preflight, and no CORS headers are served, so the
preflight fails. That property is what makes the header meaningful over and
above the token it carries.

Only login and bootstrap are exempt, because they have no session to derive from,
and `TestCSRFExemptionsAreOnlyTheSessionlessRoutes` fails the build if a third
route claims the exemption.

### Passwords

Argon2id, through `golang.org/x/crypto`. No custom cryptography anywhere.

The parameters are stored ALONGSIDE each hash rather than read from
configuration at verification time, which is what makes raising the cost safe: a
login below the current policy verifies with the parameters it was made with and
is transparently re-hashed. They are bounds-checked at construction and again
per credential, because they drive an allocation — a row claiming 64 GiB would
otherwise be an out-of-memory kill triggered by a login attempt.

An unknown algorithm, a corrupt salt, or out-of-range parameters produce
`ErrCredentialUnusable` rather than any comparison at all. Falling back to a
default would mean a corrupted row could be made to accept a chosen password.

**Enumeration resistance is paid for, not assumed.** An unknown username is
verified against a DECOY credential built at first use under the current
parameters, so the response time of a real account and an imaginary one match. A
disabled account is refused AFTER the password check, so its timing and its
error are identical to a wrong password. The per-address throttle is applied
BEFORE the username lookup, so query timing is not a side channel either.

Failures apply an exponential per-account backoff to a bounded ceiling rather
than a hard lockout: a lockout lets anyone who knows a username deny that
account service, which turns an authentication control into a denial-of-service
tool.

### Claiming an installation

A fresh installation has no accounts and no default password. It prints a
one-time bootstrap token at startup; `POST /auth/bootstrap` exchanges it for the
first administrator.

The token moves the requirement from "be first to the port" to "can read the
server's log or its data directory", which is the same bar the rest of the
deployment already assumes. It is stored as a keyed digest, compared in constant
time, expires, and is re-minted on every restart of an unclaimed installation —
so an operator who lost it restarts, and an attacker who captured an old log
does not benefit.

`harbormaster admin bootstrap` does the same from the host without a token,
because filesystem access to the database is a stronger proof than any token.
That path lives in `service.LocalAdmin`, which the API layer does not depend on
and `TestLocalAdminIsNotReachableFromTheAPI` forbids it from naming. "Never
exposed over HTTP" is a structural fact rather than a promise: a handler cannot
call what its dependency does not declare.

### Long-lived responses are re-authorized

Every route re-reads the account on each request. A STREAM makes one request and
then runs for as long as the client holds it, so authorizing it only at connect
would make it the one place where revoking a session does not stop the flow of
data.

`GET /events/stream` therefore re-checks the session AND the `event:read`
permission on every heartbeat, and ends with a `closed` frame the moment either
stops holding. The permission is re-checked as well as the session because a
demotion leaves a VALID session that no longer holds the permission.

It fails closed: a lookup that errors ends the stream. A stream is a standing
grant, and a grant that cannot be reconfirmed is one that should stop.

The residual window is one heartbeat, recorded as R40.

### Audit

Before this phase every feature recorded WHAT happened and none recorded WHO.

`auditWrite` is the one line that closes that gap, called at each write's success
point rather than from a wrapper, because only the handler knows what the write
acted on. `TestEveryWriteRouteIsAudited` walks the route table, resolves each
state-changing handler to its source method, and fails if the method's body does
not call it — or if the route is not listed as audited inside its service, with
the reason stated.

A record is who, what, to what, from where, and whether it worked. It is **not a
request log**: there is no body, no header, no environment value, and no
credential anywhere in the schema, and no column exists that could hold one. An
authorization denial records the PERMISSION that was refused rather than the
path, because a permission is a closed vocabulary and a path is
request-derived text that reaches a page an administrator reads.

Two properties are enforced by test rather than by care:
`TestCredentialMaterialIsConfinedToStoreAndService` fails the build if the
verifier type reaches the HTTP layer, and `TestNoAuditRowEverContainsASecret`
runs a real bootstrap, login, account creation, and password change and then
sweeps every recorded row for every secret that was in scope.

An audit write never fails an action. If it could, filling the disk would become
a way to disable HarborMaster, and a failed write during logout would leave the
operator logged in. The failure is logged at ERROR and the action proceeds.

#### A request and an outcome are separate facts

The two operations that change the Docker host record BOTH.

A request can be refused by the second preflight, cancelled before the first
mutation, expire in the queue, or fail partway and leave two containers behind.
An audit log holding only `execution.requested` cannot answer the question an
administrator actually asks after an incident -- *was this container replaced,
and by whom* -- so it also records `execution.completed` or `execution.failed`,
and the failure reason says whether the host was left changed.

The completions are what `Privileged()` counts and what the WARN log line
reports. Counting requests would over-report host changes, and a security
counter that over-reports is one an administrator learns to ignore.

The outcome is written by a WORKER, minutes after the request, on a goroutine
with no HTTP request and no session. So the requesting account is stored ON the
record -- see migration 0012 -- because that is the only thing the worker has.
Two fields, user id and username: no role, no session, no address, because those
belong to the request and are already audited with it. A second, staler copy
that can disagree with the first is worse than none.

It reaches the log from ONE place: a deferred call at the end of the pipeline
that reads the final state back from the store. The pipeline has a dozen
terminal paths, and an audit call on each would be a list a future path forgets
to join.

### The source address

`X-Forwarded-For` and `Forwarded` are **ignored entirely** unless a
trusted-proxy CIDR is configured and the transport peer is inside it. A
forwarding header is attacker-controlled text; believing it unconditionally
would let anyone spoof the source in the audit log and evade the per-address
throttle by rotating it.

When a proxy is trusted the chain is walked right to left and stops at the first
untrusted hop, so entries an attacker prepended move nothing.

## 4. Trust boundaries in code

| Boundary | Enforcement point |
| --- | --- |
| Anonymous → authenticated | `internal/api/auth_middleware.go` — one `enforce`, reached by every route through `guard`; there is no code path that registers a bare handler |
| Authenticated → authorized | The same `enforce`, against the permission the route declared in `internal/api/routes.go` |
| Credential material | `internal/store` and `internal/service` only; an architecture test fails the build if the verifier type appears elsewhere |
| Console recovery → HTTP | `internal/service/auth_local.go` is not a dependency of `internal/api`, enforced by test |
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
| Write endpoints | Token bucket, per process; `PATCH /drift/{id}` included |
| Drift evaluation queue | `DRIFT_MAX_PENDING_EVALUATIONS`; overflow escalates to a sweep rather than growing |
| Drift records per container | `DRIFT_MAX_RECORDS_PER_CONTAINER`; past it the evaluation is *incomplete*, never silently truncated |
| Drift evaluation wall time | `DRIFT_EVALUATION_TIMEOUT` per container; the sweep derives its own bound from it |
| Drift filter values | 32 per repeatable parameter, so one request cannot build an unbounded `IN` clause |
| Drift history | Resolved records pruned by age; **open records never are** |
| Snapshot growth | `(container_id, checksum)` unique index; retention by count and age |
| Database writer | `MaxOpenConns(1)`; prune in bounded batches |
| Cross-process lock wait | `DB_BUSY_TIMEOUT`, 100ms–5m, no "wait forever" |
| Startup integrity check | `DB_INTEGRITY_TIMEOUT`; past it the result is *incomplete*, not *damaged* |
| Integrity problem reporting | 20 lines, enforced in the pragma **and** in the reader |
| Event stream replay on reconnect | 1 hour, so a long outage cannot request the daemon's whole ring |
| Shutdown, HTTP and background | One `SHUTDOWN_TIMEOUT` budget, bounded at every step |
| Detached background work | `GraceContext`: bounded grace, then a hard maximum |
| Backup verification | 10 minutes, so a backup command cannot hang |

**Refused, not queued.** A queue converts a load spike into unbounded memory and
latency. Every ceiling here returns `429` or `503` with `Retry-After`.

## 6. Concurrency and shutdown

Every goroutine has a bounded lifetime tied to a context. Every `time.NewTimer`
has a `defer Stop()`. Every channel is created with an explicit capacity.

Shutdown order is deliberate and documented in `cmd/harbormaster/main.go`: the
HTTP server drains first, then background services, then the Docker client and
the database. The database must close *after* the background services or a final
event flush would write to a closed handle.

**Every wait in that sequence is bounded**, by one budget —
`SHUTDOWN_TIMEOUT` — shared between the HTTP drain and the background drain,
because the orchestrator counts down a single deadline for the whole process.

`service.GraceContext` is the primitive. Work that must not be interrupted
mid-transaction survives cancellation for a bounded grace period and is then
cancelled. Detached-*and*-unbounded is the anti-pattern it replaced: a
reconciliation detached with `context.WithoutCancel` and a fifteen-minute bound
meant `SIGTERM` during a large sweep held the process open until the runtime
sent `SIGKILL` — which is the abrupt, arbitrary interruption the detachment was
trying to avoid.

Where a bound is reached, HarborMaster logs what it is abandoning at error
level and closes the database anyway. An abandoned SQLite writer's transaction
is rolled back; an unbounded hang is not recoverable.

`go test -race` runs in CI on every push. It is not optional: the event engine
owns several long-lived goroutines and a bounded queue, and the shutdown
primitives own a watchdog goroutine per detached task, with a leak test to
match.

## 6a. Storage reliability

Persistence is a security boundary as well as a durability one: a database that
silently degrades is a database whose redaction and constraint guarantees can no
longer be relied on.

| Property | Mechanism | Where |
| --- | --- | --- |
| Damage detected before it is written over | `quick_check` at open, **before** migrating | `internal/store/integrity.go` |
| Damage fails closed | Refuse the open; only corruption is fatal | `internal/store/failure.go` |
| Uncertainty does not fail closed | An *incomplete* check is distinguished from *damage* and does not refuse | `IntegrityReport.Damaged` |
| Durability profile is confirmed, not assumed | Journal mode read back; WAL fallback warned or refused | `store.OpenWithOptions` |
| An old binary cannot write a new schema | Applied migrations validated against the embedded set | `internal/store/migrate.go` |
| An edited migration cannot be skipped | SHA-256 per migration, recorded at apply | same |
| Failures name the right remedy | SQLite **result codes** classified, never message text | `internal/store/failure.go` |
| Backups are consistent and verified | `VACUUM INTO` in a read transaction, then a full check | `internal/store/backup.go` |
| Backups are not world readable | Created `0600`, directory `0750` | same |
| Diagnostics never write | `OpenReadOnly` — `mode=ro`, no migration | `internal/store/store.go` |

**Diagnostics are deliberately not an endpoint.** `harbormaster diagnose`
reports filesystem paths, free space, journal mode, page counts, schema history,
and daemon reachability — reconnaissance for an attacker, necessity for an
operator. The API is unauthenticated in this phase, so the surface is a command
requiring shell access instead.
`TestDiagnosticsAreNotReachableOverHTTP` in `internal/arch` fails the build if
`internal/api` imports the diagnostics package.

The report itself carries only counts, closed-vocabulary states, timestamps,
sizes, modes, and the operator's own configured paths. It never renders a row's
contents, and a test sweeps the rendered output for a seeded value with a
positive control.

## 7. Frontend

- **Nothing about the estate renders before a session exists.** The pre-session
  states return before the application shell is constructed, so no navigation,
  no connectivity indicator, and no data hook is mounted for a visitor who has
  not signed in. Hiding them with CSS, or mounting them and letting each request
  answer 401, would leak both structure and traffic.
- **No token is stored where a script can find it.** The session token is in an
  HttpOnly cookie the app cannot read. The CSRF token is held in a module
  variable in `api/client.ts` — never `localStorage`, `sessionStorage`, a URL,
  or a component's state — so it dies with the page, which is the same lifetime
  as its usefulness.
- **Hiding a control is not authorization.** Role-aware navigation exists so the
  app does not offer buttons that will fail, which is a usability property. The
  server refuses regardless, and the two are checked against each other by
  `TestEveryPermissionRouteRefusesARoleThatLacksIt` on the backend.
- **A 401 from anywhere ends the session once.** A single listener in the
  session provider handles it, so a background poll on a page nobody is looking
  at produces one transition to the sign-in page rather than an error on every
  view that happened to be fetching.
- **The sign-in page says exactly what the server says.** One message for every
  credential failure; a friendlier "no such user" here would undo the
  enumeration resistance the backend pays an Argon2id evaluation to provide.
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
| A digest for a tag nobody resolved | Phase 10.1. A newer tag is proposed only when its OWN manifest digest was resolved in the same registry lookup. `domain.ProposedTarget` cannot be constructed from a reference and a digest resolved for a different reference, and `ChangePlan.ValidTarget` refuses an unpinnable pair before it reaches a row |
| Any Docker mutation beyond a digest-pinned pull, a single-container recreate, and a single-execution rollback | Phase 8 added one image mutation, Phase 9 added five container lifecycle methods, Phase 10 added four rollback methods. All three surfaces are pinned by test, held by separate services, and off by default. Everything else remains absent |
| **Automatic** rollback | **Explicitly not built.** A failed recreation quarantines the replacement, preserves both containers, and records manual steps. An automatic undo is another unattended mutation performed at exactly the moment HarborMaster has demonstrated its model of the host is wrong. Phase 10 added a rollback a PERSON asks for, one execution at a time; see §3g |
| Scheduled or fleet rollback | Same reasoning. No timer creates rollback work, and `ROLLBACK_MAX_CONCURRENT` defaults to one |
| Rollback to an arbitrary image, configuration, or snapshot | A rollback undoes ONE recorded recreation and derives every identity from that record. Restoring a container from a snapshot needs evidence a rollback does not have and is a later phase |
| Removing anything during a rollback | The rollback capability has four methods and none of them removes. The failed replacement is the evidence of why the recreation was backed out |
| Restore | Later phase; snapshots prepare for it |
| Automatic, scheduled, or fleet updates | Every recreation is requested by an operator and acts on ONE container. No timer creates work, and `EXECUTION_MAX_CONCURRENT` is capped at four with a default of one |
| Retrying a recreation or a rollback | HarborMaster stops at the first failure. A retried recreation is a new plan and a new acquisition; a retried rollback is a new request an operator makes deliberately |
| Image or volume deletion | Still absent. `RemoveRequest` can remove a stopped CONTAINER and has no field for volumes or force |
| Arbitrary command execution | Never. Not a feature, a category of vulnerability |
| Template or plugin execution | Same |
| User-controlled file access | Same |
| Host filesystem inspection | Interface exists, answers `unverifiable` |
| Expression languages in the API | An interpreter on an unauthenticated endpoint is a DoS and injection surface. Policies are typed structs from a closed catalogue for exactly this reason |
| Policy enforcement | HarborMaster checks configuration against a rule; changing a container to satisfy one would be a Docker mutation |
| Permanent policy deletion | Violations reference their policy and the history must survive a withdrawal. `DELETE` archives |
| Policy scoping / selectors | Every enabled policy applies to the whole estate. A selector language would be a second thing an unauthenticated caller could make expensive |
| TLS termination | A reverse proxy's job |
| Registry credentials | Every lookup is anonymous. A private repository reports `unauthorized` rather than becoming a reason to store somebody's registry password |
| Insecure / plaintext registries | HTTPS with verification, always. A local registry is reported as unsupported and never contacted |
| Proxy support for registry requests | A proxy is an internal address, which the dial guard exists to refuse |
| Image push, delete, or prune | Still absent. Acquisition ADDS to the local store and can remove nothing |
| Applying an acquired image | Downloading and running are different capabilities. A container keeps running the image it was created from; recreating it is a later phase |
| Automatic or scheduled pulls | Every acquisition is requested by a person. A timer that pulled would be a capability nobody asked for at the moment it acted |
| Tag-only pulls | A tag can move between approval and transfer. There is no branch in the adapter that produces one |
| A pull target in the API | The request carries a plan id. An endpoint that accepted a registry and repository would be a general-purpose downloader wearing HarborMaster's authority |
