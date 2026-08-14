package service

import (
	"context"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The dependency facts a container LIST needs, gathered once for the whole page.
//
// # Why this exists rather than a per-container call
//
// Rendering a list must not become a reason to issue a query per row. The
// dependency picture is derived from two indexed reads and one bounded sweep,
// whatever the page size and whatever the estate size, and this is the seam that
// keeps it that way: the API asks ONCE and indexes the answer by name.
//
// # It is a display read, and it can refuse
//
// Nothing here touches Docker, a registry, or a mutation capability -- the
// service it hangs off holds none. When the graph cannot be built this returns
// an error and the caller renders the estate exactly as it did before
// dependencies existed: no claim either way. That is the correct direction for a
// DISPLAY path. The safety decisions are made elsewhere, by components that fail
// closed on the same condition.

// DependencyAttentionReader is the bounded member sweep the attention read
// needs. Nil disables the rebind half, which reports nothing rather than
// guessing.
type DependencyAttentionReader interface {
	UnsettledMembers(ctx context.Context, limit int) ([]domain.DependencyMember, error)
}

// DependencyFacts is one container's dependency picture, for a list row.
type DependencyFacts struct {
	// State is the graph-derived verdict. Always set when this struct exists.
	State domain.DependencyState
	// BlockedBy names the container responsible, when one is.
	BlockedBy string

	// RebindFailed marks a mandatory reattachment that settled without
	// succeeding, RebindPending one still in flight. Both name the provider.
	RebindFailed   bool
	RebindPending  bool
	RebindProvider string
}

// AttentionFacts returns the dependency picture for every container it covers.
//
// Keyed on the normalised container NAME, because that is what survives a
// recreation and what every relationship is keyed on.
//
// Cost: two indexed queries for the graph plus one bounded sweep for the
// outstanding reattachments. Constant in the number of containers rendered.
func (s *DependencyService) AttentionFacts(
	ctx context.Context,
) (map[string]DependencyFacts, error) {
	view, err := s.View(ctx)
	if err != nil {
		return nil, err
	}

	facts := make(map[string]DependencyFacts, len(view.Graph.Nodes))
	for _, name := range view.Graph.Nodes {
		state := domain.DependencySatisfied
		blockedBy := ""

		switch {
		case len(view.Problems[name]) > 0:
			// A declared namespace share HarborMaster could not resolve. The
			// container is blocked whatever the edges say, and this must never
			// read as "nothing to wait for".
			state = domain.DependencyMissing
		case view.Graph.CycleBlocked(name):
			state = domain.DependencyCycle
		case view.Graph.Missing(name):
			state = domain.DependencyMissing
			// Name the first unresolvable dependency, so a row can say which.
			for _, edge := range view.Graph.DependenciesOf(name) {
				if !view.Graph.Known(edge.Dependency) {
					blockedBy = edge.Dependency
					break
				}
			}
		}

		facts[name] = DependencyFacts{State: state, BlockedBy: blockedBy}
	}

	// The reattachments. Absent reader means the recreation pipeline is not
	// wired in this deployment, so there are no operations to report.
	if s.operations == nil {
		return facts, nil
	}
	reader, ok := s.operations.(DependencyAttentionReader)
	if !ok {
		return facts, nil
	}

	// The graph half is still correct and still worth returning. A failed sweep
	// means "no reattachment is reported", which UNDERSTATES rather than
	// invents -- and the container's own detail page reads the operations
	// directly, so nothing is lost that an operator cannot reach.
	//
	// The error is deliberately dropped rather than returned: propagating it
	// would take the whole dependency picture down over one bounded read, and
	// this is a display path. Every safety decision is made elsewhere by
	// components that fail closed on the same condition.
	members, err := reader.UnsettledMembers(ctx, 0)
	if err != nil {
		//nolint:nilerr // See above: understating beats losing the whole picture.
		return facts, nil
	}

	for _, member := range members {
		name := domain.NormaliseContainerName(member.Dependent)
		row := facts[name]
		row.RebindProvider = member.Provider
		if member.State.Settled() {
			// Settled without clearing: HarborMaster stopped and will not
			// retry. The louder of the two, so it wins if a container somehow
			// carries both.
			row.RebindFailed = true
		} else {
			row.RebindPending = true
		}
		facts[name] = row
	}
	return facts, nil
}

// DependencySummary is the estate's dependency picture as a handful of counts.
//
// # Only what nothing resolves by itself
//
// There is deliberately no "waiting" count. Waiting is the system working: it
// clears without anybody, and a dashboard that reported it would be teaching an
// operator that this list contains things which do not need them.
type DependencySummary struct {
	// Cycles is the number of loops. Nothing breaks one automatically.
	Cycles int `json:"cycles"`
	// Unresolved is the number of containers whose declared namespace share
	// HarborMaster could not resolve. A refusal, not an absence.
	Unresolved int `json:"unresolved"`
	// RebindsFailed is the number of reattachments that settled without
	// succeeding. HarborMaster never retries one.
	RebindsFailed int `json:"rebindsFailed"`
	// RebindsPending is reported for completeness and is NOT attention: a
	// reattachment in flight is work, not a condition.
	RebindsPending int `json:"rebindsPending"`
}

// Summarise counts the dependency conditions worth an operator's attention.
//
// Derived from the same facts a container list reads, so the dashboard count
// and the row badges cannot disagree.
func Summarise(facts map[string]DependencyFacts) DependencySummary {
	summary := DependencySummary{}
	// Cycle membership is per-container here rather than per-loop: this is
	// derived from the same per-container facts the list rows use, and counting
	// containers is the number an operator can act on anyway.
	for _, row := range facts {
		switch row.State {
		case domain.DependencyCycle:
			summary.Cycles++
		case domain.DependencyMissing:
			summary.Unresolved++
		}
		if row.RebindFailed {
			summary.RebindsFailed++
		}
		if row.RebindPending {
			summary.RebindsPending++
		}
	}
	return summary
}

// DependencyOperationReader is the bounded summary read.
//
// Separate from the recovery store interface because it is a DISPLAY read and
// nothing else: a component holding only this can list what happened and cannot
// advance, retry, or conclude anything.
type DependencyOperationReader interface {
	Recent(ctx context.Context, limit int) ([]domain.DependencyOperation, error)
}

// DependencyOperationSummary is one coordinated provider update, for reading.
//
// # What "overall" means here, and why it is derived
//
// `Complete` is provider verified AND every mandatory rebind verified. It is
// computed from the execution records every time rather than read from the
// stored state, for the same reason restart recovery is: a stored flag is a
// claim about the past, and the execution record is the evidence.
//
// A failed operation is reported with the provider and the successful rebinds
// still marked as they are, because that is the truth of the host. HarborMaster
// does not roll a dependency group backward, and a summary that hid the
// successful half would misdescribe what an operator would find.
type DependencyOperationSummary struct {
	Operation domain.DependencyOperation `json:"operation"`
	// ProviderVerified is derived from the provider's execution record.
	ProviderVerified bool `json:"providerVerified"`
	// Complete is provider verified AND every mandatory rebind verified.
	Complete bool `json:"complete"`
	// NeedsAttention marks an operation nothing will progress on its own.
	NeedsAttention bool `json:"needsAttention"`
}

// RecentOperations returns the coordinated provider updates worth reading.
//
// A READ. It creates no plan, submits nothing, and reaches no Docker socket --
// the service it hangs off holds no capability that could.
//
// Bounded twice: the store returns at most fifty operations in two queries, and
// each one's completeness is derived from records already loaded plus one
// execution lookup per member. That per-member lookup is the reason the listing
// is capped rather than paged.
func (s *DependencyService) RecentOperations(
	ctx context.Context,
	limit int,
) ([]DependencyOperationSummary, error) {
	if s == nil || s.operations == nil {
		return nil, nil
	}
	reader, ok := s.operations.(DependencyOperationReader)
	if !ok {
		return nil, nil
	}

	operations, err := reader.Recent(ctx, limit)
	if err != nil {
		return nil, err
	}

	summaries := make([]DependencyOperationSummary, 0, len(operations))
	for _, operation := range operations {
		recovered := s.recoverOne(ctx, operation)
		summaries = append(summaries, DependencyOperationSummary{
			Operation:        recovered.Operation,
			ProviderVerified: recovered.ProviderVerified,
			Complete:         recovered.Complete,
			// Anything that is not complete and is not still moving. A
			// half-finished group is exactly what a person has to settle,
			// because nothing else will.
			NeedsAttention: !recovered.Complete && !recovered.NeedsWork(),
		})
	}
	return summaries, nil
}

// Evidence fills the attention evidence for one container from these facts.
//
// A method rather than a free function so the DependencyKnown flag cannot be
// set without the state that justifies it -- the whole point of the flag is
// that its zero value asserts nothing.
func (f DependencyFacts) Evidence(evidence *domain.ContainerEvidence) {
	evidence.DependencyKnown = true
	evidence.DependencyState = f.State
	evidence.DependencyBlockedBy = f.BlockedBy
	evidence.RebindFailed = f.RebindFailed
	evidence.RebindPending = f.RebindPending
	evidence.RebindProvider = f.RebindProvider
}
