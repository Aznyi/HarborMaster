package registry

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/config"
)

// Stage 17.5 §6: the registry bounds, pinned by value.
//
// # Why this test exists at all
//
// Stage 17.5 fixed a defect whose SYMPTOM was "nginx and redis report unknown".
// The obvious wrong fix is to raise the tag-page budget until those two
// repositories happen to fit, which cures the symptom for exactly as long as it
// takes a repository to grow past the new number, and costs every check on
// every host more registry traffic in the meantime.
//
// The actual fix was control flow: an answer HarborMaster already had stopped
// being discarded. Not one bound moved, and this test is what says so in a form
// that fails the build rather than in a sentence in a design document.
//
// # What to do when this fails
//
// If a change genuinely needs a different bound, change the number here AND say
// why in the commit. The point is not that these values are sacred; it is that
// moving one must be a decision somebody made on purpose, rather than a way of
// making a symptom go away.

// TestTheTagListingBoundsAreUnchanged pins the four caps that decide how much
// of a repository one enumeration may read.
func TestTheTagListingBoundsAreUnchanged(t *testing.T) {
	bounds := []struct {
		name string
		got  int
		want int
		why  string
	}{
		{
			name: "maxTagsPerPage", got: maxTagsPerPage, want: 1024,
			why: "how many tags are accepted from one page, whatever the registry sends",
		},
		{
			name: "maxTrackedTags", got: maxTrackedTags, want: 4096,
			why: "how many tags are held in memory across all pages of one walk",
		},
		{
			name: "maxTagsBytes", got: maxTagsBytes, want: 4 << 20,
			why: "the response-size cap on one tag page, which bounds a hostile registry",
		},
		{
			name: "DefaultImageIntelMaxTagPages", got: config.DefaultImageIntelMaxTagPages, want: 5,
			why: "the default page budget; the Stage 17.5 fix works at this value " +
				"and needs no more",
		},
	}

	for _, bound := range bounds {
		if bound.got != bound.want {
			t.Errorf("%s = %d, want %d\n\t%s\n"+
				"\tStage 17.5 was a control-flow correction and moved no bound. "+
				"If this changed to make a repository fit, that is the wrong fix: "+
				"the next repository will not fit either.",
				bound.name, bound.got, bound.want, bound.why)
		}
	}
}

// TestTheProviderPageSizesAreUnchanged pins how much is requested per page.
//
// A page size is a politeness setting as much as a bound: it is what one
// request asks a registry for, and raising it to fit more tags into the same
// page budget would be the same wrong fix wearing a different hat.
func TestTheProviderPageSizesAreUnchanged(t *testing.T) {
	sizes := map[string]struct {
		got  int
		want int
	}{
		"dockerHub": {dockerHubProvider{}.TagPageSize(), 100},
		"ghcr":      {ghcrProvider{}.TagPageSize(), 100},
		"oci":       {ociProvider{}.TagPageSize(), 50},
	}

	for name, size := range sizes {
		if size.got != size.want {
			t.Errorf("%s TagPageSize = %d, want %d", name, size.got, size.want)
		}
	}
}

// The BEHAVIOURAL half of this rule already exists and is deliberately not
// duplicated here:
//
//	TestTagsPaginate                     -- the walk uses the `last` cursor
//	TestTagsReportTruncationAtThePageBudget -- it stops at the budget and says so
//	TestTagsIgnoreTheLinkHeader          -- it will not follow a registry URL
//	TestOversizedResponsesAreRefused     -- the body cap holds
//	TestRequestTimeout                   -- the deadline holds
//
// Those establish that the loop honours its bounds. This file establishes that
// the bounds themselves did not move, which is the part a symptom-driven fix
// would have changed.
