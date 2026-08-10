package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

const (
	lineageDigestA = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	lineageDigestB = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func trackedRow(name string) domain.ImageLineage {
	now := time.Now().UTC()
	return domain.ImageLineage{
		ContainerName:     name,
		ContainerID:       strings.Repeat("a", 64),
		State:             domain.LineageTracked,
		Origin:            domain.LineageObserved,
		TrackingReference: "docker.io/library/nginx:1.27",
		TrackingFamiliar:  "nginx:1.27",
		Repository:        "library/nginx",
		RunningDigest:     lineageDigestA,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
}

func TestLineageRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	want := trackedRow("web")
	if err := db.Lineage.Upsert(ctx, want); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := db.Lineage.Get(ctx, "web")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TrackingReference != want.TrackingReference || got.Repository != want.Repository {
		t.Errorf("tracking round trip mismatch: %+v", got)
	}
	if got.RunningDigest != lineageDigestA {
		t.Errorf("RunningDigest = %q", got.RunningDigest)
	}
	if !got.Tracked() {
		t.Error("the row did not come back tracked")
	}
	if got.CreatedAt.Location() != time.UTC || got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt = %v", got.CreatedAt)
	}
}

func TestLineageGetMissingIsErrNotFound(t *testing.T) {
	db := openTestDB(t)

	_, err := db.Lineage.Get(context.Background(), "absent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// The upsert is what a recreation calls, so it has to replace rather than
// duplicate: the container keeps its name across the replacement.
func TestLineageUpsertReplacesAndKeepsCreatedAt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first := trackedRow("web")
	first.CreatedAt = time.Now().UTC().Add(-time.Hour)
	first.UpdatedAt = first.CreatedAt
	if err := db.Lineage.Upsert(ctx, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	second := trackedRow("web")
	second.RunningDigest = lineageDigestB
	second.Origin = domain.LineageRecreated
	second.CreatedAt = time.Now().UTC()
	second.UpdatedAt = second.CreatedAt
	if err := db.Lineage.Upsert(ctx, second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	all, err := db.Lineage.All(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("len = %d, want 1; the upsert duplicated the container", len(all))
	}
	if all[0].RunningDigest != lineageDigestB {
		t.Errorf("RunningDigest = %q, want the newer digest", all[0].RunningDigest)
	}
	if all[0].Origin != domain.LineageRecreated {
		t.Errorf("Origin = %q", all[0].Origin)
	}
	// The row is the same row, so its creation time is the original.
	if !all[0].CreatedAt.Equal(first.CreatedAt.Truncate(time.Nanosecond)) {
		t.Errorf("CreatedAt = %v, want the original %v", all[0].CreatedAt, first.CreatedAt)
	}
}

// A row that claims to be tracked while carrying nothing to track would be a
// row that silently does nothing.
func TestATrackedRowWithoutAReferenceIsRefused(t *testing.T) {
	db := openTestDB(t)

	broken := trackedRow("web")
	broken.TrackingReference = ""
	if err := db.Lineage.Upsert(context.Background(), broken); err == nil {
		t.Fatal("a tracked lineage with no tracking reference was accepted")
	}
}

func TestLineageRefusesUnknownStatesAndOrigins(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	bad := trackedRow("web")
	bad.State = "managed-ish"
	if err := db.Lineage.Upsert(ctx, bad); err == nil {
		t.Error("an unknown state was accepted")
	}

	bad = trackedRow("web")
	bad.Origin = "vibes"
	if err := db.Lineage.Upsert(ctx, bad); err == nil {
		t.Error("an unknown origin was accepted")
	}
}

// The tracking reference is read back and resolved against a registry, so its
// length is bounded on the way in.
func TestLineageBoundsTheTrackingReference(t *testing.T) {
	db := openTestDB(t)

	huge := trackedRow("web")
	huge.TrackingReference = "docker.io/library/" + strings.Repeat("n", domain.MaxLineageReferenceBytes)
	if err := db.Lineage.Upsert(context.Background(), huge); err == nil {
		t.Fatal("an unbounded tracking reference was accepted")
	}
}

func TestLineageRefusesANonDigestRunningDigest(t *testing.T) {
	db := openTestDB(t)

	bad := trackedRow("web")
	bad.RunningDigest = "definitely-not-a-digest"
	if err := db.Lineage.Upsert(context.Background(), bad); err == nil {
		t.Fatal("a non-digest running digest was accepted")
	}
}

func TestLineageRefusesAnUnacceptableContainerName(t *testing.T) {
	db := openTestDB(t)

	bad := trackedRow("web")
	bad.ContainerName = "../../etc/passwd"
	if err := db.Lineage.Upsert(context.Background(), bad); err == nil {
		t.Fatal("an unacceptable container name was accepted")
	}
}

func TestTrackedReturnsOnlyTrackedRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.Lineage.Upsert(ctx, trackedRow("web")); err != nil {
		t.Fatalf("upsert tracked: %v", err)
	}
	now := time.Now().UTC()
	if err := db.Lineage.Upsert(ctx, domain.ImageLineage{
		ContainerName: "pinned",
		State:         domain.LineageUntracked,
		Origin:        domain.LineageObserved,
		RunningDigest: lineageDigestB,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("upsert untracked: %v", err)
	}

	tracked, err := db.Lineage.Tracked(ctx)
	if err != nil {
		t.Fatalf("tracked: %v", err)
	}
	if len(tracked) != 1 || tracked[0].ContainerName != "web" {
		t.Fatalf("tracked = %+v, want only the tracked container", tracked)
	}

	trackedCount, untrackedCount, err := db.Lineage.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if trackedCount != 1 || untrackedCount != 1 {
		t.Errorf("counts = %d tracked, %d untracked; want 1 and 1", trackedCount, untrackedCount)
	}
}

func TestLineageDeleteIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.Lineage.Upsert(ctx, trackedRow("web")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for range 2 {
		if err := db.Lineage.Delete(ctx, "web"); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	if _, err := db.Lineage.Get(ctx, "web"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// A container that has left the estate has nothing to follow, and keeping the
// row would have update discovery resolve a reference for a workload that is
// gone.
func TestPruneAbsentRemovesLineageForContainersThatAreGone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.Lineage.Upsert(ctx, trackedRow("departed")); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	removed, err := db.Lineage.PruneAbsent(ctx, 100)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := db.Lineage.Get(ctx, "departed"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the row survived a prune: %v", err)
	}
}

func TestObservationsReadsThePresentEstate(t *testing.T) {
	db := openTestDB(t)

	// No containers seeded, so this proves the query runs and returns nothing
	// rather than erroring on the label join.
	got, err := db.Lineage.Observations(context.Background(), 0)
	if err != nil {
		t.Fatalf("observations: %v", err)
	}
	if got == nil {
		t.Fatal("expected a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}
