# Security triage

Dependency advisories that have been assessed but not remediated by a version
bump, and why. Each entry records the advisory, the feature that carries the
vulnerability, and the evidence that the feature is or is not reachable from
HarborMaster.

An entry here is not a dismissal. It is the reasoning that justifies the
scheduling decision, written down so the next person does not have to redo it.

Assessed 2026-08-04. The Docker entry was updated the same day, when the
migration described in it was carried out.

---

## GHSA-qwww-vcr4-c8h2 — React Router RSC Mode CSRF bypass

| | |
|---|---|
| Package | `react-router` (npm) |
| Was | 7.18.2, transitively via `react-router-dom` 7.18.2 |
| Now | **8.3.0**, imported directly |
| Affected | `>= 7.12.0, < 8.3.0` |
| Severity | High (7.1) |
| Status | **Resolved.** Upgraded to the patched version. It was never reachable here; the analysis below is retained as the record of why it was not an emergency. |

### How it was resolved

Not by upgrading `react-router-dom`: that package has no v8. Its latest release
is 7.18.2, which depends on `react-router` 7.18.2 — so following the DOM package
can never reach the patched version. React Router consolidated in v8, and the
DOM-specific exports now live in `react-router` itself.

The migration was therefore a package swap, not a version bump:

- removed `react-router-dom`, added `react-router` `^8.3.0`
- rewrote `from "react-router-dom"` to `from "react-router"` in the ten files
  that imported it

No API changes were needed. Every symbol in use — `BrowserRouter`, `Routes`,
`Route`, `Navigate`, `Link`, `NavLink`, `useParams`, `useLocation`, and
`MemoryRouter` in tests — is exported from `react-router` v8 unchanged, and the
app stays in Declarative Mode.

`react-router` v8 requires `react >= 19.2.7`; 19.2.8 was already installed, so
React itself did not move.

Verified with `npm run typecheck`, `npm test` (86 passing), `npm run build`, and
`npm audit` (**0 vulnerabilities**). The rebuilt bundle was then run through the
Go binary and checked in a browser-equivalent fetch: `/`, a deep link
(`/containers/<id>`), `/events`, and an unknown path all serve the SPA shell and
reference the new bundle.

### The vulnerable feature (why this was never urgent)

A CSRF bypass in React Router's **RSC (React Server Components) mode**: a
server action could execute before the request was rejected with 400. The
advisory states it "only affects your application if you are using the unstable
RSC APIs".

Exploiting it requires a server that runs router-dispatched actions. HarborMaster
has no such server.

### Why it is not reachable

The frontend is a client-only Vite SPA in Declarative Mode. Every capability the
advisory depends on is absent:

- **No server runtime for the router.** `npm run build` is `tsc --noEmit && vite
  build`, producing static assets in `web/dist`. Those are compiled into the Go
  binary by `//go:embed all:dist` ([web/embed.go:13](../web/embed.go#L13)) and
  served as files. No Node process ever renders or dispatches a route.
- **Declarative Mode, not Data Mode.** [main.tsx:15](../web/src/main.tsx#L15)
  renders `<BrowserRouter>`, and [App.tsx](../web/src/App.tsx) uses
  `<Routes>`/`<Route>`. There is no `createBrowserRouter` or `RouterProvider`
  anywhere, so the data router that owns loaders and actions is never
  instantiated.
- **No loaders or actions.** No route declares a `loader` or `action` property.
  Data is fetched through the app's own `api/client.ts`, not through the router.
- **No RSC APIs.** Searching `web/src` for `rsc`, `RSC`, and `unstable_` returns
  no matches.
- **No Framework Mode.** None of `react-router.config.*`, `entry.server.*`,
  `entry.client.*`, `root.tsx`, or `routes.ts` exists.
- **No server rendering.** No `renderToString`, `renderToPipeableStream`,
  `hydrateRoot`, or `StaticRouter`. Entry is `createRoot(...).render(...)`.
- **No untrusted redirect targets.** The only redirect is the catch-all
  `<Route path="*" element={<Navigate to="/" replace />} />`
  ([App.tsx:31](../web/src/App.tsx#L31)), whose destination is the constant
  `"/"`. No navigation target is derived from user input, query parameters, or
  API responses, and `redirect()` is never imported.

### Reproducing the evidence

```sh
cd web
rg -n "createBrowserRouter|RouterProvider|loader:|action:|redirect\(" src   # no router data-API usage
rg -n "rsc|RSC|unstable_" src                                              # no matches
rg -n "renderToString|hydrateRoot|StaticRouter" src                        # no matches
ls react-router.config.* entry.server.* root.tsx routes.ts 2>/dev/null     # absent
npm audit
```

### Rest of the dependency graph

Before the upgrade, `npm audit` reported 2 high-severity entries, both this same
advisory — once against `react-router` and once against `react-router-dom`,
which was flagged only for depending on it. No advisory applied to Data Mode,
SPA navigation, redirect handling, or `react-router-dom` itself.

After the upgrade: `found 0 vulnerabilities`.

---

## GHSA-x744-4wpc-v9h2 and four related Docker advisories

| | |
|---|---|
| Package | `github.com/docker/docker` (Go) |
| Was | `v28.5.2+incompatible` |
| Advisories | CVE-2026-34040, CVE-2026-33997, CVE-2026-41567, CVE-2026-41568, CVE-2026-42306 |
| Status | **Resolved by migrating off the module.** `github.com/docker/docker` is no longer in the dependency graph. |

### Why the module could not be upgraded

Dependabot advised upgrading to `29.3.1`. That version cannot be required,
because the module path was retired before v29:

- `github.com/docker/docker` has no `v29` tag. Its highest published version is
  `v28.5.2+incompatible` (`go list -m -versions github.com/docker/docker`).
- Upstream now tags releases `docker-v29.7.1` rather than `v29.7.1`, so Go's
  semver tag resolution finds nothing to select.
- `github.com/docker/docker/v29` resolves as a path but has zero versions.
- `go get github.com/docker/docker@v29.3.1+incompatible` fails with
  `invalid version: unknown revision v29.3.1`.

`govulncheck` agreed there was nothing to move to on that path: all three
advisories it surfaced reported `Fixed in: N/A`. The Go vulnerability database
shows the fixes were published under the renamed module `github.com/moby/moby/v2`
(`v2.0.0-beta.8` and `v2.0.0-beta.14`), never under `github.com/docker/docker`.

So the remediation is an import-path migration, not a version bump.

### What was done

Docker split the monolith at v29. HarborMaster now consumes the two public
consumer modules and nothing else:

| Old | New | Version |
|---|---|---|
| `github.com/docker/docker/client` | `github.com/moby/moby/client` | `v0.5.1` |
| `github.com/docker/docker/api/types/...` | `github.com/moby/moby/api/types/...` | `v1.55.0` |
| `github.com/docker/go-connections/nat` | `github.com/moby/moby/api/types/network` | (dropped as a direct dependency) |

`github.com/moby/moby/v2`, the engine monolith that carries the published fixes,
is deliberately **not** imported. It is not intended to be consumed as an
application library, and pulling it in would link daemon code into a read-only
observer — the opposite of what these advisories are about.

`github.com/containerd/errdefs` became a direct dependency: the moby client no
longer exports `IsErrNotFound`, and errdefs is how a 404 is now recognised.

### Reachability, before and after

All five advisories describe **daemon-side** code: AuthZ plugin dispatch,
`PUT /containers/{id}/archive` decompression, `docker cp` mount setup, and
plugin privilege comparison. HarborMaster only ever linked the client and API
type packages, and is read-only: it never calls the archive endpoints and never
installs plugins. The advisories were therefore never reachable in the
exploitable sense.

They were nonetheless *reported* against this code, because an advisory with no
fixed version and no narrowed symbol set matches the whole module. That is now
moot:

| | Before | After |
|---|---|---|
| `govulncheck ./...` | 3 vulnerabilities affecting the code, 2 more in required modules | **No vulnerabilities found** |
| `go list -deps ./...` | `github.com/docker/docker/...` present | absent |

### Breaking changes adapted

All confined to `internal/docker`:

- `Ping`, `ContainerList`, `NetworkList`, `VolumeList` take an options struct and
  return a result struct (`.Items`) instead of a bare slice.
- `ContainerInspectWithRaw` became `ContainerInspect` with
  `ContainerInspectResult{Container, Raw}` — same one-round-trip guarantee.
- `ImageInspect` returns a result wrapping `image.InspectResponse`.
- `Events` returns one `EventsResult` struct rather than two bare channels.
- `filters.Args` became `client.Filters`, whose zero value is read-only.
- `client.IsErrNotFound` is gone; `cerrdefs.IsNotFound` replaces it.
- `container.InspectResponse` flattened `ContainerJSONBase` away. "Partial
  record" is now detected by an empty ID rather than a nil embedded pointer.
- `container.Port` became `container.PortSummary`; `nat.Port`/`nat.PortSet`
  became `network.Port`/`network.PortSet`.
- Addresses became `netip.Addr` / `netip.Prefix`, and MAC addresses
  `network.HardwareAddr`. The invalid zero value is mapped back to `""` rather
  than rendered as the literal `"invalid IP"`.
- `network.Summary` now embeds `network.Network`.
- `events.Message` dropped the deprecated top-level `ID`. See the behaviour note
  below.

### Behaviour changes

Two, both unavoidable and both narrow:

1. **Legacy event actor fallback removed.** `convertEvent` used to fall back to
   the deprecated `Message.ID` when `Actor.ID` was empty. The field no longer
   exists on the typed message. `Actor.ID` has been populated since API 1.22,
   far below anything this client negotiates, so no supported daemon is
   affected. An actorless message still converts into a fingerprinted record.
2. **`kernelMemoryBytes` is always 0.** The moby API removed
   `HostConfig.KernelMemory`, which the kernel deprecated under cgroup v2. The
   domain field and the REST response shape are unchanged.

### Security validation performed

`gofmt`, `go vet ./...`, `go vet -tags integration ./...`, `go test ./...`,
`go test -race ./...` (clean over repeated runs), `go mod tidy`, `go mod verify`
(all modules verified), `govulncheck ./...` (clean), and a full CodeQL
`go-security-and-quality` run against an autobuilt database.

New guardrails in `internal/arch` fail the build if the retired module or the
engine monolith is imported anywhere, if the SDK escapes `internal/docker`, or
if the read-only runtime interface grows a mutation method. They are AST-based
rather than grep-based, so they cannot be fooled by an import alias and do not
match module paths mentioned in prose. Each was negative-tested — deliberately
violated to confirm it fails — before being kept.

New adapter tests pin the migrated error classification: a 404 maps to
`ErrContainerVanished` / `ErrImageUnavailable`, a 500 stays `ErrUnreachable`, and
engine detail never survives into what the API renders. That path changed
mechanism during the migration and nothing else covered it; had it broken, a
container vanishing mid-refresh would have failed the whole sweep instead of
producing one warning.

### A pre-existing bug found while getting CI green

Not caused by the migration, and recorded here because it was found by it.

`TestVolumeLifecycle` in the integration suite was failing on `main` before any
of this work — the same job, the same step, on commit `ff3ef0a`. The cause was
in `store.deleteMissing`, which returned early whenever the catalog to keep was
empty, on the reasoning that an empty list might have come from a failed read.

That reasoning does not hold for the paths that reach it. `RefreshVolumes` and
`RefreshNetworks` return early when the runtime read fails, so the catalog only
arrives here after a SUCCESSFUL read, and an empty one genuinely means the host
has none. The result was that removing a host's last volume left it in the
inventory until the next full reconciliation.

It hid for a structural reason: the same rule was applied to both catalogs, and
networks always have `bridge`, `host`, and `none`, so a network read is never
empty on a real daemon. Volumes have no such floor.

The fix makes the difference explicit rather than uniform — `emptyMeansEmpty`
for volumes, `emptyIsSuspect` for networks — so both catalogs get the rule that
is true of them. `ReplaceVolumes` and `ReplaceNetworks` had no unit tests at
all; they now have four, including a reproduction of the failure that needs no
Docker daemon.

### Remaining limitations

- **Docker-dependent gates were not run locally.** The live-Docker integration
  suite, the container image build, and the container smoke tests require a
  daemon, and none is installed on the machine this migration was performed on.
  CI runs all three. The binary smoke test that does not need Docker was run
  locally and passes.
- **`github.com/moby/moby/client` is pre-1.0** (`v0.5.1`). Its API may still
  change between minor versions, so upgrades should be read rather than taken
  blind. This is a supported, published consumer module — the pre-1.0 version is
  about API stability, not support status.
- **Four pre-existing `go/log-injection` findings** remain in `internal/api`
  (`middleware.go:66`, `middleware.go:93`, `response.go:94`, `static.go:74`).
  They are untouched by this change. Both `slog` handlers in use
  (`NewJSONHandler`, `NewTextHandler`) escape or quote attribute values, so a
  request path cannot forge a log record; the taint flow is real but the
  injection is not achievable. Recorded here rather than dismissed.

---

## GO-2026-5932 — golang.org/x/crypto/openpgp is unmaintained

| | |
|---|---|
| Package | `golang.org/x/crypto` (Go module) |
| Installed | v0.54.0 — **the latest release** |
| Affected package | `golang.org/x/crypto/openpgp` |
| Severity | UNKNOWN (GitHub renders it as **Note**) |
| Fixed version | **None, and there will not be one** |
| Reported by | Trivy, twice: once against `go.mod`, once against the binary |
| Status | **Accepted.** The package is not in the build. Ignored in `.trivyignore.yaml` with an expiry, and the non-use is enforced by two tests. |

### Why there is no version to upgrade to

The advisory is not "a release of this package broke something". It is *"this
package is unmaintained, unsafe by design, and has known security issues"* —
a statement about the package's existence. `openpgp` was frozen years ago and
the Go team's guidance is to use a maintained OpenPGP implementation instead.

There is therefore no `Fixed Version`, and `v0.54.0` is already the newest
release of the module. **No dependency change can clear this alert.**

### Why it does not apply to HarborMaster

HarborMaster requires `golang.org/x/crypto` for password hashing. The build
contains exactly two packages from it:

```
$ go list -deps ./... | grep '^golang.org/x/crypto/'
golang.org/x/crypto/blake2b
golang.org/x/crypto/argon2
```

Zero packages under `openpgp`. `govulncheck`, which resolves **symbols** rather
than modules, reaches the same conclusion:

```
$ govulncheck ./...
No vulnerabilities found.
...0 vulnerabilities in packages you import and 1 vulnerability in modules you
require, but your code doesn't appear to call these
```

That one uncalled module vulnerability is this advisory.

### Why Trivy reports it anyway

Because it cannot see what the build linked. A `go.mod` scan reads a manifest,
and a Go binary scan reads the embedded build info — **both list MODULES**. The
module is required, so the advisory applies to it, and nothing in either input
distinguishes `argon2` from `openpgp`.

This is not a Trivy defect. It is the granularity the data has, and it is why
`govulncheck` exists alongside it.

### What enforces the reasoning

An ignore justified by "we do not use that package" is worth exactly as much as
the check that we still do not. Two tests in `internal/arch`:

| Test | What it fails on |
|---|---|
| `TestNothingBannedIsAnywhereInTheBuild` | `golang.org/x/crypto/openpgp` entering the package set, first-party or transitive |
| `TestTheCryptoModuleIsUsedOnlyForWhatTheTriageSays` | a **third** package from this module being used, which would make the analysis above stale |

Both resolve the real package set with `go list -deps ./...` rather than
parsing imports, so a dependency that pulled `openpgp` in would be caught too.

The `.trivyignore.yaml` entry carries `expired_at: 2027-02-01`. When it lapses
the finding returns and this analysis gets redone — which is the point of an
expiry, and the reason the ignore is not simply a dismissal in the GitHub UI
where nobody would ever see it again.

### If this ever needs to change

If HarborMaster comes to need OpenPGP — verifying a signed artefact, say — do
**not** reach for this package. Remove the ignore, remove the test entry, and
pick a maintained implementation. The advisory would then be describing code
that actually runs.
