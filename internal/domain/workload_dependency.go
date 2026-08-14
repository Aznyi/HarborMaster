package domain

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// Workload dependencies: what must be stable before something else may change.
//
// # The semantic, stated once
//
//	dependent -> dependency
//
// reads "the dependent needs the dependency to be stable first". `sonarr ->
// gluetun` means gluetun must be verified before HarborMaster may recreate
// sonarr. Execution order is therefore the REVERSE of the arrow, and that
// inversion is the single most common way to misread this file -- so the graph
// builder computes stages from the arrow rather than asking any caller to
// invert it.
//
// # A dependency can only ever SUBTRACT
//
// This is the invariant the whole subsystem rests on. There is no value of any
// field here that turns a container an update policy declined into one it
// permits, and no path from a dependency record to a Docker call. Dependencies
// answer "in what order, and may this proceed at all" -- never "which
// containers may HarborMaster update", which remains the update policy's
// question alone.
//
// Concretely: nothing in this file is read by the policy selector, and nothing
// in it is read by the acquisition or execution preflights. An architecture
// test holds both.
//
// # Why relationships are keyed on NAMES
//
// A container id changes on every recreation, and a recreation is precisely the
// event this subsystem exists to survive. A dependency pinned to an id would
// stop describing the workload the moment either end of it was updated -- the
// same reasoning that keeps ids out of update policy selectors, recorded in
// store/automation_targets.go.
//
// Ids ARE retained, as evidence. Nothing decides on them.
//
// # Why a label cannot create one
//
// Anyone who can run `docker run` writes labels. Compose v2 even writes a
// `com.docker.compose.depends_on` label that looks exactly like the thing this
// file models. It is not used, and neither is any other label: a relationship
// that makes HarborMaster wait on -- or refuse -- an update is a privileged
// assertion, and the daemon enforces nothing about that label at runtime.
//
// The three discovered sources below are read from Docker CONFIGURATION that
// the daemon itself acts on. The operator source is read from a permission-
// protected, audited write. There is no third way in.

// DependencySource is where HarborMaster learned a relationship.
//
// A closed vocabulary. The zero value is not one of them, so a relationship
// whose source was never set is invalid rather than defaulting into the
// weakest -- or the strongest -- reading.
type DependencySource string

const (
	// DependencyNetworkNamespace is `network_mode: container:<other>`. The
	// dependent has no network stack of its own: it shares the dependency's.
	DependencyNetworkNamespace DependencySource = "dockerNetworkNamespace"

	// DependencyIPCNamespace is `ipc: container:<other>`.
	DependencyIPCNamespace DependencySource = "dockerIPCNamespace"

	// DependencyPIDNamespace is `pid: container:<other>`.
	DependencyPIDNamespace DependencySource = "dockerPIDNamespace"

	// DependencyOperator is an ordering an operator asserted, for an
	// application relationship Docker cannot see -- `api` needs `postgres`
	// reachable, which is true of the software and invisible to the daemon.
	DependencyOperator DependencySource = "operator"
)

// DependencySources lists every source, discovered first.
var DependencySources = []DependencySource{
	DependencyNetworkNamespace,
	DependencyIPCNamespace,
	DependencyPIDNamespace,
	DependencyOperator,
}

// DiscoveredDependencySources lists the sources HarborMaster reads off Docker.
//
// Separate from the full list because these are the ones that are DERIVED: they
// are never written to a table, and an operator can neither create nor delete
// one. Trying to is refused by name.
var DiscoveredDependencySources = []DependencySource{
	DependencyNetworkNamespace,
	DependencyIPCNamespace,
	DependencyPIDNamespace,
}

// ValidDependencySource reports whether value names a source this build knows.
func ValidDependencySource(value string) bool {
	for _, source := range DependencySources {
		if string(source) == value {
			return true
		}
	}
	return false
}

// Hard reports whether the runtime itself requires the relationship.
//
// A hard dependency is one the Docker daemon enforces: the dependent literally
// cannot run without the dependency's namespace, and destroying the dependency
// destroys the dependent's namespace with it. An operator dependency is an
// assertion about the APPLICATION -- true, useful, and not something the
// daemon knows -- so it constrains ORDER without implying that the dependent
// breaks when the dependency is replaced.
//
// The distinction is load-bearing: only a hard dependency can require a rebind.
func (s DependencySource) Hard() bool {
	switch s {
	case DependencyNetworkNamespace, DependencyIPCNamespace, DependencyPIDNamespace:
		return true
	default:
		// Includes DependencyOperator AND any unrecognised value. An unknown
		// source is not treated as hard: claiming a runtime requirement this
		// build does not understand would be asserting something HarborMaster
		// cannot check.
		return false
	}
}

// Discovered reports whether HarborMaster read the relationship off Docker.
func (s DependencySource) Discovered() bool {
	switch s {
	case DependencyNetworkNamespace, DependencyIPCNamespace, DependencyPIDNamespace:
		return true
	default:
		return false
	}
}

// Describe renders the KIND of relationship, for a UI label.
func (s DependencySource) Describe() string {
	switch s {
	case DependencyNetworkNamespace:
		return "Network namespace"
	case DependencyIPCNamespace:
		return "IPC namespace"
	case DependencyPIDNamespace:
		return "PID namespace"
	case DependencyOperator:
		return "Application ordering"
	default:
		return "An unrecognised relationship, which HarborMaster does not act on"
	}
}

// Explain is why HarborMaster believes the relationship exists.
//
// HarborMaster's own sentence, chosen from a closed vocabulary. Never a daemon
// string, never a label value, never a format verb -- the same rule invariant 13
// applies to notifications, for the same reason: this text reaches the audit
// log, the API, and the UI.
func (s DependencySource) Explain() string {
	switch s {
	case DependencyNetworkNamespace:
		return "the dependent's Docker configuration shares this container's network namespace"
	case DependencyIPCNamespace:
		return "the dependent's Docker configuration shares this container's IPC namespace"
	case DependencyPIDNamespace:
		return "the dependent's Docker configuration shares this container's PID namespace"
	case DependencyOperator:
		return "an operator recorded that the dependent needs this container"
	default:
		return "HarborMaster does not recognise how this relationship was established"
	}
}

// Origin renders who established the relationship, for the UI's two-way split.
func (s DependencySource) Origin() string {
	if s.Discovered() {
		return "Detected by HarborMaster"
	}
	if s == DependencyOperator {
		return "Configured by you"
	}
	return "Unrecognised"
}

// ------------------------------------------------------------- the record --

// WorkloadDependency is one directed relationship.
//
// There is nowhere on this type for an image, a digest, a registry, or a
// command. It names two containers and how HarborMaster came to believe they
// are related, and that is the whole of it.
type WorkloadDependency struct {
	// DependencyID is set for operator relationships only. A discovered one is
	// DERIVED from the inventory on every read and has no stored identity --
	// see the note in the repository about why there is no table for them.
	DependencyID string `json:"dependencyId,omitempty"`

	// Dependent needs Dependency. Both are container NAMES.
	Dependent  string `json:"dependent"`
	Dependency string `json:"dependency"`

	Source DependencySource `json:"source"`

	// Evidence is what HarborMaster observed. Never identity.
	Evidence DependencyEvidence `json:"evidence"`

	// CreatedAt and CreatedBy are set for operator relationships only.
	CreatedAt time.Time `json:"createdAt,omitempty"`
	CreatedBy Requester `json:"createdBy,omitzero"`
}

// Hard reports whether the runtime requires this relationship.
func (d WorkloadDependency) Hard() bool { return d.Source.Hard() }

// DependencyEvidence is what HarborMaster saw, kept so a relationship can
// explain itself without being decided on.
//
// # Why ids are here and not in the key
//
// The ids answer "which containers was this read from", which is what an
// operator needs when a relationship looks wrong. They are deliberately NOT how
// the relationship is addressed: a recreation replaces one of them within
// seconds of any update, and a subsystem that keyed on them would lose the
// relationship at exactly the moment it mattered.
type DependencyEvidence struct {
	DependentContainerID  string `json:"dependentContainerId,omitempty"`
	DependencyContainerID string `json:"dependencyContainerId,omitempty"`
	// ObservedAt is when the inventory row this was derived from was written.
	ObservedAt time.Time `json:"observedAt,omitzero"`
}

// SortDependencies orders relationships deterministically.
//
// Dependent, then dependency, then source. Every list HarborMaster returns --
// API response, graph edge list, audit reason -- goes through this, so two
// reads of an unchanged estate cannot differ.
func SortDependencies(edges []WorkloadDependency) {
	sort.SliceStable(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.Dependent != b.Dependent {
			return a.Dependent < b.Dependent
		}
		if a.Dependency != b.Dependency {
			return a.Dependency < b.Dependency
		}
		return a.Source < b.Source
	})
}

// ------------------------------------------------------ namespace parsing --

// NamespaceContainerPrefix is how Docker spells a shared namespace.
const NamespaceContainerPrefix = "container:"

// ParseNamespaceContainerRef extracts the container id a namespace mode names.
//
// # Why nothing but a full id is accepted
//
// Verified against Docker 29.6.2: the daemon RESOLVES the reference at create
// time and persists it as `container:` followed by the full 64-hex id, whatever
// the operator typed. `docker run --network container:gluetun` inspects as
// `container:07d62ee08974...`.
//
// So a mode that is not exactly that shape did not come from a running
// namespace share that this daemon resolved, and HarborMaster must not guess
// what it meant. Every other value -- `host`, `bridge`, `none`, a network name,
// a short id, a bare name -- returns false, and the caller treats a `container:`
// prefix it could not parse as a relationship it cannot establish rather than
// as no relationship at all.
//
// Deliberately NOT a name lookup. Resolving `container:gluetun` by matching a
// container called gluetun would be exactly the heuristic name parsing the
// phase brief forbids, and it would be wrong: two containers can hold that name
// over time, and the one the daemon bound is the one with the id.
func ParseNamespaceContainerRef(mode string) (string, bool) {
	if !strings.HasPrefix(mode, NamespaceContainerPrefix) {
		return "", false
	}
	id := strings.TrimPrefix(mode, NamespaceContainerPrefix)
	if !ValidFullContainerID(id) {
		return "", false
	}
	return id, true
}

// SharesNamespace reports whether a mode declares a shared container namespace
// at all, however malformed.
//
// The difference between this and ParseNamespaceContainerRef is the whole of
// the fail-closed behaviour. "This container shares somebody's namespace" and
// "this container shares NOBODY'S namespace" are opposite answers, and a mode
// carrying the prefix with an id HarborMaster cannot parse is the first, not
// the second. Reading it as the second would declare a dependent independent
// and let it be updated in any order.
func SharesNamespace(mode string) bool {
	return strings.HasPrefix(mode, NamespaceContainerPrefix)
}

// ValidFullContainerID reports whether id is a full Engine container id.
//
// Exactly 64 lowercase hex characters, matching the check the docker package
// applies before any mutation. Strict on purpose: a namespace reference is how
// one container's fate is tied to another's, and resolving a short id or a name
// could reach a different container than the one the daemon bound.
func ValidFullContainerID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		char := id[i]
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// NamespaceModes is one container's three shareable namespace declarations.
//
// Read from Docker's HostConfig and projected onto three narrow columns, so
// building the estate's graph is one query rather than a decode of every
// container's configuration blob.
type NamespaceModes struct {
	Network string `json:"networkMode,omitempty"`
	IPC     string `json:"ipcMode,omitempty"`
	PID     string `json:"pidMode,omitempty"`

	// Observed records that these three values were actually read from the
	// daemon for this container.
	//
	// # Why a positive flag rather than trusting the empty string
	//
	// An empty mode and an unobserved mode are opposite facts: the first says
	// "this container shares no namespace", the second says "HarborMaster has
	// not looked". Collapsing them would make every container in a
	// just-upgraded estate look independent for one refresh interval, which is
	// the same class of error as silently truncating a graph -- it can only
	// ever make a container appear SAFER than it is.
	//
	// The zero value is false, so a row nobody screened blocks rather than
	// clears. Migration 0024 writes 0 to every existing row for exactly that
	// reason.
	Observed bool `json:"observed"`
}

// SourceFor pairs each mode with the source it would produce.
//
// Returned in a fixed order so discovery is deterministic before any sort.
func (m NamespaceModes) SourceFor() []struct {
	Source DependencySource
	Mode   string
} {
	return []struct {
		Source DependencySource
		Mode   string
	}{
		{DependencyNetworkNamespace, m.Network},
		{DependencyIPCNamespace, m.IPC},
		{DependencyPIDNamespace, m.PID},
	}
}

// ------------------------------------------------------------ identifiers --

// DependencyIDPrefix is the fixed prefix of a generated relationship id.
const DependencyIDPrefix = "dep_"

// DependencyIDHexLength is how many hex characters follow the prefix.
const DependencyIDHexLength = 20

// NewDependencyID generates an immutable public identifier.
//
// Random rather than sequential, matching every other public id in
// HarborMaster: it appears in URLs, and a sequential one would leak how many
// relationships exist and invite a caller to walk them.
//
// Panics only if the system entropy source fails, which on every supported
// platform means the process cannot safely continue anyway.
func NewDependencyID() string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("harbormaster: system entropy source unavailable: " + err.Error())
	}
	return DependencyIDPrefix + hex.EncodeToString(raw[:])
}

// ValidDependencyID reports whether id has the shape of a generated id.
//
// Validated by SHAPE wherever it is read back, so a caller cannot probe the
// store with arbitrary strings.
func ValidDependencyID(id string) bool {
	if len(id) != len(DependencyIDPrefix)+DependencyIDHexLength {
		return false
	}
	if !strings.HasPrefix(id, DependencyIDPrefix) {
		return false
	}
	for _, char := range id[len(DependencyIDPrefix):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
