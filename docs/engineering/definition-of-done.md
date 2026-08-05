# Definition of Done

A change is done when every box below is ticked. Not when it works — when it is
finished.

This applies to every feature, fix, and refactor in HarborMaster, including
changes that look too small to need it. "Too small to need the checklist" is
where most of the defects in most projects come from.

---

## 1. Correctness

- [ ] The change does what the issue or pull request says it does.
- [ ] Edge cases are handled: empty input, absent input, malformed input,
      concurrent access, cancellation.
- [ ] Errors are handled, not swallowed. Every ignored error is written as
      `_ = f()` with a comment saying why ignoring it is correct.
- [ ] No `TODO`, `FIXME`, or commented-out code is left behind.

## 2. Security

**Zero tolerance items — a change with any of these does not merge:**

- [ ] **Zero new CodeQL alerts.**
- [ ] **Zero govulncheck findings.**
- [ ] **No High or Critical dependency vulnerability.**
- [ ] **No new Docker mutation capability**, unless the phase explicitly adds it.
- [ ] **No secret value persisted, logged, or returned.**

**Review items:**

- [ ] Untrusted input validated against a closed vocabulary or an allowlist.
- [ ] Every SQL value bound; every identifier from an allowlist.
- [ ] Every new dimension bounded — size, count, concurrency, time.
- [ ] Errors sanitised before crossing a boundary.
- [ ] Secure defaults; failure modes fail closed.
- [ ] [Secure coding standards](../security/secure-coding.md) followed.
- [ ] [Threat model](../security/threat-model.md) reviewed if a trust boundary,
      an entry point, or an asset changed.

## 3. Tests

- [ ] New behaviour has tests. Bug fixes have a regression test that fails
      without the fix.
- [ ] **Every security control has a negative test.** A control with only a happy
      path has not been verified.
- [ ] Concurrent code has a `-race` test.
- [ ] Any test that sweeps for something (a leaked secret, a forbidden pattern)
      has a positive control proving the sweep can find what it looks for.
- [ ] Frontend changes have component tests covering loading, ready,
      disconnected, and error states.

## 4. Documentation

- [ ] **OpenAPI updated** if a route, parameter, or schema changed. The route
      coverage test enforces this.
- [ ] Doc comments explain *why*, not *what*. The code says what it does; the
      comment says why it does it that way.
- [ ] `.env.example` updated for a new setting, with its security implications.
- [ ] README updated if operator-visible behaviour changed.
- [ ] Architecture or threat model updated if the shape of the system changed.
- [ ] A deliberate limitation is documented as a limitation, not left to be
      discovered.

## 5. Verification

Run locally before pushing. CI runs them again; CI is the backstop, not the
first attempt.

```sh
gofmt -l .                  # empty
go vet ./...
go test ./...
go test -race ./...         # needs cgo
golangci-lint run
govulncheck ./...

cd web
npm run typecheck
npm test
npm run build
npm audit --omit=dev
```

- [ ] All of the above pass, or what could not run locally is stated explicitly
      in the pull request with the reason.

## 6. Review

- [ ] Self-reviewed the diff before requesting review. Read it as a reviewer
      would, not as the author.
- [ ] At least one approving review.
- [ ] Architecture reviewed if a layer boundary, an interface, or a data model
      changed.
- [ ] Security reviewed if the change touches input handling, secrets, SQL,
      Docker, or CI.
- [ ] Every conversation resolved.

---

## Escalation

Some changes need more than a normal review. Treat these as architecture-review
triggers:

| Change | Requires |
| --- | --- |
| A new `docker.Runtime` method | Explicit design discussion; two reviewers |
| Any Docker mutation capability | A phase that authorises it; threat model update |
| A change to secret handling | Security review; threat model update |
| A new external dependency | Justification in the PR: purpose, maintenance, licence, transitive cost |
| A new network listener or protocol | Threat model update |
| A new trust boundary | Threat model update; architecture document update |
| Loosening a security control | An explicit, recorded decision — never a side effect |

---

## What "done" is not

- Not "it works on my machine".
- Not "tests pass" — tests passing is one line on this list.
- Not "I'll document it later".
- Not "the scanner finding is probably a false positive". Investigate it, then
  dismiss it *with a reason*, in the Security tab where the reason is visible.
- Not "it's only a small change". The size of a diff is unrelated to the size of
  its consequences.
