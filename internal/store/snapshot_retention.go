package store

import (
	"context"
	"fmt"
	"time"
)

// RetentionPolicy bounds stored snapshot history.
//
// Zero on either dimension disables it; both zero keeps everything, which is a
// valid but unbounded choice.
type RetentionPolicy struct {
	// MaxPerContainer is the maximum snapshots kept PER CONTAINER, not
	// globally. Per container because the value of history is per container: a
	// busy service must not evict a quiet one's only baseline.
	MaxPerContainer int
	// MaxAge is the maximum snapshot age.
	MaxAge time.Duration
	// BatchSize bounds one delete transaction.
	BatchSize int
	// Now is injectable for deterministic tests.
	Now time.Time
}

// RetentionResult reports what a prune pass did.
type RetentionResult struct {
	Deleted      int
	Batches      int
	MaxBatchSize int
}

// PruneSnapshots deletes snapshots outside the retention policy.
//
// # The newest is never pruned
//
// Whatever the policy says, the most recent snapshot of every container
// survives -- including when it is older than MaxAge. It is the restore
// baseline, and a retention policy that can delete the only record of how a
// container is configured is not a retention policy, it is data loss on a
// timer.
//
// # Bounded batches
//
// Deletion runs in BatchSize-sized transactions with a context check between
// them. SQLite tolerates exactly one writer, so a single unbounded DELETE over
// a large backlog would hold that writer for as long as it took and stall
// every other write in the process.
//
// Child rows cascade, so no orphans are left behind.
func (r *SnapshotRepository) PruneSnapshots(ctx context.Context, policy RetentionPolicy) (RetentionResult, error) {
	var result RetentionResult

	if policy.MaxPerContainer <= 0 && policy.MaxAge <= 0 {
		return result, nil
	}
	if policy.BatchSize <= 0 {
		policy.BatchSize = 200
	}
	if policy.Now.IsZero() {
		policy.Now = time.Now().UTC()
	}

	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		ids, err := r.prunableIDs(ctx, policy)
		if err != nil {
			return result, err
		}
		if len(ids) == 0 {
			return result, nil
		}

		deleted, err := r.deleteBatch(ctx, ids)
		if err != nil {
			return result, err
		}

		result.Deleted += deleted
		result.Batches++
		if deleted > result.MaxBatchSize {
			result.MaxBatchSize = deleted
		}

		// A batch that deleted nothing means nothing more is prunable; without
		// this the loop could spin.
		if deleted == 0 {
			return result, nil
		}
	}
}

// prunableIDs selects one batch of deletable snapshot IDs.
//
// The row_number() window ranks each container's snapshots newest first, and
// rank 1 is excluded unconditionally -- that single predicate is what
// guarantees every container keeps a baseline, under both the count policy and
// the age policy.
func (r *SnapshotRepository) prunableIDs(ctx context.Context, policy RetentionPolicy) ([]int64, error) {
	const query = `
		WITH ranked AS (
			SELECT id,
			       created_at,
			       ROW_NUMBER() OVER (
			           PARTITION BY container_id
			           ORDER BY created_at DESC, id DESC
			       ) AS rank
			FROM snapshots
		)
		SELECT id FROM ranked
		WHERE rank > 1
		  AND (
		        (? > 0 AND rank > ?)
		     OR (? <> '' AND created_at < ?)
		      )
		ORDER BY id
		LIMIT ?`

	cutoff := ""
	if policy.MaxAge > 0 {
		cutoff = formatTime(policy.Now.Add(-policy.MaxAge))
	}

	rows, err := r.db.QueryContext(ctx, query,
		policy.MaxPerContainer, policy.MaxPerContainer,
		cutoff, cutoff,
		policy.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("select prunable snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()

	ids := make([]int64, 0, policy.BatchSize)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan prunable snapshot: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// deleteBatch removes one batch in a single transaction.
func (r *SnapshotRepository) deleteBatch(ctx context.Context, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	placeholders, args := idPlaceholders(ids)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin prune transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Child rows cascade via ON DELETE CASCADE, which the connection's
	// foreign_keys pragma enables.
	res, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("delete snapshots: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted snapshots: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit prune: %w", err)
	}
	return int(affected), nil
}
