package service_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Producing the four lifecycle states in ONE database, for the browser to read.
//
// # Why this is a test rather than a fixture script
//
// Because the states have to be REAL. A browser check is only worth running if
// what the pages render came out of the same pipeline an operator's estate
// would drive: a success that really recreated a container, a recovery that
// really rolled one back, a review that really is waiting, and a monitor-only
// workload that really was left alone. Seeding rows by hand would test the
// renderer against a world that cannot happen.
//
// So this runs the same four scenarios the rest of this package proves, against
// the real daemon, into a database at a fixed path. The server is then started
// on that database and the pages are driven with a browser.
//
// # Why the containers are left behind
//
// The browser has to have something to look at after the Go test that made it
// has exited. Cleanup is the caller's job; the script that drives this removes
// every hm-c4c1-ui-* container afterwards.
//
// Off unless HARBORMASTER_C4C1_UI_DB names where the database should go.

func TestRealDockerSeedTheBrowserDatabase(t *testing.T) {
	skipUnlessRealDocker(t)

	dbPath := os.Getenv("HARBORMASTER_C4C1_UI_DB")
	if dbPath == "" {
		t.Skip("set HARBORMASTER_C4C1_UI_DB to seed a database for browser validation")
	}

	// A. automatic success.
	seedUISuccess(t, dbPath)
	// B. automatic failure, recovered.
	seedUIRecovered(t, dbPath)
	// C. review required, still waiting.
	seedUIReview(t, dbPath)
	// D. monitor only, untouched.
	seedUIMonitor(t, dbPath)

	t.Logf("browser database seeded at %s", dbPath)
}

func seedUISuccess(t *testing.T, dbPath string) {
	t.Helper()
	const name = "hm-c4c1-ui-success"

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.dbPath = dbPath
		o.keepWorkload = true
		o.policies = []domain.UpdatePolicy{c4c1Policy(name, domain.ModeAutomatic)}
	})

	rig.seed(domain.UpdateMinor, domain.CheckOK)
	rig.decide()
	rig.start()
	rig.await("the success to settle", func() bool {
		executions, _, err := rig.db.Executions.List(context.Background(),
			store.ExecutionFilter{Page: store.Page{Limit: 50}})
		if err != nil {
			return false
		}
		for _, execution := range executions {
			if execution.ContainerName == name &&
				execution.State == domain.ExecutionSucceeded {
				return true
			}
		}
		return false
	})

	// The inventory is refreshed afterwards so the pages describe the world as
	// it is now, not as it was before the recreation.
	rig.refreshInventory()
	rig.plan()
	rig.stop()
}

func seedUIRecovered(t *testing.T, dbPath string) {
	t.Helper()
	const name = "hm-c4c1-ui-recovered"

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.dbPath = dbPath
		o.keepWorkload = true
		o.healthCheck = c4c1VersionCheck
		o.policies = []domain.UpdatePolicy{c4c1Policy(name, domain.ModeAutomatic)}
	})

	rig.seed(domain.UpdateMinor, domain.CheckOK)
	rig.decide()
	rig.start()
	rig.await("the recovery to settle", func() bool {
		rollbacks, _, err := rig.db.Rollbacks.List(context.Background(),
			store.RollbackFilter{Page: store.Page{Limit: 50}})
		if err != nil {
			return false
		}
		for _, rollback := range rollbacks {
			if rollback.ContainerName == name &&
				rollback.State == domain.RollbackSucceeded {
				return true
			}
		}
		return false
	})

	rig.refreshInventory()
	rig.plan()
	rig.stop()
}

func seedUIReview(t *testing.T, dbPath string) {
	t.Helper()
	const name = "hm-c4c1-ui-review"

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.dbPath = dbPath
		o.keepWorkload = true
		o.policies = []domain.UpdatePolicy{c4c1Policy(name, domain.ModeApprove)}
	})

	rig.seed(domain.UpdateMinor, domain.CheckOK)
	// Several passes, so the decision is unambiguously standing and waiting.
	for i := 0; i < 3; i++ {
		rig.decide()
	}
	// Nothing was touched: the page must show a workload waiting for a person,
	// not one mid-update.
	//
	// Counted for THIS container. The database is shared with the success and
	// recovery seeders, so a total count would include their executions and say
	// nothing about this one.
	var executions int
	if err := rig.db.SQL().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM executions WHERE container_name = ?`, name,
	).Scan(&executions); err != nil {
		t.Fatalf("count executions for %s: %v", name, err)
	}
	if executions != 0 {
		t.Fatalf("the review-first workload was executed %d times", executions)
	}
	rig.stop()
}

func seedUIMonitor(t *testing.T, dbPath string) {
	t.Helper()
	const name = "hm-c4c1-ui-monitor"

	rig := newRealRig(t, func(o *realRigOptions) {
		o.name = name
		o.dbPath = dbPath
		o.keepWorkload = true
		o.policies = []domain.UpdatePolicy{c4c1Policy(name, domain.ModeObserve)}
	})

	rig.seed(domain.UpdateMinor, domain.CheckOK)
	for i := 0; i < 3; i++ {
		rig.decide()
	}
	rig.stop()
}

// unusedUISeedGuards keeps the imports honest if a seeder is edited down.
var _ = []any{service.Actor{}, time.Second}
