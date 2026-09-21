package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Restoring a failed manual update.
//
// # The rule
//
// A manual recreation that fails after the mutation point leaves the original
// parked and stopped. HarborMaster now asks its own rollback service to put it
// back -- through the same request an operator's rollback button submits, so
// the same preflight runs against the live host and the same checkpointed
// pipeline moves the containers. Nothing here moves a container, and the
// rollback service may refuse.
//
// Unattended updates are NOT handled here. Their recovery belongs to the
// automation follower, which consults the governing policy first.
//
// # The outcome is recorded on the update, and the record converges
//
// The execution row carries what became of the restore: requested, restored,
// failed, refused, or unavailable. "requested" is advanced by a sweep that
// reads the rollback back by its request key -- which also re-requests a
// restore the process died before submitting, under the same key, so at most
// one rollback ever exists for one update however many times any path runs.

// Restore bounds.
const (
	// executionRestoreBatch bounds one sweep.
	executionRestoreBatch = 50
	// executionRestoreWindow bounds how long a requested restore whose rollback
	// never appears is re-examined. A rollback the service accepted exists
	// immediately; a row still pending a day later is one the rollback service
	// has refused on every sweep, and each refusal was recorded.
	executionRestoreWindow = 24 * time.Hour
)

// restoreApplies reports whether a settled record is one this service should
// try to restore.
//
// Pure. The service's own setting and capability are checked by the caller;
// this asks only whether the RECORD describes a manual update that failed
// after changing the host, in an arrangement the rollback service could act
// on, with no restore recorded yet.
func restoreApplies(execution domain.Execution) bool {
	if execution.State != domain.ExecutionFailed || execution.Automatic() {
		return false
	}
	if !execution.Checkpoint.HostChanged() || execution.OriginalRemoved {
		return false
	}
	if execution.Restore.State != "" {
		return false
	}
	return domain.RollbackSufficientCheckpoint(execution.Checkpoint)
}

// willRestore reports whether a restore is about to be attempted for a record,
// so the failure notification can say so.
func (s *ExecutionService) willRestore(execution domain.Execution) bool {
	return s.cfg.RestoreOnFailure && restoreApplies(execution)
}

// restoreAfterFailure reads a settled execution back and, when it applies,
// asks for the original to be put back.
//
// Called from the pipeline's deferred conclusion, from the restart recovery
// pass, and after an adoption. Every path is bounded and every failure is
// logged and recorded rather than returned: the update is already over, and a
// restore that could fail its record would be worse than one that is merely
// unavailable.
func (s *ExecutionService) restoreAfterFailure(ctx context.Context, executionID string) {
	if !s.cfg.RestoreOnFailure || s.store == nil {
		return
	}

	writeCtx, cancel := GraceContext(ctx, executionWriteGrace, executionWriteGrace)
	defer cancel()

	execution, err := s.store.Get(writeCtx, executionID)
	if err != nil || !restoreApplies(execution) {
		return
	}
	s.requestRestore(writeCtx, execution)
}

// requestRestore submits the rollback and records what came of the request.
func (s *ExecutionService) requestRestore(ctx context.Context, execution domain.Execution) {
	id := execution.ExecutionID

	if s.restorer == nil || !s.restorer.Enabled() {
		s.recordRestore(ctx, execution, domain.ExecutionRestore{
			State: domain.RestoreUnavailable,
			Detail: "the previous container could not be restored automatically because " +
				"manual rollback is not enabled on this installation",
		}, nil)
		s.logger.WarnContext(ctx, "a manual update failed after changing the host and cannot be restored: rollback is not enabled",
			slog.String("executionId", id),
			slog.String("containerName", execution.ContainerName))
		return
	}

	// "requested" is written BEFORE the request leaves. A process that dies
	// between the two leaves a row the sweep re-examines, and the request key
	// makes the second ask find the first rollback rather than make another.
	if !s.recordRestore(ctx, execution, domain.ExecutionRestore{State: domain.RestoreRequested}, nil) {
		return
	}

	rollback, err := s.restorer.Request(ctx, RollbackRequest{
		ExecutionID: id,
		RequestKey:  domain.ManualRestoreRequestKey(id),
		// The operator who asked for the UPDATE, carried forward for the audit
		// trail exactly as automation carries its trigger. The request key,
		// not the requester, is what says nobody asked for the rollback itself.
		RequestedBy: execution.RequestedBy,
	})
	switch {
	case err == nil:
		s.recordRestore(ctx, execution, domain.ExecutionRestore{
			State: domain.RestoreRequested, RollbackID: rollback.RollbackID,
		}, nil)
		s.logger.WarnContext(ctx, "a manual update failed after changing the host; restoring the original",
			slog.String("executionId", id),
			slog.String("rollbackId", rollback.RollbackID),
			slog.String("containerName", execution.ContainerName))

	case errors.Is(err, ErrRollbackRefused):
		// The rollback preflight said no. That is the safety model working:
		// the host is not in an arrangement HarborMaster can undo without
		// guessing, and a person is needed.
		detail := "the rollback service refused to restore the original"
		var refused RollbackRefusedError
		if errors.As(err, &refused) {
			detail = refused.Refusal.Explain()
		}
		s.recordRestore(ctx, execution, domain.ExecutionRestore{
			State: domain.RestoreRefused, Detail: detail,
		}, nil)
		s.logger.ErrorContext(ctx, "a manual update failed after changing the host and the automatic restore was refused",
			slog.String("executionId", id),
			slog.String("containerName", execution.ContainerName),
			slog.String("detail", detail))

	default:
		// Could not even ask. Left as requested: the sweep asks again under
		// the same key, and a rollback service that keeps erroring keeps the
		// row pending inside the window rather than losing the restore.
		s.logger.ErrorContext(ctx, "could not request the automatic restore of a failed manual update",
			slog.String("executionId", id),
			slog.String("error", err.Error()))
	}
}

// AdvanceRestores settles the restores whose rollbacks have concluded, and
// re-asks for any whose rollback never appeared.
//
// Run on every sweep. Idempotent: a settled restore leaves the pending set.
func (s *ExecutionService) AdvanceRestores(ctx context.Context) {
	if !s.cfg.RestoreOnFailure || s.store == nil || s.restorer == nil {
		return
	}

	pending, err := s.store.RestoresPending(ctx,
		s.now().UTC().Add(-executionRestoreWindow), executionRestoreBatch)
	if err != nil {
		s.logger.WarnContext(ctx, "could not read the updates whose restore is pending",
			slog.String("error", err.Error()))
		return
	}

	for _, execution := range pending {
		if ctx.Err() != nil {
			return
		}
		rollback, found, err := s.restorer.ByRequestKey(ctx, domain.ManualRestoreRequestKey(execution.ExecutionID))
		if err != nil {
			s.logger.WarnContext(ctx, "could not read the rollback restoring a failed update",
				slog.String("executionId", execution.ExecutionID),
				slog.String("error", err.Error()))
			continue
		}
		if !found {
			// Recorded as requested, never accepted. The process died between
			// the two writes, or the request errored. Asked again, under the
			// same key.
			execution.Restore = domain.ExecutionRestore{}
			s.requestRestore(ctx, execution)
			continue
		}
		if !rollback.State.Terminal() {
			continue
		}

		switch rollback.State {
		case domain.RollbackSucceeded:
			plan := domain.RestoredRecoveryPlan(execution.ContainerName, rollback.RollbackID,
				execution.ReplacementID, rollback.ReplacementParkedName)
			s.recordRestore(ctx, execution, domain.ExecutionRestore{
				State: domain.RestoreRestored, RollbackID: rollback.RollbackID,
			}, plan)
			s.logger.WarnContext(ctx, "a failed manual update was restored: the original is serving again",
				slog.String("executionId", execution.ExecutionID),
				slog.String("rollbackId", rollback.RollbackID),
				slog.String("containerName", execution.ContainerName))

		case domain.RollbackFailed:
			s.recordRestore(ctx, execution, domain.ExecutionRestore{
				State: domain.RestoreFailed, RollbackID: rollback.RollbackID,
				Detail: rollback.Failure.Explain(),
			}, nil)
			s.logger.ErrorContext(ctx, "a failed manual update could NOT be restored; the service is down and needs attention",
				slog.String("executionId", execution.ExecutionID),
				slog.String("rollbackId", rollback.RollbackID),
				slog.String("containerName", execution.ContainerName),
				slog.String("rollbackFailure", string(rollback.Failure)))

		default:
			// Cancelled or expired: nobody restored anything.
			s.recordRestore(ctx, execution, domain.ExecutionRestore{
				State: domain.RestoreFailed, RollbackID: rollback.RollbackID,
				Detail: "the rollback that was to restore the original did not run (" + string(rollback.State) + ")",
			}, nil)
		}
	}
}

// recordRestore writes the restore outcome onto the settled execution, and
// the recovery plan that goes with it when one is supplied.
//
// A failed-to-failed transition: the update's own outcome is not touched.
func (s *ExecutionService) recordRestore(
	ctx context.Context,
	execution domain.Execution,
	restore domain.ExecutionRestore,
	plan *domain.RecoveryPlan,
) bool {
	moved, err := s.store.Advance(ctx, store.ExecutionChange{
		ExecutionID: execution.ExecutionID,
		From:        []domain.ExecutionState{domain.ExecutionFailed},
		To:          domain.ExecutionFailed,
		Failure:     execution.Failure,
		Refusal:     execution.Refusal,
		Message:     execution.Message,
		Detail:      restoreDetail(restore),
		Restore:     &restore,
		Recovery:    plan,
	}, s.now().UTC())
	if err != nil || !moved {
		s.logger.ErrorContext(ctx, "could not record the restore outcome on a failed update",
			slog.String("executionId", execution.ExecutionID),
			slog.String("restore", string(restore.State)),
			slog.Bool("moved", moved),
			slog.String("error", errorText(err)))
		return false
	}
	return true
}

// restoreDetail is the audit-trail note for a restore transition.
func restoreDetail(restore domain.ExecutionRestore) string {
	switch restore.State {
	case domain.RestoreRequested:
		if restore.RollbackID == "" {
			return "asking the rollback service to restore the original"
		}
		return "the original is being restored by rollback " + restore.RollbackID
	case domain.RestoreRestored:
		return "the original was restored and is serving again"
	case domain.RestoreFailed:
		return "the automatic restore did not complete; the service is down"
	case domain.RestoreRefused:
		return "the automatic restore was refused; the service is down"
	case domain.RestoreUnavailable:
		return "no automatic restore is available on this installation; the service is down"
	default:
		return "restore outcome recorded"
	}
}

// errorText renders an error for a log field, tolerating nil.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
