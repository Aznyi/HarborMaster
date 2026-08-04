# Task: migrate React Router 7 → 8

**Status:** done (2026-08-04)
**Driver:** GHSA-qwww-vcr4-c8h2 — see [../security-triage.md](../security-triage.md)

## Outcome

Done in one step, and it was much smaller than this task assumed.

The assumption here was a breaking major upgrade of `react-router-dom` from 7 to
8. That package has no v8: its latest release is 7.18.2, which depends on
`react-router` 7.18.2, so following it can never reach the patched version.
React Router consolidated in v8 and the DOM exports moved into `react-router`
itself.

So the change was a package swap, not an API migration:

- removed `react-router-dom`, added `react-router` `^8.3.0`
- rewrote `from "react-router-dom"` to `from "react-router"` in ten files

No routing code changed. Every symbol in use (`BrowserRouter`, `Routes`,
`Route`, `Navigate`, `Link`, `NavLink`, `useParams`, `useLocation`, and
`MemoryRouter` in tests) is exported from `react-router` v8 unchanged, and the
app stayed in Declarative Mode — Data Mode, Framework Mode, and RSC were not
adopted, so the advisory's blast radius is still not entered.

## Verified

- `npm run typecheck`, `npm test` (86 passing), `npm run build`
- `npm audit` — `found 0 vulnerabilities`
- The rebuilt bundle served through the Go binary: `/`, a deep link
  `/containers/<id>`, `/events`, and an unknown path all return the SPA shell
  and reference the new asset bundle
- `react-router` v8 requires `react >= 19.2.7`; 19.2.8 was already installed,
  so React did not move

## Worth remembering

When an advisory names a package you depend on transitively, check whether the
package you actually declare has a release that can reach the fix. Twice in this
repository the answer was no, and the remediation was to move to the successor
package rather than to raise a version:

- `github.com/docker/docker` → `github.com/moby/moby/{client,api}`
- `react-router-dom` → `react-router`
