package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// InventoryRepository owns the transactional lifecycle of an inventory
// refresh, plus the aggregate queries the inventory endpoint serves.
type InventoryRepository struct {
	db *sql.DB
}

// ContainerRecord is one container as produced by a refresh.
type ContainerRecord struct {
	Detail domain.ContainerDetail
	// RawJSON is the redacted raw inspection payload, or nil when inspection
	// failed and only summary data was available.
	RawJSON []byte
}

// RefreshCommit is everything one completed refresh wants to persist.
type RefreshCommit struct {
	Host       domain.Host
	Containers []ContainerRecord
	Images     []domain.Image
	Networks   []domain.Network
	Volumes    []domain.Volume
	Warnings   []domain.InventoryWarning
	Record     domain.RefreshRecord
	// AbsentRetention purges containers that have been absent for longer than
	// this. Zero keeps them indefinitely.
	AbsentRetention time.Duration
	// Now is injectable so tests get deterministic timestamps.
	Now time.Time
}

// CommitRefresh persists a completed refresh in a single transaction.
//
// Either the whole inventory is replaced or none of it is. A refresh that
// fails part-way leaves the previously persisted inventory untouched and still
// served, which is the entire reason this is one transaction rather than a
// sequence of writes.
//
// The generation is allocated inside the transaction, so it advances only when
// the commit actually succeeds.
func (r *InventoryRepository) CommitRefresh(ctx context.Context, commit RefreshCommit) (domain.RefreshRecord, error) {
	if commit.Now.IsZero() {
		commit.Now = time.Now().UTC()
	}
	now := commit.Now.UTC()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.RefreshRecord{}, fmt.Errorf("begin refresh transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	generation, err := nextGeneration(ctx, tx)
	if err != nil {
		return domain.RefreshRecord{}, err
	}

	if err := upsertHost(ctx, tx, commit.Host, now); err != nil {
		return domain.RefreshRecord{}, err
	}
	if err := upsertImages(ctx, tx, commit.Images, generation, now); err != nil {
		return domain.RefreshRecord{}, err
	}
	if err := upsertNetworks(ctx, tx, commit.Networks, generation, now); err != nil {
		return domain.RefreshRecord{}, err
	}
	if err := upsertVolumes(ctx, tx, commit.Volumes, generation, now); err != nil {
		return domain.RefreshRecord{}, err
	}
	for _, record := range commit.Containers {
		if err := upsertContainer(ctx, tx, record, generation, now); err != nil {
			return domain.RefreshRecord{}, err
		}
	}

	// Anything not touched by this refresh is no longer on the host. Marked
	// absent rather than deleted, so warnings and observed lifetimes survive.
	if _, err := tx.ExecContext(ctx,
		`UPDATE containers SET present = 0 WHERE host_id = ? AND generation < ?`,
		commit.Host.ID, generation); err != nil {
		return domain.RefreshRecord{}, fmt.Errorf("mark absent containers: %w", err)
	}

	if commit.AbsentRetention > 0 {
		cutoff := now.Add(-commit.AbsentRetention)
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM containers WHERE present = 0 AND last_seen_at < ?`,
			formatTime(cutoff)); err != nil {
			return domain.RefreshRecord{}, fmt.Errorf("purge absent containers: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM images WHERE generation < ? AND last_seen_at < ?`,
			generation, formatTime(cutoff)); err != nil {
			return domain.RefreshRecord{}, fmt.Errorf("purge stale images: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM networks WHERE generation < ? AND last_seen_at < ?`,
			generation, formatTime(cutoff)); err != nil {
			return domain.RefreshRecord{}, fmt.Errorf("purge stale networks: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM volumes WHERE generation < ? AND last_seen_at < ?`,
			generation, formatTime(cutoff)); err != nil {
			return domain.RefreshRecord{}, fmt.Errorf("purge stale volumes: %w", err)
		}
	}

	for _, warning := range commit.Warnings {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO inventory_warnings
				(generation, container_id, container_name, code, message, occurred_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			generation, warning.ContainerID, warning.ContainerName,
			string(warning.Code), warning.Message, formatTime(warning.OccurredAt),
		); err != nil {
			return domain.RefreshRecord{}, fmt.Errorf("insert warning: %w", err)
		}
	}

	// Warning history is bounded: only the current and previous generation are
	// kept, so a host that warns on every refresh cannot grow the table
	// without limit.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM inventory_warnings WHERE generation < ?`, generation-1); err != nil {
		return domain.RefreshRecord{}, fmt.Errorf("purge old warnings: %w", err)
	}

	record := commit.Record
	record.Generation = generation
	record.State = domain.RefreshSucceeded
	finished := now
	record.FinishedAt = &finished

	id, err := insertRefreshRecord(ctx, tx, commit.Host.ID, record)
	if err != nil {
		return domain.RefreshRecord{}, err
	}
	record.ID = id

	if err := tx.Commit(); err != nil {
		return domain.RefreshRecord{}, fmt.Errorf("commit refresh: %w", err)
	}
	return record, nil
}

// RecordFailure persists a refresh that could not complete.
//
// Written outside the inventory transaction on purpose: the point is to record
// the failure without touching the inventory the previous refresh left behind.
// The generation is not advanced.
func (r *InventoryRepository) RecordFailure(ctx context.Context, hostID string, record domain.RefreshRecord) (domain.RefreshRecord, error) {
	current, _, err := r.CurrentGeneration(ctx)
	if err != nil {
		return domain.RefreshRecord{}, err
	}

	// The failed attempt is tagged with the generation still in force, making
	// it obvious that it did not produce a new one.
	record.Generation = current
	record.State = domain.RefreshFailed

	// A host row must exist for the foreign key; a refresh can fail before the
	// host was ever recorded.
	if err := r.ensureHost(ctx, hostID); err != nil {
		return domain.RefreshRecord{}, err
	}

	id, err := insertRefreshRecord(ctx, r.db, hostID, record)
	if err != nil {
		return domain.RefreshRecord{}, err
	}
	record.ID = id
	return record, nil
}

func (r *InventoryRepository) ensureHost(ctx context.Context, hostID string) error {
	now := formatTime(time.Now().UTC())
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO hosts (id, name, runtime, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		hostID, hostID, domain.RuntimeDocker, now, now)
	if err != nil {
		return fmt.Errorf("ensure host: %w", err)
	}
	return nil
}

// CurrentGeneration returns the generation and checksum of the newest
// successfully persisted inventory. Returns (0, "") when none exists.
func (r *InventoryRepository) CurrentGeneration(ctx context.Context) (int64, string, error) {
	var (
		generation sql.NullInt64
		checksum   sql.NullString
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT generation, checksum FROM inventory_refreshes
		WHERE state = 'succeeded'
		ORDER BY generation DESC LIMIT 1`).Scan(&generation, &checksum)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, "", nil
	case err != nil:
		return 0, "", fmt.Errorf("read current generation: %w", err)
	}
	return generation.Int64, checksum.String, nil
}

// LastRefresh returns the most recent refresh record. When succeededOnly is
// true it returns the most recent successful one instead.
func (r *InventoryRepository) LastRefresh(ctx context.Context, succeededOnly bool) (*domain.RefreshRecord, error) {
	query := `
		SELECT id, generation, trigger, state, started_at, finished_at, duration_ms,
		       containers_listed, containers_inspected, containers_failed,
		       images_inspected, networks_listed, volumes_listed, warning_count,
		       checksum, error
		FROM inventory_refreshes`
	if succeededOnly {
		query += ` WHERE state = 'succeeded'`
	}
	query += ` ORDER BY id DESC LIMIT 1`

	record, err := scanRefreshRecord(r.db.QueryRowContext(ctx, query))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return record, nil
}

// Counts returns the headline tally of the persisted inventory.
//
// One aggregate query per entity rather than one per container: the dashboard
// must not become O(containers) in queries.
func (r *InventoryRepository) Counts(ctx context.Context) (domain.InventoryCounts, error) {
	counts := domain.InventoryCounts{ByState: map[domain.ContainerState]int{}}

	rows, err := r.db.QueryContext(ctx,
		`SELECT state, COUNT(*) FROM containers WHERE present = 1 GROUP BY state`)
	if err != nil {
		return counts, fmt.Errorf("count containers by state: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			state string
			count int
		)
		if err := rows.Scan(&state, &count); err != nil {
			return counts, err
		}
		counts.ByState[domain.ContainerState(state)] = count
		counts.Containers += count

		switch domain.ContainerState(state) {
		case domain.StateRunning:
			counts.Running = count
		case domain.StatePaused:
			counts.Paused = count
		case domain.StateRestarting:
			counts.Restarting = count
		case domain.StateExited, domain.StateDead:
			// "Stopped" is what an operator means by exited-or-dead; ByState
			// keeps the exact split available.
			counts.Stopped += count
		}
	}
	if err := rows.Err(); err != nil {
		return counts, err
	}

	scalars := []struct {
		query  string
		target *int
	}{
		{`SELECT COUNT(*) FROM containers WHERE present = 0`, &counts.Absent},
		{`SELECT COUNT(*) FROM containers WHERE present = 1 AND health = 'healthy'`, &counts.Healthy},
		{`SELECT COUNT(*) FROM containers WHERE present = 1 AND health = 'unhealthy'`, &counts.Unhealthy},
		{`SELECT COUNT(*) FROM images`, &counts.Images},
		{`SELECT COUNT(*) FROM networks`, &counts.Networks},
		{`SELECT COUNT(*) FROM volumes`, &counts.Volumes},
	}
	for _, scalar := range scalars {
		if err := r.db.QueryRowContext(ctx, scalar.query).Scan(scalar.target); err != nil {
			return counts, fmt.Errorf("count inventory entities: %w", err)
		}
	}

	generation, _, err := r.CurrentGeneration(ctx)
	if err != nil {
		return counts, err
	}
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM inventory_warnings WHERE generation = ?`, generation).
		Scan(&counts.Warnings); err != nil {
		return counts, fmt.Errorf("count warnings: %w", err)
	}
	return counts, nil
}

// Warnings returns the warnings recorded for a generation, newest first.
func (r *InventoryRepository) Warnings(ctx context.Context, generation int64, limit int) ([]domain.InventoryWarning, error) {
	if limit <= 0 || limit > maxPageSize {
		limit = 50
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, generation, container_id, container_name, code, message, occurred_at
		FROM inventory_warnings
		WHERE generation = ?
		ORDER BY id DESC LIMIT ?`, generation, limit)
	if err != nil {
		return nil, fmt.Errorf("query warnings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	warnings := make([]domain.InventoryWarning, 0)
	for rows.Next() {
		var (
			warning    domain.InventoryWarning
			code       string
			occurredAt string
		)
		if err := rows.Scan(&warning.ID, &warning.Generation, &warning.ContainerID,
			&warning.ContainerName, &code, &warning.Message, &occurredAt); err != nil {
			return nil, err
		}
		warning.Code = domain.WarningCode(code)
		if parsed, err := parseTime(occurredAt); err == nil {
			warning.OccurredAt = parsed
		}
		warnings = append(warnings, warning)
	}
	return warnings, rows.Err()
}

// WarningsForContainer returns the current generation's warnings for one
// container.
func (r *InventoryRepository) WarningsForContainer(ctx context.Context, containerID string) ([]domain.InventoryWarning, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, generation, container_id, container_name, code, message, occurred_at
		FROM inventory_warnings
		WHERE container_id = ?
		ORDER BY generation DESC, id DESC
		LIMIT 50`, containerID)
	if err != nil {
		return nil, fmt.Errorf("query container warnings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	warnings := make([]domain.InventoryWarning, 0)
	for rows.Next() {
		var (
			warning    domain.InventoryWarning
			code       string
			occurredAt string
		)
		if err := rows.Scan(&warning.ID, &warning.Generation, &warning.ContainerID,
			&warning.ContainerName, &code, &warning.Message, &occurredAt); err != nil {
			return nil, err
		}
		warning.Code = domain.WarningCode(code)
		if parsed, err := parseTime(occurredAt); err == nil {
			warning.OccurredAt = parsed
		}
		warnings = append(warnings, warning)
	}
	return warnings, rows.Err()
}

// ---------------------------------------------------------------- helpers --

// execer is satisfied by both *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func nextGeneration(ctx context.Context, tx *sql.Tx) (int64, error) {
	var current sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT MAX(generation) FROM inventory_refreshes WHERE state = 'succeeded'`).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("allocate generation: %w", err)
	}
	return current.Int64 + 1, nil
}

func insertRefreshRecord(ctx context.Context, db execer, hostID string, record domain.RefreshRecord) (int64, error) {
	result, err := db.ExecContext(ctx, `
		INSERT INTO inventory_refreshes
			(host_id, generation, trigger, state, started_at, finished_at, duration_ms,
			 containers_listed, containers_inspected, containers_failed,
			 images_inspected, networks_listed, volumes_listed, warning_count,
			 checksum, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hostID, record.Generation, string(record.Trigger), string(record.State),
		formatTime(record.StartedAt), nullableTime(record.FinishedAt), record.DurationMS,
		record.ContainersListed, record.ContainersInspected, record.ContainersFailed,
		record.ImagesInspected, record.NetworksListed, record.VolumesListed,
		record.WarningCount, record.Checksum, record.Error)
	if err != nil {
		return 0, fmt.Errorf("insert refresh record: %w", err)
	}
	return result.LastInsertId()
}

func scanRefreshRecord(row *sql.Row) (*domain.RefreshRecord, error) {
	var (
		record     domain.RefreshRecord
		trigger    string
		state      string
		startedAt  string
		finishedAt sql.NullString
	)
	if err := row.Scan(&record.ID, &record.Generation, &trigger, &state,
		&startedAt, &finishedAt, &record.DurationMS,
		&record.ContainersListed, &record.ContainersInspected, &record.ContainersFailed,
		&record.ImagesInspected, &record.NetworksListed, &record.VolumesListed,
		&record.WarningCount, &record.Checksum, &record.Error); err != nil {
		return nil, err
	}

	record.Trigger = domain.RefreshTrigger(trigger)
	record.State = domain.RefreshState(state)
	if parsed, err := parseTime(startedAt); err == nil {
		record.StartedAt = parsed
	}
	record.FinishedAt = scanOptionalTime(finishedAt)
	return &record, nil
}

func upsertHost(ctx context.Context, tx *sql.Tx, host domain.Host, now time.Time) error {
	if host.ID == "" {
		host.ID = domain.LocalHostID
	}
	if host.Runtime == "" {
		host.Runtime = domain.RuntimeDocker
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO hosts (id, name, runtime, api_version, os_type, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			name        = excluded.name,
			runtime     = excluded.runtime,
			api_version = excluded.api_version,
			os_type     = excluded.os_type,
			updated_at  = excluded.updated_at`,
		host.ID, host.Name, host.Runtime, host.APIVersion, host.OSType,
		formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("upsert host: %w", err)
	}
	return nil
}

// upsertImages writes image metadata, keyed by content-addressable ID so the
// same image observed across refreshes updates one row.
func upsertImages(ctx context.Context, tx *sql.Tx, images []domain.Image, generation int64, now time.Time) error {
	for _, image := range images {
		if image.ID == "" {
			continue
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO images
				(id, short_id, repo_tags, repo_digests, created_at, architecture, os,
				 os_version, variant, size, author, comment, labels,
				 first_seen_at, last_seen_at, generation)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				short_id     = excluded.short_id,
				repo_tags    = excluded.repo_tags,
				repo_digests = excluded.repo_digests,
				created_at   = excluded.created_at,
				architecture = excluded.architecture,
				os           = excluded.os,
				os_version   = excluded.os_version,
				variant      = excluded.variant,
				size         = excluded.size,
				author       = excluded.author,
				comment      = excluded.comment,
				labels       = excluded.labels,
				last_seen_at = excluded.last_seen_at,
				generation   = excluded.generation`,
			image.ID, image.ShortID, marshalStrings(image.RepoTags),
			marshalStrings(image.RepoDigests), timeOrNil(image.CreatedAt),
			image.Architecture, image.OS, image.OSVersion, image.Variant,
			image.Size, image.Author, image.Comment, marshalStringMap(image.Labels),
			formatTime(now), formatTime(now), generation)
		if err != nil {
			return fmt.Errorf("upsert image: %w", err)
		}
	}
	return nil
}

func upsertNetworks(ctx context.Context, tx *sql.Tx, networks []domain.Network, generation int64, now time.Time) error {
	for _, network := range networks {
		if network.ID == "" {
			continue
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO networks
				(id, name, driver, scope, internal, attachable, ipv6, created_at,
				 labels, subnets, last_seen_at, generation)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				name         = excluded.name,
				driver       = excluded.driver,
				scope        = excluded.scope,
				internal     = excluded.internal,
				attachable   = excluded.attachable,
				ipv6         = excluded.ipv6,
				created_at   = excluded.created_at,
				labels       = excluded.labels,
				subnets      = excluded.subnets,
				last_seen_at = excluded.last_seen_at,
				generation   = excluded.generation`,
			network.ID, network.Name, network.Driver, network.Scope,
			network.Internal, network.Attachable, network.IPv6,
			timeOrNil(network.CreatedAt), marshalStringMap(network.Labels),
			marshalStrings(network.Subnets), formatTime(now), generation)
		if err != nil {
			return fmt.Errorf("upsert network: %w", err)
		}
	}
	return nil
}

func upsertVolumes(ctx context.Context, tx *sql.Tx, volumes []domain.Volume, generation int64, now time.Time) error {
	for _, volume := range volumes {
		if volume.Name == "" {
			continue
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO volumes
				(name, driver, scope, mountpoint, created_at, labels, options,
				 last_seen_at, generation)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET
				driver       = excluded.driver,
				scope        = excluded.scope,
				mountpoint   = excluded.mountpoint,
				created_at   = excluded.created_at,
				labels       = excluded.labels,
				options      = excluded.options,
				last_seen_at = excluded.last_seen_at,
				generation   = excluded.generation`,
			volume.Name, volume.Driver, volume.Scope, volume.Mountpoint,
			timeOrNil(volume.CreatedAt), marshalStringMap(volume.Labels),
			marshalStringMap(volume.Options), formatTime(now), generation)
		if err != nil {
			return fmt.Errorf("upsert volume: %w", err)
		}
	}
	return nil
}

// upsertContainer writes one container and replaces its child rows.
//
// Child rows are deleted and re-inserted rather than merged: a container's
// labels, networks, and mounts are a set, and diffing them would risk leaving
// a stale row behind that the UI would then present as current.
// canonicalImageRef derives the intelligence key from a declared reference.
//
// The ONE place raw becomes canonical on the write path, so the column can
// never disagree with the reference it is derived from. Empty when
// domain.NormalizeImageRef refuses: an image built on this host, or a reference
// naming no registry. Empty asserts nothing rather than guessing an identity --
// see migration 0033.
func canonicalImageRef(raw string) string {
	if raw == "" {
		return ""
	}
	reference, err := domain.NormalizeImageRef(raw)
	if err != nil {
		return ""
	}
	return reference.Canonical
}

func upsertContainer(ctx context.Context, tx *sql.Tx, record ContainerRecord, generation int64, now time.Time) error {
	overview := record.Detail.Overview
	if overview.ID == "" {
		return errors.New("container record has no id")
	}

	portsJSON, err := marshalJSON(overview.Ports)
	if err != nil {
		return fmt.Errorf("encode ports: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO containers
			(id, host_id, short_id, name, image_id, image_ref, image_canonical,
			 image_repository,
			 image_tag, image_digest, state, raw_state, status, health,
			 created_at, started_at, finished_at, exit_code, restart_count,
			 restart_policy_name, restart_policy_max_retry,
			 compose_project, compose_service, compose_container_number, compose_oneoff,
			 hm_enabled, ports, present, first_seen_at, last_seen_at, generation, warning_count,
			 network_mode, ipc_mode, pid_mode, namespaces_observed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT (id) DO UPDATE SET
			short_id                 = excluded.short_id,
			name                     = excluded.name,
			image_id                 = excluded.image_id,
			image_ref                = excluded.image_ref,
			-- Rewritten with the raw reference it is derived from, never
			-- independently. See migration 0033: this is a projection of
			-- image_ref, so the two cannot drift.
			image_canonical          = excluded.image_canonical,
			image_repository         = excluded.image_repository,
			image_tag                = excluded.image_tag,
			image_digest             = excluded.image_digest,
			state                    = excluded.state,
			raw_state                = excluded.raw_state,
			status                   = excluded.status,
			health                   = excluded.health,
			created_at               = excluded.created_at,
			started_at               = excluded.started_at,
			finished_at              = excluded.finished_at,
			exit_code                = excluded.exit_code,
			restart_count            = excluded.restart_count,
			restart_policy_name      = excluded.restart_policy_name,
			restart_policy_max_retry = excluded.restart_policy_max_retry,
			compose_project          = excluded.compose_project,
			compose_service          = excluded.compose_service,
			compose_container_number = excluded.compose_container_number,
			compose_oneoff           = excluded.compose_oneoff,
			hm_enabled               = excluded.hm_enabled,
			ports                    = excluded.ports,
			present                  = 1,
			last_seen_at             = excluded.last_seen_at,
			generation               = excluded.generation,
			warning_count            = excluded.warning_count,
			-- The namespace projection, refreshed with the row it describes.
			-- namespaces_observed goes to 1 unconditionally: reaching this
			-- statement means the container was inspected, which is the whole
			-- of what the flag asserts.
			network_mode             = excluded.network_mode,
			ipc_mode                 = excluded.ipc_mode,
			pid_mode                 = excluded.pid_mode,
			namespaces_observed      = 1`,
		overview.ID, hostIDOrDefault(overview.HostID), overview.ShortID, overview.Name,
		overview.ImageID, overview.Image.Raw, canonicalImageRef(overview.Image.Raw),
		overview.Image.Repository,
		overview.Image.Tag, overview.Image.Digest,
		string(overview.State), record.Detail.State.RawState, overview.Status,
		string(overview.Health), formatTime(overview.CreatedAt),
		nullableTime(overview.StartedAt), nullableTime(overview.FinishedAt),
		nullableInt(overview.ExitCode), overview.RestartCount,
		overview.RestartPolicy.Name, overview.RestartPolicy.MaximumRetryCount,
		overview.Compose.Project, overview.Compose.Service,
		overview.Compose.ContainerNumber, overview.Compose.OneOff,
		nullableBool(overview.HarborMaster.Enabled), portsJSON,
		formatTime(now), formatTime(now), generation, len(record.Detail.Warnings),
		record.Detail.Security.NetworkMode, record.Detail.Security.IPCMode,
		record.Detail.Security.PIDMode)
	if err != nil {
		return fmt.Errorf("upsert container: %w", err)
	}

	if err := upsertContainerConfig(ctx, tx, record, generation, now); err != nil {
		return err
	}
	if err := replaceContainerLabels(ctx, tx, overview.ID, record.Detail.Labels); err != nil {
		return err
	}
	if err := replaceContainerNetworks(ctx, tx, overview.ID, record.Detail.Networks); err != nil {
		return err
	}
	return replaceContainerMounts(ctx, tx, overview.ID, record.Detail.Mounts)
}

func hostIDOrDefault(hostID string) string {
	if hostID == "" {
		return domain.LocalHostID
	}
	return hostID
}

// persistedConfig is the subset of ContainerDetail stored as JSON.
//
// Environment values here are ALREADY MASKED: domain.EnvVar tags RawValue
// `json:"-"`, so encoding a detail cannot write a raw secret to disk even by
// accident. That is deliberate. HarborMaster does not persist secret values,
// which means a future recreate will need a separate, explicitly designed path
// for them rather than silently inheriting a copy from this table.
type persistedConfig struct {
	State        domain.StateDetail          `json:"state"`
	Process      domain.Process              `json:"process"`
	HealthCheck  *domain.HealthCheck         `json:"healthCheck,omitempty"`
	Environment  []domain.EnvVar             `json:"environment"`
	Resources    domain.Resources            `json:"resources"`
	Security     domain.Security             `json:"security"`
	Logging      domain.Logging              `json:"logging"`
	Compose      domain.ComposeMetadata      `json:"compose"`
	HarborMaster domain.HarborMasterMetadata `json:"harbormaster"`
}

func upsertContainerConfig(ctx context.Context, tx *sql.Tx, record ContainerRecord, generation int64, now time.Time) error {
	config := persistedConfig{
		State:        record.Detail.State,
		Process:      record.Detail.Process,
		HealthCheck:  record.Detail.HealthCheck,
		Environment:  record.Detail.Environment,
		Resources:    record.Detail.Resources,
		Security:     record.Detail.Security,
		Logging:      record.Detail.Logging,
		Compose:      record.Detail.Compose,
		HarborMaster: record.Detail.HarborMaster,
	}

	encoded, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode container config: %w", err)
	}

	var rawJSON any
	if len(record.RawJSON) > 0 {
		rawJSON = string(record.RawJSON)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO container_config (container_id, config_json, raw_json, updated_at, generation)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (container_id) DO UPDATE SET
			config_json = excluded.config_json,
			raw_json    = excluded.raw_json,
			updated_at  = excluded.updated_at,
			generation  = excluded.generation`,
		record.Detail.Overview.ID, string(encoded), rawJSON, formatTime(now), generation)
	if err != nil {
		return fmt.Errorf("upsert container config: %w", err)
	}
	return nil
}

func replaceContainerLabels(ctx context.Context, tx *sql.Tx, containerID string, labels []domain.Label) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM container_labels WHERE container_id = ?`, containerID); err != nil {
		return fmt.Errorf("clear container labels: %w", err)
	}
	for _, label := range labels {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO container_labels (container_id, key, value, source)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (container_id, key) DO UPDATE SET
				value = excluded.value, source = excluded.source`,
			containerID, label.Key, label.Value, string(label.Source)); err != nil {
			return fmt.Errorf("insert container label: %w", err)
		}
	}
	return nil
}

func replaceContainerNetworks(ctx context.Context, tx *sql.Tx, containerID string, attachments []domain.NetworkAttachment) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM container_networks WHERE container_id = ?`, containerID); err != nil {
		return fmt.Errorf("clear container networks: %w", err)
	}
	for _, attachment := range attachments {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO container_networks
				(container_id, network_name, network_id, aliases, ipv4_address,
				 ipv6_address, gateway, mac_address, endpoint_id, links)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (container_id, network_name) DO UPDATE SET
				network_id   = excluded.network_id,
				aliases      = excluded.aliases,
				ipv4_address = excluded.ipv4_address,
				ipv6_address = excluded.ipv6_address,
				gateway      = excluded.gateway,
				mac_address  = excluded.mac_address,
				endpoint_id  = excluded.endpoint_id,
				links        = excluded.links`,
			containerID, attachment.NetworkName, attachment.NetworkID,
			marshalStrings(attachment.Aliases), attachment.IPv4Address,
			attachment.IPv6Address, attachment.Gateway, attachment.MACAddress,
			attachment.EndpointID, marshalStrings(attachment.Links)); err != nil {
			return fmt.Errorf("insert container network: %w", err)
		}
	}
	return nil
}

func replaceContainerMounts(ctx context.Context, tx *sql.Tx, containerID string, mounts []domain.Mount) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM container_mounts WHERE container_id = ?`, containerID); err != nil {
		return fmt.Errorf("clear container mounts: %w", err)
	}
	for _, mount := range mounts {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO container_mounts
				(container_id, destination, type, source, read_only, propagation,
				 consistency, volume_name, driver, driver_options, tmpfs_options)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (container_id, destination) DO UPDATE SET
				type           = excluded.type,
				source         = excluded.source,
				read_only      = excluded.read_only,
				propagation    = excluded.propagation,
				consistency    = excluded.consistency,
				volume_name    = excluded.volume_name,
				driver         = excluded.driver,
				driver_options = excluded.driver_options,
				tmpfs_options  = excluded.tmpfs_options`,
			containerID, mount.Destination, string(mount.Type), mount.Source,
			mount.ReadOnly, mount.Propagation, mount.Consistency,
			mount.VolumeName, mount.Driver, marshalStringMap(mount.DriverOptions),
			mount.TmpfsOptions); err != nil {
			return fmt.Errorf("insert container mount: %w", err)
		}
	}
	return nil
}
