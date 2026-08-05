package domain_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Tag version parsing and update classification tests.
//
// The property under test throughout is CONSERVATISM. A missed update costs an
// operator a manual check; a wrong one costs them a broken deployment. So every
// case below that could go either way is asserted to go the safe way, and the
// "refuses to guess" cases outnumber the "parses correctly" ones.

func TestParseTagVersion(t *testing.T) {
	cases := []struct {
		tag        string
		prefix     string
		major      int
		minor      int
		patch      int
		components int
		prerelease string
		variant    string
		calendar   bool
	}{
		{tag: "1.25.3", major: 1, minor: 25, patch: 3, components: 3},
		{tag: "v1.25.3", prefix: "v", major: 1, minor: 25, patch: 3, components: 3},
		{tag: "1.25", major: 1, minor: 25, components: 2},
		{tag: "7", major: 7, components: 1},
		{tag: "1.25.3-alpine", major: 1, minor: 25, patch: 3, components: 3, variant: "alpine"},
		{tag: "1.25.3-alpine3.19", major: 1, minor: 25, patch: 3, components: 3, variant: "alpine3.19"},
		{tag: "1.25.3-rc1", major: 1, minor: 25, patch: 3, components: 3, prerelease: "rc1"},
		{tag: "2.0.0-beta.2", major: 2, components: 3, prerelease: "beta.2"},
		{tag: "3.1.0-RC2", major: 3, minor: 1, components: 3, prerelease: "RC2"},
		// A purely numeric suffix is a build revision, which is a variant.
		{tag: "1.2.3-1", major: 1, minor: 2, patch: 3, components: 3, variant: "1"},
		// Six or more digits in a single component is a date or build counter.
		{tag: "20240115", major: 20240115, components: 1, calendar: true},
	}

	for _, tc := range cases {
		t.Run(tc.tag, func(t *testing.T) {
			version, ok := domain.ParseTagVersion(tc.tag)
			if !ok {
				t.Fatalf("ParseTagVersion(%q) refused a version", tc.tag)
			}
			if version.Prefix != tc.prefix {
				t.Errorf("prefix = %q, want %q", version.Prefix, tc.prefix)
			}
			if version.Major != tc.major || version.Minor != tc.minor || version.Patch != tc.patch {
				t.Errorf("version = %d.%d.%d, want %d.%d.%d",
					version.Major, version.Minor, version.Patch, tc.major, tc.minor, tc.patch)
			}
			if version.Components != tc.components {
				t.Errorf("components = %d, want %d", version.Components, tc.components)
			}
			if version.Prerelease != tc.prerelease {
				t.Errorf("prerelease = %q, want %q", version.Prerelease, tc.prerelease)
			}
			if version.Variant != tc.variant {
				t.Errorf("variant = %q, want %q", version.Variant, tc.variant)
			}
			if version.Calendar != tc.calendar {
				t.Errorf("calendar = %v, want %v", version.Calendar, tc.calendar)
			}
		})
	}
}

// A TAG IS NOT A VERSION. Everything here must be refused, so the engine falls
// back to comparing digests rather than inventing a version comparison.
func TestParseTagVersionRefusesNonVersions(t *testing.T) {
	for _, tag := range []string{
		// Channel tags. The most important group: treating "latest" as a
		// version would report a major update for every container tracking it.
		"latest", "stable", "main", "master", "edge", "nightly", "dev",
		"production", "rolling", "LATEST", "Stable",
		// Not versions at all.
		"", "alpine", "bookworm", "vault", "sha-abc123", "release-candidate",
		// Malformed numerics.
		"1.2.", ".1.2", "1.2.3.4", "v", "v-1",
		// Oversized.
		strings.Repeat("1", 11),
		strings.Repeat("a", domain.MaxTagBytes+1),
	} {
		if version, ok := domain.ParseTagVersion(tag); ok {
			t.Errorf("ParseTagVersion(%q) accepted it as %+v", tag, version)
		}
	}
}

// COMPARABILITY IS NARROW ON PURPOSE. Each pair here would produce misleading
// advice if the families were merged.
func TestOnlyTagsInTheSameFamilyCompare(t *testing.T) {
	cases := []struct {
		name       string
		left       string
		right      string
		sameFamily bool
	}{
		{"identical shape", "1.25.3", "1.26.0", true},
		{"identical variant", "1.25.3-alpine", "1.26.0-alpine", true},
		{"prerelease and release of the same shape", "1.25.3-rc1", "1.25.3", true},
		// A floating minor tag already points at the newest patch, so offering
		// a patch tag as its update would be advice to pin it.
		{"different precision", "1.25", "1.25.3", false},
		// Different base images entirely.
		{"variant against plain", "1.25.3-alpine", "1.26.0", false},
		{"different variants", "1.25.3-alpine", "1.26.0-slim", false},
		// A repository publishing both spellings publishes two series.
		{"prefixed against bare", "v1.25.3", "1.26.0", false},
		// A date is not a major version.
		{"calendar against semver", "20240115", "7", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			left, leftOK := domain.ParseTagVersion(tc.left)
			right, rightOK := domain.ParseTagVersion(tc.right)
			if !leftOK || !rightOK {
				t.Fatalf("one side did not parse: %q=%v %q=%v", tc.left, leftOK, tc.right, rightOK)
			}
			if got := left.SameFamily(right); got != tc.sameFamily {
				t.Errorf("SameFamily(%q, %q) = %v, want %v", tc.left, tc.right, got, tc.sameFamily)
			}
		})
	}
}

func TestVersionOrdering(t *testing.T) {
	cases := []struct {
		left  string
		right string
		want  int
	}{
		{"1.25.3", "1.25.4", -1},
		{"1.26.0", "1.25.9", 1},
		{"2.0.0", "1.99.99", 1},
		{"1.25.3", "1.25.3", 0},
		// Semantic versioning's own rule: a pre-release sorts before its
		// release.
		{"1.25.3-rc1", "1.25.3", -1},
		{"1.25.3", "1.25.3-rc1", 1},
		{"1.25.3-rc1", "1.25.3-rc2", -1},
	}

	for _, tc := range cases {
		left, _ := domain.ParseTagVersion(tc.left)
		right, _ := domain.ParseTagVersion(tc.right)
		if got := left.Compare(right); got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.left, tc.right, got, tc.want)
		}
	}
}

func TestClassifyTagUpdate(t *testing.T) {
	cases := []struct {
		name      string
		current   string
		tags      []string
		truncated bool
		want      domain.UpdateType
		wantTag   string
	}{
		{
			name:    "a newer patch",
			current: "1.25.3",
			tags:    []string{"1.25.2", "1.25.3", "1.25.4"},
			want:    domain.UpdatePatch,
			wantTag: "1.25.4",
		},
		{
			name:    "a newer minor",
			current: "1.25.3",
			tags:    []string{"1.25.4", "1.26.0"},
			want:    domain.UpdateMinor,
			wantTag: "1.26.0",
		},
		{
			name:    "a newer major",
			current: "1.25.3",
			tags:    []string{"1.26.0", "2.0.0"},
			want:    domain.UpdateMajor,
			wantTag: "2.0.0",
		},
		{
			name:    "nothing newer",
			current: "1.25.3",
			tags:    []string{"1.24.0", "1.25.0", "1.25.3"},
			want:    domain.UpdateNone,
		},
		{
			// A stable release always wins over a candidate: it is the
			// actionable answer, and the candidate is noise beside it.
			name:    "a release outranks a candidate",
			current: "1.25.3",
			tags:    []string{"1.26.0", "1.27.0-rc1"},
			want:    domain.UpdateMinor,
			wantTag: "1.26.0",
		},
		{
			name:    "only a candidate is available",
			current: "1.25.3",
			tags:    []string{"1.26.0-rc1"},
			want:    domain.UpdatePrerelease,
			wantTag: "1.26.0-rc1",
		},
		{
			name:    "a candidate is promoted to a release",
			current: "1.25.3-rc1",
			tags:    []string{"1.25.3"},
			want:    domain.UpdatePatch,
			wantTag: "1.25.3",
		},
		{
			name:    "variants only compare with themselves",
			current: "1.25.3-alpine",
			tags:    []string{"1.26.0", "1.26.0-slim", "1.26.0-alpine"},
			want:    domain.UpdateMinor,
			wantTag: "1.26.0-alpine",
		},
		{
			name:    "a floating minor tag is not updated to a patch tag",
			current: "1.25",
			tags:    []string{"1.25.3", "1.25.4"},
			want:    domain.UpdateNone,
		},
		{
			name:    "a calendar tag reports unknown rather than major",
			current: "20240115",
			tags:    []string{"20240201"},
			want:    domain.UpdateUnknown,
			wantTag: "20240201",
		},
		{
			// The property that matters most here: a listing that stopped early
			// has established nothing.
			name:      "a truncated listing with nothing newer is unknown",
			current:   "1.25.3",
			tags:      []string{"1.24.0"},
			truncated: true,
			want:      domain.UpdateUnknown,
		},
		{
			// ...but a truncated listing that DID find something newer still
			// reports it. The finding is true whatever else was missed.
			name:      "a truncated listing still reports what it found",
			current:   "1.25.3",
			tags:      []string{"1.26.0"},
			truncated: true,
			want:      domain.UpdateMinor,
			wantTag:   "1.26.0",
		},
		{
			name:    "a channel tag cannot be version-compared",
			current: "latest",
			tags:    []string{"1.26.0", "2.0.0"},
			want:    domain.UpdateUnknown,
		},
		{
			name:    "malformed candidates are ignored",
			current: "1.25.3",
			tags:    []string{"", "not-a-version", "1.26.0", "..."},
			want:    domain.UpdateMinor,
			wantTag: "1.26.0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assessment := domain.ClassifyTagUpdate(tc.current, tc.tags, tc.truncated)
			if assessment.Type != tc.want {
				t.Errorf("type = %q, want %q (reason %q)", assessment.Type, tc.want, assessment.Reason)
			}
			if assessment.Tag != tc.wantTag {
				t.Errorf("tag = %q, want %q", assessment.Tag, tc.wantTag)
			}
			if assessment.Reason == "" {
				t.Error("the assessment carries no reason")
			}
		})
	}
}

// The single most dangerous failure mode this feature has: reporting a major
// update for every container tracking a channel tag. Asserted directly.
func TestChannelTagsNeverProduceAVersionUpdate(t *testing.T) {
	published := []string{"1.0.0", "2.0.0", "3.0.0", "latest"}

	for _, channel := range []string{"latest", "stable", "main", "edge", "nightly"} {
		assessment := domain.ClassifyTagUpdate(channel, published, false)
		if assessment.Type != domain.UpdateUnknown {
			t.Errorf("tag %q classified as %q; a channel tag carries no version",
				channel, assessment.Type)
		}
		if assessment.Tag != "" {
			t.Errorf("tag %q was offered %q as an update", channel, assessment.Tag)
		}
	}
}

func TestUpdateTypeRanking(t *testing.T) {
	if domain.UpdateMajor.Rank() <= domain.UpdateMinor.Rank() {
		t.Error("major does not outrank minor")
	}
	if domain.UpdateMinor.Rank() <= domain.UpdatePatch.Rank() {
		t.Error("minor does not outrank patch")
	}
	if domain.UpdateNone.Rank() != 0 {
		t.Error("none is not the lowest rank")
	}

	// Available() is what the dashboard and the summary count on. "unknown"
	// must NOT count as an available update: it means the opposite.
	for _, available := range []domain.UpdateType{
		domain.UpdateMajor, domain.UpdateMinor, domain.UpdatePatch,
		domain.UpdatePrerelease, domain.UpdateDigest,
	} {
		if !available.Available() {
			t.Errorf("%q does not report as available", available)
		}
	}
	for _, notAvailable := range []domain.UpdateType{domain.UpdateNone, domain.UpdateUnknown} {
		if notAvailable.Available() {
			t.Errorf("%q reports as an available update", notAvailable)
		}
	}
}
