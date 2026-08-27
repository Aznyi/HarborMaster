package service_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Container recreation tests.
//
// This is HarborMaster's largest privilege, so the tests are written around
// what must NOT happen:
//
//   - No container is stopped without every prerequisite holding. Each refusal
//     has a test, because each names a way the world can move between approval
//     and application.
//   - The original is never removed until all four proofs pass. Every failure
//     path asserts it is still there.
//   - A checkpoint that does not land stops the pipeline. It never causes a
//     mutation to be repeated.
//   - Cancellation is impossible once the host has been changed.
//   - A restart settles interrupted work from its CHECKPOINT and issues no
//     Docker call at all.

const (
	execAcquisitionID = "acq_0011223344556677889a"
	execPlanID        = "plan_00112233445566778899"
	execDigest        = "sha256:" + "3333333333333333333333333333333333333333333333333333333333333333"
	execImageID       = "sha256:" + "4444444444444444444444444444444444444444444444444444444444444444"
)

var execContainerID = docker.FakeContainerID(1)

// ------------------------------------------------------------ evidence --

// fakeExecutionEvidence is the read-only data the preflight re-checks.
type fakeExecutionEvidence struct {
	mu sync.Mutex

	acquisition domain.Acquisition
	plan        domain.ChangePlan
	current     domain.ChangePlan
	container   *domain.ContainerDetail
	baseline    domain.Snapshot
	policy      domain.PolicyEvaluation
	refresh     *domain.RefreshRecord
	intel       domain.ImageIntel

	acquisitionErr error
	planErr        error
	currentErr     error
	containerErr   error
	baselineErr    error
	policyErr      error
	refreshErr     error
	intelErr       error

	// planCalls counts revalidations, which is what proves the preflight runs
	// again immediately before the first mutation.
	planCalls int
}

func (f *fakeExecutionEvidence) Acquisition(context.Context, string) (domain.Acquisition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acquisitionErr != nil {
		return domain.Acquisition{}, f.acquisitionErr
	}
	return f.acquisition, nil
}

func (f *fakeExecutionEvidence) Plan(context.Context, string) (domain.ChangePlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.planCalls++
	if f.planErr != nil {
		return domain.ChangePlan{}, f.planErr
	}
	return f.plan, nil
}

func (f *fakeExecutionEvidence) CurrentPlan(context.Context, string) (domain.ChangePlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.currentErr != nil {
		return domain.ChangePlan{}, f.currentErr
	}
	return f.current, nil
}

func (f *fakeExecutionEvidence) Container(context.Context, string) (*domain.ContainerDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.containerErr != nil {
		return nil, f.containerErr
	}
	return f.container, nil
}

func (f *fakeExecutionEvidence) Baseline(context.Context, string) (domain.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.baselineErr != nil {
		return domain.Snapshot{}, f.baselineErr
	}
	return f.baseline, nil
}

func (f *fakeExecutionEvidence) PolicyEvaluation(context.Context, string) (domain.PolicyEvaluation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.policyErr != nil {
		return domain.PolicyEvaluation{}, f.policyErr
	}
	return f.policy, nil
}

func (f *fakeExecutionEvidence) LastRefresh(context.Context) (*domain.RefreshRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	return f.refresh, nil
}

func (f *fakeExecutionEvidence) Intel(context.Context, string) (domain.ImageIntel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.intelErr != nil {
		return domain.ImageIntel{}, f.intelErr
	}
	return f.intel, nil
}

func (f *fakeExecutionEvidence) revalidations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.planCalls
}

// execContainerDetail is the container under test: running, healthy, with one
// sensitive variable so the preservation comparison has something to digest.
func execContainerDetail(id string) domain.ContainerDetail {
	return domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			ID:            id,
			ShortID:       domain.ShortenID(id),
			Name:          "web",
			Image:         domain.ParseImageRef("nginx:1.27.0"),
			ImageID:       "sha256:" + strings.Repeat("9", 64),
			State:         domain.StateRunning,
			Health:        domain.HealthHealthy,
			Present:       true,
			RestartPolicy: domain.RestartPolicy{Name: "unless-stopped"},
		},
		State: domain.StateDetail{
			State: domain.StateRunning, Running: true, Health: domain.HealthHealthy,
		},
		HealthCheck: &domain.HealthCheck{Test: []string{"CMD", "curl", "-f", "http://localhost/"}},
		Process:     domain.Process{Command: []string{"nginx"}, User: "nginx"},
		Environment: []domain.EnvVar{
			{Name: "PORT", Value: "8080", Sensitivity: domain.SensitivityNormal, RawValue: "8080"},
			{
				Name: "DB_PASSWORD", Value: domain.MaskedValue,
				Sensitivity: domain.SensitivitySensitive, RawValue: "hunter2",
			},
		},
		Networks: []domain.NetworkAttachment{{NetworkName: "bridge", Aliases: []string{"web"}}},
		Security: domain.Security{ReadonlyRootfs: true, CapDrop: []string{"ALL"}},
	}
}

// healthyExecutionEvidence is a world in which a recreation should be allowed.
func healthyExecutionEvidence(now time.Time) *fakeExecutionEvidence {
	completed := now.Add(-time.Hour)
	fingerprint := strings.Repeat("f", 64)

	plan := domain.ChangePlan{
		PlanID:            execPlanID,
		ContainerID:       execContainerID,
		ContainerName:     "web",
		CurrentImage:      "nginx:1.27.0",
		ProposedImage:     "nginx:1.27.1",
		ProposedDigest:    execDigest,
		UpdateType:        domain.UpdatePatch,
		SnapshotID:        7,
		SnapshotAvailable: true,
		RestoreReadiness:  domain.ReadinessReady,
		RegistryStatus:    domain.CheckOK,
		InputDigest:       fingerprint,
		Risk: domain.RiskAssessment{
			Score: 5, Band: domain.RiskVeryLow, Recommendation: domain.RecommendProceed,
		},
	}

	detail := execContainerDetail(execContainerID)
	refresh := domain.RefreshRecord{StartedAt: now.Add(-time.Minute)}

	return &fakeExecutionEvidence{
		acquisition: domain.Acquisition{
			AcquisitionID: execAcquisitionID,
			PlanID:        execPlanID,
			ContainerID:   execContainerID,
			ContainerName: "web",
			Target: domain.AcquisitionTarget{
				Registry: "docker.io", Repository: "library/nginx",
				Digest: execDigest, Reference: "nginx:1.27.1",
				Platform: domain.Platform{OS: "linux", Architecture: "amd64"},
			},
			State:           domain.AcquisitionSucceeded,
			AcquiredDigest:  execDigest,
			AcquiredImageID: execImageID,
			CompletedAt:     &completed,
			PlanDigest:      fingerprint,
		},
		plan:      plan,
		current:   plan,
		container: &detail,
		baseline: domain.Snapshot{
			ID: 7, ContainerID: execContainerID, ReadinessStatus: domain.ReadinessReady,
		},
		policy:  domain.PolicyEvaluation{ContainerID: execContainerID, EvaluatedAt: now.Add(-time.Hour), Complete: true, Compliant: true},
		refresh: &refresh,
		intel: domain.ImageIntel{
			Reference: "docker.io/library/nginx:1.27.0", Status: domain.CheckOK,
			LastSuccessAt: &completed, RemoteDigest: execDigest,
		},
	}
}

// --------------------------------------------------------------- store --

// fakeExecutionStore is an in-memory execution repository.
type fakeExecutionStore struct {
	mu sync.Mutex

	records map[string]*domain.Execution
	order   []string
	events  map[string][]domain.ExecutionEvent

	createErr  error
	advanceErr error
	// advanceErrTo fails only the transition INTO this state, which is how a
	// persistence failure is placed at an exact point without racing the
	// pipeline.
	advanceErrTo domain.ExecutionState
	// checkpointErrAt fails the checkpoint with this value, which is how a
	// persistence failure is placed at an exact point in the pipeline.
	checkpointErrAt domain.ExecutionCheckpoint

	// checkpoints records every checkpoint written, in order.
	checkpoints []domain.ExecutionCheckpoint
}

func newFakeExecutionStore() *fakeExecutionStore {
	return &fakeExecutionStore{
		records: make(map[string]*domain.Execution),
		events:  make(map[string][]domain.ExecutionEvent),
	}
}

func (f *fakeExecutionStore) Create(
	_ context.Context, execution domain.Execution, _ time.Time,
) (domain.Execution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.createErr != nil {
		return domain.Execution{}, f.createErr
	}
	for _, existing := range f.records {
		if existing.AcquisitionID == execution.AcquisitionID {
			return domain.Execution{}, store.ErrAcquisitionConsumed
		}
		if existing.State.Active() && existing.ContainerID == execution.ContainerID {
			return domain.Execution{}, store.ErrExecutionActive
		}
		if execution.RequestKey != "" && existing.RequestKey == execution.RequestKey {
			return *existing, nil
		}
	}
	if execution.State == "" {
		execution.State = domain.ExecutionQueued
	}

	stored := execution
	f.records[execution.ExecutionID] = &stored
	f.order = append(f.order, execution.ExecutionID)
	return stored, nil
}

func (f *fakeExecutionStore) Get(_ context.Context, id string) (domain.Execution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, ok := f.records[id]
	if !ok {
		return domain.Execution{}, store.ErrNotFound
	}
	return *record, nil
}

func (f *fakeExecutionStore) List(
	_ context.Context, filter store.ExecutionFilter,
) ([]domain.Execution, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]domain.Execution, 0, len(f.order))
	for _, id := range f.order {
		record := f.records[id]
		if filter.ActiveOnly && !record.State.Active() {
			continue
		}
		out = append(out, *record)
	}
	return out, len(out), nil
}

func (f *fakeExecutionStore) Events(
	_ context.Context, id string, _ int,
) ([]domain.ExecutionEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.ExecutionEvent(nil), f.events[id]...), nil
}

func (f *fakeExecutionStore) Summary(context.Context) (domain.ExecutionSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	summary := domain.ExecutionSummary{
		ByState:   make(map[domain.ExecutionState]int),
		ByFailure: make(map[domain.ExecutionFailure]int),
	}
	for _, record := range f.records {
		summary.Total++
		summary.ByState[record.State]++
		if record.State.Active() {
			summary.Active++
		}
	}
	return summary, nil
}

func (f *fakeExecutionStore) Advance(
	_ context.Context, change store.ExecutionChange, now time.Time,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.advanceErr != nil {
		return false, f.advanceErr
	}
	if f.advanceErrTo != "" && f.advanceErrTo == change.To {
		return false, errors.New("the database is unavailable")
	}
	record, ok := f.records[change.ExecutionID]
	if !ok {
		return false, store.ErrNotFound
	}

	from := change.From
	if len(from) == 0 {
		from = []domain.ExecutionState{
			domain.ExecutionQueued, domain.ExecutionValidating, domain.ExecutionCapturing,
			domain.ExecutionCreating, domain.ExecutionStarting, domain.ExecutionVerifying,
		}
	}
	allowed := false
	for _, state := range from {
		if record.State == state {
			allowed = true
			break
		}
	}
	if !allowed {
		return false, nil
	}

	record.State = change.To
	record.Failure = change.Failure
	record.Refusal = change.Refusal
	if change.Message != "" {
		record.Message = change.Message
	}
	if change.Checkpoint != domain.CheckpointNone {
		record.Checkpoint = change.Checkpoint
	}
	if change.ReplacementID != "" {
		record.ReplacementID = change.ReplacementID
	}
	if change.ParkedName != "" {
		record.ParkedName = change.ParkedName
	}
	if change.QuarantineName != "" {
		record.QuarantineName = change.QuarantineName
	}
	if change.OriginalRemoved {
		record.OriginalRemoved = true
	}
	if change.Verification != nil {
		record.Verification = *change.Verification
	}
	if change.Recovery != nil {
		record.Recovery = change.Recovery
	}
	if change.MarkStarted && record.StartedAt == nil {
		at := now
		record.StartedAt = &at
	}
	if change.MarkMutated && record.MutatedAt == nil {
		at := now
		record.MutatedAt = &at
	}
	if change.To.Terminal() && record.CompletedAt == nil {
		at := now
		record.CompletedAt = &at
	}

	f.events[change.ExecutionID] = append(f.events[change.ExecutionID],
		domain.ExecutionEvent{State: change.To, Checkpoint: change.Checkpoint,
			Detail: change.Detail, At: now})
	return true, nil
}

func (f *fakeExecutionStore) Checkpoint(
	_ context.Context, write store.ExecutionCheckpointWrite, now time.Time,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.checkpointErrAt != "" && f.checkpointErrAt == write.Checkpoint {
		return store.ErrCheckpointNotWritten
	}
	record, ok := f.records[write.ExecutionID]
	if !ok {
		return store.ErrCheckpointNotWritten
	}

	record.Checkpoint = write.Checkpoint
	if write.ReplacementID != "" {
		record.ReplacementID = write.ReplacementID
	}
	if write.ParkedName != "" {
		record.ParkedName = write.ParkedName
	}
	if write.QuarantineName != "" {
		record.QuarantineName = write.QuarantineName
	}
	if write.OriginalRemoved {
		record.OriginalRemoved = true
	}
	if write.MarkMutated && record.MutatedAt == nil {
		at := now
		record.MutatedAt = &at
	}

	f.checkpoints = append(f.checkpoints, write.Checkpoint)
	f.events[write.ExecutionID] = append(f.events[write.ExecutionID],
		domain.ExecutionEvent{State: record.State, Checkpoint: write.Checkpoint,
			Detail: write.Detail, At: now})
	return nil
}

func (f *fakeExecutionStore) ActiveCount(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	count := 0
	for _, record := range f.records {
		if record.State.Active() {
			count++
		}
	}
	return count, nil
}

func (f *fakeExecutionStore) ActiveForContainer(_ context.Context, containerID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, record := range f.records {
		if record.ContainerID == containerID && record.State.Active() {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeExecutionStore) ByAcquisition(
	_ context.Context, acquisitionID string,
) (domain.Execution, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, id := range f.order {
		if f.records[id].AcquisitionID == acquisitionID {
			return *f.records[id], true, nil
		}
	}
	return domain.Execution{}, false, nil
}

func (f *fakeExecutionStore) ByRequestKey(
	_ context.Context, key string,
) (domain.Execution, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if key == "" {
		return domain.Execution{}, false, nil
	}
	for _, id := range f.order {
		if f.records[id].RequestKey == key {
			return *f.records[id], true, nil
		}
	}
	return domain.Execution{}, false, nil
}

func (f *fakeExecutionStore) Claimable(_ context.Context, limit int) ([]domain.Execution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]domain.Execution, 0, limit)
	for _, id := range f.order {
		if f.records[id].State == domain.ExecutionQueued {
			out = append(out, *f.records[id])
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeExecutionStore) Interrupted(_ context.Context, _ int) ([]domain.Execution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]domain.Execution, 0, 4)
	for _, id := range f.order {
		record := f.records[id]
		if record.State.Active() && record.State != domain.ExecutionQueued {
			out = append(out, *record)
		}
	}
	return out, nil
}

func (f *fakeExecutionStore) ExpireStale(_ context.Context, now time.Time, _ int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var expired int64
	for _, record := range f.records {
		if record.State == domain.ExecutionQueued && record.ExpiresAt.Before(now) {
			record.State = domain.ExecutionExpired
			expired++
		}
	}
	return expired, nil
}

func (f *fakeExecutionStore) Prune(context.Context, time.Time, int) (int64, error) { return 0, nil }

func (f *fakeExecutionStore) checkpointsWritten() []domain.ExecutionCheckpoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.ExecutionCheckpoint(nil), f.checkpoints...)
}

// --------------------------------------------------------------- harness --

// execReplacementID is the id the fake mutator gives the replacement.
//
// Pinned rather than discovered, so the harness can register the runtime
// inspection that verification will read. Verification inspects through the
// READ-ONLY runtime, which is a separate double from the mutator, so the two
// have to be told about the same container.
var execReplacementID = docker.FakeContainerID(42)

// execReplacementDetail is what the replacement reports once it is running: the
// original's configuration, on the approved image, under a new container id.
//
// A faithful recreation, in other words. Tests that want a specific failure
// override one field of this.
func execReplacementDetail() domain.ContainerDetail {
	detail := execContainerDetail(execReplacementID)
	detail.Overview.Image = domain.ParseImageRef("docker.io/library/nginx@" + execDigest)
	detail.Overview.ImageID = execImageID
	return detail
}

type execHarness struct {
	service  *service.ExecutionService
	store    *fakeExecutionStore
	evidence *fakeExecutionEvidence
	runtime  *docker.Fake
	mutator  *docker.FakeMutator
	// lineage is what the container FOLLOWS. Advanced only by a recreation that
	// passed verification, which is what the refusal tests assert stays true.
	lineage *fakeLineageStore

	// base is the harness's epoch and start the wall-clock moment it was
	// created. now() returns base plus elapsed real time.
	//
	// A frozen clock would be tidier but would make every timeout untestable:
	// the health wait compares against a deadline computed from now(), so a
	// clock that never advances means a wait that never ends.
	base  time.Time
	start time.Time
	// skew is added to now(), so a test can jump the clock forward to make a
	// deadline arrive without sleeping through it.
	skew atomic.Int64

	// dependencies is the namespace evidence the preflight consults. Defaults
	// to an ordinary standalone container; the invariant A tests replace it.
	dependencies service.ExecutionDependencies

	// assurance establishes the current baseline in the preflight. NIL by
	// default, which is the pre-Phase-17 behaviour every test in this file was
	// written against; the snapshot-assurance tests wire one in through tune.
	assurance *service.SnapshotAssurance

	// approvals answers whether a person reviewed a plan. NIL by default, which
	// is the pre-Phase-17.7 behaviour: a manualReview plan is refused.
	approvals service.PlanApprovals

	// requireSnapshot overrides EXECUTION_REQUIRE_SNAPSHOT. Nil leaves it at
	// true, which is both the default and what every existing test assumes.
	requireSnapshot *bool
}

func (h *execHarness) now() time.Time {
	return h.base.Add(time.Since(h.start)).Add(time.Duration(h.skew.Load()))
}

// advance moves the harness clock forward.
func (h *execHarness) advance(by time.Duration) { h.skew.Add(int64(by)) }

// newExecHarness builds a service over a world in which a recreation should
// succeed.
func newExecHarness(t *testing.T, tune ...func(*execHarness)) *execHarness {
	t.Helper()

	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	detail := execContainerDetail(execContainerID)

	runtime := docker.NewFake()
	runtime.Containers = []domain.ContainerSummary{detail.Overview}
	runtime.Inspections = map[string]*docker.Inspection{
		execContainerID:   {Detail: detail},
		execReplacementID: {Detail: execReplacementDetail()},
	}
	runtime.Images = map[string]*domain.Image{
		"docker.io/library/nginx@" + execDigest: {
			ID:           execImageID,
			RepoDigests:  []string{"docker.io/library/nginx@" + execDigest},
			OS:           "linux",
			Architecture: "amd64",
		},
	}

	mutator := docker.NewFakeMutator()
	mutator.AddContainer(&docker.FakeContainer{
		ID: execContainerID, Name: "web", Image: "nginx:1.27.0",
		Running: true, Detail: detail,
	})
	mutator.NextID = execReplacementID

	harness := &execHarness{
		store:    newFakeExecutionStore(),
		evidence: healthyExecutionEvidence(base),
		runtime:  runtime,
		mutator:  mutator,
		lineage:  newFakeLineageStore(),
		// An ordinary container by default: nothing shares its namespace, and it
		// shares nobody's. Tunable, because the invariant A tests are entirely
		// about the two cases where that is not true.
		dependencies: service.ExecutionDependencies(standaloneDependencies{}),
		base:         base,
		start:        time.Now(),
	}
	for _, apply := range tune {
		apply(harness)
	}

	// The same installation key the snapshots use. Preservation compares
	// sensitive values as keyed digests, so the service is unusable without
	// one -- which is itself the point: no hasher means the comparison is
	// unverifiable, and unverifiable fails closed.
	key, err := service.LoadSecretKey(service.SecretKeyOptions{
		GeneratePath: filepath.Join(t.TempDir(), "secret.key"),
	})
	if err != nil {
		t.Fatalf("load secret key: %v", err)
	}

	requireSnapshot := true
	if harness.requireSnapshot != nil {
		requireSnapshot = *harness.requireSnapshot
	}

	harness.service = service.NewExecutionService(service.ExecutionOptions{
		Store:    harness.store,
		Evidence: harness.evidence,
		// Stated explicitly because the service fails closed without it: "cannot
		// establish what shares this namespace" and "nothing shares it" are
		// opposite facts, and only the second permits a stop.
		Dependencies: harness.dependencies,
		Runtime:      harness.runtime,
		Capturer:     harness.mutator,
		Mutator:      harness.mutator,
		Lineage:      harness.lineage,
		Hasher:       service.NewHasher(key),
		Assurance:    harness.assurance,
		Approvals:    harness.approvals,
		Config: config.Execution{
			Enabled:               true,
			RequireSnapshot:       requireSnapshot,
			StartupTimeout:        2 * time.Second,
			StabilityPeriod:       10 * time.Millisecond,
			HealthPollInterval:    time.Millisecond,
			StopTimeout:           time.Second,
			MaxConcurrent:         1,
			RequestTTL:            15 * time.Minute,
			AcquisitionFreshness:  24 * time.Hour,
			InventoryFreshness:    15 * time.Minute,
			PolicyFreshness:       24 * time.Hour,
			MaxEventsPerExecution: 200,
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    harness.now,
	})
	return harness
}

// runOnce dispatches the queued work and waits for it to finish.
//
// The worker's dispatch loop is driven directly rather than through Run, so a
// test observes exactly one pass and never races a background ticker.
func (h *execHarness) runOnce(t *testing.T, execution domain.Execution) domain.Execution {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.service.Run(ctx)
	}()

	deadline := time.After(10 * time.Second)
	for {
		record, err := h.store.Get(context.Background(), execution.ExecutionID)
		if err == nil && record.State.Terminal() {
			cancel()
			<-done
			return record
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("the recreation did not finish; last state %q", record.State)
		case <-time.After(time.Millisecond):
		}
	}
}

// request asks for a recreation, failing the test if it is refused.
func (h *execHarness) request(t *testing.T) domain.Execution {
	t.Helper()

	execution, err := h.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if err != nil {
		t.Fatalf("request refused: %v", err)
	}
	return execution
}

// executionRefusalFrom extracts the refusal from a request error.
func executionRefusalFrom(t *testing.T, err error) domain.ExecutionRefusal {
	t.Helper()

	var refused service.ErrExecutionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	return refused.Refusal
}

// ---------------------------------------------------------- happy path --

// TestASuccessfulRecreationFollowsTheWholePipeline is the reference case.
func TestASuccessfulRecreationFollowsTheWholePipeline(t *testing.T) {
	harness := newExecHarness(t)
	execution := harness.request(t)
	final := harness.runOnce(t, execution)

	if final.State != domain.ExecutionSucceeded {
		t.Fatalf("state = %q (%s), want succeeded", final.State, final.Message)
	}
	if final.Checkpoint != domain.CheckpointOriginalRemoved {
		t.Errorf("checkpoint = %q, want originalRemoved", final.Checkpoint)
	}
	if !final.OriginalRemoved {
		t.Error("originalRemoved is false on a succeeded recreation")
	}
	if !final.Verification.Passed() {
		t.Errorf("not every proof passed: %+v", final.Verification)
	}

	// The exact order of operations on the host. This is the safety model
	// written as a list, so a reordering fails here rather than in production.
	want := []string{"capture", "stop", "rename", "create", "start", "remove"}
	if got := harness.mutator.Ops(); !equalStrings(got, want) {
		t.Errorf("operations = %v, want %v", got, want)
	}

	// And the checkpoints, in order.
	wantCheckpoints := []domain.ExecutionCheckpoint{
		domain.CheckpointOriginalStopped,
		domain.CheckpointOriginalParked,
		domain.CheckpointReplacementCreated,
		domain.CheckpointReplacementStarted,
		domain.CheckpointReplacementVerified,
		domain.CheckpointOriginalRemoved,
	}
	got := harness.store.checkpointsWritten()
	if len(got) != len(wantCheckpoints) {
		t.Fatalf("checkpoints = %v, want %v", got, wantCheckpoints)
	}
	for i := range got {
		if got[i] != wantCheckpoints[i] {
			t.Errorf("checkpoint %d = %q, want %q", i, got[i], wantCheckpoints[i])
		}
	}

	// The original is gone and the replacement holds its name.
	if harness.mutator.Present(execContainerID) {
		t.Error("the original container was not removed after a successful recreation")
	}
	if harness.mutator.NameOf(final.ReplacementID) != "web" {
		t.Errorf("the replacement is named %q, want web",
			harness.mutator.NameOf(final.ReplacementID))
	}
}

// TestThePreflightRunsAgainBeforeTheFirstMutation is the TOCTOU property.
func TestThePreflightRunsAgainBeforeTheFirstMutation(t *testing.T) {
	harness := newExecHarness(t)

	before := harness.evidence.revalidations()
	execution := harness.request(t)
	afterRequest := harness.evidence.revalidations()

	harness.runOnce(t, execution)
	afterRun := harness.evidence.revalidations()

	if afterRequest <= before {
		t.Fatal("the request did not run a preflight")
	}
	if afterRun <= afterRequest {
		t.Fatal("the preflight did not run again before the first mutation; the whole " +
			"time-of-check/time-of-use window would be open")
	}
}

// -------------------------------------------------------- every refusal --

// TestEveryPreflightRefusalIsReachable walks the ways the world can move and
// asserts that each produces its own named refusal AND changes nothing.
func TestEveryPreflightRefusalIsReachable(t *testing.T) {
	cases := []struct {
		name string
		want domain.ExecutionRefusal
		tune func(*execHarness)
	}{
		{"the acquisition is gone", domain.ExecutionRefusalAcquisitionMissing,
			func(h *execHarness) { h.evidence.acquisitionErr = store.ErrNotFound }},

		{"the acquisition did not succeed", domain.ExecutionRefusalAcquisitionNotSucceeded,
			func(h *execHarness) { h.evidence.acquisition.State = domain.AcquisitionFailed }},

		{"the acquisition is too old", domain.ExecutionRefusalAcquisitionStale,
			func(h *execHarness) {
				old := h.base.Add(-48 * time.Hour)
				h.evidence.acquisition.CompletedAt = &old
			}},

		{"the plan is gone", domain.ExecutionRefusalPlanMissing,
			func(h *execHarness) { h.evidence.planErr = store.ErrNotFound }},

		{"a newer plan exists", domain.ExecutionRefusalPlanSuperseded,
			func(h *execHarness) { h.evidence.current.PlanID = "plan_ffffffffffffffffffff" }},

		{"the plan changed since the download", domain.ExecutionRefusalPlanChanged,
			func(h *execHarness) {
				h.evidence.plan.InputDigest = strings.Repeat("e", 64)
				h.evidence.current.InputDigest = strings.Repeat("e", 64)
			}},

		{"the plan no longer recommends the change", domain.ExecutionRefusalRecommendation,
			func(h *execHarness) {
				h.evidence.plan.Risk.Recommendation = domain.RecommendAgainst
				h.evidence.current.Risk.Recommendation = domain.RecommendAgainst
			}},

		{"the plan cannot judge the change", domain.ExecutionRefusalRecommendation,
			func(h *execHarness) {
				h.evidence.plan.Risk.Recommendation = domain.RecommendUnknown
				h.evidence.current.Risk.Recommendation = domain.RecommendUnknown
			}},

		// Phase 17.7: a plan that asks for review and has not had one is
		// refused as approvalMissing rather than `recommendation`.
		//
		// The distinction is the point. `recommendation` means the verdict can
		// never be acted on; this means the plan IS approvable and nobody has
		// approved it, which is a state with a remedy the operator can carry
		// out. No approval service is wired into this harness, which is also
		// the pre-17.7 behaviour: refused.
		{"the plan asks for manual review", domain.ExecutionRefusalApprovalMissing,
			func(h *execHarness) {
				h.evidence.plan.Risk.Recommendation = domain.RecommendManualReview
				h.evidence.current.Risk.Recommendation = domain.RecommendManualReview
			}},

		{"the inventory is stale", domain.ExecutionRefusalInventoryStale,
			func(h *execHarness) {
				stale := domain.RefreshRecord{StartedAt: h.base.Add(-2 * time.Hour)}
				h.evidence.refresh = &stale
			}},

		{"the inventory has never refreshed", domain.ExecutionRefusalInventoryStale,
			func(h *execHarness) { h.evidence.refresh = nil }},

		{"the container is gone", domain.ExecutionRefusalContainerMissing,
			func(h *execHarness) { h.evidence.containerErr = store.ErrNotFound }},

		{"the container was removed from the inventory", domain.ExecutionRefusalContainerMissing,
			func(h *execHarness) { h.evidence.container.Overview.Present = false }},

		{"something else already changed the container", domain.ExecutionRefusalContainerChanged,
			func(h *execHarness) {
				h.evidence.container.Overview.Image = domain.ParseImageRef("nginx:1.28.0")
			}},

		{"the container is in an unusable state", domain.ExecutionRefusalContainerState,
			func(h *execHarness) {
				h.evidence.container.Overview.State = domain.StateRestarting
			}},

		{"the container name cannot be parked", domain.ExecutionRefusalNameUnavailable,
			func(h *execHarness) {
				long := strings.Repeat("n", domain.MaxRecreatableNameBytes+1)
				h.evidence.container.Overview.Name = long
			}},

		{"there is no snapshot", domain.ExecutionRefusalSnapshotMissing,
			func(h *execHarness) { h.evidence.baselineErr = store.ErrNotFound }},

		{"the snapshot could not be restored from", domain.ExecutionRefusalRestoreReadiness,
			func(h *execHarness) {
				h.evidence.baseline.ReadinessStatus = domain.ReadinessNotReady
			}},

		{"there is no policy evaluation", domain.ExecutionRefusalPolicyStale,
			func(h *execHarness) { h.evidence.policyErr = store.ErrNotFound }},

		{"the policy evaluation is stale", domain.ExecutionRefusalPolicyStale,
			func(h *execHarness) {
				h.evidence.policy.EvaluatedAt = h.base.Add(-72 * time.Hour)
			}},

		{"the policy pass was incomplete", domain.ExecutionRefusalPolicyStale,
			func(h *execHarness) { h.evidence.policy.Complete = false }},

		{"a critical policy violation is open", domain.ExecutionRefusalPolicyViolation,
			func(h *execHarness) {
				h.evidence.plan.PolicyOpen = 1
				h.evidence.plan.PolicyMaxSeverity = domain.PolicySeverityCritical
				h.evidence.current = h.evidence.plan
			}},

		{"the registry evidence is missing", domain.ExecutionRefusalRegistryStale,
			func(h *execHarness) { h.evidence.intelErr = store.ErrNotFound }},

		{"the registry evidence never succeeded", domain.ExecutionRefusalRegistryStale,
			func(h *execHarness) { h.evidence.intel.LastSuccessAt = nil }},

		{"the image is no longer on the host", domain.ExecutionRefusalImageMissing,
			func(h *execHarness) { h.runtime.Images = map[string]*domain.Image{} }},

		{"the local image carries a different digest", domain.ExecutionRefusalDigestMismatch,
			func(h *execHarness) {
				h.runtime.Images["docker.io/library/nginx@"+execDigest] = &domain.Image{
					ID:          execImageID,
					RepoDigests: []string{"docker.io/library/nginx@sha256:" + strings.Repeat("7", 64)},
					OS:          "linux", Architecture: "amd64",
				}
			}},

		{"the image targets a different platform", domain.ExecutionRefusalPlatformMismatch,
			func(h *execHarness) {
				h.runtime.Images["docker.io/library/nginx@"+execDigest] = &domain.Image{
					ID:          execImageID,
					RepoDigests: []string{"docker.io/library/nginx@" + execDigest},
					OS:          "linux", Architecture: "arm64",
				}
			}},

		{"the daemon is not answering", domain.ExecutionRefusalDockerUnavailable,
			func(h *execHarness) { h.runtime.PingErr = docker.ErrUnreachable }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newExecHarness(t, tc.tune)

			_, err := harness.service.Request(context.Background(),
				service.ExecutionRequest{AcquisitionID: execAcquisitionID})
			if err == nil {
				t.Fatal("the recreation was allowed")
			}
			if got := executionRefusalFrom(t, err); got != tc.want {
				t.Errorf("refusal = %q, want %q", got, tc.want)
			}

			// The point of a refusal: nothing on the host was touched.
			if ops := harness.mutator.Ops(); len(ops) != 0 {
				t.Errorf("a refused recreation still performed %v", ops)
			}
			if !harness.mutator.Present(execContainerID) {
				t.Error("the container was removed by a refused recreation")
			}
		})
	}
}

// TestAnAcquisitionCanOnlyBeExecutedOnce is the single-use rule at the service
// boundary.
func TestAnAcquisitionCanOnlyBeExecutedOnce(t *testing.T) {
	harness := newExecHarness(t)

	execution := harness.request(t)
	harness.runOnce(t, execution)

	_, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if err == nil {
		t.Fatal("the same acquisition was executed twice")
	}
	if got := executionRefusalFrom(t, err); got != domain.ExecutionRefusalAcquisitionConsumed {
		t.Errorf("refusal = %q, want acquisitionConsumed", got)
	}
}

// TestASecondRecreationForOneContainerIsRefused.
func TestASecondRecreationForOneContainerIsRefused(t *testing.T) {
	harness := newExecHarness(t)
	harness.request(t)

	// A different acquisition, same container.
	harness.evidence.acquisition.AcquisitionID = "acq_ffeeddccbbaa99887766"
	_, err := harness.service.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: "acq_ffeeddccbbaa99887766"})
	if err == nil {
		t.Fatal("two recreations for one container were allowed")
	}
	if got := executionRefusalFrom(t, err); got != domain.ExecutionRefusalConflict {
		t.Errorf("refusal = %q, want conflict", got)
	}
}

func TestAnIdempotencyKeyReturnsTheSameRecreation(t *testing.T) {
	harness := newExecHarness(t)

	first, err := harness.service.Request(context.Background(), service.ExecutionRequest{
		AcquisitionID: execAcquisitionID, RequestKey: "double-click",
	})
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	second, err := harness.service.Request(context.Background(), service.ExecutionRequest{
		AcquisitionID: execAcquisitionID, RequestKey: "double-click",
	})
	if err != nil {
		t.Fatalf("retried request: %v", err)
	}
	if first.ExecutionID != second.ExecutionID {
		t.Fatal("a double-clicked button started two recreations")
	}
}

func TestADisabledServiceRefusesEverything(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// No mutator wired: the capability is ABSENT, not merely switched off.
	svc := service.NewExecutionService(service.ExecutionOptions{
		Store:    newFakeExecutionStore(),
		Evidence: healthyExecutionEvidence(time.Now().UTC()),
		Runtime:  docker.NewFake(),
		Config:   config.Execution{Enabled: true},
		Logger:   logger,
	})

	if svc.Enabled() {
		t.Fatal("a service with no mutation capability reported itself enabled")
	}
	_, err := svc.Request(context.Background(),
		service.ExecutionRequest{AcquisitionID: execAcquisitionID})
	if got := executionRefusalFrom(t, err); got != domain.ExecutionRefusalDisabled {
		t.Errorf("refusal = %q, want disabled", got)
	}
}

// ------------------------------------------------- failures after mutation --

// TestAnUnhealthyReplacementIsQuarantinedAndBothContainersSurvive is the
// headline failure case.
func TestAnUnhealthyReplacementIsQuarantinedAndBothContainersSurvive(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		// The REPLACEMENT reports unhealthy. Verification reads it through the
		// read-only runtime, which is where this has to be registered.
		unhealthy := execReplacementDetail()
		unhealthy.State.Health = domain.HealthUnhealthy
		unhealthy.Overview.Health = domain.HealthUnhealthy
		h.runtime.Inspections[execReplacementID] = &docker.Inspection{Detail: unhealthy}
	})

	execution := harness.request(t)
	final := harness.runOnce(t, execution)

	if final.State != domain.ExecutionFailed {
		t.Fatalf("state = %q, want failed", final.State)
	}
	if final.Failure != domain.ExecutionFailureUnhealthy {
		t.Errorf("failure = %q, want unhealthy", final.Failure)
	}
	if final.Verification.Health != domain.VerificationFailed {
		t.Errorf("health verdict = %q, want failed", final.Verification.Health)
	}

	// NEITHER container was removed.
	if !harness.mutator.Present(execContainerID) {
		t.Fatal("the original was removed after a failed recreation; there is no way back")
	}
	if final.ReplacementID == "" {
		t.Fatal("no replacement was recorded, so an operator cannot find the container that " +
			"failed verification")
	}
	if !harness.mutator.Present(final.ReplacementID) {
		t.Error("the failed replacement was removed; it is the evidence for why the image " +
			"did not work")
	}

	// The replacement was moved off the production name, and stopped.
	if name := harness.mutator.NameOf(final.ReplacementID); !strings.Contains(name, ".hm-failed-") {
		t.Errorf("the failed replacement is still named %q", name)
	}
	if harness.mutator.Containers[final.ReplacementID].Running {
		t.Error("a container that failed verification is still running under the estate")
	}

	// And an operator was told what to do.
	if final.Recovery == nil {
		t.Fatal("no recovery plan was recorded")
	}
	if !final.Recovery.ServiceInterrupted {
		t.Error("the recovery plan does not say the service is down")
	}
	if len(final.Recovery.Steps) == 0 {
		t.Error("the recovery plan has no steps")
	}
}

// TestAReplacementThatWillNotStartLeavesBothContainers.
func TestAReplacementThatWillNotStartLeavesBothContainers(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.StartErr = docker.ErrMutationFailed
	})

	execution := harness.request(t)
	final := harness.runOnce(t, execution)

	if final.Failure != domain.ExecutionFailureStart {
		t.Fatalf("failure = %q, want start", final.Failure)
	}
	if !harness.mutator.Present(execContainerID) {
		t.Fatal("the original was removed")
	}
	if final.Checkpoint == domain.CheckpointOriginalRemoved {
		t.Error("the checkpoint claims the original was removed")
	}
	if final.Recovery == nil {
		t.Fatal("no recovery plan was recorded")
	}
}

// TestACreateFailureLeavesTheOriginalParkedAndIntact.
func TestACreateFailureLeavesTheOriginalParkedAndIntact(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.CreateErr = docker.ErrMutationFailed
	})

	execution := harness.request(t)
	final := harness.runOnce(t, execution)

	if final.Failure != domain.ExecutionFailureCreate {
		t.Fatalf("failure = %q, want create", final.Failure)
	}
	if final.Checkpoint != domain.CheckpointOriginalParked {
		t.Errorf("checkpoint = %q, want originalParked", final.Checkpoint)
	}
	if !harness.mutator.Present(execContainerID) {
		t.Fatal("the original was removed")
	}
	if name := harness.mutator.NameOf(execContainerID); !strings.Contains(name, ".hm-old-") {
		t.Errorf("the original is named %q; the recovery plan tells the operator to rename it "+
			"back from the parked name", name)
	}
	if final.Recovery == nil || !final.Recovery.ServiceInterrupted {
		t.Error("the recovery plan does not report an interrupted service")
	}
}

// TestAStopFailureChangesNothingIrreversible.
func TestAStopFailureChangesNothingIrreversible(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.StopErr = docker.ErrMutationFailed
	})

	execution := harness.request(t)
	final := harness.runOnce(t, execution)

	if final.Failure != domain.ExecutionFailureStop {
		t.Fatalf("failure = %q, want stop", final.Failure)
	}
	if ops := harness.mutator.Ops(); containsString(ops, "create") || containsString(ops, "remove") {
		t.Errorf("a failed stop was followed by %v", ops)
	}
	if !harness.mutator.Present(execContainerID) {
		t.Fatal("the original was removed after the stop failed")
	}
}

// TestADaemonThatDisappearsMidPipelineIsClassifiedAsSuch.
func TestADaemonThatDisappearsMidPipelineIsClassifiedAsSuch(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.CreateErr = docker.ErrUnreachable
	})

	execution := harness.request(t)
	final := harness.runOnce(t, execution)

	if final.Failure != domain.ExecutionFailureDockerUnavailable {
		t.Fatalf("failure = %q, want dockerUnavailable", final.Failure)
	}
	if !harness.mutator.Present(execContainerID) {
		t.Error("the original was removed")
	}
}

// TestAPreservationMismatchFailsAndKeepsTheOriginal.
//
// The replacement runs, is healthy, and is on the right image -- but is
// configured differently. That is the case a naive implementation would call a
// success.
func TestAPreservationMismatchFailsAndKeepsTheOriginal(t *testing.T) {
	harness := newExecHarness(t)

	// The harness registers a FAITHFUL replacement by default; this test
	// replaces that registration with one that differs in a single field.
	changed := execReplacementDetail()
	// A dropped capability restriction: the single most consequential thing a
	// recreation could silently lose.
	changed.Security.CapDrop = nil
	harness.runtime.Inspections[execReplacementID] = &docker.Inspection{Detail: changed}

	execution := harness.request(t)
	final := harness.runOnce(t, execution)

	if final.State != domain.ExecutionFailed {
		t.Fatalf("state = %q, want failed; the replacement lost a capability restriction",
			final.State)
	}
	if final.Failure != domain.ExecutionFailurePreservation {
		t.Errorf("failure = %q, want preservation", final.Failure)
	}
	if final.Verification.Report == nil || len(final.Verification.Report.Differences) == 0 {
		t.Error("the failure names no difference, so an operator would not know what changed")
	}
	if !harness.mutator.Present(execContainerID) {
		t.Fatal("the original was removed despite the configuration not being preserved")
	}
}

// TestAnImageMismatchFailsAndKeepsTheOriginal.
func TestAnImageMismatchFailsAndKeepsTheOriginal(t *testing.T) {
	harness := newExecHarness(t)

	// Running, healthy, and on the OLD image. The case a naive implementation
	// would call a success.
	wrong := execReplacementDetail()
	wrong.Overview.Image = domain.ParseImageRef("nginx:1.27.0")
	wrong.Overview.ImageID = "sha256:" + strings.Repeat("8", 64)
	harness.runtime.Inspections[execReplacementID] = &docker.Inspection{Detail: wrong}

	execution := harness.request(t)
	final := harness.runOnce(t, execution)

	if final.Failure != domain.ExecutionFailureImageMismatch {
		t.Fatalf("failure = %q, want imageMismatch", final.Failure)
	}
	if !harness.mutator.Present(execContainerID) {
		t.Fatal("the original was removed despite the wrong image running")
	}
}

// TestALostNetworkFailsAndKeepsTheOriginal.
func TestALostNetworkFailsAndKeepsTheOriginal(t *testing.T) {
	harness := newExecHarness(t)

	detached := execReplacementDetail()
	detached.Networks = nil
	harness.runtime.Inspections[execReplacementID] = &docker.Inspection{Detail: detached}

	execution := harness.request(t)
	final := harness.runOnce(t, execution)

	if final.State != domain.ExecutionFailed {
		t.Fatalf("state = %q, want failed", final.State)
	}
	// Preservation also covers networks and runs first, so either verdict is a
	// correct refusal. What must not happen is a success.
	if final.Failure != domain.ExecutionFailureNetwork &&
		final.Failure != domain.ExecutionFailurePreservation {
		t.Errorf("failure = %q, want network or preservation", final.Failure)
	}
	if !harness.mutator.Present(execContainerID) {
		t.Fatal("the original was removed despite the replacement losing its networks")
	}
}

// TestAHealthTimeoutFailsWithoutRemovingAnything.
func TestAHealthTimeoutFailsWithoutRemovingAnything(t *testing.T) {
	harness := newExecHarness(t)

	// Perpetually STARTING: never healthy, never unhealthy. Exactly the case
	// the startup timeout exists for -- a container that neither succeeds nor
	// admits failure.
	starting := execReplacementDetail()
	starting.State.Health = domain.HealthStarting
	starting.Overview.Health = domain.HealthStarting
	harness.runtime.Inspections[execReplacementID] = &docker.Inspection{Detail: starting}

	execution := harness.request(t)

	// Jump the clock past the startup budget while the wait is polling, so the
	// deadline arrives without the test sleeping through it.
	go func() {
		time.Sleep(20 * time.Millisecond)
		harness.advance(time.Hour)
	}()

	final := harness.runOnce(t, execution)

	if final.State != domain.ExecutionFailed {
		t.Fatalf("state = %q, want failed", final.State)
	}
	if final.Failure != domain.ExecutionFailureHealthTimeout {
		t.Errorf("failure = %q, want healthTimeout", final.Failure)
	}
	if !harness.mutator.Present(execContainerID) {
		t.Fatal("the original was removed after a health timeout")
	}
}

// TestAContainerWithNoHealthCheckMustStayRunning covers the stability path.
func TestAContainerWithNoHealthCheckMustStayRunning(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		// No health check anywhere: on the original, on what the preflight
		// reads, or on the replacement. The last one matters as much as the
		// first -- a replacement that still declared one would be a real
		// configuration difference and the preservation check would say so.
		detail := execContainerDetail(execContainerID)
		detail.HealthCheck = nil
		detail.State.Health = domain.HealthNone
		detail.Overview.Health = domain.HealthNone
		h.mutator.Containers[execContainerID].Detail = detail
		h.evidence.container = &detail
		h.runtime.Inspections[execContainerID] = &docker.Inspection{Detail: detail}

		replacement := execReplacementDetail()
		replacement.HealthCheck = nil
		replacement.State.Health = domain.HealthNone
		replacement.Overview.Health = domain.HealthNone
		h.runtime.Inspections[execReplacementID] = &docker.Inspection{Detail: replacement}
	})

	execution := harness.request(t)
	final := harness.runOnce(t, execution)

	if final.State != domain.ExecutionSucceeded {
		t.Fatalf("state = %q (%s), want succeeded", final.State, final.Message)
	}
	if final.Verification.HealthChecked {
		t.Error("the record claims a health check was used on a container that declares none")
	}
	if final.Verification.StabilitySeconds < 0 {
		t.Error("the stability window was not recorded")
	}
}

// TestAContainerThatExitsDuringItsStabilityWindowFails.
func TestAContainerThatExitsDuringItsStabilityWindowFails(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		detail := execContainerDetail(execContainerID)
		detail.HealthCheck = nil
		detail.State.Health = domain.HealthNone
		h.mutator.Containers[execContainerID].Detail = detail
		h.evidence.container = &detail
	})

	exited := execReplacementDetail()
	exited.HealthCheck = nil
	exited.State = domain.StateDetail{State: domain.StateExited, Running: false}
	exited.Overview.State = domain.StateExited
	harness.runtime.Inspections[execReplacementID] = &docker.Inspection{Detail: exited}

	execution := harness.request(t)
	final := harness.runOnce(t, execution)

	if final.Failure != domain.ExecutionFailureNotStable {
		t.Fatalf("failure = %q, want notStable", final.Failure)
	}
	if !harness.mutator.Present(execContainerID) {
		t.Fatal("the original was removed after the replacement exited")
	}
}

// --------------------------------------------------------- persistence --

// TestAFailedCheckpointStopsThePipelineWithoutRepeatingAMutation is the
// requirement that a mutation is never blindly repeated.
func TestAFailedCheckpointStopsThePipelineWithoutRepeatingAMutation(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		// The checkpoint after the STOP cannot be written.
		h.store.checkpointErrAt = domain.CheckpointOriginalStopped
	})

	execution := harness.request(t)
	final := harness.runOnce(t, execution)

	if final.Failure != domain.ExecutionFailurePersistence {
		t.Fatalf("failure = %q, want persistence", final.Failure)
	}

	ops := harness.mutator.Ops()
	// The stop happened once. Nothing after it did, and crucially the stop was
	// NOT retried.
	if count := countString(ops, "stop"); count != 1 {
		t.Errorf("stop was performed %d times, want exactly 1; a mutation whose record is "+
			"uncertain must never be repeated", count)
	}
	for _, forbidden := range []string{"rename", "create", "start", "remove"} {
		if containsString(ops, forbidden) {
			t.Errorf("the pipeline performed %q after a checkpoint could not be written", forbidden)
		}
	}
	if !harness.mutator.Present(execContainerID) {
		t.Fatal("the original was removed")
	}
}

// TestAFailedCheckpointAtEachStageStopsImmediately walks the pipeline.
func TestAFailedCheckpointAtEachStageStopsImmediately(t *testing.T) {
	stages := []struct {
		checkpoint domain.ExecutionCheckpoint
		forbidden  []string
	}{
		{domain.CheckpointOriginalStopped, []string{"rename", "create", "start", "remove"}},
		{domain.CheckpointOriginalParked, []string{"create", "start", "remove"}},
		{domain.CheckpointReplacementCreated, []string{"start", "remove"}},
		{domain.CheckpointReplacementStarted, []string{"remove"}},
		{domain.CheckpointReplacementVerified, []string{"remove"}},
	}

	for _, stage := range stages {
		t.Run(string(stage.checkpoint), func(t *testing.T) {
			harness := newExecHarness(t, func(h *execHarness) {
				h.store.checkpointErrAt = stage.checkpoint
			})

			execution := harness.request(t)
			final := harness.runOnce(t, execution)

			if final.Failure != domain.ExecutionFailurePersistence {
				t.Fatalf("failure = %q, want persistence", final.Failure)
			}
			for _, forbidden := range stage.forbidden {
				if containsString(harness.mutator.Ops(), forbidden) {
					t.Errorf("performed %q after the %q checkpoint failed",
						forbidden, stage.checkpoint)
				}
			}
			if !harness.mutator.Present(execContainerID) {
				t.Error("the original was removed after a persistence failure")
			}
		})
	}
}

// TestTheOriginalIsRemovedOnlyAfterTheSuccessIsRecorded.
//
// The success write is made to fail. The original must survive, because
// HarborMaster cannot afterwards prove the replacement was ever verified.
func TestTheOriginalIsRemovedOnlyAfterTheSuccessIsRecorded(t *testing.T) {
	// Everything works except the one write that records the success. Every
	// proof passes; only the durable record of that fact does not land.
	harness := newExecHarness(t, func(h *execHarness) {
		h.store.advanceErrTo = domain.ExecutionSucceeded
	})

	execution := harness.request(t)
	final := harness.runOnce(t, execution)

	if final.State != domain.ExecutionFailed {
		t.Fatalf("state = %q, want failed", final.State)
	}
	if final.Failure != domain.ExecutionFailurePersistence {
		t.Errorf("failure = %q, want persistence", final.Failure)
	}

	// THE assertion. HarborMaster cannot prove afterwards that the replacement
	// was ever verified, so it must not have removed the only thing that could
	// restore service.
	if containsString(harness.mutator.Ops(), "remove") {
		t.Fatal("the original was removed although the success could not be recorded")
	}
	if !harness.mutator.Present(execContainerID) {
		t.Fatal("the original is gone")
	}
	if final.OriginalRemoved {
		t.Error("the record claims the original was removed")
	}
}

// ------------------------------------------------------- cancellation --

// TestCancellationIsImpossibleAfterTheMutationPoint.
func TestCancellationIsImpossibleAfterTheMutationPoint(t *testing.T) {
	harness := newExecHarness(t)
	execution := harness.request(t)
	final := harness.runOnce(t, execution)

	if final.State != domain.ExecutionSucceeded {
		t.Fatalf("state = %q, want succeeded", final.State)
	}

	_, err := harness.service.Cancel(context.Background(), execution.ExecutionID)
	if err == nil {
		t.Fatal("a finished recreation was cancelled")
	}
	var refused service.ErrExecutionRefused
	if !errors.As(err, &refused) {
		t.Errorf("err = %v, want a refusal", err)
	}
}

// TestCancellingAQueuedRecreationChangesNothing.
func TestCancellingAQueuedRecreationChangesNothing(t *testing.T) {
	harness := newExecHarness(t)
	execution := harness.request(t)

	cancelled, err := harness.service.Cancel(context.Background(), execution.ExecutionID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.State != domain.ExecutionCancelled {
		t.Fatalf("state = %q, want cancelled", cancelled.State)
	}
	if cancelled.Checkpoint != domain.CheckpointNone {
		t.Errorf("checkpoint = %q; a cancelled recreation changed nothing", cancelled.Checkpoint)
	}
	if ops := harness.mutator.Ops(); len(ops) != 0 {
		t.Errorf("a cancelled recreation performed %v", ops)
	}
	if !harness.mutator.Present(execContainerID) {
		t.Error("the container was removed by a cancelled recreation")
	}
}

// --------------------------------------------------- restart recovery --

// TestRestartRecoveryIsCheckpointAwareAndIssuesNoDockerCall.
func TestRestartRecoveryIsCheckpointAwareAndIssuesNoDockerCall(t *testing.T) {
	cases := []struct {
		checkpoint domain.ExecutionCheckpoint
		state      domain.ExecutionState
		wantUrgent bool
	}{
		{domain.CheckpointNone, domain.ExecutionCreating, true},
		{domain.CheckpointOriginalStopped, domain.ExecutionCreating, true},
		{domain.CheckpointOriginalParked, domain.ExecutionCreating, true},
		{domain.CheckpointReplacementCreated, domain.ExecutionCreating, true},
		{domain.CheckpointReplacementStarted, domain.ExecutionStarting, true},
		{domain.CheckpointReplacementVerified, domain.ExecutionVerifying, false},
	}

	for _, tc := range cases {
		t.Run(string(tc.state)+"/"+string(tc.checkpoint), func(t *testing.T) {
			harness := newExecHarness(t)

			// A row left mid-flight by a crash.
			left := domain.Execution{
				ExecutionID:   domain.NewExecutionID(),
				AcquisitionID: execAcquisitionID,
				PlanID:        execPlanID,
				ContainerID:   execContainerID,
				ContainerName: "web",
				Target: domain.ExecutionTarget{
					Registry: "docker.io", Repository: "library/nginx", Digest: execDigest,
				},
				State:       tc.state,
				Checkpoint:  tc.checkpoint,
				ParkedName:  "web.hm-old-" + domain.NewExecutionID(),
				RequestedAt: harness.now(),
				ExpiresAt:   harness.base.Add(time.Hour),
			}
			harness.store.records[left.ExecutionID] = &left
			harness.store.order = append(harness.store.order, left.ExecutionID)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			done := make(chan struct{})
			go func() {
				defer close(done)
				harness.service.Run(ctx)
			}()

			deadline := time.After(3 * time.Second)
			for {
				record, _ := harness.store.Get(context.Background(), left.ExecutionID)
				if record.State.Terminal() {
					cancel()
					<-done

					if record.Failure != domain.ExecutionFailureInterrupted {
						t.Errorf("failure = %q, want interrupted", record.Failure)
					}
					if record.Recovery == nil {
						t.Fatal("no recovery plan was recorded")
					}
					if got := record.Recovery.ServiceInterrupted; got != tc.wantUrgent {
						t.Errorf("serviceInterrupted = %v, want %v", got, tc.wantUrgent)
					}
					// THE point of this test: recovery reasons from the record
					// and touches nothing.
					if ops := harness.mutator.Ops(); len(ops) != 0 {
						t.Errorf("the recovery pass performed %v; it must issue no Docker "+
							"call at all", ops)
					}
					return
				}
				select {
				case <-deadline:
					cancel()
					<-done
					t.Fatalf("the interrupted record was not settled; state %q", record.State)
				case <-time.After(time.Millisecond):
				}
			}
		})
	}
}

// TestAnUncertainStopIsNotReportedAsHarmless is the honesty requirement.
func TestAnUncertainStopIsNotReportedAsHarmless(t *testing.T) {
	harness := newExecHarness(t)

	left := domain.Execution{
		ExecutionID:   domain.NewExecutionID(),
		AcquisitionID: execAcquisitionID,
		ContainerID:   execContainerID,
		ContainerName: "web",
		Target: domain.ExecutionTarget{
			Registry: "docker.io", Repository: "library/nginx", Digest: execDigest,
		},
		State:       domain.ExecutionCreating,
		Checkpoint:  domain.CheckpointNone,
		RequestedAt: harness.now(),
		ExpiresAt:   harness.base.Add(time.Hour),
	}
	harness.store.records[left.ExecutionID] = &left
	harness.store.order = append(harness.store.order, left.ExecutionID)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		harness.service.Run(ctx)
	}()

	deadline := time.After(3 * time.Second)
	for {
		record, _ := harness.store.Get(context.Background(), left.ExecutionID)
		if record.State.Terminal() {
			cancel()
			<-done

			if record.Recovery == nil {
				t.Fatal("no recovery plan")
			}
			if strings.Contains(record.Recovery.Situation, "Nothing on this host was changed") {
				t.Fatal("an unconfirmed stop was reported as having changed nothing")
			}
			if !record.Recovery.ServiceInterrupted {
				t.Error("an unconfirmed stop was not flagged as possibly service-interrupting")
			}
			return
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("the record was not settled")
		case <-time.After(time.Millisecond):
		}
	}
}

// ------------------------------------------------------------ shutdown --

// TestShutdownDuringARecreationIsBoundedAndLeavesARecoverableRecord.
func TestShutdownDuringARecreationIsBoundedAndLeavesARecoverableRecord(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		// Slow enough that a shutdown lands mid-pipeline.
		h.mutator.Delay = 40 * time.Millisecond
	})

	execution := harness.request(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		harness.service.Run(ctx)
	}()

	// Wait for the first mutation to be in flight, then shut down.
	select {
	case <-harness.mutator.Started:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("no mutation started")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within the shutdown budget; a wedged recreation would " +
			"hold the whole process open")
	}

	// Whatever state it reached, the record must let a restart reason about it:
	// either it is terminal, or its checkpoint says what was done.
	record, err := harness.store.Get(context.Background(), execution.ExecutionID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !record.State.Terminal() && !record.State.Active() {
		t.Fatalf("the record is in neither an active nor a terminal state: %q", record.State)
	}
	if !harness.mutator.Present(execContainerID) {
		t.Error("the original was removed during a shutdown")
	}
}

// ------------------------------------------------------------- helpers --

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}
