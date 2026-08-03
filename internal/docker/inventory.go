package docker

import (
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

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
	summaries, err := c.api.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	out := make([]domain.ContainerSummary, 0, len(summaries))
	for _, summary := range summaries {
		out = append(out, normalizeSummary(summary))
	}
	sortContainerSummaries(out)
	return out, nil
}

// InspectContainer inspects one container and normalizes the result.
func (c *Client) InspectContainer(ctx context.Context, id string) (*Inspection, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// WithRaw so the raw payload comes from the same round trip; inspecting
	// twice would risk the two views disagreeing.
	inspected, raw, err := c.api.ContainerInspectWithRaw(ctx, id, false)
	if err != nil {
		if client.IsErrNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrContainerVanished, domain.ShortenID(id))
		}
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	return c.normalizeInspection(inspected, raw), nil
}

// InspectImage returns normalized metadata for one image.
func (c *Client) InspectImage(ctx context.Context, id string) (*domain.Image, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	inspected, err := c.api.ImageInspect(ctx, id)
	if err != nil {
		if client.IsErrNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrImageUnavailable, domain.ShortenID(id))
		}
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	image := normalizeImage(inspected)
	return &image, nil
}

// ListNetworks returns normalized metadata for every network.
func (c *Client) ListNetworks(ctx context.Context) ([]domain.Network, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	summaries, err := c.api.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	out := make([]domain.Network, 0, len(summaries))
	for _, summary := range summaries {
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

	listed, err := c.api.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	out := make([]domain.Volume, 0, len(listed.Volumes))
	for _, vol := range listed.Volumes {
		if vol == nil {
			continue
		}
		out = append(out, normalizeVolume(*vol))
	}
	sortVolumes(out)
	return out, nil
}

// Compile-time check that Client provides the whole read-only surface.
var _ Runtime = (*Client)(nil)
