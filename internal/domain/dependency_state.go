package domain

// The closed vocabulary of dependency orchestration outcomes.
//
// # Why these are not "error" and not "conflict"
//
// Every state below is a REASON an operator can act on. HarborMaster refusing to
// recreate a container because the thing it depends on has not been verified is
// not a failure -- it is the subsystem working, and reporting it as an error
// would put a red row in front of an operator for a decision that was correct.
//
// The distinction the phase brief draws, kept here in one sentence: a
// downstream dependency block is NOT a failed Docker mutation, because no
// mutation was attempted.

// DependencyState is one container's dependency verdict.
//
// The zero value is deliberately not a member. A state nobody set is invalid
// rather than "satisfied" -- the same choice routeAccess and UpdateScope make,
// and for the same reason: "I forgot" and "I checked and it was fine" must not
// look identical.
type DependencyState string

const (
	// DependencyStateInvalid is the zero value. A verdict carrying it was never
	// computed, and every caller treats it as a refusal.
	DependencyStateInvalid DependencyState = ""

	// DependencySatisfied means every dependency is verified or stable.
	//
	// Reached two ways, and only these two: the dependency needed no update and
	// the inventory says it is present and running, or the dependency needed an
	// update and its execution reached ExecutionSucceeded -- which is
	// HarborMaster's terminal state AFTER the health proof, the digest
	// verification, the preservation comparison, and the network check.
	//
	// "The request was accepted" and "the recreation was submitted" satisfy
	// nothing.
	DependencySatisfied DependencyState = "dependencySatisfied"

	// DependencyWaiting means a dependency is still being processed.
	//
	// NOT terminal, and that is the point: a waiting decision is picked up again
	// by the follower on its next tick and released when its dependency
	// finishes. Nothing about the wait is held in memory.
	DependencyWaiting DependencyState = "dependencyWaiting"

	// DependencyBlocked means a required dependency failed, was refused, or
	// cannot safely be updated.
	DependencyBlocked DependencyState = "dependencyBlocked"

	// DependencyCycle means the graph is invalid and no member of the cycle may
	// proceed. Never broken automatically.
	DependencyCycle DependencyState = "dependencyCycle"

	// DependencyMissing means a required dependency cannot be resolved.
	//
	// The fail-closed answer for a namespace reference HarborMaster could not
	// match to a container it knows, for an operator relationship whose target
	// has been removed, and for a container whose namespace facts have not been
	// observed since migration 0024.
	DependencyMissing DependencyState = "dependencyMissing"

	// DependencyIneligible means a dependency needs work that HarborMaster's
	// existing policy and safety rules prohibit.
	//
	// The state that keeps invariant "a dependency is not a way to broaden a
	// policy" visible: the answer is that the DEPENDENT is blocked, never that
	// the dependency is enrolled.
	DependencyIneligible DependencyState = "dependencyIneligible"
)

// DependencyStates lists every state, satisfied first.
var DependencyStates = []DependencyState{
	DependencySatisfied,
	DependencyWaiting,
	DependencyBlocked,
	DependencyCycle,
	DependencyMissing,
	DependencyIneligible,
}

// ValidDependencyState reports whether value names a state this build knows.
func ValidDependencyState(value string) bool {
	for _, state := range DependencyStates {
		if string(state) == value {
			return true
		}
	}
	return false
}

// Clears reports whether the state permits the dependent to proceed.
//
// An ALLOWLIST of exactly one member, not a "is it bad" test. Written this way
// so that adding a state cannot accidentally make it permissive by failing to
// appear in a deny list -- the same reasoning recommendationSatisfies uses.
func (s DependencyState) Clears() bool {
	return s == DependencySatisfied
}

// Terminal reports whether the state is a final answer for this pass.
//
// Only DependencyWaiting is not: it is the state that gets looked at again.
// Everything else is a conclusion the follower does not revisit, though the
// NEXT decision pass will of course reconsider the container from scratch.
func (s DependencyState) Terminal() bool {
	switch s {
	case DependencyWaiting:
		return false
	case DependencySatisfied, DependencyBlocked, DependencyCycle,
		DependencyMissing, DependencyIneligible:
		return true
	default:
		// An unrecognised state is terminal. Fails towards "stop", not towards
		// "keep retrying something this build does not understand".
		return true
	}
}

// NeedsOperator reports whether the state is one a person has to resolve.
//
// Used to decide what reaches the dashboard. A wait resolves itself and must
// not raise anything; a cycle, a missing dependency, and an ineligible one do
// not resolve without somebody changing something.
func (s DependencyState) NeedsOperator() bool {
	switch s {
	case DependencyCycle, DependencyMissing, DependencyIneligible, DependencyBlocked:
		return true
	default:
		return false
	}
}

// Explain renders the state in operator-facing words.
//
// HarborMaster's own sentences. The specific dependency that caused it is
// carried separately, on the verdict, so this text never interpolates a name.
func (s DependencyState) Explain() string {
	switch s {
	case DependencySatisfied:
		return "every container this one depends on is verified or stable"
	case DependencyWaiting:
		return "a container this one depends on is still being updated"
	case DependencyBlocked:
		return "a container this one depends on could not be updated safely"
	case DependencyCycle:
		return "these containers depend on each other in a loop, so no safe order exists"
	case DependencyMissing:
		return "a container this one depends on could not be identified"
	case DependencyIneligible:
		return "a container this one depends on needs an update that the rules in force do not permit"
	default:
		return "HarborMaster did not establish this container's dependencies"
	}
}

// Label renders the state as a short UI badge.
func (s DependencyState) Label() string {
	switch s {
	case DependencySatisfied:
		return "Dependency satisfied"
	case DependencyWaiting:
		return "Waiting for dependency"
	case DependencyBlocked:
		return "Blocked by dependency"
	case DependencyCycle:
		return "Dependency cycle"
	case DependencyMissing:
		return "Dependency missing"
	case DependencyIneligible:
		return "Dependency not permitted"
	default:
		return "Dependencies unknown"
	}
}
