package service_test

import (
	"context"
	"sync"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Automatic restoration of a failed MANUAL update.
//
// # The product rule
//
// A person asked for an update; the replacement failed after the original was
// parked. Nothing about "a person asked" makes leaving the service down the
// right answer, so the execution service asks the rollback service to put the
// original back -- through the same request, the same preflight, and the same
// checkpointed pipeline an operator's own rollback uses. It never moves a
// container itself, and the rollback service may still refuse.
//
// The outcome is RECORDED on the execution: requested, restored, failed,
// refused, or unavailable. An operator must be able to tell "the service is
// back" from "the service is still down" without opening anything else.

// fakeRestorer stands in for the rollback service's request surface.
type fakeRestorer struct {
	mu sync.Mutex

	enabled  bool
	refusal  domain.RollbackRefusal
	err      error
	requests []service.RollbackRequest
	// rollbacks is keyed by request key, exactly as the real service's
	// idempotency lookup is.
	rollbacks map[string]domain.Rollback
}

func newFakeRestorer() *fakeRestorer {
	return &fakeRestorer{enabled: true, rollbacks: map[string]domain.Rollback{}}
}

func (f *fakeRestorer) Enabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled
}

func (f *fakeRestorer) Request(_ context.Context, request service.RollbackRequest) (domain.Rollback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, request)
	if f.err != nil {
		return domain.Rollback{}, f.err
	}
	if existing, ok := f.rollbacks[request.RequestKey]; ok && request.RequestKey != "" {
		return existing, nil
	}
	if f.refusal != domain.RollbackRefusalNone {
		return domain.Rollback{}, service.RollbackRefusedError{Refusal: f.refusal}
	}
	rollback := domain.Rollback{
		RollbackID:  domain.NewRollbackID(),
		ExecutionID: request.ExecutionID,
		RequestKey:  request.RequestKey,
		RequestedBy: request.RequestedBy,
		State:       domain.RollbackQueued,
	}
	f.rollbacks[request.RequestKey] = rollback
	return rollback, nil
}

func (f *fakeRestorer) ByRequestKey(_ context.Context, key string) (domain.Rollback, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rollback, ok := f.rollbacks[key]
	return rollback, ok, nil
}

// settle moves the rollback behind a key to a terminal state.
func (f *fakeRestorer) settle(key string, state domain.RollbackState, failure domain.RollbackFailure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rollback := f.rollbacks[key]
	rollback.State = state
	rollback.Failure = failure
	f.rollbacks[key] = rollback
}

func (f *fakeRestorer) requested() []service.RollbackRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]service.RollbackRequest(nil), f.requests...)
}

// failedManualUpdate runs a manual recreation whose create fails after the
// park, with a restorer wired.
func failedManualUpdate(t *testing.T, tune ...func(*execHarness)) (*execHarness, domain.Execution) {
	t.Helper()
	harness := newExecHarness(t, append([]func(*execHarness){func(h *execHarness) {
		h.restorer = newFakeRestorer()
		h.mutator.CreateErr = docker.ErrMutationFailed
	}}, tune...)...)
	final := harness.runOnce(t, harness.request(t))
	if final.Failure != domain.ExecutionFailureCreate {
		t.Fatalf("failure %q, want create", final.Failure)
	}
	return harness, final
}

func TestAFailedManualUpdateRequestsARestoreWithoutAnyoneAsking(t *testing.T) {
	harness, final := failedManualUpdate(t)

	requests := harness.restorer.requested()
	if len(requests) != 1 {
		t.Fatalf("%d rollback requests, want 1", len(requests))
	}
	if requests[0].ExecutionID != final.ExecutionID {
		t.Errorf("the restore names execution %q, want %q", requests[0].ExecutionID, final.ExecutionID)
	}
	if requests[0].RequestKey != domain.ManualRestoreRequestKey(final.ExecutionID) {
		t.Errorf("request key %q, want the manual-restore key", requests[0].RequestKey)
	}
	if requests[0].RequestedBy != final.RequestedBy {
		t.Errorf("the restore is attributed to %+v, want the operator who asked for the update %+v",
			requests[0].RequestedBy, final.RequestedBy)
	}
	if final.Restore.State != domain.RestoreRequested || final.Restore.RollbackID == "" {
		t.Fatalf("restore = %+v, want requested with a rollback id", final.Restore)
	}
}

func TestARestoreThatSucceedsMarksTheServiceRestored(t *testing.T) {
	harness, final := failedManualUpdate(t)
	key := domain.ManualRestoreRequestKey(final.ExecutionID)
	harness.restorer.settle(key, domain.RollbackSucceeded, domain.RollbackFailureNone)

	harness.service.AdvanceRestores(context.Background())

	settled, _ := harness.store.Get(context.Background(), final.ExecutionID)
	if settled.Restore.State != domain.RestoreRestored {
		t.Fatalf("restore state %q, want restored", settled.Restore.State)
	}
	if settled.State != domain.ExecutionFailed || settled.Failure != domain.ExecutionFailureCreate {
		t.Errorf("the update's own outcome changed to %q/%q", settled.State, settled.Failure)
	}
	if settled.Recovery == nil || settled.Recovery.ServiceInterrupted {
		t.Errorf("the plan still says the service is down: %+v", settled.Recovery)
	}
	if settled.Recovery.Urgency == domain.RecoveryUrgent {
		t.Error("a restored update is still urgent")
	}
}

func TestARestoreThatFailsSaysSoAndKeepsTheServiceDown(t *testing.T) {
	harness, final := failedManualUpdate(t)
	key := domain.ManualRestoreRequestKey(final.ExecutionID)
	harness.restorer.settle(key, domain.RollbackFailed, domain.RollbackFailureStart)

	harness.service.AdvanceRestores(context.Background())

	settled, _ := harness.store.Get(context.Background(), final.ExecutionID)
	if settled.Restore.State != domain.RestoreFailed {
		t.Fatalf("restore state %q, want failed", settled.Restore.State)
	}
	if settled.Restore.Detail != domain.RollbackFailureStart.Explain() {
		t.Errorf("detail %q, want the rollback's own explanation", settled.Restore.Detail)
	}
	if settled.Recovery == nil || !settled.Recovery.ServiceInterrupted {
		t.Error("the plan no longer says the service is down")
	}
}

func TestARefusedRestoreIsRecordedWithItsReason(t *testing.T) {
	harness, final := failedManualUpdate(t, func(h *execHarness) {
		h.restorer.refusal = domain.RollbackRefusalNameUnavailable
	})
	if final.Restore.State != domain.RestoreRefused {
		t.Fatalf("restore state %q, want refused", final.Restore.State)
	}
	if final.Restore.Detail != domain.RollbackRefusalNameUnavailable.Explain() {
		t.Errorf("detail %q, want the refusal's explanation", final.Restore.Detail)
	}
	if final.Restore.RollbackID != "" {
		t.Errorf("a refused restore carries rollback id %q", final.Restore.RollbackID)
	}
	if final.Recovery == nil || !final.Recovery.ServiceInterrupted {
		t.Error("the plan no longer says the service is down")
	}
	if len(harness.restorer.requested()) != 1 {
		t.Errorf("%d requests, want 1", len(harness.restorer.requested()))
	}
}

func TestARestoreIsUnavailableWithoutTheRollbackCapability(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.CreateErr = docker.ErrMutationFailed
	})
	final := harness.runOnce(t, harness.request(t))
	if final.Restore.State != domain.RestoreUnavailable {
		t.Fatalf("restore state %q, want unavailable", final.Restore.State)
	}
	if final.Restore.Detail == "" {
		t.Error("an unavailable restore does not say why")
	}
}

func TestRestoreOnFailureOffLeavesTheRecordAlone(t *testing.T) {
	off := false
	harness := newExecHarness(t, func(h *execHarness) {
		h.restorer = newFakeRestorer()
		h.restoreOnFailure = &off
		h.mutator.CreateErr = docker.ErrMutationFailed
	})
	final := harness.runOnce(t, harness.request(t))
	if final.Restore.State != "" {
		t.Fatalf("restore state %q with the setting off", final.Restore.State)
	}
	if len(harness.restorer.requested()) != 0 {
		t.Error("a rollback was requested with the setting off")
	}
}

func TestAnAutomaticExecutionIsLeftToTheAutomationFollower(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.restorer = newFakeRestorer()
		h.mutator.CreateErr = docker.ErrMutationFailed
	})
	execution, err := harness.service.Request(context.Background(), service.ExecutionRequest{
		AcquisitionID: execAcquisitionID,
		RequestKey:    domain.AutomationRequestKeyPrefix + "execute:" + execAcquisitionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	final := harness.runOnce(t, execution)
	if final.Restore.State != "" {
		t.Fatalf("restore state %q on an automatic execution", final.Restore.State)
	}
	if len(harness.restorer.requested()) != 0 {
		t.Error("the execution service requested a rollback the follower owns")
	}
}

func TestAFailureBeforeTheMutationPointRequestsNoRestore(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.restorer = newFakeRestorer()
		h.mutator.CaptureErr = docker.ErrCaptureFailed
	})
	final := harness.runOnce(t, harness.request(t))
	if final.Failure != domain.ExecutionFailureCapture {
		t.Fatalf("failure %q, want capture", final.Failure)
	}
	if final.Restore.State != "" || len(harness.restorer.requested()) != 0 {
		t.Error("a restore was attempted for a failure that changed nothing")
	}
}

func TestARestoreIsRequestedOnceHoweverOftenTheSweepRuns(t *testing.T) {
	harness, _ := failedManualUpdate(t)
	for i := 0; i < 3; i++ {
		harness.service.AdvanceRestores(context.Background())
	}
	if got := len(harness.restorer.requested()); got != 1 {
		t.Fatalf("%d requests after repeated sweeps, want 1", got)
	}
}

// TestAPendingRestoreWhoseRollbackNeverLandedIsRequestedAgain: the process
// died between recording "requested" and the rollback service accepting the
// request. The sweep finds no rollback under the key and asks again, with the
// same key, so the outcome converges rather than being lost.
func TestAPendingRestoreWhoseRollbackNeverLandedIsRequestedAgain(t *testing.T) {
	harness, final := failedManualUpdate(t)
	key := domain.ManualRestoreRequestKey(final.ExecutionID)
	harness.restorer.mu.Lock()
	delete(harness.restorer.rollbacks, key)
	harness.restorer.mu.Unlock()

	harness.service.AdvanceRestores(context.Background())

	requests := harness.restorer.requested()
	if len(requests) != 2 || requests[1].RequestKey != key {
		t.Fatalf("requests = %+v, want a second request under the same key", requests)
	}
}

func TestRestartRecoverySettlesAndThenRestores(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.restorer = newFakeRestorer()
	})
	interrupted := harness.request(t)
	harness.store.seedState(interrupted.ExecutionID, domain.ExecutionCreating,
		domain.CheckpointOriginalParked, "web.hm-old-"+interrupted.ExecutionID)

	harness.recover(t)

	settled, _ := harness.store.Get(context.Background(), interrupted.ExecutionID)
	if settled.Failure != domain.ExecutionFailureInterrupted {
		t.Fatalf("failure %q, want interrupted", settled.Failure)
	}
	if settled.Restore.State != domain.RestoreRequested {
		t.Fatalf("restore state %q after the recovery pass, want requested", settled.Restore.State)
	}
}
