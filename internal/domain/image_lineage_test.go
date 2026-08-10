package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

func mustRef(t *testing.T, reference string) domain.NormalizedRef {
	t.Helper()
	parsed, err := domain.NormalizeImageRef(reference)
	if err != nil {
		t.Fatalf("normalise %q: %v", reference, err)
	}
	return parsed
}

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func trackedLineage(t *testing.T, reference, running string) domain.ImageLineage {
	t.Helper()
	ref := mustRef(t, reference)
	return domain.ImageLineage{
		ContainerName:     "web",
		State:             domain.LineageTracked,
		Origin:            domain.LineageObserved,
		TrackingReference: ref.Canonical,
		TrackingFamiliar:  ref.Familiar,
		Repository:        ref.Path,
		RunningDigest:     running,
	}
}

func intelFor(t *testing.T, lineage domain.ImageLineage, remote string) domain.ImageIntel {
	t.Helper()
	ref := mustRef(t, lineage.TrackingReference)
	return domain.ImageIntel{
		Reference:    ref.Canonical,
		Familiar:     ref.Familiar,
		Repository:   ref.Path,
		Tag:          ref.Tag,
		RemoteDigest: remote,
		Status:       domain.CheckOK,
		Update:       domain.UpdateNone,
	}
}

// ------------------------------------------------------------ establishment --

func TestATaggedContainerBecomesTracked(t *testing.T) {
	got := domain.EstablishLineage("web", "abc", mustRef(t, "nginx:1.27"), digestA, time.Now())

	if !got.Tracked() {
		t.Fatalf("a container deployed from a tag must be tracked: %+v", got)
	}
	if got.TrackingReference != "docker.io/library/nginx:1.27" {
		t.Errorf("TrackingReference = %q", got.TrackingReference)
	}
	if got.Repository != "library/nginx" {
		t.Errorf("Repository = %q", got.Repository)
	}
	if got.RunningDigest != digestA {
		t.Errorf("RunningDigest = %q", got.RunningDigest)
	}
	if got.Origin != domain.LineageObserved {
		t.Errorf("Origin = %q, want observed", got.Origin)
	}
}

// The rule the whole feature depends on being conservative about: HarborMaster
// must never invent a tag for a container someone deliberately pinned.
func TestADigestPinnedContainerIsNotGivenAnInventedTag(t *testing.T) {
	got := domain.EstablishLineage("web", "abc",
		mustRef(t, "nginx@"+digestA), digestA, time.Now())

	if got.Tracked() {
		t.Fatal("a digest-pinned container must not be given a tracking reference")
	}
	if got.State != domain.LineageUntracked {
		t.Errorf("State = %q, want untracked", got.State)
	}
	if got.TrackingReference != "" {
		t.Errorf("TrackingReference = %q, want empty", got.TrackingReference)
	}
	// The digest is still recorded: it is what a later reconciliation compares.
	if got.RunningDigest != digestA {
		t.Errorf("RunningDigest = %q", got.RunningDigest)
	}
}

// ------------------------------------------------------------------ labels --

func TestALabelCannotConferManagementOnItsOwn(t *testing.T) {
	_, refusal := domain.LineageFromLabel(
		"nginx:1.27", mustRef(t, "nginx@"+digestA), false)

	if refusal != domain.LineageRefusalUncorroborated {
		t.Fatalf("refusal = %q, want uncorroborated; an unmanaged container must not be "+
			"able to declare itself managed", refusal)
	}
}

// The cross-repository substitution guard. A label may say WHICH TAG of the
// running repository to follow; it may never say which repository.
func TestALabelCannotPointLineageAtAnotherRepository(t *testing.T) {
	for _, claim := range []string{
		"evil.example.com/attacker/app:latest",
		"docker.io/attacker/nginx:1.27",
		"nginx-but-different:1.27",
	} {
		_, refusal := domain.LineageFromLabel(
			claim, mustRef(t, "nginx@"+digestA), true)
		if refusal != domain.LineageRefusalRepositoryMismatch {
			t.Errorf("claim %q: refusal = %q, want repositoryMismatch", claim, refusal)
		}
	}
}

func TestALabelMustNameATagNotADigest(t *testing.T) {
	_, refusal := domain.LineageFromLabel(
		"nginx@"+digestB, mustRef(t, "nginx@"+digestA), true)

	if refusal != domain.LineageRefusalNotTag {
		t.Fatalf("refusal = %q, want notATag", refusal)
	}
}

func TestAMalformedLabelIsRefused(t *testing.T) {
	for _, claim := range []string{
		"::::", "not a reference", strings.Repeat("a", domain.MaxLineageReferenceBytes+1),
		"nginx:" + strings.Repeat("t", 400),
	} {
		_, refusal := domain.LineageFromLabel(claim, mustRef(t, "nginx@"+digestA), true)
		if refusal == domain.LineageRefusalNone {
			t.Errorf("claim %q was accepted; it must not be", claim)
		}
	}
}

func TestACorroboratedSameRepositoryLabelIsAccepted(t *testing.T) {
	got, refusal := domain.LineageFromLabel(
		"nginx:1.27", mustRef(t, "nginx@"+digestA), true)

	if refusal != domain.LineageRefusalNone {
		t.Fatalf("refusal = %q, want acceptance", refusal)
	}
	if got.Canonical != "docker.io/library/nginx:1.27" {
		t.Errorf("Canonical = %q", got.Canonical)
	}
}

// -------------------------------------------------------------- evaluation --

// The regression this whole change exists for: a container moved onto a digest
// must still be seen to have an update when its tracking tag moves.
func TestADigestPinnedManagedContainerStillSeesItsTagMove(t *testing.T) {
	lineage := trackedLineage(t, "nginx:latest", digestA)
	intel := intelFor(t, lineage, digestB)

	got := domain.EvaluateLineage(lineage, intel, digestA)

	if !got.Usable {
		t.Fatalf("verdict is not usable: %+v", got)
	}
	if got.Update != domain.UpdateDigest {
		t.Errorf("Update = %q, want digest", got.Update)
	}
	if got.Digest != digestB {
		t.Errorf("Digest = %q, want the registry's digest", got.Digest)
	}
	if got.Reference != "docker.io/library/nginx:latest" {
		t.Errorf("Reference = %q, want the tracking reference", got.Reference)
	}
}

func TestNoUpdateWhenTheTagResolvesToWhatIsRunning(t *testing.T) {
	lineage := trackedLineage(t, "nginx:latest", digestA)
	intel := intelFor(t, lineage, digestA)

	got := domain.EvaluateLineage(lineage, intel, digestA)

	if !got.Usable {
		t.Fatal("a completed check that found nothing is a usable verdict")
	}
	if got.Update != domain.UpdateNone {
		t.Errorf("Update = %q, want none", got.Update)
	}
}

// A newer tag is preferred over a bare digest move, because the policy ceiling
// an operator wrote is about version size.
func TestANewerTagIsPreferredOverADigestMove(t *testing.T) {
	lineage := trackedLineage(t, "nginx:1.27", digestA)
	intel := intelFor(t, lineage, digestB)
	intel.Update = domain.UpdateMinor
	intel.LatestTag = "1.28"
	intel.LatestDigest = digestC

	got := domain.EvaluateLineage(lineage, intel, digestA)

	if got.Update != domain.UpdateMinor {
		t.Fatalf("Update = %q, want minor", got.Update)
	}
	if got.Digest != digestC {
		t.Errorf("Digest = %q, want the newer tag's digest", got.Digest)
	}
	if got.Reference != "docker.io/library/nginx:1.28" {
		t.Errorf("Reference = %q, want the newer tag in the SAME repository", got.Reference)
	}
}

// Every non-answer must be unusable rather than "no update".
func TestUnestablishedEvidenceIsNeverReportedAsNoUpdate(t *testing.T) {
	base := trackedLineage(t, "nginx:latest", digestA)

	cases := map[string]struct {
		lineage domain.ImageLineage
		intel   domain.ImageIntel
		running string
	}{
		"untracked container": {
			lineage: domain.ImageLineage{ContainerName: "web", State: domain.LineageUntracked},
			intel:   intelFor(t, base, digestB),
			running: digestA,
		},
		"intelligence for another reference": func() struct {
			lineage domain.ImageLineage
			intel   domain.ImageIntel
			running string
		} {
			other := intelFor(t, base, digestB)
			other.Reference = "docker.io/library/redis:latest"
			return struct {
				lineage domain.ImageLineage
				intel   domain.ImageIntel
				running string
			}{base, other, digestA}
		}(),
		"intelligence for another repository": func() struct {
			lineage domain.ImageLineage
			intel   domain.ImageIntel
			running string
		} {
			other := intelFor(t, base, digestB)
			other.Repository = "library/redis"
			return struct {
				lineage domain.ImageLineage
				intel   domain.ImageIntel
				running string
			}{base, other, digestA}
		}(),
		"check did not succeed": func() struct {
			lineage domain.ImageLineage
			intel   domain.ImageIntel
			running string
		} {
			stale := intelFor(t, base, digestB)
			stale.Status = domain.CheckFailed
			return struct {
				lineage domain.ImageLineage
				intel   domain.ImageIntel
				running string
			}{base, stale, digestA}
		}(),
		"running digest unknown": {
			lineage: trackedLineage(t, "nginx:latest", ""),
			intel:   intelFor(t, base, digestB),
			running: "",
		},
		"registry reported no digest": {
			lineage: base,
			intel:   intelFor(t, base, ""),
			running: digestA,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := domain.EvaluateLineage(tc.lineage, tc.intel, tc.running)
			if got.Usable {
				t.Fatalf("verdict reported usable: %+v", got)
			}
			if got.Update != domain.UpdateNone && got.Update != domain.UpdateUnknown {
				t.Errorf("Update = %q", got.Update)
			}
			if got.Usable && got.Update == domain.UpdateNone {
				t.Error("an unestablished verdict was reported as 'no update'")
			}
			if got.Digest != "" || got.Reference != "" {
				t.Errorf("an unusable verdict proposed something: %+v", got)
			}
		})
	}
}

// --------------------------------------------------------------- advancing --

func TestAVerifiedRecreationKeepsTheTrackingReference(t *testing.T) {
	before := trackedLineage(t, "nginx:latest", digestA)

	after := domain.AdvanceLineage(before, mustRef(t, "nginx:latest"), digestB, "newid", time.Now())

	if after.TrackingReference != before.TrackingReference {
		t.Errorf("tracking reference changed: %q -> %q", before.TrackingReference, after.TrackingReference)
	}
	if after.RunningDigest != digestB {
		t.Errorf("RunningDigest = %q, want the approved digest", after.RunningDigest)
	}
	if after.Origin != domain.LineageRecreated {
		t.Errorf("Origin = %q, want recreation", after.Origin)
	}
	if after.ContainerID != "newid" {
		t.Errorf("ContainerID = %q", after.ContainerID)
	}
}

// A series upgrade moves the tag being followed too, or the container would
// track a tag that can never move again -- the same bug in a new place.
func TestASeriesUpgradeAdvancesTheTrackedTag(t *testing.T) {
	before := trackedLineage(t, "nginx:1.27", digestA)

	after := domain.AdvanceLineage(before, mustRef(t, "nginx:1.28"), digestC, "newid", time.Now())

	if after.TrackingReference != "docker.io/library/nginx:1.28" {
		t.Errorf("TrackingReference = %q, want the approved tag", after.TrackingReference)
	}
	if after.RunningDigest != digestC {
		t.Errorf("RunningDigest = %q", after.RunningDigest)
	}
}

func TestAdvancingCannotMoveLineageToAnotherRepository(t *testing.T) {
	before := trackedLineage(t, "nginx:1.27", digestA)

	after := domain.AdvanceLineage(before, mustRef(t, "redis:7"), digestC, "newid", time.Now())

	if after.TrackingReference != before.TrackingReference {
		t.Errorf("lineage moved repository: %q", after.TrackingReference)
	}
	if after.Repository != "library/nginx" {
		t.Errorf("Repository = %q", after.Repository)
	}
}

// A digest-only approved reference must not erase what the container follows.
func TestAdvancingWithADigestOnlyReferenceKeepsTracking(t *testing.T) {
	before := trackedLineage(t, "nginx:latest", digestA)

	after := domain.AdvanceLineage(before, mustRef(t, "nginx@"+digestB), digestB, "newid", time.Now())

	if !after.Tracked() {
		t.Fatal("tracking was lost when the approved reference carried only a digest")
	}
	if after.TrackingReference != "docker.io/library/nginx:latest" {
		t.Errorf("TrackingReference = %q", after.TrackingReference)
	}
	if after.RunningDigest != digestB {
		t.Errorf("RunningDigest = %q", after.RunningDigest)
	}
}

// ---------------------------------------------------------------- rollback --

func TestRollbackReturnsLineageToTheRestoredDigest(t *testing.T) {
	before := trackedLineage(t, "nginx:latest", digestA)
	attempted := domain.AdvanceLineage(before, mustRef(t, "nginx:latest"), digestB, "newid", time.Now())

	restored := domain.RestoreLineage(attempted, digestA, "originalid", time.Now())

	if restored.RunningDigest != digestA {
		t.Fatalf("RunningDigest = %q, want the digest that is actually running again", restored.RunningDigest)
	}
	if restored.TrackingReference != "docker.io/library/nginx:latest" {
		t.Errorf("rollback lost the tracking reference: %q", restored.TrackingReference)
	}
	if restored.ContainerID != "originalid" {
		t.Errorf("ContainerID = %q", restored.ContainerID)
	}

	// And the next cycle must still see B on offer.
	intel := intelFor(t, restored, digestB)
	next := domain.EvaluateLineage(restored, intel, restored.RunningDigest)
	if next.Update != domain.UpdateDigest || next.Digest != digestB {
		t.Errorf("after a rollback the next cycle did not re-offer the update: %+v", next)
	}
}
