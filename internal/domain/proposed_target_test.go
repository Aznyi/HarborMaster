package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The tag/digest pairing invariant.
//
// The defect these guard against: a change plan that reads
// "busybox:1.36 -> busybox:1.38" while carrying 1.36's manifest digest.
// Acquisition is digest-pinned, so it would have pulled 1.36, verified it
// against the digest the plan called approved, PASSED, and recorded an update
// to 1.38 that never happened.
//
// A digest-pinned pipeline whose pin can name the wrong thing is worse than an
// unpinned one, because it reports a certainty it does not have.

func TestAProposedTargetNeedsBothHalves(t *testing.T) {
	digest := digestOf("a")

	cases := map[string]struct{ reference, digest string }{
		"no reference": {"", digest},
		"no digest":    {"busybox:1.38", ""},
		"neither":      {"", ""},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := domain.NewProposedTarget(
				testCase.reference, testCase.digest, testCase.reference)
			if !errors.Is(err, domain.ErrTargetIncomplete) {
				t.Errorf("err = %v, want ErrTargetIncomplete", err)
			}
		})
	}
}

// TestAProposedTargetRefusesADigestResolvedForAnotherReference is the
// regression test for the headline defect.
func TestAProposedTargetRefusesADigestResolvedForAnotherReference(t *testing.T) {
	_, err := domain.NewProposedTarget(
		"busybox:1.38", digestOf("a"),
		// The digest was resolved while asking about 1.36.
		"busybox:1.36",
	)

	if !errors.Is(err, domain.ErrTargetCrossed) {
		t.Fatalf("err = %v, want ErrTargetCrossed", err)
	}
}

func TestAProposedTargetRefusesAMalformedDigest(t *testing.T) {
	for name, digest := range map[string]string{
		"truncated":         "sha256:abcd",
		"no algorithm":      strings.Repeat("a", 64),
		"unknown algorithm": "md5:" + strings.Repeat("a", 32),
		"a tag":             "1.38",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := domain.NewProposedTarget("busybox:1.38", digest, "busybox:1.38")
			if !errors.Is(err, domain.ErrTargetDigestInvalid) {
				t.Errorf("err = %v, want ErrTargetDigestInvalid", err)
			}
		})
	}
}

func TestAWellFormedPairIsAccepted(t *testing.T) {
	digest := digestOf("b")

	target, err := domain.NewProposedTarget("busybox:1.38", digest, "busybox:1.38")
	if err != nil {
		t.Fatalf("NewProposedTarget: %v", err)
	}
	if !target.Valid() {
		t.Fatal("a well-formed target reports itself invalid")
	}
	if target.Reference() != "busybox:1.38" || target.Digest() != digest {
		t.Errorf("target = %q@%q", target.Reference(), target.Digest())
	}
}

// TestTheZeroTargetIsTheHonestNothing.
//
// "No proposal" is a legitimate and common assessment. It must be expressible
// without inventing a reference or a digest.
func TestTheZeroTargetIsTheHonestNothing(t *testing.T) {
	var target domain.ProposedTarget

	if target.Valid() {
		t.Error("the zero target reports itself valid")
	}
	if target.Reference() != "" || target.Digest() != "" {
		t.Errorf("the zero target carries %q@%q", target.Reference(), target.Digest())
	}
}

// TestRestoringFromPersistenceRechecksThePair.
//
// A row written by an older build can carry a crossed pair, and hand-editing
// the database is not prevented by anything. Restoring re-validates rather than
// trusting, so an unusable row reads as "nothing proposed" instead of as a
// proposal nobody can verify.
func TestRestoringFromPersistenceRechecksThePair(t *testing.T) {
	good := domain.RestoreProposedTarget("busybox:1.38", digestOf("c"))
	if !good.Valid() {
		t.Error("a well-formed stored pair did not restore")
	}

	for name, pair := range map[string][2]string{
		"a v1 row with no digest":    {"busybox:1.38", ""},
		"a malformed stored digest":  {"busybox:1.38", "sha256:abcd"},
		"a digest with no reference": {"", digestOf("d")},
	} {
		t.Run(name, func(t *testing.T) {
			restored := domain.RestoreProposedTarget(pair[0], pair[1])
			if restored.Valid() {
				t.Errorf("an unusable stored pair restored as valid: %q@%q",
					restored.Reference(), restored.Digest())
			}
		})
	}
}

// TestAPlanTargetMustBeWholeOrAbsent covers the persistence gate.
func TestAPlanTargetMustBeWholeOrAbsent(t *testing.T) {
	cases := map[string]struct {
		plan domain.ChangePlan
		want bool
	}{
		"nothing proposed": {
			domain.ChangePlan{}, true,
		},
		"a whole proposal": {
			domain.ChangePlan{ProposedImage: "busybox:1.38", ProposedDigest: digestOf("a")}, true,
		},
		"a reference with no digest": {
			domain.ChangePlan{ProposedImage: "busybox:1.38"}, false,
		},
		"a digest with no reference": {
			domain.ChangePlan{ProposedDigest: digestOf("a")}, false,
		},
		"a malformed digest": {
			domain.ChangePlan{ProposedImage: "busybox:1.38", ProposedDigest: "sha256:abcd"}, false,
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := testCase.plan.ValidTarget(); got != testCase.want {
				t.Errorf("ValidTarget = %v, want %v", got, testCase.want)
			}
		})
	}
}
