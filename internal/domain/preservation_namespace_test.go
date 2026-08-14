package domain_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Preservation and the hostname a shared network namespace assigns.
//
// # Why this is the same defect twice
//
// A container that joins another's network namespace has its hostname set by
// the DAEMON, to the provider's short id. Docker refuses `--hostname` with
// `--network container:<id>`, so no operator can have chosen it.
//
// explicitHostname already knew that a daemon-derived hostname must not be
// compared -- "an unset hostname is filled in by the daemon with the container's
// own short id, and comparing that across two different containers would fail
// every recreation" -- and recognised exactly one shape of it: the container's
// OWN short id. For a namespace-sharing container the value is the PROVIDER's,
// which matched nothing.
//
// So a rebind, whose entire purpose is to move a dependent from an old provider
// to a new one, changes this field by construction:
//
//	before  hostname = c610c207adf9   (the old provider)
//	after   hostname = 4ba4d5334e7a   (the new one)
//
// and preservation would report the workload as configuration-changed for doing
// precisely what it was asked to do. The same blind spot in copyConfigForCreate
// refused the create outright; see internal/docker/namespace_hostname_test.go.

const (
	preservationSelfID     = "1111111111111111111111111111111111111111111111111111111111111111"
	preservationProviderID = "2222222222222222222222222222222222222222222222222222222222222222"
)

func namespaceDetail(hostname, networkMode string) domain.ContainerDetail {
	return domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			ID:      preservationSelfID,
			ShortID: preservationSelfID[:12],
			Name:    "sonarr",
			Image:   domain.ImageRef{Raw: "linuxserver/sonarr:4.0.0"},
		},
		Process:  domain.Process{Hostname: hostname},
		Security: domain.Security{NetworkMode: networkMode},
	}
}

// hostnameField returns the compared value for one detail.
func hostnameField(t *testing.T, detail domain.ContainerDetail) string {
	t.Helper()

	summary := domain.BuildPreservationSummary(detail, nil)
	for _, field := range summary.Fields {
		if field.Name == "process.hostname" {
			return field.Value
		}
	}
	t.Fatal("the preservation summary carries no process.hostname field; this " +
		"test is no longer looking at what it thinks it is")
	return ""
}

// A rebind does not read as a configuration change.
func TestARebindDoesNotChangeTheComparedHostname(t *testing.T) {
	t.Parallel()

	const newProviderID = "3333333333333333333333333333333333333333333333333333333333333333"

	before := hostnameField(t, namespaceDetail(preservationProviderID[:12],
		"container:"+preservationProviderID))
	after := hostnameField(t, namespaceDetail(newProviderID[:12],
		"container:"+newProviderID))

	if before != after {
		t.Fatalf("compared hostname %q -> %q across a reattachment.\n\n"+
			"The daemon assigns this from whichever container's namespace is "+
			"joined, and a rebind changes that by definition. Comparing it "+
			"reports every successful reattachment as a configuration change.",
			before, after)
	}
	if before != "" {
		t.Fatalf("compared hostname = %q, want it excluded entirely", before)
	}
}

// An ordinary container's derived hostname is still excluded.
func TestAnOrdinaryDerivedHostnameIsStillExcluded(t *testing.T) {
	t.Parallel()

	if got := hostnameField(t, namespaceDetail(preservationSelfID[:12], "bridge")); got != "" {
		t.Fatalf("compared hostname = %q, want empty", got)
	}
}

// An operator's chosen hostname is still compared.
//
// The non-vacuity guard: if the field were simply always excluded, every test
// above would pass while a real configuration change went unnoticed.
func TestAnOperatorsHostnameIsStillCompared(t *testing.T) {
	t.Parallel()

	got := hostnameField(t, namespaceDetail("db-primary", "bridge"))
	if got != "db-primary" {
		t.Fatalf("compared hostname = %q, want %q; a real change would go "+
			"unnoticed", got, "db-primary")
	}

	// And a container sharing only IPC or PID keeps its own hostname compared,
	// because it holds its own network namespace.
	detail := namespaceDetail("db-primary", "bridge")
	detail.Security.IPCMode = "container:" + preservationProviderID
	detail.Security.PIDMode = "container:" + preservationProviderID
	if got := hostnameField(t, detail); got != "db-primary" {
		t.Fatalf("compared hostname = %q with only IPC/PID shared, want %q",
			got, "db-primary")
	}
}

// The exclusion is decided by the MODE, not by the value's shape.
func TestTheSharedNamespaceDecidesRatherThanTheHostnameShape(t *testing.T) {
	t.Parallel()

	got := hostnameField(t, namespaceDetail("looks-nothing-like-an-id",
		"container:"+preservationProviderID))
	if got != "" {
		t.Fatalf("compared hostname = %q, want empty; in this mode the daemon "+
			"assigns the value whatever it looks like", got)
	}
}

// The daemon-assigned hostname does not move the fingerprint.
//
// Two containers sharing the SAME provider's network namespace differ in the
// hostname the daemon filled in for them, and nothing else. The exclusion has
// to reach the fingerprint or it is cosmetic.
func TestADaemonAssignedHostnameDoesNotMoveTheFingerprint(t *testing.T) {
	t.Parallel()

	mode := "container:" + preservationProviderID

	// Same provider, two different daemon-assigned hostname values.
	before := domain.BuildPreservationSummary(
		namespaceDetail(preservationProviderID[:12], mode), nil)
	after := domain.BuildPreservationSummary(
		namespaceDetail("some-other-derived-value", mode), nil)

	if before.Fingerprint == "" || after.Fingerprint == "" {
		t.Fatal("a preservation summary produced no fingerprint")
	}
	if before.Fingerprint != after.Fingerprint {
		t.Fatalf("fingerprint %s -> %s on a hostname the daemon assigned; in this "+
			"mode the value is not configuration and must not be compared",
			shortFingerprint(before.Fingerprint), shortFingerprint(after.Fingerprint))
	}
}

// Changing which provider a container is attached to IS a difference.
//
// # Why this reverses an earlier expectation
//
// This test used to assert the opposite: that reattaching to a NEW provider
// fingerprinted identically to the old one. That was only true because
// `security.networkMode` was absent from the compared fields altogether, which
// Stage 5b found was an omission rather than a normalisation -- it also made a
// replacement on `host` indistinguishable from one on `none`.
//
// With the mode compared, a provider change is visible, which is correct: a
// container attached to the wrong provider is exactly the drift preservation
// exists to catch. The change a REBIND intends is reconciled by rewriting the
// EXPECTED configuration to the approved provider before the comparison, not by
// declining to look at the field.
func TestAttachingToADifferentProviderIsADifference(t *testing.T) {
	t.Parallel()

	const newProviderID = "3333333333333333333333333333333333333333333333333333333333333333"

	before := domain.BuildPreservationSummary(namespaceDetail(
		preservationProviderID[:12], "container:"+preservationProviderID), nil)
	after := domain.BuildPreservationSummary(namespaceDetail(
		newProviderID[:12], "container:"+newProviderID), nil)

	if before.Fingerprint == after.Fingerprint {
		t.Fatal("attaching to a different provider produced an identical fingerprint; " +
			"a container reattached to the wrong container would go unnoticed")
	}

	report := domain.ComparePreservation(before, after)
	if len(report.Differences) != 1 ||
		report.Differences[0].Field != "security.networkMode" {
		t.Fatalf("differences = %+v, want exactly [security.networkMode]; the "+
			"provider identity is the only thing that changed", report.Differences)
	}
}

// A namespace mode that is not shared is compared just as strictly.
//
// The case the missing field hid: `none`, `host` and `bridge` all carry no
// network ATTACHMENTS, so before the mode was compared they rendered
// identically and a replacement could come back on the wrong one.
func TestAnUnsharedNetworkModeIsStillCompared(t *testing.T) {
	t.Parallel()

	for _, pair := range [][2]string{
		{"none", "host"},
		{"none", "bridge"},
		{"host", "bridge"},
	} {
		before := domain.BuildPreservationSummary(namespaceDetail("web", pair[0]), nil)
		after := domain.BuildPreservationSummary(namespaceDetail("web", pair[1]), nil)
		if before.Fingerprint == after.Fingerprint {
			t.Fatalf("%q and %q fingerprinted identically; a replacement that came "+
				"back on the wrong network mode would pass verification",
				pair[0], pair[1])
		}
	}
}

func shortFingerprint(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

// Guard: the two files fixing this defect must stay in step.
//
// The docker package clears the hostname before a create and this package
// excludes it from the comparison. Fixing one without the other leaves either a
// refused create or a false preservation failure, so the shared premise is
// spelled out where both can be found from.
func TestTheSharedNamespacePrefixIsTheOneBothSidesUse(t *testing.T) {
	t.Parallel()

	if !domain.SharesNamespace("container:" + preservationProviderID) {
		t.Fatal("SharesNamespace no longer recognises a container namespace reference")
	}
	if domain.SharesNamespace("bridge") || domain.SharesNamespace("host") {
		t.Fatal("SharesNamespace matches an ordinary network mode")
	}
	if !strings.HasPrefix(domain.NamespaceContainerPrefix, "container") {
		t.Fatalf("the namespace prefix is %q, which is not what the daemon uses",
			domain.NamespaceContainerPrefix)
	}
}
