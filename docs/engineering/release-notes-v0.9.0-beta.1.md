# v0.9.0-beta.1

**HarborMaster's first public release.** It keeps Docker containers updated the
way an operator would do it by hand: work out what changed, decide whether it
should be applied, apply it in a bounded window, prove the replacement works,
and undo it when it does not.

What "beta" means here is narrow and literal. Everything described below is
implemented and tested, and the safety properties are enforced by tests that
fail the build rather than by convention. What it has not had is many people
running it on many different hosts, which is the only thing that finds the
problems nobody thought to write a test for. Read
[Known limitations](#known-limitations) before you point it at anything you
care about.

**Every capability that can change your host is off by default**, and each one
has to be switched on deliberately. A fresh installation observes and reports;
it does not act.

## Installing

```sh
docker pull ghcr.io/aznyi/harbormaster:0.9.0-beta.1
```

**`latest` does not point at this release.** It is reserved for stable releases,
so pin the exact version above — or use `:beta`, the rolling prerelease channel,
if you want the newest beta as it lands and accept that it moves under you.

The Compose file defaults to this version; override `HARBORMASTER_TAG` to change
channel, or set `HARBORMASTER_IMAGE` to a digest for a fully reproducible
deployment. Published for `linux/amd64` and `linux/arm64`.

### The environment file goes in `deployments/`, not the repository root

```sh
# Note the destination.
cp .env.example deployments/.env
docker compose -f deployments/compose.yaml up -d
```

`-f deployments/compose.yaml` makes `deployments/` the Compose **project
directory**, and that is the only place Compose looks for `.env`. A copy left at
the repository root is read by nothing: every `${VAR:-default}` in the compose
file quietly takes its default, and there is no error to say so.

The setting this bites hardest is the Docker socket group. Left at its default,
HarborMaster starts, reports healthy, and cannot see Docker — because the
unprivileged container is not in the group that owns the socket. Set
`HARBORMASTER_DOCKER_GID` in `deployments/.env`:

```sh
stat -c '%g' /var/run/docker.sock          # native Linux, commonly 999 or 998
```

On Docker Desktop the socket inside the VM is owned by root, so the value is
`0`; read it from inside a container rather than from the host, where that
command reports the wrong thing. The compose file explains both cases at the
`group_add` block.

## What HarborMaster does

### Automated updates

An **update policy** is a standing rule: which containers HarborMaster may
update, how far a version may move, when it may act, and what to do if the
update goes wrong. A policy names containers and a ceiling — it never names an
image or a digest, because what a matched container moves to is decided by the
planner from registry evidence and re-verified before anything is pulled.

Four modes, and only one of them changes anything:

| Mode | What happens |
| --- | --- |
| **Observe** | Evaluates every matching container and changes nothing. The correct first setting on a real host. |
| **Dry run** | Observe, plus the order things would happen in. |
| **Require approval** | Decides automatically and waits for a person to release each update. |
| **Automatic** | Acquires and recreates without asking. |

**Maintenance windows** bound when an automatic policy may act — a start, an
end, optional weekdays, and an IANA timezone. Comparisons are made in that
zone with daylight saving applied, so a spring-forward gap opens nothing and an
autumn-back repeat opens twice, both correctly. The timezone database is
embedded in the binary rather than taken from the runtime image. **A window
whose zone cannot be resolved fails closed** and refuses every update under that
policy, reported as such rather than silently ignored.

The **update ceiling** is a ceiling, not an instruction: same-tag-only, patch,
minor, or major. "Up to a minor version" permits same-tag, patch, and minor
moves when everything else agrees; it never forces one. A version change
HarborMaster could not size — a registry that did not answer, a tag listing that
ran out of budget — is `unknown`, and no ceiling permits it. Neither does any
ceiling permit a pre-release.

When a policy does act, the sequence is the same one a manual update takes:

1. **Digest-pinned acquisition.** The image is pulled by digest, and the digest
   is *computed* from what arrived rather than believed. A mismatch is recorded
   and no container is touched.
2. **Verified recreation.** The original is stopped and parked under a derived
   name — never removed — and the replacement is created from the exact artefact
   that was acquired. The replacement must then prove itself: it is running and
   stable, it is on the image that was planned, its configuration matches the
   original field by field, and its networks are attached. Any of those failing
   fails the recreation, with the original still on the host.

Automation holds no Docker capability of its own. It submits the same three
requests an operator's own click submits, to the same services, each of which
re-runs its own preflight against the live host at the moment it acts.

**Off by default.** `HARBORMASTER_AUTOMATION_ENABLED` also requires recreation
and rollback to be enabled, and HarborMaster refuses to start on an inconsistent
combination rather than quietly ignoring one.

### Approval workflow

`Require approval` is the middle ground between watching and acting: the machine
does the deciding, a person does the committing.

A pass in this mode records a decision per container and submits nothing.
Anything waiting appears in a **pending approvals queue** with what would change,
from what to what, how large the version move is, and the planner's assessment.
Approving one releases exactly that decision.

**An approval is not a bypass.** The released update goes through the identical
pipeline an automatic one does — acquisition preflight, digest verification,
recreation preflight, preservation comparison, health proof — and any of them can
still refuse it. Approval decides *whether*; it does not decide *safely*.

**A stale decision is refused rather than applied.** Between the pass and the
approval, the world can move. Before submitting, HarborMaster re-reads the
container's current change plan and refuses with a conflict if it no longer
matches the plan the decision was made against. A container that has since been
paused is not approvable at all. The approval is audited against the approver,
and the resulting acquisition and recreation are attributed to them rather than
to the scheduler.

HarborMaster's own container is refused on the approval path as well as in the
decision — approving cannot reach what the engine would not have proposed.

### Automatic rollback

A recreation that fails verification leaves a replacement that does not work and
an original that is parked and stopped. Rollback puts the original back.

It is **policy-driven and optional**. Each policy chooses between:

- **Stop and require operator review.** HarborMaster stops and writes a recovery
  plan naming both containers by name and id, with the exact commands. Nothing
  further is done automatically.
- **Roll back automatically.** HarborMaster restores the container it replaced,
  verifies the original the same way it verified the replacement, and leaves the
  failed replacement stopped and renamed aside as evidence.

**A rollback always pauses that workload afterwards**, whatever the failure
counters say. The change was wrong and the host moved twice; retrying that on a
timer is how one bad image becomes a repeated outage. The pause does not expire
on its own — an operator has to clear it.

Two things this is not:

- **It is not a guarantee.** Rollback runs only where its preconditions can be
  established from HarborMaster's own checkpoint: it names an *execution*, not a
  container, so it can only undo an arrangement HarborMaster itself created and
  recorded. Where the checkpoint cannot establish what is true of the host —
  after an interruption between a mutation and its record, for instance —
  HarborMaster stops and writes a recovery plan instead of guessing. It never
  decides for itself which container should be serving.
- **It is not free of downtime.** The replacement is stopped before the original
  is started, and the original must then start and pass its checks. There is a
  real gap, and no overlap to remove: two containers cannot hold one name.

Rollback is off by default and requires recreation to be enabled.

### Update policies can cover all eligible containers

"Keep my containers updated" is the thing most people want from an updater, and
until now it could only be expressed by typing a pattern into a selector and
hoping it meant what you thought.

A policy's breadth is now a **first-class typed scope** — its own field, its own
two-value vocabulary, its own column, and its own database constraint:

- **Selected containers**, chosen from the inventory.
- **Matching images**, by glob pattern.
- **Advanced selection**, the full name/image/label selector with exclusions.
- **All eligible containers.**

**It is not implemented as `*`.** A bare `*` image pattern is still refused by
name; an empty selector still governs *nothing*, which is the opposite meaning;
and a magic value in the container-name field is just a container name. All
three would have made the reach of a rule a property of a string, discovered by
parsing, rather than something an operator chose.

**Broad selection is not broad authorisation.** The scope widens what a policy
looks at and changes nothing else. Every container it selects still passes, in
order: the pause, the container's own opt-out labels, the ceiling, the
deployment-wide major-version rule, the planner's recommendation, the
maintenance window, the in-flight check, the mode, the run and concurrency
budgets, and all four preflights. A policy in this scope in Observe mode changes
nothing, exactly like any other policy in Observe.

**"Eligible" is not "present".** A container existing is not evidence
HarborMaster may act on it. These are never enrolled by a broad policy, and only
naming one explicitly reaches it:

- **HarborMaster's own container**, which it cannot update at all.
- **The parked originals and quarantined replacements** an earlier update left
  behind. Those are evidence; enrolling the wreckage of a failed update into the
  automation that produced it is how one bad image becomes two.
- **A container carrying `io.harbormaster.enabled=false`**, the estate-wide
  opt-out its owner controls.
- **A workload HarborMaster could not have recreated anyway**, because its name
  could not survive being parked.

**Exclusions win.** A policy's `exclude` list is checked before the scope and is
final in both, so "everything except the database" is one rule. It is the only
selector clause the broad scope accepts — a policy that also carried an
inclusion clause would mean different things depending on which field you read
first, so that combination is refused rather than reconciled.

**Existing policies do not silently broaden.** The scope column defaults to the
narrow reading, so every policy written before this release keeps exactly the
breadth its selector always had, and an archived one is untouched. A partial
edit that does not mention the scope cannot change it. And where a specific rule
and a catch-all both reach a container at the same priority, the **narrower one
wins** — adding a catch-all cannot quietly take containers away from the rules
you wrote for them.

### Operator-first interface

Earlier builds showed everything HarborMaster knew, in roughly the order it
learned it. The interface now leads with what needs a person.

- **An attention-first dashboard.** The first thing on the page is what needs
  you and why — unhealthy containers, updates waiting for approval, paused
  automation, failed recreations that left something on the host. Underneath it,
  and deliberately not hidden, is **what HarborMaster has not established**: no
  compliance policies defined is not a clean bill of health, and an empty page
  now says which of the two it is.
- **Update and attention state on the container list.** Whether a container has
  an update available, whether it needs attention, and whether automation is
  paused for it are visible without opening anything.
- **A clearer container overview**, leading with state, image, and what
  HarborMaster would do next rather than with configuration detail.
- **Pending approvals are actionable where you find them**, showing what would
  change and what it would move from and to, with the confirmation stating in
  plain words that a container will be stopped and replaced.
- **A rebuilt policy editor.** It is organised around five questions — what to
  manage, how far updates may go, how they should happen, when, and what happens
  if one fails — rather than around the shape of the stored object. Each choice
  carries the sentence that says what it does, the warning shown matches the
  mode you are actually configuring rather than always warning about the most
  dangerous one, and before saving it renders the whole policy as one plain
  sentence generated from the request it is about to send.
- **Operator-facing terminology throughout.** "Up to a minor version" rather
  than an internal strategy name, and a stated ceiling rather than an implied
  one.

## Also in this release

### Persistent image lineage — containers stay updatable

A recreation is digest-pinned on purpose: the replacement is created from the
exact artefact that was planned, acquired, approved and verified, never from
the mutable tag it came from.

That was correct and, on its own, fatal. The next inventory pass saw only a
digest, a digest cannot move, so the planner had nothing to propose and the
container fell out of automation permanently. **Every container received exactly
one automated update, and then never another one.**

HarborMaster now records, per container, what it *follows* as distinct from
what that container *runs*:

```
EXECUTION REFERENCE   repo@sha256:…   what runs. Immutable.
TRACKING REFERENCE    repo:tag        what is watched. Mutable.
```

Update discovery resolves the tracking reference and compares it against the
digest actually running. Execution still only ever uses a digest that came out
of the pipeline.

What shapes the design:

- **The database is authoritative; the container label is evidence.** A
  recreation writes `io.harbormaster.image.tracking` onto the replacement so
  lineage survives a lost database and is readable with `docker inspect`. A
  label alone can never confer managed status, and one naming a different
  repository than the image running is refused outright — otherwise anyone able
  to run `docker run` could point update discovery at a repository they
  control.
- **The digest a container runs is resolved from the local image**, matched to
  that exact repository, and ambiguity is reported as unknown rather than
  guessed.
- **A rollback returns lineage to the artefact that is running again**, without
  touching the tag you asked HarborMaster to follow.
- **A host changed underneath HarborMaster re-establishes from what is
  actually running**, recorded as *observed* — HarborMaster never credits
  itself with a change it did not make.
- **Existing digest-pinned workloads are left alone**, marked untracked and
  clearly shown as such. No tag is invented. See
  [Upgrading](upgrading.md#containers-an-older-harbormaster-already-updated).

### Operator notifications

HarborMaster can now tell you when something happened, instead of waiting for
you to look.

Seventeen events across the whole pipeline — an update discovered, an approval
needed, an image pulled or not, a container updated or not, a rollback started,
succeeded, or failed, automation paused, a pass that could not complete, drift,
a policy violation, a registry that cannot be reached, an integrity failure.

Five channels: Slack, Discord, Microsoft Teams, a generic JSON webhook, and
email through an SMTP relay.

**Off by default.** This is HarborMaster's second outbound egress, and unlike
image intelligence it goes somewhere a human typed.

What shapes the design:

- **A notification never delays the thing it is about.** Raising one puts it on
  a bounded queue and returns; it cannot block and returns no error. A rollback
  whose duration depended on somebody else's webhook server would be a worse
  rollback.
- **A full queue drops, and says so.** Recorded as `dropped` in the history —
  the honest failure mode. Blocking would stall the pipeline; an unbounded
  queue would turn an outage into unbounded memory growth.
- **A notification cannot carry a secret**, because the type has nowhere to put
  one. Every sentence lives in one file and two architecture tests hold that.
- **A destination's credential never leaves the database.** A webhook URL's
  path is a bearer token, so it is accepted on the way in and returned by
  nothing; reads get a scheme and host. Archiving a destination destroys it.
- **The address guard is two-stage.** The URL is validated when stored and the
  resolved ADDRESS is re-checked at dial time, which is what defeats DNS
  rebinding.

### HarborMaster refuses to update itself

Applied to its own container, an update is not an update: the process stops the
container it is running in and dies between the stop and the checkpoint. Nothing
verifies the replacement, nothing records what happened, and no rollback is
possible because the thing that would perform it is gone.

So it is refused — not deferred, not made safe. The refusal is enforced at four
independent layers: the automation decision, the approval path, the acquisition
preflight, and the execution preflight. Identity comes from four independent
signals — a configured id, `/proc`, the hostname, a label — any one of which
suffices and none of which is required. A signal that could not be established
is empty, and an empty signal matches nothing, so a failed detection excludes
nothing rather than the wrong container.

**No setting turns it off**, and an architecture test fails the build on one.

Update HarborMaster from outside itself, with
`docker compose pull && docker compose up -d`. The interface says so, by name,
on the container it believes is its own.

### Settings says what the deployment can actually do

Almost everything HarborMaster can do to a host is off by default, which means
an operator looking at an empty page cannot otherwise tell "switched off" from
"not working". Settings reports which capabilities exist in the running process
— booleans only, never a configured value.

### Deployment

- **The environment file location is documented and enforced.** `.env` belongs
  in `deployments/`, and the README, the compose file, and
  [Installing](#the-environment-file-goes-in-deployments-not-the-repository-root)
  all now say so with the reason. A copy at the repository root is read by
  nothing, which previously produced a HarborMaster that started, looked
  healthy, and could not see Docker.
- Every feature toggle and every security-relevant setting is forwarded by
  `deployments/compose.yaml`, and a test fails the build if one is not. A
  setting that is documented but not forwarded cannot be set at all by an
  operator following the supported deployment.
- `HARBORMASTER_DOCKER_API_VERSION` pins the Engine API version instead of
  negotiating one, for a daemon whose negotiation misbehaves and for the
  compatibility matrix in CI.
- A supported-environments matrix in the README states plainly what is
  supported, what is not, and what will not be.

### The database is owner-only, and account names are discoverable

Two findings from release validation, related by one incident: discovering a
username required copying the SQLite database off the host, which is when the
database's own permissions got looked at.

**The database is now `0600` and is restricted at every start**, before anything
reads from it. SQLite creates it subject to the process umask, which on an
ordinary host yields `0644` — readable by every account on the machine, holding
every Argon2id verifier, every live session's keyed digest, and the security
audit log. The `-wal`, `-shm`, and `-journal` sidecars carry the same pages and
get the same treatment; backups already did.

Upgrading tightens an existing database automatically and says so once. **Read
the warning literally**: tightening the file does nothing about a copy somebody
already took, so on a shared host treat the verifiers as disclosed and reset
passwords. If the mode cannot be established HarborMaster does not start, and a
database reached through a symbolic link is refused — `chmod` follows symlinks,
so honouring one would let whoever planted it choose which file gets changed.

**`harbormaster admin list-users`** answers the question that forced the copy:

```
USERNAME  ROLE            STATUS    PASSWORD
hm-admin  administrator   active    set
watcher   viewer          disabled  must change at next login
```

Four columns, and the type behind them has four fields: no verifier, no session
digest, no key material, no password timestamp. Console only, like the other
recovery commands — an account list is the first thing an unauthenticated scrape
would want. Two architecture tests hold it there, one pinning the field set and
one failing the build if the HTTP layer names it.

## Fixed before release

Defects found during pre-release validation rather than in the field. There is
no earlier release for these to have shipped in; they are recorded because how a
defect was found is worth as much as the fix.

- **Automation decisions that reached a rollback were never settled**, so
  `PendingDecisions` returned them forever and the follower re-read them on
  every tick. A Phase 11 defect, caught by a test that had been failing
  unnoticed.
- **A notification requeued from a busy destination re-ran the routing rules**,
  which would have re-delivered it to every destination that had already
  accepted it. Found before release by a test written for exactly that.
- **`NotificationRule.Validate` discarded its normalised limits.** A rule's
  destination ceiling is now the smaller of the type's bound and the
  deployment's destination limit.
- **Signing out over HTTPS deleted the plain-name session cookie without the
  Secure attribute.** A cookie's identity is its name, domain, and path, so a
  Secure deletion removes a non-Secure cookie of the same name — there was
  never a reason to send the deletion insecurely when the connection was
  secure. The deletion now follows the connection, and plain HTTP on loopback
  behaves exactly as before. This closes the one CodeQL finding the project
  carried.

## Upgrading

This is the first published release, so there is no earlier version to upgrade
from. If you have been running an `edge` build or a local build from source, see
[Upgrading HarborMaster](upgrading.md): nothing to do beyond
`docker compose pull && docker compose up -d`. Migrations run at startup and the
data volume is untouched.

Two things worth knowing if you are coming from an `edge` build:

- Notifications are off until configured. Setting
  `HARBORMASTER_NOTIFICATIONS_ENABLED` alone sends nothing; a destination and a
  rule are also needed.
- Update policies gained a **scope** field. Existing policies are migrated to
  the narrow reading and keep exactly the breadth their selector already had;
  nothing broadens, and archived policies are untouched.
- The `notification:read` and `notification:manage` permissions are added to
  the existing roles automatically. No account changes.

**HarborMaster does not upgrade itself**, by design — see
[HarborMaster refuses to update itself](#harbormaster-refuses-to-update-itself).

## Known limitations

Beyond the [supported environments](../../README.md#supported-environments)
matrix:

- **A recreation needs a completed compliance evaluation, and a fresh
  installation has none.** The execution preflight refuses with `policyStale`
  when a container has no compliance evaluation, when the evaluation is older
  than `HARBORMASTER_EXECUTION_POLICY_FRESHNESS`, or when the pass that produced
  it did not complete. On an installation with **no compliance policies defined
  at all**, no evaluation is recorded for any container, so every recreation —
  manual or automated — is refused until at least one compliance policy exists
  and a pass has run. The refusal is correct in intent and fails closed; what it
  does not do is explain that the remedy is to define a compliance policy. Watch
  for `policyStale` if updates are being decided but never applied.
- **A broad policy can decline a container you expected it to cover.** "All
  eligible containers" requires positive evidence, so a workload HarborMaster
  cannot establish as recreatable is passed over rather than swept in. The
  decision records `notEligible` and says which fact decided it. The remedy is
  to name the container explicitly, which still reaches it. The polarity is
  deliberate: failing the other way would enrol the containers a broad policy
  must never touch.
- **A container that appears after a broad policy is written is governed by the
  next pass.** That is what a standing rule over a set means, and it is equally
  true of an image pattern. It still needs a plan, a permitted recommendation,
  an open window, and every preflight; `io.harbormaster.enabled=false` on the
  container opts it out before any policy is consulted; and
  `AUTOMATION_MAX_PER_RUN` bounds how many one pass may start.
- **`backup.failed` is a selectable notification event that nothing raises.**
  Backup is a command run outside the server process, so the running server
  never observes a backup outcome. The event exists so that a future scheduled
  backup cannot ship without its notification; today it never fires.
- **The literal same-tag update sequence has not been executed against a
  registry under our control.** Repeated updates through a moving tag are
  covered live against a public registry — including a digest moving beneath an
  unchanged tag — and deterministically end to end. Reproducing the exact
  A→B→C sequence would need a private registry, and reaching one would mean
  relaxing the SSRF, TLS and registry-host controls. It was not done, and those
  controls were not touched.
- **Notification delivery has not been exercised against a real Slack,
  Discord, Teams, or SMTP endpoint.** The transport, the address guard, the
  channel formatting, and the engine are tested; the wire format has not been
  confirmed by a receiving service.
- **One dependency advisory is accepted rather than fixed.** GO-2026-5932
  reports that `golang.org/x/crypto/openpgp` is unmaintained. There is no fixed
  version and there will not be one; HarborMaster does not use the package, and
  two architecture tests fail the build if that stops being true. See
  `docs/security-triage.md`.
- **Remote and TLS Docker sockets are untested.** `HARBORMASTER_DOCKER_HOST`
  accepts one; nothing verifies client-certificate authentication.
- **Restore is still not implemented**, and neither is fleet management,
  orchestration, or editing a container's configuration. Each is absent by
  design, not pending.

## Release readiness

Every gate below ran on the release commit. The workflow results are on the
commit in GitHub Actions; the two rows marked *(local)* are pre-release checks
that are not part of CI.

| Gate | State |
| --- | --- |
| `go vet`, `gofmt`, `golangci-lint` | Clean |
| `go test ./...` | Passing |
| `go test -race` | Passing across every package with concurrency |
| `govulncheck` | No vulnerabilities in called code |
| `npm audit` | No vulnerabilities |
| CodeQL (Go) | **No findings** |
| CodeQL (JavaScript/TypeScript) | No findings |
| Trivy (dependencies, config, image) | One accepted finding, ignored with a reason, an expiry, and two enforcing tests |
| Frontend typecheck and tests | Passing |
| OpenAPI covers every routed path | Enforced by test |
| Migration matrix | Every schema version upgrades to current |
| Downgrade refusal | A database from a newer build is refused, not migrated |
| Live Docker integration (CI) | Passing across five Engine API versions: 1.44, 1.45, 1.47, 1.48, and negotiated |
| Live Docker acceptance *(local)* | Passing. Full pipeline against a real daemon on disposable containers: observe, automatic on named containers, broad scope with exclusions, self-update refusal, label opt-out, preserved-container refusal, maintenance window open and closed, and a forced verification failure that rolled back and paused. Approval mode is covered by tests rather than by this pass |
| Accessibility and responsive pass *(local)* | Passing. axe-core, no WCAG 2.0/2.1 A or AA violations on the update-policy pages at 390, 768, 1440 and 1920 px; no page-level horizontal overflow; visible focus; keyboard-only policy creation. Other routes were not re-run at these widths for this release |
| Multi-architecture image build | Passing. `linux/amd64` and `linux/arm64` |
| Container vulnerability scan | Passing. No HIGH or CRITICAL findings |
| Authenticated container smoke tests | 74 of 74 passing |
