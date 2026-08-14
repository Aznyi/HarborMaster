package docker

import (
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

// Blocker 2: a rebind must move the EXPECTED configuration too, not only the
// configuration the replacement is created from.
//
// # The live failure this reproduces
//
// Stage 5b, scenarios N and O. A provider was updated, its hard dependent was
// recreated onto the new provider, and verification then failed on ONE field
// out of fifty-four:
//
//	IPC  security.ipcMode  expected container:d91724307172...  actual container:1695b3022d89...
//	PID  security.pidMode  expected container:93da31459e4f...  actual container:c350a46e8ffc...
//
// The "actual" value was correct. The rebind's entire purpose is to move the
// dependent from the old provider to the new one, and it did. What was wrong
// was the EXPECTATION.
//
// # Root cause
//
// RebindNamespaces rewrote `c.host` -- the HostConfig the replacement is
// created from -- and left `c.detail` untouched. `Summary()` builds the
// expected projection from `c.detail`, so the expectation still named the dead
// provider while the replacement correctly named the live one.
//
// The network namespace escaped this only by accident: `security.networkMode`
// was absent from the compared field list entirely, so the same stale
// expectation was never read. Closing that blind spot (see
// preservation_namespace_test.go) makes the network case fail in exactly the
// same way unless the rewrite is applied to both.
//
// # Why the fix is a rewrite rather than an exclusion
//
// Excluding the namespace modes from comparison would hide REAL drift during an
// ordinary recreation -- a container coming back on `host` instead of `none`,
// or attached to the wrong provider altogether. The expectation is transformed
// instead, by exactly the one approved change, from the resolution map the
// coordinator established. Everything else still has to match.

const (
	rebindSelfID  = "3333333333333333333333333333333333333333333333333333333333333333"
	rebindOldID   = "4444444444444444444444444444444444444444444444444444444444444444"
	rebindNewID   = "5555555555555555555555555555555555555555555555555555555555555555"
	rebindOtherID = "6666666666666666666666666666666666666666666666666666666666666666"
)

// sharingCapture builds a capture for a container sharing one namespace.
//
// `kind` selects which of the three the share is expressed through, so each can
// be exercised independently rather than one standing in for the others.
func sharingCapture(t *testing.T, kind NamespaceKind, providerID string) *CapturedConfig {
	t.Helper()

	modes := map[NamespaceKind]string{}
	modes[kind] = domain.NamespaceContainerPrefix + providerID
	return captureSharing(t, modes)
}

// captureSharing builds a capture whose host config and normalised detail agree
// about which namespaces are shared.
//
// The two halves are set from the SAME values deliberately: that is the
// invariant the rebind has to maintain, and a helper that let them drift would
// hide the very defect under test.
func captureSharing(t *testing.T, modes map[NamespaceKind]string) *CapturedConfig {
	t.Helper()

	host := &container.HostConfig{}
	security := domain.Security{}
	for kind, mode := range modes {
		switch kind {
		case NamespaceNetwork:
			host.NetworkMode = container.NetworkMode(mode)
			security.NetworkMode = mode
		case NamespaceIPC:
			host.IpcMode = container.IpcMode(mode)
			security.IPCMode = mode
		case NamespacePID:
			host.PidMode = container.PidMode(mode)
			security.PIDMode = mode
		default:
			t.Fatalf("unknown namespace kind %q", kind)
		}
	}

	return &CapturedConfig{
		ContainerID:    rebindSelfID,
		ContainerName:  "dependent",
		ImageReference: "alpine:3.24.0",
		ImageID:        "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000",
		config:         &container.Config{Image: "alpine:3.24.0"},
		host:           host,
		networks:       &network.NetworkingConfig{},
		detail: &domain.ContainerDetail{
			Overview: domain.ContainerSummary{ID: rebindSelfID, Name: "dependent"},
			Security: security,
		},
	}
}

// summaryField reads one field out of a preservation summary.
func summaryField(t *testing.T, summary domain.PreservationSummary, name string) (string, bool) {
	t.Helper()
	for _, field := range summary.Fields {
		if field.Name == name {
			return field.Value, true
		}
	}
	return "", false
}

// namespaceCases pairs each namespace kind with the summary field that carries
// it, so every kind is proven independently rather than by inference.
var namespaceCases = []struct {
	kind  NamespaceKind
	field string
}{
	{NamespaceNetwork, "security.networkMode"},
	{NamespaceIPC, "security.ipcMode"},
	{NamespacePID, "security.pidMode"},
}

// THE REGRESSION.
//
// After a rebind, the expected projection must name the NEW provider. Before
// the fix the IPC and PID cases carried the old id here, which is precisely
// what failed verification live.
func TestARebindMovesTheExpectedSummaryOntoTheNewProvider(t *testing.T) {
	t.Parallel()

	for _, testCase := range namespaceCases {
		t.Run(string(testCase.kind), func(t *testing.T) {
			t.Parallel()

			captured := sharingCapture(t, testCase.kind, rebindOldID)

			// Positive control: the expectation names the OLD provider first,
			// so the assertion below cannot pass by the field being absent.
			before, present := summaryField(t, captured.Summary(nil), testCase.field)
			if !present {
				t.Fatalf("%s is not a compared field; a namespace mode that is never "+
					"compared cannot detect a container coming back attached to the "+
					"wrong provider", testCase.field)
			}
			if before != domain.NamespaceContainerPrefix+rebindOldID {
				t.Fatalf("%s before rebind = %q, want the old provider", testCase.field, before)
			}

			if err := captured.RebindNamespaces(map[string]string{
				rebindOldID: rebindNewID,
			}); err != nil {
				t.Fatalf("rebind: %v", err)
			}

			after, _ := summaryField(t, captured.Summary(nil), testCase.field)
			want := domain.NamespaceContainerPrefix + rebindNewID
			if after != want {
				t.Fatalf("%s after rebind = %q, want %q\n"+
					"\tThe replacement is CREATED against the new provider, so an "+
					"expectation still naming the old one fails verification on the "+
					"one field the rebind exists to change. Blocker 2 of Stage 5b.",
					testCase.field, after, want)
			}
		})
	}
}

// A rebind that is a no-op leaves the expectation exactly as it was.
//
// The ordinary case: the provider is still live, so it maps to itself. Nothing
// about the expectation may move.
func TestANoOpRebindLeavesTheExpectationUnchanged(t *testing.T) {
	t.Parallel()

	for _, testCase := range namespaceCases {
		t.Run(string(testCase.kind), func(t *testing.T) {
			t.Parallel()

			captured := sharingCapture(t, testCase.kind, rebindOldID)
			before := captured.Summary(nil)

			if err := captured.RebindNamespaces(map[string]string{
				rebindOldID: rebindOldID,
			}); err != nil {
				t.Fatalf("rebind: %v", err)
			}

			after := captured.Summary(nil)
			if before.Fingerprint != after.Fingerprint {
				t.Fatalf("a no-op rebind changed the expected configuration\n"+
					"\tbefore %s\n\tafter  %s", before.Fingerprint, after.Fingerprint)
			}
		})
	}
}

// A rebind moves ONE field and nothing else.
//
// The exception the rebind buys is exactly one approved namespace reference. If
// the transformation could move anything else, "this is a rebind" would become
// a licence for the replacement to drift.
func TestARebindMovesExactlyOneField(t *testing.T) {
	t.Parallel()

	for _, testCase := range namespaceCases {
		t.Run(string(testCase.kind), func(t *testing.T) {
			t.Parallel()

			captured := sharingCapture(t, testCase.kind, rebindOldID)
			before := captured.Summary(nil)

			if err := captured.RebindNamespaces(map[string]string{
				rebindOldID: rebindNewID,
			}); err != nil {
				t.Fatalf("rebind: %v", err)
			}
			after := captured.Summary(nil)

			if len(before.Fields) != len(after.Fields) {
				t.Fatalf("field count changed: %d -> %d", len(before.Fields), len(after.Fields))
			}
			var moved []string
			for index := range before.Fields {
				if before.Fields[index].Name != after.Fields[index].Name {
					t.Fatalf("field order changed at %d: %q -> %q",
						index, before.Fields[index].Name, after.Fields[index].Name)
				}
				if before.Fields[index].Value != after.Fields[index].Value {
					moved = append(moved, before.Fields[index].Name)
				}
			}
			if len(moved) != 1 || moved[0] != testCase.field {
				t.Fatalf("rebind moved %v, want exactly [%s]\n"+
					"\tA rebind is one approved namespace change. Anything else moving "+
					"means the replacement is permitted to drift.", moved, testCase.field)
			}
		})
	}
}

// A container sharing TWO namespaces with the same provider moves both.
//
// Docker permits `--network container:X --ipc container:X`. Leaving one of them
// pointing at the dead provider would be a container half-attached to something
// that no longer exists, which is exactly the silent breakage the rebind exists
// to prevent.
func TestARebindMovesEveryNamespaceSharedWithTheProvider(t *testing.T) {
	t.Parallel()

	captured := captureSharing(t, map[NamespaceKind]string{
		NamespaceNetwork: domain.NamespaceContainerPrefix + rebindOldID,
		NamespaceIPC:     domain.NamespaceContainerPrefix + rebindOldID,
	})

	if err := captured.RebindNamespaces(map[string]string{rebindOldID: rebindNewID}); err != nil {
		t.Fatalf("rebind: %v", err)
	}

	summary := captured.Summary(nil)
	want := domain.NamespaceContainerPrefix + rebindNewID
	for _, field := range []string{"security.networkMode", "security.ipcMode"} {
		got, _ := summaryField(t, summary, field)
		if got != want {
			t.Fatalf("%s = %q, want %q; a dependent sharing two namespaces with one "+
				"provider must have BOTH moved, or it stays half-attached to a "+
				"container that is gone", field, got, want)
		}
	}
}

// A refused rebind changes nothing, including the expectation.
//
// RebindNamespaces applies its rewrite to a scratch copy and commits only once
// every reference resolves. That guarantee has to cover the expectation too --
// a half-rewritten expectation would compare the replacement against a
// configuration that was never approved.
func TestARefusedRebindLeavesTheExpectationUntouched(t *testing.T) {
	t.Parallel()

	captured := captureSharing(t, map[NamespaceKind]string{
		NamespaceNetwork: domain.NamespaceContainerPrefix + rebindOldID,
		NamespaceIPC:     domain.NamespaceContainerPrefix + rebindOtherID,
	})
	before := captured.Summary(nil)

	// Only ONE of the two references is resolvable, so the whole rebind must be
	// refused rather than half applied.
	if err := captured.RebindNamespaces(map[string]string{
		rebindOldID: rebindNewID,
	}); err == nil {
		t.Fatal("a rebind with an unresolvable reference was accepted")
	}

	after := captured.Summary(nil)
	if before.Fingerprint != after.Fingerprint {
		t.Fatalf("a refused rebind moved the expected configuration\n"+
			"\tbefore %s\n\tafter  %s", before.Fingerprint, after.Fingerprint)
	}
}
