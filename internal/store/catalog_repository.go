package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// ImageUsage pairs an image with how many containers reference it.
type ImageUsage struct {
	Image domain.Image `json:"image"`
	// ContainerCount counts present containers running this image.
	ContainerCount int `json:"containerCount"`
}

// ImageRepository reads persisted image metadata.
type ImageRepository struct {
	db *sql.DB
}

const imageColumns = `
	i.id, i.short_id, i.repo_tags, i.repo_digests, i.created_at, i.architecture,
	i.os, i.os_version, i.variant, i.size, i.author, i.comment, i.labels`

// List returns a page of images with their reference counts.
//
// The count is a correlated subquery rather than a per-row follow-up: one
// query serves the page regardless of how many images it holds.
func (r *ImageRepository) List(ctx context.Context, page Page) ([]ImageUsage, int, error) {
	page = page.normalise()

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM images`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count images: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT`+imageColumns+`,
			(SELECT COUNT(*) FROM containers c WHERE c.image_id = i.id AND c.present = 1)
		FROM images i
		ORDER BY i.id
		LIMIT ? OFFSET ?`, page.Limit, page.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query images: %w", err)
	}
	defer func() { _ = rows.Close() }()

	usages := make([]ImageUsage, 0)
	for rows.Next() {
		usage, err := scanImageUsage(rows)
		if err != nil {
			return nil, 0, err
		}
		usages = append(usages, usage)
	}
	return usages, total, rows.Err()
}

// Get returns one image by full ID or unambiguous prefix.
func (r *ImageRepository) Get(ctx context.Context, id string) (*ImageUsage, error) {
	query := `
		SELECT` + imageColumns + `,
			(SELECT COUNT(*) FROM containers c WHERE c.image_id = i.id AND c.present = 1)
		FROM images i WHERE i.id = ?`

	usage, err := scanImageUsage(r.db.QueryRowContext(ctx, query, id))
	if err == nil {
		return &usage, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// Fall back to a prefix match, mirroring container ID resolution.
	rows, err := r.db.QueryContext(ctx, `
		SELECT`+imageColumns+`,
			(SELECT COUNT(*) FROM containers c WHERE c.image_id = i.id AND c.present = 1)
		FROM images i
		WHERE i.id LIKE ? ESCAPE '\' OR i.short_id = ?
		ORDER BY i.id LIMIT 2`, escapeLike(id)+"%", id)
	if err != nil {
		return nil, fmt.Errorf("query image by prefix: %w", err)
	}
	defer func() { _ = rows.Close() }()

	matches := make([]ImageUsage, 0, 2)
	for rows.Next() {
		usage, err := scanImageUsage(rows)
		if err != nil {
			return nil, err
		}
		matches = append(matches, usage)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	switch len(matches) {
	case 0:
		return nil, ErrNotFound
	case 1:
		return &matches[0], nil
	default:
		return nil, ErrAmbiguousID
	}
}

func scanImageUsage(row rowScanner) (ImageUsage, error) {
	var (
		usage       ImageUsage
		repoTags    string
		repoDigests string
		createdAt   sql.NullString
		labels      string
	)

	if err := row.Scan(&usage.Image.ID, &usage.Image.ShortID, &repoTags, &repoDigests,
		&createdAt, &usage.Image.Architecture, &usage.Image.OS, &usage.Image.OSVersion,
		&usage.Image.Variant, &usage.Image.Size, &usage.Image.Author,
		&usage.Image.Comment, &labels, &usage.ContainerCount); err != nil {
		return ImageUsage{}, err
	}

	usage.Image.RepoTags = unmarshalStrings(repoTags)
	usage.Image.RepoDigests = unmarshalStrings(repoDigests)
	usage.Image.Labels = unmarshalStringMap(labels)
	usage.Image.CreatedAt = scanTime(createdAt)

	if usage.Image.RepoTags == nil {
		usage.Image.RepoTags = []string{}
	}
	if usage.Image.RepoDigests == nil {
		usage.Image.RepoDigests = []string{}
	}
	return usage, nil
}

// NetworkRepository reads persisted network metadata.
type NetworkRepository struct {
	db *sql.DB
}

// List returns a page of networks.
func (r *NetworkRepository) List(ctx context.Context, page Page) ([]domain.Network, int, error) {
	page = page.normalise()

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM networks`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count networks: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, driver, scope, internal, attachable, ipv6, created_at, labels, subnets
		FROM networks ORDER BY name, id LIMIT ? OFFSET ?`, page.Limit, page.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query networks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	networks := make([]domain.Network, 0)
	for rows.Next() {
		var (
			network   domain.Network
			createdAt sql.NullString
			labels    string
			subnets   string
		)
		if err := rows.Scan(&network.ID, &network.Name, &network.Driver, &network.Scope,
			&network.Internal, &network.Attachable, &network.IPv6, &createdAt,
			&labels, &subnets); err != nil {
			return nil, 0, err
		}
		network.CreatedAt = scanTime(createdAt)
		network.Labels = unmarshalStringMap(labels)
		network.Subnets = unmarshalStrings(subnets)
		networks = append(networks, network)
	}
	return networks, total, rows.Err()
}

// VolumeRepository reads persisted volume metadata.
type VolumeRepository struct {
	db *sql.DB
}

// List returns a page of volumes.
func (r *VolumeRepository) List(ctx context.Context, page Page) ([]domain.Volume, int, error) {
	page = page.normalise()

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM volumes`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count volumes: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT name, driver, scope, mountpoint, created_at, labels, options
		FROM volumes ORDER BY name LIMIT ? OFFSET ?`, page.Limit, page.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query volumes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	volumes := make([]domain.Volume, 0)
	for rows.Next() {
		var (
			volume    domain.Volume
			createdAt sql.NullString
			labels    string
			options   string
		)
		if err := rows.Scan(&volume.Name, &volume.Driver, &volume.Scope,
			&volume.Mountpoint, &createdAt, &labels, &options); err != nil {
			return nil, 0, err
		}
		volume.CreatedAt = scanTime(createdAt)
		volume.Labels = unmarshalStringMap(labels)
		volume.Options = unmarshalStringMap(options)
		volumes = append(volumes, volume)
	}
	return volumes, total, rows.Err()
}
