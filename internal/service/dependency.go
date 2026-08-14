package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The dependency service: a reader, a resolver, and nothing else.
//
// # It holds no Docker capability
//
// Look at DependencyOptions. None of the capability interfaces from
// internal/docker appears there -- not the read-only runtime, not the image
// acquirer, not the container mutator, not the rollbacker. This service cannot
// pull, create, stop, rename, or remove anything, and it cannot ask a service
// that can: there is no AutomationPipeline here either.
//
// Its whole output is EVIDENCE. It answers three questions -- what depends on
// what, can these dependents be reattached, and which live container now holds
// this namespace -- and hands the answers to components that already own their
// own capability and re-run their own preflight.
//
// TestDependencyServiceHoldsNoDockerCapability pins this by reflection, and
// TestNoDependencyFileNamesAMutationCapability pins it by name.
//
// # Discovered relationships are derived on every read
//
// There is no cache and no stored copy. The namespace projection on the
// inventory IS the evidence, it is refreshed on the same schedule as everything
// else, and deriving from it means there is no second thing that can be stale.
// A graph costs two indexed queries.

// ErrDependencyGraphUnavailable reports that the estate could not be ordered.
//
// Returned rather than degraded. Every caller treats it as a refusal, because a
// graph that could not be built establishes nothing -- and "nothing established"
// must never read as "nothing constrains this container".
var ErrDependencyGraphUnavailable = errors.New("the dependency graph could not be built")

// DependencyStore is the persistence the service reads and writes.
//
// Narrow by construction: namespace facts and endpoints are READS, and the only
// writes are the operator relationship table. Nothing here can change a
// container.
type DependencyStore interface {
	NamespaceRows(ctx context.Context) ([]domain.ContainerNamespaceRow, error)
	Endpoints(ctx context.Context) ([]domain.DependencyEndpoint, error)
	OperatorDependencies(ctx context.Context) ([]domain.WorkloadDependency, error)
	OperatorDependencyCount(ctx context.Context) (int, error)
	Get(ctx context.Context, dependencyID string) (domain.WorkloadDependency, error)
	Create(ctx context.Context, edge domain.WorkloadDependency, now time.Time) (domain.WorkloadDependency, error)
	Delete(ctx context.Context, dependencyID string) error
}

// DependencyLineage reports what a container is ACTUALLY running.
//
// A rebind is pinned to the digest HarborMaster observed, so this is the one
// input that decides whether a dependent can be repaired at all. An
// unestablished digest is a refusal, never a fallback to the tag.
type DependencyLineage interface {
	RunningDigestFor(ctx context.Context, containerID string) (reference string, digest string, err error)
}

// DependencyContainers is the inventory read the lineage adapter needs.
type DependencyContainers interface {
	Get(ctx context.Context, containerID string) (*domain.ContainerDetail, error)
}

// NewDependencyLineage adapts the container repository to DependencyLineage.
//
// Reads the container's normalised detail and returns the reference it declares
// alongside the digest RunningDigestFor resolved for it.
//
// The distinction matters and is the reason this is not one field: the declared
// reference is frequently a mutable tag, while RunningDigest is the immutable
// artefact actually executing. A rebind proposes the reference AND pins the
// digest, so recreating cannot silently move the container onto whatever that
// tag points at today.
//
// An EMPTY digest is returned as empty rather than substituted. That refuses the
// rebind, blocks the provider, and is the correct direction: a container
// HarborMaster cannot pin is one it must not recreate.
func NewDependencyLineage(containers DependencyContainers) DependencyLineage {
	return dependencyLineage{containers: containers}
}

type dependencyLineage struct{ containers DependencyContainers }

func (l dependencyLineage) RunningDigestFor(
	ctx context.Context,
	containerID string,
) (string, string, error) {
	if l.containers == nil {
		return "", "", store.ErrNotFound
	}
	detail, err := l.containers.Get(ctx, containerID)
	if err != nil {
		return "", "", err
	}
	if detail == nil {
		return "", "", store.ErrNotFound
	}
	return detail.Overview.Image.Raw, detail.RunningDigest, nil
}

// DependencyOptions configures a DependencyService.
//
// Note what is absent: every Docker capability interface, and the automation
// pipeline. There is nowhere to wire one.
type DependencyOptions struct {
	Store   DependencyStore
	Lineage DependencyLineage
	Self    SelfReporter
	Audit   *AuditRecorder
	Logger  *slog.Logger
	Now     func() time.Time

	// Operations persists coordinated provider updates. Nil disables operation
	// tracking, which a deployment without the recreation pipeline gets.
	Operations DependencyOperationStore
	// Executions is the READ of HarborMaster's own execution records that
	// restart recovery derives member state from. Nothing on it can start,
	// retry, or cancel anything.
	Executions DependencyExecutions
	// Plans persists the rebind change plans the coordinator produces. Writing
	// a plan causes nothing to happen: a plan is an assessment, and acting on
	// one goes through the existing acquisition and execution services.
	Plans DependencyPlans

	// Notifier reports a container that could not be reattached. Nil in a
	// deployment with notifications off, which is the default.
	//
	// One method, no error return, and it cannot reach Docker. A notifier is
	// not a capability: it is somewhere to say a sentence.
	Notifier Notifier
}

// DependencyService answers dependency questions from HarborMaster's records.
type DependencyService struct {
	store   DependencyStore
	lineage DependencyLineage
	self    SelfReporter
	audit   *AuditRecorder
	logger  *slog.Logger
	now     func() time.Time

	// operations and executions are the persistence and the record read that
	// restart recovery derives outstanding work from. Both are READS plus
	// bookkeeping writes; neither can change a container.
	operations DependencyOperationStore
	executions DependencyExecutions
	plans      DependencyPlans
	notifier   Notifier
}

// NewDependencyService builds a DependencyService.
func NewDependencyService(opts DependencyOptions) *DependencyService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &DependencyService{
		store:      opts.Store,
		lineage:    opts.Lineage,
		self:       opts.Self,
		audit:      opts.Audit,
		logger:     logger,
		now:        now,
		operations: opts.Operations,
		executions: opts.Executions,
		plans:      opts.Plans,
		notifier:   opts.Notifier,
	}
}

// Readable reports whether dependency questions can be answered at all.
func (s *DependencyService) Readable() bool { return s != nil && s.store != nil }

// selfIdentity reads HarborMaster's own identity, tolerating an unwired
// reporter. The zero identity matches nothing, so an unwired build excludes
// none rather than the wrong one.
func (s *DependencyService) selfIdentity() domain.SelfIdentity {
	if s == nil || s.self == nil {
		return domain.SelfIdentity{}
	}
	return s.self.Identity()
}

// ------------------------------------------------------------ the graph --

// DependencyView is one evaluation of the estate.
//
// Built whole, because the three parts are read together or not at all: the
// graph says what the order is, the problems say what could not be established,
// and dropping either half turns a refusal into a clearance.
type DependencyView struct {
	Graph domain.DependencyGraph
	// Problems are the discovery refusals, indexed by container.
	Problems map[string][]domain.DependencyProblem
	// NamespaceRows is the projection the view was derived from, retained so a
	// caller resolving a provider does not re-query.
	NamespaceRows []domain.ContainerNamespaceRow
	// BuiltAt is when the view was derived.
	BuiltAt time.Time
}

// View derives the current dependency picture.
//
// Two queries, both indexed, both bounded. O(V + E) after that.
func (s *DependencyService) View(ctx context.Context) (DependencyView, error) {
	if !s.Readable() {
		return DependencyView{}, ErrDependencyGraphUnavailable
	}

	rows, err := s.store.NamespaceRows(ctx)
	if err != nil {
		// Includes the truncation refusal. Not degraded: a partial estate
		// produces a partial graph, and a partial graph makes containers look
		// more independent than they are.
		return DependencyView{}, fmt.Errorf("%w: %w", ErrDependencyGraphUnavailable, err)
	}
	operator, err := s.store.OperatorDependencies(ctx)
	if err != nil {
		return DependencyView{}, fmt.Errorf("%w: %w", ErrDependencyGraphUnavailable, err)
	}

	discovered, problems := domain.DiscoverDependencies(rows)

	nodes := make([]string, 0, len(rows))
	for _, row := range rows {
		nodes = append(nodes, row.Name)
	}
	edges := make([]domain.WorkloadDependency, 0, len(discovered)+len(operator))
	edges = append(edges, discovered...)
	edges = append(edges, operator...)

	graph, err := domain.BuildDependencyGraph(nodes, edges)
	if err != nil {
		return DependencyView{}, fmt.Errorf("%w: %w", ErrDependencyGraphUnavailable, err)
	}

	return DependencyView{
		Graph:         graph,
		Problems:      domain.ProblemsByContainer(problems),
		NamespaceRows: rows,
		BuiltAt:       s.now().UTC(),
	}, nil
}

// ------------------------------------------------------ namespace rebind --

// providerNameFor returns the stable NAME of the container now holding the
// namespace a stale reference points at.
//
// The same resolution as ResolveNamespaceProvider, then one step further: the
// planner needs a name to put in front of an operator and to key a rebind
// candidate by, while the recreation needs the id. Both go through the one
// chain, so a rebind can only ever be PROPOSED for a provider a recreation could
// actually resolve.
//
// Rows are passed in rather than re-read: the caller already holds the view this
// answer must be consistent with, and re-reading would let a refresh between the
// two produce a candidate naming a container the view does not contain.
func (s *DependencyService) providerNameFor(
	ctx context.Context,
	referencedID string,
	rows []domain.ContainerNamespaceRow,
) (string, error) {
	live, err := s.ResolveNamespaceProvider(ctx, referencedID)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.ContainerID != live {
			continue
		}
		name := domain.NormaliseContainerName(row.Name)
		if !domain.ValidContainerName(name) {
			return "", store.ErrNotFound
		}
		return name, nil
	}
	return "", store.ErrNotFound
}

// maxReplacementHops bounds how far a namespace reference is chased.
//
// A provider replaced repeatedly while a dependent waited leaves a chain of
// records, and each hop is one point lookup. Bounded because every walk in this
// codebase is bounded, and because a corrupt or adversarially-written chain must
// terminate rather than spin. Eight is far past any real estate: it means the
// provider was updated eight times before its dependent was reattached, which is
// a stuck operation an operator needs told about, not chased further.
const maxReplacementHops = 8

// ResolveNamespaceProvider maps a container id a capture names onto the id that
// must be used instead.
//
// The resolution chain, and every link is a record HarborMaster wrote about a
// mutation HarborMaster performed:
//
//	captured provider id -> the execution that replaced it -> its replacement id
//	                     -> re-checked against the containers present now
//
// A caller cannot supply the replacement. There is no parameter for one, and the
// only input is an id already sitting in a capture HarborMaster took itself.
//
// The identity mapping is the ordinary case: a provider that is still live maps
// to itself, and the capture is rewritten to the same value it already held.
//
// # Why the execution record and not the name
//
// This used to resolve by NAME: read the name the old id held, then find the
// container present under it. Stage 5a found two independent reasons that could
// not work, against a real daemon.
//
// The read it used filters `present = 1`, so it could never answer for a
// replaced container -- the only case it is ever asked about. And reading absent
// rows would not have saved it either: a recreation renames the original to its
// parked name before removing it, so an inventory refresh landing in that window
// records `<name>.hm-old-<executionID>` against the old id, and no lookup by
// stable name matches. Recovering the stable name by stripping that suffix is
// deliberately not done -- see IsHarborMasterDerivedName, which documents that
// the shape is a display aid and never a security decision.
//
// The execution record carries the fact itself, so none of that is inferred.
//
// # Fails closed in every direction
//
// A provider replaced by something HarborMaster did not do -- an operator
// removing and re-running it by hand -- has no record, and resolves to nothing.
// That is a refusal, and it is the right one: HarborMaster did not perform the
// replacement and cannot establish that the container now holding the name is
// the same workload. Guessing would attach a dependent's network namespace to
// whatever happens to answer to a name.
//
// Returns ErrNotFound when any link cannot be established. Every caller turns
// that into a refusal rather than proceeding.
func (s *DependencyService) ResolveNamespaceProvider(
	ctx context.Context,
	capturedID string,
) (string, error) {
	if !s.Readable() {
		return "", store.ErrNotFound
	}
	if !domain.ValidFullContainerID(capturedID) {
		return "", store.ErrNotFound
	}

	rows, err := s.store.NamespaceRows(ctx)
	if err != nil {
		return "", err
	}
	present := make(map[string]bool, len(rows))
	for _, row := range rows {
		if domain.ValidFullContainerID(row.ContainerID) {
			present[row.ContainerID] = true
		}
	}

	// Is the captured id itself still present? Then it resolves to itself and
	// nothing is rewritten.
	if present[capturedID] {
		return capturedID, nil
	}
	if s.executions == nil {
		// A build that cannot ask the question must not answer it.
		return "", store.ErrNotFound
	}

	// It is not. Follow HarborMaster's own record of what replaced it, one hop
	// at a time, until a hop lands on a container that is present.
	seen := map[string]bool{capturedID: true}
	current := capturedID
	for range maxReplacementHops {
		next, err := s.executions.ReplacementFor(ctx, current)
		if err != nil {
			return "", err
		}
		if !domain.ValidFullContainerID(next) || seen[next] {
			// A malformed id, or a cycle. Neither establishes anything.
			return "", store.ErrNotFound
		}
		seen[next] = true
		if present[next] {
			return next, nil
		}
		current = next
	}
	return "", store.ErrNotFound
}

// ------------------------------------------------------ invariant A --

// AssessProvider runs the rebindability check for one container.
//
// The check that must happen BEFORE the provider is stopped. Returns the
// assessment and whether the container is a provider at all; a container nothing
// shares a namespace with is not gated by this.
//
// An error means the check COULD NOT BE PERFORMED, which is not the same as
// passing. Every caller refuses on error.
func (s *DependencyService) AssessProvider(
	ctx context.Context,
	providerName string,
) (assessment domain.ProviderRebindAssessment, isProvider bool, err error) {
	if !s.Readable() {
		return domain.ProviderRebindAssessment{}, false, ErrDependencyGraphUnavailable
	}

	view, err := s.View(ctx)
	if err != nil {
		return domain.ProviderRebindAssessment{}, false, err
	}

	name := domain.NormaliseContainerName(providerName)
	hard := view.Graph.HardDependentsOf(name)
	if len(hard) == 0 {
		return domain.ProviderRebindAssessment{Provider: name}, false, nil
	}

	candidates, err := s.candidatesFor(ctx, view, name, hard)
	if err != nil {
		return domain.ProviderRebindAssessment{}, true, err
	}
	return domain.AssessProviderRebindability(name, candidates, s.selfIdentity()), true, nil
}

// candidatesFor assembles the rebind candidate for every hard dependent.
//
// # Why the evidence is synthesised here
//
// A provider that is ABOUT to be replaced has not been replaced yet, so
// discovery reports no stale binding for its dependents -- the reference they
// hold is still live. The evidence has to describe the situation that WILL
// exist the moment the provider is stopped.
//
// It is still derived rather than asserted: the dependent, the provider, the
// namespace source, and the id all come from the graph edge HarborMaster
// discovered from the daemon's own configuration. Nothing here is caller input,
// and the constructor still refuses anything that does not describe a hard
// namespace share.
func (s *DependencyService) candidatesFor(
	ctx context.Context,
	view DependencyView,
	provider string,
	hard []domain.WorkloadDependency,
) ([]domain.RebindCandidate, error) {
	endpoints, err := s.store.Endpoints(ctx)
	if err != nil {
		return nil, err
	}
	byName := domain.EndpointsFromNames(endpoints)

	rowsByName := make(map[string]domain.ContainerNamespaceRow, len(view.NamespaceRows))
	for _, row := range view.NamespaceRows {
		rowsByName[domain.NormaliseContainerName(row.Name)] = row
	}

	candidates := make([]domain.RebindCandidate, 0, len(hard))
	for _, edge := range hard {
		dependent := edge.Dependent
		candidate := domain.RebindCandidate{
			Name:     dependent,
			Provider: provider,
		}

		// The evidence, built from the discovered edge.
		evidence, ok := domain.RebindEvidenceFrom(domain.DependencyProblem{
			Container:    dependent,
			Source:       edge.Source,
			ReferencedID: edge.Evidence.DependencyContainerID,
			Refusal:      domain.DiscoveryUnknownContainer,
		}, provider, view.BuiltAt)
		if ok {
			candidate.Evidence = evidence
		}

		if endpoint, known := byName[dependent]; known {
			candidate.ContainerID = endpoint.ContainerID
			candidate.ImageRef = endpoint.ImageRef
			candidate.Labels = endpoint.Labels
			candidate.Present = endpoint.Present
			candidate.Derived = endpoint.Derived
			candidate.Recreatable = domain.ScreenTarget(
				dependent, endpoint.ImageRef, endpoint.Labels).Recreatable
		}
		if row, known := rowsByName[dependent]; known {
			candidate.NamespacesObserved = row.Modes.Observed
		}

		// What it is actually running. An unestablished digest refuses the
		// candidate, which blocks the provider -- the correct direction, because
		// a dependent HarborMaster cannot pin cannot be repaired.
		if s.lineage != nil && candidate.ContainerID != "" {
			reference, digest, err := s.lineage.RunningDigestFor(ctx, candidate.ContainerID)
			if err == nil {
				candidate.RunningReference = reference
				candidate.RunningDigest = digest
			}
		}

		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

// RebindCandidates returns the dependents of a provider that need reattaching
// RIGHT NOW, because their binding is already stale.
//
// Distinct from AssessProvider, which asks about a replacement that has not
// happened yet. This one reads the discovery problems: a dependent whose
// namespace reference names a container the inventory no longer has.
func (s *DependencyService) RebindCandidates(ctx context.Context) ([]domain.RebindCandidate, error) {
	view, err := s.View(ctx)
	if err != nil {
		return nil, err
	}

	endpoints, err := s.store.Endpoints(ctx)
	if err != nil {
		return nil, err
	}
	byName := domain.EndpointsFromNames(endpoints)

	var candidates []domain.RebindCandidate
	for _, container := range sortedProblemContainers(view.Problems) {
		for _, problem := range view.Problems[container] {
			if !problem.Refusal.RebindSignal() {
				continue
			}
			provider, err := s.providerNameFor(ctx, problem.ReferencedID, view.NamespaceRows)
			if err != nil {
				// The stale id names a container HarborMaster cannot map onto
				// anything present. Nothing to reattach TO, so no rebind can be
				// proposed; the dependent stays blocked as dependencyMissing.
				continue
			}
			evidence, ok := domain.RebindEvidenceFrom(problem, provider, view.BuiltAt)
			if !ok {
				continue
			}

			candidate := domain.RebindCandidate{
				Evidence: evidence,
				Name:     container,
				Provider: provider,
			}
			if endpoint, known := byName[container]; known {
				candidate.ContainerID = endpoint.ContainerID
				candidate.ImageRef = endpoint.ImageRef
				candidate.Labels = endpoint.Labels
				candidate.Present = endpoint.Present
				candidate.Derived = endpoint.Derived
				candidate.Recreatable = domain.ScreenTarget(
					container, endpoint.ImageRef, endpoint.Labels).Recreatable
			}
			candidate.NamespacesObserved = true
			if s.lineage != nil && candidate.ContainerID != "" {
				if reference, digest, err := s.lineage.RunningDigestFor(
					ctx, candidate.ContainerID); err == nil {
					candidate.RunningReference = reference
					candidate.RunningDigest = digest
				}
			}
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

// sortedProblemContainers returns the problem index's keys in sorted order.
//
// The one place this service turns a map into a slice. Sorted for the same
// reason the graph sorts everything: an evaluation of an unchanged estate must
// produce the same answer twice, including which dependent is reported first.
func sortedProblemContainers(problems map[string][]domain.DependencyProblem) []string {
	names := make([]string, 0, len(problems))
	for name := range problems {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
