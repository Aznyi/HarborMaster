package store_test

import (
	"errors"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/store"
)

// Two questions about a container, and why they must not share an operation.
//
// # The defect this pins
//
// The inventory keeps a container's row after it leaves the host, with
// present = 0, because that row is what a detail page, an Activity entry, an
// execution record and an audit trail read afterwards. ContainerRepository.Get
// returns it deliberately.
//
// planEvidence.ContainerPresent read only whether Get returned a row, and
// treated success as "the container exists". A container removed from the host
// therefore passed the presence gate standing in front of an image acquisition,
// on the strength of its own tombstone. The check was one line away and the
// helper's name promised it had been done.
//
// The fix is an operation rather than a field: Get answers the historical
// question, GetPresent answers the current one, and a call site says which it
// is asking. These tests pin both contracts so a future caller cannot conflate
// them again by omission.

func TestGetReturnsTheHistoricalRecordWhetherPresentOrNot(t *testing.T) {
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")

	// present = 1
	detail, err := db.Containers.Get(ctx, "svc-a-id")
	if err != nil || detail == nil {
		t.Fatalf("Get while present: %v", err)
	}
	if !detail.Overview.Present {
		t.Fatal("a present container did not report itself present")
	}

	// present = 0, through the real inventory path.
	commitContainers(t, db, "other")

	detail, err = db.Containers.Get(ctx, "svc-a-id")
	if err != nil {
		t.Fatalf("Get must still return a departed container's record: %v", err)
	}
	if detail == nil {
		t.Fatal("Get returned nothing for a departed container; history would be lost")
	}
	if detail.Overview.Present {
		t.Error("a departed container reports itself present")
	}
	// The record is still usable: this is what a detail page renders.
	if detail.Overview.Name != "svc-a" {
		t.Errorf("the historical record lost its name: %q", detail.Overview.Name)
	}

	// A row that never existed is still not found.
	if _, err := db.Containers.Get(ctx, "never-existed"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get for an unknown id = %v, want ErrNotFound", err)
	}
}

func TestGetPresentRefusesADepartedContainer(t *testing.T) {
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")

	detail, err := db.Containers.GetPresent(ctx, "svc-a-id")
	if err != nil || detail == nil {
		t.Fatalf("GetPresent while present: %v", err)
	}
	if detail.Overview.ID != "svc-a-id" {
		t.Errorf("returned the wrong container: %q", detail.Overview.ID)
	}

	commitContainers(t, db, "other")

	// THE CONTRACT. Both absences collapse to ErrNotFound: a caller asking this
	// question wants to know whether it may act, and "no such container" and
	// "there was one and it is gone" are the same answer to that.
	if _, err := db.Containers.GetPresent(ctx, "svc-a-id"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetPresent for a departed container = %v, want ErrNotFound -- a "+
			"tombstone must never satisfy a presence check", err)
	}
	if _, err := db.Containers.GetPresent(ctx, "never-existed"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetPresent for an unknown id = %v, want ErrNotFound", err)
	}

	// And Get still resolves it, so nothing historical was broken to achieve
	// this. The two operations disagree on purpose.
	if _, err := db.Containers.Get(ctx, "svc-a-id"); err != nil {
		t.Errorf("Get stopped returning the historical record: %v", err)
	}
}

func TestPresenceIsDerivedAndReversible(t *testing.T) {
	// Presence is current evidence, not a destructive transition. Nothing is
	// written when a container leaves and nothing has to be undone when the
	// same id is reported again.
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")

	commitContainers(t, db, "other")
	if _, err := db.Containers.GetPresent(ctx, "svc-a-id"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("while absent: %v", err)
	}

	commitContainers(t, db, "svc-a")
	detail, err := db.Containers.GetPresent(ctx, "svc-a-id")
	if err != nil || detail == nil {
		t.Fatalf("a returning container is not present again: %v", err)
	}
	if !detail.Overview.Present {
		t.Error("the returning container's row still says absent")
	}
}

func TestANewContainerDoesNotSatisfyTheOldIdsPresenceCheck(t *testing.T) {
	// Same workload name, new Docker id. Presence is asked about an INSTANCE,
	// so the replacement must not make the departed id look present -- that
	// would let evidence bound to the old container act on the new one.
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")

	commitContainersAs(t, db, map[string]string{"svc-a": "svc-a-v2-id"})

	if _, err := db.Containers.GetPresent(ctx, "svc-a-id"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the old id satisfied a presence check after recreation: %v", err)
	}
	if _, err := db.Containers.GetPresent(ctx, "svc-a-v2-id"); err != nil {
		t.Fatalf("the replacement is not present: %v", err)
	}
	// The old instance's record survives for history.
	if _, err := db.Containers.Get(ctx, "svc-a-id"); err != nil {
		t.Errorf("the departed instance's record was lost: %v", err)
	}
}

func TestGetPresentCostsOneLookupPerCall(t *testing.T) {
	// The presence question must not become a second full detail load for every
	// caller that asks it. The presence probe is a single indexed read, and the
	// detail is loaded only when the answer is yes.
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")
	commitContainers(t, db, "other")

	// An absent container costs the probe and nothing else: no detail, no
	// config, no labels, no networks.
	for i := 0; i < 200; i++ {
		if _, err := db.Containers.GetPresent(ctx, "svc-a-id"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
}
