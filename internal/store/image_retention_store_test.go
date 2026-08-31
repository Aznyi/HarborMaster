package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The evidence an image-cleanup pass reads.
//
// Every test here asks one question: does the store report a reason to KEEP
// when one exists? A missed reference is not a cosmetic defect -- it is the
// deletion of an artefact a recovery depends on -- so each retaining reference
// gets its own case rather than being covered incidentally by another.

func retentionImageID(marker string) string {
	return "sha256:" + strings.Repeat(marker, 64)
}

var (
	supersededImage = retentionImageID("a")
	currentImage    = retentionImageID("b")
	otherImage      = retentionImageID("c")
)

// settledExecution drives one recreation to the state that makes its old image
// a cleanup candidate: succeeded AND the parked original removed.
//
// Driven through the real repository rather than an INSERT, so the fixture
// cannot claim a combination of columns the lifecycle would never produce.
func settledExecution(
	t *testing.T,
	db *store.DB,
	containerID, containerName, oldImageID, targetImageID string,
	settledAt time.Time,
) domain.Execution {
	t.Helper()
	ctx := context.Background()

	execution := executionFor(containerID, domain.NewAcquisitionID())
	execution.ContainerName = containerName
	execution.OldImageID = oldImageID
	execution.Target.ImageID = targetImageID

	created, err := db.Executions.Create(ctx, execution, settledAt.Add(-time.Minute))
	if err != nil {
		t.Fatalf("create execution: %v", err)
	}

	moved, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID:     created.ExecutionID,
		To:              domain.ExecutionSucceeded,
		OriginalRemoved: true,
	}, settledAt)
	if err != nil || !moved {
		t.Fatalf("settle execution: moved=%v err=%v", moved, err)
	}
	return created
}

func candidateFor(t *testing.T, db *store.DB, imageID string) (store.ImageCleanupCandidate, bool) {
	t.Helper()
	candidates, err := db.ImageRetention.ImageCleanupCandidates(context.Background())
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	for _, candidate := range candidates {
		if candidate.ImageID == imageID {
			return candidate, true
		}
	}
	return store.ImageCleanupCandidate{}, false
}

func referencesFor(t *testing.T, db *store.DB, imageID string) store.ImageReferences {
	t.Helper()
	references, err := db.ImageRetention.ImageReferencesFor(
		context.Background(), []string{imageID})
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	return references[imageID]
}

// ------------------------------------------------------------ candidates --

func TestOnlyASettledSuccessfulUpdateMakesAnImageACandidate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	settledExecution(t, db, "svc-a-id", "svc-a", supersededImage, currentImage, now)

	// Succeeded, but the parked original could not be removed. The host still
	// carries two containers and the operator has a note: not settled.
	unfinished := executionFor("svc-b-id", domain.NewAcquisitionID())
	unfinished.ContainerName = "svc-b"
	unfinished.OldImageID = retentionImageID("d")
	if _, err := db.Executions.Create(ctx, unfinished, now); err != nil {
		t.Fatalf("create unfinished: %v", err)
	}
	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: unfinished.ExecutionID,
		To:          domain.ExecutionSucceeded,
	}, now); err != nil {
		t.Fatalf("advance unfinished: %v", err)
	}

	// Failed outright.
	failed := executionFor("svc-c-id", domain.NewAcquisitionID())
	failed.ContainerName = "svc-c"
	failed.OldImageID = retentionImageID("e")
	if _, err := db.Executions.Create(ctx, failed, now); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: failed.ExecutionID,
		To:          domain.ExecutionFailed,
		Failure:     domain.ExecutionFailureImageMismatch,
	}, now); err != nil {
		t.Fatalf("advance failed: %v", err)
	}

	if _, found := candidateFor(t, db, supersededImage); !found {
		t.Error("a settled successful update did not make its old image a candidate.\n\n" +
			"If this set is always empty, cleanup never removes anything -- which " +
			"is safe, but it also means every other test in this file proves " +
			"nothing.")
	}
	if _, found := candidateFor(t, db, retentionImageID("d")); found {
		t.Error("an execution that succeeded WITHOUT removing the parked original " +
			"made its old image a candidate.\n\n" +
			"original_removed is the marker that the lifecycle reached its safe " +
			"terminal state. Without it the original container is still on the " +
			"host, still built from that image.")
	}
	if _, found := candidateFor(t, db, retentionImageID("e")); found {
		t.Error("a FAILED execution made its old image a candidate.\n\n" +
			"The old image is what puts the workload back. It is the last thing " +
			"cleanup may touch after an update did not work.")
	}
}

func TestTheCandidateCarriesItsWorkloadAndSettlementTime(t *testing.T) {
	db := openTestDB(t)
	settledAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	settledExecution(t, db, "svc-a-id", "svc-a", supersededImage, currentImage, settledAt)

	candidate, found := candidateFor(t, db, supersededImage)
	if !found {
		t.Fatal("no candidate")
	}
	if candidate.ContainerName != "svc-a" {
		t.Errorf("ContainerName = %q, want svc-a", candidate.ContainerName)
	}
	if !candidate.SettledAt.Equal(settledAt) {
		t.Errorf("SettledAt = %s, want %s\n\n"+
			"The retention clock runs from SETTLEMENT, not from when the image "+
			"was pulled or from whenever a cleanup pass happened to look.",
			candidate.SettledAt, settledAt)
	}
}

func TestGenerationsAreRankedPerWorkload(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	oldest := retentionImageID("1")
	middle := retentionImageID("2")
	newest := retentionImageID("3")
	// A different workload, settled between them. Its generation count must not
	// be affected by this one's, or the other way round.
	elsewhere := retentionImageID("4")

	settledExecution(t, db, "svc-a-id", "svc-a", oldest, middle, base)
	settledExecution(t, db, "svc-b-id", "svc-b", elsewhere, currentImage, base.Add(time.Hour))
	settledExecution(t, db, "svc-a-id", "svc-a", middle, newest, base.Add(2*time.Hour))
	settledExecution(t, db, "svc-a-id", "svc-a", newest, currentImage, base.Add(3*time.Hour))

	want := map[string]int{oldest: 2, middle: 1, newest: 0, elsewhere: 0}
	for image, generations := range want {
		candidate, found := candidateFor(t, db, image)
		if !found {
			t.Fatalf("%s is not a candidate", image[:14])
		}
		if candidate.NewerGenerations != generations {
			t.Errorf("%s NewerGenerations = %d, want %d\n\n"+
				"Generations are counted PER WORKLOAD. Counting them globally "+
				"would let a busy container's history age out a quiet one's "+
				"only previous image.",
				image[:14], candidate.NewerGenerations, generations)
		}
	}
}

// ------------------------------------------------------------ references --

func TestAnUnreferencedImageHasNoRetainingReference(t *testing.T) {
	// The non-vacuity control. Without it, queries that silently counted
	// nothing at all would make every case below pass.
	db := openTestDB(t)
	settledExecution(t, db, "svc-a-id", "svc-a", supersededImage, currentImage,
		time.Now().UTC())

	got := referencesFor(t, db, supersededImage)
	if got != (store.ImageReferences{}) {
		t.Fatalf("an image nothing references reported %+v, want every count zero", got)
	}
}

func TestAPresentContainerRetainsItsImage(t *testing.T) {
	db := openTestDB(t)
	commitOf(t, db, records(
		buildContainer("svc-a-id", "svc-a", withImage("nginx:1.27", supersededImage))))

	if got := referencesFor(t, db, supersededImage).PresentContainers; got != 1 {
		t.Errorf("PresentContainers = %d, want 1\n\n"+
			"A container built from this image is the hardest retention reason "+
			"there is: removing it would be removing the artefact out from "+
			"under a live workload.", got)
	}
}

func TestADepartedContainerDoesNotRetainItsImage(t *testing.T) {
	db := openTestDB(t)
	commitOf(t, db, records(
		buildContainer("svc-a-id", "svc-a", withImage("nginx:1.27", supersededImage))))
	// The real departure path: an inventory that no longer lists it.
	commitOf(t, db, records(
		buildContainer("svc-z-id", "svc-z", withImage("nginx:1.27", otherImage))))

	if got := referencesFor(t, db, supersededImage).PresentContainers; got != 0 {
		t.Errorf("PresentContainers = %d for a departed container, want 0\n\n"+
			"The tombstone row is history, not a live reference. Counting it "+
			"would leave cleanup unable to remove anything, ever.", got)
	}
}

func TestAParkedOriginalRetainsItsImageAsPreservedEvidence(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	execution := executionFor("svc-a-id", domain.NewAcquisitionID())
	execution.ContainerName = "svc-a"
	execution.OldImageID = supersededImage
	created, err := db.Executions.Create(ctx, execution, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	parkedName := "svc-a" + domain.ParkedNameSuffix + created.ExecutionID
	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: created.ExecutionID,
		To:          domain.ExecutionFailed,
		Failure:     domain.ExecutionFailureImageMismatch,
		ParkedName:  parkedName,
	}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}

	commitOf(t, db, records(
		buildContainer("parked-id", parkedName, withImage("nginx:1.27", supersededImage))))

	got := referencesFor(t, db, supersededImage)
	if got.PreservedContainers != 1 {
		t.Errorf("PreservedContainers = %d, want 1\n\n"+
			"A parked original is the thing a rollback restores. It is counted "+
			"apart from an ordinary workload so the operator is told the "+
			"precise reason the image stayed.", got.PreservedContainers)
	}
	if got.PresentContainers != 0 {
		t.Errorf("PresentContainers = %d, want 0: preserved evidence must not "+
			"also be counted as a live workload", got.PresentContainers)
	}
}

func TestAnActiveAcquisitionRetainsTheImageItIsFetching(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := db.Acquisitions.Create(ctx,
		acquisitionFor("svc-a-id", "sha256:"+strings.Repeat("a", 64)), now)
	if err != nil {
		t.Fatalf("create acquisition: %v", err)
	}
	if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
		AcquisitionID:   created.AcquisitionID,
		To:              domain.AcquisitionVerifying,
		AcquiredImageID: supersededImage,
	}, now); err != nil {
		t.Fatalf("advance acquisition: %v", err)
	}

	if got := referencesFor(t, db, supersededImage).ActiveAcquisitions; got != 1 {
		t.Errorf("ActiveAcquisitions = %d, want 1\n\n"+
			"An operation that has not finished has not decided anything. "+
			"Removing the image out from under it turns a download that was "+
			"about to succeed into a failure.", got)
	}
}

func TestAnActiveExecutionRetainsBothImagesItNames(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	execution := executionFor("svc-a-id", domain.NewAcquisitionID())
	execution.ContainerName = "svc-a"
	execution.OldImageID = supersededImage
	execution.Target.ImageID = currentImage
	if _, err := db.Executions.Create(ctx, execution, time.Now().UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}

	if got := referencesFor(t, db, supersededImage).ActiveExecutions; got != 1 {
		t.Errorf("the image being LEFT: ActiveExecutions = %d, want 1", got)
	}
	if got := referencesFor(t, db, currentImage).ActiveExecutions; got != 1 {
		t.Errorf("the image being MOVED TO: ActiveExecutions = %d, want 1\n\n"+
			"A recreation in flight needs its target. Removing it mid-flight "+
			"breaks the update it is in the middle of applying.", got)
	}
}

func TestAFailedExecutionRetainsBothImagesItNames(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	execution := executionFor("svc-a-id", domain.NewAcquisitionID())
	execution.ContainerName = "svc-a"
	execution.OldImageID = supersededImage
	execution.Target.ImageID = currentImage
	if _, err := db.Executions.Create(ctx, execution, now); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: execution.ExecutionID,
		To:          domain.ExecutionFailed,
		Failure:     domain.ExecutionFailureImageMismatch,
	}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}

	if got := referencesFor(t, db, supersededImage).UnsettledFailures; got != 1 {
		t.Errorf("the original: UnsettledFailures = %d, want 1\n\n"+
			"It is what restores service.", got)
	}
	if got := referencesFor(t, db, currentImage).UnsettledFailures; got != 1 {
		t.Errorf("the failed target: UnsettledFailures = %d, want 1\n\n"+
			"It is the only artefact that can be inspected to find out why the "+
			"update did not work.", got)
	}
}

func TestARecoveryPlanRetainsEveryImageItNames(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	execution := executionFor("svc-a-id", domain.NewAcquisitionID())
	execution.ContainerName = "svc-a"
	execution.OldImageID = supersededImage
	execution.Target.ImageID = currentImage
	created, err := db.Executions.Create(ctx, execution, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	plan := domain.BuildRecoveryPlan(domain.RecoveryContext{
		ExecutionID:   created.ExecutionID,
		ContainerName: "svc-a",
		OriginalID:    strings.Repeat("a", 64),
		ParkedName:    "svc-a" + domain.ParkedNameSuffix + created.ExecutionID,
		Checkpoint:    domain.CheckpointReplacementStarted,
	})
	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: created.ExecutionID,
		To:          domain.ExecutionFailed,
		Failure:     domain.ExecutionFailureImageMismatch,
		ParkedName:  "svc-a" + domain.ParkedNameSuffix + created.ExecutionID,
		MarkMutated: true,
		Recovery:    plan,
	}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}

	for _, image := range []string{supersededImage, currentImage} {
		if got := referencesFor(t, db, image).OutstandingRecoveries; got != 1 {
			t.Errorf("%s OutstandingRecoveries = %d, want 1\n\n"+
				"A recovery note is HarborMaster telling an operator the host is "+
				"in a state it could not resolve by itself. Whatever that note "+
				"says will be carried out against these images.", image[:14], got)
		}
	}
}

func TestAnActiveRollbackRetainsTheImageItIsRestoring(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	execution := executionFor("svc-a-id", domain.NewAcquisitionID())
	execution.ContainerName = "svc-a"
	execution.OldImageID = supersededImage
	created, err := db.Executions.Create(ctx, execution, now)
	if err != nil {
		t.Fatalf("create execution: %v", err)
	}
	if _, err := db.Rollbacks.Create(ctx,
		rollbackFor(created.ExecutionID, "svc-a"), now); err != nil {
		t.Fatalf("create rollback: %v", err)
	}

	if got := referencesFor(t, db, supersededImage).ActiveRollbacks; got != 1 {
		t.Errorf("ActiveRollbacks = %d, want 1\n\n"+
			"A rollback in flight is about to create a container from this "+
			"image. It is the one moment where removing it guarantees the "+
			"recovery fails.", got)
	}
}

// ---------------------------------------------------------- plan targets --

func TestAnAcquiredButUnappliedImageIsAPlanTarget(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := db.Acquisitions.Create(ctx,
		acquisitionFor("svc-a-id", "sha256:"+strings.Repeat("a", 64)), now)
	if err != nil {
		t.Fatalf("create acquisition: %v", err)
	}
	if _, err := db.Acquisitions.Advance(ctx, store.StateChange{
		AcquisitionID:   created.AcquisitionID,
		To:              domain.AcquisitionSucceeded,
		AcquiredImageID: currentImage,
		AcquiredDigest:  "sha256:" + strings.Repeat("a", 64),
	}, now); err != nil {
		t.Fatalf("advance acquisition: %v", err)
	}

	targets, err := db.ImageRetention.PlanTargetImages(ctx)
	if err != nil {
		t.Fatalf("PlanTargetImages: %v", err)
	}
	if _, named := targets[currentImage]; !named {
		t.Error("an image that was downloaded but not yet applied is not a plan target.\n\n" +
			"Removing it turns a ready update into a failure at the moment the " +
			"operator presses the button.")
	}
}

// --------------------------------------------------------------- bounds --

func TestReferenceLookupRefusesAnUnboundedSet(t *testing.T) {
	db := openTestDB(t)

	ids := make([]string, 0, 201)
	for i := 0; i < 201; i++ {
		ids = append(ids, currentImage)
	}
	if _, err := db.ImageRetention.ImageReferencesFor(context.Background(), ids); err == nil {
		t.Error("a 201-image reference lookup was accepted.\n\n" +
			"The bound is not politeness: an unbounded IN list runs past " +
			"SQLite's variable limit, and a pass that cannot complete " +
			"establishes no evidence at all.")
	}
}

func TestAnEmptyLookupIsNotAnError(t *testing.T) {
	db := openTestDB(t)
	got, err := db.ImageRetention.ImageReferencesFor(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Errorf("empty lookup = %v, %v; want an empty map and no error", got, err)
	}
}
