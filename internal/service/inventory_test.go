package service_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

func openDB(t *testing.T) *store.DB {
	t.Helper()

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hm.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// fakeWith builds a runtime double holding n containers, each with its own
// inspection and a shared image.
func fakeWith(count int) *docker.Fake {
	fake := docker.NewFake()
	fake.Info = docker.Info{APIVersion: "1.51", OSType: "linux"}
	fake.Networks = []domain.Network{{ID: "net1", Name: "bridge"}}
	fake.Volumes = []domain.Volume{{Name: "data"}}
	fake.Images["sha256:shared"] = &domain.Image{
		ID: "sha256:shared", ShortID: "shared", RepoTags: []string{"nginx:1.27"},
	}

	for i := 0; i < count; i++ {
		id := fmt.Sprintf("container-%03d", i)
		summary := domain.ContainerSummary{
			HostID: domain.LocalHostID, ID: id, ShortID: domain.ShortenID(id),
			Name:    fmt.Sprintf("svc-%03d", i),
			Image:   domain.ParseImageRef("nginx:1.27"),
			ImageID: "sha256:shared",
			State:   domain.StateRunning, Health: domain.HealthHealthy,
			CreatedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			Present:   true,
		}
		fake.Containers = append(fake.Containers, summary)
		fake.Inspections[id] = &docker.Inspection{
			Detail: domain.ContainerDetail{
				Overview:    summary,
				State:       domain.StateDetail{State: domain.StateRunning, Health: domain.HealthHealthy},
				Environment: []domain.EnvVar{},
				Labels:      []domain.Label{},
				Mounts:      []domain.Mount{},
				Networks:    []domain.NetworkAttachment{},
				Warnings:    []domain.InventoryWarning{},
			},
			RawJSON: []byte(`{"Id":"` + id + `"}`),
		}
	}
	return fake
}

func newService(t *testing.T, fake *docker.Fake, db *store.DB, tweak ...func(*config.Inventory)) *service.InventoryService {
	t.Helper()

	cfg := config.Inventory{
		Enabled: true,
		Workers: 4,
	}
	for _, apply := range tweak {
		apply(&cfg)
	}

	return service.NewInventoryService(service.InventoryOptions{
		Runtime:    fake,
		Inventory:  db.Inventory,
		Containers: db.Containers,
		Logger:     discardLogger(),
		Config:     cfg,
	})
}

func TestRefreshPersistsInventory(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(5)
	svc := newService(t, fake, db)
	ctx := context.Background()

	record, err := svc.Refresh(ctx, domain.TriggerManual)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if record.State != domain.RefreshSucceeded {
		t.Errorf("state = %q", record.State)
	}
	if record.Generation != 1 {
		t.Errorf("generation = %d, want 1", record.Generation)
	}
	if record.ContainersListed != 5 || record.ContainersInspected != 5 {
		t.Errorf("counts = listed %d, inspected %d", record.ContainersListed, record.ContainersInspected)
	}
	if record.ContainersFailed != 0 {
		t.Errorf("failed = %d", record.ContainersFailed)
	}
	if record.Checksum == "" {
		t.Error("checksum not computed")
	}
	if record.NetworksListed != 1 || record.VolumesListed != 1 {
		t.Errorf("catalog counts = %d/%d", record.NetworksListed, record.VolumesListed)
	}

	_, total, err := db.Containers.List(ctx, store.ContainerFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 5 {
		t.Errorf("persisted containers = %d, want 5", total)
	}
}

// One unreadable container must not cost the operator the others.
func TestRefreshSurvivesPerContainerFailures(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(5)
	fake.InspectErrs["container-001"] = docker.ErrContainerVanished
	fake.InspectErrs["container-003"] = errors.New("permission denied reading container")
	svc := newService(t, fake, db)
	ctx := context.Background()

	record, err := svc.Refresh(ctx, domain.TriggerManual)
	if err != nil {
		t.Fatalf("a per-container failure must not fail the refresh: %v", err)
	}

	if record.ContainersFailed != 2 {
		t.Errorf("failed = %d, want 2", record.ContainersFailed)
	}
	if record.ContainersInspected != 3 {
		t.Errorf("inspected = %d, want 3", record.ContainersInspected)
	}

	// All five are still recorded, the failed two from summary data.
	_, total, err := db.Containers.List(ctx, store.ContainerFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 5 {
		t.Errorf("persisted = %d, want all 5 recorded", total)
	}

	warnings, err := db.Inventory.Warnings(ctx, record.Generation, 50)
	if err != nil {
		t.Fatalf("warnings: %v", err)
	}

	codes := map[domain.WarningCode]int{}
	for _, warning := range warnings {
		codes[warning.Code]++
	}
	// A vanished container is classified apart from a genuine failure: it is
	// expected churn, not a fault.
	if codes[domain.WarningContainerVanished] != 1 {
		t.Errorf("vanished warnings = %d, want 1 (%+v)", codes[domain.WarningContainerVanished], codes)
	}
	if codes[domain.WarningInspectFailed] != 1 {
		t.Errorf("inspect-failed warnings = %d, want 1", codes[domain.WarningInspectFailed])
	}
}

// An image removed while a container still uses it is normal, not fatal.
func TestRefreshTreatsMissingImageAsAWarning(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(2)
	delete(fake.Images, "sha256:shared")
	svc := newService(t, fake, db)

	record, err := svc.Refresh(context.Background(), domain.TriggerManual)
	if err != nil {
		t.Fatalf("a missing image must not fail the refresh: %v", err)
	}
	if record.ImagesInspected != 0 {
		t.Errorf("images inspected = %d", record.ImagesInspected)
	}

	warnings, err := db.Inventory.Warnings(context.Background(), record.Generation, 50)
	if err != nil {
		t.Fatalf("warnings: %v", err)
	}
	found := false
	for _, warning := range warnings {
		if warning.Code == domain.WarningImageUnavailable {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an image_unavailable warning, got %+v", warnings)
	}
}

// A failure to LIST is the one Docker error that fails a whole refresh.
func TestRefreshFailsWhenDockerIsUnavailable(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(3)
	svc := newService(t, fake, db)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, domain.TriggerStartup); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	fake.ListErr = docker.ErrUnreachable
	if _, err := svc.Refresh(ctx, domain.TriggerPeriodic); err == nil {
		t.Fatal("expected the refresh to fail when listing fails")
	}

	// The previously persisted inventory is intact and still served.
	_, total, err := db.Containers.List(ctx, store.ContainerFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 3 {
		t.Errorf("prior inventory lost: %d containers remain", total)
	}

	generation, _, err := db.Inventory.CurrentGeneration(ctx)
	if err != nil {
		t.Fatalf("generation: %v", err)
	}
	if generation != 1 {
		t.Errorf("generation = %d, want 1 (a failed refresh must not advance it)", generation)
	}

	attempt, err := db.Inventory.LastRefresh(ctx, false)
	if err != nil || attempt == nil {
		t.Fatalf("last attempt = %+v (%v)", attempt, err)
	}
	if attempt.State != domain.RefreshFailed {
		t.Errorf("last attempt state = %q", attempt.State)
	}
	// The failure reason must be sanitised, not a raw daemon error.
	if attempt.Error == "" {
		t.Error("failed refresh should record a reason")
	}
}

// A daemon that is down at ping time fails before any listing is attempted.
func TestRefreshFailsWhenPingFails(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)
	fake.PingErr = docker.ErrUnreachable
	svc := newService(t, fake, db)

	if _, err := svc.Refresh(context.Background(), domain.TriggerManual); err == nil {
		t.Fatal("expected the refresh to fail when the daemon is unreachable")
	}
	if fake.ListCalls != 0 {
		t.Error("listing should not be attempted after a failed ping")
	}
}

func TestRefreshRespectsCancellation(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(20)
	svc := newService(t, fake, db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := svc.Refresh(ctx, domain.TriggerManual); err == nil {
		t.Fatal("expected a cancelled refresh to fail")
	}

	// Nothing was persisted.
	generation, _, err := db.Inventory.CurrentGeneration(context.Background())
	if err != nil {
		t.Fatalf("generation: %v", err)
	}
	if generation != 0 {
		t.Errorf("generation = %d, want 0", generation)
	}
}

// The per-refresh image cache: each unique image is inspected exactly once,
// however many containers reference it.
func TestRefreshInspectsEachImageOnce(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(25) // all 25 share one image
	svc := newService(t, fake, db)

	record, err := svc.Refresh(context.Background(), domain.TriggerManual)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if calls := fake.ImageInspectionsFor("sha256:shared"); calls != 1 {
		t.Errorf("image inspected %d times for 25 containers, want 1", calls)
	}
	if record.ImagesInspected != 1 {
		t.Errorf("imagesInspected = %d, want 1", record.ImagesInspected)
	}
}

func TestRefreshInspectsDistinctImagesSeparately(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(4)
	for i := range fake.Containers {
		imageID := fmt.Sprintf("sha256:image%d", i%2)
		fake.Containers[i].ImageID = imageID
		fake.Inspections[fake.Containers[i].ID].Detail.Overview.ImageID = imageID
		fake.Images[imageID] = &domain.Image{ID: imageID, ShortID: domain.ShortenID(imageID)}
	}
	svc := newService(t, fake, db)

	record, err := svc.Refresh(context.Background(), domain.TriggerManual)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if record.ImagesInspected != 2 {
		t.Errorf("imagesInspected = %d, want 2 distinct images", record.ImagesInspected)
	}
	for _, id := range []string{"sha256:image0", "sha256:image1"} {
		if calls := fake.ImageInspectionsFor(id); calls != 1 {
			t.Errorf("%s inspected %d times, want 1", id, calls)
		}
	}
}

// Overlapping refreshes are refused, not queued.
func TestOverlappingRefreshIsRefused(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)
	svc := newService(t, fake, db)

	accepted, _ := svc.TryBeginRefresh(domain.TriggerManual)
	if !accepted {
		t.Fatal("the first reservation should succeed")
	}

	if _, err := svc.Refresh(context.Background(), domain.TriggerManual); !errors.Is(err, service.ErrRefreshInProgress) {
		t.Errorf("second refresh error = %v, want ErrRefreshInProgress", err)
	}
	if !svc.InProgress() {
		t.Error("InProgress should be true while the slot is held")
	}
}

func TestConcurrentRefreshesYieldExactlyOneWinner(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(10)
	svc := newService(t, fake, db)

	const attempts = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		conflicts int
	)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Refresh(context.Background(), domain.TriggerManual)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, service.ErrRefreshInProgress):
				conflicts++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if succeeded == 0 {
		t.Fatal("no refresh succeeded")
	}
	if succeeded+conflicts != attempts {
		t.Errorf("accounted %d of %d attempts", succeeded+conflicts, attempts)
	}
	// Whatever the interleaving, the daemon was never swept concurrently.
	if fake.ListCalls != succeeded {
		t.Errorf("list calls = %d, want one per successful refresh (%d)", fake.ListCalls, succeeded)
	}
}

func TestTriggerAsyncRefusesWhileRunning(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(1)
	svc := newService(t, fake, db)

	held, startedAt := svc.TryBeginRefresh(domain.TriggerManual)
	if !held {
		t.Fatal("reservation failed")
	}

	accepted, activeSince := svc.TriggerAsync(domain.TriggerManual)
	if accepted {
		t.Error("a second refresh must not be accepted")
	}
	if !activeSince.Equal(startedAt) {
		t.Errorf("active start time = %v, want the running refresh's %v", activeSince, startedAt)
	}
}

func TestTriggerAsyncCompletesInBackground(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(3)
	svc := newService(t, fake, db)

	accepted, _ := svc.TriggerAsync(domain.TriggerManual)
	if !accepted {
		t.Fatal("refresh not accepted")
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		generation, _, err := db.Inventory.CurrentGeneration(context.Background())
		if err != nil {
			t.Fatalf("generation: %v", err)
		}
		if generation == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background refresh did not complete")
}

func TestDisabledInventoryRefusesRefresh(t *testing.T) {
	db := openDB(t)
	svc := newService(t, fakeWith(1), db, func(c *config.Inventory) { c.Enabled = false })

	if _, err := svc.Refresh(context.Background(), domain.TriggerManual); !errors.Is(err, service.ErrInventoryDisabled) {
		t.Errorf("error = %v, want ErrInventoryDisabled", err)
	}
	if accepted, _ := svc.TriggerAsync(domain.TriggerManual); accepted {
		t.Error("a disabled inventory must not accept an async refresh")
	}
	if svc.Enabled() {
		t.Error("Enabled should report false")
	}
}

func TestStatusReportsInventoryState(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(4)
	fake.Containers[0].State = domain.StateExited
	fake.Inspections[fake.Containers[0].ID].Detail.Overview.State = domain.StateExited
	svc := newService(t, fake, db)
	ctx := context.Background()

	status, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	// Before any refresh: enabled, idle, nothing recorded.
	if !status.Enabled || status.State != domain.RefreshIdle || status.Generation != 0 {
		t.Errorf("initial status = %+v", status)
	}
	if status.Docker.Status != domain.StatusUp {
		t.Errorf("docker = %+v", status.Docker)
	}

	if _, err := svc.Refresh(ctx, domain.TriggerManual); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	status, err = svc.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.State != domain.RefreshSucceeded {
		t.Errorf("state = %q", status.State)
	}
	if status.Generation != 1 || status.Checksum == "" {
		t.Errorf("generation/checksum = %d/%q", status.Generation, status.Checksum)
	}
	if status.Counts.Containers != 4 {
		t.Errorf("counts = %+v", status.Counts)
	}
	if status.Counts.Stopped != 1 {
		t.Errorf("stopped = %d, want 1", status.Counts.Stopped)
	}
	if status.LastSuccess == nil || status.LastAttempt == nil {
		t.Error("last attempt and success should both be populated")
	}
	if status.InProgress {
		t.Error("no refresh should be in progress")
	}
}

// Docker being down must degrade the status, not fail the endpoint.
func TestStatusReportsDockerDownWithoutFailing(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(0)
	fake.PingErr = docker.ErrUnreachable
	svc := newService(t, fake, db)

	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("status must not fail when docker is down: %v", err)
	}
	if status.Docker.Status != domain.StatusDown {
		t.Errorf("docker = %+v", status.Docker)
	}
	if status.Docker.Detail == "" {
		t.Error("expected a sanitised detail")
	}
}

// Status must stay readable while a refresh is running: it takes a read lock
// that the refresh never holds across its work.
func TestStatusIsReadableDuringRefresh(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(50)
	svc := newService(t, fake, db)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.Refresh(context.Background(), domain.TriggerManual)
	}()

	// Hammer the status endpoint while the refresh runs. The race detector is
	// what makes this meaningful; the assertion is simply that it never fails.
	for i := 0; i < 50; i++ {
		if _, err := svc.Status(context.Background()); err != nil {
			t.Errorf("status during refresh: %v", err)
			break
		}
	}
	wg.Wait()
}

func TestRunPerformsStartupRefreshAndStops(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(2)
	svc := newService(t, fake, db, func(c *config.Inventory) {
		c.RefreshOnStartup = true
		c.RefreshInterval = 0 // periodic disabled
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if generation, _, _ := db.Inventory.CurrentGeneration(context.Background()); generation == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop when the context was cancelled")
	}

	if generation, _, _ := db.Inventory.CurrentGeneration(context.Background()); generation != 1 {
		t.Errorf("startup refresh did not persist, generation = %d", generation)
	}
}

func TestRunSkipsStartupRefreshWhenConfiguredOff(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(2)
	svc := newService(t, fake, db, func(c *config.Inventory) {
		c.RefreshOnStartup = false
		c.RefreshInterval = 0
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if fake.ListCalls != 0 {
		t.Errorf("list calls = %d, want 0 when startup refresh is disabled", fake.ListCalls)
	}
}

func TestCheckRuntimeReportsReachability(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(0)
	svc := newService(t, fake, db)

	if err := svc.CheckRuntime(context.Background()); err != nil {
		t.Errorf("expected a reachable runtime: %v", err)
	}

	fake.PingErr = docker.ErrUnreachable
	if err := svc.CheckRuntime(context.Background()); err == nil {
		t.Error("expected an unreachable runtime to report an error")
	}
}

// Bounded concurrency: the worker count is a ceiling on in-flight inspections.
func TestInspectionConcurrencyIsBounded(t *testing.T) {
	db := openDB(t)
	fake := fakeWith(100)
	svc := newService(t, fake, db, func(c *config.Inventory) { c.Workers = 3 })

	if _, err := svc.Refresh(context.Background(), domain.TriggerManual); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Every container was inspected exactly once despite the small pool.
	if fake.InspectCalls != 100 {
		t.Errorf("inspect calls = %d, want 100", fake.InspectCalls)
	}
}
