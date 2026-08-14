package domain

import (
	"sort"
	"strconv"
)

// The dependency graph: a deterministic order over the estate.
//
// # Reading the arrow
//
// An edge is `dependent -> dependency`. Execution order is the REVERSE: the
// dependency is recreated first. Stage 1 therefore holds the containers that
// depend on NOTHING, which is the bottom of the diagram an operator draws:
//
//	postgres          <- stage 1, depends on nothing
//	   |
//	  api             <- stage 2, depends on postgres
//	   |
//	worker            <- stage 3
//
// while the stored edges are `api -> postgres` and `worker -> api`.
//
// # Determinism is a security property, not a nicety
//
// The order containers are updated in decides which ones are stopped while
// something else is broken. If that order came from Go map iteration it would
// differ between two runs over an identical estate, which means a failure could
// not be reproduced and a preview could not be trusted to describe what the
// pass would actually do.
//
// So: every input set is sorted before anything reads it, the ready set at each
// Kahn step is sorted, and no map iteration reaches any output field. The tests
// assert byte-identical output across repeated builds and across shuffled
// inputs.
//
// # Every bound fails closed
//
// A graph that exceeds a bound is INVALID -- not truncated. Truncation can only
// ever remove edges, and removing an edge makes a container look MORE
// independent and therefore SAFER than it is. That is the one direction a
// safety subsystem must never fail in, so the whole build is refused and every
// container it would have covered is left ungoverned by dependency
// orchestration, which blocks rather than clears.
//
// # Complexity
//
// One indexing pass, one in-degree pass, one Kahn sweep, one DFS over the
// residue. O(V + E) in each, with no nested scan over the node set.

// Graph bounds. Exceeding any of them refuses the build.
const (
	// MaxDependencyNodes matches store.MaxAutomationTargets: a graph covers the
	// containers a pass considers, and the two bounds would be confusing if they
	// disagreed.
	MaxDependencyNodes = 2000

	// MaxDependencyEdges allows an average of four relationships per container
	// at the node ceiling. Far above any real estate; a host that reaches it has
	// something generating relationships rather than an operator writing them.
	MaxDependencyEdges = 8000

	// MaxDependencyFanOut bounds how many containers one container may depend
	// on, and MaxDependencyFanIn how many may depend on it. A VPN provider with
	// a dozen dependents is ordinary; sixty-four is not.
	MaxDependencyFanOut = 64
	MaxDependencyFanIn  = 64
)

// DependencyBound names which ceiling a graph exceeded.
//
// A closed vocabulary rather than free text, so the API and the UI can explain
// a refusal without parsing a message.
type DependencyBound string

// Dependency bounds.
const (
	BoundNodes  DependencyBound = "nodes"
	BoundEdges  DependencyBound = "edges"
	BoundFanOut DependencyBound = "fanOut"
	BoundFanIn  DependencyBound = "fanIn"
)

// DependencyBoundError reports that the estate exceeded a graph bound.
//
// Carries the numbers so an operator can see how far past the line they are,
// and the container for the per-container bounds. The container NAME is a field
// rather than part of the message: callers render it through
// SanitiseDisplayText, and building it into the error text would put unsanitised
// input into a string that reaches a log.
type DependencyBoundError struct {
	Bound DependencyBound
	Limit int
	Found int
	// Container is set for the fan bounds only.
	Container string
}

// Error renders the refusal without naming the container.
func (e DependencyBoundError) Error() string {
	return "dependency graph exceeds the " + string(e.Bound) + " bound: found " +
		strconv.Itoa(e.Found) + ", limit " + strconv.Itoa(e.Limit)
}

// Explain renders the refusal in operator-facing words.
func (e DependencyBoundError) Explain() string {
	switch e.Bound {
	case BoundNodes:
		return "this host has more containers than HarborMaster will build a dependency graph for"
	case BoundEdges:
		return "this host has more dependency relationships than HarborMaster will order at once"
	case BoundFanOut:
		return "one container depends on more containers than HarborMaster will order"
	case BoundFanIn:
		return "more containers depend on one container than HarborMaster will order"
	default:
		return "the dependency graph exceeds a bound HarborMaster does not recognise"
	}
}

// DependencyGraph is the ordered projection over one estate.
//
// Every exported slice is sorted. The index maps are unexported so a caller
// cannot range over one and inherit map ordering by accident -- the accessors
// below return sorted copies.
type DependencyGraph struct {
	// Nodes is every container the graph covers, sorted by name.
	Nodes []string `json:"nodes"`
	// Edges is every relationship, sorted by SortDependencies.
	Edges []WorkloadDependency `json:"edges"`

	// Stages is the execution order. Stage 0 may be recreated first; every
	// container in stage N depends only on containers in stages below it.
	//
	// A container that cannot be ordered -- because a dependency is missing, or
	// because it is caught in a cycle -- appears in NO stage. Absence from the
	// stages is what stops it, and it is deliberately not represented as a
	// trailing "everything else" stage that a caller might iterate into.
	Stages [][]string `json:"stages"`

	// Cycles holds one concrete loop per cyclic component, each written as a
	// closed path: ["api","postgres","worker","api"].
	Cycles [][]string `json:"cycles,omitempty"`

	// Unresolved names the edge endpoints that match no container in Nodes.
	Unresolved []string `json:"unresolved,omitempty"`

	// dependencies indexes edges by dependent, dependents by dependency.
	dependencies map[string][]WorkloadDependency
	dependents   map[string][]WorkloadDependency

	stageOf      map[string]int
	missing      map[string]struct{}
	cycleBlocked map[string]struct{}
	known        map[string]struct{}
}

// BuildDependencyGraph builds the ordered projection.
//
// nodes is every container the graph should cover -- the present estate. edges
// is every relationship, discovered and operator-defined together. Both may
// arrive in any order; the result does not depend on it.
//
// Returns an error ONLY when a bound is exceeded. A cycle is not an error: the
// containers outside it can still be ordered, and refusing the whole estate
// because one corner of it loops would turn a local misconfiguration into a
// total stop.
func BuildDependencyGraph(nodes []string, edges []WorkloadDependency) (DependencyGraph, error) {
	graph := DependencyGraph{
		dependencies: make(map[string][]WorkloadDependency),
		dependents:   make(map[string][]WorkloadDependency),
		stageOf:      make(map[string]int),
		missing:      make(map[string]struct{}),
		cycleBlocked: make(map[string]struct{}),
		known:        make(map[string]struct{}),
	}

	// 1. The node set. Normalised, deduplicated, sorted.
	for _, name := range nodes {
		normalised := NormaliseContainerName(name)
		if normalised == "" {
			continue
		}
		graph.known[normalised] = struct{}{}
	}
	if len(graph.known) > MaxDependencyNodes {
		return DependencyGraph{}, DependencyBoundError{
			Bound: BoundNodes, Limit: MaxDependencyNodes, Found: len(graph.known),
		}
	}
	graph.Nodes = sortedKeys(graph.known)

	// 2. The edges. A relationship whose ENDS are the same container is a
	// self-dependency; it is kept rather than dropped so it surfaces as a cycle
	// an operator can see, instead of vanishing into a graph that silently
	// disagrees with the table it was built from.
	if len(edges) > MaxDependencyEdges {
		return DependencyGraph{}, DependencyBoundError{
			Bound: BoundEdges, Limit: MaxDependencyEdges, Found: len(edges),
		}
	}
	graph.Edges = append([]WorkloadDependency(nil), edges...)
	SortDependencies(graph.Edges)

	unresolved := make(map[string]struct{})
	for _, edge := range graph.Edges {
		graph.dependencies[edge.Dependent] = append(graph.dependencies[edge.Dependent], edge)
		graph.dependents[edge.Dependency] = append(graph.dependents[edge.Dependency], edge)

		// An endpoint the estate does not contain. Recorded rather than
		// dropped: an operator relationship whose target has been removed must
		// BLOCK the dependent, and dropping the edge would clear it.
		if _, ok := graph.known[edge.Dependent]; !ok {
			unresolved[edge.Dependent] = struct{}{}
		}
		if _, ok := graph.known[edge.Dependency]; !ok {
			unresolved[edge.Dependency] = struct{}{}
		}
	}
	graph.Unresolved = sortedKeys(unresolved)

	for name, list := range graph.dependencies {
		if len(list) > MaxDependencyFanOut {
			return DependencyGraph{}, DependencyBoundError{
				Bound: BoundFanOut, Limit: MaxDependencyFanOut,
				Found: len(list), Container: name,
			}
		}
	}
	for name, list := range graph.dependents {
		if len(list) > MaxDependencyFanIn {
			return DependencyGraph{}, DependencyBoundError{
				Bound: BoundFanIn, Limit: MaxDependencyFanIn,
				Found: len(list), Container: name,
			}
		}
	}

	// 3. Propagate unresolvability. A container depending on something the
	// estate does not contain cannot be ordered, and neither can anything that
	// depends on IT -- transitively, because the reason does not weaken with
	// distance.
	graph.markMissing(unresolved)

	// 4. Kahn over what remains, with a sorted ready set at every step.
	graph.orderStages()

	// 5. Whatever Kahn could not place is caught in, or behind, a cycle.
	graph.findCycles()

	return graph, nil
}

// markMissing flags every container that transitively depends on something the
// estate does not contain.
//
// A breadth-first sweep over the dependents index, which is why the index is
// built before this runs. Each container is enqueued at most once, so this is
// O(V + E) rather than a repeated scan.
func (g *DependencyGraph) markMissing(unresolved map[string]struct{}) {
	if len(unresolved) == 0 {
		return
	}

	// Seed with the KNOWN containers that reference an unresolved name. The
	// unresolved names themselves are not containers and are not marked.
	queue := make([]string, 0, len(unresolved))
	for _, name := range sortedKeys(unresolved) {
		for _, edge := range g.dependents[name] {
			if _, known := g.known[edge.Dependent]; !known {
				continue
			}
			if _, already := g.missing[edge.Dependent]; already {
				continue
			}
			g.missing[edge.Dependent] = struct{}{}
			queue = append(queue, edge.Dependent)
		}
	}

	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for _, edge := range g.dependents[name] {
			if _, known := g.known[edge.Dependent]; !known {
				continue
			}
			if _, already := g.missing[edge.Dependent]; already {
				continue
			}
			g.missing[edge.Dependent] = struct{}{}
			queue = append(queue, edge.Dependent)
		}
	}
}

// orderStages runs Kahn's algorithm over the orderable containers.
//
// # Why distinct pairs rather than edges
//
// Two containers can be related by more than one source at once -- sharing a
// network namespace AND named in an operator relationship. That is ONE ordering
// constraint expressed twice, and counting it twice would leave the dependent
// with an in-degree that never reaches zero.
func (g *DependencyGraph) orderStages() {
	// Distinct (dependent, dependency) pairs among orderable containers.
	blocks := make(map[string]map[string]struct{})
	unblocks := make(map[string]map[string]struct{})

	orderable := func(name string) bool {
		if _, known := g.known[name]; !known {
			return false
		}
		_, isMissing := g.missing[name]
		return !isMissing
	}

	for _, edge := range g.Edges {
		if !orderable(edge.Dependent) || !orderable(edge.Dependency) {
			continue
		}
		if blocks[edge.Dependent] == nil {
			blocks[edge.Dependent] = make(map[string]struct{})
		}
		blocks[edge.Dependent][edge.Dependency] = struct{}{}

		if unblocks[edge.Dependency] == nil {
			unblocks[edge.Dependency] = make(map[string]struct{})
		}
		unblocks[edge.Dependency][edge.Dependent] = struct{}{}
	}

	remaining := make(map[string]int)
	ready := make([]string, 0, len(g.Nodes))
	for _, name := range g.Nodes {
		if !orderable(name) {
			continue
		}
		count := len(blocks[name])
		remaining[name] = count
		if count == 0 {
			ready = append(ready, name)
		}
	}
	// g.Nodes is already sorted, so ready is too. Sorted again would be a
	// no-op; relying on the invariant rather than re-sorting keeps the cost
	// linear and the reason visible.

	for len(ready) > 0 {
		stage := ready
		g.Stages = append(g.Stages, stage)

		next := make([]string, 0, len(stage))
		for _, name := range stage {
			g.stageOf[name] = len(g.Stages) - 1
			// Sorted, so the containers released by this stage are appended in
			// a deterministic order and `next` needs sorting only once.
			for _, dependent := range sortedKeys(unblocks[name]) {
				remaining[dependent]--
				if remaining[dependent] == 0 {
					next = append(next, dependent)
				}
			}
		}
		sort.Strings(next)
		ready = next
	}
}

// findCycles records what Kahn could not place, and one concrete loop per
// cyclic component.
//
// Everything left over is reported as cycle-blocked, which includes containers
// that are not themselves in a loop but depend on one. Both are true statements
// -- no safe order exists for either -- and both need a person, so they share a
// state. The Cycles list is what tells the operator WHICH loop to break.
func (g *DependencyGraph) findCycles() {
	residue := make(map[string]struct{})
	for _, name := range g.Nodes {
		if _, isMissing := g.missing[name]; isMissing {
			continue
		}
		if _, placed := g.stageOf[name]; placed {
			continue
		}
		residue[name] = struct{}{}
		g.cycleBlocked[name] = struct{}{}
	}
	if len(residue) == 0 {
		return
	}

	// Iterative DFS from each residue node in sorted order, following
	// dependencies. The first node revisited while still on the current path
	// closes a loop, and the slice from it to the end of the path IS the loop.
	visited := make(map[string]struct{})
	for _, start := range sortedKeys(residue) {
		if _, seen := visited[start]; seen {
			continue
		}
		if cycle := g.walkForCycle(start, residue, visited); cycle != nil {
			g.Cycles = append(g.Cycles, cycle)
		}
	}
}

// walkForCycle follows dependencies from start until it closes a loop.
//
// Returns the loop as a closed path -- the first element repeated at the end --
// or nil when this traversal reached no loop, which happens for a container
// that merely depends on one.
func (g *DependencyGraph) walkForCycle(
	start string,
	residue map[string]struct{},
	visited map[string]struct{},
) []string {
	var path []string
	onPath := make(map[string]int)

	current := start
	for {
		if index, looping := onPath[current]; looping {
			cycle := append([]string(nil), path[index:]...)
			return append(cycle, current)
		}
		if _, seen := visited[current]; seen && current != start {
			// Already explored from a previous start; its loop, if any, was
			// reported then. Not reported twice.
			return nil
		}

		onPath[current] = len(path)
		path = append(path, current)
		visited[current] = struct{}{}

		// The lowest-sorted dependency still inside the residue. Deterministic,
		// and sufficient: every residue node has at least one such dependency,
		// which is why it is in the residue at all.
		next := ""
		for _, dependency := range sortedKeys(g.dependencySet(current)) {
			if _, inResidue := residue[dependency]; inResidue {
				next = dependency
				break
			}
		}
		if next == "" {
			return nil
		}
		current = next
	}
}

// dependencySet returns the distinct containers one container depends on.
func (g *DependencyGraph) dependencySet(name string) map[string]struct{} {
	set := make(map[string]struct{}, len(g.dependencies[name]))
	for _, edge := range g.dependencies[name] {
		set[edge.Dependency] = struct{}{}
	}
	return set
}

// ------------------------------------------------------------- accessors --

// DependenciesOf returns what this container depends on, sorted.
func (g DependencyGraph) DependenciesOf(name string) []WorkloadDependency {
	return append([]WorkloadDependency(nil), g.dependencies[NormaliseContainerName(name)]...)
}

// DependentsOf returns what depends on this container, sorted.
func (g DependencyGraph) DependentsOf(name string) []WorkloadDependency {
	return append([]WorkloadDependency(nil), g.dependents[NormaliseContainerName(name)]...)
}

// HardDependentsOf returns only the dependents the runtime itself requires.
//
// The set invariant A is about: these are the containers that break when this
// one is stopped, and every one of them has to pass rebind preflight before
// this container may be touched.
func (g DependencyGraph) HardDependentsOf(name string) []WorkloadDependency {
	var hard []WorkloadDependency
	for _, edge := range g.dependents[NormaliseContainerName(name)] {
		if edge.Hard() {
			hard = append(hard, edge)
		}
	}
	return hard
}

// StageOf returns the container's execution stage, and whether it has one.
//
// A false second return is the important case: it means the container cannot be
// ordered, and the reason is one of Missing or CycleBlocked.
func (g DependencyGraph) StageOf(name string) (int, bool) {
	stage, ok := g.stageOf[NormaliseContainerName(name)]
	return stage, ok
}

// Missing reports that the container transitively depends on something the
// estate does not contain.
func (g DependencyGraph) Missing(name string) bool {
	_, ok := g.missing[NormaliseContainerName(name)]
	return ok
}

// CycleBlocked reports that the container is caught in, or behind, a cycle.
func (g DependencyGraph) CycleBlocked(name string) bool {
	_, ok := g.cycleBlocked[NormaliseContainerName(name)]
	return ok
}

// Known reports whether the graph covers this container at all.
func (g DependencyGraph) Known(name string) bool {
	_, ok := g.known[NormaliseContainerName(name)]
	return ok
}

// Independent reports that nothing constrains this container's order.
//
// Requires the container to be KNOWN. An unknown name is not independent, it is
// unassessed, and the two must not collapse: the second is the fail-closed case.
func (g DependencyGraph) Independent(name string) bool {
	normalised := NormaliseContainerName(name)
	if _, known := g.known[normalised]; !known {
		return false
	}
	return len(g.dependencies[normalised]) == 0 && len(g.dependents[normalised]) == 0
}

// DescribeCycle renders one loop as the operator-facing sentence.
//
// The caller is responsible for sanitising the names it passes through; this
// joins them in the order the loop runs.
func DescribeCycle(cycle []string) string {
	if len(cycle) == 0 {
		return ""
	}
	out := cycle[0]
	for _, name := range cycle[1:] {
		out += " -> " + name
	}
	return out
}

// sortedKeys returns a map's keys in sorted order.
//
// Every place the graph turns a map into a slice goes through this. That is the
// whole of the determinism guarantee, and it is one function so a reviewer can
// check it once.
func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
