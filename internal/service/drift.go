package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The configuration drift engine.
//
// # What it does, in one sentence
//
// It runs the Phase 3 diff engine between a container's baseline snapshot and
// its current configuration, classifies each resulting difference, and
// reconciles that against what is already stored.
//
// # What it deliberately does NOT do
//
// It does not compare anything itself. Every comparison -- key matching,
// ordering normalisation, secret handling, value truncation -- belongs to
// DiffEngine and is reused verbatim. A second comparison implementation would
// eventually disagree with the first, and the first is the one whose
// determinism the snapshot checksum depends on. This file is a CLASSIFIER and
// a RECONCILER over that engine's output.
//
// It also cannot change anything about Docker. There is no remediation path,
// no rollback, and no call into docker.Runtime anywhere below: the engine reads
// the inventory HarborMaster already persisted, not the daemon.
//
// # Two properties worth stating plainly
//
// AN INCOMPLETE COMPARISON RESOLVES NOTHING. The diff engine truncates past its
// entry budget. A truncated comparison has not established that the fields it
// never reached still match, so recording it as authoritative would silently
// clear real drift. Incomplete evaluations are stored as incomplete, and the
// repository refuses to resolve on them.
//
// A MISSING BASELINE IS NOT "NO DRIFT". A container with no snapshot has
// nothing to compare against. That is recorded as an incomplete evaluation with
// a reason, so the dashboard can say "never checked" rather than implying the
// container is clean.

// ErrNoBaseline reports that a container has no snapshot to compare against.
var ErrNoBaseline = errors.New("container has no baseline snapshot")

// ErrDriftDisabled reports that drift detection is switched off.
var ErrDriftDisabled = errors.New("drift detection is disabled")

// DriftSnapshots is the snapshot capability the drift engine needs.
//
// A narrow interface rather than *store.SnapshotRepository, so the engine is
// testable without a database and so the surface it depends on is visible.
type DriftSnapshots interface {
	Baseline(ctx context.Context, containerID string) (domain.Snapshot, error)
	Environment(ctx context.Context, snapshotID int64) ([]domain.SnapshotEnvEntry, error)
}

// DriftContainers is the inventory capability the drift engine needs.
type DriftContainers interface {
	Get(ctx context.Context, id string) (*domain.ContainerDetail, error)
	List(ctx context.Context, filter store.ContainerFilter) ([]domain.ContainerSummary, int, error)
}

// DriftStore is the persistence capability.
type DriftStore interface {
	ReconcileDrift(ctx context.Context, evaluation domain.DriftEvaluation,
		records []domain.DriftRecord, now time.Time) (store.UpsertResult, error)
}

// DriftPruner removes resolved history. Separate from DriftStore because
// retention is an optional capability: a deployment that keeps drift history
// forever supplies no pruner and the worker simply never prunes.
type DriftPruner interface {
	PruneResolved(ctx context.Context, cutoff time.Time, batch int) (int64, error)
}

// DriftInventory reports the current inventory generation, which every
// evaluation records so a drift can be tied to the observation behind it.
type DriftInventory interface {
	CurrentGeneration(ctx context.Context) (int64, string, error)
}

// DriftOptions configures a DriftService.
type DriftOptions struct {
	Snapshots  DriftSnapshots
	Containers DriftContainers
	Records    DriftStore
	Pruner     DriftPruner
	Inventory  DriftInventory
	// SpecBuilder renders a container's CURRENT configuration as a canonical
	// document, in memory. It never persists: evaluating drift must not create
	// a snapshot as a side effect, or every evaluation would become the new
	// baseline and drift would always be empty.
	SpecBuilder func(domain.ContainerDetail) domain.SnapshotSpec

	Config config.Drift
	Logger *slog.Logger
	Now    func() time.Time
}

// DriftService evaluates and persists configuration drift.
type DriftService struct {
	snapshots  DriftSnapshots
	containers DriftContainers
	records    DriftStore
	pruner     DriftPruner
	inventory  DriftInventory
	spec       func(domain.ContainerDetail) domain.SnapshotSpec

	// queue coalesces per-container evaluation requests. See drift_worker.go
	// for why drift has its own rather than joining the refresh scheduler.
	queue *evaluationQueue

	// diffs is the drift engine's OWN diff engine instance.
	//
	// Not the one the HTTP diff endpoint uses. DiffEngine bounds concurrency
	// with a semaphore, and sharing it would let a background sweep over a
	// thousand containers exhaust the slots and start returning 429 to
	// operators using the comparison endpoint. Background work must not
	// starve the foreground.
	diffs *DiffEngine

	cfg    config.Drift
	logger *slog.Logger
	now    func() time.Time

	// sweeping guards the full sweep so two cannot overlap. A second sweep
	// while one runs would re-read the same containers and contend for the
	// single SQLite writer to reach the same conclusion.
	sweeping sync.Mutex
}

// NewDriftService builds a DriftService.
func NewDriftService(opts DriftOptions) *DriftService {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	cfg := opts.Config
	if cfg.MaxRecordsPerContainer < 1 {
		cfg.MaxRecordsPerContainer = config.DefaultDriftMaxRecordsPerContainer
	}
	if cfg.EvaluationTimeout <= 0 {
		cfg.EvaluationTimeout = config.DefaultDriftEvaluationTimeout
	}
	if cfg.EvaluationDebounce <= 0 {
		cfg.EvaluationDebounce = config.DefaultDriftEvaluationDebounce
	}
	if cfg.MaxPendingEvaluations < 1 {
		cfg.MaxPendingEvaluations = config.DefaultDriftMaxPendingEvaluations
	}

	// A dedicated diff engine, deliberately single-slot: drift evaluations run
	// sequentially from one worker, so more concurrency would buy nothing and
	// would only widen the peak memory of a sweep.
	diffCfg := config.Snapshots{
		MaxConcurrentDiffs: 1,
		DiffTimeout:        cfg.EvaluationTimeout,
		MaxDiffEntries:     cfg.MaxRecordsPerContainer,
		MaxGroupEntries:    config.DefaultSnapshotMaxGroupEntries,
	}

	return &DriftService{
		snapshots:  opts.Snapshots,
		containers: opts.Containers,
		records:    opts.Records,
		pruner:     opts.Pruner,
		inventory:  opts.Inventory,
		spec:       opts.SpecBuilder,
		diffs:      NewDiffEngine(diffCfg),
		queue:      newEvaluationQueue(cfg.EvaluationDebounce, cfg.MaxPendingEvaluations, now),
		cfg:        cfg,
		logger:     logger,
		now:        now,
	}
}

// Enabled reports whether drift detection is switched on.
func (s *DriftService) Enabled() bool { return s.cfg.Enabled }

// EvaluateContainer compares one container against its baseline and persists
// the result.
//
// Returns the evaluation, which is meaningful even when it found nothing: an
// evaluation that ran and found no differences is the fact that distinguishes
// "clean" from "never checked".
func (s *DriftService) EvaluateContainer(ctx context.Context, containerID string) (domain.DriftEvaluation, error) {
	if !s.cfg.Enabled {
		return domain.DriftEvaluation{}, ErrDriftDisabled
	}
	if s.snapshots == nil || s.containers == nil || s.records == nil || s.spec == nil {
		return domain.DriftEvaluation{}, ErrDriftDisabled
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.EvaluationTimeout)
	defer cancel()

	evaluatedAt := s.now().UTC()
	generation := s.currentGeneration(ctx)

	detail, err := s.containers.Get(ctx, containerID)
	if err != nil || detail == nil {
		// A container that has left the inventory is not drift. Nothing is
		// recorded: there is no current configuration to compare, and writing
		// an evaluation would claim a comparison that did not happen.
		if errors.Is(err, store.ErrNotFound) || detail == nil {
			return domain.DriftEvaluation{}, store.ErrNotFound
		}
		return domain.DriftEvaluation{}, err
	}

	evaluation := domain.DriftEvaluation{
		ContainerID:         containerID,
		ContainerName:       detail.Overview.Name,
		EvaluatedAt:         evaluatedAt,
		InventoryGeneration: generation,
		Complete:            true,
	}

	baseline, err := s.snapshots.Baseline(ctx, containerID)
	if errors.Is(err, store.ErrNotFound) {
		// Recorded rather than skipped, so the dashboard can distinguish a
		// container with no drift from one that was never comparable.
		evaluation.Complete = false
		evaluation.Reason = "no baseline snapshot exists for this container"
		return s.persist(ctx, evaluation, nil)
	}
	if err != nil {
		return domain.DriftEvaluation{}, err
	}
	evaluation.SnapshotID = baseline.ID

	records, complete, reason, err := s.compare(ctx, baseline, *detail, evaluation)
	if err != nil {
		return domain.DriftEvaluation{}, err
	}
	evaluation.Complete = complete
	evaluation.Reason = reason
	evaluation.DriftCount = len(records)

	return s.persist(ctx, evaluation, records)
}

// compare runs the diff engine and classifies its output.
func (s *DriftService) compare(
	ctx context.Context,
	baseline domain.Snapshot,
	detail domain.ContainerDetail,
	evaluation domain.DriftEvaluation,
) (records []domain.DriftRecord, complete bool, reason string, err error) {
	var spec domain.SnapshotSpec
	if len(baseline.SpecJSON) > 0 {
		if decodeErr := json.Unmarshal(baseline.SpecJSON, &spec); decodeErr != nil {
			// Reported as an INCOMPLETE evaluation rather than as an error, and
			// the nil is deliberate.
			//
			// A baseline that cannot be decoded is a fact about that one
			// container, not a failure of the drift engine: returning an error
			// would abort the sweep and leave every container after it
			// unevaluated because one snapshot is malformed. Recording it as
			// incomplete keeps the sweep going and, because an incomplete
			// evaluation resolves nothing, this container's existing drift
			// stays exactly as it was rather than being silently cleared.
			//
			//nolint:nilerr // an undecodable baseline is an incomplete evaluation, not an engine failure.
			return nil, false, "the baseline snapshot document could not be decoded", nil
		}
	}

	env, err := s.snapshots.Environment(ctx, baseline.ID)
	if err != nil {
		return nil, false, "", err
	}

	// The current configuration, built in memory and never written.
	current := s.spec(detail)

	diff, err := s.diffs.Diff(ctx,
		DiffInput{SnapshotID: baseline.ID, Spec: spec, Env: env},
		DiffInput{SnapshotID: 0, Spec: current},
		DiffOptions{AgainstCurrent: true},
	)
	switch {
	case errors.Is(err, ErrDiffBusy):
		// Cannot happen with a single-slot engine driven by one worker, but
		// reported honestly rather than treated as "no drift" if it ever does.
		return nil, false, "the comparison could not be scheduled", nil
	case err != nil:
		return nil, false, "", err
	}

	records = make([]domain.DriftRecord, 0, diff.ChangedCount)
	for _, group := range diff.Groups {
		for _, entry := range group.Entries {
			if entry.Kind == domain.ChangeUnchanged {
				continue
			}
			records = append(records, s.record(group.Name, entry, baseline, evaluation))
		}
	}

	if diff.Truncated {
		return records, false, "the comparison exceeded its size budget; some fields were not examined", nil
	}
	return records, true, "", nil
}

// record turns one classified difference into a persistable record.
func (s *DriftService) record(
	group domain.DiffGroupName,
	entry domain.DiffEntry,
	baseline domain.Snapshot,
	evaluation domain.DriftEvaluation,
) domain.DriftRecord {
	classification := domain.ClassifyDrift(group, entry.Key, entry.Kind, entry.Old, entry.New)

	reason := classification.Reason
	if entry.Note != "" {
		// The diff engine's note explains an unverifiable comparison, which is
		// more specific than the classifier's generic phrase.
		reason = entry.Note
	}

	record := domain.DriftRecord{
		ContainerID:         evaluation.ContainerID,
		ContainerName:       evaluation.ContainerName,
		SnapshotID:          baseline.ID,
		DetectedAt:          evaluation.EvaluatedAt,
		LastSeenAt:          evaluation.EvaluatedAt,
		InventoryGeneration: evaluation.InventoryGeneration,
		Category:            classification.Category,
		Field:               entry.Key,
		Kind:                entry.Kind,
		Severity:            classification.Severity,
		Sensitive:           entry.Sensitive,
		Status:              domain.DriftStatusActive,
		Reason:              reason,
	}

	// Layer two of four. The diff engine already withheld these for a
	// sensitive entry; the record simply never carries them.
	if !entry.Sensitive {
		record.PreviousValue = entry.Old
		record.CurrentValue = entry.New
	}
	return record
}

// persist writes the evaluation and its records.
func (s *DriftService) persist(
	ctx context.Context,
	evaluation domain.DriftEvaluation,
	records []domain.DriftRecord,
) (domain.DriftEvaluation, error) {
	// Detached from cancellation but bounded: an evaluation that finished
	// comparing must not lose its result to a shutdown mid-write, and must not
	// hold the process open either.
	writeCtx, cancel := GraceContext(ctx, driftWriteGrace, driftWriteMax)
	defer cancel()

	result, err := s.records.ReconcileDrift(writeCtx, evaluation, records, s.now().UTC())
	if err != nil {
		return domain.DriftEvaluation{}, err
	}

	if result.Inserted > 0 || result.Resolved > 0 {
		s.logger.Info("drift evaluated",
			slog.String("container", domain.ShortenID(evaluation.ContainerID)),
			slog.Int("new", result.Inserted),
			slog.Int("stillPresent", result.Updated),
			slog.Int("resolved", result.Resolved),
			slog.Bool("complete", evaluation.Complete))
	}
	return evaluation, nil
}

// driftWriteGrace and driftWriteMax bound the persistence write at shutdown.
const (
	driftWriteGrace = 5 * time.Second
	driftWriteMax   = 30 * time.Second
)

// currentGeneration reads the inventory generation, tolerating failure.
//
// A generation that cannot be read is recorded as zero rather than failing the
// evaluation: the generation is provenance, and losing provenance is a smaller
// harm than not detecting drift at all.
func (s *DriftService) currentGeneration(ctx context.Context) int64 {
	if s.inventory == nil {
		return 0
	}
	generation, _, err := s.inventory.CurrentGeneration(ctx)
	if err != nil {
		return 0
	}
	return generation
}

// SweepResult reports what a full sweep did.
type SweepResult struct {
	Evaluated int
	Skipped   int
	Failed    int
}

// Sweep evaluates every container in the inventory.
//
// This is the authoritative pass. Event-driven evaluation is an optimisation
// that keeps the dashboard current between sweeps; the sweep is what
// guarantees a container whose events were missed is still checked.
//
// Containers are processed SEQUENTIALLY and in pages. A thousand containers
// evaluated concurrently would hold a thousand decoded snapshot documents in
// memory and contend for the single SQLite writer; sequential evaluation costs
// wall time, which a background sweep has, and bounds memory, which it does
// not otherwise have.
func (s *DriftService) Sweep(ctx context.Context) (SweepResult, error) {
	var result SweepResult
	if !s.cfg.Enabled {
		return result, ErrDriftDisabled
	}

	// Refused rather than queued: a second concurrent sweep re-reads the same
	// containers to reach the same conclusion.
	if !s.sweeping.TryLock() {
		return result, nil
	}
	defer s.sweeping.Unlock()

	const pageSize = 100
	for offset := 0; ; offset += pageSize {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}

		summaries, total, err := s.containers.List(ctx, store.ContainerFilter{
			Page: store.Page{Limit: pageSize, Offset: offset},
		})
		if err != nil {
			return result, err
		}
		if len(summaries) == 0 {
			return result, nil
		}

		for _, summary := range summaries {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			// An absent container has no current configuration to compare.
			if !summary.Present {
				result.Skipped++
				continue
			}

			switch _, err := s.EvaluateContainer(ctx, summary.ID); {
			case err == nil:
				result.Evaluated++
			case errors.Is(err, store.ErrNotFound):
				result.Skipped++
			default:
				result.Failed++
				s.logger.Warn("drift evaluation failed",
					slog.String("container", domain.ShortenID(summary.ID)),
					slog.String("error", err.Error()))
			}
		}

		if offset+len(summaries) >= total {
			return result, nil
		}
	}
}
