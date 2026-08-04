package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// DockerEventRepository owns the observational Docker event history.
//
// Append and read only: nothing here updates an event once written. The single
// destructive path is Prune, which enforces retention and is never reachable
// from the HTTP API.
type DockerEventRepository struct {
	db *sql.DB
}

// eventSortColumns is the allowlist mapping API sort fields to SQL columns.
//
// As in ContainerRepository, this map is the ONLY way a sort field reaches the
// query. Nothing caller-supplied is interpolated into SQL.
var eventSortColumns = map[string]string{
	"sequence":   "e.sequence",
	"observed":   "e.observed_at",
	"dockerTime": "e.docker_time",
	"type":       "e.event_type",
	"action":     "e.action",
	"result":     "e.result",
	"project":    "e.compose_project",
	"name":       "e.resource_name",
}

// EventSortFields returns the sortable field names, for validation and docs.
func EventSortFields() []string {
	fields := make([]string, 0, len(eventSortColumns))
	for field := range eventSortColumns {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

// ValidEventSortField reports whether a field name is sortable.
func ValidEventSortField(field string) bool {
	_, ok := eventSortColumns[field]
	return ok
}

// DockerEventFilter selects and orders events for the list endpoint.
type DockerEventFilter struct {
	// Types and Actions are OR-ed within themselves, AND-ed with everything else.
	Types   []domain.DockerEventType
	Actions []string
	Results []domain.EventProcessingResult

	ActorID        string
	ComposeProject string
	ComposeService string
	// Search matches the resource name or an actor ID prefix.
	Search string

	// Since and Until bound observed_at. Nil means unbounded on that side.
	Since *time.Time
	Until *time.Time

	Sort      string
	Direction SortDirection
	Page      Page
}

const dockerEventColumns = `
	e.sequence, e.fingerprint, e.host_id, e.event_type, e.action, e.actor_id,
	e.resource_name, e.scope, e.compose_project, e.compose_service,
	e.docker_time, e.docker_time_nano, e.observed_at, e.attributes,
	e.result, e.refresh_type, e.error, e.connection_state, e.created_at`

// Append writes a batch of events in one transaction.
//
// Returns the events with their assigned sequence numbers, in the order they
// were supplied, and separately how many were rejected as duplicates.
//
// One transaction per batch rather than per event: SQLite tolerates a single
// writer, and a burst of sixty container events during a `compose up` should
// cost one commit rather than sixty. A batch is all-or-nothing, so a partial
// burst never appears in the history.
//
// Duplicates are absorbed by the unique fingerprint constraint rather than
// failing the batch: the in-memory window is the first line of defence, and
// this is what catches a duplicate that outlived it or arrived after a restart.
func (r *DockerEventRepository) Append(ctx context.Context, events []domain.DockerEvent) ([]domain.DockerEvent, int, error) {
	if len(events) == 0 {
		return nil, 0, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("begin event transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stored := make([]domain.DockerEvent, 0, len(events))
	duplicates := 0

	for _, event := range events {
		event = normalizeForStorage(event)

		attributes := marshalStringMap(event.Attributes)

		// ON CONFLICT DO NOTHING plus RETURNING: a conflicting row returns no
		// rows at all, which is how a duplicate is detected without a
		// preceding SELECT.
		var sequence int64
		err := tx.QueryRowContext(ctx, `
			INSERT INTO docker_events
				(fingerprint, host_id, event_type, action, actor_id, resource_name,
				 scope, compose_project, compose_service, docker_time,
				 docker_time_nano, observed_at, attributes, result, refresh_type,
				 error, connection_state, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (fingerprint) DO NOTHING
			RETURNING sequence`,
			event.Fingerprint, event.HostID, string(event.Type), string(event.Action),
			event.ActorID, event.ActorName, event.Scope,
			event.ComposeProject, event.ComposeService,
			timeOrNil(event.DockerTime), event.DockerTimeNano,
			formatTime(event.ObservedAt), attributes,
			string(event.Result), string(event.RefreshRequested),
			event.Error, string(event.ConnectionState),
			formatTime(event.CreatedAt),
		).Scan(&sequence)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			duplicates++
			continue
		case err != nil:
			return nil, 0, fmt.Errorf("insert docker event: %w", err)
		}

		event.Sequence = sequence
		stored = append(stored, event)
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("commit events: %w", err)
	}
	return stored, duplicates, nil
}

// normalizeForStorage fills in the defaults a row cannot be written without.
//
// Applied here rather than trusted from the caller: this repository is the last
// point before the CHECK constraints, and a zero-valued result or an empty host
// would fail the insert with a message about SQL rather than about the event.
func normalizeForStorage(event domain.DockerEvent) domain.DockerEvent {
	if event.HostID == "" {
		event.HostID = domain.LocalHostID
	}
	if event.Scope == "" {
		event.Scope = "local"
	}
	if event.Type == "" {
		event.Type = domain.EventTypeOther
	}
	if event.Result == "" {
		event.Result = domain.ResultProcessed
	}
	if event.RefreshRequested == "" {
		event.RefreshRequested = domain.RefreshNone
	}
	if event.ObservedAt.IsZero() {
		event.ObservedAt = time.Now().UTC()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = event.ObservedAt
	}
	event.ObservedAt = event.ObservedAt.UTC()
	event.CreatedAt = event.CreatedAt.UTC()
	if !event.DockerTime.IsZero() {
		event.DockerTime = event.DockerTime.UTC()
	}
	return event
}

// List returns a page of events and the total matching the filter.
func (r *DockerEventRepository) List(ctx context.Context, filter DockerEventFilter) ([]domain.DockerEvent, int, error) {
	where, args := buildEventWhere(filter)
	page := filter.Page.normalise()

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM docker_events e`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count docker events: %w", err)
	}

	query := `SELECT` + dockerEventColumns + ` FROM docker_events e` + where +
		buildEventOrder(filter) + ` LIMIT ? OFFSET ?`

	rows, err := r.db.QueryContext(ctx, query, append(args, page.Limit, page.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query docker events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	events := make([]domain.DockerEvent, 0)
	for rows.Next() {
		event, err := scanDockerEvent(rows)
		if err != nil {
			return nil, 0, err
		}
		events = append(events, event)
	}
	return events, total, rows.Err()
}

// Get returns one event by its local sequence number, or ErrNotFound.
func (r *DockerEventRepository) Get(ctx context.Context, sequence int64) (*domain.DockerEvent, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT`+dockerEventColumns+` FROM docker_events e WHERE e.sequence = ?`, sequence)

	event, err := scanDockerEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &event, nil
}

// Since returns up to limit events with a sequence greater than after, oldest
// first, together with how many matched in total.
//
// This is what backs an SSE reconnect carrying Last-Event-ID. Oldest-first
// because a replay must be delivered in the order the client would have
// received it live. The total lets the handler tell a client that its replay
// was truncated rather than silently handing it a hole.
func (r *DockerEventRepository) Since(ctx context.Context, after int64, limit int) ([]domain.DockerEvent, int, error) {
	if limit <= 0 {
		return nil, 0, nil
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM docker_events WHERE sequence > ?`, after).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count replay events: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT`+dockerEventColumns+` FROM docker_events e
		 WHERE e.sequence > ? ORDER BY e.sequence ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("query replay events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	events := make([]domain.DockerEvent, 0, limit)
	for rows.Next() {
		event, err := scanDockerEvent(rows)
		if err != nil {
			return nil, 0, err
		}
		events = append(events, event)
	}
	return events, total, rows.Err()
}

// Count returns how many events are stored.
func (r *DockerEventRepository) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM docker_events`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count docker events: %w", err)
	}
	return n, nil
}

// DistinctEventProjects returns the Compose projects present in event history,
// so the UI can populate its filter from real data.
func (r *DockerEventRepository) DistinctEventProjects(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT compose_project FROM docker_events
		WHERE compose_project <> '' ORDER BY compose_project LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("query event projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	projects := make([]string, 0)
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

// DistinctEventActions returns the actions present in event history.
func (r *DockerEventRepository) DistinctEventActions(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT action FROM docker_events
		WHERE action <> '' ORDER BY action LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("query event actions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	actions := make([]string, 0)
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			return nil, err
		}
		actions = append(actions, action)
	}
	return actions, rows.Err()
}

// PruneOptions bounds a retention pass.
type PruneOptions struct {
	// MaxAge removes events observed longer ago than this. Zero disables
	// age-based pruning.
	MaxAge time.Duration
	// MaxCount caps the total row count, removing the oldest above it. Zero
	// disables count-based pruning.
	MaxCount int64
	// BatchSize bounds one DELETE. Pruning in batches keeps each write lock
	// short, so a large backlog cannot stall API reads behind one enormous
	// transaction.
	BatchSize int
	// Now is injectable for deterministic tests.
	Now time.Time
}

// Prune enforces retention and returns how many rows it removed.
//
// It touches ONLY docker_events. No inventory record, no container, no warning
// is reachable from here -- retention on an observational log must never be
// able to delete current state.
//
// Deletes run in bounded batches with a fresh statement each time, and the loop
// exits on context cancellation, so a shutdown during a large prune stops
// promptly rather than after the whole backlog.
func (r *DockerEventRepository) Prune(ctx context.Context, opts PruneOptions) (int64, error) {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 500
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}

	var removed int64

	if opts.MaxAge > 0 {
		cutoff := formatTime(opts.Now.Add(-opts.MaxAge).UTC())
		for {
			if err := ctx.Err(); err != nil {
				return removed, err
			}
			result, err := r.db.ExecContext(ctx, `
				DELETE FROM docker_events
				WHERE sequence IN (
					SELECT sequence FROM docker_events
					WHERE observed_at < ?
					ORDER BY sequence ASC
					LIMIT ?
				)`, cutoff, opts.BatchSize)
			if err != nil {
				return removed, fmt.Errorf("prune events by age: %w", err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return removed, err
			}
			removed += affected
			if affected < int64(opts.BatchSize) {
				break
			}
		}
	}

	if opts.MaxCount > 0 {
		for {
			if err := ctx.Err(); err != nil {
				return removed, err
			}

			var total int64
			if err := r.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM docker_events`).Scan(&total); err != nil {
				return removed, fmt.Errorf("count events for retention: %w", err)
			}
			excess := total - opts.MaxCount
			if excess <= 0 {
				break
			}
			if excess > int64(opts.BatchSize) {
				excess = int64(opts.BatchSize)
			}

			result, err := r.db.ExecContext(ctx, `
				DELETE FROM docker_events
				WHERE sequence IN (
					SELECT sequence FROM docker_events
					ORDER BY sequence ASC
					LIMIT ?
				)`, excess)
			if err != nil {
				return removed, fmt.Errorf("prune events by count: %w", err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return removed, err
			}
			removed += affected
			if affected == 0 {
				// Nothing was removable; stop rather than spin.
				break
			}
		}
	}

	return removed, nil
}

// ------------------------------------------------------- engine state --

// EngineState is the small slice of event-engine status worth surviving a
// restart.
type EngineState struct {
	LastConnectedAt    *time.Time
	LastDisconnectedAt *time.Time
	LastEventAt        *time.Time
	LastReconciledAt   *time.Time
	ReconnectCount     int64
	LastError          string
}

// LoadState returns the persisted engine state for a host. A host that has
// never run the engine yields a zero state and no error.
func (r *DockerEventRepository) LoadState(ctx context.Context, hostID string) (EngineState, error) {
	var (
		state                                              EngineState
		connected, disconnected, lastEvent, lastReconciled sql.NullString
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT last_connected_at, last_disconnected_at, last_event_at,
		       last_reconciled_at, reconnect_count, last_error
		FROM event_engine_state WHERE host_id = ?`, hostID).
		Scan(&connected, &disconnected, &lastEvent, &lastReconciled,
			&state.ReconnectCount, &state.LastError)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return EngineState{}, nil
	case err != nil:
		return EngineState{}, fmt.Errorf("read event engine state: %w", err)
	}

	state.LastConnectedAt = scanOptionalTime(connected)
	state.LastDisconnectedAt = scanOptionalTime(disconnected)
	state.LastEventAt = scanOptionalTime(lastEvent)
	state.LastReconciledAt = scanOptionalTime(lastReconciled)
	return state, nil
}

// SaveState upserts the persisted engine state.
//
// Called on connection transitions and after a reconciliation, never per event:
// a write on the hot path to record a timestamp nobody reads more than once a
// minute would be a poor trade against a single-writer database.
func (r *DockerEventRepository) SaveState(ctx context.Context, hostID string, state EngineState) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO event_engine_state
			(host_id, last_connected_at, last_disconnected_at, last_event_at,
			 last_reconciled_at, reconnect_count, last_error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (host_id) DO UPDATE SET
			last_connected_at    = excluded.last_connected_at,
			last_disconnected_at = excluded.last_disconnected_at,
			last_event_at        = excluded.last_event_at,
			last_reconciled_at   = excluded.last_reconciled_at,
			reconnect_count      = excluded.reconnect_count,
			last_error           = excluded.last_error,
			updated_at           = excluded.updated_at`,
		hostID, nullableTime(state.LastConnectedAt), nullableTime(state.LastDisconnectedAt),
		nullableTime(state.LastEventAt), nullableTime(state.LastReconciledAt),
		state.ReconnectCount, state.LastError, formatTime(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("save event engine state: %w", err)
	}
	return nil
}

// ------------------------------------------------------------- helpers --

// buildEventWhere assembles the predicate. Every value is a bound parameter;
// nothing from the caller is concatenated into SQL.
func buildEventWhere(filter DockerEventFilter) (string, []any) {
	clauses := make([]string, 0, 8)
	args := make([]any, 0, 8)

	if len(filter.Types) > 0 {
		placeholders := make([]string, len(filter.Types))
		for i, eventType := range filter.Types {
			placeholders[i] = "?"
			args = append(args, string(eventType))
		}
		clauses = append(clauses, "e.event_type IN ("+strings.Join(placeholders, ",")+")")
	}

	if len(filter.Actions) > 0 {
		placeholders := make([]string, len(filter.Actions))
		for i, action := range filter.Actions {
			placeholders[i] = "?"
			args = append(args, action)
		}
		clauses = append(clauses, "e.action IN ("+strings.Join(placeholders, ",")+")")
	}

	if len(filter.Results) > 0 {
		placeholders := make([]string, len(filter.Results))
		for i, result := range filter.Results {
			placeholders[i] = "?"
			args = append(args, string(result))
		}
		clauses = append(clauses, "e.result IN ("+strings.Join(placeholders, ",")+")")
	}

	if filter.ActorID != "" {
		// Prefix match so a short container ID from the UI resolves without the
		// caller having to know the full 64 characters.
		clauses = append(clauses, "e.actor_id LIKE ? ESCAPE '\\'")
		args = append(args, escapeLike(filter.ActorID)+"%")
	}
	if filter.ComposeProject != "" {
		clauses = append(clauses, "e.compose_project = ?")
		args = append(args, filter.ComposeProject)
	}
	if filter.ComposeService != "" {
		clauses = append(clauses, "e.compose_service = ?")
		args = append(args, filter.ComposeService)
	}

	if search := strings.TrimSpace(filter.Search); search != "" {
		clauses = append(clauses,
			"(e.resource_name LIKE ? ESCAPE '\\' OR e.actor_id LIKE ? ESCAPE '\\')")
		args = append(args, "%"+escapeLike(search)+"%", escapeLike(search)+"%")
	}

	if filter.Since != nil {
		clauses = append(clauses, "e.observed_at >= ?")
		args = append(args, formatTime(filter.Since.UTC()))
	}
	if filter.Until != nil {
		clauses = append(clauses, "e.observed_at <= ?")
		args = append(args, formatTime(filter.Until.UTC()))
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// buildEventOrder renders the ORDER BY.
//
// Always terminated by the local sequence, so ordering is total even when the
// requested field ties -- which it does constantly, because a burst of events
// shares a Docker timestamp. Without it, paging could repeat or skip a row.
func buildEventOrder(filter DockerEventFilter) string {
	column, ok := eventSortColumns[filter.Sort]
	if !ok {
		column = "e.sequence"
	}
	direction := "DESC"
	if filter.Direction == SortAsc {
		direction = "ASC"
	}
	if column == "e.sequence" {
		return " ORDER BY e.sequence " + direction
	}
	return " ORDER BY " + column + " " + direction + ", e.sequence " + direction
}

func scanDockerEvent(row rowScanner) (domain.DockerEvent, error) {
	var (
		event           domain.DockerEvent
		eventType       string
		action          string
		dockerTime      sql.NullString
		observedAt      string
		attributes      string
		result          string
		refreshType     string
		connectionState string
		createdAt       string
	)

	if err := row.Scan(&event.Sequence, &event.Fingerprint, &event.HostID,
		&eventType, &action, &event.ActorID, &event.ActorName, &event.Scope,
		&event.ComposeProject, &event.ComposeService,
		&dockerTime, &event.DockerTimeNano, &observedAt, &attributes,
		&result, &refreshType, &event.Error, &connectionState, &createdAt); err != nil {
		return domain.DockerEvent{}, err
	}

	event.Type = domain.DockerEventType(eventType)
	event.Action = domain.DockerEventAction(action)
	event.Result = domain.EventProcessingResult(result)
	event.RefreshRequested = domain.RefreshRequest(refreshType)
	event.ConnectionState = domain.EventConnectionState(connectionState)
	event.DockerTime = scanTime(dockerTime)

	// Timestamps fail soft: a malformed row should degrade one event's display
	// rather than fail the whole list request.
	if parsed, err := parseTime(observedAt); err == nil {
		event.ObservedAt = parsed
	}
	if parsed, err := parseTime(createdAt); err == nil {
		event.CreatedAt = parsed
	}

	event.Attributes = unmarshalStringMap(attributes)
	if event.Attributes == nil {
		event.Attributes = map[string]string{}
	}
	event.HarborMasterLabels = harborMasterLabelsFrom(event.Attributes)

	return event, nil
}

// harborMasterLabelsFrom rebuilds the io.harbormaster.* view on read rather
// than storing it twice. The attributes column is the single source.
func harborMasterLabelsFrom(attributes map[string]string) map[string]string {
	var out map[string]string
	for key, value := range attributes {
		if !strings.HasPrefix(key, domain.HarborMasterLabelPrefix) {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[strings.TrimPrefix(key, domain.HarborMasterLabelPrefix)] = value
	}
	return out
}
