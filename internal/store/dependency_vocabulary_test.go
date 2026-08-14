package store_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The remaining closed vocabularies, walked against the schema that stores them.
//
// # Why this file exists
//
// "One vocabulary in two places" is now this codebase's most frequent defect,
// and it has never once been caught by a type checker. The Go constants and the
// SQLite CHECK are written independently, drift silently, and the failure only
// appears when a particular value is finally produced -- typically during an
// incident, which is exactly when the record mattered.
//
// Six occurrences so far. 0014, 0017, 0021 and 0026 for audit target types;
// 0027 for plan update types, found live against a real daemon; 0028 for
// execution refusals. Each fix was a migration, and each time the lesson was
// that a vocabulary needs a test that WALKS it rather than a review that reads
// it.
//
// Audit targets, plan update types and the four execution columns already have
// one. These are the rest: the two dependency tables and the three enumerated
// columns on acquisitions. After this file, every closed vocabulary in the
// schema is walked by a test.
//
// # The conditional constraints matter as much as the value sets
//
// Two of these tables carry CHECKs that relate two columns rather than
// constrain one:
//
//	dependency_operations         (state IN terminal three) = (completed_at IS NOT NULL)
//	dependency_operation_members  (state = 'blocked')       = (refusal <> '')
//
// Both encode a Go-side rule -- DependencyOperationState.Terminal(), and
// "only a blocked member carries a reason" -- in SQL. A vocabulary test that
// only checked the value lists would pass while either pairing drifted, so both
// are walked here too.

// ---------------------------------------------------- dependency operations --

// operationFixture stores one operation with one member, on its own provider.
//
// A distinct provider per row: a partial unique index permits one ACTIVE
// operation per provider, so a shared fixture would fail every row after the
// first for a reason unrelated to the vocabulary under test.
func operationFixture(t *testing.T, db *store.DB, index int) domain.DependencyOperation {
	t.Helper()

	suffix := strconv.Itoa(index)
	now := time.Now().UTC()
	created, err := db.DependencyOperations.Create(context.Background(),
		domain.DependencyOperation{
			Provider:            "hm-vocabulary-provider-" + suffix,
			ProviderPlanID:      domain.NewPlanID(),
			ProviderExecutionID: domain.NewExecutionID(),
			State:               domain.OperationQueued,
			Members: []domain.DependencyMember{{
				Dependent:          "hm-vocabulary-dependent-" + suffix,
				Provider:           "hm-vocabulary-provider-" + suffix,
				Source:             domain.DependencyNetworkNamespace,
				ExpectedProviderID: "0123456789ab",
				State:              domain.MemberPending,
			}},
		}, now)
	if err != nil {
		t.Fatalf("the fixture could not be stored: %v", err)
	}
	return created
}

func TestEveryDependencyOperationStateIsAcceptedByTheSchema(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Non-vacuity: a moved vocabulary would iterate an empty list and pass
	// having stored nothing.
	if len(domain.DependencyOperationStates) < 8 {
		t.Fatalf("found %d dependency operation states; the vocabulary is not "+
			"where this test thinks it is", len(domain.DependencyOperationStates))
	}

	for index, state := range domain.DependencyOperationStates {
		operation := operationFixture(t, db, index)
		if err := db.DependencyOperations.AdvanceOperation(ctx,
			operation.OperationID, state, domain.OperationFailureNone, now); err != nil {
			t.Errorf("the schema refuses dependency operation state %q: %v\n\n"+
				"domain.DependencyOperationStates and the state CHECK on "+
				"dependency_operations are one vocabulary in two places. A "+
				"coordinated update whose state cannot be written is an update "+
				"whose progress an operator cannot see. Widen the CHECK in a new "+
				"migration.", state, err)
		}
	}
}

func TestEveryDependencyOperationFailureIsAcceptedByTheSchema(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if len(domain.DependencyOperationFailures) < 4 {
		t.Fatalf("found %d dependency operation failures; the vocabulary is not "+
			"where this test thinks it is", len(domain.DependencyOperationFailures))
	}

	for index, failure := range domain.DependencyOperationFailures {
		operation := operationFixture(t, db, 100+index)
		// Through OperationFailed, which is the state a failure accompanies.
		if err := db.DependencyOperations.AdvanceOperation(ctx,
			operation.OperationID, domain.OperationFailed, failure, now); err != nil {
			t.Errorf("the schema refuses dependency operation failure %q: %v\n\n"+
				"A failed coordinated update whose reason cannot be written is a "+
				"detached workload with no explanation attached to it.", failure, err)
		}
	}
}

// The terminal states and the completed_at constraint are the same statement.
//
// `AdvanceOperation` stamps completed_at from `state.Terminal()`, and the schema
// requires that stamp for exactly three states. If Terminal() and that list ever
// disagree, every write of the disputed state fails -- and it fails at the END
// of an operation, when the record is the only thing left describing what
// happened.
func TestTerminalOperationStatesAreExactlyTheOnesTheSchemaStamps(t *testing.T) {
	t.Parallel()

	schemaTerminal := map[domain.DependencyOperationState]bool{
		domain.OperationSucceeded: true,
		domain.OperationFailed:    true,
		domain.OperationBlocked:   true,
	}
	// Non-vacuity: the three names must still exist and still be distinct.
	if len(schemaTerminal) != 3 {
		t.Fatal("the three terminal state constants are no longer distinct")
	}

	for _, state := range domain.DependencyOperationStates {
		if state.Terminal() != schemaTerminal[state] {
			t.Errorf("state %q: Terminal() = %t, the schema stamps completed_at = %t\n\n"+
				"The CHECK on dependency_operations reads\n"+
				"  (state IN ('succeeded', 'failed', 'blocked')) = (completed_at IS NOT NULL)\n"+
				"and AdvanceOperation stamps completed_at from Terminal(). While "+
				"these disagree, writing this state is impossible.",
				state, state.Terminal(), schemaTerminal[state])
		}
	}
}

// ------------------------------------------------------- dependency members --

func TestEveryDependencyMemberStateIsAcceptedByTheSchema(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if len(domain.DependencyMemberStates) < 8 {
		t.Fatalf("found %d dependency member states; the vocabulary is not where "+
			"this test thinks it is", len(domain.DependencyMemberStates))
	}
	// The state the acquisition -> execution handoff settles a member into must
	// be present, or this test would pass while that path was unwritable.
	var foundFailed bool
	for _, state := range domain.DependencyMemberStates {
		if state == domain.MemberFailed {
			foundFailed = true
		}
	}
	if !foundFailed {
		t.Fatal("domain.DependencyMemberStates no longer contains MemberFailed")
	}

	for index, state := range domain.DependencyMemberStates {
		operation := operationFixture(t, db, 200+index)
		member := operation.Members[0]

		// A blocked member must carry a reason and every other member must not:
		// the schema ties the two columns together, so the refusal is chosen
		// from the state rather than held constant.
		refusal := domain.RebindRefusalNone
		if state == domain.MemberBlocked {
			refusal = domain.RebindRefusalDigestUnestablished
		}

		if err := db.DependencyOperations.AdvanceMember(ctx, store.MemberUpdate{
			OperationID: operation.OperationID,
			Dependent:   member.Dependent,
			State:       state,
			Refusal:     refusal,
		}, now); err != nil {
			t.Errorf("the schema refuses dependency member state %q: %v\n\n"+
				"domain.DependencyMemberStates and the state CHECK on "+
				"dependency_operation_members are one vocabulary in two places. A "+
				"member whose state cannot be written is a reattachment that "+
				"either repeats or never happens, depending on which way the "+
				"stale row reads.", state, err)
		}
	}
}

func TestEveryRebindRefusalIsAcceptedByTheSchema(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if len(domain.RebindRefusals) < 10 {
		t.Fatalf("found %d rebind refusals; the vocabulary is not where this test "+
			"thinks it is", len(domain.RebindRefusals))
	}

	for index, refusal := range domain.RebindRefusals {
		operation := operationFixture(t, db, 300+index)
		member := operation.Members[0]

		// Every refusal is written with `blocked`, which is the only state the
		// schema permits one on.
		if err := db.DependencyOperations.AdvanceMember(ctx, store.MemberUpdate{
			OperationID: operation.OperationID,
			Dependent:   member.Dependent,
			State:       domain.MemberBlocked,
			Refusal:     refusal,
		}, now); err != nil {
			t.Errorf("the schema refuses rebind refusal %q: %v\n\n"+
				"This is the reason a container was left attached to a namespace "+
				"that no longer exists. A refusal that cannot be stored is the "+
				"one an operator most needs and never sees.", refusal, err)
		}
	}
}

// A member that is not blocked cannot carry a refusal, and a blocked one must.
//
// The non-vacuity guard on the two tests above: if that CHECK were dropped, both
// would still pass while the column stopped meaning anything.
func TestOnlyABlockedMemberCarriesAReason(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("a verified member may not carry a refusal", func(t *testing.T) {
		operation := operationFixture(t, db, 400)
		if err := db.DependencyOperations.AdvanceMember(ctx, store.MemberUpdate{
			OperationID: operation.OperationID,
			Dependent:   operation.Members[0].Dependent,
			State:       domain.MemberVerified,
			Refusal:     domain.RebindRefusalNotPresent,
		}, now); err == nil {
			t.Error("a verified member was stored holding a refusal nobody reads")
		}
	})

	t.Run("a blocked member must carry one", func(t *testing.T) {
		operation := operationFixture(t, db, 401)
		if err := db.DependencyOperations.AdvanceMember(ctx, store.MemberUpdate{
			OperationID: operation.OperationID,
			Dependent:   operation.Members[0].Dependent,
			State:       domain.MemberBlocked,
			Refusal:     domain.RebindRefusalNone,
		}, now); err == nil {
			t.Error("a member was blocked with no reason recorded")
		}
	})
}

// ---------------------------------------------------------- acquisitions --

// Every acquisition state, failure and refusal is writable.
//
// The three enumerated columns on `acquisitions`, walked for the same reason as
// the four on `executions`. These have not drifted; this pins them, and the
// acquisition vocabulary is now load-bearing in a second place: the dependency
// follower reads an acquisition's terminal state to decide whether a detached
// container may be recreated.
func TestEveryAcquisitionVocabularyIsAcceptedByTheSchema(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if len(domain.AcquisitionStates) < 8 ||
		len(domain.AcquisitionFailures) < 5 ||
		len(domain.AcquisitionRefusals) < 10 {
		t.Fatalf("states=%d failures=%d refusals=%d; an acquisition vocabulary is "+
			"not where this test thinks it is", len(domain.AcquisitionStates),
			len(domain.AcquisitionFailures), len(domain.AcquisitionRefusals))
	}

	index := 0
	// fresh stores a queued acquisition on its own container. A partial unique
	// index permits one active acquisition per container.
	fresh := func() domain.Acquisition {
		index++
		acquisition := acquisitionFor("container-acq-vocabulary-"+strconv.Itoa(index),
			acqDigest)
		created, err := db.Acquisitions.Create(ctx, acquisition, now)
		if err != nil {
			t.Fatalf("the fixture could not be stored: %v", err)
		}
		return created
	}

	// Through Advance, not Create. Create writes neither failure nor refusal --
	// a queued acquisition has no verdict yet -- so a test that only inserted
	// would pass while the schema rejected every value under test. That is
	// precisely how the execution refusal guard was vacuous on its first run.
	for _, state := range domain.AcquisitionStates {
		acquisition := fresh()
		if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
			AcquisitionID: acquisition.AcquisitionID,
			To:            state,
		}, now); err != nil {
			t.Errorf("the schema refuses acquisition state %q: %v", state, err)
		}
	}
	for _, failure := range domain.AcquisitionFailures {
		acquisition := fresh()
		if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
			AcquisitionID: acquisition.AcquisitionID,
			To:            domain.AcquisitionFailed,
			Failure:       failure,
		}, now); err != nil {
			t.Errorf("the schema refuses acquisition failure %q: %v", failure, err)
		}
	}
	for _, refusal := range domain.AcquisitionRefusals {
		acquisition := fresh()
		if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
			AcquisitionID: acquisition.AcquisitionID,
			To:            domain.AcquisitionFailed,
			Refusal:       refusal,
		}, now); err != nil {
			t.Errorf("the schema refuses acquisition refusal %q: %v\n\n"+
				"A refused pull whose reason cannot be written is a reattachment "+
				"the follower will settle as failed with nothing to show for it.",
				refusal, err)
		}
	}
}
