package domain_test

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Preservation and the two spellings of a Linux capability.
//
// # The defect this pins
//
// A capability has two equivalent renderings in a Docker inspect response --
// `CHOWN` and `CAP_CHOWN` -- and which one comes back depends on how the
// container was created rather than on anything an operator chose. An original
// created by one tool reports the bare names; the replacement HarborMaster
// creates from that same configuration comes back canonicalised with the
// `CAP_` prefix.
//
// preservation compared the raw strings, so the two spellings of an IDENTICAL
// capability set read as a configuration change. That failed verification,
// quarantined a healthy replacement, and left nothing serving -- the recreation
// of `sabnzbd` in execution exec_219b15d4142b10e96e45, where 53 of 55 fields
// matched and the two that did not were `security.capAdd` and
// `security.capDrop` differing by prefix alone.
//
// # Why normalising is not a loosening
//
// `NormaliseCapability` upper-cases and strips one `CAP_` prefix. It maps two
// spellings of one capability onto one name and maps nothing else together, so
// a capability the original had and the replacement lost still diverges. The
// second test below is what holds that: preservation's security section exists
// to catch a recreation that quietly dropped a restriction, and a comparison
// that cannot fail is worth nothing.

// capabilityDetail builds a container detail carrying one capability set.
func capabilityDetail(capAdd, capDrop []string) domain.ContainerDetail {
	return domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			ID:      preservationSelfID,
			ShortID: preservationSelfID[:12],
			Name:    "sabnzbd",
			Image:   domain.ImageRef{Raw: "lscr.io/linuxserver/sabnzbd:latest"},
		},
		Security: domain.Security{CapAdd: capAdd, CapDrop: capDrop},
	}
}

// comparePreservedCapabilities compares two capability sets as a recreation would.
func comparePreservedCapabilities(
	t *testing.T,
	originalAdd, originalDrop, replacementAdd, replacementDrop []string,
) domain.PreservationReport {
	t.Helper()

	original := domain.BuildPreservationSummary(
		capabilityDetail(originalAdd, originalDrop), nil)
	replacement := domain.BuildPreservationSummary(
		capabilityDetail(replacementAdd, replacementDrop), nil)

	report := domain.ComparePreservation(original, replacement)
	if report.Unverifiable {
		t.Fatalf("the comparison could not be performed, so this test establishes "+
			"nothing: %s", report.Reason)
	}
	return report
}

// The prefixed and bare spellings of one capability set are the same set.
func TestCapabilityPrefixIsNotAConfigurationChange(t *testing.T) {
	t.Parallel()

	report := comparePreservedCapabilities(t,
		[]string{"CHOWN", "NET_RAW", "SETUID"},
		[]string{"SYS_ADMIN", "NET_ADMIN"},
		[]string{"CAP_CHOWN", "CAP_NET_RAW", "CAP_SETUID"},
		[]string{"CAP_SYS_ADMIN", "CAP_NET_ADMIN"},
	)

	if report.Status != domain.VerificationPassed {
		t.Errorf("an identical capability set spelled with the CAP_ prefix must "+
			"preserve; got %s with differences %+v",
			report.Status, report.Differences)
	}
}

// A capability the replacement actually lost is still a change.
func TestALostCapabilityIsStillAConfigurationChange(t *testing.T) {
	t.Parallel()

	report := comparePreservedCapabilities(t,
		[]string{"CHOWN", "NET_RAW", "SETUID"},
		nil,
		[]string{"CAP_CHOWN", "CAP_NET_RAW"},
		nil,
	)

	if report.Status != domain.VerificationFailed {
		t.Fatalf("a replacement that dropped SETUID must fail preservation; got %s",
			report.Status)
	}

	found := false
	for _, difference := range report.Differences {
		if difference.Field == "security.capAdd" {
			found = true
		}
	}
	if !found {
		t.Errorf("the failure must name security.capAdd; got %+v", report.Differences)
	}
}

// A capability the replacement gained is still a change.
func TestAGainedCapabilityIsStillAConfigurationChange(t *testing.T) {
	t.Parallel()

	report := comparePreservedCapabilities(t,
		[]string{"CHOWN"},
		nil,
		[]string{"CAP_CHOWN", "CAP_SYS_ADMIN"},
		nil,
	)

	if report.Status != domain.VerificationFailed {
		t.Fatalf("a replacement that gained SYS_ADMIN must fail preservation; got %s",
			report.Status)
	}
}
