package domain_test

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// edge builds an operator relationship for a test.
func edge(dependent, dependency string) domain.WorkloadDependency {
	return domain.WorkloadDependency{
		Dependent:  dependent,
		Dependency: dependency,
		Source:     domain.DependencyOperator,
	}
}

// hardEdge builds a network-namespace relationship for a test.
func hardEdge(dependent, dependency string) domain.WorkloadDependency {
	return domain.WorkloadDependency{
		Dependent:  dependent,
		Dependency: dependency,
		Source:     domain.DependencyNetworkNamespace,
	}
}

// The chain from the phase brief:
//
//	postgres -> api -> worker -> frontend
//
// stored as `api -> postgres`, `worker -> api`, `frontend -> worker`, and
// ordered postgres first.
func TestStagesInvertTheArrow(t *testing.T) {
	t.Parallel()

	graph, err := domain.BuildDependencyGraph(
		[]string{"frontend", "worker", "api", "postgres"},
		[]domain.WorkloadDependency{
			edge("api", "postgres"),
			edge("worker", "api"),
			edge("frontend", "worker"),
		})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	want := [][]string{{"postgres"}, {"api"}, {"worker"}, {"frontend"}}
	if !reflect.DeepEqual(graph.Stages, want) {
		t.Fatalf("stages = %v, want %v", graph.Stages, want)
	}
}

// Independent chains share a stage rather than serialising.
//
// The phase brief's parallelism case: A -> B and C -> D means B and D may go
// together, then A and C. A graph that produced four stages here would be
// correct but would have thrown away every opportunity to overlap.
func TestIndependentChainsShareStages(t *testing.T) {
	t.Parallel()

	graph, err := domain.BuildDependencyGraph(
		[]string{"a", "b", "c", "d"},
		[]domain.WorkloadDependency{edge("a", "b"), edge("c", "d")})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	want := [][]string{{"b", "d"}, {"a", "c"}}
	if !reflect.DeepEqual(graph.Stages, want) {
		t.Fatalf("stages = %v, want %v", graph.Stages, want)
	}
}

// The determinism guarantee, stated as a test.
//
// # Why shuffling is the real check
//
// Building the same graph twice would pass even if the implementation ranged
// over a map, because Go's map ordering is randomised per ITERATION but a
// single run's two builds could coincidentally agree. Shuffling the INPUT is
// what proves the output is a function of the set rather than of the order it
// arrived in -- which is the property an operator relies on when a preview
// claims to describe what the next pass will do.
func TestGraphOutputIsIndependentOfInputOrder(t *testing.T) {
	t.Parallel()

	nodes := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta"}
	edges := []domain.WorkloadDependency{
		edge("beta", "alpha"),
		edge("gamma", "alpha"),
		edge("delta", "beta"),
		edge("delta", "gamma"),
		edge("epsilon", "delta"),
		hardEdge("zeta", "alpha"),
		edge("eta", "zeta"),
	}

	baseline, err := domain.BuildDependencyGraph(nodes, edges)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	random := rand.New(rand.NewSource(1))
	for attempt := range 200 {
		shuffledNodes := append([]string(nil), nodes...)
		random.Shuffle(len(shuffledNodes), func(i, j int) {
			shuffledNodes[i], shuffledNodes[j] = shuffledNodes[j], shuffledNodes[i]
		})
		shuffledEdges := append([]domain.WorkloadDependency(nil), edges...)
		random.Shuffle(len(shuffledEdges), func(i, j int) {
			shuffledEdges[i], shuffledEdges[j] = shuffledEdges[j], shuffledEdges[i]
		})

		got, err := domain.BuildDependencyGraph(shuffledNodes, shuffledEdges)
		if err != nil {
			t.Fatalf("attempt %d: build: %v", attempt, err)
		}
		if !reflect.DeepEqual(got.Stages, baseline.Stages) {
			t.Fatalf("attempt %d: stages = %v, want %v", attempt, got.Stages, baseline.Stages)
		}
		if !reflect.DeepEqual(got.Nodes, baseline.Nodes) {
			t.Fatalf("attempt %d: nodes = %v, want %v", attempt, got.Nodes, baseline.Nodes)
		}
		if !reflect.DeepEqual(got.Edges, baseline.Edges) {
			t.Fatalf("attempt %d: edge order differs", attempt)
		}
	}
}

// Every cycle shape the phase brief names must be refused, and none may be
// broken automatically.
func TestCyclesAreDetectedAndNeverBroken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		nodes   []string
		edges   []domain.WorkloadDependency
		blocked []string
	}{
		{
			name:    "self dependency",
			nodes:   []string{"a"},
			edges:   []domain.WorkloadDependency{edge("a", "a")},
			blocked: []string{"a"},
		},
		{
			name:    "two node loop",
			nodes:   []string{"a", "b"},
			edges:   []domain.WorkloadDependency{edge("a", "b"), edge("b", "a")},
			blocked: []string{"a", "b"},
		},
		{
			name:  "three node loop",
			nodes: []string{"a", "b", "c"},
			edges: []domain.WorkloadDependency{
				edge("a", "b"), edge("b", "c"), edge("c", "a"),
			},
			blocked: []string{"a", "b", "c"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			graph, err := domain.BuildDependencyGraph(testCase.nodes, testCase.edges)
			if err != nil {
				// A cycle is DATA, not an error: containers outside it must
				// still be orderable.
				t.Fatalf("build returned an error for a cycle: %v", err)
			}
			if len(graph.Cycles) == 0 {
				t.Fatal("no cycle was reported")
			}
			for _, name := range testCase.blocked {
				if !graph.CycleBlocked(name) {
					t.Errorf("%q is not cycle-blocked", name)
				}
				if _, ordered := graph.StageOf(name); ordered {
					t.Errorf("%q was given an execution stage despite the cycle", name)
				}
			}
			// The loop is reported as a closed path an operator can read.
			cycle := graph.Cycles[0]
			if len(cycle) < 2 || cycle[0] != cycle[len(cycle)-1] {
				t.Errorf("cycle %v is not a closed path", cycle)
			}
		})
	}
}

// A cycle in one corner of the estate must not stop the rest of it.
func TestACycleDoesNotBlockUnrelatedContainers(t *testing.T) {
	t.Parallel()

	graph, err := domain.BuildDependencyGraph(
		[]string{"a", "b", "healthy", "alsoHealthy"},
		[]domain.WorkloadDependency{
			edge("a", "b"), edge("b", "a"),
			edge("alsoHealthy", "healthy"),
		})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if graph.CycleBlocked("healthy") || graph.CycleBlocked("alsoHealthy") {
		t.Fatal("a cycle blocked containers that are not part of it")
	}
	want := [][]string{{"healthy"}, {"alsoHealthy"}}
	if !reflect.DeepEqual(graph.Stages, want) {
		t.Fatalf("stages = %v, want %v", graph.Stages, want)
	}
}

// A container behind a cycle cannot be ordered either, and says so.
func TestAContainerBehindACycleIsBlocked(t *testing.T) {
	t.Parallel()

	// a <-> b is the loop; c depends on a, so c has no safe position either.
	graph, err := domain.BuildDependencyGraph(
		[]string{"a", "b", "c"},
		[]domain.WorkloadDependency{
			edge("a", "b"), edge("b", "a"), edge("c", "a"),
		})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if !graph.CycleBlocked("c") {
		t.Fatal("a container depending on a cycle was not blocked")
	}
	if _, ordered := graph.StageOf("c"); ordered {
		t.Fatal("a container depending on a cycle was given a stage")
	}
}

// The fail-closed rule for an endpoint the estate does not contain.
//
// The dependent must be BLOCKED, not cleared. Dropping the edge would be the
// one failure direction a safety subsystem may never take.
func TestAnUnresolvedDependencyBlocksTheDependent(t *testing.T) {
	t.Parallel()

	graph, err := domain.BuildDependencyGraph(
		[]string{"api"},
		[]domain.WorkloadDependency{edge("api", "postgres")})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if !graph.Missing("api") {
		t.Fatal("a container depending on an absent container was not marked missing")
	}
	if _, ordered := graph.StageOf("api"); ordered {
		t.Fatal("a container with an unresolvable dependency was given a stage")
	}
	if len(graph.Unresolved) != 1 || graph.Unresolved[0] != "postgres" {
		t.Fatalf("unresolved = %v, want [postgres]", graph.Unresolved)
	}
}

// Unresolvability propagates: the reason does not weaken with distance.
func TestMissingPropagatesToEveryDependent(t *testing.T) {
	t.Parallel()

	graph, err := domain.BuildDependencyGraph(
		[]string{"api", "worker", "frontend"},
		[]domain.WorkloadDependency{
			edge("api", "postgres"), // postgres is absent
			edge("worker", "api"),
			edge("frontend", "worker"),
		})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	for _, name := range []string{"api", "worker", "frontend"} {
		if !graph.Missing(name) {
			t.Errorf("%q was not marked missing", name)
		}
	}
	if len(graph.Stages) != 0 {
		t.Fatalf("stages = %v, want none", graph.Stages)
	}
}

// Two sources expressing the same ordering are ONE constraint.
//
// A container sharing a namespace with something an operator also named would
// otherwise have an in-degree that never reaches zero, and would never be
// scheduled at all.
func TestDuplicatePairsFromDifferentSourcesCountOnce(t *testing.T) {
	t.Parallel()

	graph, err := domain.BuildDependencyGraph(
		[]string{"sonarr", "gluetun"},
		[]domain.WorkloadDependency{
			hardEdge("sonarr", "gluetun"),
			edge("sonarr", "gluetun"),
		})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	want := [][]string{{"gluetun"}, {"sonarr"}}
	if !reflect.DeepEqual(graph.Stages, want) {
		t.Fatalf("stages = %v, want %v", graph.Stages, want)
	}
}

// Every bound refuses the whole build rather than truncating it.
func TestBoundsFailClosed(t *testing.T) {
	t.Parallel()

	t.Run("nodes", func(t *testing.T) {
		t.Parallel()
		nodes := make([]string, domain.MaxDependencyNodes+1)
		for i := range nodes {
			nodes[i] = "c" + strconv.Itoa(i)
		}
		_, err := domain.BuildDependencyGraph(nodes, nil)
		assertBound(t, err, domain.BoundNodes)
	})

	t.Run("edges", func(t *testing.T) {
		t.Parallel()
		edges := make([]domain.WorkloadDependency, domain.MaxDependencyEdges+1)
		for i := range edges {
			edges[i] = edge("a"+strconv.Itoa(i), "b"+strconv.Itoa(i))
		}
		_, err := domain.BuildDependencyGraph([]string{"a0"}, edges)
		assertBound(t, err, domain.BoundEdges)
	})

	t.Run("fan out", func(t *testing.T) {
		t.Parallel()
		var edges []domain.WorkloadDependency
		nodes := []string{"hub"}
		for i := range domain.MaxDependencyFanOut + 1 {
			name := "dep" + strconv.Itoa(i)
			nodes = append(nodes, name)
			edges = append(edges, edge("hub", name))
		}
		_, err := domain.BuildDependencyGraph(nodes, edges)
		assertBound(t, err, domain.BoundFanOut)
	})

	t.Run("fan in", func(t *testing.T) {
		t.Parallel()
		var edges []domain.WorkloadDependency
		nodes := []string{"provider"}
		for i := range domain.MaxDependencyFanIn + 1 {
			name := "dependent" + strconv.Itoa(i)
			nodes = append(nodes, name)
			edges = append(edges, hardEdge(name, "provider"))
		}
		_, err := domain.BuildDependencyGraph(nodes, edges)
		assertBound(t, err, domain.BoundFanIn)
	})
}

func assertBound(t *testing.T, err error, want domain.DependencyBound) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected the %s bound to refuse the build", want)
	}
	var bound domain.DependencyBoundError
	if !errors.As(err, &bound) {
		t.Fatalf("error %v is not a DependencyBoundError", err)
	}
	if bound.Bound != want {
		t.Fatalf("bound = %q, want %q", bound.Bound, want)
	}
	if bound.Explain() == "" {
		t.Fatal("the refusal has no operator-facing explanation")
	}
}

// An unknown name is not independent. It is unassessed, and the difference is
// the fail-closed case.
func TestAnUnknownContainerIsNotIndependent(t *testing.T) {
	t.Parallel()

	graph, err := domain.BuildDependencyGraph([]string{"known"}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if !graph.Independent("known") {
		t.Fatal("a container with no relationships is not independent")
	}
	if graph.Independent("never-heard-of-it") {
		t.Fatal("an unknown container reported as independent")
	}
	if graph.Known("never-heard-of-it") {
		t.Fatal("an unknown container reported as known")
	}
}

// HardDependentsOf must return only what the RUNTIME requires, because that is
// the set invariant A gates a provider on.
func TestHardDependentsExcludeOperatorRelationships(t *testing.T) {
	t.Parallel()

	graph, err := domain.BuildDependencyGraph(
		[]string{"gluetun", "sonarr", "reporting"},
		[]domain.WorkloadDependency{
			hardEdge("sonarr", "gluetun"),
			edge("reporting", "gluetun"),
		})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	hard := graph.HardDependentsOf("gluetun")
	if len(hard) != 1 || hard[0].Dependent != "sonarr" {
		t.Fatalf("hard dependents = %v, want only sonarr", hard)
	}
	if all := graph.DependentsOf("gluetun"); len(all) != 2 {
		t.Fatalf("all dependents = %v, want two", all)
	}
}

// Names are normalised on the way in, so the Docker leading slash cannot split
// one container into two nodes.
func TestNamesAreNormalisedIntoOneNode(t *testing.T) {
	t.Parallel()

	graph, err := domain.BuildDependencyGraph(
		[]string{"/api", "api", "/postgres"},
		[]domain.WorkloadDependency{edge("api", "postgres")})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if len(graph.Nodes) != 2 {
		t.Fatalf("nodes = %v, want two", graph.Nodes)
	}
	if graph.Missing("api") {
		t.Fatal("normalisation failed to match the two spellings")
	}
}

// The graph must stay O(V+E) at the scale section 16 names.
func TestGraphScalesToTheNodeBound(t *testing.T) {
	t.Parallel()

	for _, size := range []int{25, 500, 2000} {
		t.Run(strconv.Itoa(size)+" containers", func(t *testing.T) {
			t.Parallel()

			nodes := make([]string, size)
			var edges []domain.WorkloadDependency
			for i := range size {
				nodes[i] = fmt.Sprintf("c%05d", i)
				// A chain of pairs, so the graph has real depth rather than
				// being a flat set that any implementation orders trivially.
				if i%2 == 1 {
					edges = append(edges, hardEdge(
						fmt.Sprintf("c%05d", i), fmt.Sprintf("c%05d", i-1)))
				}
			}

			graph, err := domain.BuildDependencyGraph(nodes, edges)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if len(graph.Nodes) != size {
				t.Fatalf("nodes = %d, want %d", len(graph.Nodes), size)
			}
			if len(graph.Cycles) != 0 {
				t.Fatalf("cycles = %v, want none", graph.Cycles)
			}
			placed := 0
			for _, stage := range graph.Stages {
				placed += len(stage)
			}
			if placed != size {
				t.Fatalf("placed %d containers, want %d", placed, size)
			}
		})
	}
}

// DescribeCycle renders the loop the way the phase brief asks for.
func TestDescribeCycleReadsAsAPath(t *testing.T) {
	t.Parallel()

	got := domain.DescribeCycle([]string{"a", "b", "c", "a"})
	if got != "a -> b -> c -> a" {
		t.Fatalf("described %q", got)
	}
	if domain.DescribeCycle(nil) != "" {
		t.Fatal("an empty cycle should describe as empty")
	}
}
