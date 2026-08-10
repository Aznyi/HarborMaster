package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The rollback preflight.
//
// # One function answers "may this happen", and it is called twice
//
// Once synchronously inside Request, so an operator gets a real refusal rather
// than a queued job that fails a moment later. Once inside the worker,
// immediately before the first mutation, because minutes may have passed and a
// person may have been rearranging containers by hand.
//
// Only the second verdict decides whether anything is touched. The first exists
// entirely to give a better answer sooner.
//
// # Identity is checked against the LIVE host, not against the record
//
// A container id is stable, but what answers to one is not: an id can be
// removed and the record left behind. So every check re-inspects the daemon and
// compares what it finds against what HarborMaster recorded -- the name, the
// image, and for the original, that it is the container the recreation parked.
//
// This is the TOCTOU defence, and it is why the mutation requests take full
// container ids and no names. Between this check and the mutation, a name can
// come to mean a different container; an id cannot.
//
// # Every refusal is a specific one
//
// A rollback that is refused tells the operator WHICH check said no, because
// "the original is gone" and "somebody else is already rolling this back" call
// for completely different responses.

// rollbackDecision is everything the pipeline needs, derived entirely from
// HarborMaster's own records and the live host.
//
// No field here is ever filled from caller input.
type rollbackDecision struct {
	ExecutionID string

	ContainerName string
	OriginalID    string
	ParkedName    string
	ReplacementID string

	OriginalImage    string
	OriginalImageID  string
	ReplacementImage string
	// OriginalDigest is the manifest digest the container ran BEFORE the
	// recreation. It is what image lineage is returned to, so that after a
	// rollback HarborMaster compares the registry against the artefact that is
	// actually running again rather than the one it stopped.
	OriginalDigest string

	// Baseline is the original's configuration projection taken BEFORE anything
	// moved. The post-restore comparison is made against it, which is what
	// proves the rollback did not change the container it restored.
	Baseline domain.PreservationSummary
	// BaselineDetail is the original's inspected state at validation time,
	// kept for the network comparison.
	BaselineDetail domain.ContainerDetail
}

// assess runs the whole preflight and returns the derived decision.
//
// Two halves, and they are split deliberately -- see assessRecords for why.
// This one runs both, and it is the only version whose verdict is allowed to
// start a mutation.
//
// Returns a refusal rather than an error for every "the world is not what the
// record says" case; an error means HarborMaster could not establish the
// answer, which is itself a refusal at the call sites but is worth
// distinguishing in a log.
//
// excluding names the rollback being assessed, or is empty at request time. The
// conflict and limit checks skip it: on the second run the rollback is itself in
// flight, and a check that counted it would refuse every rollback for
// conflicting with itself.
func (s *RollbackService) assess(
	ctx context.Context,
	executionID string,
	excluding string,
	now time.Time,
) (rollbackDecision, domain.RollbackRefusal, error) {
	decision, refusal, err := s.assessRecords(ctx, executionID, excluding)
	if err != nil || refusal != domain.RollbackRefusalNone {
		return decision, refusal, err
	}
	return s.assessHost(ctx, decision, now)
}

// assessRecords runs the half of the preflight that reads only HarborMaster's
// own records.
//
// # Why this is a separate function
//
// The other half contacts the DAEMON: a ping, two inspections, and a container
// listing. That is four privileged socket calls, and the eligibility answer is
// read from `GET /executions/{id}` -- a page the UI polls while a recreation is
// running. An authenticated reader able to turn one HTTP request into four
// Docker calls, repeatable at will, is a denial-of-service amplifier pointed at
// the socket, which is exactly what secure-coding rule 1.4 exists to prevent.
//
// So the ADVISORY answer is built from records alone. It is honest about what
// it is: it says whether this recreation looks like one that could be undone,
// and nothing about whether the containers are still where the record says.
// Nothing is lost by that, because the answer was never permission -- the
// request path runs the full assessment, and the pipeline runs it AGAIN against
// the live host immediately before the first mutation.
func (s *RollbackService) assessRecords(
	ctx context.Context,
	executionID string,
	excluding string,
) (rollbackDecision, domain.RollbackRefusal, error) {
	var decision rollbackDecision

	// ---- the execution record ---------------------------------------------

	execution, err := s.evidence.Execution(ctx, executionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return decision, domain.RollbackRefusalExecutionMissing, nil
		}
		return decision, domain.RollbackRefusalNone, err
	}

	decision.ExecutionID = execution.ExecutionID
	decision.ContainerName = execution.ContainerName
	decision.OriginalID = execution.ContainerID
	decision.ParkedName = execution.ParkedName
	decision.ReplacementID = execution.ReplacementID
	decision.OriginalImage = execution.OldImage
	decision.OriginalImageID = execution.OldImageID
	decision.OriginalDigest = execution.OldImageDigest
	decision.ReplacementImage = execution.Target.Reference

	if execution.State.Active() {
		return decision, domain.RollbackRefusalExecutionActive, nil
	}

	// ---- is there an arrangement to undo ----------------------------------
	//
	// The checkpoint decides, not the state. A recreation that failed and one
	// that succeeded can leave the same host arrangement, and the checkpoint is
	// what says which arrangement it is.
	if execution.OriginalRemoved || execution.Checkpoint == domain.CheckpointOriginalRemoved {
		return decision, domain.RollbackRefusalOriginalRemoved, nil
	}
	if !domain.RollbackSufficientCheckpoint(execution.Checkpoint) {
		// Two different situations produce this, and they are told apart so the
		// operator is not sent looking for the wrong thing.
		if execution.Checkpoint == domain.CheckpointNone && execution.MutatedAt != nil {
			// A mutation was issued and never confirmed. HarborMaster does not
			// know where the containers are, and a rollback would be a guess.
			return decision, domain.RollbackRefusalCheckpointUncertain, nil
		}
		return decision, domain.RollbackRefusalNothingToRollBack, nil
	}

	// Both identities must be recorded. A record naming only one could not be
	// rolled back safely, and could not be recovered after a restart.
	if decision.OriginalID == "" || decision.ReplacementID == "" ||
		decision.ContainerName == "" || decision.ParkedName == "" {
		return decision, domain.RollbackRefusalNothingToRollBack, nil
	}
	if !domain.RollbackableContainerName(decision.ContainerName) {
		// The derived parked name would exceed the bound. Refusing is honest;
		// silently truncating would break the uniqueness the rollback id gives.
		return decision, domain.RollbackRefusalNameUnavailable, nil
	}

	// ---- single successful use --------------------------------------------

	rolledBack, err := s.store.SucceededForExecution(ctx, executionID)
	if err != nil {
		return decision, domain.RollbackRefusalNone, err
	}
	if rolledBack {
		return decision, domain.RollbackRefusalAlreadyRolledBack, nil
	}

	// ---- conflicts ---------------------------------------------------------
	//
	// Checked before the daemon is contacted: a conflict is cheap to detect and
	// there is no point inspecting a host we are not going to touch.
	activeRollback, err := s.store.ActiveForContainer(ctx, decision.ContainerName, excluding)
	if err != nil {
		return decision, domain.RollbackRefusalNone, err
	}
	if activeRollback {
		return decision, domain.RollbackRefusalConflict, nil
	}

	activeExecution, err := s.evidence.ExecutionActiveForContainer(ctx, decision.OriginalID)
	if err != nil {
		return decision, domain.RollbackRefusalNone, err
	}
	if activeExecution {
		return decision, domain.RollbackRefusalConflict, nil
	}

	active, err := s.store.ActiveCount(ctx, excluding)
	if err != nil {
		return decision, domain.RollbackRefusalNone, err
	}
	if active >= s.cfg.MaxConcurrent {
		return decision, domain.RollbackRefusalLimit, nil
	}

	return decision, domain.RollbackRefusalNone, nil
}

// assessHost runs the half of the preflight that contacts the daemon.
//
// Only ever reached from assess, which is only ever reached from the request
// path and from the pipeline's revalidation. Never from a read endpoint.
func (s *RollbackService) assessHost(
	ctx context.Context,
	decision rollbackDecision,
	now time.Time,
) (rollbackDecision, domain.RollbackRefusal, error) {
	// ---- the host is reachable and its picture is fresh --------------------

	if _, err := s.runtime.Ping(ctx); err != nil {
		//nolint:nilerr // an unreachable daemon is a REFUSAL, not a fault: the
		// check could not be performed, and that establishes nothing.
		return decision, domain.RollbackRefusalDockerUnavailable, nil
	}

	age, known, err := s.evidence.InventoryAge(ctx, now)
	if err != nil {
		return decision, domain.RollbackRefusalNone, err
	}
	if !known || age > s.cfg.InventoryFreshness {
		return decision, domain.RollbackRefusalInventoryStale, nil
	}

	// ---- the two containers are the ones the record names ------------------

	refusal, baseline, err := s.verifyIdentities(ctx, &decision)
	if err != nil {
		return decision, domain.RollbackRefusalNone, err
	}
	if refusal != domain.RollbackRefusalNone {
		return decision, refusal, nil
	}
	decision.BaselineDetail = baseline

	// ---- the production name is free, or held by the replacement -----------

	if refusal := s.nameHeldSafely(ctx, decision); refusal != domain.RollbackRefusalNone {
		return decision, refusal, nil
	}

	// ---- the original's configuration can be projected ---------------------
	//
	// Taken BEFORE anything moves, and compared again afterwards. That is what
	// proves the rollback did not change the container it restored -- and a
	// projection that cannot be built now means the proof could never be made,
	// so the rollback is refused rather than started unprovable.
	summary := domain.BuildPreservationSummary(baseline, s.digester())
	summary.DigestKeyID = s.digestKeyID()
	if s.digester() == nil || summary.Fingerprint == "" || len(summary.Fields) == 0 {
		return decision, domain.RollbackRefusalUnverifiable, nil
	}
	decision.Baseline = summary

	return decision, domain.RollbackRefusalNone, nil
}

// verifyIdentities re-reads both containers and checks them against the record.
//
// Returns the ORIGINAL's inspected detail, which the caller projects into the
// pre-mutation baseline and later compares the restored container against.
func (s *RollbackService) verifyIdentities(
	ctx context.Context,
	decision *rollbackDecision,
) (domain.RollbackRefusal, domain.ContainerDetail, error) {
	var empty domain.ContainerDetail

	// ---- the preserved original -------------------------------------------

	original, err := s.runtime.InspectContainer(ctx, decision.OriginalID)
	if err != nil || original == nil {
		// Absent rather than unreachable: the daemon answered the ping a moment
		// ago. Somebody removed it. A REFUSAL, not a fault.
		//nolint:nilerr // see above.
		return domain.RollbackRefusalOriginalMissing, empty, nil
	}

	// The id is what every mutation targets, so the id is what must match. It
	// does by construction -- the inspection was BY that id -- and it is
	// checked anyway, because an adapter that resolved a prefix would otherwise
	// let a shorter id match the wrong container.
	if original.Detail.Overview.ID != decision.OriginalID {
		return domain.RollbackRefusalOriginalIdentity, empty, nil
	}

	// It must still be parked under the name the recreation gave it. A
	// different name means somebody has already been rearranging, and a
	// rollback on top of that would be undoing something other than what the
	// record describes.
	currentName := domain.NormaliseContainerName(original.Detail.Overview.Name)
	if currentName != decision.ParkedName {
		return domain.RollbackRefusalOriginalIdentity, empty, nil
	}

	// It must still be on the image the recreation recorded it as running. A
	// different image means this is not the container that was replaced, and
	// restoring it would be restoring the wrong thing.
	if !sameRecordedImage(original.Detail, decision.OriginalImageID, decision.OriginalImage) {
		return domain.RollbackRefusalOriginalIdentity, empty, nil
	}

	// ---- the replacement ---------------------------------------------------

	replacement, err := s.runtime.InspectContainer(ctx, decision.ReplacementID)
	if err != nil || replacement == nil {
		//nolint:nilerr // absent rather than unreachable; a REFUSAL, not a fault.
		return domain.RollbackRefusalReplacementMissing, empty, nil
	}
	if replacement.Detail.Overview.ID != decision.ReplacementID {
		return domain.RollbackRefusalReplacementIdentity, empty, nil
	}

	// The replacement must hold the PRODUCTION name or a name the recreation
	// derived for it. Anything else means a third party has renamed it, and the
	// rollback would be stopping a container it cannot account for.
	replacementName := domain.NormaliseContainerName(replacement.Detail.Overview.Name)
	if replacementName != decision.ContainerName &&
		!derivedFromContainer(replacementName, decision.ContainerName) {
		return domain.RollbackRefusalReplacementIdentity, empty, nil
	}

	return domain.RollbackRefusalNone, original.Detail, nil
}

// nameHeldSafely checks that the production name is not held by a third
// container.
//
// The name must be free, or held by the replacement itself. Held by anything
// else means restoring it would collide, and the rollback would fail after
// having already stopped the replacement.
func (s *RollbackService) nameHeldSafely(
	ctx context.Context,
	decision rollbackDecision,
) domain.RollbackRefusal {
	containers, err := s.runtime.ListContainers(ctx)
	if err != nil {
		// The listing could not be read, so the check could not be PERFORMED.
		// Refusing is the fail-closed reading: a name collision discovered
		// after the replacement is stopped is a rollback that can neither
		// complete nor be undone.
		return domain.RollbackRefusalDockerUnavailable
	}

	for _, container := range containers {
		if domain.NormaliseContainerName(container.Name) != decision.ContainerName {
			continue
		}
		if container.ID == decision.ReplacementID {
			continue
		}
		return domain.RollbackRefusalNameUnavailable
	}
	return domain.RollbackRefusalNone
}

// sameRecordedImage reports whether a container is on the image a record names.
//
// The image ID is preferred: it is content-addressed and cannot be repointed. A
// reference comparison is the fallback for a record written before the id was
// resolvable, and a record with neither cannot establish identity at all --
// which is why an empty record fails rather than passes.
func sameRecordedImage(detail domain.ContainerDetail, imageID, reference string) bool {
	if imageID != "" {
		return detail.Overview.ImageID == imageID
	}
	if reference != "" {
		return detail.Overview.Image.Raw == reference
	}
	return false
}

// derivedFromContainer reports whether a name is one HarborMaster derived from
// a container's own name.
//
// A replacement may legitimately hold the production name, or a quarantine name
// its own recreation gave it after a failed verification. Both are names
// HarborMaster created from this container's name, and neither is a sign of
// interference.
func derivedFromContainer(name, containerName string) bool {
	if name == "" || containerName == "" {
		return false
	}
	for _, suffix := range []string{
		domain.QuarantineNameSuffix,
		domain.ParkedNameSuffix,
		domain.RollbackParkedNameSuffix,
	} {
		prefix := containerName + suffix
		if len(name) > len(prefix) && name[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// logRefusal records a preflight refusal at a level that matches its meaning.
func (s *RollbackService) logRefusal(
	ctx context.Context,
	rollbackID string,
	refusal domain.RollbackRefusal,
) {
	s.logger.InfoContext(ctx, "rollback refused by preflight",
		slog.String("rollbackId", rollbackID),
		slog.String("refusal", string(refusal)))
}
