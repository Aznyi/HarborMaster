package store

import (
	"context"
	"fmt"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The containers a scheduler pass considers, and their labels.
//
// # Why this is one query and not one per container
//
// A pass evaluates every present container against every enabled policy. Done
// naively that is one containers query plus one labels query per container --
// the N+1 that turns a 200-container estate into 201 round trips on a timer.
// Two bounded queries, joined in memory, keep a pass's read cost constant.
//
// # Why it returns a projection
//
// domain.SelectionTarget carries a name, an image, and labels, and nothing
// else. A selector must not be able to reach configuration it has no business
// reading, and a narrow row makes the matching function pure.

// AutomationTarget is one container a pass may act on.
//
// The container ID is here and NOT on domain.SelectionTarget, deliberately: the
// selector must never match on an id, because an id changes on every recreation
// and a policy pinned to one would stop governing the container the moment it
// acted. The id is carried alongside for the plan lookup, which needs it.
type AutomationTarget struct {
	ContainerID string
	Selection   domain.SelectionTarget
}

// MaxAutomationTargets bounds one pass's container load.
//
// A pass that would consider more containers than this reports the truncation
// on its run record rather than silently examining a prefix of the estate. The
// bound exists because a pass runs on a timer and must have a computable worst
// case, not because any real host is expected to reach it.
const MaxAutomationTargets = 2000

// maxAutomationLabels bounds the label load.
//
// Labels are attacker-influenced in the sense that anyone who can run
// `docker run` writes them. Ten per container across the whole estate is
// generous; past the bound the remaining labels are dropped, which can only
// make a selector match LESS. Failing towards "not selected" is the safe
// direction: a container that should have been automated and was not is a
// missed update, while the converse would be an unintended one.
const maxAutomationLabels = MaxAutomationTargets * 10

// AutomationTargets returns every present container with its labels.
//
// Reports truncated = true when the estate exceeds MaxAutomationTargets, so the
// caller can say so on the run rather than quietly covering part of the host.
func (r *ContainerRepository) AutomationTargets(
	ctx context.Context,
) (targets []AutomationTarget, truncated bool, err error) {
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM containers WHERE present = 1`).Scan(&total); err != nil {
		return nil, false, fmt.Errorf("count automation targets: %w", AsError(err))
	}
	truncated = total > MaxAutomationTargets

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, image_ref
		  FROM containers
		 WHERE present = 1
		 ORDER BY id
		 LIMIT ?`, MaxAutomationTargets)
	if err != nil {
		return nil, false, fmt.Errorf("query automation targets: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	byID := make(map[string]int, MaxAutomationTargets)
	targets = make([]AutomationTarget, 0, 64)
	for rows.Next() {
		var target AutomationTarget
		if err := rows.Scan(&target.ContainerID, &target.Selection.Name, &target.Selection.Image); err != nil {
			return nil, false, fmt.Errorf("scan automation target: %w", AsError(err))
		}
		byID[target.ContainerID] = len(targets)
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(targets) == 0 {
		return targets, truncated, nil
	}

	// One labels query for the whole estate, restricted to present containers
	// by the same predicate rather than by an IN list of ids, so the query text
	// is fixed and carries no caller-length-dependent shape.
	labelRows, err := r.db.QueryContext(ctx, `
		SELECT l.container_id, l.key, l.value
		  FROM container_labels l
		  JOIN containers c ON c.id = l.container_id
		 WHERE c.present = 1
		 ORDER BY l.container_id, l.key
		 LIMIT ?`, maxAutomationLabels)
	if err != nil {
		return nil, false, fmt.Errorf("query automation target labels: %w", AsError(err))
	}
	defer func() { _ = labelRows.Close() }()

	for labelRows.Next() {
		var containerID, key, value string
		if err := labelRows.Scan(&containerID, &key, &value); err != nil {
			return nil, false, fmt.Errorf("scan automation target label: %w", AsError(err))
		}
		index, ok := byID[containerID]
		if !ok {
			// A label for a container past the target bound. Nothing to attach
			// it to, and nothing to report: the container itself was already
			// counted in the truncation.
			continue
		}
		if targets[index].Selection.Labels == nil {
			targets[index].Selection.Labels = make(map[string]string, 4)
		}
		targets[index].Selection.Labels[key] = value
	}
	return targets, truncated, labelRows.Err()
}
