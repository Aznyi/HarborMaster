package domain_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Scope review of the preservation label exclusion.
//
// A recreation stamps domain.LineageLabel onto the replacement, and the original
// it was captured from does not carry it, so preservation had to stop comparing
// that one key or every tracked recreation would fail verification and roll back
// a replacement that was correct.
//
// The danger in that fix is over-reach. Preservation is the check that proves the
// OPERATOR's configuration survived; widening the exclusion to a prefix, to
// HarborMaster's namespace, or to labels generally would silently blind it. These
// tests exist to hold the exclusion to EXACTLY one key.

// labelled returns detail with its label set replaced.
func labelled(detail domain.ContainerDetail, labels ...domain.Label) domain.ContainerDetail {
	detail.Labels = labels
	return detail
}

func label(key, value string) domain.Label {
	return domain.Label{Key: key, Value: value, Source: domain.LabelSourceUser}
}

// TestOnlyTheLineageLabelIsExcludedFromPreservation.
//
// Every case below is a label difference that MUST still fail. The one
// difference that must not is proved separately, in
// TestPreservationIgnoresTheLineageLabelHarborMasterWritesItself.
func TestOnlyTheLineageLabelIsExcludedFromPreservation(t *testing.T) {
	base := detailFor(strings.Repeat("a", 64), "web")

	original := labelled(base,
		label("app", "web"),
		label("io.harbormaster.update.policy", "nightly"),
		label(domain.LineageLabel, "docker.io/library/nginx:1.27"),
	)

	for _, testCase := range []struct {
		name        string
		replacement domain.ContainerDetail
		why         string
	}{
		{
			name: "an operator label is removed",
			replacement: labelled(base,
				label("io.harbormaster.update.policy", "nightly"),
				label(domain.LineageLabel, "docker.io/library/nginx:1.27"),
			),
			why: "a label the recreation dropped is configuration the operator lost",
		},
		{
			name: "an operator label is added",
			replacement: labelled(base,
				label("app", "web"),
				label("injected", "by-something"),
				label("io.harbormaster.update.policy", "nightly"),
				label(domain.LineageLabel, "docker.io/library/nginx:1.27"),
			),
			why: "a label that appeared from nowhere is a configuration change nobody approved",
		},
		{
			name: "an operator label's value changes",
			replacement: labelled(base,
				label("app", "web-v2"),
				label("io.harbormaster.update.policy", "nightly"),
				label(domain.LineageLabel, "docker.io/library/nginx:1.27"),
			),
			why: "a changed value is a changed configuration",
		},
		{
			name: "ANOTHER io.harbormaster label changes",
			replacement: labelled(base,
				label("app", "web"),
				label("io.harbormaster.update.policy", "never"),
				label(domain.LineageLabel, "docker.io/library/nginx:1.27"),
			),
			why: "the exclusion is one key, not the io.harbormaster namespace; an update " +
				"policy label is the operator's and must still be compared",
		},
		{
			name: "every label disappears",
			replacement: labelled(base,
				label(domain.LineageLabel, "docker.io/library/nginx:1.27"),
			),
			why: "a recreation that lost the whole label set must never verify",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			report := domain.ComparePreservation(
				domain.BuildPreservationSummary(original, fixedDigester),
				domain.BuildPreservationSummary(testCase.replacement, fixedDigester),
			)
			if report.Status == domain.VerificationPassed {
				t.Fatalf("preservation PASSED when %s\n\t%s", testCase.name, testCase.why)
			}
		})
	}
}

// TestTheExcludedKeyIsMatchedExactlyRatherThanByPrefix.
//
// A label whose key merely BEGINS with the excluded one is a different label and
// must still be compared. Anyone can set it, so a prefix match would hand an
// unprivileged user on the host a way to make arbitrary configuration
// differences invisible to verification.
func TestTheExcludedKeyIsMatchedExactlyRatherThanByPrefix(t *testing.T) {
	base := detailFor(strings.Repeat("a", 64), "web")

	for _, key := range []string{
		domain.LineageLabel + ".extra",
		domain.LineageLabel + "-suffix",
		"prefix." + domain.LineageLabel,
		strings.ToUpper(domain.LineageLabel),
	} {
		t.Run(key, func(t *testing.T) {
			original := labelled(base, label("app", "web"), label(key, "before"))
			replacement := labelled(base, label("app", "web"), label(key, "after"))

			report := domain.ComparePreservation(
				domain.BuildPreservationSummary(original, fixedDigester),
				domain.BuildPreservationSummary(replacement, fixedDigester),
			)
			if report.Status == domain.VerificationPassed {
				t.Fatalf("a change to %q was ignored by preservation\n"+
					"\tthe exclusion must match one exact key; anything looser lets a "+
					"label anybody can set hide a configuration change", key)
			}
		})
	}
}

// TestALineageLabelIsNotTheOnlyThingHoldingUpTheComparison.
//
// A container with no labels at all still compares, and two label sets that
// differ only in the excluded key still compare EQUAL rather than becoming
// unverifiable. The filter removes a field's content, never the field.
func TestPreservationStillComparesWhenOnlyTheLineageLabelRemains(t *testing.T) {
	base := detailFor(strings.Repeat("a", 64), "web")

	original := labelled(base, label(domain.LineageLabel, "docker.io/library/nginx:1.27"))
	replacement := labelled(base, label(domain.LineageLabel, "docker.io/library/nginx:1.28"))

	report := domain.ComparePreservation(
		domain.BuildPreservationSummary(original, fixedDigester),
		domain.BuildPreservationSummary(replacement, fixedDigester),
	)
	if report.Status != domain.VerificationPassed {
		t.Fatalf("status = %s (%s); two containers differing only in HarborMaster's own "+
			"bookkeeping label are the same configuration", report.Status, report.Reason)
	}
	if report.Unverifiable {
		t.Error("the comparison became unverifiable once the excluded key was removed; " +
			"an empty label set is a known state, not an absence of evidence")
	}
}
