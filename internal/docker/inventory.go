package docker

import (
	"context"
	"errors"
	"fmt"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// ErrContainerVanished reports that a container disappeared between being
// listed and being inspected.
//
// This is routine churn on a busy host, not a fault, and the refresh treats it
// as a warning rather than a failure.
var ErrContainerVanished = errors.New("container no longer exists")

// ErrImageUnavailable reports that an image could not be inspected, usually
// because it was removed while a container still referenced it.
var ErrImageUnavailable = errors.New("image unavailable")

// Runtime is the read-only capability set the application layer depends on.
//
// Every method is an observation. There is deliberately no method that
// creates, starts, stops, removes, pulls, or executes anything, and no
// accessor that hands out the underlying SDK client -- so gaining the ability
// to mutate Docker requires editing this interface and this package, which
// makes it visible in review.
//
// The service layer depends on this interface rather than *Client, which is
// also what would let a second runtime adapter (podman, a remote agent) be
// added without touching the service or API layers.
type Runtime interface {
	Pinger

	// ListContainers returns every container, including stopped ones.
	ListContainers(ctx context.Context) ([]domain.ContainerSummary, error)
	// InspectContainer returns the full normalized view of one container,
	// along with the redacted raw inspection payload.
	InspectContainer(ctx context.Context, id string) (*Inspection, error)
	// InspectImage returns normalized image metadata.
	InspectImage(ctx context.Context, id string) (*domain.Image, error)
	// ListNetworks returns normalized network metadata.
	ListNetworks(ctx context.Context) ([]domain.Network, error)
	// ListVolumes returns normalized volume metadata.
	ListVolumes(ctx context.Context) ([]domain.Volume, error)

	// StreamEvents subscribes to the runtime's event stream, optionally
	// resuming from a point in time.
	//
	// Observation only. Subscribing changes nothing on the host, and the
	// subscription hands out HarborMaster's own event records rather than the
	// SDK's, so no caller can reach the underlying stream.
	StreamEvents(ctx context.Context, since time.Time) (*EventSubscription, error)
}

// Inspection is the result of inspecting one container.
type Inspection struct {
	// Detail is the normalized container, minus the fields only the list
	// operation knows (first/last seen, generation).
	Detail domain.ContainerDetail
	// RawJSON is the runtime's inspection payload with sensitive values
	// removed. It exists for troubleshooting and future fidelity work only.
	//
	// It is NOT a faithful copy of the container's configuration: environment
	// values and log-driver credentials have been replaced. It cannot be used
	// to recreate a container exactly.
	RawJSON []byte
	// Warnings are non-fatal problems found while normalizing this container.
	Warnings []domain.InventoryWarning
}

// ListContainers returns every container on the host, running or not.
func (c *Client) ListContainers(ctx context.Context) ([]domain.ContainerSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// All: true is the whole point -- an inventory that showed only running
	// containers would miss exactly the ones an operator is looking for.
	listed, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	out := make([]domain.ContainerSummary, 0, len(listed.Items))
	for _, summary := range listed.Items {
		out = append(out, normalizeSummary(summary))
	}
	sortContainerSummaries(out)
	return out, nil
}

// InspectContainer inspects one container and normalizes the result.
func (c *Client) InspectContainer(ctx context.Context, id string) (*Inspection, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// ContainerInspect returns the decoded record and the raw payload from the
	// SAME round trip. The v28 SDK spelled this ContainerInspectWithRaw; the
	// guarantee is unchanged, and it still matters -- inspecting twice would
	// risk the two views disagreeing.
	//
	// Size is left false deliberately: computing a container's filesystem size
	// walks the layer tree on the daemon, which is expensive and is not
	// something the inventory reports.
	inspected, err := c.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrContainerVanished, domain.ShortenID(id))
		}
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	return c.normalizeInspection(inspected.Container, inspected.Raw), nil
}

// InspectImage returns normalized metadata for one image.
func (c *Client) InspectImage(ctx context.Context, id string) (*domain.Image, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	inspected, err := c.api.ImageInspect(ctx, id)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrImageUnavailable, domain.ShortenID(id))
		}
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	image := normalizeImage(inspected.InspectResponse)
	return &image, nil
}

// ListNetworks returns normalized metadata for every network.
func (c *Client) ListNetworks(ctx context.Context) ([]domain.Network, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	listed, err := c.api.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	out := make([]domain.Network, 0, len(listed.Items))
	for _, summary := range listed.Items {
		out = append(out, normalizeNetwork(summary))
	}
	sortNetworks(out)
	return out, nil
}

// ListVolumes returns normalized metadata for every volume.
//
// Only metadata: HarborMaster never reads volume contents.
func (c *Client) ListVolumes(ctx context.Context) ([]domain.Volume, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	listed, err := c.api.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	// Items are values in the moby client; the v28 SDK returned pointers, and
	// the nil entry this loop used to skip can no longer occur.
	out := make([]domain.Volume, 0, len(listed.Items))
	for _, vol := range listed.Items {
		out = append(out, normalizeVolume(vol))
	}
	sortVolumes(out)
	return out, nil
}

// Compile-time check that Client provides the whole read-only surface.
var _ Runtime = (*Client)(nil)
