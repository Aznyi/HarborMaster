package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

func newTestHasher(t *testing.T) *Hasher {
	t.Helper()
	key, err := LoadSecretKey(SecretKeyOptions{Value: validKeyHex})
	if err != nil {
		t.Fatalf("LoadSecretKey: %v", err)
	}
	return NewHasher(key)
}

func TestHasherProducesStableDigestForSameValue(t *testing.T) {
	h := newTestHasher(t)

	a := h.Digest("hunter2")
	b := h.Digest("hunter2")

	if a.Digest != b.Digest {
		t.Error("same value produced different digests")
	}
	if a.Digest == "" {
		t.Error("Digest is empty")
	}
	if a.Algorithm != domain.DigestHMACSHA256 {
		t.Errorf("Algorithm = %q, want %q", a.Algorithm, domain.DigestHMACSHA256)
	}
	if a.KeyID == "" {
		t.Error("KeyID must be recorded so a future rotation can tell digests apart")
	}
	if !a.Present {
		t.Error("Present should be true for a set variable")
	}
	if a.Length != len("hunter2") {
		t.Errorf("Length = %d, want %d", a.Length, len("hunter2"))
	}
}

func TestHasherDigestChangesWithValue(t *testing.T) {
	h := newTestHasher(t)
	if h.Digest("a").Digest == h.Digest("b").Digest {
		t.Error("different values produced the same digest")
	}
}

// The digest must not be an unkeyed SHA-256. That is precisely what makes a
// stolen database insufficient to recover a weak secret with a wordlist.
func TestDigestIsNotPlainSHA256(t *testing.T) {
	h := newTestHasher(t)

	plain := sha256.Sum256([]byte("hunter2"))
	if h.Digest("hunter2").Digest == hex.EncodeToString(plain[:]) {
		t.Error("digest is an unkeyed SHA-256; a stolen database would be crackable offline")
	}
}

// Serialising a SecretDigest must not emit the digest, the algorithm, or the
// key ID -- the json:"-" tags are the enforcement point for the whole design.
func TestDigestNeverSerialisesSensitiveFields(t *testing.T) {
	h := newTestHasher(t)
	d := h.Digest("hunter2")

	blob, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(blob)

	if strings.Contains(rendered, "hunter2") {
		t.Errorf("serialised digest leaked the value: %s", rendered)
	}
	if strings.Contains(rendered, d.Digest) {
		t.Errorf("serialised digest leaked the digest: %s", rendered)
	}
	if strings.Contains(rendered, d.KeyID) {
		t.Errorf("serialised digest leaked the key ID: %s", rendered)
	}
	// What SHOULD survive: presence and length, which an operator needs and
	// which reveal nothing.
	if !strings.Contains(rendered, "present") || !strings.Contains(rendered, "length") {
		t.Errorf("serialised digest dropped the operator-facing fields: %s", rendered)
	}
}

func TestAbsentIsDistinctFromEmptyString(t *testing.T) {
	h := newTestHasher(t)

	absent := h.Absent()
	empty := h.Digest("")

	if absent.Present {
		t.Error("Absent().Present should be false")
	}
	if !empty.Present {
		t.Error("an empty string is still a set variable")
	}
	if absent.Equal(empty) {
		t.Error("unset and set-to-empty must not compare equal; they are different configurations")
	}
}

func TestDigestsFromDifferentKeysAreNotComparable(t *testing.T) {
	a := newTestHasher(t)

	otherKey, err := LoadSecretKey(SecretKeyOptions{Value: strings.Repeat("cd", secretKeyBytes)})
	if err != nil {
		t.Fatal(err)
	}
	b := NewHasher(otherKey)

	da, db := a.Digest("hunter2"), b.Digest("hunter2")
	if da.Comparable(db) {
		t.Error("digests from different keys reported as comparable")
	}
	if da.Equal(db) {
		t.Error("digests from different keys compared equal")
	}
}

func TestSameValueSameKeyCompares(t *testing.T) {
	h := newTestHasher(t)
	if !h.Digest("hunter2").Equal(h.Digest("hunter2")) {
		t.Error("identical values under one key did not compare equal")
	}
	if h.Digest("hunter2").Equal(h.Digest("hunter3")) {
		t.Error("different values compared equal")
	}
}

// A zero-valued digest carries no algorithm or key, so it can never be
// mistaken for a real comparison result.
func TestZeroDigestIsNotComparable(t *testing.T) {
	var zero domain.SecretDigest
	if zero.Comparable(zero) {
		t.Error("a zero digest reported as comparable to itself")
	}
}

func TestHasherKeyIDMatchesKey(t *testing.T) {
	key, err := LoadSecretKey(SecretKeyOptions{Value: validKeyHex})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHasher(key)
	if h.KeyID() != key.KeyID {
		t.Errorf("KeyID() = %q, want %q", h.KeyID(), key.KeyID)
	}
}
