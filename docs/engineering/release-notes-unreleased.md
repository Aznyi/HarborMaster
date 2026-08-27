# Unreleased

Notes for the next release. Each entry is written when the change lands, so the
release itself is an editing job rather than an archaeology one.

## Behaviour corrections

### A republished tag is no longer reported as "unknown" on large repositories

**What changed.** HarborMaster asks a registry two independent questions about
an image: does the tag you configured still resolve to the digest this container
is running, and does a newer version tag exist anywhere in the repository. The
first is answered by a single lookup and is always available. The second needs a
tag listing, which is bounded by
`HARBORMASTER_IMAGE_INTEL_MAX_TAG_PAGES` so that one image cannot make
HarborMaster walk a repository of unbounded size.

Until now, a bounded listing that ran out of budget **discarded the answer to
the first question**. An image on a versioned tag — `nginx:1.27.4`,
`redis:7.2.5` — whose publisher had genuinely republished that tag was reported
as `unknown` rather than as a digest update, because the search for something
else had not finished.

`unknown` is refused by every update strategy, by design: HarborMaster does not
automate a change whose size it could not establish. So the practical effect was
that these containers were never updated, and never would be — a repository does
not get smaller.

Measured against Docker Hub, both `library/nginx` and `library/redis` exceed the
default budget on **every** check, so any versioned tag in either repository was
affected.

HarborMaster now keeps the two questions apart. A digest movement it has
positively established on the configured tag is reported as a digest update even
when version discovery could not finish, and it says so:

> the registry serves a different digest for this tag; the publisher has
> republished it. Version discovery also reached the configured registry search
> limit, so a newer version tag may exist beyond what was read

**What this means for an existing policy.** If you already run an automatic
policy with the `digestOnly` strategy over an image on a versioned tag,
HarborMaster may now perform updates it previously reported as unknown and
skipped. That is the correction working, and it is worth knowing about before
you next look at the automation page.

Nothing about your policy changed. Its scope, its selector, its strategy, its
mode, its maintenance window, and its limits are exactly as you wrote them, and
the only thing that moved is the accuracy of the assessment feeding them. Every
gate still runs in full: the container must still be eligible, the change must
still be within the strategy ceiling, the planner's recommendation must still
satisfy the policy's minimum, the window must still be open, the acquisition
preflight must still re-verify the digest against the registry, and the
execution preflight must still re-check the container, its snapshot, its policy
compliance, and its dependencies immediately before anything is stopped.

If you would rather review these updates before they happen, set the policy's
mode to `approvalRequired` or `observe`.

**What did not change.** No registry safety bound moved. The page budget, the
per-page and total tag caps, the response-size limit, the request timeout, the
retry policy, the registry allowlist, the SSRF and TLS controls, and the
requirement that every acquisition is pinned to a digest are all exactly as they
were. The fix is a correction to how HarborMaster reads evidence it had already
gathered, not permission to gather more.

### A search that could not finish still says so

Two states that both used to render as "unknown" are now distinguishable.

When version discovery hits its limit and the configured tag's digest has **not**
moved, HarborMaster still reports that it does not know — an unfinished search
cannot establish that no newer version exists, and reporting "up to date" would
be a claim it cannot support. The wording now names the cause:

> HarborMaster could not finish version discovery within the configured registry
> search limit, so it cannot determine whether a newer version tag exists

This is a bound working as intended, not an error, and no registry response text
is shown.
