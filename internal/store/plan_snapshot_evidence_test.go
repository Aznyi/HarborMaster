package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Stage 17.2 reproduction: what a snapshot captured AFTER a plan does to that
// plan.
//
// # The invariant these tests exist to pin
//
//	The snapshot/readiness evidence used to authorize an update must be part of
//	the immutable evidence of the ChangePlan that is executed.
//
// Automatic snapshot assurance is the feature that makes "keep my estate
// updated" work without an operator snapshotting every container by hand. The
// tempting implementation is to capture a snapshot at the moment the pipeline
// needs one and let the existing plan proceed. That is precisely the thing
// these tests forbid: a plan whose risk assessment was computed against "no
// baseline exists" would then be executed as though it had been computed
// against a baseline that did exist, and the assessment an operator reads would
// not be the assessment that authorized the change.
//
// So the three cases below establish, against the REAL store, what the
// persistence layer already guarantees and where the guarantee has to be
// enforced by code above it.
//
// These are written before the assurance service exists, deliberately. They
// describe current behaviour; whichever of them would fail after Stage 17.2 is
// the one that would have been a regression.

const (
	evidenceDigestA = "sha256:" + "aa11111111111111111111111111111111111111111111111111111111111111"
	evidenceDigestB = "sha256:" + "bb22222222222222222222222222222222222222222222222222222222222222"
)

// evidenceSnapshot builds a snapshot row for one container whose spec document
// is derived from marker, so two different markers checksum differently and one
// marker checksums identically however many times it is captured.
func evidenceSnapshot(containerID, marker string, at time.Time) domain.Snapshot {
	spec := []byte(`{"specVersion":1,"identity":{"containerId":"` +
		containerID + `"},"marker":"` + marker + `"}`)
	sum := sha256.Sum256(spec)

	return domain.Snapshot{
		ContainerID:     containerID,
		ContainerName:   "web",
		ImageReference:  "nginx:1.27.0",
		ImageDigest:     evidenceDigestA,
		SpecVersion:     domain.SnapshotSpecVersion,
		SpecJSON:        spec,
		Checksum:        hex.EncodeToString(sum[:]),
		Trigger:         domain.SnapshotTriggerAPI,
		ReadinessStatus: domain.ReadinessReady,
		CreatedAt:       at,
	}
}

// evidencePlan builds a plan carrying exactly the snapshot evidence given.
//
// The fingerprint is computed by the SAME domain function the planner uses, so
// a plan written here is indistinguishable from one the planner would write for
// the same world -- which is what makes "the fingerprint moved" meaningful
// rather than an artefact of the fixture.
func evidencePlan(containerID string, snapshotID int64, readiness domain.ReadinessStatus, at time.Time) domain.ChangePlan {
	inputs := domain.PlanInputs{
		ContainerID:      containerID,
		ContainerName:    "web",
		CurrentImage:     "nginx:1.27.0",
		ProposedImage:    "nginx:1.27.1",
		CurrentDigest:    evidenceDigestA,
		ProposedDigest:   evidenceDigestB,
		CurrentTag:       "1.27.0",
		UpdateType:       domain.UpdatePatch,
		SnapshotID:       snapshotID,
		RestoreReadiness: readiness,
		RegistryStatus:   domain.CheckOK,
		EvaluatedAt:      at,
	}

	return domain.ChangePlan{
		PlanID:            domain.NewPlanID(),
		ContainerID:       containerID,
		ContainerName:     "web",
		CurrentImage:      "nginx:1.27.0",
		ProposedImage:     "nginx:1.27.1",
		CurrentDigest:     evidenceDigestA,
		ProposedDigest:    evidenceDigestB,
		UpdateType:        domain.UpdatePatch,
		SnapshotID:        snapshotID,
		SnapshotAvailable: snapshotID != 0,
		RestoreReadiness:  readiness,
		RegistryStatus:    domain.CheckOK,
		Risk: domain.RiskAssessment{
			Score:          10,
			Band:           domain.RiskLow,
			Recommendation: domain.RecommendProceed,
			Summary:        "a patch update",
			Factors:        []domain.RiskFactor{},
		},
		PlanVersion:    domain.PlanSchemaVersion,
		PlannerVersion: domain.PlannerVersion,
		InputDigest:    inputs.Fingerprint(),
		GeneratedAt:    at,
	}
}

// TestCaseA_SnapshotCapturedAfterAPlanDoesNotEnterThatPlan is the reproduction
// the whole stage turns on.
//
// A plan is written while the container has no baseline. A snapshot is then
// captured. The stored plan must still say what it said: no snapshot, readiness
// unknown. If this ever fails, something has made an immutable plan mutable and
// the evidence an operator reviewed is no longer the evidence that authorizes.
func TestCaseA_SnapshotCapturedAfterAPlanDoesNotEnterThatPlan(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)

	// 1 and 2. A plan generated with no baseline at all.
	planned := evidencePlan("container-a", 0, domain.ReadinessUnknown, now)
	if _, err := db.Plans.InsertPlans(ctx, []domain.ChangePlan{planned}, now); err != nil {
		t.Fatalf("insert plan: %v", err)
	}

	before, err := db.Plans.Current(ctx, "container-a")
	if err != nil {
		t.Fatalf("read current plan: %v", err)
	}
	if before.SnapshotAvailable {
		t.Fatalf("fixture defect: the plan under test must start with no snapshot evidence")
	}

	// 3. Assurance captures a snapshot.
	captured, err := db.Snapshots.Create(ctx,
		evidenceSnapshot("container-a", "original", now.Add(time.Minute)), nil, nil, nil)
	if err != nil {
		t.Fatalf("capture snapshot: %v", err)
	}
	if captured.ID == 0 {
		t.Fatalf("fixture defect: capture returned no snapshot id")
	}

	// 4. The plan is re-read exactly as the acquisition preflight would.
	after, err := db.Plans.Current(ctx, "container-a")
	if err != nil {
		t.Fatalf("re-read current plan: %v", err)
	}

	if after.PlanID != before.PlanID {
		t.Fatalf("capturing a snapshot changed which plan is current: %q -> %q",
			before.PlanID, after.PlanID)
	}
	if after.SnapshotAvailable {
		t.Error("the plan reports a snapshot it was never assessed against; " +
			"an immutable plan has been mutated by a later capture")
	}
	if after.SnapshotID != 0 {
		t.Errorf("snapshotId = %d, want 0; the plan must not adopt a later baseline", after.SnapshotID)
	}
	if after.RestoreReadiness != before.RestoreReadiness {
		t.Errorf("restoreReadiness = %q, want %q; readiness evidence must not move under a stored plan",
			after.RestoreReadiness, before.RestoreReadiness)
	}
	if after.InputDigest != before.InputDigest {
		t.Error("the plan's input digest moved without a new plan being written")
	}

	// And the fact that makes the refusal correct rather than merely
	// conservative: the world genuinely has a baseline now, and the plan
	// genuinely does not know about it.
	baselines, err := db.Snapshots.BaselineIDs(ctx)
	if err != nil {
		t.Fatalf("baseline ids: %v", err)
	}
	if baselines["container-a"] != captured.ID {
		t.Fatalf("baseline = %d, want the captured snapshot %d",
			baselines["container-a"], captured.ID)
	}
	if after.SnapshotID == baselines["container-a"] {
		t.Error("the plan's baseline and the live baseline agree; " +
			"this case is meant to reproduce them disagreeing")
	}
}

// TestCaseB_ASnapshotTakenAfterAPlanDivergesFromIt covers the more dangerous
// shape: the plan DID have a baseline, and the container has since changed.
//
// The plan is not wrong about anything it recorded -- S1 existed and was ready.
// It is wrong about the world, and the divergence is detectable without
// reinterpreting the plan: the plan's snapshot id and the live baseline differ.
func TestCaseB_ASnapshotTakenAfterAPlanDivergesFromIt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)

	// 1. S1.
	first, err := db.Snapshots.Create(ctx,
		evidenceSnapshot("container-b", "original", now), nil, nil, nil)
	if err != nil {
		t.Fatalf("capture S1: %v", err)
	}

	// 2. A plan assessed against S1.
	planned := evidencePlan("container-b", first.ID, domain.ReadinessReady, now.Add(time.Minute))
	if _, err := db.Plans.InsertPlans(ctx, []domain.ChangePlan{planned}, now); err != nil {
		t.Fatalf("insert plan: %v", err)
	}

	// 3 and 4. The container's configuration changes, so the next capture is a
	// different document and a different checksum.
	second, err := db.Snapshots.Create(ctx,
		evidenceSnapshot("container-b", "reconfigured", now.Add(2*time.Minute)), nil, nil, nil)
	if err != nil {
		t.Fatalf("capture S2: %v", err)
	}
	if second.Deduplicated {
		t.Fatal("fixture defect: a changed configuration must not deduplicate")
	}
	if second.ID == first.ID {
		t.Fatal("fixture defect: S2 must be a distinct row")
	}

	// 5. What the execution path would see.
	current, err := db.Plans.Current(ctx, "container-b")
	if err != nil {
		t.Fatalf("read current plan: %v", err)
	}
	if current.SnapshotID != first.ID {
		t.Errorf("the stored plan's baseline moved from %d to %d", first.ID, current.SnapshotID)
	}

	baselines, err := db.Snapshots.BaselineIDs(ctx)
	if err != nil {
		t.Fatalf("baseline ids: %v", err)
	}
	live := baselines["container-b"]
	if live != second.ID {
		t.Fatalf("live baseline = %d, want S2 %d", live, second.ID)
	}

	// The property Stage 17.2 must enforce above this layer. The store makes
	// the divergence VISIBLE; it cannot make anybody check it.
	if current.SnapshotID == live {
		t.Error("plan baseline and live baseline agree after a configuration change; " +
			"the staleness this case exists to detect would be invisible")
	}
}

// TestCaseC_UnchangedConfigurationDeduplicatesAndLeavesThePlanValid.
//
// The settled case, and the one that must stay cheap: re-capturing an unchanged
// container produces no new row, so the plan's baseline is still the live
// baseline and nothing needs replanning.
func TestCaseC_UnchangedConfigurationDeduplicatesAndLeavesThePlanValid(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)

	first, err := db.Snapshots.Create(ctx,
		evidenceSnapshot("container-c", "original", now), nil, nil, nil)
	if err != nil {
		t.Fatalf("capture S1: %v", err)
	}

	planned := evidencePlan("container-c", first.ID, domain.ReadinessReady, now.Add(time.Minute))
	if _, err := db.Plans.InsertPlans(ctx, []domain.ChangePlan{planned}, now); err != nil {
		t.Fatalf("insert plan: %v", err)
	}

	// Re-capture the SAME configuration, several times. A later CreatedAt is
	// supplied deliberately: dedup must key on the document, not on the clock.
	for attempt := 0; attempt < 4; attempt++ {
		again, err := db.Snapshots.Create(ctx,
			evidenceSnapshot("container-c", "original",
				now.Add(time.Duration(attempt+2)*time.Minute)), nil, nil, nil)
		if err != nil {
			t.Fatalf("re-capture %d: %v", attempt, err)
		}
		if !again.Deduplicated {
			t.Errorf("re-capture %d inserted a new row; unchanged configuration must deduplicate", attempt)
		}
		if again.ID != first.ID {
			t.Errorf("re-capture %d returned snapshot %d, want the existing %d", attempt, again.ID, first.ID)
		}
	}

	// Count the rows rather than trusting the reported outcome.
	var rows int
	if err := db.SQL().QueryRow(
		`SELECT COUNT(*) FROM snapshots WHERE container_id = ?`, "container-c").Scan(&rows); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d snapshot rows for an unchanged container, want 1", rows)
	}

	current, err := db.Plans.Current(ctx, "container-c")
	if err != nil {
		t.Fatalf("read current plan: %v", err)
	}
	baselines, err := db.Snapshots.BaselineIDs(ctx)
	if err != nil {
		t.Fatalf("baseline ids: %v", err)
	}
	if current.SnapshotID != baselines["container-c"] {
		t.Errorf("plan baseline %d and live baseline %d disagree after a no-op capture",
			current.SnapshotID, baselines["container-c"])
	}
}

// TestAPlanIsNeverUpdatedInPlace is the structural half of the invariant.
//
// Re-inserting the identical assessment is reported as unchanged and writes
// nothing; re-inserting a DIFFERENT assessment writes a second row and leaves
// the first exactly as it was. Together those are what "insert-only" means, and
// they are what lets an approval or an execution bind to a plan id.
func TestAPlanIsNeverUpdatedInPlace(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)

	// Two real snapshot rows. change_plans.snapshot_id carries a FOREIGN KEY
	// onto snapshots, so a plan can only ever name a baseline that exists --
	// which is itself part of why a plan's snapshot evidence is trustworthy.
	snapOne, err := db.Snapshots.Create(ctx,
		evidenceSnapshot("container-d", "original", now), nil, nil, nil)
	if err != nil {
		t.Fatalf("capture S1: %v", err)
	}
	snapTwo, err := db.Snapshots.Create(ctx,
		evidenceSnapshot("container-d", "reconfigured", now.Add(time.Minute)), nil, nil, nil)
	if err != nil {
		t.Fatalf("capture S2: %v", err)
	}

	first := evidencePlan("container-d", snapOne.ID, domain.ReadinessReady, now)
	if _, err := db.Plans.InsertPlans(ctx, []domain.ChangePlan{first}, now); err != nil {
		t.Fatalf("insert first: %v", err)
	}

	// The same assessment again, under a different plan id. The fingerprint is
	// what identifies it, so this must write nothing.
	repeat := first
	repeat.PlanID = domain.NewPlanID()
	result, err := db.Plans.InsertPlans(ctx, []domain.ChangePlan{repeat}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("insert repeat: %v", err)
	}
	if result.Inserted != 0 || result.Unchanged != 1 {
		t.Errorf("inserted=%d unchanged=%d, want 0/1 for an identical assessment",
			result.Inserted, result.Unchanged)
	}

	// A different baseline is a different assessment, so it is a new row.
	moved := evidencePlan("container-d", snapTwo.ID, domain.ReadinessReady, now.Add(2*time.Minute))
	if moved.InputDigest == first.InputDigest {
		t.Fatal("fixture defect: a changed snapshot id must change the fingerprint")
	}
	if _, err := db.Plans.InsertPlans(ctx, []domain.ChangePlan{moved}, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("insert moved: %v", err)
	}

	original, err := db.Plans.Get(ctx, first.PlanID)
	if err != nil {
		t.Fatalf("re-read the original plan: %v", err)
	}
	if original.SnapshotID != snapOne.ID {
		t.Errorf("the original plan's baseline is now %d, want %d", original.SnapshotID, snapOne.ID)
	}
	if !original.Superseded {
		t.Error("the original plan should read as superseded once a newer one exists")
	}

	var stored int
	if err := db.SQL().QueryRow(
		`SELECT COUNT(*) FROM change_plans WHERE container_id = ?`, "container-d").Scan(&stored); err != nil {
		t.Fatalf("count plans: %v", err)
	}
	if stored != 2 {
		t.Errorf("%d plan rows, want 2 (one per distinct assessment)", stored)
	}
}

// TestTheBaselineRollupThePlannerReadsSeesANewSnapshotImmediately.
//
// The other half of the design: a snapshot captured BEFORE a planner pass is
// visible to that pass through the same grouped read the planner uses. This is
// what makes "assure, then plan" produce a plan that contains the evidence,
// rather than one that needs a second pass to notice.
func TestTheBaselineRollupThePlannerReadsSeesANewSnapshotImmediately(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)

	inputs, err := db.Plans.GatherInputs(ctx, []string{"container-e"}, nil)
	if err != nil {
		t.Fatalf("gather before: %v", err)
	}
	if _, found := inputs.Baselines["container-e"]; found {
		t.Fatal("fixture defect: the container must start with no baseline")
	}

	captured, err := db.Snapshots.Create(ctx,
		evidenceSnapshot("container-e", "original", now), nil, nil, nil)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	inputs, err = db.Plans.GatherInputs(ctx, []string{"container-e"}, nil)
	if err != nil {
		t.Fatalf("gather after: %v", err)
	}
	rollup, found := inputs.Baselines["container-e"]
	if !found {
		t.Fatal("the planner's baseline read does not see a snapshot captured moments earlier")
	}
	if rollup.SnapshotID != captured.ID {
		t.Errorf("planner sees baseline %d, want %d", rollup.SnapshotID, captured.ID)
	}
	if rollup.Readiness != domain.ReadinessReady {
		t.Errorf("planner sees readiness %q, want %q", rollup.Readiness, domain.ReadinessReady)
	}
}

// ensure the store package is referenced even if the assertions above change.
var _ = store.Page{}
