package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Container attention evidence.
//
// # What these pin
//
// Two different kinds of property, and they matter for different reasons.
//
// The CORRECTNESS tests are about an operator being told the truth: a
// container with no plan must come back with no plan rather than an empty one,
// a preserved container must be attributed to the workload it belongs to, and
// a name that merely LOOKS like HarborMaster's must be reported as a suspicion
// rather than as a fact.
//
// The COST tests are about the Phase 10 precedent. This lookup runs on every
// container-list render, so its cost must be constant in the page size. An
// N+1 regression would not fail a correctness test -- it would return exactly
// the right answer, slowly, forever. So the shape is asserted directly:
// looking up 200 containers must not take anything like 200 times as long as
// looking up one.
//
// Following `automation_scale_test.go`, the timing bounds are generous by
// orders of magnitude. What would fail them is a per-container query, not a
// slow machine.

// seedAttentionEstate commits n containers named `svc-000000`... and returns
// their keys.
func seedAttentionEstate(t *testing.T, db *store.DB, n int) []store.ContainerKey {
	t.Helper()

	records := make([]store.ContainerRecord, 0, n)
	keys := make([]store.ContainerKey, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("container-%06d", i)
		name := fmt.Sprintf("svc-%06d", i)
		keys = append(keys, store.ContainerKey{ID: id, Name: name})

		health := domain.HealthHealthy
		if i%17 == 0 {
			health = domain.HealthUnhealthy
		}
		records = append(records, store.ContainerRecord{
			Detail: domain.ContainerDetail{
				Overview: domain.ContainerSummary{
					HostID:        domain.LocalHostID,
					ID:            id,
					ShortID:       domain.ShortenID(id),
					Name:          name,
					Image:         domain.ParseImageRef("ghcr.io/acme/service:1.2.3"),
					ImageID:       "sha256:image1",
					State:         domain.StateRunning,
					Health:        health,
					CreatedAt:     time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
					RestartPolicy: domain.RestartPolicy{Name: "unless-stopped"},
					Present:       true,
				},
				State:       domain.StateDetail{State: domain.StateRunning, RawState: "running"},
				Labels:      []domain.Label{},
				Environment: []domain.EnvVar{},
				Mounts:      []domain.Mount{},
				Networks:    []domain.NetworkAttachment{},
				Warnings:    []domain.InventoryWarning{},
			},
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
	return keys
}

// seedPlans gives every third container a current assessment.
//
// Built on `planFor`, the helper the plan tests already use, so this fixture
// cannot drift away from what the store will actually accept.
func seedPlans(t *testing.T, db *store.DB, keys []store.ContainerKey) {
	t.Helper()

	plans := make([]domain.ChangePlan, 0, len(keys)/3+1)
	for i, key := range keys {
		if i%3 != 0 {
			continue
		}
		plan := planFor(key.ID, fmt.Sprintf("%064x", i))
		plan.ContainerName = key.Name
		plans = append(plans, plan)
	}
	if _, err := db.Plans.InsertPlans(context.Background(), plans, time.Now().UTC()); err != nil {
		t.Fatalf("insert %d plans: %v", len(plans), err)
	}
}

// seedExecutionWithParkedName records an execution that parked `parked` aside
// for the workload `workload`, and returns the parked name.
//
// The point of going through the real repository rather than an INSERT: the
// exclusion and attribution must key off the name HarborMaster ACTUALLY
// writes, and a hand-written row could disagree with it silently.
func seedExecutionWithParkedName(t *testing.T, db *store.DB, workload, parked string) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	execution := domain.Execution{
		ExecutionID:   domain.NewExecutionID(),
		AcquisitionID: "acq_0011223344556677889a",
		PlanID:        "plan_00112233445566778899",
		SnapshotID:    7,
		ContainerID:   "container-web",
		ContainerName: workload,
		OldImage:      "nginx:1.27.0",
		Target: domain.ExecutionTarget{
			Registry:   "docker.io",
			Repository: "library/nginx",
			Digest:     "sha256:" + strings.Repeat("a", 64),
			Reference:  "nginx:1.27.1",
			Platform:   domain.Platform{OS: "linux", Architecture: "amd64"},
		},
		State:       domain.ExecutionQueued,
		RequestedAt: now,
		ExpiresAt:   now.Add(15 * time.Minute),
		PlanDigest:  strings.Repeat("f", 64),
	}
	created, err := db.Executions.Create(ctx, execution, now)
	if err != nil {
		t.Fatalf("create execution: %v", err)
	}
	if _, err := db.Executions.Advance(ctx, store.ExecutionChange{
		ExecutionID: created.ExecutionID,
		To:          domain.ExecutionCreating,
		ParkedName:  parked,
	}, now); err != nil {
		t.Fatalf("record the parked name: %v", err)
	}
	return parked
}

// ---------------------------------------------------------- correctness --

func TestAttentionReportsAbsentEvidenceAsAbsent(t *testing.T) {
	// The property the whole model rests on. A container nothing has looked at
	// must come back with PlanKnown false, so the assessment says "not
	// checked" rather than "up to date".
	db := openTestDB(t)
	keys := seedAttentionEstate(t, db, 3)

	evidence, err := db.Containers.Attention(context.Background(), keys)
	if err != nil {
		t.Fatalf("Attention: %v", err)
	}
	if len(evidence) != 3 {
		t.Fatalf("got evidence for %d containers, want 3", len(evidence))
	}
	for _, key := range keys {
		row := evidence[key.ID]
		if row.PlanKnown || row.LineageKnown {
			t.Fatalf("%s: nothing has been recorded, yet %+v", key.Name, row)
		}
		if domain.AssessContainer(row).State != domain.AttentionNotChecked {
			t.Fatalf("%s: an unexamined container is not 'not checked'", key.Name)
		}
	}
}

func TestAttentionReadsTheCurrentPlanOnly(t *testing.T) {
	// A container accumulates an assessment per planner pass. The row must
	// report the standing one; reporting a superseded plan would show an
	// operator an update that was withdrawn.
	db := openTestDB(t)
	ctx := context.Background()
	keys := seedAttentionEstate(t, db, 1)

	older := planFor(keys[0].ID, fmt.Sprintf("%064d", 1))
	older.ContainerName = keys[0].Name
	older.ProposedImage = "ghcr.io/acme/service:9.9.9"
	older.UpdateType = domain.UpdateMajor

	newer := planFor(keys[0].ID, fmt.Sprintf("%064d", 2))
	newer.ContainerName = keys[0].Name
	newer.ProposedImage = "ghcr.io/acme/service:1.2.4"
	newer.UpdateType = domain.UpdatePatch

	for _, plan := range []domain.ChangePlan{older, newer} {
		if _, err := db.Plans.InsertPlans(ctx, []domain.ChangePlan{plan}, time.Now().UTC()); err != nil {
			t.Fatalf("insert plan: %v", err)
		}
	}

	evidence, err := db.Containers.Attention(ctx, keys)
	if err != nil {
		t.Fatalf("Attention: %v", err)
	}
	row := evidence[keys[0].ID]
	if !row.PlanKnown {
		t.Fatal("the assessment was not found at all")
	}
	if row.UpdateType != domain.UpdatePatch ||
		row.ProposedImage != "ghcr.io/acme/service:1.2.4" {
		t.Fatalf("a superseded plan is being reported: %+v", row)
	}
}

func TestAttentionRefusesMoreContainersThanItCanBound(t *testing.T) {
	// A partial answer here would render checked containers as unchecked,
	// which is the one direction this model must never fail in. So the lookup
	// refuses rather than truncating.
	db := openTestDB(t)

	keys := make([]store.ContainerKey, 501)
	for i := range keys {
		keys[i] = store.ContainerKey{ID: fmt.Sprintf("c-%d", i), Name: fmt.Sprintf("n-%d", i)}
	}

	if _, err := db.Containers.Attention(context.Background(), keys); err == nil {
		t.Fatal("an over-long lookup must be refused, not truncated")
	}
}

func TestAttentionNamesTheWorkloadAPreservedContainerBelongsTo(t *testing.T) {
	// The audit finding: parked recovery containers appeared as ordinary
	// workload rows with no explanation. Attribution comes from HarborMaster's
	// OWN execution record, not from the shape of the name.
	db := openTestDB(t)
	ctx := context.Background()

	execution := seedExecutionWithParkedName(t, db, "web", "web.hm-old-exec_00112233445566778899")

	keys := []store.ContainerKey{
		{ID: "container-parked", Name: execution},
		{ID: "container-web", Name: "web"},
	}
	evidence, err := db.Containers.Attention(ctx, keys)
	if err != nil {
		t.Fatalf("Attention: %v", err)
	}

	parked := evidence["container-parked"]
	if parked.Preserved != domain.PreservedOriginal {
		t.Fatalf("preserved kind = %q, want %q", parked.Preserved, domain.PreservedOriginal)
	}
	if parked.PreservedFor != "web" {
		t.Fatalf("preserved for %q, want %q", parked.PreservedFor, "web")
	}
	if evidence["container-web"].Preserved != domain.PreservedNone {
		t.Fatal("the workload itself must not be marked preserved")
	}
}

func TestAnUnrecordedLookalikeIsOnlySuspected(t *testing.T) {
	// A container an operator named this way themselves. HarborMaster says so
	// rather than claiming it parked it -- and, because the kind is not
	// evidenced, the default listing may not hide it.
	db := openTestDB(t)

	keys := []store.ContainerKey{
		{ID: "container-lookalike", Name: "web.hm-old-exec_ffffffffffffffffffff"},
	}
	evidence, err := db.Containers.Attention(context.Background(), keys)
	if err != nil {
		t.Fatalf("Attention: %v", err)
	}

	row := evidence["container-lookalike"]
	if row.Preserved != domain.PreservedSuspected {
		t.Fatalf("preserved kind = %q, want %q", row.Preserved, domain.PreservedSuspected)
	}
	if row.Preserved.Evidenced() {
		t.Fatal("a name-only match must never count as evidence")
	}
	if row.PreservedFor != "" {
		t.Fatal("a suspicion cannot name a workload it was never recorded against")
	}
}

func TestTheDefaultListingHidesOnlyRecordedPreservedContainers(t *testing.T) {
	// The exclusion is by RECORD. A container HarborMaster wrote down parking
	// disappears from the default workload view; one that merely wears the
	// same name shape does not, because an operator who named it that way
	// themselves would otherwise lose it from their own inventory with no way
	// to find out why.
	db := openTestDB(t)
	ctx := context.Background()

	recorded := "web.hm-old-exec_00112233445566778899"
	lookalike := "api.hm-old-exec_ffffffffffffffffffff"
	commitNamedContainers(t, db, "web", recorded, lookalike)
	seedExecutionWithParkedName(t, db, "web", recorded)

	names := func(filter store.ContainerFilter) []string {
		t.Helper()
		filter.Sort, filter.Direction = "name", store.SortAsc
		filter.Page = store.Page{Limit: 50}
		page, _, err := db.Containers.List(ctx, filter)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		out := make([]string, 0, len(page))
		for _, summary := range page {
			out = append(out, summary.Name)
		}
		return out
	}

	def := names(store.ContainerFilter{ExcludePreserved: true})
	if contains(def, recorded) {
		t.Fatalf("the recorded parked container is still in the default view: %v", def)
	}
	if !contains(def, lookalike) {
		t.Fatalf("an unrecorded lookalike was hidden: %v", def)
	}
	if !contains(def, "web") {
		t.Fatalf("the workload itself vanished: %v", def)
	}

	// And it is one checkbox away, never deleted.
	all := names(store.ContainerFilter{})
	if !contains(all, recorded) {
		t.Fatalf("the preserved container is unreachable: %v", all)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// commitNamedContainers commits an inventory of the given names, with ids
// derived from them.
func commitNamedContainers(t *testing.T, db *store.DB, names ...string) {
	t.Helper()

	records := make([]store.ContainerRecord, 0, len(names))
	for i, name := range names {
		id := fmt.Sprintf("container-%02d", i)
		records = append(records, store.ContainerRecord{
			Detail: domain.ContainerDetail{
				Overview: domain.ContainerSummary{
					HostID:        domain.LocalHostID,
					ID:            id,
					ShortID:       domain.ShortenID(id),
					Name:          name,
					Image:         domain.ParseImageRef("ghcr.io/acme/service:1.2.3"),
					State:         domain.StateRunning,
					Health:        domain.HealthHealthy,
					CreatedAt:     time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
					RestartPolicy: domain.RestartPolicy{Name: "unless-stopped"},
					Present:       true,
				},
				State:       domain.StateDetail{State: domain.StateRunning, RawState: "running"},
				Labels:      []domain.Label{},
				Environment: []domain.EnvVar{},
				Mounts:      []domain.Mount{},
				Networks:    []domain.NetworkAttachment{},
				Warnings:    []domain.InventoryWarning{},
			},
			RawJSON: []byte(`{"Id":"` + id + `"}`),
		})
	}
	if _, err := db.Inventory.CommitRefresh(context.Background(), store.RefreshCommit{
		Host:       domain.Host{ID: domain.LocalHostID, Name: "local", Runtime: domain.RuntimeDocker},
		Containers: records,
		Record: domain.RefreshRecord{
			Trigger:          domain.TriggerManual,
			StartedAt:        time.Now().UTC(),
			ContainersListed: len(names),
			Checksum:         "checksum-named",
		},
		Now: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("commit named containers: %v", err)
	}
}

// ----------------------------------------------------------------- cost --

func TestAttentionCostDoesNotGrowWithPageSize(t *testing.T) {
	// The Phase 10 property, restated for a read-heavy page: this lookup runs
	// a FIXED number of queries whatever the page size. An N+1 implementation
	// would pass every correctness test above and fail this one.
	if testing.Short() {
		t.Skip("scale test")
	}
	db := openTestDB(t)
	ctx := context.Background()

	const estate = 1000
	keys := seedAttentionEstate(t, db, estate)
	seedPlans(t, db, keys)

	measure := func(page []store.ContainerKey) time.Duration {
		t.Helper()
		// One warm pass, then the measured one: the first call pays for query
		// planning that a real server pays once at startup.
		if _, err := db.Containers.Attention(ctx, page); err != nil {
			t.Fatalf("Attention(%d): %v", len(page), err)
		}
		start := time.Now()
		if _, err := db.Containers.Attention(ctx, page); err != nil {
			t.Fatalf("Attention(%d): %v", len(page), err)
		}
		return time.Since(start)
	}

	one := measure(keys[:1])
	twentyFive := measure(keys[:25])
	twoHundred := measure(keys[:200])

	t.Logf("attention lookup over a %d-container estate: 1 row %s, 25 rows %s, 200 rows %s",
		estate, one, twentyFive, twoHundred)

	// Seven queries for one row and seven for two hundred. The per-row work
	// that remains is scanning the returned rows, which is why this is a
	// generous multiple rather than equality -- but nothing like the 200x an
	// N+1 would cost.
	floor := one
	if floor < time.Millisecond {
		floor = time.Millisecond
	}
	if twoHundred > 20*floor {
		t.Fatalf("200 rows took %s against %s for one row; a per-container query has crept in",
			twoHundred, one)
	}
	// And the absolute cost stays inside anything a page render can afford.
	if twoHundred > 2*time.Second {
		t.Fatalf("a 200-row attention lookup took %s", twoHundred)
	}
}

func TestListingALargeEstateWithAttentionStaysBounded(t *testing.T) {
	// The whole read a container-list request performs: the page, the count,
	// and the attention lookup. Measured end to end so the reported number is
	// the one an operator's browser waits for.
	if testing.Short() {
		t.Skip("scale test")
	}
	db := openTestDB(t)
	ctx := context.Background()

	const estate = 1000
	keys := seedAttentionEstate(t, db, estate)
	seedPlans(t, db, keys)

	start := time.Now()
	page, total, err := db.Containers.List(ctx, store.ContainerFilter{
		Sort: "name", Direction: store.SortAsc,
		Page: store.Page{Limit: 25, Offset: 0},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	pageKeys := make([]store.ContainerKey, 0, len(page))
	for _, summary := range page {
		pageKeys = append(pageKeys, store.ContainerKey{ID: summary.ID, Name: summary.Name})
	}
	evidence, err := db.Containers.Attention(ctx, pageKeys)
	if err != nil {
		t.Fatalf("Attention: %v", err)
	}
	elapsed := time.Since(start)

	if total != estate {
		t.Fatalf("total = %d, want %d", total, estate)
	}
	// Pagination is intact: the browser holds one page, never the estate.
	if len(page) != 25 || len(evidence) != 25 {
		t.Fatalf("page held %d rows and %d evidence entries, want 25 of each",
			len(page), len(evidence))
	}
	t.Logf("list page of 25 with attention, over %d containers: %s", estate, elapsed)

	if elapsed > 2*time.Second {
		t.Fatalf("a single list page took %s over %d containers", elapsed, estate)
	}
}
