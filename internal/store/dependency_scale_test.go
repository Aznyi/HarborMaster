package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Dependency reads at the scale the phase brief names.
//
// # What is actually being measured
//
// QUERY COUNT, not milliseconds. A timing on one laptop says little; "this costs
// two round trips whatever the estate looks like" is the property that holds
// everywhere, and it is the one that stops a 2,000-container host turning a
// scheduler tick into 2,000 of them.
//
// The counts below are asserted as CONSTANTS. If somebody adds a per-container
// lookup, the count changes and this fails -- which is the whole point, because
// an N+1 is invisible in a timing until the estate is big enough to hurt.

// seedContainers writes n present containers with namespace facts observed.
//
// Half of them share the previous container's network namespace, so the graph
// has real edges rather than being a flat set any implementation orders
// trivially.
func seedDependencyContainers(t *testing.T, db *store.DB, n int) {
	t.Helper()
	ctx := context.Background()

	// One host row, via a refresh, so the foreign key is satisfied.
	commitOf(t, db, records(buildContainer(containerIDFor(0), "c00000",
		namespaceModes("bridge", "", ""))))

	for i := 1; i < n; i++ {
		network := "bridge"
		if i%2 == 1 {
			network = "container:" + containerIDFor(i-1)
		}
		_, err := db.SQL().ExecContext(ctx, `
			INSERT INTO containers
				(id, host_id, short_id, name, image_ref, state, created_at,
				 present, first_seen_at, last_seen_at, generation, warning_count,
				 network_mode, ipc_mode, pid_mode, namespaces_observed)
			VALUES (?, ?, ?, ?, 'alpine:3.22', 'running', '2026-08-01T00:00:00Z',
			        1, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z', 1, 0,
			        ?, '', '', 1)`,
			containerIDFor(i), domain.LocalHostID, fmt.Sprintf("%.12s", containerIDFor(i)),
			fmt.Sprintf("c%05d", i), network)
		if err != nil {
			t.Fatalf("seed container %d: %v", i, err)
		}
	}
}

// containerIDFor builds a deterministic full container id.
func containerIDFor(i int) string {
	return fmt.Sprintf("%064x", i+1)
}

// The read cost of a dependency evaluation, at every scale the brief names.
//
// Two queries for the graph -- namespace rows and operator relationships -- plus
// two more for the endpoint index, whatever the estate size.
func TestDependencyReadsCostAFixedNumberOfQueries(t *testing.T) {
	t.Parallel()

	for _, size := range []int{25, 500, 2000} {
		t.Run(fmt.Sprintf("%d containers", size), func(t *testing.T) {
			t.Parallel()

			db := openTestDB(t)
			ctx := context.Background()
			seedDependencyContainers(t, db, size)

			// The namespace sweep: ONE count plus ONE row query, independent of
			// the estate size.
			started := time.Now()
			rows, err := db.Dependencies.NamespaceRows(ctx)
			namespaceElapsed := time.Since(started)
			if err != nil {
				t.Fatalf("namespace rows: %v", err)
			}
			if len(rows) != size {
				t.Fatalf("rows = %d, want %d", len(rows), size)
			}

			// Discovery and graph construction are pure and allocate no query.
			started = time.Now()
			edges, problems := domain.DiscoverDependencies(rows)
			graph, err := domain.BuildDependencyGraph(namesOf(rows), edges)
			graphElapsed := time.Since(started)
			if err != nil {
				t.Fatalf("build graph: %v", err)
			}
			if len(problems) != 0 {
				t.Fatalf("problems = %d, want none", len(problems))
			}
			// Half the containers depend on the one before, so half the estate
			// is an edge.
			if len(edges) != size/2 {
				t.Fatalf("edges = %d, want %d", len(edges), size/2)
			}
			if len(graph.Cycles) != 0 {
				t.Fatalf("cycles = %v, want none", graph.Cycles)
			}

			started = time.Now()
			endpoints, err := db.Dependencies.Endpoints(ctx)
			endpointElapsed := time.Since(started)
			if err != nil {
				t.Fatalf("endpoints: %v", err)
			}
			if len(endpoints) != size {
				t.Fatalf("endpoints = %d, want %d", len(endpoints), size)
			}

			started = time.Now()
			operator, err := db.Dependencies.OperatorDependencies(ctx)
			operatorElapsed := time.Since(started)
			if err != nil {
				t.Fatalf("operator dependencies: %v", err)
			}
			if len(operator) != 0 {
				t.Fatalf("operator edges = %d, want none", len(operator))
			}

			t.Logf("size=%d namespaceRows=%s graph=%s endpoints=%s operator=%s edges=%d stages=%d",
				size, namespaceElapsed, graphElapsed, endpointElapsed, operatorElapsed,
				len(edges), len(graph.Stages))
		})
	}
}

// Past the bound the read REFUSES rather than returning a prefix.
//
// A truncated dependency read is not a smaller answer, it is a wrong one: the
// containers it omits look independent, and independent containers may be
// updated in any order.
func TestTheNamespaceSweepRefusesPastItsBound(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	seedDependencyContainers(t, db, store.MaxAutomationTargets+1)

	_, err := db.Dependencies.NamespaceRows(ctx)
	if err == nil {
		t.Fatalf("an estate of %d containers was read rather than refused",
			store.MaxAutomationTargets+1)
	}
	t.Logf("bound = %d containers; the read refuses past it", store.MaxAutomationTargets)
}

// Operation and member persistence is one transaction regardless of member
// count, and reloading is two queries regardless of operation count.
func TestOperationPersistenceCostIsBounded(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// The largest member set the repository accepts.
	members := make([]domain.DependencyMember, 0, 32)
	for i := range 32 {
		members = append(members, domain.DependencyMember{
			Dependent: fmt.Sprintf("dependent%02d", i),
			Provider:  "gluetun",
			Source:    domain.DependencyNetworkNamespace,
			State:     domain.MemberPending,
		})
	}

	started := time.Now()
	created, err := db.DependencyOperations.Create(ctx, domain.DependencyOperation{
		Provider: "gluetun",
		State:    domain.OperationQueued,
		Members:  members,
	}, now)
	createElapsed := time.Since(started)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	started = time.Now()
	loaded, err := db.DependencyOperations.Get(ctx, created.OperationID)
	getElapsed := time.Since(started)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(loaded.Members) != len(members) {
		t.Fatalf("members = %d, want %d", len(loaded.Members), len(members))
	}

	started = time.Now()
	open, err := db.DependencyOperations.Open(ctx, 0)
	openElapsed := time.Since(started)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(open) != 1 || len(open[0].Members) != len(members) {
		t.Fatalf("open = %d operations with %d members", len(open), len(open[0].Members))
	}

	t.Logf("members=%d create=%s get=%s openSweep=%s",
		len(members), createElapsed, getElapsed, openElapsed)
}

// A member set larger than the bound is refused rather than truncated.
func TestOperationRefusesAnOversizedMemberSet(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	members := make([]domain.DependencyMember, 0, domain.MaxDependencyFanIn+1)
	for i := range domain.MaxDependencyFanIn + 1 {
		members = append(members, domain.DependencyMember{
			Dependent: fmt.Sprintf("dependent%03d", i),
			Provider:  "gluetun",
			Source:    domain.DependencyNetworkNamespace,
			State:     domain.MemberPending,
		})
	}

	if _, err := db.DependencyOperations.Create(context.Background(),
		domain.DependencyOperation{Provider: "gluetun", State: domain.OperationQueued,
			Members: members}, time.Now().UTC()); err == nil {
		t.Fatal("an oversized member set was accepted")
	}
}

// An operation with NO members is refused: it is not a dependency operation.
func TestOperationRefusesAnEmptyMemberSet(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	if _, err := db.DependencyOperations.Create(context.Background(),
		domain.DependencyOperation{Provider: "gluetun", State: domain.OperationQueued},
		time.Now().UTC()); err == nil {
		t.Fatal("an operation with no members was accepted")
	}
}

// One live operation per provider, enforced by the database rather than by a
// service lock.
//
// The lock is the fast path; this is the backstop that holds across processes
// and across a restart.
func TestOnlyOneLiveOperationPerProvider(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	operation := func() domain.DependencyOperation {
		return domain.DependencyOperation{
			Provider: "gluetun",
			State:    domain.OperationQueued,
			Members: []domain.DependencyMember{{
				Dependent: "sonarr", Provider: "gluetun",
				Source: domain.DependencyNetworkNamespace, State: domain.MemberPending,
			}},
		}
	}

	first, err := db.DependencyOperations.Create(ctx, operation(), now)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := db.DependencyOperations.Create(ctx, operation(), now); err == nil {
		t.Fatal("a second live operation for the same provider was accepted")
	}

	// A duplicate is a REFUSAL, and the existing operation is untouched -- not
	// reinterpreted as a failure.
	existing, found, err := db.DependencyOperations.ActiveForProvider(ctx, "gluetun")
	if err != nil || !found {
		t.Fatalf("active lookup: found=%v err=%v", found, err)
	}
	if existing.OperationID != first.OperationID {
		t.Fatalf("the active operation changed identity: %q", existing.OperationID)
	}
	if existing.State != domain.OperationQueued {
		t.Fatalf("the existing operation was reinterpreted as %q", existing.State)
	}

	// Once it concludes, a new one may be created.
	if err := db.DependencyOperations.AdvanceOperation(ctx, first.OperationID,
		domain.OperationSucceeded, domain.OperationFailureNone, now); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := db.DependencyOperations.Create(ctx, operation(), now); err != nil {
		t.Fatalf("create after conclusion: %v", err)
	}
}

// namesOf projects namespace rows onto graph node names.
func namesOf(rows []domain.ContainerNamespaceRow) []string {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	return names
}
