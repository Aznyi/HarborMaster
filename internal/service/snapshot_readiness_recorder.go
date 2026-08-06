package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Persisting restore readiness.
//
// # The defect
//
// `snapshots.readiness_status` and `readiness_evaluated_at` have existed since
// migration 0004, and nothing ever wrote them. The engine that computes a
// verdict ran only inside `GET /snapshots/{id}/restore-readiness`, which
// returned the report to the caller and threw it away.
//
// So every snapshot sat at `unknown` forever. That is a change-plan INPUT: the
// planner reads it, the risk model charges points for an unknown readiness, and
// the acquisition preflight refuses a plan whose snapshot is NOT_READY. An
// estate where nothing had ever been evaluated therefore carried permanent
// unexplained risk on every plan.
//
// # Why a recorder rather than a call at each site
//
// Evaluating needs the snapshot AND its three section tables; recording needs
// the write path. Putting that in the capture service would give it a
// dependency on readiness internals, and putting it in the handler would leave
// the capture path unevaluated. One small type owns the whole operation and
// both callers use it.

// ReadinessSections reads the stored sections a readiness evaluation needs.
type ReadinessSections interface {
	Get(ctx context.Context, id int64) (domain.Snapshot, error)
	Environment(ctx context.Context, snapshotID int64) ([]domain.SnapshotEnvEntry, error)
	Mounts(ctx context.Context, snapshotID int64) ([]domain.SnapshotMountRow, error)
	Networks(ctx context.Context, snapshotID int64) ([]domain.SnapshotNetworkRow, error)
	RecordReadiness(ctx context.Context, report domain.ReadinessReport) error
}

// ReadinessRecorder evaluates one snapshot's readiness and stores the verdict.
type ReadinessRecorder struct {
	engine   *ReadinessEngine
	store    ReadinessSections
	logger   *slog.Logger
	deadline time.Duration
}

// readinessRecordBudget bounds one detached evaluation.
//
// Generous relative to the work -- four indexed reads and a pure computation --
// and bounded because it runs on a goroutine nobody waits for.
const readinessRecordBudget = 30 * time.Second

// NewReadinessRecorder builds a ReadinessRecorder.
func NewReadinessRecorder(
	engine *ReadinessEngine,
	sections ReadinessSections,
	logger *slog.Logger,
) *ReadinessRecorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &ReadinessRecorder{
		engine:   engine,
		store:    sections,
		logger:   logger,
		deadline: readinessRecordBudget,
	}
}

// Evaluate computes a snapshot's readiness and records it, returning the report.
//
// The caller gets the report whether or not the write succeeded: a readiness
// verdict is still true when the cache of it could not be updated, and failing
// the request would turn a bookkeeping problem into an outage of a read
// endpoint.
func (r *ReadinessRecorder) Evaluate(
	ctx context.Context,
	snapshot domain.Snapshot,
	environment []domain.SnapshotEnvEntry,
	mounts []domain.SnapshotMountRow,
	networks []domain.SnapshotNetworkRow,
) (domain.ReadinessReport, error) {
	if r == nil || r.engine == nil {
		return domain.ReadinessReport{}, ErrSnapshotsDisabled
	}

	report, err := r.engine.Evaluate(ctx, snapshot, environment, mounts, networks)
	if err != nil {
		return domain.ReadinessReport{}, err
	}

	if r.store != nil {
		if recordErr := r.store.RecordReadiness(ctx, report); recordErr != nil {
			r.logger.WarnContext(ctx, "could not cache a restore-readiness verdict",
				slog.Int64("snapshotId", snapshot.ID),
				slog.String("error", recordErr.Error()))
		}
	}
	return report, nil
}

// Record evaluates one snapshot by id, loading everything it needs.
//
// Used by the capture path, which has a snapshot id and nothing else.
func (r *ReadinessRecorder) Record(ctx context.Context, snapshotID int64) error {
	if r == nil || r.engine == nil || r.store == nil {
		return nil
	}

	snapshot, err := r.store.Get(ctx, snapshotID)
	if err != nil {
		return err
	}
	environment, err := r.store.Environment(ctx, snapshotID)
	if err != nil {
		return err
	}
	mounts, err := r.store.Mounts(ctx, snapshotID)
	if err != nil {
		return err
	}
	networks, err := r.store.Networks(ctx, snapshotID)
	if err != nil {
		return err
	}

	_, err = r.Evaluate(ctx, snapshot, environment, mounts, networks)
	return err
}

// RecordDetached evaluates a snapshot on a bounded goroutine.
//
// Capture must not wait for this. Readiness reads the inventory, the image
// catalogue, the network list, and the volume list, and a capture requested
// from the UI should return as soon as the snapshot is durable.
//
// GraceContext rather than the request context: the request is usually already
// finished by the time this runs, and a readiness verdict that was abandoned
// because the caller disconnected would leave the same permanent `unknown` this
// exists to remove.
func (r *ReadinessRecorder) RecordDetached(ctx context.Context, snapshotID int64) {
	if r == nil || r.engine == nil || r.store == nil {
		return
	}

	go func() {
		evalCtx, cancel := GraceContext(ctx, r.deadline, r.deadline)
		defer cancel()

		if err := r.Record(evalCtx, snapshotID); err != nil {
			// WARN, not ERROR: the snapshot is captured and correct, and the
			// next evaluation will fill the gap. Nothing is lost but freshness.
			r.logger.WarnContext(evalCtx, "could not evaluate restore readiness after capture",
				slog.Int64("snapshotId", snapshotID),
				slog.String("error", err.Error()))
		}
	}()
}

// StoreReadinessSections adapts the snapshot repository to ReadinessSections.
type StoreReadinessSections struct {
	Snapshots *store.SnapshotRepository
}

func (s StoreReadinessSections) Get(ctx context.Context, id int64) (domain.Snapshot, error) {
	return s.Snapshots.Get(ctx, id)
}

func (s StoreReadinessSections) Environment(
	ctx context.Context, snapshotID int64,
) ([]domain.SnapshotEnvEntry, error) {
	return s.Snapshots.Environment(ctx, snapshotID)
}

func (s StoreReadinessSections) Mounts(
	ctx context.Context, snapshotID int64,
) ([]domain.SnapshotMountRow, error) {
	return s.Snapshots.Mounts(ctx, snapshotID)
}

func (s StoreReadinessSections) Networks(
	ctx context.Context, snapshotID int64,
) ([]domain.SnapshotNetworkRow, error) {
	return s.Snapshots.Networks(ctx, snapshotID)
}

func (s StoreReadinessSections) RecordReadiness(
	ctx context.Context, report domain.ReadinessReport,
) error {
	return s.Snapshots.RecordReadiness(ctx, report)
}

var _ ReadinessSections = StoreReadinessSections{}
