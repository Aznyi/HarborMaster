package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Adopting a replacement the record never named.
//
// # The window this closes
//
// The original is parked. ContainerCreate is issued. The daemon completes it,
// and HarborMaster never learns the id: the client's deadline expired first,
// the checkpoint write failed, or the process died between the two. A
// container now holds the production name and no record names it. Nothing can
// stop it, park it, or roll it back, and the rollback preflight -- rightly --
// treats it as a stranger and refuses.
//
// # What counts as evidence
//
// Every replacement is created carrying two labels only HarborMaster writes:
// the execution that created it and the original it replaces, by full id. A
// container is adopted when ALL of the following hold, re-read from the live
// host at the moment of adoption:
//
//   - it holds the production name and is not the original;
//   - its execution label names this execution;
//   - its original label names the parked original;
//   - it is on the approved image;
//   - the record says the original was parked and nothing further was
//     recorded, which is the only arrangement an unrecorded replacement can
//     be in.
//
// The labels are identity evidence, not authorisation. Anyone who can run
// `docker run -l` can write them; what they cannot write is HarborMaster's own
// record of which execution parked which original under which name, and every
// label is checked against that record. A mismatch on any part refuses, and a
// refusal touches nothing.
//
// # Adoption is a read and a record, never a mutation
//
// The pass lists containers, inspects one, and writes a checkpoint. It moves
// nothing. What happens next is what would have happened had the id been
// recorded in the first place: the pipeline quarantines it, or the rollback
// stops and parks it -- each through its own preflight against the live host.
//
// # It converges
//
// A record leaves the candidate set the moment a replacement id is written,
// so running the pass again finds nothing to do. The checkpoint write is
// monotonic, so a second adoption of the same record cannot move it backwards.

// Adoption bounds.
const (
	// executionAdoptionBatch bounds one reconciliation pass.
	executionAdoptionBatch = 50
	// executionAdoptionWindow bounds how long after its failure a record is
	// re-examined. A create that landed does so within seconds of the failure;
	// a record still empty a day later has nothing coming, and re-reading it
	// on every sweep forever would be a listing against a privileged socket
	// for nothing.
	executionAdoptionWindow = 24 * time.Hour
)

// adoptionRefusal decides, from the record and one live inspection, whether a
// container holding the production name is this execution's replacement.
//
// Returns the reason it is not, in HarborMaster's own words, or "" when every
// piece of evidence agrees. Pure: it reads nothing and changes nothing.
func adoptionRefusal(execution domain.Execution, candidate domain.ContainerDetail) string {
	if execution.ReplacementID != "" {
		return "a replacement is already recorded for this recreation"
	}
	if execution.Checkpoint != domain.CheckpointOriginalParked {
		return "the recreation's checkpoint is not originalParked, so no unrecorded create can exist"
	}

	id := candidate.Overview.ID
	if !domain.ValidFullContainerID(id) {
		return "the candidate does not report a full container id"
	}
	if id == execution.ContainerID {
		return "the candidate is the original itself"
	}
	if domain.NormaliseContainerName(candidate.Overview.Name) != execution.ContainerName {
		return "the candidate does not hold the production name"
	}

	labels := make(map[string]string, len(candidate.Labels))
	for _, label := range candidate.Labels {
		labels[label.Key] = label.Value
	}
	if labels[domain.LabelExecutionOwner] != execution.ExecutionID {
		return "the candidate's execution label does not name this recreation"
	}
	if labels[domain.LabelReplacementOf] != execution.ContainerID {
		return "the candidate's original label does not name the parked original"
	}

	if !imageMatchesTarget(candidate, execution.Target) {
		return "the candidate is not on the approved image"
	}
	return ""
}

// adoptUnrecordedReplacement looks for the container this execution created
// and did not record, and records it.
//
// Returns the adopted id. Every path that does not adopt returns false and has
// touched nothing; every failure is logged and swallowed, because adoption is
// a best-effort improvement to a record that is already settled or about to
// be, and an adoption that could fail a recovery pass would be worse than
// none.
func (s *ExecutionService) adoptUnrecordedReplacement(
	ctx context.Context,
	execution domain.Execution,
) (string, bool) {
	if execution.ReplacementID != "" || execution.Checkpoint != domain.CheckpointOriginalParked {
		return "", false
	}
	if s.runtime == nil || s.store == nil {
		return "", false
	}

	containers, err := s.runtime.ListContainers(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "could not list containers to look for an unrecorded replacement",
			slog.String("executionId", execution.ExecutionID),
			slog.String("error", err.Error()))
		return "", false
	}

	for _, summary := range containers {
		if domain.NormaliseContainerName(summary.Name) != execution.ContainerName ||
			summary.ID == execution.ContainerID {
			continue
		}

		inspection, err := s.runtime.InspectContainer(ctx, summary.ID)
		if err != nil || inspection == nil {
			// Gone between the listing and the inspection, or unreadable.
			// Nothing was established, so nothing is adopted.
			continue
		}

		if reason := adoptionRefusal(execution, inspection.Detail); reason != "" {
			// A container holds the production name and is NOT this
			// execution's. Said at WARN: an operator reading why a rollback
			// refused with nameUnavailable needs this line.
			s.logger.WarnContext(ctx, "a container holds the production name and was not adopted",
				slog.String("executionId", execution.ExecutionID),
				slog.String("containerName", execution.ContainerName),
				slog.String("candidateId", domain.ShortenID(summary.ID)),
				slog.String("reason", reason))
			continue
		}

		if err := s.store.Checkpoint(ctx, store.ExecutionCheckpointWrite{
			ExecutionID:   execution.ExecutionID,
			Checkpoint:    domain.CheckpointReplacementCreated,
			Detail:        "adopted a replacement the create produced after HarborMaster stopped waiting for it, identified by its ownership labels",
			ReplacementID: summary.ID,
		}, s.now().UTC()); err != nil {
			s.logger.ErrorContext(ctx, "identified an unrecorded replacement but could not record it",
				slog.String("executionId", execution.ExecutionID),
				slog.String("replacementId", domain.ShortenID(summary.ID)),
				slog.String("error", err.Error()))
			return "", false
		}

		s.logger.WarnContext(ctx, "adopted a replacement the record did not name",
			slog.String("executionId", execution.ExecutionID),
			slog.String("containerName", execution.ContainerName),
			slog.String("replacementId", domain.ShortenID(summary.ID)))
		return summary.ID, true
	}
	return "", false
}

// Reconcile adopts replacements that settled failures left unrecorded.
//
// Run at startup after the recovery pass, and on every sweep. Reads the
// bounded candidate set, examines the live host once per candidate, and
// rewrites the recovery plan of any record it adopted for. Idempotent: an
// adopted record is no longer a candidate.
func (s *ExecutionService) Reconcile(ctx context.Context) {
	if s.store == nil || s.runtime == nil {
		return
	}

	candidates, err := s.store.AdoptionCandidates(ctx,
		s.now().UTC().Add(-executionAdoptionWindow), executionAdoptionBatch)
	if err != nil {
		s.logger.WarnContext(ctx, "could not read recreations that may have left an unrecorded replacement",
			slog.String("error", err.Error()))
		return
	}

	for _, execution := range candidates {
		if ctx.Err() != nil {
			return
		}
		adopted, ok := s.adoptUnrecordedReplacement(ctx, execution)
		if !ok {
			continue
		}
		execution.ReplacementID = adopted
		execution.Checkpoint = domain.CheckpointReplacementCreated

		// The plan describes the host as it is now known to be: both
		// containers present, the replacement unproved.
		plan := domain.BuildRecoveryPlan(domain.RecoveryContext{
			ExecutionID:           execution.ExecutionID,
			ContainerName:         execution.ContainerName,
			OriginalID:            execution.ContainerID,
			ParkedName:            execution.ParkedName,
			ReplacementID:         execution.ReplacementID,
			Checkpoint:            execution.Checkpoint,
			Failure:               execution.Failure,
			OriginalRestartPolicy: execution.OriginalRestartPolicy.Encode(),
			MutationAttempted:     true,
		})
		change := store.ExecutionChange{
			ExecutionID: execution.ExecutionID,
			From:        []domain.ExecutionState{domain.ExecutionFailed},
			To:          domain.ExecutionFailed,
			Failure:     execution.Failure,
			Refusal:     execution.Refusal,
			Message:     execution.Message,
			Detail:      "the recovery plan was rewritten around the adopted replacement",
			Recovery:    plan,
		}
		// A restore the rollback service refused because a stranger held the
		// name is a restore worth asking for again now that the stranger is
		// this execution's own replacement. Cleared here; asked below.
		if execution.Restore.State == domain.RestoreRefused {
			change.Restore = &domain.ExecutionRestore{}
		}
		if _, err := s.store.Advance(ctx, change, s.now().UTC()); err != nil {
			s.logger.WarnContext(ctx, "adopted a replacement but could not rewrite the recovery plan",
				slog.String("executionId", execution.ExecutionID),
				slog.String("error", err.Error()))
			continue
		}
		if execution.Restore.State == domain.RestoreRefused {
			s.restoreAfterFailure(ctx, execution.ExecutionID)
		}
	}
}
