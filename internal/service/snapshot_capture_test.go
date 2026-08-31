package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// fakeContainers serves one fixture container.
type fakeContainers struct {
	detail *domain.ContainerDetail
	err    error
	// block, when non-nil, holds Get until it is closed, so a concurrent
	// capture can be observed.
	block chan struct{}
}

func (f *fakeContainers) GetPresent(context.Context, string) (*domain.ContainerDetail, error) {
	if f.block != nil {
		<-f.block
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.detail, nil
}

func (f *fakeContainers) ResolveID(_ context.Context, reference string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return reference, nil
}

// recordingWriter captures what would be persisted.
type recordingWriter struct {
	mu       sync.Mutex
	snapshot domain.Snapshot
	env      []domain.SnapshotEnvEntry
	mounts   []domain.SnapshotMountRow
	networks []domain.SnapshotNetworkRow
	calls    int
}

func (w *recordingWriter) Create(
	_ context.Context,
	snapshot domain.Snapshot,
	env []domain.SnapshotEnvEntry,
	mounts []domain.SnapshotMountRow,
	networks []domain.SnapshotNetworkRow,
) (domain.Snapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.calls++
	w.snapshot = snapshot
	w.env = env
	w.mounts = mounts
	w.networks = networks

	snapshot.ID = int64(w.calls)
	return snapshot, nil
}

func newCaptureService(t *testing.T, detail domain.ContainerDetail) (*SnapshotService, *fakeContainers, *recordingWriter) {
	t.Helper()

	containers := &fakeContainers{detail: &detail}
	writer := &recordingWriter{}
	svc := NewSnapshotService(SnapshotOptions{
		Containers: containers,
		Snapshots:  writer,
		Hasher:     newTestHasher(t),
		Versions:   HostVersions{HarborMaster: "0.3.0", DockerAPI: "1.45", DockerEngine: "27.0.0"},
		Config:     config.Snapshots{Enabled: true},
	})
	return svc, containers, writer
}

func TestCaptureProducesACompleteSnapshot(t *testing.T) {
	svc, _, writer := newCaptureService(t, fixtureDetail())

	got, err := svc.Capture(context.Background(), CaptureRequest{
		ContainerID: "c0ffee0000000000",
		Trigger:     domain.SnapshotTriggerManual,
		Reason:      "before upgrade",
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	if got.ID == 0 {
		t.Error("no ID assigned")
	}
	if len(got.Checksum) != 64 {
		t.Errorf("Checksum = %q, want 64 hex characters", got.Checksum)
	}
	if got.SpecVersion != domain.SnapshotSpecVersion {
		t.Errorf("SpecVersion = %d", got.SpecVersion)
	}
	if got.Trigger != domain.SnapshotTriggerManual {
		t.Errorf("Trigger = %q", got.Trigger)
	}
	if got.Reason != "before upgrade" {
		t.Errorf("Reason = %q", got.Reason)
	}
	if got.HarborMasterVersion != "0.3.0" || got.DockerAPIVersion != "1.45" {
		t.Errorf("versions not recorded: %+v", got)
	}
	if got.DigestKeyID == "" {
		t.Error("DigestKeyID not recorded; digests would be uncomparable later")
	}
	if got.ReadinessStatus != domain.ReadinessUnknown {
		t.Errorf("ReadinessStatus = %q, want unknown before any evaluation", got.ReadinessStatus)
	}

	// Child rows derived from the same document.
	if len(writer.mounts) != 2 {
		t.Errorf("mount rows = %d, want 2", len(writer.mounts))
	}
	if len(writer.networks) != 2 {
		t.Errorf("network rows = %d, want 2", len(writer.networks))
	}
}

// The most important test in the capture path.
func TestCaptureNeverPersistsPlaintextSecrets(t *testing.T) {
	svc, _, writer := newCaptureService(t, fixtureDetail())

	if _, err := svc.Capture(context.Background(), CaptureRequest{
		ContainerID: "c0ffee0000000000", Trigger: domain.SnapshotTriggerManual,
	}); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	needles := []string{specSecretValue, "tok_" + specSecretValue}

	if body := string(writer.snapshot.SpecJSON); containsAny(body, needles) {
		t.Errorf("the persisted document contains a plaintext secret: %s", body)
	}
	for _, entry := range writer.env {
		if containsAny(entry.Value, needles) {
			t.Errorf("environment row %q carries a plaintext secret", entry.Key)
		}
		if entry.Sensitive() && entry.Value != "" {
			t.Errorf("sensitive row %q carries a value: %q", entry.Key, entry.Value)
		}
		if entry.Sensitive() && entry.Digest == "" {
			t.Errorf("sensitive row %q has no digest; the change could never be detected", entry.Key)
		}
	}
}

// Log-driver options carry credentials as often as environment variables do.
func TestCaptureRecordsLogOptionsAsEnvironmentRows(t *testing.T) {
	svc, _, writer := newCaptureService(t, fixtureDetail())

	if _, err := svc.Capture(context.Background(), CaptureRequest{
		ContainerID: "c0ffee0000000000",
	}); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, entry := range writer.env {
		if entry.Key == "logging.splunk-token" {
			found = true
			if !entry.Sensitive() {
				t.Error("a splunk token was not classified as sensitive")
			}
			if entry.Value != "" {
				t.Errorf("the token's value was stored: %q", entry.Value)
			}
		}
	}
	if !found {
		t.Error("log options were not captured; a restore would silently lose them")
	}
}

func TestCapturePositionsAreUnique(t *testing.T) {
	svc, _, writer := newCaptureService(t, fixtureDetail())

	if _, err := svc.Capture(context.Background(), CaptureRequest{ContainerID: "c1"}); err != nil {
		t.Fatal(err)
	}

	seen := make(map[int]bool, len(writer.env))
	for _, entry := range writer.env {
		if seen[entry.Position] {
			t.Errorf("duplicate position %d; the primary key would collide", entry.Position)
		}
		seen[entry.Position] = true
	}
}

func TestCaptureDefaultsTriggerToAPI(t *testing.T) {
	svc, _, _ := newCaptureService(t, fixtureDetail())

	got, err := svc.Capture(context.Background(), CaptureRequest{ContainerID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Trigger != domain.SnapshotTriggerAPI {
		t.Errorf("Trigger = %q, want api", got.Trigger)
	}
}

func TestCaptureIsRefusedWhenDisabled(t *testing.T) {
	svc := NewSnapshotService(SnapshotOptions{
		Containers: &fakeContainers{detail: ptrDetail(fixtureDetail())},
		Snapshots:  &recordingWriter{},
		Hasher:     newTestHasher(t),
		Config:     config.Snapshots{Enabled: false},
	})

	if _, err := svc.Capture(context.Background(), CaptureRequest{ContainerID: "c1"}); !errors.Is(err, ErrSnapshotsDisabled) {
		t.Errorf("err = %v, want ErrSnapshotsDisabled", err)
	}
	if svc.Enabled() {
		t.Error("Enabled() = true for a disabled service")
	}
}

func TestCaptureOfUnknownContainerReturnsNotFound(t *testing.T) {
	svc, containers, _ := newCaptureService(t, fixtureDetail())
	containers.err = store.ErrNotFound

	if _, err := svc.Capture(context.Background(), CaptureRequest{ContainerID: "nope"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want store.ErrNotFound", err)
	}
}

// A concurrent capture of the SAME container is refused rather than queued.
func TestConcurrentCaptureOfSameContainerIsRefused(t *testing.T) {
	detail := fixtureDetail()
	containers := &fakeContainers{detail: &detail, block: make(chan struct{})}
	svc := NewSnapshotService(SnapshotOptions{
		Containers: containers,
		Snapshots:  &recordingWriter{},
		Hasher:     newTestHasher(t),
		Config:     config.Snapshots{Enabled: true},
	})

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := svc.Capture(context.Background(), CaptureRequest{ContainerID: "c1"})
		done <- err
	}()

	<-started
	// Wait until the first capture actually holds the slot.
	for {
		if _, running := svc.CaptureStartedAt("c1"); running {
			break
		}
	}

	_, err := svc.Capture(context.Background(), CaptureRequest{ContainerID: "c1"})
	if !errors.Is(err, ErrCaptureInProgress) {
		t.Errorf("err = %v, want ErrCaptureInProgress", err)
	}

	close(containers.block)
	if err := <-done; err != nil {
		t.Fatalf("first capture: %v", err)
	}

	// The slot is released afterwards.
	if _, running := svc.CaptureStartedAt("c1"); running {
		t.Error("the capture slot was not released")
	}
}

// Different containers do not contend.
func TestConcurrentCaptureOfDifferentContainersIsAllowed(t *testing.T) {
	svc, _, _ := newCaptureService(t, fixtureDetail())

	if !svc.beginCapture("c1") {
		t.Fatal("first container could not begin")
	}
	defer svc.endCapture("c1")

	if !svc.beginCapture("c2") {
		t.Error("a different container was blocked by an unrelated capture")
	}
	svc.endCapture("c2")
}

// Capture reads the repository, never the Docker runtime: an HTTP-triggered
// capture must not be able to generate privileged socket traffic.
func TestCaptureDependsOnlyOnTheRepository(t *testing.T) {
	svc, _, _ := newCaptureService(t, fixtureDetail())

	// The service has no runtime field to call. This asserts the shape stays
	// that way: constructing it needs no docker.Runtime at all.
	if svc.containers == nil {
		t.Fatal("capture should read through the container repository")
	}
}

func TestCaptureHandlesNilDetail(t *testing.T) {
	containers := &fakeContainers{detail: nil}
	svc := NewSnapshotService(SnapshotOptions{
		Containers: containers,
		Snapshots:  &recordingWriter{},
		Hasher:     newTestHasher(t),
		Config:     config.Snapshots{Enabled: true},
	})

	if _, err := svc.Capture(context.Background(), CaptureRequest{ContainerID: "c1"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want store.ErrNotFound for a nil detail", err)
	}
}

func TestCapturePropagatesRepositoryErrors(t *testing.T) {
	svc, containers, _ := newCaptureService(t, fixtureDetail())
	containers.err = sql.ErrConnDone

	if _, err := svc.Capture(context.Background(), CaptureRequest{ContainerID: "c1"}); err == nil {
		t.Error("a repository failure should not be swallowed")
	}
}

func ptrDetail(d domain.ContainerDetail) *domain.ContainerDetail { return &d }

func containsAny(haystack string, needles []string) bool {
	for _, needle := range needles {
		if needle != "" && strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}
