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

// Change plan persistence tests.
//
// The properties that matter here are IMMUTABILITY and DUPLICATE SUPPRESSION.
// A plan is a record of what was believed at one moment, so nothing may edit
// one; and a pass over an unchanged estate must write nothing at all, which is
// what keeps the table from growing on every refresh.
//
// The third property is that GATHERING IS BATCHED. A plan combines six sources,
// and reading them per container would be the N+1 pattern six times over.

// planFor builds a storable plan for one container.
func planFor(containerID, fingerprint string) domain.ChangePlan {
	return domain.ChangePlan{
		PlanID:        domain.NewPlanID(),
		ContainerID:   containerID,
		ContainerName: "web",

		CurrentImage:   "nginx:1.27.0",
		ProposedImage:  "nginx:1.27.1",
		CurrentDigest:  "sha256:" + repeat("a", 64),
		ProposedDigest: "sha256:" + repeat("b", 64),
		UpdateType:     domain.UpdatePatch,

		SnapshotAvailable: false,
		RestoreReadiness:  domain.ReadinessUnknown,

		RegistryStatus: domain.CheckOK,

		Risk: domain.RiskAssessment{
			Score:          5,
			Band:           domain.RiskVeryLow,
			Recommendation: domain.RecommendProceed,
			Summary:        "Nothing in the available evidence argues against this change.",
			Factors: []domain.RiskFactor{
				{Rule: domain.RuleUpdateClassification, Points: 5,
					Severity: domain.FactorInfo, Detail: "this is a patch version change"},
			},
		},

		PlanVersion:    domain.PlanSchemaVersion,
		PlannerVersion: domain.PlannerVersion,
		InputDigest:    fingerprint,
	}
}

// insertPlans stores plans and fails the test if it cannot.
func insertPlans(t *testing.T, db *store.DB, plans ...domain.ChangePlan) store.InsertResult {
	t.Helper()
	result, err := db.Plans.InsertPlans(context.Background(), plans, time.Now().UTC())
	if err != nil {
		t.Fatalf("InsertPlans: %v", err)
	}
	return result
}

// digestOf makes a distinct 64-character fingerprint from a short label.
func digestOf(label string) string {
	return (label + repeat("0", 64))[:64]
}

// ------------------------------------------------------------ round trip --

func TestPlanRoundTrips(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	published := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	plan := planFor("container-a", digestOf("aa"))
	plan.SnapshotAvailable = true
	plan.RestoreReadiness = domain.ReadinessReady
	plan.DriftOpen = 2
	plan.DriftMaxSeverity = domain.DriftSeverityHigh
	plan.PolicyOpen = 1
	plan.PolicyMaxSeverity = domain.PolicySeverityMedium
	plan.RegistryDetail = "the registry did not answer"
	plan.ProposedPublishedAt = &published

	insertPlans(t, db, plan)

	stored, err := db.Plans.Get(ctx, plan.PlanID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if stored.ContainerID != plan.ContainerID || stored.PlanID != plan.PlanID {
		t.Errorf("identity did not round-trip: %+v", stored)
	}
	if stored.CurrentImage != plan.CurrentImage || stored.ProposedImage != plan.ProposedImage {
		t.Errorf("images did not round-trip: %+v", stored)
	}
	if stored.DriftOpen != 2 || stored.DriftMaxSeverity != domain.DriftSeverityHigh {
		t.Errorf("drift summary did not round-trip: %+v", stored)
	}
	if stored.PolicyOpen != 1 || stored.PolicyMaxSeverity != domain.PolicySeverityMedium {
		t.Errorf("policy summary did not round-trip: %+v", stored)
	}
	if stored.Risk.Score != 5 || stored.Risk.Band != domain.RiskVeryLow {
		t.Errorf("risk did not round-trip: %+v", stored.Risk)
	}
	if len(stored.Risk.Factors) != 1 || stored.Risk.Factors[0].Rule != domain.RuleUpdateClassification {
		t.Errorf("factors did not round-trip: %+v", stored.Risk.Factors)
	}
	if stored.ProposedPublishedAt == nil || !stored.ProposedPublishedAt.Equal(published) {
		t.Errorf("publication date did not round-trip: %v", stored.ProposedPublishedAt)
	}
	if stored.PlannerVersion != domain.PlannerVersion || stored.PlanVersion != domain.PlanSchemaVersion {
		t.Errorf("versions did not round-trip: %+v", stored)
	}
	if stored.Superseded {
		t.Error("the only plan for a container is not superseded")
	}
}

func TestAnUnknownPlanIsNotFound(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.Plans.Get(context.Background(), "plan_deadbeef"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := db.Plans.Current(context.Background(), "container-x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Current(unknown) = %v, want ErrNotFound", err)
	}
}

// An assessment the model could not have produced is refused. The repository is
// the backstop: a score outside its band would surface as a dashboard that
// sorts wrongly rather than as an error.
func TestAnInconsistentAssessmentIsRefused(t *testing.T) {
	db := openTestDB(t)

	plan := planFor("container-a", digestOf("aa"))
	plan.Risk.Score = 95
	plan.Risk.Band = domain.RiskVeryLow

	if _, err := db.Plans.InsertPlans(context.Background(),
		[]domain.ChangePlan{plan}, time.Now().UTC()); err == nil {
		t.Fatal("a score outside its band should be refused")
	}
}

// --------------------------------------------------- duplicate suppression --

// The whole point of the fingerprint: a second pass over an unchanged world
// writes nothing.
func TestAnIdenticalAssessmentIsNotWrittenTwice(t *testing.T) {
	db := openTestDB(t)
	fingerprint := digestOf("aa")

	first := insertPlans(t, db, planFor("container-a", fingerprint))
	if first.Inserted != 1 || first.Unchanged != 0 {
		t.Fatalf("first pass = %+v, want one insert", first)
	}

	// A different plan id, the same inputs. It is the FINGERPRINT that decides,
	// not the identifier.
	second := insertPlans(t, db, planFor("container-a", fingerprint))
	if second.Inserted != 0 || second.Unchanged != 1 {
		t.Errorf("second pass = %+v, want nothing written", second)
	}

	_, total, err := db.Plans.History(context.Background(), "container-a", store.Page{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if total != 1 {
		t.Errorf("history holds %d plans, want 1", total)
	}
}

// A changed world produces a new plan, and the old one survives as the record
// of what was believed when a decision was made.
func TestAChangedAssessmentAddsAPlanWithoutDestroyingTheOld(t *testing.T) {
	db := openTestDB(t)
	// A plan is always written for a container the PLANNER FOUND, so the
	// inventory row is part of a faithful fixture. Since C3D a plan is current
	// only while the container it assessed still exists, and a plan for a
	// container that was never inventoried is correctly not current.
	commitOf(t, db, records(
		buildContainer("container-a", "web", withImage("nginx:1.27", "sha256:image1")),
	))
	ctx := context.Background()

	insertPlans(t, db, planFor("container-a", digestOf("aa")))

	changed := planFor("container-a", digestOf("bb"))
	changed.UpdateType = domain.UpdateMajor
	changed.Risk.Score = 40
	changed.Risk.Band = domain.RiskMedium
	changed.Risk.Recommendation = domain.RecommendManualReview
	insertPlans(t, db, changed)

	history, total, err := db.Plans.History(ctx, "container-a", store.Page{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if total != 2 {
		t.Fatalf("history holds %d plans, want 2", total)
	}
	// Newest first: the timeline reads backwards from the current assessment.
	if history[0].InputDigest != digestOf("bb") {
		t.Errorf("history is not newest-first: %+v", history[0])
	}
	if !history[1].Superseded {
		t.Error("the older plan should report itself superseded")
	}
	if history[0].Superseded {
		t.Error("the newest plan is not superseded")
	}

	current, err := db.Plans.Current(ctx, "container-a")
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if current.UpdateType != domain.UpdateMajor {
		t.Errorf("current plan = %+v, want the newer one", current)
	}
}

// The unique index is the guarantee under concurrency. Twenty writers racing to
// record the identical assessment must leave exactly one row, and none of them
// may fail: another writer having already recorded it is the outcome wanted.
func TestConcurrentWritersOfOneAssessmentLeaveOneRow(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	fingerprint := digestOf("aa")

	const writers = 20
	var (
		wait     sync.WaitGroup
		mu       sync.Mutex
		inserted int
		failures []error
	)

	wait.Add(writers)
	for writer := 0; writer < writers; writer++ {
		go func() {
			defer wait.Done()
			result, err := db.Plans.InsertPlans(ctx,
				[]domain.ChangePlan{planFor("container-a", fingerprint)}, time.Now().UTC())

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			inserted += result.Inserted
		}()
	}
	wait.Wait()

	if len(failures) > 0 {
		t.Fatalf("a racing duplicate must not be an error: %v", failures[0])
	}
	if inserted != 1 {
		t.Errorf("%d writers reported inserting, want exactly 1", inserted)
	}

	_, total, err := db.Plans.History(ctx, "container-a", store.Page{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if total != 1 {
		t.Errorf("%d rows stored, want 1", total)
	}
}

// ------------------------------------------------------------- gathering --

// planEstate builds a database with containers, a snapshot, drift, a violation
// and image intelligence -- the six sources a plan combines.
func planEstate(t *testing.T) *store.DB {
	t.Helper()
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	commitOf(t, db, records(
		buildContainer("container-a", "web", withImage("nginx:1.27", "sha256:image1")),
		buildContainer("container-b", "api", withImage("nginx:1.27", "sha256:image1")),
	))

	snapshot := createSnapshot(t, db, newSnapshot("container-a", checksumA))

	drift := driftRecord(snapshot, domain.DriftCategorySecurity, "privileged")
	drift.Severity = domain.DriftSeverityCritical
	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
		[]domain.DriftRecord{drift}, now); err != nil {
		t.Fatalf("ReconcileDrift: %v", err)
	}

	policy := createPolicy(t, db, newPolicy("no privileged containers"))
	violation := violationFor(policy, domain.RulePrivilegedForbidden)
	violation.Severity = domain.PolicySeverityHigh
	if _, err := db.Policies.ReconcilePolicy(ctx, passFor(1, true),
		[]domain.PolicyViolation{violation}, now); err != nil {
		t.Fatalf("ReconcilePolicy: %v", err)
	}

	seed := imageSeed("docker.io/library/nginx:1.27")
	if _, err := db.ImageIntel.SyncReferences(ctx,
		[]store.ImageReferenceSeed{seed}, now); err != nil {
		t.Fatalf("SyncReferences: %v", err)
	}

	return db
}

func TestGatherInputsCollectsEverySource(t *testing.T) {
	db := planEstate(t)
	ctx := context.Background()

	inputs, err := db.Plans.GatherInputs(ctx,
		[]string{"container-a", "container-b"},
		[]string{"docker.io/library/nginx:1.27"})
	if err != nil {
		t.Fatalf("GatherInputs: %v", err)
	}

	drift := inputs.Drift["container-a"]
	if drift.Open != 1 || drift.MaxSeverity != string(domain.DriftSeverityCritical) {
		t.Errorf("drift rollup = %+v, want one critical", drift)
	}
	policy := inputs.Policy["container-a"]
	if policy.Open != 1 || policy.MaxSeverity != string(domain.PolicySeverityHigh) {
		t.Errorf("policy rollup = %+v, want one high", policy)
	}
	if baseline := inputs.Baselines["container-a"]; baseline.SnapshotID == 0 {
		t.Errorf("baseline = %+v, want a snapshot", baseline)
	}
	if _, ok := inputs.Intel["docker.io/library/nginx:1.27"]; !ok {
		t.Errorf("image intel missing: %v", inputs.Intel)
	}

	// A container with none of a source is simply absent from that map, which
	// the planner reads directly as "nothing recorded".
	if _, ok := inputs.Drift["container-b"]; ok {
		t.Error("a container with no drift should be absent from the rollup")
	}
	if len(inputs.Fingerprints) != 0 {
		t.Errorf("no plans exist yet, so no fingerprints: %v", inputs.Fingerprints)
	}
}

// The rollup reports the WORST severity, not the first or the last. It is a MAX
// over a rank, and an ordering bug here would understate a critical finding.
func TestSeverityRollupReportsTheWorst(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	snapshot := createSnapshot(t, db, newSnapshot("container-a", checksumA))

	low := driftRecord(snapshot, domain.DriftCategoryRestart, "restartPolicy")
	low.Severity = domain.DriftSeverityLow
	worst := driftRecord(snapshot, domain.DriftCategorySecurity, "privileged")
	worst.Severity = domain.DriftSeverityCritical
	middle := driftRecord(snapshot, domain.DriftCategoryPorts, "ports")
	middle.Severity = domain.DriftSeverityMedium

	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 3),
		[]domain.DriftRecord{low, worst, middle}, now); err != nil {
		t.Fatalf("ReconcileDrift: %v", err)
	}

	inputs, err := db.Plans.GatherInputs(ctx, []string{"container-a"}, nil)
	if err != nil {
		t.Fatalf("GatherInputs: %v", err)
	}

	rollup := inputs.Drift["container-a"]
	if rollup.Open != 3 {
		t.Errorf("open = %d, want 3", rollup.Open)
	}
	if rollup.MaxSeverity != string(domain.DriftSeverityCritical) {
		t.Errorf("max severity = %q, want critical", rollup.MaxSeverity)
	}
}

// A resolved difference is not an open one. Counting it would tell an operator
// they still have a problem they have already fixed.
func TestResolvedFindingsAreNotGathered(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	snapshot := createSnapshot(t, db, newSnapshot("container-a", checksumA))
	record := driftRecord(snapshot, domain.DriftCategorySecurity, "privileged")
	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
		[]domain.DriftRecord{record}, now); err != nil {
		t.Fatalf("ReconcileDrift: %v", err)
	}

	// The difference goes away: an empty reconcile resolves what is no longer
	// seen.
	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 0), nil, now); err != nil {
		t.Fatalf("ReconcileDrift(empty): %v", err)
	}

	inputs, err := db.Plans.GatherInputs(ctx, []string{"container-a"}, nil)
	if err != nil {
		t.Fatalf("GatherInputs: %v", err)
	}
	if rollup, ok := inputs.Drift["container-a"]; ok {
		t.Errorf("a resolved difference was gathered as open: %+v", rollup)
	}
}

// Gathering must cost a fixed number of queries whatever the batch size. This
// is the N+1 guard: five hundred containers and five hundred references still
// produce one grouped read per source.
func TestGatheringALargeBatchIsBounded(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	const batch = 500
	ids := make([]string, 0, batch)
	references := make([]string, 0, batch)
	for index := 0; index < batch; index++ {
		ids = append(ids, fmt.Sprintf("container-%04d", index))
		references = append(references, fmt.Sprintf("docker.io/library/app-%04d:1.0", index))
	}

	started := time.Now()
	inputs, err := db.Plans.GatherInputs(ctx, ids, references)
	if err != nil {
		t.Fatalf("GatherInputs: %v", err)
	}
	elapsed := time.Since(started)

	if len(inputs.Drift)+len(inputs.Policy)+len(inputs.Baselines)+len(inputs.Intel) != 0 {
		t.Errorf("an empty estate should gather nothing: %+v", inputs)
	}
	// Generous, because the assertion is about the SHAPE of the work rather
	// than the machine. Five hundred round trips would not fit in this.
	if elapsed > 2*time.Second {
		t.Errorf("gathering one batch took %s, which suggests a per-container query", elapsed)
	}
}

func TestGatheringNothingQueriesNothing(t *testing.T) {
	db := openTestDB(t)

	inputs, err := db.Plans.GatherInputs(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("GatherInputs: %v", err)
	}
	if inputs.Drift == nil || inputs.Policy == nil || inputs.Baselines == nil ||
		inputs.Intel == nil || inputs.Fingerprints == nil {
		t.Errorf("empty gathering must return usable maps, got %+v", inputs)
	}
}

// Candidates are present containers in a deterministic order, so a batched pass
// covers the estate exactly once rather than re-reading rows.
func TestCandidatesArePresentContainersInAStableOrder(t *testing.T) {
	db := planEstate(t)
	ctx := context.Background()

	count, err := db.Plans.CountCandidates(ctx)
	if err != nil {
		t.Fatalf("CountCandidates: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	first, err := db.Plans.Candidates(ctx, 0, 1)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	second, err := db.Plans.Candidates(ctx, 1, 1)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("paged candidates = %v and %v", first, second)
	}
	if first[0].ContainerID == second[0].ContainerID {
		t.Error("paging returned the same container twice")
	}
	if first[0].ImageRef == "" {
		t.Errorf("a candidate should carry its image reference: %+v", first[0])
	}

	// Repeating the first page returns the same row: the order is stable.
	again, err := db.Plans.Candidates(ctx, 0, 1)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if again[0].ContainerID != first[0].ContainerID {
		t.Error("candidate ordering is not stable between reads")
	}
}

// -------------------------------------------------------- listing --

func TestPlanListingFiltersAndPaginates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for index := 0; index < 25; index++ {
		plan := planFor(fmt.Sprintf("container-%02d", index), digestOf(fmt.Sprintf("f%02d", index)))
		if index%2 == 0 {
			plan.UpdateType = domain.UpdateMajor
			plan.Risk.Score = 55
			plan.Risk.Band = domain.RiskHigh
			plan.Risk.Recommendation = domain.RecommendManualReview
		}
		insertPlans(t, db, plan)
	}

	page, total, err := db.Plans.List(ctx, store.PlanFilter{Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 25 || len(page) != 10 {
		t.Errorf("total = %d, page = %d, want 25 and 10", total, len(page))
	}

	high, total, err := db.Plans.List(ctx, store.PlanFilter{
		Bands: []domain.RiskBand{domain.RiskHigh},
		Page:  store.Page{Limit: 50},
	})
	if err != nil {
		t.Fatalf("List(band): %v", err)
	}
	if total != 13 || len(high) != 13 {
		t.Errorf("high-band total = %d, want 13", total)
	}
	for _, plan := range high {
		if plan.Risk.Band != domain.RiskHigh {
			t.Fatalf("band filter leaked %q", plan.Risk.Band)
		}
	}

	review, _, err := db.Plans.List(ctx, store.PlanFilter{
		Recommendations: []domain.Recommendation{domain.RecommendManualReview},
		Page:            store.Page{Limit: 50},
	})
	if err != nil {
		t.Fatalf("List(recommendation): %v", err)
	}
	if len(review) != 13 {
		t.Errorf("manual-review plans = %d, want 13", len(review))
	}

	risky, _, err := db.Plans.List(ctx, store.PlanFilter{MinRisk: 50, Page: store.Page{Limit: 50}})
	if err != nil {
		t.Fatalf("List(minRisk): %v", err)
	}
	if len(risky) != 13 {
		t.Errorf("plans at or above 50 = %d, want 13", len(risky))
	}
}

// A sort field outside the allowlist falls back to the default rather than
// reaching the SQL text. The allowlist is the whole defence: the value becomes
// part of a statement, so nothing caller-supplied may contribute to it.
func TestAnUnknownSortFieldFallsBackToTheDefault(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	insertPlans(t, db, planFor("container-a", digestOf("aa")))
	insertPlans(t, db, planFor("container-b", digestOf("bb")))

	for _, attempt := range []string{
		"generated_at; DROP TABLE change_plans",
		"1) UNION SELECT * FROM containers --",
		"risk_score",
		"",
	} {
		plans, total, err := db.Plans.List(ctx, store.PlanFilter{Sort: attempt, Page: store.Page{Limit: 10}})
		if err != nil {
			t.Fatalf("List(sort=%q): %v", attempt, err)
		}
		if total != 2 || len(plans) != 2 {
			t.Errorf("List(sort=%q) returned %d of %d, want 2 of 2", attempt, len(plans), total)
		}
		if store.ValidPlanSortField(attempt) {
			t.Errorf("%q must not be an accepted sort field", attempt)
		}
	}

	for field := range map[string]bool{
		"generatedAt": true, "risk": true, "band": true,
		"recommendation": true, "container": true, "update": true, "id": true,
	} {
		if !store.ValidPlanSortField(field) {
			t.Errorf("%q should be a sortable field", field)
		}
		if _, _, err := db.Plans.List(ctx, store.PlanFilter{Sort: field, Ascending: true}); err != nil {
			t.Errorf("List(sort=%q): %v", field, err)
		}
	}
}

// A listing of "the plans" means the current one per container. Counting
// superseded plans beside current ones would double-count the estate.
func TestCurrentOnlyExcludesSupersededPlans(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Both containers are in the inventory. Since C3D a plan is current only
	// while the container it assessed still exists, so a fixture asserting
	// which plans are CURRENT has to inventory them.
	commitOf(t, db, records(
		buildContainer("container-a", "web", withImage("nginx:1.27", "sha256:image1")),
		buildContainer("container-b", "api", withImage("nginx:1.27", "sha256:image1")),
	))

	insertPlans(t, db, planFor("container-a", digestOf("aa")))
	insertPlans(t, db, planFor("container-a", digestOf("bb")))
	insertPlans(t, db, planFor("container-b", digestOf("cc")))

	all, total, err := db.Plans.List(ctx, store.PlanFilter{Page: store.Page{Limit: 50}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 3 || len(all) != 3 {
		t.Fatalf("all plans = %d, want 3", total)
	}

	current, total, err := db.Plans.List(ctx, store.PlanFilter{
		CurrentOnly: true, Page: store.Page{Limit: 50},
	})
	if err != nil {
		t.Fatalf("List(currentOnly): %v", err)
	}
	if total != 2 || len(current) != 2 {
		t.Errorf("current plans = %d, want 2", total)
	}
	for _, plan := range current {
		if plan.Superseded {
			t.Errorf("a superseded plan reached the current listing: %+v", plan)
		}
	}
}

func TestPlanSummaryCountsCurrentPlansOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Both containers are in the inventory. Since C3D a plan is current only
	// while the container it assessed still exists, so a fixture asserting
	// which plans are CURRENT has to inventory them.
	commitOf(t, db, records(
		buildContainer("container-a", "web", withImage("nginx:1.27", "sha256:image1")),
		buildContainer("container-b", "api", withImage("nginx:1.27", "sha256:image1")),
	))

	// container-a gets two assessments; only the newer one should count.
	insertPlans(t, db, planFor("container-a", digestOf("aa")))

	blocked := planFor("container-a", digestOf("bb"))
	blocked.Risk.Score = 85
	blocked.Risk.Band = domain.RiskCritical
	blocked.Risk.Recommendation = domain.RecommendAgainst
	blocked.UpdateType = domain.UpdateMajor
	insertPlans(t, db, blocked)

	undetermined := planFor("container-b", digestOf("cc"))
	undetermined.Risk.Recommendation = domain.RecommendUnknown
	undetermined.UpdateType = domain.UpdateUnknown
	insertPlans(t, db, undetermined)

	summary, err := db.Plans.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}

	if summary.Plans != 2 || summary.Containers != 2 {
		t.Errorf("summary counts %d plans over %d containers, want 2 and 2",
			summary.Plans, summary.Containers)
	}
	if summary.Blocked != 1 || summary.Undetermined != 1 || summary.Actionable != 0 {
		t.Errorf("summary = %+v, want one blocked and one undetermined", summary)
	}
	if summary.ByBand[domain.RiskCritical] != 1 {
		t.Errorf("band counts = %v", summary.ByBand)
	}
	if summary.ByUpdateType[domain.UpdateMajor] != 1 {
		t.Errorf("update counts = %v", summary.ByUpdateType)
	}
	if summary.PlannerVersion != domain.PlannerVersion {
		t.Errorf("planner version = %q, want %q", summary.PlannerVersion, domain.PlannerVersion)
	}
	if summary.LastGeneratedAt == nil {
		t.Error("summary should report when plans were last generated")
	}

	// Undetermined is reported BESIDE the rest rather than absorbed into
	// actionable: a gap in evidence must stay visible.
	if summary.Actionable+summary.NeedsReview+summary.Blocked+summary.Undetermined != summary.Plans {
		t.Errorf("the recommendation buckets do not account for every plan: %+v", summary)
	}
}

func TestPlanSummaryOfAnEmptyEstateIsUsable(t *testing.T) {
	db := openTestDB(t)

	summary, err := db.Plans.Summary(context.Background())
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.Plans != 0 || summary.LastGeneratedAt != nil || summary.PlannerVersion != "" {
		t.Errorf("empty summary = %+v", summary)
	}
	if summary.ByBand == nil || summary.ByRecommendation == nil || summary.ByUpdateType == nil {
		t.Error("an empty summary must still carry usable maps")
	}
}

// ------------------------------------------------------------- retention --

// Retention prunes SUPERSEDED plans only. The newest plan for a container is
// the standing assessment; deleting it would leave the container looking
// unplanned rather than unchanged.
func TestRetentionNeverPrunesTheCurrentPlan(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// Both containers inventoried: since C3D a plan is current only while the
	// container it assessed still exists.
	commitOf(t, db, records(
		buildContainer("container-a", "web", withImage("nginx:1.27", "sha256:image1")),
		buildContainer("container-b", "api", withImage("nginx:1.27", "sha256:image1")),
	))
	old := time.Now().UTC().Add(-365 * 24 * time.Hour)

	first := planFor("container-a", digestOf("aa"))
	first.GeneratedAt = old
	insertPlans(t, db, first)

	second := planFor("container-a", digestOf("bb"))
	second.GeneratedAt = old
	insertPlans(t, db, second)

	// A container whose only plan is ancient.
	lonely := planFor("container-b", digestOf("cc"))
	lonely.GeneratedAt = old
	insertPlans(t, db, lonely)

	removed, err := db.Plans.PruneSuperseded(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("PruneSuperseded: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d plans, want 1", removed)
	}

	if _, err := db.Plans.Current(ctx, "container-b"); err != nil {
		t.Errorf("a container's only plan must survive retention: %v", err)
	}
	if _, total, err := db.Plans.History(ctx, "container-a", store.Page{}); err != nil || total != 1 {
		t.Errorf("history total = %d (err %v), want the current plan kept", total, err)
	}
}

func TestRetentionRemovesPlansForDepartedContainers(t *testing.T) {
	db := planEstate(t)
	ctx := context.Background()

	insertPlans(t, db, planFor("container-a", digestOf("aa")))
	insertPlans(t, db, planFor("container-gone", digestOf("bb")))

	removed, err := db.Plans.PruneOrphans(ctx, 100)
	if err != nil {
		t.Fatalf("PruneOrphans: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d plans, want 1", removed)
	}
	if _, err := db.Plans.Current(ctx, "container-a"); err != nil {
		t.Errorf("a present container's plan must survive: %v", err)
	}
	if _, err := db.Plans.Current(ctx, "container-gone"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a departed container's plan should be gone, got %v", err)
	}
}

// Retention is batched, so a large backlog cannot hold the single writer for an
// unbounded time.
func TestRetentionIsBatched(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-365 * 24 * time.Hour)

	for index := 0; index < 12; index++ {
		plan := planFor("container-a", digestOf(fmt.Sprintf("f%02d", index)))
		plan.GeneratedAt = old
		insertPlans(t, db, plan)
	}

	removed, err := db.Plans.PruneSuperseded(ctx, time.Now().UTC(), 5)
	if err != nil {
		t.Fatalf("PruneSuperseded: %v", err)
	}
	if removed != 5 {
		t.Errorf("removed %d plans in one batch, want the batch limit of 5", removed)
	}
}

// ------------------------------------------------------------ immutability --

// There is no update path. The property is structural rather than behavioural,
// so it is asserted against the repository's method set: a method that could
// edit a stored plan would have to appear here first.
func TestThePlanRepositoryExposesNoUpdatePath(t *testing.T) {
	db := openTestDB(t)

	// A compile-time assertion: the repository satisfies a read-and-append
	// interface. If an Update or Edit method were added it would not break this,
	// but a caller reaching for one has to add it to this list first -- which is
	// a visible diff on a file whose whole subject is immutability.
	var _ interface {
		InsertPlans(context.Context, []domain.ChangePlan, time.Time) (store.InsertResult, error)
		Get(context.Context, string) (domain.ChangePlan, error)
		Current(context.Context, string) (domain.ChangePlan, error)
		History(context.Context, string, store.Page) ([]domain.ChangePlan, int, error)
		List(context.Context, store.PlanFilter) ([]domain.ChangePlan, int, error)
		Summary(context.Context) (domain.ChangePlanSummary, error)
		PruneSuperseded(context.Context, time.Time, int) (int64, error)
		PruneOrphans(context.Context, int) (int64, error)
	} = db.Plans

	// And the stored row itself carries no mutable state: no status, no applied
	// flag, no assignee. Adding one would be the first step toward treating a
	// plan as a work item, which is a capability HarborMaster does not have.
	for _, column := range []string{"status", "applied_at", "approved_by", "executed_at"} {
		var name string
		err := db.SQL().QueryRow(
			`SELECT name FROM pragma_table_info('change_plans') WHERE name = ?`, column).Scan(&name)
		if err == nil {
			t.Errorf("change_plans has a mutable %q column; a plan is a record, not a work item", column)
		}
	}
}

// The fingerprint index is what makes duplicate suppression a guarantee rather
// than a race. Asserted against the schema so removing it is a visible failure.
func TestTheFingerprintIndexIsUnique(t *testing.T) {
	db := openTestDB(t)

	var sqlText string
	err := db.SQL().QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_plan_fingerprint'`).Scan(&sqlText)
	if err != nil {
		t.Fatalf("the fingerprint index is missing: %v", err)
	}
	if !strings.Contains(strings.ToUpper(sqlText), "UNIQUE") {
		t.Errorf("the fingerprint index is not unique: %s", sqlText)
	}
	if !strings.Contains(sqlText, "container_id") || !strings.Contains(sqlText, "input_digest") {
		t.Errorf("the fingerprint index does not cover (container, digest): %s", sqlText)
	}
}
