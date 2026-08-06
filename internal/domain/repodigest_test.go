package domain_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Resolving the running image's digest, and refusing to guess.
//
// These are the regression tests for the defect that made image acquisition,
// container recreation, and rollback unreachable: a tag-referenced container
// had no local digest, which forced every change plan to "cannot advise", which
// made acquisition refuse every plan.
//
// The tests that matter most are the ones proving what is NOT returned. A wrong
// digest here is a digest substitution: HarborMaster would compare a foreign
// repository's content against this repository's registry answer and propose a
// change built on it.

func digestOf(fill string) string { return "sha256:" + strings.Repeat(fill, 64) }

func mustNormalize(t *testing.T, reference string) domain.NormalizedRef {
	t.Helper()
	ref, err := domain.NormalizeImageRef(reference)
	if err != nil {
		t.Fatalf("normalize %q: %v", reference, err)
	}
	return ref
}

// TestATagReferencedImageResolvesItsRepoDigest is the defect, stated directly.
func TestATagReferencedImageResolvesItsRepoDigest(t *testing.T) {
	want := digestOf("a")
	ref := mustNormalize(t, "nginx:1.27.0-alpine")

	got, match := domain.SelectRepoDigest([]string{"nginx@" + want}, ref)

	if match != domain.RepoDigestResolved {
		t.Fatalf("match = %q, want resolved", match)
	}
	if got != want {
		t.Errorf("digest = %q, want %q", got, want)
	}
}

// TestTheFamiliarAndCanonicalSpellingsResolveTheSame.
//
// Docker reports RepoDigests in whichever form the image was pulled by. All of
// these name the same repository, and a resolver that matched on the string
// rather than on the normalized repository would accept only one of them.
func TestTheFamiliarAndCanonicalSpellingsResolveTheSame(t *testing.T) {
	want := digestOf("b")
	ref := mustNormalize(t, "redis:7.2-alpine")

	for _, entry := range []string{
		"redis@" + want,
		"library/redis@" + want,
		"docker.io/library/redis@" + want,
	} {
		got, match := domain.SelectRepoDigest([]string{entry}, ref)
		if match != domain.RepoDigestResolved || got != want {
			t.Errorf("%q resolved to %q/%q, want %q/resolved", entry, got, match, want)
		}
	}
}

// TestADigestFromAnotherRepositoryIsNeverUsed is the substitution guard.
//
// One local image can carry RepoDigests for several repositories -- an image
// pulled from Docker Hub and pushed to a private registry has two, and they are
// different manifests. Returning the wrong one would silently compare content
// from somewhere else against this repository's registry answer.
func TestADigestFromAnotherRepositoryIsNeverUsed(t *testing.T) {
	foreign := digestOf("c")
	ref := mustNormalize(t, "nginx:1.27.0-alpine")

	cases := map[string][]string{
		"a different repository on the same registry": {"httpd@" + foreign},
		"the same name on a different registry":       {"ghcr.io/acme/nginx@" + foreign},
		"a different namespace":                       {"acme/nginx@" + foreign},
		"a longer path that merely ends the same":     {"quay.io/team/nginx@" + foreign},
	}

	for name, repoDigests := range cases {
		t.Run(name, func(t *testing.T) {
			got, match := domain.SelectRepoDigest(repoDigests, ref)
			if got != "" {
				t.Errorf("returned %q from another repository", got)
			}
			if match != domain.RepoDigestNone {
				t.Errorf("match = %q, want none", match)
			}
		})
	}
}

// TestTheRightRepositoryIsFoundAmongForeignOnes.
func TestTheRightRepositoryIsFoundAmongForeignOnes(t *testing.T) {
	want := digestOf("d")
	foreign := digestOf("e")
	ref := mustNormalize(t, "nginx:1.27.0-alpine")

	got, match := domain.SelectRepoDigest([]string{
		"ghcr.io/acme/nginx@" + foreign,
		"nginx@" + want,
		"quay.io/other/nginx@" + foreign,
	}, ref)

	if match != domain.RepoDigestResolved || got != want {
		t.Fatalf("digest = %q/%q, want %q/resolved", got, match, want)
	}
}

// TestConflictingDigestsForOneRepositoryAreAmbiguous.
//
// Two different digests for the same repository means the daemon cannot say
// which one this image is. Picking either would be a coin flip printed as a
// fact, so the answer is unknown -- which costs a recommendation and nothing
// else.
func TestConflictingDigestsForOneRepositoryAreAmbiguous(t *testing.T) {
	ref := mustNormalize(t, "nginx:1.27.0-alpine")

	got, match := domain.SelectRepoDigest([]string{
		"nginx@" + digestOf("a"),
		"nginx@" + digestOf("b"),
	}, ref)

	if got != "" {
		t.Errorf("returned %q for a conflicted repository", got)
	}
	if match != domain.RepoDigestAmbiguous {
		t.Errorf("match = %q, want ambiguous", match)
	}
}

// TestDuplicateIdenticalDigestsAreNotAmbiguous.
//
// The same digest listed twice, or once per spelling, is one answer rather than
// a conflict.
func TestDuplicateIdenticalDigestsAreNotAmbiguous(t *testing.T) {
	want := digestOf("f")
	ref := mustNormalize(t, "nginx:1.27.0-alpine")

	got, match := domain.SelectRepoDigest([]string{
		"nginx@" + want,
		"docker.io/library/nginx@" + want,
	}, ref)

	if match != domain.RepoDigestResolved || got != want {
		t.Fatalf("digest = %q/%q, want %q/resolved", got, match, want)
	}
}

// TestTheOrderOfRepoDigestsDoesNotChangeTheAnswer.
//
// Determinism: Docker does not promise an order, and an answer that depended on
// it would be irreproducible between two inspections of the same image.
func TestTheOrderOfRepoDigestsDoesNotChangeTheAnswer(t *testing.T) {
	want := digestOf("1")
	ref := mustNormalize(t, "nginx:1.27.0-alpine")

	forward := []string{"ghcr.io/acme/nginx@" + digestOf("2"), "nginx@" + want}
	reverse := []string{"nginx@" + want, "ghcr.io/acme/nginx@" + digestOf("2")}

	first, firstMatch := domain.SelectRepoDigest(forward, ref)
	second, secondMatch := domain.SelectRepoDigest(reverse, ref)

	if first != second || firstMatch != secondMatch {
		t.Errorf("order changed the answer: %q/%q vs %q/%q",
			first, firstMatch, second, secondMatch)
	}
	if first != want {
		t.Errorf("digest = %q, want %q", first, want)
	}
}

// TestAnImageWithNoRepoDigestsReportsNone.
//
// The normal case for an image built on this host. It must read as an absence
// of evidence rather than as a failure, because it is one.
func TestAnImageWithNoRepoDigestsReportsNone(t *testing.T) {
	ref := mustNormalize(t, "harbormaster:local")

	got, match := domain.SelectRepoDigest(nil, ref)

	if got != "" || match != domain.RepoDigestNone {
		t.Errorf("digest = %q/%q, want empty/none", got, match)
	}
	if !strings.Contains(match.Explain(), "built on this host") {
		t.Errorf("the explanation does not describe a local build: %q", match.Explain())
	}
}

// TestMalformedEntriesAreRejectedRatherThanParsedLoosely.
func TestMalformedEntriesAreRejectedRatherThanParsedLoosely(t *testing.T) {
	ref := mustNormalize(t, "nginx:1.27.0-alpine")

	for name, entry := range map[string]string{
		"no digest at all":     "nginx:1.27.0-alpine",
		"a truncated digest":   "nginx@sha256:abcd",
		"an unknown algorithm": "nginx@md5:" + strings.Repeat("a", 32),
		"empty":                "",
		"oversized":            "nginx@sha256:" + strings.Repeat("a", 5000),
	} {
		t.Run(name, func(t *testing.T) {
			got, match := domain.SelectRepoDigest([]string{entry}, ref)
			if got != "" {
				t.Errorf("accepted %q from %q", got, entry)
			}
			if match == domain.RepoDigestResolved {
				t.Errorf("match = resolved for %q", entry)
			}
		})
	}
}

// TestEveryOutcomeExplainsItself, so a plan never renders an empty reason.
func TestEveryOutcomeExplainsItself(t *testing.T) {
	for _, match := range []domain.RepoDigestMatch{
		domain.RepoDigestResolved,
		domain.RepoDigestNone,
		domain.RepoDigestAmbiguous,
		domain.RepoDigestMalformed,
	} {
		if strings.TrimSpace(match.Explain()) == "" {
			t.Errorf("%q has no explanation", match)
		}
	}
}
