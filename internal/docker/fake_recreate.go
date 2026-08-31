package docker

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// FakeMutator is an in-memory ContainerMutator and ConfigCapturer for tests.
//
// Deliberately SEPARATE from Fake and from FakeAcquirer, for the same reason
// those two are separate from each other: a test holds the capability only when
// it asked for one, which is the property the production wiring has. A service
// handed only a Fake cannot recreate a container in a test any more than it can
// in production.
//
// It models the situations that decide whether this feature is safe and that
// are painful to produce against a real daemon: a create that fails after the
// original is already parked, a rename that collides, a stop that hangs until
// cancelled, and a daemon that goes away partway through.
type FakeMutator struct {
	mu sync.Mutex

	// Containers is the modelled host, keyed by container id.
	Containers map[string]*FakeContainer

	// Per-operation errors. Each fails the NEXT call to that operation, which
	// is what lets a test place a failure at an exact point in the pipeline.
	CaptureErr error
	CreateErr  error
	StartErr   error
	StopErr    error
	RenameErr  error
	RemoveErr  error

	// FailCreateAfter, when positive, allows that many creates before failing.
	FailCreateAfter int

	// Delay is slept inside every mutation, so a test can cancel one that is
	// genuinely in flight.
	Delay time.Duration

	// RemoveEntered and HoldRemove are a DETERMINISTIC barrier around the
	// removal of the parked original.
	//
	// That removal is the last act of a successful recreation and happens
	// AFTER the success is durably recorded -- see ExecutionService.succeed,
	// where the order is the safety property. So there is a real window in
	// which the store reads "succeeded" while the housekeeping is still
	// running, and a test that wants to observe that window has to stop time
	// at exactly that point.
	//
	// Both nil by default, so every existing test is unaffected. When set,
	// RemoveContainer signals RemoveEntered on arrival and then blocks until
	// HoldRemove is closed or the context ends. Channels rather than a sleep:
	// a duration that is long enough on one machine is a flake on another,
	// which is the class of bug this barrier exists to test for.
	RemoveEntered chan struct{}
	HoldRemove    chan struct{}

	// NextID is the id given to the next created container. Empty generates a
	// deterministic one from the call count.
	NextID string

	// Call records, in order. The ONE thing most tests assert on: the safety
	// properties of this feature are almost all statements about which
	// operations happened and in what order.
	Calls []FakeCall

	// Started is signalled on the first mutation.
	Started chan struct{}
}

// FakeCall is one recorded mutation.
type FakeCall struct {
	// Op is one of "capture", "create", "start", "stop", "rename", "remove".
	Op string
	// ContainerID is the target, and Name the new name for a rename or create.
	ContainerID string
	Name        string
	// Image is the pinned reference a create was asked for.
	Image string
}

// FakeContainer is one container on the modelled host.
type FakeContainer struct {
	ID    string
	Name  string
	Image string
	// Running reports whether it is started.
	Running bool
	// Health is the health state successive inspections report. When Health has
	// more than one entry, each inspection consumes the next, which is how a
	// test models "starting, starting, healthy".
	Health []domain.HealthState
	// Detail is the normalised configuration this container reports.
	Detail domain.ContainerDetail
	// Removed marks a container that has been removed. Retained rather than
	// deleted so a test can assert that something was NOT removed.
	Removed bool
}

// NewFakeMutator builds an empty modelled host.
func NewFakeMutator() *FakeMutator {
	return &FakeMutator{
		Containers: make(map[string]*FakeContainer),
		Started:    make(chan struct{}, 8),
	}
}

// AddContainer places a container on the modelled host.
func (f *FakeMutator) AddContainer(c *FakeContainer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Containers == nil {
		f.Containers = make(map[string]*FakeContainer)
	}
	f.Containers[c.ID] = c
}

// CaptureConfig reads a modelled container's configuration.
func (f *FakeMutator) CaptureConfig(ctx context.Context, containerID string) (*CapturedConfig, error) {
	if !validContainerID(containerID) {
		return nil, ErrMutationRefused
	}
	if err := f.pause(ctx); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.Calls = append(f.Calls, FakeCall{Op: "capture", ContainerID: containerID})
	if f.CaptureErr != nil {
		return nil, f.CaptureErr
	}

	existing, found := f.Containers[containerID]
	if !found || existing.Removed {
		return nil, ErrContainerVanished
	}

	// Copied so the capture is a snapshot, exactly as the real adapter's is: a
	// test that mutates the modelled container afterwards must not retroactively
	// change what was captured.
	detail := existing.Detail

	return &CapturedConfig{
		ContainerID:    existing.ID,
		ContainerName:  domain.NormaliseContainerName(existing.Name),
		ImageReference: existing.Image,
		ImageID:        existing.Detail.Overview.ImageID,
		CapturedAt:     time.Now().UTC(),
		// Populated so Valid() holds. The contents are never sent anywhere in a
		// test, but a capture that failed validation would exercise the refusal
		// path rather than the path under test.
		config:   &container.Config{},
		host:     &container.HostConfig{},
		networks: &network.NetworkingConfig{},
		detail:   &detail,
	}, nil
}

// CreateContainer creates a modelled replacement.
func (f *FakeMutator) CreateContainer(ctx context.Context, request CreateRequest) (string, error) {
	if err := request.Validate(); err != nil {
		return "", err
	}
	if err := f.pause(ctx); err != nil {
		return "", err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.signal()
	f.Calls = append(f.Calls, FakeCall{
		Op:          "create",
		ContainerID: request.Captured.ContainerID,
		Name:        request.Name,
		Image:       request.Image.PinnedReference(),
	})

	if f.CreateErr != nil {
		if f.FailCreateAfter <= 0 {
			return "", f.CreateErr
		}
		f.FailCreateAfter--
	}

	// The name must be free, exactly as the daemon requires. A test that
	// forgets to park the original therefore fails here rather than silently
	// modelling something Docker would refuse.
	for _, existing := range f.Containers {
		if !existing.Removed && domain.NormaliseContainerName(existing.Name) == request.Name {
			return "", ErrNameConflict
		}
	}

	id := f.NextID
	if id == "" {
		id = fakeContainerID(len(f.Containers) + 1)
	}
	f.NextID = ""

	source := f.Containers[request.Captured.ContainerID]
	created := &FakeContainer{
		ID:    id,
		Name:  request.Name,
		Image: request.Image.PinnedReference(),
	}
	if source != nil {
		// The replacement reports the source's configuration by default, which
		// is what a faithful recreation looks like. A test that wants a
		// preservation failure overwrites this afterwards.
		created.Detail = source.Detail
		created.Health = append([]domain.HealthState(nil), source.Health...)
	}
	created.Detail.Overview.ID = id
	created.Detail.Overview.ShortID = domain.ShortenID(id)
	created.Detail.Overview.Name = request.Name
	created.Detail.Overview.Image = domain.ParseImageRef(request.Image.PinnedReference())

	f.Containers[id] = created
	return id, nil
}

// StartContainer starts a modelled container.
func (f *FakeMutator) StartContainer(ctx context.Context, request StartRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := f.pause(ctx); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.signal()
	f.Calls = append(f.Calls, FakeCall{Op: "start", ContainerID: request.ContainerID})
	if f.StartErr != nil {
		return f.StartErr
	}

	existing, found := f.Containers[request.ContainerID]
	if !found || existing.Removed {
		return ErrContainerVanished
	}
	existing.Running = true
	existing.Detail.Overview.State = domain.StateRunning
	existing.Detail.State.State = domain.StateRunning
	existing.Detail.State.Running = true
	return nil
}

// StopContainer stops a modelled container.
func (f *FakeMutator) StopContainer(ctx context.Context, request StopRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := f.pause(ctx); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.signal()
	f.Calls = append(f.Calls, FakeCall{Op: "stop", ContainerID: request.ContainerID})
	if f.StopErr != nil {
		return f.StopErr
	}

	existing, found := f.Containers[request.ContainerID]
	if !found || existing.Removed {
		return ErrContainerVanished
	}
	existing.Running = false
	existing.Detail.Overview.State = domain.StateExited
	existing.Detail.State.State = domain.StateExited
	existing.Detail.State.Running = false
	return nil
}

// RenameContainer renames a modelled container.
func (f *FakeMutator) RenameContainer(ctx context.Context, request RenameRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := f.pause(ctx); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.signal()
	f.Calls = append(f.Calls, FakeCall{
		Op: "rename", ContainerID: request.ContainerID, Name: request.NewName,
	})
	if f.RenameErr != nil {
		return f.RenameErr
	}

	existing, found := f.Containers[request.ContainerID]
	if !found || existing.Removed {
		return ErrContainerVanished
	}
	for id, other := range f.Containers {
		if id == request.ContainerID || other.Removed {
			continue
		}
		if domain.NormaliseContainerName(other.Name) == request.NewName {
			return ErrNameConflict
		}
	}

	existing.Name = request.NewName
	existing.Detail.Overview.Name = request.NewName
	return nil
}

// RemoveContainer removes a modelled container.
//
// It refuses to remove a RUNNING container, exactly as the daemon does without
// force -- and the mutator has no force. A test that removes something it did
// not stop therefore fails, which is the whole point of there being no force.
func (f *FakeMutator) RemoveContainer(ctx context.Context, request RemoveRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := f.pause(ctx); err != nil {
		return err
	}
	// The barrier is taken BEFORE the lock, so a test holding the removal can
	// still read the modelled host while it waits.
	if err := f.holdRemoval(ctx); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.signal()
	f.Calls = append(f.Calls, FakeCall{Op: "remove", ContainerID: request.ContainerID})
	if f.RemoveErr != nil {
		return f.RemoveErr
	}

	existing, found := f.Containers[request.ContainerID]
	if !found || existing.Removed {
		// Already gone: idempotent, matching the real adapter.
		return nil
	}
	if existing.Running {
		return ErrMutationFailed
	}
	existing.Removed = true
	return nil
}

// pause honours Delay and cancellation outside the lock.
func (f *FakeMutator) pause(ctx context.Context) error {
	f.mu.Lock()
	delay := f.Delay
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return ctx.Err()
}

// holdRemoval implements the RemoveEntered/HoldRemove barrier.
//
// Returns the context's error if it ends first, which is what a real adapter
// would do and what keeps a held test from hanging past its deadline.
func (f *FakeMutator) holdRemoval(ctx context.Context) error {
	f.mu.Lock()
	entered, hold := f.RemoveEntered, f.HoldRemove
	f.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if hold == nil {
		return nil
	}
	select {
	case <-hold:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// signal notifies a waiting test that a mutation has begun. Called with the
// lock held.
func (f *FakeMutator) signal() {
	if f.Started == nil {
		return
	}
	select {
	case f.Started <- struct{}{}:
	default:
	}
}

// Ops returns the recorded operations in order.
func (f *FakeMutator) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.Calls))
	for _, call := range f.Calls {
		out = append(out, call.Op)
	}
	return out
}

// Present reports whether a container is still on the modelled host.
func (f *FakeMutator) Present(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	existing, found := f.Containers[id]
	return found && !existing.Removed
}

// NameOf returns a container's current name.
func (f *FakeMutator) NameOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	existing, found := f.Containers[id]
	if !found {
		return ""
	}
	return existing.Name
}

// fakeContainerID builds a deterministic 64-character id.
//
// The real adapter validates ids strictly, and the fake must produce ones that
// pass, or a test would exercise the refusal path instead of the path it means
// to.
func fakeContainerID(n int) string {
	seed := "abcdef0123456789"
	var builder strings.Builder
	builder.Grow(64)
	for i := 0; i < 64; i++ {
		builder.WriteByte(seed[(i+n)%len(seed)])
	}
	return builder.String()
}

// FakeContainerID exposes the deterministic id builder to tests in other
// packages, so a service test can place a container on the modelled host with
// an id the adapter will accept.
func FakeContainerID(n int) string { return fakeContainerID(n) }

// Compile-time checks that the fake really is both capabilities.
var (
	_ ConfigCapturer   = (*FakeMutator)(nil)
	_ ContainerMutator = (*FakeMutator)(nil)
)
