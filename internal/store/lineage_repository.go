package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// LineageRepository owns what a managed container FOLLOWS, as distinct from
// what it RUNS.
//
// The authoritative record. Containers also carry
// `io.harbormaster.image.tracking`, which makes lineage recoverable from Docker
// alone, but a label is written by whoever created the container: it is
// evidence for a row that exists, never a reason to create one.
//
// See migrations/0022_image_lineage.sql for why the key is a container name.
type LineageRepository struct {
	db *sql.DB
}

// MaxLineageRows bounds a single read of the lineage set.
//
// Every caller here reads the whole tracked set at once -- update discovery
// seeds from it on each inventory refresh, and a plan batch joins against it --
// so the bound is on the query rather than on a page a caller forgot to turn.
const MaxLineageRows = 10000

// lineageColumns is the select list, in scan order.
const lineageColumns = `container_name, container_id, state, origin,
	tracking_reference, tracking_familiar, repository, running_digest,
	created_at, updated_at`

// Upsert writes one container's lineage, replacing whatever was there.
//
// Last write wins, and deliberately: every caller has just established the
// state it is writing from evidence it holds -- an observation, a verified
// recreation, a completed rollback -- and a merge here would silently combine
// two claims neither caller made.
func (r *LineageRepository) Upsert(ctx context.Context, lineage domain.ImageLineage) error {
	if err := validateLineage(lineage); err != nil {
		return err
	}

	created := lineage.CreatedAt
	if created.IsZero() {
		created = lineage.UpdatedAt
	}

	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO image_lineage (
			container_name, container_id, state, origin,
			tracking_reference, tracking_familiar, repository, running_digest,
			created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (container_name) DO UPDATE SET
			container_id       = excluded.container_id,
			state              = excluded.state,
			origin             = excluded.origin,
			tracking_reference = excluded.tracking_reference,
			tracking_familiar  = excluded.tracking_familiar,
			repository         = excluded.repository,
			running_digest     = excluded.running_digest,
			updated_at         = excluded.updated_at`,
		lineage.ContainerName, lineage.ContainerID,
		string(lineage.State), string(lineage.Origin),
		lineage.TrackingReference, lineage.TrackingFamiliar,
		lineage.Repository, lineage.RunningDigest,
		formatTime(created.UTC()), formatTime(lineage.UpdatedAt.UTC()),
	); err != nil {
		return fmt.Errorf("write image lineage: %w", AsError(err))
	}
	return nil
}

// Get returns one container's lineage, or ErrNotFound.
func (r *LineageRepository) Get(ctx context.Context, containerName string) (domain.ImageLineage, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+lineageColumns+` FROM image_lineage WHERE container_name = ?`, containerName)

	lineage, err := scanLineage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ImageLineage{}, ErrNotFound
	}
	if err != nil {
		return domain.ImageLineage{}, fmt.Errorf("read image lineage: %w", AsError(err))
	}
	return lineage, nil
}

// All returns every lineage row, oldest name first.
//
// Bounded by MaxLineageRows. A plan batch and an update-discovery seed both
// want the whole set, and one query for it is what keeps those paths from
// becoming a read per container.
func (r *LineageRepository) All(ctx context.Context) ([]domain.ImageLineage, error) {
	return r.query(ctx, `SELECT `+lineageColumns+` FROM image_lineage
		ORDER BY container_name LIMIT ?`, MaxLineageRows)
}

// Tracked returns only the rows update discovery can act on.
func (r *LineageRepository) Tracked(ctx context.Context) ([]domain.ImageLineage, error) {
	return r.query(ctx, `SELECT `+lineageColumns+` FROM image_lineage
		WHERE state = 'tracked' AND tracking_reference <> ''
		ORDER BY container_name LIMIT ?`, MaxLineageRows)
}

// Delete removes a container's lineage.
//
// Called when a container leaves the estate for good. Absent is success: the
// caller wants the row gone, and it is.
func (r *LineageRepository) Delete(ctx context.Context, containerName string) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM image_lineage WHERE container_name = ?`, containerName); err != nil {
		return fmt.Errorf("delete image lineage: %w", AsError(err))
	}
	return nil
}

// Count reports how many rows exist in each state, for the health surface.
func (r *LineageRepository) Count(ctx context.Context) (tracked, untracked int, err error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN state = 'tracked'   THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN state = 'untracked' THEN 1 ELSE 0 END), 0)
		FROM image_lineage`)
	if err := row.Scan(&tracked, &untracked); err != nil {
		return 0, 0, fmt.Errorf("count image lineage: %w", AsError(err))
	}
	return tracked, untracked, nil
}

func (r *LineageRepository) query(ctx context.Context, statement string, args ...any) ([]domain.ImageLineage, error) {
	rows, err := r.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query image lineage: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	lineages := make([]domain.ImageLineage, 0, 32)
	for rows.Next() {
		lineage, scanErr := scanLineage(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("read image lineage: %w", AsError(scanErr))
		}
		lineages = append(lineages, lineage)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read image lineage: %w", AsError(err))
	}
	return lineages, nil
}

// scanner is the shared shape of *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanLineage(row scanner) (domain.ImageLineage, error) {
	var (
		lineage            domain.ImageLineage
		state, origin      string
		createdAt, updated string
	)
	if err := row.Scan(
		&lineage.ContainerName, &lineage.ContainerID, &state, &origin,
		&lineage.TrackingReference, &lineage.TrackingFamiliar,
		&lineage.Repository, &lineage.RunningDigest,
		&createdAt, &updated,
	); err != nil {
		return domain.ImageLineage{}, err
	}
	lineage.State = domain.LineageState(state)
	lineage.Origin = domain.LineageOrigin(origin)

	created, err := parseTime(createdAt)
	if err != nil {
		return domain.ImageLineage{}, fmt.Errorf("parse lineage created_at: %w", err)
	}
	lineage.CreatedAt = created

	updatedAt, err := parseTime(updated)
	if err != nil {
		return domain.ImageLineage{}, fmt.Errorf("parse lineage updated_at: %w", err)
	}
	lineage.UpdatedAt = updatedAt
	return lineage, nil
}

// validateLineage refuses a row the schema would refuse, with a better message.
//
// The CHECK constraints in 0022 are the real gate; this exists so a caller gets
// "a tracked lineage needs a tracking reference" rather than a driver's
// constraint text, and so the bound on stored reference length is applied on the
// way in rather than discovered on the way out.
func validateLineage(lineage domain.ImageLineage) error {
	if strings.TrimSpace(lineage.ContainerName) == "" {
		return errors.New("write image lineage: no container name")
	}
	if !domain.ValidContainerName(lineage.ContainerName) {
		return errors.New("write image lineage: the container name is not acceptable")
	}
	if !domain.ValidLineageState(string(lineage.State)) {
		return fmt.Errorf("write image lineage: unknown state %q", lineage.State)
	}
	if !domain.ValidLineageOrigin(string(lineage.Origin)) {
		return fmt.Errorf("write image lineage: unknown origin %q", lineage.Origin)
	}
	if lineage.State == domain.LineageTracked &&
		(lineage.TrackingReference == "" || lineage.Repository == "") {
		return errors.New("write image lineage: a tracked lineage needs a tracking reference and a repository")
	}
	// The tracking reference reaches this table from an observation, from an
	// approved plan, or from a validated label. Bounded anyway: this is the
	// column update discovery reads back and resolves against a registry.
	if len(lineage.TrackingReference) > domain.MaxLineageReferenceBytes ||
		len(lineage.TrackingFamiliar) > domain.MaxLineageReferenceBytes ||
		len(lineage.Repository) > domain.MaxLineageReferenceBytes {
		return errors.New("write image lineage: the tracking reference is too long")
	}
	if lineage.RunningDigest != "" && !domain.ValidImageDigest(lineage.RunningDigest) {
		return errors.New("write image lineage: the running digest is not a digest")
	}
	if lineage.UpdatedAt.IsZero() {
		return errors.New("write image lineage: no update time")
	}
	return nil
}

// LineageObservation is one container as the inventory currently sees it.
//
// The input to establishment and to reconciliation. Read in one query for the
// whole estate rather than one per container.
type LineageObservation struct {
	ContainerID   string
	ContainerName string
	ImageRef      string
	// ImageDigest is the digest carried by the DECLARED REFERENCE, which is set
	// only for a container created from a digest-pinned reference.
	ImageDigest string
	// RepoDigests are the registry manifest references the local image is known
	// by. For a TAG-created container this is the only place the running
	// image's digest can be found, and reconciliation cannot correct a lineage
	// record the host contradicts without it.
	//
	// Handed over raw. domain.RunningDigestFor decides what may be used --
	// exact repository match, ambiguity refused -- because that decision is the
	// same one every other consumer makes and must not be re-implemented here.
	RepoDigests   []string
	TrackingLabel string
}

// Observations returns every present container with what it declares.
//
// The label is read here so reconciliation can consider it, and is NOT trusted
// by this layer: domain.LineageFromLabel decides what it is worth, and refuses
// it outright for a container HarborMaster has no record of managing.
func (r *LineageRepository) Observations(ctx context.Context, limit int) ([]LineageObservation, error) {
	if limit < 1 || limit > MaxLineageRows {
		limit = MaxLineageRows
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT c.id, c.name, c.image_ref, c.image_digest,
		       COALESCE(im.repo_digests, '[]'),
		       COALESCE(l.value, '')
		FROM containers c
		LEFT JOIN images im ON im.id = c.image_id
		LEFT JOIN container_labels l
		       ON l.container_id = c.id AND l.key = ?
		WHERE c.present = 1 AND c.name <> ''
		ORDER BY c.name
		LIMIT ?`, domain.LineageLabel, limit)
	if err != nil {
		return nil, fmt.Errorf("query lineage observations: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	observations := make([]LineageObservation, 0, 64)
	for rows.Next() {
		var (
			observation LineageObservation
			repoDigests string
		)
		if err := rows.Scan(&observation.ContainerID, &observation.ContainerName,
			&observation.ImageRef, &observation.ImageDigest,
			&repoDigests, &observation.TrackingLabel); err != nil {
			return nil, fmt.Errorf("read lineage observation: %w", AsError(err))
		}
		// A malformed column is an absent list rather than a failure: the
		// container is still worth reconciling, it just has no local digest to
		// establish what it runs, and an unknown digest is a handled outcome.
		if repoDigests != "" {
			_ = json.Unmarshal([]byte(repoDigests), &observation.RepoDigests)
		}
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read lineage observations: %w", AsError(err))
	}
	return observations, nil
}

// PruneAbsent removes lineage for containers that are no longer present.
//
// Bounded per call. A container that has left the estate has nothing to follow,
// and keeping the row would have update discovery resolve a registry reference
// on behalf of a workload that does not exist.
func (r *LineageRepository) PruneAbsent(ctx context.Context, batch int) (int64, error) {
	if batch < 1 {
		batch = 500
	}
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM image_lineage
		WHERE container_name IN (
			SELECT l.container_name
			FROM image_lineage l
			LEFT JOIN containers c
			       ON c.name = l.container_name AND c.present = 1
			WHERE c.id IS NULL
			LIMIT ?
		)`, batch)
	if err != nil {
		return 0, fmt.Errorf("prune image lineage: %w", AsError(err))
	}
	removed, err := result.RowsAffected()
	if err != nil {
		// The delete SUCCEEDED; only the count is unavailable. Reporting a
		// failure here would have a caller retry work that is already done.
		//nolint:nilerr // the prune happened; only its count is unknown.
		return 0, nil
	}
	return removed, nil
}
