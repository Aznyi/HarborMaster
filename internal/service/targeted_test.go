package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Targeted refresh tests.
//
// The property under test throughout: a targeted refresh RE-READS the resource
// through the same adapter a full refresh uses, and joins the current
// generation without advancing it. Anything that wrote container state from an
// event payload, or minted a new generation for one row, would be a bug.

func TestTargetedRefreshUpdatesOneContainer(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(3)
	svc := newService(t, fake, db)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, domain.TriggerStartup); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	// The container stops out from under HarborMaster. Only the adapter knows;
	// the event would just say "something happened".
	stopped := fake.Inspections["container-001"]
	stopped.Detail.Overview.State = domain.StateExited
	stopped.Detail.State.State = domain.StateExited

	before := fake.InspectCalls
	if err := svc.RefreshContainer(ctx, "container-001"); err != nil {
		t.Fatalf("RefreshContainer: %v", err)
	}

	if fake.InspectCalls != before+1 {
		t.Errorf("inspected %d times, want exactly one targeted inspection",
			fake.InspectCalls-before)
	}

	detail, err := db.Containers.Get(ctx, "container-001")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if detail.Overview.State != domain.StateExited {
		t.Errorf("state = %q, want exited", detail.Overview.State)
	}

	// The other containers must be untouched, and still present.
	for _, id := range []string{"container-000", "container-002"} {
		other, err := db.Containers.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if !other.Overview.Present {
			t.Errorf("%s was marked absent by a refresh of a different container", id)
		}
	}
}

// Generation and checksum mean "a complete sweep observed this". A single-row
// update is not that, so advancing them would tell a client its whole inventory
// had been re-verified when one container had been touched.
func TestTargetedRefreshDoesNotAdvanceTheGeneration(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(2)
	svc := newService(t, fake, db)
	ctx := context.Background()

	record, err := svc.Refresh(ctx, domain.TriggerStartup)
	if err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	if err := svc.RefreshContainer(ctx, "container-000"); err != nil {
		t.Fatalf("RefreshContainer: %v", err)
	}

	generation, checksum, err := db.Inventory.CurrentGeneration(ctx)
	if err != nil {
		t.Fatalf("CurrentGeneration: %v", err)
	}
	if generation != record.Generation {
		t.Errorf("generation = %d, want it unchanged at %d", generation, record.Generation)
	}
	if checksum != record.Checksum {
		t.Errorf("checksum changed after a targeted refresh; it describes a full sweep only")
	}
}

// A container that vanished between the event and the inspection is the
// ordinary outcome of a destroy racing its own refresh, not an error.
func TestTargetedRefreshOfAVanishedContainerMarksItAbsent(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(2)
	svc := newService(t, fake, db)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, domain.TriggerStartup); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	fake.InspectErrs["container-000"] = docker.ErrContainerVanished

	if err := svc.RefreshContainer(ctx, "container-000"); err != nil {
		t.Fatalf("a vanished container must not be an error: %v", err)
	}

	detail, err := db.Containers.Get(ctx, "container-000")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if detail.Overview.Present {
		t.Error("a vanished container must be marked absent")
	}
}

// The row is retained rather than deleted, so its observed lifetime and its
// warnings survive the container being removed.
func TestMarkContainerAbsentRetainsTheRow(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(2)
	svc := newService(t, fake, db)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, domain.TriggerStartup); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	if err := svc.MarkContainerAbsent(ctx, "container-000"); err != nil {
		t.Fatalf("MarkContainerAbsent: %v", err)
	}

	detail, err := db.Containers.Get(ctx, "container-000")
	if err != nil {
		t.Fatalf("the row must be retained: %v", err)
	}
	if detail.Overview.Present {
		t.Error("the container must be marked absent")
	}
}

// A `docker run --rm` container can live and die entirely between two sweeps,
// so its destroy event is the first and only thing HarborMaster ever sees.
func TestMarkAbsentForAnUninventoriedContainerIsNotAnError(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)
	svc := newService(t, fake, db)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, domain.TriggerStartup); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	if err := svc.MarkContainerAbsent(ctx, "never-inventoried"); err != nil {
		t.Fatalf("a destroy for an unknown container must not error: %v", err)
	}
}

// Writing one container into an empty inventory would produce a host that
// appears to have exactly one container. The caller must escalate instead.
func TestTargetedRefreshWithoutAnInventoryIsRefused(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)
	svc := newService(t, fake, db)

	err := svc.RefreshContainer(context.Background(), "container-000")
	if !errors.Is(err, service.ErrTargetedUnavailable) {
		t.Fatalf("err = %v, want ErrTargetedUnavailable", err)
	}
}

func TestTargetedRefreshIsRefusedWhenInventoryIsDisabled(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)
	svc := newService(t, fake, db, func(cfg *config.Inventory) {
		cfg.Enabled = false
	})

	for name, call := range map[string]func() error{
		"container": func() error { return svc.RefreshContainer(context.Background(), "x") },
		"absent":    func() error { return svc.MarkContainerAbsent(context.Background(), "x") },
		"image":     func() error { return svc.RefreshImage(context.Background(), "x") },
		"networks":  func() error { return svc.RefreshNetworks(context.Background()) },
		"volumes":   func() error { return svc.RefreshVolumes(context.Background()) },
	} {
		if err := call(); !errors.Is(err, service.ErrInventoryDisabled) {
			t.Errorf("%s = %v, want ErrInventoryDisabled", name, err)
		}
	}
}

func TestTargetedImageRefresh(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)
	svc := newService(t, fake, db)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, domain.TriggerStartup); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	fake.Images["sha256:fresh"] = &domain.Image{
		ID: "sha256:fresh", ShortID: "fresh", RepoTags: []string{"alpine:3.20"},
	}

	if err := svc.RefreshImage(ctx, "sha256:fresh"); err != nil {
		t.Fatalf("RefreshImage: %v", err)
	}

	usage, err := db.Images.Get(ctx, "sha256:fresh")
	if err != nil {
		t.Fatalf("the image must be persisted: %v", err)
	}
	if len(usage.Image.RepoTags) == 0 || usage.Image.RepoTags[0] != "alpine:3.20" {
		t.Errorf("repoTags = %v, want alpine:3.20", usage.Image.RepoTags)
	}
}

// `docker rmi` on an image a container still references leaves exactly this
// state, and it is reported by the very events this reacts to.
func TestTargetedImageRefreshToleratesAMissingImage(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)
	svc := newService(t, fake, db)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, domain.TriggerStartup); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	fake.ImageErrs["sha256:gone"] = docker.ErrImageUnavailable

	if err := svc.RefreshImage(ctx, "sha256:gone"); err != nil {
		t.Fatalf("an unavailable image must not be an error: %v", err)
	}
}

func TestTargetedNetworkAndVolumeRefresh(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)
	svc := newService(t, fake, db)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, domain.TriggerStartup); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	fake.Networks = []domain.Network{
		{ID: "net1", Name: "bridge"},
		{ID: "net2", Name: "shop_default"},
	}
	fake.Volumes = []domain.Volume{{Name: "data"}, {Name: "cache"}}

	if err := svc.RefreshNetworks(ctx); err != nil {
		t.Fatalf("RefreshNetworks: %v", err)
	}
	if err := svc.RefreshVolumes(ctx); err != nil {
		t.Fatalf("RefreshVolumes: %v", err)
	}

	networks, total, err := db.Networks.List(ctx, store.Page{Limit: 10})
	if err != nil {
		t.Fatalf("network List: %v", err)
	}
	if total != 2 {
		t.Errorf("networks = %d, want 2 (%v)", total, networks)
	}

	_, volumeTotal, err := db.Volumes.List(ctx, store.Page{Limit: 10})
	if err != nil {
		t.Fatalf("volume List: %v", err)
	}
	if volumeTotal != 2 {
		t.Errorf("volumes = %d, want 2", volumeTotal)
	}
}

// A removed network must not linger: a stale row would show an attachment
// target that cannot be attached to.
func TestNetworkRefreshRemovesVanishedNetworks(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)
	fake.Networks = []domain.Network{{ID: "net1", Name: "bridge"}, {ID: "net2", Name: "temp"}}
	svc := newService(t, fake, db)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, domain.TriggerStartup); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	fake.Networks = []domain.Network{{ID: "net1", Name: "bridge"}}
	if err := svc.RefreshNetworks(ctx); err != nil {
		t.Fatalf("RefreshNetworks: %v", err)
	}

	_, total, err := db.Networks.List(ctx, store.Page{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Errorf("networks = %d, want 1 after one was removed", total)
	}
}

// An empty list is indistinguishable at this layer from a read that returned
// nothing, so the safe reading is "do nothing" rather than "wipe the table".
func TestNetworkRefreshWithAnEmptyListDoesNotWipe(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)
	svc := newService(t, fake, db)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, domain.TriggerStartup); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	fake.Networks = nil
	if err := svc.RefreshNetworks(ctx); err != nil {
		t.Fatalf("RefreshNetworks: %v", err)
	}

	_, total, err := db.Networks.List(ctx, store.Page{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total == 0 {
		t.Error("an empty read must not empty the catalog")
	}
}

// Targeted writes are serialised against each other, so concurrent events for
// the same container cannot interleave their writes. Run with -race, this is
// what proves the mutex is doing its job.
func TestConcurrentTargetedRefreshesAreSerialised(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(4)
	svc := newService(t, fake, db)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, domain.TriggerStartup); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	done := make(chan error, 8)
	for range 8 {
		go func() {
			done <- svc.RefreshContainer(ctx, "container-000")
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Errorf("concurrent RefreshContainer: %v", err)
		}
	}

	detail, err := db.Containers.Get(ctx, "container-000")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !detail.Overview.Present {
		t.Error("concurrent targeted writes left the container in a wrong state")
	}
}

// Reconcile reuses the Phase 2 pipeline, so a reconciliation and a manual
// refresh must be indistinguishable in what they produce.
func TestReconcileUsesTheFullRefreshPipeline(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(3)
	svc := newService(t, fake, db)
	ctx := context.Background()

	if err := svc.Reconcile(ctx, domain.TriggerReconcile); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	last, err := db.Inventory.LastRefresh(ctx, true)
	if err != nil {
		t.Fatalf("LastRefresh: %v", err)
	}
	if last == nil {
		t.Fatal("a reconciliation must record a refresh")
	}
	if last.Trigger != domain.TriggerReconcile {
		t.Errorf("trigger = %q, want reconcile", last.Trigger)
	}
	if last.Checksum == "" {
		t.Error("a reconciliation must produce a checksum like any full refresh")
	}
	if last.ContainersListed != 3 {
		t.Errorf("containersListed = %d, want 3", last.ContainersListed)
	}
}

// A sweep already under way satisfies the request, so it is not an error to
// find one running.
func TestReconcileReportsAnInFlightRefresh(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)
	svc := newService(t, fake, db)

	if started, _ := svc.TryBeginRefresh(domain.TriggerManual); !started {
		t.Fatal("could not reserve the refresh slot")
	}

	err := svc.Reconcile(context.Background(), domain.TriggerReconcile)
	if !errors.Is(err, service.ErrRefreshInProgress) {
		t.Fatalf("err = %v, want ErrRefreshInProgress", err)
	}
}

// The suppressed ticker is how exactly one component owns the periodic sweep.
func TestSuppressedPeriodicRefreshDoesNotTick(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)

	svc := service.NewInventoryService(service.InventoryOptions{
		Runtime:    fake,
		Inventory:  db.Inventory,
		Containers: db.Containers,
		Logger:     discardLogger(),
		Config: config.Inventory{
			Enabled:          true,
			Workers:          2,
			RefreshOnStartup: false,
			// Short enough that an unsuppressed ticker would fire many times.
			RefreshInterval: 5 * time.Millisecond,
		},
		SuppressPeriodic: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	svc.Run(ctx)

	if fake.ListCalls != 0 {
		t.Errorf("the suppressed ticker ran %d refreshes, want 0", fake.ListCalls)
	}
}

func TestUnsuppressedPeriodicRefreshStillTicks(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)

	svc := service.NewInventoryService(service.InventoryOptions{
		Runtime:    fake,
		Inventory:  db.Inventory,
		Containers: db.Containers,
		Logger:     discardLogger(),
		Config: config.Inventory{
			Enabled:          true,
			Workers:          2,
			RefreshOnStartup: false,
			RefreshInterval:  5 * time.Millisecond,
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	svc.Run(ctx)

	if fake.ListCalls == 0 {
		t.Error("Phase 2 behaviour must be unchanged when the engine is not running")
	}
}
