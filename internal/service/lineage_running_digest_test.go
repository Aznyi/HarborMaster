package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The running digest's source of truth: Defect 2, and the six cases the fix has
// to get right.
//
// The running digest used to be read from the DECLARED REFERENCE alone. That is
// empty for every container created from a tag, so reconciliation observed
// nothing for the ordinary case: it could not establish a digest, and -- worse
// -- it could not CORRECT one, because the guard that adopts an observed digest
// is necessarily skipped when nothing was observed. A lineage row the host
// contradicted therefore stayed wrong indefinitely, and the next planning pass
// compared the registry against a digest that was not running.
//
// The digest now comes from the local image's RepoDigests through
// domain.RunningDigestFor: exact repository match, ambiguity refused, never a
// registry lookup, and never the tracking label.

// pinnedRef is a digest-pinned reference into the tracked repository.
func pinnedRef(digest string) string { return "docker.io/library/app@" + digest }

// repoDigest renders a RepoDigests entry for the tracked repository.
func repoDigest(digest string) string { return "docker.io/library/app@" + digest }

// A. A tag-created container's digest is resolved from its RepoDigests.
func TestTheRunningDigestOfATagCreatedContainerComesFromRepoDigests(t *testing.T) {
	fake := newFakeReconcileStore()
	fake.observations = []store.LineageObservation{{
		ContainerID:   strings.Repeat("9", 64),
		ContainerName: "app",
		ImageRef:      "app:latest",
		// No ImageDigest. The declared reference carries none, which is the
		// entire point: this is what the store really returns.
		RepoDigests: []string{repoDigest(lineageA)},
	}}

	if _, err := reconciler(t, fake).Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	row := fake.row("app")
	if row.RunningDigest != lineageA {
		t.Fatalf("RunningDigest = %q, want %q\n"+
			"\ta tag-created container's digest exists only in the local image's "+
			"RepoDigests; reading the declared reference alone finds nothing",
			row.RunningDigest, lineageA)
	}
	if !row.Tracked() || row.TrackingReference != trackingRef {
		t.Errorf("lineage = %+v, want tracking %s", row, trackingRef)
	}
}

// B. THE REGRESSION. Lineage claims B while the host runs A; reconciliation
// must adopt A rather than leave the contradiction standing.
//
// This is the state a rolled-back workload was left in. The danger is not an
// untidy row: EvaluateLineage compares the registry against RunningDigest, so a
// row claiming the newest digest answers "nothing to do" for a container that is
// running something older -- silently, and forever.
func TestReconciliationCorrectsALineageDigestTheHostContradicts(t *testing.T) {
	fake := newFakeReconcileStore()
	fake.rows["app"] = domain.ImageLineage{
		ContainerName:     "app",
		State:             domain.LineageTracked,
		Origin:            domain.LineageRecreated,
		TrackingReference: trackingRef,
		TrackingFamiliar:  trackingFamiliar,
		Repository:        trackingRepo,
		// What a failed-then-rolled-back update left behind.
		RunningDigest: lineageB,
	}
	fake.observations = []store.LineageObservation{
		observation("app", "app:latest", lineageA, ""),
	}

	result, err := reconciler(t, fake).Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	row := fake.row("app")
	if row.RunningDigest != lineageA {
		t.Fatalf("RunningDigest = %q, want %q\n"+
			"\tlineage went on claiming the replacement's digest after the rollback "+
			"removed it; the next pass reads this workload as already up to date",
			row.RunningDigest, lineageA)
	}
	if result.Reestablished != 1 {
		t.Errorf("Reestablished = %d, want 1", result.Reestablished)
	}
	// A rollback undoes which ARTEFACT runs, never which tag is followed.
	if row.TrackingReference != trackingRef {
		t.Errorf("TrackingReference = %q, want %q", row.TrackingReference, trackingRef)
	}
	if row.Origin != domain.LineageObserved {
		t.Errorf("Origin = %q, want %q; HarborMaster recorded a change it did not perform",
			row.Origin, domain.LineageObserved)
	}
}

// C. Conflicting RepoDigests for one repository resolve to UNKNOWN.
//
// Unknown must not overwrite a digest already established, and must never be
// guessed at: two different manifests for one repository is a real conflict, and
// taking whichever came first would be a digest substitution.
func TestConflictingRepoDigestsAreNeverGuessedBetween(t *testing.T) {
	fake := newFakeReconcileStore()
	fake.rows["app"] = domain.ImageLineage{
		ContainerName:     "app",
		State:             domain.LineageTracked,
		Origin:            domain.LineageRecreated,
		TrackingReference: trackingRef,
		TrackingFamiliar:  trackingFamiliar,
		Repository:        trackingRepo,
		RunningDigest:     lineageA,
	}
	fake.observations = []store.LineageObservation{{
		ContainerID:   strings.Repeat("9", 64),
		ContainerName: "app",
		ImageRef:      "app:latest",
		RepoDigests:   []string{repoDigest(lineageB), repoDigest(lineageC)},
	}}

	if _, err := reconciler(t, fake).Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	row := fake.row("app")
	if row.RunningDigest == lineageB || row.RunningDigest == lineageC {
		t.Fatalf("RunningDigest = %q; one of two conflicting digests was guessed at",
			row.RunningDigest)
	}
	if row.RunningDigest != lineageA {
		t.Errorf("RunningDigest = %q, want the established %q left untouched by an "+
			"unresolvable observation", row.RunningDigest, lineageA)
	}
}

// D. A RepoDigest belonging to ANOTHER repository is never selected.
//
// The cross-repository substitution guard. An image pulled from one repository
// and re-tagged into another carries both; taking the wrong one would have
// HarborMaster compare a foreign manifest against this repository's registry
// answer.
func TestARepoDigestFromAnotherRepositoryIsNeverUsed(t *testing.T) {
	fake := newFakeReconcileStore()
	fake.observations = []store.LineageObservation{{
		ContainerID:   strings.Repeat("9", 64),
		ContainerName: "app",
		ImageRef:      "app:latest", // docker.io/library/app
		RepoDigests: []string{
			"ghcr.io/attacker/app@" + lineageB,
			"docker.io/library/other@" + lineageC,
		},
	}}

	if _, err := reconciler(t, fake).Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := fake.row("app").RunningDigest; got != "" {
		t.Fatalf("RunningDigest = %q, want empty\n"+
			"\ta digest belonging to a different repository was adopted as this "+
			"container's", got)
	}
}

// E. An external recreation moves the container A -> B. The observation is
// adopted, and HarborMaster does not claim it performed the update.
func TestAnExternalRecreationIsObservedRatherThanClaimed(t *testing.T) {
	fake := newFakeReconcileStore()
	fake.rows["app"] = domain.ImageLineage{
		ContainerName:     "app",
		State:             domain.LineageTracked,
		Origin:            domain.LineageRecreated, // HarborMaster's own last update
		TrackingReference: trackingRef,
		TrackingFamiliar:  trackingFamiliar,
		Repository:        trackingRepo,
		RunningDigest:     lineageA,
	}
	fake.observations = []store.LineageObservation{
		observation("app", "app:latest", lineageB, ""),
	}

	result, err := reconciler(t, fake).Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	row := fake.row("app")
	if row.RunningDigest != lineageB {
		t.Fatalf("RunningDigest = %q, want %q", row.RunningDigest, lineageB)
	}
	if row.Origin != domain.LineageObserved {
		t.Fatalf("Origin = %q, want %q\n"+
			"\tan external change recorded as a recreation credits HarborMaster with "+
			"a host change it never made, and that record is what the audit trail "+
			"rests on", row.Origin, domain.LineageObserved)
	}
	if result.Reestablished != 1 {
		t.Errorf("Reestablished = %d, want 1", result.Reestablished)
	}
}

// F. A digest-only container with no trusted lineage stays untracked, even
// though its RepoDigests would happily supply a digest.
//
// Resolving what a container RUNS must never be mistaken for knowing what it
// FOLLOWS, and a label must never bridge the two.
func TestADigestPinnedContainerStaysUntrackedEvenWhenItsDigestResolves(t *testing.T) {
	fake := newFakeReconcileStore()
	fake.observations = []store.LineageObservation{{
		ContainerID:   strings.Repeat("9", 64),
		ContainerName: "pinned",
		ImageRef:      pinnedRef(lineageA),
		ImageDigest:   lineageA,
		RepoDigests:   []string{repoDigest(lineageA)},
		// A label claiming a tag, which must not enrol it.
		TrackingLabel: trackingRef,
	}}

	result, err := reconciler(t, fake).Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	row := fake.row("pinned")
	if row.Tracked() || row.TrackingReference != "" {
		t.Fatalf("lineage = %+v\n"+
			"\ta container an operator deliberately pinned was given a tag to follow, "+
			"and an untrusted label supplied it", row)
	}
	if result.Untracked != 1 {
		t.Errorf("Untracked = %d, want 1", result.Untracked)
	}
	// What it RUNS is still knowable even when what it FOLLOWS is not.
	if row.RunningDigest != lineageA {
		t.Errorf("RunningDigest = %q, want %q", row.RunningDigest, lineageA)
	}
}

// The shared definition itself, exercised directly.
//
// RunningDigestFor is now the only answer to "what is this container running",
// so its ordering is worth pinning: a digest-pinned reference wins outright,
// because the container was created from that exact manifest.
func TestRunningDigestForPrefersThePinnedReferenceOverTheImageStore(t *testing.T) {
	pinned, err := domain.NormalizeImageRef(pinnedRef(lineageA))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}

	// The local image also carries a DIFFERENT digest for the same repository,
	// which is what a re-tag on the host produces.
	digest, match := domain.RunningDigestFor(pinned, []string{repoDigest(lineageB)})
	if digest != lineageA || match != domain.RepoDigestResolved {
		t.Fatalf("RunningDigestFor = (%q, %q), want (%q, resolved)\n"+
			"\tthe reference the container was created from is the authority on what "+
			"it is running", digest, match, lineageA)
	}

	tagged, err := domain.NormalizeImageRef("app:latest")
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if digest, match := domain.RunningDigestFor(tagged, nil); digest != "" ||
		match != domain.RepoDigestNone {
		t.Errorf("RunningDigestFor with no evidence = (%q, %q), want unknown", digest, match)
	}
}
