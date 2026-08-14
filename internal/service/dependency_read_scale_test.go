package service_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The READ paths the dependency management page uses, at supported scale.
//
// # What is being established
//
// The page issues two requests: the relationship listing and the order preview.
// Both are answered from the same two indexed queries, and the claim worth
// pinning is that the number of queries does NOT depend on how many containers
// or relationships exist. That is the difference between a projection and an
// N+1, and it is not visible in a timing: an N+1 over a 25-container estate is
// fast too.
//
// So the counts are ASSERTED and the timings are merely reported. A regression
// that turned the graph build into a per-container lookup would fail here at 25
// containers, long before anybody noticed it on a real host.
//
// # No Docker call and no registry call, at any size
//
// Structural rather than measured: DependencyOptions has nowhere to put a Docker
// capability or a registry client, which TestDependencyServiceHoldsNoDockerCapability
// pins by reflection. The store below is the ONLY collaborator this service has,
// so a read that reached anything else could not be served at all.

// countingDependencyStore records how often each query runs.
//
// Its own fixture rather than the shared fakeDependencyStore: the property under
// test is a call count, and sharing a fixture with tests that call it for other
// reasons would make the count mean something else.
type countingDependencyStore struct {
	mu        sync.Mutex
	rows      []domain.ContainerNamespaceRow
	endpoints []domain.DependencyEndpoint
	operator  []domain.WorkloadDependency

	namespaceCalls int
	endpointCalls  int
	operatorCalls  int
	countCalls     int
	nameCalls      int
}

func (s *countingDependencyStore) NamespaceRows(context.Context) ([]domain.ContainerNamespaceRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.namespaceCalls++
	return append([]domain.ContainerNamespaceRow(nil), s.rows...), nil
}

func (s *countingDependencyStore) Endpoints(context.Context) ([]domain.DependencyEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpointCalls++
	return append([]domain.DependencyEndpoint(nil), s.endpoints...), nil
}

// NameForContainerID is a point lookup, and is counted like every other round
// trip so the scale assertions below stay honest if a read path starts using it.
//
// It answers from the endpoint fixture; these tests never exercise a replaced
// container, so the retention behaviour the real repository has is modelled in
// fakeDependencyStore instead.
func (s *countingDependencyStore) NameForContainerID(_ context.Context, containerID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nameCalls++
	for _, endpoint := range s.endpoints {
		if endpoint.ContainerID == containerID {
			return endpoint.Name, nil
		}
	}
	return "", store.ErrNotFound
}

func (s *countingDependencyStore) OperatorDependencies(context.Context) ([]domain.WorkloadDependency, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.operatorCalls++
	return append([]domain.WorkloadDependency(nil), s.operator...), nil
}

func (s *countingDependencyStore) OperatorDependencyCount(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.countCalls++
	return len(s.operator), nil
}

func (s *countingDependencyStore) Get(context.Context, string) (domain.WorkloadDependency, error) {
	return domain.WorkloadDependency{}, store.ErrNotFound
}

func (s *countingDependencyStore) Create(
	context.Context, domain.WorkloadDependency, time.Time,
) (domain.WorkloadDependency, error) {
	return domain.WorkloadDependency{}, store.ErrNotFound
}

func (s *countingDependencyStore) Delete(context.Context, string) error { return store.ErrNotFound }

// queries returns the total number of store round trips so far.
func (s *countingDependencyStore) queries() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.namespaceCalls + s.endpointCalls + s.operatorCalls + s.countCalls + s.nameCalls
}

// scaleContainerID builds a distinct 64-hex container id.
//
// The full form matters: ParseNamespaceContainerRef accepts exactly 64 lowercase
// hex characters, so a shortened id would produce no discovered relationship at
// all and the estate would silently be smaller than it looks.
func scaleContainerID(index int) string { return fmt.Sprintf("%064x", index+1) }

// readScaleEstate builds the same sparse shape the decision-pass scale test uses.
//
// One fan-out provider with sixteen namespace dependents, plus short operator
// chains over roughly a tenth of the estate. A complete graph is not a bigger
// real estate, it is a different shape.
func readScaleEstate(n int) *countingDependencyStore {
	fixture := &countingDependencyStore{}

	names := make([]string, 0, n)
	for i := range n {
		names = append(names, fmt.Sprintf("c%05d", i))
	}

	for i, name := range names {
		row := domain.ContainerNamespaceRow{
			ContainerID: scaleContainerID(i),
			Name:        name,
			// Observed is the fail-closed flag: false means HarborMaster has not
			// read this container's configuration, which is a REFUSAL rather than
			// an absence of relationships.
			Modes: domain.NamespaceModes{Observed: true},
		}
		// The gluetun shape: sixteen containers riding the first one's network.
		if n > 20 && i >= 1 && i <= 16 {
			row.Modes.Network = "container:" + scaleContainerID(0)
		}
		fixture.rows = append(fixture.rows, row)
		fixture.endpoints = append(fixture.endpoints, domain.DependencyEndpoint{
			Name:        name,
			ContainerID: scaleContainerID(i),
			ImageRef:    "ghcr.io/acme/" + name + ":1",
			Present:     true,
		})
	}

	for i := 20; i+2 < n && i < n/10*3+20; i += 3 {
		fixture.operator = append(fixture.operator,
			operatorDep(names[i+1], names[i]),
			operatorDep(names[i+2], names[i+1]))
	}
	return fixture
}

// The listing, the order preview, and one container's view, at 25/500/2,000.
func TestDependencyReadsCostAFixedNumberOfQueries(t *testing.T) {
	t.Parallel()

	for _, size := range []int{25, 500, 2000} {
		t.Run(fmt.Sprintf("%d containers", size), func(t *testing.T) {
			t.Parallel()

			fixture := readScaleEstate(size)
			svc := service.NewDependencyService(service.DependencyOptions{
				Store: fixture,
			})
			ctx := context.Background()

			// ---- the relationship listing -------------------------------
			started := time.Now()
			listing, err := svc.List(ctx)
			listElapsed := time.Since(started)
			if err != nil {
				t.Fatalf("List at %d: %v", size, err)
			}
			listQueries := fixture.queries()
			if listQueries != 2 {
				t.Fatalf("List cost %d store queries, want exactly 2\n"+
					"\ta listing is a projection over the namespace rows and the "+
					"operator table. A count that grows with the estate is an N+1.",
					listQueries)
			}

			// ---- the order preview --------------------------------------
			started = time.Now()
			graph, err := svc.Graph(ctx)
			graphElapsed := time.Since(started)
			if err != nil {
				t.Fatalf("Graph at %d: %v", size, err)
			}
			if got := fixture.queries() - listQueries; got != 2 {
				t.Fatalf("Graph cost %d store queries, want exactly 2", got)
			}

			// ---- one container's view -----------------------------------
			started = time.Now()
			_, err = svc.ForContainer(ctx, "c00001")
			containerElapsed := time.Since(started)
			if err != nil {
				t.Fatalf("ForContainer at %d: %v", size, err)
			}
			if got := fixture.queries() - listQueries - 2; got != 2 {
				t.Fatalf("ForContainer cost %d store queries, want exactly 2", got)
			}

			// The endpoint listing is NOT read on any of the three. It is only
			// needed to validate a write or to assess a rebind, and a read path
			// that pulled it would be doing work the page never uses.
			if fixture.endpointCalls != 0 {
				t.Errorf("the read paths issued %d endpoint queries, want 0",
					fixture.endpointCalls)
			}

			// ---- the estate really is the size claimed ------------------
			//
			// Without this the query-count assertions above would hold just as
			// well over an empty graph, and the test would pass vacuously at
			// every size.
			if len(graph.Stages) == 0 {
				t.Fatal("the graph produced no stages; the fixture is not an estate")
			}
			expectEdges := 0
			if size > 20 {
				expectEdges = 16
			}
			expectEdges += len(fixture.operator)
			if len(listing.Edges) != expectEdges {
				t.Fatalf("listing has %d relationships, want %d",
					len(listing.Edges), expectEdges)
			}
			if len(listing.Problems) != 0 {
				t.Fatalf("the fixture produced %d discovery refusals; it should be "+
					"a clean estate", len(listing.Problems))
			}

			// Timings are indicative, not asserted: they run on whatever machine
			// the suite runs on. An order-of-magnitude regression would show.
			t.Logf("%4d containers, %3d relationships: list %v, graph %v, container %v",
				size, len(listing.Edges), listElapsed, graphElapsed, containerElapsed)
		})
	}
}

// The page's summary needs no query of its own.
//
// It is five counts over the listing the page already holds, which is why there
// is no aggregate endpoint here to add one. This asserts the absence: the
// numbers the summary shows are all derivable from a single List.
func TestTheDependencySummaryIsDerivableFromOneListing(t *testing.T) {
	t.Parallel()

	fixture := readScaleEstate(500)
	svc := service.NewDependencyService(service.DependencyOptions{Store: fixture})

	listing, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if fixture.queries() != 2 {
		t.Fatalf("List cost %d queries", fixture.queries())
	}

	var detected, configured int
	for _, edge := range listing.Edges {
		if edge.Source == domain.DependencyOperator {
			configured++
			continue
		}
		detected++
	}
	if detected+configured != len(listing.Edges) {
		t.Fatal("an edge belonged to neither origin")
	}
	if detected == 0 || configured == 0 {
		t.Fatalf("detected=%d configured=%d; the fixture does not exercise both",
			detected, configured)
	}
	// And the problem count, which the summary also shows, arrives on the same
	// response rather than needing a second read.
	_ = listing.Problems
}

// An estate past the node bound refuses rather than answering with a subset.
//
// The fail-closed direction: a truncated graph makes containers look MORE
// independent than they are, which is the one way a safety projection must never
// be wrong.
func TestDependencyReadsRefusePastTheNodeBound(t *testing.T) {
	t.Parallel()

	fixture := readScaleEstate(domain.MaxDependencyNodes + 1)
	svc := service.NewDependencyService(service.DependencyOptions{Store: fixture})
	ctx := context.Background()

	if _, err := svc.List(ctx); err == nil {
		t.Error("List answered for an estate past the node bound")
	}
	if _, err := svc.Graph(ctx); err == nil {
		t.Error("Graph answered for an estate past the node bound")
	}
	if _, err := svc.ForContainer(ctx, "c00001"); err == nil {
		t.Error("ForContainer answered for an estate past the node bound")
	}
}
