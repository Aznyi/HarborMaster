package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Capture errors.
var (
	// ErrSnapshotsDisabled reports that capture is switched off.
	ErrSnapshotsDisabled = errors.New("snapshots are disabled")
	// ErrCaptureInProgress reports a concurrent capture of the same container.
	// Refused rather than queued, for the same reason inventory refresh is.
	ErrCaptureInProgress = errors.New("a snapshot capture is already in progress for this container")
)

// SnapshotContainers is the container data capture reads.
//
// Capture goes through the repository, never through docker.Runtime. Two
// reasons: a snapshot must record exactly what the inventory observed, so the
// snapshot and the UI never disagree; and an HTTP-triggered capture must not be
// able to generate traffic against a privileged socket, which would make the
// endpoint a denial-of-service amplifier.
type SnapshotContainers interface {
	// GetPresent, not Get: this service reasons about a container's CURRENT
	// configuration, and the inventory keeps a row after the container leaves.
	// Get would hand back that tombstone and the evaluation would describe a
	// container that is not there. See ContainerRepository.Get.
	GetPresent(ctx context.Context, id string) (*domain.ContainerDetail, error)
	ResolveID(ctx context.Context, reference string) (string, error)
}

// SnapshotWriter persists captured snapshots.
type SnapshotWriter interface {
	Create(
		ctx context.Context,
		snapshot domain.Snapshot,
		env []domain.SnapshotEnvEntry,
		mounts []domain.SnapshotMountRow,
		networks []domain.SnapshotNetworkRow,
	) (domain.Snapshot, error)
}

// SnapshotGeneration reports the inventory state at capture time.
type SnapshotGeneration interface {
	CurrentGeneration(ctx context.Context) (int64, string, error)
}

// HostVersions describes the host a snapshot was taken from.
type HostVersions struct {
	HarborMaster string
	DockerAPI    string
	DockerEngine string
}

// CaptureRequest asks for one snapshot.
type CaptureRequest struct {
	// ContainerID may be a full ID, a short ID, or a name; it is resolved
	// through the repository before anything else happens.
	ContainerID string
	Trigger     domain.SnapshotTrigger
	// Reason is operator-supplied prose, already length- and UTF-8 validated by
	// the API layer.
	Reason string
}

// SnapshotService captures immutable configuration snapshots.
//
// HarborMaster is read-only with respect to Docker, and nothing in this service
// changes that: capture reads HarborMaster's own inventory and writes to
// HarborMaster's own database.
type SnapshotService struct {
	containers SnapshotContainers
	snapshots  SnapshotWriter
	inventory  SnapshotGeneration
	hasher     *Hasher
	versions   HostVersions
	cfg        config.Snapshots
	logger     *slog.Logger
	// readiness evaluates the restore readiness of a freshly captured
	// snapshot. Optional: nil leaves the verdict unevaluated, which is what
	// every snapshot used to be.
	readiness *ReadinessRecorder

	// mu guards capturing only. It is never held across a database write, so a
	// running capture cannot block an unrelated one.
	mu        sync.Mutex
	capturing map[string]time.Time

	now func() time.Time
}

// SnapshotOptions configures a SnapshotService.
type SnapshotOptions struct {
	Containers SnapshotContainers
	Snapshots  SnapshotWriter
	Inventory  SnapshotGeneration
	Hasher     *Hasher
	Versions   HostVersions
	Config     config.Snapshots
	Logger     *slog.Logger
	Now        func() time.Time

	// Readiness evaluates a captured snapshot's restore readiness. Optional;
	// nil leaves every verdict at `unknown`, which is what the planner then
	// charges risk points for.
	Readiness *ReadinessRecorder
}

// NewSnapshotService builds a SnapshotService.
func NewSnapshotService(opts SnapshotOptions) *SnapshotService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	return &SnapshotService{
		containers: opts.Containers,
		snapshots:  opts.Snapshots,
		inventory:  opts.Inventory,
		hasher:     opts.Hasher,
		versions:   opts.Versions,
		cfg:        opts.Config,
		logger:     logger,
		readiness:  opts.Readiness,
		capturing:  make(map[string]time.Time),
		now:        now,
	}
}

// Enabled reports whether capture is switched on.
func (s *SnapshotService) Enabled() bool { return s.cfg.Enabled }

// Capture records one container's configuration.
//
// Re-capturing an unchanged configuration returns the existing snapshot with
// Deduplicated set rather than inserting a duplicate row.
func (s *SnapshotService) Capture(ctx context.Context, req CaptureRequest) (domain.Snapshot, error) {
	if !s.cfg.Enabled {
		return domain.Snapshot{}, ErrSnapshotsDisabled
	}
	if s.containers == nil || s.snapshots == nil || s.hasher == nil {
		return domain.Snapshot{}, ErrSnapshotsDisabled
	}

	containerID, err := s.containers.ResolveID(ctx, req.ContainerID)
	if err != nil {
		return domain.Snapshot{}, err
	}

	if !s.beginCapture(containerID) {
		return domain.Snapshot{}, ErrCaptureInProgress
	}
	defer s.endCapture(containerID)

	detail, err := s.containers.GetPresent(ctx, containerID)
	if err != nil {
		return domain.Snapshot{}, err
	}
	if detail == nil {
		return domain.Snapshot{}, store.ErrNotFound
	}

	trigger := req.Trigger
	if trigger == "" {
		trigger = domain.SnapshotTriggerAPI
	}

	// The one place raw secrets are read. BuildSpec hashes them and discards
	// them; nothing downstream of this call holds a plaintext value.
	spec := BuildSpec(*detail, detail.Image, s.hasher)

	blob, checksum, err := MarshalSpec(spec)
	if err != nil {
		return domain.Snapshot{}, fmt.Errorf("build snapshot document: %w", err)
	}

	snapshot := domain.Snapshot{
		HostID:              detail.Overview.HostID,
		ContainerID:         containerID,
		ContainerName:       detail.Overview.Name,
		ImageReference:      detail.Overview.Image.Raw,
		ImageDigest:         detail.Overview.Image.Digest,
		ImageID:             detail.Overview.ImageID,
		SpecVersion:         spec.SpecVersion,
		SpecJSON:            blob,
		Checksum:            checksum,
		HarborMasterVersion: s.versions.HarborMaster,
		DockerAPIVersion:    s.versions.DockerAPI,
		DockerEngineVersion: s.versions.DockerEngine,
		Trigger:             trigger,
		Reason:              req.Reason,
		Warnings:            detail.Warnings,
		ReadinessStatus:     domain.ReadinessUnknown,
		DigestKeyID:         s.hasher.KeyID(),
		CreatedAt:           s.now(),
	}
	if snapshot.HostID == "" {
		snapshot.HostID = domain.LocalHostID
	}
	if snapshot.Warnings == nil {
		snapshot.Warnings = []domain.InventoryWarning{}
	}

	if s.inventory != nil {
		if generation, _, genErr := s.inventory.CurrentGeneration(ctx); genErr == nil {
			snapshot.InventoryGeneration = generation
		}
	}

	stored, err := s.snapshots.Create(ctx, snapshot,
		environmentRows(spec), mountRows(spec), networkRows(spec))
	if err != nil {
		return domain.Snapshot{}, err
	}

	// Logged WITHOUT the reason: it is operator-supplied text, and a log record
	// is not the place to echo user input back.
	s.logger.InfoContext(ctx, "configuration snapshot captured",
		slog.Int64("snapshotId", stored.ID),
		slog.String("containerId", domain.ShortenID(containerID)),
		slog.String("trigger", string(trigger)),
		slog.Bool("deduplicated", stored.Deduplicated))

	// Evaluate the restore readiness, off the request's critical path.
	//
	// A deduplicated capture returns an EXISTING snapshot whose verdict is
	// already current, so re-evaluating it would be work for no answer.
	if !stored.Deduplicated {
		s.readiness.RecordDetached(ctx, stored.ID)
	}

	return stored, nil
}

// beginCapture reserves the capture slot for one container.
func (s *SnapshotService) beginCapture(containerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, running := s.capturing[containerID]; running {
		return false
	}
	s.capturing[containerID] = s.now()
	return true
}

func (s *SnapshotService) endCapture(containerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.capturing, containerID)
}

// CaptureStartedAt reports when an in-flight capture began, for a 409 body.
func (s *SnapshotService) CaptureStartedAt(containerID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	startedAt, running := s.capturing[containerID]
	return startedAt, running
}

// environmentRows derives the environment child rows from the document.
//
// Log-driver options are included alongside environment variables: a Splunk
// token or a Loki basic-auth URL is a credential by any other name, and
// readiness must count it when reporting what a restore would need.
func environmentRows(spec domain.SnapshotSpec) []domain.SnapshotEnvEntry {
	rows := make([]domain.SnapshotEnvEntry, 0, len(spec.Environment)+len(spec.Logging.Options))

	position := 0
	for _, entry := range spec.Environment {
		rows = append(rows, envRow(position, entry.Name, entry))
		position++
	}
	for _, option := range spec.Logging.Options {
		rows = append(rows, envRow(position, "logging."+option.Name, option))
		position++
	}
	return rows
}

func envRow(position int, key string, entry domain.SpecEnvVar) domain.SnapshotEnvEntry {
	row := domain.SnapshotEnvEntry{
		Position:        position,
		Key:             key,
		Classification:  entry.Sensitivity,
		Present:         entry.Present,
		Length:          entry.Length,
		Digest:          entry.Digest,
		DigestAlgorithm: entry.DigestAlgorithm,
		DigestKeyID:     entry.DigestKeyID,
	}
	// A sensitive entry never carries a value out of here. The repository
	// blanks it again, and the schema rejects it: three independent layers,
	// because this is the one mistake that cannot be undone.
	if !entry.Sensitive() {
		row.Value = entry.Value
		row.Length = len(entry.Value)
	}
	return row
}

func mountRows(spec domain.SnapshotSpec) []domain.SnapshotMountRow {
	rows := make([]domain.SnapshotMountRow, 0, len(spec.Mounts))
	for _, mount := range spec.Mounts {
		rows = append(rows, domain.SnapshotMountRow{
			Destination: mount.Destination,
			Type:        mount.Type,
			Source:      mount.Source,
			ReadOnly:    mount.ReadOnly,
			VolumeName:  mount.VolumeName,
			Driver:      mount.Driver,
		})
	}
	return rows
}

func networkRows(spec domain.SnapshotSpec) []domain.SnapshotNetworkRow {
	rows := make([]domain.SnapshotNetworkRow, 0, len(spec.Networks))
	for _, network := range spec.Networks {
		rows = append(rows, domain.SnapshotNetworkRow{
			NetworkName: network.NetworkName,
			Aliases:     network.Aliases,
		})
	}
	return rows
}
