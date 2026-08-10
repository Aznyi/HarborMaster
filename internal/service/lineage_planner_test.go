package service_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The Phase 13.1 regression.
//
// Phase 13 validation found that a container HarborMaster had updated fell out
// of automation permanently: recreation is digest-pinned, so the next pass saw
// only `repo@sha256:...`, image intelligence correctly answered that a digest
// cannot move, and the planner had nothing to propose. Every container received
// exactly one automated update.
//
// These tests drive the REAL planner. The A -> B -> C case below fails against
// the pre-fix planner -- it produces one plan and then nothing -- and passes
// once the container's tracking reference is followed instead of the immutable
// reference it declares.

const (
	lineageA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	lineageB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	lineageC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	trackingRef      = "docker.io/library/app:latest"
	trackingFamiliar = "app:latest"
	trackingRepo     = "library/app"
)

// fakeLineageStore is an in-memory lineage table.
type fakeLineageStore struct {
	mu   sync.Mutex
	rows map[string]domain.ImageLineage
	err  error
}

func newFakeLineageStore() *fakeLineageStore {
	return &fakeLineageStore{rows: map[string]domain.ImageLineage{}}
}

func (f *fakeLineageStore) Get(_ context.Context, name string) (domain.ImageLineage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return domain.ImageLineage{}, f.err
	}
	row, ok := f.rows[name]
	if !ok {
		return domain.ImageLineage{}, store.ErrNotFound
	}
	return row, nil
}

func (f *fakeLineageStore) All(context.Context) ([]domain.ImageLineage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	all := make([]domain.ImageLineage, 0, len(f.rows))
	for _, row := range f.rows {
		all = append(all, row)
	}
	return all, nil
}

func (f *fakeLineageStore) Tracked(ctx context.Context) ([]domain.ImageLineage, error) {
	all, err := f.All(ctx)
	if err != nil {
		return nil, err
	}
	tracked := make([]domain.ImageLineage, 0, len(all))
	for _, row := range all {
		if row.Tracked() {
			tracked = append(tracked, row)
		}
	}
	return tracked, nil
}

func (f *fakeLineageStore) Upsert(_ context.Context, row domain.ImageLineage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.rows[row.ContainerName] = row
	return nil
}

func (f *fakeLineageStore) set(row domain.ImageLineage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[row.ContainerName] = row
}

// lineageRow builds a tracked row running the given digest.
func lineageRow(running string) domain.ImageLineage {
	now := time.Now().UTC()
	return domain.ImageLineage{
		ContainerName:     "app",
		ContainerID:       strings.Repeat("1", 64),
		State:             domain.LineageTracked,
		Origin:            domain.LineageRecreated,
		TrackingReference: trackingRef,
		TrackingFamiliar:  trackingFamiliar,
		Repository:        trackingRepo,
		RunningDigest:     running,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
}

// registryAt builds the intelligence for the tracking tag resolving to digest.
func registryAt(digest string) domain.ImageIntel {
	return domain.ImageIntel{
		Reference:    trackingRef,
		Familiar:     trackingFamiliar,
		Repository:   trackingRepo,
		Tag:          "latest",
		RemoteDigest: digest,
		Status:       domain.CheckOK,
		Update:       domain.UpdateNone,
	}
}

// digestPinnedCandidate is the container as it looks AFTER HarborMaster has
// updated it: declaring an immutable digest, which is the state that used to be
// terminal.
func digestPinnedCandidate(digest string) store.PlanCandidate {
	return store.PlanCandidate{
		ContainerID:   strings.Repeat("1", 64),
		ContainerName: "app",
		ImageRef:      "docker.io/library/app@" + digest,
		ImageID:       digest,
		ImageDigest:   digest,
	}
}

func lineagePlanner(t *testing.T, planStore *fakePlanStore, lineages *fakeLineageStore) *service.PlannerService {
	t.Helper()
	return service.NewPlannerService(service.PlannerOptions{
		Store:   planStore,
		Lineage: lineages,
		Config: config.Planner{
			Enabled:       true,
			BatchSize:     50,
			MaxContainers: 500,
		},
		Logger: discardLogger(),
		Now:    func() time.Time { return time.Now().UTC() },
	})
}

// THE REGRESSION TEST.
//
// A container is updated A -> B, then B -> C, entirely through planning. Before
// Phase 13.1 the second pass produced nothing at all, because the container
// declared an immutable digest and there was no record of what it followed.
func TestAContainerOnADigestIsStillPlannedAsItsTagMoves(t *testing.T) {
	lineages := newFakeLineageStore()
	planStore := newFakePlanStore()
	planner := lineagePlanner(t, planStore, lineages)
	ctx := context.Background()

	// ---- the container is running A, and the registry now serves B --------
	lineages.set(lineageRow(lineageA))
	planStore.candidates = []store.PlanCandidate{digestPinnedCandidate(lineageA)}
	planStore.intel = map[string]domain.ImageIntel{trackingRef: registryAt(lineageB)}

	if _, err := planner.Generate(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	plans := planStore.plans()
	if len(plans) != 1 {
		t.Fatalf("A -> B produced %d plans, want 1; a digest-pinned managed container "+
			"was not assessed against its tracking reference", len(plans))
	}
	if plans[0].ProposedDigest != lineageB {
		t.Fatalf("ProposedDigest = %q, want B", plans[0].ProposedDigest)
	}
	if plans[0].CurrentDigest != lineageA {
		t.Errorf("CurrentDigest = %q, want the digest actually running", plans[0].CurrentDigest)
	}
	if plans[0].UpdateType != domain.UpdateDigest {
		t.Errorf("UpdateType = %q, want digest", plans[0].UpdateType)
	}

	// ---- the update is applied: now running B, registry serves C ----------
	lineages.set(lineageRow(lineageB))
	planStore.candidates = []store.PlanCandidate{digestPinnedCandidate(lineageB)}
	planStore.intel = map[string]domain.ImageIntel{trackingRef: registryAt(lineageC)}

	if _, err := planner.Generate(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	plans = planStore.plans()
	if len(plans) != 2 {
		t.Fatalf("B -> C produced no second plan (total %d); the container fell out of "+
			"automation after its first update -- this is the Phase 13 defect", len(plans))
	}
	if plans[1].ProposedDigest != lineageC {
		t.Errorf("second ProposedDigest = %q, want C", plans[1].ProposedDigest)
	}
	if plans[1].CurrentDigest != lineageB {
		t.Errorf("second CurrentDigest = %q, want B", plans[1].CurrentDigest)
	}

	// The tracking reference never moved: both plans propose the same tag, and
	// that is what makes a third cycle possible.
	for i, plan := range plans {
		if plan.ProposedImage != trackingFamiliar {
			t.Errorf("plan %d ProposedImage = %q, want the followed tag %q",
				i, plan.ProposedImage, trackingFamiliar)
		}
	}
}

// A settled container proposes nothing, and "settled" must be a real answer
// rather than the absence of one.
func TestATrackedContainerAtTheRegistryDigestProposesNothing(t *testing.T) {
	lineages := newFakeLineageStore()
	planStore := newFakePlanStore()
	planner := lineagePlanner(t, planStore, lineages)

	lineages.set(lineageRow(lineageA))
	planStore.candidates = []store.PlanCandidate{digestPinnedCandidate(lineageA)}
	planStore.intel = map[string]domain.ImageIntel{trackingRef: registryAt(lineageA)}

	if _, err := planner.Generate(context.Background()); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got := len(planStore.plans()); got != 0 {
		t.Fatalf("a container already running the registry's digest produced %d plans", got)
	}
}

// A genuinely digest-pinned container that HarborMaster does not manage must
// stay out of automation. It is the case the conservative establishment rule
// exists for, and the planner must not invent an assessment for it.
func TestAnUntrackedDigestPinnedContainerIsStillNotPlanned(t *testing.T) {
	lineages := newFakeLineageStore()
	planStore := newFakePlanStore()
	planner := lineagePlanner(t, planStore, lineages)

	lineages.set(domain.ImageLineage{
		ContainerName: "app",
		State:         domain.LineageUntracked,
		Origin:        domain.LineageObserved,
		RunningDigest: lineageA,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	})
	planStore.candidates = []store.PlanCandidate{digestPinnedCandidate(lineageA)}
	planStore.intel = map[string]domain.ImageIntel{trackingRef: registryAt(lineageB)}

	if _, err := planner.Generate(context.Background()); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got := len(planStore.plans()); got != 0 {
		t.Fatalf("an untracked digest-pinned container was planned (%d plans); HarborMaster "+
			"invented a tag for a workload somebody pinned deliberately", got)
	}
}

// External change: what is running is not what HarborMaster approved. The
// planner must refuse rather than propose a change from a starting point it
// cannot vouch for.
func TestAContainerChangedOutsideHarborMasterIsNotPlanned(t *testing.T) {
	lineages := newFakeLineageStore()
	planStore := newFakePlanStore()
	planner := lineagePlanner(t, planStore, lineages)

	// Lineage says A is running; the host says C.
	lineages.set(lineageRow(lineageA))
	planStore.candidates = []store.PlanCandidate{digestPinnedCandidate(lineageC)}
	planStore.intel = map[string]domain.ImageIntel{trackingRef: registryAt(lineageB)}

	if _, err := planner.Generate(context.Background()); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got := len(planStore.plans()); got != 0 {
		t.Fatalf("a container whose running digest HarborMaster did not establish was "+
			"planned (%d plans)", got)
	}
}

// Registry evidence that did not complete is not "no update".
func TestATrackedContainerWithFailedRegistryEvidenceIsNotProposedAChange(t *testing.T) {
	lineages := newFakeLineageStore()
	planStore := newFakePlanStore()
	planner := lineagePlanner(t, planStore, lineages)

	stale := registryAt(lineageB)
	stale.Status = domain.CheckFailed

	lineages.set(lineageRow(lineageA))
	planStore.candidates = []store.PlanCandidate{digestPinnedCandidate(lineageA)}
	planStore.intel = map[string]domain.ImageIntel{trackingRef: stale}

	if _, err := planner.Generate(context.Background()); err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, plan := range planStore.plans() {
		if plan.ProposedDigest != "" {
			t.Fatalf("a change was proposed from registry evidence that did not complete: %+v", plan)
		}
	}
}

// An unreadable lineage table must degrade to the previous behaviour rather
// than stopping planning for the whole estate.
func TestAnUnreadableLineageTableDoesNotStopPlanning(t *testing.T) {
	lineages := newFakeLineageStore()
	lineages.err = context.DeadlineExceeded
	planStore := newFakePlanStore()
	planner := lineagePlanner(t, planStore, lineages)

	// A perfectly ordinary tag-referenced container, which the declared-
	// reference path has always handled.
	planStore.candidates = []store.PlanCandidate{{
		ContainerID:   strings.Repeat("2", 64),
		ContainerName: "plain",
		ImageRef:      "docker.io/library/app:1.0",
		ImageID:       lineageA,
		ImageDigest:   lineageA,
	}}
	intel := domain.ImageIntel{
		Reference: "docker.io/library/app:1.0", Familiar: "app:1.0",
		Repository: trackingRepo, Tag: "1.0",
		LocalDigest: lineageA, RemoteDigest: lineageB,
		Status: domain.CheckOK, Update: domain.UpdateDigest,
	}
	planStore.intel = map[string]domain.ImageIntel{"docker.io/library/app:1.0": intel}

	if _, err := planner.Generate(context.Background()); err != nil {
		t.Fatalf("generate returned an error when lineage was unreadable: %v", err)
	}
	if got := len(planStore.plans()); got != 1 {
		t.Fatalf("planning stopped when the lineage table was unreadable: %d plans", got)
	}
}
