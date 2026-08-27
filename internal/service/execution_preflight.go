package service

import (
	"context"
	"errors"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The recreation preflight.
//
// # Why every check is in one function
//
// A refusal must be attributable to a named check, and the set of checks must
// be readable in one place. A reviewer asking "what would stop HarborMaster
// replacing a container it should not have replaced" should have exactly one
// function to read, top to bottom, in the order the checks run.
//
// # It runs twice
//
// Once when the operator asks, so they get an immediate answer, and again
// inside the worker immediately before the first mutation. The second run is
// the one that matters: it is what closes the time-of-check/time-of-use window
// between "an operator pressed a button" and "a container is being stopped".
//
// # It fails closed, everywhere
//
// Any check that cannot be COMPLETED refuses. Refusing is cheap -- an operator
// asks again -- while proceeding on the strength of a check nobody performed is
// how a tool stops a container it had no business stopping.

// executionDecision is what revalidation concluded.
type executionDecision struct {
	Acquisition domain.Acquisition
	Plan        domain.ChangePlan
	Target      domain.ExecutionTarget

	ContainerID    string
	ContainerName  string
	OldImage       string
	OldImageID     string
	OldImageDigest string
	SnapshotID     int64

	// ParkedName and QuarantineName are derived here, in the preflight, so a
	// name that cannot be produced is a refusal BEFORE anything is stopped
	// rather than a failure after.
	ParkedName     string
	QuarantineName string

	// Refusal is ExecutionRefusalNone when the request may proceed.
	Refusal domain.ExecutionRefusal
}

// preflight re-reads every prerequisite and decides whether to proceed.
func (s *ExecutionService) preflight(
	ctx context.Context,
	acquisitionID string,
) (executionDecision, error) {
	var decision executionDecision

	if !s.cfg.Enabled || s.mutator == nil || s.capturer == nil {
		decision.Refusal = domain.ExecutionRefusalDisabled
		return decision, nil
	}

	// ---- the acquisition -------------------------------------------------
	//
	// The root of the whole evidence chain. It is what establishes that the
	// image is on this host and that its digest was confirmed by inspection --
	// neither of which a plan can establish, because a plan reads a registry.

	acquisition, err := s.evidence.Acquisition(ctx, acquisitionID)
	if errors.Is(err, store.ErrNotFound) {
		decision.Refusal = domain.ExecutionRefusalAcquisitionMissing
		return decision, nil
	}
	if err != nil {
		return decision, err
	}
	decision.Acquisition = acquisition

	// HARBORMASTER ITSELF.
	//
	// The last of three independent refusals, and the one that matters most:
	// this is the layer that actually stops a container. The automation engine
	// refuses first, the acquisition preflight refuses second, and this refuses
	// whatever the other two did -- because an operator can POST /executions
	// with any acquisition id, and because a bug in either of the layers above
	// must not be the only thing standing here.
	//
	// The identity is compared against the ACQUISITION's own record of what it
	// was for, not against anything the caller supplied.
	if s.self != nil {
		identity := s.self.Identity()
		if self, _ := identity.SelfMatch(domain.SelfTarget{
			ContainerID:   acquisition.ContainerID,
			ContainerName: acquisition.ContainerName,
			ImageRef:      acquisition.Target.Reference,
		}); self {
			decision.Refusal = domain.ExecutionRefusalSelfUpdate
			return decision, nil
		}
	}

	if acquisition.State != domain.AcquisitionSucceeded {
		decision.Refusal = domain.ExecutionRefusalAcquisitionNotSucceeded
		return decision, nil
	}
	// A succeeded acquisition without a completion time is a record that
	// contradicts itself. Refused rather than reasoned about.
	if acquisition.CompletedAt == nil {
		decision.Refusal = domain.ExecutionRefusalAcquisitionNotSucceeded
		return decision, nil
	}
	if s.now().UTC().Sub(acquisition.CompletedAt.UTC()) > s.cfg.AcquisitionFreshness {
		// An old download does not establish that the image is still present:
		// an operator may have pruned it, and the local check below would then
		// be the only thing standing between here and a create that fails after
		// the original is already stopped.
		decision.Refusal = domain.ExecutionRefusalAcquisitionStale
		return decision, nil
	}

	// SINGLE USE is deliberately NOT checked here, and the omission is the same
	// one the acquisition service documents for its duplicate check.
	//
	// This function runs a second time inside the worker, by which point THIS
	// execution's own row exists and names this acquisition. A single-use check
	// here would find that row and refuse the execution as a duplicate of
	// itself -- so every recreation would fail at the mutation point, having
	// passed every other check.
	//
	// It is enforced in admit, which runs once when the request is accepted,
	// and guaranteed by a unique index on acquisition_id that no race can get
	// past. See ExecutionService.admit.

	// ---- the plan --------------------------------------------------------

	plan, err := s.evidence.Plan(ctx, acquisition.PlanID)
	if errors.Is(err, store.ErrNotFound) {
		decision.Refusal = domain.ExecutionRefusalPlanMissing
		return decision, nil
	}
	if err != nil {
		return decision, err
	}
	decision.Plan = plan

	current, err := s.evidence.CurrentPlan(ctx, plan.ContainerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		decision.Refusal = domain.ExecutionRefusalPlanMissing
		return decision, nil
	case err != nil:
		return decision, err
	case current.PlanID != plan.PlanID:
		// A superseded plan means the operator approved an assessment that has
		// since been replaced. The newer one may say something different, and
		// acting on the older is acting on an opinion nobody currently holds.
		decision.Refusal = domain.ExecutionRefusalPlanSuperseded
		return decision, nil
	}

	// The fingerprint the ACQUISITION recorded must still be the plan's. This
	// is the exact TOCTOU check across the gap between downloading an image and
	// applying it, which can be days.
	if plan.InputDigest == "" || acquisition.PlanDigest == "" ||
		plan.InputDigest != acquisition.PlanDigest {
		decision.Refusal = domain.ExecutionRefusalPlanChanged
		return decision, nil
	}

	// The recommendation gate. "unknown" refuses alongside "not recommended":
	// a gap in evidence is not permission.
	//
	// Stricter than acquisition's, deliberately. Downloading an image on a plan
	// that asks for manual review is reasonable -- the operator is reviewing.
	// Replacing a running container on one is not, UNLESS the review has
	// actually happened and was recorded against this exact immutable plan.
	switch plan.Risk.Recommendation {
	case domain.RecommendProceed, domain.RecommendCaution:
		// Unchanged.

	case domain.RecommendManualReview:
		// The plan asks for a person. Stage 17.7 gives that request an answer:
		// a plan-bound approval is evidence the review HAPPENED, and it
		// satisfies exactly that one fact.
		//
		// It is not a bypass. Nothing below this line is skipped -- the
		// container, the snapshot, the dependencies, the digest, the policy,
		// the self-update refusal and every preservation check all still run,
		// in the same order, immediately before anything is stopped. The
		// approval replaces one refusal with one permission and changes nothing
		// else.
		//
		// A deployment with no approval service configured behaves exactly as
		// it did before: refused.
		if s.approvals == nil {
			decision.Refusal = domain.ExecutionRefusalApprovalMissing
			return decision, nil
		}
		refusal, err := s.approvals.ApprovalFor(ctx, plan)
		if err != nil {
			// Could not establish that a review happened. That establishes
			// nothing, and nothing is not permission -- so this becomes a
			// REFUSAL rather than an error: the caller gets a decision that
			// names why the recreation will not proceed, and the host is
			// untouched either way.
			decision.Refusal = domain.ExecutionRefusalApprovalMissing
			//nolint:nilerr // Fails closed: the error IS the refusal.
			return decision, nil
		}
		if refusal != domain.PlanApprovalRefusalNone {
			decision.Refusal = domain.ExecutionRefusalApprovalMissing
			return decision, nil
		}

	default:
		// notRecommended and unknown. Refused whatever anybody approved: the
		// first is the model arguing against the change, and the second is a
		// gap in evidence, which is not a thing a person can vouch for.
		decision.Refusal = domain.ExecutionRefusalRecommendation
		return decision, nil
	}

	// ---- the inventory ---------------------------------------------------
	//
	// Checked BEFORE the container, because every check below reads the
	// inventory's view and a stale view would make all of them meaningless
	// rather than merely imprecise.

	refresh, err := s.evidence.LastRefresh(ctx)
	if err != nil {
		return decision, err
	}
	if refresh == nil || refresh.StartedAt.IsZero() {
		decision.Refusal = domain.ExecutionRefusalInventoryStale
		return decision, nil
	}
	if s.now().UTC().Sub(refresh.StartedAt.UTC()) > s.cfg.InventoryFreshness {
		decision.Refusal = domain.ExecutionRefusalInventoryStale
		return decision, nil
	}

	// ---- the container ---------------------------------------------------

	container, err := s.evidence.Container(ctx, plan.ContainerID)
	if errors.Is(err, store.ErrNotFound) {
		decision.Refusal = domain.ExecutionRefusalContainerMissing
		return decision, nil
	}
	if err != nil {
		return decision, err
	}
	if container == nil || !container.Overview.Present {
		decision.Refusal = domain.ExecutionRefusalContainerMissing
		return decision, nil
	}

	decision.ContainerID = container.Overview.ID
	decision.ContainerName = domain.NormaliseContainerName(container.Overview.Name)
	decision.OldImage = container.Overview.Image.Raw
	decision.OldImageID = container.Overview.ImageID
	// What the container is RUNNING, not what its reference happens to spell.
	//
	// This value's only job is to survive into the execution record so that a
	// ROLLBACK can put lineage back onto the artefact it restored. Taking it
	// from the declared reference left it empty for every tag-created container,
	// so a rollback restored the original while lineage went on claiming the
	// replacement's digest -- and the next pass then read the workload as
	// already up to date.
	decision.OldImageDigest = container.RunningDigest
	if decision.OldImageDigest == "" {
		// A digest-pinned reference still answers when the image itself is not
		// in the inventory yet.
		decision.OldImageDigest = container.Overview.Image.Digest
	}

	// The container must still be running the image the plan assessed. If it is
	// not, something else has already changed it, and the plan describes a
	// container that no longer exists in that form.
	if !sameImage(container.Overview.Image.Raw, plan.CurrentImage) {
		decision.Refusal = domain.ExecutionRefusalContainerChanged
		return decision, nil
	}

	// States a recreation cannot safely start from. Restarting and removing are
	// transitional -- the container is already moving and stopping it would race
	// the daemon. Dead means the daemon itself could not clean it up.
	switch container.Overview.State {
	case domain.StateRunning, domain.StateExited, domain.StateCreated, domain.StatePaused:
	default:
		decision.Refusal = domain.ExecutionRefusalContainerState
		return decision, nil
	}

	// ---- the names -------------------------------------------------------
	//
	// Derived and checked HERE, before anything is stopped. A container whose
	// name is too long to park, or whose parked name is already taken, cannot
	// be recreated -- and discovering that after the original is stopped would
	// be the worst possible moment.

	if !domain.RecreatableContainerName(decision.ContainerName) {
		decision.Refusal = domain.ExecutionRefusalNameUnavailable
		return decision, nil
	}

	// ---- invariant A: what shares this container's namespace --------------
	//
	// HERE, in the preflight, because the preflight is what runs before the
	// mutation point -- once when the request is made, and again inside the
	// worker immediately before anything is stopped.
	//
	// The live experiment established that STOPPING is the moment dependents
	// break: a container sharing this one's network namespace is left running
	// with no network, silently, and `docker restart` cannot repair it. So a
	// provider whose dependents cannot be positively established as repairable
	// is a provider that must not be stopped, and the check has to be on this
	// side of the line.
	//
	// Placed after the name check and before the snapshot deliberately: it is a
	// property of the HOST rather than of HarborMaster's records, and grouping
	// it with the cheap structural checks would suggest it is one.
	if refusal := s.providerRebindable(ctx, decision.ContainerName); refusal != domain.ExecutionRefusalNone {
		decision.Refusal = refusal
		return decision, nil
	}

	// ---- the snapshot ----------------------------------------------------
	//
	// # Assurance runs HERE, and it is a measurement rather than a preparation
	//
	// This is the last point before the mutation, and it is reached twice: once
	// when the request is made and once inside the worker immediately before
	// anything is stopped. Both matter. Capturing only at request time would
	// leave the whole time-of-check/time-of-use window between the request and
	// the stop unguarded, which is the window a configuration change actually
	// lands in.
	//
	// What assurance establishes is not "a snapshot exists" but "the snapshot
	// that describes this container NOW is the one the plan was assessed
	// against". Those are different claims, and only the second one makes the
	// plan's risk assessment mean anything.
	//
	// A capture that writes a NEW row is therefore not a success here. It is the
	// discovery that the container was reconfigured after it was assessed. The
	// new snapshot is kept -- it is the evidence the next planner pass builds
	// from -- but it does not authorise the plan already in flight. Reading it
	// into that plan is precisely the thing the immutable-evidence invariant
	// forbids, and the reason this file may not simply take the newest baseline
	// and carry on.
	//
	// # Why this write is acceptable inside a decision function
	//
	// Capture reaches HarborMaster's own container repository and never Docker
	// -- see the comment on SnapshotContainers -- and it is idempotent: an
	// unchanged configuration deduplicates transactionally against
	// (container_id, checksum) and writes nothing. Running the preflight twice
	// therefore captures at most once.

	if s.assurance != nil && s.assurance.Available() {
		result := s.assurance.EnsureCurrent(ctx, decision.ContainerID, domain.SnapshotTriggerPreUpdate)
		switch {
		case result.Outcome == AssuranceNotReady:
			if s.cfg.RequireSnapshot {
				decision.Refusal = domain.ExecutionRefusalRestoreReadiness
				return decision, nil
			}
		case !result.Usable():
			// No baseline could be established. Fails closed under the gate, and
			// leaves the host untouched: nothing below this line has run.
			if s.cfg.RequireSnapshot {
				decision.Refusal = domain.ExecutionRefusalSnapshotMissing
				return decision, nil
			}
		case plan.SnapshotID != result.SnapshotID:
			// THE STALENESS CHECK.
			//
			// Gated on RequireSnapshot with the rest of the snapshot policy, and
			// deliberately so. A deployment that switched the gate off has said
			// it will recreate without snapshot evidence; on such a deployment a
			// plan legitimately carries SnapshotID 0 while assurance produces a
			// real id, and refusing on that difference would break it. Every
			// other time-of-use check -- the plan still being current, the
			// container still running the assessed image, the digest -- is
			// ungated and still runs.
			if s.cfg.RequireSnapshot {
				decision.Refusal = domain.ExecutionRefusalSnapshotChanged
				return decision, nil
			}
			decision.SnapshotID = result.SnapshotID
		default:
			decision.SnapshotID = result.SnapshotID
		}
	}

	baseline, err := s.evidence.Baseline(ctx, decision.ContainerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if s.cfg.RequireSnapshot {
			decision.Refusal = domain.ExecutionRefusalSnapshotMissing
			return decision, nil
		}
	case err != nil:
		return decision, err
	default:
		decision.SnapshotID = baseline.ID
		// A warning is accepted; a failed readiness check is not. A warning
		// means the reference point is imperfect, which is the normal state of
		// a real estate. NotReady means restoration would fail, and recreating
		// without a usable record of what came before is the situation this
		// gate exists to prevent.
		if s.cfg.RequireSnapshot && baseline.ReadinessStatus == domain.ReadinessNotReady {
			decision.Refusal = domain.ExecutionRefusalRestoreReadiness
			return decision, nil
		}
	}

	// ---- policy ----------------------------------------------------------

	evaluation, err := s.evidence.PolicyEvaluation(ctx, decision.ContainerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		decision.Refusal = domain.ExecutionRefusalPolicyStale
		return decision, nil
	case err != nil:
		return decision, err
	}
	if s.now().UTC().Sub(evaluation.EvaluatedAt.UTC()) > s.cfg.PolicyFreshness {
		decision.Refusal = domain.ExecutionRefusalPolicyStale
		return decision, nil
	}
	// An INCOMPLETE pass did not establish compliance; it stopped looking. That
	// is not the same as finding nothing, and treating it as such is exactly
	// the fail-open mistake this codebase refuses everywhere else.
	if !evaluation.Complete {
		decision.Refusal = domain.ExecutionRefusalPolicyStale
		return decision, nil
	}
	// A critical violation means the organisation has already said this
	// container should not be running as it is. Replacing it is not the moment
	// to overrule that.
	if plan.PolicyOpen > 0 && plan.PolicyMaxSeverity == domain.PolicySeverityCritical {
		decision.Refusal = domain.ExecutionRefusalPolicyViolation
		return decision, nil
	}

	// ---- registry intelligence -------------------------------------------
	//
	// Weaker evidence than the local image check below, and checked for a
	// different reason: the local image establishes WHAT will run, while the
	// intel establishes that HarborMaster's picture of the image's provenance
	// is not so old that the plan resting on it is meaningless.

	reference, normalised := normalisedReference(plan.CurrentImage)
	if !normalised {
		decision.Refusal = domain.ExecutionRefusalRegistryStale
		return decision, nil
	}
	intel, err := s.evidence.Intel(ctx, reference.Canonical)
	switch {
	case errors.Is(err, store.ErrNotFound):
		decision.Refusal = domain.ExecutionRefusalRegistryStale
		return decision, nil
	case err != nil:
		return decision, err
	}
	if intel.Status != domain.CheckOK || intel.LastSuccessAt == nil {
		decision.Refusal = domain.ExecutionRefusalRegistryStale
		return decision, nil
	}

	// ---- the target ------------------------------------------------------

	decision.Target = domain.ExecutionTarget{
		Registry:   acquisition.Target.Registry,
		Repository: acquisition.Target.Repository,
		Digest:     acquisition.Target.Digest,
		Reference:  acquisition.Target.Reference,
		ImageID:    acquisition.AcquiredImageID,
		Platform:   acquisition.Target.Platform,
	}
	if !decision.Target.Valid() {
		decision.Refusal = domain.ExecutionRefusalDigestMismatch
		return decision, nil
	}
	// The acquisition's own verification must have found the approved digest.
	// A succeeded acquisition always did; checked anyway, because this is the
	// value the container is about to be created from.
	if acquisition.AcquiredDigest != "" && acquisition.AcquiredDigest != acquisition.Target.Digest {
		decision.Refusal = domain.ExecutionRefusalDigestMismatch
		return decision, nil
	}

	// ---- the host --------------------------------------------------------

	if !s.dockerReachable(ctx) {
		// Nothing can be established about the host, so nothing may be done to
		// it. Fails closed.
		decision.Refusal = domain.ExecutionRefusalDockerUnavailable
		return decision, nil
	}

	// The image must still be PRESENT LOCALLY, carrying the approved digest and
	// targeting this platform. The acquisition proved all three at download
	// time; this proves them now, because an operator can prune an image
	// between the two.
	if refusal := s.verifyLocalImage(ctx, decision.Target); refusal != domain.ExecutionRefusalNone {
		decision.Refusal = refusal
		return decision, nil
	}

	return decision, nil
}

// derivedNames fills in the parked and quarantine names for a decision.
//
// Separate from the preflight because it needs the execution id, which does not
// exist until the record is created. The preflight proves the names CAN be
// derived; this derives them.
func derivedNames(decision *executionDecision, executionID string) bool {
	parked, ok := domain.ParkedContainerName(decision.ContainerName, executionID)
	if !ok {
		return false
	}
	quarantine, ok := domain.QuarantineContainerName(decision.ContainerName, executionID)
	if !ok {
		return false
	}
	decision.ParkedName = parked
	decision.QuarantineName = quarantine
	return true
}

// verifyLocalImage confirms the approved image is on the host.
//
// Inspected by the DIGEST-PINNED reference, so the daemon resolves exactly what
// was approved rather than whatever a tag currently points at. Fails closed:
// an inspection that cannot be performed establishes nothing.
func (s *ExecutionService) verifyLocalImage(
	ctx context.Context,
	target domain.ExecutionTarget,
) domain.ExecutionRefusal {
	image, err := s.runtime.InspectImage(ctx, target.PinnedReference())
	if err != nil {
		return domain.ExecutionRefusalImageMissing
	}
	if image == nil {
		return domain.ExecutionRefusalImageMissing
	}
	if !imageCarriesDigest(*image, target.Digest) {
		return domain.ExecutionRefusalDigestMismatch
	}
	if !target.Platform.Empty() &&
		(image.OS != target.Platform.OS || image.Architecture != target.Platform.Architecture) {
		return domain.ExecutionRefusalPlatformMismatch
	}
	return domain.ExecutionRefusalNone
}

// nameAvailable reports whether no container on the host holds this name.
//
// Read through the READ-ONLY runtime rather than through the inventory, because
// this is the one question whose answer must be current to the second: a name
// collision discovered after the original is parked is a recreation that cannot
// complete and cannot be undone.
//
// A listing failure yields false -- the name is treated as unavailable -- for
// the usual reason: a check that could not be performed establishes nothing.
func (s *ExecutionService) nameAvailable(ctx context.Context, names ...string) bool {
	containers, err := s.runtime.ListContainers(ctx)
	if err != nil {
		return false
	}

	taken := make(map[string]struct{}, len(containers))
	for _, container := range containers {
		taken[domain.NormaliseContainerName(container.Name)] = struct{}{}
	}
	for _, name := range names {
		if name == "" {
			return false
		}
		if _, used := taken[name]; used {
			return false
		}
	}
	return true
}

// dockerReachable reports whether the daemon answered.
//
// A boolean rather than an error: a daemon that is down means the preflight
// cannot establish anything about the host, which is a refusal rather than a
// fault. The underlying error carries daemon detail and is deliberately
// discarded rather than propagated toward a response.
func (s *ExecutionService) dockerReachable(ctx context.Context) bool {
	_, err := s.runtime.Ping(ctx)
	return err == nil
}

// sameImage reports whether two image references name the same thing.
//
// Compared after normalisation, because the inventory and the planner can spell
// the same image differently -- "nginx", "nginx:latest", and
// "docker.io/library/nginx:latest" are one image written three ways, and
// refusing on the spelling would refuse almost every real container.
//
// Falls back to a trimmed exact comparison when either side cannot be
// normalised, which is stricter rather than looser.
func sameImage(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return false
	}

	left, leftOK := normalisedReference(a)
	right, rightOK := normalisedReference(b)
	if !leftOK || !rightOK {
		return false
	}
	return left.Canonical == right.Canonical
}
