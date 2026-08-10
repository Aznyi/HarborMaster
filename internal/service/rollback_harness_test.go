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

// The rollback test world.
//
// A rollback is judged almost entirely on WHICH host operations happened, in
// what order, and what was durably recorded between them. So the doubles here
// are built around exactly those two things: one modelled host that records
// every operation in order, and one store that can be made to fail at an exact
// point.
//
// The host double is a single object that is BOTH the read-only runtime and the
// rollback capability, unlike the recreation tests where they are separate.
// That is deliberate: a rollback verifies the container it just moved, so a
// runtime that did not see the move would verify a container that only exists
// in the fixture. Two doubles that disagree about the host would let a rollback
// "pass" verification it should have failed.

const (
	rbExecutionID   = "exec_00112233445566778899"
	rbContainerName = "web"
	rbOldImage      = "nginx:1.27.0"
	rbNewImage      = "nginx:1.27.1"
)

var (
	rbOriginalID    = docker.FakeContainerID(11)
	rbReplacementID = docker.FakeContainerID(12)
	rbStrangerID    = docker.FakeContainerID(13)

	rbOldImageID = "sha256:" + strings.Repeat("a", 64)
	rbNewImageID = "sha256:" + strings.Repeat("b", 64)

	// rbParkedName is where the recreation left the original.
	rbParkedName = rbContainerName + domain.ParkedNameSuffix + rbExecutionID
)

// errDaemonGone stands in for an unreachable daemon.
var errRollbackDaemonGone = docker.ErrUnreachable

// ------------------------------------------------------------ the host --

// rbContainer is one container on the modelled host.
type rbContainer struct {
	detail domain.ContainerDetail
	// healthOnStart is the health the container reports once started. Lets a
	// test model an original that comes back unhealthy.
	healthOnStart domain.HealthState
	// exitOnStart models a container that comes up and immediately falls over.
	exitOnStart bool
	// onStart runs once the container has been started, so a test can model a
	// container that comes back configured differently from how it went in.
	onStart func(*rbContainer)
}

// rollbackHost is the modelled Docker host.
//
// Implements docker.Runtime by embedding the read-only fake for the methods
// rollback never varies, and docker.ContainerRollbacker for the four mutations.
type rollbackHost struct {
	*docker.Fake

	mu         sync.Mutex
	containers map[string]*rbContainer

	// Injected failures, each failing the NEXT call to that operation.
	pingErr    error
	listErr    error
	inspectErr error
	stopErr    error
	parkErr    error
	restoreErr error
	startErr   error

	// ops records every rollback mutation, in order. Most assertions in this
	// file are statements about this slice.
	ops []string
	// reads counts every call that reaches the modelled daemon, mutating or
	// not. A read endpoint that can drive these is a denial-of-service
	// amplifier pointed at a privileged socket, so one test asserts it is zero.
	reads int
	// delay is slept inside every mutation, so a test can observe one in flight.
	delay time.Duration
}

func newRollbackHost() *rollbackHost {
	return &rollbackHost{
		Fake:       docker.NewFake(),
		containers: make(map[string]*rbContainer),
	}
}

func (h *rollbackHost) add(c *rbContainer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.containers[c.detail.Overview.ID] = c
}

// with runs fn against one modelled container under the host lock.
func (h *rollbackHost) with(id string, fn func(*rbContainer)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.containers[id]; ok {
		fn(c)
	}
}

// remove takes a container off the modelled host.
func (h *rollbackHost) remove(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.containers, id)
}

// nameOf reports a container's current name.
func (h *rollbackHost) nameOf(id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.containers[id]
	if !ok {
		return ""
	}
	return domain.NormaliseContainerName(c.detail.Overview.Name)
}

// running reports whether a container is running.
func (h *rollbackHost) running(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.containers[id]
	return ok && c.detail.State.Running
}

// present reports whether a container is still on the modelled host.
func (h *rollbackHost) present(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.containers[id]
	return ok
}

// operations returns the recorded mutations in order.
func (h *rollbackHost) operations() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ops...)
}

// daemonCalls returns how many calls have reached the modelled daemon.
func (h *rollbackHost) daemonCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reads + len(h.ops)
}

// setErr sets an injected failure under the lock.
func (h *rollbackHost) setErr(field *error, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	*field = err
}

// ---- docker.Runtime -----------------------------------------------------

func (h *rollbackHost) Ping(ctx context.Context) (docker.Info, error) {
	h.mu.Lock()
	h.reads++
	err := h.pingErr
	h.mu.Unlock()
	if err != nil {
		return docker.Info{}, err
	}
	return docker.Info{}, ctx.Err()
}

func (h *rollbackHost) ListContainers(ctx context.Context) ([]domain.ContainerSummary, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.reads++

	if h.listErr != nil {
		return nil, h.listErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	out := make([]domain.ContainerSummary, 0, len(h.containers))
	for _, c := range h.containers {
		out = append(out, c.detail.Overview)
	}
	return out, nil
}

func (h *rollbackHost) InspectContainer(ctx context.Context, id string) (*docker.Inspection, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.reads++

	if h.inspectErr != nil {
		return nil, h.inspectErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c, ok := h.containers[id]
	if !ok {
		return nil, nil
	}
	// Copied, so a caller holding the inspection does not observe later
	// mutations. The real adapter returns a snapshot too.
	detail := c.detail
	return &docker.Inspection{Detail: detail}, nil
}

// ---- docker.ContainerRollbacker -----------------------------------------

func (h *rollbackHost) StopReplacement(ctx context.Context, request docker.RollbackStopRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := h.pause(ctx); err != nil {
		return err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.ops = append(h.ops, "stop:"+request.ReplacementID)
	if h.stopErr != nil {
		return h.stopErr
	}

	c, ok := h.containers[request.ReplacementID]
	if !ok {
		return docker.ErrContainerVanished
	}
	c.detail.State.Running = false
	c.detail.State.State = domain.StateExited
	c.detail.Overview.State = domain.StateExited
	return nil
}

func (h *rollbackHost) ParkReplacement(ctx context.Context, request docker.RollbackParkRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := h.pause(ctx); err != nil {
		return err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.ops = append(h.ops, "park:"+request.ParkedName)
	if h.parkErr != nil {
		return h.parkErr
	}
	return h.rename(request.ReplacementID, request.ParkedName)
}

func (h *rollbackHost) RestoreOriginalName(
	ctx context.Context, request docker.RollbackRestoreRequest,
) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := h.pause(ctx); err != nil {
		return err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.ops = append(h.ops, "restore:"+request.Name)
	if h.restoreErr != nil {
		return h.restoreErr
	}
	return h.rename(request.OriginalID, request.Name)
}

func (h *rollbackHost) StartOriginal(ctx context.Context, request docker.RollbackStartRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := h.pause(ctx); err != nil {
		return err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.ops = append(h.ops, "start:"+request.OriginalID)
	if h.startErr != nil {
		return h.startErr
	}

	c, ok := h.containers[request.OriginalID]
	if !ok {
		return docker.ErrContainerVanished
	}
	if c.exitOnStart {
		c.detail.State.Running = false
		c.detail.State.State = domain.StateExited
		c.detail.Overview.State = domain.StateExited
		return nil
	}
	c.detail.State.Running = true
	c.detail.State.State = domain.StateRunning
	c.detail.Overview.State = domain.StateRunning
	if c.healthOnStart != "" {
		c.detail.State.Health = c.healthOnStart
		c.detail.Overview.Health = c.healthOnStart
	}
	if c.onStart != nil {
		c.onStart(c)
	}
	return nil
}

// rename moves a container to a new name, refusing a collision exactly as the
// daemon does. Called with the lock held.
func (h *rollbackHost) rename(id, name string) error {
	c, ok := h.containers[id]
	if !ok {
		return docker.ErrContainerVanished
	}
	for otherID, other := range h.containers {
		if otherID == id {
			continue
		}
		if domain.NormaliseContainerName(other.detail.Overview.Name) == name {
			return docker.ErrNameConflict
		}
	}
	c.detail.Overview.Name = name
	return nil
}

// pause honours the configured delay and cancellation outside the lock.
func (h *rollbackHost) pause(ctx context.Context) error {
	h.mu.Lock()
	delay := h.delay
	h.mu.Unlock()

	if delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return ctx.Err()
}

var (
	_ docker.Runtime             = (*rollbackHost)(nil)
	_ docker.ContainerRollbacker = (*rollbackHost)(nil)
)

// ----------------------------------------------------------- the store --

// fakeRollbackStore is an in-memory rollback repository.
//
// It reproduces the CONDITIONAL semantics of the real one, because those are
// what the pipeline's safety rests on: a transition that names From states only
// applies from those states, and nothing moves a terminal record.
type fakeRollbackStore struct {
	mu sync.Mutex

	records map[string]*domain.Rollback
	order   []string
	events  map[string][]domain.RollbackEvent

	createErr  error
	advanceErr error
	// advanceErrTo fails only the transition INTO this state, which places a
	// persistence failure at an exact point without racing the pipeline.
	advanceErrTo domain.RollbackState
	// checkpointErrAt fails the checkpoint carrying this value.
	checkpointErrAt domain.RollbackCheckpoint

	// checkpoints records every checkpoint written, in order.
	checkpoints []domain.RollbackCheckpoint
	// states records every state written, in order.
	states []domain.RollbackState
}

func newFakeRollbackStore() *fakeRollbackStore {
	return &fakeRollbackStore{
		records: make(map[string]*domain.Rollback),
		events:  make(map[string][]domain.RollbackEvent),
	}
}

func (f *fakeRollbackStore) Create(
	_ context.Context, rollback domain.Rollback, _ time.Time,
) (domain.Rollback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.createErr != nil {
		return domain.Rollback{}, f.createErr
	}
	for _, existing := range f.records {
		if rollback.RequestKey != "" && existing.RequestKey == rollback.RequestKey {
			return *existing, nil
		}
		if existing.State.Active() && existing.ContainerName == rollback.ContainerName {
			return domain.Rollback{}, store.ErrRollbackActive
		}
		if existing.State == domain.RollbackSucceeded &&
			existing.ExecutionID == rollback.ExecutionID {
			return domain.Rollback{}, store.ErrRollbackAlreadySucceeded
		}
	}
	if rollback.State == "" {
		rollback.State = domain.RollbackQueued
	}

	stored := rollback
	f.records[rollback.RollbackID] = &stored
	f.order = append(f.order, rollback.RollbackID)
	f.states = append(f.states, stored.State)
	return stored, nil
}

func (f *fakeRollbackStore) Advance(
	_ context.Context, change store.RollbackChange, now time.Time,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.advanceErr != nil {
		return false, f.advanceErr
	}
	if f.advanceErrTo != "" && change.To == f.advanceErrTo {
		return false, errors.New("injected persistence failure")
	}

	record, ok := f.records[change.RollbackID]
	if !ok {
		return false, store.ErrNotFound
	}
	// Nothing moves a terminal record, and a From list restricts further.
	if record.State.Terminal() {
		return false, nil
	}
	if len(change.From) > 0 {
		matched := false
		for _, from := range change.From {
			if record.State == from {
				matched = true
				break
			}
		}
		if !matched {
			return false, nil
		}
	}

	record.State = change.To
	if change.Checkpoint != "" {
		record.Checkpoint = change.Checkpoint
	}
	if change.Failure != "" {
		record.Failure = change.Failure
	}
	if change.Refusal != "" {
		record.Refusal = change.Refusal
	}
	if change.Message != "" {
		record.Message = change.Message
	}
	if change.ReplacementParkedName != "" {
		record.ReplacementParkedName = change.ReplacementParkedName
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

	f.states = append(f.states, change.To)
	f.events[change.RollbackID] = append(f.events[change.RollbackID], domain.RollbackEvent{
		RollbackID: change.RollbackID,
		State:      change.To,
		Checkpoint: record.Checkpoint,
		Detail:     change.Detail,
		At:         now,
	})
	return true, nil
}

func (f *fakeRollbackStore) Checkpoint(
	_ context.Context, write store.RollbackCheckpointWrite, now time.Time,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.checkpointErrAt != "" && write.Checkpoint == f.checkpointErrAt {
		return false, errors.New("injected checkpoint persistence failure")
	}

	record, ok := f.records[write.RollbackID]
	if !ok {
		return false, store.ErrNotFound
	}
	if record.State.Terminal() {
		return false, nil
	}

	record.Checkpoint = write.Checkpoint
	if write.ReplacementParkedName != "" {
		record.ReplacementParkedName = write.ReplacementParkedName
	}
	if write.MarkMutated && record.MutatedAt == nil {
		at := now
		record.MutatedAt = &at
	}

	f.checkpoints = append(f.checkpoints, write.Checkpoint)
	f.events[write.RollbackID] = append(f.events[write.RollbackID], domain.RollbackEvent{
		RollbackID: write.RollbackID,
		State:      record.State,
		Checkpoint: write.Checkpoint,
		Detail:     write.Detail,
		At:         now,
	})
	return true, nil
}

func (f *fakeRollbackStore) Get(_ context.Context, rollbackID string) (domain.Rollback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	record, ok := f.records[rollbackID]
	if !ok {
		return domain.Rollback{}, store.ErrNotFound
	}
	return *record, nil
}

func (f *fakeRollbackStore) List(
	_ context.Context, filter store.RollbackFilter,
) ([]domain.Rollback, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []domain.Rollback
	for _, id := range f.order {
		record := f.records[id]
		if filter.ExecutionID != "" && record.ExecutionID != filter.ExecutionID {
			continue
		}
		if filter.ActiveOnly && !record.State.Active() {
			continue
		}
		out = append(out, *record)
	}
	return out, len(out), nil
}

func (f *fakeRollbackStore) Events(
	_ context.Context, rollbackID string, _ int,
) ([]domain.RollbackEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.RollbackEvent(nil), f.events[rollbackID]...), nil
}

func (f *fakeRollbackStore) ActiveCount(_ context.Context, excluding string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var active int
	for id, record := range f.records {
		if id == excluding {
			continue
		}
		if record.State.Active() {
			active++
		}
	}
	return active, nil
}

func (f *fakeRollbackStore) ActiveForContainer(
	_ context.Context, containerName, excluding string,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for id, record := range f.records {
		if id == excluding {
			continue
		}
		if record.State.Active() && record.ContainerName == containerName {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeRollbackStore) SucceededForExecution(
	_ context.Context, executionID string,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, record := range f.records {
		if record.State == domain.RollbackSucceeded && record.ExecutionID == executionID {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeRollbackStore) ByRequestKey(
	_ context.Context, key string,
) (domain.Rollback, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if key == "" {
		return domain.Rollback{}, false, nil
	}
	for _, id := range f.order {
		if record := f.records[id]; record.RequestKey == key {
			return *record, true, nil
		}
	}
	return domain.Rollback{}, false, nil
}

func (f *fakeRollbackStore) Claimable(_ context.Context, limit int) ([]domain.Rollback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []domain.Rollback
	for _, id := range f.order {
		if len(out) >= limit {
			break
		}
		if record := f.records[id]; record.State == domain.RollbackQueued {
			out = append(out, *record)
		}
	}
	return out, nil
}

func (f *fakeRollbackStore) Interrupted(_ context.Context, limit int) ([]domain.Rollback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []domain.Rollback
	for _, id := range f.order {
		if len(out) >= limit {
			break
		}
		record := f.records[id]
		if record.State.Active() && record.State != domain.RollbackQueued {
			out = append(out, *record)
		}
	}
	return out, nil
}

func (f *fakeRollbackStore) Summary(context.Context) (domain.RollbackSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var summary domain.RollbackSummary
	for _, record := range f.records {
		summary.Total++
		switch {
		case record.State.Active():
			summary.Active++
		case record.State == domain.RollbackSucceeded:
			summary.Succeeded++
		case record.State == domain.RollbackFailed:
			summary.Failed++
			if record.Checkpoint.HostChanged() {
				summary.NeedsAttention++
			}
		}
	}
	return summary, nil
}

func (f *fakeRollbackStore) ExpireStale(
	_ context.Context, now time.Time, _ int,
) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var expired int64
	for _, record := range f.records {
		if record.State == domain.RollbackQueued && record.ExpiresAt.Before(now) {
			record.State = domain.RollbackExpired
			expired++
		}
	}
	return expired, nil
}

func (f *fakeRollbackStore) Prune(context.Context, time.Time, int) (int64, error) { return 0, nil }

// checkpointsWritten returns the durable checkpoints, in order.
func (f *fakeRollbackStore) checkpointsWritten() []domain.RollbackCheckpoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.RollbackCheckpoint(nil), f.checkpoints...)
}

// statesWritten returns every state the record moved through, in order.
func (f *fakeRollbackStore) statesWritten() []domain.RollbackState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.RollbackState(nil), f.states...)
}

// seed places a record directly, for tests about restart recovery and conflict.
func (f *fakeRollbackStore) seed(rollback domain.Rollback) {
	f.mu.Lock()
	defer f.mu.Unlock()

	stored := rollback
	f.records[rollback.RollbackID] = &stored
	f.order = append(f.order, rollback.RollbackID)
}

var _ service.RollbackStore = (*fakeRollbackStore)(nil)

// -------------------------------------------------------- the evidence --

// fakeRollbackEvidence is the execution record a rollback is derived from.
type fakeRollbackEvidence struct {
	mu sync.Mutex

	execution    domain.Execution
	executionErr error

	activeExecution bool
	inventoryAge    time.Duration
	inventoryKnown  bool
}

func (e *fakeRollbackEvidence) Execution(
	_ context.Context, executionID string,
) (domain.Execution, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.executionErr != nil {
		return domain.Execution{}, e.executionErr
	}
	if e.execution.ExecutionID != executionID {
		return domain.Execution{}, store.ErrNotFound
	}
	return e.execution, nil
}

func (e *fakeRollbackEvidence) ExecutionActiveForContainer(
	context.Context, string,
) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.activeExecution, nil
}

func (e *fakeRollbackEvidence) InventoryAge(
	context.Context, time.Time,
) (time.Duration, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inventoryAge, e.inventoryKnown, nil
}

// edit mutates the evidence under its lock.
func (e *fakeRollbackEvidence) edit(fn func(*fakeRollbackEvidence)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fn(e)
}

var _ service.RollbackEvidence = (*fakeRollbackEvidence)(nil)

// ----------------------------------------------------------- fixtures --

// rbOriginalDetail is the preserved original as the recreation left it:
// stopped, parked under a derived name, still on its old image.
func rbOriginalDetail() domain.ContainerDetail {
	detail := domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			ID:            rbOriginalID,
			ShortID:       domain.ShortenID(rbOriginalID),
			Name:          rbParkedName,
			Image:         domain.ParseImageRef(rbOldImage),
			ImageID:       rbOldImageID,
			State:         domain.StateExited,
			Health:        domain.HealthNone,
			Present:       true,
			RestartPolicy: domain.RestartPolicy{Name: "unless-stopped"},
		},
		State: domain.StateDetail{
			State: domain.StateExited, Running: false, Health: domain.HealthNone,
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
	return detail
}

// rbReplacementDetail is the container the recreation created, holding the
// production name and running the new image.
func rbReplacementDetail() domain.ContainerDetail {
	detail := rbOriginalDetail()
	detail.Overview.ID = rbReplacementID
	detail.Overview.ShortID = domain.ShortenID(rbReplacementID)
	detail.Overview.Name = rbContainerName
	detail.Overview.Image = domain.ParseImageRef(rbNewImage)
	detail.Overview.ImageID = rbNewImageID
	detail.Overview.State = domain.StateRunning
	detail.Overview.Health = domain.HealthHealthy
	detail.State = domain.StateDetail{
		State: domain.StateRunning, Running: true, Health: domain.HealthHealthy,
	}
	return detail
}

// rbExecution is a recreation that completed and left an arrangement to undo.
func rbExecution(now time.Time) domain.Execution {
	mutated := now.Add(-time.Hour)
	completed := now.Add(-time.Hour).Add(time.Minute)

	return domain.Execution{
		ExecutionID:   rbExecutionID,
		ContainerID:   rbOriginalID,
		ContainerName: rbContainerName,
		OldImage:      rbOldImage,
		OldImageID:    rbOldImageID,
		Target:        domain.ExecutionTarget{Reference: rbNewImage},
		State:         domain.ExecutionSucceeded,
		Checkpoint:    domain.CheckpointReplacementVerified,
		ReplacementID: rbReplacementID,
		ParkedName:    rbParkedName,
		RequestedAt:   now.Add(-2 * time.Hour),
		MutatedAt:     &mutated,
		CompletedAt:   &completed,
	}
}

// --------------------------------------------------------------- harness --

type rbHarness struct {
	service  *service.RollbackService
	store    *fakeRollbackStore
	evidence *fakeRollbackEvidence
	host     *rollbackHost
	// lineage is what the container FOLLOWS. A rollback must put the running
	// digest back without disturbing the tracking reference.
	lineage *fakeLineageStore

	base  time.Time
	start time.Time
	skew  atomic.Int64
}

func (h *rbHarness) now() time.Time {
	return h.base.Add(time.Since(h.start)).Add(time.Duration(h.skew.Load()))
}

// advance moves the harness clock forward.
func (h *rbHarness) advance(by time.Duration) { h.skew.Add(int64(by)) }

// newRollbackHarness builds a service over a world in which a rollback should
// succeed.
func newRollbackHarness(t *testing.T, tune ...func(*rbHarness)) *rbHarness {
	t.Helper()

	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	host := newRollbackHost()
	host.add(&rbContainer{detail: rbOriginalDetail(), healthOnStart: domain.HealthHealthy})
	host.add(&rbContainer{detail: rbReplacementDetail()})

	harness := &rbHarness{
		store: newFakeRollbackStore(),
		evidence: &fakeRollbackEvidence{
			execution:      rbExecution(base),
			inventoryAge:   time.Minute,
			inventoryKnown: true,
		},
		host:    host,
		lineage: newFakeLineageStore(),
		base:    base,
		start:   time.Now(),
	}
	for _, apply := range tune {
		apply(harness)
	}

	// The same installation key the snapshots use: preservation compares
	// sensitive values as keyed digests, and no hasher makes the comparison
	// unverifiable, which fails closed.
	key, err := service.LoadSecretKey(service.SecretKeyOptions{
		GeneratePath: filepath.Join(t.TempDir(), "secret.key"),
	})
	if err != nil {
		t.Fatalf("load secret key: %v", err)
	}

	harness.service = service.NewRollbackService(service.RollbackOptions{
		Store:      harness.store,
		Evidence:   harness.evidence,
		Runtime:    harness.host,
		Rollbacker: harness.host,
		Lineage:    harness.lineage,
		Hasher:     service.NewHasher(key),
		Config: config.Rollback{
			Enabled:              true,
			MaxConcurrent:        1,
			RequestTTL:           10 * time.Minute,
			StartupTimeout:       2 * time.Second,
			StabilityPeriod:      10 * time.Millisecond,
			HealthPollInterval:   time.Millisecond,
			StopTimeout:          time.Second,
			InventoryFreshness:   15 * time.Minute,
			SweepInterval:        time.Hour,
			PruneInterval:        time.Hour,
			MaxEventsPerRollback: 200,
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    harness.now,
	})
	return harness
}

// request asks for a rollback, failing the test if it is refused.
func (h *rbHarness) request(t *testing.T) domain.Rollback {
	t.Helper()

	rollback, err := h.service.Request(context.Background(),
		service.RollbackRequest{
			ExecutionID: rbExecutionID,
			RequestedBy: domain.Requester{UserID: "usr_rollback", Username: "operator"},
		})
	if err != nil {
		t.Fatalf("rollback refused: %v", err)
	}
	return rollback
}

// refusal asks for a rollback and returns the refusal, failing if there is none.
func (h *rbHarness) refusal(t *testing.T) domain.RollbackRefusal {
	t.Helper()

	_, err := h.service.Request(context.Background(),
		service.RollbackRequest{ExecutionID: rbExecutionID})
	if err == nil {
		t.Fatal("the rollback was accepted; want a refusal")
	}

	var refused service.RollbackRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return refused.Refusal
}

// runOnce drives the worker until the rollback reaches a terminal state.
func (h *rbHarness) runOnce(t *testing.T, rollback domain.Rollback) domain.Rollback {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.service.Run(ctx)
	}()

	deadline := time.After(15 * time.Second)
	for {
		record, err := h.store.Get(context.Background(), rollback.RollbackID)
		if err == nil && record.State.Terminal() {
			cancel()
			<-done
			return record
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("the rollback did not finish; last state %q", record.State)
		case <-time.After(time.Millisecond):
		}
	}
}
