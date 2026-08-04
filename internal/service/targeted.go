package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Targeted inventory refresh.
//
// A Docker event says a resource MAY have changed. It never says what the
// resource now looks like, and HarborMaster never believes it if it did: these
// methods re-read the resource through the same docker.Runtime the full refresh
// uses and persist it through the same repository helpers. Nothing here parses
// an event payload into container state.
//
// Concurrency: targeted writes are serialised against each other by targetedMu,
// so two events for the same container cannot interleave their writes. They are
// NOT serialised against a full refresh -- a full refresh can take minutes on a
// large host, and blocking every event behind it would defeat the point. That
// is safe because a full refresh commits atomically at a NEW generation and
// re-reads every container itself, so it overwrites whatever a concurrent
// targeted write left behind rather than merging with it.

// ErrTargetedUnavailable reports that targeted refresh cannot run because no
// full inventory has been persisted yet.
var ErrTargetedUnavailable = errors.New("targeted refresh needs an existing inventory")

// targetedMu guards single-resource writes. It is a field rather than a package
// variable so two InventoryService instances in a test do not contend.
type targetedState struct {
	mu sync.Mutex
}

// RefreshContainer re-inspects one container by full ID and persists it.
//
// A container that has vanished between the event and the inspection is not an
// error: it is the ordinary outcome of a destroy event racing its own refresh.
// The container is marked absent instead, which is the same conclusion a full
// refresh would reach.
func (s *InventoryService) RefreshContainer(ctx context.Context, containerID string) error {
	if !s.cfg.Enabled {
		return ErrInventoryDisabled
	}
	if containerID == "" {
		return errors.New("container id is required")
	}

	inspection, err := s.runtime.InspectContainer(ctx, containerID)
	if err != nil {
		if errors.Is(err, docker.ErrContainerVanished) {
			// It is gone. Recording that is the correct, complete answer.
			return s.MarkContainerAbsent(ctx, containerID)
		}
		return err
	}

	detail := inspection.Detail
	if detail.Overview.ID == "" {
		// An inspection that produced no identity cannot be persisted safely;
		// the caller escalates to a full reconciliation.
		return errors.New("inspection returned no container identity")
	}

	s.targeted.mu.Lock()
	defer s.targeted.mu.Unlock()

	err = s.inventory.UpsertContainer(ctx, store.ContainerRecord{
		Detail:  detail,
		RawJSON: inspection.RawJSON,
	}, s.now())
	if errors.Is(err, store.ErrNoInventory) {
		return ErrTargetedUnavailable
	}
	if err != nil {
		return err
	}

	// The container's image may be one HarborMaster has not recorded, which is
	// the normal case for a container created from a freshly pulled image.
	// Resolving it here keeps the detail view complete without waiting for the
	// next full sweep. A failure is not fatal: an image removed while still in
	// use is a normal state, and the container row is already correct.
	if imageID := detail.Overview.ImageID; imageID != "" {
		if image, imageErr := s.runtime.InspectImage(ctx, imageID); imageErr == nil {
			if err := s.inventory.UpsertImage(ctx, *image, s.now()); err != nil {
				s.logger.DebugContext(ctx, "targeted image write failed",
					slog.String("error", err.Error()))
			}
		}
	}

	s.logger.DebugContext(ctx, "targeted container refresh complete",
		slog.String("container", domain.ShortenID(containerID)),
		slog.String("state", string(detail.Overview.State)))
	return nil
}

// MarkContainerAbsent records a container as no longer present.
//
// Called for a confirmed destroy. A container HarborMaster never inventoried is
// not an error: a `--rm` container can be created and destroyed entirely
// between two sweeps, so its destroy event is the first and only thing seen.
func (s *InventoryService) MarkContainerAbsent(ctx context.Context, containerID string) error {
	if !s.cfg.Enabled {
		return ErrInventoryDisabled
	}

	s.targeted.mu.Lock()
	defer s.targeted.mu.Unlock()

	err := s.inventory.MarkContainerAbsent(ctx, containerID, s.now())
	if errors.Is(err, store.ErrNotFound) {
		s.logger.DebugContext(ctx, "destroy event for an uninventoried container",
			slog.String("container", domain.ShortenID(containerID)))
		return nil
	}
	return err
}

// RefreshImage re-inspects one image and persists its metadata.
//
// An image that cannot be inspected is not an error either: `docker rmi` on an
// image a container still references leaves exactly that state, and it is
// reported by the image events HarborMaster is reacting to in the first place.
func (s *InventoryService) RefreshImage(ctx context.Context, reference string) error {
	if !s.cfg.Enabled {
		return ErrInventoryDisabled
	}
	if reference == "" {
		return errors.New("image reference is required")
	}

	image, err := s.runtime.InspectImage(ctx, reference)
	if err != nil {
		if errors.Is(err, docker.ErrImageUnavailable) {
			// Nothing to write. The image is genuinely gone, and removing its
			// row is a full refresh's job: a container may still reference it,
			// and dropping it here would break that container's detail view.
			return nil
		}
		return err
	}

	s.targeted.mu.Lock()
	defer s.targeted.mu.Unlock()

	if err := s.inventory.UpsertImage(ctx, *image, s.now()); err != nil {
		if errors.Is(err, store.ErrNoInventory) {
			return ErrTargetedUnavailable
		}
		return err
	}
	return nil
}

// RefreshNetworks re-reads the network catalog.
//
// A set operation rather than a single-network read: see
// store.InventoryRepository.ReplaceNetworks for why.
func (s *InventoryService) RefreshNetworks(ctx context.Context) error {
	if !s.cfg.Enabled {
		return ErrInventoryDisabled
	}

	networks, err := s.runtime.ListNetworks(ctx)
	if err != nil {
		return err
	}

	s.targeted.mu.Lock()
	defer s.targeted.mu.Unlock()

	if err := s.inventory.ReplaceNetworks(ctx, networks, s.now()); err != nil {
		if errors.Is(err, store.ErrNoInventory) {
			return ErrTargetedUnavailable
		}
		return err
	}
	return nil
}

// RefreshVolumes re-reads the volume catalog.
func (s *InventoryService) RefreshVolumes(ctx context.Context) error {
	if !s.cfg.Enabled {
		return ErrInventoryDisabled
	}

	volumes, err := s.runtime.ListVolumes(ctx)
	if err != nil {
		return err
	}

	s.targeted.mu.Lock()
	defer s.targeted.mu.Unlock()

	if err := s.inventory.ReplaceVolumes(ctx, volumes, s.now()); err != nil {
		if errors.Is(err, store.ErrNoInventory) {
			return ErrTargetedUnavailable
		}
		return err
	}
	return nil
}

// Reconcile runs one full inventory refresh through the Phase 2 pipeline and
// waits for it to finish.
//
// This is the event engine's escalation path. It reuses Refresh rather than
// reimplementing anything, so a reconciliation and a manual refresh produce
// byte-identical inventories, advance the generation the same way, and compute
// the same checksum.
//
// Returns ErrRefreshInProgress when one is already running, which the caller
// treats as success: a sweep that is already under way satisfies the request.
func (s *InventoryService) Reconcile(ctx context.Context, trigger domain.RefreshTrigger) error {
	_, err := s.Refresh(ctx, trigger)
	return err
}

// maxTargetedRefresh bounds one targeted operation.
//
// Much shorter than maxAsyncRefresh: this inspects one resource, so a call that
// has not returned in this long means the daemon is not answering, and the
// right response is to give the slot back rather than hold the queue.
const maxTargetedRefresh = 60 * time.Second

// TargetedContext derives the bounded context a targeted refresh runs under.
//
// Exported so the event engine uses the same bound rather than inventing one,
// and so the relationship between the two is stated in one place.
func TargetedContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, maxTargetedRefresh)
}
