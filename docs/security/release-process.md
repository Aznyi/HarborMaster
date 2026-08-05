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
| **CodeQL** (Go + TypeScript) | `codeql.yml` | Any new alert |
| **govulncheck** | `security.yml` | Any reachable advisory |
| **golangci-lint** | `security.yml` | Any finding |
| **Trivy** filesystem + config | `security.yml` | Fixable High/Critical |
| **Trivy** image | `security.yml` | Fixable High/Critical |
| **npm audit** (production) | `security.yml` | High or above |
| **Gitleaks** | `security.yml` | Any secret in history |
| **Dependency review** | `dependency-review.yml` | High/Critical advisory, or a denied licence |
| OpenAPI route coverage | `internal/api` tests | A route documented but not served, or served but not documented |
| Architecture invariants | `internal/arch` tests | SDK leak, or a new runtime method |

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
5. **Publish a GitHub release** with a semver tag.
6. `container.yml` then builds multi-arch, pushes to GHCR, and attaches an SBOM,
   in-toto provenance, and a keyless attestation.
7. **Verify the published artifact yourself** before announcing it, using the
   commands in §4. A release nobody verified is a release nobody can trust.

### Tagging policy

| Event | Tags written |
| --- | --- |
| Pull request | none — build only |
| Push to `main` | `edge`, `sha-<full-sha>` |
| Release published | `X.Y.Z`, `X.Y`, `X`, `latest` |
| Manual dispatch | `dispatch-<sha>`, only if explicitly requested |

`latest` moves on releases and nowhere else, so a push to `main` can never
replace the tag production is pulling.

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

```sh
# Provenance and signature — proves this image was built by this repository's
# workflow from this commit, and not by anyone else.
gh attestation verify oci://ghcr.io/aznyi/harbormaster:latest -R Aznyi/HarborMaster

# SBOM
docker buildx imagetools inspect ghcr.io/aznyi/harbormaster:latest \
  --format '{{ json .SBOM }}'

# Pin what you deploy to a digest, not a tag.
docker buildx imagetools inspect ghcr.io/aznyi/harbormaster:latest \
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
3. Move `latest` to the fixed release.
4. If the bad artifact is dangerous, mark the GHCR version deprecated and say so
   loudly in the release notes and the advisory.
5. Record what happened and what gate should have caught it. If no gate would
   have, add one.
