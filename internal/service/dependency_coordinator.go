package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The coordinator: the two production paths that write dependency state.
//
// # EnsureOperation runs BEFORE the provider is stopped
//
// Not "around the same time" -- before. The live experiment against Docker
// 29.6.2 established that STOPPING a namespace provider is the instant its
// dependents lose their network, silently. A record written afterwards would
// describe containers that are already broken; a record written before is what
// a restart reads to find out what was supposed to happen.
//
// So the ordering is: establish the complete member set, persist it atomically,
// RELOAD it to prove it is durable, and only then let the pipeline reach the
// mutation point. Every failure in that sequence refuses, and refusing means no
// Docker call has been made.
//
// # ProduceRebindPlans is the ONLY caller of BuildRebindPlan
//
// An architecture test pins that. The chain from evidence to plan is closed:
// RebindEvidence cannot be constructed outside internal/domain, BuildRebindPlan
// refuses the zero value, and this is the one place in the service layer that
// holds one.
//
// # Neither path can reach Docker
//
// This file writes HarborMaster's own tables and reads its own records. The
// mutations belong to the acquisition and execution services, which own their
// capabilities and re-run their own preflights.

// DependencyPlans is the change-plan persistence the coordinator needs.
//
// Three methods, and note what is absent: nothing that acquires, executes, or
// approves. A plan is an ASSESSMENT, and writing one causes nothing to happen.
type DependencyPlans interface {
	InsertPlans(ctx context.Context, plans []domain.ChangePlan, now time.Time) (store.InsertResult, error)
	Get(ctx context.Context, planID string) (domain.ChangePlan, error)
	GatherInputs(ctx context.Context, containerIDs []string, imageRefs []string) (store.PlanBatchInputs, error)
}

// ErrOperationEvidenceIncomplete reports that the mandatory member set could not
// be established.
//
// Returned rather than degraded. A partial member set would let a provider be
// stopped while one of its dependents is unknown to the coordinator, which is
// exactly the silent breakage this whole phase exists to prevent.
var ErrOperationEvidenceIncomplete = errors.New(
	"the containers sharing this one's namespace could not be completely established")

// ------------------------------------------------- pre-stop persistence --

// EnsureOperation establishes and persists the dependency operation for a
// provider that is about to be recreated.
//
// Returns the operation id and whether the container is a namespace provider at
// all. An ORDINARY container -- one nothing shares a namespace with -- gets
// ("", false, nil) and no row: the normal execution path is unchanged for a
// workload that has no dependents, and manufacturing an empty operation for one
// would put a record in front of an operator describing nothing.
//
// Every error means the operation could NOT be established, and every caller
// refuses on it without mutating anything.
//
// # Idempotent
//
// A provider that already has a live operation reuses it. That covers a retried
// request, a second worker claim, and a restart between persistence and the
// stop. The existing operation is returned UNCHANGED -- never reinterpreted as
// a failure, and never supplemented with a second member set.
func (s *DependencyService) EnsureOperation(
	ctx context.Context,
	provider string,
	planID string,
	requestedBy domain.Requester,
) (operationID string, isProvider bool, err error) {
	if s == nil || s.operations == nil {
		// The subsystem is not wired. The caller treats this as a refusal, which
		// is why this returns an error rather than "not a provider".
		return "", false, ErrDependencyGraphUnavailable
	}

	name := domain.NormaliseContainerName(provider)

	// 1. Is there already one? Before any assessment, so a retry costs one
	//    indexed read rather than a full re-derivation.
	if existing, found, err := s.operations.ActiveForProvider(ctx, name); err != nil {
		return "", false, err
	} else if found {
		s.logger.InfoContext(ctx, "reusing the dependency operation already recorded for this container",
			slog.String("operationId", existing.OperationID),
			slog.String("provider", domain.SanitiseDisplayText(name, domain.MaxContainerNameBytes)),
			slog.String("state", string(existing.State)),
			slog.Int("members", len(existing.Members)))
		return existing.OperationID, true, nil
	}

	// 2-7. The complete member set, re-derived from the live records.
	view, err := s.View(ctx)
	if err != nil {
		return "", false, err
	}
	edges := view.Graph.HardDependentsOf(name)
	if len(edges) == 0 {
		// An ordinary container. No operation, no row, no change to the path.
		return "", false, nil
	}

	// Discovery reports one edge per shared NAMESPACE; a member is one container
	// to REATTACH. A dependent sharing both this provider's network and IPC
	// namespaces is two edges and one member -- and building a row per edge
	// collided on the member table's own primary key, refusing every update of
	// such a provider for ever. See domain.CollapseHardDependents.
	hard, consistent := domain.CollapseHardDependentsChecked(edges)
	if !consistent {
		// Half-attached: the dependent names different ids for the same provider
		// in different namespaces. One member row carries one expected id, so
		// there is no honest record to write.
		return "", true, fmt.Errorf("%w: a container sharing several of this one's "+
			"namespaces names a different provider in each", ErrOperationEvidenceIncomplete)
	}

	candidates, err := s.candidatesFor(ctx, view, name, hard)
	if err != nil {
		return "", true, err
	}
	// The member set must be COMPLETE. One candidate per hard dependent, and a
	// count that disagrees means something moved between the graph read and the
	// candidate assembly.
	if len(candidates) != len(hard) {
		return "", true, ErrOperationEvidenceIncomplete
	}

	assessment := domain.AssessProviderRebindability(name, candidates, s.selfIdentity())
	if !assessment.MayStop() {
		// Invariant A, re-run here as well as in the execution preflight. The
		// preflight ran earlier; this runs against the records the operation
		// will be built from, so the two cannot disagree about what is being
		// promised.
		blocked := assessment.Blocked[0]
		return "", true, fmt.Errorf("%w: %s cannot be reattached (%s)",
			ErrOperationEvidenceIncomplete,
			domain.SanitiseDisplayText(blocked.Dependent, domain.MaxContainerNameBytes),
			blocked.Refusal)
	}

	members := make([]domain.DependencyMember, 0, len(candidates))
	for _, candidate := range candidates {
		if !candidate.Evidence.Established() {
			// Cannot happen for a candidate that passed the assessment above;
			// refused anyway, because a member with no evidence is a member no
			// rebind can be built for.
			return "", true, ErrOperationEvidenceIncomplete
		}
		members = append(members, domain.DependencyMember{
			Dependent: candidate.Name,
			Provider:  name,
			Source:    candidate.Evidence.Source(),
			// The id the dependent NAMES today -- the one about to go stale.
			ExpectedProviderID: candidate.Evidence.StaleContainerID(),
			State:              domain.MemberPending,
		})
	}

	// 8. Atomic. Either every member lands or nothing does.
	created, err := s.operations.Create(ctx, domain.DependencyOperation{
		Provider:       name,
		ProviderPlanID: planID,
		State:          domain.OperationQueued,
		RequestedBy:    requestedBy,
		Members:        members,
	}, s.now())
	if err != nil {
		if errors.Is(err, store.ErrDependencyOperationActive) {
			// A concurrent creator won the unique index. Not a failure: read
			// theirs and continue with it.
			if existing, found, lookupErr := s.operations.ActiveForProvider(ctx, name); lookupErr == nil && found {
				return existing.OperationID, true, nil
			}
		}
		return "", true, err
	}

	// 9. Reload, and prove it is durable and complete.
	//
	// Not paranoia: the whole value of this record is that a DIFFERENT PROCESS
	// can read it after a crash. Confirming it reads back is the cheapest
	// possible proof that it will.
	reloaded, err := s.operations.Get(ctx, created.OperationID)
	if err != nil {
		return "", true, err
	}
	if len(reloaded.Members) != len(members) {
		return "", true, ErrOperationEvidenceIncomplete
	}

	s.logger.InfoContext(ctx, "recorded a dependency operation before changing the host",
		slog.String("operationId", reloaded.OperationID),
		slog.String("provider", domain.SanitiseDisplayText(name, domain.MaxContainerNameBytes)),
		slog.Int("mandatoryRebinds", len(reloaded.Members)))

	return reloaded.OperationID, true, nil
}

// AttachProviderExecution links the operation to the execution performing it.
func (s *DependencyService) AttachProviderExecution(
	ctx context.Context,
	operationID, executionID string,
) error {
	if s == nil || s.operations == nil || operationID == "" {
		return nil
	}
	return s.operations.AttachProviderExecution(ctx, operationID, executionID, s.now())
}

// ------------------------------------------------- rebind plan production --

// RebindPlanResult is what one production pass did.
type RebindPlanResult struct {
	// Created maps dependent name to the plan id produced for it.
	Created map[string]string
	// Reused maps dependent name to a plan id that already existed.
	Reused map[string]string
	// Skipped maps dependent name to the closed-vocabulary reason no plan was
	// produced. A skip is not a failure: the commonest one is "this container is
	// already correctly attached".
	Skipped map[string]domain.RebindRefusal
	// Satisfied names dependents that need no rebind because they are already
	// bound to the current provider.
	Satisfied []string
}

// ProduceRebindPlans creates the change plans one operation's members need.
//
// THE ONLY CALLER OF domain.BuildRebindPlan. An architecture test pins that.
//
// Every condition is re-read HERE, immediately before the plan is built, rather
// than carried from when the operation was created. The operation may be minutes
// old and the estate does not hold still: a dependent can be recreated by hand,
// removed, opted out, or repointed at a different provider in that time, and a
// plan built from what was true then would recreate a container on evidence that
// has expired.
//
// Produces nothing when the provider has not reached verified success. There is
// nothing to reattach TO until it has.
func (s *DependencyService) ProduceRebindPlans(
	ctx context.Context,
	operationID string,
) (RebindPlanResult, error) {
	result := RebindPlanResult{
		Created: make(map[string]string),
		Reused:  make(map[string]string),
		Skipped: make(map[string]domain.RebindRefusal),
	}
	if s == nil || s.operations == nil || s.plans == nil {
		return result, ErrDependencyGraphUnavailable
	}

	recovered, err := s.RecoverOperation(ctx, operationID)
	if err != nil {
		return result, err
	}
	// The provider must have SUCCEEDED. Not "been requested", not "be running".
	if !recovered.ProviderVerified {
		return result, nil
	}

	// The live picture, read now.
	view, err := s.View(ctx)
	if err != nil {
		return result, err
	}
	endpoints, err := s.store.Endpoints(ctx)
	if err != nil {
		return result, err
	}
	byName := domain.EndpointsFromNames(endpoints)

	// The provider's CURRENT identity. Everything below is a comparison against
	// this, so it is established once and refused if absent.
	currentProviderID, ok := currentIDFor(view, recovered.Operation.Provider)
	if !ok {
		return result, ErrOperationEvidenceIncomplete
	}

	var toInsert []domain.ChangePlan
	pending := make([]domain.DependencyMember, 0, len(recovered.Operation.Members))
	for _, member := range recovered.Operation.Members {
		if member.State.Clears() || member.State.Settled() {
			continue
		}
		// A member whose recreation is ALREADY UNDER WAY is not planned again.
		//
		// # Why the execution id rather than the state
		//
		// The state is a cache; the execution id is the fact. A member that
		// names an execution has had one requested, and producing a fresh plan
		// for it would be preparing a SECOND recreation of a container that is
		// currently being recreated.
		//
		// The execution service's own recovery owns what happens to an
		// interrupted execution. Dependency code consumes that verdict and does
		// not race it -- which is exactly what the restart scenario in
		// TestProductionRestartLeavesAnUnfinishedMemberExecutionAlone requires.
		if member.ExecutionID != "" {
			continue
		}
		pending = append(pending, member)
	}
	if len(pending) == 0 {
		return result, nil
	}

	// The planner inputs for every pending dependent, in ONE batch read rather
	// than one per member.
	//
	// The references are NORMALISED first, exactly as the planner normalises
	// them. `image_intel` is keyed on the canonical form -- a container's own
	// `alpine:3.22.1` is stored as `docker.io/library/alpine:3.22.1` -- so
	// passing the raw reference matched no row, every rebind was assessed with
	// its registry evidence missing, and the model reported "cannot advise".
	//
	// Stage 5 found that against a real daemon, one layer down from where it
	// surfaced: the acquisition service refused the reattachment because the
	// plan carried no recommendation, and the dependent was never reattached.
	containerIDs := make([]string, 0, len(pending))
	imageRefs := make([]string, 0, len(pending))
	for _, member := range pending {
		endpoint, known := byName[member.Dependent]
		if !known {
			continue
		}
		containerIDs = append(containerIDs, endpoint.ContainerID)
		if reference, ok := normalisedReference(endpoint.ImageRef); ok {
			imageRefs = append(imageRefs, reference.Canonical)
		}
	}
	batch, err := s.plans.GatherInputs(ctx, containerIDs, imageRefs)
	if err != nil {
		return result, err
	}

	planForMember := make(map[string]string, len(pending))
	for _, member := range pending {
		// DEDUPLICATION, before anything is built. A member that already holds a
		// usable plan for THIS provider transition keeps it.
		if reused, ok := s.existingPlanFor(ctx, member, currentProviderID); ok {
			result.Reused[member.Dependent] = reused
			continue
		}

		candidate, refusal := s.rebindCandidateNow(ctx, view, byName, member, currentProviderID)
		switch {
		case refusal == rebindAlreadySatisfied:
			// Already attached to the replacement. Nothing to do, and the member
			// is cleared rather than planned.
			result.Satisfied = append(result.Satisfied, member.Dependent)
			continue
		case refusal != domain.RebindRefusalNone:
			result.Skipped[member.Dependent] = refusal
			continue
		}

		inputs := s.planInputsFor(candidate, byName[member.Dependent], batch)
		planID := domain.NewPlanID()
		plan, refusal := domain.BuildRebindPlan(
			candidate.Evidence, candidate, s.selfIdentity(), inputs, planID, s.now())
		if refusal != domain.RebindRefusalNone {
			result.Skipped[member.Dependent] = refusal
			continue
		}

		toInsert = append(toInsert, plan)
		planForMember[member.Dependent] = planID
	}

	if len(toInsert) == 0 {
		s.markSatisfied(ctx, operationID, result.Satisfied)
		return result, nil
	}

	// One transaction for every plan.
	if _, err := s.plans.InsertPlans(ctx, toInsert, s.now()); err != nil {
		return result, err
	}

	// Then the member rows. A failure here leaves an ORPHANED PLAN, which is
	// harmless -- a plan causes nothing to happen -- and the next pass finds the
	// member still without a plan id and builds another. That is the safe
	// direction: an extra assessment nobody acted on, rather than a member that
	// believes it has a plan it does not.
	for dependent, planID := range planForMember {
		if err := s.operations.AdvanceMember(ctx, store.MemberUpdate{
			OperationID:      operationID,
			Dependent:        dependent,
			State:            domain.MemberPlanCreated,
			PlanID:           planID,
			TargetProviderID: currentProviderID,
		}, s.now()); err != nil {
			s.logger.WarnContext(ctx, "a rebind plan was recorded but not linked to its member",
				slog.String("operationId", operationID),
				slog.String("dependent",
					domain.SanitiseDisplayText(dependent, domain.MaxContainerNameBytes)),
				slog.Any("error", err))
			continue
		}
		result.Created[dependent] = planID
	}

	s.markSatisfied(ctx, operationID, result.Satisfied)
	return result, nil
}

// rebindAlreadySatisfied is an internal sentinel: not a refusal an operator
// sees, but the answer "there is nothing to do here".
const rebindAlreadySatisfied domain.RebindRefusal = "alreadyAttached"

// rebindCandidateNow re-derives one member's candidate from the LIVE records.
//
// # The TOCTOU boundary
//
// Everything the plan will rest on is re-read here: whether the dependent still
// exists, what it currently declares, what it is currently running, and whether
// it is still something HarborMaster may act on. The member row is treated as a
// statement of INTENT, never as evidence.
//
// Each of these is a real situation, not a theoretical one. Between an operation
// being created and its plans being produced, an operator can `docker compose
// up` the dependent, remove it, add an opt-out label, or repoint it at a
// different provider entirely.
func (s *DependencyService) rebindCandidateNow(
	ctx context.Context,
	view DependencyView,
	byName map[string]domain.DependencyEndpoint,
	member domain.DependencyMember,
	currentProviderID string,
) (domain.RebindCandidate, domain.RebindRefusal) {
	// Only a hard Docker namespace source can require a rebind. An operator
	// relationship constrains ORDER. The schema refuses to store one as a
	// member, and this refuses to act on one if it somehow appears.
	if !member.Source.Hard() {
		return domain.RebindCandidate{}, domain.RebindRefusalNoEvidence
	}

	// The dependent must still be in the estate.
	row, present := namespaceRowFor(view, member.Dependent)
	if !present {
		return domain.RebindCandidate{}, domain.RebindRefusalNotPresent
	}
	if !row.Modes.Observed {
		return domain.RebindCandidate{}, domain.RebindRefusalNamespaceStale
	}

	// What it declares NOW for the namespace this member is about.
	declared := modeFor(row.Modes, member.Source)
	if !domain.SharesNamespace(declared) {
		// It no longer shares a namespace at all -- recreated by hand onto a
		// bridge network, say. There is nothing to reattach.
		return domain.RebindCandidate{}, domain.RebindRefusalProviderMismatch
	}
	declaredID, parsed := domain.ParseNamespaceContainerRef(declared)
	if !parsed {
		return domain.RebindCandidate{}, domain.RebindRefusalNamespaceStale
	}

	switch declaredID {
	case currentProviderID:
		// Already attached to the replacement. Somebody else fixed it, or it was
		// recreated after the provider was. Nothing to do.
		return domain.RebindCandidate{}, rebindAlreadySatisfied
	case member.ExpectedProviderID:
		// The expected stale binding. This is the case a rebind exists for.
	default:
		// It points at neither the id we recorded nor the current one. The
		// transition this member describes is not the transition that happened,
		// so the member's evidence has expired.
		return domain.RebindCandidate{}, domain.RebindRefusalProviderMismatch
	}

	endpoint, known := byName[member.Dependent]
	if !known || !endpoint.Present {
		return domain.RebindCandidate{}, domain.RebindRefusalNotPresent
	}

	candidate := domain.RebindCandidate{
		Name:               member.Dependent,
		Provider:           member.Provider,
		ContainerID:        endpoint.ContainerID,
		ImageRef:           endpoint.ImageRef,
		Labels:             endpoint.Labels,
		Present:            true,
		NamespacesObserved: true,
		Derived:            endpoint.Derived,
		Recreatable: domain.ScreenTarget(
			member.Dependent, endpoint.ImageRef, endpoint.Labels).Recreatable,
	}

	// What it is running NOW. Never the digest recorded earlier: recreating on a
	// digest the container has since moved off would silently change what
	// executes while claiming to repair a network attachment.
	if s.lineage != nil && candidate.ContainerID != "" {
		if reference, digest, err := s.lineage.RunningDigestFor(ctx, candidate.ContainerID); err == nil {
			candidate.RunningReference = reference
			candidate.RunningDigest = digest
		}
	}

	// The evidence, rebuilt from what is true now.
	evidence, ok := domain.RebindEvidenceFrom(domain.DependencyProblem{
		Container:    member.Dependent,
		Source:       member.Source,
		ReferencedID: declaredID,
		Refusal:      domain.DiscoveryUnknownContainer,
	}, member.Provider, view.BuiltAt)
	if !ok {
		return domain.RebindCandidate{}, domain.RebindRefusalNoEvidence
	}
	candidate.Evidence = evidence

	// The full rebindability assessment -- self, preserved, opted out,
	// unrecreatable, unpinnable. Re-run rather than assumed from the operation.
	if refusal := domain.AssessRebind(candidate, s.selfIdentity()); refusal != domain.RebindRefusalNone {
		return domain.RebindCandidate{}, refusal
	}
	return candidate, domain.RebindRefusalNone
}

// existingPlanFor reports a usable plan this member already holds.
//
// # Deduplication, and why uncertainty does NOT produce a second plan
//
// A member with a plan id is only cleared to build another if the plan is
// genuinely unusable for THIS transition -- missing, or built for a different
// provider identity. A plan that cannot be READ is treated as still valid: "the
// row could not be fetched" is not evidence that it is wrong, and building a
// second plan on that basis is how one member ends up with two.
func (s *DependencyService) existingPlanFor(
	ctx context.Context,
	member domain.DependencyMember,
	currentProviderID string,
) (string, bool) {
	if member.PlanID == "" {
		return "", false
	}
	// A plan recorded for a DIFFERENT provider identity is stale: the provider
	// changed again since it was built.
	if member.TargetProviderID != "" && member.TargetProviderID != currentProviderID {
		return "", false
	}

	plan, err := s.plans.Get(ctx, member.PlanID)
	if errors.Is(err, store.ErrNotFound) {
		// The member points at a plan that is not there. Genuinely unusable, so
		// a replacement may be built.
		return "", false
	}
	if err != nil {
		// Unreadable. Kept, deliberately -- see the note above.
		s.logger.WarnContext(ctx, "could not read the rebind plan a member names; not building a second",
			slog.String("planId", member.PlanID), slog.Any("error", err))
		return member.PlanID, true
	}
	if plan.UpdateType != domain.UpdateRebind {
		return "", false
	}
	return member.PlanID, true
}

// planInputsFor assembles the risk inputs for one dependent.
//
// The SAME inputs an ordinary plan is assessed from -- snapshot, readiness,
// drift, compliance -- so a rebind of a container with no snapshot scores
// exactly as an update of it would. The image fields are overridden by
// BuildRebindPlan and are deliberately not set here.
func (s *DependencyService) planInputsFor(
	candidate domain.RebindCandidate,
	endpoint domain.DependencyEndpoint,
	batch store.PlanBatchInputs,
) domain.PlanInputs {
	baseline := batch.Baselines[endpoint.ContainerID]
	drift := batch.Drift[endpoint.ContainerID]
	policy := batch.Policy[endpoint.ContainerID]

	// The dependent's REAL registry record, looked up exactly as the planner
	// does. Not fabricated and not skipped: the container has an image, that
	// image has an intelligence record, and the plan reports what it actually
	// says.
	//
	// Reported honestly in both directions. A stale or failed record raises the
	// risk score and may push the plan to manualReview, which is correct -- the
	// execution preflight independently requires a fresh OK record before it
	// will act, so a plan claiming otherwise would only be a plan that gets
	// refused later with a less useful explanation.
	var intel domain.ImageIntel
	if reference, ok := normalisedReference(endpoint.ImageRef); ok {
		intel = batch.Intel[reference.Canonical]
	}

	return domain.PlanInputs{
		ContainerID:   endpoint.ContainerID,
		ContainerName: candidate.Name,

		CurrentDigest: candidate.RunningDigest,
		CurrentTag:    intel.Tag,

		SnapshotID:       baseline.SnapshotID,
		RestoreReadiness: readinessOrUnknown(baseline),

		DriftOpen:        drift.Open,
		DriftMaxSeverity: domain.DriftSeverity(drift.MaxSeverity),

		PolicyOpen:        policy.Open,
		PolicyMaxSeverity: domain.PolicySeverity(policy.MaxSeverity),

		RegistryStatus:          intel.Status,
		RegistryDetail:          intel.StatusDetail,
		LocalPlatform:           intel.Platform,
		ProposedPlatformMissing: intel.PlatformMissing,
		ContainerCount:          intel.ContainerCount,
	}
}

// markSatisfied clears members that turned out to need no rebind.
func (s *DependencyService) markSatisfied(ctx context.Context, operationID string, satisfied []string) {
	for _, dependent := range satisfied {
		if err := s.operations.AdvanceMember(ctx, store.MemberUpdate{
			OperationID: operationID,
			Dependent:   dependent,
			State:       domain.MemberVerified,
		}, s.now()); err != nil {
			s.logger.WarnContext(ctx, "could not clear a dependent that was already reattached",
				slog.String("operationId", operationID),
				slog.String("dependent",
					domain.SanitiseDisplayText(dependent, domain.MaxContainerNameBytes)),
				slog.Any("error", err))
		}
	}
}

// ------------------------------------------------------------- helpers --

// currentIDFor returns a container's live id from the view.
func currentIDFor(view DependencyView, name string) (string, bool) {
	normalised := domain.NormaliseContainerName(name)
	for _, row := range view.NamespaceRows {
		if domain.NormaliseContainerName(row.Name) != normalised {
			continue
		}
		if !domain.ValidFullContainerID(row.ContainerID) {
			return "", false
		}
		return row.ContainerID, true
	}
	return "", false
}

// namespaceRowFor returns a container's namespace row from the view.
func namespaceRowFor(view DependencyView, name string) (domain.ContainerNamespaceRow, bool) {
	normalised := domain.NormaliseContainerName(name)
	for _, row := range view.NamespaceRows {
		if domain.NormaliseContainerName(row.Name) == normalised {
			return row, true
		}
	}
	return domain.ContainerNamespaceRow{}, false
}

// modeFor returns the mode string for one namespace source.
func modeFor(modes domain.NamespaceModes, source domain.DependencySource) string {
	switch source {
	case domain.DependencyNetworkNamespace:
		return modes.Network
	case domain.DependencyIPCNamespace:
		return modes.IPC
	case domain.DependencyPIDNamespace:
		return modes.PID
	default:
		return ""
	}
}

// RebindMemberSubmitted records that a reattachment's image acquisition has been
// requested.
//
// Bookkeeping. The acquisition itself was performed by the acquisition service,
// which owns that capability and ran its own preflight; this records which
// record to follow.
//
// The member moves to `acquired` -- NOT to anything that clears it. Only a
// verified execution does that, and recovery derives it from the execution
// record rather than from this write.
func (s *DependencyService) RebindMemberSubmitted(
	ctx context.Context,
	operationID, dependent, acquisitionID string,
) error {
	if s == nil || s.operations == nil {
		return store.ErrNotFound
	}
	return s.operations.AdvanceMember(ctx, store.MemberUpdate{
		OperationID:   operationID,
		Dependent:     dependent,
		State:         domain.MemberAcquired,
		AcquisitionID: acquisitionID,
	}, s.now())
}

// RebindMemberExecuting records that a reattachment's recreation is under way.
func (s *DependencyService) RebindMemberExecuting(
	ctx context.Context,
	operationID, dependent, executionID string,
) error {
	if s == nil || s.operations == nil {
		return store.ErrNotFound
	}
	return s.operations.AdvanceMember(ctx, store.MemberUpdate{
		OperationID: operationID,
		Dependent:   dependent,
		State:       domain.MemberExecuting,
		ExecutionID: executionID,
	}, s.now())
}

// RebindMemberFailed settles a reattachment that cannot proceed.
//
// # When this is the right answer, and when it is not
//
// Only when something SETTLED unsuccessfully. The one caller is the follower,
// and it calls this for exactly one situation: the member's image acquisition
// reached a terminal state that was not success, so there is no verified local
// image to recreate from and there never will be under this acquisition.
//
// It must NOT be called because a record could not be read, because a request
// could not be submitted, or because a pull is still running. All three are
// transient and all three are retried on a later tick; settling on any of them
// would leave the dependent detached and the operation terminal, with nothing
// to retry it. That distinction is CLAUDE.md invariant 5 and it is why the
// follower establishes "failed" positively rather than as "not succeeded".
//
// No refusal is written. RebindRefusal is a judgement about whether the
// CONTAINER can be reattached at all -- it cannot be pinned, it is opted out,
// it is HarborMaster itself -- and none of those is what happened here. The
// acquisition record carries the reason, in its own closed vocabulary.
//
// Nothing is undone. See domain.GroupRollbackIsNotPerformed.
func (s *DependencyService) RebindMemberFailed(
	ctx context.Context,
	operationID, dependent string,
) error {
	if s == nil || s.operations == nil {
		return store.ErrNotFound
	}
	return s.operations.AdvanceMember(ctx, store.MemberUpdate{
		OperationID: operationID,
		Dependent:   dependent,
		State:       domain.MemberFailed,
	}, s.now())
}
