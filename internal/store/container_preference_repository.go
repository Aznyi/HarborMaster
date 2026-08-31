package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// ContainerPreferenceRepository stores one chosen update behaviour per
// container.
//
// # Keyed by NAME, deliberately
//
// A preference has to survive the update it authorises. A successful recreation
// gives the container a new Docker id, so a preference keyed on the id would be
// discarded by the first update it permitted. The name is the identity
// `automation_pauses` and `image_lineage` already use for exactly this reason,
// and this table follows that rule rather than inventing a third.
//
// The container id is stored as evidence of what was observed when the row was
// written, and nothing resolves a preference by it.
//
// # Nothing here reaches Docker
//
// This repository writes one row. Setting a preference performs no container
// operation of any kind -- that is the property C2 exists to guarantee, and it
// is structural: there is no Docker capability on this type to misuse.
type ContainerPreferenceRepository struct {
	db *sql.DB
}

const selectContainerPreferenceColumns = `
	SELECT container_name, behavior, container_id, set_by_username, created_at, updated_at
	  FROM container_update_preferences`

// SetContainerPreference records an operator's choice for one container.
//
// Upsert on the name, so choosing twice edits one row rather than accumulating
// rows whose order would decide the answer.
func (r *ContainerPreferenceRepository) SetContainerPreference(
	ctx context.Context,
	preference domain.ContainerUpdatePreference,
	actorID, actorName string,
	now time.Time,
) (domain.ContainerUpdatePreference, error) {
	stamp := formatTime(now.UTC())
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO container_update_preferences
			(container_name, behavior, container_id, set_by_user_id, set_by_username,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (host_id, container_name) DO UPDATE SET
			behavior        = excluded.behavior,
			container_id    = excluded.container_id,
			set_by_user_id  = excluded.set_by_user_id,
			set_by_username = excluded.set_by_username,
			updated_at      = excluded.updated_at`,
		preference.ContainerName, string(preference.Behavior), preference.ContainerID,
		actorID, actorName, stamp, stamp)
	if err != nil {
		return domain.ContainerUpdatePreference{}, fmt.Errorf(
			"set container update preference: %w", AsError(err))
	}
	return r.ContainerPreference(ctx, preference.ContainerName)
}

// ClearContainerPreference removes one container's choice.
//
// The container returns to whatever the governing policy says, which is the
// same state it was in before anybody chose. Clearing a preference that does
// not exist is not an error: the caller asked for "no preference" and that is
// what they have.
func (r *ContainerPreferenceRepository) ClearContainerPreference(
	ctx context.Context,
	containerName string,
) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM container_update_preferences WHERE container_name = ?`, containerName)
	if err != nil {
		return fmt.Errorf("clear container update preference: %w", AsError(err))
	}
	return nil
}

// ContainerPreference reads one container's choice.
//
// Returns ErrNotFound when nobody has chosen, which the caller must treat as
// "no restriction" rather than as a failure.
func (r *ContainerPreferenceRepository) ContainerPreference(
	ctx context.Context,
	containerName string,
) (domain.ContainerUpdatePreference, error) {
	row := r.db.QueryRowContext(ctx,
		selectContainerPreferenceColumns+` WHERE container_name = ?`, containerName)

	var preference domain.ContainerUpdatePreference
	var behavior string
	err := row.Scan(&preference.ContainerName, &behavior, &preference.ContainerID,
		&preference.SetByUsername, &preference.CreatedAt, &preference.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ContainerUpdatePreference{}, ErrNotFound
	}
	if err != nil {
		return domain.ContainerUpdatePreference{}, fmt.Errorf(
			"read container update preference: %w", AsError(err))
	}
	preference.Behavior = domain.UpdateBehavior(behavior)
	return preference, nil
}

// ContainerPreferences reads every stored choice, by container name.
//
// One query for the whole estate. The automation pass needs a preference per
// container it considers, and asking per container would be the N+1 pattern on
// the path that runs on a timer.
//
// Bounded by maxContainerPreferences: an estate cannot have more preferences
// than it has containers, and an unbounded load on every pass is how a
// scheduler becomes the thing that takes a host down.
func (r *ContainerPreferenceRepository) ContainerPreferences(
	ctx context.Context,
) (map[string]domain.UpdateBehavior, error) {
	rows, err := r.db.QueryContext(ctx,
		selectContainerPreferenceColumns+` ORDER BY container_name LIMIT ?`,
		maxContainerPreferences)
	if err != nil {
		return nil, fmt.Errorf("read container update preferences: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]domain.UpdateBehavior)
	for rows.Next() {
		var preference domain.ContainerUpdatePreference
		var behavior string
		if err := rows.Scan(&preference.ContainerName, &behavior, &preference.ContainerID,
			&preference.SetByUsername, &preference.CreatedAt, &preference.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan container update preference: %w", err)
		}
		out[preference.ContainerName] = domain.UpdateBehavior(behavior)
	}
	return out, rows.Err()
}

// ListContainerPreferences reads every stored choice in full, for the summary
// an operator reads.
func (r *ContainerPreferenceRepository) ListContainerPreferences(
	ctx context.Context,
) ([]domain.ContainerUpdatePreference, error) {
	rows, err := r.db.QueryContext(ctx,
		selectContainerPreferenceColumns+` ORDER BY container_name LIMIT ?`,
		maxContainerPreferences)
	if err != nil {
		return nil, fmt.Errorf("list container update preferences: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	var out []domain.ContainerUpdatePreference
	for rows.Next() {
		var preference domain.ContainerUpdatePreference
		var behavior string
		if err := rows.Scan(&preference.ContainerName, &behavior, &preference.ContainerID,
			&preference.SetByUsername, &preference.CreatedAt, &preference.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan container update preference: %w", err)
		}
		preference.Behavior = domain.UpdateBehavior(behavior)
		out = append(out, preference)
	}
	return out, rows.Err()
}

// maxContainerPreferences bounds every read of this table.
const maxContainerPreferences = 10000

// ContainerPreferences on AutomationRepository, so the automation pass reads
// its per-container data through the one repository it already holds.
//
// The same query as ContainerPreferenceRepository.ContainerPreferences, reached
// from the type the scheduler was already given. Adding a second repository to
// the engine's composition would widen what the engine can reach for no gain --
// it needs one map, once a pass.
func (r *AutomationRepository) ContainerPreferences(
	ctx context.Context,
) (map[string]domain.UpdateBehavior, error) {
	return (&ContainerPreferenceRepository{db: r.db}).ContainerPreferences(ctx)
}

// ContainerPreferenceRow is one stored choice, resolved against the inventory.
//
// A preference is keyed by container NAME so it survives a recreation, which
// also means a row can outlive the container it describes. Resolving here --
// once, in SQL -- is what lets a caller tell an ACTIVE preference from a saved
// one whose container is gone, without asking the inventory per row.
type ContainerPreferenceRow struct {
	domain.ContainerUpdatePreference
	// Present is true when a container of this name is currently on the host.
	Present bool
	// CurrentContainerID is that container's id NOW, empty when it is absent.
	//
	// Deliberately not the id stored on the preference: that one is evidence of
	// what was observed when the choice was made, and a recreation has almost
	// certainly changed it. A link built from the stored id would 404 for
	// exactly the containers that have been updated most.
	CurrentContainerID string
}

// ListContainerPreferencesWithPresence reads every stored choice and says, for
// each, whether its container is still here.
//
// ONE query. The obvious implementation -- list the preferences, then look up
// each container -- is the N+1 pattern on a page an operator opens to get an
// overview, and the index on (present, name) is what makes the join cheap.
//
// A container name is not unique across rows: an absent container keeps its row
// after a recreation, so the same name can appear twice. MAX(id) over the
// PRESENT rows picks one deterministically, matching how the rest of this
// package resolves "the container by that name now".
func (r *ContainerPreferenceRepository) ListContainerPreferencesWithPresence(
	ctx context.Context,
) ([]ContainerPreferenceRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT p.container_name, p.behavior, p.container_id,
		       p.set_by_username, p.created_at, p.updated_at,
		       COALESCE((SELECT MAX(c.id) FROM containers c
		                  WHERE c.name = p.container_name AND c.present = 1), '')
		  FROM container_update_preferences p
		 ORDER BY p.container_name
		 LIMIT ?`, maxContainerPreferences)
	if err != nil {
		return nil, fmt.Errorf("list container update preferences: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	var out []ContainerPreferenceRow
	for rows.Next() {
		var row ContainerPreferenceRow
		var behavior string
		if err := rows.Scan(&row.ContainerName, &behavior, &row.ContainerID,
			&row.SetByUsername, &row.CreatedAt, &row.UpdatedAt,
			&row.CurrentContainerID); err != nil {
			return nil, fmt.Errorf("scan container update preference: %w", err)
		}
		row.Behavior = domain.UpdateBehavior(behavior)
		row.Present = row.CurrentContainerID != ""
		out = append(out, row)
	}
	return out, rows.Err()
}
