package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Image and snapshot identity: historical evidence stays historical (C3F).
//
// # The defect class
//
// C3E found that a NAME can promise stronger evidence than the implementation
// establishes -- ContainerPresent meant only "a row exists". These pin the two
// remaining places the same confusion could live: what a container is RUNNING,
// and which snapshot is its BASELINE.
//
// Both are bound to a container INSTANCE, not to a workload name. A recreation
// produces a new Docker id, and evidence gathered about the old instance must
// not silently become evidence about the new one -- which is the opposite of
// preferences, pauses and lineage, where following the name is the point.
//
// Nothing here deletes or rewrites history. A departed container's recorded
// image and its snapshots stay readable forever; the question is only whether
// they may be presented as CURRENT.

const identityImage = "ghcr.io/acme/identity:1.0.0"

func identityContainers(t *testing.T, db *store.DB, names ...string) {
	t.Helper()
	commitContainersWithImage(t, db, identityImage, names...)
}

// ------------------------------------------------------------ image identity --

func TestADepartedContainersImageRecordStaysReadable(t *testing.T) {
	db, ctx := preferenceRepo(t)
	identityContainers(t, db, "svc-a")

	identityContainers(t, db, "other")

	// The historical record: what it WAS running, still readable.
	detail, err := db.Containers.Get(ctx, "svc-a-id")
	if err != nil || detail == nil {
		t.Fatalf("the departed container's image record was lost: %v", err)
	}
	if detail.Overview.Image.Raw != identityImage {
		t.Errorf("image reference = %q, want %q", detail.Overview.Image.Raw, identityImage)
	}
	if detail.Overview.Present {
		t.Error("the record claims the container is present")
	}
}

func TestADepartedContainerIsNotReportedAsCurrentlyRunning(t *testing.T) {
	// The C3E defect class applied to image identity: the presence-gated
	// operation must refuse, so no caller can read a running image off a
	// tombstone.
	db, ctx := preferenceRepo(t)
	identityContainers(t, db, "svc-a")

	if _, err := db.Containers.GetPresent(ctx, "svc-a-id"); err != nil {
		t.Fatalf("a present container: %v", err)
	}

	identityContainers(t, db, "other")

	if _, err := db.Containers.GetPresent(ctx, "svc-a-id"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetPresent returned a departed container's image state: %v", err)
	}
}

func TestANewContainerDoesNotInheritTheOldOnesImageEvidence(t *testing.T) {
	// Same workload name, new Docker id, DIFFERENT image. The old instance's
	// record must not describe the new one.
	db, ctx := preferenceRepo(t)
	identityContainers(t, db, "svc-a")

	commitContainersAs(t, db, map[string]string{"svc-a": "svc-a-v2-id"})

	// The replacement is present and carries its own record.
	replacement, err := db.Containers.GetPresent(ctx, "svc-a-v2-id")
	if err != nil || replacement == nil {
		t.Fatalf("the replacement is not present: %v", err)
	}
	if replacement.Overview.ID != "svc-a-v2-id" {
		t.Errorf("the replacement's id = %q", replacement.Overview.ID)
	}

	// The old instance is history and cannot answer a current question.
	if _, err := db.Containers.GetPresent(ctx, "svc-a-id"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the old instance still answers a current question: %v", err)
	}
	old, err := db.Containers.Get(ctx, "svc-a-id")
	if err != nil || old == nil {
		t.Fatalf("the old instance's record was lost: %v", err)
	}
	if old.Overview.ID == replacement.Overview.ID {
		t.Fatal("the two instances share an id; this test proves nothing")
	}
}

// --------------------------------------------------------- snapshot identity --

// identitySnapshot writes one snapshot for a container, reusing the package's
// own fixture helpers so this exercises the real create path.
func identitySnapshot(t *testing.T, db *store.DB, containerID, marker string) domain.Snapshot {
	t.Helper()
	snapshot := newSnapshot(containerID, identityChecksum(marker))
	snapshot.ImageReference = identityImage
	return createSnapshot(t, db, snapshot)
}

// identityChecksum pads a readable marker to the 64 characters the schema
// requires, so a failure message still says which snapshot it means.
func identityChecksum(marker string) string {
	const width = 64
	if len(marker) >= width {
		return marker[:width]
	}
	return marker + strings.Repeat("0", width-len(marker))
}

func TestASnapshotStaysReadableAfterItsContainerLeaves(t *testing.T) {
	db, ctx := preferenceRepo(t)
	identityContainers(t, db, "svc-a")
	snapshot := identitySnapshot(t, db, "svc-a-id", "original")

	identityContainers(t, db, "other")

	// History: unchanged and readable. Nothing about a container leaving may
	// delete or rewrite what was captured while it was there.
	got, err := db.Snapshots.Get(ctx, snapshot.ID)
	if err != nil {
		t.Fatalf("the snapshot was lost when its container left: %v", err)
	}
	if got.ContainerID != "svc-a-id" || got.Checksum != identityChecksum("original") {
		t.Errorf("the snapshot was altered: %+v", got)
	}
	// And it is still that container's newest recorded snapshot.
	baseline, err := db.Snapshots.Baseline(ctx, "svc-a-id")
	if err != nil || baseline.ID != snapshot.ID {
		t.Errorf("baseline for the departed container = %+v (err %v)", baseline, err)
	}
}

func TestANewContainerDoesNotInheritTheOldOnesBaseline(t *testing.T) {
	// THE INSTANCE-BINDING PROPERTY. Snapshots are keyed by container id, so a
	// replacement starts with no baseline and must be captured afresh. Were
	// they keyed by name, the new container would inherit a restore point
	// describing a configuration it never had.
	db, ctx := preferenceRepo(t)
	identityContainers(t, db, "svc-a")
	old := identitySnapshot(t, db, "svc-a-id", "original")

	commitContainersAs(t, db, map[string]string{"svc-a": "svc-a-v2-id"})

	if _, err := db.Snapshots.Baseline(ctx, "svc-a-v2-id"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the replacement inherited a baseline: %v", err)
	}
	// The old one is still the old instance's, unchanged.
	baseline, err := db.Snapshots.Baseline(ctx, "svc-a-id")
	if err != nil || baseline.ID != old.ID {
		t.Errorf("the old instance's baseline moved: %+v (err %v)", baseline, err)
	}
}

func TestTheBaselineIsTheNewestRecordedSnapshotForThatInstance(t *testing.T) {
	// What Baseline actually establishes, stated so a caller does not read more
	// into the name: the newest recorded snapshot for one container id. It does
	// NOT establish that the snapshot still describes the container -- the
	// execution preflight compares the plan's snapshot id against a fresh
	// capture for that, and refuses `snapshotChanged` when they differ.
	db, ctx := preferenceRepo(t)
	identityContainers(t, db, "svc-a")

	first := identitySnapshot(t, db, "svc-a-id", "first")
	second := identitySnapshot(t, db, "svc-a-id", "second")

	baseline, err := db.Snapshots.Baseline(ctx, "svc-a-id")
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if baseline.ID != second.ID {
		t.Fatalf("baseline = %d, want the newer snapshot %d", baseline.ID, second.ID)
	}
	// The older one is history, not deleted.
	if _, err := db.Snapshots.Get(ctx, first.ID); err != nil {
		t.Errorf("the earlier snapshot was lost: %v", err)
	}
}

func TestAnUnknownContainerHasNoBaseline(t *testing.T) {
	// Fail closed: no snapshot means no restore point, reported as absence
	// rather than as an empty one that a gate might accept.
	db, ctx := preferenceRepo(t)
	identityContainers(t, db, "svc-a")

	if _, err := db.Snapshots.Baseline(ctx, "never-existed"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("baseline for an unknown container = %v, want ErrNotFound", err)
	}
}
