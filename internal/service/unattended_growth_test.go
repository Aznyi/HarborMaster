package service_test

import (
	"context"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// What a quiet estate costs.
//
// An unattended HarborMaster runs a decision pass every fifteen minutes for
// years. Almost every one of those passes finds nothing to do. If a pass that
// changes nothing still writes rows, the database grows without bound on a
// system where nothing is happening -- which is the worst shape of growth,
// because the operator has no event to associate it with.
//
// So this measures. It runs many passes over an unchanged world and reports what
// each table did, then asserts on the ones that must not grow per-pass.

// tableCounts is a census of the lifecycle tables.
type tableCounts struct {
	plans        int
	acquisitions int
	executions   int
	rollbacks    int
	runs         int
	decisions    int
	dedup        int
	deliveries   int
}

func (r *unattendedRig) census() tableCounts {
	r.t.Helper()

	count := func(table string) int {
		var n int
		// The table name is a constant from the list below, never a value.
		if err := r.db.SQL().QueryRowContext(context.Background(),
			"SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			r.t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	return tableCounts{
		plans:        count("change_plans"),
		acquisitions: count("acquisitions"),
		executions:   count("executions"),
		rollbacks:    count("rollbacks"),
		runs:         count("automation_runs"),
		decisions:    count("automation_decisions"),
		dedup:        count("notification_dedup"),
		deliveries:   count("notification_deliveries"),
	}
}

func TestRepeatedPassesOverAnUnchangedWorldDoNotGrowWithoutBound(t *testing.T) {
	// Deliberately a world with NOTHING to do: no governing policy, so every
	// pass considers the container, decides against acting, and stops. That is
	// the overwhelmingly common case on a real estate and the one whose cost
	// compounds.
	rig := newUnattendedRig(t)
	defer rig.stop()

	seedDiscovery(t, rig, domain.UpdateMinor)
	rig.start()

	const passes = 20

	before := rig.census()
	for i := 0; i < passes; i++ {
		rig.decide()
	}
	after := rig.census()

	t.Logf("over %d unchanged passes: plans %d->%d, acquisitions %d->%d, "+
		"executions %d->%d, rollbacks %d->%d, runs %d->%d, decisions %d->%d, "+
		"dedup %d->%d, deliveries %d->%d",
		passes,
		before.plans, after.plans,
		before.acquisitions, after.acquisitions,
		before.executions, after.executions,
		before.rollbacks, after.rollbacks,
		before.runs, after.runs,
		before.decisions, after.decisions,
		before.dedup, after.dedup,
		before.deliveries, after.deliveries)

	// The lifecycle tables must be FLAT. A pass that changes nothing must not
	// create work, and these are the rows that represent work.
	if after.plans != before.plans {
		t.Errorf("plans grew from %d to %d over unchanged passes.\n\n"+
			"A plan is written when the assessment CHANGES. Re-writing an "+
			"identical plan on every pass would grow the table forever and "+
			"make plan history unreadable.", before.plans, after.plans)
	}
	for _, growth := range []struct {
		name          string
		before, after int
	}{
		{"acquisitions", before.acquisitions, after.acquisitions},
		{"executions", before.executions, after.executions},
		{"rollbacks", before.rollbacks, after.rollbacks},
	} {
		if growth.after != growth.before {
			t.Errorf("%s grew from %d to %d over unchanged passes; a pass that "+
				"decided to do nothing created work",
				growth.name, growth.before, growth.after)
		}
	}

	// Runs and decisions DO grow, one per pass, and that is the design: a run
	// is the record that a pass happened and a decision is its reasoning about
	// one container. "Why did you not update that container" is unanswerable
	// without them. They are bounded by AUTOMATION_RETENTION_AGE and pruned by
	// the engine's own retention loop, which is why this asserts the shape
	// rather than demanding they stay flat.
	if after.runs != before.runs+passes {
		t.Errorf("runs went %d->%d over %d passes, want one per pass",
			before.runs, after.runs, passes)
	}
	if grew := after.decisions - before.decisions; grew != passes {
		t.Errorf("decisions grew by %d over %d passes, want one per pass "+
			"(one container considered each time)", grew, passes)
	}
}

func TestRepeatedPassesAfterASettledUpdateDoNotReDoIt(t *testing.T) {
	// The other quiet case: a container that HAS been updated. The plan is
	// spent, the container is current, and the passes must find nothing.
	rig := newUnattendedRig(t, func(o *rigOptions) {
		o.policies = []domain.UpdatePolicy{c4cAutomaticPolicy()}
	})
	defer rig.stop()

	seedDiscovery(t, rig, domain.UpdateMinor)
	rig.decide()
	rig.start()
	rig.await("the update to settle", func() bool {
		return rig.executionCount() == 1 && rig.terminalExecution().State.Terminal()
	})

	// Re-discover, as production does after a recreation.
	rig.refreshInventory()
	rig.syncIntelReferences()
	rig.evaluateCompliance()
	rig.plan()

	before := rig.census()
	for i := 0; i < 20; i++ {
		rig.decide()
	}
	after := rig.census()

	t.Logf("after a settled update, 20 passes: acquisitions %d->%d, "+
		"executions %d->%d, host creates %d",
		before.acquisitions, after.acquisitions,
		before.executions, after.executions, rig.host.countOps("create:"))

	if after.acquisitions != before.acquisitions || after.executions != before.executions {
		t.Errorf("a settled container was acted on again: acquisitions %d->%d, "+
			"executions %d->%d", before.acquisitions, after.acquisitions,
			before.executions, after.executions)
	}
	if got := rig.host.countOps("create:"); got != 1 {
		t.Errorf("the host performed %d creates in total, want 1", got)
	}
}
