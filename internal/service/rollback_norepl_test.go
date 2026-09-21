package service_test

import (
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Rolling back a recreation that never produced a replacement.
//
// # The defect these tests pin
//
// A create that fails after the original has been stopped and parked leaves the
// workload DOWN with nothing to stop and nothing to park: the original is the
// only container, and putting it back is a rename and a start. The rollback
// path refused exactly this arrangement with "nothing to roll back", which was
// the one answer that could not be true of a container that is stopped under a
// parked name. The automation follower asked, was refused, counted the failure,
// and paused the container -- and the service stayed down until a person ran
// two docker commands.
//
// So every test here is about an execution record with an EMPTY replacement id
// and a checkpoint at or before originalParked.

// rbCreateFailedExecution is a recreation whose create failed after the park.
func rbCreateFailedExecution(now time.Time) domain.Execution {
	execution := rbExecution(now)
	execution.State = domain.ExecutionFailed
	execution.Failure = domain.ExecutionFailureCreate
	execution.Checkpoint = domain.CheckpointOriginalParked
	execution.ReplacementID = ""
	return execution
}

// rbRenameFailedExecution is a recreation whose park rename failed after the
// stop: the original is stopped and still carries its own name.
func rbRenameFailedExecution(now time.Time) domain.Execution {
	execution := rbCreateFailedExecution(now)
	execution.Failure = domain.ExecutionFailureRename
	execution.Checkpoint = domain.CheckpointOriginalStopped
	return execution
}

func TestARollbackRestoresAnOriginalWhoseReplacementWasNeverCreated(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.host.remove(rbReplacementID)
		h.evidence.execution = rbCreateFailedExecution(h.base)
	})

	final := harness.runOnce(t, harness.request(t))

	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state %q/%q/%q, want succeeded",
			final.State, final.Failure, final.Refusal)
	}
	if name := harness.host.nameOf(rbOriginalID); name != rbContainerName {
		t.Errorf("the original is named %q, want %q", name, rbContainerName)
	}
	if !harness.host.running(rbOriginalID) {
		t.Error("the original is not running; the whole point was to restore service")
	}

	// Two mutations and no more: there is no replacement to stop or park, and a
	// rollback that tried would be acting on a container that does not exist.
	want := []string{
		"restore:" + rbContainerName,
		"start:" + rbOriginalID,
	}
	if got := harness.host.operations(); !equalStrings(got, want) {
		t.Errorf("host operations %v, want %v", got, want)
	}
	wantCheckpoints := []domain.RollbackCheckpoint{
		domain.RollbackCheckpointOriginalRestored,
		domain.RollbackCheckpointOriginalStarted,
		domain.RollbackCheckpointOriginalVerified,
	}
	if got := harness.store.checkpointsWritten(); !equalCheckpoints(got, wantCheckpoints) {
		t.Errorf("checkpoints %v, want %v", got, wantCheckpoints)
	}
	if final.ReplacementID != "" || final.ReplacementParkedName != "" {
		t.Errorf("the record names a replacement (%q parked as %q) that never existed",
			final.ReplacementID, final.ReplacementParkedName)
	}
	if !final.Verification.Passed() {
		t.Errorf("verification did not pass: %+v", final.Verification)
	}
}

func TestARollbackStartsAnOriginalThatStillHoldsItsOwnName(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.host.remove(rbReplacementID)
		h.host.with(rbOriginalID, func(c *rbContainer) {
			c.detail.Overview.Name = rbContainerName
		})
		h.evidence.execution = rbRenameFailedExecution(h.base)
	})

	final := harness.runOnce(t, harness.request(t))

	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state %q/%q/%q, want succeeded",
			final.State, final.Failure, final.Refusal)
	}
	// ONE mutation. The original already answers to its own name, and renaming
	// a container to the name it already holds is an error the daemon reports.
	want := []string{"start:" + rbOriginalID}
	if got := harness.host.operations(); !equalStrings(got, want) {
		t.Errorf("host operations %v, want %v", got, want)
	}
	wantCheckpoints := []domain.RollbackCheckpoint{
		domain.RollbackCheckpointOriginalStarted,
		domain.RollbackCheckpointOriginalVerified,
	}
	if got := harness.store.checkpointsWritten(); !equalCheckpoints(got, wantCheckpoints) {
		t.Errorf("checkpoints %v, want %v", got, wantCheckpoints)
	}
	if !harness.host.running(rbOriginalID) {
		t.Error("the original is not running")
	}
}

// TestTheRestoreIsDecidedFromTheLiveNameNotTheCheckpoint covers a park rename
// that timed out on the client and landed on the daemon anyway: the record says
// originalStopped, the host says parked.
func TestTheRestoreIsDecidedFromTheLiveNameNotTheCheckpoint(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.host.remove(rbReplacementID)
		// The original is parked on the host (the fixture default) while the
		// record claims the rename never happened.
		h.evidence.execution = rbRenameFailedExecution(h.base)
	})

	final := harness.runOnce(t, harness.request(t))

	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state %q/%q/%q, want succeeded",
			final.State, final.Failure, final.Refusal)
	}
	want := []string{"restore:" + rbContainerName, "start:" + rbOriginalID}
	if got := harness.host.operations(); !equalStrings(got, want) {
		t.Errorf("host operations %v, want %v", got, want)
	}
	if name := harness.host.nameOf(rbOriginalID); name != rbContainerName {
		t.Errorf("the original is named %q, want %q", name, rbContainerName)
	}
}

// TestANoReplacementRollbackRefusesWhenAStrangerHoldsTheName is the case the
// rollback must NOT repair by force: a container HarborMaster did not record
// answers to the production name. It may be the replacement a timed-out create
// produced, or something an operator started by hand. Either way, renaming the
// original onto that name would collide, and stopping the stranger would be
// acting on a container nobody approved.
func TestANoReplacementRollbackRefusesWhenAStrangerHoldsTheName(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.host.remove(rbReplacementID)
		stranger := rbReplacementDetail()
		stranger.Overview.ID = rbStrangerID
		stranger.Overview.ShortID = domain.ShortenID(rbStrangerID)
		h.host.add(&rbContainer{detail: stranger})
		h.evidence.execution = rbCreateFailedExecution(h.base)
	})

	if refusal := harness.refusal(t); refusal != domain.RollbackRefusalNameUnavailable {
		t.Fatalf("refusal %q, want nameUnavailable", refusal)
	}
	if ops := harness.host.operations(); len(ops) != 0 {
		t.Errorf("a refused rollback touched the host: %v", ops)
	}
}

// TestANoReplacementRollbackRequiresAConsistentRecord refuses a record that
// claims a replacement was created but does not say which container it is.
// Such a record cannot be acted on safely in either direction.
func TestANoReplacementRollbackRequiresAConsistentRecord(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.host.remove(rbReplacementID)
		execution := rbCreateFailedExecution(h.base)
		execution.Checkpoint = domain.CheckpointReplacementCreated
		h.evidence.execution = execution
	})

	if refusal := harness.refusal(t); refusal != domain.RollbackRefusalNothingToRollBack {
		t.Fatalf("refusal %q, want nothingToRollBack", refusal)
	}
	if ops := harness.host.operations(); len(ops) != 0 {
		t.Errorf("a refused rollback touched the host: %v", ops)
	}
}

// TestANoReplacementRollbackStillVerifiesTheOriginal: restoring service is not
// the same as proving it. An original that comes back unhealthy is a failed
// rollback with a plan, exactly as it is when a replacement was involved.
func TestANoReplacementRollbackStillVerifiesTheOriginal(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.host.remove(rbReplacementID)
		h.host.with(rbOriginalID, func(c *rbContainer) {
			c.healthOnStart = domain.HealthUnhealthy
		})
		h.evidence.execution = rbCreateFailedExecution(h.base)
	})

	final := harness.runOnce(t, harness.request(t))

	if final.State != domain.RollbackFailed || final.Failure != domain.RollbackFailureUnhealthy {
		t.Fatalf("state %q/%q, want failed/unhealthy", final.State, final.Failure)
	}
	if final.Recovery == nil {
		t.Fatal("a failed rollback carries no recovery plan")
	}
	if final.Checkpoint != domain.RollbackCheckpointOriginalStarted {
		t.Errorf("checkpoint %q, want originalStarted", final.Checkpoint)
	}
}
