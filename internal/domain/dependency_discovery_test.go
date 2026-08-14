package domain_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Full 64-hex container ids, as the daemon actually records them.
const (
	providerID  = "07d62ee08974aceac5ff1fe8a366e9187da8270855c3c5d6d02abbec6ae56c0e"
	dependentID = "eb68ac597e61f1fae5928f1d0ea94cabcbde0c2585496a0009e5ced064291ae3"
	replacedID  = "e917703d5ca6386a14ee1d250ee794f8878438c8d90d0b2961cb19714ede9067"
)

func observed(mode string) domain.NamespaceModes {
	return domain.NamespaceModes{Network: mode, Observed: true}
}

// The reference shape Docker 29.6.2 actually persists.
//
// Confirmed live: `docker run --network container:hm-ns-provider` inspects as
// `container:07d62ee08974…`, the resolved full id, never the operator's string.
func TestParseNamespaceContainerRef(t *testing.T) {
	t.Parallel()

	cases := []struct {
		mode   string
		wantID string
		wantOK bool
	}{
		{"container:" + providerID, providerID, true},

		// Everything below must NOT resolve. Each is a value the daemon can
		// legitimately report, and none of them is a namespace share this
		// build may act on.
		{"bridge", "", false},
		{"host", "", false},
		{"none", "", false},
		{"my-custom-network", "", false},
		{"", "", false},

		// A short id. Refused rather than expanded: resolving a prefix could
		// reach a different container than the one the daemon bound.
		{"container:07d62ee08974", "", false},
		// A NAME. Refused because matching it would be exactly the heuristic
		// name parsing the phase brief forbids.
		{"container:gluetun", "", false},
		// Uppercase hex. The daemon writes lowercase; anything else did not
		// come from a reference it resolved.
		{"container:" + strings.ToUpper(providerID), "", false},
		// Right length, wrong alphabet.
		{"container:" + strings.Repeat("z", 64), "", false},
		{"container:", "", false},
	}

	for _, testCase := range cases {
		t.Run(testCase.mode, func(t *testing.T) {
			t.Parallel()

			id, ok := domain.ParseNamespaceContainerRef(testCase.mode)
			if ok != testCase.wantOK || id != testCase.wantID {
				t.Fatalf("parse(%q) = (%q, %v), want (%q, %v)",
					testCase.mode, id, ok, testCase.wantID, testCase.wantOK)
			}
		})
	}
}

// The distinction the whole fail-closed design rests on.
//
// "Shares a namespace HarborMaster cannot resolve" and "shares no namespace"
// are opposite answers, and only the second one clears a container.
func TestSharesNamespaceIsNotTheSameQuestionAsParsing(t *testing.T) {
	t.Parallel()

	malformed := "container:gluetun"
	if !domain.SharesNamespace(malformed) {
		t.Fatal("a malformed container reference must still count as sharing")
	}
	if _, ok := domain.ParseNamespaceContainerRef(malformed); ok {
		t.Fatal("a malformed reference must not parse")
	}
	if domain.SharesNamespace("bridge") {
		t.Fatal("bridge shares no container namespace")
	}
}

// The ordinary case: sonarr uses gluetun's network namespace.
func TestDiscoversTheNetworkNamespaceRelationship(t *testing.T) {
	t.Parallel()

	edges, problems := domain.DiscoverDependencies([]domain.ContainerNamespaceRow{
		{ContainerID: providerID, Name: "gluetun", Modes: observed("bridge")},
		{ContainerID: dependentID, Name: "sonarr", Modes: observed("container:" + providerID)},
	})

	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if len(edges) != 1 {
		t.Fatalf("edges = %v, want one", edges)
	}
	got := edges[0]
	if got.Dependent != "sonarr" || got.Dependency != "gluetun" {
		t.Fatalf("edge = %s -> %s, want sonarr -> gluetun", got.Dependent, got.Dependency)
	}
	if got.Source != domain.DependencyNetworkNamespace || !got.Hard() {
		t.Fatalf("source = %q, hard = %v", got.Source, got.Hard())
	}
	// Ids are evidence, and the relationship is keyed on names.
	if got.Evidence.DependencyContainerID != providerID {
		t.Fatal("the provider id was not retained as evidence")
	}
	if got.DependencyID != "" {
		t.Fatal("a discovered relationship must have no stored identity")
	}
}

// All three namespaces are discovered, and each keeps its own source.
func TestDiscoversAllThreeNamespaces(t *testing.T) {
	t.Parallel()

	edges, problems := domain.DiscoverDependencies([]domain.ContainerNamespaceRow{
		{ContainerID: providerID, Name: "provider", Modes: domain.NamespaceModes{Observed: true}},
		{ContainerID: dependentID, Name: "dependent", Modes: domain.NamespaceModes{
			Network:  "container:" + providerID,
			IPC:      "container:" + providerID,
			PID:      "container:" + providerID,
			Observed: true,
		}},
	})

	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if len(edges) != 3 {
		t.Fatalf("edges = %d, want three", len(edges))
	}
	seen := make(map[domain.DependencySource]bool)
	for _, e := range edges {
		seen[e.Source] = true
		if !e.Hard() {
			t.Errorf("%s should be a hard dependency", e.Source)
		}
	}
	for _, source := range domain.DiscoveredDependencySources {
		if !seen[source] {
			t.Errorf("%s was not discovered", source)
		}
	}
}

// Every way discovery can fail must BLOCK, never clear.
//
// This is the table the phase brief's "fail closed whenever dependency state
// cannot be established safely" reduces to. Each case produces a problem and no
// edge, so the caller has something to refuse on.
func TestDiscoveryFailsClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		rows        []domain.ContainerNamespaceRow
		wantRefusal domain.DiscoveryRefusal
	}{
		{
			name: "namespace facts never read",
			rows: []domain.ContainerNamespaceRow{
				// Exactly the state migration 0024 leaves every row in.
				{ContainerID: dependentID, Name: "sonarr",
					Modes: domain.NamespaceModes{Observed: false}},
			},
			wantRefusal: domain.DiscoveryUnobserved,
		},
		{
			name: "reference is not a full id",
			rows: []domain.ContainerNamespaceRow{
				{ContainerID: dependentID, Name: "sonarr", Modes: observed("container:gluetun")},
			},
			wantRefusal: domain.DiscoveryUnparseableRef,
		},
		{
			name: "reference names a container that is gone",
			rows: []domain.ContainerNamespaceRow{
				// The state the live experiment produced: the provider was
				// recreated, so the id sonarr names no longer exists.
				{ContainerID: dependentID, Name: "sonarr", Modes: observed("container:" + replacedID)},
			},
			wantRefusal: domain.DiscoveryUnknownContainer,
		},
		{
			name: "referenced container has no usable name",
			rows: []domain.ContainerNamespaceRow{
				{ContainerID: providerID, Name: "", Modes: domain.NamespaceModes{Observed: true}},
				{ContainerID: dependentID, Name: "sonarr", Modes: observed("container:" + providerID)},
			},
			wantRefusal: domain.DiscoveryUnnamedContainer,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			edges, problems := domain.DiscoverDependencies(testCase.rows)

			// The load-bearing assertion: an unresolvable share produces NO
			// edge, which would make the container look independent -- so it
			// must produce a PROBLEM instead.
			if len(edges) != 0 {
				t.Fatalf("edges = %v, want none", edges)
			}
			found := false
			for _, problem := range problems {
				if problem.Refusal == testCase.wantRefusal {
					found = true
					if problem.Refusal.Explain() == "" {
						t.Error("the refusal has no operator-facing explanation")
					}
				}
			}
			if !found {
				t.Fatalf("problems = %v, want a %q refusal", problems, testCase.wantRefusal)
			}
		})
	}
}

// Only the stale-binding refusal is evidence that a rebind is needed. The
// others are states of not knowing.
func TestOnlyAnUnknownContainerSignalsARebind(t *testing.T) {
	t.Parallel()

	if !domain.DiscoveryUnknownContainer.RebindSignal() {
		t.Fatal("a reference to a replaced container is the rebind signal")
	}
	for _, refusal := range []domain.DiscoveryRefusal{
		domain.DiscoveryUnobserved,
		domain.DiscoveryUnparseableRef,
		domain.DiscoveryUnnamedContainer,
	} {
		if refusal.RebindSignal() {
			t.Errorf("%q must not be read as a rebind signal", refusal)
		}
	}
}

// A container that shares nothing is the only case producing neither an edge
// nor a problem.
func TestAContainerSharingNothingIsClean(t *testing.T) {
	t.Parallel()

	edges, problems := domain.DiscoverDependencies([]domain.ContainerNamespaceRow{
		{ContainerID: providerID, Name: "standalone", Modes: domain.NamespaceModes{
			Network: "bridge", IPC: "private", PID: "", Observed: true,
		}},
	})

	if len(edges) != 0 || len(problems) != 0 {
		t.Fatalf("edges = %v, problems = %v, want both empty", edges, problems)
	}
}

// Discovery output must not depend on row order.
func TestDiscoveryIsDeterministic(t *testing.T) {
	t.Parallel()

	forward := []domain.ContainerNamespaceRow{
		{ContainerID: providerID, Name: "gluetun", Modes: observed("bridge")},
		{ContainerID: dependentID, Name: "sonarr", Modes: observed("container:" + providerID)},
		{ContainerID: replacedID, Name: "radarr", Modes: observed("container:" + providerID)},
	}
	reversed := []domain.ContainerNamespaceRow{forward[2], forward[1], forward[0]}

	first, _ := domain.DiscoverDependencies(forward)
	second, _ := domain.DiscoverDependencies(reversed)

	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("edges = %d and %d, want two each", len(first), len(second))
	}
	for i := range first {
		if first[i].Dependent != second[i].Dependent ||
			first[i].Dependency != second[i].Dependency {
			t.Fatalf("discovery order differs at %d: %v vs %v", i, first[i], second[i])
		}
	}
}

// A label must never create a relationship. There is no code path from a label
// to an edge, and this pins it: a container carrying every label that has ever
// been suggested as a dependency source produces nothing.
func TestLabelsCannotCreateARelationship(t *testing.T) {
	t.Parallel()

	// Discovery does not even accept labels -- ContainerNamespaceRow has no
	// field for them. That is the real defence, and this test documents it by
	// showing the only inputs that exist.
	edges, problems := domain.DiscoverDependencies([]domain.ContainerNamespaceRow{
		{ContainerID: providerID, Name: "db", Modes: observed("bridge")},
		{ContainerID: dependentID, Name: "api", Modes: observed("bridge")},
	})

	if len(edges) != 0 || len(problems) != 0 {
		t.Fatalf("two ordinary containers produced edges %v / problems %v", edges, problems)
	}
}
