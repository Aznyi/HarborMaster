package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Acquisition persistence tests.
//
// This table backs HarborMaster's ONLY Docker mutation, so the properties that
// matter are about what cannot happen:
//
//   - A target without a digest cannot be stored. There is no such thing as a
//     safe record of an unpinned pull.
//   - Two acquisitions for the same image cannot be active at once, even when
//     the requests race. The partial unique index is the guarantee.
//   - A terminal record is never moved. Nothing rewrites what happened.
//   - No column can hold a credential, a daemon error, or a registry response.

const acqDigest = "sha256:" + "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func acquisitionFor(containerID, digest string) domain.Acquisition {
	now := time.Now().UTC()
	return domain.Acquisition{
		AcquisitionID: domain.NewAcquisitionID(),
		PlanID:        "plan_00112233445566778899",
		ContainerID:   containerID,
		ContainerName: "web",
		Target: domain.AcquisitionTarget{
			Registry:   "docker.io",
			Repository: "library/nginx",
			Digest:     digest,
			Reference:  "nginx:1.27.1",
			Platform:   domain.Platform{OS: "linux", Architecture: "amd64"},
		},
		State:       domain.AcquisitionQueued,
		RequestedAt: now,
		ExpiresAt:   now.Add(time.Hour),
		PlanDigest:  strings.Repeat("f", 64),
	}
}

func createAcquisition(t *testing.T, db *store.DB, acquisition domain.Acquisition) domain.Acquisition {
	t.Helper()
	created, err := db.Acquisitions.Create(context.Background(), acquisition, time.Now().UTC())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

// ------------------------------------------------------------ round trip --

func TestAcquisitionRoundTrips(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	created := createAcquisition(t, db, acquisitionFor("container-a", acqDigest))

	stored, err := db.Acquisitions.Get(ctx, created.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if stored.AcquisitionID != created.AcquisitionID || stored.PlanID != created.PlanID {
		t.Errorf("identity did not round-trip: %+v", stored)
	}
	if stored.Target.Digest != acqDigest || stored.Target.Repository != "library/nginx" {
		t.Errorf("target did not round-trip: %+v", stored.Target)
	}
	if stored.Target.Platform.String() != "linux/amd64" {
		t.Errorf("platform did not round-trip: %q", stored.Target.Platform.String())
	}
	if stored.State != domain.AcquisitionQueued {
		t.Errorf("state = %q, want queued", stored.State)
	}
	if stored.PlanDigest != created.PlanDigest {
		t.Error("the approved plan fingerprint did not round-trip")
	}

	// The request itself is the first audit entry, so the trail describes the
	// whole life of the acquisition rather than starting once work began.
	events, err := db.Acquisitions.Events(ctx, created.AcquisitionID, 100)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || events[0].State != domain.AcquisitionQueued {
		t.Errorf("events = %+v, want one queued entry", events)
	}
}

// A record that does not name one immutable image cannot be stored. Reaching
// this means a layer above let something through, and storing it would record
// an approval for a pull that could fetch anything.
func TestAnUnpinnedTargetCannotBeStored(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for name, mutate := range map[string]func(*domain.Acquisition){
		"no digest":     func(a *domain.Acquisition) { a.Target.Digest = "" },
		"a tag":         func(a *domain.Acquisition) { a.Target.Digest = "1.27.1" },
		"no repository": func(a *domain.Acquisition) { a.Target.Repository = "" },
		"local registry": func(a *domain.Acquisition) {
			a.Target.Registry = "registry.local:5000"
		},
		"loopback registry": func(a *domain.Acquisition) { a.Target.Registry = "127.0.0.1" },
	} {
		t.Run(name, func(t *testing.T) {
			acquisition := acquisitionFor("container-a", acqDigest)
			mutate(&acquisition)

			if _, err := db.Acquisitions.Create(ctx, acquisition, time.Now().UTC()); err == nil {
				t.Fatalf("target %+v should be refused", acquisition.Target)
			}
		})
	}
}

// -------------------------------------------------- duplicate suppression --

// At most one acquisition per (container, digest) may be active. The check is a
// database invariant rather than something the service performs and hopes to
// win.
func TestASecondActiveAcquisitionForOneTargetIsRefused(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	createAcquisition(t, db, acquisitionFor("container-a", acqDigest))

	_, err := db.Acquisitions.Create(ctx, acquisitionFor("container-a", acqDigest), time.Now().UTC())
	if !errors.Is(err, store.ErrAcquisitionActive) {
		t.Fatalf("second request = %v, want ErrAcquisitionActive", err)
	}

	// A DIFFERENT digest for the same container is not a duplicate: it is a
	// different image.
	other := acquisitionFor("container-a", "sha256:"+strings.Repeat("b", 64))
	if _, err := db.Acquisitions.Create(ctx, other, time.Now().UTC()); err != nil {
		t.Errorf("a different digest should be allowed: %v", err)
	}

	// And a different container may acquire the same image.
	if _, err := db.Acquisitions.Create(ctx,
		acquisitionFor("container-b", acqDigest), time.Now().UTC()); err != nil {
		t.Errorf("a different container should be allowed: %v", err)
	}
}

// Once an acquisition has finished, the same image may be acquired again --
// which is exactly what a retry after a transient failure needs.
func TestATerminalAcquisitionDoesNotBlockANewOne(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first := createAcquisition(t, db, acquisitionFor("container-a", acqDigest))

	moved, err := db.Acquisitions.Advance(ctx, store.StateChange{
		AcquisitionID: first.AcquisitionID,
		To:            domain.AcquisitionFailed,
		Failure:       domain.AcquisitionFailureTransfer,
		Message:       "the transfer did not complete",
	}, time.Now().UTC())
	if err != nil || !moved {
		t.Fatalf("Advance: moved=%v err=%v", moved, err)
	}

	if _, err := db.Acquisitions.Create(ctx,
		acquisitionFor("container-a", acqDigest), time.Now().UTC()); err != nil {
		t.Errorf("a retry after a failure should be allowed: %v", err)
	}
}

// The race that a button an operator can double-click actually produces.
func TestConcurrentRequestsForOneTargetLeaveOneActive(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	const writers = 16
	var (
		wait     sync.WaitGroup
		mu       sync.Mutex
		accepted int
		other    []error
	)

	wait.Add(writers)
	for writer := 0; writer < writers; writer++ {
		go func() {
			defer wait.Done()
			_, err := db.Acquisitions.Create(ctx,
				acquisitionFor("container-a", acqDigest), time.Now().UTC())

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, store.ErrAcquisitionActive):
				// The expected refusal.
			default:
				other = append(other, err)
			}
		}()
	}
	wait.Wait()

	if len(other) > 0 {
		t.Fatalf("a racing duplicate must be a refusal, not a fault: %v", other[0])
	}
	if accepted != 1 {
		t.Errorf("%d requests were accepted, want exactly 1", accepted)
	}

	total, perRegistry, err := db.Acquisitions.ActiveCount(ctx, "docker.io")
	if err != nil {
		t.Fatalf("ActiveCount: %v", err)
	}
	if total != 1 || perRegistry != 1 {
		t.Errorf("active counts = %d total, %d for the registry; want 1 and 1", total, perRegistry)
	}
}

// A retried request with the same idempotency key returns the EXISTING record
// rather than starting a second download.
func TestAnIdempotencyKeyReturnsTheExistingRecord(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first := acquisitionFor("container-a", acqDigest)
	first.RequestKey = "operator-click-1"
	created := createAcquisition(t, db, first)

	second := acquisitionFor("container-a", acqDigest)
	second.RequestKey = "operator-click-1"

	repeated, err := db.Acquisitions.Create(ctx, second, time.Now().UTC())
	if err != nil {
		t.Fatalf("a repeated key should return the existing record: %v", err)
	}
	if repeated.AcquisitionID != created.AcquisitionID {
		t.Errorf("got a new acquisition %q, want the existing %q",
			repeated.AcquisitionID, created.AcquisitionID)
	}

	// Two UNKEYED requests must not collide with each other, which is why the
	// index is partial.
	third := acquisitionFor("container-b", acqDigest)
	fourth := acquisitionFor("container-c", acqDigest)
	if _, err := db.Acquisitions.Create(ctx, third, time.Now().UTC()); err != nil {
		t.Fatalf("unkeyed request: %v", err)
	}
	if _, err := db.Acquisitions.Create(ctx, fourth, time.Now().UTC()); err != nil {
		t.Errorf("a second unkeyed request must not collide with the first: %v", err)
	}
}

// ------------------------------------------------------------ lifecycle --

func TestTheLifecycleAdvancesAndRecordsEachStep(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created := createAcquisition(t, db, acquisitionFor("container-a", acqDigest))

	for _, step := range []struct {
		from []domain.AcquisitionState
		to   domain.AcquisitionState
	}{
		{[]domain.AcquisitionState{domain.AcquisitionQueued}, domain.AcquisitionValidating},
		{[]domain.AcquisitionState{domain.AcquisitionValidating}, domain.AcquisitionPulling},
		{[]domain.AcquisitionState{domain.AcquisitionPulling}, domain.AcquisitionVerifying},
	} {
		moved, err := db.Acquisitions.Advance(ctx, store.StateChange{
			AcquisitionID: created.AcquisitionID,
			From:          step.from,
			To:            step.to,
			MarkStarted:   step.to == domain.AcquisitionValidating,
		}, now)
		if err != nil || !moved {
			t.Fatalf("advance to %s: moved=%v err=%v", step.to, moved, err)
		}
	}

	moved, err := db.Acquisitions.Advance(ctx, store.StateChange{
		AcquisitionID:    created.AcquisitionID,
		From:             []domain.AcquisitionState{domain.AcquisitionVerifying},
		To:               domain.AcquisitionSucceeded,
		Message:          "the image is present locally",
		AcquiredImageID:  "sha256:image1",
		AcquiredDigest:   acqDigest,
		AcquiredPlatform: domain.Platform{OS: "linux", Architecture: "amd64"},
		SizeBytes:        123456,
		Layers:           4,
		BytesTransferred: 99999,
	}, now)
	if err != nil || !moved {
		t.Fatalf("advance to succeeded: moved=%v err=%v", moved, err)
	}

	stored, err := db.Acquisitions.Get(ctx, created.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.State != domain.AcquisitionSucceeded {
		t.Errorf("state = %q", stored.State)
	}
	if stored.AcquiredImageID != "sha256:image1" || stored.AcquiredDigest != acqDigest {
		t.Errorf("verification result did not round-trip: %+v", stored)
	}
	if stored.SizeBytes != 123456 || stored.Layers != 4 {
		t.Errorf("transfer summary did not round-trip: %+v", stored)
	}
	if stored.StartedAt == nil || stored.CompletedAt == nil {
		t.Errorf("timestamps = started %v completed %v", stored.StartedAt, stored.CompletedAt)
	}

	events, err := db.Acquisitions.Events(ctx, created.AcquisitionID, 100)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("audit trail has %d entries, want 5", len(events))
	}
	// Oldest first: the trail is read as the narrative of one operation.
	if events[0].State != domain.AcquisitionQueued ||
		events[len(events)-1].State != domain.AcquisitionSucceeded {
		t.Errorf("the trail is not in lifecycle order: %+v", events)
	}
}

// A terminal record is never moved. This is what makes the audit trail an audit
// trail rather than a status field.
func TestATerminalAcquisitionCannotBeMoved(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created := createAcquisition(t, db, acquisitionFor("container-a", acqDigest))

	if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
		AcquisitionID: created.AcquisitionID,
		To:            domain.AcquisitionCancelled,
		Message:       "cancelled by an operator",
	}, now); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	for _, to := range []domain.AcquisitionState{
		domain.AcquisitionPulling, domain.AcquisitionSucceeded, domain.AcquisitionFailed,
	} {
		moved, err := db.Acquisitions.Advance(ctx, store.StateChange{
			AcquisitionID: created.AcquisitionID,
			To:            to,
		}, now)
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
		if moved {
			t.Errorf("a cancelled acquisition was moved to %q", to)
		}
	}

	stored, err := db.Acquisitions.Get(ctx, created.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.State != domain.AcquisitionCancelled {
		t.Errorf("state = %q, want cancelled", stored.State)
	}
}

// An advance from the wrong state does nothing and says so. This is how the
// service learns it lost a race to a cancellation.
func TestAdvanceFromTheWrongStateReportsNoChange(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	created := createAcquisition(t, db, acquisitionFor("container-a", acqDigest))

	moved, err := db.Acquisitions.Advance(ctx, store.StateChange{
		AcquisitionID: created.AcquisitionID,
		From:          []domain.AcquisitionState{domain.AcquisitionPulling},
		To:            domain.AcquisitionVerifying,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if moved {
		t.Error("an advance from a state the acquisition is not in should not apply")
	}
}

// ------------------------------------------------------------- progress --

// Progress is recorded only while pulling. An update arriving after
// cancellation would extend the trail of an acquisition that has finished.
func TestProgressIsRecordedOnlyWhilePulling(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created := createAcquisition(t, db, acquisitionFor("container-a", acqDigest))

	// Queued: nothing recorded.
	if err := db.Acquisitions.RecordProgress(ctx, created.AcquisitionID,
		"Downloading", 100, 1, now, 100); err != nil {
		t.Fatalf("RecordProgress: %v", err)
	}
	stored, err := db.Acquisitions.Get(ctx, created.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Progress != "" {
		t.Errorf("progress was recorded for a queued acquisition: %q", stored.Progress)
	}

	// Pulling: recorded.
	for _, to := range []domain.AcquisitionState{
		domain.AcquisitionValidating, domain.AcquisitionPulling,
	} {
		if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
			AcquisitionID: created.AcquisitionID, To: to,
		}, now); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}

	if err := db.Acquisitions.RecordProgress(ctx, created.AcquisitionID,
		"Downloading", 5000, 3, now, 100); err != nil {
		t.Fatalf("RecordProgress: %v", err)
	}

	stored, err = db.Acquisitions.Get(ctx, created.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Progress != "Downloading" || stored.BytesTransferred != 5000 || stored.Layers != 3 {
		t.Errorf("progress did not record: %+v", stored)
	}

	// Byte and layer counters only advance, so a message for a small layer does
	// not appear to undo progress.
	if err := db.Acquisitions.RecordProgress(ctx, created.AcquisitionID,
		"Downloading", 10, 1, now, 100); err != nil {
		t.Fatalf("RecordProgress: %v", err)
	}
	stored, err = db.Acquisitions.Get(ctx, created.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.BytesTransferred != 5000 || stored.Layers != 3 {
		t.Errorf("counters went backwards: %+v", stored)
	}
}

// Hostile progress text is sanitised on the way into the column, not on the way
// out. The adapter already bounds it; this is the second of three independent
// defences, and the one that holds if a caller bypasses the first.
func TestHostileProgressTextIsSanitisedOnWrite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created := createAcquisition(t, db, acquisitionFor("container-a", acqDigest))
	for _, to := range []domain.AcquisitionState{
		domain.AcquisitionValidating, domain.AcquisitionPulling,
	} {
		if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
			AcquisitionID: created.AcquisitionID, To: to,
		}, now); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}

	hostile := "Downloading\r\n\x00<script>alert(1)</script>" + strings.Repeat("A", 5000)
	if err := db.Acquisitions.RecordProgress(ctx, created.AcquisitionID,
		hostile, 1, 1, now, 100); err != nil {
		t.Fatalf("RecordProgress: %v", err)
	}

	stored, err := db.Acquisitions.Get(ctx, created.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(stored.Progress) > domain.MaxAcquisitionProgressBytes {
		t.Errorf("stored progress is %d bytes, want at most %d",
			len(stored.Progress), domain.MaxAcquisitionProgressBytes)
	}
	if strings.ContainsAny(stored.Progress, "\r\n\x00") {
		t.Errorf("stored progress carries control characters: %q", stored.Progress)
	}
}

// The audit trail is capped per acquisition, so a chatty pull cannot turn one
// operator action into an unbounded number of rows.
func TestTheAuditTrailIsCappedPerAcquisition(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created := createAcquisition(t, db, acquisitionFor("container-a", acqDigest))
	for _, to := range []domain.AcquisitionState{
		domain.AcquisitionValidating, domain.AcquisitionPulling,
	} {
		if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
			AcquisitionID: created.AcquisitionID, To: to,
		}, now); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}

	const cap = 10
	for index := 0; index < 500; index++ {
		if err := db.Acquisitions.RecordProgress(ctx, created.AcquisitionID,
			fmt.Sprintf("Downloading %d", index), int64(index), 1, now, cap); err != nil {
			t.Fatalf("RecordProgress: %v", err)
		}
	}

	events, err := db.Acquisitions.Events(ctx, created.AcquisitionID, 1000)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) > cap {
		t.Errorf("the trail grew to %d entries, want at most the cap of %d", len(events), cap)
	}

	// The transitions written first survive: they are the audit trail, and
	// dropping them to make room for progress noise would lose the history to
	// the chatter.
	if events[0].State != domain.AcquisitionQueued {
		t.Errorf("the first entry is %q, want the original request", events[0].State)
	}

	// Progress itself keeps flowing to the record even once the trail is full.
	stored, err := db.Acquisitions.Get(ctx, created.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.BytesTransferred != 499 {
		t.Errorf("bytes = %d, want the latest despite the capped trail", stored.BytesTransferred)
	}
}

// ------------------------------------------------------------- recovery --

// An acquisition left mid-flight by a crash is a claim about a process that no
// longer exists. It is failed honestly rather than resumed: the transfer was
// never verified, and an unverified image must never be recorded as acquired.
func TestInterruptedAcquisitionsAreFailedRatherThanResumed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	pulling := createAcquisition(t, db, acquisitionFor("container-a", acqDigest))
	for _, to := range []domain.AcquisitionState{
		domain.AcquisitionValidating, domain.AcquisitionPulling,
	} {
		if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
			AcquisitionID: pulling.AcquisitionID, To: to,
		}, now); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}

	// A queued one is NOT touched: it never started, so there is nothing
	// unresolved about it.
	queued := createAcquisition(t, db, acquisitionFor("container-b", acqDigest))

	recovered, err := db.Acquisitions.RecoverInterrupted(ctx, now)
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if recovered != 1 {
		t.Errorf("recovered %d, want 1", recovered)
	}

	after, err := db.Acquisitions.Get(ctx, pulling.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != domain.AcquisitionFailed {
		t.Errorf("state = %q, want failed", after.State)
	}
	if after.Failure != domain.AcquisitionFailureInternal {
		t.Errorf("failure = %q", after.Failure)
	}
	if after.CompletedAt == nil {
		t.Error("a recovered acquisition should be completed")
	}
	// Never reported as succeeded: nothing verified it.
	if after.AcquiredDigest != "" {
		t.Errorf("a recovered acquisition claims an acquired digest: %q", after.AcquiredDigest)
	}

	stillQueued, err := db.Acquisitions.Get(ctx, queued.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stillQueued.State != domain.AcquisitionQueued {
		t.Errorf("a queued acquisition was disturbed by recovery: %q", stillQueued.State)
	}
}

// A request that waited past its deadline is abandoned unstarted. Running it
// later would be acting on an approval whose evidence has aged.
func TestStaleQueuedRequestsExpire(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	stale := acquisitionFor("container-a", acqDigest)
	stale.ExpiresAt = now.Add(-time.Hour)
	created := createAcquisition(t, db, stale)

	fresh := createAcquisition(t, db, acquisitionFor("container-b", acqDigest))

	expired, err := db.Acquisitions.ExpireStale(ctx, now, 100)
	if err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}
	if expired != 1 {
		t.Errorf("expired %d, want 1", expired)
	}

	after, err := db.Acquisitions.Get(ctx, created.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != domain.AcquisitionExpired {
		t.Errorf("state = %q, want expired", after.State)
	}

	stillQueued, err := db.Acquisitions.Get(ctx, fresh.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stillQueued.State != domain.AcquisitionQueued {
		t.Errorf("a request inside its deadline expired: %q", stillQueued.State)
	}
}

// --------------------------------------------------------------- listing --

func TestAcquisitionListingFiltersAndPaginates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for index := 0; index < 12; index++ {
		acquisition := acquisitionFor(fmt.Sprintf("container-%02d", index), acqDigest)
		created := createAcquisition(t, db, acquisition)

		if index%2 == 0 {
			if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
				AcquisitionID: created.AcquisitionID,
				To:            domain.AcquisitionFailed,
				Failure:       domain.AcquisitionFailureDigestMismatch,
			}, now); err != nil {
				t.Fatalf("advance: %v", err)
			}
		}
	}

	page, total, err := db.Acquisitions.List(ctx, store.AcquisitionFilter{Page: store.Page{Limit: 5}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 12 || len(page) != 5 {
		t.Errorf("total = %d, page = %d, want 12 and 5", total, len(page))
	}

	active, total, err := db.Acquisitions.List(ctx, store.AcquisitionFilter{
		ActiveOnly: true, Page: store.Page{Limit: 50},
	})
	if err != nil {
		t.Fatalf("List(active): %v", err)
	}
	if total != 6 || len(active) != 6 {
		t.Errorf("active total = %d, want 6", total)
	}
	for _, acquisition := range active {
		if !acquisition.State.Active() {
			t.Fatalf("the active filter leaked %q", acquisition.State)
		}
	}

	mismatches, _, err := db.Acquisitions.List(ctx, store.AcquisitionFilter{
		Failures: []domain.AcquisitionFailure{domain.AcquisitionFailureDigestMismatch},
		Page:     store.Page{Limit: 50},
	})
	if err != nil {
		t.Fatalf("List(failure): %v", err)
	}
	if len(mismatches) != 6 {
		t.Errorf("digest mismatches = %d, want 6", len(mismatches))
	}
}

// A sort field outside the allowlist falls back to the default rather than
// reaching the SQL text.
func TestAnUnknownAcquisitionSortFieldFallsBack(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	createAcquisition(t, db, acquisitionFor("container-a", acqDigest))
	createAcquisition(t, db, acquisitionFor("container-b", acqDigest))

	for _, attempt := range []string{
		"requested_at; DROP TABLE acquisitions",
		"1) UNION SELECT * FROM containers --",
		"target_digest",
		"",
	} {
		items, total, err := db.Acquisitions.List(ctx, store.AcquisitionFilter{
			Sort: attempt, Page: store.Page{Limit: 10},
		})
		if err != nil {
			t.Fatalf("List(sort=%q): %v", attempt, err)
		}
		if total != 2 || len(items) != 2 {
			t.Errorf("List(sort=%q) returned %d of %d", attempt, len(items), total)
		}
		if store.ValidAcquisitionSortField(attempt) {
			t.Errorf("%q must not be an accepted sort field", attempt)
		}
	}

	for _, field := range []string{"requestedAt", "completedAt", "state", "container", "id"} {
		if !store.ValidAcquisitionSortField(field) {
			t.Errorf("%q should be a sortable field", field)
		}
		if _, _, err := db.Acquisitions.List(ctx, store.AcquisitionFilter{Sort: field}); err != nil {
			t.Errorf("List(sort=%q): %v", field, err)
		}
	}
}

func TestAcquisitionSummaryCountsByState(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	succeeded := createAcquisition(t, db, acquisitionFor("container-a", acqDigest))
	if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
		AcquisitionID: succeeded.AcquisitionID, To: domain.AcquisitionSucceeded,
	}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}

	failed := createAcquisition(t, db, acquisitionFor("container-b", acqDigest))
	if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
		AcquisitionID: failed.AcquisitionID,
		To:            domain.AcquisitionFailed,
		Failure:       domain.AcquisitionFailureDigestMismatch,
	}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}

	createAcquisition(t, db, acquisitionFor("container-c", acqDigest))

	summary, err := db.Acquisitions.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.Total != 3 || summary.Active != 1 || summary.Succeeded != 1 || summary.Failed != 1 {
		t.Errorf("summary = %+v", summary)
	}
	if summary.ByFailure[domain.AcquisitionFailureDigestMismatch] != 1 {
		t.Errorf("failure counts = %v", summary.ByFailure)
	}
	if summary.LastCompletedAt == nil {
		t.Error("summary should report when something last completed")
	}
}

// ------------------------------------------------------------ retention --

// A completed record is the evidence that an image was downloaded, so the most
// recent one per container is never pruned.
func TestRetentionKeepsTheMostRecentRecordPerContainer(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-365 * 24 * time.Hour)

	for index := 0; index < 3; index++ {
		acquisition := acquisitionFor("container-a", fmt.Sprintf("sha256:%s", strings.Repeat(fmt.Sprint(index), 64)))
		created := createAcquisition(t, db, acquisition)
		if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
			AcquisitionID: created.AcquisitionID, To: domain.AcquisitionSucceeded,
		}, old); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}

	pruned, err := db.Acquisitions.Prune(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned != 2 {
		t.Errorf("pruned %d, want 2", pruned)
	}

	_, total, err := db.Acquisitions.List(ctx, store.AcquisitionFilter{
		ContainerID: "container-a", Page: store.Page{Limit: 50},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Errorf("%d records remain, want the most recent one kept", total)
	}
}

// Pruning an acquisition removes its audit trail with it. An orphaned progress
// row is noise rather than history.
func TestPruningRemovesTheAuditTrail(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-365 * 24 * time.Hour)

	first := createAcquisition(t, db, acquisitionFor("container-a", acqDigest))
	if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
		AcquisitionID: first.AcquisitionID, To: domain.AcquisitionSucceeded,
	}, old); err != nil {
		t.Fatalf("advance: %v", err)
	}
	second := createAcquisition(t, db, acquisitionFor("container-a", "sha256:"+strings.Repeat("c", 64)))
	if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
		AcquisitionID: second.AcquisitionID, To: domain.AcquisitionSucceeded,
	}, old); err != nil {
		t.Fatalf("advance: %v", err)
	}

	if _, err := db.Acquisitions.Prune(ctx, time.Now().UTC(), 100); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	var orphans int
	if err := db.SQL().QueryRow(`
		SELECT COUNT(*) FROM acquisition_events e
		WHERE NOT EXISTS (
			SELECT 1 FROM acquisitions a WHERE a.acquisition_id = e.acquisition_id
		)`).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d orphaned audit rows remain", orphans)
	}
}

// ------------------------------------------------------------- schema --

// The table has no column that could hold a credential, a raw daemon error, or
// a registry response body. Asserted against the schema, because the absence is
// the control.
func TestTheAcquisitionSchemaHasNoPlaceForASecret(t *testing.T) {
	db := openTestDB(t)

	rows, err := db.SQL().Query(`SELECT name FROM pragma_table_info('acquisitions')`)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		lowered := strings.ToLower(name)
		for _, forbidden := range []string{
			"auth", "credential", "password", "secret", "token", "bearer",
			"header", "body", "response", "raw",
		} {
			if strings.Contains(lowered, forbidden) {
				t.Errorf("acquisitions has a %q column, which could hold %q", name, forbidden)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}

// Nor does it have a column describing a container being changed, because that
// operation does not exist.
func TestTheAcquisitionSchemaDescribesNoContainerChange(t *testing.T) {
	db := openTestDB(t)

	for _, column := range []string{
		"applied", "applied_at", "rolled_back", "restored_at",
		"container_updated", "recreated_at", "restarted_at",
	} {
		var name string
		err := db.SQL().QueryRow(
			`SELECT name FROM pragma_table_info('acquisitions') WHERE name = ?`, column).Scan(&name)
		if err == nil {
			t.Errorf("acquisitions has a %q column; HarborMaster acquires images and "+
				"does not apply them", column)
		}
	}
}

// The active-target index is what makes duplicate suppression a guarantee.
// Asserted against the schema so removing it is a visible failure.
func TestTheActiveAcquisitionIndexIsUniqueAndPartial(t *testing.T) {
	db := openTestDB(t)

	var sqlText string
	err := db.SQL().QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_acquisition_active_target'`,
	).Scan(&sqlText)
	if err != nil {
		t.Fatalf("the active-target index is missing: %v", err)
	}

	upper := strings.ToUpper(sqlText)
	if !strings.Contains(upper, "UNIQUE") {
		t.Errorf("the index is not unique: %s", sqlText)
	}
	if !strings.Contains(upper, "WHERE") {
		t.Errorf("the index is not partial, so a finished acquisition would block a retry: %s", sqlText)
	}
	for _, column := range []string{"container_id", "target_digest"} {
		if !strings.Contains(sqlText, column) {
			t.Errorf("the index does not cover %s: %s", column, sqlText)
		}
	}
}
