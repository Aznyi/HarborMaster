package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// maxPageSize bounds any list query so a caller cannot ask the process to
// materialise the whole table.
const maxPageSize = 200

// SnapshotRepository reads and writes container configuration snapshots.
//
// Snapshots are append-only. Nothing in this repository updates or deletes a
// row: a snapshot is evidence, and rollback depends on it being unaltered.
type SnapshotRepository struct {
	db *sql.DB
}

// Page describes a bounded slice of a result set.
type Page struct {
	Limit  int
	Offset int
}

func (p Page) normalise() Page {
	if p.Limit <= 0 || p.Limit > maxPageSize {
		p.Limit = 50
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	return p
}

// Create stores a snapshot and returns it with its assigned ID and timestamp.
//
// Re-capturing an unchanged configuration is a no-op: the existing snapshot is
// returned instead of a duplicate row, which keeps history meaningful.
func (r *SnapshotRepository) Create(ctx context.Context, s domain.Snapshot) (domain.Snapshot, error) {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	s.CreatedAt = s.CreatedAt.UTC()

	const insert = `
		INSERT INTO snapshots
			(container_id, container_name, source, image, image_id, spec, checksum, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (container_id, checksum) DO NOTHING
		RETURNING id`

	err := r.db.QueryRowContext(ctx, insert,
		s.ContainerID, s.ContainerName, string(s.Source), s.Image, s.ImageID,
		s.Spec, s.Checksum, s.Note, formatTime(s.CreatedAt),
	).Scan(&s.ID)

	switch {
	case err == nil:
		return s, nil
	case errors.Is(err, sql.ErrNoRows):
		// Identical configuration already captured for this container.
		return r.getByChecksum(ctx, s.ContainerID, s.Checksum)
	default:
		return domain.Snapshot{}, fmt.Errorf("insert snapshot: %w", err)
	}
}

// Get returns a snapshot by ID, or ErrNotFound.
func (r *SnapshotRepository) Get(ctx context.Context, id int64) (domain.Snapshot, error) {
	const query = selectSnapshotColumns + ` FROM snapshots WHERE id = ?`
	return scanSnapshotRow(r.db.QueryRowContext(ctx, query, id))
}

// ListByContainer returns a container's snapshots, newest first.
func (r *SnapshotRepository) ListByContainer(ctx context.Context, containerID string, page Page) ([]domain.Snapshot, error) {
	page = page.normalise()
	const query = selectSnapshotColumns + `
		FROM snapshots
		WHERE container_id = ?
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?`
	return r.query(ctx, query, containerID, page.Limit, page.Offset)
}

// List returns all snapshots, newest first.
func (r *SnapshotRepository) List(ctx context.Context, page Page) ([]domain.Snapshot, error) {
	page = page.normalise()
	const query = selectSnapshotColumns + `
		FROM snapshots
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?`
	return r.query(ctx, query, page.Limit, page.Offset)
}

// Count returns the total number of stored snapshots.
func (r *SnapshotRepository) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshots`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count snapshots: %w", err)
	}
	return n, nil
}

func (r *SnapshotRepository) getByChecksum(ctx context.Context, containerID, checksum string) (domain.Snapshot, error) {
	const query = selectSnapshotColumns + ` FROM snapshots WHERE container_id = ? AND checksum = ?`
	return scanSnapshotRow(r.db.QueryRowContext(ctx, query, containerID, checksum))
}

func (r *SnapshotRepository) query(ctx context.Context, query string, args ...any) ([]domain.Snapshot, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Non-nil empty slice: the API must serialise an empty result as [], not null.
	snapshots := make([]domain.Snapshot, 0)
	for rows.Next() {
		s, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, s)
	}
	return snapshots, rows.Err()
}

const selectSnapshotColumns = `
	SELECT id, container_id, container_name, source, image, image_id, spec, checksum, note, created_at`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSnapshotRow(row *sql.Row) (domain.Snapshot, error) {
	s, err := scanSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Snapshot{}, ErrNotFound
	}
	return s, err
}

func scanSnapshot(row rowScanner) (domain.Snapshot, error) {
	var (
		s         domain.Snapshot
		source    string
		createdAt string
	)
	if err := row.Scan(
		&s.ID, &s.ContainerID, &s.ContainerName, &source, &s.Image, &s.ImageID,
		&s.Spec, &s.Checksum, &s.Note, &createdAt,
	); err != nil {
		return domain.Snapshot{}, err
	}
	s.Source = domain.SnapshotSource(source)

	parsed, err := parseTime(createdAt)
	if err != nil {
		return domain.Snapshot{}, fmt.Errorf("snapshot %d: %w", s.ID, err)
	}
	s.CreatedAt = parsed
	return s, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored timestamp: %w", err)
	}
	return t.UTC(), nil
}
