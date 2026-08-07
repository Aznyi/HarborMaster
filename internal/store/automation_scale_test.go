package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Scale tests for the automation reads a pass performs.
//
// # What these actually pin
//
// A scheduler pass runs on a timer, unattended, and must have a computable
// worst case. Its read cost is meant to be CONSTANT in the number of
// containers: one policy query and two inventory queries, joined in memory --
// not one label query per container, which is the N+1 that turns a 200-container
// estate into 201 round trips every quarter of an hour.
//
// So the assertions below are about SHAPE rather than about milliseconds. The
// timings are generous by two orders of magnitude; what would actually fail
// them is a regression that made a read quadratic or per-container.
//
// They also exercise the bounds. MaxAutomationTargets and the policy load limit
// exist so a pathological estate degrades into "some containers were not
// examined, and the run says so" rather than into an unbounded pass.

// seedContainers commits an inventory of n containers, each with labels.
func seedContainers(t *testing.T, db *store.DB, n int) {
	t.Helper()

	records := make([]store.ContainerRecord, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("container-%06d", i)
		name := fmt.Sprintf("svc-%06d", i)

		detail := domain.ContainerDetail{
			Overview: domain.ContainerSummary{
				HostID:        domain.LocalHostID,
				ID:            id,
				ShortID:       domain.ShortenID(id),
				Name:          name,
				Image:         domain.ParseImageRef("ghcr.io/acme/service:1.2.3"),
				ImageID:       "sha256:image1",
				State:         domain.StateRunning,
				Health:        domain.HealthHealthy,
				CreatedAt:     time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
				RestartPolicy: domain.RestartPolicy{Name: "unless-stopped"},
				Present:       true,
			},
			State: domain.StateDetail{State: domain.StateRunning, RawState: "running"},
			Labels: []domain.Label{
				{Key: "tier", Value: "front", Source: domain.LabelSourceUser},
				{Key: "team", Value: "platform", Source: domain.LabelSourceUser},
				{Key: domain.LabelUpdateStrategy, Value: "digestOnly", Source: domain.LabelSourceUser},
			},
			Environment: []domain.EnvVar{},
			Mounts:      []domain.Mount{},
			Networks:    []domain.NetworkAttachment{},
			Warnings:    []domain.InventoryWarning{},
		}
		records = append(records, store.ContainerRecord{
			Detail:  detail,
			RawJSON: []byte(`{"Id":"` + id + `"}`),
		})
	}

	if _, err := db.Inventory.CommitRefresh(context.Background(), store.RefreshCommit{
		Host:       domain.Host{ID: domain.LocalHostID, Name: "local", Runtime: domain.RuntimeDocker},
		Containers: records,
		Record: domain.RefreshRecord{
			Trigger:          domain.TriggerManual,
			StartedAt:        time.Now().UTC(),
			ContainersListed: n,
			Checksum:         fmt.Sprintf("checksum-%d", n),
		},
		Now: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("commit %d containers: %v", n, err)
	}
}

func TestAutomationTargetsIsTwoQueriesForTenThousandContainers(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	db := openTestDB(t)
	ctx := context.Background()

	const containers = 10000
	seedContainers(t, db, containers)

	start := time.Now()
	targets, truncated, err := db.Containers.AutomationTargets(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("AutomationTargets: %v", err)
	}

	// Ten thousand exceeds the bound, so the estate is truncated and SAYS so
	// rather than quietly covering a prefix of the host.
	if !truncated {
		t.Fatalf("an estate of %d must report truncation past the bound of %d",
			containers, store.MaxAutomationTargets)
	}
	if len(targets) != store.MaxAutomationTargets {
		t.Fatalf("returned %d targets, want the bound of %d",
			len(targets), store.MaxAutomationTargets)
	}
	// Every target carries its labels, which is the whole reason this is not
	// one query per container.
	if got := targets[0].Selection.Labels["tier"]; got != "front" {
		t.Fatalf("labels did not come back with the target: %+v", targets[0].Selection.Labels)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("reading the estate took %s; a per-container query has crept in", elapsed)
	}
	t.Logf("read %d of %d containers with labels in %s", len(targets), containers, elapsed)
}

func TestAutomationTargetsUnderTheBoundIsNotTruncated(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	seedContainers(t, db, 500)

	targets, truncated, err := db.Containers.AutomationTargets(ctx)
	if err != nil {
		t.Fatalf("AutomationTargets: %v", err)
	}
	if truncated {
		t.Fatal("500 containers is well inside the bound")
	}
	if len(targets) != 500 {
		t.Fatalf("returned %d targets, want 500", len(targets))
	}
	for _, target := range targets {
		if len(target.Selection.Labels) != 3 {
			t.Fatalf("%s carries %d labels, want 3",
				target.Selection.Name, len(target.Selection.Labels))
		}
	}
}

func TestActivePoliciesIsBoundedAtOneThousandPolicies(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	db := openTestDB(t)
	ctx := context.Background()

	// More policies than any real estate has, and more than one pass may load.
	const policies = 1000
	for i := 0; i < policies; i++ {
		policy := newUpdatePolicy(fmt.Sprintf("policy-%04d", i))
		policy.Priority = i % 1000
		policy.Selector = domain.UpdateSelector{
			Images: []string{"ghcr.io/acme/*"},
			Labels: map[string]string{"tier": "front"},
		}
		if _, err := db.UpdatePolicies.CreateUpdatePolicy(ctx, policy, time.Now().UTC()); err != nil {
			t.Fatalf("create policy %d: %v", i, err)
		}
	}

	start := time.Now()
	active, err := db.UpdatePolicies.ActivePolicies(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ActivePolicies: %v", err)
	}

	// Bounded. A pass that loaded a thousand rules would be a pass whose cost
	// nobody had computed.
	if len(active) != 200 {
		t.Fatalf("loaded %d policies, want the bound of 200", len(active))
	}
	// Ordered for selection: highest priority first.
	for i := 1; i < len(active); i++ {
		if active[i].Priority > active[i-1].Priority {
			t.Fatalf("policies are not in priority order at %d: %d after %d",
				i, active[i].Priority, active[i-1].Priority)
		}
	}
	if elapsed > 5*time.Second {
		t.Fatalf("loading the policy set took %s", elapsed)
	}
	t.Logf("loaded %d of %d policies in %s", len(active), policies, elapsed)
}

func TestOnePassWorthOfDecisionsWritesInOneTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	db := openTestDB(t)
	ctx := context.Background()

	run := startRun(t, db, domain.AutoTriggerSchedule)

	const decisions = 2000
	batch := make([]domain.AutomationDecision, 0, decisions)
	for i := 0; i < decisions; i++ {
		batch = append(batch, domain.AutomationDecision{
			RunID:         run.RunID,
			ContainerID:   fmt.Sprintf("container-%06d", i),
			ContainerName: fmt.Sprintf("svc-%06d", i),
			Verdict:       domain.VerdictSkip,
			Reason:        domain.ReasonWindowClosed,
			Detail:        "the maintenance window is 02:00-04:00 UTC, every day",
			Position:      i,
			DecidedAt:     time.Now().UTC(),
		})
	}

	start := time.Now()
	written, err := db.Automation.RecordDecisions(ctx, batch)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RecordDecisions: %v", err)
	}
	if written != decisions {
		t.Fatalf("wrote %d decisions, want %d", written, decisions)
	}
	// One transaction rather than one per row, which is what keeps the
	// scheduler from becoming the busiest writer on a single-writer database.
	if elapsed > 10*time.Second {
		t.Fatalf("writing %d decisions took %s", decisions, elapsed)
	}
	t.Logf("wrote %d decisions in %s", decisions, elapsed)

	// And the read back is paginated rather than a full scan.
	page, total, err := db.Automation.ListDecisions(ctx, store.AutomationDecisionFilter{
		RunID: run.RunID,
		Page:  store.Page{Limit: 50},
	})
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(page) != 50 || total != decisions {
		t.Fatalf("page = %d of %d, want 50 of %d", len(page), total, decisions)
	}
}

func TestDecisionWritesAreBoundedPerPass(t *testing.T) {
	// A pathological inventory must not turn one pass into an unbounded write.
	db := openTestDB(t)
	ctx := context.Background()

	run := startRun(t, db, domain.AutoTriggerSchedule)

	const attempted = 6000
	batch := make([]domain.AutomationDecision, 0, attempted)
	for i := 0; i < attempted; i++ {
		batch = append(batch, domain.AutomationDecision{
			RunID:         run.RunID,
			ContainerName: fmt.Sprintf("svc-%06d", i),
			Verdict:       domain.VerdictSkip,
			Reason:        domain.ReasonNotSelected,
			Position:      i,
			DecidedAt:     time.Now().UTC(),
		})
	}

	written, err := db.Automation.RecordDecisions(ctx, batch)
	if err != nil {
		t.Fatalf("RecordDecisions: %v", err)
	}
	if written != 5000 {
		t.Fatalf("wrote %d decisions, want the per-pass bound of 5000", written)
	}
}
