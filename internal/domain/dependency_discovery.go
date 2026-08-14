package domain

import (
	"sort"
	"time"
)

// Deriving the hard runtime dependencies from what Docker actually configured.
//
// # A pure function over inventory rows
//
// Discovery reads NOTHING. It is handed the namespace facts the inventory
// already recorded and returns the relationships they imply, which means it can
// be tested exhaustively without a host and cannot become a second place that
// talks to Docker.
//
// # Resolution is by id, through HarborMaster's own records
//
// Docker persists a shared namespace as `container:` plus the full 64-hex id it
// resolved at create time -- verified against Docker 29.6.2, and documented on
// ParseNamespaceContainerRef. So resolution is an id lookup against the
// inventory, and the relationship is then recorded under the resolved
// container's NAME.
//
// At no point is a container name parsed, guessed, or matched by shape. The
// phase brief forbids it and it would be wrong anyway: the daemon bound one
// specific container, and two containers can hold the same name over time.
//
// # Every failure is a REFUSAL, never an omission
//
// This is the part that matters. A container declaring `container:` in a
// namespace mode has told HarborMaster it shares somebody's namespace. If the
// reference cannot be resolved, the honest answer is "HarborMaster cannot
// establish what this depends on" -- not "this depends on nothing".
//
// Returning no edge would make the container look INDEPENDENT and let it be
// updated in any order, which is the failure direction a safety subsystem may
// never take. So every unresolvable reference produces a PROBLEM, the problem
// blocks the container, and the caller can say exactly which one and why.

// DiscoveryRefusal is why a namespace reference produced no relationship.
//
// A closed vocabulary. Each value is a distinct sentence to an operator asking
// "why is HarborMaster refusing to order this container".
type DiscoveryRefusal string

// Discovery refusals.
const (
	// DiscoveryUnobserved means the container's namespace facts have not been
	// read from the daemon since they became something HarborMaster records.
	//
	// The state every row is in immediately after migration 0024, and the
	// reason that migration is safe: an unobserved container blocks rather than
	// reads as independent.
	DiscoveryUnobserved DiscoveryRefusal = "namespacesUnobserved"

	// DiscoveryUnparseableRef means the mode declares a shared namespace whose
	// reference is not a full container id.
	//
	// Not something a daemon HarborMaster understands produces. It blocks
	// rather than being ignored, because the container HAS said it shares
	// something.
	DiscoveryUnparseableRef DiscoveryRefusal = "referenceNotParseable"

	// DiscoveryUnknownContainer means the reference resolved to an id the
	// inventory has no present container for.
	//
	// The state a dependent lands in after its provider was recreated: the id
	// it names is the replaced container, which is gone. This is precisely the
	// signal a rebind is required.
	DiscoveryUnknownContainer DiscoveryRefusal = "referencedContainerUnknown"

	// DiscoveryUnnamedContainer means the referenced container was found but
	// carries no name HarborMaster can key a relationship on.
	DiscoveryUnnamedContainer DiscoveryRefusal = "referencedContainerUnnamed"
)

// DiscoveryRefusals lists every refusal.
var DiscoveryRefusals = []DiscoveryRefusal{
	DiscoveryUnobserved,
	DiscoveryUnparseableRef,
	DiscoveryUnknownContainer,
	DiscoveryUnnamedContainer,
}

// ValidDiscoveryRefusal reports whether value names a refusal this build knows.
func ValidDiscoveryRefusal(value string) bool {
	for _, refusal := range DiscoveryRefusals {
		if string(refusal) == value {
			return true
		}
	}
	return false
}

// Explain renders the refusal in HarborMaster's own words.
//
// Never interpolates the reference, the id, or any daemon text. The identifiers
// live on the problem record, where a caller sanitises them.
func (r DiscoveryRefusal) Explain() string {
	switch r {
	case DiscoveryUnobserved:
		return "HarborMaster has not read this container's namespace configuration since " +
			"it began recording it; the next inventory refresh will establish it"
	case DiscoveryUnparseableRef:
		return "this container shares another container's namespace, but the reference " +
			"is not one HarborMaster can resolve"
	case DiscoveryUnknownContainer:
		return "this container shares the namespace of a container that is no longer " +
			"present, so it is bound to a namespace that has been replaced or removed"
	case DiscoveryUnnamedContainer:
		return "this container shares the namespace of a container HarborMaster cannot " +
			"identify by name"
	default:
		return "HarborMaster could not establish what this container depends on"
	}
}

// RebindSignal reports whether the refusal is the specific evidence that a
// dependent has been left bound to a replaced namespace.
//
// Only DiscoveryUnknownContainer is. The others are states of not knowing;
// this one is a state of knowing that the binding is stale.
func (r DiscoveryRefusal) RebindSignal() bool {
	return r == DiscoveryUnknownContainer
}

// ContainerNamespaceRow is one container's namespace facts, as the inventory
// recorded them.
//
// The narrow projection discovery runs on. It carries no image, no labels, and
// no configuration: a relationship is derived from namespace modes and identity
// alone, and a wider row would invite a future check to read something it has
// no business reading.
type ContainerNamespaceRow struct {
	ContainerID string
	Name        string
	Modes       NamespaceModes
	// ObservedAt is when the inventory row was written.
	ObservedAt time.Time
}

// DependencyProblem is a relationship HarborMaster could not establish.
//
// Distinct from a relationship that does not exist. This type IS the difference,
// and a caller that ignores it turns every fail-closed refusal into a silent
// clearance.
type DependencyProblem struct {
	// Container is the DEPENDENT: the container that declared the share.
	Container string `json:"container"`
	// Source is which namespace, or empty when the whole row is unobserved.
	Source DependencySource `json:"source,omitempty"`
	// ReferencedID is the id the mode named, when one could be parsed. Evidence
	// for an operator; never used to decide anything.
	ReferencedID string           `json:"referencedId,omitempty"`
	Refusal      DiscoveryRefusal `json:"refusal"`
}

// DiscoverDependencies derives the hard runtime relationships.
//
// Returns the edges it could establish and the problems it could not, both
// sorted. A container appearing in the problem list must be treated as
// dependency-blocked whatever the edge list says about it -- the two results are
// read together or not at all.
//
// One pass to index by id, one pass to resolve: O(V) with no nested scan.
func DiscoverDependencies(rows []ContainerNamespaceRow) ([]WorkloadDependency, []DependencyProblem) {
	// Index every row by id. Built from the SAME slice the resolution loop
	// reads, so a reference can only ever resolve to a container in this
	// estate -- there is no lookup that reaches outside it.
	byID := make(map[string]ContainerNamespaceRow, len(rows))
	for _, row := range rows {
		if row.ContainerID == "" {
			continue
		}
		byID[row.ContainerID] = row
	}

	var (
		edges    []WorkloadDependency
		problems []DependencyProblem
	)

	for _, row := range rows {
		name := NormaliseContainerName(row.Name)
		if name == "" {
			// A container with no usable name cannot be an endpoint. It is not
			// silently skipped: it is reported, because a workload HarborMaster
			// cannot name is one it cannot order.
			problems = append(problems, DependencyProblem{
				Container: row.Name,
				Refusal:   DiscoveryUnnamedContainer,
			})
			continue
		}

		// The whole row is unscreened. Reported once rather than three times:
		// the operator's action is the same for all three namespaces, and one
		// sentence is the useful one.
		if !row.Modes.Observed {
			problems = append(problems, DependencyProblem{
				Container: name,
				Refusal:   DiscoveryUnobserved,
			})
			continue
		}

		for _, pair := range row.Modes.SourceFor() {
			// The overwhelmingly common case: no shared namespace at all. This
			// is the ONLY branch that produces neither an edge nor a problem,
			// and it is reached only when the mode does not claim a share.
			if !SharesNamespace(pair.Mode) {
				continue
			}

			referenced, ok := ParseNamespaceContainerRef(pair.Mode)
			if !ok {
				problems = append(problems, DependencyProblem{
					Container: name,
					Source:    pair.Source,
					Refusal:   DiscoveryUnparseableRef,
				})
				continue
			}

			target, found := byID[referenced]
			if !found {
				problems = append(problems, DependencyProblem{
					Container:    name,
					Source:       pair.Source,
					ReferencedID: referenced,
					Refusal:      DiscoveryUnknownContainer,
				})
				continue
			}

			targetName := NormaliseContainerName(target.Name)
			if targetName == "" {
				problems = append(problems, DependencyProblem{
					Container:    name,
					Source:       pair.Source,
					ReferencedID: referenced,
					Refusal:      DiscoveryUnnamedContainer,
				})
				continue
			}

			edges = append(edges, WorkloadDependency{
				Dependent:  name,
				Dependency: targetName,
				Source:     pair.Source,
				Evidence: DependencyEvidence{
					DependentContainerID:  row.ContainerID,
					DependencyContainerID: target.ContainerID,
					ObservedAt:            row.ObservedAt,
				},
			})
		}
	}

	SortDependencies(edges)
	SortDependencyProblems(problems)
	return edges, problems
}

// SortDependencyProblems orders problems deterministically.
func SortDependencyProblems(problems []DependencyProblem) {
	sort.SliceStable(problems, func(i, j int) bool {
		a, b := problems[i], problems[j]
		if a.Container != b.Container {
			return a.Container < b.Container
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Refusal < b.Refusal
	})
}

// ProblemsByContainer indexes problems for a per-container lookup.
//
// Returned as a map because the caller does a point lookup per container; every
// LIST this package returns is sorted, and this is not one.
func ProblemsByContainer(problems []DependencyProblem) map[string][]DependencyProblem {
	byContainer := make(map[string][]DependencyProblem, len(problems))
	for _, problem := range problems {
		byContainer[problem.Container] = append(byContainer[problem.Container], problem)
	}
	return byContainer
}
