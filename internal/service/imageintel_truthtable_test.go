package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/registry"
)

// Stage 17.5: the complete assessment truth table.
//
// # Why a table rather than a case per behaviour
//
// The defect this stage fixed was an INTERACTION: two signals, each correct on
// its own, combined in the wrong order. A suite of individually reasonable
// tests is how that survived, because no single test looked at both axes at
// once.
//
// So the axes are enumerated explicitly -- truncated, digest moved, newer tag
// observed, candidate pinnable -- and every meaningful combination names the
// UpdateType it must produce. A future change that alters one cell has to say
// which cell and why.

const digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// truthCase is one row of the table.
type truthCase struct {
	name string

	// The world.
	configuredTag string
	localDigest   string
	remoteDigest  string
	tags          []string
	truncated     bool
	// candidateUnpinnable names a tag whose manifest lookup fails, which is how
	// "a newer tag exists but cannot be pinned" is reached.
	candidateUnpinnable string

	// The verdict.
	wantUpdate domain.UpdateType
	wantTag    string
	// wantReasonContains is a fragment of HarborMaster's own sentence. Checked
	// as a fragment rather than in full so a wording improvement does not break
	// the table, but checked at all so the operator-facing distinction between
	// "digest moved" and "search incomplete" is pinned.
	wantReasonContains string

	why string
}

func (c truthCase) run(t *testing.T) {
	t.Helper()

	harness := newIntelHarness(t, intelConfig())
	harness.store.due = []domain.ImageIntel{
		trackedRef("docker.io/library/nginx:"+c.configuredTag, c.configuredTag, c.localDigest),
	}
	harness.registry.digest = c.remoteDigest
	harness.registry.tags = c.tags
	harness.registry.truncated = c.truncated

	if c.candidateUnpinnable != "" {
		harness.registry.errByTag = map[string]error{
			c.candidateUnpinnable: errors.New("manifest unavailable"),
		}
		// And the configured tag must still answer, or the case would be about
		// a failed check rather than an unpinnable candidate.
		harness.registry.digestByTag = map[string]string{
			c.configuredTag: c.remoteDigest,
		}
	}

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	recorded := harness.store.recorded()
	if len(recorded) != 1 {
		t.Fatalf("%d outcomes, want 1", len(recorded))
	}
	outcome := recorded[0]

	if outcome.Update != c.wantUpdate {
		t.Errorf("update = %q, want %q\n\t%s", outcome.Update, c.wantUpdate, c.why)
	}
	if outcome.LatestTag != c.wantTag {
		t.Errorf("latestTag = %q, want %q", outcome.LatestTag, c.wantTag)
	}
	if c.wantReasonContains != "" &&
		!strings.Contains(outcome.UpdateReason, c.wantReasonContains) {
		t.Errorf("reason = %q\n\twant it to contain %q", outcome.UpdateReason, c.wantReasonContains)
	}
}

// TestTheAssessmentTruthTable walks every meaningful combination.
func TestTheAssessmentTruthTable(t *testing.T) {
	cases := []truthCase{
		{
			name:               "1. complete search, nothing newer, digest still",
			configuredTag:      "1.27.4",
			localDigest:        digestA,
			remoteDigest:       digestA,
			tags:               []string{"1.27.0", "1.27.4"},
			truncated:          false,
			wantUpdate:         domain.UpdateNone,
			wantReasonContains: "already in use",
			why:                "everything was established and nothing has changed",
		},
		{
			name:               "2. complete search, nothing newer, digest moved",
			configuredTag:      "1.27.4",
			localDigest:        digestA,
			remoteDigest:       digestB,
			tags:               []string{"1.27.0", "1.27.4"},
			truncated:          false,
			wantUpdate:         domain.UpdateDigest,
			wantReasonContains: "republished",
			why:                "the publisher moved the tag; the search confirmed nothing newer",
		},
		{
			name:               "3. truncated search, nothing newer seen, digest still",
			configuredTag:      "1.27.4",
			localDigest:        digestA,
			remoteDigest:       digestA,
			tags:               []string{"1.27.0"},
			truncated:          true,
			wantUpdate:         domain.UpdateUnknown,
			wantReasonContains: "search limit",
			why: "an incomplete search cannot establish that no newer version " +
				"exists; this must never become `none`",
		},
		{
			name:               "4. truncated search, nothing newer seen, digest moved",
			configuredTag:      "1.27.4",
			localDigest:        digestA,
			remoteDigest:       digestB,
			tags:               []string{"1.27.0"},
			truncated:          true,
			wantUpdate:         domain.UpdateDigest,
			wantReasonContains: "search limit",
			why: "THE STAGE 17.5 FIX. The configured tag's digest movement is " +
				"positively known; an unfinished search for something else must " +
				"not erase it",
		},
		{
			name:               "5. truncated search, newer patch seen, digest moved",
			configuredTag:      "1.27.4",
			localDigest:        digestA,
			remoteDigest:       digestB,
			tags:               []string{"1.27.4", "1.27.5"},
			truncated:          true,
			wantUpdate:         domain.UpdatePatch,
			wantTag:            "1.27.5",
			wantReasonContains: "same series",
			why:                "a concrete newer tag is the more actionable answer",
		},
		{
			name:               "6. truncated search, newer minor seen, digest moved",
			configuredTag:      "1.27.4",
			localDigest:        digestA,
			remoteDigest:       digestB,
			tags:               []string{"1.27.4", "1.28.0"},
			truncated:          true,
			wantUpdate:         domain.UpdateMinor,
			wantTag:            "1.28.0",
			wantReasonContains: "same series",
			why:                "a concrete newer tag outranks digest movement, whatever its size",
		},
		{
			name:               "7. truncated search, newer major seen, digest moved",
			configuredTag:      "1.27.4",
			localDigest:        digestA,
			remoteDigest:       digestB,
			tags:               []string{"1.27.4", "2.0.0"},
			truncated:          true,
			wantUpdate:         domain.UpdateMajor,
			wantTag:            "2.0.0",
			wantReasonContains: "same series",
			why:                "the strategy ceiling refuses it later; the ASSESSMENT still reports it",
		},
		{
			name:                "8. newer tag seen but its digest cannot be pinned",
			configuredTag:       "1.27.4",
			localDigest:         digestA,
			remoteDigest:        digestB,
			tags:                []string{"1.27.4", "1.27.5"},
			truncated:           false,
			candidateUnpinnable: "1.27.5",
			wantUpdate:          domain.UpdateUnknown,
			wantTag:             "",
			wantReasonContains:  "could not be",
			why: "acquisition is digest-pinned, so an unpinnable tag is not " +
				"something an operator can be offered",
		},
		{
			name:               "9. only a pre-release is newer",
			configuredTag:      "1.27.4",
			localDigest:        digestA,
			remoteDigest:       digestA,
			tags:               []string{"1.27.4", "1.28.0-rc1"},
			truncated:          false,
			wantUpdate:         domain.UpdatePrerelease,
			wantTag:            "1.28.0-rc1",
			wantReasonContains: "pre-release",
			why:                "existing pre-release semantics, preserved; no strategy permits it",
		},
		{
			name:               "10. mutable tag, digest moved, no enumeration",
			configuredTag:      "latest",
			localDigest:        digestA,
			remoteDigest:       digestB,
			tags:               []string{"1.27.5"},
			truncated:          true,
			wantUpdate:         domain.UpdateDigest,
			wantReasonContains: "republished",
			why: "`latest` is not a version, so discovery never runs and the " +
				"digest comparison stands alone. The Watchtower case.",
		},
		{
			name:               "11. truncated search, older tags only, digest moved to a third value",
			configuredTag:      "1.27.4",
			localDigest:        digestA,
			remoteDigest:       digestC,
			tags:               []string{"1.20.0", "1.21.0", "1.22.0"},
			truncated:          true,
			wantUpdate:         domain.UpdateDigest,
			wantReasonContains: "search limit",
			why:                "the same fix, with a digest that is neither fixture default",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) { testCase.run(t) })
	}
}

// -------------------------------------------------------- fail-closed set --

// TestFailClosedCasesSurviveTheFix is §4 of the brief.
//
// Each of these is a way HarborMaster can fail to establish something, and each
// must keep producing a verdict that no strategy permits. The fix touched the
// order of two signals; it must not have turned any absence of evidence into
// permission.
func TestFailClosedCasesSurviveTheFix(t *testing.T) {
	t.Run("A. truncated, nothing newer, no movement", func(t *testing.T) {
		truthCase{
			configuredTag: "1.27.4", localDigest: digestA, remoteDigest: digestA,
			tags: []string{"1.20.0"}, truncated: true,
			wantUpdate: domain.UpdateUnknown,
			why:        "unknown, and never none",
		}.run(t)
	})

	t.Run("B. no local digest to compare", func(t *testing.T) {
		truthCase{
			configuredTag: "1.27.4", localDigest: "", remoteDigest: digestB,
			tags: []string{"1.27.4"}, truncated: false,
			wantUpdate:         domain.UpdateUnknown,
			wantReasonContains: "no registry digest to compare",
			why: "a locally built image has nothing to compare, so a differing " +
				"remote digest establishes nothing",
		}.run(t)
	})

	t.Run("C. no remote digest for the configured tag", func(t *testing.T) {
		// The manifest answers, but with no digest. digestMoved treats an absent
		// digest on either side as an absence of evidence, never as a difference.
		truthCase{
			configuredTag: "1.27.4", localDigest: digestA, remoteDigest: "",
			tags: []string{"1.27.4"}, truncated: false,
			wantUpdate: domain.UpdateNone,
			why: "no UpdateDigest may be claimed without a remote digest; the " +
				"completed search having found nothing newer is what makes this none",
		}.run(t)
	})

	t.Run("C2. no remote digest and a truncated search", func(t *testing.T) {
		truthCase{
			configuredTag: "1.27.4", localDigest: digestA, remoteDigest: "",
			tags: []string{"1.20.0"}, truncated: true,
			wantUpdate: domain.UpdateUnknown,
			why: "no digest claim AND an unfinished search leaves nothing " +
				"established at all",
		}.run(t)
	})

	t.Run("D. candidate cannot be pinned", func(t *testing.T) {
		truthCase{
			configuredTag: "1.27.4", localDigest: digestA, remoteDigest: digestB,
			tags: []string{"1.27.4", "1.27.5"}, truncated: true,
			candidateUnpinnable: "1.27.5",
			wantUpdate:          domain.UpdateUnknown,
			wantTag:             "",
			why:                 "an unpinnable candidate is refused even when a digest also moved",
		}.run(t)
	})

	t.Run("E. the registry request fails", func(t *testing.T) {
		harness := newIntelHarness(t, intelConfig())
		harness.store.due = []domain.ImageIntel{
			trackedRef("docker.io/library/nginx:1.27.4", "1.27.4", digestA),
		}
		harness.registry.manifestErr = registry.ErrNotFound

		if _, err := harness.service.Collect(context.Background()); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		outcome := harness.store.recorded()[0]
		if outcome.Status == domain.CheckOK {
			t.Error("a failed manifest lookup reported an OK check")
		}
		if outcome.Update == domain.UpdateDigest {
			t.Error("a failed lookup claimed a digest update")
		}
	})
}

// ------------------------------------------------------------- requests --

// TestTheFixAddsNoRegistryRequests is §11 and §21.
//
// The success criterion for Stage 17.5 is better use of evidence already
// fetched, not more traffic. The exact-tag manifest is looked up ONCE, before
// enumeration, and the fix must not have added a second lookup to re-establish
// what was already known.
func TestTheFixAddsNoRegistryRequests(t *testing.T) {
	cases := []struct {
		name          string
		configuredTag string
		remote        string
		tags          []string
		truncated     bool
		wantManifests int
		wantTagLists  int
	}{
		{
			name:          "versioned tag, truncated, digest moved -- the fixed case",
			configuredTag: "1.27.4", remote: digestB,
			tags: []string{"1.20.0"}, truncated: true,
			// One HEAD for the configured tag. No candidate was found, so no
			// second manifest. One tag listing.
			wantManifests: 1, wantTagLists: 1,
		},
		{
			name:          "versioned tag, complete, nothing newer",
			configuredTag: "1.27.4", remote: digestA,
			tags: []string{"1.27.4"}, truncated: false,
			wantManifests: 1, wantTagLists: 1,
		},
		{
			name:          "versioned tag, newer candidate -- one extra lookup, to pin it",
			configuredTag: "1.27.4", remote: digestB,
			tags: []string{"1.27.4", "1.27.5"}, truncated: false,
			// The second manifest is the CANDIDATE's, which is required: a tag
			// without its own digest cannot be proposed.
			wantManifests: 2, wantTagLists: 1,
		},
		{
			name:          "mutable tag -- no enumeration at all",
			configuredTag: "latest", remote: digestB,
			tags: []string{"1.27.5"}, truncated: true,
			wantManifests: 1, wantTagLists: 0,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newIntelHarness(t, intelConfig())
			harness.store.due = []domain.ImageIntel{
				trackedRef("docker.io/library/nginx:"+testCase.configuredTag,
					testCase.configuredTag, digestA),
			}
			harness.registry.digest = testCase.remote
			harness.registry.tags = testCase.tags
			harness.registry.truncated = testCase.truncated

			if _, err := harness.service.Collect(context.Background()); err != nil {
				t.Fatalf("Collect: %v", err)
			}

			manifests, tagLists := harness.registry.calls()
			if manifests != testCase.wantManifests {
				t.Errorf("manifest requests = %d, want %d (looked up %v)",
					manifests, testCase.wantManifests, harness.registry.lookedUp())
			}
			if tagLists != testCase.wantTagLists {
				t.Errorf("tag listings = %d, want %d", tagLists, testCase.wantTagLists)
			}

			// And the FIRST manifest is always the configured tag's, never a
			// candidate's: the exact-tag fact is established before anything
			// else is attempted.
			if looked := harness.registry.lookedUp(); len(looked) > 0 &&
				looked[0] != testCase.configuredTag {
				t.Errorf("first manifest lookup was for %q, want the configured tag %q",
					looked[0], testCase.configuredTag)
			}
		})
	}
}

// TestOneAssessmentWalksThePagesOnce guards §14.
//
// The restructured assess holds the discovery result in a variable rather than
// re-running the comparison in each branch. A second walk would double the
// registry load on every check of every versioned image on the host.
func TestOneAssessmentWalksThePagesOnce(t *testing.T) {
	harness := newIntelHarness(t, intelConfig())
	harness.store.due = []domain.ImageIntel{
		trackedRef("docker.io/library/nginx:1.27.4", "1.27.4", digestA),
	}
	harness.registry.digest = digestB
	harness.registry.tags = []string{"1.20.0"}
	harness.registry.truncated = true

	if _, err := harness.service.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if _, tagLists := harness.registry.calls(); tagLists != 1 {
		t.Errorf("%d tag listings in one assessment, want exactly 1", tagLists)
	}
}
