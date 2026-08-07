package service_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "time/tzdata"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Engine tests: the pass, the follower, automatic rollback, and pausing.
//
// # Why fakes rather than a database and a daemon
//
// The properties under test are about ORCHESTRATION -- what the engine submits,
// in what order, and what it does with the answer. A real database would test
// the repository (which has its own tests), and a real daemon would test the
// three services (which have theirs).
//
// The pipeline fake is the important one. It records every request and lets a
// test decide what each one returns, so "does automation ever act on anything
// it did not derive from its own records" is checkable by INSPECTING WHAT IT
// ASKED FOR, which is the whole safety argument stated as an assertion.

// ------------------------------------------------------------- the fakes --

// fakeAutomationStore is an in-memory AutomationStore.
type fakeAutomationStore struct {
	mu sync.Mutex

	runs      []domain.AutomationRun
	decisions []domain.AutomationDecision
	pauses    []domain.PausedContainer
	failures  map[string]store.AutomationFailureCount
	settled   map[string]bool

	// startErr forces StartRun to fail, for the busy path.
	startErr error
}

func newFakeAutomationStore() *fakeAutomationStore {
	return &fakeAutomationStore{
		failures: make(map[string]store.AutomationFailureCount),
		settled:  make(map[string]bool),
	}
}

func (f *fakeAutomationStore) StartRun(_ context.Context, run domain.AutomationRun) (domain.AutomationRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return domain.AutomationRun{}, f.startErr
	}
	for _, existing := range f.runs {
		if existing.State == domain.RunRunning {
			return domain.AutomationRun{}, store.ErrAutomationRunActive
		}
	}
	run.State = domain.RunRunning
	run.ID = int64(len(f.runs) + 1)
	f.runs = append(f.runs, run)
	return run, nil
}

func (f *fakeAutomationStore) FinishRun(
	_ context.Context,
	runID string,
	state domain.AutomationRunState,
	counts domain.AutomationRun,
	completedAt time.Time,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.runs {
		if f.runs[i].RunID != runID || f.runs[i].State != domain.RunRunning {
			continue
		}
		f.runs[i].State = state
		f.runs[i].Considered = counts.Considered
		f.runs[i].Eligible = counts.Eligible
		f.runs[i].Submitted = counts.Submitted
		f.runs[i].Skipped = counts.Skipped
		f.runs[i].Failed = counts.Failed
		f.runs[i].Message = counts.Message
		finished := completedAt
		f.runs[i].CompletedAt = &finished
		return nil
	}
	return store.ErrNotFound
}

func (f *fakeAutomationStore) InterruptRuns(_ context.Context, _ time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for i := range f.runs {
		if f.runs[i].State == domain.RunRunning {
			f.runs[i].State = domain.RunInterrupted
			count++
		}
	}
	return count, nil
}

func (f *fakeAutomationStore) RecordDecisions(_ context.Context, decisions []domain.AutomationDecision) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisions = append(f.decisions, decisions...)
	return len(decisions), nil
}

func (f *fakeAutomationStore) PromoteApproved(_ context.Context, runID, containerName, acquisitionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.decisions {
		if f.decisions[i].RunID == runID &&
			f.decisions[i].ContainerName == containerName &&
			f.decisions[i].Verdict == domain.VerdictAwaitingApproval {
			f.decisions[i].Verdict = domain.VerdictUpdate
			f.decisions[i].AcquisitionID = acquisitionID
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeAutomationStore) AttachExecution(_ context.Context, runID, acquisitionID, executionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.decisions {
		if f.decisions[i].RunID == runID && f.decisions[i].AcquisitionID == acquisitionID {
			f.decisions[i].ExecutionID = executionID
		}
	}
	return nil
}

func (f *fakeAutomationStore) AttachRollback(_ context.Context, runID, executionID, rollbackID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.decisions {
		if f.decisions[i].RunID == runID && f.decisions[i].ExecutionID == executionID {
			f.decisions[i].RollbackID = rollbackID
		}
	}
	return nil
}

func (f *fakeAutomationStore) LatestRun(_ context.Context) (domain.AutomationRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.runs) == 0 {
		return domain.AutomationRun{}, store.ErrNotFound
	}
	return f.runs[len(f.runs)-1], nil
}

func (f *fakeAutomationStore) RunByID(_ context.Context, runID string) (domain.AutomationRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, run := range f.runs {
		if run.RunID == runID {
			return run, nil
		}
	}
	return domain.AutomationRun{}, store.ErrNotFound
}

func (f *fakeAutomationStore) ListRuns(
	_ context.Context,
	filter store.AutomationRunFilter,
) ([]domain.AutomationRun, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.AutomationRun, 0, len(f.runs))
	for i := len(f.runs) - 1; i >= 0; i-- {
		if filter.ActedOnly && f.runs[i].Submitted == 0 {
			continue
		}
		out = append(out, f.runs[i])
	}
	return out, len(out), nil
}

func (f *fakeAutomationStore) ListDecisions(
	_ context.Context,
	filter store.AutomationDecisionFilter,
) ([]domain.AutomationDecision, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]domain.AutomationDecision, 0, len(f.decisions))
	for _, decision := range f.decisions {
		if filter.RunID != "" && decision.RunID != filter.RunID {
			continue
		}
		if filter.ContainerName != "" && decision.ContainerName != filter.ContainerName {
			continue
		}
		if len(filter.Verdicts) > 0 {
			matched := false
			for _, verdict := range filter.Verdicts {
				if decision.Verdict == verdict {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		out = append(out, decision)
	}
	return out, len(out), nil
}

func (f *fakeAutomationStore) PendingDecisions(
	_ context.Context,
	limit int,
) ([]domain.AutomationDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]domain.AutomationDecision, 0, len(f.decisions))
	// Newest first, matching the repository.
	for i := len(f.decisions) - 1; i >= 0; i-- {
		decision := f.decisions[i]
		if decision.Verdict != domain.VerdictUpdate {
			continue
		}
		if decision.AcquisitionID == "" || f.settled[decisionKey(decision)] {
			continue
		}
		out = append(out, decision)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeAutomationStore) SettleDecision(
	_ context.Context,
	runID, containerName string,
	_ time.Time,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settled[runID+"/"+containerName] = true
	return nil
}

// decisionKey is the settled map's key.
func decisionKey(decision domain.AutomationDecision) string {
	return decision.RunID + "/" + decision.ContainerName
}

func (f *fakeAutomationStore) RunSummary(_ context.Context) (domain.AutomationRunSummary, error) {
	return domain.AutomationRunSummary{}, nil
}

func (f *fakeAutomationStore) CountAwaitingApproval(_ context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, decision := range f.decisions {
		if decision.Verdict == domain.VerdictAwaitingApproval {
			count++
		}
	}
	return count, nil
}

func (f *fakeAutomationStore) PruneRuns(_ context.Context, _ time.Time, _ int) (int, error) {
	return 0, nil
}

func (f *fakeAutomationStore) Pause(_ context.Context, pause domain.PausedContainer) (domain.PausedContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.pauses {
		if existing.ContainerName == pause.ContainerName && existing.AcknowledgedAt == nil {
			return domain.PausedContainer{}, store.ErrPauseActive
		}
	}
	pause.ID = int64(len(f.pauses) + 1)
	f.pauses = append(f.pauses, pause)
	return pause, nil
}

func (f *fakeAutomationStore) Resume(
	_ context.Context,
	containerName string,
	by domain.Requester,
	at time.Time,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.pauses {
		if f.pauses[i].ContainerName == containerName && f.pauses[i].AcknowledgedAt == nil {
			cleared := at
			f.pauses[i].AcknowledgedAt = &cleared
			f.pauses[i].AcknowledgedBy = by
			delete(f.failures, containerName)
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeAutomationStore) ActivePauses(_ context.Context) ([]domain.PausedContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.PausedContainer, 0, len(f.pauses))
	for _, pause := range f.pauses {
		if pause.AcknowledgedAt == nil {
			out = append(out, pause)
		}
	}
	return out, nil
}

func (f *fakeAutomationStore) PauseFor(_ context.Context, containerName string) (domain.PausedContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pause := range f.pauses {
		if pause.ContainerName == containerName && pause.AcknowledgedAt == nil {
			return pause, nil
		}
	}
	return domain.PausedContainer{}, store.ErrNotFound
}

func (f *fakeAutomationStore) ListPauses(
	_ context.Context,
	activeOnly bool,
	_ store.Page,
) ([]domain.PausedContainer, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.PausedContainer, 0, len(f.pauses))
	for _, pause := range f.pauses {
		if activeOnly && pause.AcknowledgedAt != nil {
			continue
		}
		out = append(out, pause)
	}
	return out, len(out), nil
}

func (f *fakeAutomationStore) CountActivePauses(_ context.Context) (int, error) {
	pauses, _ := f.ActivePauses(context.Background())
	return len(pauses), nil
}

func (f *fakeAutomationStore) RecordFailure(
	_ context.Context,
	containerName, detail string,
	_ int,
	at time.Time,
) (store.AutomationFailureCount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := f.failures[containerName]
	count.ContainerName = containerName
	count.Consecutive++
	count.Windowed++
	moment := at
	count.LastFailureAt = &moment
	count.LastDetail = detail
	f.failures[containerName] = count
	return count, nil
}

func (f *fakeAutomationStore) RecordSuccess(_ context.Context, containerName string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := f.failures[containerName]
	count.ContainerName = containerName
	count.Consecutive = 0
	moment := at
	count.LastSuccessAt = &moment
	f.failures[containerName] = count
	return nil
}

func (f *fakeAutomationStore) FailureCount(_ context.Context, containerName string) (store.AutomationFailureCount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures[containerName], nil
}

// autoPolicyStore serves a fixed policy set.
type autoPolicyStore struct {
	policies []domain.UpdatePolicy
}

func (f *autoPolicyStore) ActivePolicies(context.Context) ([]domain.UpdatePolicy, error) {
	return f.policies, nil
}

func (f *autoPolicyStore) CountUpdatePolicies(context.Context) (int, int, error) {
	return len(f.policies), len(f.policies), nil
}

// autoEvidence serves a fixed world.
type autoEvidence struct {
	targets  []store.AutomationTarget
	plans    map[string]domain.ChangePlan
	inFlight map[string]bool
	total    int
}

func (f *autoEvidence) Targets(context.Context) ([]store.AutomationTarget, bool, error) {
	return f.targets, false, nil
}

func (f *autoEvidence) CurrentPlan(_ context.Context, containerID string) (domain.ChangePlan, error) {
	plan, ok := f.plans[containerID]
	if !ok {
		return domain.ChangePlan{}, store.ErrNotFound
	}
	return plan, nil
}

func (f *autoEvidence) AcquisitionActive(_ context.Context, containerID string) (bool, error) {
	return f.inFlight[containerID], nil
}

func (f *autoEvidence) ExecutionActive(context.Context, string) (bool, error) { return false, nil }

func (f *autoEvidence) InFlightTotal(context.Context) (int, error) { return f.total, nil }

// autoRecordedRequest is one thing the engine asked the pipeline for.
type autoRecordedRequest struct {
	kind string
	id   string
	by   domain.Requester
}

// autoPipeline records every request and returns scripted answers.
type autoPipeline struct {
	mu       sync.Mutex
	requests []autoRecordedRequest

	rollbackOn bool

	acquisitions map[string]domain.Acquisition
	executions   map[string]domain.Execution

	acquireErr  error
	executeErr  error
	rollbackErr error

	nextAcquisition int
	nextExecution   int
	nextRollback    int
}

func newAutoPipeline() *autoPipeline {
	return &autoPipeline{
		rollbackOn:   true,
		acquisitions: make(map[string]domain.Acquisition),
		executions:   make(map[string]domain.Execution),
	}
}

func (f *autoPipeline) AcquisitionEnabled() bool { return true }
func (f *autoPipeline) ExecutionEnabled() bool   { return true }
func (f *autoPipeline) RollbackEnabled() bool    { return f.rollbackOn }

func (f *autoPipeline) RequestAcquisition(
	_ context.Context,
	request service.AcquisitionRequest,
) (domain.Acquisition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, autoRecordedRequest{"acquire", request.PlanID, request.RequestedBy})
	if f.acquireErr != nil {
		return domain.Acquisition{}, f.acquireErr
	}
	f.nextAcquisition++
	id := "acq_" + padAutoID(f.nextAcquisition)
	acquisition := domain.Acquisition{
		AcquisitionID: id,
		PlanID:        request.PlanID,
		State:         domain.AcquisitionSucceeded,
	}
	f.acquisitions[id] = acquisition
	return acquisition, nil
}

func (f *autoPipeline) RequestExecution(
	_ context.Context,
	request service.ExecutionRequest,
) (domain.Execution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, autoRecordedRequest{"execute", request.AcquisitionID, request.RequestedBy})
	if f.executeErr != nil {
		return domain.Execution{}, f.executeErr
	}
	f.nextExecution++
	id := "exec_" + padAutoID(f.nextExecution)
	execution := domain.Execution{
		ExecutionID:   id,
		AcquisitionID: request.AcquisitionID,
		State:         domain.ExecutionSucceeded,
	}
	f.executions[id] = execution
	return execution, nil
}

func (f *autoPipeline) RequestRollback(
	_ context.Context,
	request service.RollbackRequest,
) (domain.Rollback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, autoRecordedRequest{"rollback", request.ExecutionID, request.RequestedBy})
	if f.rollbackErr != nil {
		return domain.Rollback{}, f.rollbackErr
	}
	f.nextRollback++
	return domain.Rollback{RollbackID: "rbk_" + padAutoID(f.nextRollback)}, nil
}

func (f *autoPipeline) Acquisition(_ context.Context, id string) (domain.Acquisition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	acquisition, ok := f.acquisitions[id]
	if !ok {
		return domain.Acquisition{}, store.ErrNotFound
	}
	return acquisition, nil
}

func (f *autoPipeline) Execution(_ context.Context, id string) (domain.Execution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	execution, ok := f.executions[id]
	if !ok {
		return domain.Execution{}, store.ErrNotFound
	}
	return execution, nil
}

func (f *autoPipeline) recorded(kind string) []autoRecordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]autoRecordedRequest, 0, len(f.requests))
	for _, request := range f.requests {
		if request.kind == kind {
			out = append(out, request)
		}
	}
	return out
}

// setExecution overwrites a recorded execution, so a test can say what the
// recreation did.
func (f *autoPipeline) setExecution(id string, execution domain.Execution) {
	f.mu.Lock()
	defer f.mu.Unlock()
	execution.ExecutionID = id
	f.executions[id] = execution
}

func padAutoID(n int) string {
	text := strconv.Itoa(n)
	for len(text) < 20 {
		text = "0" + text
	}
	return text
}

// ------------------------------------------------------------- harness --

type automationHarness struct {
	engine   *service.AutomationService
	store    *fakeAutomationStore
	policies *autoPolicyStore
	evidence *autoEvidence
	pipeline *autoPipeline
	now      time.Time
}

func newAutomationHarness(t *testing.T, policies ...domain.UpdatePolicy) *automationHarness {
	t.Helper()

	if len(policies) == 0 {
		policies = []domain.UpdatePolicy{automaticPolicy()}
	}

	harness := &automationHarness{
		store:    newFakeAutomationStore(),
		policies: &autoPolicyStore{policies: policies},
		evidence: &autoEvidence{
			targets: []store.AutomationTarget{{
				ContainerID: "container-web",
				Selection:   domain.SelectionTarget{Name: "web", Image: "nginx:1.27.3"},
			}},
			plans:    map[string]domain.ChangePlan{"container-web": patchPlan()},
			inFlight: map[string]bool{},
		},
		pipeline: newAutoPipeline(),
		now:      decideAt,
	}

	harness.engine = service.NewAutomationService(service.AutomationOptions{
		Store:    harness.store,
		Policies: harness.policies,
		Evidence: harness.evidence,
		Pipeline: harness.pipeline,
		Config: config.Automation{
			Enabled:       true,
			Interval:      15 * time.Minute,
			PassTimeout:   time.Minute,
			MaxConcurrent: 4,
			MaxPerRun:     10,
		},
		Now: func() time.Time { return harness.now },
	})
	return harness
}

// ------------------------------------------------------------- the pass --

func TestAPassSubmitsOnlyThePlanIdentifier(t *testing.T) {
	// The whole safety argument, stated as an assertion: automation asks for a
	// PLAN, not for an image.
	harness := newAutomationHarness(t)

	run, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if run.Submitted != 1 {
		t.Fatalf("submitted = %d, want 1 (decisions: %+v)", run.Submitted, decisions)
	}

	requests := harness.pipeline.recorded("acquire")
	if len(requests) != 1 {
		t.Fatalf("want one acquisition request, got %d", len(requests))
	}
	if requests[0].id != patchPlan().PlanID {
		t.Fatalf("the request named %q, want the plan id", requests[0].id)
	}
}

func TestAPassRecordsEveryContainerItConsidered(t *testing.T) {
	// Including the ones it declined. "Why did you not update that container"
	// is unanswerable unless the reasoning was recorded at the time.
	harness := newAutomationHarness(t)
	harness.evidence.targets = append(harness.evidence.targets,
		store.AutomationTarget{
			ContainerID: "container-cache",
			Selection:   domain.SelectionTarget{Name: "cache", Image: "redis:7"},
		})

	run, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if run.Considered != 2 {
		t.Fatalf("considered = %d, want 2", run.Considered)
	}
	if len(decisions) != 2 {
		t.Fatalf("want a decision per container, got %d", len(decisions))
	}

	var cache domain.AutomationDecision
	for _, decision := range decisions {
		if decision.ContainerName == "cache" {
			cache = decision
		}
	}
	if cache.Reason != domain.ReasonNotSelected {
		t.Fatalf("the unselected container's reason = %q, want notSelected", cache.Reason)
	}
}

func TestADryRunPassSubmitsNothing(t *testing.T) {
	// Whatever the policy said. The pass-level flag is the last word: an
	// operator who asked for a preview gets a preview.
	harness := newAutomationHarness(t)

	run, decisions, err := harness.engine.RunNow(context.Background(), true, domain.Requester{})
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if run.Submitted != 0 {
		t.Fatalf("a dry run submitted %d updates", run.Submitted)
	}
	if len(harness.pipeline.recorded("acquire")) != 0 {
		t.Fatal("a dry run must not reach the acquisition service at all")
	}
	if run.Eligible != 1 {
		t.Fatalf("eligible = %d, want 1 -- a dry run still reports what it would do", run.Eligible)
	}
	if decisions[0].Verdict != domain.VerdictWouldUpdate {
		t.Fatalf("verdict = %q, want wouldUpdate", decisions[0].Verdict)
	}
}

func TestAnObservePolicyNeverReachesTheAcquisitionService(t *testing.T) {
	policy := automaticPolicy()
	policy.Mode = domain.ModeObserve
	harness := newAutomationHarness(t, policy)

	if _, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{}); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if len(harness.pipeline.recorded("acquire")) != 0 {
		t.Fatal("observe mode is read-only by construction, not by remembering to skip a call")
	}
}

func TestAManualPassCarriesTheOperatorIntoTheAcquisition(t *testing.T) {
	// A host change automation made on somebody's instruction is attributed to
	// them, not to "the scheduler".
	harness := newAutomationHarness(t)
	operator := domain.Requester{UserID: "usr_1", Username: "colby"}

	if _, _, err := harness.engine.RunNow(context.Background(), false, operator); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	requests := harness.pipeline.recorded("acquire")
	if len(requests) != 1 || requests[0].by.Username != "colby" {
		t.Fatalf("the acquisition was attributed to %+v", requests[0].by)
	}
}

func TestASecondPassIsRefusedWhileOneIsRunning(t *testing.T) {
	harness := newAutomationHarness(t)
	harness.store.startErr = store.ErrAutomationRunActive

	_, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if !errors.Is(err, service.ErrAutomationBusy) {
		t.Fatalf("want ErrAutomationBusy, got %v", err)
	}
}

func TestAPausedContainerIsNotSubmitted(t *testing.T) {
	harness := newAutomationHarness(t)
	if _, err := harness.store.Pause(context.Background(), domain.PausedContainer{
		ContainerName: "web",
		Reason:        domain.PauseRolledBack,
		PausedAt:      decideAt.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	run, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if run.Submitted != 0 {
		t.Fatal("a paused container must not be submitted")
	}
	if decisions[0].Reason != domain.ReasonPaused {
		t.Fatalf("reason = %q, want automationPaused", decisions[0].Reason)
	}
	if len(harness.pipeline.recorded("acquire")) != 0 {
		t.Fatal("a paused container must not reach the acquisition service")
	}
}

func TestAPreflightRefusalIsRecordedNotRaised(t *testing.T) {
	harness := newAutomationHarness(t)
	harness.pipeline.acquireErr = service.ErrAcquisitionRefused{
		Refusal: domain.AcquisitionRefusalDisabled,
	}

	run, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("a refusal is the preflight working, not a pass failure: %v", err)
	}
	if run.Failed != 1 {
		t.Fatalf("failed = %d, want 1", run.Failed)
	}
	if decisions[0].Reason != domain.ReasonRefused {
		t.Fatalf("reason = %q, want refusedByService", decisions[0].Reason)
	}
	if run.State != domain.RunCompleted {
		t.Fatalf("state = %q; a pass whose submission was refused still completed", run.State)
	}
}

func TestTheEngineCeilingBoundsOnePass(t *testing.T) {
	policy := automaticPolicy()
	policy.Limits.MaxConcurrent = 8
	policy.Limits.MaxPerRegistry = 8
	policy.Limits.MaxPerRun = 8
	harness := newAutomationHarness(t, policy)

	// Ten eligible containers against an engine ceiling of two.
	harness.evidence.targets = nil
	harness.evidence.plans = make(map[string]domain.ChangePlan)
	for i := 0; i < 10; i++ {
		name := "web" + strconv.Itoa(i)
		id := "container-" + name
		harness.evidence.targets = append(harness.evidence.targets, store.AutomationTarget{
			ContainerID: id,
			Selection:   domain.SelectionTarget{Name: name, Image: "nginx:1.27.3"},
		})
		plan := patchPlan()
		plan.PlanID = "plan_" + padAutoID(i)
		plan.ContainerID = id
		plan.ContainerName = name
		harness.evidence.plans[id] = plan
	}
	// Every container is selected by name.
	names := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		names = append(names, "web"+strconv.Itoa(i))
	}
	policy.Selector = domain.UpdateSelector{Include: names}
	harness.policies.policies = []domain.UpdatePolicy{policy}

	harness.engine = service.NewAutomationService(service.AutomationOptions{
		Store:    harness.store,
		Policies: harness.policies,
		Evidence: harness.evidence,
		Pipeline: harness.pipeline,
		Config: config.Automation{
			Enabled: true, Interval: 15 * time.Minute, PassTimeout: time.Minute,
			MaxConcurrent: 2, MaxPerRun: 2,
		},
		Now: func() time.Time { return harness.now },
	})

	run, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if run.Submitted != 2 {
		t.Fatalf("submitted = %d, want 2 -- the engine ceiling bounds one pass", run.Submitted)
	}
	if run.Eligible != 10 {
		t.Fatalf("eligible = %d, want 10 -- eligibility and admission are separate", run.Eligible)
	}
	if len(harness.pipeline.recorded("acquire")) != 2 {
		t.Fatalf("the ceiling must bound what actually reaches the service, got %d",
			len(harness.pipeline.recorded("acquire")))
	}
}

// ----------------------------------------------------------- the follower --

func TestTheFollowerRequestsTheRecreationOnceThePullSucceeded(t *testing.T) {
	harness := newAutomationHarness(t)

	if _, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{}); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	service.FollowForTest(harness.engine, context.Background())

	requests := harness.pipeline.recorded("execute")
	if len(requests) != 1 {
		t.Fatalf("want one recreation request, got %d", len(requests))
	}
	if requests[0].id != "acq_"+padAutoID(1) {
		t.Fatalf("the recreation named %q, want the acquisition the pass started", requests[0].id)
	}
}

func TestTheFollowerDoesNotRecreateAFailedPull(t *testing.T) {
	harness := newAutomationHarness(t)

	if _, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{}); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	// The image never arrived.
	id := "acq_" + padAutoID(1)
	harness.pipeline.mu.Lock()
	acquisition := harness.pipeline.acquisitions[id]
	acquisition.State = domain.AcquisitionFailed
	harness.pipeline.acquisitions[id] = acquisition
	harness.pipeline.mu.Unlock()

	service.FollowForTest(harness.engine, context.Background())

	if len(harness.pipeline.recorded("execute")) != 0 {
		t.Fatal("a recreation must never be requested for an image that did not arrive")
	}
	count, _ := harness.store.FailureCount(context.Background(), "web")
	if count.Consecutive != 1 {
		t.Fatalf("the failure must be counted, got %+v", count)
	}
	// Nothing on the host changed, so there is nothing to roll back.
	if len(harness.pipeline.recorded("rollback")) != 0 {
		t.Fatal("a failed pull leaves the host unchanged; there is nothing to undo")
	}
}

func TestASuccessfulUpdateClearsTheConsecutiveCount(t *testing.T) {
	harness := newAutomationHarness(t)

	if _, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{}); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	service.FollowForTest(harness.engine, context.Background()) // requests the recreation
	service.FollowForTest(harness.engine, context.Background()) // reads its outcome

	count, _ := harness.store.FailureCount(context.Background(), "web")
	if count.LastSuccessAt == nil {
		t.Fatal("a successful update must be recorded")
	}
	pauses, _ := harness.store.ActivePauses(context.Background())
	if len(pauses) != 0 {
		t.Fatal("a successful update must not pause anything")
	}
}

// ------------------------------------------------- automatic rollback --

// failedAutoExecution is a recreation that got far enough to be undoable.
func failedAutoExecution(id string) domain.Execution {
	return domain.Execution{
		ExecutionID:     id,
		State:           domain.ExecutionFailed,
		Failure:         domain.ExecutionFailureUnhealthy,
		Checkpoint:      domain.CheckpointReplacementStarted,
		OriginalRemoved: false,
	}
}

func TestAFinishedUpdateStopsBeingFollowed(t *testing.T) {
	// The defect this pins: a SUCCESSFUL update never gets a rollback id, so a
	// follower whose "outstanding" test was "has no rollback" re-read the same
	// finished execution on every tick, forever. Nothing was done twice to the
	// host, but the log filled and the backlog never drained.
	harness := newAutomationHarness(t)

	if _, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{}); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	service.FollowForTest(harness.engine, context.Background()) // requests the recreation
	service.FollowForTest(harness.engine, context.Background()) // reads its success

	before := len(harness.pipeline.recorded("execute"))

	// Several more ticks must find nothing to do.
	for i := 0; i < 5; i++ {
		service.FollowForTest(harness.engine, context.Background())
	}

	if got := len(harness.pipeline.recorded("execute")); got != before {
		t.Fatalf("the follower kept working a finished decision: %d requests, want %d", got, before)
	}
	pending, err := harness.store.PendingDecisions(context.Background(), 100)
	if err != nil {
		t.Fatalf("PendingDecisions: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("a finished update must leave the outstanding set, got %+v", pending)
	}
}

func TestAFailedPullStopsBeingFollowed(t *testing.T) {
	harness := newAutomationHarness(t)

	if _, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{}); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	id := "acq_" + padAutoID(1)
	harness.pipeline.mu.Lock()
	acquisition := harness.pipeline.acquisitions[id]
	acquisition.State = domain.AcquisitionFailed
	harness.pipeline.acquisitions[id] = acquisition
	harness.pipeline.mu.Unlock()

	service.FollowForTest(harness.engine, context.Background())
	// A second tick must not count the same failure again.
	service.FollowForTest(harness.engine, context.Background())

	count, _ := harness.store.FailureCount(context.Background(), "web")
	if count.Consecutive != 1 {
		t.Fatalf("the failure was counted %d times, want once", count.Consecutive)
	}
}

func TestAFailedUpdateIsRolledBackAndAlwaysPauses(t *testing.T) {
	harness := newAutomationHarness(t)

	if _, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{}); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	service.FollowForTest(harness.engine, context.Background())

	executionID := "exec_" + padAutoID(1)
	harness.pipeline.setExecution(executionID, failedAutoExecution(executionID))

	service.FollowForTest(harness.engine, context.Background())

	rollbacks := harness.pipeline.recorded("rollback")
	if len(rollbacks) != 1 {
		t.Fatalf("want one rollback request, got %d", len(rollbacks))
	}
	if rollbacks[0].id != executionID {
		t.Fatalf("the rollback named %q, want the execution", rollbacks[0].id)
	}

	// A rollback ALWAYS pauses, whatever the failure counters say: the change
	// was wrong and the host was moved twice.
	pauses, _ := harness.store.ActivePauses(context.Background())
	if len(pauses) != 1 {
		t.Fatalf("a rolled-back container must be paused, got %d pauses", len(pauses))
	}
	if pauses[0].Reason != domain.PauseRolledBack {
		t.Fatalf("pause reason = %q, want automaticRollback", pauses[0].Reason)
	}
	if pauses[0].ResumeAfter != nil {
		t.Fatal("a rollback pause must never expire on its own; a person has to look")
	}
	// The pause must name the rollback that caused it. Without it the record
	// says "rolled back" and points at nothing.
	if pauses[0].RollbackID == "" {
		t.Fatal("the pause must name the rollback that produced it")
	}
}

func TestAPolicyThatForbidsRollbackIsHonoured(t *testing.T) {
	policy := automaticPolicy()
	policy.Failure.AutoRollback = false
	harness := newAutomationHarness(t, policy)

	if _, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{}); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	service.FollowForTest(harness.engine, context.Background())

	executionID := "exec_" + padAutoID(1)
	harness.pipeline.setExecution(executionID, failedAutoExecution(executionID))
	service.FollowForTest(harness.engine, context.Background())

	if len(harness.pipeline.recorded("rollback")) != 0 {
		t.Fatal("the policy switched automatic rollback off")
	}
}

func TestNoRollbackWhenTheCapabilityIsAbsent(t *testing.T) {
	harness := newAutomationHarness(t)
	harness.pipeline.rollbackOn = false

	if _, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{}); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	service.FollowForTest(harness.engine, context.Background())

	executionID := "exec_" + padAutoID(1)
	harness.pipeline.setExecution(executionID, failedAutoExecution(executionID))
	service.FollowForTest(harness.engine, context.Background())

	if len(harness.pipeline.recorded("rollback")) != 0 {
		t.Fatal("a deployment without the rollback capability cannot roll anything back")
	}
	// It still counts the failure, which is what eventually pauses it.
	count, _ := harness.store.FailureCount(context.Background(), "web")
	if count.Consecutive != 1 {
		t.Fatalf("the failure must still be counted, got %+v", count)
	}
}

func TestNoRollbackWhenTheOriginalIsAlreadyGone(t *testing.T) {
	// There is nothing to put back, and the rollback service would refuse.
	harness := newAutomationHarness(t)

	if _, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{}); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	service.FollowForTest(harness.engine, context.Background())

	executionID := "exec_" + padAutoID(1)
	execution := failedAutoExecution(executionID)
	execution.OriginalRemoved = true
	harness.pipeline.setExecution(executionID, execution)
	service.FollowForTest(harness.engine, context.Background())

	if len(harness.pipeline.recorded("rollback")) != 0 {
		t.Fatal("a removed original cannot be restored by a rollback")
	}
}

func TestRepeatedFailurePausesAtTheThreshold(t *testing.T) {
	policy := automaticPolicy()
	policy.Failure.AutoRollback = false
	policy.Failure.PauseAfterFailures = 2
	harness := newAutomationHarness(t, policy)

	for attempt := 1; attempt <= 2; attempt++ {
		// Finish the previous pass so a new one may start.
		harness.store.mu.Lock()
		for i := range harness.store.runs {
			harness.store.runs[i].State = domain.RunCompleted
		}
		harness.store.mu.Unlock()

		if _, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{}); err != nil {
			t.Fatalf("attempt %d RunNow: %v", attempt, err)
		}
		service.FollowForTest(harness.engine, context.Background())

		executionID := "exec_" + padAutoID(attempt)
		harness.pipeline.setExecution(executionID, failedAutoExecution(executionID))
		service.FollowForTest(harness.engine, context.Background())

		pauses, _ := harness.store.ActivePauses(context.Background())
		switch attempt {
		case 1:
			if len(pauses) != 0 {
				t.Fatal("one failure is below the threshold of two")
			}
		case 2:
			if len(pauses) != 1 {
				t.Fatalf("the second failure reaches the threshold, got %d pauses", len(pauses))
			}
			if pauses[0].Reason != domain.PauseRepeatedFailure {
				t.Fatalf("pause reason = %q, want repeatedFailure", pauses[0].Reason)
			}
		}
	}
}

func TestPauseThresholdOfZeroNeverPauses(t *testing.T) {
	policy := automaticPolicy()
	policy.Failure.AutoRollback = false
	policy.Failure.PauseAfterFailures = 0
	harness := newAutomationHarness(t, policy)

	if _, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{}); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	service.FollowForTest(harness.engine, context.Background())
	executionID := "exec_" + padAutoID(1)
	harness.pipeline.setExecution(executionID, failedAutoExecution(executionID))
	service.FollowForTest(harness.engine, context.Background())

	pauses, _ := harness.store.ActivePauses(context.Background())
	if len(pauses) != 0 {
		t.Fatal("a threshold of zero disables pausing, which the policy warnings say is unwise")
	}
}

// ------------------------------------------------------------ approval --

func TestApprovingReleasesTheHeldDecision(t *testing.T) {
	policy := automaticPolicy()
	policy.Mode = domain.ModeApprove
	harness := newAutomationHarness(t, policy)

	run, decisions, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if decisions[0].Verdict != domain.VerdictAwaitingApproval {
		t.Fatalf("verdict = %q, want awaitingApproval", decisions[0].Verdict)
	}
	if len(harness.pipeline.recorded("acquire")) != 0 {
		t.Fatal("a held decision must not have acted")
	}

	approver := domain.Requester{UserID: "usr_1", Username: "colby"}
	released, err := harness.engine.Approve(context.Background(), run.RunID, "web",
		approver, service.Actor{})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if released.AcquisitionID == "" {
		t.Fatal("an approved decision must name the acquisition it started")
	}

	requests := harness.pipeline.recorded("acquire")
	if len(requests) != 1 {
		t.Fatalf("want one acquisition, got %d", len(requests))
	}
	if requests[0].by.Username != "colby" {
		t.Fatalf("a change a person released is that person's change, got %+v", requests[0].by)
	}
}

func TestAnApprovedDecisionIsFollowedThroughToTheRecreation(t *testing.T) {
	// The defect this pins: an approval promotes a decision AFTER its pass has
	// finished, so that run's `submitted` counter is zero and stays zero. A
	// follower that filtered runs on "this pass submitted something" left every
	// approved update with an acquired image and no recreation, and the
	// pipeline stopped silently.
	policy := automaticPolicy()
	policy.Mode = domain.ModeApprove
	harness := newAutomationHarness(t, policy)

	run, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if run.Submitted != 0 {
		t.Fatalf("submitted = %d; an approval-mode pass submits nothing itself", run.Submitted)
	}

	if _, err := harness.engine.Approve(context.Background(), run.RunID, "web",
		domain.Requester{UserID: "usr_1", Username: "colby"}, service.Actor{}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	service.FollowForTest(harness.engine, context.Background())

	requests := harness.pipeline.recorded("execute")
	if len(requests) != 1 {
		t.Fatalf("want the approved update to reach the recreation service, got %d requests",
			len(requests))
	}
}

func TestApprovingAStaleDecisionIsRefused(t *testing.T) {
	// Approving a proposal is approving THAT proposal. A registry that
	// republished a tag in the meantime has made it a different one.
	policy := automaticPolicy()
	policy.Mode = domain.ModeApprove
	harness := newAutomationHarness(t, policy)

	run, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}

	newer := patchPlan()
	newer.PlanID = "plan_ffffffffffffffffffff"
	newer.ProposedImage = "nginx:1.27.5"
	harness.evidence.plans["container-web"] = newer

	_, err = harness.engine.Approve(context.Background(), run.RunID, "web",
		domain.Requester{UserID: "usr_1", Username: "colby"}, service.Actor{})
	if !errors.Is(err, service.ErrDecisionNotApprovable) {
		t.Fatalf("want ErrDecisionNotApprovable, got %v", err)
	}
	if len(harness.pipeline.recorded("acquire")) != 0 {
		t.Fatal("a stale approval must reach no service at all")
	}
}

func TestApprovingRequiresAnAccount(t *testing.T) {
	policy := automaticPolicy()
	policy.Mode = domain.ModeApprove
	harness := newAutomationHarness(t, policy)

	run, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if _, err := harness.engine.Approve(context.Background(), run.RunID, "web",
		domain.Requester{}, service.Actor{}); err == nil {
		t.Fatal("an approval whose approver is unknown is not an approval")
	}
}

func TestApprovingAContainerWithNoHeldDecisionApprovesNothing(t *testing.T) {
	// The container name SELECTS a held decision; it does not describe a
	// target. A name that matches nothing must reach no service.
	harness := newAutomationHarness(t)

	run, _, err := harness.engine.RunNow(context.Background(), false, domain.Requester{})
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	before := len(harness.pipeline.recorded("acquire"))

	_, err = harness.engine.Approve(context.Background(), run.RunID, "some-other-container",
		domain.Requester{UserID: "usr_1", Username: "colby"}, service.Actor{})
	if !errors.Is(err, service.ErrDecisionNotApprovable) {
		t.Fatalf("want ErrDecisionNotApprovable, got %v", err)
	}
	if len(harness.pipeline.recorded("acquire")) != before {
		t.Fatal("approving a name with no held decision must submit nothing")
	}
}

// ------------------------------------------------------------- upcoming --

func TestUpcomingWritesNothing(t *testing.T) {
	harness := newAutomationHarness(t)

	decisions, err := harness.engine.Upcoming(context.Background())
	if err != nil {
		t.Fatalf("Upcoming: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Verdict != domain.VerdictUpdate {
		t.Fatalf("the preview must report what the pass would do, got %+v", decisions)
	}

	harness.store.mu.Lock()
	runs, recorded := len(harness.store.runs), len(harness.store.decisions)
	harness.store.mu.Unlock()

	if runs != 0 || recorded != 0 {
		t.Fatalf("a preview wrote %d runs and %d decisions; it must write neither", runs, recorded)
	}
	if len(harness.pipeline.recorded("acquire")) != 0 {
		t.Fatal("a preview must reach no service")
	}
}

// --------------------------------------------------------------- status --

func TestStatusReportsTheNextWindow(t *testing.T) {
	policy := automaticPolicy()
	policy.Window = domain.MaintenanceWindow{Start: "02:00", End: "04:00"}
	harness := newAutomationHarness(t, policy)
	// Midday: the window is closed and opens again at 02:00 tomorrow.
	harness.now = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	status, err := harness.engine.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.WindowOpen {
		t.Fatal("the window is closed at midday")
	}
	if status.NextWindowOpensAt == nil {
		t.Fatal("a dashboard must be able to say when automation will next act")
	}
	want := time.Date(2026, 3, 2, 2, 0, 0, 0, time.UTC)
	if !status.NextWindowOpensAt.Equal(want) {
		t.Fatalf("next window = %s, want %s", status.NextWindowOpensAt, want)
	}
}

func TestStatusSurvivesAnUnresolvableTimezone(t *testing.T) {
	// The policy's own decisions fail closed, which is where the safety
	// property lives. The dashboard must not 500 because of it.
	policy := automaticPolicy()
	policy.Window = domain.MaintenanceWindow{Timezone: "Mars/Olympus_Mons", Start: "02:00", End: "04:00"}
	harness := newAutomationHarness(t, policy)

	status, err := harness.engine.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.WindowOpen {
		t.Fatal("an unresolvable window is never open")
	}
}
