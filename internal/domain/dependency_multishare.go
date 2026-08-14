package domain

import (
	"sort"
	"strings"
)

// Collapsing several shared namespaces onto one container to reattach.
//
// # The distinction this file exists to keep
//
// Discovery works in EDGES: a container declaring `network_mode: container:X`
// and `ipc: container:X` shares two namespaces with X and produces two hard
// relationships, which is correct -- each is a separate fact about the runtime.
//
// A dependency operation works in MEMBERS, and a member is a CONTAINER TO
// REATTACH. The rebind recreates that container once, and the capture's
// namespace rewrite moves every reference it holds in that single recreation.
// So two edges for one dependent are one member.
//
// Stage 5c found the consequence of conflating them: the operation recorder
// built a row per edge and the insert collided on the member table's own
// primary key, (operation_id, dependent_name). The provider was refused before
// any mutation -- the safe direction -- but could never be updated at all.
//
// # Why the half-attached case is refused rather than merged
//
// A member row carries ONE expected provider id: the id the dependent names
// today, which the rebind is going to move away from. If a dependent's two
// namespaces name two DIFFERENT ids for the same provider, it is already
// half-attached -- one namespace was reattached at some point and the other was
// not. Merging them would mean recording an expectation for one namespace and
// silently leaving the other pointing at a container that is gone, which is
// exactly the breakage the operation record exists to prevent. So it is refused
// and an operator is told.

// CollapseHardDependents reduces hard relationships to one entry per dependent.
//
// Returns the edges sorted by dependent name, with a single representative edge
// for each. Deterministic: the member set is persisted and re-derived, and two
// evaluations of an unchanged estate must agree.
//
// The representative is the first edge in source order, which is stable because
// the input is sorted before the choice is made. Which source is recorded on
// the member row is EVIDENCE only -- the rebind moves every namespace the
// dependent shares, from the resolved map, not from this field.
func CollapseHardDependents(edges []WorkloadDependency) []WorkloadDependency {
	collapsed, _ := CollapseHardDependentsChecked(edges)
	return collapsed
}

// CollapseHardDependentsChecked is CollapseHardDependents with the
// half-attached refusal reported.
//
// Returns ok=false when one dependent's edges name different provider container
// ids. A caller that gets false must refuse the operation: there is no member
// row that can honestly describe that dependent.
func CollapseHardDependentsChecked(edges []WorkloadDependency) ([]WorkloadDependency, bool) {
	if len(edges) == 0 {
		return nil, true
	}

	// Sorted first, so the representative chosen for each dependent does not
	// depend on the order discovery happened to produce.
	ordered := append([]WorkloadDependency(nil), edges...)
	SortDependencies(ordered)

	seen := make(map[string]int, len(ordered))
	collapsed := make([]WorkloadDependency, 0, len(ordered))
	consistent := true

	for _, edge := range ordered {
		name := NormaliseContainerName(edge.Dependent)
		if name == "" {
			continue
		}
		index, already := seen[name]
		if !already {
			seen[name] = len(collapsed)
			collapsed = append(collapsed, edge)
			continue
		}

		// A second namespace shared with the same provider. The ids must agree:
		// see the note above on the half-attached case.
		first := strings.TrimSpace(collapsed[index].Evidence.DependencyContainerID)
		second := strings.TrimSpace(edge.Evidence.DependencyContainerID)
		if first != "" && second != "" && !strings.EqualFold(first, second) {
			consistent = false
		}
	}

	sort.SliceStable(collapsed, func(i, j int) bool {
		return NormaliseContainerName(collapsed[i].Dependent) <
			NormaliseContainerName(collapsed[j].Dependent)
	})
	return collapsed, consistent
}
