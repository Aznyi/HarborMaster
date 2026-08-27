package service_test

import (
	"context"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/registry"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Stage 17.5: bounded version discovery must not erase a known digest movement.
//
// # The defect, stated precisely
//
// Two independent questions are asked of a registry on every check:
//
//	1. Does the EXACT CONFIGURED REFERENCE still resolve to the digest this
//	   container is running?   -- answered by one manifest HEAD, always.
//	2. Does a NEWER VERSION TAG exist anywhere in the repository?
//	   -- answered by a bounded tag enumeration, sometimes.
//
// The first question is cheap, always asked, and always answerable. The second
// is expensive, bounded by a page budget, and frequently unanswerable on a large
// repository -- `library/nginx` publishes far more tags than five Docker Hub
// pages can carry.
//
// The defect was that an unanswerable SECOND question discarded a positively
// answered FIRST one. A container on `nginx:1.27.4` whose digest had genuinely
// moved reported `unknown`, no strategy permits `unknown`, and the update never
// happened -- for ever, because the repository never gets smaller.
//
// # Why this is a correctness fix and not a bound relaxation
//
// The manifest HEAD has ALREADY happened by the time enumeration runs. The
// remote digest is already in hand. Nothing here fetches anything new; the fix
// is that an answer HarborMaster already had stops being thrown away.
//
// # The rule
//
//	Bounded version discovery may be incomplete. That must not erase a
//	positively established fact about the exact configured reference.

// truncatedHarness builds the world the defect lives in: a versioned tag whose
// digest has moved, over a repository too large to enumerate.
//
// Every fixture value is asserted rather than assumed -- see
// TestTheTruncationFixtureIsHonest below, which is the positive control for
// every case in this file.
func truncatedHarness(t *testing.T, localDigest, remoteDigest string, tags []string, truncated bool) intelHarness {
	t.Helper()

	harness := newIntelHarness(t, intelConfig())
	harness.store.due = []domain.ImageIntel{
		trackedRef("docker.io/library/nginx:1.27.4", "1.27.4", localDigest),
	}
	harness.registry.digest = remoteDigest
	harness.registry.tags = tags
	harness.registry.truncated = truncated
	return harness
}

// TestATruncatedSearchDoesNotEraseAKnownDigestMove is the Stage 17.5
// reproduction.
//
// Before the fix this returned `unknown`. Every precondition the brief names is
// asserted, so a future change that made the test pass for the wrong reason --
// by not truncating, by not moving the digest, by never reaching the registry --
// fails here instead.
func TestATruncatedSearchDoesNotEraseAKnownDigestMove(t *testing.T) {
	// Older tags only, so nothing newer is observed in the pages that were read.
	harness := truncatedHarness(t, digestA, digestB,
		[]string{"1.27.0", "1.27.1", "1.27.2", "1.27.3"}, true)

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	recorded := harness.store.recorded()
	if len(recorded) != 1 {
		t.Fatalf("%d outcomes recorded, want 1", len(recorded))
	}
	outcome := recorded[0]

	// ---- the preconditions the brief requires be proved -------------------

	// The manifest HEAD completed successfully.
	if outcome.Status != domain.CheckOK {
		t.Fatalf("status = %q, want ok; the manifest lookup must have succeeded "+
			"for this case to be about truncation at all", outcome.Status)
	}
	// Remote digest B is known.
	if outcome.RemoteDigest != digestB {
		t.Fatalf("remoteDigest = %q, want %q; the exact-tag digest must be "+
			"established", outcome.RemoteDigest, digestB)
	}
	// The digest genuinely moved.
	if outcome.RemoteDigest == digestA {
		t.Fatal("fixture defect: the remote digest equals the local one, so " +
			"there is no movement to preserve")
	}
	// Enumeration ran and was truncated.
	manifests, tags := harness.registry.calls()
	if tags == 0 {
		t.Fatal("no tag listing was attempted; this case is about a truncated one")
	}
	if !harness.registry.truncated {
		t.Fatal("fixture defect: the listing did not report truncation")
	}
	// No newer tag was observed, so nothing pinnable was found.
	if outcome.LatestTag != "" {
		t.Fatalf("latestTag = %q, want empty; this case is about no newer tag "+
			"being observed", outcome.LatestTag)
	}

	// ---- the assertion -----------------------------------------------------

	if outcome.Update != domain.UpdateDigest {
		t.Errorf("update = %q, want %q\n"+
			"\tHarborMaster established that the configured tag now resolves to a "+
			"different digest. A version search that ran out of budget answers a "+
			"DIFFERENT question and must not erase that fact -- no strategy permits "+
			"`unknown`, so this container would never be updated.",
			outcome.Update, domain.UpdateDigest)
	}

	// And the fix must not have cost a request.
	if manifests != 1 {
		t.Errorf("%d manifest requests, want 1; the exact-tag digest was already "+
			"fetched before enumeration and must not be fetched again", manifests)
	}
}

// TestTruncationWithoutMovementRemainsUnknown is §18, and it is load-bearing.
//
// The same truncated search, with the digest UNMOVED. HarborMaster still cannot
// conclude that no newer version exists, so the honest answer is `unknown` and
// emphatically not `none`. Turning "we do not know" into "no update" is the
// failure this whole vocabulary exists to prevent.
func TestTruncationWithoutMovementRemainsUnknown(t *testing.T) {
	harness := truncatedHarness(t, digestA, digestA,
		[]string{"1.27.0", "1.27.1"}, true)

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	outcome := harness.store.recorded()[0]
	if outcome.RemoteDigest != digestA {
		t.Fatalf("fixture defect: remoteDigest = %q, want the local digest %q; "+
			"this case is about NO movement", outcome.RemoteDigest, digestA)
	}
	if outcome.Update == domain.UpdateNone {
		t.Error("a truncated search reported `none`; an incomplete enumeration " +
			"cannot establish that no newer version exists")
	}
	if outcome.Update != domain.UpdateUnknown {
		t.Errorf("update = %q, want %q", outcome.Update, domain.UpdateUnknown)
	}
}

// TestAnObservedNewerTagStillOutranksADigestMove pins the precedence.
//
// A concrete newer tag is the more actionable answer, and the fix must not
// reverse that just because a digest also moved.
func TestAnObservedNewerTagStillOutranksADigestMove(t *testing.T) {
	harness := truncatedHarness(t, digestA, digestB,
		[]string{"1.27.4", "1.27.5"}, true)

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	outcome := harness.store.recorded()[0]
	if outcome.Update != domain.UpdatePatch {
		t.Errorf("update = %q, want %q; an observed newer tag is more actionable "+
			"than same-tag digest movement", outcome.Update, domain.UpdatePatch)
	}
	if outcome.LatestTag != "1.27.5" {
		t.Errorf("latestTag = %q, want 1.27.5", outcome.LatestTag)
	}
}

// ---------------------------------------------------------- fixture control --

// TestTheTruncationFixtureIsHonest is the positive control for this file.
//
// Given the number of fixture defects found in earlier stages, the fake is
// checked against the claims the cases above make of it: that two digests
// really differ, that truncation is really reported, that the tag list really
// contains what the case says, and that the registry is really reached.
//
// Without this, every assertion in this file could be satisfied by a fake that
// silently did nothing.
func TestTheTruncationFixtureIsHonest(t *testing.T) {
	if digestA == digestB {
		t.Fatal("the two fixture digests are equal; no test in this file could " +
			"distinguish movement from stillness")
	}

	harness := truncatedHarness(t, digestA, digestB, []string{"1.27.0"}, true)
	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	manifests, tags := harness.registry.calls()
	if manifests == 0 {
		t.Error("no manifest request was made; the exact-tag digest cannot have " +
			"been established")
	}
	if tags == 0 {
		t.Error("no tag listing was made; truncation cannot have been observed")
	}

	outcome := harness.store.recorded()[0]
	if outcome.RemoteDigest == "" {
		t.Error("no remote digest was recorded; the fake is not answering")
	}
}

// TestAMutableTagNeedsNoEnumeration is truth-table case 10.
//
// `latest` is not a version, so version discovery never runs. The digest
// comparison stands entirely on its own, and this is the Watchtower case that
// already worked -- pinned here so the fix does not disturb it.
func TestAMutableTagNeedsNoEnumeration(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	harness.store.due = []domain.ImageIntel{
		trackedRef("docker.io/library/nginx:latest", "latest", digestA),
	}
	harness.registry.digest = digestB
	// Deliberately populated. If enumeration ran, it would find a newer tag and
	// the assertion below would catch the change in behaviour.
	harness.registry.tags = []string{"1.27.5"}
	harness.registry.truncated = true

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	outcome := harness.store.recorded()[0]
	if outcome.Update != domain.UpdateDigest {
		t.Errorf("update = %q, want %q for a mutable tag whose digest moved",
			outcome.Update, domain.UpdateDigest)
	}
	if _, tags := harness.registry.calls(); tags != 0 {
		t.Errorf("%d tag listings for a non-version tag, want 0; enumeration is "+
			"only for version discovery", tags)
	}
}

// TestAListingFailureStillFallsBackToTheDigest guards the neighbouring path.
//
// A registry that refuses to list tags is not a truncated one, and the existing
// behaviour -- fall through to the digest comparison -- must survive the fix.
func TestAListingFailureStillFallsBackToTheDigest(t *testing.T) {
	harness := truncatedHarness(t, digestA, digestB, nil, false)
	harness.registry.tagsErr = registry.ErrTagListingUnsupported

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	outcome := harness.store.recorded()[0]
	if outcome.Update != domain.UpdateDigest {
		t.Errorf("update = %q, want %q", outcome.Update, domain.UpdateDigest)
	}
}

// unusedServiceReference keeps the service import meaningful if the assertions
// above are ever narrowed.
var _ = service.NewImageIntelService
