# Release Process

How a HarborMaster release is produced, what must be true before it ships, and
how an operator verifies what they received.

---

## 1. Release gates

A release is blocked until every one of these is green. They are not advisory.

| Gate | Where | Blocks on |
| --- | --- | --- |
| `gofmt` | `ci.yml` | Any unformatted file |
| `go vet` | `ci.yml` | Any finding |
| `go mod tidy` clean | `ci.yml` | A dirty `go.mod`/`go.sum` |
| `go test -race` | `ci.yml` | Any failure or data race |
| Integration tests | `ci.yml` | Any failure; leftover test resources |
| Frontend typecheck, test, build | `ci.yml` | Any failure |
| Binary smoke test | `ci.yml` | Health, version, SPA, or subcommand failure |
| **CodeQL** (Go + TypeScript) | `codeql.yml` | Any new alert not on the accepted list below |
| **govulncheck** | `security.yml` | Any reachable advisory |
| **golangci-lint** | `security.yml` | Any finding |
| **Trivy** filesystem + config | `security.yml` | Fixable High/Critical |
| **Trivy** image | `security.yml` | Fixable High/Critical |
| **npm audit** (production) | `security.yml` | High or above |
| **Gitleaks** | `security.yml` | Any secret in history |
| **Dependency review** | `dependency-review.yml` | High/Critical advisory, or a denied licence |
| OpenAPI route coverage | `internal/api` tests | A route documented but not served, or served but not documented |
| Architecture invariants | `internal/arch` tests | SDK leak, or a new runtime method |

### Reviewed and accepted CodeQL alerts

The gate is "any new alert". An alert that is genuinely not a defect is recorded
HERE, dismissed in the Security tab with the same reasoning, and re-reviewed
whenever the code around it changes. It is never suppressed by a repo-wide query
filter: that would hide the next genuine instance of the same rule.

| Rule | Location | Why it is accepted |
| --- | --- | --- |
| *(none)* | | |

**The table is empty, and it took a fix to get there.**

`go/cookie-secure-not-set` sat here for three phases with a paragraph of
justification: the flagged cookie carries an empty value and a negative
`Max-Age`, its only purpose is to delete the one that did carry a token, and it
could not be written with `Secure` unconditionally because a browser rejects a
Secure cookie set from an insecure origin — so on a plain-HTTP loopback
deployment the deletion would be silently dropped and the dead token would stay
in the browser.

Every sentence of that was true, and the conclusion was still wrong. A cookie's
identity is its NAME, DOMAIN, and PATH; `Secure` is a property of the cookie
being written, not part of what it matches. So a Secure deletion removes a
non-Secure cookie of the same name, and the deletion could simply follow the
CONNECTION: Secure over HTTPS, not over plain HTTP. The loopback case behaves
exactly as before and the HTTPS case got better.

The lesson is worth more than the fix. **A justification that survives review
is not the same as a justification that is the best available answer**, and a
row in this table is a standing invitation to stop looking. Prefer a fix.

A reviewer adding a row here is asserting that they read the query's intent and
the code, not that the alert was inconvenient. If the justification takes more
than a paragraph, the alert is probably a defect.

**No `--force`, no "we'll fix it after release".** A gate that can be waived is
not a gate. If something must ship with a known issue, it goes in the release
notes with a severity and a tracking issue, and the decision is recorded.

---

## 2. Producing a release

1. **Confirm `main` is green.** All workflows, not just CI.
2. **Review the dependency delta** since the last tag:
   `git diff <last-tag>..HEAD -- go.mod go.sum web/package-lock.json`
3. **Update the threat model** if a trust boundary moved.
4. **Draft release notes** including a Security section: fixes, advisories,
   and any accepted risk.
5. **Commit everything the tag must carry, and confirm the docs match what will
   exist.** See [Documentation is part of the artifact](#documentation-is-part-of-the-artifact).
6. **Publish a GitHub release** with a semver tag, and **tick "Set as a
   pre-release" for anything that is not stable**. That checkbox is what routes
   the image tags; see [Tagging policy](#tagging-policy).
7. `container.yml` then builds multi-arch, pushes to GHCR, and attaches an SBOM,
   in-toto provenance, and a keyless attestation.
8. **Verify the published artifact yourself** before announcing it, using the
   commands in §4. A release nobody verified is a release nobody can trust.

### Documentation is part of the artifact

Two failure modes, both of which produce a release that reads as finished and is
not. Neither is caught by any workflow, because both are about the gap between
what the documents claim and what the registry holds.

**Do not document a tag before it exists.** Version numbers land in the README,
the compose default, and the wiki while a release is still being prepared, and
until the release is published there is no image behind any of them. An operator
following those instructions gets:

```
docker: Error response from daemon: manifest unknown.
```

— which reads as a broken registry rather than as a release that has not
happened. Either publish before the documentation references the version, or
say plainly in the text that the tag is not yet available. `edge` is the tag
that always exists between releases.

**Everything the release body links to must be in the commit the tag captures.**
A GitHub release is created from a target branch at publication time, and links
into `blob/<tag>/…` resolve against that commit alone. Release notes written but
not committed, or committed after publishing, give a release whose own links
404 or serve a stale revision. Commit first, push, then publish.

Check both before ticking the box:

```sh
git status --short                                   # nothing uncommitted that the tag needs
git ls-tree -r --name-only HEAD | grep release-notes # the notes are actually in the commit
```

### Tagging policy

| Event | Tags written |
| --- | --- |
| Pull request | none — build only |
| Push to `main` | `edge`, `sha-<full-sha>` |
| Release published, **stable** | `X.Y.Z`, `X.Y`, `X`, `latest`, `sha-<full-sha>` |
| Release published, **prerelease** | `X.Y.Z-<pre>`, `beta`, `sha-<full-sha>` |
| Manual dispatch | `dispatch-<sha>`, only if explicitly requested |

A prerelease writes **no** `latest`, **no** `X.Y`, and **no** `X`. Publishing
`v0.9.0-beta.1` produces exactly `0.9.0-beta.1`, `beta`, and
`sha-<full-sha>` — never `latest`, `0.9`, or `0`.

`latest` moves on STABLE releases and nowhere else, so neither a push to `main`
nor a beta can replace the tag production is pulling.

#### The release must be marked as a prerelease

**This is the operator action the routing depends on.** The rules in
`container.yml` branch on `github.event.release.prerelease`, which is the
checkbox on the GitHub release form. A beta published with that box unticked is
treated as a stable release: it would take `latest`, `X.Y`, and `X`, and every
deployment following the documented Compose file would be upgraded onto a
prerelease without asking.

Nothing downstream can correct that afterwards — the tags are already written —
so the check belongs in the publication steps, not in review.

#### Why the version tags collapse

Two independent mechanisms, both in `container.yml`:

1. **`latest` and `beta` are explicit.** `flavor: latest=false` disables
   metadata-action's automatic `latest` entirely, so the only things that can
   write it are the two `type=raw` rules — one gated on
   `github.event.release.prerelease == false`, the other on `== true`.
2. **`X.Y` and `X` collapse on their own.** For a semver tag with a prerelease
   component, metadata-action recompiles every non-`{{raw}}` pattern as
   `{{version}}`. So all three `type=semver` lines — `{{version}}`,
   `{{major}}.{{minor}}`, and `{{major}}` — each yield the full prerelease
   version and deduplicate to one tag. `0.9` and `0` are not suppressed by a
   condition in this repository; they are never generated.

The second mechanism is behaviour of a pinned third-party action rather than of
this repository, so it is worth re-reading at
`docker/metadata-action@<pinned-sha>` `src/meta.ts` whenever that pin moves.
The first mechanism holds regardless.

---

## 3. Supply-chain controls

| Control | Implementation |
| --- | --- |
| Pinned actions | Every `uses:` is a commit SHA with the version in a comment |
| Least privilege | `contents: read` at workflow level; widened per job |
| No `pull_request_target` | Avoids the classic fork-PR secret-exfiltration pattern |
| No untrusted interpolation | No `github.event.*` text inside a `run:` block |
| Reproducible build | `-trimpath`, pinned Go and Node, `npm ci` |
| SBOM | SPDX, attached to the image index |
| Provenance | In-toto, `mode=max` |
| Signature | Keyless OIDC via `actions/attest-build-provenance` |
| Base images | Pinned tags; Dependabot `docker` ecosystem keeps them current |

---

## 4. Verifying a release

Verify the tag you actually published. `latest` exists only for stable
releases, so for a prerelease use the exact version — `0.9.0-beta.1` — or the
immutable `sha-<full-sha>`.

```sh
# The tag under verification. For a stable release this may be `latest`.
TAG=0.9.0-beta.1

# Provenance and signature — proves this image was built by this repository's
# workflow from this commit, and not by anyone else.
gh attestation verify "oci://ghcr.io/aznyi/harbormaster:$TAG" -R Aznyi/HarborMaster

# SBOM
docker buildx imagetools inspect "ghcr.io/aznyi/harbormaster:$TAG" \
  --format '{{ json .SBOM }}'

# Pin what you deploy to a digest, not a tag.
docker buildx imagetools inspect "ghcr.io/aznyi/harbormaster:$TAG" \
  --format '{{ .Manifest.Digest }}'
```

Deploy the digest. A tag is mutable; a digest is not.

---

## 5. Security releases

A vulnerability fix follows a different path.

1. Work in a **private fork or a draft security advisory**, never a public
   branch. A public commit is disclosure.
2. Fix, plus a regression test that fails without the fix.
3. Run every gate against the private branch.
4. Request a CVE through GitHub if the issue is operator-facing.
5. Publish fix, advisory, and release together — never the fix first.
6. Credit the reporter unless they decline.

**Timelines:** Critical/High within 30 days of a confirmed report; Medium/Low in
the next scheduled release. Targets, not guarantees — communicated if they slip.

---

## 6. Branch protection

Recommended settings for `main`. These are **not** enforced by anything in this
repository — they are repository settings an owner applies, and they are listed
here so their absence is a visible decision rather than an oversight.

- Require a pull request before merging.
- Require at least one approving review.
- Dismiss stale approvals on new commits.
- **Require these status checks:** `Backend`, `Frontend`, `Build binary`,
  `Container image`, `Analyze (go)`, `Analyze (javascript-typescript)`,
  `Go vulnerabilities`, `Go lint`, `Filesystem and config scan`,
  `Container image scan`, `Frontend dependencies`, `Secret scan`,
  `Review dependency changes`.
- Require branches to be up to date before merging.
- Require conversation resolution.
- Require signed commits.
- Include administrators. A gate that the owner can walk past is not a gate.
- Restrict force pushes and deletions.
- Enable Dependabot alerts, security updates, and secret scanning with push
  protection.

---

## 7. If a release goes wrong

1. **Do not delete the tag.** Anyone who pulled it has it; deletion only removes
   the evidence.
2. Publish a fixed patch release.
3. Move the channel tag to the fixed release — `latest` for a stable release,
   `beta` for a prerelease. Publishing the fix with the correct prerelease
   setting does this; there is no manual retag.
4. If the bad artifact is dangerous, mark the GHCR version deprecated and say so
   loudly in the release notes and the advisory.
5. Record what happened and what gate should have caught it. If no gate would
   have, add one.
