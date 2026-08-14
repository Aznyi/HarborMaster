package service

import (
	"context"
	"errors"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Wiring the automation engine to the components it uses.
//
// # Why adapters rather than passing the real things in
//
// The engine's dependencies are declared as three narrow interfaces --
// AutomationEvidence, AutomationPipeline, AutomationStore -- and this file is
// where the real repositories and services are fitted to them.
//
// The indirection buys the property the whole subsystem rests on. The pipeline
// adapter is the ONLY place automation can reach a service that owns a Docker
// capability, and it exposes exactly five methods: three request submissions
// and two record reads. There is no method on it that pulls, creates, starts,
// stops, renames, or removes anything, because there is no such method to
// forward to -- the services expose requests, not operations.
//
// A test substitutes both interfaces wholesale and needs no Docker at all.

// ------------------------------------------------------------- evidence --

// automationEvidence adapts the repositories to AutomationEvidence.
type automationEvidence struct {
	containers   *store.ContainerRepository
	plans        *store.PlanRepository
	acquisitions *store.AcquisitionRepository
	executions   *store.ExecutionRepository
	// lineage and intel answer ONE question between them: which containers has
	// HarborMaster positively established as needing no update. Both may be
	// nil, and then nothing is established -- see CurrentAssessments.
	lineage *store.LineageRepository
	intel   *store.ImageIntelRepository
}

// NewAutomationEvidence builds the read-only evidence source.
//
// Every repository here is read from and never written to through this
// adapter: the interface it satisfies has no method that could.
func NewAutomationEvidence(
	containers *store.ContainerRepository,
	plans *store.PlanRepository,
	acquisitions *store.AcquisitionRepository,
	executions *store.ExecutionRepository,
	lineage *store.LineageRepository,
	intel *store.ImageIntelRepository,
) AutomationEvidence {
	return &automationEvidence{
		containers:   containers,
		plans:        plans,
		acquisitions: acquisitions,
		executions:   executions,
		lineage:      lineage,
		intel:        intel,
	}
}

// currentAssessmentFreshness is how recent a registry check must be for its
// answer to release a dependent.
//
// Matches the acquisition path's registry freshness, deliberately: the question
// is the same one -- "is this registry answer recent enough to act on" -- and
// two different windows would mean HarborMaster could consider evidence fresh
// enough to hold a container back but not fresh enough to move it, or the
// reverse.
//
// Not configurable. A deployment that could widen this could make a stale
// "everything is fine" release dependents indefinitely, and the failure would
// be silent.
const currentAssessmentFreshness = 24 * time.Hour

// CurrentAssessments returns HarborMaster's positive "needs no update"
// findings, by container name.
//
// # Two reads, whatever the estate size
//
// One for the tracked lineage rows, one for the registry evidence behind the
// references they name. The verdict itself is domain.AssessCurrent -- the same
// pure function, over the same inputs, that the planner reaches its own
// conclusion with, so the two cannot disagree about whether a container is
// settled.
//
// # Every failure direction establishes nothing
//
// A nil repository, an unreadable table, a container with no lineage, a
// reference never checked: all leave the container out of the map, which is the
// zero assessment, which holds its dependents. There is no path here that
// returns Established from an absence.
func (e *automationEvidence) CurrentAssessments(
	ctx context.Context,
	now time.Time,
) (map[string]domain.CurrentAssessment, error) {
	if e.lineage == nil || e.intel == nil {
		return map[string]domain.CurrentAssessment{}, nil
	}

	tracked, err := e.lineage.Tracked(ctx)
	if err != nil {
		return nil, err
	}
	if len(tracked) == 0 {
		return map[string]domain.CurrentAssessment{}, nil
	}

	// The distinct references, so a hundred containers on one image cost one
	// lookup rather than a hundred.
	seen := make(map[string]struct{}, len(tracked))
	references := make([]string, 0, len(tracked))
	for _, row := range tracked {
		if row.TrackingReference == "" {
			continue
		}
		if _, already := seen[row.TrackingReference]; already {
			continue
		}
		seen[row.TrackingReference] = struct{}{}
		references = append(references, row.TrackingReference)
	}

	evidence, err := e.intel.ByReferences(ctx, references)
	if err != nil {
		return nil, err
	}

	assessments := make(map[string]domain.CurrentAssessment, len(tracked))
	for _, row := range tracked {
		// The container id the assessment is checked against is the one LINEAGE
		// recorded. The pass compares its own observed id separately; what
		// matters here is that no caller supplies either.
		assessment := domain.AssessCurrent(
			row, evidence[row.TrackingReference], row.ContainerID,
			now, currentAssessmentFreshness)
		if !assessment.Established {
			// Only positive findings are carried. An unestablished entry and an
			// absent one mean the same thing, and storing the difference would
			// invite a reader to treat "we looked and could not tell" as a
			// weaker kind of established.
			continue
		}
		assessments[domain.NormaliseContainerName(row.ContainerName)] = assessment
	}
	return assessments, nil
}

func (e *automationEvidence) Targets(ctx context.Context) ([]store.AutomationTarget, bool, error) {
	return e.containers.AutomationTargets(ctx)
}

func (e *automationEvidence) CurrentPlan(ctx context.Context, containerID string) (domain.ChangePlan, error) {
	return e.plans.Current(ctx, containerID)
}

func (e *automationEvidence) AcquisitionActive(ctx context.Context, containerID string) (bool, error) {
	_, total, err := e.acquisitions.List(ctx, store.AcquisitionFilter{
		ContainerID: containerID,
		ActiveOnly:  true,
		Page:        store.Page{Limit: 1},
	})
	if err != nil {
		return false, err
	}
	return total > 0, nil
}

func (e *automationEvidence) ExecutionActive(ctx context.Context, containerID string) (bool, error) {
	return e.executions.ActiveForContainer(ctx, containerID)
}

func (e *automationEvidence) InFlightTotal(ctx context.Context) (int, error) {
	// Acquisitions and executions counted together, because the engine's
	// concurrency ceiling is on UPDATES rather than on either stage: a
	// container being pulled for and a container being recreated are both
	// outstanding work this pass must not add to without limit.
	acquiring, _, err := e.acquisitions.ActiveCount(ctx, "")
	if err != nil {
		return 0, err
	}
	recreating, err := e.executions.ActiveCount(ctx)
	if err != nil {
		return 0, err
	}
	return acquiring + recreating, nil
}

// ------------------------------------------------------------- pipeline --

// automationPipeline adapts the three services to AutomationPipeline.
//
// The whole mutation surface automation can reach, and it is five methods.
// Each Request forwards a request type whose fields are an identifier and an
// idempotency key; each service then runs its own preflight against the live
// host and may refuse. Nothing here can bypass that, because nothing here
// implements any part of it.
type automationPipeline struct {
	acquisitions *AcquisitionService
	executions   *ExecutionService
	rollbacks    *RollbackService
}

// NewAutomationPipeline builds the pipeline adapter.
//
// A nil service means the capability is absent in this deployment, which the
// Enabled reports below turn into a refusal rather than a panic.
func NewAutomationPipeline(
	acquisitions *AcquisitionService,
	executions *ExecutionService,
	rollbacks *RollbackService,
) AutomationPipeline {
	return &automationPipeline{
		acquisitions: acquisitions,
		executions:   executions,
		rollbacks:    rollbacks,
	}
}

func (p *automationPipeline) AcquisitionEnabled() bool {
	return p.acquisitions != nil && p.acquisitions.Enabled()
}

func (p *automationPipeline) ExecutionEnabled() bool {
	return p.executions != nil && p.executions.Enabled()
}

func (p *automationPipeline) RollbackEnabled() bool {
	return p.rollbacks != nil && p.rollbacks.Enabled()
}

func (p *automationPipeline) RequestAcquisition(
	ctx context.Context,
	request AcquisitionRequest,
) (domain.Acquisition, error) {
	if p.acquisitions == nil {
		return domain.Acquisition{}, ErrAcquisitionDisabled
	}
	return p.acquisitions.Request(ctx, request)
}

func (p *automationPipeline) RequestExecution(
	ctx context.Context,
	request ExecutionRequest,
) (domain.Execution, error) {
	if p.executions == nil {
		return domain.Execution{}, ErrExecutionRefused{Refusal: domain.ExecutionRefusalDisabled}
	}
	return p.executions.Request(ctx, request)
}

func (p *automationPipeline) RequestRollback(
	ctx context.Context,
	request RollbackRequest,
) (domain.Rollback, error) {
	if p.rollbacks == nil {
		return domain.Rollback{}, RollbackRefusedError{Refusal: domain.RollbackRefusalDisabled}
	}
	return p.rollbacks.Request(ctx, request)
}

func (p *automationPipeline) Acquisition(
	ctx context.Context,
	acquisitionID string,
) (domain.Acquisition, error) {
	if p.acquisitions == nil {
		return domain.Acquisition{}, store.ErrNotFound
	}
	acquisition, _, err := p.acquisitions.Get(ctx, acquisitionID)
	if err != nil {
		return domain.Acquisition{}, err
	}
	return acquisition, nil
}

func (p *automationPipeline) Execution(
	ctx context.Context,
	executionID string,
) (domain.Execution, error) {
	if p.executions == nil {
		return domain.Execution{}, store.ErrNotFound
	}
	execution, _, err := p.executions.Get(ctx, executionID)
	if err != nil {
		return domain.Execution{}, err
	}
	return execution, nil
}

// ErrAutomationNotFound is returned when an automation record names nothing.
var ErrAutomationNotFound = errors.New("no automation record with that identifier")
