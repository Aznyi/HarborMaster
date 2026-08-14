package domain_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Preservation still notices a sensitive value changing, and still never says
// what it changed to.
//
// # Why this guard exists
//
// Phase 16.1 moved the SNAPSHOT's sensitive evidence to a digest taken at
// classification, because a snapshot is built from the persisted inventory
// where the raw value is already gone. Preservation is a different path: it
// compares two details read straight from the daemon, so the raw value is live
// on both sides and renderEnv digests it directly, unchanged.
//
// That distinction is easy to lose in a later refactor. If preservation ever
// started reading the carried digest instead, an unkeyed or absent digest would
// make two different secrets render identically -- and a credential swapped
// during a recreation would verify as preserved. These pin the behaviour from
// the outside so the difference is deliberate rather than incidental.

// preservationSecretDigester stands in for the installation-keyed digester the
// execution service supplies.
//
// It HASHES rather than wrapping the value, because the real one does: a stub
// that embedded the input would put the secret into every rendered field and
// the "no value in the report" assertion below would be testing the stub.
func preservationSecretDigester(value string) string {
	sum := sha256.Sum256([]byte("preservation-test:" + value))
	return hex.EncodeToString(sum[:])
}

func detailWithSecret(secret string) domain.ContainerDetail {
	return domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			ID: preservationSelfID, ShortID: preservationSelfID[:12], Name: "app",
		},
		Environment: []domain.EnvVar{
			{Name: "MODE", Value: "production", RawValue: "production"},
			{
				Name: "DB_PASSWORD", Value: domain.MaskedValue,
				Sensitivity: domain.SensitivitySensitive, RawValue: secret,
			},
		},
	}
}

// The secret survived the recreation: preservation passes.
func TestAPreservedSecretVerifies(t *testing.T) {
	t.Parallel()

	const secret = "value-alpha-8f2b"
	expected := domain.BuildPreservationSummary(detailWithSecret(secret), preservationSecretDigester)
	actual := domain.BuildPreservationSummary(detailWithSecret(secret), preservationSecretDigester)

	report := domain.ComparePreservation(expected, actual)
	if report.Status != domain.VerificationPassed {
		t.Fatalf("an unchanged secret was reported as drift: %+v", report.Differences)
	}
}

// The secret changed underneath the recreation: preservation fails, and says so
// without saying what it changed to.
func TestAChangedSecretFailsPreservationWithoutRevealingIt(t *testing.T) {
	t.Parallel()

	const before = "value-alpha-8f2b"
	const after = "value-bravo-19dc"

	expectedSummary := domain.BuildPreservationSummary(detailWithSecret(before), preservationSecretDigester)
	actualSummary := domain.BuildPreservationSummary(detailWithSecret(after), preservationSecretDigester)

	report := domain.ComparePreservation(expectedSummary, actualSummary)
	if report.Status == domain.VerificationPassed {
		t.Fatal("a replacement running a DIFFERENT secret verified as preserved; a " +
			"swapped credential would go unnoticed")
	}

	var named bool
	for _, difference := range report.Differences {
		if difference.Field == "environment" {
			named = true
		}
		// The report is operator-facing. Neither value may appear in it.
		for _, secret := range []string{before, after} {
			if strings.Contains(difference.Expected, secret) ||
				strings.Contains(difference.Actual, secret) {
				t.Fatal("a sensitive value appeared in a preservation difference")
			}
		}
	}
	if !named {
		t.Fatalf("differences = %+v, want one naming the environment", report.Differences)
	}
}

// With no digester a sensitive variable renders as UNVERIFIABLE.
//
// # What this layer does and does not guarantee
//
// It marks the field rather than distinguishing the values, and it cannot do
// otherwise: with no key there is nothing to tell two unknown secrets apart, so
// any deterministic token they both render matches itself. Two different
// secrets therefore still compare equal here.
//
// The guarantee lives one layer up: ExecutionService is always constructed with
// a Hasher in the composition root, so the nil-digester path is not reachable in
// a wired deployment. This test pins what the domain layer actually owns -- that
// the token says "unverifiable" and is never a real digest that could be
// mistaken for evidence -- rather than asserting a property the architecture
// deliberately places elsewhere. See TestPreservationWithoutADigesterCannotPass.
func TestWithoutADigesterSensitiveFieldsAreMarkedUnverifiable(t *testing.T) {
	t.Parallel()

	summary := domain.BuildPreservationSummary(detailWithSecret("value-alpha-8f2b"), nil)

	value, found := summary.FieldValue("environment")
	if !found {
		t.Fatal("the environment field was not rendered at all")
	}
	if !strings.Contains(value, "unverifiable") {
		t.Fatalf("a nil digester produced %q; a token that looks like evidence would "+
			"let two different secrets compare equal with nothing saying so", value)
	}
	if strings.Contains(value, "value-alpha-8f2b") {
		t.Fatal("the raw value was rendered when no digester was configured")
	}
}
