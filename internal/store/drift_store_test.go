package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Drift persistence tests.
//
// The properties that matter here are about IDENTITY and LIFECYCLE: the same
// difference seen twice must be one record, a difference that goes away must
// resolve, an operator's status must survive re-evaluation, and no secret may
// ever reach a column.

// driftFixture returns a database with one container and one baseline snapshot.
func driftFixture(t *testing.T) (*store.DB, domain.Snapshot) {
	t.Helper()

	db := openTestDB(t)
	snapshot := createSnapshot(t, db, newSnapshot("container-a", checksumA))
	return db, snapshot
}

// driftRecord builds a record for the fixture's container.
func driftRecord(snapshot domain.Snapshot, category domain.DriftCategory, field string) domain.DriftRecord {
	return domain.DriftRecord{
		ContainerID:   "container-a",
		ContainerName: "web",
		SnapshotID:    snapshot.ID,
		Category:      category,
		Field:         field,
		Kind:          domain.ChangeModified,
		Severity:      domain.DriftSeverityHigh,
		PreviousValue: "before",
		CurrentValue:  "after",
		Status:        domain.DriftStatusActive,
		Reason:        "a setting changed",
	}
}

func evaluationFor(snapshot domain.Snapshot, count int) domain.DriftEvaluation {
	return domain.DriftEvaluation{
		ContainerID:   "container-a",
		ContainerName: "web",
		SnapshotID:    snapshot.ID,
		DriftCount:    count,
		Complete:      true,
	}
}

func TestDriftRecordRoundTrips(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	record := driftRecord(snapshot, domain.DriftCategorySecurity, "privileged")
	record.Severity = domain.DriftSeverityCritical

	result, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
		[]domain.DriftRecord{record}, now)
	if err != nil {
		t.Fatalf("ReconcileDrift: %v", err)
	}
	if result.Inserted != 1 {
		t.Errorf("inserted = %d, want 1", result.Inserted)
	}

	records, total, err := db.Drift.List(ctx, store.DriftFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 || len(records) != 1 {
		t.Fatalf("total = %d, len = %d, want 1 and 1", total, len(records))
	}

	got := records[0]
	if got.Field != "privileged" || got.Severity != domain.DriftSeverityCritical {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if got.Status != domain.DriftStatusActive {
		t.Errorf("status = %q, want active", got.Status)
	}
	if got.PreviousValue != "before" || got.CurrentValue != "after" {
		t.Errorf("values did not survive: %q -> %q", got.PreviousValue, got.CurrentValue)
	}
	if got.DetectedAt.IsZero() || got.DetectedAt.Location() != time.UTC {
		t.Errorf("DetectedAt = %v, want a non-zero UTC time", got.DetectedAt)
	}
}

// The same difference seen again is ONE record, not two. This is what bounds
// table growth under an event storm.
func TestRepeatedEvaluationDoesNotDuplicate(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()
	start := time.Now().UTC()

	record := driftRecord(snapshot, domain.DriftCategorySecurity, "privileged")

	for i := range 25 {
		at := start.Add(time.Duration(i) * time.Minute)
		if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
			[]domain.DriftRecord{record}, at); err != nil {
			t.Fatalf("evaluation %d: %v", i, err)
		}
	}

	records, total, err := db.Drift.List(ctx, store.DriftFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d after 25 evaluations, want 1", total)
	}

	// detected_at names when the drift STARTED and must not move; last_seen_at
	// must track the most recent evaluation.
	got := records[0]
	if !got.DetectedAt.Equal(start.Truncate(0)) && got.DetectedAt.After(start.Add(time.Minute)) {
		t.Errorf("DetectedAt = %v, want it pinned near the first evaluation at %v", got.DetectedAt, start)
	}
	if !got.LastSeenAt.After(got.DetectedAt) {
		t.Errorf("LastSeenAt %v must advance past DetectedAt %v", got.LastSeenAt, got.DetectedAt)
	}
}

// A difference that stops appearing is resolved rather than deleted: the
// history of what was true is the point of the record.
func TestVanishedDriftIsResolved(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	privileged := driftRecord(snapshot, domain.DriftCategorySecurity, "privileged")
	labels := driftRecord(snapshot, domain.DriftCategoryLabels, "com.example.owner")

	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 2),
		[]domain.DriftRecord{privileged, labels}, now); err != nil {
		t.Fatalf("first evaluation: %v", err)
	}

	// The second evaluation sees only the label difference.
	result, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
		[]domain.DriftRecord{labels}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second evaluation: %v", err)
	}
	if result.Resolved != 1 {
		t.Errorf("resolved = %d, want 1", result.Resolved)
	}

	all, _, err := db.Drift.List(ctx, store.DriftFilter{OpenOnly: false})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d, want both records retained", len(all))
	}

	for _, record := range all {
		switch record.Field {
		case "privileged":
			if record.Status != domain.DriftStatusResolved {
				t.Errorf("privileged status = %q, want resolved", record.Status)
			}
			if record.ResolvedAt == nil {
				t.Error("a resolved record must carry resolvedAt")
			}
		case "com.example.owner":
			if record.Status != domain.DriftStatusActive {
				t.Errorf("label status = %q, want active", record.Status)
			}
		}
	}

	// The default listing hides resolved history.
	open, total, err := db.Drift.List(ctx, store.DriftFilter{OpenOnly: true})
	if err != nil {
		t.Fatalf("List open: %v", err)
	}
	if total != 1 || len(open) != 1 || open[0].Field != "com.example.owner" {
		t.Errorf("open listing = %d records, want just the label drift", total)
	}
}

// AN INCOMPLETE EVALUATION RESOLVES NOTHING.
//
// The most important property in this file. A truncated comparison did not
// establish that the fields it never reached still match; resolving on that
// basis would silently clear real drift, which is the worst failure this
// feature can have.
func TestIncompleteEvaluationResolvesNothing(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	privileged := driftRecord(snapshot, domain.DriftCategorySecurity, "privileged")
	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
		[]domain.DriftRecord{privileged}, now); err != nil {
		t.Fatalf("seed: %v", err)
	}

	incomplete := evaluationFor(snapshot, 0)
	incomplete.Complete = false
	incomplete.Reason = "the comparison exceeded its size budget"

	result, err := db.Drift.ReconcileDrift(ctx, incomplete, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("incomplete evaluation: %v", err)
	}
	if result.Resolved != 0 {
		t.Errorf("resolved = %d; an incomplete evaluation must resolve nothing", result.Resolved)
	}

	records, _, err := db.Drift.List(ctx, store.DriftFilter{OpenOnly: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 || records[0].Status != domain.DriftStatusActive {
		t.Error("the drift must still be active after an incomplete evaluation")
	}
}

// An operator's status survives re-evaluation. Without this, ignoring a
// difference would last until the next event.
func TestOperatorStatusSurvivesReEvaluation(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	record := driftRecord(snapshot, domain.DriftCategoryLabels, "com.example.owner")
	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
		[]domain.DriftRecord{record}, now); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stored, _, err := db.Drift.List(ctx, store.DriftFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := db.Drift.UpdateStatus(ctx, stored[0].ID,
		domain.DriftStatusIgnored, "known and intended", now); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	// The same difference is seen again, with a changed value.
	record.CurrentValue = "changed-again"
	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
		[]domain.DriftRecord{record}, now.Add(time.Minute)); err != nil {
		t.Fatalf("re-evaluation: %v", err)
	}

	after, err := db.Drift.Get(ctx, stored[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Status != domain.DriftStatusIgnored {
		t.Errorf("status = %q, want ignored; an operator's decision must survive re-evaluation", after.Status)
	}
	if after.Note != "known and intended" {
		t.Errorf("note = %q, want it preserved", after.Note)
	}
	// The VALUES do refresh, so an operator sees what it is now.
	if after.CurrentValue != "changed-again" {
		t.Errorf("currentValue = %q, want the refreshed value", after.CurrentValue)
	}
}

// A resolved difference that comes back returns to active. That is
// engine-owned news about the world rather than an operator's opinion.
func TestResolvedDriftReactivatesWhenItReturns(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	record := driftRecord(snapshot, domain.DriftCategorySecurity, "privileged")

	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
		[]domain.DriftRecord{record}, now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 0),
		nil, now.Add(time.Minute)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
		[]domain.DriftRecord{record}, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("reappear: %v", err)
	}

	records, total, err := db.Drift.List(ctx, store.DriftFilter{OpenOnly: false})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want the same record reused rather than a second one", total)
	}
	if records[0].Status != domain.DriftStatusActive {
		t.Errorf("status = %q, want active", records[0].Status)
	}
	if records[0].ResolvedAt != nil {
		t.Error("resolvedAt must be cleared when the drift returns")
	}
}

// ---------------------------------------------------------- secret safety --

// A sensitive record must reach the database with no values, and the CHECK
// constraint must refuse one that does.
func TestSensitiveDriftCarriesNoValues(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()

	const secret = "hunter2-SUPER-SECRET"

	record := driftRecord(snapshot, domain.DriftCategoryEnvironment, "DB_PASSWORD")
	record.Sensitive = true
	// The caller tries to supply values anyway. The repository blanks them.
	record.PreviousValue = secret
	record.CurrentValue = secret

	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
		[]domain.DriftRecord{record}, time.Now().UTC()); err != nil {
		t.Fatalf("ReconcileDrift: %v", err)
	}

	records, _, err := db.Drift.List(ctx, store.DriftFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := records[0]
	if got.PreviousValue != "" || got.CurrentValue != "" {
		t.Errorf("a sensitive record carries values: %q / %q", got.PreviousValue, got.CurrentValue)
	}
	if !got.Sensitive {
		t.Error("the sensitive flag must survive, so a UI can explain the withheld values")
	}

	// And nothing resembling the secret is anywhere in the database.
	assertDatabaseHasNoValue(t, db, secret)
}

// The schema-level backstop: a direct INSERT carrying a value on a sensitive
// row must be rejected. This is the layer that survives a bug in the layers
// above it.
func TestCheckConstraintRefusesASensitiveValue(t *testing.T) {
	db, snapshot := driftFixture(t)

	_, err := db.SQL().Exec(`
		INSERT INTO drift_records
			(container_id, container_name, snapshot_id, detected_at, last_seen_at,
			 category, field, kind, severity, previous_value, current_value,
			 sensitive, status, reason, created_at, updated_at)
		VALUES ('container-a', 'web', ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z',
		        'environment', 'DB_PASSWORD', 'modified', 'medium', 'hunter2', '',
		        1, 'active', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		snapshot.ID)
	if err == nil {
		t.Fatal("the CHECK constraint allowed a value on a sensitive drift record")
	}
}

// The positive control for the sweep above: it must be able to find a value
// that IS present, or the leak test passes vacuously forever.
func TestDatabaseSweepDetectsAValueThatIsPresent(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()

	const marker = "PLAINTEXT-MARKER-4471"
	record := driftRecord(snapshot, domain.DriftCategoryLabels, "com.example.marker")
	record.CurrentValue = marker

	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
		[]domain.DriftRecord{record}, time.Now().UTC()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if !databaseContains(t, db, marker) {
		t.Fatal("the sweep cannot find a value that is definitely stored; " +
			"TestSensitiveDriftCarriesNoValues would pass vacuously")
	}
}

// assertDatabaseHasNoValue sweeps every text column of the drift tables.
func assertDatabaseHasNoValue(t *testing.T, db *store.DB, value string) {
	t.Helper()
	if databaseContains(t, db, value) {
		t.Errorf("the value %q reached the database", value)
	}
}

// databaseContains reports whether any text column in the drift tables holds
// the value.
//
// The table and column names come from sqlite_master, which is schema-derived
// and not caller input; the VALUE travels as a bound parameter.
func databaseContains(t *testing.T, db *store.DB, value string) bool {
	t.Helper()

	for _, table := range []string{"drift_records", "drift_evaluations"} {
		rows, err := db.SQL().Query(
			`SELECT name FROM pragma_table_info(?) WHERE type IN ('TEXT', 'BLOB')`, table)
		if err != nil {
			t.Fatalf("read columns of %s: %v", table, err)
		}

		columns := make([]string, 0, 16)
		for rows.Next() {
			var column string
			if err := rows.Scan(&column); err != nil {
				_ = rows.Close()
				t.Fatalf("scan column: %v", err)
			}
			columns = append(columns, column)
		}
		_ = rows.Close()

		for _, column := range columns {
			var count int
			// The identifier is schema-derived; the value is bound.
			query := fmt.Sprintf(
				`SELECT COUNT(*) FROM %s WHERE CAST(%s AS TEXT) LIKE ?`, table, column)
			if err := db.SQL().QueryRow(query, "%"+value+"%").Scan(&count); err != nil {
				t.Fatalf("scan %s.%s: %v", table, column, err)
			}
			if count > 0 {
				return true
			}
		}
	}
	return false
}

// ------------------------------------------------------ filtering, paging --

func TestDriftFilteringAndPagination(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	records := []domain.DriftRecord{
		withSeverity(driftRecord(snapshot, domain.DriftCategorySecurity, "privileged"), domain.DriftSeverityCritical),
		withSeverity(driftRecord(snapshot, domain.DriftCategorySecurity, "capAdd"), domain.DriftSeverityCritical),
		withSeverity(driftRecord(snapshot, domain.DriftCategoryLabels, "owner"), domain.DriftSeverityLow),
		withSeverity(driftRecord(snapshot, domain.DriftCategoryLabels, "team"), domain.DriftSeverityLow),
		withSeverity(driftRecord(snapshot, domain.DriftCategoryPorts, "80/tcp"), domain.DriftSeverityHigh),
	}
	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, len(records)), records, now); err != nil {
		t.Fatalf("seed: %v", err)
	}

	t.Run("by category", func(t *testing.T) {
		got, total, err := db.Drift.List(ctx, store.DriftFilter{
			Categories: []domain.DriftCategory{domain.DriftCategorySecurity},
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 2 || len(got) != 2 {
			t.Errorf("total = %d, want 2", total)
		}
	})

	t.Run("by severity", func(t *testing.T) {
		_, total, err := db.Drift.List(ctx, store.DriftFilter{
			Severities: []domain.DriftSeverity{domain.DriftSeverityCritical, domain.DriftSeverityHigh},
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 3 {
			t.Errorf("total = %d, want 3", total)
		}
	})

	t.Run("pagination reports the whole match", func(t *testing.T) {
		got, total, err := db.Drift.List(ctx, store.DriftFilter{
			Page: store.Page{Limit: 2},
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("len = %d, want 2", len(got))
		}
		// The total is the whole match, not the page, or pagination controls
		// would render wrongly.
		if total != 5 {
			t.Errorf("total = %d, want 5", total)
		}
	})

	t.Run("severity sorts by rank not alphabet", func(t *testing.T) {
		got, _, err := db.Drift.List(ctx, store.DriftFilter{Sort: "severity"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) == 0 || got[0].Severity != domain.DriftSeverityCritical {
			t.Errorf("first = %q, want critical; alphabetically 'critical' < 'high' < 'low' would order wrongly",
				got[0].Severity)
		}
		// low must come last, which alphabetical ordering would not produce.
		if got[len(got)-1].Severity != domain.DriftSeverityLow {
			t.Errorf("last = %q, want low", got[len(got)-1].Severity)
		}
	})

	t.Run("an unknown sort field falls back safely", func(t *testing.T) {
		// The allowlist rejects it before it reaches SQL; the repository
		// defaults rather than interpolating.
		if store.ValidDriftSortField("id); DROP TABLE drift_records; --") {
			t.Fatal("an injection payload was accepted as a sort field")
		}
		got, _, err := db.Drift.List(ctx, store.DriftFilter{Sort: "id); DROP TABLE drift_records; --"})
		if err != nil {
			t.Fatalf("List with a bogus sort must not error: %v", err)
		}
		if len(got) != 5 {
			t.Errorf("len = %d, want the default ordering to have served all 5", len(got))
		}
		// The table survived.
		if _, _, err := db.Drift.List(ctx, store.DriftFilter{}); err != nil {
			t.Fatalf("the table did not survive: %v", err)
		}
	})
}

func withSeverity(record domain.DriftRecord, severity domain.DriftSeverity) domain.DriftRecord {
	record.Severity = severity
	return record
}

// ----------------------------------------------------------------- summary --

func TestDriftSummaryAggregates(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	records := []domain.DriftRecord{
		withSeverity(driftRecord(snapshot, domain.DriftCategorySecurity, "privileged"), domain.DriftSeverityCritical),
		withSeverity(driftRecord(snapshot, domain.DriftCategoryLabels, "owner"), domain.DriftSeverityLow),
	}
	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 2), records, now); err != nil {
		t.Fatalf("seed: %v", err)
	}

	summary, err := db.Drift.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}

	if summary.Total != 2 || summary.Open != 2 {
		t.Errorf("total = %d, open = %d, want 2 and 2", summary.Total, summary.Open)
	}
	if summary.BySeverity[domain.DriftSeverityCritical] != 1 {
		t.Errorf("critical = %d, want 1", summary.BySeverity[domain.DriftSeverityCritical])
	}
	if summary.ByCategory[domain.DriftCategorySecurity] != 1 {
		t.Errorf("security = %d, want 1", summary.ByCategory[domain.DriftCategorySecurity])
	}
	if summary.ContainersWithDrift != 1 {
		t.Errorf("containersWithDrift = %d, want 1", summary.ContainersWithDrift)
	}
	if summary.ContainersEvaluated != 1 {
		t.Errorf("containersEvaluated = %d, want 1", summary.ContainersEvaluated)
	}
	if summary.LastEvaluatedAt == nil {
		t.Error("lastEvaluatedAt must be set once something has been evaluated")
	}
	if summary.Incomplete {
		t.Error("a complete evaluation must not mark the summary incomplete")
	}
}

// An incomplete evaluation is surfaced in the summary. A dashboard that hid it
// would read as "these are all the differences", which is exactly wrong.
func TestSummaryReportsIncompleteEvaluations(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()

	incomplete := evaluationFor(snapshot, 0)
	incomplete.Complete = false
	incomplete.Reason = "no baseline snapshot exists for this container"

	if _, err := db.Drift.ReconcileDrift(ctx, incomplete, nil, time.Now().UTC()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	summary, err := db.Drift.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if !summary.Incomplete {
		t.Error("the summary must report that an evaluation was incomplete")
	}
	if summary.ContainersEvaluated != 1 {
		t.Errorf("containersEvaluated = %d, want 1; an attempt counts even when it found nothing",
			summary.ContainersEvaluated)
	}
}

// "No drift" and "never checked" must stay distinguishable.
func TestEvaluationDistinguishesCleanFromNeverChecked(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()

	if _, err := db.Drift.Evaluation(ctx, "container-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an unevaluated container must report ErrNotFound, got %v", err)
	}

	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 0), nil, time.Now().UTC()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	evaluation, err := db.Drift.Evaluation(ctx, "container-a")
	if err != nil {
		t.Fatalf("Evaluation: %v", err)
	}
	if evaluation.DriftCount != 0 || !evaluation.Complete {
		t.Errorf("evaluation = %+v, want a complete evaluation with no drift", evaluation)
	}
}

// --------------------------------------------------------------- lifecycle --

func TestUpdateStatusRejectsAMissingRecord(t *testing.T) {
	db, _ := driftFixture(t)

	_, err := db.Drift.UpdateStatus(context.Background(), 9999,
		domain.DriftStatusIgnored, "", time.Now().UTC())
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want store.ErrNotFound", err)
	}
}

// Resolved history is prunable; open records never are. An unreviewed
// difference does not become less true with age.
func TestPruneRemovesOnlyResolvedHistory(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-90 * 24 * time.Hour)

	stale := driftRecord(snapshot, domain.DriftCategoryLabels, "stale")
	active := driftRecord(snapshot, domain.DriftCategorySecurity, "privileged")

	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 2),
		[]domain.DriftRecord{stale, active}, old); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Resolve the label drift, at the old timestamp.
	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
		[]domain.DriftRecord{active}, old); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	removed, err := db.Drift.PruneResolved(ctx, time.Now().UTC().Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatalf("PruneResolved: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	remaining, total, err := db.Drift.List(ctx, store.DriftFilter{OpenOnly: false})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 || remaining[0].Field != "privileged" {
		t.Error("pruning must keep the open record and remove only resolved history")
	}
}

// A drift record measured against a pruned snapshot is meaningless, so the
// foreign key cascades.
func TestDriftCascadesWhenItsBaselineIsDeleted(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()

	record := driftRecord(snapshot, domain.DriftCategorySecurity, "privileged")
	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
		[]domain.DriftRecord{record}, time.Now().UTC()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := db.SQL().Exec(`DELETE FROM snapshots WHERE id = ?`, snapshot.ID); err != nil {
		t.Fatalf("delete snapshot: %v", err)
	}

	_, total, err := db.Drift.List(ctx, store.DriftFilter{OpenOnly: false})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d; drift measured against a deleted baseline must cascade", total)
	}
}

// ------------------------------------------------------------ concurrency --

// Concurrent evaluations of DIFFERENT containers must not corrupt anything or
// deadlock. Run under -race in CI.
func TestConcurrentEvaluationsAreSafe(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	const containers = 8
	snapshots := make([]domain.Snapshot, containers)
	for i := range containers {
		snapshots[i] = createSnapshot(t, db,
			newSnapshot(fmt.Sprintf("container-%02d", i), checksumA[:62]+padHex(i)))
	}

	var group sync.WaitGroup
	errCh := make(chan error, containers)

	for i := range containers {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			snapshot := snapshots[index]
			containerID := fmt.Sprintf("container-%02d", index)

			for round := range 10 {
				record := domain.DriftRecord{
					ContainerID:   containerID,
					ContainerName: containerID,
					SnapshotID:    snapshot.ID,
					Category:      domain.DriftCategorySecurity,
					Field:         "privileged",
					Kind:          domain.ChangeModified,
					Severity:      domain.DriftSeverityCritical,
					PreviousValue: "false",
					CurrentValue:  "true",
					Status:        domain.DriftStatusActive,
				}
				evaluation := domain.DriftEvaluation{
					ContainerID:   containerID,
					ContainerName: containerID,
					SnapshotID:    snapshot.ID,
					DriftCount:    1,
					Complete:      true,
				}
				if _, err := db.Drift.ReconcileDrift(ctx, evaluation,
					[]domain.DriftRecord{record}, now.Add(time.Duration(round)*time.Second)); err != nil {
					errCh <- err
					return
				}
			}
		}(i)
	}

	group.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent evaluation failed: %v", err)
	}

	// One record per container, not one per round.
	_, total, err := db.Drift.List(ctx, store.DriftFilter{OpenOnly: false})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != containers {
		t.Errorf("total = %d, want %d; the identity index must collapse repeats", total, containers)
	}
}

// A large estate must stay practical: the summary is what a dashboard polls,
// so it must not become O(records).
func TestLargeDriftSetRemainsQueryable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	const (
		containers        = 100
		recordsPerHost    = 20
		expectedRecordSum = containers * recordsPerHost
	)

	for i := range containers {
		containerID := fmt.Sprintf("container-%03d", i)
		snapshot := createSnapshot(t, db, newSnapshot(containerID, checksumA[:60]+padHex(i/16)+padHex(i%16)))

		records := make([]domain.DriftRecord, 0, recordsPerHost)
		for j := range recordsPerHost {
			records = append(records, domain.DriftRecord{
				ContainerID:   containerID,
				ContainerName: containerID,
				SnapshotID:    snapshot.ID,
				Category:      domain.DriftCategories[j%len(domain.DriftCategories)],
				Field:         fmt.Sprintf("field-%02d", j),
				Kind:          domain.ChangeModified,
				Severity:      domain.DriftSeverities[j%len(domain.DriftSeverities)],
				Status:        domain.DriftStatusActive,
			})
		}
		if _, err := db.Drift.ReconcileDrift(ctx, domain.DriftEvaluation{
			ContainerID: containerID, ContainerName: containerID,
			SnapshotID: snapshot.ID, DriftCount: len(records), Complete: true,
		}, records, now); err != nil {
			t.Fatalf("seed %s: %v", containerID, err)
		}
	}

	started := time.Now()
	summary, err := db.Drift.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	summaryTook := time.Since(started)

	if summary.Total != expectedRecordSum {
		t.Errorf("total = %d, want %d", summary.Total, expectedRecordSum)
	}
	if summary.ContainersWithDrift != containers {
		t.Errorf("containersWithDrift = %d, want %d", summary.ContainersWithDrift, containers)
	}
	t.Logf("summary over %d records in %s", expectedRecordSum, summaryTook.Round(time.Microsecond))

	started = time.Now()
	page, total, err := db.Drift.List(ctx, store.DriftFilter{
		Severities: []domain.DriftSeverity{domain.DriftSeverityCritical},
		Page:       store.Page{Limit: 25},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	t.Logf("filtered page of %d (of %d) in %s", len(page), total, time.Since(started).Round(time.Microsecond))

	if len(page) != 25 {
		t.Errorf("page = %d, want 25", len(page))
	}
}

// A note must round-trip intact; the API bounds and validates it before it
// arrives here.
func TestStatusNoteRoundTrips(t *testing.T) {
	db, snapshot := driftFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	record := driftRecord(snapshot, domain.DriftCategoryLabels, "owner")
	if _, err := db.Drift.ReconcileDrift(ctx, evaluationFor(snapshot, 1),
		[]domain.DriftRecord{record}, now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stored, _, err := db.Drift.List(ctx, store.DriftFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	const note = "expected until the 2026-Q1 migration completes"
	updated, err := db.Drift.UpdateStatus(ctx, stored[0].ID, domain.DriftStatusExpected, note, now)
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if updated.Note != note {
		t.Errorf("note = %q, want %q", updated.Note, note)
	}
	if updated.StatusChangedAt == nil {
		t.Error("statusChangedAt must be set")
	}
	if updated.Status != domain.DriftStatusExpected {
		t.Errorf("status = %q, want expected", updated.Status)
	}
	if strings.TrimSpace(updated.Reason) == "" {
		t.Error("the engine's reason must survive a status change")
	}
}
