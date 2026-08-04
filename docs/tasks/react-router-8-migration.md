# Task: migrate React Router 7 → 8

**Status:** open
**Opened:** 2026-08-04
**Driver:** GHSA-qwww-vcr4-c8h2 (see [../security-triage.md](../security-triage.md))
**Priority:** low — the advisory is not reachable from this codebase. This is
hygiene and alert-clearing, not incident response.

## Why this is its own task

`react-router` 8.3.0 is the patched version, and reaching it means moving
`react-router-dom` from `^7.6.0` to `^8`. That is a breaking major upgrade
across every routing surface in the app, so it does not belong in a patch sweep
and must not ride along with an unrelated change. `npm audit fix` cannot do it
without `--force`.

Because the vulnerable feature (RSC mode) is not used here, there is no reason
to rush this or to accept a partly-verified upgrade.

## Scope

Routing is small and centralised, which is the main thing in this migration's
favour:

- [web/src/main.tsx](../../web/src/main.tsx) — `<BrowserRouter>` mount
- [web/src/App.tsx](../../web/src/App.tsx) — `<Routes>` / `<Route>` / `<Navigate>`
- [web/src/components/AppShell.tsx](../../web/src/components/AppShell.tsx) — `NavLink`, `useLocation`
- [web/src/pages/ContainerDetail.tsx](../../web/src/pages/ContainerDetail.tsx) — `Link`, `useParams`
- [web/src/pages/Dashboard.tsx](../../web/src/pages/Dashboard.tsx), [Containers.tsx](../../web/src/pages/Containers.tsx), [Events.tsx](../../web/src/pages/Events.tsx) — `Link`
- Tests using `MemoryRouter`: `App.test.tsx`, `pages/Events.test.tsx`,
  `pages/Inventory.test.tsx`

## Steps

1. Read the React Router 8 upgrade guide and note which v7 APIs used above were
   removed or renamed.
2. Decide whether v8 still ships `react-router-dom` as a separate package or
   whether imports move to `react-router`. Update imports accordingly.
3. Upgrade: `npm install react-router-dom@^8` in `web/`, commit the lockfile.
4. Keep Declarative Mode. Do **not** adopt Data Mode, Framework Mode, or RSC as
   part of this change — that is a separate design decision, and adopting RSC
   would move this app into the advisory's blast radius for the first time.
5. Update `MemoryRouter` usage in tests if its API changed.
6. Verify, all from `web/`:
   - `npm run typecheck`
   - `npm test`
   - `npm run build`
   - `npm audit` — expect 0 advisories for `react-router`
7. Confirm the embedded bundle still serves: build the binary and check the SPA
   shell and client-side routing, including a deep link such as
   `/containers/<id>` and the catch-all redirect to `/`.

## Done when

- `react-router` resolves to >= 8.3.0 in `web/package-lock.json`
- `npm audit` is clean
- Type-check, tests, and build all pass
- Deep links and the catch-all redirect still work against the embedded bundle
- Dependabot alert #6 closes on its own
- The React Router entry in [../security-triage.md](../security-triage.md) is
  updated to resolved
