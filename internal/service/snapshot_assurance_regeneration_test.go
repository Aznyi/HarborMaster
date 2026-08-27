package service_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Stage 17.2's headline acceptance, without a Docker daemon.
//
// # The sequence
//
//	a container with no snapshot and an update on offer
//	    -> planner pass 1: assurance captures the baseline FIRST, then plans
//	    -> the plan that pass writes CONTAINS the baseline
//	    -> acquisition accepts it
//
// and the thing that makes it correct rather than convenient:
//
//	a plan written BEFORE the baseline existed stays unusable, and is never
//	mutated.
//
// Every layer here is real: store.PlanRepository, store.SnapshotRepository,
// service.SnapshotService, service.SnapshotAssurance, service.SnapshotPreparer,
// service.PlannerService. The only doubles are the container detail the capture
// reads and the acquisition service's evidence, neither of which is what these
// assertions are about.

const (
	regenContainerID = "container-regen-0001"
	regenName        = "regen-web"
	regenImage       = "docker.io/library/nginx:1.27.0"
	regenDigestNow   = "sha256:" + "cc33333333333333333333333333333333333333333333333333333333333333"
	regenDigestNext  = "sha256:" + "dd44444444444444444444444444444444444444444444444444444444444444"
)

// regenEstate builds a database holding one present container with an update on
// offer and NO snapshot.
func regenEstate(t *testing.T) *store.DB {
	t.Helper()

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hm.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	now := time.Now().UTC()

	detail := domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			HostID:        domain.LocalHostID,
			ID:            regenContainerID,
			ShortID:       domain.ShortenID(regenContainerID),
			Name:          regenName,
			Image:         domain.ParseImageRef(regenImage),
			ImageID:       "sha256:image-regen",
			State:         domain.StateRunning,
			Health:        domain.HealthHealthy,
			CreatedAt:     now.Add(-time.Hour),
			RestartPolicy: domain.RestartPolicy{Name: "unless-stopped"},
			Present:       true,
		},
		State:       domain.StateDetail{State: domain.StateRunning, RawState: "running"},
		Labels:      []domain.Label{{Key: "tier", Value: "front", Source: domain.LabelSourceUser}},
		Environment: []domain.EnvVar{},
		Mounts:      []domain.Mount{},
		Networks:    []domain.NetworkAttachment{},
		Warnings:    []domain.InventoryWarning{},
	}

	if _, err := db.Inventory.CommitRefresh(ctx, store.RefreshCommit{
		Host: domain.Host{ID: domain.LocalHostID, Name: "local", Runtime: domain.RuntimeDocker},
		Containers: []store.ContainerRecord{{
			Detail:  detail,
			RawJSON: []byte(`{"Id":"` + regenContainerID + `"}`),
		}},
		Record: domain.RefreshRecord{
			Trigger:          domain.TriggerManual,
			StartedAt:        now,
			ContainersListed: 1,
			Checksum:         "regen-checksum",
		},
		Now: now,
	}); err != nil {
		t.Fatalf("commit inventory: %v", err)
	}

	// Registry intelligence offering a patch update, so the planner has
	// something to assess. Without it every pass reports "nothing to plan" and
	// the acceptance would prove nothing.
	if _, err := db.ImageIntel.SyncReferences(ctx, []store.ImageReferenceSeed{{
		Reference:      regenImage,
		Familiar:       "nginx:1.27.0",
		Kind:           domain.RegistryDockerHub,
		Registry:       "docker.io",
		Namespace:      "library",
		Repository:     "library/nginx",
		Tag:            "1.27.0",
		LocalDigest:    regenDigestNow,
		ContainerCount: 1,
		Supported:      true,
	}}, now); err != nil {
		t.Fatalf("seed image intel: %v", err)
	}
	if err := db.ImageIntel.RecordCheck(ctx, store.CheckOutcome{
		Reference:    regenImage,
		Status:       domain.CheckOK,
		RemoteDigest: regenDigestNow,
		Update:       domain.UpdatePatch,
		LatestTag:    "1.27.1",
		LatestDigest: regenDigestNext,
	}, now); err != nil {
		t.Fatalf("record intel check: %v", err)
	}

	return db
}

// regenPlanner assembles the real planner over the real store, with assurance
// wired exactly as the composition root wires it.
func regenPlanner(t *testing.T, db *store.DB, detail domain.ContainerDetail, policies service.AutomationPolicyStore) *service.PlannerService {
	t.Helper()

	snapshots := service.NewSnapshotService(service.SnapshotOptions{
		Containers: &staticContainers{detail: &detail},
		Snapshots:  db.Snapshots,
		Hasher:     regenHasher(t),
		Versions:   service.HostVersions{HarborMaster: "0.9.0"},
		Config:     config.Snapshots{Enabled: true},
	})
	assurance := service.NewSnapshotAssurance(service.SnapshotAssuranceOptions{
		Capturer: snapshots,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	preparer := service.NewSnapshotPreparer(service.SnapshotPreparerOptions{
		Assurance: assurance,
		Policies:  policies,
		Targets:   db.Containers,
		Baselines: db.Snapshots,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	return service.NewPlannerService(service.PlannerOptions{
		Store:   db.Plans,
		Lineage: db.Lineage,
		Prepare: preparer,
		Config: config.Planner{
			Enabled: true, BatchSize: 100, MaxContainers: 1000,
			GenerationTimeout: time.Minute,
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func regenHasher(t *testing.T) *service.Hasher {
	t.Helper()
	key, err := service.LoadSecretKey(service.SecretKeyOptions{
		GeneratePath: filepath.Join(t.TempDir(), "secret.key"),
	})
	if err != nil {
		t.Fatalf("load secret key: %v", err)
	}
	return service.NewHasher(key)
}

// staticContainers serves one container detail to the capture path.
type staticContainers struct{ detail *domain.ContainerDetail }

func (s *staticContainers) Get(context.Context, string) (*domain.ContainerDetail, error) {
	return s.detail, nil
}

func (s *staticContainers) ResolveID(_ context.Context, reference string) (string, error) {
	return reference, nil
}

// governingPolicies is a policy store holding one broad automatic policy, which
// is what makes the container one the preparer will prepare.
type governingPolicies struct{ policies []domain.UpdatePolicy }

func (p *governingPolicies) ActivePolicies(context.Context) ([]domain.UpdatePolicy, error) {
	return p.policies, nil
}

func (p *governingPolicies) CountUpdatePolicies(context.Context) (int, int, error) {
	return len(p.policies), len(p.policies), nil
}

func regenBroadPolicy() *governingPolicies {
	policy := domain.UpdatePolicy{
		PolicyID:              domain.NewUpdatePolicyID(),
		Name:                  "keep everything current",
		Enabled:               true,
		Scope:                 domain.ScopeAllEligible,
		Strategy:              domain.StrategyPatch,
		MinimumRecommendation: domain.RecommendCaution,
		Mode:                  domain.ModeAutomatic,
		Window:                domain.MaintenanceWindow{AlwaysOpen: true},
	}
	policy.Normalise()
	return &governingPolicies{policies: []domain.UpdatePolicy{policy}}
}

// ------------------------------------------------------------ the test --

// TestAPlanGeneratedAfterAssuranceCarriesTheBaseline is §16 of the brief.
func TestAPlanGeneratedAfterAssuranceCarriesTheBaseline(t *testing.T) {
	db := regenEstate(t)
	ctx := context.Background()

	stored, err := db.Containers.Get(ctx, regenContainerID)
	if err != nil {
		t.Fatalf("read container: %v", err)
	}
	detail := *stored

	// ---- the world before -------------------------------------------------

	baselines, err := db.Snapshots.BaselineIDs(ctx)
	if err != nil {
		t.Fatalf("baselines: %v", err)
	}
	if _, exists := baselines[regenContainerID]; exists {
		t.Fatal("fixture defect: the container must start with no snapshot")
	}

	// ---- P1: a plan written while there was no baseline --------------------
	//
	// Generated by a planner with NO preparer, which is exactly the
	// pre-Phase-17 build.
	withoutPreparation := service.NewPlannerService(service.PlannerOptions{
		Store:   db.Plans,
		Lineage: db.Lineage,
		Config: config.Planner{
			Enabled: true, BatchSize: 100, MaxContainers: 1000,
			GenerationTimeout: time.Minute,
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if _, err := withoutPreparation.Generate(ctx); err != nil {
		t.Fatalf("generate P1: %v", err)
	}

	first, err := db.Plans.Current(ctx, regenContainerID)
	if err != nil {
		t.Fatalf("read P1: %v", err)
	}
	if first.SnapshotAvailable {
		t.Fatal("fixture defect: P1 must have been assessed with no baseline")
	}
	if first.ProposedImage == "" {
		t.Fatal("fixture defect: P1 proposes no change, so the acceptance would prove nothing")
	}

	// P1 is not acquirable, and that is the CURRENT behaviour rather than
	// something this stage introduces.
	assertAcquisitionRefused(t, first)

	// ---- pass 2: assurance, then planning ---------------------------------

	planner := regenPlanner(t, db, detail, regenBroadPolicy())
	if _, err := planner.Generate(ctx); err != nil {
		t.Fatalf("generate P2: %v", err)
	}

	// A baseline now exists.
	baselines, err = db.Snapshots.BaselineIDs(ctx)
	if err != nil {
		t.Fatalf("baselines after preparation: %v", err)
	}
	captured := baselines[regenContainerID]
	if captured == 0 {
		t.Fatal("preparation captured no baseline for a governed container")
	}

	// ---- P2 contains it ----------------------------------------------------

	second, err := db.Plans.Current(ctx, regenContainerID)
	if err != nil {
		t.Fatalf("read P2: %v", err)
	}
	if second.PlanID == first.PlanID {
		t.Fatal("no new plan was written after the baseline was captured; " +
			"the update would stay unacquirable for ever")
	}
	if !second.SnapshotAvailable {
		t.Error("P2 does not report a snapshot, so preparation ran too late to be read")
	}
	if second.SnapshotID != captured {
		t.Errorf("P2 names baseline %d, want the captured %d", second.SnapshotID, captured)
	}
	if second.InputDigest == first.InputDigest {
		t.Error("P1 and P2 share a fingerprint; the baseline did not reach the assessment")
	}

	// ---- P1 is untouched ---------------------------------------------------

	reread, err := db.Plans.Get(ctx, first.PlanID)
	if err != nil {
		t.Fatalf("re-read P1: %v", err)
	}
	if reread.SnapshotAvailable {
		t.Error("P1 was mutated: it now reports a snapshot it was never assessed against")
	}
	if reread.SnapshotID != 0 {
		t.Errorf("P1 adopted baseline %d", reread.SnapshotID)
	}
	if reread.InputDigest != first.InputDigest {
		t.Error("P1's fingerprint moved; an immutable plan was rewritten")
	}
	if !reread.Superseded {
		t.Error("P1 should read as superseded once P2 exists")
	}

	// And P1 is STILL unacquirable, after the baseline exists. This is the
	// invariant in one assertion: a snapshot captured later does not make an
	// older plan executable.
	assertAcquisitionRefused(t, reread)

	// ---- P2 is acquirable --------------------------------------------------

	assertAcquisitionAccepted(t, second)
}

// TestPreparationIsIdempotentAcrossPasses.
//
// The planner runs after every committed inventory refresh, so preparation runs
// often. It must cost nothing once the estate has baselines.
func TestPreparationIsIdempotentAcrossPasses(t *testing.T) {
	db := regenEstate(t)
	ctx := context.Background()

	got, err := db.Containers.Get(ctx, regenContainerID)
	if err != nil {
		t.Fatalf("read container: %v", err)
	}
	planner := regenPlanner(t, db, *got, regenBroadPolicy())

	for pass := 0; pass < 5; pass++ {
		if _, err := planner.Generate(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}

	var rows int
	if err := db.SQL().QueryRow(
		`SELECT COUNT(*) FROM snapshots WHERE container_id = ?`, regenContainerID).Scan(&rows); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d snapshot rows after five planner passes, want 1", rows)
	}

	var plans int
	if err := db.SQL().QueryRow(
		`SELECT COUNT(*) FROM change_plans WHERE container_id = ?`, regenContainerID).Scan(&plans); err != nil {
		t.Fatalf("count plans: %v", err)
	}
	if plans != 1 {
		t.Errorf("%d plans after five passes over an unchanged world, want 1", plans)
	}
}

// TestPreparationCapturesNothingWithoutAPolicy.
//
// The early exit that keeps assurance free on a deployment that has not asked
// for automation. A container nothing governs is a container HarborMaster has
// no reason to snapshot on a timer.
func TestPreparationCapturesNothingWithoutAPolicy(t *testing.T) {
	db := regenEstate(t)
	ctx := context.Background()

	got, err := db.Containers.Get(ctx, regenContainerID)
	if err != nil {
		t.Fatalf("read container: %v", err)
	}
	planner := regenPlanner(t, db, *got, &governingPolicies{})

	if _, err := planner.Generate(ctx); err != nil {
		t.Fatalf("generate: %v", err)
	}

	baselines, err := db.Snapshots.BaselineIDs(ctx)
	if err != nil {
		t.Fatalf("baselines: %v", err)
	}
	if id, exists := baselines[regenContainerID]; exists {
		t.Errorf("a container no policy governs was snapshotted (id %d)", id)
	}
}

// ------------------------------------------------------- acquisition --

// assertAcquisitionRefused drives the REAL acquisition preflight over a plan.
func assertAcquisitionRefused(t *testing.T, plan domain.ChangePlan) {
	t.Helper()

	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		h.evidence.plan = adaptPlanForAcquisition(plan)
		h.evidence.current = h.evidence.plan
	})
	_, err := harness.service.Request(t.Context(),
		service.AcquisitionRequest{PlanID: h(plan)})
	if err == nil {
		t.Fatalf("plan %s was acquirable; it carries no snapshot evidence", plan.PlanID)
	}
	if refusal := refusalFrom(t, err); refusal != domain.AcquisitionRefusalRestoreReadiness {
		t.Errorf("refusal = %q, want %q", refusal, domain.AcquisitionRefusalRestoreReadiness)
	}
	if harness.acquirer.Calls != 0 {
		t.Errorf("%d pulls for a refused plan, want 0", harness.acquirer.Calls)
	}
}

func assertAcquisitionAccepted(t *testing.T, plan domain.ChangePlan) {
	t.Helper()

	harness := newAcquisitionHarness(t, func(h *acquisitionHarness) {
		h.evidence.plan = adaptPlanForAcquisition(plan)
		h.evidence.current = h.evidence.plan
	})
	if _, err := harness.service.Request(t.Context(),
		service.AcquisitionRequest{PlanID: h(plan)}); err != nil {
		t.Fatalf("plan %s carries a baseline and was still refused: %v", plan.PlanID, err)
	}
}

// adaptPlanForAcquisition keeps the SNAPSHOT EVIDENCE from the generated plan
// and takes everything else from the acquisition harness's healthy fixture.
//
// The acquisition preflight checks registry freshness, digest agreement, and
// platform against doubles this test does not model; reproducing all of them
// would be testing the acquisition service rather than the invariant. What is
// carried across is exactly the three fields under test.
func adaptPlanForAcquisition(generated domain.ChangePlan) domain.ChangePlan {
	plan := healthyEvidence(time.Now().UTC()).plan
	plan.SnapshotID = generated.SnapshotID
	plan.SnapshotAvailable = generated.SnapshotAvailable
	plan.RestoreReadiness = generated.RestoreReadiness
	return plan
}

// h returns the plan id the acquisition harness is keyed on.
//
// The harness resolves one fixed id; the generated plan's own id is not what is
// under test here, and threading it through would mean rebuilding the harness's
// entire evidence graph.
func h(domain.ChangePlan) string { return acqPlanID }
