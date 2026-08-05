package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Image intelligence persistence tests.
//
// The properties that matter here are about WHAT SURVIVES A FAILURE and WHAT
// THE SCHEDULER SEES: a failed lookup must not overwrite a good answer, an
// inventory refresh must not re-queue work already done, and "what is due" must
// be an indexed question rather than a scan.

func imageSeed(reference string) store.ImageReferenceSeed {
	return store.ImageReferenceSeed{
		Reference:   reference,
		Familiar:    reference,
		Kind:        domain.RegistryDockerHub,
		Registry:    "docker.io",
		Namespace:   "library",
		Repository:  "library/nginx",
		Tag:         "1.25",
		LocalDigest: "sha256:" + repeat("a", 64),
		ImageID:     "sha256:" + repeat("1", 64),
		Platform:    domain.Platform{OS: "linux", Architecture: "amd64"},
		Supported:   true,
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s[0])
	}
	return string(out)
}

// syncOne stores one reference and returns the tracked record.
func syncOne(t *testing.T, db *store.DB, seed store.ImageReferenceSeed) domain.ImageIntel {
	t.Helper()
	ctx := context.Background()

	if _, err := db.ImageIntel.SyncReferences(ctx,
		[]store.ImageReferenceSeed{seed}, time.Now().UTC()); err != nil {
		t.Fatalf("SyncReferences: %v", err)
	}
	record, err := db.ImageIntel.Get(ctx, seed.Reference)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return record
}

func TestImageReferenceRoundTrips(t *testing.T) {
	db := openTestDB(t)
	seed := imageSeed("docker.io/library/nginx:1.25")

	record := syncOne(t, db, seed)

	if record.Reference != seed.Reference || record.Familiar != seed.Familiar {
		t.Errorf("identity did not round-trip: %+v", record)
	}
	if record.Kind != domain.RegistryDockerHub || record.Registry != "docker.io" {
		t.Errorf("registry did not round-trip: %+v", record)
	}
	if record.LocalDigest != seed.LocalDigest || record.ImageID != seed.ImageID {
		t.Errorf("local state did not round-trip: %+v", record)
	}
	if record.Platform.String() != "linux/amd64" {
		t.Errorf("platform = %q", record.Platform.String())
	}
	// A new reference is scheduled immediately, so a newly deployed container
	// is not invisible until the next interval.
	if record.NextCheckAt == nil {
		t.Error("a new reference was not scheduled")
	}
	if record.Status != domain.CheckPending {
		t.Errorf("status = %q, want pending", record.Status)
	}
}

// An unsupported reference is TRACKED but never scheduled. Dropping it would
// leave a silent gap in coverage; scheduling it would queue work that cannot
// succeed.
func TestAnUnsupportedReferenceIsTrackedButNotScheduled(t *testing.T) {
	db := openTestDB(t)

	seed := imageSeed("localhost:5000/app:1")
	seed.Supported = false
	seed.Detail = "the image reference cannot be looked up"

	record := syncOne(t, db, seed)

	if record.Status != domain.CheckUnsupported {
		t.Errorf("status = %q, want unsupported", record.Status)
	}
	if record.NextCheckAt != nil {
		t.Error("an unsupported reference was scheduled")
	}
	if record.StatusDetail == "" {
		t.Error("an unsupported reference carries no explanation")
	}

	// And the scheduler must not pick it up.
	due, err := db.ImageIntel.Due(context.Background(), time.Now().UTC().Add(time.Hour), 50, nil)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	for _, candidate := range due {
		if candidate.Reference == seed.Reference {
			t.Error("an unsupported reference came due")
		}
	}
}

// THE CENTRAL SYNC GUARANTEE. An inventory refresh knows nothing about the
// registry, so it must not overwrite what the registry established -- doing so
// would discard a real answer and re-queue work already done.
func TestSyncPreservesRegistryEstablishedState(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seed := imageSeed("docker.io/library/nginx:1.25")
	syncOne(t, db, seed)

	remote := "sha256:" + repeat("b", 64)
	later := time.Now().UTC().Add(time.Hour)
	if err := db.ImageIntel.RecordCheck(ctx, store.CheckOutcome{
		Reference:    seed.Reference,
		Status:       domain.CheckOK,
		RemoteDigest: remote,
		Update:       domain.UpdateMinor,
		LatestTag:    "1.26",
		UpdateReason: "a newer tag is published",
		ETag:         `"v1"`,
		NextCheckAt:  later,
	}, time.Now().UTC()); err != nil {
		t.Fatalf("RecordCheck: %v", err)
	}

	// A second inventory refresh, with a moved local digest.
	seed.LocalDigest = "sha256:" + repeat("c", 64)
	after := syncOne(t, db, seed)

	// What the daemon knows IS refreshed.
	if after.LocalDigest != seed.LocalDigest {
		t.Errorf("local digest = %q, want the refreshed one", after.LocalDigest)
	}
	// What the registry established is NOT.
	if after.RemoteDigest != remote {
		t.Errorf("remote digest = %q, want it preserved", after.RemoteDigest)
	}
	if after.Update != domain.UpdateMinor || after.LatestTag != "1.26" {
		t.Errorf("verdict = %q/%q, want it preserved", after.Update, after.LatestTag)
	}
	if after.Status != domain.CheckOK {
		t.Errorf("status = %q, want it preserved", after.Status)
	}
	if after.ETag != `"v1"` {
		t.Errorf("etag = %q, want it preserved", after.ETag)
	}
	if after.NextCheckAt == nil || !after.NextCheckAt.Equal(later.Truncate(time.Nanosecond)) {
		t.Errorf("schedule = %v, want the registry's %v", after.NextCheckAt, later)
	}
}

// A reference that becomes supported again is re-queued, and one that becomes
// unsupported stops being queued.
func TestSyncMovesAReferenceBetweenSupportedAndNot(t *testing.T) {
	db := openTestDB(t)
	seed := imageSeed("docker.io/library/nginx:1.25")
	seed.Supported = false
	seed.Detail = "unsupported"
	syncOne(t, db, seed)

	seed.Supported = true
	seed.Detail = ""
	back := syncOne(t, db, seed)

	if back.Status != domain.CheckPending {
		t.Errorf("status = %q, want pending after becoming supported", back.Status)
	}
	if back.NextCheckAt == nil {
		t.Error("a newly supported reference was not re-queued")
	}
	if back.StatusDetail != "" {
		t.Errorf("detail = %q, want it cleared", back.StatusDetail)
	}
}

// THE OTHER CENTRAL GUARANTEE. A failed lookup must not blank a good answer:
// "we could not ask" and "no update is available" are different claims.
func TestAFailedCheckPreservesThePreviousAnswer(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seed := imageSeed("docker.io/library/nginx:1.25")
	syncOne(t, db, seed)

	remote := "sha256:" + repeat("b", 64)
	if err := db.ImageIntel.RecordCheck(ctx, store.CheckOutcome{
		Reference:    seed.Reference,
		Status:       domain.CheckOK,
		RemoteDigest: remote,
		Update:       domain.UpdateMajor,
		LatestTag:    "2.0",
		NextCheckAt:  time.Now().UTC().Add(time.Hour),
	}, time.Now().UTC()); err != nil {
		t.Fatalf("RecordCheck: %v", err)
	}

	if err := db.ImageIntel.RecordCheck(ctx, store.CheckOutcome{
		Reference:    seed.Reference,
		Status:       domain.CheckFailed,
		Detail:       "the registry did not respond in time",
		FailureCount: 3,
		NextCheckAt:  time.Now().UTC().Add(2 * time.Hour),
	}, time.Now().UTC()); err != nil {
		t.Fatalf("RecordCheck(failure): %v", err)
	}

	after, err := db.ImageIntel.Get(ctx, seed.Reference)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if after.Status != domain.CheckFailed || after.FailureCount != 3 {
		t.Errorf("failure was not recorded: %+v", after)
	}
	if after.RemoteDigest != remote {
		t.Errorf("remote digest = %q, want the previous answer preserved", after.RemoteDigest)
	}
	if after.Update != domain.UpdateMajor || after.LatestTag != "2.0" {
		t.Errorf("verdict = %q/%q, want the previous answer preserved",
			after.Update, after.LatestTag)
	}
	// The last SUCCESS timestamp must not move; the last ATTEMPT must.
	if after.LastSuccessAt == nil {
		t.Error("the previous success was forgotten")
	}
	if after.LastCheckedAt == nil {
		t.Error("the failed attempt was not recorded")
	}
}

// History records CHANGES, not checks. A pass that found everything unchanged
// writes nothing, which is what keeps the table readable and bounds its growth.
func TestHistoryRecordsOnlyWhatTheServiceSupplies(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seed := imageSeed("docker.io/library/nginx:1.25")
	syncOne(t, db, seed)

	for round := 0; round < 5; round++ {
		outcome := store.CheckOutcome{
			Reference:    seed.Reference,
			Status:       domain.CheckOK,
			RemoteDigest: "sha256:" + repeat("b", 64),
			Update:       domain.UpdateNone,
			NextCheckAt:  time.Now().UTC().Add(time.Hour),
		}
		// Only the first pass carries an event, exactly as the service would
		// compute it.
		if round == 0 {
			outcome.Events = []domain.ImageUpdateEvent{{
				Reference:     seed.Reference,
				Kind:          domain.ImageEventDiscovered,
				CurrentDigest: outcome.RemoteDigest,
				Status:        domain.CheckOK,
				Detail:        "resolved for the first time",
			}}
		}
		if err := db.ImageIntel.RecordCheck(ctx, outcome, time.Now().UTC()); err != nil {
			t.Fatalf("RecordCheck: %v", err)
		}
	}

	events, total, err := db.ImageIntel.History(ctx, seed.Reference, store.Page{Limit: 50})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if total != 1 || len(events) != 1 {
		t.Fatalf("history = %d rows after five passes, want 1", total)
	}
	if events[0].Kind != domain.ImageEventDiscovered {
		t.Errorf("kind = %q", events[0].Kind)
	}
	if events[0].ObservedAt.IsZero() {
		t.Error("the event carries no timestamp")
	}
}

// The scheduler's question. Due must respect the schedule, the batch bound, and
// the per-host backoff.
func TestDueRespectsScheduleBoundsAndHostBackoff(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seeds := make([]store.ImageReferenceSeed, 0, 6)
	for index := 0; index < 6; index++ {
		seed := imageSeed(fmt.Sprintf("docker.io/library/app%d:1", index))
		if index >= 3 {
			seed.Registry = "ghcr.io"
			seed.Kind = domain.RegistryGHCR
		}
		seeds = append(seeds, seed)
	}
	if _, err := db.ImageIntel.SyncReferences(ctx, seeds, now); err != nil {
		t.Fatalf("SyncReferences: %v", err)
	}

	// Everything is due immediately, and the batch bound applies.
	due, err := db.ImageIntel.Due(ctx, now.Add(time.Second), 4, nil)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 4 {
		t.Errorf("due = %d, want the batch bound of 4", len(due))
	}

	// A host that is backing off is excluded IN SQL, so a rate-limited registry
	// does not fill the batch with work that would be skipped.
	due, err = db.ImageIntel.Due(ctx, now.Add(time.Second), 50, []string{"docker.io"})
	if err != nil {
		t.Fatalf("Due(excluding): %v", err)
	}
	if len(due) != 3 {
		t.Errorf("due = %d, want only the three ghcr references", len(due))
	}
	for _, record := range due {
		if record.Registry == "docker.io" {
			t.Error("a backing-off host's reference came due")
		}
	}

	// A reference scheduled into the future is not due.
	if err := db.ImageIntel.RecordCheck(ctx, store.CheckOutcome{
		Reference:   seeds[0].Reference,
		Status:      domain.CheckOK,
		NextCheckAt: now.Add(6 * time.Hour),
	}, now); err != nil {
		t.Fatalf("RecordCheck: %v", err)
	}

	count, err := db.ImageIntel.CountDue(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatalf("CountDue: %v", err)
	}
	if count != 5 {
		t.Errorf("due count = %d, want 5", count)
	}
}

// Registry health is per HOST, so one registry's outage backs off every
// reference it serves rather than each discovering it alone.
func TestRegistryHostHealth(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	syncOne(t, db, imageSeed("docker.io/library/nginx:1.25"))

	if err := db.ImageIntel.RecordHostOutcome(ctx, store.HostOutcome{
		Host:                "docker.io",
		Kind:                domain.RegistryDockerHub,
		Success:             false,
		Detail:              "the registry is rate-limiting requests",
		RateLimited:         true,
		AvailableAt:         now.Add(time.Hour),
		ConsecutiveFailures: 2,
	}, now); err != nil {
		t.Fatalf("RecordHostOutcome: %v", err)
	}

	health, err := db.ImageIntel.RegistryHealth(ctx)
	if err != nil {
		t.Fatalf("RegistryHealth: %v", err)
	}
	if len(health) != 1 {
		t.Fatalf("health = %d rows, want 1", len(health))
	}
	entry := health[0]
	if entry.Host != "docker.io" || !entry.RateLimited || entry.ConsecutiveFailures != 2 {
		t.Errorf("health = %+v", entry)
	}
	if entry.Healthy() {
		t.Error("a failing host reports as healthy")
	}
	if entry.Images != 1 {
		t.Errorf("images = %d, want the reference it serves", entry.Images)
	}

	unavailable, err := db.ImageIntel.UnavailableHosts(ctx, now)
	if err != nil {
		t.Fatalf("UnavailableHosts: %v", err)
	}
	if len(unavailable) != 1 || unavailable[0] != "docker.io" {
		t.Errorf("unavailable = %v", unavailable)
	}

	failures, err := db.ImageIntel.HostFailureCount(ctx, "docker.io")
	if err != nil {
		t.Fatalf("HostFailureCount: %v", err)
	}
	if failures != 2 {
		t.Errorf("failures = %d, want 2", failures)
	}

	// A success clears the hold.
	if err := db.ImageIntel.RecordHostOutcome(ctx, store.HostOutcome{
		Host: "docker.io", Kind: domain.RegistryDockerHub, Success: true,
	}, now); err != nil {
		t.Fatalf("RecordHostOutcome(success): %v", err)
	}
	unavailable, err = db.ImageIntel.UnavailableHosts(ctx, now)
	if err != nil {
		t.Fatalf("UnavailableHosts: %v", err)
	}
	if len(unavailable) != 0 {
		t.Errorf("unavailable = %v after a success", unavailable)
	}

	// An unknown host has no failures rather than an error.
	if count, err := db.ImageIntel.HostFailureCount(ctx, "never.seen"); err != nil || count != 0 {
		t.Errorf("HostFailureCount(unknown) = %d, %v", count, err)
	}
}

func TestImageSortAllowlistIsClosed(t *testing.T) {
	for _, field := range []string{
		"reference; DROP TABLE image_intel", "update_type", "", "etag", "1",
		"i.reference",
	} {
		if store.ValidImageIntelSortField(field) {
			t.Errorf("sort field %q is accepted", field)
		}
	}
	for _, field := range []string{
		"reference", "registry", "update", "status", "lastChecked", "containers", "id",
	} {
		if !store.ValidImageIntelSortField(field) {
			t.Errorf("sort field %q is rejected", field)
		}
	}
}

// Filter VALUES travel as bound parameters, so SQL in one is matched literally
// rather than executed. The surviving tables are the assertion.
func TestImageFilterValuesCannotCarrySQL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	syncOne(t, db, imageSeed("docker.io/library/nginx:1.25"))

	for _, injection := range []string{
		"docker.io'; DROP TABLE image_intel; --",
		"' OR '1'='1",
		"%",
		"_",
	} {
		if _, _, err := db.ImageIntel.List(ctx, store.ImageIntelFilter{
			Registries: []string{injection},
			Search:     injection,
		}); err != nil {
			t.Fatalf("List(%q): %v", injection, err)
		}
	}

	if _, total, err := db.ImageIntel.List(ctx, store.ImageIntelFilter{}); err != nil || total != 1 {
		t.Fatalf("after injection attempts: total=%d err=%v", total, err)
	}
}

// A LIKE search term must widen only its own match, not every row.
func TestImageSearchEscapesLikeMetacharacters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	syncOne(t, db, imageSeed("docker.io/library/nginx:1.25"))
	syncOne(t, db, imageSeed("docker.io/library/redis:7"))

	if _, total, err := db.ImageIntel.List(ctx, store.ImageIntelFilter{Search: "%"}); err != nil || total != 0 {
		t.Errorf("a bare %% matched %d rows (err %v); the metacharacter was not escaped", total, err)
	}
	if _, total, err := db.ImageIntel.List(ctx, store.ImageIntelFilter{Search: "nginx"}); err != nil || total != 1 {
		t.Errorf("search returned %d rows (err %v)", total, err)
	}
}

// Resolved history is prunable; the current state is not, because it is one row
// per reference and is bounded by the inventory.
func TestPruningHistoryAndOrphans(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seed := imageSeed("docker.io/library/nginx:1.25")
	syncOne(t, db, seed)

	old := time.Now().UTC().Add(-48 * time.Hour)
	if err := db.ImageIntel.RecordCheck(ctx, store.CheckOutcome{
		Reference:   seed.Reference,
		Status:      domain.CheckOK,
		NextCheckAt: time.Now().UTC().Add(time.Hour),
		Events: []domain.ImageUpdateEvent{{
			Reference:  seed.Reference,
			ObservedAt: old,
			Kind:       domain.ImageEventDiscovered,
			Status:     domain.CheckOK,
		}},
	}, time.Now().UTC()); err != nil {
		t.Fatalf("RecordCheck: %v", err)
	}

	removed, err := db.ImageIntel.PruneHistory(ctx, time.Now().UTC().Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("PruneHistory: %v", err)
	}
	if removed != 1 {
		t.Errorf("pruned %d history rows, want 1", removed)
	}
	// The reference itself survives: pruning history is not forgetting the
	// image.
	if _, err := db.ImageIntel.Get(ctx, seed.Reference); err != nil {
		t.Errorf("the reference was removed with its history: %v", err)
	}

	// No present container declares this reference, so it is an orphan.
	orphans, err := db.ImageIntel.PruneOrphans(ctx, 100)
	if err != nil {
		t.Fatalf("PruneOrphans: %v", err)
	}
	if orphans != 1 {
		t.Errorf("pruned %d orphans, want 1", orphans)
	}
	if _, err := db.ImageIntel.Get(ctx, seed.Reference); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the orphan survived: %v", err)
	}
}

// The summary must stay cheap on an estate large enough for it to matter, and
// must report COVERAGE beside the update counts -- an update count without "how
// many were actually checked" invites reading a stale estate as a healthy one.
func TestImageSummaryAtScale(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	const references = 2000
	seeds := make([]store.ImageReferenceSeed, 0, references)
	for index := 0; index < references; index++ {
		seed := imageSeed(fmt.Sprintf("docker.io/library/app%04d:1.0", index))
		if index%3 == 0 {
			seed.Registry = "ghcr.io"
			seed.Kind = domain.RegistryGHCR
		}
		seeds = append(seeds, seed)
	}
	if _, err := db.ImageIntel.SyncReferences(ctx, seeds, now); err != nil {
		t.Fatalf("SyncReferences: %v", err)
	}

	// A quarter of them have been checked and carry an update.
	for index := 0; index < references/4; index++ {
		if err := db.ImageIntel.RecordCheck(ctx, store.CheckOutcome{
			Reference:    seeds[index].Reference,
			Status:       domain.CheckOK,
			RemoteDigest: "sha256:" + repeat("b", 64),
			Update:       domain.UpdateMinor,
			LatestTag:    "1.1",
			NextCheckAt:  now.Add(6 * time.Hour),
		}, now); err != nil {
			t.Fatalf("RecordCheck: %v", err)
		}
	}

	started := time.Now()
	summary, err := db.ImageIntel.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	elapsed := time.Since(started)

	if summary.Images != references {
		t.Errorf("images = %d, want %d", summary.Images, references)
	}
	if summary.UpdatesAvailable != references/4 {
		t.Errorf("updates = %d, want %d", summary.UpdatesAvailable, references/4)
	}
	if summary.Checked != references/4 {
		t.Errorf("checked = %d, want %d", summary.Checked, references/4)
	}
	// The number that stops a stale estate reading as a healthy one.
	if summary.Pending != references-references/4 {
		t.Errorf("pending = %d, want %d", summary.Pending, references-references/4)
	}
	if coverage := summary.Coverage(); coverage < 0.24 || coverage > 0.26 {
		t.Errorf("coverage = %v, want about 0.25", coverage)
	}
	if summary.ByRegistry["docker.io"] == 0 || summary.ByRegistry["ghcr.io"] == 0 {
		t.Errorf("byRegistry = %v", summary.ByRegistry)
	}

	// Deliberately loose: this fails on an implementation that scans, not on a
	// slow machine.
	if elapsed > 3*time.Second {
		t.Errorf("the summary took %s over %d references", elapsed, references)
	}
	t.Logf("summary over %d references took %s", references, elapsed)

	// The list must be paged and index-backed at the same scale.
	started = time.Now()
	records, total, err := db.ImageIntel.List(ctx, store.ImageIntelFilter{
		UpdatesOnly: true,
		Sort:        "update",
		Page:        store.Page{Limit: 25},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != references/4 || len(records) != 25 {
		t.Errorf("list = %d of %d", len(records), total)
	}
	t.Logf("filtered page of 25 from %d took %s", total, time.Since(started))
}

// Concurrent syncs of the same reference must collapse to one row. The unique
// constraint is the guarantee; this proves the upsert honours it under
// contention rather than failing.
func TestConcurrentSyncsCollapseToOneRow(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seed := imageSeed("docker.io/library/nginx:1.25")

	errs := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		go func() {
			_, err := db.ImageIntel.SyncReferences(ctx,
				[]store.ImageReferenceSeed{seed}, time.Now().UTC())
			errs <- err
		}()
	}
	for worker := 0; worker < 8; worker++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent SyncReferences: %v", err)
		}
	}

	_, total, err := db.ImageIntel.List(ctx, store.ImageIntelFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Errorf("rows = %d after eight concurrent syncs, want 1", total)
	}
}
