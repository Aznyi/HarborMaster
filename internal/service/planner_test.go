package service_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Change planner tests.
//
// The properties under test:
//
//   - DETERMINISM. Two passes over an identical estate produce identical plans,
//     including the fingerprint that decides whether anything is written.
//   - NO N+1. A pass over ten thousand containers issues a bounded number of
//     queries, counted here rather than asserted about.
//   - UNCHANGED WORK IS NOT REDONE. A second pass over a settled estate writes
//     nothing.
//   - NOTHING IS APPLIED. The planner's store interface has no mutation on it
//     at all, and nothing below reaches Docker.

// ------------------------------------------------------------ fake store --

// fakePlanStore records what the planner asked for, so query COUNTS can be
// asserted rather than described.
type fakePlanStore struct {
	mu sync.Mutex

	candidates []store.PlanCandidate
	intel      map[string]domain.ImageIntel
	drift      map[string]store.SeverityRollup
	policy     map[string]store.SeverityRollup
	baselines  map[string]store.BaselineRollup
	// fingerprints is the stored plan digest per container, updated by
	// InsertPlans so a second pass sees what the first wrote.
	fingerprints map[string]string

	// inserted holds every plan written, in order.
	inserted []domain.ChangePlan

	// gatherCalls and candidateCalls count round trips. These are the N+1
	// guard: a per-container read would multiply them by the estate size.
	gatherCalls    atomic.Int64
	candidateCalls atomic.Int64

	// gatheredContainers and gatheredRefs record the batch sizes asked for.
	gatheredContainers []int
	gatheredRefs       []int

	insertErr error
}

func newFakePlanStore() *fakePlanStore {
	return &fakePlanStore{
		intel:        make(map[string]domain.ImageIntel),
		drift:        make(map[string]store.SeverityRollup),
		policy:       make(map[string]store.SeverityRollup),
		baselines:    make(map[string]store.BaselineRollup),
		fingerprints: make(map[string]string),
	}
}

func (f *fakePlanStore) Candidates(_ context.Context, offset, limit int) ([]store.PlanCandidate, error) {
	f.candidateCalls.Add(1)

	f.mu.Lock()
	defer f.mu.Unlock()

	if offset >= len(f.candidates) {
		return nil, nil
	}
	end := offset + limit
	if end > len(f.candidates) {
		end = len(f.candidates)
	}
	page := make([]store.PlanCandidate, end-offset)
	copy(page, f.candidates[offset:end])
	return page, nil
}

func (f *fakePlanStore) CountCandidates(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.candidates), nil
}

func (f *fakePlanStore) GatherInputs(
	_ context.Context,
	containerIDs, imageRefs []string,
) (store.PlanBatchInputs, error) {
	f.gatherCalls.Add(1)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.gatheredContainers = append(f.gatheredContainers, len(containerIDs))
	f.gatheredRefs = append(f.gatheredRefs, len(imageRefs))

	inputs := store.PlanBatchInputs{
		Drift:        make(map[string]store.SeverityRollup),
		Policy:       make(map[string]store.SeverityRollup),
		Baselines:    make(map[string]store.BaselineRollup),
		Intel:        make(map[string]domain.ImageIntel),
		Fingerprints: make(map[string]string),
	}
	for _, id := range containerIDs {
		if value, ok := f.drift[id]; ok {
			inputs.Drift[id] = value
		}
		if value, ok := f.policy[id]; ok {
			inputs.Policy[id] = value
		}
		if value, ok := f.baselines[id]; ok {
			inputs.Baselines[id] = value
		}
		if value, ok := f.fingerprints[id]; ok {
			inputs.Fingerprints[id] = value
		}
	}
	for _, reference := range imageRefs {
		if value, ok := f.intel[reference]; ok {
			inputs.Intel[reference] = value
		}
	}
	return inputs, nil
}

func (f *fakePlanStore) InsertPlans(
	_ context.Context,
	plans []domain.ChangePlan,
	_ time.Time,
) (store.InsertResult, error) {
	if f.insertErr != nil {
		return store.InsertResult{}, f.insertErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var result store.InsertResult
	for _, plan := range plans {
		// The unique index, in miniature: the same (container, digest) pair is
		// not written twice.
		if f.fingerprints[plan.ContainerID] == plan.InputDigest {
			result.Unchanged++
			continue
		}
		f.fingerprints[plan.ContainerID] = plan.InputDigest
		f.inserted = append(f.inserted, plan)
		result.Inserted++
	}
	return result, nil
}

func (f *fakePlanStore) PruneSuperseded(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func (f *fakePlanStore) PruneOrphans(context.Context, int) (int64, error) { return 0, nil }

func (f *fakePlanStore) plans() []domain.ChangePlan {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.ChangePlan, len(f.inserted))
	copy(out, f.inserted)
	return out
}

// ------------------------------------------------------------- fixtures --

func plannerConfig() config.Planner {
	return config.Planner{
		Enabled:           true,
		Interval:          time.Hour,
		BatchSize:         50,
		MaxContainers:     20000,
		GenerationTimeout: time.Minute,
		RetentionAge:      90 * 24 * time.Hour,
		PruneInterval:     time.Hour,
	}
}

// plannerAt builds a planner with a pinned clock, so every time-dependent rule
// is reproducible.
func plannerAt(store service.PlanStore, now time.Time) *service.PlannerService {
	return service.NewPlannerService(service.PlannerOptions{
		Store:  store,
		Config: plannerConfig(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return now },
	})
}

const plannerNowRFC = "2026-08-05T12:00:00Z"

func plannerNow(t *testing.T) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, plannerNowRFC)
	if err != nil {
		t.Fatalf("parse clock: %v", err)
	}
	return parsed
}

// Three distinct, WELL FORMED digests.
//
// Well formed because a proposal is now built by domain.NewProposedTarget,
// which refuses anything that is not a real manifest digest -- a fixture using
// "sha256:aaaa" would exercise the refusal path rather than the path under
// test. Distinct because the whole point of Phase 10.1 is that the current
// tag's digest and the proposed tag's digest are different values that must not
// be interchanged.
const (
	planLocalDigest  = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	planRemoteDigest = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	planLatestDigest = "sha256:" + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// updatableIntel is an image with a patch update on offer.
func updatableIntel(reference, familiar, tag string) domain.ImageIntel {
	published := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	return domain.ImageIntel{
		Reference:    reference,
		Familiar:     familiar,
		Kind:         domain.RegistryDockerHub,
		Registry:     "docker.io",
		Repository:   "library/nginx",
		Tag:          "1.27.0",
		LocalDigest:  planLocalDigest,
		RemoteDigest: planRemoteDigest,
		Platform:     domain.Platform{OS: "linux", Architecture: "amd64"},
		Update:       domain.UpdatePatch,
		LatestTag:    tag,
		// The newer tag's OWN digest, resolved with it. Without this the
		// planner proposes nothing, which is the correct refusal for a tag that
		// cannot be pinned.
		LatestDigest:   planLatestDigest,
		Status:         domain.CheckOK,
		PublishedAt:    &published,
		ContainerCount: 1,
	}
}

func candidate(id, name, reference string) store.PlanCandidate {
	return store.PlanCandidate{
		ContainerID:   id,
		ContainerName: name,
		ImageRef:      reference,
		ImageID:       "sha256:image1",
	}
}

// oneContainerEstate wires a single container onto an updatable image.
func oneContainerEstate() *fakePlanStore {
	fake := newFakePlanStore()
	fake.candidates = []store.PlanCandidate{candidate("container-a", "web", "nginx:1.27.0")}
	fake.intel["docker.io/library/nginx:1.27.0"] =
		updatableIntel("docker.io/library/nginx:1.27.0", "nginx:1.27.0", "1.27.1")
	return fake
}

// ----------------------------------------------------------- generation --

func TestGenerateProducesOnePlanPerChangedContainer(t *testing.T) {
	fake := oneContainerEstate()
	planner := plannerAt(fake, plannerNow(t))

	result, err := planner.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Generated != 1 || result.Skipped != 0 {
		t.Fatalf("result = %+v, want one plan", result)
	}

	plans := fake.plans()
	if len(plans) != 1 {
		t.Fatalf("stored %d plans, want 1", len(plans))
	}

	plan := plans[0]
	if plan.ContainerID != "container-a" || plan.ContainerName != "web" {
		t.Errorf("plan names the wrong container: %+v", plan)
	}
	if plan.CurrentImage != "nginx:1.27.0" || plan.ProposedImage != "nginx:1.27.1" {
		t.Errorf("proposed change = %q -> %q", plan.CurrentImage, plan.ProposedImage)
	}
	// The digest of the PROPOSED tag, not of the tag the container runs now.
	//
	// This assertion is the regression guard for Phase 10.1's headline defect:
	// the planner used to pair a newer tag with planRemoteDigest, which is the
	// digest resolved for the CURRENT tag.
	if plan.ProposedDigest != planLatestDigest {
		t.Errorf("proposed digest = %q, want the proposed tag's own digest %q",
			plan.ProposedDigest, planLatestDigest)
	}
	if plan.ProposedDigest == planRemoteDigest {
		t.Error("the proposed tag was paired with the CURRENT tag's digest")
	}
	if plan.UpdateType != domain.UpdatePatch {
		t.Errorf("update type = %q", plan.UpdateType)
	}
	if plan.PlannerVersion != domain.PlannerVersion || plan.PlanVersion != domain.PlanSchemaVersion {
		t.Errorf("plan does not record its versions: %+v", plan)
	}
	if plan.InputDigest == "" {
		t.Error("plan has no fingerprint, so it could never be suppressed")
	}
	if len(plan.Risk.Factors) == 0 || plan.Risk.Summary == "" {
		t.Errorf("plan carries no reasoning: %+v", plan.Risk)
	}
	if !plan.GeneratedAt.Equal(plannerNow(t)) {
		t.Errorf("generated at %s, want the pinned clock", plan.GeneratedAt)
	}
}

// A moved digest under an unchanged tag is reported as the SAME reference. The
// alternative would suggest editing something that does not need editing.
func TestADigestOnlyUpdateKeepsTheReference(t *testing.T) {
	fake := oneContainerEstate()
	intel := fake.intel["docker.io/library/nginx:1.27.0"]
	intel.Update = domain.UpdateDigest
	intel.LatestTag = ""
	fake.intel["docker.io/library/nginx:1.27.0"] = intel

	planner := plannerAt(fake, plannerNow(t))
	if _, err := planner.Generate(context.Background()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	plan := fake.plans()[0]
	if plan.ProposedImage != plan.CurrentImage {
		t.Errorf("a digest update should keep the reference: %q -> %q",
			plan.CurrentImage, plan.ProposedImage)
	}
	if plan.ProposedDigest == plan.CurrentDigest {
		t.Error("a digest update should move the digest")
	}
}

// A container with nothing on offer produces no plan. Writing "no change
// proposed" for every settled container would fill the table with non-events.
func TestASettledContainerProducesNoPlan(t *testing.T) {
	fake := oneContainerEstate()
	intel := fake.intel["docker.io/library/nginx:1.27.0"]
	intel.Update = domain.UpdateNone
	intel.LatestTag = ""
	fake.intel["docker.io/library/nginx:1.27.0"] = intel

	planner := plannerAt(fake, plannerNow(t))
	result, err := planner.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Generated != 0 || result.Skipped != 1 {
		t.Errorf("result = %+v, want one skip", result)
	}
}

// But a container whose registry evidence is MISSING is not settled. "We do not
// know" and "no update" are different states, and an operator must be able to
// tell them apart.
func TestAnUnassessableContainerStillGetsAPlan(t *testing.T) {
	fake := oneContainerEstate()
	intel := fake.intel["docker.io/library/nginx:1.27.0"]
	intel.Update = domain.UpdateNone
	intel.LatestTag = ""
	intel.Status = domain.CheckFailed
	fake.intel["docker.io/library/nginx:1.27.0"] = intel

	planner := plannerAt(fake, plannerNow(t))
	result, err := planner.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Generated != 1 {
		t.Fatalf("result = %+v, want a plan recording the gap", result)
	}
	if got := fake.plans()[0].Risk.Recommendation; got != domain.RecommendUnknown {
		t.Errorf("recommendation = %q, want unknown", got)
	}
}

// A container with no tracked intelligence at all has not been looked at yet.
func TestAnUntrackedImageIsSkipped(t *testing.T) {
	fake := newFakePlanStore()
	fake.candidates = []store.PlanCandidate{candidate("container-a", "web", "nginx:1.27.0")}

	planner := plannerAt(fake, plannerNow(t))
	result, err := planner.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Generated != 0 || result.Skipped != 1 {
		t.Errorf("result = %+v, want one skip", result)
	}
}

// A reference that cannot be normalised is skipped rather than planned against
// a lookup that could never have happened.
func TestAnUnnormalisableReferenceIsSkipped(t *testing.T) {
	fake := newFakePlanStore()
	fake.candidates = []store.PlanCandidate{
		candidate("container-a", "web", "NOT A REFERENCE:::"),
		candidate("container-b", "empty", ""),
	}

	planner := plannerAt(fake, plannerNow(t))
	result, err := planner.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Generated != 0 || result.Skipped != 2 {
		t.Errorf("result = %+v, want two skips", result)
	}
}

// The planner reads every source it claims to. Each of the six contributes to
// the plan, so a wiring mistake in any one of them is visible.
func TestAPlanCombinesEverySource(t *testing.T) {
	fake := oneContainerEstate()
	fake.drift["container-a"] = store.SeverityRollup{
		Open: 3, MaxSeverity: string(domain.DriftSeverityCritical),
	}
	fake.policy["container-a"] = store.SeverityRollup{
		Open: 2, MaxSeverity: string(domain.PolicySeverityHigh),
	}
	fake.baselines["container-a"] = store.BaselineRollup{
		SnapshotID: 42, Readiness: domain.ReadinessWarning,
	}

	planner := plannerAt(fake, plannerNow(t))
	if _, err := planner.Generate(context.Background()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	plan := fake.plans()[0]
	if plan.DriftOpen != 3 || plan.DriftMaxSeverity != domain.DriftSeverityCritical {
		t.Errorf("drift did not reach the plan: %+v", plan)
	}
	if plan.PolicyOpen != 2 || plan.PolicyMaxSeverity != domain.PolicySeverityHigh {
		t.Errorf("policy did not reach the plan: %+v", plan)
	}
	if plan.SnapshotID != 42 || !plan.SnapshotAvailable {
		t.Errorf("baseline did not reach the plan: %+v", plan)
	}
	if plan.RestoreReadiness != domain.ReadinessWarning {
		t.Errorf("readiness did not reach the plan: %q", plan.RestoreReadiness)
	}
	if plan.RegistryStatus != domain.CheckOK {
		t.Errorf("registry status did not reach the plan: %q", plan.RegistryStatus)
	}
}

// A container with no snapshot records unknown readiness rather than an empty
// string, which would fail the column's CHECK constraint.
func TestAContainerWithNoSnapshotRecordsUnknownReadiness(t *testing.T) {
	fake := oneContainerEstate()

	planner := plannerAt(fake, plannerNow(t))
	if _, err := planner.Generate(context.Background()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	plan := fake.plans()[0]
	if plan.SnapshotAvailable {
		t.Error("no snapshot was recorded, so none is available")
	}
	if plan.RestoreReadiness != domain.ReadinessUnknown {
		t.Errorf("readiness = %q, want unknown", plan.RestoreReadiness)
	}
}

// ---------------------------------------------------------- determinism --

// Two planners over identical estates produce identical plans, field for field
// apart from the random plan id. This is the property that makes a stored plan
// re-derivable.
func TestTwoPassesOverIdenticalEstatesAgree(t *testing.T) {
	now := plannerNow(t)

	run := func() []domain.ChangePlan {
		fake := newFakePlanStore()
		for index := 0; index < 20; index++ {
			id := fmt.Sprintf("container-%02d", index)
			reference := fmt.Sprintf("docker.io/library/app-%02d:1.0.0", index)
			fake.candidates = append(fake.candidates,
				candidate(id, "service-"+id, fmt.Sprintf("app-%02d:1.0.0", index)))
			fake.intel[reference] = updatableIntel(reference,
				fmt.Sprintf("app-%02d:1.0.0", index), "1.0.1")
			fake.drift[id] = store.SeverityRollup{
				Open: index % 3, MaxSeverity: string(domain.DriftSeverityMedium),
			}
		}

		planner := plannerAt(fake, now)
		if _, err := planner.Generate(context.Background()); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		return fake.plans()
	}

	first, second := run(), run()
	if len(first) != len(second) || len(first) == 0 {
		t.Fatalf("plan counts differ: %d and %d", len(first), len(second))
	}

	for index := range first {
		a, b := first[index], second[index]
		if a.PlanID == b.PlanID {
			t.Errorf("plan ids must be unguessable, not reproducible: %q", a.PlanID)
		}
		if a.InputDigest != b.InputDigest {
			t.Errorf("fingerprints differ for %s: %q and %q", a.ContainerID, a.InputDigest, b.InputDigest)
		}
		if a.Risk.Score != b.Risk.Score || a.Risk.Band != b.Risk.Band ||
			a.Risk.Recommendation != b.Risk.Recommendation || a.Risk.Summary != b.Risk.Summary {
			t.Errorf("assessments differ for %s:\n %+v\n %+v", a.ContainerID, a.Risk, b.Risk)
		}
		if len(a.Risk.Factors) != len(b.Risk.Factors) {
			t.Fatalf("factor counts differ for %s", a.ContainerID)
		}
		for factorIndex := range a.Risk.Factors {
			if a.Risk.Factors[factorIndex] != b.Risk.Factors[factorIndex] {
				t.Errorf("factor %d differs for %s: %+v and %+v", factorIndex, a.ContainerID,
					a.Risk.Factors[factorIndex], b.Risk.Factors[factorIndex])
			}
		}
	}
}

// One clock for the whole pass. Reading it per container would let two
// containers assessed a millisecond apart use different "now" values, which for
// the image-age rule means two identical estates could disagree.
func TestOneClockIsUsedForTheWholePass(t *testing.T) {
	fake := newFakePlanStore()
	for index := 0; index < 10; index++ {
		id := fmt.Sprintf("container-%02d", index)
		reference := fmt.Sprintf("docker.io/library/app-%02d:1.0.0", index)
		fake.candidates = append(fake.candidates,
			candidate(id, id, fmt.Sprintf("app-%02d:1.0.0", index)))
		fake.intel[reference] = updatableIntel(reference, fmt.Sprintf("app-%02d:1.0.0", index), "1.0.1")
	}

	// A clock that moves on every read. If the planner consulted it per
	// container, the generated timestamps would differ.
	var ticks atomic.Int64
	base := plannerNow(t)
	planner := service.NewPlannerService(service.PlannerOptions{
		Store:  fake,
		Config: plannerConfig(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time {
			return base.Add(time.Duration(ticks.Add(1)) * time.Hour)
		},
	})

	if _, err := planner.Generate(context.Background()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	plans := fake.plans()
	if len(plans) < 2 {
		t.Fatalf("expected several plans, got %d", len(plans))
	}
	for _, plan := range plans[1:] {
		if !plan.GeneratedAt.Equal(plans[0].GeneratedAt) {
			t.Fatalf("plans in one pass carry different timestamps: %s and %s",
				plans[0].GeneratedAt, plan.GeneratedAt)
		}
	}
}

// -------------------------------------------------- duplicate suppression --

// A second pass over an unchanged estate writes nothing. This is what makes it
// safe to trigger generation after every inventory refresh.
func TestASecondPassOverAnUnchangedEstateWritesNothing(t *testing.T) {
	fake := oneContainerEstate()
	planner := plannerAt(fake, plannerNow(t))
	ctx := context.Background()

	if _, err := planner.Generate(ctx); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	second, err := planner.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate (second): %v", err)
	}
	if second.Generated != 0 {
		t.Errorf("second pass generated %d plans, want none", second.Generated)
	}
	if second.Unchanged != 1 {
		t.Errorf("second pass = %+v, want one unchanged", second)
	}
	if len(fake.plans()) != 1 {
		t.Errorf("stored %d plans, want 1", len(fake.plans()))
	}
}

// A world that moved produces a new plan. The counterpart to the test above:
// suppression must not be so eager that a real change goes unrecorded.
func TestAChangedEstateProducesANewPlan(t *testing.T) {
	fake := oneContainerEstate()
	planner := plannerAt(fake, plannerNow(t))
	ctx := context.Background()

	if _, err := planner.Generate(ctx); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Drift appears. Nothing about the image changed.
	fake.drift["container-a"] = store.SeverityRollup{
		Open: 1, MaxSeverity: string(domain.DriftSeverityCritical),
	}

	second, err := planner.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate (second): %v", err)
	}
	if second.Generated != 1 {
		t.Fatalf("second pass = %+v, want a new plan", second)
	}

	plans := fake.plans()
	if plans[1].InputDigest == plans[0].InputDigest {
		t.Error("a changed world produced the same fingerprint")
	}
	if plans[1].Risk.Score <= plans[0].Risk.Score {
		t.Errorf("critical drift did not raise the score: %d then %d",
			plans[0].Risk.Score, plans[1].Risk.Score)
	}
}

// ------------------------------------------------------ scale and bounds --

// The N+1 guard, counted rather than asserted about: ten thousand containers in
// batches of five hundred must cost one gather per batch, not one per
// container.
func TestALargeEstateCostsOneGatherPerBatch(t *testing.T) {
	const (
		containers = 10000
		batch      = 500
	)

	fake := newFakePlanStore()
	for index := 0; index < containers; index++ {
		// Twenty distinct images across ten thousand containers, which is the
		// realistic shape: the reference set must be DEDUPED before the query.
		imageIndex := index % 20
		reference := fmt.Sprintf("docker.io/library/app-%02d:1.0.0", imageIndex)
		familiar := fmt.Sprintf("app-%02d:1.0.0", imageIndex)

		fake.candidates = append(fake.candidates,
			candidate(fmt.Sprintf("container-%05d", index), "service", familiar))
		fake.intel[reference] = updatableIntel(reference, familiar, "1.0.1")
	}

	cfg := plannerConfig()
	cfg.BatchSize = batch
	planner := service.NewPlannerService(service.PlannerOptions{
		Store:  fake,
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return plannerNow(t) },
	})

	started := time.Now()
	result, err := planner.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	elapsed := time.Since(started)

	if result.Generated != containers {
		t.Errorf("generated %d plans, want %d", result.Generated, containers)
	}
	if got := fake.gatherCalls.Load(); got != containers/batch {
		t.Errorf("gathered %d times, want %d -- one per batch", got, containers/batch)
	}
	if got := fake.candidateCalls.Load(); got > containers/batch+1 {
		t.Errorf("read candidates %d times, want about %d", got, containers/batch)
	}

	// Every batch asked for its containers at once, and for the DEDUPED
	// reference set rather than one reference per container.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for index, size := range fake.gatheredContainers {
		if size != batch {
			t.Errorf("batch %d gathered %d containers, want %d", index, size, batch)
		}
		if fake.gatheredRefs[index] > 20 {
			t.Errorf("batch %d asked for %d references, want at most the 20 distinct images",
				index, fake.gatheredRefs[index])
		}
	}

	t.Logf("planned %d containers in %s", containers, elapsed)
}

// MaxContainers caps the whole pass, so a pathologically large inventory
// produces a bounded amount of work rather than an unbounded one.
func TestAPassIsCappedByMaxContainers(t *testing.T) {
	fake := newFakePlanStore()
	for index := 0; index < 1000; index++ {
		reference := "docker.io/library/nginx:1.27.0"
		fake.candidates = append(fake.candidates,
			candidate(fmt.Sprintf("container-%04d", index), "web", "nginx:1.27.0"))
		fake.intel[reference] = updatableIntel(reference, "nginx:1.27.0", "1.27.1")
	}

	cfg := plannerConfig()
	cfg.BatchSize = 100
	cfg.MaxContainers = 250
	planner := service.NewPlannerService(service.PlannerOptions{
		Store:  fake,
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return plannerNow(t) },
	})

	result, err := planner.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Generated > 250 {
		t.Errorf("generated %d plans, want no more than the cap of 250", result.Generated)
	}
}

// A cancelled pass stops rather than running to completion against a dead
// context.
func TestACancelledPassStops(t *testing.T) {
	fake := newFakePlanStore()
	for index := 0; index < 500; index++ {
		reference := "docker.io/library/nginx:1.27.0"
		fake.candidates = append(fake.candidates,
			candidate(fmt.Sprintf("container-%04d", index), "web", "nginx:1.27.0"))
		fake.intel[reference] = updatableIntel(reference, "nginx:1.27.0", "1.27.1")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	planner := plannerAt(fake, plannerNow(t))
	if _, err := planner.Generate(ctx); err == nil {
		t.Error("a cancelled pass should report why it stopped")
	}
	if len(fake.plans()) != 0 {
		t.Errorf("a cancelled pass wrote %d plans", len(fake.plans()))
	}
}

// ---------------------------------------------------------- concurrency --

// Two passes must not overlap. The second would read the same containers,
// compute the same fingerprints, and race to insert rows the unique index would
// then reject -- work for no result.
func TestConcurrentGenerationDoesNotOverlap(t *testing.T) {
	fake := newFakePlanStore()
	for index := 0; index < 200; index++ {
		reference := "docker.io/library/nginx:1.27.0"
		fake.candidates = append(fake.candidates,
			candidate(fmt.Sprintf("container-%04d", index), "web", "nginx:1.27.0"))
		fake.intel[reference] = updatableIntel(reference, "nginx:1.27.0", "1.27.1")
	}

	planner := plannerAt(fake, plannerNow(t))

	const passes = 6
	var (
		wait      sync.WaitGroup
		mu        sync.Mutex
		generated int
	)
	wait.Add(passes)
	for pass := 0; pass < passes; pass++ {
		go func() {
			defer wait.Done()
			result, err := planner.Generate(context.Background())
			if err != nil {
				t.Errorf("Generate: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			generated += result.Generated
		}()
	}
	wait.Wait()

	// Exactly one pass did the work; the others were refused and returned an
	// empty result rather than duplicating it.
	if generated != 200 {
		t.Errorf("generated %d plans across %d concurrent passes, want 200", generated, passes)
	}
	if len(fake.plans()) != 200 {
		t.Errorf("stored %d plans, want 200", len(fake.plans()))
	}
}

// ------------------------------------------------------------- disabled --

func TestADisabledPlannerGeneratesNothing(t *testing.T) {
	fake := oneContainerEstate()

	cfg := plannerConfig()
	cfg.Enabled = false
	planner := service.NewPlannerService(service.PlannerOptions{
		Store:  fake,
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if planner.Enabled() {
		t.Error("a disabled planner reports itself enabled")
	}
	if _, err := planner.Generate(context.Background()); err == nil {
		t.Error("a disabled planner should refuse to generate")
	}
	if len(fake.plans()) != 0 {
		t.Errorf("a disabled planner wrote %d plans", len(fake.plans()))
	}

	// And the request path is inert rather than queueing work for a planner
	// that will never run it.
	planner.RequestGeneration()
	planner.InventoryRefreshed(1)
	if status := planner.Status(); status.Pending {
		t.Error("a disabled planner should not report pending work")
	}
}

// ---------------------------------------------------------------- worker --

// An inventory refresh asks for a pass, and the ask never blocks the refresh:
// the inventory service must not wait on a downstream consumer.
func TestAnInventoryRefreshRequestsAPassWithoutBlocking(t *testing.T) {
	fake := oneContainerEstate()
	planner := plannerAt(fake, plannerNow(t))

	done := make(chan struct{})
	go func() {
		defer close(done)
		// More notifications than the one-slot channel can hold. A blocking
		// send would deadlock here rather than dropping the redundant ones.
		for index := 0; index < 100; index++ {
			planner.InventoryRefreshed(int64(index))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("notifying the planner blocked the caller")
	}

	if !planner.Status().Pending {
		t.Error("a refresh should leave a pass owed")
	}
}

func TestTheWorkerGeneratesOnStartupAndStopsWithItsContext(t *testing.T) {
	fake := oneContainerEstate()

	cfg := plannerConfig()
	cfg.GenerateOnStartup = true
	cfg.Interval = time.Hour
	cfg.PruneInterval = time.Hour
	planner := service.NewPlannerService(service.PlannerOptions{
		Store:  fake,
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return plannerNow(t) },
	})

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		planner.Run(ctx)
	}()

	deadline := time.After(5 * time.Second)
	for len(fake.plans()) == 0 {
		select {
		case <-deadline:
			t.Fatal("the startup pass never ran")
		case <-time.After(5 * time.Millisecond):
		}
	}

	status := planner.Status()
	if !status.Enabled || status.PlannerVersion != domain.PlannerVersion {
		t.Errorf("status = %+v", status)
	}
	if status.LastRunAt == nil || status.Generated != 1 {
		t.Errorf("status does not describe the pass: %+v", status)
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not stop with its context")
	}
}

// A disabled planner's worker returns immediately rather than idling on a
// ticker it will never act on.
func TestADisabledWorkerReturnsImmediately(t *testing.T) {
	cfg := plannerConfig()
	cfg.Enabled = false
	planner := service.NewPlannerService(service.PlannerOptions{
		Store:  newFakePlanStore(),
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		planner.Run(context.Background())
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("a disabled worker should return rather than idle")
	}
}

// ------------------------------------------------------------ read-only --

// The planner's store interface is the WHOLE of what the planner can reach, so
// its method set is the capability. Asserted by name rather than by behaviour:
// a method that applied a plan, pulled an image, or recreated a container would
// have to appear in this set first, and this test is what makes that visible.
func TestThePlannerCapabilityIsReadAndAppendOnly(t *testing.T) {
	capability := reflect.TypeOf((*service.PlanStore)(nil)).Elem()

	// The exact surface. A new method fails this test rather than slipping in.
	expected := map[string]bool{
		"Candidates":      true,
		"CountCandidates": true,
		"GatherInputs":    true,
		"InsertPlans":     true,
		"PruneSuperseded": true,
		"PruneOrphans":    true,
	}

	found := make(map[string]bool, capability.NumMethod())
	for index := 0; index < capability.NumMethod(); index++ {
		name := capability.Method(index).Name
		found[name] = true
		if !expected[name] {
			t.Errorf("PlanStore gained the method %q; the planner's capability widened", name)
		}
	}
	for name := range expected {
		if !found[name] {
			t.Errorf("PlanStore lost the method %q", name)
		}
	}

	// And nothing on it is named for an action HarborMaster cannot perform.
	// A blunt check, deliberately: the point is that the vocabulary of this
	// interface stays read-and-append.
	for name := range found {
		for _, forbidden := range []string{
			"Apply", "Execute", "Pull", "Recreate", "Restore", "Rollback",
			"Update", "Schedule", "Approve",
		} {
			if strings.Contains(name, forbidden) {
				t.Errorf("PlanStore.%s suggests a capability HarborMaster does not have", name)
			}
		}
	}
}
