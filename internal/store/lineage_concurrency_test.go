package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Overlapping automation passes against one lineage-managed workload.
//
// # What is actually being tested, and what is deliberately not
//
// Automation passes can overlap: a slow pass, a manual RunNow, and a scheduled
// tick can all be deciding about the same container at once. Every one of them
// submits through the same request keys, so the question this file answers is
// whether the EXISTING backstop -- the unique index on request_key -- holds when
// they collide for real, in one database, from several goroutines.
//
// No lineage-specific lock is introduced, and none should be. Lineage advances
// only from a recreation that verified, so "no duplicate lineage advancement" is
// a consequence of "no duplicate execution" rather than a separate property
// needing its own mechanism. If a second chain could never be created, a second
// advancement can never be reached.
//
// Run under -race, which is where a shared *sql.DB and SQLite's own locking are
// exercised together.

const concurrentAttempts = 12

// TestConcurrentAcquisitionRequestsCollapseToOneChain.
//
// The first submission in the pipeline. Twelve goroutines present the SAME
// idempotency key -- which is what overlapping passes for one container do,
// because the key is derived from the plan rather than from the pass.
func TestConcurrentAcquisitionRequestsCollapseToOneChain(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	const key = "automation:acquire:plan_00112233445566778899"

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created []string
		failed  int
	)
	start := make(chan struct{})

	for i := 0; i < concurrentAttempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			acquisition := acquisitionFor("container-web", acqDigest)
			acquisition.RequestKey = key

			<-start // maximise the overlap
			result, err := db.Acquisitions.Create(ctx, acquisition, time.Now().UTC())

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				return
			}
			created = append(created, result.AcquisitionID)
		}()
	}
	close(start)
	wg.Wait()

	// Every caller must come away with the SAME acquisition. A request key that
	// merely rejected the losers would be a correctness problem of its own: a
	// pass told "conflict" cannot tell whether the work is under way or lost.
	if len(created) == 0 {
		t.Fatalf("all %d concurrent requests failed; the idempotency key rejected "+
			"every caller instead of collapsing them onto one record", concurrentAttempts)
	}
	first := created[0]
	for _, id := range created {
		if id != first {
			t.Fatalf("two different acquisitions were created for one request key: %q and %q\n"+
				"\toverlapping automation passes would each drive their own pull and "+
				"their own recreation", first, id)
		}
	}

	// And the database holds exactly one row, whatever the callers were told.
	acquisitions, _, err := db.Acquisitions.List(ctx, store.AcquisitionFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(acquisitions) != 1 {
		t.Fatalf("the acquisitions table holds %d rows, want 1", len(acquisitions))
	}
	if acquisitions[0].Target.Digest != acqDigest {
		t.Errorf("stored digest = %q, want the approved %q",
			acquisitions[0].Target.Digest, acqDigest)
	}
}

// TestConcurrentLineageUpsertsLeaveOneCoherentRow.
//
// Reconciliation, a verified recreation and a completed rollback can all write
// the same row. None of them may produce a torn one: a record carrying one
// writer's digest and another's tracking reference would point update discovery
// at the wrong repository.
//
// Note what is NOT asserted: that the digest only ever moves forward. It must
// not -- reconciliation exists to move it BACKWARD when the host says the
// container is running something older, which is exactly what happens after a
// rollback. The invariant is coherence, not monotonicity.
func TestConcurrentLineageUpsertsLeaveOneCoherentRow(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.Lineage.Upsert(ctx, trackedRow("web")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < concurrentAttempts; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			row := trackedRow("web")
			// Alternate between the two digests a real collision produces: the
			// replacement's, written by a recreation, and the original's,
			// written by the rollback that undid it.
			if n%2 == 0 {
				row.RunningDigest = lineageDigestB
				row.Origin = domain.LineageRecreated
			}
			row.UpdatedAt = time.Now().UTC()

			<-start
			if err := db.Lineage.Upsert(ctx, row); err != nil {
				t.Errorf("concurrent upsert: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	final, err := db.Lineage.Get(ctx, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Exactly one row, and a whole one.
	all, err := db.Lineage.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("the lineage table holds %d rows for one container, want 1", len(all))
	}

	if final.RunningDigest != lineageDigestA && final.RunningDigest != lineageDigestB {
		t.Fatalf("RunningDigest = %q, which no writer wrote", final.RunningDigest)
	}
	// The tracking reference is the one thing no concurrent writer varies, so a
	// change to it here is corruption rather than a race between two intents.
	if final.TrackingReference != "docker.io/library/nginx:1.27" {
		t.Fatalf("TrackingReference = %q; concurrent writes corrupted what the "+
			"container follows", final.TrackingReference)
	}
	if final.Repository != "library/nginx" {
		t.Errorf("Repository = %q; the row is torn between writers", final.Repository)
	}
	if !final.Tracked() {
		t.Errorf("the row is no longer tracked after concurrent writes: %+v", final)
	}
}

// TestAConcurrentUpsertCannotIntroduceAnUnapprovedDigest.
//
// The row is validated on the way in whichever goroutine wins. A writer
// presenting a malformed digest is refused rather than stored, so no amount of
// contention can leave lineage naming something that could not have come out of
// the pipeline.
func TestAConcurrentUpsertCannotIntroduceAnUnapprovedDigest(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.Lineage.Upsert(ctx, trackedRow("web")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})

	for _, bad := range []string{
		"not-a-digest",
		"sha256:short",
		"sha256:" + strings.Repeat("z", 64), // right shape, not hex
	} {
		wg.Add(1)
		go func(digest string) {
			defer wg.Done()
			row := trackedRow("web")
			row.RunningDigest = digest
			<-start
			if err := db.Lineage.Upsert(ctx, row); err == nil {
				t.Errorf("a malformed running digest %q was accepted", digest)
			}
		}(bad)
	}
	close(start)
	wg.Wait()

	final, err := db.Lineage.Get(ctx, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.RunningDigest != lineageDigestA {
		t.Fatalf("RunningDigest = %q, want the original %q left untouched",
			final.RunningDigest, lineageDigestA)
	}
}
