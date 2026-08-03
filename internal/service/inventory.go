package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// ErrRefreshInProgress reports that a refresh was requested while one was
// already running.
//
// HarborMaster refuses the second request rather than queuing it. Queuing
// would mean an operator who clicks Refresh three times gets three sequential
// passes over a privileged socket, and the answer to "is it running" would
// stop being a simple yes or no.
var ErrRefreshInProgress = errors.New("inventory refresh already in progress")

// ErrInventoryDisabled reports that the inventory engine is switched off.
var ErrInventoryDisabled = errors.New("inventory is disabled")

// InventoryService owns the inventory refresh lifecycle.
//
// It depends on docker.Runtime, not on the Docker SDK, so a second runtime
// adapter could be substituted without this file changing.
type InventoryService struct {
	runtime    docker.Runtime
	inventory  *store.InventoryRepository
	containers *store.ContainerRepository
	logger     *slog.Logger
	cfg        config.Inventory

	// mu guards the refresh-in-progress state only. It is never held across a
	// Docker call or a database write, so a running refresh cannot block a
	// status read or an API request.
	mu       sync.RWMutex
	running  bool
	runState refreshState

	// now is injectable for deterministic tests.
	now func() time.Time
}

// refreshState is the in-memory view of the current or most recent attempt.
type refreshState struct {
	trigger   domain.RefreshTrigger
	startedAt time.Time
}

// InventoryOptions configures an InventoryService.
type InventoryOptions struct {
	Runtime    docker.Runtime
	Inventory  *store.InventoryRepository
	Containers *store.ContainerRepository
	Logger     *slog.Logger
	Config     config.Inventory
	Now        func() time.Time
}

// NewInventoryService builds an InventoryService.
func NewInventoryService(opts InventoryOptions) *InventoryService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Config.Workers < 1 {
		opts.Config.Workers = config.DefaultInventoryWorkers
	}

	return &InventoryService{
		runtime:    opts.Runtime,
		inventory:  opts.Inventory,
		containers: opts.Containers,
		logger:     logger,
		cfg:        opts.Config,
		now:        now,
	}
}

// Enabled reports whether the inventory engine is switched on.
func (s *InventoryService) Enabled() bool { return s.cfg.Enabled }

// InProgress reports whether a refresh is running right now.
func (s *InventoryService) InProgress() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

// Run drives startup and periodic refreshes until ctx is cancelled.
//
// Runs in its own goroutine, off the HTTP request path entirely: a refresh
// over a thousand containers must never be something an HTTP handler waits on.
func (s *InventoryService) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("inventory engine disabled by configuration")
		return
	}

	if s.cfg.RefreshOnStartup {
		if _, err := s.Refresh(ctx, domain.TriggerStartup); err != nil {
			// A failed startup refresh is not fatal. HarborMaster serves the
			// previously persisted inventory, or an empty one, and retries on
			// the next tick.
			s.logger.Warn("startup inventory refresh failed",
				slog.String("error", docker.SanitizeError(err)))
		}
	}

	if s.cfg.RefreshInterval <= 0 {
		s.logger.Info("periodic inventory refresh disabled; use the refresh endpoint")
		<-ctx.Done()
		return
	}

	ticker := time.NewTicker(s.cfg.RefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.Refresh(ctx, domain.TriggerPeriodic); err != nil {
				if errors.Is(err, ErrRefreshInProgress) {
					// The previous tick is still working. Skipping is correct:
					// the next tick will pick it up.
					s.logger.Debug("skipped periodic refresh; one is already running")
					continue
				}
				s.logger.Warn("periodic inventory refresh failed",
					slog.String("error", docker.SanitizeError(err)))
			}
		}
	}
}

// TryBeginRefresh reserves the refresh slot.
//
// Returns false when a refresh is already running, together with when it
// started, so a caller can report the active refresh instead of a bare
// conflict.
func (s *InventoryService) TryBeginRefresh(trigger domain.RefreshTrigger) (bool, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return false, s.runState.startedAt
	}
	s.running = true
	s.runState = refreshState{trigger: trigger, startedAt: s.now()}
	return true, s.runState.startedAt
}

func (s *InventoryService) endRefresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
}

// Refresh performs one full inventory refresh.
//
// Returns ErrRefreshInProgress if one is already running. The refresh either
// completes and is persisted atomically, or fails and leaves the previously
// persisted inventory untouched.
func (s *InventoryService) Refresh(ctx context.Context, trigger domain.RefreshTrigger) (domain.RefreshRecord, error) {
	if !s.cfg.Enabled {
		return domain.RefreshRecord{}, ErrInventoryDisabled
	}

	started, startedAt := s.TryBeginRefresh(trigger)
	if !started {
		return domain.RefreshRecord{}, ErrRefreshInProgress
	}
	defer s.endRefresh()

	return s.execute(ctx, trigger, startedAt)
}

// maxAsyncRefresh bounds a background refresh. Generous, because a host with a
// thousand containers is the design target, but finite so a wedged daemon
// cannot leave the refresh slot reserved forever.
const maxAsyncRefresh = 15 * time.Minute

// TriggerAsync starts a refresh in the background and returns immediately.
//
// This is what the manual refresh endpoint uses. The HTTP request must not
// wait on a sweep of a thousand containers: it would exceed the server's write
// timeout and hold a connection open for minutes.
//
// Returns false when a refresh is already running, along with when that one
// started.
func (s *InventoryService) TriggerAsync(trigger domain.RefreshTrigger) (bool, time.Time) {
	if !s.cfg.Enabled {
		return false, time.Time{}
	}

	started, startedAt := s.TryBeginRefresh(trigger)
	if !started {
		return false, startedAt
	}

	go func() {
		defer s.endRefresh()

		// Detached from any request: the refresh outlives the HTTP call that
		// asked for it, and is bounded by its own timeout instead.
		ctx, cancel := context.WithTimeout(context.Background(), maxAsyncRefresh)
		defer cancel()

		if _, err := s.execute(ctx, trigger, startedAt); err != nil {
			s.logger.Warn("background inventory refresh failed",
				slog.String("trigger", string(trigger)),
				slog.String("error", docker.SanitizeError(err)))
		}
	}()
	return true, startedAt
}

// CheckRuntime reports whether the container runtime is reachable.
//
// Used by the manual refresh endpoint so an operator gets an immediate 503
// rather than an accepted refresh that fails moments later in the background.
func (s *InventoryService) CheckRuntime(ctx context.Context) error {
	_, err := s.runtime.Ping(ctx)
	return err
}

// execute runs a refresh. The caller must already hold the refresh slot.
func (s *InventoryService) execute(ctx context.Context, trigger domain.RefreshTrigger, startedAt time.Time) (domain.RefreshRecord, error) {
	record := domain.RefreshRecord{
		Trigger:   trigger,
		State:     domain.RefreshRunning,
		StartedAt: startedAt,
	}

	result, err := s.collect(ctx, &record)
	if err != nil {
		record.DurationMS = s.now().Sub(startedAt).Milliseconds()
		finished := s.now()
		record.FinishedAt = &finished
		// Sanitised: a refresh failure reaches the API, and the raw error can
		// name the socket path.
		record.Error = docker.SanitizeError(err)

		s.logger.Warn("inventory refresh failed",
			slog.String("trigger", string(trigger)),
			slog.String("error", record.Error))

		// Recorded with a detached context so a cancelled refresh still leaves
		// a trace, unless the process is going away entirely.
		failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		failed, recordErr := s.inventory.RecordFailure(failCtx, domain.LocalHostID, record)
		if recordErr != nil {
			s.logger.Error("could not record refresh failure",
				slog.String("error", recordErr.Error()))
			return record, err
		}
		return failed, err
	}

	record.DurationMS = s.now().Sub(startedAt).Milliseconds()
	record.Checksum = InventoryChecksum(result.details, result.images, result.networks, result.volumes)

	commit := store.RefreshCommit{
		Host: domain.Host{
			ID:         domain.LocalHostID,
			Name:       "local",
			Runtime:    domain.RuntimeDocker,
			APIVersion: result.info.APIVersion,
			OSType:     result.info.OSType,
		},
		Containers:      result.records,
		Images:          result.images,
		Networks:        result.networks,
		Volumes:         result.volumes,
		Warnings:        result.warnings,
		Record:          record,
		AbsentRetention: s.cfg.AbsentRetention,
		Now:             s.now(),
	}

	persisted, err := s.inventory.CommitRefresh(ctx, commit)
	if err != nil {
		s.logger.Error("could not persist inventory refresh", slog.String("error", err.Error()))
		return record, err
	}

	s.logger.Info("inventory refresh complete",
		slog.String("trigger", string(trigger)),
		slog.Int64("generation", persisted.Generation),
		slog.Int("containers", persisted.ContainersListed),
		slog.Int("failed", persisted.ContainersFailed),
		slog.Int("warnings", persisted.WarningCount),
		slog.Int64("durationMs", persisted.DurationMS))
	return persisted, nil
}

// collectResult is everything one refresh gathered before persistence.
type collectResult struct {
	info     docker.Info
	records  []store.ContainerRecord
	details  []domain.ContainerDetail
	images   []domain.Image
	networks []domain.Network
	volumes  []domain.Volume
	warnings []domain.InventoryWarning
}

// collect gathers the inventory from the runtime.
//
// Only a failure to LIST containers fails the refresh. Every per-container
// problem becomes a warning, because one unreadable container must not cost an
// operator the other nine hundred.
func (s *InventoryService) collect(ctx context.Context, record *domain.RefreshRecord) (*collectResult, error) {
	result := &collectResult{
		warnings: make([]domain.InventoryWarning, 0),
	}

	// Ping first: it distinguishes "Docker is down" from "listing failed",
	// and populates the host's API version.
	info, err := s.runtime.Ping(ctx)
	if err != nil {
		return nil, err
	}
	result.info = info

	summaries, err := s.runtime.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	record.ContainersListed = len(summaries)

	inspections := s.inspectContainers(ctx, summaries)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Unique image IDs first, then one inspection each: the cache is the
	// absence of duplicate work rather than a lookup structure bolted on after.
	imageIDs := make([]string, 0, len(inspections))
	seenImages := make(map[string]struct{}, len(inspections))

	result.records = make([]store.ContainerRecord, 0, len(inspections))
	result.details = make([]domain.ContainerDetail, 0, len(inspections))

	for i := range inspections {
		outcome := &inspections[i]
		result.warnings = append(result.warnings, outcome.warnings...)

		if outcome.failed {
			record.ContainersFailed++
		} else {
			record.ContainersInspected++
		}

		result.records = append(result.records, store.ContainerRecord{
			Detail:  outcome.detail,
			RawJSON: outcome.raw,
		})
		result.details = append(result.details, outcome.detail)

		imageID := outcome.detail.Overview.ImageID
		if imageID == "" {
			continue
		}
		if _, seen := seenImages[imageID]; seen {
			continue
		}
		seenImages[imageID] = struct{}{}
		imageIDs = append(imageIDs, imageID)
	}

	images, imageWarnings := s.inspectImages(ctx, imageIDs)
	result.images = images
	result.warnings = append(result.warnings, imageWarnings...)
	record.ImagesInspected = len(images)

	// Networks and volumes are metadata; failing to list them degrades the
	// inventory rather than invalidating it.
	if networks, err := s.runtime.ListNetworks(ctx); err != nil {
		result.warnings = append(result.warnings, domain.InventoryWarning{
			Code:       domain.WarningIncompleteData,
			Message:    "network metadata could not be listed",
			OccurredAt: s.now(),
		})
	} else {
		result.networks = networks
		record.NetworksListed = len(networks)
	}

	if volumes, err := s.runtime.ListVolumes(ctx); err != nil {
		result.warnings = append(result.warnings, domain.InventoryWarning{
			Code:       domain.WarningIncompleteData,
			Message:    "volume metadata could not be listed",
			OccurredAt: s.now(),
		})
	} else {
		result.volumes = volumes
		record.VolumesListed = len(volumes)
	}

	record.WarningCount = len(result.warnings)
	return result, nil
}

// inspectOutcome is one container's inspection result.
type inspectOutcome struct {
	detail   domain.ContainerDetail
	raw      []byte
	warnings []domain.InventoryWarning
	failed   bool
}

// inspectContainers inspects every container with bounded concurrency.
//
// Results are written into a pre-sized slice at the container's own index, so
// the output order matches the input order regardless of which worker finished
// first -- no mutex, and no post-hoc sort to get determinism.
func (s *InventoryService) inspectContainers(ctx context.Context, summaries []domain.ContainerSummary) []inspectOutcome {
	outcomes := make([]inspectOutcome, len(summaries))

	workers := s.cfg.Workers
	if workers < 1 {
		workers = 1
	}
	if workers > len(summaries) {
		workers = len(summaries)
	}
	if workers == 0 {
		return outcomes
	}

	// A buffered channel as a semaphore: at most `workers` goroutines are in
	// flight, and no goroutine is created beyond one per container.
	semaphore := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i := range summaries {
		// Cancellation stops the sweep promptly rather than after every
		// remaining container has been dispatched.
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		semaphore <- struct{}{}

		go func(index int, summary domain.ContainerSummary) {
			defer wg.Done()
			defer func() { <-semaphore }()
			outcomes[index] = s.inspectOne(ctx, summary)
		}(i, summaries[i])
	}

	wg.Wait()
	return outcomes
}

// inspectOne inspects a single container, degrading to summary data on failure.
func (s *InventoryService) inspectOne(ctx context.Context, summary domain.ContainerSummary) inspectOutcome {
	inspection, err := s.runtime.InspectContainer(ctx, summary.ID)
	if err == nil {
		detail := inspection.Detail
		// The summary knows first/last seen and generation; the inspection
		// knows everything else. Preserve the summary's identity fields when
		// inspection returned an empty record.
		if detail.Overview.ID == "" {
			detail.Overview = summary
		}
		return inspectOutcome{
			detail:   detail,
			raw:      inspection.RawJSON,
			warnings: inspection.Warnings,
		}
	}

	// A container that vanished between list and inspect is routine churn on a
	// busy host, not a fault. It is still recorded, from summary data, with a
	// warning explaining why the detail is thin.
	code := domain.WarningInspectFailed
	message := "container could not be inspected; recorded from summary data only"
	if errors.Is(err, docker.ErrContainerVanished) {
		code = domain.WarningContainerVanished
		message = "container was removed while the inventory was being collected"
	}

	return inspectOutcome{
		detail: domain.ContainerDetail{
			Overview:     summary,
			State:        domain.StateDetail{State: summary.State, Health: summary.Health, Status: summary.Status},
			Compose:      summary.Compose,
			HarborMaster: summary.HarborMaster,
			Ports:        summary.Ports,
			Environment:  []domain.EnvVar{},
			Labels:       []domain.Label{},
			Mounts:       []domain.Mount{},
			Networks:     []domain.NetworkAttachment{},
			Warnings:     []domain.InventoryWarning{},
		},
		failed: true,
		warnings: []domain.InventoryWarning{{
			ContainerID:   summary.ID,
			ContainerName: summary.Name,
			Code:          code,
			Message:       message,
			OccurredAt:    s.now(),
		}},
	}
}

// inspectImages resolves image metadata once per unique image ID.
func (s *InventoryService) inspectImages(ctx context.Context, imageIDs []string) ([]domain.Image, []domain.InventoryWarning) {
	images := make([]domain.Image, len(imageIDs))
	failures := make([]*domain.InventoryWarning, len(imageIDs))

	workers := s.cfg.Workers
	if workers > len(imageIDs) {
		workers = len(imageIDs)
	}
	if workers < 1 {
		return []domain.Image{}, nil
	}

	semaphore := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i := range imageIDs {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		semaphore <- struct{}{}

		go func(index int, imageID string) {
			defer wg.Done()
			defer func() { <-semaphore }()

			image, err := s.runtime.InspectImage(ctx, imageID)
			if err != nil {
				// An image removed while a container still uses it is a normal
				// state, not a refresh failure.
				failures[index] = &domain.InventoryWarning{
					Code:       domain.WarningImageUnavailable,
					Message:    "image metadata unavailable for " + domain.ShortenID(imageID),
					OccurredAt: s.now(),
				}
				return
			}
			images[index] = *image
		}(i, imageIDs[i])
	}
	wg.Wait()

	resolved := make([]domain.Image, 0, len(images))
	warnings := make([]domain.InventoryWarning, 0)
	for i := range images {
		if failures[i] != nil {
			warnings = append(warnings, *failures[i])
			continue
		}
		if images[i].ID != "" {
			resolved = append(resolved, images[i])
		}
	}
	return resolved, warnings
}

// Status assembles the inventory status payload.
func (s *InventoryService) Status(ctx context.Context) (domain.InventoryStatus, error) {
	status := domain.InventoryStatus{
		Enabled: s.cfg.Enabled,
		Runtime: domain.RuntimeDocker,
		State:   domain.RefreshIdle,
	}

	s.mu.RLock()
	status.InProgress = s.running
	s.mu.RUnlock()

	// Docker connectivity is probed live rather than read from the last
	// refresh: an operator asking "what is the state" wants now, not then.
	if info, err := s.runtime.Ping(ctx); err != nil {
		status.Docker = domain.Component{
			Status: domain.StatusDown,
			Detail: docker.SanitizeError(err),
		}
	} else {
		status.Docker = domain.Component{Status: domain.StatusUp, Version: info.APIVersion}
	}

	generation, checksum, err := s.inventory.CurrentGeneration(ctx)
	if err != nil {
		return status, err
	}
	status.Generation = generation
	status.Checksum = checksum

	if status.LastAttempt, err = s.inventory.LastRefresh(ctx, false); err != nil {
		return status, err
	}
	if status.LastSuccess, err = s.inventory.LastRefresh(ctx, true); err != nil {
		return status, err
	}

	switch {
	case status.InProgress:
		status.State = domain.RefreshRunning
	case status.LastAttempt != nil:
		status.State = status.LastAttempt.State
	}

	if status.Counts, err = s.inventory.Counts(ctx); err != nil {
		return status, err
	}
	if status.Warnings, err = s.inventory.Warnings(ctx, generation, 50); err != nil {
		return status, err
	}
	return status, nil
}
