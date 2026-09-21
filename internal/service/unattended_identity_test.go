package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// After a successful replacement, nothing may go on treating the OLD container
// id as the workload's current identity. A stale id in any repository is how a
// later operation stops, inspects, or plans against a container that no longer
// exists -- or, worse, against whatever the daemon reuses the name for.

func TestAfterASuccessfulUpdateNoRecordTreatsTheOldContainerAsCurrent(t *testing.T) {
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
	})
	defer rig.stop()

	seedDiscovery(t, rig, domain.UpdateMinor)
	if run, _ := rig.decide(); run.Submitted != 1 {
		t.Fatalf("submitted = %d, want 1", run.Submitted)
	}
	rig.start()
	rig.await("the recreation to settle and the original to be removed", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 10}})
		return err == nil && len(executions) == 1 &&
			executions[0].State == domain.ExecutionSucceeded && executions[0].OriginalRemoved
	})

	execution := rig.terminalExecution()
	replacementID := execution.ReplacementID
	if replacementID == "" || replacementID == c4cContainerID {
		t.Fatalf("the execution records replacement %q", replacementID)
	}

	// The inventory, after the refresh that follows any update.
	rig.refreshInventory()
	ctx := context.Background()

	// 1. The container repository: the old id is absent, the new one is the
	// present container under the production name.
	if old, err := rig.db.Containers.Get(ctx, c4cContainerID); err == nil && old != nil && old.Overview.Present {
		t.Errorf("the container repository still reports the old id %s as present", domain.ShortenID(c4cContainerID))
	}
	current, err := rig.db.Containers.Get(ctx, replacementID)
	if err != nil || current == nil || !current.Overview.Present {
		t.Fatalf("the container repository does not report the replacement as present: %v", err)
	}
	if domain.NormaliseContainerName(current.Overview.Name) != c4cName {
		t.Errorf("the replacement is recorded under %q, want %q", current.Overview.Name, c4cName)
	}

	// 2. Plans: no current plan may name the old id.
	if _, err := rig.db.Plans.Current(ctx, c4cContainerID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a current plan still names the old container id (err=%v)", err)
	}

	// 3. Lineage follows the workload by NAME and observed the new id.
	lineage, err := rig.db.Lineage.Get(ctx, c4cName)
	if err != nil {
		t.Fatalf("lineage: %v", err)
	}
	if lineage.ContainerID != replacementID {
		t.Errorf("lineage observes container %s, want the replacement %s",
			domain.ShortenID(lineage.ContainerID), domain.ShortenID(replacementID))
	}

	// 4. The execution record resolves the old id to the new one for anything
	// that captured the old id into its configuration.
	resolved, err := rig.db.Executions.ReplacementFor(ctx, c4cContainerID)
	if err != nil || resolved != replacementID {
		t.Errorf("ReplacementFor(old) = %q, %v; want %q", resolved, err, replacementID)
	}

	// 5. And no active work names the old id.
	if active, _ := rig.db.Executions.ActiveForContainer(ctx, c4cContainerID); active {
		t.Error("an execution is still active for the old container id")
	}
}
