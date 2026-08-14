package service_test

import (
	"errors"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/store"
)

// The namespace re-resolution itself: old provider id -> new provider id.
//
// # Why this had no test, and what that cost
//
// ResolveNamespaceProvider is the function that performs the actual rebind. The
// execution worker calls it once per shared-namespace reference in a captured
// config, and what it returns is written straight into the replacement
// container's `NetworkMode`. It is the whole of Phase 16's effect on a host.
//
// Every test that touched it used a DOUBLE returning the identity -- four of
// them, in dependency_testsupport_test.go and execution_operation_test.go, each
// written as `return capturedID, nil`. Doubles for the callers' sake, which is
// reasonable; the problem was that no test ever exercised the real one. So the
// only case that matters -- the provider has been REPLACED and the captured id
// no longer exists -- was never run, and it never worked.
//
// Live evidence, Stage 5a, Docker 29.7.2. The follower drove the dependent all
// the way to a recreation, which then refused:
//
//	"could not establish which container now holds a shared namespace; refusing"
//	  namespace=network capturedProvider=178885f7e4ef
//
// # The defect, which had two layers
//
// Resolution went by NAME: read the name the old id held, then find the
// container present under it.
//
// The first layer was that nameForContainerID read `Endpoints`, whose query
// filters `present = 1`, while its own doc comment said it was chosen
// "deliberately ... rather than the present-only namespace rows: the container
// being looked up is by definition one that was replaced, so it may no longer be
// present." The intent was right and the query was not. The identity case
// returned early and worked; the rebind case could not resolve, ever.
//
// Reading absent rows would not have fixed it either, and this is the second
// layer -- found only by looking at the live database. A recreation renames the
// original to its parked name BEFORE removing it, so an inventory refresh
// landing in that window records the parked name against the old id. The
// retained row said:
//
//	1aa8d12a230a  present=0  hm16-provider.hm-old-exec_4197648fe5c78423e38d
//
// No lookup by stable name matches that. Recovering the stable name by stripping
// the suffix is deliberately NOT done: IsHarborMasterDerivedName documents that
// the shape is a display aid and never a security decision, because an operator
// can name a container that way themselves.
//
// # What replaced it
//
// The execution record. HarborMaster writes both ends of every swap it performs
// -- `container_id` and `replacement_id` -- so the mapping is a fact it recorded
// rather than an identity it infers from a name. Chased one hop at a time,
// bounded, and every hop re-checked against the containers present now.
//
// Refusing was the correct response to not knowing -- nothing on the host was
// touched, and the execution recorded `originalRemoved: false`. But it left the
// dependent attached to a namespace whose container had been removed, which is
// precisely the state Phase 16 exists to repair.

// A replaced provider resolves to the container now holding its name.
func TestAReplacedProviderResolvesToItsReplacement(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	// The estate as it actually moves: the provider is on id A, is replaced,
	// and is now on id B. A is gone from the host and retained in the
	// inventory, which is the only state this lookup is ever asked about.
	harness.beforeReplacement()
	harness.afterReplacement(providerBID)

	resolved, err := harness.service().ResolveNamespaceProvider(t.Context(), providerAID)
	if err != nil {
		t.Fatalf("a replaced provider could not be resolved: %v\n\n"+
			"This is the whole of the rebind. The captured config names the OLD "+
			"provider id, and what this returns is what the replacement container "+
			"is attached to. An error here refuses the recreation and leaves the "+
			"dependent bound to a container that no longer exists.", err)
	}
	if resolved != providerBID {
		t.Fatalf("resolved to %q, want the replacement %q", resolved, providerBID)
	}
}

// A provider that is still live resolves to itself.
//
// The ordinary case, and the non-vacuity guard on the one above: if resolution
// started answering with whatever it found under a name, this would still pass
// only because the name's holder IS the captured id.
func TestALiveProviderResolvesToItself(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	harness.beforeReplacement()

	resolved, err := harness.service().ResolveNamespaceProvider(t.Context(), providerAID)
	if err != nil {
		t.Fatalf("a live provider could not be resolved: %v", err)
	}
	if resolved != providerAID {
		t.Fatalf("resolved to %q, want the id it already had %q", resolved, providerAID)
	}
}

// An id HarborMaster has no record of resolves to nothing.
//
// Fails closed, and it is the reason the lookup is not simply "find any
// container by name": an id this instance never saw cannot be mapped onto one it
// did, and guessing would attach a container to an arbitrary namespace.
func TestAnUnknownProviderIDDoesNotResolve(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	harness.beforeReplacement()
	harness.afterReplacement(providerBID)

	const strangerID = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	if _, err := harness.service().ResolveNamespaceProvider(t.Context(), strangerID); err == nil {
		t.Fatal("an id HarborMaster has no record of was resolved to something")
	}
}

// A provider that was replaced by NOTHING does not resolve.
//
// The container was removed and no replacement took its name. There is nothing
// to attach to, and inventing one would produce a container the daemon refuses
// to start.
func TestAProviderReplacedByNothingDoesNotResolve(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	harness.beforeReplacement()
	harness.store.removeContainer(depProvider)

	_, err := harness.service().ResolveNamespaceProvider(t.Context(), providerAID)
	if err == nil {
		t.Fatal("a provider with no replacement resolved to something")
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound so the caller refuses", err)
	}
}

// A provider replaced TWICE resolves across both hops.
//
// The case a delayed rebind produces: the dependent's capture still names A, the
// provider has since gone A -> B -> C, and only C is present. Resolving one hop
// would land on B, which is not on the host, and refuse a reattachment that is
// perfectly possible.
func TestAProviderReplacedTwiceResolvesAcrossTheChain(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	harness.beforeReplacement()
	harness.afterReplacement(providerBID)
	harness.afterReplacement(providerCID)

	resolved, err := harness.service().ResolveNamespaceProvider(t.Context(), providerAID)
	if err != nil {
		t.Fatalf("a twice-replaced provider could not be resolved: %v", err)
	}
	if resolved != providerCID {
		t.Fatalf("resolved to %q, want the container present now %q", resolved, providerCID)
	}
}

// A cycle in the records terminates rather than spinning.
//
// Not reachable from HarborMaster's own writes -- a container id is never
// reused -- which is exactly why it is worth pinning: this walks records, and a
// walk over records that cannot terminate is a hang in the recreation path.
func TestACycleInTheReplacementRecordsDoesNotSpin(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	harness.beforeReplacement()
	// A -> B -> A.
	harness.executions.replacements[providerAID] = providerBID
	harness.executions.replacements[providerBID] = providerAID
	// Neither is present, so neither hop can terminate the walk by succeeding.
	harness.store.removeContainer(depProvider)

	if _, err := harness.service().ResolveNamespaceProvider(t.Context(), providerAID); err == nil {
		t.Fatal("a cyclic replacement chain resolved to something")
	}
}

// A malformed id is refused before any lookup.
func TestAMalformedProviderIDDoesNotResolve(t *testing.T) {
	t.Parallel()

	harness := newDepHarness()
	harness.beforeReplacement()
	harness.afterReplacement(providerBID)

	for _, id := range []string{"", "short", providerAID + "extra", "../../etc/passwd"} {
		if _, err := harness.service().ResolveNamespaceProvider(t.Context(), id); err == nil {
			t.Errorf("the malformed id %q was resolved", id)
		}
	}
}
