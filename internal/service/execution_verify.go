package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Proving a replacement.
//
// # Four proofs, and all four must pass
//
//   - HEALTH or STABILITY. Does the container actually work?
//   - IMAGE. Is it running the digest that was approved?
//   - PRESERVATION. Is it configured the way the original was?
//   - NETWORK. Is it attached to everything the original was attached to?
//
// The parked original is removed only when every one of them reads `passed`.
// An `unknown` is not a pass: a proof that could not be PERFORMED establishes
// nothing, and treating "we could not check" as "it is fine" is the failure
// mode this whole feature is arranged to avoid.
//
// # Why network is separate from preservation
//
// Preservation is a fidelity question and network is a security question. A
// container attached to the wrong set of networks is reachable by the wrong
// things and can reach the wrong things, which is a different kind of wrong
// from a mismatched memory limit -- so it gets its own verdict, its own
// classification, and its own sentence in the operator's record.

// verify runs all four proofs, in the order that fails cheapest first.
//
// Returns ExecutionFailureNone when every proof passed. The verification record
// on the pipeline is filled in as it goes, so a failure carries the verdicts
// that were reached as well as the one that failed.
func (s *ExecutionService) verify(ctx, parent context.Context, work *pipeline) domain.ExecutionFailure {
	// ---- health or stability ----------------------------------------------
	//
	// First, because it is the slowest and the most likely to fail, and because
	// every proof below it is meaningless on a container that is not up.
	if failure := s.verifyHealth(ctx, parent, work); failure != domain.ExecutionFailureNone {
		return failure
	}

	// ---- everything else, from ONE inspection -----------------------------
	//
	// The remaining three read the same container state, so they read it once.
	// Three inspections could disagree with each other, and a verification that
	// can contradict itself is not a verification.
	inspection, err := s.runtime.InspectContainer(ctx, work.replacementID)
	if err != nil || inspection == nil {
		// The checks could not be PERFORMED. They stay unknown, and the
		// recreation fails: nothing was established about what is now running.
		s.logger.WarnContext(ctx, "could not inspect the replacement for verification",
			slog.String("executionId", work.execution.ExecutionID))
		return domain.ExecutionFailurePreservation
	}
	detail := inspection.Detail

	// ---- image -------------------------------------------------------------

	if s.verifyImage(detail, work.decision.Target) {
		work.verification.Image = domain.VerificationPassed
	} else {
		work.verification.Image = domain.VerificationFailed
		return domain.ExecutionFailureImageMismatch
	}

	// ---- configuration preservation ---------------------------------------

	actual := domain.BuildPreservationSummary(detail, s.digester())
	actual.DigestKeyID = s.digestKeyID()

	report := domain.ComparePreservation(work.expected, actual)
	work.verification.Report = &report
	work.verification.Preservation = report.Status

	if report.Status != domain.VerificationPassed {
		s.logger.WarnContext(ctx, "the replacement's configuration does not match the original",
			slog.String("executionId", work.execution.ExecutionID),
			slog.Int("checked", report.Checked),
			slog.Int("matched", report.Matched),
			slog.Bool("unverifiable", report.Unverifiable))
		return domain.ExecutionFailurePreservation
	}

	// ---- networks ----------------------------------------------------------
	//
	// Compared against the ORIGINAL's attachments rather than against the
	// preservation projection, even though the projection covers them too. The
	// projection would already have failed above; this asks the narrower
	// question that matters on its own -- is the replacement on every network
	// the original was on -- and answers it in its own vocabulary.
	if s.verifyNetworks(work.captured.Detail(), detail) {
		work.verification.Network = domain.VerificationPassed
	} else {
		work.verification.Network = domain.VerificationFailed
		return domain.ExecutionFailureNetwork
	}

	return domain.ExecutionFailureNone
}

// verifyImage reports whether the replacement is running the approved image.
//
// Compared on the DIGEST, not the reference. A reference is a name and can be
// repointed; the digest is the content, and the content is what was approved.
func (s *ExecutionService) verifyImage(
	detail domain.ContainerDetail,
	target domain.ExecutionTarget,
) bool {
	// The image the container was created from, as the daemon resolved it. The
	// container was created from a digest-pinned reference, so this is expected
	// to carry the digest directly.
	if detail.Overview.Image.Digest == target.Digest {
		return true
	}
	// The resolved image, cross-checked. A container records the image ID it
	// was created from, and the acquisition recorded which ID carried the
	// approved digest; agreeing on that is the same proof by another route.
	if target.ImageID != "" && detail.Overview.ImageID == target.ImageID {
		return true
	}
	// Last, the image record the inspection carried, which lists every repo
	// digest the local image is known by.
	if detail.Image != nil && imageCarriesDigest(*detail.Image, target.Digest) {
		return true
	}
	return false
}

// verifyNetworks reports whether the replacement is on every network the
// original was.
//
// Names and aliases only. Addresses are assigned by the daemon and a
// replacement always has different ones, so comparing them would fail every
// recreation while proving nothing about reachability.
//
// The comparison is one-directional on purpose: the replacement must have at
// least the original's attachments. An EXTRA network would be caught by the
// preservation check, which compares the sets exactly; this narrower check
// exists to give the security-relevant half its own verdict.
func (s *ExecutionService) verifyNetworks(original, replacement domain.ContainerDetail) bool {
	return networksPreserved(original, replacement)
}

// networksPreserved reports whether `after` carries every attachment `before`
// had.
//
// Shared by recreation and rollback. Both ask the same question about the same
// shape of data -- is this container on the networks it is supposed to be on --
// and two implementations of it would eventually disagree, which for a
// security-relevant verdict is worse than either being slightly wrong.
func networksPreserved(before, after domain.ContainerDetail) bool {
	original, replacement := before, after

	expected := make(map[string][]string, len(original.Networks))
	for _, attachment := range original.Networks {
		expected[attachment.NetworkName] = attachment.Aliases
	}

	actual := make(map[string]map[string]struct{}, len(replacement.Networks))
	for _, attachment := range replacement.Networks {
		aliases := make(map[string]struct{}, len(attachment.Aliases))
		for _, alias := range attachment.Aliases {
			aliases[alias] = struct{}{}
		}
		actual[attachment.NetworkName] = aliases
	}

	for name, aliases := range expected {
		present, attached := actual[name]
		if !attached {
			return false
		}
		for _, alias := range aliases {
			// The daemon adds the container's own short id as an alias on user
			// defined networks. A replacement is a different container and gets
			// a different one, so an alias equal to the ORIGINAL's short id is
			// not a configured alias and is not required.
			if isGeneratedAlias(alias, original.Overview.ID, original.Overview.ShortID) {
				continue
			}
			if _, found := present[alias]; !found {
				return false
			}
		}
	}
	return true
}

// isGeneratedAlias reports whether an alias is the daemon's own.
func isGeneratedAlias(alias, containerID, shortID string) bool {
	if alias == "" {
		return true
	}
	if shortID != "" && alias == shortID {
		return true
	}
	return len(containerID) >= 12 && alias == containerID[:12]
}

// -------------------------------------------------------- health waiting --

// verifyHealth waits for the replacement to prove itself up.
//
// Two paths, because containers come in two kinds and pretending otherwise
// would mean either rejecting half of them or accepting half of them blindly:
//
//   - A container that declares a HEALTH CHECK gets a real verdict. Polled
//     until healthy, and an explicit `unhealthy` fails IMMEDIATELY rather than
//     waiting out the clock -- an unhealthy verdict is an answer, and making an
//     operator wait five minutes for it adds nothing.
//   - A container that declares none is held to a STABILITY window: it must
//     stay running, continuously, for the configured period. Weaker evidence,
//     and recorded as such: it establishes that the container did not crash on
//     startup and nothing more.
//
// Bounded, cancellable, and polled at a configured interval that cannot be set
// low enough to become a busy loop against the Docker socket.
func (s *ExecutionService) verifyHealth(ctx, parent context.Context, work *pipeline) domain.ExecutionFailure {
	deadline := s.now().UTC().Add(s.cfg.StartupTimeout)

	// Whether the container declares a health check is read from the CAPTURE
	// rather than from the replacement: it is configuration, it was captured
	// from the original, and reading it from the thing being tested would let a
	// replacement that lost its health check be judged by the weaker standard.
	declared := healthCheckDeclared(work.captured.Detail())
	work.verification.HealthChecked = declared

	ticker := time.NewTicker(s.cfg.HealthPollInterval)
	defer ticker.Stop()

	// runningSince tracks the start of the current uninterrupted running
	// stretch, for the stability path. Reset whenever the container is seen not
	// running, so a container that restarts has to earn the whole window again.
	var runningSince time.Time
	if !declared {
		work.verification.StabilitySeconds = int(s.cfg.StabilityPeriod / time.Second)
	}

	for {
		inspection, err := s.runtime.InspectContainer(ctx, work.replacementID)
		switch {
		case err != nil:
			// A container that vanished, or a daemon that stopped answering.
			// Neither establishes health, so neither is treated as progress --
			// but a transient inspection failure should not fail the recreation
			// outright while there is still budget, so the loop continues and
			// the deadline decides.
			runningSince = time.Time{}

		case inspection == nil:
			runningSince = time.Time{}

		default:
			state := inspection.Detail.State

			if declared {
				switch state.Health {
				case domain.HealthHealthy:
					work.verification.Health = domain.VerificationPassed
					work.verification.HealthState = domain.HealthHealthy
					return domain.ExecutionFailureNone

				case domain.HealthUnhealthy:
					// An answer, not a delay. Failing now rather than at the
					// deadline gets the operator to the recovery plan sooner.
					work.verification.Health = domain.VerificationFailed
					work.verification.HealthState = domain.HealthUnhealthy
					return domain.ExecutionFailureUnhealthy

				case domain.HealthNone:
					// The replacement reports no health check although the
					// original declared one. That is a configuration difference
					// the preservation check would also catch, and here it means
					// the health verdict can never arrive -- so it fails rather
					// than spinning until the deadline.
					if !state.Running {
						work.verification.Health = domain.VerificationFailed
						work.verification.HealthState = domain.HealthNone
						return domain.ExecutionFailureNotStable
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
						return domain.ExecutionFailureNone
					}
				} else {
					// Exited, restarting, or dead. The window starts again if it
					// comes back, and the deadline is what eventually decides.
					runningSince = time.Time{}
					if state.State == domain.StateExited || state.State == domain.StateDead {
						work.verification.Health = domain.VerificationFailed
						work.verification.HealthState = domain.HealthNone
						return domain.ExecutionFailureNotStable
					}
				}
			}
		}

		if !s.now().UTC().Before(deadline) {
			work.verification.Health = domain.VerificationFailed
			if declared {
				return domain.ExecutionFailureHealthTimeout
			}
			return domain.ExecutionFailureNotStable
		}

		select {
		case <-ctx.Done():
			// The mutation budget expiring. Nothing was established, and the
			// verdict stays failed rather than unknown: the pipeline is about
			// to quarantine the replacement, and a container that was never
			// proved must not be reported as though it might have been fine.
			work.verification.Health = domain.VerificationFailed
			return domain.ExecutionFailureInterrupted

		case <-parent.Done():
			// SHUTDOWN. Watched separately from ctx, which carries a grace
			// period so a Docker call in flight can finish -- there is no
			// reason to spend that grace on a polling loop. Verification is
			// entirely reads, so abandoning it changes nothing on the host.
			work.verification.Health = domain.VerificationFailed
			return domain.ExecutionFailureInterrupted

		case <-ticker.C:
		}
	}
}

// healthCheckDeclared reports whether a container declares a usable health
// check.
//
// A check that is explicitly DISABLED does not count. Docker's convention is
// that `NONE` turns off an image's health check, and treating a disabled one as
// declared would leave the wait spinning for a verdict that never comes.
func healthCheckDeclared(detail domain.ContainerDetail) bool {
	check := detail.HealthCheck
	if check == nil || check.Disabled {
		return false
	}
	if len(check.Test) == 0 {
		return false
	}
	// Docker spells "no health check" as a Test of exactly ["NONE"].
	if len(check.Test) == 1 && check.Test[0] == "NONE" {
		return false
	}
	return true
}
