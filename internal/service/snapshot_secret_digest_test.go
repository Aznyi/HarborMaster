package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The sensitive-value comparison digest must distinguish distinct secrets.
//
// # The defect this reproduces
//
// A snapshot never stores a sensitive value. It stores a keyed HMAC instead,
// whose entire purpose is to answer one question: did this value change between
// two snapshots. Live inspection of a real estate found it answering that
// question wrongly -- three different secrets in one container produced the
// SAME digest, and so did a completely unrelated variable in another container:
//
//	DB_PASSWORD          -> 72632149a3b45216...
//	API_TOKEN            -> 72632149a3b45216...
//	REGISTRY_CREDENTIAL  -> 72632149a3b45216...
//
// Confidentiality was never at risk: no plaintext is written anywhere. What was
// lost is change detection, which is the only thing the digest exists for.
//
// # Why the round trip is the test
//
// buildSpecEnv digests domain.EnvVar.RawValue, and that is correct. But a
// snapshot is captured from HarborMaster's OWN INVENTORY, deliberately -- see
// the note on SnapshotService: "capture reads HarborMaster's own inventory" so
// that a snapshot records exactly what was observed. The inventory persists the
// detail as JSON, and EnvVar.RawValue is tagged `json:"-"` precisely so a raw
// secret can never reach the disk.
//
// So by the time capture runs, RawValue is "" for every variable, and every
// sensitive value digests the empty string. The JSON round trip below is not a
// contrivance; it is the exact step the store performs, and the defect lives in
// the seam between "never persist the value" and "digest the value".

// digestTestSecrets are three DISTINCT values. They are fixtures, not real
// credentials, and never appear in any report.
const (
	digestSecretA = "value-alpha-8f2b"
	digestSecretB = "value-bravo-19dc"
	digestSecretC = "value-charlie-4a7e"
)

// inventoryRoundTrip reproduces what the inventory repository does to a detail:
// encode it as JSON to persist, and decode it again on read.
func inventoryRoundTrip(t *testing.T, vars []domain.EnvVar) []domain.EnvVar {
	t.Helper()

	encoded, err := json.Marshal(vars)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var out []domain.EnvVar
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// digestTestMasker is the masker as the composition root builds it: classifying
// AND digesting, because those are the same decision at the same instant.
func digestTestMasker(t *testing.T) *domain.Masker {
	t.Helper()
	return domain.NewMasker(domain.DefaultMaskPatterns).
		WithDigester(NewHasher(digestTestKey(t)).DigestValue)
}

func digestTestEnv(t *testing.T) []domain.EnvVar {
	t.Helper()
	masker := digestTestMasker(t)
	return []domain.EnvVar{
		masker.Classify("DB_PASSWORD", digestSecretA),
		masker.Classify("API_TOKEN", digestSecretB),
		masker.Classify("REGISTRY_CREDENTIAL", digestSecretC),
		masker.Classify("LOG_LEVEL", "info"),
	}
}

// THE REGRESSION.
func TestDistinctSecretsProduceDistinctDigestsAfterPersistence(t *testing.T) {
	t.Parallel()

	hasher := NewHasher(digestTestKey(t))

	// Exactly what capture sees: the detail as the inventory returns it.
	persisted := inventoryRoundTrip(t, digestTestEnv(t))
	spec := buildSpecEnv(persisted, hasher)

	digests := map[string]string{}
	for _, entry := range spec {
		if entry.Sensitivity != domain.SensitivitySensitive {
			continue
		}
		digests[entry.Name] = entry.Digest
	}
	if len(digests) != 3 {
		t.Fatalf("expected three sensitive variables, got %d", len(digests))
	}

	seen := map[string]string{}
	for name, digest := range digests {
		if digest == "" {
			t.Fatalf("%s produced an empty digest; a snapshot cannot compare it at all", name)
		}
		if other, clash := seen[digest]; clash {
			t.Fatalf("%s and %s produced the SAME comparison digest from DIFFERENT "+
				"secrets\n\tThe digest exists to answer 'did this value change'. Sharing "+
				"one across distinct values makes a rotated credential indistinguishable "+
				"from an unchanged one.", other, name)
		}
		seen[digest] = name
	}
}

// A changed secret in the same variable must change that variable's digest.
func TestChangingASecretChangesItsDigest(t *testing.T) {
	t.Parallel()

	hasher := NewHasher(digestTestKey(t))
	masker := digestTestMasker(t)

	before := buildSpecEnv(inventoryRoundTrip(t,
		[]domain.EnvVar{masker.Classify("DB_PASSWORD", digestSecretA)}), hasher)
	after := buildSpecEnv(inventoryRoundTrip(t,
		[]domain.EnvVar{masker.Classify("DB_PASSWORD", digestSecretB)}), hasher)

	if before[0].Digest == after[0].Digest {
		t.Fatal("rotating a credential did not change its comparison digest; a snapshot " +
			"comparison would report the configuration as unchanged")
	}
}

// The same secret must digest the same way, so an unchanged container compares
// equal across captures.
func TestAnUnchangedSecretDigestsStably(t *testing.T) {
	t.Parallel()

	hasher := NewHasher(digestTestKey(t))
	masker := digestTestMasker(t)

	first := buildSpecEnv(inventoryRoundTrip(t,
		[]domain.EnvVar{masker.Classify("DB_PASSWORD", digestSecretA)}), hasher)
	second := buildSpecEnv(inventoryRoundTrip(t,
		[]domain.EnvVar{masker.Classify("DB_PASSWORD", digestSecretA)}), hasher)

	if first[0].Digest != second[0].Digest {
		t.Fatal("the same secret digested differently across two captures; every " +
			"comparison would report a spurious change")
	}
}

// No plaintext survives the persistence path, before or after the fix.
//
// The property the defect never broke, and the one no fix may break.
func TestNoPlaintextSecretIsPersistedOrProjected(t *testing.T) {
	t.Parallel()

	hasher := NewHasher(digestTestKey(t))
	vars := digestTestEnv(t)

	encoded, err := json.Marshal(vars)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, secret := range []string{digestSecretA, digestSecretB, digestSecretC} {
		if containsSubstring(string(encoded), secret) {
			t.Fatal("a sensitive value reached the persisted JSON form")
		}
	}

	spec := buildSpecEnv(inventoryRoundTrip(t, vars), hasher)
	projected, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("encode spec: %v", err)
	}
	for _, secret := range []string{digestSecretA, digestSecretB, digestSecretC} {
		if containsSubstring(string(projected), secret) {
			t.Fatal("a sensitive value reached the snapshot spec")
		}
	}
	for _, entry := range spec {
		if entry.Sensitivity == domain.SensitivitySensitive && entry.Value != "" {
			t.Fatalf("%s carried a value in the spec; a sensitive entry stores a digest "+
				"and nothing else", entry.Name)
		}
	}
}

func containsSubstring(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// digestTestKey builds a deterministic installation key for these tests.
//
// Composed rather than written out, like validKeyHex: a long hex literal in a
// file about secrets is indistinguishable from a real one to a scanner.
func digestTestKey(t *testing.T) SecretKey {
	t.Helper()
	key, err := LoadSecretKey(SecretKeyOptions{
		Value: strings.Repeat("3c", secretKeyBytes),
	})
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	return key
}
