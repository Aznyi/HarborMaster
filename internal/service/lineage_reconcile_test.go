package service_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Reconciliation: keeping HarborMaster's claim honest against a host anybody
// can change. These are the cases from §7 of Phase 13.1.
//
// The property every one of them asserts, in one sentence: HarborMaster must
// never end a pass claiming something the host contradicts, and must never
// record that it performed a change it did not perform.

type fakeReconcileStore struct {
	mu           sync.Mutex
	observations []store.LineageObservation
	rows         map[string]domain.ImageLineage
	pruned       int64
}

func newFakeReconcileStore() *fakeReconcileStore {
	return &fakeReconcileStore{rows: map[string]domain.ImageLineage{}}
}

func (f *fakeReconcileStore) Observations(context.Context, int) ([]store.LineageObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observations, nil
}

func (f *fakeReconcileStore) Get(_ context.Context, name string) (domain.ImageLineage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[name]
	if !ok {
		return domain.ImageLineage{}, store.ErrNotFound
	}
	return row, nil
}

func (f *fakeReconcileStore) Upsert(_ context.Context, row domain.ImageLineage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[row.ContainerName] = row
	return nil
}

func (f *fakeReconcileStore) PruneAbsent(context.Context, int) (int64, error) {
	return f.pruned, nil
}

func (f *fakeReconcileStore) row(name string) domain.ImageLineage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[name]
}

func reconciler(t *testing.T, s *fakeReconcileStore) *service.LineageReconciler {
	t.Helper()
	return service.NewLineageReconciler(s, discardLogger(), func() time.Time {
		return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	})
}

// observation models what LineageRepository.Observations really returns for a
// container running `digest`.
//
// The distinction it encodes is the whole of Defect 2. The store fills
// ImageDigest from the DECLARED REFERENCE, so a tag-created container has none
// -- its digest exists only in the local image's RepoDigests. A test that put
// the digest in ImageDigest for a tagged container was asserting against a row
// the store never produces, which is exactly how the defect survived a green
// suite.
func observation(name, reference, digest, label string) store.LineageObservation {
	observed := store.LineageObservation{
		ContainerID:   strings.Repeat("9", 64),
		ContainerName: name,
		ImageRef:      reference,
		TrackingLabel: label,
	}
	if digest == "" {
		return observed
	}

	parsed, err := domain.NormalizeImageRef(reference)
	if err != nil {
		return observed
	}
	if parsed.Digest != "" {
		// A digest-pinned reference: the store records it on the container row.
		observed.ImageDigest = parsed.Digest
		return observed
	}
	// A tag: the daemon reports the digest against the image, and the store
	// hands the list over untouched.
	observed.RepoDigests = []string{parsed.Host + "/" + parsed.Path + "@" + digest}
	return observed
}

// ------------------------------------------------------------ establishment --

func TestReconcileEstablishesLineageForATaggedContainer(t *testing.T) {
	fake := newFakeReconcileStore()
	fake.observations = []store.LineageObservation{observation("app", "app:latest", lineageA, "")}

	result, err := reconciler(t, fake).Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Established != 1 {
		t.Fatalf("Established = %d, want 1", result.Established)
	}
	row := fake.row("app")
	if !row.Tracked() || row.TrackingReference != trackingRef {
		t.Errorf("lineage = %+v", row)
	}
	if row.RunningDigest != lineageA {
		t.Errorf("RunningDigest = %q", row.RunningDigest)
	}
}

func TestReconcileLeavesAGenuineDigestPinnedContainerUntracked(t *testing.T) {
	fake := newFakeReconcileStore()
	fake.observations = []store.LineageObservation{
		observation("pinned", "docker.io/library/app@"+lineageA, lineageA, ""),
	}

	result, err := reconciler(t, fake).Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Untracked != 1 {
		t.Fatalf("Untracked = %d, want 1", result.Untracked)
	}
	if fake.row("pinned").Tracked() {
		t.Error("a deliberately pinned container was given a tracking reference")
	}
}

// The forgery case. A container nobody manages carries the label; it must not
// become managed by saying so.
func TestAForgedLabelCannotEnrolAnUnmanagedContainer(t *testing.T) {
	fake := newFakeReconcileStore()
	fake.observations = []store.LineageObservation{
		observation("hostile", "docker.io/library/app@"+lineageA, lineageA,
			"evil.example.com/attacker/app:latest"),
	}

	if _, err := reconciler(t, fake).Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	row := fake.row("hostile")
	if row.Tracked() {
		t.Fatalf("a label enrolled an unmanaged container: %+v", row)
	}
	if row.TrackingReference != "" {
		t.Errorf("TrackingReference = %q, want empty", row.TrackingReference)
	}
}

// ------------------------------------------------------- external changes --

// §7 A: managed repo:latest at A, user recreates it at B of the SAME tag.
func TestAManualRecreationOfTheSameTagAdoptsTheNewDigest(t *testing.T) {
	fake := newFakeReconcileStore()
	fake.rows["app"] = lineageRow(lineageA)
	fake.observations = []store.LineageObservation{observation("app", "app:latest", lineageB, "")}

	result, err := reconciler(t, fake).Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reestablished != 1 {
		t.Fatalf("Reestablished = %d, want 1", result.Reestablished)
	}
	row := fake.row("app")
	if row.RunningDigest != lineageB {
		t.Errorf("RunningDigest = %q, want the digest actually running", row.RunningDigest)
	}
	if row.TrackingReference != trackingRef {
		t.Errorf("the tracking reference changed: %q", row.TrackingReference)
	}
	// HarborMaster must not record that it performed this change.
	if row.Origin != domain.LineageObserved {
		t.Errorf("Origin = %q, want observed; HarborMaster claimed a change it did not perform", row.Origin)
	}
}

// §7 B: managed repo:latest at A, user recreates it as repo@digestB.
//
// The one case where a digest-pinned container stays tracked, and it is safe
// precisely because the claim predates the change.
func TestAManualPinKeepsTheExistingTrackingReference(t *testing.T) {
	fake := newFakeReconcileStore()
	fake.rows["app"] = lineageRow(lineageA)
	fake.observations = []store.LineageObservation{
		observation("app", "docker.io/library/app@"+lineageB, lineageB, ""),
	}

	if _, err := reconciler(t, fake).Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	row := fake.row("app")
	if !row.Tracked() || row.TrackingReference != trackingRef {
		t.Errorf("the tracking reference was lost: %+v", row)
	}
	if row.RunningDigest != lineageB {
		t.Errorf("RunningDigest = %q", row.RunningDigest)
	}
}

// §7 C: the operator changes the tag. Their declaration wins.
func TestChangingTheTagRepointsTracking(t *testing.T) {
	fake := newFakeReconcileStore()
	fake.rows["app"] = lineageRow(lineageA)
	fake.observations = []store.LineageObservation{observation("app", "app:stable", lineageC, "")}

	if _, err := reconciler(t, fake).Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	row := fake.row("app")
	if row.TrackingReference != "docker.io/library/app:stable" {
		t.Fatalf("TrackingReference = %q, want the tag the operator declared", row.TrackingReference)
	}
	if row.RunningDigest != lineageC {
		t.Errorf("RunningDigest = %q", row.RunningDigest)
	}
	if row.Origin != domain.LineageObserved {
		t.Errorf("Origin = %q, want observed", row.Origin)
	}
}

// §7 E: same configuration, new container id. Bookkeeping only.
func TestANewContainerIDAloneIsNotAReestablishment(t *testing.T) {
	fake := newFakeReconcileStore()
	existing := lineageRow(lineageA)
	existing.ContainerID = strings.Repeat("5", 64)
	fake.rows["app"] = existing
	fake.observations = []store.LineageObservation{
		observation("app", "app:latest", lineageA, ""),
	}

	result, err := reconciler(t, fake).Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reestablished != 0 {
		t.Errorf("Reestablished = %d; a refreshed id is not a change of what is running", result.Reestablished)
	}
	if fake.row("app").ContainerID != strings.Repeat("9", 64) {
		t.Error("the observed container id was not recorded as evidence")
	}
}

// §7 F: nothing changed at all. The pass must be a no-op.
func TestAnAgreeingEstateIsConfirmedAndNotRewritten(t *testing.T) {
	fake := newFakeReconcileStore()
	existing := lineageRow(lineageA)
	existing.ContainerID = strings.Repeat("9", 64)
	fake.rows["app"] = existing
	fake.observations = []store.LineageObservation{observation("app", "app:latest", lineageA, "")}

	result, err := reconciler(t, fake).Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Confirmed != 1 || result.Reestablished != 0 || result.Established != 0 {
		t.Fatalf("result = %+v, want a single confirmation", result)
	}
}

// A reference HarborMaster cannot parse is left alone rather than guessed at.
func TestAnUnparseableReferenceEstablishesNothing(t *testing.T) {
	fake := newFakeReconcileStore()
	fake.observations = []store.LineageObservation{observation("odd", "::::not a reference", "", "")}

	if _, err := reconciler(t, fake).Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, exists := fake.rows["odd"]; exists {
		t.Error("lineage was invented for a reference that does not parse")
	}
}
