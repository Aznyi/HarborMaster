package service

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The Stage 17.9 drift regression, at the seam where it actually bit.
//
// # What live acceptance saw
//
// A container following `alpine:3.22`, running the digest that tag used to
// resolve to, on a host where `alpine:3.22` had been republished and a newer
// `alpine:3.24` existed. The planner correctly proposed the tag it follows at
// the tag's current digest. Every acquisition was then refused:
//
//	"the digest on offer has changed since the plan was written"
//
// Nothing had changed. The acquisition preflight re-derived the planner's
// CHOICE from the same evidence using a second implementation of the preference
// rule, got the newer tag, and compared it against a plan that named the
// tracking tag. Two opinions about one registry answer, and the safer-sounding
// one won by refusing forever.
//
// The check no longer holds an opinion: it asks which images the registry is
// serving, and the plan has to be one of them.
func TestBothCurrentlyServedTargetsAreOnOffer(t *testing.T) {
	// The exact shape read off the live rig.
	intel := domain.ImageIntel{
		Familiar:     "alpine:3.22",
		LocalDigest:  "sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d",
		RemoteDigest: "sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce",
		LatestTag:    "3.24",
		LatestDigest: "sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b",
		Update:       domain.UpdateMinor,
	}

	targets := offeredTargets(intel)
	if len(targets) != 2 {
		t.Fatalf("offeredTargets returned %d targets, want 2 "+
			"(the tracking reference and the newer tag)", len(targets))
	}

	byReference := make(map[string]string, len(targets))
	for _, target := range targets {
		if !target.Valid() {
			t.Fatalf("an invalid target reached the offer list: %+v", target)
		}
		byReference[target.Reference()] = target.Digest()
	}

	// The one the planner proposes for a container behind on its own tag. This
	// is the entry whose absence refused every Follow-current-tag acquisition.
	if got := byReference["alpine:3.22"]; got != intel.RemoteDigest {
		t.Errorf("the tracking reference is not on offer (got %q)\n"+
			"\ta plan that follows a moved tag is then refused as though the "+
			"registry had changed underneath it, forever", got)
	}
	// And the version move is still on offer, so nothing was traded away.
	if got := byReference["alpine:3.24"]; got != intel.LatestDigest {
		t.Errorf("the newer tag is not on offer (got %q)", got)
	}
}

// A target is only on offer when the registry actually resolved it.
func TestAnUnresolvedTagIsNeverOnOffer(t *testing.T) {
	intel := domain.ImageIntel{
		Familiar:     "alpine:3.22",
		RemoteDigest: "sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce",
		LatestTag:    "3.24",
		LatestDigest: "", // listed, never resolved
	}
	targets := offeredTargets(intel)
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want only the tracking reference", len(targets))
	}
	if targets[0].Reference() != "alpine:3.22" {
		t.Errorf("Reference = %q", targets[0].Reference())
	}

	// And with nothing resolved at all, nothing is on offer.
	intel.RemoteDigest = ""
	if got := offeredTargets(intel); len(got) != 0 {
		t.Errorf("got %d targets, want none: an unpinnable image is not an offer", len(got))
	}
}
