package service_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// ONE modelled host, shared by all five capability interfaces.
//
// # Why this exists rather than composing the existing fakes
//
// Every other test in this package fakes one capability at a time, and that is
// right for testing one service. It cannot test the UNATTENDED LIFECYCLE,
// because the existing fakes hold separate state: docker.Fake has a container
// list, docker.FakeMutator has a different map, and a replacement the mutator
// creates is invisible to the runtime that has to verify it. Wiring those
// together means hand-syncing two maps at every step, and a test that hand-syncs
// the world is a test that can be made to pass by syncing it wrong.
//
// So this is one struct, one map, one mutex, implementing all seventeen methods
// across docker.Runtime, ImageAcquirer, ConfigCapturer, ContainerMutator and
// ContainerRollbacker. A container created here is a container the runtime
// lists. An image pulled here is an image the execution preflight can find.
// Coherence is a property of the type rather than of the test.
//
// # What it does NOT do
//
// It grants no shortcut. Every service under test holds only the interfaces the
// composition root gives it, and reaches this through those. Nothing in the
// lifecycle tests touches this host except to set the world up, to inject a
// deterministic failure, and to read what happened afterwards.

// hostContainer is one container on the modelled host.
type hostContainer struct {
	id      string
	name    string
	image   string
	imageID string
	running bool
	removed bool
	// health is what successive inspections report. The last entry repeats, so
	// a container can be modelled as permanently unhealthy.
	health []domain.HealthState
	detail domain.ContainerDetail
}

// unattendedHost is the whole Docker world for one lifecycle test.
type unattendedHost struct {
	mu sync.Mutex

	containers map[string]*hostContainer
	images     map[string]*domain.Image

	// capturer produces the CapturedConfig this host cannot build itself.
	//
	// docker.CapturedConfig carries unexported SDK structures that only
	// internal/docker can populate, and that is a boundary worth keeping: the
	// capture is the one value in the pipeline that holds raw environment
	// values, and the encapsulation is what stops anything outside the adapter
	// constructing or reading one. So capture DELEGATES to the real fake rather
	// than the fake being weakened to let a test build one.
	//
	// Kept in step with `containers` by add and CreateContainer. Only its own
	// locked methods are called, so the two mutexes never interleave.
	capturer *docker.FakeMutator

	// nextID supplies ids for containers the mutator creates. Sequential and
	// prefixed so a failure message says which one it means.
	created int

	// ops records every mutation in order. The primary evidence for "exactly
	// one recreation happened": a count of rows proves what HarborMaster
	// RECORDED, and this proves what it DID.
	ops []string

	// pulled records every digest acquired.
	pulled []string

	// failStartOf makes StartContainer fail for a container name, modelling a
	// replacement the daemon will not start.
	failStartOf string
	// badImage models an image whose process exits the moment it starts.
	//
	// The most common real update failure, and a deterministic one: a container
	// created from it is started and is immediately not running, so
	// verification's stability path sees an exited container and fails. The
	// daemon telling the truth about a bad image -- no check is weakened, and
	// the workload here declares no health check, so this is the path the
	// verification actually takes.
	badImage string
}

func newUnattendedHost() *unattendedHost {
	return &unattendedHost{
		containers: map[string]*hostContainer{},
		images:     map[string]*domain.Image{},
		capturer:   docker.NewFakeMutator(),
	}
}

// ------------------------------------------------------------- setup --

func (h *unattendedHost) add(c *hostContainer) {
	h.mu.Lock()
	h.containers[c.id] = c
	mirror := &docker.FakeContainer{
		ID: c.id, Name: c.name, Image: c.image,
		Running: c.running, Detail: c.detail,
	}
	h.mu.Unlock()

	// Outside this host's lock: AddContainer takes the fake's own.
	h.capturer.AddContainer(mirror)
}

func (h *unattendedHost) addImage(reference string, image *domain.Image) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.images[reference] = image
}

// operations returns every mutation performed, in order.
func (h *unattendedHost) operations() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ops...)
}

// pulls returns every digest acquired, in order.
func (h *unattendedHost) pulls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.pulled...)
}

// countOps counts operations whose name begins with a prefix.
func (h *unattendedHost) countOps(prefix string) int {
	count := 0
	for _, op := range h.operations() {
		if strings.HasPrefix(op, prefix) {
			count++
		}
	}
	return count
}

// byName returns the live container answering to a name.
func (h *unattendedHost) byName(name string) (hostContainer, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.containers {
		if c.name == name && !c.removed {
			return *c, true
		}
	}
	return hostContainer{}, false
}

// snapshotOf returns a copy of one container by id, removed or not.
func (h *unattendedHost) snapshotOf(id string) (hostContainer, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, known := h.containers[id]
	if !known {
		return hostContainer{}, false
	}
	return *c, true
}

// live returns every container still present, by name.
func (h *unattendedHost) live() map[string]hostContainer {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]hostContainer{}
	for _, c := range h.containers {
		if !c.removed {
			out[c.name] = *c
		}
	}
	return out
}

// record appends one operation. The caller holds the lock.
func (h *unattendedHost) record(op string) { h.ops = append(h.ops, op) }

// ------------------------------------------------------ docker.Runtime --

func (h *unattendedHost) Ping(context.Context) (docker.Info, error) {
	return docker.Info{APIVersion: "1.49", OSType: "linux"}, nil
}

func (h *unattendedHost) ListContainers(context.Context) ([]domain.ContainerSummary, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]domain.ContainerSummary, 0, len(h.containers))
	for _, c := range h.containers {
		if c.removed {
			continue
		}
		out = append(out, h.summaryOf(c))
	}
	return out, nil
}

// summaryOf renders one container as the inventory sees it. Caller holds lock.
func (h *unattendedHost) summaryOf(c *hostContainer) domain.ContainerSummary {
	summary := c.detail.Overview
	summary.ID = c.id
	summary.ShortID = domain.ShortenID(c.id)
	summary.Name = c.name
	summary.Image = domain.ParseImageRef(c.image)
	summary.ImageID = c.imageID
	summary.Present = true
	summary.State = domain.StateRunning
	if !c.running {
		summary.State = domain.StateExited
	}
	summary.Health = h.healthOf(c)
	return summary
}

// healthOf consumes the next modelled health reading. Caller holds lock.
func (h *unattendedHost) healthOf(c *hostContainer) domain.HealthState {
	if len(c.health) == 0 {
		return domain.HealthNone
	}
	next := c.health[0]
	if len(c.health) > 1 {
		c.health = c.health[1:]
	}
	return next
}

func (h *unattendedHost) InspectContainer(
	_ context.Context, id string,
) (*docker.Inspection, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	c, known := h.containers[id]
	if !known || c.removed {
		return nil, docker.ErrContainerVanished
	}

	detail := c.detail
	detail.Overview = h.summaryOf(c)
	// Running is its own field and is what verification actually reads. The
	// State enum and this boolean have to agree, because the stability check
	// consults the boolean and the exited/dead shortcut consults the enum.
	detail.State = domain.StateDetail{
		State:     detail.Overview.State,
		RawState:  string(detail.Overview.State),
		Status:    detail.Overview.Status,
		Running:   c.running,
		Health:    detail.Overview.Health,
		StartedAt: detail.State.StartedAt,
	}
	return &docker.Inspection{
		Detail:  detail,
		RawJSON: []byte(`{"Id":"` + c.id + `","Config":{"Env":["SAFE=value"]}}`),
	}, nil
}

func (h *unattendedHost) InspectImage(_ context.Context, ref string) (*domain.Image, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if image, known := h.images[ref]; known {
		return image, nil
	}
	return nil, docker.ErrImageUnavailable
}

func (h *unattendedHost) ListNetworks(context.Context) ([]domain.Network, error) {
	return []domain.Network{{ID: "net-bridge", Name: "bridge", Driver: "bridge"}}, nil
}

func (h *unattendedHost) ListVolumes(context.Context) ([]domain.Volume, error) {
	return []domain.Volume{{Name: "hm-c4c-data", Driver: "local"}}, nil
}

func (h *unattendedHost) StreamEvents(
	ctx context.Context, _ time.Time,
) (*docker.EventSubscription, error) {
	// No events. The lifecycle tests drive the schedulers directly rather than
	// through event wakeups, and a subscription that never delivers is exactly
	// what a quiet daemon looks like.
	<-ctx.Done()
	return nil, ctx.Err()
}

// -------------------------------------------------- docker.ImageAcquirer --

func (h *unattendedHost) PullByDigest(
	_ context.Context, target docker.PullTarget, progress func(docker.PullProgress),
) (docker.PullResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	reference := target.Registry + "/" + target.Repository + "@" + target.Digest
	h.record("pull:" + target.Digest)
	h.pulled = append(h.pulled, target.Digest)

	// The image becomes locally available under BOTH the digest reference and
	// the bare digest, because the execution preflight resolves it by
	// reference and the inventory records the local id.
	image := &domain.Image{
		ID:           imageIDForDigest(target.Digest),
		ShortID:      domain.ShortenID(imageIDForDigest(target.Digest)),
		RepoDigests:  []string{reference},
		OS:           target.Platform.OS,
		Architecture: target.Platform.Architecture,
		Size:         1024,
	}
	h.images[reference] = image

	if progress != nil {
		progress(docker.PullProgress{Status: "Downloading", Layers: 1})
	}
	return docker.PullResult{
		Reference: reference, Messages: 1, BytesReported: 1024, Layers: 1,
	}, nil
}

// imageIDForDigest derives a stable local image id from a manifest digest.
//
// DIFFERENT from the digest, deliberately: C3F established that an image id and
// a manifest digest are different identifiers, and a fake that made them equal
// would let a confusion between them pass every test here.
func imageIDForDigest(digest string) string {
	body := strings.TrimPrefix(digest, "sha256:")
	return "sha256:" + strings.Repeat("c", 8) + body[8:]
}

// ------------------------------------------------- docker.ConfigCapturer --

func (h *unattendedHost) CaptureConfig(
	ctx context.Context, containerID string,
) (*docker.CapturedConfig, error) {
	h.mu.Lock()
	c, known := h.containers[containerID]
	if !known || c.removed {
		h.mu.Unlock()
		return nil, docker.ErrCaptureFailed
	}
	h.record("capture:" + c.name)
	h.mu.Unlock()

	return h.capturer.CaptureConfig(ctx, containerID)
}

// ------------------------------------------------ docker.ContainerMutator --

func (h *unattendedHost) CreateContainer(
	_ context.Context, request docker.CreateRequest,
) (string, error) {
	if err := request.Validate(); err != nil {
		return "", err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	source, known := h.containers[request.Captured.ContainerID]
	if !known {
		return "", docker.ErrContainerVanished
	}

	h.created++
	id := fmt.Sprintf("%064d", h.created) // a full-length id, distinct per create
	reference := request.Image.Registry + "/" + request.Image.Repository + "@" + request.Image.Digest

	// The replacement inherits the original's configuration, which is what the
	// preservation comparison is about, and runs the NEW image.
	replacement := &hostContainer{
		id:      id,
		name:    request.Name,
		image:   reference,
		imageID: imageIDForDigest(request.Image.Digest),
		detail:  source.detail,
	}
	replacement.health = []domain.HealthState{domain.HealthHealthy}

	h.containers[id] = replacement
	h.record("create:" + request.Name)
	mirror := &docker.FakeContainer{
		ID: id, Name: request.Name, Image: reference,
		Detail: replacement.detail,
	}
	h.mu.Unlock()
	h.capturer.AddContainer(mirror)
	h.mu.Lock()
	return id, nil
}

func (h *unattendedHost) StartContainer(
	_ context.Context, request docker.StartRequest,
) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	c, known := h.containers[request.ContainerID]
	if !known || c.removed {
		return docker.ErrContainerVanished
	}
	if h.failStartOf != "" && c.name == h.failStartOf {
		h.record("start-refused:" + c.name)
		return errors.New("the daemon refused to start the container")
	}
	if h.badImage != "" && strings.Contains(c.image, h.badImage) {
		// The daemon accepted the start and the process died. `docker start`
		// returns success; the container is exited a moment later. Nothing here
		// lies to HarborMaster -- the next inspection reports what is true.
		h.record("start-exited:" + c.name)
		c.running = false
		return nil
	}
	c.running = true
	h.record("start:" + c.name)
	return nil
}

func (h *unattendedHost) StopContainer(
	_ context.Context, request docker.StopRequest,
) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	c, known := h.containers[request.ContainerID]
	if !known || c.removed {
		return docker.ErrContainerVanished
	}
	c.running = false
	h.record("stop:" + c.name)
	return nil
}

func (h *unattendedHost) RenameContainer(
	_ context.Context, request docker.RenameRequest,
) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	c, known := h.containers[request.ContainerID]
	if !known || c.removed {
		return docker.ErrContainerVanished
	}
	for _, other := range h.containers {
		if other.id != c.id && other.name == request.NewName && !other.removed {
			return docker.ErrNameConflict
		}
	}
	h.record("rename:" + c.name + "->" + request.NewName)
	c.name = request.NewName
	return nil
}

func (h *unattendedHost) RemoveContainer(
	_ context.Context, request docker.RemoveRequest,
) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	c, known := h.containers[request.ContainerID]
	if !known {
		return docker.ErrContainerVanished
	}
	if c.running {
		// The daemon refuses to remove a running container without force, and
		// HarborMaster never forces.
		return errors.New("the container is still running")
	}
	c.removed = true
	h.record("remove:" + c.name)
	return nil
}

// -------------------------------------------- docker.ContainerRollbacker --

func (h *unattendedHost) StopReplacement(
	_ context.Context, request docker.RollbackStopRequest,
) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	c, known := h.containers[request.ReplacementID]
	if !known || c.removed {
		return docker.ErrContainerVanished
	}
	c.running = false
	h.record("rb-stop:" + c.name)
	return nil
}

func (h *unattendedHost) ParkReplacement(
	_ context.Context, request docker.RollbackParkRequest,
) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	c, known := h.containers[request.ReplacementID]
	if !known || c.removed {
		return docker.ErrContainerVanished
	}
	h.record("rb-park:" + c.name + "->" + request.ParkedName)
	c.name = request.ParkedName
	return nil
}

func (h *unattendedHost) RestoreOriginalName(
	_ context.Context, request docker.RollbackRestoreRequest,
) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	c, known := h.containers[request.OriginalID]
	if !known || c.removed {
		return docker.ErrContainerVanished
	}
	for _, other := range h.containers {
		if other.id != c.id && other.name == request.Name && !other.removed {
			return docker.ErrNameConflict
		}
	}
	h.record("rb-restore:" + c.name + "->" + request.Name)
	c.name = request.Name
	return nil
}

func (h *unattendedHost) StartOriginal(
	_ context.Context, request docker.RollbackStartRequest,
) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	c, known := h.containers[request.OriginalID]
	if !known || c.removed {
		return docker.ErrContainerVanished
	}
	c.running = true
	// The restored original is healthy again: it is the image that was working
	// before the update, and nothing about it changed.
	c.health = []domain.HealthState{domain.HealthHealthy}
	h.record("rb-start:" + c.name)
	return nil
}

// The host satisfies every capability the unattended lifecycle needs, and the
// compiler says so. A capability that grew a method would break here first.
var (
	_ docker.Runtime             = (*unattendedHost)(nil)
	_ docker.ImageAcquirer       = (*unattendedHost)(nil)
	_ docker.ConfigCapturer      = (*unattendedHost)(nil)
	_ docker.ContainerMutator    = (*unattendedHost)(nil)
	_ docker.ContainerRollbacker = (*unattendedHost)(nil)
)
