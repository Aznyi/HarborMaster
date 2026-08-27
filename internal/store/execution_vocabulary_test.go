package store_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The execution vocabularies and the schema that stores them.
//
// # Why this test exists
//
// `domain.ExecutionRefusals` and the `refusal` CHECK on `executions` are ONE
// vocabulary in two places. They had drifted, and had been shipping drifted:
// three refusals could be produced by the code and not written down.
//
//	selfUpdate                 the container HarborMaster is running in
//	namespaceProviderMissing   a shared namespace whose provider is gone
//	dependentsNotRebindable    invariant A
//
// The consequence is subtle and bad. The recreation was still REFUSED -- that
// decision is made in Go -- but recording it failed, so an operator saw a
// refused execution with no reason. Self-update protection and invariant A are
// precisely the two refusals somebody most needs explained, because both look
// from outside like HarborMaster declining for no reason.
//
// This is the sixth occurrence of the same shape. 0014, 0017, 0021 and 0026
// fixed it for audit target types; 0027 for plan update types, found live
// against a real daemon. Each time the fix was a migration and each time the
// lesson was the same: a vocabulary in two places needs a test that walks it.
//
// Modelled on TestEveryAuditTargetTypeIsAcceptedByTheSchema, which has caught
// this class twice now before it reached a host.

func TestEveryExecutionRefusalIsAcceptedByTheSchema(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Non-vacuity: if the vocabulary moved, this would iterate an empty list
	// and pass having stored nothing.
	if len(domain.ExecutionRefusals) < 25 {
		t.Fatalf("found %d execution refusals; the vocabulary is not where this "+
			"test thinks it is", len(domain.ExecutionRefusals))
	}
	// And the three that were missing must be present, or this test would pass
	// while the defect it was written for was reintroduced.
	for _, required := range []domain.ExecutionRefusal{
		domain.ExecutionRefusalSelfUpdate,
		domain.ExecutionRefusalNamespaceProviderMissing,
		domain.ExecutionRefusalDependentsNotRebindable,
		// Added by Phase 17.2 with migration 0030. Listed here for the same
		// reason as the three above: this test's value is that it writes a real
		// row per vocabulary entry, and a value dropped from the vocabulary
		// would silently stop being covered.
		domain.ExecutionRefusalSnapshotChanged,
		// Added by Phase 17.7 with migration 0031, for the same reason.
		domain.ExecutionRefusalApprovalMissing,
	} {
		var found bool
		for _, refusal := range domain.ExecutionRefusals {
			if refusal == required {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("domain.ExecutionRefusals no longer contains %q; this test "+
				"would not cover the case it was written for", required)
		}
	}

	for index, refusal := range domain.ExecutionRefusals {
		// Through Advance, not Create. Create never writes the refusal column --
		// a queued execution has no verdict yet -- so a test that only inserted
		// would pass while the schema rejected every value under test. It did:
		// this test was vacuous on its first run and reported success against a
		// CHECK that still refused three of them.
		execution := refusalFixture(index, domain.ExecutionRefusalNone, now)
		if _, err := db.Executions.Create(ctx, execution, now); err != nil {
			t.Fatalf("the fixture could not be stored: %v", err)
		}
		_, err := db.Executions.Advance(ctx, store.ExecutionChange{
			ExecutionID: execution.ExecutionID,
			To:          domain.ExecutionFailed,
			Refusal:     refusal,
		}, now)
		if err != nil {
			t.Errorf("the schema refuses execution refusal %q: %v\n\n"+
				"domain.ExecutionRefusals and the refusal CHECK on executions are "+
				"one vocabulary in two places. A refused execution whose record "+
				"cannot be written is a refusal the operator never sees a reason "+
				"for -- and the two most important refusals in the product, "+
				"self-update protection and invariant A, were in exactly that "+
				"state. Widen the CHECK in a new migration; see "+
				"0028_execution_refusal_vocabulary.sql.",
				refusal, err)
		}
	}
}

// Every execution FAILURE the vocabulary declares is writable too.
//
// The failure column has its own CHECK, so it can fail the same way. It has not
// drifted; this pins that.
func TestEveryExecutionFailureIsAcceptedByTheSchema(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if len(domain.ExecutionFailures) < 15 {
		t.Fatalf("found %d execution failures; the vocabulary is not where this "+
			"test thinks it is", len(domain.ExecutionFailures))
	}

	for index, failure := range domain.ExecutionFailures {
		execution := refusalFixture(1000+index, domain.ExecutionRefusalNone, now)
		if _, err := db.Executions.Create(ctx, execution, now); err != nil {
			t.Fatalf("the fixture could not be stored: %v", err)
		}
		if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
			ExecutionID: execution.ExecutionID,
			To:          domain.ExecutionFailed,
			Failure:     failure,
		}, now); err != nil {
			t.Errorf("the schema refuses execution failure %q: %v\n\n"+
				"domain.ExecutionFailures and the failure CHECK on executions are "+
				"one vocabulary in two places.", failure, err)
		}
	}
}

// Every execution STATE and CHECKPOINT is writable.
//
// The other two enumerated columns on the same table, walked for the same
// reason. A checkpoint that cannot be written is worse than a refusal that
// cannot: the checkpoint is what says WHAT IS TRUE OF THE HOST after a failed
// recreation, and it is the first thing a recovery runbook reads.
func TestEveryExecutionStateAndCheckpointIsAcceptedByTheSchema(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if len(domain.ExecutionStates) < 8 || len(domain.ExecutionCheckpoints) < 5 {
		t.Fatalf("states=%d checkpoints=%d; a vocabulary is not where this test "+
			"thinks it is", len(domain.ExecutionStates), len(domain.ExecutionCheckpoints))
	}

	index := 2000
	for _, state := range domain.ExecutionStates {
		execution := refusalFixture(index, domain.ExecutionRefusalNone, now)
		index++
		if _, err := db.Executions.Create(ctx, execution, now); err != nil {
			t.Fatalf("the fixture could not be stored: %v", err)
		}
		if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
			ExecutionID: execution.ExecutionID,
			To:          state,
		}, now); err != nil {
			t.Errorf("the schema refuses execution state %q: %v", state, err)
		}
	}
	for _, checkpoint := range domain.ExecutionCheckpoints {
		if checkpoint == "" {
			continue
		}
		execution := refusalFixture(index, domain.ExecutionRefusalNone, now)
		index++
		if _, err := db.Executions.Create(ctx, execution, now); err != nil {
			t.Fatalf("the fixture could not be stored: %v", err)
		}
		if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
			ExecutionID: execution.ExecutionID,
			To:          domain.ExecutionVerifying,
			Checkpoint:  checkpoint,
		}, now); err != nil {
			t.Errorf("the schema refuses execution checkpoint %q: %v", checkpoint, err)
		}
	}
}

// refusalFixture builds a storable execution carrying one refusal.
//
// Every identifier is unique per index: `acquisition_id` and `execution_id` are
// both UNIQUE, so a shared fixture would fail on the second row for a reason
// that has nothing to do with the vocabulary under test.
func refusalFixture(
	index int,
	refusal domain.ExecutionRefusal,
	now time.Time,
) domain.Execution {
	// A DISTINCT container per row. Only one execution may be active for a
	// container at a time -- a partial unique index enforces it -- so a shared
	// container would fail every row after the first for a reason that has
	// nothing to do with the vocabulary under test.
	suffix := strconv.Itoa(index)
	return domain.Execution{
		ExecutionID:   domain.NewExecutionID(),
		AcquisitionID: domain.NewAcquisitionID(),
		PlanID:        domain.NewPlanID(),
		ContainerID:   "container-vocabulary-" + suffix,
		ContainerName: "hm-vocabulary-" + suffix,
		Target: domain.ExecutionTarget{
			Registry:   "docker.io",
			Repository: "library/alpine",
			Digest: "sha256:" +
				"4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1",
			Reference: "alpine:3.22.1",
		},
		State:       domain.ExecutionQueued,
		Refusal:     refusal,
		RequestedAt: now,
		ExpiresAt:   now.Add(time.Hour),
	}
}
