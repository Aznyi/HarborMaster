package domain

import (
	"strconv"
	"strings"
)

// Tag version parsing and update classification.
//
// # The rule this file exists to enforce
//
// A TAG IS NOT A VERSION. It is an arbitrary label that a publisher may point
// anywhere, and most of them are not semantic versions at all. Treating
// "latest" or "stable" as though it carried a version would produce confident
// nonsense -- "a major update is available" for a tag that simply moved.
//
// So parsing is deliberately conservative and REFUSES more than it accepts.
// When a tag cannot be read as a version, the engine falls back to comparing
// digests, which is always true even when it is less informative.
//
// # Comparability is narrow on purpose
//
// Two tags are comparable only when they belong to the same FAMILY: the same
// "v" prefix, the same number of components, and the same variant suffix.
//
// Each of those matters:
//
//   - "1.25" and "1.25.3" are not comparable. "1.25" is a floating tag that
//     already points at the newest 1.25.x, so offering 1.25.3 as an update to
//     it would be advice to pin something the operator deliberately left
//     floating.
//   - "1.25-alpine" and "1.26" are not comparable. They are different images;
//     the second is not an update to the first, it is a different base.
//   - "v1.2.3" and "1.2.3" are not comparable, because a repository that
//     publishes both is publishing two series.
//
// Narrow comparability means some real updates are reported as "unknown". That
// is the correct direction: a missed update costs an operator a manual check,
// while a wrong one costs them a broken deployment.

// UpdateType classifies what kind of update is available.
type UpdateType string

// Update types.
const (
	// UpdateNone means the image is current: the tag has not moved and no
	// newer comparable tag exists.
	UpdateNone UpdateType = "none"
	// UpdateDigest means the SAME TAG now resolves to a different digest. The
	// publisher moved a mutable tag. This is the only update type available for
	// a tag that carries no version.
	UpdateDigest UpdateType = "digest"
	UpdatePatch  UpdateType = "patch"
	UpdateMinor  UpdateType = "minor"
	UpdateMajor  UpdateType = "major"
	// UpdatePrerelease means the newest comparable tag is a prerelease. Kept
	// separate from patch/minor/major because "there is an rc" and "there is a
	// release" call for different decisions.
	UpdatePrerelease UpdateType = "prerelease"
	// UpdateUnknown means an update may exist but its size could not be
	// determined -- a calendar tag, an incomplete tag listing, or a comparison
	// the parser refused to make. Reported honestly rather than guessed.
	UpdateUnknown UpdateType = "unknown"

	// UpdateRebind means the container must be recreated on the digest IT IS
	// ALREADY RUNNING, because the namespace it shares belongs to a container
	// that has been replaced.
	//
	// # Why this is an update type at all
	//
	// It is not an image change, and calling it one is uncomfortable. But the
	// alternative was worse. Verified against Docker 29.6.2: recreating a
	// namespace provider leaves its dependents RUNNING WITH NO NETWORK, silently
	// -- and `docker restart` cannot repair one, it only takes it down. The
	// repair is a recreation with the reference re-resolved, and the only safe
	// way to perform a recreation in HarborMaster is through the change plan
	// that founds an acquisition that founds an execution.
	//
	// Expressing the repair as a plan means it inherits every gate rather than
	// needing new ones: the registry allowlist, the digest verification, the
	// preservation comparison, the compliance evaluation, the self-update
	// refusal, and all four verification proofs apply to it unchanged.
	//
	// # It is the most tightly pinned operation HarborMaster performs
	//
	// The proposed digest is not a digest a registry offered. It is the digest
	// HarborMaster OBSERVED the container running. A rebind cannot change what
	// executes on the host; it can only change which namespace it is attached
	// to.
	//
	// # And it cannot be asked for
	//
	// No endpoint constructs one. A rebind plan exists only because the
	// dependency coordinator observed a stale namespace reference in
	// HarborMaster's own inventory. See RebindEvidence.
	UpdateRebind UpdateType = "rebind"
)

// UpdateTypes lists every type, in report order: most to least urgent, with
// the two non-answers last.
//
// UpdateRebind sits after the version changes and before the non-answers: it is
// real work that must happen, but it is not a version moving.
var UpdateTypes = []UpdateType{
	UpdateMajor, UpdateMinor, UpdatePatch, UpdatePrerelease,
	UpdateDigest, UpdateRebind, UpdateUnknown, UpdateNone,
}

// ValidUpdateType reports whether name is a known update type.
func ValidUpdateType(name string) bool {
	for _, updateType := range UpdateTypes {
		if string(updateType) == name {
			return true
		}
	}
	return false
}

// Available reports whether the type describes an actual update.
func (u UpdateType) Available() bool {
	return u != UpdateNone && u != UpdateUnknown
}

// Rank orders update types by how much they matter, higher first. Used for
// sorting through a CASE expression built from these constants.
func (u UpdateType) Rank() int {
	switch u {
	case UpdateRebind:
		// Above every version change, deliberately. A rebind means the
		// container is broken RIGHT NOW -- attached to a namespace that no
		// longer exists -- while a major update means a newer image is
		// available. Ranked so an operator sorting by what matters sees the
		// outage before the opportunity.
		return 7
	case UpdateMajor:
		return 6
	case UpdateMinor:
		return 5
	case UpdatePatch:
		return 4
	case UpdatePrerelease:
		return 3
	case UpdateDigest:
		return 2
	case UpdateUnknown:
		return 1
	default:
		return 0
	}
}

// mutableTags are tag names that name a CHANNEL rather than a version.
//
// A tag in this set is never parsed as a version however it is spelled, so an
// image on "latest" is only ever compared by digest. Without this, a repository
// that also publishes "2.0" would produce "a major update is available" for
// every container tracking latest -- which is exactly backwards, since latest
// is usually already the newest thing.
var mutableTags = map[string]struct{}{
	"latest": {}, "stable": {}, "current": {}, "release": {}, "rolling": {},
	"main": {}, "master": {}, "trunk": {}, "head": {}, "next": {}, "edge": {},
	"dev": {}, "devel": {}, "development": {}, "develop": {},
	"nightly": {}, "canary": {}, "unstable": {}, "experimental": {},
	"prod": {}, "production": {}, "staging": {}, "test": {}, "testing": {},
	"alpha": {}, "beta": {}, "rc": {}, "preview": {}, "snapshot": {},
	"lts": {}, "slim": {}, "alpine": {},
}

// prereleaseTokens are suffix words that mark a pre-release rather than a
// build variant.
//
// The distinction matters and cannot be derived: "1.2.3-rc1" is a candidate for
// 1.2.3, while "1.2.3-alpine" is a different build of it. Anything NOT in this
// set is treated as a variant, which is the conservative default -- a variant
// only ever compares against the identical variant.
var prereleaseTokens = map[string]struct{}{
	"rc": {}, "alpha": {}, "beta": {}, "pre": {}, "preview": {},
	"dev": {}, "snapshot": {}, "nightly": {}, "canary": {},
	"milestone": {}, "ea": {}, "next": {}, "insiders": {},
}

// IsMutableTag reports whether a tag names a channel rather than a version.
func IsMutableTag(tag string) bool {
	_, ok := mutableTags[strings.ToLower(strings.TrimSpace(tag))]
	return ok
}

// TagVersion is a tag successfully read as a version.
type TagVersion struct {
	// Tag is the original tag.
	Tag string
	// Prefix is "v" when the tag carried one, otherwise empty. Part of the
	// family: a repository publishing both "v1.2.3" and "1.2.3" publishes two
	// series.
	Prefix string

	Major, Minor, Patch int
	// Components is how many numeric parts the tag carried: 1, 2, or 3. Part of
	// the family, because "1.25" is a floating tag over the 1.25.x line and
	// comparing it against "1.25.3" would advise pinning it.
	Components int

	// Prerelease is the pre-release identifier, e.g. "rc1", empty for a stable
	// release.
	Prerelease string
	// Variant is the build suffix, e.g. "alpine". Two tags compare only when
	// their variants are identical.
	Variant string

	// Calendar marks a single numeric component long enough to be a date or a
	// build number rather than a major version. Such tags are ORDERED but their
	// changes are classified as unknown, because calling a date bump "major"
	// would imply a breaking change nobody claimed.
	Calendar bool
}

// Prereleased reports whether the version is a pre-release.
func (v TagVersion) Prereleased() bool { return v.Prerelease != "" }

// SameFamily reports whether two versions may be compared at all.
func (v TagVersion) SameFamily(other TagVersion) bool {
	return v.Prefix == other.Prefix &&
		v.Components == other.Components &&
		v.Variant == other.Variant &&
		v.Calendar == other.Calendar
}

// Compare orders two versions in the same family: -1, 0, or 1.
//
// Pre-release ordering follows the semantic-versioning rule that a pre-release
// sorts BEFORE its release, so 1.2.0-rc1 < 1.2.0. Two different pre-releases of
// the same version are ordered by their identifier, which is a string compare
// and therefore approximate -- "rc10" sorts before "rc9". That is stated rather
// than fixed: the alternative is a full semantic-version identifier parser for
// a case that only decides which of two release candidates is newer.
func (v TagVersion) Compare(other TagVersion) int {
	if c := compareInt(v.Major, other.Major); c != 0 {
		return c
	}
	if c := compareInt(v.Minor, other.Minor); c != 0 {
		return c
	}
	if c := compareInt(v.Patch, other.Patch); c != 0 {
		return c
	}

	switch {
	case v.Prerelease == "" && other.Prerelease == "":
		return 0
	case v.Prerelease == "":
		return 1
	case other.Prerelease == "":
		return -1
	}
	return strings.Compare(v.Prerelease, other.Prerelease)
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// maxVersionComponent bounds a parsed numeric component.
//
// A tag comes from a container's configuration, so its digits are untrusted
// input. The bound stops a tag of a thousand digits from becoming an expensive
// parse, and keeps every component inside an int on every platform.
const maxVersionComponent = 1 << 31

// ParseTagVersion reads a tag as a version, reporting whether it could.
//
// Refuses, rather than guesses, for:
//
//   - a mutable channel tag (see mutableTags)
//   - anything that does not begin with a digit after an optional "v"
//   - a component that is not a bounded decimal number
//   - more than three numeric components
func ParseTagVersion(tag string) (TagVersion, bool) {
	trimmed := strings.TrimSpace(tag)
	if trimmed == "" || len(trimmed) > MaxTagBytes {
		return TagVersion{}, false
	}
	if IsMutableTag(trimmed) {
		return TagVersion{}, false
	}

	version := TagVersion{Tag: trimmed}
	body := trimmed

	// A single leading "v" or "V", and only when a digit follows it. "vault" is
	// not version 0.
	if len(body) > 1 && (body[0] == 'v' || body[0] == 'V') && isDigit(body[1]) {
		version.Prefix = "v"
		body = body[1:]
	}

	// The suffix begins at the first character that cannot belong to a dotted
	// numeric version.
	numeric := body
	suffix := ""
	for index := 0; index < len(body); index++ {
		if !isDigit(body[index]) && body[index] != '.' {
			numeric, suffix = body[:index], body[index:]
			break
		}
	}
	// A trailing dot belongs to neither part, e.g. "1.2." -- refuse rather than
	// silently reading it as 1.2.
	if strings.HasSuffix(numeric, ".") {
		return TagVersion{}, false
	}
	if numeric == "" {
		return TagVersion{}, false
	}

	parts := strings.Split(numeric, ".")
	if len(parts) > 3 {
		return TagVersion{}, false
	}

	values := make([]int, 0, 3)
	for _, part := range parts {
		if part == "" || len(part) > 10 {
			return TagVersion{}, false
		}
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || value >= maxVersionComponent {
			return TagVersion{}, false
		}
		values = append(values, value)
	}

	version.Components = len(values)
	version.Major = values[0]
	if len(values) > 1 {
		version.Minor = values[1]
	}
	if len(values) > 2 {
		version.Patch = values[2]
	}

	// A lone number of six or more digits is a date or a build counter, not a
	// major version. Ordered, but never described as "major".
	version.Calendar = version.Components == 1 && len(parts[0]) >= 6

	if suffix != "" {
		version.Prerelease, version.Variant = splitSuffix(suffix)
	}
	return version, true
}

// splitSuffix separates a pre-release identifier from a build variant.
//
// The suffix is examined token by token. A leading token in prereleaseTokens
// makes the whole suffix a pre-release; anything else makes it a variant.
// Ambiguity resolves to VARIANT, which is the conservative answer: a variant
// only ever compares against an identical variant, so a misread suffix
// suppresses a comparison rather than inventing one.
func splitSuffix(suffix string) (prerelease, variant string) {
	body := strings.TrimLeft(suffix, "-_+.")
	if body == "" {
		return "", ""
	}

	// The first token, delimited by any of the separators publishers use.
	token := body
	if index := strings.IndexAny(body, "-_+."); index >= 0 {
		token = body[:index]
	}

	// "rc1" and "beta2" glue the token to a number; compare on the letters.
	letters := strings.ToLower(strings.TrimRight(token, "0123456789"))
	if letters == "" {
		// A purely numeric first token, e.g. "1.2.3-1". A build revision, which
		// is a variant rather than a pre-release.
		return "", body
	}

	if _, ok := prereleaseTokens[letters]; ok {
		return body, ""
	}
	return "", body
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// UpdateAssessment is the verdict for one image.
type UpdateAssessment struct {
	Type UpdateType
	// Tag is the newer tag, when one was found by version comparison.
	Tag string
	// Reason explains the verdict, from a fixed set of phrases. Never an error
	// string and never registry-supplied text.
	Reason string
}

// ClassifyTagUpdate picks the best available update from a candidate tag list.
//
// Only tags in the CURRENT tag's family are considered, and only ones that sort
// strictly newer. A stable release always wins over a pre-release, so a
// repository that publishes both 1.3.0 and 1.4.0-rc1 offers "minor" rather than
// "prerelease" -- the release is the actionable answer and the candidate is
// noise beside it.
//
// truncated reports that the tag listing was cut short by its budget. When it
// is set and nothing newer was found, the verdict is UNKNOWN rather than NONE:
// a listing that stopped early has not established that no newer tag exists.
func ClassifyTagUpdate(currentTag string, candidates []string, truncated bool) UpdateAssessment {
	current, ok := ParseTagVersion(currentTag)
	if !ok {
		return UpdateAssessment{
			Type:   UpdateUnknown,
			Reason: "the current tag is not a version, so only its digest can be compared",
		}
	}

	var (
		bestStable     TagVersion
		haveStable     bool
		bestPrerelease TagVersion
		havePrerelease bool
	)

	for _, candidate := range candidates {
		parsed, parsedOK := ParseTagVersion(candidate)
		if !parsedOK || !current.SameFamily(parsed) {
			continue
		}
		if parsed.Compare(current) <= 0 {
			continue
		}

		if parsed.Prereleased() {
			if !havePrerelease || parsed.Compare(bestPrerelease) > 0 {
				bestPrerelease, havePrerelease = parsed, true
			}
			continue
		}
		if !haveStable || parsed.Compare(bestStable) > 0 {
			bestStable, haveStable = parsed, true
		}
	}

	switch {
	case haveStable:
		return UpdateAssessment{
			Type:   describeChange(current, bestStable),
			Tag:    bestStable.Tag,
			Reason: "a newer tag is published in the same series",
		}
	case havePrerelease:
		return UpdateAssessment{
			Type:   UpdatePrerelease,
			Tag:    bestPrerelease.Tag,
			Reason: "the only newer tag in this series is a pre-release",
		}
	case truncated:
		return UpdateAssessment{
			Type: UpdateUnknown,
			Reason: "the tag listing exceeded its budget, so a newer tag may exist " +
				"beyond what was read",
		}
	default:
		return UpdateAssessment{
			Type:   UpdateNone,
			Reason: "no newer tag is published in this series",
		}
	}
}

// describeChange names the size of a version change.
//
// A calendar tag returns UNKNOWN: its single component did increase, but
// calling that "major" would assert a breaking change that nobody claimed.
func describeChange(current, candidate TagVersion) UpdateType {
	if current.Calendar || candidate.Calendar {
		return UpdateUnknown
	}
	switch {
	case candidate.Major != current.Major:
		return UpdateMajor
	case candidate.Minor != current.Minor:
		return UpdateMinor
	case candidate.Patch != current.Patch:
		return UpdatePatch
	default:
		// Same numbers, and the candidate sorted newer, so the current tag was
		// a pre-release that has now been released.
		return UpdatePatch
	}
}
