# v0.9.0-beta.1

The first beta. Everything below is implemented, tested, and enforced; what
"beta" means here is that it has not yet been run by many people on many hosts.

> **Not tagged or published.** This document is the prepared release note. See
> [Release readiness](#release-readiness) for what is still outstanding.

## Installing this beta

```sh
docker pull ghcr.io/aznyi/harbormaster:0.9.0-beta.1
```

**`latest` does not point at this release.** It is reserved for stable releases,
so pin the exact version above — or use `:beta`, the rolling prerelease channel,
if you want the newest beta as it lands and accept that it moves under you.

The Compose file defaults to this version; override `HARBORMASTER_TAG` to change
channel, or set `HARBORMASTER_IMAGE` to a digest for a fully reproducible
deployment. Published for `linux/amd64` and `linux/arm64`.

## What is new

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

Previously nothing stopped a plan naming HarborMaster's own container, and
acting on one would have killed the process partway through a recreation it
could then neither verify nor undo.

The refusal is enforced at four independent layers: the automation decision,
the approval path, the acquisition preflight, and the execution preflight.
Identity comes from four independent signals — a configured id, `/proc`, the
hostname, a label — any one of which suffices and none of which is required.

**No setting turns it off**, and an architecture test fails the build on one.

### Settings says what the deployment can actually do

Almost everything HarborMaster can do to a host is off by default, which meant
an operator looking at an empty page could not tell "switched off" from "not
working". Settings now reports which capabilities exist in the running process
— booleans only, never a configured value.

### Deployment

- Every feature toggle and every security-relevant setting is now forwarded by
  `deployments/compose.yaml`, and a test fails the build if one is not. A
  setting that is documented but not forwarded cannot be set at all by an
  operator following the supported deployment, and that has shipped before.
- `HARBORMASTER_DOCKER_API_VERSION` pins the Engine API version instead of
  negotiating one, for a daemon whose negotiation misbehaves and for the
  compatibility matrix in CI.
- A supported-environments matrix in the README states plainly what is
  supported, what is not, and what will not be.

## Fixed

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

See [Upgrading HarborMaster](upgrading.md). Nothing to do beyond
`docker compose pull && docker compose up -d`; migrations run at startup and
the data volume is untouched.

Two things worth knowing:

- Notifications are new and off. Setting `HARBORMASTER_NOTIFICATIONS_ENABLED`
  alone sends nothing; a destination and a rule are also needed.
- Two permissions are new, `notification:read` and `notification:manage`. They
  are added to the existing roles automatically. No account changes.

## Known limitations

Beyond the [supported environments](../../README.md#supported-environments)
matrix:

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
| Live Docker acceptance | Passing. Full pipeline, repeated updates, rollback, and interruption recovery against a real daemon |
| Multi-architecture image build | Passing. `linux/amd64` and `linux/arm64` |
| Container vulnerability scan | Passing. No HIGH or CRITICAL findings |
| Authenticated container smoke tests | 74 of 74 passing |
| Browser acceptance pass | Passing. Every route, no console errors or failed requests |
