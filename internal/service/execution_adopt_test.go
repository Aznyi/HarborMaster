package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Adopting a replacement the record never named.
//
// # The window
//
// The original is parked. ContainerCreate is issued. The daemon completes it --
// and HarborMaster never learns the id: the client gave up first, the
// checkpoint write failed, or the process died. A container now holds the
// production name and no record names it, so nothing can stop it, park it, or
// roll it back, and the rollback preflight rightly treats it as a stranger.
//
// # The evidence
//
// The replacement was created with two labels only HarborMaster writes: the
// execution that created it and the original it replaces. Together with the
// production name and the approved image, they identify it. Anything less is a
// stranger and is never touched.

// adoptableReplacement is the container a create produced for execHarness's
// recreation: the production name, the approved image, and the ownership
// labels the adapter writes.
func adoptableReplacement(executionID string) domain.ContainerDetail {
	detail := execReplacementDetail()
	detail.Labels = append([]domain.Label(nil), detail.Labels...)
	detail.Labels = append(detail.Labels,
		domain.Label{Key: domain.LabelExecutionOwner, Value: executionID, Source: domain.LabelSourceHarborMaster},
		domain.Label{Key: domain.LabelReplacementOf, Value: execContainerID, Source: domain.LabelSourceHarborMaster},
	)
	return detail
}

// TestACreateThatLandedAfterTheClientGaveUpIsAdopted is the in-pipeline case:
// the daemon completed the create, the adapter reported a failure, and the
// container carries this execution's labels.
func TestACreateThatLandedAfterTheClientGaveUpIsAdopted(t *testing.T) {
	var executionID string
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.CreateLandsButErrs = docker.ErrMutationFailed
	})
	execution := harness.request(t)
	executionID = execution.ExecutionID

	// The read-only runtime is what the adoption reads. It reports the
	// replacement the create produced, under the production name, with the
	// labels the adapter wrote.
	harness.runtime.Containers = append(harness.runtime.Containers,
		domain.ContainerSummary{ID: execReplacementID, Name: "/web", Present: true})
	harness.runtime.Inspections[execReplacementID] = &docker.Inspection{Detail: adoptableReplacement(executionID)}

	final := harness.runOnce(t, execution)

	if final.State != domain.ExecutionFailed || final.Failure != domain.ExecutionFailureCreate {
		t.Fatalf("state %q/%q, want failed/create", final.State, final.Failure)
	}
	if final.ReplacementID != execReplacementID {
		t.Fatalf("replacement id %q, want the adopted container %q", final.ReplacementID, execReplacementID)
	}
	// Adopted, and then treated like any failed replacement: stopped and moved
	// off the production name, so nothing unverified serves under it.
	if name := harness.mutator.NameOf(execReplacementID); !strings.Contains(name, domain.QuarantineNameSuffix) {
		t.Errorf("the adopted replacement is named %q, want it quarantined", name)
	}
	if final.Checkpoint != domain.CheckpointReplacementQuarantined {
		t.Errorf("checkpoint %q, want replacementQuarantined", final.Checkpoint)
	}
	if !harness.mutator.Present(execContainerID) {
		t.Fatal("the original was removed")
	}
	// The record is now one a rollback can act on.
	if !domain.RollbackSufficientCheckpoint(final.Checkpoint) {
		t.Error("the adopted record is not rollbackable")
	}
}

// TestAStrangerHoldingTheProductionNameIsNeverAdopted: same failure, but the
// container under the name carries no ownership labels. It is left exactly
// where it is, and the record says no replacement was recorded.
func TestAStrangerHoldingTheProductionNameIsNeverAdopted(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.CreateLandsButErrs = docker.ErrMutationFailed
	})
	execution := harness.request(t)

	stranger := execReplacementDetail()
	stranger.Labels = nil
	harness.runtime.Containers = append(harness.runtime.Containers,
		domain.ContainerSummary{ID: execReplacementID, Name: "/web", Present: true})
	harness.runtime.Inspections[execReplacementID] = &docker.Inspection{Detail: stranger}

	final := harness.runOnce(t, execution)

	if final.ReplacementID != "" {
		t.Fatalf("a container with no ownership labels was adopted as %q", final.ReplacementID)
	}
	if final.Checkpoint != domain.CheckpointOriginalParked {
		t.Errorf("checkpoint %q, want originalParked", final.Checkpoint)
	}
	for _, call := range harness.mutator.Calls {
		if call.ContainerID == execReplacementID && call.Op != "create" {
			t.Errorf("the stranger was touched: %+v", call)
		}
	}
	if final.Recovery == nil || !strings.Contains(strings.ToLower(final.Recovery.Situation), "recorded") {
		t.Errorf("the plan does not say no replacement was RECORDED: %+v", final.Recovery)
	}
}

// TestAWrongExecutionLabelIsNeverAdopted: a container from ANOTHER execution of
// the same workload -- an older quarantined replacement someone renamed back,
// say -- is not this execution's to move.
func TestAWrongExecutionLabelIsNeverAdopted(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.CreateLandsButErrs = docker.ErrMutationFailed
	})
	execution := harness.request(t)

	harness.runtime.Containers = append(harness.runtime.Containers,
		domain.ContainerSummary{ID: execReplacementID, Name: "/web", Present: true})
	harness.runtime.Inspections[execReplacementID] = &docker.Inspection{
		Detail: adoptableReplacement("exec_ffffffffffffffffffff"),
	}

	final := harness.runOnce(t, execution)
	if final.ReplacementID != "" {
		t.Fatalf("a container from another execution was adopted as %q", final.ReplacementID)
	}
}

// TestRestartRecoveryAdoptsAReplacementTheCrashLeftUnrecorded: the process died
// between the create and its checkpoint. The startup pass finds the container
// by its labels, records it, and the settled record names both containers.
func TestRestartRecoveryAdoptsAReplacementTheCrashLeftUnrecorded(t *testing.T) {
	harness := newExecHarness(t)

	interrupted := harness.request(t)
	harness.store.seedState(interrupted.ExecutionID, domain.ExecutionCreating,
		domain.CheckpointOriginalParked, "web.hm-old-"+interrupted.ExecutionID)

	harness.runtime.Containers = append(harness.runtime.Containers,
		domain.ContainerSummary{ID: execReplacementID, Name: "/web", Present: true})
	harness.runtime.Inspections[execReplacementID] = &docker.Inspection{
		Detail: adoptableReplacement(interrupted.ExecutionID),
	}

	harness.recover(t)

	settled, err := harness.store.Get(context.Background(), interrupted.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != domain.ExecutionFailed || settled.Failure != domain.ExecutionFailureInterrupted {
		t.Fatalf("state %q/%q, want failed/interrupted", settled.State, settled.Failure)
	}
	if settled.ReplacementID != execReplacementID {
		t.Fatalf("replacement id %q, want the adopted %q", settled.ReplacementID, execReplacementID)
	}
	if settled.Checkpoint != domain.CheckpointReplacementCreated {
		t.Errorf("checkpoint %q, want replacementCreated", settled.Checkpoint)
	}
	if settled.Recovery == nil || !strings.Contains(strings.ToLower(settled.Recovery.Situation), "both containers") {
		t.Errorf("the plan does not describe both containers: %+v", settled.Recovery)
	}
	// Recovery issues NO mutation. Adoption is a read and a record.
	if ops := harness.mutator.Ops(); len(ops) != 0 {
		t.Errorf("the recovery pass mutated the host: %v", ops)
	}

	// Running it again changes nothing: the record already names the
	// replacement, so there is nothing left to adopt.
	before := harness.store.checkpointsWritten()
	harness.reconcile(t)
	if after := harness.store.checkpointsWritten(); len(after) != len(before) {
		t.Errorf("a second reconciliation wrote %d more checkpoints", len(after)-len(before))
	}
}

// TestReconciliationAdoptsAReplacementThatAppearedAfterTheFailure: the record
// settled as a create failure with nothing to adopt, and the container turned
// up afterwards -- the daemon finishing a create the client had abandoned. The
// sweep adopts it, durably, and repeating the sweep converges.
func TestReconciliationAdoptsAReplacementThatAppearedAfterTheFailure(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.CreateErr = docker.ErrMutationFailed
	})
	execution := harness.request(t)
	final := harness.runOnce(t, execution)
	if final.Failure != domain.ExecutionFailureCreate || final.ReplacementID != "" {
		t.Fatalf("unexpected outcome %q / replacement %q", final.Failure, final.ReplacementID)
	}

	// Now the container appears.
	harness.runtime.Containers = append(harness.runtime.Containers,
		domain.ContainerSummary{ID: execReplacementID, Name: "/web", Present: true})
	harness.runtime.Inspections[execReplacementID] = &docker.Inspection{
		Detail: adoptableReplacement(execution.ExecutionID),
	}

	harness.reconcile(t)
	adopted, _ := harness.store.Get(context.Background(), execution.ExecutionID)
	if adopted.ReplacementID != execReplacementID {
		t.Fatalf("replacement id %q after reconciliation, want %q", adopted.ReplacementID, execReplacementID)
	}
	if adopted.Checkpoint != domain.CheckpointReplacementCreated {
		t.Errorf("checkpoint %q, want replacementCreated", adopted.Checkpoint)
	}
	if adopted.State != domain.ExecutionFailed || adopted.Failure != domain.ExecutionFailureCreate {
		t.Errorf("the settled outcome changed to %q/%q", adopted.State, adopted.Failure)
	}

	checkpoints := len(harness.store.checkpointsWritten())
	harness.reconcile(t)
	harness.reconcile(t)
	if got := len(harness.store.checkpointsWritten()); got != checkpoints {
		t.Errorf("repeated reconciliation kept writing: %d checkpoints, want %d", got, checkpoints)
	}
}

// TestReconciliationLeavesRecordsWithoutACandidateAlone: no container holds the
// name, so nothing is adopted and nothing is written.
func TestReconciliationLeavesRecordsWithoutACandidateAlone(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.CreateErr = docker.ErrMutationFailed
	})
	execution := harness.request(t)
	harness.runOnce(t, execution)

	checkpoints := len(harness.store.checkpointsWritten())
	harness.reconcile(t)
	record, _ := harness.store.Get(context.Background(), execution.ExecutionID)
	if record.ReplacementID != "" {
		t.Fatalf("something was adopted with no candidate on the host: %q", record.ReplacementID)
	}
	if got := len(harness.store.checkpointsWritten()); got != checkpoints {
		t.Errorf("reconciliation wrote %d checkpoints with nothing to adopt", got-checkpoints)
	}
}

// ---- harness helpers for the recovery paths -------------------------------

// seedState rewrites a stored execution as a crash would have left it.
func (f *fakeExecutionStore) seedState(
	executionID string, state domain.ExecutionState,
	checkpoint domain.ExecutionCheckpoint, parkedName string,
) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record := f.records[executionID]
	record.State = state
	record.Checkpoint = checkpoint
	record.ParkedName = parkedName
	at := record.RequestedAt
	record.StartedAt = &at
	record.MutatedAt = &at
}

// recover runs the worker just long enough for its startup recovery pass to
// settle every interrupted row.
func (h *execHarness) recover(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.service.Run(ctx)
	}()
	deadline := time.After(4 * time.Second)
	for {
		h.store.mu.Lock()
		settled := true
		for _, record := range h.store.records {
			if record.State.Active() {
				settled = false
			}
		}
		h.store.mu.Unlock()
		if settled {
			cancel()
			<-done
			return
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("the recovery pass did not settle every interrupted row")
		case <-time.After(time.Millisecond):
		}
	}
}

// reconcile runs one reconciliation pass by hand.
func (h *execHarness) reconcile(t *testing.T) {
	t.Helper()
	h.service.Reconcile(context.Background())
}

// TestReconciliationStopsLookingAfterTheAdoptionWindow: the window is a bound
// on how long a failed record keeps being re-read against a privileged socket,
// not a grace period for the host. A container that turns up under the
// production name a day after the failure is a stranger for all the record
// knows -- a create that lands does so within seconds -- so it is neither
// adopted nor touched, and the record is neither rewritten nor removed. The
// operator's recovery plan already tells them to look at what holds the name.
func TestReconciliationStopsLookingAfterTheAdoptionWindow(t *testing.T) {
	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.CreateErr = docker.ErrMutationFailed
	})
	execution := harness.request(t)
	final := harness.runOnce(t, execution)
	if final.Failure != domain.ExecutionFailureCreate || final.ReplacementID != "" {
		t.Fatalf("unexpected outcome %q / replacement %q", final.Failure, final.ReplacementID)
	}

	harness.advance(24*time.Hour + time.Minute)
	harness.runtime.Containers = append(harness.runtime.Containers,
		domain.ContainerSummary{ID: execReplacementID, Name: "/web", Present: true})
	harness.runtime.Inspections[execReplacementID] = &docker.Inspection{
		Detail: adoptableReplacement(execution.ExecutionID),
	}

	checkpoints := len(harness.store.checkpointsWritten())
	operations := len(harness.mutator.Ops())
	harness.reconcile(t)

	record, _ := harness.store.Get(context.Background(), execution.ExecutionID)
	if record.ReplacementID != "" || record.Checkpoint != domain.CheckpointOriginalParked {
		t.Fatalf("a record past the window was adopted: replacement %q checkpoint %q",
			record.ReplacementID, record.Checkpoint)
	}
	if record.State != domain.ExecutionFailed || record.Failure != domain.ExecutionFailureCreate {
		t.Errorf("the settled outcome changed to %q/%q", record.State, record.Failure)
	}
	if got := len(harness.store.checkpointsWritten()); got != checkpoints {
		t.Errorf("reconciliation past the window wrote %d checkpoints", got-checkpoints)
	}
	if got := len(harness.mutator.Ops()); got != operations {
		t.Errorf("reconciliation past the window mutated the host: %v", harness.mutator.Ops()[operations:])
	}
}
