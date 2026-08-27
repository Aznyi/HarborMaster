package domain

// First-run and engine state: one answer to "why is automation doing that".
//
// # The question this exists to answer
//
// A fresh installation has several perfectly ordinary states that all LOOK like
// "nothing is happening", and an operator cannot tell them apart from the
// outside:
//
//	the estate has not been assessed yet
//	the estate was assessed and nothing needs updating
//	assessment is switched off in this deployment
//	the engine is off
//	the engine is on and no policy exists
//	a policy exists and only watches
//	a policy would act and nothing currently qualifies
//
// Rendering any of those as "0 eligible" or "automation off" is the failure
// this file prevents. They mean different things, they have different remedies,
// and an operator who is told the wrong one goes and changes the wrong thing.
//
// # Why it is a pure function
//
// Every input is a fact some other component already established: the
// capability flags from the health report, the policy counts from the
// automation status, the assessment state from the planner, and the counts from
// the Stage 17.4 readiness report. Nothing here re-derives any of them, and
// there is no second engine, second planner or second readiness model.
//
// It is a PROJECTION, exactly like SummariseAutomationReadiness. Given the same
// facts it returns the same state, so it can be exercised exhaustively without
// a database, a Docker daemon or a clock.

// FirstRunState is what an installation is currently doing about updates.
//
// Ordered from "not ready yet" to "working", which is the order an operator
// progresses through. The order is not load-bearing for any decision -- nothing
// compares these -- it is for reading.
type FirstRunState string

const (
	// FirstRunInventoryPending means HarborMaster has not established what is
	// running yet. Everything else waits on this.
	FirstRunInventoryPending FirstRunState = "inventoryPending"
	// FirstRunAssessmentPending means the estate is known and the planner has
	// not finished its first pass.
	//
	// The state this file exists for. It is NOT "nothing needs updating": the
	// question has not been asked yet, and answering it as though it had would
	// tell an operator their estate is settled when nothing has looked.
	FirstRunAssessmentPending FirstRunState = "assessmentPending"
	// FirstRunAssessmentUnavailable means change planning is switched off in
	// this deployment, so there will never be an assessment.
	//
	// Distinct from pending: pending resolves by waiting, this one resolves by
	// changing the deployment.
	FirstRunAssessmentUnavailable FirstRunState = "assessmentUnavailable"

	// FirstRunEngineDisabled means the estate is assessed and the update engine
	// is off. HarborMaster reports; it will not act.
	FirstRunEngineDisabled FirstRunState = "engineDisabled"
	// FirstRunNoPolicy means the engine is on and nothing tells it what to do.
	FirstRunNoPolicy FirstRunState = "noPolicy"
	// FirstRunObserveOnly means policies exist and none of them may act.
	//
	// Deliberately NOT "automation disabled": the engine may be running
	// perfectly, and telling an operator it is off would send them to change a
	// deployment setting that is already correct.
	FirstRunObserveOnly FirstRunState = "observeOnly"

	// FirstRunNothingEligible means an acting policy exists and no container
	// currently qualifies. A normal, healthy steady state.
	FirstRunNothingEligible FirstRunState = "nothingEligible"
	// FirstRunActive means an acting policy exists and containers currently
	// qualify.
	FirstRunActive FirstRunState = "active"

	// FirstRunNeedsAttention means something is waiting for a person: a paused
	// container, or a plan that asks for manual review.
	//
	// Ranked above `active` because it is the one state with an action attached
	// to it, and an operator who only reads the headline should read that one.
	FirstRunNeedsAttention FirstRunState = "needsAttention"

	// FirstRunUnknown means the facts could not be established.
	//
	// Never rendered as a count. "HarborMaster could not tell you" and "the
	// answer is zero" are opposite messages.
	FirstRunUnknown FirstRunState = "unknown"
)

// FirstRunFacts are the established facts this projection reads.
//
// Every field is something another component owns. Nothing here is computed by
// the caller: a caller that had to work out `ActingPolicies` would be
// reimplementing policy semantics, so it is taken from the automation status.
type FirstRunFacts struct {
	// Features are the deployment's capabilities, from the health report.
	Features Features

	// InventoryEstablished reports that HarborMaster knows what is running.
	InventoryEstablished bool
	// Assessed reports that the planner has completed at least one pass.
	//
	// From PlannerStatus.LastRunAt. NOT inferred from the absence of plans: a
	// settled estate legitimately has none, and NOT inferred from elapsed time,
	// which would be a guess dressed as a fact.
	Assessed bool

	// Policies and ActingPolicies come from the automation status. An acting
	// policy is one whose mode may change a container.
	Policies       int
	ActingPolicies int

	// PausedContainers and ManualReviews are the two things that wait for a
	// person, from Stage 17.6 and Stage 17.7 respectively.
	PausedContainers int
	ManualReviews    int

	// Eligible is the Stage 17.4 readiness count, and ReadinessKnown says
	// whether it could be established at all.
	//
	// The pair matters: a readiness request that failed must not be read as
	// zero eligible containers.
	Eligible       int
	ReadinessKnown bool
}

// DescribeFirstRun projects the facts onto one state.
//
// # The order of the checks IS the semantics
//
// Each one answers a question that makes the ones below it meaningless. There
// is no point telling an operator that no container is eligible when nothing
// has been assessed, or that they have no policy when the engine that would run
// it is switched off.
func DescribeFirstRun(facts FirstRunFacts) FirstRunState {
	// 1. Nothing is known about the host yet.
	if !facts.InventoryEstablished {
		return FirstRunInventoryPending
	}

	// 2. Assessment. Switched off is a different answer from not finished, and
	// both are different from "nothing needs updating".
	if !facts.Features.Planner {
		return FirstRunAssessmentUnavailable
	}
	if !facts.Assessed {
		return FirstRunAssessmentPending
	}

	// 3. The engine. Checked before the policies, because a policy cannot run
	// without it and telling an operator to write one first would waste their
	// time.
	if !facts.Features.Automation {
		return FirstRunEngineDisabled
	}

	// 4. What the operator has told it to do.
	if facts.Policies == 0 {
		return FirstRunNoPolicy
	}
	if facts.ActingPolicies == 0 {
		return FirstRunObserveOnly
	}

	// 5. Something is waiting for a person. Ranked above the eligible count
	// because it is the state with an action attached.
	if facts.PausedContainers > 0 || facts.ManualReviews > 0 {
		return FirstRunNeedsAttention
	}

	// 6. The readiness answer. An unestablished one is NOT zero.
	if !facts.ReadinessKnown {
		return FirstRunUnknown
	}
	if facts.Eligible == 0 {
		return FirstRunNothingEligible
	}
	return FirstRunActive
}

// Explain renders the state in HarborMaster's own words.
//
// One sentence about what is true, never a promise about what will happen. The
// UI adds the action; this supplies the fact, so a client with no mapping still
// renders something true rather than an identifier.
func (s FirstRunState) Explain() string {
	switch s {
	case FirstRunInventoryPending:
		return "HarborMaster is still establishing what is running on this host"
	case FirstRunAssessmentPending:
		return "HarborMaster knows what is running and has not finished assessing " +
			"it for updates yet"
	case FirstRunAssessmentUnavailable:
		return "change planning is switched off in this deployment, so HarborMaster " +
			"is not assessing containers for updates"
	case FirstRunEngineDisabled:
		return "the update engine is switched off in this deployment: HarborMaster " +
			"assesses containers and will not change any of them"
	case FirstRunNoPolicy:
		return "the update engine is running and no update policy tells it what to do"
	case FirstRunObserveOnly:
		return "HarborMaster is assessing updates and reporting what it would do, " +
			"without changing any container"
	case FirstRunNeedsAttention:
		return "automatic updates are configured, and something is waiting for a person"
	case FirstRunNothingEligible:
		return "automatic updates are configured, and no container is currently " +
			"eligible for one"
	case FirstRunActive:
		return "automatic updates are configured, and containers are currently eligible"
	case FirstRunUnknown:
		return "HarborMaster could not establish what it would do right now"
	default:
		return ""
	}
}

// NeedsSetup reports whether this state is waiting on the OPERATOR.
//
// Used to decide whether an onboarding item deserves dashboard space. The
// waiting-on-HarborMaster states are deliberately excluded: an item that told
// an operator to act while HarborMaster was still starting up would be asking
// them to fix something that is not broken.
func (s FirstRunState) NeedsSetup() bool {
	switch s {
	case FirstRunEngineDisabled, FirstRunNoPolicy:
		return true
	default:
		return false
	}
}

// AutomationCapabilities are the capabilities an acting policy needs.
//
// # Why this depends on the policy
//
// The set is the deployment's, not any individual policy's. Every entry is a
// capability HarborMaster REFUSES TO START without once automation is enabled,
// so the list is the one an operator can actually apply.
type AutomationCapabilities struct {
	// Acquisition downloads the approved image.
	Acquisition bool
	// Execution replaces the container. The dangerous one.
	Execution bool
	// Automation is the engine that decides unattended.
	Automation bool
	// Rollback puts a failed update back.
	Rollback bool
}

// RequiredForAutomation reports what an unattended update actually needs.
//
// # Why rollback is unconditional
//
// It is tempting to require rollback only when some policy asks for automatic
// rollback, on the reasoning that nobody should be told to enable a capability
// their policies never use. That reasoning is wrong here, and the way it is
// wrong is the worst kind: config validation refuses to start a process with
// AUTOMATION_ENABLED and ROLLBACK_ENABLED unset, whatever any policy says.
//
// So a list that omitted rollback would be a set of settings an operator could
// apply exactly, recreate the container, and find HarborMaster refusing to
// boot -- onboarding instructions that take the installation down. A capability
// list is only useful if applying all of it works.
//
// The startup rules in internal/config are the authority for this function. If
// they change, this changes with them.
func RequiredForAutomation() AutomationCapabilities {
	return AutomationCapabilities{
		Acquisition: true,
		Execution:   true,
		Automation:  true,
		Rollback:    true,
	}
}

// MissingForAutomation lists the required capabilities this deployment lacks.
//
// Returns names an operator can act on, in a fixed order so the answer is
// stable. Empty means an acting policy can run.
func MissingForAutomation(features Features, required AutomationCapabilities) []string {
	var missing []string
	if required.Acquisition && !features.Acquisition {
		missing = append(missing, "acquisition")
	}
	if required.Execution && !features.Execution {
		missing = append(missing, "execution")
	}
	if required.Automation && !features.Automation {
		missing = append(missing, "automation")
	}
	if required.Rollback && !features.Rollback {
		missing = append(missing, "rollback")
	}
	return missing
}
