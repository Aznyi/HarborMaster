package service

import (
	"strconv"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The decision function: everything the engine decides before it acts.
//
// # Why this is a pure function
//
// Deciding whether a container may be updated automatically is the most
// consequential judgement HarborMaster makes, and it is made on a timer with
// nobody watching. So it is a function of its inputs and nothing else: no
// clock reading of its own, no database, no Docker, no configuration lookup.
// Every input arrives as a parameter, and the result is a decision record
// carrying the reason.
//
// That buys three things. It can be tested exhaustively without a host. Its
// answer for a given world is identical on every pass, so "why did it not
// update that container" has one answer rather than a re-derivation. And the
// checks that follow it -- the ones that DO touch the world -- cannot silently
// become the place a policy decision is made.
//
// # The order of the checks is the security design
//
// Cheapest and most absolute first, so a container that must never be touched
// is refused before anything expensive or fallible runs:
//
//	 0. self            -- HarborMaster's own container, which it cannot update
//	 1. paused          -- HarborMaster already decided not to
//	 2. label opt-out   -- the container's owner already decided not to
//	 3. policy selection-- nothing governs it
//	 4. policy disabled -- the rule exists but is off
//	 5. plan exists     -- there is nothing to do
//	 6. plan proposes a change
//	 7. strategy ceiling-- the change is bigger than permitted
//	 8. recommendation  -- the planner wants a person
//	 9. window          -- not now
//	10. mode            -- observe, dry run, or approval
//
// Only a container that passes all eleven is eligible, and eligibility is still
// not action: the run-level limits are applied afterwards, in order, and the
// services each re-run their own preflight against the live host.
//
// Check 0 is applied a SECOND time by the acquisition and execution services.
// The engine is not the only caller of those, and a self-update is the one
// refusal that must hold even if this function is never reached.

// AutomationInput is everything one container's decision is made from.
//
// Every field is copied from a record HarborMaster wrote itself. There is
// nowhere on this type for a caller-supplied container id, image, or digest,
// which is why the decision function cannot be aimed.
type AutomationInput struct {
	// Target is the container as the inventory saw it.
	Target domain.SelectionTarget
	// ContainerID is carried for the plan lookup only. The SELECTOR never sees
	// it: an id changes on every recreation, and a policy pinned to one would
	// stop governing the container the moment it acted.
	ContainerID string

	// Policies are every enabled policy, already loaded once for the pass.
	Policies []domain.UpdatePolicy

	// Plan is the container's current change plan, and HasPlan says whether one
	// exists at all. A missing plan is a reason, not an error: it means the
	// planner has not assessed this container, and automating a change nobody
	// assessed is exactly what this engine must not do.
	Plan    domain.ChangePlan
	HasPlan bool

	// Pause is the active pause, when there is one.
	Pause    domain.PausedContainer
	IsPaused bool

	// InFlight reports that work HarborMaster started for this container has
	// not finished. Submitting a second update on top of the first is how one
	// pass becomes two concurrent recreations of the same container.
	InFlight bool

	// Now is the instant the whole pass is decided against. ONE reading for the
	// whole pass: two containers whose windows differ by a millisecond must not
	// get different answers because the clock moved between them.
	Now time.Time

	// RequireApprovalForMajor is the deployment-wide refusal to apply a major
	// version update unattended, applied on top of the policy's strategy.
	RequireApprovalForMajor bool

	// Self is what HarborMaster knows about its own container. The zero value
	// matches nothing, so a build that has not wired detection is no less safe
	// than one where every probe failed -- and both are still covered by the
	// second refusal in the execution service.
	Self domain.SelfIdentity
}

// AutomationOutcome is one container's decision, with the effective policy that
// produced it.
type AutomationOutcome struct {
	Decision domain.AutomationDecision
	// Effective is the policy after the container's labels were applied. Empty
	// when no policy governs.
	Effective domain.EffectivePolicy
	// Governed reports that a policy selected this container at all.
	Governed bool
}

// Eligible reports whether the decision may proceed to submission.
func (o AutomationOutcome) Eligible() bool {
	return o.Decision.Verdict == domain.VerdictUpdate
}

// DecideAutomation makes one container's decision.
//
// Never returns an error. Every way this can decline is a REASON, recorded on
// the decision, because "the engine errored" is not something an operator can
// act on and "the maintenance window was closed" is.
func DecideAutomation(input AutomationInput) AutomationOutcome {
	decision := domain.AutomationDecision{
		ContainerID:   input.ContainerID,
		ContainerName: input.Target.Name,
		CurrentImage:  input.Target.Image,
		Verdict:       domain.VerdictSkip,
		DecidedAt:     input.Now,
	}

	// 0. HarborMaster itself.
	//
	// FIRST, before the pause, before the labels, before the policy lookup.
	// Nothing an operator configures may reorder this: a self-update is not a
	// change that needs approving, it is an operation that cannot complete --
	// the process stops its own container and dies before it can verify,
	// checkpoint, or roll back.
	if self, why := input.Self.SelfMatch(domain.SelfTarget{
		ContainerID:   input.ContainerID,
		ContainerName: input.Target.Name,
		ImageRef:      input.Target.Image,
		Labels:        input.Target.Labels,
	}); self {
		decision.Reason = domain.ReasonSelfUpdate
		decision.Detail = why + "; update HarborMaster from outside it, with " +
			"`docker compose pull && docker compose up -d`"
		return AutomationOutcome{Decision: decision}
	}

	// 1. HarborMaster already decided not to touch this container.
	//
	// First, and before the policy lookup, because a pause is a refusal that
	// outranks every rule: it was recorded because something went wrong, and a
	// policy edit must not clear it.
	if input.IsPaused && input.Pause.Active(input.Now) {
		decision.Reason = domain.ReasonPaused
		decision.Detail = pauseDetail(input.Pause)
		return AutomationOutcome{Decision: decision}
	}

	// 2. The container's owner already decided not to.
	//
	// Read before the policy is selected so an opt-out holds even when no
	// policy governs -- which costs nothing and means the label means the same
	// thing whatever the policy set looks like.
	overrides := domain.ParseUpdateLabels(input.Target.Labels)
	switch {
	case overrides.Disabled:
		decision.Reason = domain.ReasonLabelOff
		decision.Detail = "the container carries " + domain.LabelUpdateEnabled + "=false"
		return AutomationOutcome{Decision: decision}
	case overrides.Paused:
		decision.Reason = domain.ReasonLabelPaused
		decision.Detail = "the container carries " + domain.LabelUpdatePause + "=true"
		return AutomationOutcome{Decision: decision}
	}

	// 3. Nothing governs it.
	//
	// The identity is passed to the selection, not just consulted in step 0
	// above. Step 0 is a refusal of a container a policy already selected; this
	// is the broad scope declining to select it at all. Both hold, and neither
	// depends on the other having run.
	policy, governed := domain.SelectUpdatePolicy(input.Policies, input.Target, input.Self)
	if !governed {
		decision.Reason, decision.Detail = notGovernedReason(input)
		return AutomationOutcome{Decision: decision}
	}

	effective := domain.Resolve(policy, input.Target.Labels)
	decision.PolicyID = policy.PolicyID
	decision.PolicyName = policy.Name
	outcome := AutomationOutcome{Effective: effective, Governed: true}

	// 4. The rule exists but is off. SelectUpdatePolicy already skips disabled
	// policies, so reaching this means Resolve declined for a label reason the
	// two checks above did not catch -- kept as a distinct branch rather than
	// folded in, so a future label that disables cannot pass silently.
	if effective.Disabled {
		decision.Reason = domain.ReasonPolicyOff
		decision.Detail = effective.Reason
		outcome.Decision = decision
		return outcome
	}

	// 5. There is nothing to do, because nobody assessed it.
	if !input.HasPlan {
		decision.Reason = domain.ReasonNoPlan
		decision.Detail = "the planner has not produced a change plan for this container"
		outcome.Decision = decision
		return outcome
	}

	plan := input.Plan
	decision.PlanID = plan.PlanID
	decision.CurrentImage = plan.CurrentImage
	decision.ProposedImage = plan.ProposedImage
	decision.ProposedDigest = plan.ProposedDigest
	decision.UpdateType = plan.UpdateType
	decision.Recommendation = plan.Risk.Recommendation

	// 6. The plan proposes no change.
	//
	// ValidTarget is re-checked here as well as in the acquisition preflight.
	// A plan whose proposed reference and proposed digest were not resolved for
	// each other is the Phase 10.1 defect class, and the engine must not be the
	// component that carries one forward.
	switch {
	case plan.UpdateType == domain.UpdateNone || plan.ProposedImage == "":
		decision.Reason = domain.ReasonNoUpdate
		decision.Detail = "the current change plan proposes no change"
		outcome.Decision = decision
		return outcome
	case !plan.ValidTarget():
		decision.Reason = domain.ReasonRefused
		decision.Detail = "the change plan's proposed reference and digest were not resolved together"
		outcome.Decision = decision
		return outcome
	}

	// 7. The change is bigger than the policy permits.
	if !effective.Strategy.Permits(plan.UpdateType) {
		decision.Reason = domain.ReasonStrategy
		decision.Detail = "the change is a " + string(plan.UpdateType) +
			" update and the policy permits at most " + string(effective.Strategy)
		outcome.Decision = decision
		return outcome
	}
	// The deployment-wide refusal, applied on top. A policy may say major; this
	// says not without a person, whatever the policy says.
	if input.RequireApprovalForMajor && plan.UpdateType == domain.UpdateMajor {
		decision.Verdict = domain.VerdictAwaitingApproval
		decision.Reason = domain.ReasonNeedApproval
		decision.Detail = "this deployment requires a person to approve every major version update"
		outcome.Decision = decision
		return outcome
	}

	// 8. The planner wants a person.
	if !recommendationSatisfies(plan.Risk.Recommendation, policy.MinimumRecommendation) {
		decision.Reason = domain.ReasonRecommendation
		decision.Detail = "the planner recommends " + string(plan.Risk.Recommendation) +
			" and the policy requires at least " + string(policy.MinimumRecommendation)
		outcome.Decision = decision
		return outcome
	}

	// 9. Not now.
	open, err := effective.Window.Open(input.Now)
	switch {
	case err != nil:
		// Fails CLOSED. A window nobody can evaluate authorises nothing.
		decision.Reason = domain.ReasonWindowInvalid
		decision.Detail = "the policy's maintenance window names a timezone this host cannot resolve"
		outcome.Decision = decision
		return outcome
	case !open:
		decision.Reason = domain.ReasonWindowClosed
		decision.Detail = "the maintenance window is " + effective.Window.Describe()
		outcome.Decision = decision
		return outcome
	}

	// 10. Work is already in flight for this container.
	//
	// After the window check so a container that is busy during a closed window
	// reports the window, which is the more useful of two true answers.
	if input.InFlight {
		decision.Reason = domain.ReasonInFlight
		decision.Detail = "an acquisition or recreation HarborMaster started has not finished"
		outcome.Decision = decision
		return outcome
	}

	// 11. The mode. Everything above established that the change COULD happen;
	// this decides whether the engine is allowed to make it.
	switch effective.Mode {
	case domain.ModeObserve:
		decision.Verdict = domain.VerdictWouldUpdate
		decision.Reason = domain.ReasonObserveMode
		decision.Detail = "the policy is in observe mode, so nothing was changed"
	case domain.ModeDryRun:
		decision.Verdict = domain.VerdictWouldUpdate
		decision.Reason = domain.ReasonDryRunMode
		decision.Detail = "the policy is in dry-run mode, so nothing was changed"
	case domain.ModeApprove:
		decision.Verdict = domain.VerdictAwaitingApproval
		decision.Reason = domain.ReasonNeedApproval
		decision.Detail = "the policy requires a person to approve each update"
	case domain.ModeAutomatic:
		decision.Verdict = domain.VerdictUpdate
		decision.Reason = domain.ReasonEligible
		decision.Detail = "every check passed"
	default:
		// An unrecognised mode is not a mode that may act. Fails closed rather
		// than defaulting into the branch that changes the host.
		decision.Verdict = domain.VerdictSkip
		decision.Reason = domain.ReasonRefused
		decision.Detail = "the policy names a mode this build does not understand"
	}

	outcome.Decision = decision
	return outcome
}

// notGovernedReason explains why no policy took this container.
//
// # Why a broad policy's refusal gets its own answer
//
// "No policy's selector matches this container" is the right sentence when
// every policy names what it governs. It is the WRONG sentence once one of them
// says "all eligible containers": an operator reading it would go and edit a
// selector that is not the reason, on a policy that has none.
//
// So when a broad policy exists and declined this container, the decision
// carries the fact that decided it -- and it is HarborMaster's own sentence,
// from BroadlySelectable, not a rephrasing invented here.
//
// The narrowest true answer wins. A container that a broad policy declined AND
// that no selector names is reported as the former, because that is the one an
// operator can act on.
func notGovernedReason(input AutomationInput) (domain.AutomationReason, string) {
	if len(input.Policies) == 0 {
		return domain.ReasonNoPolicy, "no update policy is defined"
	}
	for _, policy := range input.Policies {
		if !policy.Scope.Broad() || !policy.Enabled || policy.Archived {
			continue
		}
		if policy.Selector.Excludes(input.Target.Name) {
			return domain.ReasonNotEligible,
				"the policy " + policy.Name + " covers all eligible containers and excludes this one"
		}
		if selectable, why := input.Target.BroadlySelectable(input.Self); !selectable {
			return domain.ReasonNotEligible, why
		}
	}
	return domain.ReasonNotSelected, "no update policy's selector matches this container"
}

// recommendationSatisfies reports whether a plan's verdict meets the policy's
// minimum.
//
// An ALLOWLIST comparison rather than an ordering: `proceed` satisfies both
// minimums, `proceedWithCaution` satisfies only itself, and every other verdict
// satisfies nothing. Expressed this way rather than as a rank so that adding a
// verdict cannot accidentally make it automatable by landing at a low number.
func recommendationSatisfies(actual, minimum domain.Recommendation) bool {
	switch minimum {
	case domain.RecommendProceed:
		return actual == domain.RecommendProceed
	case domain.RecommendCaution:
		return actual == domain.RecommendProceed || actual == domain.RecommendCaution
	default:
		return false
	}
}

// pauseDetail renders why a container is paused, in HarborMaster's own words.
func pauseDetail(pause domain.PausedContainer) string {
	detail := pause.Reason.Explain()
	if pause.Failures > 0 {
		detail += " (" + strconv.Itoa(pause.Failures) + " recorded)"
	}
	if pause.ResumeAfter == nil {
		return detail + "; an operator must resume it"
	}
	return detail + "; it resumes automatically after " +
		pause.ResumeAfter.UTC().Format(time.RFC3339)
}

// ------------------------------------------------------------ run limits --

// AutomationBudget applies the run-level ceilings, in order.
//
// Separate from DecideAutomation because these are properties of the PASS
// rather than of the container: whether a container is eligible does not depend
// on how many others were, but whether it is submitted does. Keeping them apart
// means a dry run can report "eligible, but would have been the eleventh of
// ten" rather than conflating the two.
type AutomationBudget struct {
	// MaxPerRun and MaxConcurrent are the engine-wide ceilings from
	// configuration. A policy's own limits are applied on top and can only
	// lower them.
	MaxPerRun     int
	MaxConcurrent int
	// InFlight is how much work HarborMaster already has outstanding, counted
	// once at the start of the pass.
	InFlight int

	started      int
	perPolicy    map[string]int
	perRegistry  map[string]int
	registryOfFn func(string) string
}

// NewAutomationBudget builds a budget for one pass.
func NewAutomationBudget(maxPerRun, maxConcurrent, inFlight int, registryOf func(string) string) *AutomationBudget {
	if maxPerRun < 1 {
		maxPerRun = 1
	}
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	if registryOf == nil {
		registryOf = func(string) string { return "" }
	}
	return &AutomationBudget{
		MaxPerRun:     maxPerRun,
		MaxConcurrent: maxConcurrent,
		InFlight:      inFlight,
		perPolicy:     make(map[string]int),
		perRegistry:   make(map[string]int),
		registryOfFn:  registryOf,
	}
}

// Admit reports whether one eligible decision may be submitted, and why not.
//
// Checks the engine-wide ceilings first, then the policy's. Reserves the slot
// on success, so a caller that admits and then does not submit has consumed
// budget -- deliberately: an admitted update that failed to submit still put
// load on the host, and the pass should not immediately try another.
func (b *AutomationBudget) Admit(
	decision domain.AutomationDecision,
	effective domain.EffectivePolicy,
) (domain.AutomationReason, bool) {
	if b.started >= b.MaxPerRun {
		return domain.ReasonRunLimit, false
	}
	if b.InFlight+b.started >= b.MaxConcurrent {
		return domain.ReasonConcurrency, false
	}

	limits := effective.Policy.Limits
	policyID := effective.Policy.PolicyID
	if limits.MaxPerRun > 0 && b.perPolicy[policyID] >= limits.MaxPerRun {
		return domain.ReasonRunLimit, false
	}
	if limits.MaxConcurrent > 0 && b.perPolicy[policyID] >= limits.MaxConcurrent {
		return domain.ReasonConcurrency, false
	}

	registry := b.registryOfFn(decision.ProposedImage)
	if limits.MaxPerRegistry > 0 && b.perRegistry[registry] >= limits.MaxPerRegistry {
		return domain.ReasonRegistryLimit, false
	}

	b.started++
	b.perPolicy[policyID]++
	b.perRegistry[registry]++
	return domain.ReasonEligible, true
}

// Started reports how many updates this pass has admitted.
func (b *AutomationBudget) Started() int { return b.started }
