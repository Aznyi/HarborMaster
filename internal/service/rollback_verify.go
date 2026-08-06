package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Proving a restored original.
//
// # Four proofs, and all four must pass
//
//   - HEALTH or STABILITY. Is the container actually working again?
//   - IMAGE. Is it on the image the recreation recorded it as running?
//   - PRESERVATION. Is it configured the way it was before the rollback moved
//     it?
//   - NETWORK. Is it attached to everything it was attached to?
//
// A rollback is recorded as succeeded only when every one of them reads
// `passed`. An `unknown` is not a pass: a proof that could not be PERFORMED
// establishes nothing.
//
// # What "preservation" means for a rollback, and why it differs
//
// A recreation compares the REPLACEMENT against the ORIGINAL's captured
// configuration, because the replacement is a new container that is supposed to
// reproduce an old one.
//
// A rollback has no new container. The original was never reconfigured -- the
// recreation only stopped and renamed it -- so the property worth proving is
// that the ROLLBACK did not change it either. The comparison is therefore
// against the projection taken during validation, BEFORE anything moved.
//
// The name is excluded from that comparison by construction: BuildPreservation
// Summary records the container name for display and does not compare it. A
// rollback renames the container on purpose, and a comparison that failed on
// the change it was asked to make would fail every time.
//
// # Why the image check uses the EXECUTION's record
//
// The rollback's own record cannot establish what the original should be
// running -- it copied that from the execution. Comparing against the
// execution's recorded old image is what makes "this is the container that was
// replaced" a checked fact rather than an assumption carried forward.

// verify runs all four proofs, in the order that fails cheapest first.
//
// Returns RollbackFailureNone when every proof passed. The verification record
// is filled in as it goes, so a failure carries the verdicts that were reached
// as well as the one that failed.
func (s *RollbackService) verify(ctx, parent context.Context, work *rollbackWork) domain.RollbackFailure {
	// ---- health or stability ----------------------------------------------
	//
	// First, because it is the slowest and the most likely to fail, and because
	// every proof below it is meaningless on a container that is not up.
	if failure := s.verifyHealth(ctx, work); failure != domain.RollbackFailureNone {
		return failure
	}

	// ---- everything else, from ONE inspection -----------------------------
	//
	// The remaining three read the same container state, so they read it once.
	// Three inspections could disagree with each other, and a verification that
	// can contradict itself is not a verification.
	inspection, err := s.runtime.InspectContainer(ctx, work.decision.OriginalID)
	if err != nil || inspection == nil {
		// The checks could not be PERFORMED. They stay unknown, and the
		// rollback fails: nothing was established about what is now running.
		s.logger.WarnContext(ctx, "could not inspect the restored original for verification",
			slog.String("rollbackId", work.rollback.RollbackID))
		return domain.RollbackFailurePreservation
	}
	detail := inspection.Detail

	// ---- identity ----------------------------------------------------------
	//
	// The inspection was BY id, so this is expected to hold. Checked anyway: an
	// adapter that resolved a prefix would otherwise let a shorter id match the
	// wrong container, and every proof below would then be about that one.
	if detail.Overview.ID != work.decision.OriginalID {
		return domain.RollbackFailurePreservation
	}

	// ---- image -------------------------------------------------------------

	if sameRecordedImage(detail, work.decision.OriginalImageID, work.decision.OriginalImage) {
		work.verification.Image = domain.VerificationPassed
	} else {
		work.verification.Image = domain.VerificationFailed
		return domain.RollbackFailureImageMismatch
	}

	// ---- configuration preservation ---------------------------------------

	actual := domain.BuildPreservationSummary(detail, s.digester())
	actual.DigestKeyID = s.digestKeyID()

	report := domain.ComparePreservation(work.decision.Baseline, actual)
	work.verification.Report = &report
	work.verification.Preservation = report.Status

	if report.Status != domain.VerificationPassed {
		s.logger.WarnContext(ctx, "the restored original's configuration is not what it was",
			slog.String("rollbackId", work.rollback.RollbackID),
			slog.Int("checked", report.Checked),
			slog.Int("matched", report.Matched),
			slog.Bool("unverifiable", report.Unverifiable))
		return domain.RollbackFailurePreservation
	}

	// ---- networks ----------------------------------------------------------
	//
	// Compared against the BASELINE inspection rather than against the
	// projection, even though the projection covers them too. The projection
	// would already have failed above; this asks the narrower question that
	// matters on its own -- is the container on every network it was on -- and
	// answers it in its own vocabulary, because a container on the wrong
	// networks is reachable by the wrong things.
	if s.verifyNetworks(work.decision.BaselineDetail, detail) {
		work.verification.Network = domain.VerificationPassed
	} else {
		work.verification.Network = domain.VerificationFailed
		return domain.RollbackFailureNetwork
	}

	return domain.RollbackFailureNone
}

// verifyNetworks reports whether the restored original is on every network it
// was on before the rollback.
//
// Reuses the execution service's comparison: it is the same question about the
// same shape of data, and two implementations of "is this container on the
// right networks" would eventually disagree.
func (s *RollbackService) verifyNetworks(before, after domain.ContainerDetail) bool {
	return networksPreserved(before, after)
}

// -------------------------------------------------------- health waiting --

// verifyHealth waits for the restored original to prove itself up.
//
// Two paths, because containers come in two kinds:
//
//   - A container that declares a HEALTH CHECK gets a real verdict. Polled
//     until healthy, and an explicit `unhealthy` fails IMMEDIATELY rather than
//     waiting out the clock -- an unhealthy verdict is an answer.
//   - A container that declares none is held to a STABILITY window: it must
//     stay running, continuously, for the configured period. Weaker evidence,
//     and recorded as such.
//
// Whether a health check is declared is read from the BASELINE inspection taken
// before the rollback moved anything, not from the container as it is now. The
// container is the thing being tested, and letting it choose which standard it
// is judged by would let one that lost its health check pass the weaker one.
//
// Bounded, cancellable, and polled at a configured interval that cannot be set
// low enough to become a busy loop against the Docker socket.
func (s *RollbackService) verifyHealth(ctx context.Context, work *rollbackWork) domain.RollbackFailure {
	deadline := s.now().UTC().Add(s.cfg.StartupTimeout)

	declared := healthCheckDeclared(work.decision.BaselineDetail)
	work.verification.HealthChecked = declared

	ticker := time.NewTicker(s.cfg.HealthPollInterval)
	defer ticker.Stop()

	// runningSince tracks the start of the current uninterrupted running
	// stretch, for the stability path. Reset whenever the container is seen not
	// running, so one that restarts has to earn the whole window again.
	var runningSince time.Time
	if !declared {
		work.verification.StabilitySeconds = int(s.cfg.StabilityPeriod / time.Second)
	}

	for {
		inspection, err := s.runtime.InspectContainer(ctx, work.decision.OriginalID)
		switch {
		case err != nil, inspection == nil:
			// A container that vanished, or a daemon that stopped answering.
			// Neither establishes health, so neither is treated as progress --
			// but a transient inspection failure should not fail the rollback
			// outright while there is still budget, so the loop continues and
			// the deadline decides.
			runningSince = time.Time{}

		default:
			state := inspection.Detail.State

			if declared {
				switch state.Health {
				case domain.HealthHealthy:
					work.verification.Health = domain.VerificationPassed
					work.verification.HealthState = domain.HealthHealthy
					return domain.RollbackFailureNone

				case domain.HealthUnhealthy:
					// An answer, not a delay. Failing now rather than at the
					// deadline gets the operator to the recovery plan sooner --
					// and this is the case where something is not serving, so
					// sooner matters more than usual.
					work.verification.Health = domain.VerificationFailed
					work.verification.HealthState = domain.HealthUnhealthy
					return domain.RollbackFailureUnhealthy

				case domain.HealthNone:
					// It reports no health check although it declared one
					// before the rollback. The verdict can never arrive, so
					// this fails rather than spinning until the deadline.
					if !state.Running {
						work.verification.Health = domain.VerificationFailed
						work.verification.HealthState = domain.HealthNone
						return domain.RollbackFailureNotStable
					}
				}
				work.verification.HealthState = state.Health

			} else {
				if state.Running {
					if runningSince.IsZero() {
						runningSince = s.now().UTC()
					}
					if s.now().UTC().Sub(runningSince) >= s.cfg.StabilityPeriod {
						work.verification.Health = domain.VerificationPassed
						work.verification.HealthState = domain.HealthNone
						return domain.RollbackFailureNone
					}
				} else {
					runningSince = time.Time{}
					if state.State == domain.StateExited || state.State == domain.StateDead {
						// It came up and fell over. Waiting out the clock would
						// add nothing: the container is not going to become
						// stable by itself.
						work.verification.Health = domain.VerificationFailed
						work.verification.HealthState = domain.HealthNone
						return domain.RollbackFailureNotStable
					}
				}
			}
		}

		if s.now().UTC().After(deadline) {
			work.verification.Health = domain.VerificationFailed
			if declared {
				return domain.RollbackFailureHealthTimeout
			}
			return domain.RollbackFailureNotStable
		}

		select {
		case <-ctx.Done():
			// Shutdown or the mutation budget. Recorded as unknown rather than
			// failed: the wait was cut short, which establishes nothing about
			// the container.
			work.verification.Health = domain.VerificationUnknown
			return domain.RollbackFailureTimeout
		case <-ticker.C:
		}
	}
}
