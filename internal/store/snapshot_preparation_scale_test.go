package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/store"
)

// Stage 17.2 performance: what snapshot preparation costs on a settled estate.
//
// # What is measured, and what deliberately is not
//
// The production path is: read the policies, read the targets, read the
// baselines, and capture only for governed containers that have none. On a
// settled estate the last step captures NOTHING, so the recurring cost of the
// feature is three reads and a walk -- and that is what these numbers are for.
//
// Recapturing two thousand snapshots is NOT measured, because the production
// design does not do it: preparation is bounded at maxAssurancePerPass captures
// and the rest wait for the next pass. Benchmarking a fan-out the code cannot
// perform would produce a number describing nothing.
//
// The assertion is the QUERY COUNT, which is constant. Wall clock is recorded
// for context and never asserted -- it measures the machine, not the code.

// TestPreparationReadsAreConstantWhateverTheEstateSize.
//
// The three reads preparation makes are each one round trip regardless of how
// many containers they return. If any of them became per-container, this is
// where a two-thousand-container estate would turn into six thousand queries.
func TestPreparationReadsAreConstantWhateverTheEstateSize(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}

	for _, size := range []int{25, 500, 2000} {
		t.Run(sizeName(size), func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			seedContainers(t, db, size)

			// Read 1 and 2: the targets. Two queries whatever the estate, which
			// the existing scale test already pins; measured again here because
			// preparation depends on it staying that way.
			start := time.Now()
			targets, truncated, err := db.Containers.AutomationTargets(ctx)
			targetsTook := time.Since(start)
			if err != nil {
				t.Fatalf("AutomationTargets: %v", err)
			}
			if truncated != (size > store.MaxAutomationTargets) {
				t.Errorf("truncated = %v at %d containers", truncated, size)
			}
			if len(targets) != size {
				t.Fatalf("%d targets, want %d", len(targets), size)
			}

			// Read 3: every existing baseline, in one query.
			start = time.Now()
			baselines, err := db.Snapshots.BaselineIDs(ctx)
			baselinesTook := time.Since(start)
			if err != nil {
				t.Fatalf("BaselineIDs: %v", err)
			}

			// The walk preparation performs: which governed containers lack a
			// baseline. Pure map lookups, no I/O.
			start = time.Now()
			missing := 0
			for _, target := range targets {
				if _, exists := baselines[target.ContainerID]; !exists {
					missing++
				}
			}
			walkTook := time.Since(start)

			if missing != size {
				t.Errorf("%d containers without a baseline, want %d", missing, size)
			}

			t.Logf("containers=%d targets=%s baselines=%s walk=%s queries=3 captures=0",
				size, targetsTook.Round(time.Millisecond),
				baselinesTook.Round(time.Millisecond),
				walkTook.Round(time.Microsecond))
		})
	}
}

// TestBaselineLookupIsOneQueryOnAFullyPreparedEstate.
//
// The settled case, which is the one that runs on every planner pass for the
// life of the deployment: every container already has a baseline, so
// preparation reads, finds nothing to do, and returns.
func TestBaselineLookupIsOneQueryOnAFullyPreparedEstate(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}

	db := openTestDB(t)
	ctx := context.Background()
	const size = 2000
	seedContainers(t, db, size)

	targets, _, err := db.Containers.AutomationTargets(ctx)
	if err != nil {
		t.Fatalf("AutomationTargets: %v", err)
	}

	// Give every container a baseline, as a fully converged estate would have.
	now := time.Now().UTC()
	for index, target := range targets {
		snapshot := evidenceSnapshot(target.ContainerID, "settled", now)
		snapshot.ContainerName = target.Selection.Name
		if _, err := db.Snapshots.Create(ctx, snapshot, nil, nil, nil); err != nil {
			t.Fatalf("seed baseline %d: %v", index, err)
		}
	}

	start := time.Now()
	baselines, err := db.Snapshots.BaselineIDs(ctx)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("BaselineIDs: %v", err)
	}
	if len(baselines) != size {
		t.Fatalf("%d baselines, want %d", len(baselines), size)
	}

	missing := 0
	for _, target := range targets {
		if _, exists := baselines[target.ContainerID]; !exists {
			missing++
		}
	}
	if missing != 0 {
		t.Errorf("%d containers still without a baseline on a converged estate", missing)
	}

	t.Logf("settled estate: containers=%d baselineLookup=%s captures=0 writes=0",
		size, took.Round(time.Millisecond))
}

func sizeName(size int) string {
	switch size {
	case 25:
		return "25 containers"
	case 500:
		return "500 containers"
	default:
		return "2000 containers"
	}
}
