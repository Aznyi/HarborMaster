package docker

import (
	"fmt"

	"github.com/moby/moby/api/types/container"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Shared namespace references, and re-pointing them at a live provider.
//
// # The defect this closes
//
// Verified against Docker 29.6.2. A container created with
// `--network container:<other>` has its reference PERSISTED AS THE RESOLVED FULL
// ID, and copyHostConfigForCreate copies that string verbatim into the
// replacement. So a recreation whose capture names a provider that has since
// been replaced sends the daemon a dead id, and the daemon refuses:
//
//	joining network namespace of container: No such container: e917703d…
//
// leaving the replacement stuck in `created` with the original already parked.
// That is the worst possible time to discover it: the host has been changed and
// the new container cannot start.
//
// # What this does about it
//
// Two narrow methods. One REPORTS the references a capture carries; one
// REWRITES them from a mapping the caller resolved. Neither reads the inventory,
// neither talks to the daemon, and neither accepts a container name -- the
// mapping is old-id to new-id, both full ids, and both validated here.
//
// The resolution itself belongs to the service layer, which is the only place
// that can consult HarborMaster's own records. This package supplies the
// mechanism and refuses anything malformed; it decides nothing.

// NamespaceKind names which namespace a reference belongs to.
//
// A closed vocabulary so a caller cannot ask for a namespace this build does
// not handle, and so the three sites that must be kept in step are one list.
type NamespaceKind string

// Namespace kinds.
const (
	NamespaceNetwork NamespaceKind = "network"
	NamespaceIPC     NamespaceKind = "ipc"
	NamespacePID     NamespaceKind = "pid"
)

// NamespaceReference is one shared-namespace declaration in a capture.
type NamespaceReference struct {
	Kind NamespaceKind
	// ContainerID is the provider the capture names, full length.
	ContainerID string
}

// ErrNamespaceRebindRefused reports that a capture's namespace references could
// not be safely rewritten.
var ErrNamespaceRebindRefused = fmt.Errorf("%w: a shared namespace reference could not be resolved",
	ErrMutationRefused)

// NamespaceReferences returns every shared-namespace declaration in the capture.
//
// Only references naming a container are returned. `bridge`, `host`, `none`, and
// a network name declare no dependency on another container and are not this
// function's business.
//
// A `container:` reference that does not parse is NOT silently dropped: it is
// returned with an empty ContainerID, so the caller sees that the capture claims
// a share it could not read and refuses rather than proceeding as if the
// container shared nothing. That is the same SharesNamespace vs Parse
// distinction the domain draws, and it fails in the same direction.
func (c *CapturedConfig) NamespaceReferences() []NamespaceReference {
	if c == nil || c.host == nil {
		return nil
	}

	modes := []struct {
		kind NamespaceKind
		mode string
	}{
		{NamespaceNetwork, string(c.host.NetworkMode)},
		{NamespaceIPC, string(c.host.IpcMode)},
		{NamespacePID, string(c.host.PidMode)},
	}

	var refs []NamespaceReference
	for _, entry := range modes {
		if !domain.SharesNamespace(entry.mode) {
			continue
		}
		// An unparseable reference yields an empty id deliberately. See above.
		id, _ := domain.ParseNamespaceContainerRef(entry.mode)
		refs = append(refs, NamespaceReference{Kind: entry.kind, ContainerID: id})
	}
	return refs
}

// RebindNamespaces re-points the capture's shared-namespace references.
//
// resolved maps the id a reference NAMES onto the id that must be used instead.
// A reference whose provider is still live is expected to map to itself, which
// keeps the ordinary case a no-op rather than a special case.
//
// Refuses, and changes NOTHING, when:
//
//   - the capture is not complete;
//   - a reference could not be parsed at all;
//   - a reference has no entry in the mapping;
//   - a replacement id is not a full container id.
//
// Refusing is the whole point. The alternative -- passing a stale reference to
// the daemon and letting it fail -- happens AFTER the original has been stopped
// and parked, which is precisely the situation this exists to prevent.
//
// The rewrite is applied to a scratch copy and only committed once every
// reference has resolved, so a partial failure cannot leave a capture carrying
// half-rewritten configuration.
func (c *CapturedConfig) RebindNamespaces(resolved map[string]string) error {
	if c == nil || c.host == nil {
		return ErrNamespaceRebindRefused
	}

	network, ipc, pid := c.host.NetworkMode, c.host.IpcMode, c.host.PidMode

	for _, ref := range c.NamespaceReferences() {
		if ref.ContainerID == "" {
			// The capture claims a share whose reference this build cannot
			// read. Nothing to map it from, so nothing may be assumed about it.
			return ErrNamespaceRebindRefused
		}
		replacement, ok := resolved[ref.ContainerID]
		if !ok || !validContainerID(replacement) {
			return ErrNamespaceRebindRefused
		}

		mode := domain.NamespaceContainerPrefix + replacement
		switch ref.Kind {
		case NamespaceNetwork:
			network = container.NetworkMode(mode)
		case NamespaceIPC:
			ipc = container.IpcMode(mode)
		case NamespacePID:
			pid = container.PidMode(mode)
		default:
			// Unreachable: NamespaceReferences produces only the three above.
			// Refused rather than ignored, so a fourth kind added without
			// updating this switch cannot pass through unrewritten.
			return ErrNamespaceRebindRefused
		}
	}

	c.host.NetworkMode, c.host.IpcMode, c.host.PidMode = network, ipc, pid

	// THE EXPECTATION MOVES WITH THE CREATE SPEC.
	//
	// `host` is what the replacement is created from; `detail` is what
	// Summary() builds the projection the replacement is VERIFIED against. They
	// describe the same container and must not disagree.
	//
	// They did. Rewriting only `host` left the expectation naming the dead
	// provider while the replacement correctly named the live one, so
	// verification failed on the single field the rebind exists to change:
	//
	//	security.ipcMode  expected container:<old>  actual container:<new>
	//
	// -- 53 of 54 fields matching, the dependent stopped and parked, the
	// operation recorded as rebindFailed. Found live in Stage 5b, scenarios N
	// and O. The network namespace escaped only because its mode was not a
	// compared field at all; closing that omission would have made all three
	// fail identically.
	//
	// This is the whole of the exception a rebind gets: the SAME approved
	// replacement ids, applied to the same three fields, from the same map. No
	// other field is touched, and the map comes from the coordinator's resolved
	// providers rather than from any caller.
	if c.detail != nil {
		c.detail.Security.NetworkMode = string(network)
		c.detail.Security.IPCMode = string(ipc)
		c.detail.Security.PIDMode = string(pid)
	}
	return nil
}
