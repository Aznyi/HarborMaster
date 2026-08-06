package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// What these tests are about.
//
// Rollback is the highest-risk capability in the project: it stops a container
// an operator depends on and starts a different one in its place. Almost every
// test here is therefore a statement about one of three things --
//
//   - which host operations happened, and in what order;
//   - what was durably recorded between them;
//   - what HarborMaster refused to do, and why.
//
// A test that only asserted the final state would pass for a pipeline that
// stopped a container twice, or that renamed one without recording it. So the
// assertions are on the operation log and the checkpoint log, not just on the
// verdict.

// ------------------------------------------------------- the happy path --

// TestASuccessfulRollbackRestoresTheOriginalAndKeepsTheReplacement is the whole
// feature in one test.
func TestASuccessfulRollbackRestoresTheOriginalAndKeepsTheReplacement(t *testing.T) {
	harness := newRollbackHarness(t)

	final := harness.runOnce(t, harness.request(t))

	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state %q/%q/%q, want succeeded",
			final.State, final.Failure, final.Refusal)
	}

	// ---- the host ---------------------------------------------------------

	if name := harness.host.nameOf(rbOriginalID); name != rbContainerName {
		t.Errorf("the original is named %q, want %q", name, rbContainerName)
	}
	if !harness.host.running(rbOriginalID) {
		t.Error("the original is not running")
	}

	// The replacement is preserved. It is the evidence of why the recreation
	// was backed out, and the capability has no method that could remove it.
	if !harness.host.present(rbReplacementID) {
		t.Fatal("the replacement was removed; a rollback must preserve it")
	}
	if harness.host.running(rbReplacementID) {
		t.Error("the replacement is still running")
	}
	parked := harness.host.nameOf(rbReplacementID)
	if !strings.Contains(parked, domain.RollbackParkedNameSuffix) {
		t.Errorf("the replacement is named %q, want a rollback-parked name", parked)
	}
	if final.ReplacementParkedName != parked {
		t.Errorf("recorded parked name %q, host says %q", final.ReplacementParkedName, parked)
	}

	// ---- the order ---------------------------------------------------------
	//
	// Exactly four mutations, exactly once each. The order is the safety
	// property: the replacement must be off the name before the original can
	// take it back, and the original must hold its own name before it starts.
	want := []string{
		"stop:" + rbReplacementID,
		"park:" + parked,
		"restore:" + rbContainerName,
		"start:" + rbOriginalID,
	}
	if got := harness.host.operations(); !equalStrings(got, want) {
		t.Errorf("host operations %v, want %v", got, want)
	}

	// ---- what was recorded, and when ---------------------------------------

	wantCheckpoints := []domain.RollbackCheckpoint{
		domain.RollbackCheckpointReplacementStopped,
		domain.RollbackCheckpointReplacementParked,
		domain.RollbackCheckpointOriginalRestored,
		domain.RollbackCheckpointOriginalStarted,
		domain.RollbackCheckpointOriginalVerified,
	}
	if got := harness.store.checkpointsWritten(); !equalCheckpoints(got, wantCheckpoints) {
		t.Errorf("checkpoints %v, want %v", got, wantCheckpoints)
	}

	if !final.Verification.Passed() {
		t.Errorf("verification did not pass: %+v", final.Verification)
	}
	for name, verdict := range map[string]domain.VerificationResult{
		"health":       final.Verification.Health,
		"image":        final.Verification.Image,
		"preservation": final.Verification.Preservation,
		"network":      final.Verification.Network,
	} {
		if verdict != domain.VerificationPassed {
			t.Errorf("%s verdict %q, want passed", name, verdict)
		}
	}
	if final.MutatedAt == nil {
		t.Error("mutatedAt was never stamped")
	}
	if final.Recovery != nil {
		t.Error("a successful rollback carries a recovery plan")
	}
}

// TestTheLifecycleIsWalkedInOrder pins the state sequence.
func TestTheLifecycleIsWalkedInOrder(t *testing.T) {
	harness := newRollbackHarness(t)
	harness.runOnce(t, harness.request(t))

	want := []domain.RollbackState{
		domain.RollbackQueued,
		domain.RollbackValidating,
		domain.RollbackStoppingReplacement,
		domain.RollbackRestoringName,
		domain.RollbackStartingOriginal,
		domain.RollbackVerifyingOriginal,
		domain.RollbackSucceeded,
	}
	if got := harness.store.statesWritten(); !equalStates(got, want) {
		t.Errorf("states %v, want %v", got, want)
	}
}

// ----------------------------------------------------------- eligibility --

// TestEveryEligibilityRefusal walks each reason a rollback is refused.
//
// One table, because the point is coverage: an operator must get a specific
// refusal for each, and each must leave the host untouched.
func TestEveryEligibilityRefusal(t *testing.T) {
	cases := []struct {
		name  string
		world func(*rbHarness)
		want  domain.RollbackRefusal
	}{
		{
			name: "the execution is not recorded",
			world: func(h *rbHarness) {
				h.evidence.execution.ExecutionID = "exec_ffffffffffffffffffff"
			},
			want: domain.RollbackRefusalExecutionMissing,
		},
		{
			name: "the recreation is still running",
			world: func(h *rbHarness) {
				h.evidence.execution.State = domain.ExecutionStarting
			},
			want: domain.RollbackRefusalExecutionActive,
		},
		{
			name: "the recreation never changed anything",
			world: func(h *rbHarness) {
				h.evidence.execution.Checkpoint = domain.CheckpointNone
				h.evidence.execution.MutatedAt = nil
			},
			want: domain.RollbackRefusalNothingToRollBack,
		},
		{
			name: "the recreation stopped short of parking the original",
			world: func(h *rbHarness) {
				h.evidence.execution.Checkpoint = domain.CheckpointOriginalStopped
			},
			want: domain.RollbackRefusalNothingToRollBack,
		},
		{
			name: "a mutation was issued and never confirmed",
			world: func(h *rbHarness) {
				h.evidence.execution.Checkpoint = domain.CheckpointNone
			},
			want: domain.RollbackRefusalCheckpointUncertain,
		},
		{
			name: "the original was already removed",
			world: func(h *rbHarness) {
				h.evidence.execution.OriginalRemoved = true
			},
			want: domain.RollbackRefusalOriginalRemoved,
		},
		{
			name: "the recreation removed the original as its last act",
			world: func(h *rbHarness) {
				h.evidence.execution.Checkpoint = domain.CheckpointOriginalRemoved
			},
			want: domain.RollbackRefusalOriginalRemoved,
		},
		{
			name: "the record names only one container",
			world: func(h *rbHarness) {
				h.evidence.execution.ReplacementID = ""
			},
			want: domain.RollbackRefusalNothingToRollBack,
		},
		{
			name: "this execution has already been rolled back",
			world: func(h *rbHarness) {
				h.store.seed(domain.Rollback{
					RollbackID:    domain.NewRollbackID(),
					ExecutionID:   rbExecutionID,
					ContainerName: rbContainerName,
					State:         domain.RollbackSucceeded,
				})
			},
			want: domain.RollbackRefusalAlreadyRolledBack,
		},
		{
			name: "another rollback of this container is in flight",
			world: func(h *rbHarness) {
				h.store.seed(domain.Rollback{
					RollbackID:    domain.NewRollbackID(),
					ExecutionID:   "exec_aaaaaaaaaaaaaaaaaaaa",
					ContainerName: rbContainerName,
					State:         domain.RollbackStoppingReplacement,
				})
			},
			want: domain.RollbackRefusalConflict,
		},
		{
			name: "a recreation of this container is in flight",
			world: func(h *rbHarness) {
				h.evidence.activeExecution = true
			},
			want: domain.RollbackRefusalConflict,
		},
		{
			name: "the concurrency limit is reached",
			world: func(h *rbHarness) {
				h.store.seed(domain.Rollback{
					RollbackID:    domain.NewRollbackID(),
					ExecutionID:   "exec_bbbbbbbbbbbbbbbbbbbb",
					ContainerName: "another-container",
					State:         domain.RollbackStartingOriginal,
				})
			},
			want: domain.RollbackRefusalLimit,
		},
		{
			name: "the daemon is unreachable",
			world: func(h *rbHarness) {
				h.host.pingErr = errRollbackDaemonGone
			},
			want: domain.RollbackRefusalDockerUnavailable,
		},
		{
			name: "the host picture is stale",
			world: func(h *rbHarness) {
				h.evidence.inventoryAge = 48 * time.Hour
			},
			want: domain.RollbackRefusalInventoryStale,
		},
		{
			name: "the host has never been inventoried",
			world: func(h *rbHarness) {
				h.evidence.inventoryKnown = false
			},
			want: domain.RollbackRefusalInventoryStale,
		},
		{
			name: "the preserved original is gone",
			world: func(h *rbHarness) {
				h.host.remove(rbOriginalID)
			},
			want: domain.RollbackRefusalOriginalMissing,
		},
		{
			name: "somebody renamed the preserved original",
			world: func(h *rbHarness) {
				h.host.with(rbOriginalID, func(c *rbContainer) {
					c.detail.Overview.Name = "someone-elses-name"
				})
			},
			want: domain.RollbackRefusalOriginalIdentity,
		},
		{
			name: "the preserved original is on a different image",
			world: func(h *rbHarness) {
				h.host.with(rbOriginalID, func(c *rbContainer) {
					c.detail.Overview.ImageID = "sha256:" + strings.Repeat("c", 64)
				})
			},
			want: domain.RollbackRefusalOriginalIdentity,
		},
		{
			name: "the replacement is gone",
			world: func(h *rbHarness) {
				h.host.remove(rbReplacementID)
			},
			want: domain.RollbackRefusalReplacementMissing,
		},
		{
			name: "somebody renamed the replacement",
			world: func(h *rbHarness) {
				h.host.with(rbReplacementID, func(c *rbContainer) {
					c.detail.Overview.Name = "not-a-harbormaster-name"
				})
			},
			want: domain.RollbackRefusalReplacementIdentity,
		},
		{
			name: "a third container holds the production name",
			world: func(h *rbHarness) {
				h.host.with(rbReplacementID, func(c *rbContainer) {
					c.detail.Overview.Name = rbContainerName + domain.QuarantineNameSuffix + rbExecutionID
				})
				stranger := rbReplacementDetail()
				stranger.Overview.ID = rbStrangerID
				stranger.Overview.ShortID = domain.ShortenID(rbStrangerID)
				stranger.Overview.Name = rbContainerName
				h.host.add(&rbContainer{detail: stranger})
			},
			want: domain.RollbackRefusalNameUnavailable,
		},
		{
			name: "the container listing cannot be read",
			world: func(h *rbHarness) {
				h.host.listErr = errRollbackDaemonGone
			},
			want: domain.RollbackRefusalDockerUnavailable,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newRollbackHarness(t, func(h *rbHarness) { testCase.world(h) })

			if got := harness.refusal(t); got != testCase.want {
				t.Errorf("refusal %q, want %q", got, testCase.want)
			}
			if ops := harness.host.operations(); len(ops) != 0 {
				t.Errorf("a refused rollback touched the host: %v", ops)
			}
		})
	}
}

// TestARollbackIsRefusedWithoutTheCapability covers the two ways the feature
// can be off.
func TestARollbackIsRefusedWithoutTheCapability(t *testing.T) {
	cases := []struct {
		name    string
		options func(*service.RollbackOptions)
	}{
		{
			name:    "the configuration flag is off",
			options: func(o *service.RollbackOptions) { o.Config.Enabled = false },
		},
		{
			name: "the process was not handed the capability",
			options: func(o *service.RollbackOptions) {
				// The flag is on, but the interface is nil. A deployment that
				// set the flag without the capability has not got it, and
				// offering the button would be offering one that cannot work.
				o.Rollbacker = nil
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newRollbackHarness(t)

			options := service.RollbackOptions{
				Store:      harness.store,
				Evidence:   harness.evidence,
				Runtime:    harness.host,
				Rollbacker: harness.host,
				Hasher:     newTestHasher(t),
				Config:     config.Rollback{Enabled: true, MaxConcurrent: 1},
				Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
				Now:        harness.now,
			}
			testCase.options(&options)
			svc := service.NewRollbackService(options)

			if svc.Enabled() {
				t.Fatal("the service reports itself enabled")
			}
			_, err := svc.Request(context.Background(),
				service.RollbackRequest{ExecutionID: rbExecutionID})

			var refused service.RollbackRefusedError
			if !errors.As(err, &refused) || refused.Refusal != domain.RollbackRefusalDisabled {
				t.Fatalf("error %v, want a disabled refusal", err)
			}
			if ops := harness.host.operations(); len(ops) != 0 {
				t.Errorf("a disabled service touched the host: %v", ops)
			}
		})
	}
}

// TestARollbackWithNoHasherIsRefusedAsUnverifiable proves the fail-closed
// reading of a missing installation key.
//
// Without one the preservation comparison cannot be made at all. A rollback
// that could never be PROVED is refused rather than run unprovable, which is
// the difference between "the check failed" and "the check was not possible".
func TestARollbackWithNoHasherIsRefusedAsUnverifiable(t *testing.T) {
	harness := newRollbackHarness(t)

	svc := service.NewRollbackService(service.RollbackOptions{
		Store:      harness.store,
		Evidence:   harness.evidence,
		Runtime:    harness.host,
		Rollbacker: harness.host,
		Hasher:     nil,
		Config: config.Rollback{
			Enabled: true, MaxConcurrent: 1, InventoryFreshness: 15 * time.Minute,
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    harness.now,
	})

	_, err := svc.Request(context.Background(),
		service.RollbackRequest{ExecutionID: rbExecutionID})

	var refused service.RollbackRefusedError
	if !errors.As(err, &refused) || refused.Refusal != domain.RollbackRefusalUnverifiable {
		t.Fatalf("error %v, want an unverifiable refusal", err)
	}
	if ops := harness.host.operations(); len(ops) != 0 {
		t.Errorf("an unverifiable rollback touched the host: %v", ops)
	}
}

// TestAMalformedExecutionIDIsRefusedWithoutReadingAnything proves the id is
// validated before it reaches the evidence layer.
func TestAMalformedExecutionIDIsRefusedWithoutReadingAnything(t *testing.T) {
	harness := newRollbackHarness(t)

	for _, id := range []string{"", "exec_", "../../etc/passwd", "exec_zzzzzzzzzzzzzzzzzzzz",
		"exec_00112233445566778899 OR 1=1"} {
		_, err := harness.service.Request(context.Background(),
			service.RollbackRequest{ExecutionID: id})

		var refused service.RollbackRefusedError
		if !errors.As(err, &refused) ||
			refused.Refusal != domain.RollbackRefusalExecutionMissing {
			t.Errorf("id %q gave %v, want an executionMissing refusal", id, err)
		}
	}
}

// TestEligibilityNeverTouchesTheDockerSocket is a denial-of-service property.
//
// The eligibility answer is served from the execution detail endpoint, which
// the UI POLLS. If computing it reached the daemon, an authenticated reader
// could turn one HTTP request into a ping, two inspections, and a container
// listing, repeatable at will — an amplifier pointed at a privileged socket.
//
// So the advisory answer reads records only. Nothing is lost: the request path
// runs the full assessment, and the pipeline runs it again against the live
// host immediately before the first mutation.
func TestEligibilityNeverTouchesTheDockerSocket(t *testing.T) {
	harness := newRollbackHarness(t)

	for i := 0; i < 20; i++ {
		if _, err := harness.service.Eligible(context.Background(), rbExecutionID); err != nil {
			t.Fatalf("eligible: %v", err)
		}
	}

	if calls := harness.host.daemonCalls(); calls != 0 {
		t.Errorf("%d Docker calls from %d eligibility reads; a read endpoint that "+
			"can drive the socket is a denial-of-service amplifier", calls, 20)
	}

	// The REQUEST path does reach the daemon, which is what makes the absence
	// above a deliberate split rather than a check that was forgotten.
	harness.request(t)
	if calls := harness.host.daemonCalls(); calls == 0 {
		t.Error("the request path made no Docker call, so it cannot have verified " +
			"the two container identities")
	}
}

// TestEligibilityAgreesWithTheRequest proves the advisory answer and the
// enforced one come from the same assessment.
func TestEligibilityAgreesWithTheRequest(t *testing.T) {
	t.Run("eligible", func(t *testing.T) {
		harness := newRollbackHarness(t)

		eligibility, err := harness.service.Eligible(context.Background(), rbExecutionID)
		if err != nil {
			t.Fatalf("eligible: %v", err)
		}
		if !eligibility.Eligible {
			t.Fatalf("not eligible: %q", eligibility.Refusal)
		}
		// The identities a confirmation dialogue must show, all derived.
		if eligibility.OriginalID != rbOriginalID ||
			eligibility.ReplacementID != rbReplacementID ||
			eligibility.ContainerName != rbContainerName ||
			eligibility.ParkedName != rbParkedName {
			t.Errorf("eligibility identities are wrong: %+v", eligibility)
		}
	})

	t.Run("refused", func(t *testing.T) {
		harness := newRollbackHarness(t, func(h *rbHarness) {
			h.evidence.execution.OriginalRemoved = true
		})

		eligibility, err := harness.service.Eligible(context.Background(), rbExecutionID)
		if err != nil {
			t.Fatalf("eligible: %v", err)
		}
		if eligibility.Eligible {
			t.Fatal("eligible, want refused")
		}
		if eligibility.Refusal != domain.RollbackRefusalOriginalRemoved {
			t.Errorf("refusal %q, want originalRemoved", eligibility.Refusal)
		}
		if eligibility.Reason == "" {
			t.Error("a refusal with no operator-facing reason")
		}

		// And the request refuses for the same reason.
		if got := harness.refusal(t); got != eligibility.Refusal {
			t.Errorf("request refused with %q, eligibility said %q", got, eligibility.Refusal)
		}
	})
}

// ------------------------------------------------------------ duplicates --

// TestASecondRollbackOfTheSameContainerIsRefused covers the conflict index.
func TestASecondRollbackOfTheSameContainerIsRefused(t *testing.T) {
	harness := newRollbackHarness(t)
	harness.request(t)

	if got := harness.refusal(t); got != domain.RollbackRefusalConflict {
		t.Errorf("second request refused with %q, want conflict", got)
	}
}

// TestARepeatedRequestKeyReturnsTheSameRollback proves a double-clicked button
// does not start two rollbacks.
func TestARepeatedRequestKeyReturnsTheSameRollback(t *testing.T) {
	harness := newRollbackHarness(t)

	first, err := harness.service.Request(context.Background(), service.RollbackRequest{
		ExecutionID: rbExecutionID,
		RequestKey:  "idem-0001",
	})
	if err != nil {
		t.Fatalf("first request: %v", err)
	}

	second, err := harness.service.Request(context.Background(), service.RollbackRequest{
		ExecutionID: rbExecutionID,
		RequestKey:  "idem-0001",
	})
	if err != nil {
		t.Fatalf("second request: %v", err)
	}

	if second.RollbackID != first.RollbackID {
		t.Errorf("second request made %q, want the same rollback %q",
			second.RollbackID, first.RollbackID)
	}
}

// TestARollbackCannotBeRunTwice proves a succeeded execution stays rolled back.
func TestARollbackCannotBeRunTwice(t *testing.T) {
	harness := newRollbackHarness(t)

	final := harness.runOnce(t, harness.request(t))
	if final.State != domain.RollbackSucceeded {
		t.Fatalf("first rollback %q, want succeeded", final.State)
	}
	before := len(harness.host.operations())

	if got := harness.refusal(t); got != domain.RollbackRefusalAlreadyRolledBack {
		t.Errorf("second rollback refused with %q, want alreadyRolledBack", got)
	}
	if after := len(harness.host.operations()); after != before {
		t.Errorf("the refused second rollback ran %d more operations", after-before)
	}
}

// --------------------------------------------------------- cancellation --

// TestCancellingBeforeTheMutationPointChangesNothing is the case an operator
// is entitled to.
func TestCancellingBeforeTheMutationPointChangesNothing(t *testing.T) {
	harness := newRollbackHarness(t)
	rollback := harness.request(t)

	cancelled, err := harness.service.Cancel(context.Background(), rollback.RollbackID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.State != domain.RollbackCancelled {
		t.Fatalf("state %q, want cancelled", cancelled.State)
	}
	if ops := harness.host.operations(); len(ops) != 0 {
		t.Errorf("a cancelled rollback touched the host: %v", ops)
	}
	if cancelled.MutatedAt != nil {
		t.Error("a cancelled rollback stamped mutatedAt")
	}

	// And the worker leaves it alone: a cancelled row is not claimable.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	harness.service.Run(ctx)

	if ops := harness.host.operations(); len(ops) != 0 {
		t.Errorf("the worker ran a cancelled rollback: %v", ops)
	}
}

// TestCancellingAfterTheMutationPointIsRefused is the case an operator is not.
//
// A rollback that has begun moving containers must reach a recorded
// conclusion. Abandoning it partway would leave a container neither running nor
// recorded as down, which is precisely the arrangement this feature exists to
// get out of.
func TestCancellingAfterTheMutationPointIsRefused(t *testing.T) {
	for _, state := range []domain.RollbackState{
		domain.RollbackStoppingReplacement,
		domain.RollbackRestoringName,
		domain.RollbackStartingOriginal,
		domain.RollbackVerifyingOriginal,
	} {
		t.Run(string(state), func(t *testing.T) {
			harness := newRollbackHarness(t)
			id := domain.NewRollbackID()
			harness.store.seed(domain.Rollback{
				RollbackID:    id,
				ExecutionID:   rbExecutionID,
				ContainerName: rbContainerName,
				OriginalID:    rbOriginalID,
				ReplacementID: rbReplacementID,
				State:         state,
				Checkpoint:    domain.RollbackCheckpointReplacementStopped,
			})

			_, err := harness.service.Cancel(context.Background(), id)
			if !errors.Is(err, service.ErrRollbackNotCancellable) {
				t.Errorf("cancel in %q gave %v, want ErrRollbackNotCancellable", state, err)
			}
		})
	}
}

// TestCancellingAFinishedRollbackIsANoop proves cancel is idempotent rather
// than an error at the end.
func TestCancellingAFinishedRollbackIsANoop(t *testing.T) {
	harness := newRollbackHarness(t)
	final := harness.runOnce(t, harness.request(t))

	after, err := harness.service.Cancel(context.Background(), final.RollbackID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if after.State != domain.RollbackSucceeded {
		t.Errorf("cancel changed a finished rollback to %q", after.State)
	}
}

// TestCancellingAnUnknownRollbackIsNotFound proves a malformed or absent id
// cannot be used to probe.
func TestCancellingAnUnknownRollbackIsNotFound(t *testing.T) {
	harness := newRollbackHarness(t)

	for _, id := range []string{"", "rbk_", "../admin", "rbk_zzzzzzzzzzzzzzzzzzzz"} {
		if _, err := harness.service.Cancel(context.Background(), id); err == nil {
			t.Errorf("cancel of %q was accepted", id)
		}
	}
}

// ------------------------------------------------- persistence failures --

// TestACheckpointThatCannotBeWrittenStopsThePipeline is the most important
// property in the file.
//
// HarborMaster has changed the host and cannot prove it recorded the fact. It
// stops. It does not retry the mutation, and it does not proceed to the next
// one: repeating a stop, a rename, or a start against a host whose recorded
// state is uncertain is how a recoverable situation becomes an unrecoverable
// one.
func TestACheckpointThatCannotBeWrittenStopsThePipeline(t *testing.T) {
	cases := []struct {
		checkpoint domain.RollbackCheckpoint
		// wantOps is how many host mutations should have run before the stop.
		wantOps int
	}{
		{domain.RollbackCheckpointReplacementStopped, 1},
		{domain.RollbackCheckpointReplacementParked, 2},
		{domain.RollbackCheckpointOriginalRestored, 3},
		{domain.RollbackCheckpointOriginalStarted, 4},
	}

	for _, testCase := range cases {
		t.Run(string(testCase.checkpoint), func(t *testing.T) {
			harness := newRollbackHarness(t, func(h *rbHarness) {
				h.store.checkpointErrAt = testCase.checkpoint
			})

			final := harness.runOnce(t, harness.request(t))

			if final.State != domain.RollbackFailed {
				t.Fatalf("state %q, want failed", final.State)
			}
			if final.Failure != domain.RollbackFailurePersistence {
				t.Errorf("failure %q, want persistence", final.Failure)
			}

			ops := harness.host.operations()
			if len(ops) != testCase.wantOps {
				t.Fatalf("%d host operations (%v), want %d", len(ops), ops, testCase.wantOps)
			}
			// No operation was issued twice. That is what "never repeat an
			// uncertain mutation" means in practice.
			seen := make(map[string]int, len(ops))
			for _, op := range ops {
				seen[op]++
				if seen[op] > 1 {
					t.Errorf("operation %q was issued %d times", op, seen[op])
				}
			}

			// Both containers are still there for an operator to settle.
			if !harness.host.present(rbOriginalID) || !harness.host.present(rbReplacementID) {
				t.Error("a failed rollback did not preserve both containers")
			}
			if final.Recovery == nil {
				t.Fatal("a failure that changed the host carries no recovery plan")
			}
			if len(final.Recovery.Steps) == 0 {
				t.Error("the recovery plan has no steps")
			}
		})
	}
}

// TestAStateTransitionThatCannotBeWrittenStopsThePipeline covers the intent
// writes, as distinct from the checkpoints.
func TestAStateTransitionThatCannotBeWrittenStopsThePipeline(t *testing.T) {
	t.Run("before the first mutation", func(t *testing.T) {
		harness := newRollbackHarness(t, func(h *rbHarness) {
			h.store.advanceErrTo = domain.RollbackStoppingReplacement
		})

		final := harness.runOnce(t, harness.request(t))

		if final.Failure != domain.RollbackFailurePersistence {
			t.Errorf("failure %q, want persistence", final.Failure)
		}
		if ops := harness.host.operations(); len(ops) != 0 {
			t.Errorf("the host was touched although the intent went unrecorded: %v", ops)
		}
		if final.MutatedAt != nil {
			t.Error("mutatedAt was stamped although nothing was mutated")
		}
	})

	t.Run("after the first mutation", func(t *testing.T) {
		harness := newRollbackHarness(t, func(h *rbHarness) {
			h.store.advanceErrTo = domain.RollbackStartingOriginal
		})

		final := harness.runOnce(t, harness.request(t))

		if final.Failure != domain.RollbackFailurePersistence {
			t.Errorf("failure %q, want persistence", final.Failure)
		}
		// Stopped, parked, restored -- and stopped there rather than starting
		// the original on the strength of an unrecorded intent.
		if ops := harness.host.operations(); len(ops) != 3 {
			t.Errorf("%d host operations (%v), want 3", len(ops), ops)
		}
		if final.Recovery == nil {
			t.Error("no recovery plan after a failure that changed the host")
		}
	})
}

// ------------------------------------------------------- Docker failures --

// TestADaemonThatGoesAwayMidRollbackIsRecordedAsSuch proves an unreachable
// daemon is told apart from an operation that failed.
func TestADaemonThatGoesAwayMidRollbackIsRecordedAsSuch(t *testing.T) {
	cases := []struct {
		name    string
		inject  func(*rollbackHost)
		wantOps int
	}{
		{
			name:    "the stop cannot be issued",
			inject:  func(h *rollbackHost) { h.stopErr = errRollbackDaemonGone },
			wantOps: 1,
		},
		{
			name:    "the park cannot be issued",
			inject:  func(h *rollbackHost) { h.parkErr = errRollbackDaemonGone },
			wantOps: 2,
		},
		{
			name:    "the restore cannot be issued",
			inject:  func(h *rollbackHost) { h.restoreErr = errRollbackDaemonGone },
			wantOps: 3,
		},
		{
			name:    "the start cannot be issued",
			inject:  func(h *rollbackHost) { h.startErr = errRollbackDaemonGone },
			wantOps: 4,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newRollbackHarness(t, func(h *rbHarness) { testCase.inject(h.host) })

			final := harness.runOnce(t, harness.request(t))

			if final.State != domain.RollbackFailed {
				t.Fatalf("state %q, want failed", final.State)
			}
			if final.Failure != domain.RollbackFailureDockerUnavailable {
				t.Errorf("failure %q, want dockerUnavailable", final.Failure)
			}
			if ops := harness.host.operations(); len(ops) != testCase.wantOps {
				t.Errorf("%d host operations (%v), want %d", len(ops), ops, testCase.wantOps)
			}
			if !harness.host.present(rbOriginalID) || !harness.host.present(rbReplacementID) {
				t.Error("a failed rollback did not preserve both containers")
			}
			if final.Recovery == nil {
				t.Fatal("no recovery plan")
			}
			// HarborMaster does not guess which container should serve traffic.
			for _, step := range final.Recovery.Steps {
				if strings.Contains(strings.ToLower(step.Description), "harbormaster will") {
					t.Errorf("the recovery plan promises automatic action: %q", step.Description)
				}
			}
		})
	}
}

// TestARenameCollisionIsARollbackFailureNotACorrection proves HarborMaster
// stops rather than trying to put things back.
func TestARenameCollisionIsARollbackFailureNotACorrection(t *testing.T) {
	harness := newRollbackHarness(t)

	// A third container takes the production name after the preflight passed
	// but before the restore. The rename will collide.
	harness.host.with(rbReplacementID, func(c *rbContainer) {})
	rollback := harness.request(t)

	stranger := rbReplacementDetail()
	stranger.Overview.ID = rbStrangerID
	stranger.Overview.ShortID = domain.ShortenID(rbStrangerID)
	stranger.Overview.Name = "interloper"
	harness.host.add(&rbContainer{detail: stranger})
	harness.host.setErr(&harness.host.restoreErr, docker.ErrNameConflict)

	final := harness.runOnce(t, rollback)

	if final.State != domain.RollbackFailed {
		t.Fatalf("state %q, want failed", final.State)
	}
	if final.Failure != domain.RollbackFailureRename {
		t.Errorf("failure %q, want rename", final.Failure)
	}
	// Three operations, and no attempt to undo the first two.
	if ops := harness.host.operations(); len(ops) != 3 {
		t.Errorf("%d host operations (%v), want 3", len(ops), ops)
	}
}

// --------------------------------------------------------- verification --

// TestAnOriginalThatComesBackUnhealthyFailsTheRollback proves a rollback is not
// recorded as succeeded just because the containers moved.
func TestAnOriginalThatComesBackUnhealthyFailsTheRollback(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.host.with(rbOriginalID, func(c *rbContainer) {
			c.healthOnStart = domain.HealthUnhealthy
		})
	})

	final := harness.runOnce(t, harness.request(t))

	if final.State != domain.RollbackFailed {
		t.Fatalf("state %q, want failed", final.State)
	}
	if final.Failure != domain.RollbackFailureUnhealthy {
		t.Errorf("failure %q, want unhealthy", final.Failure)
	}
	if final.Verification.Health != domain.VerificationFailed {
		t.Errorf("health verdict %q, want failed", final.Verification.Health)
	}
	// The proofs below health were never reached, and say so rather than
	// reading as "nothing to report".
	if final.Verification.Image != domain.VerificationUnknown {
		t.Errorf("image verdict %q, want unknown", final.Verification.Image)
	}
	if final.Verification.Passed() {
		t.Error("an unhealthy original passed verification")
	}
	if !harness.host.present(rbReplacementID) {
		t.Error("the replacement was not preserved")
	}
}

// TestAnOriginalThatFallsOverIsNotStable covers the no-health-check path.
func TestAnOriginalThatFallsOverIsNotStable(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.host.with(rbOriginalID, func(c *rbContainer) {
			// No declared health check, so it is held to the stability window
			// -- and it exits the moment it is started.
			c.detail.HealthCheck = nil
			c.exitOnStart = true
		})
	})

	final := harness.runOnce(t, harness.request(t))

	if final.Failure != domain.RollbackFailureNotStable {
		t.Errorf("failure %q, want notStable", final.Failure)
	}
	if final.Verification.HealthChecked {
		t.Error("recorded as health-checked although none was declared")
	}
}

// TestARestoredOriginalThatIsNotWhatItWasFailsPreservation proves the
// before/after comparison is real.
func TestARestoredOriginalThatIsNotWhatItWasFailsPreservation(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.host.with(rbOriginalID, func(c *rbContainer) {
			c.onStart = func(started *rbContainer) {
				// It came back with its hardening removed. Something changed
				// it, and a rollback that reported success would be certifying
				// a container that is not the one it preserved.
				started.detail.Security.ReadonlyRootfs = false
				started.detail.Security.CapDrop = nil
			}
		})
	})

	final := harness.runOnce(t, harness.request(t))

	if final.Failure != domain.RollbackFailurePreservation {
		t.Fatalf("failure %q, want preservation", final.Failure)
	}
	if final.Verification.Preservation != domain.VerificationFailed {
		t.Errorf("preservation verdict %q, want failed", final.Verification.Preservation)
	}
	if final.Verification.Passed() {
		t.Error("a changed original passed verification")
	}
}

// TestARestoredOriginalOnTheWrongImageFailsTheImageProof.
func TestARestoredOriginalOnTheWrongImageFailsTheImageProof(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.host.with(rbOriginalID, func(c *rbContainer) {
			c.onStart = func(started *rbContainer) {
				started.detail.Overview.ImageID = "sha256:" + strings.Repeat("d", 64)
			}
		})
	})

	final := harness.runOnce(t, harness.request(t))

	if final.Failure != domain.RollbackFailureImageMismatch {
		t.Errorf("failure %q, want imageMismatch", final.Failure)
	}
	if final.Verification.Image != domain.VerificationFailed {
		t.Errorf("image verdict %q, want failed", final.Verification.Image)
	}
}

// TestARestoredOriginalOffItsNetworksFailsTheNetworkProof.
func TestARestoredOriginalOffItsNetworksFailsTheNetworkProof(t *testing.T) {
	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.host.with(rbOriginalID, func(c *rbContainer) {
			c.onStart = func(started *rbContainer) {
				started.detail.Networks = []domain.NetworkAttachment{
					{NetworkName: "somewhere-else"},
				}
			}
		})
	})

	final := harness.runOnce(t, harness.request(t))

	// The projection covers networks too, so preservation fails first. Either
	// verdict is a refusal to record success; what must not happen is a pass.
	if final.State != domain.RollbackFailed {
		t.Fatalf("state %q, want failed", final.State)
	}
	if final.Verification.Passed() {
		t.Error("a container on the wrong networks passed verification")
	}
	if final.Verification.Network == domain.VerificationPassed {
		t.Error("the network proof passed for a container that lost its network")
	}
}

// ------------------------------------------------------ restart recovery --

// TestARestartSettlesAnInterruptedRollbackFromEveryCheckpoint proves recovery
// covers the whole lifecycle rather than the tidy cases.
func TestARestartSettlesAnInterruptedRollbackFromEveryCheckpoint(t *testing.T) {
	cases := []struct {
		state      domain.RollbackState
		checkpoint domain.RollbackCheckpoint
		// wantAttention is whether the operator must settle containers.
		wantAttention bool
	}{
		{domain.RollbackValidating, domain.RollbackCheckpointNone, false},
		{domain.RollbackStoppingReplacement, domain.RollbackCheckpointNone, true},
		{domain.RollbackStoppingReplacement, domain.RollbackCheckpointReplacementStopped, true},
		{domain.RollbackRestoringName, domain.RollbackCheckpointReplacementParked, true},
		{domain.RollbackStartingOriginal, domain.RollbackCheckpointOriginalRestored, true},
		// The last one is deliberately NOT an interruption of service. The
		// original is running under its own name, so the container is serving;
		// what was interrupted is the PROOF that it is serving correctly, and
		// telling an operator their site is down when it is up would send them
		// to the wrong emergency.
		{domain.RollbackVerifyingOriginal, domain.RollbackCheckpointOriginalStarted, false},
	}

	for _, testCase := range cases {
		name := string(testCase.state) + "/" + string(testCase.checkpoint)
		t.Run(name, func(t *testing.T) {
			harness := newRollbackHarness(t)
			id := domain.NewRollbackID()
			harness.store.seed(domain.Rollback{
				RollbackID:            id,
				ExecutionID:           rbExecutionID,
				ContainerName:         rbContainerName,
				OriginalID:            rbOriginalID,
				ParkedName:            rbParkedName,
				ReplacementID:         rbReplacementID,
				ReplacementParkedName: rbContainerName + domain.RollbackParkedNameSuffix + id,
				State:                 testCase.state,
				Checkpoint:            testCase.checkpoint,
				RequestedAt:           harness.now(),
				ExpiresAt:             harness.now().Add(time.Hour),
			})

			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			harness.service.Run(ctx)

			final, err := harness.store.Get(context.Background(), id)
			if err != nil {
				t.Fatalf("get: %v", err)
			}

			if final.State != domain.RollbackFailed {
				t.Fatalf("state %q, want failed", final.State)
			}
			if final.Failure != domain.RollbackFailureInterrupted {
				t.Errorf("failure %q, want interrupted", final.Failure)
			}
			// The checkpoint is preserved. It is the only durable statement
			// about where the containers actually are.
			if final.Checkpoint != testCase.checkpoint {
				t.Errorf("checkpoint %q, want %q", final.Checkpoint, testCase.checkpoint)
			}
			if final.Recovery == nil {
				t.Fatal("no recovery plan")
			}
			if final.Recovery.ServiceInterrupted != testCase.wantAttention {
				t.Errorf("serviceInterrupted %v, want %v",
					final.Recovery.ServiceInterrupted, testCase.wantAttention)
			}

			// Recovery settles records. It never touches the host: a process
			// that has just started has no idea what happened while it was
			// down, and acting on that would be the unattended mutation this
			// design exists to avoid.
			if ops := harness.host.operations(); len(ops) != 0 {
				t.Errorf("restart recovery touched the host: %v", ops)
			}
		})
	}
}

// TestRecoveryRunsBeforeAnythingNewIsClaimed proves the ordering.
func TestRecoveryRunsBeforeAnythingNewIsClaimed(t *testing.T) {
	harness := newRollbackHarness(t)

	interrupted := domain.NewRollbackID()
	harness.store.seed(domain.Rollback{
		RollbackID:    interrupted,
		ExecutionID:   rbExecutionID,
		ContainerName: rbContainerName,
		OriginalID:    rbOriginalID,
		ReplacementID: rbReplacementID,
		State:         domain.RollbackRestoringName,
		Checkpoint:    domain.RollbackCheckpointReplacementParked,
		RequestedAt:   harness.now(),
		ExpiresAt:     harness.now().Add(time.Hour),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	harness.service.Run(ctx)

	settled, err := harness.store.Get(context.Background(), interrupted)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !settled.State.Terminal() {
		t.Errorf("the interrupted rollback is still %q", settled.State)
	}
}

// --------------------------------------------------------------- expiry --

// TestAQueuedRequestExpiresWithoutTouchingTheHost.
func TestAQueuedRequestExpiresWithoutTouchingTheHost(t *testing.T) {
	harness := newRollbackHarness(t)
	rollback := harness.request(t)

	// Past the TTL, and blocked from starting so the sweep is what settles it.
	harness.evidence.edit(func(e *fakeRollbackEvidence) { e.activeExecution = true })
	harness.advance(2 * time.Hour)

	final := harness.runOnce(t, rollback)

	if final.State != domain.RollbackExpired && final.State != domain.RollbackFailed {
		t.Fatalf("state %q, want expired or failed", final.State)
	}
	if ops := harness.host.operations(); len(ops) != 0 {
		t.Errorf("an expired rollback touched the host: %v", ops)
	}
}

// -------------------------------------------------- the second preflight --

// TestTheHostIsRecheckedImmediatelyBeforeTheFirstMutation is the TOCTOU
// property.
//
// The request passed its preflight. Then somebody moved a container. The
// rollback must refuse at the mutation point rather than act on an assessment
// that is now minutes old.
func TestTheHostIsRecheckedImmediatelyBeforeTheFirstMutation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*rbHarness)
		want   domain.RollbackRefusal
	}{
		{
			name:   "the original was removed after the request",
			change: func(h *rbHarness) { h.host.remove(rbOriginalID) },
			want:   domain.RollbackRefusalOriginalMissing,
		},
		{
			name:   "the replacement was removed after the request",
			change: func(h *rbHarness) { h.host.remove(rbReplacementID) },
			want:   domain.RollbackRefusalReplacementMissing,
		},
		{
			name: "the original was renamed after the request",
			change: func(h *rbHarness) {
				h.host.with(rbOriginalID, func(c *rbContainer) {
					c.detail.Overview.Name = "moved-by-hand"
				})
			},
			want: domain.RollbackRefusalOriginalIdentity,
		},
		{
			name: "the original was moved onto another image after the request",
			change: func(h *rbHarness) {
				h.host.with(rbOriginalID, func(c *rbContainer) {
					c.detail.Overview.ImageID = "sha256:" + strings.Repeat("e", 64)
				})
			},
			want: domain.RollbackRefusalOriginalIdentity,
		},
		{
			name:   "the daemon went away after the request",
			change: func(h *rbHarness) { h.host.setErr(&h.host.pingErr, errRollbackDaemonGone) },
			want:   domain.RollbackRefusalDockerUnavailable,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newRollbackHarness(t)
			rollback := harness.request(t)

			testCase.change(harness)

			final := harness.runOnce(t, rollback)

			if final.State != domain.RollbackFailed {
				t.Fatalf("state %q, want failed", final.State)
			}
			if final.Refusal != testCase.want {
				t.Errorf("refusal %q, want %q", final.Refusal, testCase.want)
			}
			if final.Failure != domain.RollbackFailurePreflight {
				t.Errorf("failure %q, want preflight", final.Failure)
			}
			if ops := harness.host.operations(); len(ops) != 0 {
				t.Errorf("a rollback refused at the mutation point touched the host: %v", ops)
			}
			if final.MutatedAt != nil {
				t.Error("mutatedAt was stamped although nothing was mutated")
			}
		})
	}
}

// ------------------------------------------------------------- secrecy --

// TestNoRollbackRecordCarriesASecret is the leakage test.
//
// The original container declares a sensitive environment variable. Nothing a
// rollback writes -- record, events, verification report, recovery plan -- may
// contain its value.
func TestNoRollbackRecordCarriesASecret(t *testing.T) {
	harness := newRollbackHarness(t)
	final := harness.runOnce(t, harness.request(t))

	events, err := harness.store.Events(context.Background(), final.RollbackID, 500)
	if err != nil {
		t.Fatalf("events: %v", err)
	}

	encoded, err := json.Marshal(struct {
		Rollback domain.Rollback
		Events   []domain.RollbackEvent
	}{final, events})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rendered := string(encoded)

	// The secret value, and the socket path a daemon error would carry.
	for _, forbidden := range []string{"hunter2", "/var/run/docker.sock", "DB_PASSWORD=hunter2"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("a rollback record contains %q", forbidden)
		}
	}
}

// TestAFailedRollbackDoesNotCarryTheDaemonsWords proves adapter errors are
// classified, never quoted.
func TestAFailedRollbackDoesNotCarryTheDaemonsWords(t *testing.T) {
	leak := "Error response from daemon: cannot stop container: " +
		"/var/run/docker.sock: permission denied (env DB_PASSWORD=hunter2)"

	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.host.stopErr = errors.New(leak)
	})

	final := harness.runOnce(t, harness.request(t))

	if final.State != domain.RollbackFailed {
		t.Fatalf("state %q, want failed", final.State)
	}
	encoded, err := json.Marshal(final)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, fragment := range []string{"hunter2", "/var/run/docker.sock", "Error response from daemon"} {
		if strings.Contains(string(encoded), fragment) {
			t.Errorf("the record quotes the daemon: %q", fragment)
		}
	}
	if final.Message == "" {
		t.Error("a failure with no message of HarborMaster's own")
	}
}

// ---------------------------------------------------------- derivation --

// TestNothingAboutARollbackComesFromTheCaller is the input-trust property.
//
// The request carries an execution id and an idempotency key. Every identity
// the rollback acts on is derived from the execution record HarborMaster wrote
// itself, so a caller cannot aim a rollback at a container of their choosing.
func TestNothingAboutARollbackComesFromTheCaller(t *testing.T) {
	harness := newRollbackHarness(t)

	rollback, err := harness.service.Request(context.Background(), service.RollbackRequest{
		ExecutionID: rbExecutionID,
		RequestKey:  "some-key",
		RequestedBy: domain.Requester{UserID: "usr_x", Username: "operator"},
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	if rollback.OriginalID != rbOriginalID {
		t.Errorf("originalId %q, want the execution's %q", rollback.OriginalID, rbOriginalID)
	}
	if rollback.ReplacementID != rbReplacementID {
		t.Errorf("replacementId %q, want the execution's %q", rollback.ReplacementID, rbReplacementID)
	}
	if rollback.ContainerName != rbContainerName {
		t.Errorf("containerName %q, want the execution's %q", rollback.ContainerName, rbContainerName)
	}
	if rollback.ParkedName != rbParkedName {
		t.Errorf("parkedName %q, want the execution's %q", rollback.ParkedName, rbParkedName)
	}
	if rollback.OriginalImageID != rbOldImageID {
		t.Errorf("originalImageId %q, want the execution's %q",
			rollback.OriginalImageID, rbOldImageID)
	}
	if rollback.RequestedBy.UserID != "usr_x" {
		t.Errorf("requestedBy %+v, want the requesting account", rollback.RequestedBy)
	}
}

// TestTheParkedNameIsDerivedFromTheRollbackID proves the replacement's new name
// cannot collide and cannot be chosen.
func TestTheParkedNameIsDerivedFromTheRollbackID(t *testing.T) {
	harness := newRollbackHarness(t)
	final := harness.runOnce(t, harness.request(t))

	want := rbContainerName + domain.RollbackParkedNameSuffix + final.RollbackID
	if final.ReplacementParkedName != want {
		t.Errorf("parked name %q, want %q", final.ReplacementParkedName, want)
	}
	if got := harness.host.nameOf(rbReplacementID); got != want {
		t.Errorf("the host says %q, the record says %q", got, want)
	}
}

// TestARollbackIsRefusedWhenTheDerivedNameWouldNotFit.
func TestARollbackIsRefusedWhenTheDerivedNameWouldNotFit(t *testing.T) {
	long := strings.Repeat("n", domain.MaxRollbackableNameBytes+1)

	harness := newRollbackHarness(t, func(h *rbHarness) {
		h.evidence.execution.ContainerName = long
		h.evidence.execution.ParkedName = long + domain.ParkedNameSuffix + rbExecutionID
	})

	if got := harness.refusal(t); got != domain.RollbackRefusalNameUnavailable {
		t.Errorf("refusal %q, want nameUnavailable", got)
	}
	if ops := harness.host.operations(); len(ops) != 0 {
		t.Errorf("the host was touched: %v", ops)
	}
}

// ------------------------------------------------------------- helpers --

func newTestHasher(t *testing.T) *service.Hasher {
	t.Helper()

	key, err := service.LoadSecretKey(service.SecretKeyOptions{
		GeneratePath: filepath.Join(t.TempDir(), "secret.key"),
	})
	if err != nil {
		t.Fatalf("load secret key: %v", err)
	}
	return service.NewHasher(key)
}

func equalCheckpoints(got, want []domain.RollbackCheckpoint) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equalStates(got, want []domain.RollbackState) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
