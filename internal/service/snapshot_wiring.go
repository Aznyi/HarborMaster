package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// StoreReadinessInventory adapts the concrete repositories to the narrow
// interface the readiness engine consumes.
//
// The adapter exists so ReadinessEngine depends on behaviour rather than on
// four repository structs, which keeps it testable without a database. It also
// makes the "measure age from the last SUCCESS" rule explicit at the boundary
// rather than leaving it as a bool argument at the call site.
type StoreReadinessInventory struct {
	Inventory *store.InventoryRepository
	Images    *store.ImageRepository
	Networks  *store.NetworkRepository
	Volumes   *store.VolumeRepository
}

// CurrentGeneration reports the newest committed inventory generation.
func (s StoreReadinessInventory) CurrentGeneration(ctx context.Context) (int64, string, error) {
	return s.Inventory.CurrentGeneration(ctx)
}

// LastSuccessfulRefresh returns the most recent SUCCESSFUL refresh.
//
// succeededOnly is true, deliberately: a failed attempt does not make the
// inventory any fresher, and measuring age from it would hide staleness at
// exactly the moment staleness matters most.
func (s StoreReadinessInventory) LastSuccessfulRefresh(ctx context.Context) (*domain.RefreshRecord, error) {
	return s.Inventory.LastRefresh(ctx, true)
}

// ImageExists reports whether an image is still present locally.
func (s StoreReadinessInventory) ImageExists(ctx context.Context, imageID, reference string) (bool, error) {
	return s.Images.ImageExists(ctx, imageID, reference)
}

// NetworkNames returns every network name in the current inventory.
func (s StoreReadinessInventory) NetworkNames(ctx context.Context) (map[string]struct{}, error) {
	return s.Networks.NetworkNames(ctx)
}

// VolumeNames returns every volume name in the current inventory.
func (s StoreReadinessInventory) VolumeNames(ctx context.Context) (map[string]struct{}, error) {
	return s.Volumes.VolumeNames(ctx)
}

// RetentionService prunes snapshot history on a schedule.
type RetentionService struct {
	snapshots *store.SnapshotRepository
	cfg       config.Snapshots
	logger    *slog.Logger
	now       func() time.Time
}

// NewRetentionService builds a RetentionService.
func NewRetentionService(
	snapshots *store.SnapshotRepository,
	cfg config.Snapshots,
	logger *slog.Logger,
) *RetentionService {
	if logger == nil {
		logger = slog.Default()
	}
	return &RetentionService{
		snapshots: snapshots,
		cfg:       cfg,
		logger:    logger,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// Run prunes on an interval until ctx is cancelled.
//
// Runs in its own goroutine, off the request path: pruning a large backlog
// holds the single SQLite writer, and no HTTP request should ever wait on that.
func (s *RetentionService) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		return
	}
	if s.cfg.RetentionCount <= 0 && s.cfg.RetentionAge <= 0 {
		s.logger.Info("snapshot retention disabled; history is kept indefinitely")
		<-ctx.Done()
		return
	}
	if s.cfg.PruneInterval <= 0 {
		<-ctx.Done()
		return
	}

	ticker := time.NewTicker(s.cfg.PruneInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pruneOnce(ctx)
		}
	}
}

func (s *RetentionService) pruneOnce(ctx context.Context) {
	result, err := s.snapshots.PruneSnapshots(ctx, store.RetentionPolicy{
		MaxPerContainer: s.cfg.RetentionCount,
		MaxAge:          s.cfg.RetentionAge,
		BatchSize:       s.cfg.PruneBatch,
		Now:             s.now(),
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Warn("snapshot retention failed", slog.String("error", err.Error()))
		return
	}
	if result.Deleted > 0 {
		s.logger.Info("snapshot retention pruned history",
			slog.Int("deleted", result.Deleted),
			slog.Int("batches", result.Batches))
	}
}

// ResolveSnapshotKey loads the installation HMAC key for a deployment.
//
// The prior key ID is read from the database first, so a key that has been in
// use and has since disappeared fails startup instead of being silently
// regenerated. A fresh key would make every historical digest compare unequal,
// which reads to an operator as "every secret in every container changed at
// once" -- a false security alarm indistinguishable from a real breach.
func ResolveSnapshotKey(
	ctx context.Context,
	snapshots *store.SnapshotRepository,
	cfg config.Snapshots,
	dataDir string,
) (SecretKey, error) {
	priorKeyID, err := snapshots.DistinctDigestKeyID(ctx)
	if err != nil {
		return SecretKey{}, err
	}

	return LoadSecretKey(SecretKeyOptions{
		File:          cfg.HMACKeyFile,
		Value:         cfg.HMACKey,
		GeneratePath:  dataDir,
		PriorKeyIDSet: priorKeyID != "",
	})
}
