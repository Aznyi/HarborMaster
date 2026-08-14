package service

import (
	"sort"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The dependency gate: the last thing consulted before an eligible update is
// submitted, and the only thing in this subsystem that can change an outcome.
//
// # It can only SUBTRACT
//
// This is the invariant the whole phase rests on, and it is enforced
// structurally rather than by care. DecideDependency reads the verdict it was
// given and returns either THAT VERDICT or domain.VerdictSkip. There is no
// branch that writes domain.VerdictUpdate, and no input that produces one from
// anything else -- the value is copied from the input or replaced with a
// refusal.
//
// TestDependencyGateCanOnlySubtract walks the entire verdict vocabulary crossed
// with the entire dependency state vocabulary and asserts exactly that.
//
// The consequence an operator can rely on: a container no update policy selects
// is not made selectable by anything here. A dependency relationship is not a
// route into a policy's scope, past an exclusion, past an opt-out label, or past
// the self-update refusal. It cannot enrol; it can only hold back.
//
// # Why the state is computed even when the verdict is not touched
//
// A container in observe mode, or awaiting approval, still has dependencies, and
// an operator looking at why it is waiting needs the answer whether or not the
// gate acted. So the STATE is always computed and always reported; only the
// VERDICT is conditional on the decision having been an eligible one.
//
// # Purity
//
// Same rules as DecideAutomation, for the same reasons: no clock, no database,
// no Docker, no configuration lookup. Every input is a parameter and the output
// is a value. That is what lets the property test above be exhaustive.

// DependencyFact is what a pass established about one container.
//
// Every field is a POSITIVE fact. The zero value asserts nothing, which means a
// dependency the caller failed to look up satisfies nothing and holds its
// dependents -- the fail-closed direction.
type DependencyFact struct {
	// Present and Running come from the inventory.
	Present bool
	Running bool
	// NeedsWork says the pass found something to do for this container: an
	// update, or a rebind.
	NeedsWork bool
	// Eligible says the pass decided the work MAY proceed. False for a container
	// no policy governs, one in observe mode, one awaiting approval, and one a
	// safety rule declined.
	Eligible bool
	// Verified says the container's work reached verified success -- an
	// execution record in domain.ExecutionSucceeded. NOT "submitted", NOT
	// "accepted".
	Verified bool
}

// Satisfies reports whether this container clears its dependents to proceed.
//
// Exactly two ways, and both require positive facts:
//
//  1. It needed no work and the inventory says it is present and running. A
//     healthy container is not recreated merely because something depends on it.
//  2. It needed work and that work reached VERIFIED success.
//
// Everything else holds: a container that needs work and has not finished it, a
// container that needs work and may not do it, and a container nobody
// established anything about.
func (f DependencyFact) Satisfies() bool {
	if !f.NeedsWork {
		return f.Present && f.Running
	}
	return f.Verified
}

// DependencyInput is everything one container's dependency verdict is made from.
type DependencyInput struct {
	// Container is the name being decided.
	Container string
	// Verdict and Reason are what the automation decision concluded. The gate
	// may replace them with a refusal and may do nothing else to them.
	Verdict domain.AutomationVerdict
	Reason  domain.AutomationReason

	// Graph is the estate's ordering.
	Graph domain.DependencyGraph
	// Problems are the discovery refusals recorded against THIS container.
	Problems []domain.DependencyProblem
	// Facts is what the pass established about every container, by name.
	Facts map[string]DependencyFact

	// Provider is invariant A's assessment, when this container is a hard
	// namespace provider. The zero value has no blocks, which is correct only
	// when HasHardDependents is false -- see the check in DecideDependency.
	Provider domain.ProviderRebindAssessment
	// HasHardDependents says something shares this container's namespace.
	HasHardDependents bool
	// ProviderAssessed says invariant A actually ran. Required to be true
	// whenever HasHardDependents is: an assessment that did not happen
	// establishes nothing, and must not be read as "nothing blocked".
	ProviderAssessed bool
}

// DependencyVerdict is the gate's answer.
type DependencyVerdict struct {
	// State is always populated, whatever happened to the verdict.
	State domain.DependencyState
	// Verdict is the input verdict, or domain.VerdictSkip.
	Verdict domain.AutomationVerdict
	// Reason is the input reason, or the dependency reason that replaced it.
	Reason domain.AutomationReason
	// BlockedBy names the specific container responsible, when one is.
	BlockedBy string
	// Detail is HarborMaster's own sentence.
	Detail string
}

// DecideDependency applies the ordering constraints to one container.
//
// The order is most absolute first: a graph that cannot be ordered at all is
// answered before anything about individual dependencies is consulted.
func DecideDependency(input DependencyInput) DependencyVerdict {
	name := domain.NormaliseContainerName(input.Container)

	// The verdict starts as whatever it was. Every path below either leaves it
	// alone or calls refuse(); nothing else may write it.
	verdict := DependencyVerdict{
		State:   domain.DependencySatisfied,
		Verdict: input.Verdict,
		Reason:  input.Reason,
	}

	// refuse downgrades the decision, and ONLY when the decision was one that
	// would have acted. A container already skipping keeps the reason it had --
	// replacing "no policy selects this" with "waiting for a dependency" would
	// be a less true answer, not a more restrictive one.
	refuse := func(state domain.DependencyState, blockedBy, detail string) DependencyVerdict {
		verdict.State = state
		verdict.BlockedBy = blockedBy
		verdict.Detail = detail
		if input.Verdict == domain.VerdictUpdate {
			verdict.Verdict = domain.VerdictSkip
			verdict.Reason = domain.DependencyReasonFor(state)
		}
		return verdict
	}

	// 1. The container is not covered by the graph at all.
	//
	// Not "it has no dependencies" -- that is Independent. This is HarborMaster
	// having no row for it, which means nothing was established, which is the
	// fail-closed case.
	if !input.Graph.Known(name) {
		return refuse(domain.DependencyMissing, "",
			"HarborMaster did not establish this container's dependencies")
	}

	// 2. Discovery could not resolve something this container declared.
	//
	// Before the graph's own verdicts, because a namespace share HarborMaster
	// could not resolve is a fact ABOUT THIS CONTAINER, and it is the more
	// specific answer.
	if problem, found := firstProblem(input.Problems); found {
		return refuse(domain.DependencyMissing, "", problem.Refusal.Explain())
	}

	// 3. No safe order exists.
	if input.Graph.CycleBlocked(name) {
		return refuse(domain.DependencyCycle, "", describeFirstCycle(input.Graph, name))
	}

	// 4. Something it depends on is not in the estate.
	if input.Graph.Missing(name) {
		return refuse(domain.DependencyMissing, "",
			domain.DependencyMissing.Explain())
	}

	// 5. Invariant A. BEFORE the per-dependency checks, because this is the one
	// that prevents a silent outage rather than an out-of-order update.
	//
	// The assessment must have RUN. A provider with hard dependents whose
	// assessment was never performed is refused: a check that could not be
	// performed establishes nothing, and reading an empty block list as "nothing
	// blocked" is exactly the inversion invariant A exists to prevent.
	if input.HasHardDependents {
		if !input.ProviderAssessed {
			return refuse(domain.DependencyBlocked, "",
				"HarborMaster did not establish whether the containers sharing this "+
					"one's namespace could be reattached")
		}
		if !input.Provider.MayStop() {
			blocked := input.Provider.Blocked[0]
			verdict.State = domain.DependencyBlocked
			verdict.BlockedBy = blocked.Dependent
			verdict.Detail = blocked.Refusal.Explain()
			if input.Verdict == domain.VerdictUpdate {
				verdict.Verdict = domain.VerdictSkip
				verdict.Reason = domain.ReasonDependentsNotRebindable
			}
			return verdict
		}
	}

	// 6. Each dependency, in a deterministic order so the reported blocker is
	// the same on every evaluation of an unchanged estate.
	for _, dependency := range sortedDependencyNames(input.Graph, name) {
		fact, known := input.Facts[dependency]
		if !known {
			// Nothing established. Holds.
			return refuse(domain.DependencyMissing, dependency,
				"HarborMaster established nothing about a container this one depends on")
		}
		if fact.Satisfies() {
			continue
		}

		switch {
		case fact.NeedsWork && !fact.Eligible:
			// The dependency needs work the rules in force do not permit. The
			// answer is that THIS container is blocked -- never that the
			// dependency is enrolled.
			return refuse(domain.DependencyIneligible, dependency,
				domain.DependencyIneligible.Explain())
		case fact.NeedsWork:
			return refuse(domain.DependencyWaiting, dependency,
				domain.DependencyWaiting.Explain())
		default:
			// It needs no work but is not present and running, so "stable" was
			// never established.
			return refuse(domain.DependencyBlocked, dependency,
				domain.DependencyBlocked.Explain())
		}
	}

	verdict.Detail = domain.DependencySatisfied.Explain()
	return verdict
}

// firstProblem returns the first discovery refusal, in a deterministic order.
func firstProblem(problems []domain.DependencyProblem) (domain.DependencyProblem, bool) {
	if len(problems) == 0 {
		return domain.DependencyProblem{}, false
	}
	ordered := append([]domain.DependencyProblem(nil), problems...)
	domain.SortDependencyProblems(ordered)
	return ordered[0], true
}

// sortedDependencyNames returns the distinct containers one container depends
// on, sorted.
func sortedDependencyNames(graph domain.DependencyGraph, name string) []string {
	edges := graph.DependenciesOf(name)
	seen := make(map[string]struct{}, len(edges))
	names := make([]string, 0, len(edges))
	for _, edge := range edges {
		if _, already := seen[edge.Dependency]; already {
			continue
		}
		seen[edge.Dependency] = struct{}{}
		names = append(names, edge.Dependency)
	}
	sort.Strings(names)
	return names
}

// describeFirstCycle renders the loop this container is caught in or behind.
//
// The names come from the graph, which built them from container names
// HarborMaster read from the daemon. Callers sanitise before logging.
func describeFirstCycle(graph domain.DependencyGraph, name string) string {
	for _, cycle := range graph.Cycles {
		for _, member := range cycle {
			if member == name {
				return "dependency cycle detected: " + domain.DescribeCycle(cycle)
			}
		}
	}
	if len(graph.Cycles) > 0 {
		return "this container depends on a dependency cycle: " +
			domain.DescribeCycle(graph.Cycles[0])
	}
	return domain.DependencyCycle.Explain()
}
