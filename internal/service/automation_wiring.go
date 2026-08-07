package service

import (
	"context"
	"errors"

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
) AutomationEvidence {
	return &automationEvidence{
		containers:   containers,
		plans:        plans,
		acquisitions: acquisitions,
		executions:   executions,
	}
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
