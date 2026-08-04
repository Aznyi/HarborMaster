package docker

import (
	"context"
	"sync"
	"time"

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

	// ---------------------------------------------------------- events --

	// StreamErr, when set, fails StreamEvents outright. Use it to model a
	// daemon that is down when the event engine tries to connect.
	StreamErr error
	// StreamCalls counts subscription attempts, which is what a backoff test
	// asserts on.
	StreamCalls int
	// SinceValues records the `since` argument of every subscription, so a test
	// can prove a reconnect resumes rather than restarting from now.
	SinceValues []time.Time

	// streams holds the live subscriptions so a test can push events into them
	// and fail them on demand.
	streams []*fakeStream
}

// fakeStream is one subscription handed out by Fake.StreamEvents.
type fakeStream struct {
	events chan domain.DockerEvent
	errs   chan error
	done   chan struct{}
	// closed guards against a test failing the same stream twice, which would
	// otherwise panic on a double close.
	closed bool
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

// StreamEvents implements Runtime.
//
// The returned subscription stays open until the test fails it, until every
// stream is closed, or until ctx is cancelled -- the same three ways a real
// subscription ends.
func (f *Fake) StreamEvents(ctx context.Context, since time.Time) (*EventSubscription, error) {
	f.mu.Lock()
	f.StreamCalls++
	f.SinceValues = append(f.SinceValues, since)
	streamErr := f.StreamErr
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if streamErr != nil {
		return nil, streamErr
	}

	stream := &fakeStream{
		// Unbuffered, matching the SDK, so a test that pushes an event knows
		// the engine has taken it once Emit returns.
		events: make(chan domain.DockerEvent),
		errs:   make(chan error, 1),
		done:   make(chan struct{}),
	}

	f.mu.Lock()
	f.streams = append(f.streams, stream)
	f.mu.Unlock()

	// Mirrors the real adapter's ownership: one goroutine per subscription,
	// exiting on context cancellation, and it is the only closer of `events`.
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		if !stream.closed {
			stream.closed = true
			close(stream.done)
			close(stream.events)
		}
	}()

	return &EventSubscription{Events: stream.events, Errors: stream.errs}, nil
}

// Emit pushes an event into the newest open subscription.
//
// It blocks until the engine receives the event or ctx is cancelled, which is
// what lets a test assert on the effect of an event without sleeping. Reports
// false when there is no open stream or the send was abandoned.
func (f *Fake) Emit(ctx context.Context, event domain.DockerEvent) bool {
	stream := f.currentStream()
	if stream == nil {
		return false
	}

	select {
	case stream.events <- event:
		return true
	case <-stream.done:
		return false
	case <-ctx.Done():
		return false
	}
}

// FailStream ends the newest open subscription with err, modelling a dropped
// connection. Reports false when there was nothing to fail.
func (f *Fake) FailStream(err error) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i := len(f.streams) - 1; i >= 0; i-- {
		stream := f.streams[i]
		if stream.closed {
			continue
		}
		stream.closed = true
		stream.errs <- err
		close(stream.done)
		close(stream.events)
		return true
	}
	return false
}

// OpenStreams reports how many subscriptions are still live. A test asserting
// no goroutine leak checks this reaches zero after shutdown.
func (f *Fake) OpenStreams() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	open := 0
	for _, stream := range f.streams {
		if !stream.closed {
			open++
		}
	}
	return open
}

// Subscriptions reports how many times StreamEvents was called.
func (f *Fake) Subscriptions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.StreamCalls
}

// InspectCallCount reports how many times InspectContainer was called.
//
// Read the counters through these accessors, never through the fields, whenever
// the fake is shared with a running engine. The engine inspects from its own
// worker goroutines, so reading the field directly from the test goroutine is a
// data race -- one the race detector reports against whichever test happens to
// observe it.
func (f *Fake) InspectCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.InspectCalls
}

// ListCallCount reports how many times ListContainers was called.
func (f *Fake) ListCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ListCalls
}

// ImageCallCount reports how many times InspectImage was called.
func (f *Fake) ImageCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ImageCalls
}

// SetStreamErr sets or clears the subscription failure, so a test can model
// Docker going away and coming back.
func (f *Fake) SetStreamErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.StreamErr = err
}

// SetPingErr sets or clears the ping failure.
func (f *Fake) SetPingErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PingErr = err
}

func (f *Fake) currentStream() *fakeStream {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i := len(f.streams) - 1; i >= 0; i-- {
		if !f.streams[i].closed {
			return f.streams[i]
		}
	}
	return nil
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
