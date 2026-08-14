package service

import (
	"encoding/json"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The security properties of the sensitive-value comparison evidence.
//
// The digest exists to answer one question -- did this value change -- without
// storing what was compared. These pin the ways that can go wrong: a digest an
// attacker could precompute, one that survives a key change and is trusted
// anyway, one derived from the masked placeholder rather than the value, and
// evidence written before the defect was fixed being read as though it meant
// something.

func integrityMasker(t *testing.T, keyHex string) *domain.Masker {
	t.Helper()
	key, err := LoadSecretKey(SecretKeyOptions{Value: keyHex})
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	return domain.NewMasker(domain.DefaultMaskPatterns).
		WithDigester(NewHasher(key).DigestValue)
}

const (
	integrityKeyA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	integrityKeyB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// H. A different installation produces a different digest for the same secret.
//
// Two HarborMasters holding the same credential must not produce the same
// token: it would make one installation's recorded evidence a lookup table for
// another's.
func TestADifferentInstallationKeyChangesTheDigest(t *testing.T) {
	t.Parallel()

	first := integrityMasker(t, integrityKeyA).Classify("DB_PASSWORD", digestSecretA)
	second := integrityMasker(t, integrityKeyB).Classify("DB_PASSWORD", digestSecretA)

	if first.Digest == "" || second.Digest == "" {
		t.Fatal("a configured digester produced no evidence")
	}
	if first.Digest == second.Digest {
		t.Fatal("the same secret digested identically under two installation keys")
	}
	if first.DigestKeyID == second.DigestKeyID {
		t.Fatal("two different keys reported the same key id; evidence from one " +
			"installation would be compared against the other's")
	}
	// And the recorded key id is what makes them refuse to be compared.
	if first.SecretEvidence().Comparable(second.SecretEvidence()) {
		t.Fatal("evidence from two different keys reported itself comparable; a key " +
			"change would read as every secret having changed, or worse, as unchanged")
	}
}

// I. Without a key there is no evidence, and no evidence is not a match.
//
// The fail-closed direction. A masker with no digester is what a deployment
// that could not resolve its key would have, and its output must not compare
// equal to anything -- including another unkeyed one.
func TestWithoutAKeyThereIsNoComparableEvidence(t *testing.T) {
	t.Parallel()

	unkeyed := domain.NewMasker(domain.DefaultMaskPatterns)
	first := unkeyed.Classify("DB_PASSWORD", digestSecretA)
	second := unkeyed.Classify("DB_PASSWORD", digestSecretB)

	if first.Digest != "" || first.DigestAlgorithm != "" {
		t.Fatal("an unkeyed masker produced digest evidence")
	}
	if first.SecretEvidence().Comparable(second.SecretEvidence()) {
		t.Fatal("two unkeyed variables reported themselves comparable; every secret " +
			"would compare equal to every other")
	}
	// Still masked: no key must never mean no masking.
	if first.Value != domain.MaskedValue || first.RawValue != digestSecretA {
		t.Fatal("classification changed when no digester was configured")
	}
}

// K. The masked placeholder is never the digest input.
//
// The defect in one assertion: every sensitive variable renders as the same
// placeholder, so digesting it gives every secret on the host one token.
func TestTheMaskedPlaceholderIsNeverDigested(t *testing.T) {
	t.Parallel()

	masker := integrityMasker(t, integrityKeyA)
	secret := masker.Classify("DB_PASSWORD", digestSecretA)
	placeholder := masker.Classify("OTHER_PASSWORD", domain.MaskedValue)

	if secret.Digest == placeholder.Digest {
		t.Fatal("a real secret and the masked placeholder digested identically; the " +
			"placeholder is being used as the input")
	}
	// And the length recorded is the value's, not the placeholder's.
	if secret.ValueLength != len(digestSecretA) {
		t.Fatalf("recorded length %d, want %d", secret.ValueLength, len(digestSecretA))
	}
}

// Old evidence is readable, and is not trusted.
//
// Snapshots written before this fix carry hmac-sha256 digests computed over an
// already-emptied value: all identical, all meaningless. They cannot be
// repaired, so the only safe reading is "cannot be compared".
func TestSupersededEvidenceIsNotComparableWithCurrentEvidence(t *testing.T) {
	t.Parallel()

	current := integrityMasker(t, integrityKeyA).
		Classify("DB_PASSWORD", digestSecretA).SecretEvidence()

	// What a pre-fix snapshot holds: the old algorithm, the same key, and the
	// digest of the empty string.
	old := domain.SecretDigest{
		Present:   true,
		Digest:    "72632149a3b45216",
		Algorithm: domain.DigestHMACSHA256,
		KeyID:     current.KeyID,
	}

	if old.Comparable(current) {
		t.Fatal("superseded evidence reported itself comparable with current " +
			"evidence; a snapshot written before the fix would be read as " +
			"authoritative about whether a secret changed")
	}
	if old.Equal(current) {
		t.Fatal("superseded evidence compared EQUAL to current evidence")
	}
}

// J. Non-sensitive variables are untouched.
func TestNonSensitiveVariablesCarryNoEvidence(t *testing.T) {
	t.Parallel()

	plain := integrityMasker(t, integrityKeyA).Classify("LOG_LEVEL", "info")

	if plain.Sensitive() {
		t.Fatal("LOG_LEVEL was classified sensitive")
	}
	if plain.Value != "info" || plain.RawValue != "info" {
		t.Fatal("a non-sensitive value was altered")
	}
	if plain.Digest != "" || plain.ValueLength != 0 {
		t.Fatal("a non-sensitive variable carried digest evidence; its real value is " +
			"already stored and a digest beside it says nothing")
	}
}

// The evidence is serialisable and the value is not.
//
// The two halves of the design in one place: what survives persistence is
// exactly the comparison token, and never the thing it was computed from.
func TestOnlyTheEvidenceSurvivesEncoding(t *testing.T) {
	t.Parallel()

	secret := integrityMasker(t, integrityKeyA).Classify("DB_PASSWORD", digestSecretA)
	encoded, err := json.Marshal(secret)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if containsSubstring(string(encoded), digestSecretA) {
		t.Fatal("the raw value reached the encoded form")
	}

	var decoded domain.EnvVar
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.RawValue != "" {
		t.Fatal("RawValue survived a round trip; the persistence boundary is the " +
			"whole confidentiality guarantee")
	}
	if decoded.Digest != secret.Digest ||
		decoded.DigestAlgorithm != secret.DigestAlgorithm ||
		decoded.DigestKeyID != secret.DigestKeyID {
		t.Fatal("the comparison evidence did not survive the round trip, which is " +
			"the one thing that has to")
	}
}
