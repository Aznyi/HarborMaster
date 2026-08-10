package domain

// Resolving the digest of the image a container is actually running.
//
// # The defect this file exists to prevent
//
// A container started as `nginx:1.27.0-alpine` declares a reference with no
// digest in it. HarborMaster used to read the local digest from that reference
// alone, so for every tag-referenced container -- which is to say almost every
// container anywhere -- the local digest was empty.
//
// An empty local digest is not a harmless gap. It reaches the change planner as
// "the running image has no registry digest", which is an UNKNOWN rather than a
// risk, and an unknown forces the recommendation to `unknown`. Acquisition
// refuses an unrecommended plan, so image acquisition, container recreation,
// and rollback were all unreachable on any ordinary host.
//
// The daemon knew the answer the whole time. `docker inspect` reports
// RepoDigests -- the registry manifest references the local image is known by --
// and HarborMaster already reads and stores them.
//
// # Why this is not simply "take the first RepoDigest"
//
// One local image can carry RepoDigests for SEVERAL repositories. An image
// pulled from `docker.io/library/redis` and re-tagged and pushed to
// `ghcr.io/acme/redis` has two, and they are different manifests.
//
// Taking the wrong one would be a digest substitution: HarborMaster would
// compare a foreign repository's digest against this repository's registry
// answer, conclude the tag had moved, and propose a change built on content
// from somewhere else entirely. So the rule is exact-repository matching, and
// anything ambiguous reports UNKNOWN rather than guessing. An unknown costs a
// recommendation; a wrong digest costs the guarantee the whole product rests
// on.

// RepoDigestMatch reports the outcome of resolving a local digest.
//
// Distinguishing "none" from "ambiguous" matters to the operator-facing reason:
// the first is a locally built image, the second is a real conflict somebody
// should look at.
type RepoDigestMatch string

const (
	// RepoDigestResolved means exactly one digest matched this repository.
	RepoDigestResolved RepoDigestMatch = "resolved"
	// RepoDigestNone means no RepoDigest named this repository.
	RepoDigestNone RepoDigestMatch = "none"
	// RepoDigestAmbiguous means this repository appeared more than once with
	// DIFFERENT digests, so no single answer is trustworthy.
	RepoDigestAmbiguous RepoDigestMatch = "ambiguous"
	// RepoDigestMalformed means entries named this repository but none of them
	// carried a digest this build is willing to parse.
	RepoDigestMalformed RepoDigestMatch = "malformed"
)

// SelectRepoDigest picks the manifest digest a local image carries for one
// repository.
//
// `want` is the normalized reference of the image the container declares. Only
// RepoDigests whose registry AND repository path match it are considered; a
// digest belonging to any other repository is never returned, whatever else is
// available.
//
// Deterministic by construction: the answer does not depend on the order Docker
// happened to list the entries. Identical duplicates collapse to one answer,
// and genuinely conflicting entries resolve to ambiguous rather than to
// whichever came first.
func SelectRepoDigest(repoDigests []string, want NormalizedRef) (string, RepoDigestMatch) {
	if want.Host == "" || want.Path == "" {
		return "", RepoDigestNone
	}

	var (
		chosen  string
		matched bool
		// named records that the repository appeared at all, so an entry that
		// matched but carried an unusable digest is told apart from no entry.
		named bool
	)

	for _, entry := range repoDigests {
		if len(entry) > MaxReferenceBytes {
			// Bounded before parsing. An oversized entry cannot have come from
			// a reference the inventory accepted, so it is not this image's.
			continue
		}

		parsed, err := NormalizeImageRef(entry)
		if err != nil {
			continue
		}
		if parsed.Host != want.Host || parsed.Path != want.Path {
			// A different repository. Never a candidate -- see the file header.
			continue
		}
		named = true

		if parsed.Digest == "" || !ValidImageDigest(parsed.Digest) {
			continue
		}

		switch {
		case !matched:
			chosen, matched = parsed.Digest, true
		case chosen != parsed.Digest:
			// Two different digests for one repository. The daemon cannot tell
			// us which one this image is, and neither can we.
			return "", RepoDigestAmbiguous
		}
	}

	switch {
	case matched:
		return chosen, RepoDigestResolved
	case named:
		return "", RepoDigestMalformed
	default:
		return "", RepoDigestNone
	}
}

// RunningDigestFor resolves the manifest digest a container is ACTUALLY
// running, from the reference it declares and the local image's RepoDigests.
//
// # Why this exists as one function
//
// "The digest this container is running" was being derived independently in
// four places, and three of them derived it wrongly: they read the digest out
// of the DECLARED REFERENCE, which is empty for every container created from a
// tag -- which is to say almost every container anywhere.
//
// The consequences were not cosmetic. Lineage never learned what a tag-created
// container was running, so a lineage record contradicted by the host could
// never correct itself; the planner's external-change guard compared against an
// empty string and so never fired; and a rollback recorded no original digest,
// which left lineage claiming the REPLACEMENT's digest after the replacement
// had been removed. That last one is the Phase 13 defect reintroduced through
// the failure path: the next pass compares the registry against a digest that
// is not running and concludes there is nothing to do.
//
// So there is now exactly one definition, and every caller uses it.
//
// # The order, and why it is this way round
//
//  1. A DIGEST-PINNED REFERENCE is the answer. The container was created from
//     that exact manifest, so nothing the local image store says can be more
//     authoritative about what it is running.
//  2. Otherwise the local image's RepoDigests, matched to this repository by
//     SelectRepoDigest -- exact registry and path, ambiguity refused.
//
// Anything else is UNKNOWN, reported as an empty digest. Unknown is a usable
// answer here: every caller treats an unestablished digest as "cannot assess"
// rather than as "no change", which is the fail-closed direction. A guess would
// not be.
func RunningDigestFor(declared NormalizedRef, repoDigests []string) (string, RepoDigestMatch) {
	if declared.Digest != "" && ValidImageDigest(declared.Digest) {
		return declared.Digest, RepoDigestResolved
	}
	return SelectRepoDigest(repoDigests, declared)
}

// Explain renders a match outcome as the operator-facing reason for a missing
// local digest.
//
// Fixed phrases. Nothing here is a daemon or registry string.
func (m RepoDigestMatch) Explain() string {
	switch m {
	case RepoDigestResolved:
		return "the running image's digest was read from the local image"
	case RepoDigestAmbiguous:
		return "the local image reports more than one digest for this repository, " +
			"so which one is running cannot be established"
	case RepoDigestMalformed:
		return "the local image names this repository without a usable digest"
	default:
		return "the local image carries no registry digest for this repository, " +
			"which is normal for an image built on this host"
	}
}
