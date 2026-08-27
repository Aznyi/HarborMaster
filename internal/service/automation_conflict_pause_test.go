package service_test

import (
	"context"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// A concurrency conflict is not an update that failed.
//
// # What live acceptance found
//
// A scheduled pass decided a container that a manual pass had already
// submitted. The second recreation was correctly refused by the execution
// preflight -- "another recreation is already running for this container" --
// and that refusal was counted toward the policy's pauseAfterFailures.
//
// The container was paused while its update was still progressing. Worse, the
// pause then reported the concurrency clash as its reason, so an operator
// reading the paused list was told about a race when what actually happened
// moments later was a failed health check and a rollback. Two different
// situations rendered as one, which is the failure Phase 17 exists to prevent.
//
// # Why settling is the right outcome rather than counting
//
// The decision is redundant: the work it wanted is already under way under
// another decision, and whatever that execution does -- including failing --
// is recorded against it. Leaving this decision open would bring it back on the
// next follow tick to submit a SECOND recreation once the first finished, which
// is the duplicate mutation the phase forbids outright.
func TestAConcurrencyConflictNeitherCountsNorRetries(t *testing.T) {
	harness := newAutomationHarness(t)

	// A decision that has an acquisition and is ready to be recreated.
	decision := domain.AutomationDecision{
		RunID:         "run_00000000000000000001",
		ContainerName: "web",
		ContainerID:   "container-web",
		PolicyID:      "upd_aaaaaaaaaaaaaaaaaaaa",
		AcquisitionID: "acq_00000000000000000001",
		Verdict:       domain.VerdictUpdate,
	}
	harness.pipeline.acquisitions["acq_00000000000000000001"] = domain.Acquisition{
		AcquisitionID: "acq_00000000000000000001",
		PlanID:        "plan_0123456789abcdef0123",
		State:         domain.AcquisitionSucceeded,
	}
	harness.store.decisions = []domain.AutomationDecision{decision}

	// The execution service refuses: the same container is already being
	// recreated under a different decision.
	harness.pipeline.executeErr = service.ErrExecutionRefused{
		Refusal: domain.ExecutionRefusalConflict,
	}

	service.FollowForTest(harness.engine, context.Background())

	if got := len(harness.store.failures); got != 0 {
		t.Errorf("failures recorded for %d container(s), want 0\n"+
			"\ta refusal that says the work is ALREADY HAPPENING is not an update "+
			"that failed; counting it pauses a container whose update is "+
			"progressing, and misreports why", got)
	}
	if got := len(harness.store.pauses); got != 0 {
		t.Errorf("pauses = %d, want 0: a concurrency conflict alone must not pause", got)
	}
	if len(harness.store.settled) == 0 {
		t.Error("the redundant decision was left open\n" +
			"\tit would return on the next tick and submit a second recreation " +
			"once the first finished")
	}
}

// Every other refusal still counts. Without this the fix above would be a way
// to stop pausing altogether.
func TestOtherPreflightRefusalsStillCount(t *testing.T) {
	harness := newAutomationHarness(t)

	decision := domain.AutomationDecision{
		RunID:         "run_00000000000000000001",
		ContainerName: "web",
		ContainerID:   "container-web",
		PolicyID:      "upd_aaaaaaaaaaaaaaaaaaaa",
		AcquisitionID: "acq_00000000000000000001",
		Verdict:       domain.VerdictUpdate,
	}
	harness.pipeline.acquisitions["acq_00000000000000000001"] = domain.Acquisition{
		AcquisitionID: "acq_00000000000000000001",
		PlanID:        "plan_0123456789abcdef0123",
		State:         domain.AcquisitionSucceeded,
	}
	harness.store.decisions = []domain.AutomationDecision{decision}

	harness.pipeline.executeErr = service.ErrExecutionRefused{
		Refusal: domain.ExecutionRefusalSnapshotChanged,
	}

	service.FollowForTest(harness.engine, context.Background())

	if len(harness.store.failures) == 0 {
		t.Error("a real preflight refusal was not counted; pauseAfterFailures " +
			"would never fire")
	}
}
