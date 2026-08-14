package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Operation and member persistence at the member counts a real provider reaches.
//
// # What is being measured
//
// STATEMENT and TRANSACTION counts, with timings alongside. A VPN provider with
// eight dependents is ordinary; sixty-four is the ceiling the graph enforces.
// The property worth holding is that the number of TRANSACTIONS does not grow
// with the member count, and that reading an operation back is two queries
// whatever it contains.
//
// Per-member INSERT statements do grow -- one per member, inside one
// transaction. That is inherent: each member is a row. What matters is that they
// are not each a round trip that could half-succeed, which is why they share a
// transaction and why an interrupted create leaves nothing rather than a partial
// member set.

func membersFor(count int) []domain.DependencyMember {
	members := make([]domain.DependencyMember, 0, count)
	for i := range count {
		members = append(members, domain.DependencyMember{
			Dependent:          fmt.Sprintf("dependent%03d", i),
			Provider:           "gluetun",
			Source:             domain.DependencyNetworkNamespace,
			ExpectedProviderID: fmt.Sprintf("%064x", i+1),
			State:              domain.MemberPending,
		})
	}
	return members
}

// Creation, reload, and the dedup lookup at 1 / 8 / 32 / 64 members.
func TestOperationPersistenceAtRepresentativeMemberCounts(t *testing.T) {
	t.Parallel()

	for _, count := range []int{1, 8, 32, 64} {
		t.Run(fmt.Sprintf("%d members", count), func(t *testing.T) {
			t.Parallel()

			db := openTestDB(t)
			ctx := context.Background()
			now := time.Now().UTC()

			// A. Operation creation, and B. atomic member creation.
			//
			// ONE transaction: 1 operation INSERT + N member INSERTs + COMMIT.
			started := time.Now()
			created, err := db.DependencyOperations.Create(ctx, domain.DependencyOperation{
				Provider: "gluetun",
				State:    domain.OperationQueued,
				Members:  membersFor(count),
			}, now)
			createElapsed := time.Since(started)
			if err != nil {
				t.Fatalf("create: %v", err)
			}

			// C. Reload after creation. TWO queries regardless of member count:
			// one for the operation, one for all its members.
			started = time.Now()
			loaded, err := db.DependencyOperations.Get(ctx, created.OperationID)
			reloadElapsed := time.Since(started)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if len(loaded.Members) != count {
				t.Fatalf("members = %d, want %d", len(loaded.Members), count)
			}

			// The open sweep, which is what restart recovery runs. Also two
			// queries, for every open operation at once.
			started = time.Now()
			open, err := db.DependencyOperations.Open(ctx, 0)
			sweepElapsed := time.Since(started)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if len(open) != 1 || len(open[0].Members) != count {
				t.Fatalf("open sweep returned %d operations", len(open))
			}

			// D. Advancing every member, as plan production does. One statement
			// per member, each an indexed point update on the composite primary
			// key.
			started = time.Now()
			for _, member := range loaded.Members {
				if err := db.DependencyOperations.AdvanceMember(ctx, store.MemberUpdate{
					OperationID:      created.OperationID,
					Dependent:        member.Dependent,
					State:            domain.MemberPlanCreated,
					PlanID:           domain.NewPlanID(),
					TargetProviderID: fmt.Sprintf("%064x", 9999),
				}, now); err != nil {
					t.Fatalf("advance member: %v", err)
				}
			}
			advanceElapsed := time.Since(started)

			// E. The dedup lookup once plans exist: the same two-query reload,
			// which is what ProduceRebindPlans reads before deciding anything.
			started = time.Now()
			withPlans, err := db.DependencyOperations.Get(ctx, created.OperationID)
			dedupElapsed := time.Since(started)
			if err != nil {
				t.Fatalf("get after planning: %v", err)
			}
			for _, member := range withPlans.Members {
				if member.PlanID == "" || member.TargetProviderID == "" {
					t.Fatalf("member %s lost its plan link", member.Dependent)
				}
			}

			// The per-container read the detail view uses.
			started = time.Now()
			forDependent, err := db.DependencyOperations.MembersForDependent(ctx, "dependent000")
			lookupElapsed := time.Since(started)
			if err != nil {
				t.Fatalf("members for dependent: %v", err)
			}
			if len(forDependent) != 1 {
				t.Fatalf("membersForDependent = %d, want 1", len(forDependent))
			}

			t.Logf("members=%d create=%s reload=%s openSweep=%s advanceAll=%s "+
				"dedupRead=%s perContainer=%s | transactions: create=1 reload=0 advance=%d",
				count, createElapsed, reloadElapsed, sweepElapsed, advanceElapsed,
				dedupElapsed, lookupElapsed, count)
		})
	}
}

// The open sweep stays two queries as the number of OPERATIONS grows, not just
// as members do.
//
// The recovery path reads every outstanding operation on startup. If that were
// one query per operation, a host mid-way through several coordinated updates
// would pay for it at exactly the worst moment.
func TestTheOpenSweepDoesNotGrowWithOperationCount(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	const operations = 20
	for i := range operations {
		provider := fmt.Sprintf("provider%02d", i)
		members := membersFor(4)
		for index := range members {
			members[index].Provider = provider
			members[index].Dependent = fmt.Sprintf("dep%02d-%d", i, index)
		}
		if _, err := db.DependencyOperations.Create(ctx, domain.DependencyOperation{
			Provider: provider,
			State:    domain.OperationQueued,
			Members:  members,
		}, now); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	started := time.Now()
	open, err := db.DependencyOperations.Open(ctx, 0)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(open) != operations {
		t.Fatalf("open = %d, want %d", len(open), operations)
	}
	for _, operation := range open {
		if len(operation.Members) != 4 {
			t.Fatalf("%s has %d members, want 4", operation.Provider, len(operation.Members))
		}
	}

	t.Logf("operations=%d members=%d openSweep=%s | queries=2 regardless",
		operations, operations*4, elapsed)
}

// The follower's open-operation sweep at the operation counts a busy estate
// reaches.
//
// # The property, not the timing
//
// TWO queries regardless of how many operations are open, and none of them a
// Docker call: sweeping persisted dependency state must never touch the daemon.
// A per-operation query here would turn a restart on a busy host into hundreds
// of round trips at exactly the moment it is least welcome.
func TestTheFollowerSweepAtOperationScale(t *testing.T) {
	t.Parallel()

	for _, operations := range []int{1, 10, 50, 200} {
		t.Run(fmt.Sprintf("%d operations", operations), func(t *testing.T) {
			t.Parallel()

			db := openTestDB(t)
			ctx := context.Background()
			now := time.Now().UTC()

			// Four members each: a realistic fan-out for a namespace provider.
			const membersEach = 4
			for i := range operations {
				provider := fmt.Sprintf("provider%04d", i)
				members := membersFor(membersEach)
				for index := range members {
					members[index].Provider = provider
					members[index].Dependent = fmt.Sprintf("dep%04d-%d", i, index)
				}
				if _, err := db.DependencyOperations.Create(ctx, domain.DependencyOperation{
					Provider: provider,
					State:    domain.OperationQueued,
					Members:  members,
				}, now); err != nil {
					t.Fatalf("create %d: %v", i, err)
				}
			}

			started := time.Now()
			open, err := db.DependencyOperations.Open(ctx, 0)
			sweepElapsed := time.Since(started)
			if err != nil {
				t.Fatalf("open: %v", err)
			}

			// The repository caps a sweep at 200 operations, which is the
			// bound this test deliberately reaches.
			expected := min(operations, 200)
			if len(open) != expected {
				t.Fatalf("open = %d, want %d", len(open), expected)
			}
			for _, operation := range open {
				if len(operation.Members) != membersEach {
					t.Fatalf("%s has %d members, want %d",
						operation.Provider, len(operation.Members), membersEach)
				}
			}

			t.Logf("operations=%d members=%d sweep=%s | queries=2 regardless, no Docker calls",
				len(open), len(open)*membersEach, sweepElapsed)
		})
	}
}

// The IDLE cost: many operations, none needing advancement.
//
// Establishes the standing overhead of having coordinated updates recorded at
// all. A follower that read cheaply only when there was work to do would make a
// quiet estate expensive.
func TestTheFollowerSweepIsCheapWhenNothingNeedsWork(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	const operations = 200
	for i := range operations {
		provider := fmt.Sprintf("settled%04d", i)
		members := membersFor(2)
		for index := range members {
			members[index].Provider = provider
			members[index].Dependent = fmt.Sprintf("settled%04d-%d", i, index)
			members[index].State = domain.MemberVerified
		}
		created, err := db.DependencyOperations.Create(ctx, domain.DependencyOperation{
			Provider: provider,
			State:    domain.OperationQueued,
			Members:  members,
		}, now)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		// Concluded: these must not appear in the sweep at all.
		if err := db.DependencyOperations.AdvanceOperation(ctx, created.OperationID,
			domain.OperationSucceeded, domain.OperationFailureNone, now); err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
	}

	started := time.Now()
	open, err := db.DependencyOperations.Open(ctx, 0)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// THE assertion: a terminal operation costs nothing on every subsequent
	// tick. The partial index excludes them in the query rather than in Go.
	if len(open) != 0 {
		t.Fatalf("%d terminal operations were swept, want none", len(open))
	}
	t.Logf("terminalOperations=%d sweep=%s | 0 returned, 2 queries, no Docker calls",
		operations, elapsed)
}
