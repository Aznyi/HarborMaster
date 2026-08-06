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

// Manual rollback persistence tests.
//
// A rollback moves containers an operator depends on, so the properties worth
// proving here are all about what the DATABASE refuses, not what the service
// remembers to check:
//
//   - Two rollbacks of one container cannot be active at once. Both would
//     rename what the other just renamed.
//   - One execution cannot be rolled back twice successfully.
//   - An idempotency key names exactly one rollback.
//   - A transition applies only from the states it names, and nothing moves a
//     terminal record.
//   - A checkpoint and the host fact it establishes land in one transaction.
//   - Expiry touches only queued rows, so it can never abandon a rollback that
//     has begun changing the host.
//   - Retention never removes a failure that left containers behind.

const rollbackStoreContainer = "web"

func rollbackFor(executionID, containerName string) domain.Rollback {
	now := time.Now().UTC()
	return domain.Rollback{
		RollbackID:       domain.NewRollbackID(),
		ExecutionID:      executionID,
		ContainerName:    containerName,
		OriginalID:       strings.Repeat("a", 64),
		ParkedName:       containerName + domain.ParkedNameSuffix + executionID,
		ReplacementID:    strings.Repeat("b", 64),
		OriginalImage:    "nginx:1.27.0",
		OriginalImageID:  "sha256:" + strings.Repeat("c", 64),
		ReplacementImage: "nginx:1.27.1",
		State:            domain.RollbackQueued,
		RequestedAt:      now,
		ExpiresAt:        now.Add(10 * time.Minute),
		RequestedBy:      domain.Requester{UserID: "usr_0011223344556677889a", Username: "opsy"},
	}
}

// ------------------------------------------------------------- creating --

func TestRollbackCreateAndRead(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := db.Rollbacks.Create(ctx,
		rollbackFor("exec_00112233445566778899", rollbackStoreContainer), now)
	if err != nil {
		t.Fatalf("create rollback: %v", err)
	}

	read, err := db.Rollbacks.Get(ctx, created.RollbackID)
	if err != nil {
		t.Fatalf("get rollback: %v", err)
	}

	if read.State != domain.RollbackQueued {
		t.Errorf("state = %q, want queued", read.State)
	}
	if read.Checkpoint != domain.RollbackCheckpointNone {
		t.Errorf("checkpoint = %q, want empty; nothing has been done to the host",
			read.Checkpoint)
	}
	if read.MutatedAt != nil {
		t.Error("mutatedAt is set on a rollback that has changed nothing")
	}
	if read.RequestedBy.Username != "opsy" {
		t.Errorf("requester = %+v, want the account that asked", read.RequestedBy)
	}
	// The four proofs start UNKNOWN rather than empty: "never checked" and
	// "nothing to report" are different facts.
	for name, verdict := range map[string]domain.VerificationResult{
		"health":       read.Verification.Health,
		"image":        read.Verification.Image,
		"preservation": read.Verification.Preservation,
		"network":      read.Verification.Network,
	} {
		if verdict != domain.VerificationUnknown {
			t.Errorf("%s verdict = %q, want unknown", name, verdict)
		}
	}
}

// TestOnlyOneRollbackPerContainerCanBeActive is the collision the partial
// unique index exists to prevent.
func TestOnlyOneRollbackPerContainerCanBeActive(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := db.Rollbacks.Create(ctx,
		rollbackFor("exec_00112233445566778899", rollbackStoreContainer), now); err != nil {
		t.Fatalf("create first: %v", err)
	}

	_, err := db.Rollbacks.Create(ctx,
		rollbackFor("exec_aaaaaaaaaaaaaaaaaaaa", rollbackStoreContainer), now)
	if !errors.Is(err, store.ErrRollbackActive) {
		t.Fatalf("second create gave %v, want ErrRollbackActive", err)
	}

	// A rollback of a DIFFERENT container is unaffected: the constraint is
	// about contending for one name, not about rollbacks in general.
	if _, err := db.Rollbacks.Create(ctx,
		rollbackFor("exec_bbbbbbbbbbbbbbbbbbbb", "api"), now); err != nil {
		t.Fatalf("create for another container: %v", err)
	}
}

// TestAFinishedRollbackFreesTheContainer proves the index is PARTIAL.
func TestAFinishedRollbackFreesTheContainer(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first, err := db.Rollbacks.Create(ctx,
		rollbackFor("exec_00112233445566778899", rollbackStoreContainer), now)
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
		RollbackID: first.RollbackID,
		To:         domain.RollbackCancelled,
		Message:    "cancelled",
	}, now); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	if _, err := db.Rollbacks.Create(ctx,
		rollbackFor("exec_aaaaaaaaaaaaaaaaaaaa", rollbackStoreContainer), now); err != nil {
		t.Fatalf("create after the first finished: %v", err)
	}
}

// TestOneExecutionCannotBeRolledBackTwice is the duplicate-mutation guard.
func TestOneExecutionCannotBeRolledBackTwice(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const executionID = "exec_00112233445566778899"

	first, err := db.Rollbacks.Create(ctx, rollbackFor(executionID, rollbackStoreContainer), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
		RollbackID: first.RollbackID,
		To:         domain.RollbackSucceeded,
		Message:    "done",
	}, now); err != nil {
		t.Fatalf("succeed: %v", err)
	}

	_, err = db.Rollbacks.Create(ctx, rollbackFor(executionID, rollbackStoreContainer), now)
	if !errors.Is(err, store.ErrRollbackAlreadySucceeded) {
		t.Fatalf("second create gave %v, want ErrRollbackAlreadySucceeded", err)
	}

	rolledBack, err := db.Rollbacks.SucceededForExecution(ctx, executionID)
	if err != nil {
		t.Fatalf("succeeded for execution: %v", err)
	}
	if !rolledBack {
		t.Error("the execution does not report itself rolled back")
	}
}

// TestAnIdempotencyKeyNamesOneRollback.
func TestAnIdempotencyKeyNamesOneRollback(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first := rollbackFor("exec_00112233445566778899", rollbackStoreContainer)
	first.RequestKey = "idem-0001"
	created, err := db.Rollbacks.Create(ctx, first, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	found, ok, err := db.Rollbacks.ByRequestKey(ctx, "idem-0001")
	if err != nil {
		t.Fatalf("by request key: %v", err)
	}
	if !ok || found.RollbackID != created.RollbackID {
		t.Fatalf("key lookup gave %+v/%v, want %q", found, ok, created.RollbackID)
	}

	if _, ok, err := db.Rollbacks.ByRequestKey(ctx, "not-a-key"); err != nil || ok {
		t.Errorf("unknown key gave %v/%v, want not found", ok, err)
	}
	if _, ok, err := db.Rollbacks.ByRequestKey(ctx, ""); err != nil || ok {
		t.Errorf("empty key gave %v/%v, want not found", ok, err)
	}
}

// ------------------------------------------------------------ advancing --

// TestATransitionAppliesOnlyFromTheStatesItNames.
func TestATransitionAppliesOnlyFromTheStatesItNames(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := db.Rollbacks.Create(ctx,
		rollbackFor("exec_00112233445566778899", rollbackStoreContainer), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The row is queued, so a transition out of validating must not apply.
	moved, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
		RollbackID: created.RollbackID,
		From:       []domain.RollbackState{domain.RollbackValidating},
		To:         domain.RollbackStoppingReplacement,
	}, now)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if moved {
		t.Fatal("a transition applied from a state it did not name")
	}

	read, err := db.Rollbacks.Get(ctx, created.RollbackID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.State != domain.RollbackQueued {
		t.Errorf("state = %q, want queued", read.State)
	}
}

// TestNothingMovesATerminalRollback is the immutability property.
func TestNothingMovesATerminalRollback(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := db.Rollbacks.Create(ctx,
		rollbackFor("exec_00112233445566778899", rollbackStoreContainer), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
		RollbackID: created.RollbackID,
		To:         domain.RollbackFailed,
		Failure:    domain.RollbackFailureStop,
		Message:    "the replacement could not be stopped",
	}, now); err != nil {
		t.Fatalf("fail: %v", err)
	}

	for _, to := range []domain.RollbackState{
		domain.RollbackSucceeded, domain.RollbackQueued,
		domain.RollbackStoppingReplacement, domain.RollbackCancelled,
	} {
		moved, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
			RollbackID: created.RollbackID,
			To:         to,
		}, now)
		if err != nil {
			t.Fatalf("advance to %q: %v", to, err)
		}
		if moved {
			t.Errorf("a terminal rollback was moved to %q", to)
		}
	}

	// And a checkpoint cannot be written onto one either.
	recorded, err := db.Rollbacks.Checkpoint(ctx, store.RollbackCheckpointWrite{
		RollbackID: created.RollbackID,
		Checkpoint: domain.RollbackCheckpointOriginalVerified,
	}, now)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if recorded {
		t.Error("a checkpoint was written onto a terminal rollback")
	}

	read, err := db.Rollbacks.Get(ctx, created.RollbackID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.State != domain.RollbackFailed || read.Failure != domain.RollbackFailureStop {
		t.Errorf("record is now %q/%q, want failed/stop", read.State, read.Failure)
	}
}

// TestACheckpointAndItsHostFactLandTogether.
//
// Recording "the replacement was parked" without recording WHERE would leave a
// recovery pass unable to describe the host, so both are written in one
// transaction.
func TestACheckpointAndItsHostFactLandTogether(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := db.Rollbacks.Create(ctx,
		rollbackFor("exec_00112233445566778899", rollbackStoreContainer), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
		RollbackID:  created.RollbackID,
		From:        []domain.RollbackState{domain.RollbackQueued},
		To:          domain.RollbackStoppingReplacement,
		MarkStarted: true,
	}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}

	parked := rollbackStoreContainer + domain.RollbackParkedNameSuffix + created.RollbackID
	recorded, err := db.Rollbacks.Checkpoint(ctx, store.RollbackCheckpointWrite{
		RollbackID:            created.RollbackID,
		Checkpoint:            domain.RollbackCheckpointReplacementParked,
		Detail:                "the replacement container was renamed aside",
		ReplacementParkedName: parked,
		MarkMutated:           true,
	}, now)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if !recorded {
		t.Fatal("the checkpoint did not land")
	}

	read, err := db.Rollbacks.Get(ctx, created.RollbackID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.Checkpoint != domain.RollbackCheckpointReplacementParked {
		t.Errorf("checkpoint = %q", read.Checkpoint)
	}
	if read.ReplacementParkedName != parked {
		t.Errorf("parked name = %q, want %q", read.ReplacementParkedName, parked)
	}
	if read.MutatedAt == nil {
		t.Error("mutatedAt was not stamped by the first checkpoint")
	}

	// The trail records both the transition and the checkpoint.
	events, err := db.Rollbacks.Events(ctx, created.RollbackID, 100)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) < 2 {
		t.Fatalf("%d events, want at least the transition and the checkpoint", len(events))
	}
}

// TestTheEventTrailIsBounded proves one rollback cannot grow an unbounded
// document in a response.
func TestTheEventTrailIsBounded(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := db.Rollbacks.Create(ctx,
		rollbackFor("exec_00112233445566778899", rollbackStoreContainer), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
		RollbackID: created.RollbackID,
		To:         domain.RollbackStoppingReplacement,
	}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}
	for i := 0; i < 40; i++ {
		if _, err := db.Rollbacks.Checkpoint(ctx, store.RollbackCheckpointWrite{
			RollbackID: created.RollbackID,
			Checkpoint: domain.RollbackCheckpointReplacementStopped,
			Detail:     "repeated",
		}, now); err != nil {
			t.Fatalf("checkpoint %d: %v", i, err)
		}
	}

	events, err := db.Rollbacks.Events(ctx, created.RollbackID, 1_000_000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) > 200 {
		t.Errorf("%d events returned for an unbounded limit; the cap is not applied",
			len(events))
	}
}

// -------------------------------------------------------------- queuing --

// TestOnlyQueuedRollbacksAreClaimableAndExpirable is the property that stops
// expiry abandoning a rollback that has begun changing the host.
func TestOnlyQueuedRollbacksAreClaimableAndExpirable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Hour)

	queued := rollbackFor("exec_00112233445566778899", "queued-container")
	queued.ExpiresAt = past
	if _, err := db.Rollbacks.Create(ctx, queued, past); err != nil {
		t.Fatalf("create queued: %v", err)
	}

	mutating := rollbackFor("exec_aaaaaaaaaaaaaaaaaaaa", "mutating-container")
	mutating.ExpiresAt = past
	created, err := db.Rollbacks.Create(ctx, mutating, past)
	if err != nil {
		t.Fatalf("create mutating: %v", err)
	}
	if _, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
		RollbackID: created.RollbackID,
		To:         domain.RollbackStoppingReplacement,
	}, past); err != nil {
		t.Fatalf("advance: %v", err)
	}

	claimable, err := db.Rollbacks.Claimable(ctx, 100)
	if err != nil {
		t.Fatalf("claimable: %v", err)
	}
	if len(claimable) != 1 || claimable[0].ContainerName != "queued-container" {
		t.Fatalf("claimable = %d rows, want only the queued one", len(claimable))
	}

	expired, err := db.Rollbacks.ExpireStale(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired %d, want 1", expired)
	}

	read, err := db.Rollbacks.Get(ctx, created.RollbackID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.State != domain.RollbackStoppingReplacement {
		t.Errorf("the mutating rollback was expired to %q", read.State)
	}
}

// TestInterruptedFindsEveryRollbackThatWasMidFlight.
func TestInterruptedFindsEveryRollbackThatWasMidFlight(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	states := []domain.RollbackState{
		domain.RollbackValidating,
		domain.RollbackStoppingReplacement,
		domain.RollbackRestoringName,
		domain.RollbackStartingOriginal,
		domain.RollbackVerifyingOriginal,
	}
	for i, state := range states {
		rollback := rollbackFor("exec_0011223344556677889"+string(rune('a'+i)),
			"container-"+string(rune('a'+i)))
		created, err := db.Rollbacks.Create(ctx, rollback, now)
		if err != nil {
			t.Fatalf("create %q: %v", state, err)
		}
		if _, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
			RollbackID: created.RollbackID,
			To:         state,
		}, now); err != nil {
			t.Fatalf("advance to %q: %v", state, err)
		}
	}
	// A queued one, which was never mid-flight.
	if _, err := db.Rollbacks.Create(ctx,
		rollbackFor("exec_ffffffffffffffffffff", "container-queued"), now); err != nil {
		t.Fatalf("create queued: %v", err)
	}

	interrupted, err := db.Rollbacks.Interrupted(ctx, 100)
	if err != nil {
		t.Fatalf("interrupted: %v", err)
	}
	if len(interrupted) != len(states) {
		t.Fatalf("%d interrupted, want %d (queued must not be included)",
			len(interrupted), len(states))
	}
	for _, rollback := range interrupted {
		if rollback.State == domain.RollbackQueued {
			t.Error("a queued rollback was reported as interrupted")
		}
	}
}

// TestTheActiveCountsExcludeTheRollbackBeingAssessed.
//
// The preflight runs a second time from inside the pipeline, where the rollback
// is itself active. Without the exclusion every rollback would refuse for
// conflicting with itself, and the feature would never mutate anything.
func TestTheActiveCountsExcludeTheRollbackBeingAssessed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := db.Rollbacks.Create(ctx,
		rollbackFor("exec_00112233445566778899", rollbackStoreContainer), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
		RollbackID: created.RollbackID,
		To:         domain.RollbackValidating,
	}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}

	count, err := db.Rollbacks.ActiveCount(ctx, "")
	if err != nil {
		t.Fatalf("active count: %v", err)
	}
	if count != 1 {
		t.Errorf("active count without exclusion = %d, want 1", count)
	}

	count, err = db.Rollbacks.ActiveCount(ctx, created.RollbackID)
	if err != nil {
		t.Fatalf("active count excluding self: %v", err)
	}
	if count != 0 {
		t.Errorf("active count excluding self = %d, want 0", count)
	}

	busy, err := db.Rollbacks.ActiveForContainer(ctx, rollbackStoreContainer, "")
	if err != nil {
		t.Fatalf("active for container: %v", err)
	}
	if !busy {
		t.Error("the container does not report an active rollback")
	}

	busy, err = db.Rollbacks.ActiveForContainer(ctx, rollbackStoreContainer, created.RollbackID)
	if err != nil {
		t.Fatalf("active for container excluding self: %v", err)
	}
	if busy {
		t.Error("a rollback reports itself as a conflicting rollback")
	}
}

// ------------------------------------------------------------ retention --

// TestRetentionNeverRemovesAFailureThatLeftContainers.
func TestRetentionNeverRemovesAFailureThatLeftContainers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-365 * 24 * time.Hour)

	// One clean success and one failure that changed the host, both ancient.
	clean, err := db.Rollbacks.Create(ctx, rollbackFor("exec_00112233445566778899", "clean"), old)
	if err != nil {
		t.Fatalf("create clean: %v", err)
	}
	if _, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
		RollbackID: clean.RollbackID,
		To:         domain.RollbackSucceeded,
		Checkpoint: domain.RollbackCheckpointOriginalVerified,
	}, old); err != nil {
		t.Fatalf("succeed: %v", err)
	}

	stranded, err := db.Rollbacks.Create(ctx, rollbackFor("exec_aaaaaaaaaaaaaaaaaaaa", "stranded"), old)
	if err != nil {
		t.Fatalf("create stranded: %v", err)
	}
	if _, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
		RollbackID: stranded.RollbackID,
		To:         domain.RollbackFailed,
		Failure:    domain.RollbackFailureStart,
		Checkpoint: domain.RollbackCheckpointOriginalRestored,
		Message:    "the original could not be started",
	}, old); err != nil {
		t.Fatalf("fail: %v", err)
	}

	if _, err := db.Rollbacks.Prune(ctx, time.Now().UTC(), 500); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if _, err := db.Rollbacks.Get(ctx, stranded.RollbackID); err != nil {
		t.Errorf("a failure that left containers on the host was pruned: %v", err)
	}
	if _, err := db.Rollbacks.Get(ctx, clean.RollbackID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the clean success survived retention: %v", err)
	}
}

// TestTheSummaryCountsWhatAnOperatorNeedsToSee.
func TestTheSummaryCountsWhatAnOperatorNeedsToSee(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	succeeded, err := db.Rollbacks.Create(ctx, rollbackFor("exec_00112233445566778899", "a"), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
		RollbackID: succeeded.RollbackID,
		To:         domain.RollbackSucceeded,
		Checkpoint: domain.RollbackCheckpointOriginalVerified,
	}, now); err != nil {
		t.Fatalf("succeed: %v", err)
	}

	stranded, err := db.Rollbacks.Create(ctx, rollbackFor("exec_aaaaaaaaaaaaaaaaaaaa", "b"), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
		RollbackID: stranded.RollbackID,
		To:         domain.RollbackFailed,
		Failure:    domain.RollbackFailureStart,
		Checkpoint: domain.RollbackCheckpointOriginalRestored,
	}, now); err != nil {
		t.Fatalf("fail: %v", err)
	}

	if _, err := db.Rollbacks.Create(ctx, rollbackFor("exec_bbbbbbbbbbbbbbbbbbbb", "c"), now); err != nil {
		t.Fatalf("create: %v", err)
	}

	summary, err := db.Rollbacks.Summary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Total != 3 || summary.Succeeded != 1 || summary.Failed != 1 || summary.Active != 1 {
		t.Errorf("summary = %+v, want 3/1 succeeded/1 failed/1 active", summary)
	}
	if summary.NeedsAttention != 1 {
		t.Errorf("needsAttention = %d, want 1", summary.NeedsAttention)
	}
}

// ---------------------------------------------------------- concurrency --

// TestConcurrentRollbackCreatesProduceOneWinner proves the constraint holds
// under a race rather than only in sequence.
func TestConcurrentRollbackCreatesProduceOneWinner(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Six, because the execution ids below are built from hex digits.
	const attempts = 6
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		refused   int
	)

	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(n int) {
			defer wg.Done()
			rollback := rollbackFor("exec_0011223344556677889"+string(rune('a'+n)),
				rollbackStoreContainer)
			_, err := db.Rollbacks.Create(ctx, rollback, now)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, store.ErrRollbackActive):
				refused++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if succeeded != 1 {
		t.Errorf("%d creates succeeded, want exactly 1", succeeded)
	}
	if refused != attempts-1 {
		t.Errorf("%d refused, want %d", refused, attempts-1)
	}
}

// ---------------------------------------------------------------- audit --

// TestEveryAuditTargetTypeTheCodeUsesCanActuallyBeRecorded.
//
// # Why this test exists
//
// `audit_events.target_type` carries a CHECK against a closed vocabulary, and
// the vocabulary lives in a migration while the constants live in the domain
// package. Phase 10 added `rollback` to the constants and not to the
// constraint, so every attempt to audit a rollback was refused by the database
// -- and the recorder, correctly, logs that failure and carries on rather than
// failing the operation it is describing.
//
// The result was a host-changing capability with no security-audit trail and
// nothing in the running system saying so. This test is what makes the next
// addition fail loudly instead.
func TestEveryAuditTargetTypeTheCodeUsesCanActuallyBeRecorded(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	targets := []domain.AuditTargetType{
		domain.AuditTargetUser, domain.AuditTargetSession,
		domain.AuditTargetContainer, domain.AuditTargetSnapshot,
		domain.AuditTargetDrift, domain.AuditTargetPolicy,
		domain.AuditTargetViolation, domain.AuditTargetPlan,
		domain.AuditTargetAcquisition, domain.AuditTargetExecution,
		domain.AuditTargetRollback, domain.AuditTargetInventory,
		domain.AuditTargetSystem,
	}

	for _, target := range targets {
		if err := db.Audit.Record(ctx, domain.AuditEvent{
			Action:     domain.AuditRollbackCompleted,
			Outcome:    domain.AuditSucceeded,
			TargetType: target,
			TargetID:   "identifier",
		}, now); err != nil {
			t.Errorf("target type %q cannot be recorded: %v", target, err)
		}
	}
}

// ------------------------------------------------------------- secrecy --

// TestNoRollbackColumnCanHoldASecret is the leakage guard at the storage
// layer.
func TestNoRollbackColumnCanHoldASecret(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := db.Rollbacks.Create(ctx,
		rollbackFor("exec_00112233445566778899", rollbackStoreContainer), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
		RollbackID: created.RollbackID,
		To:         domain.RollbackFailed,
		Failure:    domain.RollbackFailureStart,
		Checkpoint: domain.RollbackCheckpointOriginalRestored,
		Message:    domain.RollbackFailureStart.Explain(),
		Recovery: domain.BuildRollbackRecoveryPlan(domain.RollbackRecoveryContext{
			RollbackID:            created.RollbackID,
			ExecutionID:           created.ExecutionID,
			ContainerName:         rollbackStoreContainer,
			OriginalID:            created.OriginalID,
			ParkedName:            created.ParkedName,
			ReplacementID:         created.ReplacementID,
			ReplacementParkedName: rollbackStoreContainer + domain.RollbackParkedNameSuffix + created.RollbackID,
			Checkpoint:            domain.RollbackCheckpointOriginalRestored,
			Failure:               domain.RollbackFailureStart,
			MutationAttempted:     true,
		}),
	}, now); err != nil {
		t.Fatalf("fail: %v", err)
	}

	// Every text column of the row, concatenated.
	var dump string
	if err := db.SQL().QueryRowContext(ctx, `
		SELECT COALESCE(rollback_id,'') || COALESCE(execution_id,'') ||
		       COALESCE(container_name,'') || COALESCE(parked_name,'') ||
		       COALESCE(replacement_parked_name,'') || COALESCE(original_image,'') ||
		       COALESCE(original_image_id,'') || COALESCE(replacement_image,'') ||
		       COALESCE(message,'') || COALESCE(failure,'') || COALESCE(refusal,'') ||
		       COALESCE(preservation_report,'') || COALESCE(recovery_plan,'') ||
		       COALESCE(requested_by_username,'')
		FROM rollbacks WHERE rollback_id = ?`, created.RollbackID).Scan(&dump); err != nil {
		t.Fatalf("dump row: %v", err)
	}

	for _, forbidden := range []string{
		"hunter2", "password", "Authorization", "Bearer ",
		"/var/run/docker.sock", "Error response from daemon",
	} {
		if strings.Contains(dump, forbidden) {
			t.Errorf("a rollback row contains %q", forbidden)
		}
	}
}
