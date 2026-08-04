package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// --------------------------------------------------------------- fixtures --

type eventOpt func(*domain.DockerEvent)

// buildEvent produces a realistic Docker event. Options keep each test stating
// only the thing it is actually about.
func buildEvent(fingerprint string, opts ...eventOpt) domain.DockerEvent {
	at := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)

	event := domain.DockerEvent{
		Fingerprint:      fingerprint,
		HostID:           domain.LocalHostID,
		Type:             domain.EventTypeContainer,
		Action:           domain.ActionStart,
		ActorID:          "b8f1c0d2e3a4b5c6d7e8f9a0b1c2d3e4",
		ActorName:        "shop-web-1",
		Scope:            "local",
		Attributes:       map[string]string{"name": "shop-web-1"},
		ComposeProject:   "shop",
		ComposeService:   "web",
		DockerTime:       at,
		DockerTimeNano:   at.UnixNano(),
		ObservedAt:       at,
		CreatedAt:        at,
		Result:           domain.ResultProcessed,
		RefreshRequested: domain.RefreshContainer,
		ConnectionState:  domain.ConnStateConnected,
	}
	for _, opt := range opts {
		opt(&event)
	}
	return event
}

func withEventType(eventType domain.DockerEventType) eventOpt {
	return func(e *domain.DockerEvent) { e.Type = eventType }
}

func withAction(action domain.DockerEventAction) eventOpt {
	return func(e *domain.DockerEvent) { e.Action = action }
}

func withResult(result domain.EventProcessingResult) eventOpt {
	return func(e *domain.DockerEvent) { e.Result = result }
}

func withObservedAt(at time.Time) eventOpt {
	return func(e *domain.DockerEvent) {
		e.ObservedAt = at
		e.DockerTime = at
		e.DockerTimeNano = at.UnixNano()
		e.CreatedAt = at
	}
}

func withActor(id, name string) eventOpt {
	return func(e *domain.DockerEvent) {
		e.ActorID = id
		e.ActorName = name
		e.Attributes = map[string]string{"name": name}
	}
}

func withProject(project, svc string) eventOpt {
	return func(e *domain.DockerEvent) {
		e.ComposeProject = project
		e.ComposeService = svc
	}
}

func appendEvents(t *testing.T, db *store.DB, events ...domain.DockerEvent) []domain.DockerEvent {
	t.Helper()
	stored, _, err := db.DockerEvents.Append(context.Background(), events)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return stored
}

// ------------------------------------------------------------- migration --

// The event tables must exist after migration, and the widened refresh trigger
// must accept 'reconcile' -- the whole reason 0003 rebuilds that table.
func TestMigrationCreatesEventTables(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, table := range []string{"docker_events", "event_engine_state"} {
		var name string
		err := db.SQL().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %s missing after migration: %v", table, err)
		}
	}

	names, err := store.MigrationNames()
	if err != nil {
		t.Fatalf("MigrationNames: %v", err)
	}
	// Lexical order is apply order, so 0003 must sort after 0002.
	if len(names) < 3 || names[2] != "0003_events.sql" {
		t.Fatalf("migrations = %v, want 0003_events.sql applied third", names)
	}
}

func TestMigrationWidensTheRefreshTrigger(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Reconcile is the trigger Phase 2.5 added; 0002's CHECK would reject it.
	if _, err := commitInventory(ctx, db, domain.TriggerReconcile); err != nil {
		t.Fatalf("a reconcile-triggered refresh must be storable: %v", err)
	}
}

// commitInventory persists a minimal successful refresh so targeted writes and
// generation-dependent queries have something to join.
func commitInventory(ctx context.Context, db *store.DB, trigger domain.RefreshTrigger) (domain.RefreshRecord, error) {
	return db.Inventory.CommitRefresh(ctx, store.RefreshCommit{
		Host:   domain.Host{ID: domain.LocalHostID, Name: "local", Runtime: domain.RuntimeDocker},
		Record: domain.RefreshRecord{Trigger: trigger, StartedAt: time.Now().UTC()},
		Now:    time.Now().UTC(),
	})
}

// ------------------------------------------------------------- insertion --

func TestAppendAssignsMonotonicSequences(t *testing.T) {
	db := openTestDB(t)

	stored := appendEvents(t, db,
		buildEvent("fp-1"), buildEvent("fp-2"), buildEvent("fp-3"))

	if len(stored) != 3 {
		t.Fatalf("stored %d events, want 3", len(stored))
	}
	for i := 1; i < len(stored); i++ {
		if stored[i].Sequence <= stored[i-1].Sequence {
			t.Fatalf("sequences must increase: %d then %d",
				stored[i-1].Sequence, stored[i].Sequence)
		}
	}
}

// The unique fingerprint constraint is the last line of defence behind the
// in-memory window: a duplicate arriving after a restart must still be rejected.
func TestAppendRejectsDuplicateFingerprints(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	appendEvents(t, db, buildEvent("fp-dup"))

	stored, duplicates, err := db.DockerEvents.Append(ctx,
		[]domain.DockerEvent{buildEvent("fp-dup"), buildEvent("fp-new")})
	if err != nil {
		t.Fatalf("a duplicate must not fail the batch: %v", err)
	}
	if duplicates != 1 {
		t.Errorf("duplicates = %d, want 1", duplicates)
	}
	if len(stored) != 1 || stored[0].Fingerprint != "fp-new" {
		t.Errorf("stored = %v, want only the new event", stored)
	}

	count, err := db.DockerEvents.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestAppendFillsDefaults(t *testing.T) {
	db := openTestDB(t)

	// A bare event: no host, scope, result, or timestamps. The repository is
	// the last point before the CHECK constraints, so it must fill these in.
	stored := appendEvents(t, db, domain.DockerEvent{
		Fingerprint: "fp-bare",
		Action:      domain.ActionStart,
	})

	if len(stored) != 1 {
		t.Fatalf("stored %d events, want 1", len(stored))
	}
	event := stored[0]
	if event.HostID != domain.LocalHostID {
		t.Errorf("hostId = %q, want %q", event.HostID, domain.LocalHostID)
	}
	if event.Type != domain.EventTypeOther {
		t.Errorf("type = %q, want other", event.Type)
	}
	if event.Result != domain.ResultProcessed {
		t.Errorf("result = %q, want processed", event.Result)
	}
	if event.ObservedAt.IsZero() {
		t.Error("observedAt must be filled in")
	}
}

// A batch is all-or-nothing, so a partial burst never appears in the history.
func TestAppendRollsBackOnFailure(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// An event type outside the CHECK constraint fails the insert. Everything
	// in the same batch must be rolled back with it.
	_, _, err := db.DockerEvents.Append(ctx, []domain.DockerEvent{
		buildEvent("fp-ok"),
		buildEvent("fp-bad", withEventType(domain.DockerEventType("not-a-type"))),
	})
	if err == nil {
		t.Fatal("an invalid event type must fail the batch")
	}

	count, err := db.DockerEvents.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0; a failed batch must leave nothing behind", count)
	}
}

func TestAppendEmptyBatchIsANoOp(t *testing.T) {
	db := openTestDB(t)

	stored, duplicates, err := db.DockerEvents.Append(context.Background(), nil)
	if err != nil || len(stored) != 0 || duplicates != 0 {
		t.Fatalf("empty batch = (%v, %d, %v), want no work and no error", stored, duplicates, err)
	}
}

// ---------------------------------------------------------------- reading --

func TestGetReturnsOneEvent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	stored := appendEvents(t, db, buildEvent("fp-get"))

	event, err := db.DockerEvents.Get(ctx, stored[0].Sequence)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if event.Fingerprint != "fp-get" {
		t.Errorf("fingerprint = %q, want fp-get", event.Fingerprint)
	}
	if event.Attributes["name"] != "shop-web-1" {
		t.Errorf("attributes did not round-trip: %v", event.Attributes)
	}
}

func TestGetUnknownSequenceIsNotFound(t *testing.T) {
	db := openTestDB(t)

	_, err := db.DockerEvents.Get(context.Background(), 99999)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestListPaginatesNewestFirst(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := range 10 {
		appendEvents(t, db, buildEvent(fmt.Sprintf("fp-%02d", i)))
	}

	first, total, err := db.DockerEvents.List(ctx, store.DockerEventFilter{
		Sort: "sequence", Direction: store.SortDesc,
		Page: store.Page{Limit: 4, Offset: 0},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 10 {
		t.Errorf("total = %d, want 10", total)
	}
	if len(first) != 4 {
		t.Fatalf("page held %d events, want 4", len(first))
	}
	if first[0].Fingerprint != "fp-09" {
		t.Errorf("first event = %q, want the newest (fp-09)", first[0].Fingerprint)
	}

	second, _, err := db.DockerEvents.List(ctx, store.DockerEventFilter{
		Sort: "sequence", Direction: store.SortDesc,
		Page: store.Page{Limit: 4, Offset: 4},
	})
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	// Pages must not overlap: that is what the deterministic ordering buys.
	for _, a := range first {
		for _, b := range second {
			if a.Sequence == b.Sequence {
				t.Fatalf("sequence %d appeared on two pages", a.Sequence)
			}
		}
	}
}

// A burst of events shares a Docker timestamp, so the local sequence tiebreak
// is what stops paging repeating or skipping a row.
func TestListOrderingIsTotalWhenTheSortFieldTies(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	at := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	for i := range 6 {
		appendEvents(t, db, buildEvent(fmt.Sprintf("fp-tie-%d", i), withObservedAt(at)))
	}

	seen := map[int64]bool{}
	for offset := 0; offset < 6; offset += 2 {
		page, _, err := db.DockerEvents.List(ctx, store.DockerEventFilter{
			Sort: "dockerTime", Direction: store.SortDesc,
			Page: store.Page{Limit: 2, Offset: offset},
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, event := range page {
			if seen[event.Sequence] {
				t.Fatalf("sequence %d repeated across pages despite tied timestamps", event.Sequence)
			}
			seen[event.Sequence] = true
		}
	}
	if len(seen) != 6 {
		t.Errorf("saw %d distinct events across all pages, want 6", len(seen))
	}
}

func TestListFilters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	appendEvents(t, db,
		buildEvent("fp-c1", withActor("aaa111", "web")),
		buildEvent("fp-c2", withAction(domain.ActionDie), withActor("aaa111", "web")),
		buildEvent("fp-i1", withEventType(domain.EventTypeImage), withAction(domain.ActionPull),
			withActor("sha256:deadbeef", "nginx:1.27")),
		buildEvent("fp-v1", withEventType(domain.EventTypeVolume), withAction(domain.ActionCreate),
			withActor("data", "data"), withProject("", "")),
		buildEvent("fp-w1", withResult(domain.ResultWarning), withActor("bbb222", "api"),
			withProject("other", "api")),
	)

	tests := []struct {
		name   string
		filter store.DockerEventFilter
		want   int
	}{
		{"by type", store.DockerEventFilter{Types: []domain.DockerEventType{domain.EventTypeImage}}, 1},
		{"by two types", store.DockerEventFilter{Types: []domain.DockerEventType{
			domain.EventTypeImage, domain.EventTypeVolume}}, 2},
		{"by action", store.DockerEventFilter{Actions: []string{"die"}}, 1},
		{"by actor prefix", store.DockerEventFilter{ActorID: "aaa"}, 2},
		{"by project", store.DockerEventFilter{ComposeProject: "shop"}, 3},
		{"by service", store.DockerEventFilter{ComposeService: "api"}, 1},
		{"by result", store.DockerEventFilter{Results: []domain.EventProcessingResult{domain.ResultWarning}}, 1},
		{"by name search", store.DockerEventFilter{Search: "nginx"}, 1},
		{"by actor search", store.DockerEventFilter{Search: "bbb222"}, 1},
		{"no match", store.DockerEventFilter{ComposeProject: "nonexistent"}, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			filter := tc.filter
			filter.Page = store.Page{Limit: 50}
			events, total, err := db.DockerEvents.List(ctx, filter)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if total != tc.want || len(events) != tc.want {
				t.Errorf("got %d events (total %d), want %d", len(events), total, tc.want)
			}
		})
	}
}

func TestListFiltersByTimeRange(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	for i := range 5 {
		appendEvents(t, db, buildEvent(fmt.Sprintf("fp-t%d", i),
			withObservedAt(base.Add(time.Duration(i)*time.Hour))))
	}

	since := base.Add(time.Hour)
	until := base.Add(3 * time.Hour)

	events, total, err := db.DockerEvents.List(ctx, store.DockerEventFilter{
		Since: &since, Until: &until, Page: store.Page{Limit: 50},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Inclusive on both ends: hours 1, 2, and 3.
	if total != 3 || len(events) != 3 {
		t.Errorf("got %d events (total %d), want 3", len(events), total)
	}
}

// -------------------------------------------------------------- replay --

func TestSinceReturnsOldestFirstForReplay(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	stored := appendEvents(t, db,
		buildEvent("fp-r1"), buildEvent("fp-r2"), buildEvent("fp-r3"), buildEvent("fp-r4"))

	events, total, err := db.DockerEvents.Since(ctx, stored[1].Sequence, 10)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if total != 2 || len(events) != 2 {
		t.Fatalf("got %d events (total %d), want 2", len(events), total)
	}
	// Replay must be in the order the client would have received it live.
	if events[0].Fingerprint != "fp-r3" || events[1].Fingerprint != "fp-r4" {
		t.Errorf("replay = %q then %q, want fp-r3 then fp-r4",
			events[0].Fingerprint, events[1].Fingerprint)
	}
}

// The total tells the handler its replay was capped, so it can warn the client
// rather than hand it a silent hole.
func TestSinceReportsTruncation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := range 10 {
		appendEvents(t, db, buildEvent(fmt.Sprintf("fp-cap-%02d", i)))
	}

	events, total, err := db.DockerEvents.Since(ctx, 0, 3)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(events) != 3 {
		t.Errorf("returned %d events, want the 3 asked for", len(events))
	}
	if total != 10 {
		t.Errorf("total = %d, want 10 so truncation is detectable", total)
	}
}

func TestSinceWithZeroLimitReturnsNothing(t *testing.T) {
	db := openTestDB(t)
	appendEvents(t, db, buildEvent("fp-x"))

	events, _, err := db.DockerEvents.Since(context.Background(), 0, 0)
	if err != nil || len(events) != 0 {
		t.Fatalf("Since with limit 0 = (%v, %v), want nothing", events, err)
	}
}

// ------------------------------------------------------------ retention --

func TestPruneByAge(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	appendEvents(t, db,
		buildEvent("fp-old-1", withObservedAt(now.Add(-48*time.Hour))),
		buildEvent("fp-old-2", withObservedAt(now.Add(-30*time.Hour))),
		buildEvent("fp-new-1", withObservedAt(now.Add(-1*time.Hour))),
	)

	removed, err := db.DockerEvents.Prune(ctx, store.PruneOptions{
		MaxAge: 24 * time.Hour,
		Now:    now,
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d events, want 2", removed)
	}

	count, _ := db.DockerEvents.Count(ctx)
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestPruneByCount(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := range 10 {
		appendEvents(t, db, buildEvent(fmt.Sprintf("fp-n%02d", i)))
	}

	removed, err := db.DockerEvents.Prune(ctx, store.PruneOptions{MaxCount: 4})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 6 {
		t.Errorf("removed %d events, want 6", removed)
	}

	events, _, err := db.DockerEvents.List(ctx, store.DockerEventFilter{
		Sort: "sequence", Direction: store.SortAsc, Page: store.Page{Limit: 50},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("kept %d events, want 4", len(events))
	}
	// The OLDEST are removed, so the newest survive.
	if events[0].Fingerprint != "fp-n06" {
		t.Errorf("oldest surviving = %q, want fp-n06", events[0].Fingerprint)
	}
}

// Pruning must run in bounded batches so a large backlog does not hold one
// enormous write lock. A batch size well below the backlog exercises the loop.
func TestPruneRunsInBoundedBatches(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := range 25 {
		appendEvents(t, db, buildEvent(fmt.Sprintf("fp-b%02d", i)))
	}

	removed, err := db.DockerEvents.Prune(ctx, store.PruneOptions{
		MaxCount:  5,
		BatchSize: 3,
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 20 {
		t.Errorf("removed %d events, want 20 across several batches", removed)
	}

	count, _ := db.DockerEvents.Count(ctx)
	if count != 5 {
		t.Errorf("count = %d, want 5", count)
	}
}

// Retention operates on the observational log ONLY. Current inventory must be
// untouchable from here.
func TestPruneNeverTouchesInventory(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.Inventory.CommitRefresh(ctx, store.RefreshCommit{
		Host: domain.Host{ID: domain.LocalHostID, Name: "local", Runtime: domain.RuntimeDocker},
		Containers: []store.ContainerRecord{
			{Detail: buildContainer("cafe1234567890ab", "keeper")},
		},
		Record: domain.RefreshRecord{Trigger: domain.TriggerManual, StartedAt: time.Now().UTC()},
		Now:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CommitRefresh: %v", err)
	}

	appendEvents(t, db, buildEvent("fp-prune-me", withObservedAt(time.Now().UTC().Add(-72*time.Hour))))

	if _, err := db.DockerEvents.Prune(ctx, store.PruneOptions{
		MaxAge: time.Hour, MaxCount: 1, Now: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	_, total, err := db.Containers.List(ctx, store.ContainerFilter{Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatalf("container List: %v", err)
	}
	if total != 1 {
		t.Fatalf("containers = %d, want 1; retention must never remove inventory", total)
	}
}

func TestPruneWithNoPolicyIsANoOp(t *testing.T) {
	db := openTestDB(t)
	appendEvents(t, db, buildEvent("fp-keep"))

	removed, err := db.DockerEvents.Prune(context.Background(), store.PruneOptions{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed %d events, want 0 when no policy is configured", removed)
	}
}

func TestPruneStopsOnCancellation(t *testing.T) {
	db := openTestDB(t)

	for i := range 10 {
		appendEvents(t, db, buildEvent(fmt.Sprintf("fp-c%02d", i)))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A cancelled context must stop the batch loop rather than run to
	// completion, so a shutdown mid-prune returns promptly.
	if _, err := db.DockerEvents.Prune(ctx, store.PruneOptions{
		MaxCount: 1, BatchSize: 1,
	}); err == nil {
		t.Fatal("Prune must report cancellation")
	}
}

// ---------------------------------------------------------- engine state --

func TestEngineStateRoundTrips(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	connected := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	disconnected := connected.Add(time.Hour)

	if err := db.DockerEvents.SaveState(ctx, domain.LocalHostID, store.EngineState{
		LastConnectedAt:    &connected,
		LastDisconnectedAt: &disconnected,
		ReconnectCount:     7,
		LastError:          "docker engine unreachable",
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	state, err := db.DockerEvents.LoadState(ctx, domain.LocalHostID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.LastConnectedAt == nil || !state.LastConnectedAt.Equal(connected) {
		t.Errorf("lastConnectedAt = %v, want %v", state.LastConnectedAt, connected)
	}
	if state.ReconnectCount != 7 {
		t.Errorf("reconnectCount = %d, want 7", state.ReconnectCount)
	}
	if state.LastError != "docker engine unreachable" {
		t.Errorf("lastError = %q", state.LastError)
	}
}

// A host that has never run the engine is not an error: it is a fresh install.
func TestLoadStateForAnUnknownHostIsEmpty(t *testing.T) {
	db := openTestDB(t)

	state, err := db.DockerEvents.LoadState(context.Background(), "never-seen")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.LastConnectedAt != nil || state.ReconnectCount != 0 {
		t.Errorf("state = %+v, want zero", state)
	}
}

func TestSaveStateUpserts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, count := range []int64{1, 2, 3} {
		if err := db.DockerEvents.SaveState(ctx, domain.LocalHostID,
			store.EngineState{ReconnectCount: count}); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
	}

	state, err := db.DockerEvents.LoadState(ctx, domain.LocalHostID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.ReconnectCount != 3 {
		t.Errorf("reconnectCount = %d, want the last written value", state.ReconnectCount)
	}
}

// -------------------------------------------------------------- filters --

func TestDistinctEventVocabularies(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	appendEvents(t, db,
		buildEvent("fp-d1", withProject("shop", "web")),
		buildEvent("fp-d2", withProject("shop", "api"), withAction(domain.ActionDie)),
		buildEvent("fp-d3", withProject("blog", "web"), withAction(domain.ActionStart)),
	)

	projects, err := db.DockerEvents.DistinctEventProjects(ctx)
	if err != nil {
		t.Fatalf("DistinctEventProjects: %v", err)
	}
	if len(projects) != 2 || projects[0] != "blog" || projects[1] != "shop" {
		t.Errorf("projects = %v, want [blog shop]", projects)
	}

	actions, err := db.DockerEvents.DistinctEventActions(ctx)
	if err != nil {
		t.Fatalf("DistinctEventActions: %v", err)
	}
	if len(actions) != 2 {
		t.Errorf("actions = %v, want die and start", actions)
	}
}

func TestEventSortFieldAllowlist(t *testing.T) {
	if store.ValidEventSortField("sequence") != true {
		t.Error("sequence must be sortable")
	}
	// Nothing outside the allowlist may reach the query builder.
	for _, field := range []string{"", "attributes", "1; DROP TABLE docker_events"} {
		if store.ValidEventSortField(field) {
			t.Errorf("%q must not be an accepted sort field", field)
		}
	}
	if len(store.EventSortFields()) == 0 {
		t.Error("EventSortFields must report the allowlist")
	}
}
