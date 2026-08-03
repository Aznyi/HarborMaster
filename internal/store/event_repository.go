package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// EventRepository appends to and reads HarborMaster's audit log.
//
// The log is append-only: there is deliberately no update or delete path.
type EventRepository struct {
	db *sql.DB
}

// Append records an event and returns it with its assigned ID and timestamp.
func (r *EventRepository) Append(ctx context.Context, e domain.Event) (domain.Event, error) {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	e.OccurredAt = e.OccurredAt.UTC()
	if e.Severity == "" {
		e.Severity = domain.SeverityInfo
	}

	const insert = `
		INSERT INTO events (type, severity, container_id, message, details, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING id`

	if err := r.db.QueryRowContext(ctx, insert,
		string(e.Type), string(e.Severity), e.ContainerID, e.Message,
		nullableBlob(e.Details), formatTime(e.OccurredAt),
	).Scan(&e.ID); err != nil {
		return domain.Event{}, fmt.Errorf("insert event: %w", err)
	}
	return e, nil
}

// List returns events newest first.
func (r *EventRepository) List(ctx context.Context, page Page) ([]domain.Event, error) {
	page = page.normalise()
	const query = selectEventColumns + `
		FROM events
		ORDER BY occurred_at DESC, id DESC
		LIMIT ? OFFSET ?`
	return r.query(ctx, query, page.Limit, page.Offset)
}

// ListByContainer returns a container's events, newest first.
func (r *EventRepository) ListByContainer(ctx context.Context, containerID string, page Page) ([]domain.Event, error) {
	page = page.normalise()
	const query = selectEventColumns + `
		FROM events
		WHERE container_id = ?
		ORDER BY occurred_at DESC, id DESC
		LIMIT ? OFFSET ?`
	return r.query(ctx, query, containerID, page.Limit, page.Offset)
}

// Count returns the total number of recorded events.
func (r *EventRepository) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return n, nil
}

func (r *EventRepository) query(ctx context.Context, query string, args ...any) ([]domain.Event, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	events := make([]domain.Event, 0)
	for rows.Next() {
		var (
			e          domain.Event
			eventType  string
			severity   string
			details    []byte
			occurredAt string
		)
		if err := rows.Scan(&e.ID, &eventType, &severity, &e.ContainerID,
			&e.Message, &details, &occurredAt); err != nil {
			return nil, err
		}
		e.Type = domain.EventType(eventType)
		e.Severity = domain.EventSeverity(severity)
		e.Details = details

		parsed, err := parseTime(occurredAt)
		if err != nil {
			return nil, fmt.Errorf("event %d: %w", e.ID, err)
		}
		e.OccurredAt = parsed
		events = append(events, e)
	}
	return events, rows.Err()
}

const selectEventColumns = `
	SELECT id, type, severity, container_id, message, details, occurred_at`

func nullableBlob(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
