package docker

import (
	"context"
	"sync"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Fake is an in-memory Runtime for tests.
//
// It lives in the production package so the service, API, and store test
// suites share one double instead of each growing its own. Because Docker is
// frequently unavailable in CI and on developer machines, this is the primary
// way the inventory pipeline is tested -- so it models the awkward cases
// deliberately: a container that vanishes between list and inspect, an image
// that cannot be resolved, a daemon that is simply down.
type Fake struct {
	mu sync.Mutex

	// Info and PingErr drive Ping.
	Info    Info
	PingErr error

	// Containers is returned by ListContainers.
	Containers []domain.ContainerSummary
	// ListErr, when set, fails ListContainers. A listing failure is the one
	// Docker error that fails a whole refresh.
	ListErr error

	// Inspections maps container ID to its inspection result.
	Inspections map[string]*Inspection
	// InspectErrs maps container ID to an error, overriding Inspections.
	// Use ErrContainerVanished to model inventory churn.
	InspectErrs map[string]error

	// Images maps image ID to normalized metadata.
	Images map[string]*domain.Image
	// ImageErrs maps image ID to an error.
	ImageErrs map[string]error

	Networks   []domain.Network
	NetworkErr error
	Volumes    []domain.Volume
	VolumeErr  error

	// Call counters, for asserting the image cache actually caches and that
	// nothing inspects more than it needs to.
	PingCalls    int
	ListCalls    int
	InspectCalls int
	ImageCalls   int
	NetworkCalls int
	VolumeCalls  int

	// ImageCallsByID counts per-image inspections, which is what proves the
	// per-refresh cache works.
	ImageCallsByID map[string]int
}

// NewFake returns a Fake with initialised maps.
func NewFake() *Fake {
	return &Fake{
		Inspections:    map[string]*Inspection{},
		InspectErrs:    map[string]error{},
		Images:         map[string]*domain.Image{},
		ImageErrs:      map[string]error{},
		ImageCallsByID: map[string]int{},
	}
}

// Ping implements Pinger.
func (f *Fake) Ping(ctx context.Context) (Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.PingCalls++
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if f.PingErr != nil {
		return Info{}, f.PingErr
	}
	return f.Info, nil
}

// ListContainers implements Runtime.
func (f *Fake) ListContainers(ctx context.Context) ([]domain.ContainerSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ListCalls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.ListErr != nil {
		return nil, f.ListErr
	}

	out := make([]domain.ContainerSummary, len(f.Containers))
	copy(out, f.Containers)
	return out, nil
}

// InspectContainer implements Runtime.
func (f *Fake) InspectContainer(ctx context.Context, id string) (*Inspection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.InspectCalls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err, failing := f.InspectErrs[id]; failing {
		return nil, err
	}
	if inspection, ok := f.Inspections[id]; ok {
		return inspection, nil
	}
	return nil, ErrContainerVanished
}

// InspectImage implements Runtime.
func (f *Fake) InspectImage(ctx context.Context, id string) (*domain.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ImageCalls++
	f.ImageCallsByID[id]++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err, failing := f.ImageErrs[id]; failing {
		return nil, err
	}
	if img, ok := f.Images[id]; ok {
		return img, nil
	}
	return nil, ErrImageUnavailable
}

// ListNetworks implements Runtime.
func (f *Fake) ListNetworks(ctx context.Context) ([]domain.Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.NetworkCalls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.NetworkErr != nil {
		return nil, f.NetworkErr
	}
	out := make([]domain.Network, len(f.Networks))
	copy(out, f.Networks)
	return out, nil
}

// ListVolumes implements Runtime.
func (f *Fake) ListVolumes(ctx context.Context) ([]domain.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.VolumeCalls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.VolumeErr != nil {
		return nil, f.VolumeErr
	}
	out := make([]domain.Volume, len(f.Volumes))
	copy(out, f.Volumes)
	return out, nil
}

// ImageInspectionsFor reports how many times one image was inspected.
func (f *Fake) ImageInspectionsFor(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ImageCallsByID[id]
}

var (
	_ Pinger  = (*Fake)(nil)
	_ Runtime = (*Fake)(nil)
)
