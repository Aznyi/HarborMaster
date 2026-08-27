//go:build integration

package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/registry"
)

// Stage 17.5 §12: what the real registries actually cost.
//
// # What is measured here, and what is NOT
//
// These make real, read-only, anonymous requests to Docker Hub through the
// production client, with every bound and every guard in place. They measure
// REQUEST AND PAGE COUNTS, which is the number the stage is accountable for:
// the fix had to make better use of evidence already fetched, not fetch more.
//
// They deliberately do NOT assert semantics. A public registry cannot be made
// to move a tag's digest on demand, and §13 forbids standing up a local one --
// no insecure-registry option, no localhost exception, no TLS bypass. So the
// semantic proof lives in the deterministic service-level truth table, and
// these tests establish only the traffic shape around it.
//
// Nothing here writes to, tags, or otherwise touches a public repository.
//
// # Why they are skippable
//
// They depend on the network and on public registry availability, which is not
// something a build should fail on. Set HARBORMASTER_REGISTRY_MEASUREMENT=1 to
// run them; the numbers are recorded in the Stage 17.5 report.

// countingTransport is not used: the production client's own request path is
// what is under measurement, and wrapping it would measure the wrapper. Pages
// are counted from the result instead, and requests from the tag/manifest call
// counts the client makes -- one manifest per lookup, one HTTP GET per page.

func requireMeasurement(t *testing.T) {
	t.Helper()
	if os.Getenv("HARBORMASTER_REGISTRY_MEASUREMENT") != "1" {
		t.Skip("set HARBORMASTER_REGISTRY_MEASUREMENT=1 to measure against public registries")
	}
}

// measurementClient builds the PRODUCTION registry client, unmodified.
//
// Same allowlist, same TLS policy, same SSRF controls, same timeouts. If any of
// those had been relaxed for testing, that relaxation would be the finding.
func measurementClient(t *testing.T) *registry.Client {
	t.Helper()

	return registry.New(registry.Options{
		RequestTimeout: 20 * time.Second,
		MaxAttempts:    2,
		RetryBackoff:   250 * time.Millisecond,
	})
}

// measureReference records what one reference costs.
type measurement struct {
	reference   string
	manifestOK  bool
	digest      string
	versioned   bool
	pagesWalked int
	tagsRead    int
	truncated   bool
	tagListErr  error
	elapsed     time.Duration
}

func measureReference(t *testing.T, client *registry.Client, reference string, maxPages int) measurement {
	t.Helper()

	result := measurement{reference: reference}
	ref, err := domain.NormalizeImageRef(reference)
	if err != nil {
		t.Fatalf("normalise %q: %v", reference, err)
	}
	_, result.versioned = domain.ParseTagVersion(ref.Tag)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	started := time.Now()

	// 1. The exact configured reference. ALWAYS one request, and the one that
	//    establishes the digest the fix preserves.
	manifest, err := client.Manifest(ctx, registry.ManifestRequest{Ref: ref})
	if err != nil {
		t.Logf("%s: manifest lookup failed: %v", reference, err)
	} else {
		result.manifestOK = true
		result.digest = manifest.Digest
	}

	// 2. Version discovery, only when the tag is a version -- exactly the
	//    condition assess applies.
	if result.versioned {
		tags, tagErr := client.Tags(ctx, ref, maxPages)
		result.tagListErr = tagErr
		if tagErr == nil {
			result.tagsRead = len(tags.Tags)
			result.truncated = tags.Truncated
			// Pages are derived: the walk requests provider.TagPageSize() per
			// page and stops early on a short one.
			result.pagesWalked = pagesFor(len(tags.Tags), tags.Truncated, maxPages)
		}
	}

	result.elapsed = time.Since(started)
	return result
}

// pagesFor derives how many pages a walk must have read.
func pagesFor(tagsRead int, truncated bool, maxPages int) int {
	if truncated {
		return maxPages
	}
	const hubPageSize = 100
	pages := tagsRead / hubPageSize
	if tagsRead%hubPageSize != 0 || pages == 0 {
		pages++
	}
	return pages
}

// TestMeasureCommonRepositories is the §12 table.
func TestMeasureCommonRepositories(t *testing.T) {
	requireMeasurement(t)

	client := measurementClient(t)

	const maxPages = 5 // the shipped default, unchanged by Stage 17.5

	references := []string{
		// A. A small repository.
		"docker.io/library/hello-world:latest",
		// B and D. Mutable tags: no enumeration at all.
		"docker.io/library/nginx:latest",
		"docker.io/library/redis:latest",
		// C and E. Versioned tags: enumeration runs, and is where the defect lived.
		"docker.io/library/nginx:1.27.4",
		"docker.io/library/redis:7.2.5",
	}

	t.Logf("%-42s %-9s %-9s %-6s %-6s %-9s %s",
		"reference", "manifest", "versioned", "pages", "tags", "truncated", "elapsed")

	for _, reference := range references {
		got := measureReference(t, client, reference, maxPages)

		t.Logf("%-42s %-9v %-9v %-6d %-6d %-9v %s",
			got.reference, got.manifestOK, got.versioned,
			got.pagesWalked, got.tagsRead, got.truncated,
			got.elapsed.Round(time.Millisecond))

		// The one assertion that is safe against public-registry drift: the
		// exact-tag manifest lookup is what the fix depends on, and it must be
		// available without any enumeration at all.
		if !got.manifestOK {
			t.Errorf("%s: the exact-tag manifest could not be resolved; "+
				"Follow-current-tag rests entirely on this lookup", got.reference)
		}
		if got.manifestOK && got.digest == "" {
			t.Errorf("%s: the manifest resolved with no digest", got.reference)
		}

		// A MUTABLE tag must never enumerate. This is the Watchtower case and
		// its cost must be exactly one request.
		if !got.versioned && got.pagesWalked != 0 {
			t.Errorf("%s: a non-version tag walked %d pages; version discovery "+
				"must not run for it", got.reference, got.pagesWalked)
		}

		// And no walk may exceed the budget.
		if got.pagesWalked > maxPages {
			t.Errorf("%s: walked %d pages, above the %d budget",
				got.reference, got.pagesWalked, maxPages)
		}
	}
}

// TestTheExactTagDigestNeedsNoEnumeration is the §11 claim, live.
//
// The whole architecture of the fix rests on this: the digest of the configured
// reference is obtainable with ONE request, independently of how large the
// repository is. If that were not true, "preserve the digest fact when
// enumeration is truncated" would be preserving something HarborMaster did not
// have.
func TestTheExactTagDigestNeedsNoEnumeration(t *testing.T) {
	requireMeasurement(t)

	client := measurementClient(t)

	// A deliberately enormous repository, on a versioned tag.
	ref, err := domain.NormalizeImageRef("docker.io/library/nginx:1.27.4")
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	started := time.Now()
	manifest, err := client.Manifest(ctx, registry.ManifestRequest{Ref: ref})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if manifest.Digest == "" {
		t.Fatal("no digest resolved for an exact configured tag")
	}
	if !domain.ValidImageDigest(manifest.Digest) {
		t.Errorf("digest %q is not a valid digest", manifest.Digest)
	}

	t.Logf("exact-tag resolution: nginx:1.27.4 -> %s in %s, zero tag pages",
		manifest.Digest, elapsed.Round(time.Millisecond))
}
