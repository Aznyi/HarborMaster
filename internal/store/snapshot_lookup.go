package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Existence lookups used by restore-readiness validation.
//
// These answer "is this resource still present" from HarborMaster's own
// inventory, never by calling Docker. Readiness is a read endpoint, and a read
// endpoint that can drive traffic against a privileged socket is a
// denial-of-service amplifier: one HTTP request would become one Docker call
// per referenced resource, multiplied by however many times a caller repeats it.
//
// The cost is that an answer is only as current as the last refresh, which is
// why every readiness report carries the inventory's age and degrades its
// verdict when that age is excessive.

// ImageExists reports whether an image is present in the current inventory.
//
// Matched by ID first and then by repository tag, because a snapshot may record
// either: an ID pins exactly one image, while a tag is what an operator wrote
// and may since have been repointed.
func (r *ImageRepository) ImageExists(ctx context.Context, imageID, reference string) (bool, error) {
	if imageID != "" {
		var found int
		err := r.db.QueryRowContext(ctx,
			`SELECT 1 FROM images WHERE id = ? LIMIT 1`, imageID).Scan(&found)
		switch {
		case err == nil:
			return true, nil
		case !errors.Is(err, sql.ErrNoRows):
			return false, fmt.Errorf("look up image by id: %w", err)
		}
	}

	if reference == "" {
		return false, nil
	}

	// repo_tags is a JSON array; a LIKE against it would match a tag that is
	// merely a prefix of another, so the reference is matched with its
	// surrounding quotes.
	var found int
	err := r.db.QueryRowContext(ctx,
		`SELECT 1 FROM images WHERE repo_tags LIKE ? LIMIT 1`,
		`%"`+reference+`"%`).Scan(&found)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("look up image by reference: %w", err)
	}
}

// NetworkNames returns every network name in the current inventory.
//
// The whole set in one query rather than one lookup per referenced network: a
// container attached to five networks would otherwise be five round trips
// against a database with a single writer.
func (r *NetworkRepository) NetworkNames(ctx context.Context) (map[string]struct{}, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT name FROM networks`)
	if err != nil {
		return nil, fmt.Errorf("list network names: %w", err)
	}
	defer func() { _ = rows.Close() }()

	names := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan network name: %w", err)
		}
		names[name] = struct{}{}
	}
	return names, rows.Err()
}

// VolumeNames returns every volume name in the current inventory.
func (r *VolumeRepository) VolumeNames(ctx context.Context) (map[string]struct{}, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT name FROM volumes`)
	if err != nil {
		return nil, fmt.Errorf("list volume names: %w", err)
	}
	defer func() { _ = rows.Close() }()

	names := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan volume name: %w", err)
		}
		names[name] = struct{}{}
	}
	return names, rows.Err()
}
