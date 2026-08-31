package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// The evidence an image-cleanup decision is made from.
//
// # Why this reads records rather than the image store
//
// A cleanup that started from "every image on this host" would be deciding
// about artefacts HarborMaster never introduced and knows nothing about. It
// starts instead from HarborMaster's own lifecycle evidence: an image is a
// candidate only when a successful, settled update moved a workload OFF it.
// Anything else is the operator's image store and is not HarborMaster's to
// remove.
//
// # The cost rule
//
// A cleanup pass is a background maintenance task, and it must not become a
// per-image walk over six tables. Candidates come back in ONE bounded query,
// and the references that would retain them come back in a FIXED number of
// batched queries over that candidate set -- the same shape, and for the same
// reason, as the container attention projection.

// maxCleanupCandidates bounds one pass.
//
// A pass is periodic and idempotent, so examining a bounded slice and looking
// at the rest next time costs nothing but a little latency. An unbounded pass
// on a host with years of history would hold the whole set in memory and issue
// IN lists past SQLite's variable limit.
const maxCleanupCandidates = 200

// ImageRetentionRepository reads the evidence an image-cleanup pass needs.
//
// Reads only. It holds no Docker capability and cannot remove anything: the
// service that acts on its answers holds the capability, and this type exists
// so that the reasoning and the destruction stay in separate places.
type ImageRetentionRepository struct {
	db *sql.DB
}

// ImageCleanupCandidate is one image a settled update moved a workload off.
//
// Being a candidate establishes only that HarborMaster knows where the image
// came from. Every retention question is answered separately; see
// domain.DecideImageRetention.
type ImageCleanupCandidate struct {
	// ImageID is the local image the workload was running before the update.
	ImageID string
	// ContainerName is the workload it was superseded for. Used to order
	// generations, which is a per-workload rule.
	ContainerName string
	// SettledAt is when the most recent successful update off this image
	// reached its safe terminal state.
	SettledAt time.Time
	// NewerGenerations is how many MORE RECENT images the same workload has
	// since been moved off. Zero means this is the immediately previous one.
	NewerGenerations int
}

// ImageCleanupCandidates returns images a settled successful update superseded.
//
// # What makes a row a candidate
//
// All of these, in one predicate:
//
//	state = 'succeeded'        the recreation finished
//	original_removed = 1       the parked original was cleaned up, which is the
//	                           last act of a successful execution and therefore
//	                           the marker that the lifecycle reached its safe
//	                           terminal state
//	old_image_id <> ''         HarborMaster knows what it replaced
//
// `original_removed` is doing real work here rather than being a tidy extra.
// It is written only after the replacement passed every verification AND the
// success was durably recorded -- see ExecutionService.succeed, where that
// ordering is the safety property. An execution that succeeded but could not
// remove the original is NOT settled: both containers are still on the host and
// an operator has a recovery note, so its old image stays out of this set.
//
// Ordered oldest-settled first, so a bounded pass makes progress from the end
// most likely to be eligible rather than re-examining the newest every time.
func (r *ImageRetentionRepository) ImageCleanupCandidates(
	ctx context.Context,
) ([]ImageCleanupCandidate, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT old_image_id, container_name, MAX(completed_at) AS settled_at
		  FROM executions
		 WHERE state = 'succeeded'
		   AND original_removed = 1
		   AND old_image_id <> ''
		   AND completed_at IS NOT NULL
		 GROUP BY old_image_id, container_name
		 ORDER BY settled_at ASC
		 LIMIT ?`, maxCleanupCandidates)
	if err != nil {
		return nil, fmt.Errorf("query image cleanup candidates: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	candidates := make([]ImageCleanupCandidate, 0, 32)
	for rows.Next() {
		var (
			candidate ImageCleanupCandidate
			settled   string
		)
		if err := rows.Scan(&candidate.ImageID, &candidate.ContainerName, &settled); err != nil {
			return nil, fmt.Errorf("scan image cleanup candidate: %w", AsError(err))
		}
		parsed, err := parseTime(settled)
		if err != nil {
			// A row whose settlement time cannot be read establishes no clock,
			// so it is skipped rather than treated as settled long ago.
			continue
		}
		candidate.SettledAt = parsed
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read image cleanup candidates: %w", AsError(err))
	}

	rankGenerations(candidates)
	return candidates, nil
}

// rankGenerations fills NewerGenerations per workload.
//
// Computed in Go over the already-bounded candidate set rather than as a
// window function, because the ordering rule is per WORKLOAD while the query
// is ordered globally, and a correlated subquery per row is exactly the
// amplification this file avoids.
func rankGenerations(candidates []ImageCleanupCandidate) {
	// Newest settlement first within each workload, so position is generation.
	byWorkload := map[string][]int{}
	for index, candidate := range candidates {
		byWorkload[candidate.ContainerName] = append(
			byWorkload[candidate.ContainerName], index)
	}
	for _, indexes := range byWorkload {
		// Insertion sort over a short slice: one workload's generations.
		for i := 1; i < len(indexes); i++ {
			for j := i; j > 0 && candidates[indexes[j]].SettledAt.After(
				candidates[indexes[j-1]].SettledAt); j-- {
				indexes[j], indexes[j-1] = indexes[j-1], indexes[j]
			}
		}
		for rank, index := range indexes {
			candidates[index].NewerGenerations = rank
		}
	}
}

// ImageReferences is every count the retention decision reads, for one image.
type ImageReferences struct {
	PresentContainers     int
	PreservedContainers   int
	ActiveAcquisitions    int
	ActiveExecutions      int
	ActiveRollbacks       int
	UnsettledFailures     int
	OutstandingRecoveries int
	PlanTargets           int
}

// ImageReferencesFor gathers the retention references for a set of images.
//
// SEVEN queries whatever the candidate count, each taking the image ids as bound
// parameters. A cleanup pass over two hundred candidates costs six statements,
// not twelve hundred.
//
// Every query counts a reason to KEEP. A count this cannot establish is not a
// zero: the caller treats any error as incomplete evidence and retains, so a
// failure here can only ever make cleanup do less.
func (r *ImageRetentionRepository) ImageReferencesFor(
	ctx context.Context,
	imageIDs []string,
) (map[string]ImageReferences, error) {
	references := make(map[string]ImageReferences, len(imageIDs))
	if len(imageIDs) == 0 {
		return references, nil
	}
	if len(imageIDs) > maxCleanupCandidates {
		return nil, fmt.Errorf("image reference lookup accepts at most %d images",
			maxCleanupCandidates)
	}

	ids := make([]any, 0, len(imageIDs))
	for _, id := range imageIDs {
		references[id] = ImageReferences{}
		ids = append(ids, id)
	}
	list := placeholders(len(ids))

	// Present containers, split into workloads and preserved evidence. Both
	// retain; they are counted apart so the operator-facing reason is the
	// precise one rather than "in use" for a parked original.
	//
	// Preserved is defined the way the attention projection defines it: a
	// container whose NAME an execution recorded as the parked original or the
	// quarantined replacement.
	if err := r.countInto(ctx, references, `
		SELECT c.image_id, COUNT(*)
		  FROM containers c
		 WHERE c.present = 1 AND c.image_id IN (`+list+`)
		   AND c.name NOT IN (SELECT parked_name FROM executions WHERE parked_name <> '')
		   AND c.name NOT IN (SELECT quarantine_name FROM executions WHERE quarantine_name <> '')
		 GROUP BY c.image_id`, ids,
		func(r *ImageReferences, n int) { r.PresentContainers = n }); err != nil {
		return nil, err
	}

	if err := r.countInto(ctx, references, `
		SELECT c.image_id, COUNT(*)
		  FROM containers c
		 WHERE c.present = 1 AND c.image_id IN (`+list+`)
		   AND (c.name IN (SELECT parked_name FROM executions WHERE parked_name <> '')
		     OR c.name IN (SELECT quarantine_name FROM executions WHERE quarantine_name <> ''))
		 GROUP BY c.image_id`, ids,
		func(r *ImageReferences, n int) { r.PreservedContainers = n }); err != nil {
		return nil, err
	}

	// In flight. An operation that has not finished has not decided anything,
	// so the images it names are part of an outcome that does not exist yet.
	if err := r.countInto(ctx, references, `
		SELECT acquired_image_id, COUNT(*)
		  FROM acquisitions
		 WHERE state NOT IN ('succeeded', 'failed', 'cancelled', 'expired')
		   AND acquired_image_id IN (`+list+`)
		 GROUP BY acquired_image_id`, ids,
		func(r *ImageReferences, n int) { r.ActiveAcquisitions = n }); err != nil {
		return nil, err
	}

	// An execution names TWO images -- the one it is leaving and the one it is
	// moving to -- and both are needed until it settles.
	if err := r.countInto(ctx, references, `
		SELECT image_id, COUNT(*) FROM (
		       SELECT old_image_id AS image_id FROM executions
		        WHERE state NOT IN ('succeeded', 'failed', 'cancelled', 'expired')
		          AND old_image_id IN (`+list+`)
		       UNION ALL
		       SELECT target_image_id AS image_id FROM executions
		        WHERE state NOT IN ('succeeded', 'failed', 'cancelled', 'expired')
		          AND target_image_id IN (`+list+`))
		 GROUP BY image_id`, append(append([]any{}, ids...), ids...),
		func(r *ImageReferences, n int) { r.ActiveExecutions = n }); err != nil {
		return nil, err
	}

	// A FAILED execution, whichever image it names.
	//
	// Both are kept, and the condition is the failure alone rather than any
	// judgement about how bad it was. The original is what restores service if
	// the operator decides to go back; the target is the artefact that failed
	// and the only thing that can be inspected to find out why. Neither is
	// HarborMaster's to destroy while the update it belongs to is unresolved.
	if err := r.countInto(ctx, references, `
		SELECT image_id, COUNT(*) FROM (
		       SELECT old_image_id AS image_id FROM executions
		        WHERE state = 'failed' AND old_image_id IN (`+list+`)
		       UNION ALL
		       SELECT target_image_id AS image_id FROM executions
		        WHERE state = 'failed' AND target_image_id IN (`+list+`))
		 GROUP BY image_id`, append(append([]any{}, ids...), ids...),
		func(r *ImageReferences, n int) { r.UnsettledFailures = n }); err != nil {
		return nil, err
	}

	// A rollback in flight.
	if err := r.countInto(ctx, references, `
		SELECT e.old_image_id, COUNT(*)
		  FROM rollbacks b
		  JOIN executions e ON e.execution_id = b.execution_id
		 WHERE b.state NOT IN ('succeeded', 'failed', 'cancelled', 'expired')
		   AND e.old_image_id IN (`+list+`)
		 GROUP BY e.old_image_id`, ids,
		func(r *ImageReferences, n int) { r.ActiveRollbacks = n }); err != nil {
		return nil, err
	}

	// An OUTSTANDING manual recovery, from either path.
	//
	// A non-empty recovery_plan is HarborMaster telling an operator that the
	// host is in a state it could not finish resolving by itself. Whatever that
	// note says, it is going to be carried out against the images the record
	// names -- so those images are exactly the ones cleanup must not touch.
	// The state is deliberately not narrowed: a recovery note is outstanding
	// until the record carrying it ages out.
	if err := r.countInto(ctx, references, `
		SELECT image_id, COUNT(*) FROM (
		       SELECT original_image_id AS image_id FROM rollbacks
		        WHERE recovery_plan <> '' AND original_image_id IN (`+list+`)
		       UNION ALL
		       SELECT old_image_id AS image_id FROM executions
		        WHERE recovery_plan <> '' AND old_image_id IN (`+list+`)
		       UNION ALL
		       SELECT target_image_id AS image_id FROM executions
		        WHERE recovery_plan <> '' AND target_image_id IN (`+list+`))
		 GROUP BY image_id`,
		append(append(append([]any{}, ids...), ids...), ids...),
		func(r *ImageReferences, n int) { r.OutstandingRecoveries = n }); err != nil {
		return nil, err
	}

	return references, nil
}

// countInto runs one grouped count and folds it into the reference map.
//
// The query text is a constant from this file; only the placeholder run varies,
// and it is built from a slice LENGTH. No caller value is ever concatenated.
func (r *ImageRetentionRepository) countInto(
	ctx context.Context,
	references map[string]ImageReferences,
	query string,
	args []any,
	apply func(*ImageReferences, int),
) error {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("count image references: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			imageID string
			count   int
		)
		if err := rows.Scan(&imageID, &count); err != nil {
			return fmt.Errorf("scan image reference count: %w", AsError(err))
		}
		if entry, known := references[imageID]; known {
			apply(&entry, count)
			references[imageID] = entry
		}
	}
	return rows.Err()
}

// PlanTargetImages returns the images a current plan or a spent acquisition
// still proposes.
//
// Returned as a set rather than a count: the question is only whether the
// pipeline still points at the image at all.
//
// A succeeded acquisition whose execution has not run is included, because
// removing an image somebody downloaded and has not applied yet turns a ready
// update into a failure at the moment they press the button.
func (r *ImageRetentionRepository) PlanTargetImages(
	ctx context.Context,
) (map[string]struct{}, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT acquired_image_id
		  FROM acquisitions
		 WHERE state = 'succeeded'
		   AND acquired_image_id <> ''
		   AND acquisition_id NOT IN (
		       SELECT acquisition_id FROM executions
		        WHERE state IN ('succeeded', 'failed'))
		 LIMIT ?`, maxCleanupCandidates)
	if err != nil {
		return nil, fmt.Errorf("query plan target images: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	targets := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan plan target image: %w", AsError(err))
		}
		targets[id] = struct{}{}
	}
	return targets, rows.Err()
}
