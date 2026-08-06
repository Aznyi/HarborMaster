package domain

import "errors"

// The reference and digest a change plan proposes moving a container onto.
//
// # Why this is a type and not two strings
//
// It used to be two strings, and they got crossed. The planner rendered a newer
// TAG by string-editing the current reference, and paired it with the only
// digest it had -- the one the registry had resolved for the CURRENT tag. A
// plan read `busybox:1.36 -> busybox:1.38` and carried 1.36's manifest digest.
//
// Nothing downstream could catch it. Acquisition is digest-pinned by design, so
// it would have pulled exactly what the plan said: the OLD image. Verification
// compares what arrived against the plan's digest, so it would have passed.
// Recreation would then have moved the container onto the old image and every
// record would have said it was now on 1.38.
//
// A digest-pinned pipeline whose pin can name the wrong thing is worse than an
// unpinned one, because it reports certainty it does not have.
//
// # The invariant
//
// A ProposedTarget can only be built by NewProposedTarget, which requires the
// reference and the digest TOGETHER and rejects any pair it cannot vouch for.
// The fields are unexported, so no call site can assemble one from a reference
// it has and a digest it happens to be holding -- which is exactly how the
// original defect was written.
//
// The zero value is the honest "nothing is proposed", and Valid reports it.

// ErrTargetIncomplete reports a proposal missing its reference or its digest.
var ErrTargetIncomplete = errors.New("a proposed target needs both a reference and a digest")

// ErrTargetDigestInvalid reports a malformed digest.
var ErrTargetDigestInvalid = errors.New("a proposed target's digest is not a valid manifest digest")

// ErrTargetCrossed reports a proposal whose digest belongs to a different
// reference than the one it names.
var ErrTargetCrossed = errors.New("a proposed target's digest was resolved for a different reference")

// ProposedTarget is a reference and the digest resolved for THAT reference.
type ProposedTarget struct {
	reference string
	digest    string
}

// NewProposedTarget pairs a reference with the digest resolved for it.
//
// `resolvedFor` is the reference the caller actually asked the registry about.
// It must equal `reference`: passing the current reference while naming a newer
// one is precisely the defect this type exists to prevent, and it is refused
// rather than silently accepted.
func NewProposedTarget(reference, digest, resolvedFor string) (ProposedTarget, error) {
	if reference == "" || digest == "" {
		return ProposedTarget{}, ErrTargetIncomplete
	}
	if !ValidImageDigest(digest) {
		return ProposedTarget{}, ErrTargetDigestInvalid
	}
	if resolvedFor != reference {
		return ProposedTarget{}, ErrTargetCrossed
	}
	return ProposedTarget{reference: reference, digest: digest}, nil
}

// Valid reports whether anything is proposed.
func (t ProposedTarget) Valid() bool { return t.reference != "" && t.digest != "" }

// Reference is the image reference to move onto.
func (t ProposedTarget) Reference() string { return t.reference }

// Digest is the manifest digest resolved for Reference.
func (t ProposedTarget) Digest() string { return t.digest }

// RestoreProposedTarget rebuilds a target from persisted columns.
//
// Used ONLY when reading a row back. It re-checks the pair rather than trusting
// the database, so a row written by an older build -- or edited by hand --
// cannot reintroduce a crossed pair on the read path. An unusable pair restores
// as the zero value, which reads as "nothing proposed" rather than as a
// proposal nobody can verify.
func RestoreProposedTarget(reference, digest string) ProposedTarget {
	target, err := NewProposedTarget(reference, digest, reference)
	if err != nil {
		return ProposedTarget{}
	}
	return target
}
