# Secure Coding Standards

Rules for contributing to HarborMaster. Each one exists because breaking it
would create a specific vulnerability in *this* codebase, not because it appears
on a generic checklist.

Where a rule is enforced by a test or a linter, that is named. A rule nothing
enforces is a rule that will eventually be broken.

---

## 1. Docker

**1.1 Never add a mutating method to `docker.Runtime`.**
The interface is seven read-only methods. Adding an eighth requires editing
`internal/arch/arch_test.go` twice, which is deliberate friction.

**1.2 Never import the Docker SDK outside `internal/docker`.**
Enforced by `TestMobySDKIsConfinedToTheAdapter`. Convert to a domain type at the
boundary instead.

**1.3 Never return a raw Docker error to a caller.**
Docker errors embed socket paths and daemon internals. Log the real error; return
`docker.SanitizeError(err)`.

**1.4 Never let a request handler drive Docker directly.**
Handlers read HarborMaster's inventory. A read endpoint that can generate
privileged socket traffic is a denial-of-service amplifier: one HTTP request
becomes a full host sweep, repeatable at will.

**1.5 Never widen a mutation interface without editing its test.**
There are exactly three: `docker.ImageAcquirer` (one method),
`docker.ContainerMutator` (five), and `docker.ContainerRollbacker` (four). Each
is pinned by an exact-set test, a verb test, and a source-level test naming the
packages allowed to reference it. Adding a method means editing tests whose
entire subject is that limit — which is what makes the change visible in review.

**Do not merge the rollback interface into the mutator.** They are separate
because capability is granted by what a constructor is handed: the rollback
service cannot create or remove a container, and the execution service cannot
perform a rollback, and both facts are true because neither holds the other's
interface. A single wide interface would make both statements false in one
edit.

**1.6 Never take a Docker SDK option struct as a parameter.**
Every mutation method takes a HarborMaster-owned request struct built from
validated components. An exported SDK options field is a field somebody can
eventually fill from a request body.

**1.7 Always target a mutation by full container id.**
Sixty-four lowercase hex, validated at the adapter. A short id or a name can
resolve to a different container than the one the preflight checked, and the
whole safety model rests on those being the same container.

**1.7a Never give the automation engine a Docker capability.**
There is nowhere on `service.AutomationOptions` to put one, no automation source
names one, and eight architecture tests hold it. The engine's entire ability to
affect the host is `AutomationPipeline` — three request submissions and two
record reads — and every one of those goes to a service that owns its capability
and runs its own preflight.

If a change needs the engine to "just check whether the daemon is reachable", it
needs a service to answer that question, not an interface. A capability field
here is how the whole safety argument stops being true.

**1.7b Never add a field to `AcquisitionRequest`, `ExecutionRequest`, or
`RollbackRequest`.**
Each is exactly three fields: an identifier HarborMaster generated, an
idempotency key, and the requester. `TestAutomationSubmitsOnlyIdentifiersItGenerated`
pins the count and the names. A fourth field on a request that changes the host
is a fourth thing a caller can influence, and automation is now one of the
callers.

**1.8 Never add an exported field or method to `docker.CapturedConfig`.**
It holds a container's real environment and log-driver credentials in unexported
fields, and that is the entire secret boundary for recreation. If a caller needs
to know something about a captured configuration, add it to the value-free
projection `Summary` returns.

**1.9 A failed checkpoint stops the pipeline; it never triggers a retry.**
Repeating a stop, a rename, or a remove against a host whose recorded state is
uncertain is how a recoverable situation becomes an unrecoverable one. Record
the uncertainty and hand it to a person.

**1.10 Never remove the original before the replacement is proved AND the
success is durable.**
All four verifications must read `passed` — an `unknown` is not a pass — and the
success record must have been written. If either is missing, preserve both
containers and fail closed.

**1.10a Never pair a reference with a digest resolved for another one.**
Build the pair with `domain.NewProposedTarget`, which takes the reference the
registry was actually asked about and refuses a mismatch. Two loose strings is
how a change plan came to read `busybox:1.36 -> busybox:1.38` while carrying
1.36's digest: acquisition is digest-pinned, so it would have pulled the OLD
image, verified it against the digest the plan called approved, and recorded an
update that never happened.

**1.10b Resolve the local digest from RepoDigests, matched to the repository.**
A tag-referenced container carries no digest in its reference. The daemon's
RepoDigests do, but one image can carry entries for several repositories, so
only an exact registry-and-repository match is eligible and anything ambiguous
reports unknown. Never take "the first one". See `domain.SelectRepoDigest`.

**1.11 Never add a remove capability to the rollback interface.**
`docker.ContainerRollbacker` has four methods and none of them destroys
anything. The replacement a rollback displaces is the evidence of why the
recreation was backed out, and a capability that could delete it would
eventually be used to. Pinned by
`TestTheRollbackInterfaceCannotCreateOrDestroy`.

**1.12 Keep the rollback renames one-way.**
`ParkReplacement` REQUIRES the rollback marker in the name it is given;
`RestoreOriginalName` REFUSES any HarborMaster-derived marker. Between them
those two rules mean nothing in the rollback path can move a container into a
name that says something untrue about how it got there. Relaxing either turns
two narrow operations into a general rename.

**1.13 Derive every rollback identity from HarborMaster's own record.**
The request carries an execution id and an optional idempotency key, and there
is no type in the rollback path with anywhere to put a container id, a name, or
an image. That is what makes "roll back to an attacker-selected target"
structurally impossible rather than merely checked for. Adding a field would
remove the property, not merely add an input to validate.

---

## 2. Secrets

**2.1 Never persist a secret value, in any encoding.**
Not encrypted, not base64, not "temporarily". Store a keyed digest.

**2.2 Never add a serialisable field that could hold one.**
Digest, algorithm, and key-ID fields are `json:"-"`. Keep them that way — the tag
is what makes accidental serialisation impossible rather than merely unlikely.

**2.3 Never log a value that came from container configuration.**
Not environment values, not log-driver options, not raw inspection payloads. Log
names, counts, and IDs.

**2.4 When classification is unknown, treat it as sensitive.**
The cost of being wrong that way is a value an operator cannot see. The cost of
the other way is a leaked credential, and only one of those is recoverable.

**2.5 Never echo request input in an error message.**
`invalidParam("trigger", "a known snapshot trigger")` — not the offending value.
An error message is not a place to reflect attacker-controlled input.

**2.6 A password, a session token, and a CSRF token are secrets under every
rule above.** No column, no struct field, no log attribute, no audit reason, and
no API response may hold one. Passwords are verified and discarded; session
tokens are stored as keyed digests; the CSRF token is derived and stored nowhere
at all.

**2.7 Never write custom cryptography.** Argon2id comes from
`golang.org/x/crypto`; HMAC-SHA256 from the standard library. Compare every
secret with `subtle.ConstantTimeCompare`: a byte-by-byte comparison tells an
attacker how far they got, which recovers the value one character at a time.

---

## 3. SQL

**3.1 Bind every value. No exceptions.**
`fmt.Sprintf` into a query is grounds for rejecting a pull request.

**3.2 Identifiers come from an allowlist that maps to compile-time constants.**
Caller input selects *from* a fixed set; it never contributes *to* the SQL text.

```go
var snapshotSortFields = map[string]string{
    "createdAt": "created_at",   // caller says "createdAt"
    "id":        "id",           // the column name is ours
}
```

**3.3 Every write that touches more than one table runs in a transaction.**
Derived rows must never be able to drift from the document they came from.

**3.4 Bound every query.** `LIMIT` on anything that could return many rows;
batches for anything that deletes many.

**3.5 Check `rows.Err()`.** Enforced by `rowserrcheck`. A partial read that looks
like an empty result is worse than an error.

---

## 4. HTTP

**4.1 Validate every enumerated parameter against a closed vocabulary.**

**4.2 Reject, do not clamp.** Silently serving page 1 to a client that asked for
page −3 hides a bug in the caller.

**4.3 Strict JSON on every write.** `DisallowUnknownFields`, exactly one object,
UTF-8 validated on the *raw bytes* — Go's decoder silently replaces invalid
sequences with U+FFFD, so post-decode validation cannot detect them.

**4.4 Bound every dimension** a caller controls: body size, page size,
concurrency, wall time, output size.

**4.5 Refuse, do not queue.** Return `429`/`503` with `Retry-After`.

**4.6 Never trust Fetch Metadata or `Origin` as a primary control.**
Both are absent in older browsers and trivially omitted by a non-browser client.
Use them as an extra layer; make sure the real controls hold without them, and
test that they do.

**4.7 Errors carry a stable code and a generic message.** No stack traces, no
wrapped internal errors, no filesystem paths.

**4.8 Never echo a client-supplied request ID.**

---

## 4a. Authentication and authorization

**4a.1 Every route declares an access policy, and the zero value is invalid.**
`public()`, `bootstrapOnly()`, `authenticated()`, `duringPasswordChange()`, or
`requires(permission)`. A route registered without one is refused at runtime and
fails `TestEveryRouteDeclaresAnAccessPolicy`. Do not add a code path that
registers a bare handler.

**4a.2 Adding a public route is a two-line change, and the second line is a
test.** `TestThePublicSurfaceIsExactlyTheDocumentedRoutes` pins the
unauthenticated surface. If you are editing it, say why in the route table too.

**4a.3 Never check a role in a handler.** Authorization is decided once, by the
middleware, from the route table. A handler receives an already-authorized
request; the identity it can read is for AUDIT ATTRIBUTION.
`TestNoHandlerChecksARoleDirectly` fails the build on a role constant in a
handler file.

**4a.4 Use typed permission constants.** `domain.PermExecutionCreate`, never the
string. A typo in a string is a permission nobody holds, which fails open in the
worst possible way — the check simply never matches.

**4a.5 An unrecognised role holds nothing.** Never default an unknown role to
the least privileged one: a corrupt row must not become a silent grant.

**4a.6 Every credential failure looks the same, and takes the same time.** One
error, one status, one message, for an unknown username, a wrong password, and a
disabled account. Hash against the decoy on the paths that have nothing to hash,
or the endpoint becomes an account directory.

**4a.7 Back off; do not lock out.** A hard lockout lets anyone who knows a
username deny that account service.

**4a.8 Bound anything that reaches a hash.** Password length before hashing, and
Argon2id parameters both at construction and per stored credential — they drive
an allocation, and a corrupt row must not be able to request a gigabyte.

**4a.9 Every state-changing route requires the CSRF header.** The only
exemptions are routes with no session to derive a token from, and there are
exactly two.

**4a.10 Never trust a forwarding header.** `X-Forwarded-For` and `Forwarded` are
ignored unless a trusted-proxy CIDR is configured and the peer is inside it.
Walk the chain right to left and stop at the first untrusted hop.

**4a.11 Hiding a control is not authorization.** A UI may omit a button the role
cannot use. The server refuses regardless, and the test that proves it lives on
the server.

**4a.12 Every permission guards a route, and every guarded permission is
reachable from a role.** A permission that grants nothing is a capability
`GET /roles` advertises and the API does not have; a route whose permission no
role holds answers 403 to everyone including the administrator. Two tests fail
the build on either.

---

## 4b. Audit

**4b.1 Every state-changing route records who did it.** Call
`s.auditWrite(...)` at the write's success point, or list the route in
`auditedElsewhere` with the service method that records it.
`TestEveryWriteRouteIsAudited` fails the build otherwise.

**4b.2 An audit record is the SHAPE of an action, never its content.** No
request body, no header, no cookie, no environment value, no credential. If you
are adding a field, ask what an attacker would put in it.

**4b.3 Record a closed vocabulary, not request-derived text.** An authorization
denial records the permission that was refused, never the path.

**4b.4 An audit write must never fail an action.** If it could, filling the disk
would become a way to disable HarborMaster. Log at ERROR and proceed.

**4b.5 Bound every audit field at one choke point.** `prepareAuditEvent` is that
point. Do not bound at the call site; there are thirty of them and they will
drift.

**4b.6 Audit the OUTCOME of anything that changes the host, not just the
request.** A request is an intention: it can be refused, cancelled, expire, or
fail partway. Record the completion too, attributed to the account that asked,
and say in the reason whether the host was left changed.

**4b.7 Attribute asynchronous work from the RECORD, not from the request.** A
worker that finishes a job minutes later has no request and no session. Store
the requester on the record at request time -- user id and username only.

**4b.8 Record an outcome from ONE place.** A deferred read-back of the final
state at the end of the pipeline, not an audit call on each terminal path. A
list of paths is a list a future path forgets to join.

---

## 4c. Long-lived responses

**4c.1 A stream is re-authorized, not authorized once.** Every ordinary route
re-reads the account per request; a stream makes one request and runs for days.
Re-check the session AND the permission on the heartbeat, and end the response
when either stops holding.

**4c.2 Fail closed.** A check that cannot be performed ends the stream. A
standing grant that cannot be reconfirmed is one that should stop.

**4c.3 Say why you closed it.** A client that is not told stops reconnecting
into a 401 loop only by accident.

---

## 5. Concurrency

**5.1 Every goroutine ends on a context.** No fire-and-forget.

**5.2 Every `time.NewTimer`/`NewTicker` gets `defer Stop()`.**

**5.3 Every channel is created with a deliberate capacity**, and a comment when
that capacity is a policy decision.

**5.4 Never hold a mutex across a Docker call or a database write.**

**5.5 A full queue drops and records; it never blocks the producer.**
A blocked reader on the Docker event stream stalls the whole stream.

**5.6 New concurrent code ships with a `-race` test.**

**5.7 Detached is not a substitute for bounded.** Work that must survive
cancellation to finish a transaction uses `service.GraceContext`, which gives it
a bounded grace period and then cancels it. `context.WithoutCancel` with a long
timeout is the anti-pattern: it converts "do not interrupt this transaction"
into "hold the process open until the runtime sends SIGKILL", which interrupts
it anyway, at a worse moment.

**5.8 Every shutdown wait has a bound and says what it abandoned.**
`sync.WaitGroup.Wait` at shutdown is a hang waiting for one wedged goroutine.
Use `service.WaitGroupTimeout` and log at error level when the bound is reached.

---

## 6. Frontend

**6.1 No `dangerouslySetInnerHTML`.** If you think you need it, you need a
different component.

**6.2 No URL built from backend data** without an explicit scheme allowlist.

**6.3 Never render a value the API should not have sent.**
If a secret appears in a payload, that is a backend bug — but the component must
not display it either.

**6.4 No inline styles or inline scripts.** The CSP forbids both, and
`TestContentSecurityPolicyForbidsInlineAndRemoteContent` fails if the policy is
reopened. If you genuinely need one, use a nonce or a hash.

**6.5 Handle all four resource states** — loading, ready, disconnected, error.
"Backend unreachable" and "backend rejected the request" have different remedies.

**6.6 A malformed payload must not crash a view.** Optional-chain into API data;
render the empty state instead of throwing.

---

## 7. Dependencies

**7.1 Justify every new direct dependency in the pull request.** What it does,
why the standard library will not, who maintains it, what it pulls in.

**7.2 No copyleft.** HarborMaster is MIT; the dependency review workflow denies
GPL, AGPL, LGPL, and SSPL.

**7.3 SHA-pin every GitHub Action.** A tag is mutable.

**7.4 Run `go mod tidy` before pushing.** CI fails on a dirty `go.mod`.

---

## 8. Configuration

**8.1 Defaults are safe.** Loopback binding, masking on, limits enabled. A
misconfiguration should reduce functionality, not protection.

**8.2 Validate configuration at startup, even for disabled features.**
An error that only surfaces the day someone enables a feature is worse than one
caught at boot.

**8.3 Fail closed.** If a security property cannot be established, refuse to
start. A missing HMAC key that was previously in use aborts startup rather than
regenerating — a fresh key would make every historical digest compare unequal,
which reads to an operator as "every secret changed at once".

**8.4 Never log configuration values.** `Config.String()` reports which knobs
exist, never what they hold. Do not add interpolation to it.

---

## 9. Files

**9.1 Refuse symlinks when opening anything security-relevant.**
`O_NOFOLLOW` on Unix. A symlinked key file is a redirection primitive.

**9.2 Inspect the descriptor, not the path.** `f.Stat()` after opening, never
`os.Stat(path)` then open — that leaves a TOCTOU window.

**9.3 Bound every read.** `io.LimitReader`, even for a file you expect to be
small. A mistyped path pointing at `/dev/zero` should not exhaust memory.

**9.4 Write atomically.** Temp file with `O_EXCL`, `fsync`, rename, `fsync` the
directory. A crash mid-write must not leave a truncated key.

**9.5 Explicit permissions.** `0600` for secrets and for anything carrying
database contents (a backup is a complete copy of the database); `0750` for data
directories. Set the mode explicitly after creating the file — the process
umask decides otherwise, and on a permissive host that is `0644`.

**9.6 Never silently overwrite an operator's file.** A command that writes to a
path the operator named refuses an existing destination rather than replacing
it. Overwriting the previous backup with a bad one is how a single failure
destroys the recovery path.

**9.7 Remove a partial artifact on the failure path.** A half-written file that
looks like a backup is worse than no file, because it will be trusted. The
exception is a file that FAILED VERIFICATION rather than failed to write: leave
that one, and say so, because it is the evidence.

---

## 10. Tests

**10.1 Every security control gets a negative test.** A control with only a happy
path is a control nobody has verified.

**10.2 Sweeps beat targeted checks.** `scanDatabaseFor` walks every column of
every table. A targeted check only tests the leak the author already imagined.

**10.3 Every sweep needs a positive control.**
`TestSecretScannerActuallyDetectsAValue` proves the scanner can find something —
otherwise the leak test passes vacuously forever.

**10.4 Pin security-relevant constants by test.** The CSP string, the runtime
method set, the list of routed paths. Weakening them should break a test.

**10.5 Test the invariant, not just the feature.**
`TestNoRestoreEndpointExists` asserts something that does not exist and must not
come to exist.

**10.6 Induce failures, do not mock them.** A mock of `SQLITE_FULL` proves the
mock works. `PRAGMA max_page_count` produces the real result code a full volume
produces; a second connection holding `BEGIN IMMEDIATE` produces a real lock
conflict; overwriting content pages produces the corruption a bad sector
produces. Test against the condition, not against your model of it.

**10.7 Bound every test that waits.** A shutdown test asserts on ELAPSED TIME as
well as outcome — a process that shuts down "successfully" in fifteen minutes
has the defect the test was written to catch, and only a time assertion sees it.

---

## 4d. Automation

**4d.1 Never let automation decide anything outside `DecideAutomation`.**
It is a pure function of its inputs — no clock, no database, no Docker — which
is what lets the most consequential judgement in the codebase be tested
exhaustively. A check added anywhere else is a policy decision made in a place
nobody will look for it.

**4d.2 Always fail closed on evidence you could not gather.**
A plan that could not be read is no plan. A window whose zone will not load is
closed. A policy that cannot be re-read does not authorise a rollback. An
unrecognised mode does not act. The direction is always towards "not updated",
because a missed update is a missed update and an unintended one is an incident.

**4d.3 Always read the clock once per pass.**
Two containers whose windows differ by a millisecond must not get different
answers because time moved between them.

**4d.4 Never let a container label widen anything.**
A label may narrow the strategy, replace the window's times, opt out, or pause.
It may not enrol a container, widen a ceiling, or change the mode. Anyone who
can run `docker run` can set a label; the asymmetry is the whole reason labels
are safe to honour at all.

**4d.5 Always record the decision, including the ones that declined.**
Every pass writes a run row before it examines anything, and every container it
considered gets a decision with a closed-vocabulary reason. "Why did you not
update that container" is only answerable from a record written at the time.

**4d.6 A rollback always pauses.**
Whatever the failure counters say, and whatever cooldown the policy configures.
The change was wrong and the host was moved twice to discover it.

**4d.7 Never audit an automated mutation twice.**
The acquisition, execution, and rollback services already record the outcome,
attributed to whoever the pass was attributed to. Recording it again in the
engine would make the host-change counter over-report the one number an
administrator most needs to trust.

**4d.8 Bound every automation read by count, not just by page.**
A pass's estate read, its policy load, its decision write, and the follower's
window are all capped, and a cap that bites is REPORTED on the run rather than
silently truncating. A scheduler that quietly covered a prefix of the host would
be worse than one that refused.

## Pull request checklist

- [ ] No new `docker.Runtime` method
- [ ] No new method on any of the three mutation interfaces
- [ ] No Docker capability, and no new request field, reachable from automation
- [ ] Any new automation check lives in `DecideAutomation` and fails closed
- [ ] No Docker SDK import outside `internal/docker`
- [ ] Every SQL value bound; every identifier from an allowlist
- [ ] Every new input validated and bounded
- [ ] No secret in a response, a log, or the database
- [ ] Every new route declares an access policy, and a public one says why
- [ ] Every new state-changing route calls `auditWrite` or is listed as audited
- [ ] No role compared in a handler; permissions are typed constants
- [ ] Any new long-lived response re-authorizes on its own cadence
- [ ] Any new host-changing operation audits its outcome, attributed
- [ ] Any new documented filter is actually applied, not accepted and dropped
- [ ] Errors sanitised
- [ ] Negative tests for each new control
- [ ] `gofmt`, `go vet`, `go test -race`, `golangci-lint`, `govulncheck` clean
- [ ] OpenAPI updated if a route or schema changed
- [ ] Threat model reviewed if a trust boundary moved
