package arch_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A rolling container tag is a promise about maturity, and the workflow is the
// only thing that keeps it.
//
// # The failure this exists to prevent
//
// `container.yml` derives its tags from one `tags:` block, and the rolling
// aliases -- `latest`, `rc`, `beta` -- are `type=raw` lines gated on a GitHub
// expression. GitHub offers exactly one signal about how finished a release is:
// `github.event.release.prerelease`, a checkbox. It is a boolean, and a boolean
// cannot name a channel.
//
// That was the original defect. `beta` was enabled on `prerelease == true` and
// nothing else, so EVERY prerelease took it. Publishing `v0.9.0-rc.1` would have
// moved the `beta` tag onto a release candidate: an operator who subscribed to
// `beta` would have been handed a build published under a different maturity
// claim, with no release, no tag and no log line saying the channel had changed
// meaning underneath them.
//
// The mirror-image failure has already happened in this repository, which is why
// this is a test and not a comment. `v0.9.0-beta.2` was published with the
// prerelease box UNTICKED. `latest` is gated on `prerelease == false`, so it
// evaluated true, and `latest` -- the tag reserved for stable releases, of which
// there has never been one -- moved onto a beta. Nothing downstream could
// correct it: the tag was already written. The `beta` channel meanwhile stayed
// on `beta.1`, because its own gate had gone false.
//
// So the invariant is: a rolling alias must be pinned by something more specific
// than the checkbox.
//
//   - `latest` may be written only when the release is NOT a prerelease.
//   - Every alias that a prerelease may take must ALSO require its own marker in
//     the tag being built, so the alias and the version agree by construction.
//
// A prerelease matching no channel marker takes no rolling alias at all. That is
// the fail-closed direction: an unrecognised prerelease publishes its exact
// version and its immutable `sha-…` and repurposes nobody's channel.

var (
	// The `tags: |` block of the metadata-action step, up to the next key at
	// the same indentation.
	metadataTagsBlock = regexp.MustCompile(`(?ms)^          tags: \|\n(.*?)^          [a-z]`)

	// One `type=raw,value=<alias>,enable=<expression>` rule.
	rawTagRule = regexp.MustCompile(`type=raw,value=([A-Za-z0-9._-]+),enable=(.*)$`)
)

// rollingAliases are the tags whose whole purpose is to move. A pinned version
// tag cannot mislead anyone; these can.
var rollingAliases = map[string]struct{}{
	"latest": {},
	"rc":     {},
	"beta":   {},
	"stable": {},
	"alpha":  {},
	"edge":   {},
}

func containerWorkflowTagRules(t *testing.T) []struct{ Alias, Enable string } {
	t.Helper()

	path := filepath.Join(moduleRoot(t), ".github", "workflows", "container.yml")
	source, err := os.ReadFile(path) //nolint:gosec // a fixed path inside the repository
	if err != nil {
		t.Fatalf("read container.yml: %v", err)
	}

	block := metadataTagsBlock.FindSubmatch(source)
	if block == nil {
		t.Fatal("container.yml has no `tags: |` block under the metadata-action step; " +
			"the tags this repository publishes are then derived somewhere this test " +
			"cannot see, and no channel guarantee below is being checked at all")
	}

	var rules []struct{ Alias, Enable string }
	for _, line := range strings.Split(string(block[1]), "\n") {
		match := rawTagRule.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		rules = append(rules, struct{ Alias, Enable string }{
			Alias:  match[1],
			Enable: match[2],
		})
	}
	return rules
}

func TestLatestIsWrittenOnlyByAStableRelease(t *testing.T) {
	t.Parallel()

	var seen bool
	for _, rule := range containerWorkflowTagRules(t) {
		if rule.Alias != "latest" {
			continue
		}
		seen = true

		if !strings.Contains(rule.Enable, "github.event.release.prerelease == false") {
			t.Errorf("the `latest` tag rule does not require "+
				"`github.event.release.prerelease == false`; its condition is %q. "+
				"`latest` is what the Compose default and every published example "+
				"resolve through, so a prerelease able to claim it upgrades every "+
				"operator who followed the documentation onto an unfinished build "+
				"without asking", rule.Enable)
		}
		if !strings.Contains(rule.Enable, "github.event_name == 'release'") {
			t.Errorf("the `latest` tag rule does not require a release event; its "+
				"condition is %q. A push to main could then replace the tag a "+
				"production host is pulling", rule.Enable)
		}
	}

	if !seen {
		t.Fatal("container.yml declares no `type=raw,value=latest` rule; either " +
			"`latest` is no longer published, or it is now written by something " +
			"this test does not read")
	}
}

func TestEveryPrereleaseChannelAliasIsPinnedToItsOwnVersionMarker(t *testing.T) {
	t.Parallel()

	// The marker each channel alias must find in the tag before it may claim
	// that alias. `-rc.` and `-beta.` are the semver prerelease identifiers this
	// repository publishes under.
	markers := map[string]string{
		"rc":    "-rc.",
		"beta":  "-beta.",
		"alpha": "-alpha.",
	}

	var channels []string
	for _, rule := range containerWorkflowTagRules(t) {
		if _, rolling := rollingAliases[rule.Alias]; !rolling {
			continue
		}
		if !strings.Contains(rule.Enable, "github.event.release.prerelease == true") {
			continue
		}
		channels = append(channels, rule.Alias)

		marker, known := markers[rule.Alias]
		if !known {
			t.Errorf("the rolling alias %q is written for a prerelease but this test "+
				"knows no version marker for it. Add one here at the same time as the "+
				"workflow rule, or the channel is unpinned: every prerelease would "+
				"take it, whatever it was actually cut as", rule.Alias)
			continue
		}

		if !strings.Contains(rule.Enable, marker) {
			t.Errorf("the %q tag rule is gated on `prerelease == true` without also "+
				"requiring %q in the tag; its condition is %q.\n\n"+
				"`prerelease` is a checkbox: it says a build is unfinished, not HOW "+
				"unfinished. Gated on it alone, %q is claimed by every prerelease, so "+
				"publishing a release candidate silently moves the channel an operator "+
				"subscribed to for %s builds.",
				rule.Alias, marker, rule.Enable, rule.Alias, rule.Alias)
		}
	}

	if len(channels) < 2 {
		t.Errorf("container.yml declares %d prerelease channel alias(es) (%v); this "+
			"repository publishes release candidates and betas as separate channels, "+
			"so collapsing them back to one means a release again joins a channel it "+
			"was not cut for", len(channels), channels)
	}
}

func TestReleaseDocumentationNamesTheReleaseCandidateChannel(t *testing.T) {
	t.Parallel()

	// The tag table an operator reads is the only place the channel contract is
	// visible to them. A workflow that publishes `rc` while the documentation
	// still describes one prerelease channel is a documented promise the
	// software does not keep.
	// `rc` as a standalone token rather than as a substring, so "source" and
	// "arch" do not count as mentions. The files use different markup -- README
	// and the process document are Markdown, `.env.example` is shell comments --
	// so the token is matched bare and the human-readable phrase is required
	// alongside it.
	rcToken := regexp.MustCompile(`(^|[^A-Za-z0-9_-])rc([^A-Za-z0-9_.-]|$)`)

	root := moduleRoot(t)
	for _, doc := range []string{
		"README.md",
		filepath.Join("docs", "security", "release-process.md"),
		".env.example",
	} {
		source, err := os.ReadFile(filepath.Join(root, doc)) //nolint:gosec // a fixed path inside the repository
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		text := string(source)

		if !rcToken.MatchString(text) {
			t.Errorf("%s never names the `rc` tag, but container.yml publishes it. "+
				"An operator reading this file cannot discover the channel that carries "+
				"the release they are being asked to run", doc)
		}
		if !strings.Contains(strings.ToLower(text), "release candidate") {
			t.Errorf("%s never uses the phrase \"release candidate\", so the `rc` tag "+
				"appears in it without anything saying what maturity that channel "+
				"claims", doc)
		}
	}
}
