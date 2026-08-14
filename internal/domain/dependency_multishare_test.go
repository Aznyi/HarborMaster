package domain_test

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// A dependent sharing SEVERAL namespaces with one provider is ONE container to
// reattach, not several.
//
// # The live failure this reproduces
//
// Stage 5c, the multi-namespace acceptance. `hm16-multidep` was created with
// both `--network container:<provider>` and `--ipc container:<provider>`.
// Discovery correctly produced two hard edges for it, one per namespace, and
// the operation recorder then built one member row per EDGE:
//
//	insert dependency operation member: constraint failed:
//	UNIQUE constraint failed: dependency_operation_members.operation_id,
//	                          dependency_operation_members.dependent_name
//
// The provider was refused before any mutation and automation paused it after
// three attempts, so the host was never touched -- the failure direction was
// correct. But the provider could never be updated at all.
//
// # Why one row per dependent is the right model
//
// A member is a CONTAINER TO REATTACH. The rebind recreates that container
// once, and the capture's namespace rewrite moves every reference it holds from
// the same resolved map in that single recreation. Two rows would describe two
// recreations of one container, which is not what happens and not what the
// table's primary key permits.

func multiShareEdges(provider string, sources ...domain.DependencySource) []domain.WorkloadDependency {
	edges := make([]domain.WorkloadDependency, 0, len(sources))
	for _, source := range sources {
		edges = append(edges, domain.WorkloadDependency{
			Dependent:  "multidep",
			Dependency: provider,
			Source:     source,
			Evidence: domain.DependencyEvidence{
				DependentContainerID:  "1111111111111111111111111111111111111111111111111111111111111111",
				DependencyContainerID: "2222222222222222222222222222222222222222222222222222222222222222",
			},
		})
	}
	return edges
}

// THE REGRESSION.
//
// Collapsing the edges of one dependent must yield exactly one entry.
func TestOneDependentSharingTwoNamespacesIsOneMember(t *testing.T) {
	t.Parallel()

	edges := multiShareEdges("provider",
		domain.DependencyNetworkNamespace, domain.DependencyIPCNamespace)

	collapsed := domain.CollapseHardDependents(edges)
	if len(collapsed) != 1 {
		t.Fatalf("collapsed %d hard edges into %d entries, want 1\n"+
			"\tA dependent sharing two namespaces with one provider is one container "+
			"to reattach. One row per edge collides on "+
			"(operation_id, dependent_name).", len(edges), len(collapsed))
	}
	if collapsed[0].Dependent != "multidep" {
		t.Fatalf("dependent = %q, want multidep", collapsed[0].Dependent)
	}
}

// Distinct dependents are never collapsed together.
//
// The non-vacuity guard: a helper that returned one entry for everything would
// satisfy the test above and lose a container that has to be reattached.
func TestDistinctDependentsAreNotCollapsed(t *testing.T) {
	t.Parallel()

	edges := []domain.WorkloadDependency{
		{Dependent: "a", Dependency: "provider", Source: domain.DependencyNetworkNamespace},
		{Dependent: "b", Dependency: "provider", Source: domain.DependencyNetworkNamespace},
		{Dependent: "c", Dependency: "provider", Source: domain.DependencyIPCNamespace},
	}

	collapsed := domain.CollapseHardDependents(edges)
	if len(collapsed) != 3 {
		t.Fatalf("collapsed %d distinct dependents into %d, want 3; a dependent "+
			"dropped here is a container left attached to a dead provider",
			len(edges), len(collapsed))
	}
}

// The collapse is deterministic and stable.
//
// The member set is persisted and compared; an unstable order would make two
// evaluations of an unchanged estate disagree.
func TestCollapsingHardDependentsIsDeterministic(t *testing.T) {
	t.Parallel()

	edges := []domain.WorkloadDependency{
		{Dependent: "zeta", Dependency: "provider", Source: domain.DependencyIPCNamespace},
		{Dependent: "alpha", Dependency: "provider", Source: domain.DependencyNetworkNamespace},
		{Dependent: "alpha", Dependency: "provider", Source: domain.DependencyIPCNamespace},
		{Dependent: "mid", Dependency: "provider", Source: domain.DependencyPIDNamespace},
	}

	first := domain.CollapseHardDependents(edges)
	second := domain.CollapseHardDependents(edges)
	if len(first) != len(second) {
		t.Fatalf("two collapses produced %d and %d entries", len(first), len(second))
	}
	for index := range first {
		if first[index].Dependent != second[index].Dependent {
			t.Fatalf("collapse is not deterministic at %d: %q vs %q",
				index, first[index].Dependent, second[index].Dependent)
		}
	}
	if len(first) != 3 {
		t.Fatalf("collapsed to %d entries, want 3 (alpha, mid, zeta)", len(first))
	}
	// Sorted, so the persisted member set has one canonical order.
	for index := 1; index < len(first); index++ {
		if first[index-1].Dependent > first[index].Dependent {
			t.Fatalf("collapsed entries are not sorted: %q before %q",
				first[index-1].Dependent, first[index].Dependent)
		}
	}
}

// A dependent whose namespaces name DIFFERENT provider ids is not collapsed
// into one member.
//
// This is the half-attached case: one namespace was reattached and another was
// not, so the container names two different ids for the same provider. One
// member row carries one expected id, so merging them would record a promise
// HarborMaster cannot keep. It must be refused instead.
func TestAHalfAttachedDependentIsRefusedRatherThanMerged(t *testing.T) {
	t.Parallel()

	edges := []domain.WorkloadDependency{
		{
			Dependent: "multidep", Dependency: "provider",
			Source: domain.DependencyNetworkNamespace,
			Evidence: domain.DependencyEvidence{
				DependencyContainerID: "2222222222222222222222222222222222222222222222222222222222222222",
			},
		},
		{
			Dependent: "multidep", Dependency: "provider",
			Source: domain.DependencyIPCNamespace,
			Evidence: domain.DependencyEvidence{
				// A DIFFERENT id for the same provider.
				DependencyContainerID: "3333333333333333333333333333333333333333333333333333333333333333",
			},
		},
	}

	if _, ok := domain.CollapseHardDependentsChecked(edges); ok {
		t.Fatal("a dependent naming two different provider ids was collapsed into " +
			"one member; one row carries one expected id, so this must be refused " +
			"rather than silently picking one and leaving the other attached to a " +
			"container that is gone")
	}

	// And the ordinary case is still accepted, so the check is not vacuous.
	if _, ok := domain.CollapseHardDependentsChecked(multiShareEdges("provider",
		domain.DependencyNetworkNamespace, domain.DependencyIPCNamespace)); !ok {
		t.Fatal("a dependent naming ONE provider id across both namespaces was refused")
	}
}
