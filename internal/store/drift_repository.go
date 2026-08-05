package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// DriftRepository owns persistence for configuration drift.
//
// # No duplicated snapshot data
//
// A drift row references its baseline by id and stores nothing the snapshot
// already holds. Rendering a record with its baseline's details is a join at
// read time, never a copy at write time: a snapshot is immutable evidence, and
// a second copy of evidence is a second thing to keep in sync.
//
// # Bounded reads
//
// Every list is paginated, every sort field comes from an allowlist, and the
// summary is ONE aggregate query rather than a list that the caller counts.
// The summary is the endpoint a dashboard polls, so it is the one that must
// not become O(records).
type DriftRepository struct {
	db *sql.DB
}

// driftSortFields is the allowlist of sortable columns.
//
// An allowlist rather than validation-by-rejection: the value becomes part of
// the SQL text, so the only safe design is one where caller input SELECTS from
// a fixed set of literals rather than contributing to it. `severity` maps to a
// CASE expression built from domain constants, because alphabetical ordering of
// the severity words would put critical after high.
var driftSortFields = map[string]string{
	"detectedAt": "detected_at",
	"lastSeenAt": "last_seen_at",
	"id":         "id",
	"container":  "container_name",
	"category":   "category",
	"field":      "field",
	"status":     "status",
	"severity":   driftSeverityRankSQL,
}

// driftSeverityRankSQL orders by how much a record matters rather than by the
// spelling of its severity. Built from literals only.
const driftSeverityRankSQL = `CASE severity ` +
	`WHEN 'critical' THEN 4 ` +
	`WHEN 'high' THEN 3 ` +
	`WHEN 'medium' THEN 2 ` +
	`WHEN 'low' THEN 1 ` +
	`ELSE 0 END`

// ValidDriftSortField reports whether field names a sortable column.
func ValidDriftSortField(field string) bool {
	_, ok := driftSortFields[field]
	return ok
}

// DriftFilter narrows a drift listing.
//
// Every field is a closed vocabulary or an exact identifier validated by the
// API layer before it arrives. None of them becomes SQL text.
type DriftFilter struct {
	ContainerID string
	SnapshotID  int64
	Categories  []domain.DriftCategory
	Severities  []domain.DriftSeverity
	Statuses    []domain.DriftStatus
	// OpenOnly restricts to records that are not resolved. It is the default
	// the dashboard uses: a list dominated by resolved history would bury the
	// differences that still stand.
	OpenOnly bool

	Sort      string
	Ascending bool
	Page      Page
}

const selectDriftColumns = `
	SELECT id, container_id, container_name, snapshot_id,
	       detected_at, last_seen_at, resolved_at, inventory_generation,
	       category, field, kind, severity,
	       previous_value, current_value, sensitive,
	       status, reason, note, status_changed_at
	FROM drift_records`

// UpsertResult reports what one evaluation changed, for logging and metrics.
type UpsertResult struct {
	Inserted int
	Updated  int
	Resolved int
}

// ReconcileDrift persists one container's evaluation in a single transaction.
//
// # Why one transaction
//
// An evaluation is a statement about a container at one moment: these fields
// differ, and no others do. Writing the differences and the resolutions
// separately would leave a window where a reader sees both the new drift and
// the stale records the same evaluation just disproved -- a dashboard showing
// a difference that the very same comparison established was gone.
//
// # Status is preserved
//
// An operator's acknowledged, ignored or expected SURVIVES re-evaluation. Only
// the values, timestamps and severity are refreshed. A record that had been
// resolved and is seen again returns to active, because the world changed back
// and that is engine-owned news rather than an operator's opinion.
func (r *DriftRepository) ReconcileDrift(
	ctx context.Context,
	evaluation domain.DriftEvaluation,
	records []domain.DriftRecord,
	now time.Time,
) (UpsertResult, error) {
	var result UpsertResult
	stamp := formatTime(now.UTC())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("begin drift transaction: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	seen := make([]string, 0, len(records))
	for _, record := range records {
		inserted, err := upsertDriftRecord(ctx, tx, record, stamp)
		if err != nil {
			return result, err
		}
		if inserted {
			result.Inserted++
		} else {
			result.Updated++
		}
		seen = append(seen, driftIdentityKey(record))
	}

	resolved, err := resolveVanishedDrift(ctx, tx, evaluation, seen, stamp)
	if err != nil {
		return result, err
	}
	result.Resolved = resolved

	if err := recordEvaluation(ctx, tx, evaluation, stamp); err != nil {
		return result, err
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit drift: %w", AsError(err))
	}
	return result, nil
}

// upsertDriftRecord writes one difference, preserving operator intent.
//
// Reports whether the row was newly inserted, which is what distinguishes new
// drift from drift that is merely still there.
func upsertDriftRecord(ctx context.Context, tx *sql.Tx, record domain.DriftRecord, stamp string) (bool, error) {
	// Defence in depth, layer three of four. The diff engine already withheld
	// these, and a CHECK constraint would reject them, but blanking here means
	// a value cannot reach the statement at all.
	previous, current := record.PreviousValue, record.CurrentValue
	if record.Sensitive {
		previous, current = "", ""
	}

	sensitive := 0
	if record.Sensitive {
		sensitive = 1
	}

	// ON CONFLICT over the identity index. detected_at is deliberately NOT
	// updated: the age of a drift record is the age of the drift, and moving
	// it on every evaluation would make everything look like it started just
	// now.
	//
	// The status expression is the operator-intent rule in SQL: a resolved
	// record seen again returns to active; every other status is left alone.
	//
	// RETURNING created_at tells insert from update in the SAME statement. A
	// follow-up SELECT would be a second round trip per differing field, which
	// on a container with fifty differences is fifty avoidable queries inside
	// the transaction that holds the single SQLite writer.
	var createdAt string
	err := tx.QueryRowContext(ctx, `
		INSERT INTO drift_records
			(container_id, container_name, snapshot_id,
			 detected_at, last_seen_at, inventory_generation,
			 category, field, kind, severity,
			 previous_value, current_value, sensitive,
			 status, reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?)
		ON CONFLICT (container_id, snapshot_id, category, field) DO UPDATE SET
			container_name       = excluded.container_name,
			last_seen_at         = excluded.last_seen_at,
			inventory_generation = excluded.inventory_generation,
			kind                 = excluded.kind,
			severity             = excluded.severity,
			previous_value       = excluded.previous_value,
			current_value        = excluded.current_value,
			sensitive            = excluded.sensitive,
			reason               = excluded.reason,
			resolved_at          = NULL,
			status               = CASE
				WHEN drift_records.status = 'resolved' THEN 'active'
				ELSE drift_records.status
			END,
			updated_at = excluded.updated_at
		RETURNING created_at`,
		record.ContainerID, record.ContainerName, record.SnapshotID,
		stamp, stamp, record.InventoryGeneration,
		string(record.Category), record.Field, string(record.Kind), string(record.Severity),
		previous, current, sensitive,
		record.Reason, stamp, stamp,
	).Scan(&createdAt)
	if err != nil {
		return false, fmt.Errorf("upsert drift record: %w", AsError(err))
	}

	// created_at is only ever written by the INSERT arm, so a row whose
	// created_at is this evaluation's stamp is one this evaluation created.
	return createdAt == stamp, nil
}

// resolveVanishedDrift marks records the evaluation no longer saw.
//
// Scoped to the container AND the baseline it was measured against, so an
// evaluation against a new baseline does not resolve the drift measured
// against the old one -- those are different comparisons and one says nothing
// about the other.
//
// Already-resolved rows are excluded so resolved_at keeps naming the moment
// the drift actually went away rather than the most recent sweep.
func resolveVanishedDrift(
	ctx context.Context,
	tx *sql.Tx,
	evaluation domain.DriftEvaluation,
	seen []string,
	stamp string,
) (int, error) {
	// An INCOMPLETE evaluation resolves nothing. It did not establish that the
	// unseen fields match; it established that it stopped looking. Resolving
	// on that basis would silently clear real drift, which is the worst
	// failure this feature can have.
	if !evaluation.Complete {
		return 0, nil
	}

	query := `
		UPDATE drift_records
		SET status = 'resolved', resolved_at = ?, updated_at = ?
		WHERE container_id = ? AND snapshot_id = ? AND status <> 'resolved'`
	args := []any{stamp, stamp, evaluation.ContainerID, evaluation.SnapshotID}

	if len(seen) > 0 {
		// A placeholder run generated from a slice LENGTH, never from content.
		// The identity keys travel as bound parameters.
		//
		// char(31) rather than a '\x1f' literal: SQLite has no backslash
		// escapes in string literals, so '\x1f' would be the four characters
		// backslash-x-1-f. Two identities could then collide through a field
		// name containing that text, and a collision here silently resolves a
		// drift that is still present.
		query += ` AND (category || char(31) || field) NOT IN (` +
			placeholders(len(seen)) + `)`
		for _, key := range seen {
			args = append(args, key)
		}
	}

	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("resolve drift: %w", AsError(err))
	}

	// The count is used for a log line only, so a driver that cannot report it
	// is not worth failing the transaction over -- the resolutions themselves
	// have already been applied by the statement above. Discarded explicitly
	// rather than returned as a nil error beside a real one.
	affected, _ := result.RowsAffected()
	return int(affected), nil
}

// driftIdentityKey renders the identity used by the NOT IN clause above.
//
// The separator is a unit separator (0x1f), which cannot occur in a category
// from the closed vocabulary nor in a diff-engine field name. Using a
// character that could appear in a field would let two different identities
// collide and silently resolve a real drift.
func driftIdentityKey(record domain.DriftRecord) string {
	return string(record.Category) + "\x1f" + record.Field
}

// recordEvaluation stores the attempt, so "no drift" and "never checked" stay
// distinguishable.
func recordEvaluation(ctx context.Context, tx *sql.Tx, evaluation domain.DriftEvaluation, stamp string) error {
	complete := 0
	if evaluation.Complete {
		complete = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO drift_evaluations
			(container_id, container_name, snapshot_id, evaluated_at,
			 inventory_generation, drift_count, complete, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (container_id) DO UPDATE SET
			container_name       = excluded.container_name,
			snapshot_id          = excluded.snapshot_id,
			evaluated_at         = excluded.evaluated_at,
			inventory_generation = excluded.inventory_generation,
			drift_count          = excluded.drift_count,
			complete             = excluded.complete,
			reason               = excluded.reason`,
		evaluation.ContainerID, evaluation.ContainerName, evaluation.SnapshotID, stamp,
		evaluation.InventoryGeneration, evaluation.DriftCount, complete, evaluation.Reason,
	); err != nil {
		return fmt.Errorf("record drift evaluation: %w", AsError(err))
	}
	return nil
}

// List returns a page of drift records and the total matching the filter.
func (r *DriftRepository) List(ctx context.Context, filter DriftFilter) ([]domain.DriftRecord, int, error) {
	where, args := driftWhere(filter)

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM drift_records`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count drift: %w", AsError(err))
	}

	column, ok := driftSortFields[filter.Sort]
	if !ok {
		column = "detected_at"
	}
	direction := "DESC"
	if filter.Ascending {
		direction = "ASC"
	}

	page := filter.Page.normalise()
	// column and direction come from the allowlist above and a two-valued
	// switch; neither is caller text. Every VALUE is bound.
	query := selectDriftColumns + where +
		` ORDER BY ` + column + ` ` + direction + `, id ` + direction +
		` LIMIT ? OFFSET ?`

	rows, err := r.db.QueryContext(ctx, query, append(args, page.Limit, page.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query drift: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	records, err := scanDriftRecords(rows)
	if err != nil {
		return nil, 0, err
	}
	return records, total, nil
}

// Get returns one record.
func (r *DriftRepository) Get(ctx context.Context, id int64) (domain.DriftRecord, error) {
	rows, err := r.db.QueryContext(ctx, selectDriftColumns+` WHERE id = ?`, id)
	if err != nil {
		return domain.DriftRecord{}, fmt.Errorf("query drift record: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	records, err := scanDriftRecords(rows)
	if err != nil {
		return domain.DriftRecord{}, err
	}
	if len(records) == 0 {
		return domain.DriftRecord{}, ErrNotFound
	}
	return records[0], nil
}

// UpdateStatus applies an operator's status transition.
//
// The status vocabulary is validated by the API against
// domain.OperatorDriftStatuses before it arrives, and the CHECK constraint is
// the backstop. Nothing here can set active or resolved: those are the
// engine's, and a caller asserting "resolved" would be asserting a fact about
// the world rather than an intent.
func (r *DriftRepository) UpdateStatus(
	ctx context.Context,
	id int64,
	status domain.DriftStatus,
	note string,
	now time.Time,
) (domain.DriftRecord, error) {
	stamp := formatTime(now.UTC())

	result, err := r.db.ExecContext(ctx, `
		UPDATE drift_records
		SET status = ?, note = ?, status_changed_at = ?, updated_at = ?
		WHERE id = ?`,
		string(status), note, stamp, stamp, id)
	if err != nil {
		return domain.DriftRecord{}, fmt.Errorf("update drift status: %w", AsError(err))
	}
	affected, err := result.RowsAffected()
	if err == nil && affected == 0 {
		return domain.DriftRecord{}, ErrNotFound
	}
	return r.Get(ctx, id)
}

// Summary computes the dashboard aggregate.
//
// Four small grouped queries plus two scalars, rather than one list the caller
// counts. Each is served by an index and each returns at most a handful of
// rows, so the cost is proportional to the number of DISTINCT severities,
// statuses and categories -- constants -- not to the number of records.
func (r *DriftRepository) Summary(ctx context.Context) (domain.DriftSummary, error) {
	summary := domain.DriftSummary{
		BySeverity: make(map[domain.DriftSeverity]int, len(domain.DriftSeverities)),
		ByStatus:   make(map[domain.DriftStatus]int, len(domain.DriftStatuses)),
		ByCategory: make(map[domain.DriftCategory]int, len(domain.DriftCategories)),
	}

	// Severity and category counts describe OPEN drift only. Including
	// resolved history would make a dashboard's "3 critical" mean "3 critical
	// at some point since installation", which is not what anyone reads it as.
	for _, group := range []struct {
		query string
		apply func(key string, count int)
	}{
		{
			`SELECT severity, COUNT(*) FROM drift_records WHERE status <> 'resolved' GROUP BY severity`,
			func(key string, count int) { summary.BySeverity[domain.DriftSeverity(key)] = count },
		},
		{
			`SELECT status, COUNT(*) FROM drift_records GROUP BY status`,
			func(key string, count int) { summary.ByStatus[domain.DriftStatus(key)] = count },
		},
		{
			`SELECT category, COUNT(*) FROM drift_records WHERE status <> 'resolved' GROUP BY category`,
			func(key string, count int) { summary.ByCategory[domain.DriftCategory(key)] = count },
		},
	} {
		if err := r.scanCounts(ctx, group.query, group.apply); err != nil {
			return summary, err
		}
	}

	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status <> 'resolved' THEN 1 ELSE 0 END), 0)
		FROM drift_records`).Scan(&summary.Total, &summary.Open); err != nil {
		return summary, fmt.Errorf("count drift totals: %w", AsError(err))
	}

	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT container_id) FROM drift_records WHERE status <> 'resolved'`,
	).Scan(&summary.ContainersWithDrift); err != nil {
		return summary, fmt.Errorf("count drifting containers: %w", AsError(err))
	}

	var (
		lastEvaluated sql.NullString
		incomplete    int
	)
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*), MAX(evaluated_at),
		       COALESCE(SUM(CASE WHEN complete = 0 THEN 1 ELSE 0 END), 0)
		FROM drift_evaluations`,
	).Scan(&summary.ContainersEvaluated, &lastEvaluated, &incomplete); err != nil {
		return summary, fmt.Errorf("read drift evaluations: %w", AsError(err))
	}
	summary.Incomplete = incomplete > 0

	if lastEvaluated.Valid && lastEvaluated.String != "" {
		parsed, err := parseTime(lastEvaluated.String)
		if err != nil {
			return summary, err
		}
		summary.LastEvaluatedAt = &parsed
	}
	return summary, nil
}

// Evaluation returns the most recent comparison attempt for one container.
func (r *DriftRepository) Evaluation(ctx context.Context, containerID string) (domain.DriftEvaluation, error) {
	var (
		evaluation domain.DriftEvaluation
		evaluated  string
		complete   int
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT container_id, container_name, snapshot_id, evaluated_at,
		       inventory_generation, drift_count, complete, reason
		FROM drift_evaluations WHERE container_id = ?`, containerID,
	).Scan(&evaluation.ContainerID, &evaluation.ContainerName, &evaluation.SnapshotID,
		&evaluated, &evaluation.InventoryGeneration, &evaluation.DriftCount,
		&complete, &evaluation.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DriftEvaluation{}, ErrNotFound
	}
	if err != nil {
		return domain.DriftEvaluation{}, fmt.Errorf("read drift evaluation: %w", AsError(err))
	}

	evaluation.Complete = complete == 1
	if evaluation.EvaluatedAt, err = parseTime(evaluated); err != nil {
		return domain.DriftEvaluation{}, err
	}
	return evaluation, nil
}

// PruneResolved deletes resolved records older than the cutoff.
//
// Resolved drift is history, and history is the dimension that grows without
// bound on a busy host. Open records are never pruned: an unreviewed
// difference does not become less true with age.
func (r *DriftRepository) PruneResolved(ctx context.Context, cutoff time.Time, batch int) (int64, error) {
	if batch < 1 {
		batch = 500
	}
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM drift_records
		WHERE id IN (
			SELECT id FROM drift_records
			WHERE status = 'resolved' AND resolved_at IS NOT NULL AND resolved_at < ?
			LIMIT ?
		)`, formatTime(cutoff.UTC()), batch)
	if err != nil {
		return 0, fmt.Errorf("prune resolved drift: %w", AsError(err))
	}

	// Reported for logging only; the deletion has already happened.
	removed, _ := result.RowsAffected()
	return removed, nil
}

// scanCounts runs a two-column GROUP BY and applies each row.
func (r *DriftRepository) scanCounts(ctx context.Context, query string, apply func(string, int)) error {
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("aggregate drift: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			key   string
			count int
		)
		if err := rows.Scan(&key, &count); err != nil {
			return fmt.Errorf("scan drift aggregate: %w", err)
		}
		apply(key, count)
	}
	return rows.Err()
}

// driftWhere builds the filter clause. Every value is bound; only the
// placeholder RUN length varies with the input.
func driftWhere(filter DriftFilter) (string, []any) {
	clauses := make([]string, 0, 6)
	args := make([]any, 0, 8)

	if filter.ContainerID != "" {
		clauses = append(clauses, "container_id = ?")
		args = append(args, filter.ContainerID)
	}
	if filter.SnapshotID > 0 {
		clauses = append(clauses, "snapshot_id = ?")
		args = append(args, filter.SnapshotID)
	}
	if filter.OpenOnly {
		clauses = append(clauses, "status <> 'resolved'")
	}

	if len(filter.Categories) > 0 {
		clauses = append(clauses, "category IN ("+placeholders(len(filter.Categories))+")")
		for _, category := range filter.Categories {
			args = append(args, string(category))
		}
	}
	if len(filter.Severities) > 0 {
		clauses = append(clauses, "severity IN ("+placeholders(len(filter.Severities))+")")
		for _, severity := range filter.Severities {
			args = append(args, string(severity))
		}
	}
	if len(filter.Statuses) > 0 {
		clauses = append(clauses, "status IN ("+placeholders(len(filter.Statuses))+")")
		for _, status := range filter.Statuses {
			args = append(args, string(status))
		}
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// placeholders renders a bound-parameter run of the given length.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func scanDriftRecords(rows *sql.Rows) ([]domain.DriftRecord, error) {
	records := make([]domain.DriftRecord, 0, 16)

	for rows.Next() {
		var (
			record          domain.DriftRecord
			detected        string
			lastSeen        string
			resolved        sql.NullString
			statusChanged   sql.NullString
			sensitive       int
			category        string
			kind            string
			severity        string
			recordStatusRaw string
		)
		if err := rows.Scan(
			&record.ID, &record.ContainerID, &record.ContainerName, &record.SnapshotID,
			&detected, &lastSeen, &resolved, &record.InventoryGeneration,
			&category, &record.Field, &kind, &severity,
			&record.PreviousValue, &record.CurrentValue, &sensitive,
			&recordStatusRaw, &record.Reason, &record.Note, &statusChanged,
		); err != nil {
			return nil, fmt.Errorf("scan drift record: %w", err)
		}

		record.Category = domain.DriftCategory(category)
		record.Kind = domain.ChangeKind(kind)
		record.Severity = domain.DriftSeverity(severity)
		record.Status = domain.DriftStatus(recordStatusRaw)
		record.Sensitive = sensitive == 1

		// Defence in depth on the READ path too. A row that somehow carried a
		// value for a sensitive field -- a hand-edited database, a restored
		// backup from a version without the constraint -- must not serve it.
		if record.Sensitive {
			record.PreviousValue = ""
			record.CurrentValue = ""
		}

		var err error
		if record.DetectedAt, err = parseTime(detected); err != nil {
			return nil, err
		}
		if record.LastSeenAt, err = parseTime(lastSeen); err != nil {
			return nil, err
		}
		if resolved.Valid && resolved.String != "" {
			parsed, err := parseTime(resolved.String)
			if err != nil {
				return nil, err
			}
			record.ResolvedAt = &parsed
		}
		if statusChanged.Valid && statusChanged.String != "" {
			parsed, err := parseTime(statusChanged.String)
			if err != nil {
				return nil, err
			}
			record.StatusChangedAt = &parsed
		}

		records = append(records, record)
	}
	return records, rows.Err()
}
