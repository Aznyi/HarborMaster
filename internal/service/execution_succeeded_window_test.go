package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The window between "succeeded" and "finished" (CI reliability).
//
// # What CI caught
//
// TestASuccessfulRecreationFollowsTheWholePipeline failed under
// `go test -race -coverprofile`:
//
//	checkpoint = "replacementVerified", want originalRemoved
//	originalRemoved is false on a succeeded recreation
//
// Nothing in the pipeline was wrong. ExecutionService.succeed records
// ExecutionSucceeded and only THEN advances lineage, removes the parked
// original and writes CheckpointOriginalRemoved -- an ordering its own comment
// calls the safety property:
//
//	"The success is written FIRST. If that write does not land, HarborMaster
//	 cannot prove afterwards that the replacement was ever verified -- so it
//	 stops with both containers on the host and a recovery plan, rather than
//	 removing the only thing that could restore service on the strength of a
//	 record it could not write."
//
// The record is therefore legitimately observable, for a moment, as succeeded
// with the original still present. The test harness polled for a terminal
// state, waited for the worker, and then returned the snapshot it had taken
// BEFORE waiting. Race and coverage instrumentation widened the window until
// the poll landed inside it.
//
// # Why this test exists rather than a rerun
//
// Re-running under -race proves nothing: the failure is a scheduling
// coincidence and a green run is not evidence. These two tests stop time at
// exactly the contended point with a channel barrier, so the window is
// observed on purpose and the harness contract is asserted rather than hoped
// for. No sleeps: a duration long enough on one machine is a flake on another,
// which is the bug being fixed.

// TestTheSucceededWindowIsRealAndObservable pins the production ordering.
//
// If this ever fails, the pipeline stopped recording success before removing
// the original -- which would be a genuine safety regression, not a test bug.
func TestTheSucceededWindowIsRealAndObservable(t *testing.T) {
	entered := make(chan struct{}, 1)
	hold := make(chan struct{})

	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.RemoveEntered = entered
		h.mutator.HoldRemove = hold
	})
	execution := harness.request(t)

	// The service runs while the removal is held.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		harness.service.Run(ctx)
	}()

	// Wait for the pipeline to reach the removal. Everything before it --
	// verification and the durable success write -- has happened by now.
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		close(hold)
		cancel()
		<-done
		t.Fatal("the pipeline never reached the removal of the parked original")
	}

	// THE WINDOW. The store says succeeded; the housekeeping has not run.
	held, err := harness.store.Get(context.Background(), execution.ExecutionID)
	if err != nil {
		close(hold)
		cancel()
		<-done
		t.Fatalf("read while held: %v", err)
	}
	if held.State != domain.ExecutionSucceeded {
		close(hold)
		cancel()
		<-done
		t.Fatalf("state while held = %q, want succeeded -- success must be "+
			"recorded BEFORE the original is removed", held.State)
	}
	if held.Checkpoint != domain.CheckpointReplacementVerified {
		t.Errorf("checkpoint while held = %q, want replacementVerified",
			held.Checkpoint)
	}
	if held.OriginalRemoved {
		t.Error("originalRemoved is true before the removal has run")
	}
	// And the original really is still on the modelled host, which is the
	// safety property the ordering exists for.
	if !harness.mutator.Present(execContainerID) {
		t.Error("the original was removed before the success was recorded")
	}

	// Release the removal and let the worker finish.
	close(hold)
	cancel()
	<-done

	// THE FINAL RECORD. Everything the happy-path test asserts.
	final, err := harness.store.Get(context.Background(), execution.ExecutionID)
	if err != nil {
		t.Fatalf("read after completion: %v", err)
	}
	if final.State != domain.ExecutionSucceeded {
		t.Fatalf("final state = %q, want succeeded", final.State)
	}
	if final.Checkpoint != domain.CheckpointOriginalRemoved {
		t.Errorf("final checkpoint = %q, want originalRemoved", final.Checkpoint)
	}
	if !final.OriginalRemoved {
		t.Error("originalRemoved is false after the worker finished")
	}
}

// TestRunOnceReturnsTheRecordAfterTheWorkerFinished is the regression itself.
//
// It forces the harness to observe the terminal state INSIDE the window -- the
// exact scheduling CI produced by accident -- and then requires runOnce to
// return the finished record anyway. Against the old helper, which returned the
// snapshot taken before it waited, this fails with precisely the CI message.
func TestRunOnceReturnsTheRecordAfterTheWorkerFinished(t *testing.T) {
	entered := make(chan struct{}, 1)
	hold := make(chan struct{})

	harness := newExecHarness(t, func(h *execHarness) {
		h.mutator.RemoveEntered = entered
		h.mutator.HoldRemove = hold
	})
	execution := harness.request(t)

	// The removal is released only once the HARNESS has been handed a terminal
	// record while the housekeeping is still outstanding. That is the exact
	// scheduling CI produced by accident, reached here on purpose:
	//
	//	worker            reaches RemoveContainer and blocks   (state: succeeded)
	//	runOnce           polls, is handed succeeded           <- window observed
	//	this goroutine    releases the removal
	//	worker            removes, writes originalRemoved
	//	runOnce           returns -- and must return the FINISHED record
	//
	// Nothing here sleeps to arrange it: both steps are channel handoffs.
	terminalSeen := make(chan domain.Execution, 8)
	harness.store.ObserveTerminalReads(terminalSeen)

	released := make(chan struct{})
	var windowErr string
	go func() {
		defer close(released)
		defer close(hold)

		select {
		case <-entered:
		case <-time.After(15 * time.Second):
			windowErr = "the pipeline never reached the removal of the parked original"
			return
		}
		select {
		case observed := <-terminalSeen:
			// Non-vacuity: if the harness were handed an already-finished
			// record, this test would prove nothing about the window.
			if observed.OriginalRemoved ||
				observed.Checkpoint == domain.CheckpointOriginalRemoved {
				windowErr = "the harness observed an already-finished record, so " +
					"the succeeded-before-cleanup window was not exercised"
			}
		case <-time.After(15 * time.Second):
			windowErr = "the harness never observed a terminal record"
		}
	}()

	final := harness.runOnce(t, execution)
	<-released
	if windowErr != "" {
		t.Fatal(windowErr)
	}

	// The contract callers rely on: `final` is the FINAL persisted record.
	if final.State != domain.ExecutionSucceeded {
		t.Fatalf("state = %q (%s), want succeeded", final.State, final.Message)
	}
	if final.Checkpoint != domain.CheckpointOriginalRemoved {
		t.Fatalf("checkpoint = %q, want originalRemoved -- runOnce returned a "+
			"snapshot taken before the worker finished its housekeeping",
			final.Checkpoint)
	}
	if !final.OriginalRemoved {
		t.Error("originalRemoved is false on a succeeded recreation")
	}
	// The housekeeping really did happen, so the assertions above are not
	// passing on a record that merely looks finished.
	if harness.mutator.Present(execContainerID) {
		t.Error("the parked original is still on the host after a completed recreation")
	}
}
