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

### A republished tag is now applied even when a newer tag exists

When a container followed `nginx:1.27.4` and the publisher republished that
exact tag — a security rebuild, most often — HarborMaster proposed moving to
`nginx:1.28` instead, because a newer tag in the series always won. The
republished digest of the tag the container actually follows was known on every
pass and never offered.

For a policy that permits version movement this was merely the larger step
taken first. For a policy that permits a digest move and nothing else it meant
**no update ever happened**: the proposal was always a version change, and a
digest-only ceiling refuses those. Any container not already on the newest tag
in its series silently stopped receiving the content of the tag it was told to
follow.

That is now ordered the other way. A container behind on its own tag follows its
own tag first; once it is current there, a newer tag is proposed exactly as
before. Nothing is lost — the version move happens on the following pass — and
each step is smaller and separately verified.

This is what makes **Follow current tag** mean what it says, and it is the
difference between HarborMaster being able to replace Watchtower and not.

Two guards were added with it. A container already running the newest tag's
content is never moved backwards onto an older digest — a real hazard in the
window after an update, while a container runs the new content but still tracks
the old tag. And the acquisition preflight no longer re-derives the planner's
choice to check it: it asks which images the registry is currently serving and
requires the plan to name one of them. Deriving the choice twice meant the
planner and the check could disagree about a registry neither had misread, and
refuse every acquisition with "the digest on offer has changed".

### A container is no longer paused because its update was already running

A policy that pauses after N failures counted every refusal from the recreation
preflight, including the one that means *another recreation is already running
for this container*.

That refusal is not an update that failed — it is the same update, in progress,
under a different decision. Counting it could pause a workload while its update
was succeeding, and the paused list then reported the concurrency clash as the
reason, hiding whatever the real outcome turned out to be. On a deployment that
pauses after one failure, an ordinary overlap between a scheduled pass and a
manually triggered one was enough.

The redundant decision is now settled rather than counted, so it neither counts
against the container nor returns on the next tick to submit a second
recreation. Every other refusal still counts.

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

## New

### Update policies can be configured by outcome

The update policy editor now opens on a short list of outcomes rather than on
the full set of controls:

| Preset | What it does |
| --- | --- |
| Observe only | Evaluates matching containers and changes nothing |
| Keep containers safely updated | Applies patch versions and republished tags |
| Follow current tag | Stays on each configured tag, moving only when its digest changes |
| Allow minor version updates | Adds minor versions; major versions are still held for a person |
| Custom | Every control, as before |

**A preset is a configuration shortcut and nothing more.** It writes a few
fields of an ordinary policy and gets out of the way. There is no preset field
in the API, no preset column in the database, and no code path in HarborMaster
that reads one — after you save, a policy that came from a preset is
indistinguishable from one typed by hand, and behaves identically. Every gate
still runs in full.

Choosing an outcome writes four fields — the update ceiling, the mode, the
recommendation floor, and automatic rollback. Everything that decides *which*
containers and *when* stays yours: the name, the scope, the selector, the
exclusions, the maintenance window, the priority, and the pause-after-failures
setting all survive switching between outcomes. Changing one of the four fields
by hand moves the policy to Custom, because it is no longer the outcome its
label claims.

### Presets act on updates the planner flagged for caution

Each acting preset carries a recommendation floor of `proceedWithCaution`
rather than the stricter `proceed`, and the editor says so before you save.

This is deliberate and was measured rather than chosen. HarborMaster's risk
model reaches its `medium` band on *caution* factors alone — a republished
digest scores 12, a mutable tag such as `latest` or `stable` scores another 12,
and an image published within the last 48 hours scores 8. None of those says the
change is unsafe, but together they total 32, which is `proceedWithCaution`. At
the stricter floor a policy would refuse exactly the workload the "Follow
current tag" preset exists to serve, and — because the freshness factor ages out
after two days — would then start working later with no explanation.

**Nothing that needs a person was widened.** The floor admits one additional
verdict, the one meaning "probably fine, but something is worth reading first".
Every warning-severity finding still produces `manualReview`, which no policy
may automate: a major version change, open configuration drift, a failed restore
readiness check, a missing configuration snapshot, a platform mismatch, and an
unresolved critical policy violation are all still held for a person.

**What this means for a policy you already have.** Nothing changes about it. A
stored policy keeps every value it was saved with, and HarborMaster will not
rewrite one because you opened it.

Policies written before this release did not send a recommendation floor, so
they were stored with `proceed`. Those policies will show as **Custom** in the
new editor rather than under a preset name, because they genuinely differ from
the preset: they refuse the republished-tag updates the preset performs. If you
want the preset's behaviour, select it and save; if you prefer the stricter
floor, leave the policy alone and it will keep working exactly as it does today.

### The policy editor says how many containers a policy could act on

Configuring an update policy no longer means guessing. As you describe a policy,
the editor reports how many containers HarborMaster could act on right now, and
groups the rest by the reason it would not:

> **Based on current assessment**
> 3 containers are currently eligible.
> 6 · the current change plan proposes no change
> 2 · something it depends on is being updated first

**The number comes from the real decision code.** The estate is evaluated by the
same two phases a scheduled pass runs — the decision function and the dependency
gate — so the count cannot drift from what automation will actually do. There is
no second eligibility model, in the browser or on the server.

**Nothing is changed to produce it.** No policy is stored, no pass is recorded,
no image is downloaded, no container is touched, and no registry is contacted.
The preview reads assessment HarborMaster already holds.

**It is a reading, not a promise.** The count describes the estate as currently
assessed and can change a second later — an image is republished, a pass takes a
container, someone pauses one. Every safety check still runs again before
anything is replaced, so a container that is counted here can still be refused
later, and that is the system working.

Previewing an unsaved policy is supported, so you can see what an outcome would
do before committing to it. Previewing an edit to a stored policy measures the
edit, and leaves the stored policy exactly as it is.

### Two corrections to what the automation preview reported

The "what the next pass would do" preview had two defects, both found while
building the readiness surface above, and both in the direction that overstated
what automation would handle.

It did not apply the dependency gate. A container held because something it
depends on has to be updated first was reported as though automation would
update it. It now runs the same two phases a real pass runs, so the preview and
the pass cannot disagree.

It also did not say when the estate had been cut at the per-pass limit. On a host
with more containers than one pass may consider, the preview answered for part of
the estate with nothing indicating it. It now reports that explicitly, and the
readiness panel says so in as many words.

### An update that needs a person can now be approved

HarborMaster sometimes assesses an update as needing human review. Until now
that was the end of the road: the image could be downloaded, the container was
never replaced, and the refusal said why — *"the review has not happened"* —
without offering any way for it to happen. Every major version update reached
this, so approving one from the automation page downloaded an image and then
did nothing.

The change plan page now shows **Manual review required** for such an update,
with the current and proposed images and digests, the score, and the reasoning
in plain English. An operator can record that they reviewed it:

> **Approve this exact update**

**Approving does not change anything.** No image is downloaded and no container
is replaced. The operator then applies the update through the ordinary
download-and-recreate flow, and every safety check runs again at that point.

**It does not change the assessment either.** The risk score, the risk factors
and the recommendation stay exactly as the planner wrote them. The approval is
recorded next to the plan, not on it, so "the model rated this high risk" and
"a named person approved this exact digest" remain separately answerable in the
audit log.

**An approval covers one plan and one digest.** If the evidence moves — a new
image is published, a snapshot changes, the policy changes — the planner writes
a new plan, and the earlier approval does not carry over. Withdrawing an
approval is possible at any time before the update is applied.

**Nothing else is waived.** An approved update is still refused if it would
touch HarborMaster's own container, if the configuration snapshot has changed
since the plan was assessed, if a dependency is not ready, if the container no
longer matches the plan, if the inventory is stale, or if any other check fails.
The approval satisfies one requirement — that the review happened — and nothing
more.

**Automation does not use these approvals.** Approving a plan does not let the
update engine act on it unattended; applying it remains a deliberate manual
step. Approving also works with the update engine switched off, because it is
not automation.

Approving needs the new `plan:approve` permission, which operators and
administrators hold. A viewer cannot approve anything.

### Pending approvals no longer offer an action that cannot work

An automation decision held for a person whose plan needs manual review used to
show an Approve button. Releasing it downloaded an image and the recreation was
then refused. That row now says **Manual review required** and links to the
plan, which is where the decision can actually be made.

### A fresh installation says what it is waiting for

An installation with nothing configured looked identical to one that had
assessed everything and found nothing to do: an automation page with no rows.
Several genuinely different situations rendered the same way, and each needed
something different — waiting, a policy, a deployment setting, or nothing at
all.

The automation page now names the state it is in, and only that state:

> **HarborMaster has not finished its first update assessment**
> No action is required. This page will fill in on its own.

**"Not established" is never rendered as "nothing to do".** An estate that has
not been assessed does not report zero eligible containers, and a readiness read
that failed says it failed. A check that could not be performed establishes
nothing, and the copy says so rather than resolving it into the reassuring
answer.

**A disabled engine and a policy on Observe are never shown as each other.**
They are different switches with different fixes — one is a deployment setting
and a container recreation, the other is a field in the policy editor — and
reporting one as the other sends an operator to change something that is already
correct.

Where a capability is missing, the page prints the exact environment variables
and stops there. There is no button: these are read at startup, so applying them
means editing this deployment's configuration and recreating the HarborMaster
container, which HarborMaster cannot do to itself. A control that appeared to
work and had not would be worse than no control.

Opening the page changes nothing. It composes reads HarborMaster already serves
— it does not start a planning pass, write a policy, contact a registry, or
touch a container.

Operators arriving from Watchtower are pointed at the **Follow current tag**
outcome, with the difference stated rather than glossed: HarborMaster still
applies its snapshot requirement, recommendation floor, dependency ordering,
maintenance window, verification and rollback, so it will sometimes do nothing
where Watchtower would have acted.
