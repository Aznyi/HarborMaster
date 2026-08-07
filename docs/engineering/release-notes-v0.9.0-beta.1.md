# v0.9.0-beta.1

The first beta. Everything below is implemented, tested, and enforced; what
"beta" means here is that it has not yet been run by many people on many hosts.

> **Not tagged or published.** This document is the prepared release note. See
> [Release readiness](#release-readiness) for what is still outstanding.

## What is new

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
- **No live-Docker acceptance test was run for this release on the development
  machine**, which has no Docker daemon. The integration suite runs in CI
  against the runner's daemon across five Engine API versions.
- **No multi-architecture image build or container vulnerability scan was run
  locally**, for the same reason. Both run in CI.
- **No browser acceptance pass was run against a live deployment.** The
  frontend's behaviour is covered by 257 component tests asserting on roles and
  accessible names, but nobody has clicked through a running instance for this
  release.
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
| Live Docker acceptance | **Not run.** No daemon on the development machine |
| Multi-architecture image build | **Not run locally.** CI only |
| Container vulnerability scan | **Not run locally.** CI only |
| Browser acceptance pass | **Not run** |
