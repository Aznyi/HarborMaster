import { Link, useLocation } from "react-router";

import { PageIntro } from "../components/PageIntro";

/**
 * The page for an address HarborMaster does not have.
 *
 * # Why this replaced a redirect
 *
 * The wildcard route used to send every unknown path to the dashboard. That is
 * the friendliest possible failure and it hid real defects: Batch A found three
 * links pointing at `/automation/policies`, a route that has never existed.
 * Every one of them "worked" -- the operator pressed a link, landed on a
 * plausible page, and had no reason to think anything had gone wrong. The bug
 * was invisible precisely because the recovery was silent.
 *
 * # The URL is deliberately left alone
 *
 * Rendering rather than redirecting keeps the mistyped address in the address
 * bar, which is the whole point:
 *
 *   - an operator can SEE which path failed, and correct it;
 *   - a screenshot or a support log stays truthful about where somebody was;
 *   - a bookmark to a dead link does not quietly become a dashboard bookmark;
 *   - an automated check can tell a broken internal link from a working one.
 *
 * # What it does not do
 *
 * It does not echo the path back into the page. The address bar already shows
 * it, so printing it again would add nothing except a place where a crafted URL
 * becomes page content. It reaches no API and reports no route table: a visitor
 * learns that this address is not a page, and nothing about which addresses are.
 */
export function NotFound() {
  const location = useLocation();

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Page not found"
        description="HarborMaster does not have a page at this address. The address bar shows the one that was requested."
      />

      <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <p className="max-w-prose text-sm text-content-muted">
          If a link inside HarborMaster brought you here, that is a defect worth
          reporting &mdash; every in-app link is meant to resolve to a real page.
          The sidebar still lists everywhere you can go.
        </p>

        <div className="mt-4 flex flex-wrap gap-3">
          <Link
            to="/"
            className="inline-flex min-h-11 items-center rounded-lg bg-accent px-4 py-2 text-sm font-medium text-surface"
          >
            Go to Dashboard
          </Link>
          {/*
            `-1` rather than a computed destination. React Router's navigate(-1)
            is the browser's own Back, so it cannot be pointed anywhere: there is
            no attacker-supplied value in it and no open-redirect shape. Rendered
            as a real button because it performs an action rather than naming a
            destination -- a link would put an href on it that no href could
            honestly hold.
          */}
          <BackButton />
        </div>
      </section>

      {/*
        A stable hook for the end-to-end checks, carrying the pathname the
        router matched. Not rendered, so nothing user-controlled reaches the
        page text; `data-*` values are escaped as attributes.
      */}
      <span hidden data-testid="not-found-path" data-path={location.pathname} />
    </div>
  );
}

/** Browser Back, using the router's own history. */
function BackButton() {
  return (
    <button
      type="button"
      onClick={() => window.history.back()}
      className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle px-4 py-2 text-sm font-medium"
    >
      Go back
    </button>
  );
}
