package service

import (
	"context"
	"log/slog"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The two namespace checks the recreation path runs, and where each one sits.
//
// # Invariant A runs BEFORE the stop, and that is the whole point
//
// The live experiment against Docker 29.6.2 established that STOPPING a
// namespace provider is the moment its dependents break. Not removing it, not
// recreating it -- stopping. A dependent left behind stays `Up`, keeps its PID,
// logs nothing, and has no network.
//
// So providerRebindable is called from the PREFLIGHT, which runs twice: once
// when the request is made, and once inside the worker immediately before the
// mutation point. Both are before `mutate`, which contains the first
// StopContainer call. TestProviderIsAssessedBeforeAnythingIsStopped drives a
// real pipeline with a recording mutator and asserts that a refused provider
// produced no Docker mutation at all.
//
// # The resolution runs after the capture and before the create
//
// It cannot run earlier: it rewrites the captured configuration, and there is
// no capture before the capture step. It must not run later: after the stop,
// a refusal is a container that is already down.
//
// Both fail CLOSED. A check that could not be performed establishes nothing.

// providerRebindable runs invariant A for the container about to be recreated.
//
// Returns ExecutionRefusalNone when the container is not a namespace provider,
// or is one and every dependent positively passed.
//
// # Why a nil resolver refuses rather than skips
//
// "The dependency subsystem is not wired" and "nothing shares this container's
// namespace" are opposite facts, and only the second is a reason to proceed. A
// build that cannot answer the question must not answer it optimistically --
// that is the same reasoning migration 0024's observation flag rests on.
//
// The cost of getting this wrong in the safe direction is a refusal an operator
// can see. The cost in the unsafe direction is three application containers
// running with no network and nothing reporting it.
func (s *ExecutionService) providerRebindable(
	ctx context.Context,
	containerName string,
) domain.ExecutionRefusal {
	if s.dependencies == nil {
		// Cannot be reached in a wired deployment: an architecture test fails
		// the build if the composition root stops supplying this. Refusing
		// rather than skipping keeps the property true for any build that
		// somehow does.
		s.logger.ErrorContext(ctx,
			"the dependency subsystem is not wired; refusing to recreate a container "+
				"without establishing what shares its namespace",
			slog.String("containerName",
				domain.SanitiseDisplayText(containerName, domain.MaxContainerNameBytes)))
		return domain.ExecutionRefusalDependentsNotRebindable
	}

	assessment, isProvider, err := s.dependencies.AssessProvider(ctx, containerName)
	if err != nil {
		// The check could not be performed. That establishes nothing, so it
		// cannot clear anything.
		s.logger.WarnContext(ctx,
			"could not establish whether other containers share this one's namespace; refusing",
			slog.String("containerName",
				domain.SanitiseDisplayText(containerName, domain.MaxContainerNameBytes)),
			slog.Any("error", err))
		return domain.ExecutionRefusalDependentsNotRebindable
	}
	if !isProvider {
		// Nothing shares this container's namespace. The ordinary case, and the
		// only one that clears without an assessment.
		return domain.ExecutionRefusalNone
	}
	if !assessment.MayStop() {
		blocked := assessment.Blocked[0]
		s.logger.WarnContext(ctx,
			"refusing to recreate a container because something sharing its namespace "+
				"could not be established as repairable",
			slog.String("containerName",
				domain.SanitiseDisplayText(containerName, domain.MaxContainerNameBytes)),
			slog.String("dependent",
				domain.SanitiseDisplayText(blocked.Dependent, domain.MaxContainerNameBytes)),
			// The closed-vocabulary refusal name, never a daemon string.
			slog.String("refusal", string(blocked.Refusal)))
		return domain.ExecutionRefusalDependentsNotRebindable
	}

	s.logger.InfoContext(ctx,
		"every container sharing this one's namespace can be reattached",
		slog.String("containerName",
			domain.SanitiseDisplayText(containerName, domain.MaxContainerNameBytes)),
		slog.Int("dependents", len(assessment.Rebindable)))
	return domain.ExecutionRefusalNone
}

// resolveNamespaces re-points a capture's shared-namespace references at the
// containers currently holding them.
//
// A no-op for the overwhelming majority of containers: one that shares no
// namespace has no references, and one whose provider is still live resolves to
// the id it already had.
//
// Every replacement id comes from HarborMaster's own records, re-verified
// against the live host. There is no parameter a caller could put an id into,
// and the docker package validates both ends of the mapping again before
// rewriting anything.
func (s *ExecutionService) resolveNamespaces(
	ctx context.Context,
	captured *docker.CapturedConfig,
) domain.ExecutionRefusal {
	references := captured.NamespaceReferences()
	if len(references) == 0 {
		return domain.ExecutionRefusalNone
	}

	if s.dependencies == nil {
		return domain.ExecutionRefusalNamespaceProviderMissing
	}

	resolved := make(map[string]string, len(references))
	for _, reference := range references {
		if reference.ContainerID == "" {
			// The capture claims a namespace share whose reference this build
			// cannot read. Refused rather than guessed at.
			s.logger.WarnContext(ctx,
				"a container declares a shared namespace HarborMaster cannot resolve; refusing",
				slog.String("namespace", string(reference.Kind)))
			return domain.ExecutionRefusalNamespaceProviderMissing
		}
		if _, done := resolved[reference.ContainerID]; done {
			continue
		}

		live, err := s.dependencies.ResolveNamespaceProvider(ctx, reference.ContainerID)
		if err != nil {
			s.logger.WarnContext(ctx,
				"could not establish which container now holds a shared namespace; refusing",
				slog.String("namespace", string(reference.Kind)),
				slog.String("capturedProvider", domain.ShortenID(reference.ContainerID)))
			return domain.ExecutionRefusalNamespaceProviderMissing
		}

		// The live host is the authority, not the record. A name that resolves
		// to an id the daemon does not currently have is a record that has moved
		// on since the last refresh.
		if !s.containerLive(ctx, live) {
			s.logger.WarnContext(ctx,
				"the container HarborMaster resolved a shared namespace to is not on the host; refusing",
				slog.String("namespace", string(reference.Kind)))
			return domain.ExecutionRefusalNamespaceProviderMissing
		}
		resolved[reference.ContainerID] = live

		if live != reference.ContainerID {
			s.logger.InfoContext(ctx,
				"re-pointed a shared namespace reference at the container currently holding it",
				slog.String("namespace", string(reference.Kind)),
				slog.String("from", domain.ShortenID(reference.ContainerID)),
				slog.String("to", domain.ShortenID(live)))
		}
	}

	if err := captured.RebindNamespaces(resolved); err != nil {
		s.logger.WarnContext(ctx,
			"a capture's shared namespace references could not be rewritten; refusing",
			slog.Any("error", err))
		return domain.ExecutionRefusalNamespaceProviderMissing
	}
	return domain.ExecutionRefusalNone
}

// recordDependencyOperation persists what must be reattached, BEFORE the
// provider is stopped.
//
// # Why this is the last thing before the mutation point
//
// The live experiment established that STOPPING a namespace provider is the
// instant its dependents lose their network, silently. A record written after
// that describes containers that are already broken; a record written before is
// what a restarted process reads to find out what was supposed to happen.
//
// So this is placed at the very edge of the mutation point, after the capture
// and after the namespace references have been resolved, and every failure in it
// refuses. Refusing here means nothing on the host has been touched --
// TestOperationPersistenceFailuresProduceZeroMutations asserts exactly that, by
// checking the mutator's call list is EMPTY rather than that an error came back.
//
// # An ordinary container gets no record
//
// A container nothing shares a namespace with returns an empty operation id and
// proceeds down the unchanged path. Manufacturing an empty operation for one
// would put a row in front of an operator describing nothing.
func (s *ExecutionService) recordDependencyOperation(
	ctx context.Context,
	work *pipeline,
) domain.ExecutionRefusal {
	if s.dependencies == nil {
		// Unreachable in a wired deployment. Refused rather than skipped, for
		// the same reason providerRebindable refuses: a build that cannot answer
		// the question must not answer it optimistically.
		return domain.ExecutionRefusalDependentsNotRebindable
	}

	operationID, isProvider, err := s.dependencies.EnsureOperation(ctx,
		work.decision.ContainerName,
		work.execution.PlanID,
		work.execution.RequestedBy)
	if err != nil {
		s.logger.WarnContext(ctx,
			"could not record what must be reattached after this container is replaced; refusing",
			slog.String("containerName", domain.SanitiseDisplayText(
				work.decision.ContainerName, domain.MaxContainerNameBytes)),
			slog.Any("error", err))
		return domain.ExecutionRefusalDependentsNotRebindable
	}
	if !isProvider {
		// Nothing shares this container's namespace. The ordinary case.
		return domain.ExecutionRefusalNone
	}

	work.operationID = operationID

	// Link the record to the execution performing it. A failure here costs the
	// history a cross-reference and does NOT refuse: the member set is already
	// durable, which is the property that matters, and the recovery path finds
	// the operation by provider rather than by execution.
	if err := s.dependencies.AttachProviderExecution(ctx, operationID,
		work.execution.ExecutionID); err != nil {
		s.logger.WarnContext(ctx, "could not link a dependency operation to its execution",
			slog.String("operationId", operationID), slog.Any("error", err))
	}

	s.logger.WarnContext(ctx,
		"replacing a container whose namespace other containers share",
		slog.String("operationId", operationID),
		slog.String("containerName", domain.SanitiseDisplayText(
			work.decision.ContainerName, domain.MaxContainerNameBytes)))
	return domain.ExecutionRefusalNone
}

// containerLive reports whether the daemon currently has this container.
//
// Uses the READ-ONLY runtime. A read, immediately before acting on the answer,
// which is the same discipline every other identity check in this pipeline
// follows.
func (s *ExecutionService) containerLive(ctx context.Context, containerID string) bool {
	if s.runtime == nil || !domain.ValidFullContainerID(containerID) {
		return false
	}
	inspection, err := s.runtime.InspectContainer(ctx, containerID)
	if err != nil || inspection == nil {
		return false
	}
	return inspection.Detail.Overview.ID == containerID
}
