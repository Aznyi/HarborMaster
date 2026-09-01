# v0.9.0-rc.1

**The first release candidate for 0.9.0.** The 0.9.0 feature set is complete and
frozen. What remains before a stable release is running time on hosts that are
not ours.

What "release candidate" means here is narrow and literal: this is the build we
intend to release as 0.9.0, and the only changes expected before then are fixes
for defects it turns up. It is not a preview of unfinished work — everything
described below is implemented, tested, and enforced by tests that fail the
build rather than by convention.

**Every capability that can change your host is off by default**, and each has
to be switched on deliberately. A fresh installation observes and reports; it
does not act.

## Installing

```sh
docker pull ghcr.io/aznyi/harbormaster:0.9.0-rc.1
```

**`latest` does not point at this release.** It is reserved for stable
releases, of which there has not yet been one. Pin the exact version above, or
use `:rc` — the rolling release-candidate channel — if you want the newest
candidate as it lands and accept that it moves under you.

`rc` and `beta` are separate channels, and a release joins exactly one. The
`beta` tag stays where it is; it does not follow release candidates.

The Compose file defaults to this version. Override `HARBORMASTER_TAG` to change
channel, or set `HARBORMASTER_IMAGE` to a digest for a fully reproducible
deployment. Published for `linux/amd64` and `linux/arm64`.

## Highlights

Since `v0.9.0-beta.2`, HarborMaster gained an unattended update lifecycle that
runs end to end, a per-container way to say how much autonomy each workload
gets, retained-image cleanup that cannot destroy anything recovery depends on,
and notification wording that distinguishes an update that succeeded from one
that was recovered.

## Automation

### Three behaviours, set per container

Fleet-wide policy decides what HarborMaster may do in general. A **container
update behaviour** decides how much of that applies to one workload, and it can
only ever make automation *safer*:

| Behaviour | What it does |
| --- | --- |
| **Automatic** | Imposes no restriction. The governing policy decides — which is also what happens when no behaviour is set. It is stored rather than represented by absence, so "an operator looked and chose automatic" and "nobody has looked" stay different facts. |
| **Review First** | Caps the mode at `approvalRequired`. Automation still decides; a person releases each decision. |
| **Monitor Only** | Caps the mode at `observe`. HarborMaster keeps checking registries and reporting, and never changes the container by itself. |

A behaviour is a *cap*, never a grant. Setting one on a container a policy does
not select changes nothing, and no behaviour can widen what a policy permits.

### The unattended lifecycle runs end to end

An automatic policy now carries an update from detection to a verified,
notified outcome without a person in the loop, and each stage re-establishes its
own preconditions rather than trusting the previous one:

1. **Digest-pinned acquisition.** The image is pulled by digest, and the digest
   is *computed* from what arrived rather than believed. A mismatch is recorded
   and no container is touched.
2. **Verified recreation.** The original is stopped and parked under a derived
   name — never removed — and the replacement is created from the exact artefact
   that was acquired. Configuration is preserved field by field and then
   *verified* against the original: the replacement must be running and stable,
   on the image that was planned, matching the original's configuration, and
   with its networks attached.
3. **Policy-gated automatic rollback.** If the replacement fails to prove
   itself, and the governing policy permits it, HarborMaster restores the parked
   original. Rollback is a policy decision, not an automatic reflex — a policy
   that does not permit it leaves the failure in place, preserved and visible,
   for a person.

Automation holds no Docker capability of its own. It submits the same three
requests an operator's own click submits, to the same services, each of which
re-runs its own preflight against the live host at the moment it acts.

**Broad selection is not broad authorisation.** A policy's breadth is a typed
field with a two-value vocabulary, not a wildcard inside a selector. An empty
selector governs nothing, `*` is refused by name, and `scope: allEligible` is
something an operator sets deliberately. It widens what a policy *looks at* and
changes no other check.

## Recovery

### A recovered update is not a successful one

When an update fails and the rollback succeeds, the host is back where it
started — but something still went wrong, and reporting that as success would
hide the one outcome most worth reading. **Recovered is a distinct terminal
state** throughout: in the execution record, in the audit log, in the UI, and in
notifications.

The distinction matters operationally. A run of successes needs no attention. A
run of *recoveries* means updates are being attempted and failing, and the fact
that each one was caught is not a reason to stop looking.

### Configuration preservation is proved, not assumed

A recreated container's configuration is compared against the original after the
replacement is running. A field that did not survive fails the recreation with
the original still on the host, rather than leaving a silently different
container running under the same name.

## Image safety

Retained-image cleanup removes local images that HarborMaster's own settled
updates superseded. It is the first capability in HarborMaster that can destroy
something, and it is built accordingly.

**It is off by default** (`HARBORMASTER_IMAGE_CLEANUP_ENABLED=false`) and must be
enabled deliberately.

**Candidates are derived, never named.** They come from HarborMaster's own
execution records. No endpoint removes an image, and there is no field anywhere
in the API for a caller-supplied image identifier.

**It never force-removes.** Force is not a parameter — there is nowhere in the
removal request to put it, the call site passes a literal `false`, and a static
check fails the build on any attempt to add one. A daemon that refuses because
something still references an image has *answered*: the answer is keep, and it
is never retried.

**It retains anything recovery could need.** An image is kept when a present
container is running it, when it backs HarborMaster itself, when it belongs to a
parked original or a quarantined replacement, when an acquisition, execution or
rollback is in flight for it, when a failure has not settled, when a recovery
plan is outstanding, when a current plan targets it, or when it is a recent
rollback generation. Every gate fails closed: an unreadable source retains
everything, because a check that could not be *performed* establishes nothing.

**A known limit, stated plainly.** HarborMaster's image provenance cannot
distinguish an image *it* introduced from one that was already on the host. It
compensates with the age floor, the retained-generation count, the per-pass cap
and the retention gates above, and every candidate must still trace to a settled
HarborMaster execution — but the underlying fact is a limitation of what Docker
records, not something HarborMaster has solved.

## Visibility

**Notifications distinguish outcomes.** Success, failure and recovered are three
different messages, not one message with a status field, so a destination that
shows only the first line still shows which of the three happened.

Every sentence HarborMaster can send is written in one file, and no notification
carries anything but HarborMaster's own words — no error text, no registry
response, no configuration value. Notification egress is off by default, and the
destination credential is stored as a separate type in a separate table,
returned by no endpoint, and destroyed with the destination.

**The policy editor says what a policy would act on.** As you describe a policy,
the editor reports how many containers HarborMaster could act on right now and
groups the rest by why it would not — computed by the same decision code a
scheduled pass runs, so the count cannot drift from what automation will
actually do. Nothing is stored, downloaded, contacted or touched to produce it.

**A fresh installation says what it is waiting for.** An estate that has not been
assessed no longer renders identically to one that was assessed and found
nothing to do. "Not established" is never shown as "nothing to do".

## Security

- The Go toolchain floor moved to 1.26.6, clearing eight standard-library
  advisories that a skewed runner had been analysing around.
- `golang.org/x/crypto` was updated for CVE-2026-56854.
- `modernc.org/sqlite` moved to 1.57.0.
- The published API contract is now checked as a document, not only as a list of
  paths. Two defects this surfaced are fixed in this release: the specification
  was not valid YAML, and sixteen `$ref`s named components that did not exist.
  Both are covered by tests that fail the build.
- Container release channels are now pinned to the version being published.
  `rc` and `beta` each require their own marker in the tag, so a release cannot
  join a channel it was not cut for.

## Upgrading

Follow [Upgrading HarborMaster](upgrading.md). The named volume carries the
database across the replacement; nothing in this release requires a manual
migration step.

## Known RC limitations

- **`Notification.Fields` is not delivered.** The structured field set exists on
  the notification type and is not yet rendered by the delivery path. Messages
  carry their prose; the structured payload is post-RC work.
- **Image-cleanup pass status is audit-oriented.** A pass records what it
  considered and what it did to the audit log. There is no HTTP surface
  reporting a "last pass" object, and adding one is post-RC work.
- **Image cleanup cannot attribute provenance.** As above: HarborMaster cannot
  tell whether it originally introduced a given image.
- **Remote and TCP Docker sockets are not supported for 0.9.0.**
  `HARBORMASTER_DOCKER_HOST` accepts one, but nothing tests TLS client
  authentication against a remote daemon.
- **One host, one socket.** There is no fleet concept and no code that could
  address a second daemon. Kubernetes and Swarm are not supported and not
  planned.
- **No restore.** HarborMaster records a container's configuration and reports
  whether it *could* be restored. Rollback is bounded to undoing an execution
  HarborMaster itself performed.
