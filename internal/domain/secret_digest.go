package domain

// DigestAlgorithm names how a SecretDigest was produced.
//
// Recorded with every digest rather than assumed globally, so a future key
// rotation or algorithm change can tell old digests from new ones without a
// schema migration, and so a comparison between incompatible digests can be
// detected instead of silently producing a wrong answer.
type DigestAlgorithm string

// Digest algorithms.
const (
	// DigestHMACSHA256 is HMAC-SHA-256 under the installation key.
	//
	// SUPERSEDED, and deliberately still named. Digests recorded under it were
	// computed after the raw value had already been dropped by the persistence
	// boundary, so every sensitive value in every snapshot written this way
	// digests the EMPTY STRING and they are all identical. They reveal nothing
	// -- confidentiality was never the defect -- but they answer "did this value
	// change" wrongly, always with "no".
	//
	// They cannot be repaired: the input they should have had was never stored,
	// which is the whole point of the design. So they are not rewritten and not
	// trusted. Comparable() already refuses to compare across algorithms, so an
	// old digest and a v2 digest are incomparable by construction and the
	// comparison reports UNVERIFIABLE rather than a false match.
	DigestHMACSHA256 DigestAlgorithm = "hmac-sha256"

	// DigestHMACSHA256V2 is HMAC-SHA-256 under the installation key, derived
	// through a named purpose, over the value as it was READ FROM THE DAEMON.
	//
	// The version exists to make the two distinguishable at a glance and, more
	// importantly, incomparable to each other. See PurposeSnapshotValue.
	DigestHMACSHA256V2 DigestAlgorithm = "hmac-sha256-v2"
)

// SecretDigest describes a sensitive value without containing it.
//
// # What it is for
//
// Exactly one question: did this value change between two snapshots. It
// supports no other operation.
//
// # Why the fields are not serialised
//
// Digest, Algorithm, and KeyID are tagged `json:"-"` for the same reason
// EnvVar.RawValue is. Even if a SecretDigest reaches a JSON encoder by mistake
// -- in an API response, an error payload, or a log record -- the digest cannot
// be serialised. Reaching it requires naming the field deliberately, which
// makes every such use greppable and reviewable.
//
// The digest is not a credential, but it is a verifier for one: an attacker
// holding both a digest and the installation key can test candidate values
// offline. Keeping it off the wire keeps that attack behind two separate
// compromises rather than one.
//
// # What survives to the API
//
// Present and Length. Whether a variable is set, and how long its value is,
// are what an operator needs to reason about a restore, and neither reveals
// the value.
type SecretDigest struct {
	// Present is false when the variable was not set at all. Distinct from
	// being set to the empty string, and a future restore must not confuse the
	// two.
	Present bool `json:"present"`
	// Length is the byte length of the raw value.
	Length int `json:"length"`

	// Digest is the hex-encoded keyed digest. Never serialised.
	Digest string `json:"-"`
	// Algorithm and KeyID identify how Digest was produced. Never serialised.
	Algorithm DigestAlgorithm `json:"-"`
	KeyID     string          `json:"-"`
}

// Comparable reports whether two digests can meaningfully be compared.
//
// Digests produced under different keys or different algorithms are not
// comparable, and treating them as such would report every secret as changed
// after a key rotation -- a false alarm indistinguishable from a real breach.
// Callers that cannot compare must say so rather than guess.
func (d SecretDigest) Comparable(other SecretDigest) bool {
	return d.Algorithm == other.Algorithm &&
		d.KeyID == other.KeyID &&
		d.Algorithm != "" &&
		d.KeyID != ""
}

// Equal reports whether two digests describe the same value.
//
// Callers must check Comparable first; Equal on incomparable digests is
// meaningless and returns false.
func (d SecretDigest) Equal(other SecretDigest) bool {
	if !d.Comparable(other) {
		return false
	}
	return d.Present == other.Present && d.Digest == other.Digest
}
