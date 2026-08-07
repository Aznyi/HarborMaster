# Security Policy

HarborMaster reads a Docker socket. Access to that socket is equivalent to root
on the host. Please read [Threat model](docs/security/threat-model.md) before
deploying, and the section below before reporting an issue.

## Supported versions

HarborMaster is pre-1.0. Only the latest release receives security fixes.

| Version | Supported |
| --- | --- |
| Latest release | Yes |
| `main` (unreleased) | Yes, best effort |
| Anything older | No |

**HarborMaster is in beta.** The security properties below are tested and
enforced, and the ones that matter most are enforced by architecture tests
rather than by review. What beta means here is that the software has not yet
been run by many people on many hosts — not that the guarantees are
provisional. A report that contradicts one of them is a vulnerability report,
and will be treated as one.

## Reporting a vulnerability

**Do not open a public issue for a security problem.**

Report privately through GitHub's
[private vulnerability reporting](https://github.com/Aznyi/HarborMaster/security/advisories/new).
That opens a draft advisory visible only to the maintainers, which is the right
place for details, a proof of concept, and the eventual fix.

If private reporting is unavailable to you, email the maintainer listed on the
repository profile with `HarborMaster security` in the subject and *no* technical
detail in the body — we will move the conversation somewhere private first.

### What to include

- What you did, what happened, and what you expected.
- The version, commit, or image digest you tested.
- How HarborMaster was deployed: bare binary, container, behind a proxy.
- A proof of concept, if you have one. A curl command is ideal.
- Your assessment of impact, even if rough.

### What to expect

| Stage | Target |
| --- | --- |
| Acknowledgement | 3 business days |
| Initial assessment with a severity | 10 business days |
| Fix or documented mitigation for Critical/High | 30 days |
| Fix or documented mitigation for Medium/Low | Next release |
| Public advisory | On release of the fix, or by agreement |

This is a volunteer-maintained project. These are targets, not contractual
guarantees, and we will tell you if something is going to slip.

### Disclosure

Coordinated disclosure. We will credit you in the advisory unless you ask us not
to. Please give us the timelines above before publishing; if you believe an
issue is being actively exploited, say so and we will treat it as an emergency.

## What counts as a vulnerability

**In scope:**

- Any path that lets HarborMaster create, modify, start, stop, remove, or exec
  into a container. HarborMaster has exactly ONE Docker write capability --
  downloading an approved, digest-pinned image into the local image store -- and
  it touches no container. A break in that limit is the most serious class of
  bug this project has.
- Any path that causes an image to be downloaded without a current change plan
  that recommends it, or that lets a caller choose the registry, repository, or
  digest. The target is derived server-side and revalidated immediately before
  the transfer; a way around either is in scope.
- Any path that reports an image as acquired without the post-transfer digest
  and platform verification having passed.
- Disclosure of a secret: an environment variable value, a registry credential,
  a token, a private key, or the snapshot HMAC key, through the API, the UI, the
  database, the event stream, or a log record.
- SQL injection, command injection, path traversal, SSRF, XSS, or response
  splitting.
- Remote crash, unbounded resource consumption, or any unauthenticated request
  that can render the service unavailable.
- Container escape or privilege escalation from the HarborMaster container.
- Supply-chain compromise: a malicious dependency, a workflow that can be made
  to execute attacker-controlled code, or a release artifact that cannot be
  verified.

**Out of scope — known and documented, not bugs:**

- **HarborMaster does not terminate TLS.** It authenticates every route, but the
  transport is a proxy's job. "The session cookie travels in the clear on a
  plain-HTTP deployment" is documented behaviour, not a finding. Bind to
  loopback or put a TLS-terminating proxy in front of it.
- **The four public routes are public by design.** `GET /health` (reduced to a
  single status field for an anonymous caller), `GET /version`,
  `POST /auth/login`, and `GET /auth/bootstrap`. Each has a stated reason in
  `internal/api/routes.go`, and a test fails the build if a fifth appears.
- **There is no second authentication factor**, no SSO, and no password reset by
  email. Accounts are local and recovery is another administrator or the
  console. See [Threat model](docs/security/threat-model.md) R33 and R38.
- **The bootstrap token is printed to the server's log.** Anyone who can read
  the startup output of an *unclaimed* installation can claim it. That is the
  design — the alternative is a default account — and the window closes
  permanently the moment an administrator exists. See R35.
- **The Docker socket is root-equivalent.** Mounting it into any container,
  including this one, grants host control. The `:ro` flag on the bind mount does
  not change that.
- Findings that require an attacker to already have root on the host, or to
  already be able to write to the Docker socket directly.
- Missing security headers on a deployment that has replaced them at a proxy.
- Vulnerabilities in a dependency with no reachable call path — please still
  tell us, but they are handled as maintenance rather than as advisories.
- Reports generated by a scanner with no analysis attached.

## Security properties HarborMaster claims

These are the guarantees a report can meaningfully contradict:

1. **Read-only with respect to Docker.** The runtime adapter exposes seven
   methods, all observational. An architecture test fails the build if an
   eighth appears or if a method name begins with a mutating verb.
2. **The Docker SDK is confined to `internal/docker`.** Enforced by the same
   architecture test.
3. **No secret value is persisted.** Sensitive environment values and log-driver
   options are stored as keyed HMAC digests. Plaintext exists only in memory
   during capture. Enforced by tests that sweep every column of every table.
4. **No secret reaches the API, the UI, or a log.** Digest fields are
   unserialisable by construction.
5. **Every response carries a strict Content-Security-Policy** with no
   `unsafe-inline` and no remote origins.
6. **Every write endpoint is bounded**: strict JSON decoding, body limits, rate
   limiting, and concurrency limits.
7. **Every route requires a session except four.** The exceptions are listed
   above. `TestOnlyTheFourPublicRoutesAnswerWithoutASession` walks the real
   route table and fails the build if anything else answers anonymously.
8. **Default deny is a property of the type system.** Every route declares an
   access policy whose zero value is invalid, so a route registered without one
   is refused at runtime and fails `TestEveryRouteDeclaresAnAccessPolicy`.
9. **Authorization is decided in one place.** No handler compares a role;
   `TestNoHandlerChecksARoleDirectly` fails the build if one does.
10. **No password, session token, or CSRF token is stored, logged, or returned.**
    Passwords are Argon2id verifiers; session and bootstrap tokens are keyed
    digests; the CSRF token is derived and stored nowhere.
    `TestNoAuditRowEverContainsASecret` sweeps a real recorded audit log for
    every secret that was in scope during a bootstrap, a sign-in, an account
    creation, and a password change.
11. **Every state-changing request is attributed.** `TestEveryWriteRouteIsAudited`
    resolves each write route to its handler's source and fails if it records no
    actor.
12. **A forwarding header is never believed** unless a trusted-proxy range is
    configured and the peer is inside it.
13. **HarborMaster cannot update itself, and no setting permits it.** The
    refusal is enforced at four independent layers — the automation decision,
    the approval path, the acquisition preflight, and the execution preflight.
    `TestEverySelfUpdateRefusalSiteIsPresent` pins all four,
    `TestTheSelfUpdateRefusalCannotBeConfiguredAway` fails the build on a
    configuration flag that would disable one, and
    `TestTheSelfIdentityIsWiredIntoEveryServiceThatRefuses` fails it if the
    composition root stops supplying the identity those layers consult.
14. **A notification cannot carry a secret.** There is nowhere in the type to
    put an environment value, a registry credential, a session token, or a raw
    Docker error. Every notification's wording lives in one file;
    `TestEveryNotificationIsWrittenInOnePlace` fails the build if another file
    composes one, and `TestNoNotificationCarriesAnErrorsText` fails it if that
    file interpolates a format verb or an error's text.
15. **A notification destination's credential never leaves the database.** A
    webhook URL and an SMTP password live in a separate type in a separate
    table, reachable through exactly one repository method and returned by no
    endpoint. `TestACredentialIsOnlyEverConstructedOutsideTheTrustedPackages`
    fails the build if the type appears anywhere it could travel outward, and
    archiving a destination destroys its stored credential.
16. **A notification destination cannot become an SSRF primitive.** The URL is
    validated when stored — HTTPS only, a hostname rather than an IP literal,
    no userinfo — and the RESOLVED ADDRESS is re-checked at dial time, which is
    what defeats DNS rebinding. Redirects are refused, no proxy is consulted,
    the response body is bounded and discarded, and link-local, multicast,
    CGNAT, benchmarking ranges, and `169.254.169.254` are refused whatever the
    private-destination setting says.

## Hardening checklist for operators

- Bind to `127.0.0.1` unless a TLS-terminating reverse proxy sits in front.
- Behind a proxy, set `HARBORMASTER_COOKIE_SECURE=true` and set
  `HARBORMASTER_TRUSTED_PROXIES` to the proxy's ranges and nothing wider.
- Claim the installation immediately after first start. An unclaimed one is one
  bootstrap token away from being somebody else's.
- Give each operator their own account. A shared account makes the audit log
  answer "somebody" rather than "who".
- Grant `viewer` unless the person needs to act. Only `operator` and above can
  download an image or replace a container.
- Run the published image, which is distroless and runs as UID 65532.
- Keep `read_only: true`, `cap_drop: [ALL]`, and `no-new-privileges:true`.
- Supply the snapshot HMAC key via `HARBORMASTER_SNAPSHOT_HMAC_KEY_FILE` and a
  Docker secret, not via an environment variable.
- Back up the HMAC key. Losing it makes historical secret digests uncomparable,
  and HarborMaster will refuse to start rather than silently regenerate it.
- Verify the image before running it:

  ```sh
  gh attestation verify oci://ghcr.io/aznyi/harbormaster:latest -R Aznyi/HarborMaster
  ```
