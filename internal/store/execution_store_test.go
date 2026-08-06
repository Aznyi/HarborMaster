package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Container recreation persistence tests.
//
// This table backs HarborMaster's largest privilege, so the properties that
// matter are about what cannot happen:
//
//   - Two recreations of one container cannot be active at once. Both would
//     stop it and both would try to take its name.
//   - An acquisition cannot be executed twice. Single use, enforced by a unique
//     index rather than by a check the service performs and hopes to win.
//   - A checkpoint never moves backwards, and a checkpoint that does not land
//     is reported as an ERROR rather than as information.
//   - A failure that left containers on the host is never pruned.
//   - No column can hold a credential, a daemon error, or a registry response.

const execStoreDigest = "sha256:" + "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func executionFor(containerID, acquisitionID string) domain.Execution {
	now := time.Now().UTC()
	return domain.Execution{
		ExecutionID:   domain.NewExecutionID(),
		AcquisitionID: acquisitionID,
		PlanID:        "plan_00112233445566778899",
		SnapshotID:    7,
		ContainerID:   containerID,
		ContainerName: "web",
		OldImage:      "nginx:1.27.0",
		Target: domain.ExecutionTarget{
			Registry:   "docker.io",
			Repository: "library/nginx",
			Digest:     execStoreDigest,
			Reference:  "nginx:1.27.1",
			Platform:   domain.Platform{OS: "linux", Architecture: "amd64"},
		},
		State:       domain.ExecutionQueued,
		RequestedAt: now,
		ExpiresAt:   now.Add(15 * time.Minute),
		PlanDigest:  strings.Repeat("f", 64),
	}
}

// ------------------------------------------------------------- creating --

func TestExecutionCreateAndRead(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := db.Executions.Create(ctx, executionFor("container-a", "acq_0011223344556677889a"), now)
	if err != nil {
		t.Fatalf("create execution: %v", err)
	}

	read, err := db.Executions.Get(ctx, created.ExecutionID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}

	if read.State != domain.ExecutionQueued {
		t.Errorf("state = %q, want queued", read.State)
	}
	if read.Checkpoint != domain.CheckpointNone {
		t.Errorf("checkpoint = %q, want empty; nothing has been done to the host", read.Checkpoint)
	}
	if read.Target.Digest != execStoreDigest {
		t.Errorf("digest = %q", read.Target.Digest)
	}
	if read.MutatedAt != nil {
		t.Error("mutatedAt is set on a queued record; nothing has been changed")
	}

	// Every proof starts UNKNOWN, which is the fail-closed default: a proof
	// that was never reached must not read as one that passed.
	for name, result := range map[string]domain.VerificationResult{
		"health":       read.Verification.Health,
		"image":        read.Verification.Image,
		"preservation": read.Verification.Preservation,
		"network":      read.Verification.Network,
	} {
		if result != domain.VerificationUnknown {
			t.Errorf("%s verification = %q on a fresh record, want unknown", name, result)
		}
	}

	events, err := db.Executions.Events(ctx, created.ExecutionID, 100)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events on creation, want 1", len(events))
	}
}

func TestExecutionRefusesAnUnpinnedTarget(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	execution := executionFor("container-a", "acq_0011223344556677889a")
	execution.Target.Digest = ""

	_, err := db.Executions.Create(ctx, execution, time.Now().UTC())
	if !errors.Is(err, store.ErrExecutionTarget) {
		t.Fatalf("err = %v, want ErrExecutionTarget; a container must never be created from "+
			"anything but a digest", err)
	}
}

// TestOnlyOneRecreationPerContainerCanBeActive is the constraint that stops two
// pipelines fighting over one container.
func TestOnlyOneRecreationPerContainerCanBeActive(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := db.Executions.Create(ctx,
		executionFor("container-a", "acq_0011223344556677889a"), now); err != nil {
		t.Fatalf("first create: %v", err)
	}

	_, err := db.Executions.Create(ctx,
		executionFor("container-a", "acq_ffeeddccbbaa99887766"), now)
	if !errors.Is(err, store.ErrExecutionActive) {
		t.Fatalf("err = %v, want ErrExecutionActive", err)
	}

	// A DIFFERENT container is unaffected.
	if _, err := db.Executions.Create(ctx,
		executionFor("container-b", "acq_ffeeddccbbaa99887766"), now); err != nil {
		t.Fatalf("a second container was refused: %v", err)
	}
}

// TestAnAcquisitionCanBeExecutedOnlyOnce is the single-use rule.
//
// Deliberately unconditional: even a CANCELLED execution consumes its
// acquisition. The cost is that an operator must re-acquire; the benefit is
// that a stale approval can never be applied twice.
func TestAnAcquisitionCanBeExecutedOnlyOnce(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first, err := db.Executions.Create(ctx,
		executionFor("container-a", "acq_0011223344556677889a"), now)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Finish it, so no ACTIVE-container constraint is in play and the only
	// thing that can refuse the second is the single-use index.
	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: first.ExecutionID,
		To:          domain.ExecutionCancelled,
	}, now); err != nil {
		t.Fatalf("cancel first: %v", err)
	}

	_, err = db.Executions.Create(ctx,
		executionFor("container-a", "acq_0011223344556677889a"), now)
	if !errors.Is(err, store.ErrAcquisitionConsumed) {
		t.Fatalf("err = %v, want ErrAcquisitionConsumed", err)
	}

	// And the lookup used by the preflight agrees.
	_, consumed, err := db.Executions.ByAcquisition(ctx, "acq_0011223344556677889a")
	if err != nil {
		t.Fatalf("by acquisition: %v", err)
	}
	if !consumed {
		t.Error("ByAcquisition did not report the acquisition as used")
	}
}

func TestExecutionIdempotencyKeyReturnsTheExistingRecord(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first := executionFor("container-a", "acq_0011223344556677889a")
	first.RequestKey = "operator-click-1"
	created, err := db.Executions.Create(ctx, first, now)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	second := executionFor("container-a", "acq_ffeeddccbbaa99887766")
	second.RequestKey = "operator-click-1"
	retried, err := db.Executions.Create(ctx, second, now)
	if err != nil {
		t.Fatalf("retried create: %v", err)
	}
	if retried.ExecutionID != created.ExecutionID {
		t.Fatal("a retried request created a second recreation")
	}
}

// ------------------------------------------------------------ advancing --

func TestTerminalExecutionsAreNeverMoved(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := db.Executions.Create(ctx,
		executionFor("container-a", "acq_0011223344556677889a"), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: created.ExecutionID,
		To:          domain.ExecutionFailed,
		Failure:     domain.ExecutionFailurePreflight,
	}, now); err != nil {
		t.Fatalf("fail it: %v", err)
	}

	moved, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: created.ExecutionID,
		To:          domain.ExecutionSucceeded,
	}, now)
	if err != nil {
		t.Fatalf("second advance: %v", err)
	}
	if moved {
		t.Fatal("a terminal execution was moved; history was rewritten")
	}
}

func TestAdvanceRecordsVerificationAndRecovery(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := db.Executions.Create(ctx,
		executionFor("container-a", "acq_0011223344556677889a"), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	report := domain.PreservationReport{
		Status: domain.VerificationFailed, Checked: 40, Matched: 39,
		Differences: []domain.PreservationDifference{
			{Field: "security.capDrop", Kind: domain.PreservationChanged,
				Expected: "ALL", Actual: ""},
		},
	}
	plan := domain.BuildRecoveryPlan(domain.RecoveryContext{
		ExecutionID:   created.ExecutionID,
		ContainerName: "web",
		OriginalID:    strings.Repeat("a", 64),
		ParkedName:    "web.hm-old-" + created.ExecutionID,
		Checkpoint:    domain.CheckpointReplacementStarted,
	})

	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID:    created.ExecutionID,
		To:             domain.ExecutionFailed,
		Failure:        domain.ExecutionFailurePreservation,
		Message:        domain.ExecutionFailurePreservation.Explain(),
		ReplacementID:  strings.Repeat("b", 64),
		ParkedName:     "web.hm-old-" + created.ExecutionID,
		QuarantineName: "web.hm-failed-" + created.ExecutionID,
		MarkMutated:    true,
		Verification: &domain.ExecutionVerification{
			Health: domain.VerificationPassed, HealthChecked: true,
			HealthState:  domain.HealthHealthy,
			Image:        domain.VerificationPassed,
			Preservation: domain.VerificationFailed,
			Network:      domain.VerificationUnknown,
			Report:       &report,
		},
		Recovery: plan,
	}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}

	read, err := db.Executions.Get(ctx, created.ExecutionID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if read.Verification.Preservation != domain.VerificationFailed {
		t.Errorf("preservation verdict = %q", read.Verification.Preservation)
	}
	if read.Verification.Network != domain.VerificationUnknown {
		t.Errorf("network verdict = %q, want unknown; it was never reached",
			read.Verification.Network)
	}
	if read.Verification.Report == nil || len(read.Verification.Report.Differences) != 1 {
		t.Fatal("the preservation report did not round-trip")
	}
	if read.Recovery == nil || len(read.Recovery.Steps) == 0 {
		t.Fatal("the recovery plan did not round-trip")
	}
	if read.MutatedAt == nil {
		t.Error("mutatedAt was not stamped on a record that changed the host")
	}
	if read.QuarantineName == "" || read.ParkedName == "" {
		t.Error("the container names an operator needs were not recorded")
	}
}

// ---------------------------------------------------------- checkpoints --

// TestCheckpointsAdvanceAndNeverGoBackwards is what stops a late write from a
// goroutine that lost a race un-recording progress.
func TestCheckpointsAdvanceAndNeverGoBackwards(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := db.Executions.Create(ctx,
		executionFor("container-a", "acq_0011223344556677889a"), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	forward := []domain.ExecutionCheckpoint{
		domain.CheckpointOriginalStopped,
		domain.CheckpointOriginalParked,
		domain.CheckpointReplacementCreated,
		domain.CheckpointReplacementStarted,
		domain.CheckpointReplacementVerified,
	}
	for _, checkpoint := range forward {
		if err := db.Executions.Checkpoint(ctx, store.ExecutionCheckpointWrite{
			ExecutionID: created.ExecutionID,
			Checkpoint:  checkpoint,
			Detail:      "advanced",
			MarkMutated: checkpoint == domain.CheckpointOriginalStopped,
		}, now); err != nil {
			t.Fatalf("checkpoint %s: %v", checkpoint, err)
		}
	}

	read, err := db.Executions.Get(ctx, created.ExecutionID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.Checkpoint != domain.CheckpointReplacementVerified {
		t.Fatalf("checkpoint = %q, want replacementVerified", read.Checkpoint)
	}
	if read.MutatedAt == nil {
		t.Error("the first checkpoint did not stamp mutatedAt")
	}

	// Backwards is refused, and the refusal is an ERROR rather than a quiet
	// no-op: the caller must never proceed past a checkpoint it cannot confirm.
	err = db.Executions.Checkpoint(ctx, store.ExecutionCheckpointWrite{
		ExecutionID: created.ExecutionID,
		Checkpoint:  domain.CheckpointOriginalStopped,
	}, now)
	if !errors.Is(err, store.ErrCheckpointNotWritten) {
		t.Fatalf("err = %v, want ErrCheckpointNotWritten", err)
	}

	read, _ = db.Executions.Get(ctx, created.ExecutionID)
	if read.Checkpoint != domain.CheckpointReplacementVerified {
		t.Errorf("the checkpoint moved backwards to %q", read.Checkpoint)
	}
}

// TestCheckpointOnAMissingRowIsAnError is the property the pipeline depends on.
//
// Everywhere else in HarborMaster "no row matched" is information. Here it
// means the host was changed and the fact was not recorded, and the caller must
// stop rather than attempt another mutation.
func TestCheckpointOnAMissingRowIsAnError(t *testing.T) {
	db := openTestDB(t)

	err := db.Executions.Checkpoint(context.Background(), store.ExecutionCheckpointWrite{
		ExecutionID: domain.NewExecutionID(),
		Checkpoint:  domain.CheckpointOriginalStopped,
	}, time.Now().UTC())

	if !errors.Is(err, store.ErrCheckpointNotWritten) {
		t.Fatalf("err = %v, want ErrCheckpointNotWritten", err)
	}
}

func TestCheckpointRejectsAnEmptyOrUnknownValue(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, _ := db.Executions.Create(ctx,
		executionFor("container-a", "acq_0011223344556677889a"), now)

	for _, checkpoint := range []domain.ExecutionCheckpoint{"", "halfway"} {
		err := db.Executions.Checkpoint(ctx, store.ExecutionCheckpointWrite{
			ExecutionID: created.ExecutionID,
			Checkpoint:  checkpoint,
		}, now)
		if !errors.Is(err, store.ErrCheckpointNotWritten) {
			t.Errorf("checkpoint %q: err = %v, want ErrCheckpointNotWritten", checkpoint, err)
		}
	}
}

// TestCheckpointDoesNotChangeTheLifecycleState keeps the two concepts apart.
func TestCheckpointDoesNotChangeTheLifecycleState(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, _ := db.Executions.Create(ctx,
		executionFor("container-a", "acq_0011223344556677889a"), now)

	if err := db.Executions.Checkpoint(ctx, store.ExecutionCheckpointWrite{
		ExecutionID: created.ExecutionID,
		Checkpoint:  domain.CheckpointOriginalStopped,
	}, now); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	read, _ := db.Executions.Get(ctx, created.ExecutionID)
	if read.State != domain.ExecutionQueued {
		t.Errorf("state = %q, want queued; a checkpoint says what is true of the HOST, not "+
			"what HarborMaster is doing", read.State)
	}
}

// -------------------------------------------------------------- racing --

// TestConcurrentCreatesProduceExactlyOneActiveRecreation is the race the
// service's own check cannot close.
func TestConcurrentCreatesProduceExactlyOneActiveRecreation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	const attempts = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		conflicts int
	)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()

			execution := executionFor("container-a", domain.NewAcquisitionID())
			_, err := db.Executions.Create(ctx, execution, now)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, store.ErrExecutionActive):
				conflicts++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if succeeded != 1 {
		t.Fatalf("%d creates succeeded, want exactly 1; two recreations of one container would "+
			"both stop it and both fight for its name", succeeded)
	}
	if conflicts != attempts-1 {
		t.Errorf("%d conflicts, want %d", conflicts, attempts-1)
	}
}

// ------------------------------------------------------------- recovery --

// TestInterruptedReturnsRowsRatherThanUpdatingThem is what makes restart
// recovery state-aware.
func TestInterruptedReturnsRowsRatherThanUpdatingThem(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// One that never mutated, and one that had parked the original.
	queued, _ := db.Executions.Create(ctx,
		executionFor("container-a", "acq_0011223344556677889a"), now)
	mutating, _ := db.Executions.Create(ctx,
		executionFor("container-b", "acq_ffeeddccbbaa99887766"), now)

	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: mutating.ExecutionID,
		From:        []domain.ExecutionState{domain.ExecutionQueued},
		To:          domain.ExecutionCreating,
	}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := db.Executions.Checkpoint(ctx, store.ExecutionCheckpointWrite{
		ExecutionID: mutating.ExecutionID,
		Checkpoint:  domain.CheckpointOriginalParked,
		ParkedName:  "web.hm-old-" + mutating.ExecutionID,
		MarkMutated: true,
	}, now); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	interrupted, err := db.Executions.Interrupted(ctx, 100)
	if err != nil {
		t.Fatalf("interrupted: %v", err)
	}

	// The QUEUED one is not interrupted: it never started, and the expiry pass
	// owns it.
	if len(interrupted) != 1 {
		t.Fatalf("got %d interrupted rows, want 1", len(interrupted))
	}
	if interrupted[0].ExecutionID != mutating.ExecutionID {
		t.Fatalf("the wrong row was reported interrupted")
	}
	if interrupted[0].Checkpoint != domain.CheckpointOriginalParked {
		t.Errorf("checkpoint = %q; recovery reads this to decide what to say",
			interrupted[0].Checkpoint)
	}

	// And nothing was changed by the read.
	still, _ := db.Executions.Get(ctx, queued.ExecutionID)
	if still.State != domain.ExecutionQueued {
		t.Error("Interrupted modified a row it merely reported")
	}
}

func TestExpireOnlyTouchesQueuedRequests(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	stale := executionFor("container-a", "acq_0011223344556677889a")
	stale.ExpiresAt = now.Add(-time.Hour)
	queued, _ := db.Executions.Create(ctx, stale, now)

	mutating := executionFor("container-b", "acq_ffeeddccbbaa99887766")
	mutating.ExpiresAt = now.Add(-time.Hour)
	started, _ := db.Executions.Create(ctx, mutating, now)
	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: started.ExecutionID,
		From:        []domain.ExecutionState{domain.ExecutionQueued},
		To:          domain.ExecutionCreating,
	}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}

	expired, err := db.Executions.ExpireStale(ctx, now, 100)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired %d, want 1", expired)
	}

	read, _ := db.Executions.Get(ctx, queued.ExecutionID)
	if read.State != domain.ExecutionExpired {
		t.Errorf("the queued request is %q, want expired", read.State)
	}

	// The MUTATING one must not be expired out from under the recovery pass: it
	// may have containers on the host, and "expired" would say it did not.
	read, _ = db.Executions.Get(ctx, started.ExecutionID)
	if read.State != domain.ExecutionCreating {
		t.Errorf("a mutating recreation was expired to %q; only the recovery pass may settle "+
			"one, because only it reads the checkpoint", read.State)
	}
}

// ------------------------------------------------------------ retention --

// TestPruneKeepsFailuresThatLeftContainersBehind.
//
// Removing that record would leave an operator with two unexplained containers
// and nothing accounting for them -- the exact situation the record exists to
// prevent.
func TestPruneKeepsFailuresThatLeftContainersBehind(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-400 * 24 * time.Hour)

	// A clean failure on container-a: nothing on the host.
	clean, err := db.Executions.Create(ctx,
		executionFor("container-a", "acq_0011223344556677889a"), old)
	if err != nil {
		t.Fatalf("create clean: %v", err)
	}
	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: clean.ExecutionID,
		To:          domain.ExecutionFailed,
		Failure:     domain.ExecutionFailurePreflight,
	}, old); err != nil {
		t.Fatalf("fail clean: %v", err)
	}

	stranded, err := db.Executions.Create(ctx,
		executionFor("container-b", "acq_ffeeddccbbaa99887766"), old)
	if err != nil {
		t.Fatalf("create stranded: %v", err)
	}

	// A failure that left both containers.
	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: stranded.ExecutionID,
		From:        []domain.ExecutionState{domain.ExecutionQueued},
		To:          domain.ExecutionCreating,
	}, old); err != nil {
		t.Fatalf("advance stranded: %v", err)
	}
	if err := db.Executions.Checkpoint(ctx, store.ExecutionCheckpointWrite{
		ExecutionID: stranded.ExecutionID,
		Checkpoint:  domain.CheckpointReplacementQuarantined,
		MarkMutated: true,
	}, old); err != nil {
		t.Fatalf("checkpoint stranded: %v", err)
	}
	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: stranded.ExecutionID,
		To:          domain.ExecutionFailed,
		Failure:     domain.ExecutionFailureUnhealthy,
	}, old); err != nil {
		t.Fatalf("fail stranded: %v", err)
	}

	// A newer record on each container, so the "never prune the most recent per
	// container" rule does not mask what is being tested. Created only now,
	// because a container may hold at most one ACTIVE recreation.
	for container, acquisition := range map[string]string{
		"container-a": "acq_11111111111111111111",
		"container-b": "acq_22222222222222222222",
	} {
		if _, err := db.Executions.Create(ctx, executionFor(container, acquisition), old); err != nil {
			t.Fatalf("create newer record for %s: %v", container, err)
		}
	}

	if _, err := db.Executions.Prune(ctx, time.Now().UTC(), 200); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if _, err := db.Executions.Get(ctx, stranded.ExecutionID); err != nil {
		t.Fatalf("the record for a failure that left containers behind was pruned: %v", err)
	}
	if _, err := db.Executions.Get(ctx, clean.ExecutionID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a clean old failure was not pruned: %v", err)
	}
}

// ------------------------------------------------------------- reading --

func TestExecutionSummaryCountsWhatNeedsAttention(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	stranded, _ := db.Executions.Create(ctx, executionFor("container-a", "acq_0011223344556677889a"), now)
	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: stranded.ExecutionID,
		From:        []domain.ExecutionState{domain.ExecutionQueued},
		To:          domain.ExecutionCreating,
	}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := db.Executions.Checkpoint(ctx, store.ExecutionCheckpointWrite{
		ExecutionID: stranded.ExecutionID,
		Checkpoint:  domain.CheckpointOriginalParked,
		MarkMutated: true,
	}, now); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: stranded.ExecutionID,
		To:          domain.ExecutionFailed,
		Failure:     domain.ExecutionFailureCreate,
	}, now); err != nil {
		t.Fatalf("fail: %v", err)
	}

	clean, _ := db.Executions.Create(ctx, executionFor("container-b", "acq_ffeeddccbbaa99887766"), now)
	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: clean.ExecutionID,
		To:          domain.ExecutionFailed,
		Failure:     domain.ExecutionFailurePreflight,
	}, now); err != nil {
		t.Fatalf("fail clean: %v", err)
	}

	summary, err := db.Executions.Summary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Failed != 2 {
		t.Errorf("failed = %d, want 2", summary.Failed)
	}
	if summary.NeedsAttention != 1 {
		t.Errorf("needsAttention = %d, want 1; only the one that changed the host counts",
			summary.NeedsAttention)
	}
}

func TestExecutionListFiltersByAttention(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	stranded, _ := db.Executions.Create(ctx, executionFor("container-a", "acq_0011223344556677889a"), now)
	db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: stranded.ExecutionID,
		From:        []domain.ExecutionState{domain.ExecutionQueued},
		To:          domain.ExecutionCreating,
	}, now)
	db.Executions.Checkpoint(ctx, store.ExecutionCheckpointWrite{
		ExecutionID: stranded.ExecutionID,
		Checkpoint:  domain.CheckpointOriginalParked,
	}, now)
	db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: stranded.ExecutionID,
		To:          domain.ExecutionFailed,
		Failure:     domain.ExecutionFailureCreate,
	}, now)

	db.Executions.Create(ctx, executionFor("container-b", "acq_ffeeddccbbaa99887766"), now)

	items, total, err := db.Executions.List(ctx, store.ExecutionFilter{
		NeedsAttention: true,
		Page:           store.Page{Limit: 50},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("got %d items (total %d), want 1", len(items), total)
	}
	if items[0].ExecutionID != stranded.ExecutionID {
		t.Error("the wrong record was reported as needing attention")
	}
}

func TestExecutionSortFieldsAreAnAllowlist(t *testing.T) {
	for _, field := range []string{"requestedAt", "completedAt", "state", "container", "id"} {
		if !store.ValidExecutionSortField(field) {
			t.Errorf("%q should be sortable", field)
		}
	}
	for _, field := range []string{
		"", "message", "container_name; DROP TABLE executions", "1", "target_digest",
	} {
		if store.ValidExecutionSortField(field) {
			t.Errorf("%q must not be sortable", field)
		}
	}
}

// TestExecutionMessagesAreBounded keeps an unbounded string out of a column an
// API returns and a browser renders.
func TestExecutionMessagesAreBounded(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, _ := db.Executions.Create(ctx,
		executionFor("container-a", "acq_0011223344556677889a"), now)

	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: created.ExecutionID,
		To:          domain.ExecutionFailed,
		Failure:     domain.ExecutionFailureInternal,
		Message:     strings.Repeat("x", 5000),
		Detail:      strings.Repeat("y", 5000),
	}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}

	read, _ := db.Executions.Get(ctx, created.ExecutionID)
	if len(read.Message) > domain.MaxExecutionMessageBytes {
		t.Errorf("message is %d bytes, the bound is %d",
			len(read.Message), domain.MaxExecutionMessageBytes)
	}

	events, _ := db.Executions.Events(ctx, created.ExecutionID, 100)
	for _, event := range events {
		if len(event.Detail) > domain.MaxExecutionMessageBytes {
			t.Errorf("event detail is %d bytes, the bound is %d",
				len(event.Detail), domain.MaxExecutionMessageBytes)
		}
	}
}

// TestControlCharactersAreStrippedFromStoredText closes the log- and
// UI-forgery path.
func TestControlCharactersAreStrippedFromStoredText(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, _ := db.Executions.Create(ctx,
		executionFor("container-a", "acq_0011223344556677889a"), now)

	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: created.ExecutionID,
		To:          domain.ExecutionFailed,
		Failure:     domain.ExecutionFailureInternal,
		Message:     "line one\nlevel=error msg=\"forged\"\r\x00",
	}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}

	read, _ := db.Executions.Get(ctx, created.ExecutionID)
	if strings.ContainsAny(read.Message, "\n\r\x00") {
		t.Errorf("control characters survived into a stored message: %q", read.Message)
	}
}
