package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Targeted, single-resource inventory writes.
//
// These exist so an event-driven refresh can update one container without
// re-reading a thousand of them. They deliberately reuse the SAME upsert
// helpers a full refresh uses (upsertContainer, upsertImages, and friends), so
// there is exactly one place that knows how a container becomes rows. A second
// implementation would drift, and the drift would show up as an inventory that
// disagrees with itself depending on what triggered the write.
//
// Generation policy: a targeted write joins the CURRENT generation. It never
// allocates a new one, and it never touches the checksum. Generation and
// checksum mean "this is the state a complete sweep observed", and a
// single-container update is not that. Advancing them here would tell a client
// its whole inventory was re-verified when one row had been touched.
//
// Every write reads the current generation INSIDE its own transaction, so a
// full refresh committing concurrently cannot leave a targeted row stranded at
// a generation that no longer exists.

// ErrNoInventory reports that no successful full refresh has been persisted, so
// there is no generation for a targeted write to join.
//
// The caller's correct response is to request a full reconciliation: writing
// one container into an empty inventory would produce a host that appears to
// have exactly one container.
var ErrNoInventory = errors.New("no inventory generation exists yet")

// UpsertContainer writes one container at the current generation.
//
// Used by an event-triggered targeted refresh. The record comes from the same
// docker.Runtime inspection path a full refresh uses, so normalization is
// identical.
func (r *InventoryRepository) UpsertContainer(ctx context.Context, record ContainerRecord, now time.Time) error {
	if record.Detail.Overview.ID == "" {
		return errors.New("container record has no id")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin targeted container write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	generation, err := currentGenerationTx(ctx, tx)
	if err != nil {
		return err
	}
	if generation == 0 {
		return ErrNoInventory
	}

	// The host row must exist for the container's foreign key. A targeted write
	// can be the first thing that touches a host after a restart.
	if err := ensureHostTx(ctx, tx, hostIDOrDefault(record.Detail.Overview.HostID)); err != nil {
		return err
	}

	if err := upsertContainer(ctx, tx, record, generation, now.UTC()); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit targeted container write: %w", err)
	}
	return nil
}

// MarkContainerAbsent records that a container is confirmed gone.
//
// The row is retained with present = 0 rather than deleted, exactly as a full
// refresh does it, so the container's observed lifetime and its warnings
// survive its removal. Retention still governs eventual deletion.
//
// Reports ErrNotFound when the container was never recorded, which is the
// normal outcome for a `docker run --rm` that lived and died between two
// sweeps: HarborMaster saw the destroy event for something it never inventoried.
func (r *InventoryRepository) MarkContainerAbsent(ctx context.Context, containerID string, now time.Time) error {
	if containerID == "" {
		return errors.New("container id is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	result, err := r.db.ExecContext(ctx,
		`UPDATE containers SET present = 0, last_seen_at = ? WHERE id = ? AND present = 1`,
		formatTime(now.UTC()), containerID)
	if err != nil {
		return fmt.Errorf("mark container absent: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// UpsertImage writes one image's metadata at the current generation.
func (r *InventoryRepository) UpsertImage(ctx context.Context, image domain.Image, now time.Time) error {
	if image.ID == "" {
		return errors.New("image record has no id")
	}
	return r.inCurrentGeneration(ctx, now, "targeted image write",
		func(ctx context.Context, tx *sql.Tx, generation int64, at time.Time) error {
			return upsertImages(ctx, tx, []domain.Image{image}, generation, at)
		})
}

// ReplaceNetworks writes the whole network catalog at the current generation.
//
// Networks are refreshed as a set rather than one at a time. The Docker adapter
// lists networks in a single call and has no per-network inspect, so a targeted
// "refresh this network" would either widen the read-only adapter surface for
// no gain or do the same list call anyway. Doing it as a coalesced set keeps
// the adapter narrow, and the debouncer collapses a burst of network events
// into one of these regardless.
func (r *InventoryRepository) ReplaceNetworks(ctx context.Context, networks []domain.Network, now time.Time) error {
	return r.inCurrentGeneration(ctx, now, "targeted network write",
		func(ctx context.Context, tx *sql.Tx, generation int64, at time.Time) error {
			if err := upsertNetworks(ctx, tx, networks, generation, at); err != nil {
				return err
			}
			// A network the daemon no longer reports is removed rather than
			// retained. Unlike a container, a network carries no history worth
			// keeping, and a stale row would show an attachment target that
			// cannot be attached to.
			// emptyIsSuspect: a daemon reporting zero networks is reporting
			// something impossible, since bridge, host, and none always exist.
			return deleteMissing(ctx, tx, "networks", "id", idsOfNetworks(networks), emptyIsSuspect)
		})
}

// ReplaceVolumes writes the whole volume catalog at the current generation.
// See ReplaceNetworks for why this is a set operation.
func (r *InventoryRepository) ReplaceVolumes(ctx context.Context, volumes []domain.Volume, now time.Time) error {
	return r.inCurrentGeneration(ctx, now, "targeted volume write",
		func(ctx context.Context, tx *sql.Tx, generation int64, at time.Time) error {
			if err := upsertVolumes(ctx, tx, volumes, generation, at); err != nil {
				return err
			}
			// emptyMeansEmpty: a host with no volumes is an ordinary state, and
			// the last volume being removed is exactly when this matters.
			return deleteMissing(ctx, tx, "volumes", "name", namesOfVolumes(volumes), emptyMeansEmpty)
		})
}

// inCurrentGeneration runs write inside one transaction at the current
// generation, or fails with ErrNoInventory when there is none.
func (r *InventoryRepository) inCurrentGeneration(
	ctx context.Context,
	now time.Time,
	what string,
	write func(ctx context.Context, tx *sql.Tx, generation int64, at time.Time) error,
) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s: %w", what, err)
	}
	defer func() { _ = tx.Rollback() }()

	generation, err := currentGenerationTx(ctx, tx)
	if err != nil {
		return err
	}
	if generation == 0 {
		return ErrNoInventory
	}

	if err := write(ctx, tx, generation, now.UTC()); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", what, err)
	}
	return nil
}

// emptyRule says how a catalog read of zero rows is to be interpreted.
//
// The two catalogs differ, and treating them the same was a bug. A read only
// reaches this layer when it SUCCEEDED -- the services return early on error --
// so the question is not "did the read work" but "is zero a believable answer
// for this catalog".
type emptyRule int

const (
	// emptyMeansEmpty: zero is a legitimate state, so the catalog is emptied.
	// True of volumes: a host with no volumes is ordinary.
	emptyMeansEmpty emptyRule = iota

	// emptyIsSuspect: zero cannot occur on a healthy daemon, so it is read as a
	// degraded response and the catalog is left alone. True of networks, where
	// bridge, host, and none always exist. Retaining a stale row briefly is a
	// better failure than blanking the catalog on a hiccup, and the next
	// reconciliation corrects it either way.
	emptyIsSuspect
)

// deleteMissing removes rows of a catalog table whose key is not in keep.
//
// When keep is empty the behaviour is governed by rule; see emptyRule.
//
// This function previously returned early on ANY empty set. That cost a real
// bug: on a host whose only volume was removed, the correct catalog is empty,
// the early return skipped the delete, and the removed volume stayed in the
// inventory until the next full reconciliation -- fifteen minutes by default.
// It went unnoticed because networks, which always have a floor of three, can
// never exercise the empty case on a real daemon.
//
// The full-refresh path does NOT come through here. It writes a new generation
// wholesale and does tolerate an empty list from a failed read: see
// InventoryService.collect, which records a warning and degrades rather than
// failing.
func deleteMissing(ctx context.Context, tx *sql.Tx, table, keyColumn string, keep []string, rule emptyRule) error {
	// table and keyColumn are compile-time constants from this file only; no
	// caller-supplied string reaches the query text.
	if len(keep) == 0 {
		if rule == emptyIsSuspect {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("prune %s: %w", table, err)
		}
		return nil
	}

	placeholders := make([]byte, 0, len(keep)*2)
	args := make([]any, 0, len(keep))
	for i, key := range keep {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, key)
	}

	query := "DELETE FROM " + table + " WHERE " + keyColumn + " NOT IN (" + string(placeholders) + ")"
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("prune %s: %w", table, err)
	}
	return nil
}

func idsOfNetworks(networks []domain.Network) []string {
	ids := make([]string, 0, len(networks))
	for _, network := range networks {
		if network.ID != "" {
			ids = append(ids, network.ID)
		}
	}
	return ids
}

func namesOfVolumes(volumes []domain.Volume) []string {
	names := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		if volume.Name != "" {
			names = append(names, volume.Name)
		}
	}
	return names
}

// currentGenerationTx reads the current generation inside a transaction.
func currentGenerationTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	var generation sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT MAX(generation) FROM inventory_refreshes WHERE state = 'succeeded'`).
		Scan(&generation)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read current generation: %w", err)
	}
	return generation.Int64, nil
}

func ensureHostTx(ctx context.Context, tx *sql.Tx, hostID string) error {
	now := formatTime(time.Now().UTC())
	_, err := tx.ExecContext(ctx, `
		INSERT INTO hosts (id, name, runtime, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		hostID, hostID, domain.RuntimeDocker, now, now)
	if err != nil {
		return fmt.Errorf("ensure host: %w", err)
	}
	return nil
}
