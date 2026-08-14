package service

import (
	"context"
	"errors"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The operator-facing dependency operations: read the estate, record an
// ordering, remove one.
//
// # None of this can change a container
//
// Every read is a projection over the inventory and one small table. The two
// writes touch the operator relationship table and nothing else. There is no
// method here that pulls, creates, stops, renames, or removes anything, and no
// parameter anywhere that carries an image, a digest, a registry, or a container
// id -- the create takes two container NAMES.
//
// # Creating is conservative; deleting is not
//
// A relationship can only make HarborMaster wait or refuse, so recording one is
// the safe direction. REMOVING one takes a safety constraint away, which is why
// both sit behind an administrator-only permission.

// ErrDependencyRefused reports that a proposed relationship was not accepted.
//
// Carries the closed-vocabulary refusal so the API reports which check said no
// rather than a generic failure -- matching ErrExecutionRefused.
type ErrDependencyRefused struct {
	Refusal domain.DependencyRefusal
}

func (e ErrDependencyRefused) Error() string { return e.Refusal.Explain() }

// DependencyListing is every relationship in force, discovered and operator.
type DependencyListing struct {
	// Edges are all relationships, sorted.
	Edges []domain.WorkloadDependency `json:"edges"`
	// Problems are the namespace shares HarborMaster could not resolve. Read
	// TOGETHER with the edges or not at all: a container appearing here is
	// dependency-blocked whatever the edge list says about it.
	Problems []domain.DependencyProblem `json:"problems,omitempty"`
	// Unresolved names edge endpoints the estate does not contain.
	Unresolved []string `json:"unresolved,omitempty"`
}

// List returns every relationship in force.
func (s *DependencyService) List(ctx context.Context) (DependencyListing, error) {
	view, err := s.View(ctx)
	if err != nil {
		return DependencyListing{}, err
	}

	listing := DependencyListing{
		Edges:      view.Graph.Edges,
		Unresolved: view.Graph.Unresolved,
	}
	for _, container := range sortedProblemContainers(view.Problems) {
		listing.Problems = append(listing.Problems, view.Problems[container]...)
	}
	return listing, nil
}

// ContainerDependencies is one container's two directions.
type ContainerDependencies struct {
	Container string `json:"container"`
	// DependsOn is what this container needs stable first.
	DependsOn []domain.WorkloadDependency `json:"dependsOn"`
	// DependedOnBy is what needs THIS container stable first.
	DependedOnBy []domain.WorkloadDependency `json:"dependedOnBy"`

	// Stage is this container's position in the execution order, when it has
	// one. Absent means it cannot be ordered -- see State.
	Stage *int `json:"stage,omitempty"`
	// State is the dependency verdict for this container.
	State domain.DependencyState `json:"state"`
	// Detail is HarborMaster's own sentence for that state.
	Detail string `json:"detail"`

	// Problems are the namespace shares HarborMaster could not resolve for this
	// container.
	Problems []domain.DependencyProblem `json:"problems,omitempty"`
	// OutstandingRebinds are the reattachments this container owes, if any.
	OutstandingRebinds []domain.DependencyMember `json:"outstandingRebinds,omitempty"`
}

// ForContainer returns one container's relationships in both directions.
//
// Takes a container NAME. The API resolves an id to a name before calling, so
// the two ways of addressing a container agree about which one is meant.
func (s *DependencyService) ForContainer(
	ctx context.Context,
	name string,
) (ContainerDependencies, error) {
	view, err := s.View(ctx)
	if err != nil {
		return ContainerDependencies{}, err
	}

	normalised := domain.NormaliseContainerName(name)
	if !view.Graph.Known(normalised) {
		return ContainerDependencies{}, store.ErrNotFound
	}

	result := ContainerDependencies{
		Container:    normalised,
		DependsOn:    view.Graph.DependenciesOf(normalised),
		DependedOnBy: view.Graph.DependentsOf(normalised),
		Problems:     view.Problems[normalised],
	}
	if stage, ordered := view.Graph.StageOf(normalised); ordered {
		result.Stage = &stage
	}

	// The state, from the same vocabulary the automation gate uses. Computed
	// here from the graph alone -- this is a READ, and it consults no policy and
	// no automation decision.
	switch {
	case len(result.Problems) > 0:
		result.State = domain.DependencyMissing
	case view.Graph.CycleBlocked(normalised):
		result.State = domain.DependencyCycle
	case view.Graph.Missing(normalised):
		result.State = domain.DependencyMissing
	default:
		result.State = domain.DependencySatisfied
	}
	result.Detail = result.State.Explain()

	if s.operations != nil {
		members, err := s.operations.MembersForDependent(ctx, normalised)
		if err == nil {
			for _, member := range members {
				if !member.State.Clears() && !member.State.Settled() {
					result.OutstandingRebinds = append(result.OutstandingRebinds, member)
				}
			}
		}
	}
	return result, nil
}

// DependencyGraphProjection is the estate's order, for the preview.
type DependencyGraphProjection struct {
	// Stages is the execution order. Stage 0 may go first.
	Stages [][]string `json:"stages"`
	// Cycles are the loops that must be broken, each a closed path.
	Cycles [][]string `json:"cycles,omitempty"`
	// CycleDescriptions render each loop as an operator-facing sentence.
	CycleDescriptions []string `json:"cycleDescriptions,omitempty"`
	// Unresolved names edge endpoints the estate does not contain.
	Unresolved []string `json:"unresolved,omitempty"`
	// Blocked names containers that cannot be ordered, with why.
	Blocked map[string]string `json:"blocked,omitempty"`
	// Edges are the relationships the order was derived from.
	Edges []domain.WorkloadDependency `json:"edges"`
}

// Graph returns the deterministic order over the estate.
//
// A PROJECTION over work that is already valid. It creates no plan, approves
// nothing, and bypasses nothing: reading it changes no record and touches no
// Docker socket.
func (s *DependencyService) Graph(ctx context.Context) (DependencyGraphProjection, error) {
	view, err := s.View(ctx)
	if err != nil {
		return DependencyGraphProjection{}, err
	}

	projection := DependencyGraphProjection{
		Stages:     view.Graph.Stages,
		Cycles:     view.Graph.Cycles,
		Unresolved: view.Graph.Unresolved,
		Edges:      view.Graph.Edges,
	}
	for _, cycle := range view.Graph.Cycles {
		projection.CycleDescriptions = append(projection.CycleDescriptions,
			domain.DescribeCycle(cycle))
	}

	// Every container that has no stage, and the reason. A container absent
	// from the stages is one HarborMaster will not order, and saying so beats
	// leaving a reader to notice the absence.
	for _, name := range view.Graph.Nodes {
		if _, ordered := view.Graph.StageOf(name); ordered {
			continue
		}
		if projection.Blocked == nil {
			projection.Blocked = make(map[string]string)
		}
		switch {
		case view.Graph.CycleBlocked(name):
			projection.Blocked[name] = domain.DependencyCycle.Explain()
		default:
			projection.Blocked[name] = domain.DependencyMissing.Explain()
		}
	}
	return projection, nil
}

// CreateOperatorDependency records an ordering an operator asserted.
//
// Takes two container NAMES and nothing else. Validation runs against the live
// estate -- both endpoints must exist, be present, not be preserved, not be
// HarborMaster, not duplicate an existing relationship from ANY source, and not
// close a cycle -- and the cycle check builds the graph the write WOULD produce
// rather than reasoning pairwise.
func (s *DependencyService) CreateOperatorDependency(
	ctx context.Context,
	dependent, dependency string,
	requestedBy domain.Requester,
) (domain.WorkloadDependency, error) {
	if !s.Readable() {
		return domain.WorkloadDependency{}, ErrDependencyGraphUnavailable
	}

	endpoints, err := s.store.Endpoints(ctx)
	if err != nil {
		return domain.WorkloadDependency{}, err
	}
	view, err := s.View(ctx)
	if err != nil {
		return domain.WorkloadDependency{}, err
	}
	count, err := s.store.OperatorDependencyCount(ctx)
	if err != nil {
		return domain.WorkloadDependency{}, err
	}

	edge, refusal := domain.ValidateOperatorDependency(domain.DependencyValidationInput{
		Dependent:  dependent,
		Dependency: dependency,
		// Never taken from the request. The only storable source is operator,
		// and the validator refuses anything else by name.
		Source:        domain.DependencyOperator,
		Containers:    domain.EndpointsFromNames(endpoints),
		Existing:      view.Graph.Edges,
		OperatorCount: count,
		Self:          s.selfIdentity(),
	})
	if refusal != domain.DependencyRefusalNone {
		return domain.WorkloadDependency{}, ErrDependencyRefused{Refusal: refusal}
	}

	edge.CreatedBy = requestedBy
	created, err := s.store.Create(ctx, edge, s.now())
	if err != nil {
		// The unique index caught a race the validation above could not close.
		// A refusal, not a fault.
		if errors.Is(err, store.ErrDependencyExists) {
			return domain.WorkloadDependency{},
				ErrDependencyRefused{Refusal: domain.DependencyRefusalDuplicate}
		}
		return domain.WorkloadDependency{}, err
	}
	return created, nil
}

// DeleteOperatorDependency removes an ordering an operator recorded.
//
// Reaches OPERATOR rows only, and that is structural rather than checked: a
// discovered relationship is derived from the inventory on every read and has no
// stored row, so there is no id a caller could name. An id that matches nothing
// is ErrNotFound, which is also what a malformed id gets -- a caller must not be
// able to learn which ids exist by observing a different outcome.
func (s *DependencyService) DeleteOperatorDependency(
	ctx context.Context,
	dependencyID string,
) (domain.WorkloadDependency, error) {
	if !s.Readable() {
		return domain.WorkloadDependency{}, ErrDependencyGraphUnavailable
	}

	// Read first, so the audit record can name what was removed. A delete that
	// reported only an id would leave the log unable to answer "which ordering
	// stopped being enforced".
	existing, err := s.store.Get(ctx, dependencyID)
	if err != nil {
		return domain.WorkloadDependency{}, err
	}
	if err := s.store.Delete(ctx, dependencyID); err != nil {
		return domain.WorkloadDependency{}, err
	}
	return existing, nil
}
