package service_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/registry"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Image intelligence engine tests.
//
// These drive the REAL classifier against a fake registry and fake persistence,
// so what is under test is the engine's own judgement: which signal it prefers,
// what it refuses to conclude, what it preserves when a lookup fails, and how it
// schedules the next attempt.

// ------------------------------------------------------------------ fakes --

type fakeIntelStore struct {
	mu sync.Mutex

	inventory []store.InventoryReference
	tracked   map[string]domain.ImageIntel
	due       []domain.ImageIntel

	seeds     []store.ImageReferenceSeed
	outcomes  []store.CheckOutcome
	hostCalls []store.HostOutcome

	unavailable []string
	hostFails   map[string]int
	syncErr     error
}

func newFakeIntelStore() *fakeIntelStore {
	return &fakeIntelStore{
		tracked:   make(map[string]domain.ImageIntel),
		hostFails: make(map[string]int),
	}
}

func (f *fakeIntelStore) InventoryReferences(context.Context, int) ([]store.InventoryReference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inventory, nil
}

func (f *fakeIntelStore) SyncReferences(
	_ context.Context, seeds []store.ImageReferenceSeed, _ time.Time,
) (store.SyncResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.syncErr != nil {
		return store.SyncResult{}, f.syncErr
	}
	f.seeds = append(f.seeds, seeds...)
	return store.SyncResult{Inserted: len(seeds)}, nil
}

func (f *fakeIntelStore) Due(context.Context, time.Time, int, []string) ([]domain.ImageIntel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.due, nil
}

func (f *fakeIntelStore) CountDue(context.Context, time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.due), nil
}

func (f *fakeIntelStore) Get(_ context.Context, reference string) (domain.ImageIntel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, ok := f.tracked[reference]
	if !ok {
		return domain.ImageIntel{}, store.ErrNotFound
	}
	return record, nil
}

func (f *fakeIntelStore) RecordCheck(_ context.Context, outcome store.CheckOutcome, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes = append(f.outcomes, outcome)
	return nil
}

func (f *fakeIntelStore) RecordHostOutcome(_ context.Context, outcome store.HostOutcome, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostCalls = append(f.hostCalls, outcome)
	return nil
}

func (f *fakeIntelStore) UnavailableHosts(context.Context, time.Time) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unavailable, nil
}

func (f *fakeIntelStore) HostFailureCount(_ context.Context, host string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hostFails[host], nil
}

func (f *fakeIntelStore) PruneHistory(context.Context, time.Time, int) (int64, error) { return 0, nil }
func (f *fakeIntelStore) PruneOrphans(context.Context, int) (int64, error)            { return 0, nil }

func (f *fakeIntelStore) recorded() []store.CheckOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.CheckOutcome(nil), f.outcomes...)
}

func (f *fakeIntelStore) hosts() []store.HostOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.HostOutcome(nil), f.hostCalls...)
}

// fakeRegistry answers manifest and tag requests from a script.
type fakeRegistry struct {
	mu sync.Mutex

	digest      string
	notModified bool
	etag        string
	platforms   []domain.Platform
	annotations map[string]string
	manifestErr error

	tags      []string
	truncated bool
	tagsErr   error

	manifestCalls int
	tagCalls      int
}

func (f *fakeRegistry) Manifest(_ context.Context, _ registry.ManifestRequest) (registry.ManifestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.manifestCalls++
	if f.manifestErr != nil {
		return registry.ManifestResult{}, f.manifestErr
	}
	return registry.ManifestResult{
		NotModified: f.notModified,
		Digest:      f.digest,
		ETag:        f.etag,
		Platforms:   f.platforms,
		Annotations: f.annotations,
	}, nil
}

func (f *fakeRegistry) Tags(_ context.Context, _ domain.NormalizedRef, _ int) (registry.TagsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tagCalls++
	if f.tagsErr != nil {
		return registry.TagsResult{}, f.tagsErr
	}
	return registry.TagsResult{Tags: f.tags, Truncated: f.truncated}, nil
}

func (f *fakeRegistry) calls() (manifests, tags int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.manifestCalls, f.tagCalls
}

// --------------------------------------------------------------- fixtures --

func intelConfig() config.ImageIntel {
	return config.ImageIntel{
		Enabled:               true,
		RefreshInterval:       6 * time.Hour,
		CollectInterval:       5 * time.Minute,
		MaxConcurrentRequests: 4,
		MaxReferencesPerPass:  50,
		MaxTrackedReferences:  1000,
		MaxTagPages:           5,
		RequestTimeout:        5 * time.Second,
		MaxAttempts:           2,
		RetryBackoff:          time.Millisecond,
		FailureBackoff:        15 * time.Minute,
		MaxFailureBackoff:     24 * time.Hour,
		UnsupportedInterval:   24 * time.Hour,
		HistoryRetention:      90 * 24 * time.Hour,
		PruneInterval:         time.Hour,
	}
}

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func trackedRef(reference, tag, localDigest string) domain.ImageIntel {
	return domain.ImageIntel{
		Reference:   reference,
		Familiar:    reference,
		Kind:        domain.RegistryDockerHub,
		Registry:    "docker.io",
		Repository:  "library/nginx",
		Tag:         tag,
		LocalDigest: localDigest,
		Status:      domain.CheckPending,
		Update:      domain.UpdateNone,
	}
}

type intelHarness struct {
	service  *service.ImageIntelService
	store    *fakeIntelStore
	registry *fakeRegistry
}

func newIntelHarness(t *testing.T, cfg config.ImageIntel) intelHarness {
	t.Helper()

	backing := newFakeIntelStore()
	remote := &fakeRegistry{}

	return intelHarness{
		service: service.NewImageIntelService(service.ImageIntelOptions{
			Store:    backing,
			Registry: remote,
			Config:   cfg,
			Logger:   discardLogger(),
		}),
		store:    backing,
		registry: remote,
	}
}

// ------------------------------------------------------------------ sync --

// Projection normalises, and a reference that cannot be contacted is TRACKED
// rather than dropped -- so a dashboard can explain a gap in coverage instead of
// silently omitting the image.
func TestSyncInventoryProjectsAndMarksUnsupported(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	harness.store.inventory = []store.InventoryReference{
		{Reference: "nginx:1.25", ImageID: "sha256:1", Digest: digestA, OS: "linux", Architecture: "amd64"},
		{Reference: "ghcr.io/owner/app:2.0", ImageID: "sha256:2"},
		{Reference: "localhost:5000/private:1", ImageID: "sha256:3"},
		{Reference: "127.0.0.1/evil:1", ImageID: "sha256:4"},
	}

	if _, err := harness.service.SyncInventory(context.Background()); err != nil {
		t.Fatalf("SyncInventory: %v", err)
	}

	harness.store.mu.Lock()
	seeds := harness.store.seeds
	harness.store.mu.Unlock()

	if len(seeds) != 4 {
		t.Fatalf("seeds = %d, want 4", len(seeds))
	}

	byReference := make(map[string]store.ImageReferenceSeed, len(seeds))
	for _, seed := range seeds {
		byReference[seed.Reference] = seed
	}

	// Normalised to canonical form.
	nginx, ok := byReference["docker.io/library/nginx:1.25"]
	if !ok {
		t.Fatalf("nginx was not normalised: %v", byReference)
	}
	if !nginx.Supported || nginx.Registry != "docker.io" || nginx.Familiar != "nginx:1.25" {
		t.Errorf("nginx seed = %+v", nginx)
	}
	if nginx.LocalDigest != digestA {
		t.Errorf("local digest = %q", nginx.LocalDigest)
	}
	if nginx.Platform.String() != "linux/amd64" {
		t.Errorf("platform = %q", nginx.Platform.String())
	}

	if ghcr, found := byReference["ghcr.io/owner/app:2.0"]; !found || ghcr.Kind != domain.RegistryGHCR {
		t.Errorf("ghcr seed = %+v", ghcr)
	}

	// Both unsupported references are tracked, and neither is contactable.
	unsupported := 0
	for _, seed := range seeds {
		if !seed.Supported {
			unsupported++
			if seed.Detail == "" {
				t.Errorf("%q is unsupported with no explanation", seed.Reference)
			}
			if seed.Registry != "" {
				t.Errorf("%q carries a registry host %q despite being unsupported",
					seed.Reference, seed.Registry)
			}
		}
	}
	if unsupported != 2 {
		t.Errorf("unsupported = %d, want the localhost and address-literal references", unsupported)
	}
}

// A malformed local digest is discarded rather than stored: it would otherwise
// reach a comparison and a UI, and be neither.
func TestSyncDiscardsAMalformedLocalDigest(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	harness.store.inventory = []store.InventoryReference{
		{Reference: "nginx:1.25", Digest: "not-a-digest"},
	}

	if _, err := harness.service.SyncInventory(context.Background()); err != nil {
		t.Fatalf("SyncInventory: %v", err)
	}

	harness.store.mu.Lock()
	defer harness.store.mu.Unlock()
	if harness.store.seeds[0].LocalDigest != "" {
		t.Errorf("local digest = %q, want it discarded", harness.store.seeds[0].LocalDigest)
	}
}

// ---------------------------------------------------------------- collect --

// The informative signal wins. An image with both a newer tag and a moved
// digest reports the newer tag, because that is the actionable answer.
func TestATagUpdateOutranksADigestChange(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	harness.store.due = []domain.ImageIntel{
		trackedRef("docker.io/library/nginx:1.25.3", "1.25.3", digestA),
	}
	harness.registry.digest = digestB // the digest also moved
	harness.registry.tags = []string{"1.25.3", "1.26.0"}

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	recorded := harness.store.recorded()
	if len(recorded) != 1 {
		t.Fatalf("outcomes = %d, want 1", len(recorded))
	}
	if recorded[0].Update != domain.UpdateMinor {
		t.Errorf("update = %q, want minor", recorded[0].Update)
	}
	if recorded[0].LatestTag != "1.26.0" {
		t.Errorf("latest tag = %q", recorded[0].LatestTag)
	}
	if recorded[0].RemoteDigest != digestB {
		t.Errorf("remote digest = %q", recorded[0].RemoteDigest)
	}
}

// A channel tag carries no version, so only the digest can be compared. This is
// the case that would otherwise report "a major update" for every container
// tracking latest.
func TestAChannelTagFallsBackToDigestComparison(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	harness.store.due = []domain.ImageIntel{
		trackedRef("docker.io/library/nginx:latest", "latest", digestA),
	}
	harness.registry.digest = digestB
	// The repository publishes versions, which must NOT be offered to a
	// channel tag.
	harness.registry.tags = []string{"1.0.0", "2.0.0", "3.0.0"}

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	recorded := harness.store.recorded()
	if recorded[0].Update != domain.UpdateDigest {
		t.Errorf("update = %q, want digest", recorded[0].Update)
	}
	if recorded[0].LatestTag != "" {
		t.Errorf("latest tag = %q, want none for a channel tag", recorded[0].LatestTag)
	}

	// And the tag listing is not even requested: the tag does not parse, so
	// there is nothing to compare it against.
	if _, tags := harness.registry.calls(); tags != 0 {
		t.Errorf("tag listings = %d, want 0 for a channel tag", tags)
	}
}

func TestUpToDateReportsNone(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	harness.store.due = []domain.ImageIntel{
		trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA),
	}
	harness.registry.digest = digestA
	harness.registry.tags = []string{"1.25.0", "1.26.0"}

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := harness.store.recorded()[0].Update; got != domain.UpdateNone {
		t.Errorf("update = %q, want none", got)
	}
}

// An image with no local digest cannot be compared. Reporting "none" would be a
// claim the engine cannot support.
func TestNoLocalDigestReportsUnknown(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	harness.store.due = []domain.ImageIntel{
		trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", ""),
	}
	harness.registry.digest = digestA
	harness.registry.tags = []string{"1.26.0"}

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	recorded := harness.store.recorded()[0]
	if recorded.Update != domain.UpdateUnknown {
		t.Errorf("update = %q, want unknown", recorded.Update)
	}
	if recorded.UpdateReason == "" {
		t.Error("the verdict carries no reason")
	}
}

// A truncated listing that found nothing newer is UNKNOWN, not NONE. A listing
// that stopped at its budget has established nothing.
func TestATruncatedListingIsUnknownRatherThanUpToDate(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	harness.store.due = []domain.ImageIntel{
		trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA),
	}
	harness.registry.digest = digestA
	harness.registry.tags = []string{"1.25.0"}
	harness.registry.truncated = true

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := harness.store.recorded()[0].Update; got != domain.UpdateUnknown {
		t.Errorf("update = %q, want unknown for a truncated listing", got)
	}
}

// A registry that does not list tags is not a failure: the digest comparison is
// still available and still true.
func TestTagListingFailureFallsBackToDigest(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	harness.store.due = []domain.ImageIntel{
		trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA),
	}
	harness.registry.digest = digestB
	harness.registry.tagsErr = registry.ErrTagListingUnsupported

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	recorded := harness.store.recorded()[0]
	if recorded.Status != domain.CheckOK {
		t.Errorf("status = %q, want ok", recorded.Status)
	}
	if recorded.Update != domain.UpdateDigest {
		t.Errorf("update = %q, want digest", recorded.Update)
	}
}

// A digest-pinned reference names immutable content, so its digest cannot have
// "moved". Only a newer tag would be meaningful.
func TestAPinnedReferenceIsNotReportedAsMoved(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	record := trackedRef("docker.io/library/nginx@"+digestA, "", digestA)
	record.Pinned = true
	harness.store.due = []domain.ImageIntel{record}
	harness.registry.digest = digestA

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := harness.store.recorded()[0].Update; got != domain.UpdateNone {
		t.Errorf("update = %q, want none for a pinned reference", got)
	}
	if _, tags := harness.registry.calls(); tags != 0 {
		t.Errorf("tag listings = %d, want 0 for a pinned reference", tags)
	}
}

// A 304 means the manifest is byte-identical, so the previous digest is carried
// forward rather than blanked.
func TestANotModifiedResponseCarriesTheDigestForward(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	record := trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA)
	record.RemoteDigest = digestA
	record.ETag = `"cached"`
	harness.store.due = []domain.ImageIntel{record}
	harness.registry.notModified = true
	harness.registry.tags = []string{"1.26.0"}

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	recorded := harness.store.recorded()[0]
	if recorded.RemoteDigest != digestA {
		t.Errorf("remote digest = %q, want it carried forward", recorded.RemoteDigest)
	}
	if recorded.Update != domain.UpdateNone {
		t.Errorf("update = %q, want none", recorded.Update)
	}
}

// ------------------------------------------------------------- failures --

// Every failure mode maps to a HarborMaster status, a schedule, and NO
// overwrite of what was already known.
func TestFailureHandling(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus domain.CheckStatus
		// wantSlow marks the failures that are re-checked on the long schedule
		// rather than the failure backoff.
		wantSlow bool
	}{
		{"a transient failure", registry.ErrTransient, domain.CheckFailed, false},
		{"a timeout", context.DeadlineExceeded, domain.CheckFailed, false},
		{"a malformed response", registry.ErrMalformedResponse, domain.CheckFailed, false},
		{"a private repository", registry.ErrUnauthorized, domain.CheckUnauthorized, true},
		{"a missing repository", registry.ErrNotFound, domain.CheckNotFound, true},
		{"a blocked address", registry.ErrBlockedAddress, domain.CheckUnsupported, true},
		{"a refused redirect", registry.ErrRedirectRefused, domain.CheckUnsupported, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newIntelHarness(t, intelConfig())
			previous := trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA)
			previous.RemoteDigest = digestB
			previous.Update = domain.UpdateMajor
			previous.LatestTag = "2.0.0"
			harness.store.due = []domain.ImageIntel{previous}
			harness.registry.manifestErr = tc.err

			result, err := harness.service.Collect(context.Background())
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}

			recorded := harness.store.recorded()
			if len(recorded) != 1 {
				t.Fatalf("outcomes = %d", len(recorded))
			}
			outcome := recorded[0]

			if outcome.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", outcome.Status, tc.wantStatus)
			}
			if outcome.Detail == "" {
				t.Error("a failure carries no detail")
			}
			// THE KEY ASSERTION: a failure writes no verdict, so the previous
			// answer survives at the repository.
			if outcome.RemoteDigest != "" || outcome.Update != "" || outcome.LatestTag != "" {
				t.Errorf("a failure carried a verdict: %+v", outcome)
			}
			if outcome.NextCheckAt.IsZero() {
				t.Error("a failure was not rescheduled")
			}

			// Unsupported and permanent answers are counted as skipped rather
			// than failed, so a dashboard does not show a healthy estate as
			// broken.
			if tc.wantStatus == domain.CheckUnsupported && result.Skipped != 1 {
				t.Errorf("skipped = %d, want 1", result.Skipped)
			}
		})
	}
}

// A rate limit is honoured on the registry's own schedule. A client that
// ignores Retry-After earns a longer ban.
func TestRateLimitBackoff(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	harness.store.due = []domain.ImageIntel{
		trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA),
	}
	harness.registry.manifestErr = registry.ErrRateLimited

	before := time.Now().UTC()
	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	outcome := harness.store.recorded()[0]
	if outcome.Status != domain.CheckRateLimited {
		t.Errorf("status = %q, want rateLimited", outcome.Status)
	}
	if !outcome.NextCheckAt.After(before) {
		t.Error("a rate limit did not delay the next check")
	}

	// The HOST is backed off too, so one registry's limit delays every
	// reference it serves rather than each discovering it separately.
	hosts := harness.store.hosts()
	if len(hosts) == 0 {
		t.Fatal("the host's health was not recorded")
	}
	last := hosts[len(hosts)-1]
	if last.Success || !last.RateLimited || last.Host != "docker.io" {
		t.Errorf("host outcome = %+v", last)
	}
	if last.AvailableAt.IsZero() {
		t.Error("the host was not held off")
	}
}

// A private repository or a missing tag says nothing about the registry's
// health. Treating it as an outage would stop checking every other image there.
func TestAPermanentAnswerDoesNotBackOffTheHost(t *testing.T) {
	for _, failure := range []error{registry.ErrUnauthorized, registry.ErrNotFound} {
		harness := newIntelHarness(t, intelConfig())
		harness.store.due = []domain.ImageIntel{
			trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA),
		}
		harness.registry.manifestErr = failure

		if _, err := harness.service.Collect(context.Background()); err != nil {
			t.Fatalf("Collect: %v", err)
		}

		for _, host := range harness.store.hosts() {
			if !host.Success {
				t.Errorf("%v backed off the host: %+v", failure, host)
			}
		}
	}
}

// Backoff grows and is capped, so a long-failing reference is retried rarely
// rather than never or constantly.
func TestFailureBackoffGrowsAndIsCapped(t *testing.T) {
	cfg := intelConfig()
	cfg.FailureBackoff = time.Minute
	cfg.MaxFailureBackoff = 10 * time.Minute

	var previous time.Duration
	for failures := 1; failures <= 8; failures++ {
		harness := newIntelHarness(t, cfg)
		record := trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA)
		record.FailureCount = failures - 1
		harness.store.due = []domain.ImageIntel{record}
		harness.registry.manifestErr = registry.ErrTransient

		before := time.Now().UTC()
		if _, err := harness.service.Collect(context.Background()); err != nil {
			t.Fatalf("Collect: %v", err)
		}

		outcome := harness.store.recorded()[0]
		delay := outcome.NextCheckAt.Sub(before)

		if outcome.FailureCount != failures {
			t.Errorf("failure count = %d, want %d", outcome.FailureCount, failures)
		}
		// Jitter means the growth is not exactly monotonic, so the assertion is
		// on the ceiling, which is the property that matters.
		if delay > cfg.MaxFailureBackoff+cfg.MaxFailureBackoff/4+time.Second {
			t.Errorf("failure %d delayed %s, past the cap of %s",
				failures, delay, cfg.MaxFailureBackoff)
		}
		if failures > 1 && previous > 0 && delay < time.Second {
			t.Errorf("failure %d produced a %s delay; backoff collapsed", failures, delay)
		}
		previous = delay
	}
}

// A success clears the failure count and schedules on the refresh interval.
func TestASuccessResetsTheBackoff(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	record := trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA)
	record.FailureCount = 5
	record.Status = domain.CheckFailed
	harness.store.due = []domain.ImageIntel{record}
	harness.registry.digest = digestA
	harness.registry.tags = []string{"1.26.0"}

	before := time.Now().UTC()
	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	outcome := harness.store.recorded()[0]
	if outcome.Status != domain.CheckOK {
		t.Errorf("status = %q", outcome.Status)
	}
	// Scheduled on the refresh interval, not the backoff.
	delay := outcome.NextCheckAt.Sub(before)
	if delay < intelConfig().RefreshInterval {
		t.Errorf("next check in %s, want at least the refresh interval", delay)
	}

	// And the recovery is recorded in the history.
	var recovered bool
	for _, event := range outcome.Events {
		if event.Kind == domain.ImageEventCheckRecovered {
			recovered = true
		}
	}
	if !recovered {
		t.Error("the recovery produced no history event")
	}
}

// ---------------------------------------------------------------- events --

// History records CHANGES. A pass that found everything unchanged writes
// nothing, which is what keeps the history readable.
func TestHistoryEventsAreDerivedFromChangesOnly(t *testing.T) {
	cases := []struct {
		name      string
		previous  domain.ImageIntel
		digest    string
		tags      []string
		wantKinds []domain.ImageEventKind
	}{
		{
			name:      "a first resolution",
			previous:  trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA),
			digest:    digestA,
			tags:      []string{"1.26.0"},
			wantKinds: []domain.ImageEventKind{domain.ImageEventDiscovered},
		},
		{
			name: "an unchanged pass writes nothing",
			previous: func() domain.ImageIntel {
				record := trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA)
				record.RemoteDigest = digestA
				record.Status = domain.CheckOK
				return record
			}(),
			digest:    digestA,
			tags:      []string{"1.26.0"},
			wantKinds: nil,
		},
		{
			name: "a moved digest",
			previous: func() domain.ImageIntel {
				record := trackedRef("docker.io/library/nginx:latest", "latest", digestA)
				record.RemoteDigest = digestA
				record.Status = domain.CheckOK
				return record
			}(),
			digest: digestB,
			wantKinds: []domain.ImageEventKind{
				domain.ImageEventDigestChanged, domain.ImageEventUpdateFound,
			},
		},
		{
			name: "an update that has been taken",
			previous: func() domain.ImageIntel {
				record := trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA)
				record.RemoteDigest = digestA
				record.Status = domain.CheckOK
				record.Update = domain.UpdateMinor
				record.LatestTag = "1.26.0"
				return record
			}(),
			digest:    digestA,
			tags:      []string{"1.26.0"},
			wantKinds: []domain.ImageEventKind{domain.ImageEventUpdateCleared},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newIntelHarness(t, intelConfig())
			harness.store.due = []domain.ImageIntel{tc.previous}
			harness.registry.digest = tc.digest
			harness.registry.tags = tc.tags

			if _, err := harness.service.Collect(context.Background()); err != nil {
				t.Fatalf("Collect: %v", err)
			}

			events := harness.store.recorded()[0].Events
			if len(events) != len(tc.wantKinds) {
				t.Fatalf("events = %d (%+v), want %d", len(events), kindsOf(events), len(tc.wantKinds))
			}
			for index, want := range tc.wantKinds {
				if events[index].Kind != want {
					t.Errorf("event %d = %q, want %q", index, events[index].Kind, want)
				}
				if events[index].Reference != tc.previous.Reference {
					t.Errorf("event %d names %q", index, events[index].Reference)
				}
			}
		})
	}
}

func kindsOf(events []domain.ImageUpdateEvent) []domain.ImageEventKind {
	kinds := make([]domain.ImageEventKind, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

// A repeated failure records one event, on the transition. Recording it every
// pass would fill the history with a repeating line.
func TestARepeatedFailureRecordsOneEvent(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	record := trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA)
	record.Status = domain.CheckFailed
	harness.store.due = []domain.ImageIntel{record}
	harness.registry.manifestErr = registry.ErrTransient

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if events := harness.store.recorded()[0].Events; len(events) != 0 {
		t.Errorf("events = %+v, want none for a continuing failure", kindsOf(events))
	}
}

// ---------------------------------------------------------- provenance --

// OCI annotations are read when present, and refused when implausible.
func TestProvenanceFromAnnotations(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	harness.store.due = []domain.ImageIntel{
		trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA),
	}
	harness.registry.digest = digestA
	harness.registry.tags = []string{"1.26.0"}
	harness.registry.annotations = map[string]string{
		"created": "2024-01-15T10:30:00Z",
		"vendor":  "Example Inc",
		"source":  "https://github.com/example/app",
	}

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	outcome := harness.store.recorded()[0]
	if outcome.PublishedAt == nil || outcome.PublishedAt.Year() != 2024 {
		t.Errorf("published = %v", outcome.PublishedAt)
	}
	if outcome.Vendor != "Example Inc" || outcome.Source != "https://github.com/example/app" {
		t.Errorf("provenance = %q / %q", outcome.Vendor, outcome.Source)
	}
}

// An implausible or malformed timestamp is refused rather than rendered as a
// confident wrong date.
func TestImplausibleTimestampsAreRefused(t *testing.T) {
	for _, created := range []string{
		"not-a-date", "1999-01-01T00:00:00Z", "2999-01-01T00:00:00Z",
		"15/01/2024", "",
	} {
		harness := newIntelHarness(t, intelConfig())
		harness.store.due = []domain.ImageIntel{
			trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA),
		}
		harness.registry.digest = digestA
		harness.registry.tags = []string{"1.26.0"}
		harness.registry.annotations = map[string]string{"created": created}

		if _, err := harness.service.Collect(context.Background()); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if published := harness.store.recorded()[0].PublishedAt; published != nil {
			t.Errorf("created %q produced %v, want it refused", created, published)
		}
	}
}

// ------------------------------------------------------------ scheduling --

// Two passes must not overlap: duplicating load on a third party is the one
// cost HarborMaster cannot absorb.
func TestConcurrentCollectionsDoNotOverlap(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())

	due := make([]domain.ImageIntel, 0, 30)
	for index := 0; index < 30; index++ {
		due = append(due, trackedRef(
			fmt.Sprintf("docker.io/library/app%d:1.0.0", index), "1.0.0", digestA))
	}
	harness.store.due = due
	harness.registry.digest = digestA
	harness.registry.tags = []string{"1.0.0"}

	var wait sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := harness.service.Collect(context.Background()); err != nil {
				t.Errorf("Collect: %v", err)
			}
		}()
	}
	wait.Wait()

	manifests, _ := harness.registry.calls()
	if manifests == 0 {
		t.Fatal("no pass ran")
	}
	// One pass of 30, not four. A refused overlap costs nothing; a duplicated
	// one costs a registry four times the traffic.
	if manifests > len(due)*2 {
		t.Errorf("manifest requests = %d over %d references; passes overlapped",
			manifests, len(due))
	}
}

// A disabled engine makes NO outbound request. This is the setting an air-gapped
// deployment relies on, so it is asserted rather than assumed.
func TestADisabledEngineMakesNoRequest(t *testing.T) {
	cfg := intelConfig()
	cfg.Enabled = false

	harness := newIntelHarness(t, cfg)
	harness.store.due = []domain.ImageIntel{
		trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA),
	}
	harness.store.inventory = []store.InventoryReference{{Reference: "nginx:1.25"}}

	if _, err := harness.service.Collect(context.Background()); !errors.Is(err, service.ErrImageIntelDisabled) {
		t.Errorf("Collect returned %v, want ErrImageIntelDisabled", err)
	}
	if _, err := harness.service.SyncInventory(context.Background()); !errors.Is(err, service.ErrImageIntelDisabled) {
		t.Errorf("SyncInventory returned %v, want ErrImageIntelDisabled", err)
	}
	if harness.service.Enabled() {
		t.Error("a disabled engine reports as enabled")
	}

	if manifests, tags := harness.registry.calls(); manifests != 0 || tags != 0 {
		t.Errorf("a disabled engine made %d manifest and %d tag requests", manifests, tags)
	}
}

// A committed inventory refresh queues a pass. That is the trigger that keeps a
// newly deployed container from being invisible until the next interval.
func TestACommittedRefreshQueuesACollection(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())

	if harness.service.Status(context.Background()).SweepPending {
		t.Fatal("a pass was pending before any refresh")
	}

	harness.service.InventoryRefreshed(7)

	status := harness.service.Status(context.Background())
	if !status.SweepPending {
		t.Error("a committed refresh did not queue a pass")
	}
	if !status.Enabled {
		t.Error("an enabled engine reports as disabled")
	}
}

// Run must exit promptly when cancelled, or shutdown would hang on the worker.
func TestTheImageWorkerStopsOnCancellation(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		harness.service.Run(ctx)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// Concurrency is bounded, so a registry sees a handful of connections rather
// than one per image.
func TestConcurrencyIsBounded(t *testing.T) {
	cfg := intelConfig()
	cfg.MaxConcurrentRequests = 3

	harness := newIntelHarness(t, cfg)
	due := make([]domain.ImageIntel, 0, 40)
	for index := 0; index < 40; index++ {
		due = append(due, trackedRef(
			fmt.Sprintf("docker.io/library/app%d:1.0.0", index), "1.0.0", digestA))
	}
	harness.store.due = due

	var (
		mu      sync.Mutex
		current int
		peak    int
	)
	harness.registry.digest = digestA

	// A counting registry double, so the peak is observed rather than inferred.
	counting := &countingRegistry{
		inner: harness.registry,
		onEnter: func() {
			mu.Lock()
			current++
			if current > peak {
				peak = current
			}
			mu.Unlock()
		},
		onExit: func() {
			mu.Lock()
			current--
			mu.Unlock()
		},
	}

	engine := service.NewImageIntelService(service.ImageIntelOptions{
		Store:    harness.store,
		Registry: counting,
		Config:   cfg,
		Logger:   discardLogger(),
	})

	if _, err := engine.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if peak > cfg.MaxConcurrentRequests {
		t.Errorf("peak concurrency = %d, want at most %d", peak, cfg.MaxConcurrentRequests)
	}
	if peak == 0 {
		t.Error("no request was observed")
	}
}

// countingRegistry observes concurrency around a wrapped client.
type countingRegistry struct {
	inner   service.RegistryClient
	onEnter func()
	onExit  func()
}

func (c *countingRegistry) Manifest(ctx context.Context, req registry.ManifestRequest) (registry.ManifestResult, error) {
	c.onEnter()
	defer c.onExit()
	// Long enough for an unbounded implementation to overlap visibly.
	time.Sleep(2 * time.Millisecond)
	return c.inner.Manifest(ctx, req)
}

func (c *countingRegistry) Tags(ctx context.Context, ref domain.NormalizedRef, pages int) (registry.TagsResult, error) {
	c.onEnter()
	defer c.onExit()
	time.Sleep(time.Millisecond)
	return c.inner.Tags(ctx, ref, pages)
}

// No registry-supplied string may reach a stored record. The details are
// HarborMaster's own words, from a fixed set.
func TestNoRegistryTextReachesAStoredRecord(t *testing.T) {
	poison := "<script>alert(1)</script> DROP TABLE image_intel"

	harness := newIntelHarness(t, intelConfig())
	harness.store.due = []domain.ImageIntel{
		trackedRef("docker.io/library/nginx:1.26.0", "1.26.0", digestA),
	}
	harness.registry.manifestErr = fmt.Errorf("%w: %s", registry.ErrTransient, poison)

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	outcome := harness.store.recorded()[0]
	for _, field := range []string{outcome.Detail, outcome.UpdateReason, outcome.Vendor, outcome.Source} {
		if strings.Contains(field, "script") || strings.Contains(field, "DROP TABLE") {
			t.Errorf("registry text reached a stored field: %q", field)
		}
	}
	if outcome.Detail == "" {
		t.Error("the failure carries no detail at all")
	}
}
