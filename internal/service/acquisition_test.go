package service_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Image acquisition tests.
//
// This is HarborMaster's only Docker mutation, so the tests are written around
// what must NOT happen:
//
//   - Nothing is pulled without a current plan that recommends the change. Every
//     preflight refusal has a test, because each one names a way the world can
//     move between approval and download.
//   - Nothing is reported as acquired that verification did not confirm. A
//     digest or platform mismatch fails, and the evidence is recorded.
//   - Nothing exceeds the configured concurrency, globally or per registry.
//   - Nothing survives a restart claiming to be in flight.
//
// The fake acquirer performs the same target validation the real client does,
// so a test cannot accidentally prove that an illegal target is acceptable.

const (
	// acqTestDigest is the PROPOSED tag's digest, acqCurrentDigest the digest
	// of the tag the container runs now, and acqOtherDigest an unrelated one
	// used to move the world after a plan was written.
	//
	// Three distinct values, deliberately: a fixture that used one digest for
	// the current and proposed tags could not tell a correct pairing from a
	// crossed one, which is how the original defect survived its own tests.
	acqTestDigest    = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	acqOtherDigest   = "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
	acqCurrentDigest = "sha256:" + "3333333333333333333333333333333333333333333333333333333333333333"
	acqPlanID        = "plan_00112233445566778899"
)

// ------------------------------------------------------------- evidence --

// fakeEvidence is the read-only data the preflight re-checks.
type fakeEvidence struct {
	mu sync.Mutex

	plan    domain.ChangePlan
	current domain.ChangePlan
	intel   domain.ImageIntel

	planErr    error
	currentErr error
	intelErr   error

	present    bool
	presentErr error

	// planCalls counts revalidations, which is what proves the preflight runs
	// again immediately before the pull rather than trusting the first run.
	planCalls int
}

func (f *fakeEvidence) Plan(context.Context, string) (domain.ChangePlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.planCalls++
	if f.planErr != nil {
		return domain.ChangePlan{}, f.planErr
	}
	return f.plan, nil
}

func (f *fakeEvidence) CurrentPlan(context.Context, string) (domain.ChangePlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.currentErr != nil {
		return domain.ChangePlan{}, f.currentErr
	}
	return f.current, nil
}

func (f *fakeEvidence) Intel(context.Context, string) (domain.ImageIntel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.intelErr != nil {
		return domain.ImageIntel{}, f.intelErr
	}
	return f.intel, nil
}

func (f *fakeEvidence) ContainerPresent(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.present, f.presentErr
}

func (f *fakeEvidence) revalidations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.planCalls
}

// healthyEvidence is a world in which an acquisition should be allowed.
func healthyEvidence(now time.Time) *fakeEvidence {
	checked := now.Add(-time.Hour)
	plan := domain.ChangePlan{
		PlanID:         acqPlanID,
		ContainerID:    "container-a",
		ContainerName:  "web",
		CurrentImage:   "nginx:1.27.0",
		ProposedImage:  "nginx:1.27.1",
		ProposedDigest: acqTestDigest,
		UpdateType:     domain.UpdatePatch,

		SnapshotID:        7,
		SnapshotAvailable: true,
		RestoreReadiness:  domain.ReadinessReady,
		RegistryStatus:    domain.CheckOK,
		InputDigest:       strings.Repeat("f", 64),

		Risk: domain.RiskAssessment{
			Score:          5,
			Band:           domain.RiskVeryLow,
			Recommendation: domain.RecommendProceed,
		},
	}

	return &fakeEvidence{
		plan:    plan,
		current: plan,
		present: true,
		intel: domain.ImageIntel{
			Reference:  "docker.io/library/nginx:1.27.0",
			Familiar:   "nginx:1.27.0",
			Registry:   "docker.io",
			Repository: "library/nginx",
			Tag:        "1.27.0",
			// The CURRENT tag's digest, which is deliberately NOT the one the
			// plan proposes. Before Phase 10.1 these were the same value,
			// because the planner paired a newer tag with the current tag's
			// digest -- so a fixture that used one digest for both looked
			// coherent while encoding the defect.
			RemoteDigest: acqCurrentDigest,
			LocalDigest:  acqCurrentDigest,
			// The newer tag and ITS OWN digest, resolved together. This pair is
			// what the plan proposes and what acquisition re-checks.
			Update:        domain.UpdatePatch,
			LatestTag:     "1.27.1",
			LatestDigest:  acqTestDigest,
			Platform:      domain.Platform{OS: "linux", Architecture: "amd64"},
			Status:        domain.CheckOK,
			LastSuccessAt: &checked,
		},
	}
}

// -------------------------------------------------------------- store --

// fakeAcquisitionStore is an in-memory acquisition repository.
type fakeAcquisitionStore struct {
	mu sync.Mutex

	records  map[string]*domain.Acquisition
	order    []string
	events   map[string][]domain.AcquisitionEvent
	progress map[string]int

	createErr  error
	advanceErr error
}

func newFakeAcquisitionStore() *fakeAcquisitionStore {
	return &fakeAcquisitionStore{
		records:  make(map[string]*domain.Acquisition),
		events:   make(map[string][]domain.AcquisitionEvent),
		progress: make(map[string]int),
	}
}

func (f *fakeAcquisitionStore) Create(
	_ context.Context, acquisition domain.Acquisition, _ time.Time,
) (domain.Acquisition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.createErr != nil {
		return domain.Acquisition{}, f.createErr
	}
	if !acquisition.Target.Valid() {
		return domain.Acquisition{}, store.ErrAcquisitionTarget
	}
	// The partial unique index, in miniature.
	for _, existing := range f.records {
		if existing.State.Active() &&
			existing.ContainerID == acquisition.ContainerID &&
			existing.Target.Digest == acquisition.Target.Digest {
			return domain.Acquisition{}, store.ErrAcquisitionActive
		}
		if acquisition.RequestKey != "" && existing.RequestKey == acquisition.RequestKey {
			return *existing, nil
		}
	}
	if acquisition.State == "" {
		acquisition.State = domain.AcquisitionQueued
	}

	stored := acquisition
	f.records[acquisition.AcquisitionID] = &stored
	f.order = append(f.order, acquisition.AcquisitionID)
	return stored, nil
}

func (f *fakeAcquisitionStore) Get(_ context.Context, id string) (domain.Acquisition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, ok := f.records[id]
	if !ok {
		return domain.Acquisition{}, store.ErrNotFound
	}
	return *record, nil
}

func (f *fakeAcquisitionStore) List(
	_ context.Context, filter store.AcquisitionFilter,
) ([]domain.Acquisition, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]domain.Acquisition, 0, len(f.order))
	for _, id := range f.order {
		record := f.records[id]
		if filter.ActiveOnly && !record.State.Active() {
			continue
		}
		out = append(out, *record)
	}
	return out, len(out), nil
}

func (f *fakeAcquisitionStore) Events(
	_ context.Context, id string, _ int,
) ([]domain.AcquisitionEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.AcquisitionEvent(nil), f.events[id]...), nil
}

func (f *fakeAcquisitionStore) Summary(context.Context) (domain.AcquisitionSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	summary := domain.AcquisitionSummary{
		ByState:   make(map[domain.AcquisitionState]int),
		ByFailure: make(map[domain.AcquisitionFailure]int),
	}
	for _, record := range f.records {
		summary.Total++
		summary.ByState[record.State]++
		if record.State.Active() {
			summary.Active++
		}
	}
	return summary, nil
}

func (f *fakeAcquisitionStore) Advance(
	_ context.Context, change store.StateChange, now time.Time,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.advanceErr != nil {
		return false, f.advanceErr
	}
	record, ok := f.records[change.AcquisitionID]
	if !ok {
		return false, store.ErrNotFound
	}

	from := change.From
	if len(from) == 0 {
		from = []domain.AcquisitionState{
			domain.AcquisitionQueued, domain.AcquisitionValidating,
			domain.AcquisitionPulling, domain.AcquisitionVerifying,
		}
	}
	allowed := false
	for _, state := range from {
		if record.State == state {
			allowed = true
			break
		}
	}
	if !allowed {
		return false, nil
	}

	record.State = change.To
	record.Failure = change.Failure
	record.Refusal = change.Refusal
	if change.Message != "" {
		record.Message = change.Message
	}
	if change.AcquiredImageID != "" {
		record.AcquiredImageID = change.AcquiredImageID
	}
	if change.AcquiredDigest != "" {
		record.AcquiredDigest = change.AcquiredDigest
	}
	if !change.AcquiredPlatform.Empty() {
		record.AcquiredPlatform = change.AcquiredPlatform
	}
	if change.SizeBytes > 0 {
		record.SizeBytes = change.SizeBytes
	}
	if change.Layers > 0 {
		record.Layers = change.Layers
	}
	if change.BytesTransferred > 0 {
		record.BytesTransferred = change.BytesTransferred
	}
	if change.MarkStarted && record.StartedAt == nil {
		at := now
		record.StartedAt = &at
	}
	if change.To.Terminal() && record.CompletedAt == nil {
		at := now
		record.CompletedAt = &at
	}

	f.events[change.AcquisitionID] = append(f.events[change.AcquisitionID],
		domain.AcquisitionEvent{State: change.To, Detail: change.Detail, At: now})
	return true, nil
}

func (f *fakeAcquisitionStore) RecordProgress(
	_ context.Context, id, progress string, bytes int64, layers int, _ time.Time, _ int,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	record, ok := f.records[id]
	if !ok || record.State != domain.AcquisitionPulling {
		return nil
	}
	record.Progress = progress
	if bytes > record.BytesTransferred {
		record.BytesTransferred = bytes
	}
	if layers > record.Layers {
		record.Layers = layers
	}
	f.progress[id]++
	return nil
}

func (f *fakeAcquisitionStore) ActiveCount(_ context.Context, registry string) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var total, perRegistry int
	for _, record := range f.records {
		if !record.State.Active() {
			continue
		}
		total++
		if record.Target.Registry == registry {
			perRegistry++
		}
	}
	return total, perRegistry, nil
}

func (f *fakeAcquisitionStore) ActiveForTarget(
	_ context.Context, containerID, digest string,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, record := range f.records {
		if record.State.Active() &&
			record.ContainerID == containerID && record.Target.Digest == digest {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeAcquisitionStore) ByRequestKey(
	_ context.Context, key string,
) (domain.Acquisition, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if key == "" {
		return domain.Acquisition{}, false, nil
	}
	for _, record := range f.records {
		if record.RequestKey == key {
			return *record, true, nil
		}
	}
	return domain.Acquisition{}, false, nil
}

func (f *fakeAcquisitionStore) Claimable(_ context.Context, limit int) ([]domain.Acquisition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]domain.Acquisition, 0, limit)
	for _, id := range f.order {
		record := f.records[id]
		if record.State != domain.AcquisitionQueued {
			continue
		}
		out = append(out, *record)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeAcquisitionStore) RecoverInterrupted(_ context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var recovered int64
	for _, record := range f.records {
		switch record.State {
		case domain.AcquisitionValidating, domain.AcquisitionPulling, domain.AcquisitionVerifying:
			record.State = domain.AcquisitionFailed
			record.Failure = domain.AcquisitionFailureInternal
			at := now
			record.CompletedAt = &at
			recovered++
		}
	}
	return recovered, nil
}

func (f *fakeAcquisitionStore) ExpireStale(_ context.Context, now time.Time, _ int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var expired int64
	for _, record := range f.records {
		if record.State == domain.AcquisitionQueued && record.ExpiresAt.Before(now) {
			record.State = domain.AcquisitionExpired
			at := now
			record.CompletedAt = &at
			expired++
		}
	}
	return expired, nil
}

func (f *fakeAcquisitionStore) Prune(context.Context, time.Time, int) (int64, error) { return 0, nil }

func (f *fakeAcquisitionStore) states() map[string]domain.AcquisitionState {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]domain.AcquisitionState, len(f.records))
	for id, record := range f.records {
		out[id] = record.State
	}
	return out
}

func (f *fakeAcquisitionStore) progressWrites(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.progress[id]
}

// ------------------------------------------------------------- harness --

type acquisitionHarness struct {
	service  *service.AcquisitionService
	store    *fakeAcquisitionStore
	evidence *fakeEvidence
	runtime  *docker.Fake
	acquirer *docker.FakeAcquirer
	now      time.Time
}

func acquisitionConfig() config.Acquisition {
	return config.Acquisition{
		Enabled:                 true,
		RequireSnapshot:         true,
		MaxConcurrent:           2,
		MaxPerRegistry:          1,
		PullTimeout:             time.Minute,
		RequestTTL:              time.Hour,
		RegistryFreshness:       24 * time.Hour,
		MaxEventsPerAcquisition: 50,
		SweepInterval:           time.Hour,
		PruneInterval:           time.Hour,
		RetentionAge:            time.Hour,
	}
}

// verifiedImage is what inspection reports for a successful acquisition.
func verifiedImage(digest string) *domain.Image {
	return &domain.Image{
		ID:           "sha256:image1",
		RepoDigests:  []string{"docker.io/library/nginx@" + digest},
		OS:           "linux",
		Architecture: "amd64",
		Size:         123456,
	}
}

func newAcquisitionHarness(t *testing.T, mutate ...func(*acquisitionHarness)) *acquisitionHarness {
	t.Helper()

	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	harness := &acquisitionHarness{
		store:    newFakeAcquisitionStore(),
		evidence: healthyEvidence(now),
		runtime:  &docker.Fake{Images: map[string]*domain.Image{}},
		acquirer: docker.NewFakeAcquirer(),
		now:      now,
	}
	// Inspection resolves the digest-pinned reference, so the fake is keyed by
	// exactly the string the service asks for.
	harness.runtime.Images["docker.io/library/nginx@"+acqTestDigest] = verifiedImage(acqTestDigest)

	for _, apply := range mutate {
		apply(harness)
	}

	harness.service = service.NewAcquisitionService(service.AcquisitionOptions{
		Store:    harness.store,
		Evidence: harness.evidence,
		Runtime:  harness.runtime,
		Acquirer: harness.acquirer,
		Config:   acquisitionConfig(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return harness.now },
	})
	return harness
}

// request asks for an acquisition and fails the test if it is refused.
func (h *acquisitionHarness) request(t *testing.T) domain.Acquisition {
	t.Helper()
	acquisition, err := h.service.Request(t.Context(), service.AcquisitionRequest{PlanID: acqPlanID})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	return acquisition
}

// refusalFrom extracts the refusal from an error, or fails the test.
func refusalFrom(t *testing.T, err error) domain.AcquisitionRefusal {
	t.Helper()
	var refused service.ErrAcquisitionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error %v is not a refusal", err)
	}
	return refused.Refusal
}

// ---------------------------------------------------------- the happy path --

func TestAnApprovedImageIsAcquiredAndVerified(t *testing.T) {
	harness := newAcquisitionHarness(t)

	acquisition := harness.request(t)
	if acquisition.State != domain.AcquisitionQueued {
		t.Fatalf("state = %q, want queued", acquisition.State)
	}
	// The target is derived from the plan and the registry evidence, never from
	// the request.
	if acquisition.Target.Digest != acqTestDigest {
		t.Errorf("target digest = %q", acquisition.Target.Digest)
	}
	if acquisition.Target.PinnedReference() != "docker.io/library/nginx@"+acqTestDigest {
		t.Errorf("pinned reference = %q", acquisition.Target.PinnedReference())
	}

	harness.runOnce(t)

	final, err := harness.store.Get(t.Context(), acquisition.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != domain.AcquisitionSucceeded {
		t.Fatalf("state = %q (%s), want succeeded", final.State, final.Message)
	}
	if final.AcquiredDigest != acqTestDigest || final.AcquiredImageID != "sha256:image1" {
		t.Errorf("verification result = %+v", final)
	}
	if final.SizeBytes != 123456 {
		t.Errorf("size = %d, want the value inspection reported", final.SizeBytes)
	}

	// The pull was digest-pinned, and only the approved target was requested.
	target, ok := harness.acquirer.LastTarget()
	if !ok {
		t.Fatal("nothing was pulled")
	}
	if target.Digest != acqTestDigest || target.Repository != "library/nginx" {
		t.Errorf("pulled %+v", target)
	}
	if !strings.Contains(target.Reference(), "@sha256:") {
		t.Errorf("pull reference %q is not digest-pinned", target.Reference())
	}
}

// runOnce drives one dispatch pass to completion.
//
// The worker's own Run loop is exercised separately; this drains the queue
// deterministically so a test can assert on the outcome without sleeping.
func (h *acquisitionHarness) runOnce(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.service.Run(ctx)
	}()

	deadline := time.After(10 * time.Second)
	for {
		settled := true
		for _, state := range h.store.states() {
			if state.Active() {
				settled = false
				break
			}
		}
		if settled {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("acquisitions did not settle: %v", h.store.states())
		case <-time.After(2 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not stop with its context")
	}
}

// The preflight runs AGAIN immediately before the pull. This is the whole
// time-of-check/time-of-use defence, so it is asserted directly.
func TestThePlanIsRevalidatedImmediatelyBeforeThePull(t *testing.T) {
	harness := newAcquisitionHarness(t)

	harness.request(t)
	afterRequest := harness.evidence.revalidations()

	harness.runOnce(t)

	if harness.evidence.revalidations() <= afterRequest {
		t.Error("the plan was not re-read before the pull; the request-time check was trusted")
	}
}

// -------------------------------------------------------------- refusals --

// Each of these is a way the world can move between approval and download. The
// table is the safety model, stated as tests.
func TestThePreflightRefusesEachUnsafeSituation(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*acquisitionHarness)
		want   domain.AcquisitionRefusal
	}{
		"the plan is gone": {
			mutate: func(h *acquisitionHarness) { h.evidence.planErr = store.ErrNotFound },
			want:   domain.AcquisitionRefusalPlanMissing,
		},
		"a newer plan exists": {
			mutate: func(h *acquisitionHarness) {
				h.evidence.current.PlanID = "plan_ffffffffffffffffffff"
			},
			want: domain.AcquisitionRefusalPlanSuperseded,
		},
		"the plan advises against the change": {
			mutate: func(h *acquisitionHarness) {
				h.evidence.plan.Risk.Recommendation = domain.RecommendAgainst
				h.evidence.current.Risk.Recommendation = domain.RecommendAgainst
			},
			want: domain.AcquisitionRefusalRecommendation,
		},
		"the plan could not judge the change": {
			mutate: func(h *acquisitionHarness) {
				h.evidence.plan.Risk.Recommendation = domain.RecommendUnknown
				h.evidence.current.Risk.Recommendation = domain.RecommendUnknown
			},
			want: domain.AcquisitionRefusalRecommendation,
		},
		"the plan names no digest": {
			mutate: func(h *acquisitionHarness) {
				h.evidence.plan.ProposedDigest = ""
				h.evidence.current.ProposedDigest = ""
			},
			want: domain.AcquisitionRefusalDigestUnavailable,
		},
		"the proposed tag's digest moved after approval": {
			mutate: func(h *acquisitionHarness) { h.evidence.intel.LatestDigest = acqOtherDigest },
			want:   domain.AcquisitionRefusalDigestChanged,
		},
		// A DIFFERENT newer tag appeared, so the plan's reference is no longer
		// what is on offer even though its digest may still resolve. Before
		// Phase 10.1 the reference was not part of this comparison at all, so a
		// re-pointed proposal would have been accepted.
		"a different newer tag appeared after approval": {
			mutate: func(h *acquisitionHarness) { h.evidence.intel.LatestTag = "1.27.9" },
			want:   domain.AcquisitionRefusalDigestChanged,
		},
		// The newer tag is still on offer but can no longer be pinned. An
		// unpinnable target must refuse rather than fall back to the current
		// tag's digest, which is exactly what the old code did.
		// The plan named a newer tag that can no longer be pinned, but the
		// tracking reference still resolves. Something IS on offer -- just not
		// what this plan was assessed against -- so the honest refusal is that
		// the offer changed, not that nothing could be pinned.
		//
		// Before Stage 17.9 the check re-derived the planner's choice and this
		// read as digestUnavailable. It now compares the plan against every
		// target the registry currently serves, which is what makes the planner
		// and the check unable to disagree.
		"the proposed tag can no longer be pinned": {
			mutate: func(h *acquisitionHarness) { h.evidence.intel.LatestDigest = "" },
			want:   domain.AcquisitionRefusalDigestChanged,
		},
		// Nothing at all resolves: no newer tag, and the tracking reference has
		// no digest either. This is what digestUnavailable means, and it keeps
		// the refusal reachable.
		"nothing the registry serves can be pinned": {
			mutate: func(h *acquisitionHarness) {
				h.evidence.intel.LatestDigest = ""
				h.evidence.intel.RemoteDigest = ""
			},
			want: domain.AcquisitionRefusalDigestUnavailable,
		},
		"the registry has never been checked": {
			mutate: func(h *acquisitionHarness) {
				h.evidence.intel.Status = domain.CheckPending
				h.evidence.intel.LastSuccessAt = nil
			},
			want: domain.AcquisitionRefusalRegistryStale,
		},
		"the registry check failed": {
			mutate: func(h *acquisitionHarness) { h.evidence.intel.Status = domain.CheckFailed },
			want:   domain.AcquisitionRefusalRegistryStale,
		},
		"the registry evidence is old": {
			mutate: func(h *acquisitionHarness) {
				stale := h.now.Add(-72 * time.Hour)
				h.evidence.intel.LastSuccessAt = &stale
			},
			want: domain.AcquisitionRefusalRegistryStale,
		},
		"there is no registry record at all": {
			mutate: func(h *acquisitionHarness) { h.evidence.intelErr = store.ErrNotFound },
			want:   domain.AcquisitionRefusalRegistryStale,
		},
		"the image does not publish this platform": {
			mutate: func(h *acquisitionHarness) { h.evidence.intel.PlatformMissing = true },
			want:   domain.AcquisitionRefusalPlatformUnavailable,
		},
		"the container is gone": {
			mutate: func(h *acquisitionHarness) { h.evidence.present = false },
			want:   domain.AcquisitionRefusalContainerMissing,
		},
		"a critical policy violation is open": {
			mutate: func(h *acquisitionHarness) {
				h.evidence.plan.PolicyOpen = 1
				h.evidence.plan.PolicyMaxSeverity = domain.PolicySeverityCritical
				h.evidence.current = h.evidence.plan
			},
			want: domain.AcquisitionRefusalPolicyViolation,
		},
		"there is no usable snapshot": {
			mutate: func(h *acquisitionHarness) {
				h.evidence.plan.SnapshotAvailable = false
				h.evidence.current = h.evidence.plan
			},
			want: domain.AcquisitionRefusalRestoreReadiness,
		},
		"restore readiness failed": {
			mutate: func(h *acquisitionHarness) {
				h.evidence.plan.RestoreReadiness = domain.ReadinessNotReady
				h.evidence.current = h.evidence.plan
			},
			want: domain.AcquisitionRefusalRestoreReadiness,
		},
		"docker is unavailable": {
			mutate: func(h *acquisitionHarness) {
				h.runtime.PingErr = errors.New("dial unix /var/run/docker.sock: connect: no such file")
			},
			want: domain.AcquisitionRefusalDockerUnavailable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			harness := newAcquisitionHarness(t, tc.mutate)

			_, err := harness.service.Request(t.Context(),
				service.AcquisitionRequest{PlanID: acqPlanID})
			if err == nil {
				t.Fatal("the request should have been refused")
			}
			if got := refusalFrom(t, err); got != tc.want {
				t.Errorf("refusal = %q, want %q", got, tc.want)
			}

			// Nothing was pulled, which is the property that actually matters.
			if harness.acquirer.CallCount() != 0 {
				t.Errorf("a refused request reached the daemon %d times", harness.acquirer.CallCount())
			}
		})
	}
}

// A world that moves AFTER the request is accepted is caught by the second
// preflight, and the acquisition fails with the specific refusal recorded.
func TestAWorldThatMovesAfterTheRequestIsCaughtBeforeThePull(t *testing.T) {
	harness := newAcquisitionHarness(t)

	acquisition := harness.request(t)

	// The PROPOSED tag was republished between approval and the worker picking
	// the job up, so the digest the plan pinned is no longer what that tag
	// serves.
	//
	// The proposed tag's digest is the one that matters here. Moving the
	// current tag's digest would be a different event -- and before Phase 10.1
	// the two were the same field, so this test could not tell them apart.
	harness.evidence.mu.Lock()
	harness.evidence.intel.LatestDigest = acqOtherDigest
	harness.evidence.mu.Unlock()

	harness.runOnce(t)

	final, err := harness.store.Get(t.Context(), acquisition.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != domain.AcquisitionFailed {
		t.Fatalf("state = %q, want failed", final.State)
	}
	if final.Refusal != domain.AcquisitionRefusalDigestChanged {
		t.Errorf("refusal = %q, want digestChanged", final.Refusal)
	}
	if final.Failure != domain.AcquisitionFailurePreflight {
		t.Errorf("failure = %q, want preflight", final.Failure)
	}
	if harness.acquirer.CallCount() != 0 {
		t.Error("the pull went ahead after the digest changed")
	}
}

// A refusal that is not the operator's fault must still be reported clearly.
// The message is HarborMaster's own words, never a daemon string.
func TestARefusalMessageCarriesNoDaemonText(t *testing.T) {
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		h.runtime.PingErr = errors.New(
			"Cannot connect to the Docker daemon at unix:///var/run/docker.sock")
	})

	_, err := harness.service.Request(t.Context(), service.AcquisitionRequest{PlanID: acqPlanID})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), "/var/run/docker.sock") {
		t.Errorf("the refusal leaked the socket path: %v", err)
	}
}

// ---------------------------------------------------------- verification --

// The pull's own success is never the proof. An image that arrives carrying a
// different digest is a FAILURE, and the evidence is recorded.
func TestADigestMismatchFailsAndRecordsWhatArrived(t *testing.T) {
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		// The daemon reports success, but the local store holds something else.
		h.runtime.Images["docker.io/library/nginx@"+acqTestDigest] = &domain.Image{
			ID:           "sha256:someotherimage",
			RepoDigests:  []string{"docker.io/library/nginx@" + acqOtherDigest},
			OS:           "linux",
			Architecture: "amd64",
			Size:         999,
		}
	})

	acquisition := harness.request(t)
	harness.runOnce(t)

	final, err := harness.store.Get(t.Context(), acquisition.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != domain.AcquisitionSucceeded {
		// The assertion is the point: it must NOT have succeeded.
	} else {
		t.Fatal("an image with the wrong digest was reported as acquired")
	}
	if final.Failure != domain.AcquisitionFailureDigestMismatch {
		t.Errorf("failure = %q, want digestMismatch", final.Failure)
	}
	// The evidence: what actually arrived.
	if final.AcquiredImageID != "sha256:someotherimage" {
		t.Errorf("the acquired image id was not recorded: %+v", final)
	}
	// A digest mismatch is never retryable: repeating a pull that produced the
	// wrong content is how a transient substitution becomes a persistent one.
	if final.Failure.Retryable() {
		t.Error("a digest mismatch must not be reported as retryable")
	}
}

// An image that arrives targeting the wrong platform would not run here, and
// reporting it as acquired would be a promise HarborMaster cannot keep.
func TestAPlatformMismatchFails(t *testing.T) {
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		image := verifiedImage(acqTestDigest)
		image.Architecture = "arm64"
		h.runtime.Images["docker.io/library/nginx@"+acqTestDigest] = image
	})

	acquisition := harness.request(t)
	harness.runOnce(t)

	final, err := harness.store.Get(t.Context(), acquisition.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != domain.AcquisitionFailed {
		t.Fatalf("state = %q, want failed", final.State)
	}
	if final.Failure != domain.AcquisitionFailurePlatformMismatch {
		t.Errorf("failure = %q, want platformMismatch", final.Failure)
	}
	if final.AcquiredPlatform.Architecture != "arm64" {
		t.Errorf("the acquired platform was not recorded: %+v", final.AcquiredPlatform)
	}
}

// A verification that could not be PERFORMED establishes nothing, which is not
// the same as a mismatch. Both fail; they are classified differently so an
// operator can tell them apart.
func TestAVerificationThatCannotRunIsItsOwnFailure(t *testing.T) {
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		h.runtime.Images = map[string]*domain.Image{}
		h.runtime.ImageErrs = map[string]error{
			"docker.io/library/nginx@" + acqTestDigest: docker.ErrImageUnavailable,
		}
	})

	acquisition := harness.request(t)
	harness.runOnce(t)

	final, err := harness.store.Get(t.Context(), acquisition.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Failure != domain.AcquisitionFailureVerification {
		t.Errorf("failure = %q, want verification", final.Failure)
	}
	if final.State == domain.AcquisitionSucceeded {
		t.Error("an unverifiable image was reported as acquired")
	}
}

// ------------------------------------------------------------- failures --

func TestATransferFailureIsRecordedAsRetryable(t *testing.T) {
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		h.acquirer.Err = docker.ErrUnreachable
	})

	acquisition := harness.request(t)
	harness.runOnce(t)

	final, err := harness.store.Get(t.Context(), acquisition.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != domain.AcquisitionFailed {
		t.Fatalf("state = %q, want failed", final.State)
	}
	if final.Failure != domain.AcquisitionFailureDockerUnavailable {
		t.Errorf("failure = %q, want dockerUnavailable", final.Failure)
	}
	if !final.Failure.Retryable() {
		t.Error("a daemon that was down should be retryable")
	}
	if strings.Contains(final.Message, "docker.sock") {
		t.Errorf("the message leaked daemon detail: %q", final.Message)
	}
}

// A registry that refused is a permanent answer, not a transient one:
// HarborMaster holds no credentials by design.
func TestARegistryRefusalIsNotRetryable(t *testing.T) {
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		h.acquirer.Err = fmt.Errorf("%w: the registry requires credentials", docker.ErrPullFailed)
	})

	acquisition := harness.request(t)
	harness.runOnce(t)

	final, err := harness.store.Get(t.Context(), acquisition.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Failure != domain.AcquisitionFailureRegistry {
		t.Errorf("failure = %q, want registry", final.Failure)
	}
	if final.Failure.Retryable() {
		t.Error("a registry refusal should not invite an automatic retry")
	}
}

// A database failure while recording must not be reported as a success.
func TestADatabaseFailureDoesNotProduceASuccess(t *testing.T) {
	harness := newAcquisitionHarness(t)

	acquisition := harness.request(t)

	harness.store.mu.Lock()
	harness.store.advanceErr = errors.New("database is locked")
	harness.store.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		harness.service.Run(ctx)
	}()
	<-ctx.Done()
	<-done

	harness.store.mu.Lock()
	harness.store.advanceErr = nil
	harness.store.mu.Unlock()

	final, err := harness.store.Get(t.Context(), acquisition.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State == domain.AcquisitionSucceeded {
		t.Error("a database failure produced a success")
	}
	if harness.acquirer.CallCount() != 0 {
		t.Error("a pull ran despite the claim failing")
	}
}

// ------------------------------------------------------------ duplicates --

func TestASecondRequestForTheSameTargetIsRefused(t *testing.T) {
	harness := newAcquisitionHarness(t)

	harness.request(t)

	_, err := harness.service.Request(t.Context(), service.AcquisitionRequest{PlanID: acqPlanID})
	if err == nil {
		t.Fatal("a second request for an in-flight target should be refused")
	}
	if got := refusalFrom(t, err); got != domain.AcquisitionRefusalDuplicate {
		t.Errorf("refusal = %q, want duplicate", got)
	}
}

// An idempotency key means a double-clicked button produces one download.
func TestAnIdempotentRequestDoesNotStartASecondDownload(t *testing.T) {
	harness := newAcquisitionHarness(t)

	first, err := harness.service.Request(t.Context(), service.AcquisitionRequest{
		PlanID: acqPlanID, RequestKey: "click-1",
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	second, err := harness.service.Request(t.Context(), service.AcquisitionRequest{
		PlanID: acqPlanID, RequestKey: "click-1",
	})
	if err != nil {
		t.Fatalf("repeated request: %v", err)
	}
	if second.AcquisitionID != first.AcquisitionID {
		t.Errorf("a repeated key produced a new acquisition: %q then %q",
			first.AcquisitionID, second.AcquisitionID)
	}
}

// ------------------------------------------------------------ concurrency --

// The per-registry limit is what protects a third party: anonymous rate limits
// are shared by egress address.
func TestConcurrentTransfersRespectThePerRegistryLimit(t *testing.T) {
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		// Slow enough that a second dispatch would overlap if the limit were
		// not enforced.
		h.acquirer.Progress = docker.ProgressFor("Downloading", 4)
		h.acquirer.Delay = 20 * time.Millisecond
	})

	// Three containers, one registry, all approved.
	for index := 0; index < 3; index++ {
		containerID := fmt.Sprintf("container-%d", index)
		acquisition := domain.Acquisition{
			AcquisitionID: domain.NewAcquisitionID(),
			PlanID:        acqPlanID,
			ContainerID:   containerID,
			Target: domain.AcquisitionTarget{
				Registry:   "docker.io",
				Repository: "library/nginx",
				Digest:     acqTestDigest,
				Platform:   domain.Platform{OS: "linux", Architecture: "amd64"},
			},
			State:       domain.AcquisitionQueued,
			RequestedAt: harness.now,
			ExpiresAt:   harness.now.Add(time.Hour),
		}
		if _, err := harness.store.Create(t.Context(), acquisition, harness.now); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		harness.service.Run(ctx)
	}()

	// Watch the in-flight count while work drains.
	peak := 0
	deadline := time.After(15 * time.Second)
	for {
		active := 0
		for _, state := range harness.store.states() {
			if state == domain.AcquisitionPulling || state == domain.AcquisitionValidating {
				active++
			}
		}
		if active > peak {
			peak = active
		}

		settled := true
		for _, state := range harness.store.states() {
			if state.Active() {
				settled = false
				break
			}
		}
		if settled {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("work did not drain: %v", harness.store.states())
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	<-done

	if peak > acquisitionConfig().MaxPerRegistry {
		t.Errorf("%d transfers ran against one registry at once, want at most %d",
			peak, acquisitionConfig().MaxPerRegistry)
	}
	if harness.acquirer.CallCount() != 3 {
		t.Errorf("pulled %d times, want all 3 eventually", harness.acquirer.CallCount())
	}
}

// The limit is also checked at request time, so an operator is told rather than
// silently queued behind a long transfer.
func TestARequestBeyondTheGlobalLimitIsRefused(t *testing.T) {
	harness := newAcquisitionHarness(t)

	// Two acquisitions in flight is the configured global maximum.
	for index := 0; index < 2; index++ {
		acquisition := domain.Acquisition{
			AcquisitionID: domain.NewAcquisitionID(),
			ContainerID:   fmt.Sprintf("other-%d", index),
			Target: domain.AcquisitionTarget{
				Registry:   "ghcr.io",
				Repository: "org/app",
				Digest:     acqOtherDigest,
			},
			State:       domain.AcquisitionPulling,
			RequestedAt: harness.now,
			ExpiresAt:   harness.now.Add(time.Hour),
		}
		if _, err := harness.store.Create(t.Context(), acquisition, harness.now); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	_, err := harness.service.Request(t.Context(), service.AcquisitionRequest{PlanID: acqPlanID})
	if err == nil {
		t.Fatal("a request beyond the limit should be refused")
	}
	if got := refusalFrom(t, err); got != domain.AcquisitionRefusalLimit {
		t.Errorf("refusal = %q, want limit", got)
	}
}

// ---------------------------------------------------------- cancellation --

// An operator can stop a transfer that is already running.
func TestCancellingStopsATransferInFlight(t *testing.T) {
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		h.acquirer.Progress = docker.ProgressFor("Downloading", 200)
		h.acquirer.Delay = 20 * time.Millisecond
	})

	acquisition := harness.request(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		harness.service.Run(ctx)
	}()

	// Wait for the transfer to actually be running, rather than sleeping.
	select {
	case <-harness.acquirer.Started:
	case <-time.After(10 * time.Second):
		t.Fatal("the pull never started")
	}

	if _, err := harness.service.Cancel(t.Context(), acquisition.AcquisitionID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	final, err := harness.store.Get(t.Context(), acquisition.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != domain.AcquisitionCancelled {
		t.Errorf("state = %q, want cancelled", final.State)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not stop after a cancellation")
	}

	// A cancelled acquisition is never reported as acquired.
	after, err := harness.store.Get(t.Context(), acquisition.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State == domain.AcquisitionSucceeded {
		t.Error("a cancelled transfer was reported as succeeded")
	}
}

// Verifying is deliberately not cancellable: the bytes are already on the host,
// and stopping the confirmation would leave an unverified image with no record
// saying so.
func TestVerificationCannotBeCancelled(t *testing.T) {
	harness := newAcquisitionHarness(t)

	acquisition := harness.request(t)
	if _, err := harness.store.Advance(t.Context(), store.StateChange{
		AcquisitionID: acquisition.AcquisitionID,
		To:            domain.AcquisitionVerifying,
	}, harness.now); err != nil {
		t.Fatalf("advance: %v", err)
	}

	if _, err := harness.service.Cancel(t.Context(), acquisition.AcquisitionID); err == nil {
		t.Fatal("verifying should not be cancellable")
	}

	final, err := harness.store.Get(t.Context(), acquisition.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != domain.AcquisitionVerifying {
		t.Errorf("state = %q, want it left verifying", final.State)
	}
}

// A pull that outruns its budget is timed out rather than left holding a slot.
func TestAPullThatExceedsItsBudgetTimesOut(t *testing.T) {
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		h.acquirer.Progress = docker.ProgressFor("Downloading", 1000)
		h.acquirer.Delay = 50 * time.Millisecond
	})
	// Rebuilt with a tiny budget, which is the thing under test.
	harness.service = service.NewAcquisitionService(service.AcquisitionOptions{
		Store:    harness.store,
		Evidence: harness.evidence,
		Runtime:  harness.runtime,
		Acquirer: harness.acquirer,
		Config: func() config.Acquisition {
			cfg := acquisitionConfig()
			cfg.PullTimeout = 100 * time.Millisecond
			return cfg
		}(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return harness.now },
	})

	acquisition := harness.request(t)
	harness.runOnce(t)

	final, err := harness.store.Get(t.Context(), acquisition.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != domain.AcquisitionFailed {
		t.Fatalf("state = %q, want failed", final.State)
	}
	if final.Failure != domain.AcquisitionFailureTimeout {
		t.Errorf("failure = %q, want timeout", final.Failure)
	}
}

// ------------------------------------------------------------- progress --

// Progress writes are bounded, so a chatty transfer cannot turn one operator
// action into an unbounded number of writes.
func TestProgressWritesAreBounded(t *testing.T) {
	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		h.acquirer.Progress = docker.ProgressFor("Downloading", 500)
	})
	harness.service = service.NewAcquisitionService(service.AcquisitionOptions{
		Store:    harness.store,
		Evidence: harness.evidence,
		Runtime:  harness.runtime,
		Acquirer: harness.acquirer,
		Config: func() config.Acquisition {
			cfg := acquisitionConfig()
			cfg.MaxEventsPerAcquisition = 10
			return cfg
		}(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return harness.now },
	})

	acquisition := harness.request(t)
	harness.runOnce(t)

	if writes := harness.store.progressWrites(acquisition.AcquisitionID); writes > 10 {
		t.Errorf("%d progress writes, want at most the configured 10", writes)
	}
}

// ------------------------------------------------------------- recovery --

// An acquisition left mid-flight by a restart is failed honestly rather than
// resumed. The transfer was never verified, and an unverified image must never
// be recorded as acquired.
func TestARestartFailsInterruptedWorkRatherThanResumingIt(t *testing.T) {
	harness := newAcquisitionHarness(t)

	acquisition := harness.request(t)
	if _, err := harness.store.Advance(t.Context(), store.StateChange{
		AcquisitionID: acquisition.AcquisitionID,
		To:            domain.AcquisitionPulling,
	}, harness.now); err != nil {
		t.Fatalf("advance: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		harness.service.Run(ctx)
	}()
	<-done

	final, err := harness.store.Get(t.Context(), acquisition.AcquisitionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != domain.AcquisitionFailed {
		t.Errorf("state = %q, want failed", final.State)
	}
	if final.AcquiredDigest != "" {
		t.Errorf("an unverified acquisition claims a digest: %q", final.AcquiredDigest)
	}
	if harness.acquirer.CallCount() != 0 {
		t.Error("recovery resumed a transfer instead of failing it")
	}
}

// -------------------------------------------------------------- disabled --

// A deployment that has not opted in has no capability at all, which is
// different from having one that is switched off.
func TestADisabledServiceHasNoCapability(t *testing.T) {
	harness := newAcquisitionHarness(t)
	disabled := service.NewAcquisitionService(service.AcquisitionOptions{
		Store:    harness.store,
		Evidence: harness.evidence,
		Runtime:  harness.runtime,
		// No acquirer: the capability is absent rather than merely unused.
		Acquirer: nil,
		Config:   acquisitionConfig(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return harness.now },
	})

	if disabled.Enabled() {
		t.Error("a service with no acquirer reports itself enabled")
	}

	_, err := disabled.Request(t.Context(), service.AcquisitionRequest{PlanID: acqPlanID})
	if err == nil {
		t.Fatal("a disabled service should refuse")
	}
	if got := refusalFrom(t, err); got != domain.AcquisitionRefusalDisabled {
		t.Errorf("refusal = %q, want disabled", got)
	}

	// And the worker returns immediately rather than idling.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		disabled.Run(context.Background())
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("a disabled worker should return rather than idle")
	}
}

// Configuration alone is not enough: a deployment that enabled the feature but
// wired no capability must report disabled rather than accept requests it can
// never run.
func TestEnabledRequiresBothConfigurationAndCapability(t *testing.T) {
	harness := newAcquisitionHarness(t)

	cfg := acquisitionConfig()
	cfg.Enabled = false
	withoutConfig := service.NewAcquisitionService(service.AcquisitionOptions{
		Store: harness.store, Evidence: harness.evidence,
		Runtime: harness.runtime, Acquirer: harness.acquirer,
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if withoutConfig.Enabled() {
		t.Error("a service disabled by configuration reports itself enabled")
	}
}

// -------------------------------------------------------------- eligible --

// The eligibility check runs the SAME preflight as the request path, so the
// button and the outcome cannot disagree -- and it records nothing.
func TestEligibilityUsesTheSameChecksAsARequest(t *testing.T) {
	harness := newAcquisitionHarness(t)

	target, refusal, err := harness.service.Eligible(t.Context(), acqPlanID)
	if err != nil {
		t.Fatalf("Eligible: %v", err)
	}
	if refusal != domain.AcquisitionRefusalNone {
		t.Fatalf("a healthy plan should be eligible, got %q", refusal)
	}
	if target.Digest != acqTestDigest {
		t.Errorf("target = %+v", target)
	}
	if len(harness.store.states()) != 0 {
		t.Error("an eligibility check recorded an acquisition")
	}

	harness.evidence.mu.Lock()
	harness.evidence.intel.LatestDigest = acqOtherDigest
	harness.evidence.mu.Unlock()

	_, refusal, err = harness.service.Eligible(t.Context(), acqPlanID)
	if err != nil {
		t.Fatalf("Eligible: %v", err)
	}
	if refusal != domain.AcquisitionRefusalDigestChanged {
		t.Errorf("refusal = %q, want digestChanged", refusal)
	}
}
