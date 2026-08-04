package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
// # Append-only
//
// A snapshot is evidence. Nothing here updates a captured configuration, a
// checksum, an image identity, a trigger, or any metadata column. The single
// UPDATE statement in this file touches readiness_status and
// readiness_evaluated_at -- a denormalised cache of the newest row in
// snapshot_restore_checks -- and no other column. There is deliberately no
// general-purpose update method to misuse, and TestRepositoryUpdatesOnlyReadinessSummary
// fails the build if another UPDATE appears.
//
// Deleting whole snapshots under a configured retention policy is the only
// other write, and deleting one is not modifying one.
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

// snapshotSortFields is the allowlist of sortable columns.
//
// An allowlist rather than validation-by-rejection: the value becomes part of
// the SQL text, so the only safe design is one where caller input selects from
// a fixed set of literals rather than contributing to it.
var snapshotSortFields = map[string]string{
	"createdAt": "created_at",
	"id":        "id",
	"container": "container_name",
	"readiness": "readiness_status",
	"trigger":   "trigger",
}

// ValidSnapshotSortField reports whether field names a sortable column.
func ValidSnapshotSortField(field string) bool {
	_, ok := snapshotSortFields[field]
	return ok
}

// SnapshotSortFields lists the sortable fields, for error messages.
func SnapshotSortFields() []string {
	fields := make([]string, 0, len(snapshotSortFields))
	for field := range snapshotSortFields {
		fields = append(fields, field)
	}
	// Sorted so the message is stable.
	for i := 1; i < len(fields); i++ {
		for j := i; j > 0 && fields[j] < fields[j-1]; j-- {
			fields[j], fields[j-1] = fields[j-1], fields[j]
		}
	}
	return fields
}

// SnapshotFilter selects and orders a page of snapshots.
//
// Every enumerated value is validated by the caller against a closed domain
// vocabulary, and Sort against snapshotSortFields, so nothing caller-controlled
// reaches the SQL text as an identifier. Everything else travels as a bound
// parameter.
type SnapshotFilter struct {
	ContainerID string
	Triggers    []domain.SnapshotTrigger
	Readiness   []domain.ReadinessStatus
	Checksum    string
	Since       *time.Time
	Until       *time.Time

	Sort      string
	Direction SortDirection
	Page      Page
}

// Create stores a snapshot and its derived child rows in one transaction.
//
// Re-capturing an unchanged configuration is not an error and does not insert:
// the existing snapshot is returned with Deduplicated set. That is what keeps
// history meaningful instead of filling with identical captures, and it is the
// durable bound on database growth from repeated requests.
//
// The child rows are derived from the same document in the same transaction, so
// they can never drift from it.
func (r *SnapshotRepository) Create(
	ctx context.Context,
	snapshot domain.Snapshot,
	env []domain.SnapshotEnvEntry,
	mounts []domain.SnapshotMountRow,
	networks []domain.SnapshotNetworkRow,
) (domain.Snapshot, error) {
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = time.Now().UTC()
	}
	snapshot.CreatedAt = snapshot.CreatedAt.UTC()
	if snapshot.HostID == "" {
		snapshot.HostID = domain.LocalHostID
	}
	if snapshot.ReadinessStatus == "" {
		snapshot.ReadinessStatus = domain.ReadinessUnknown
	}

	warningsJSON, err := json.Marshal(snapshot.Warnings)
	if err != nil {
		return domain.Snapshot{}, fmt.Errorf("encode snapshot warnings: %w", err)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Snapshot{}, fmt.Errorf("begin snapshot transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const insertSnapshot = `
		INSERT INTO snapshots (
			host_id, container_id, container_name,
			image_reference, image_digest, image_id,
			spec_version, spec_json, checksum,
			harbormaster_version, docker_api_version, docker_engine_version,
			trigger, reason,
			inventory_generation, event_sequence,
			warning_count, warnings_json,
			readiness_status, digest_key_id, created_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (container_id, checksum) DO NOTHING
		RETURNING id`

	err = tx.QueryRowContext(ctx, insertSnapshot,
		snapshot.HostID, snapshot.ContainerID, snapshot.ContainerName,
		snapshot.ImageReference, snapshot.ImageDigest, snapshot.ImageID,
		snapshot.SpecVersion, []byte(snapshot.SpecJSON), snapshot.Checksum,
		snapshot.HarborMasterVersion, snapshot.DockerAPIVersion, snapshot.DockerEngineVersion,
		string(snapshot.Trigger), snapshot.Reason,
		snapshot.InventoryGeneration, snapshot.EventSequence,
		len(snapshot.Warnings), warningsJSON,
		string(snapshot.ReadinessStatus), snapshot.DigestKeyID, formatTime(snapshot.CreatedAt),
	).Scan(&snapshot.ID)

	if errors.Is(err, sql.ErrNoRows) {
		// Identical configuration already captured for this container. Roll
		// back and return what is already stored.
		_ = tx.Rollback()
		existing, getErr := r.getByChecksum(ctx, snapshot.ContainerID, snapshot.Checksum)
		if getErr != nil {
			return domain.Snapshot{}, getErr
		}
		existing.Deduplicated = true
		return existing, nil
	}
	if err != nil {
		return domain.Snapshot{}, fmt.Errorf("insert snapshot: %w", err)
	}

	if err := insertEnvironment(ctx, tx, snapshot.ID, env); err != nil {
		return domain.Snapshot{}, err
	}
	if err := insertMounts(ctx, tx, snapshot.ID, mounts); err != nil {
		return domain.Snapshot{}, err
	}
	if err := insertNetworks(ctx, tx, snapshot.ID, networks); err != nil {
		return domain.Snapshot{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Snapshot{}, fmt.Errorf("commit snapshot: %w", err)
	}
	snapshot.WarningCount = len(snapshot.Warnings)
	return snapshot, nil
}

func insertEnvironment(ctx context.Context, tx *sql.Tx, snapshotID int64, env []domain.SnapshotEnvEntry) error {
	if len(env) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO snapshot_environment
			(snapshot_id, position, key, classification, present, value,
			 value_length, digest, digest_algorithm, digest_key_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare environment insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, entry := range env {
		classification, value := classifyForStorage(entry)

		if _, err := stmt.ExecContext(ctx,
			snapshotID, entry.Position, entry.Key, string(classification),
			entry.Present, value, entry.Length,
			entry.Digest, string(entry.DigestAlgorithm), entry.DigestKeyID,
		); err != nil {
			return fmt.Errorf("insert snapshot environment: %w", err)
		}
	}
	return nil
}

// classifyForStorage decides what classification and value actually get
// written, independently of what the caller supplied.
//
// This is defence in depth behind the CHECK constraint and the capture logic. A
// sensitive entry's value is blanked here too, so a bug upstream cannot write a
// plaintext secret.
//
// An entry with NO classification is treated as sensitive, not as normal. The
// caller failing to classify means HarborMaster does not know whether the value
// is a secret, and the fail-closed answer to "is this safe to store" is no. The
// cost of being wrong in this direction is a value an operator cannot see; the
// cost of the other direction is a leaked credential, and only one of those is
// recoverable.
func classifyForStorage(entry domain.SnapshotEnvEntry) (domain.Sensitivity, string) {
	switch entry.Classification {
	case domain.SensitivityNormal:
		return domain.SensitivityNormal, entry.Value
	case domain.SensitivitySensitive:
		return domain.SensitivitySensitive, ""
	default:
		// Unknown or unset: withhold the value.
		return domain.SensitivitySensitive, ""
	}
}

func insertMounts(ctx context.Context, tx *sql.Tx, snapshotID int64, mounts []domain.SnapshotMountRow) error {
	if len(mounts) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO snapshot_mounts
			(snapshot_id, destination, type, source, read_only, volume_name, driver)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare mount insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, mount := range mounts {
		if _, err := stmt.ExecContext(ctx,
			snapshotID, mount.Destination, string(mount.Type), mount.Source,
			mount.ReadOnly, mount.VolumeName, mount.Driver,
		); err != nil {
			return fmt.Errorf("insert snapshot mount: %w", err)
		}
	}
	return nil
}

func insertNetworks(ctx context.Context, tx *sql.Tx, snapshotID int64, networks []domain.SnapshotNetworkRow) error {
	if len(networks) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO snapshot_networks (snapshot_id, network_name, aliases_json)
		VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare network insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, network := range networks {
		aliases, err := json.Marshal(network.Aliases)
		if err != nil {
			return fmt.Errorf("encode network aliases: %w", err)
		}
		if _, err := stmt.ExecContext(ctx, snapshotID, network.NetworkName, string(aliases)); err != nil {
			return fmt.Errorf("insert snapshot network: %w", err)
		}
	}
	return nil
}

const selectSnapshotColumns = `
	SELECT id, host_id, container_id, container_name,
	       image_reference, image_digest, image_id,
	       spec_version, spec_json, checksum,
	       harbormaster_version, docker_api_version, docker_engine_version,
	       trigger, reason,
	       inventory_generation, event_sequence,
	       warning_count, warnings_json,
	       readiness_status, readiness_evaluated_at,
	       digest_key_id, created_at`

// Get returns a snapshot by ID, or ErrNotFound.
func (r *SnapshotRepository) Get(ctx context.Context, id int64) (domain.Snapshot, error) {
	const query = selectSnapshotColumns + ` FROM snapshots WHERE id = ?`
	return scanSnapshotRow(r.db.QueryRowContext(ctx, query, id))
}

func (r *SnapshotRepository) getByChecksum(ctx context.Context, containerID, checksum string) (domain.Snapshot, error) {
	const query = selectSnapshotColumns + ` FROM snapshots WHERE container_id = ? AND checksum = ?`
	return scanSnapshotRow(r.db.QueryRowContext(ctx, query, containerID, checksum))
}

// List returns a filtered page of snapshots and the total matching count.
func (r *SnapshotRepository) List(ctx context.Context, filter SnapshotFilter) ([]domain.Snapshot, int, error) {
	where, args := filter.build()

	var total int
	countQuery := `SELECT COUNT(*) FROM snapshots` + where
	if err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count snapshots: %w", err)
	}

	page := filter.Page.normalise()
	column, ok := snapshotSortFields[filter.Sort]
	if !ok {
		column = "created_at"
	}
	direction := "DESC"
	if filter.Direction == SortAsc {
		direction = "ASC"
	}

	// column and direction come from the allowlist above and from a two-valued
	// switch; neither is caller text.
	query := selectSnapshotColumns + ` FROM snapshots` + where +
		` ORDER BY ` + column + ` ` + direction + `, id ` + direction +
		` LIMIT ? OFFSET ?`

	rows, err := r.db.QueryContext(ctx, query, append(args, page.Limit, page.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Non-nil empty slice: the API must serialise an empty result as [].
	snapshots := make([]domain.Snapshot, 0)
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			return nil, 0, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, total, rows.Err()
}

// build renders the WHERE clause and its bound arguments.
//
// Every caller-supplied value is a placeholder. The only text this method
// contributes is fixed SQL and the correct number of "?" separators.
func (f SnapshotFilter) build() (string, []any) {
	var (
		clauses []string
		args    []any
	)

	if f.ContainerID != "" {
		clauses = append(clauses, "container_id = ?")
		args = append(args, f.ContainerID)
	}
	if f.Checksum != "" {
		clauses = append(clauses, "checksum = ?")
		args = append(args, f.Checksum)
	}
	if len(f.Triggers) > 0 {
		placeholders := make([]string, len(f.Triggers))
		for i, trigger := range f.Triggers {
			placeholders[i] = "?"
			args = append(args, string(trigger))
		}
		clauses = append(clauses, "trigger IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(f.Readiness) > 0 {
		placeholders := make([]string, len(f.Readiness))
		for i, status := range f.Readiness {
			placeholders[i] = "?"
			args = append(args, string(status))
		}
		clauses = append(clauses, "readiness_status IN ("+strings.Join(placeholders, ", ")+")")
	}
	if f.Since != nil {
		clauses = append(clauses, "created_at >= ?")
		args = append(args, formatTime(*f.Since))
	}
	if f.Until != nil {
		clauses = append(clauses, "created_at <= ?")
		args = append(args, formatTime(*f.Until))
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// Count returns the total number of stored snapshots.
func (r *SnapshotRepository) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshots`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count snapshots: %w", err)
	}
	return n, nil
}

// Environment returns one snapshot's environment entries, in captured order.
func (r *SnapshotRepository) Environment(ctx context.Context, snapshotID int64) ([]domain.SnapshotEnvEntry, error) {
	byID, err := r.EnvironmentFor(ctx, []int64{snapshotID})
	if err != nil {
		return nil, err
	}
	entries := byID[snapshotID]
	if entries == nil {
		entries = []domain.SnapshotEnvEntry{}
	}
	return entries, nil
}

// EnvironmentFor loads environment entries for several snapshots at once.
//
// One query for the whole page rather than one per snapshot: rendering a list
// of fifty snapshots must not become fifty round trips against a database with
// a single writer.
func (r *SnapshotRepository) EnvironmentFor(ctx context.Context, snapshotIDs []int64) (map[int64][]domain.SnapshotEnvEntry, error) {
	result := make(map[int64][]domain.SnapshotEnvEntry, len(snapshotIDs))
	if len(snapshotIDs) == 0 {
		return result, nil
	}

	placeholders, args := idPlaceholders(snapshotIDs)
	query := `
		SELECT snapshot_id, position, key, classification, present, value,
		       value_length, digest, digest_algorithm, digest_key_id
		FROM snapshot_environment
		WHERE snapshot_id IN (` + placeholders + `)
		ORDER BY snapshot_id, position`

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query snapshot environment: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			snapshotID     int64
			entry          domain.SnapshotEnvEntry
			classification string
			algorithm      string
		)
		if err := rows.Scan(
			&snapshotID, &entry.Position, &entry.Key, &classification, &entry.Present,
			&entry.Value, &entry.Length, &entry.Digest, &algorithm, &entry.DigestKeyID,
		); err != nil {
			return nil, fmt.Errorf("scan snapshot environment: %w", err)
		}
		entry.Classification = domain.Sensitivity(classification)
		entry.DigestAlgorithm = domain.DigestAlgorithm(algorithm)
		result[snapshotID] = append(result[snapshotID], entry)
	}
	return result, rows.Err()
}

// Mounts returns one snapshot's mounts.
func (r *SnapshotRepository) Mounts(ctx context.Context, snapshotID int64) ([]domain.SnapshotMountRow, error) {
	const query = `
		SELECT destination, type, source, read_only, volume_name, driver
		FROM snapshot_mounts
		WHERE snapshot_id = ?
		ORDER BY destination`

	rows, err := r.db.QueryContext(ctx, query, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("query snapshot mounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	mounts := make([]domain.SnapshotMountRow, 0)
	for rows.Next() {
		var (
			mount     domain.SnapshotMountRow
			mountType string
		)
		if err := rows.Scan(&mount.Destination, &mountType, &mount.Source,
			&mount.ReadOnly, &mount.VolumeName, &mount.Driver); err != nil {
			return nil, fmt.Errorf("scan snapshot mount: %w", err)
		}
		mount.Type = domain.MountType(mountType)
		mounts = append(mounts, mount)
	}
	return mounts, rows.Err()
}

// Networks returns one snapshot's network attachments.
func (r *SnapshotRepository) Networks(ctx context.Context, snapshotID int64) ([]domain.SnapshotNetworkRow, error) {
	const query = `
		SELECT network_name, aliases_json
		FROM snapshot_networks
		WHERE snapshot_id = ?
		ORDER BY network_name`

	rows, err := r.db.QueryContext(ctx, query, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("query snapshot networks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	networks := make([]domain.SnapshotNetworkRow, 0)
	for rows.Next() {
		var (
			network domain.SnapshotNetworkRow
			aliases string
		)
		if err := rows.Scan(&network.NetworkName, &aliases); err != nil {
			return nil, fmt.Errorf("scan snapshot network: %w", err)
		}
		if aliases != "" {
			if err := json.Unmarshal([]byte(aliases), &network.Aliases); err != nil {
				// A malformed alias list degrades that one field rather than
				// failing the whole read.
				network.Aliases = nil
			}
		}
		networks = append(networks, network)
	}
	return networks, rows.Err()
}

// RecordReadiness appends a readiness evaluation and refreshes the summary.
//
// This is the ONLY method that updates an existing snapshot row, and it touches
// exactly two columns. Both are a denormalised cache of the rows it just
// inserted, kept so the list endpoint can filter and sort without a join.
func (r *SnapshotRepository) RecordReadiness(ctx context.Context, report domain.ReadinessReport) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin readiness transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO snapshot_restore_checks (snapshot_id, evaluated_at, check_id, status, detail)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare readiness insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	evaluatedAt := formatTime(report.EvaluatedAt)
	for _, check := range report.Checks {
		if _, err := stmt.ExecContext(ctx,
			report.SnapshotID, evaluatedAt, string(check.ID), string(check.Status), check.Detail,
		); err != nil {
			return fmt.Errorf("insert readiness check: %w", err)
		}
	}

	// The one UPDATE in this package. Adding a column here should be a visible
	// diff: TestRepositoryUpdatesOnlyReadinessSummary asserts it stays this way.
	if _, err := tx.ExecContext(ctx, `
		UPDATE snapshots
		SET readiness_status = ?, readiness_evaluated_at = ?
		WHERE id = ?`,
		string(report.Status), evaluatedAt, report.SnapshotID); err != nil {
		return fmt.Errorf("update readiness summary: %w", err)
	}

	return tx.Commit()
}

// LatestReadiness returns the most recent stored evaluation for a snapshot.
//
// Historical record only. A current answer is recomputed live, because a stored
// verdict ages the moment it is written.
func (r *SnapshotRepository) LatestReadiness(ctx context.Context, snapshotID int64) ([]domain.ReadinessCheck, *time.Time, error) {
	const query = `
		SELECT check_id, status, detail, evaluated_at
		FROM snapshot_restore_checks
		WHERE snapshot_id = ?
		  AND evaluated_at = (
			SELECT MAX(evaluated_at) FROM snapshot_restore_checks WHERE snapshot_id = ?
		  )
		ORDER BY id`

	rows, err := r.db.QueryContext(ctx, query, snapshotID, snapshotID)
	if err != nil {
		return nil, nil, fmt.Errorf("query readiness checks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		checks      = make([]domain.ReadinessCheck, 0)
		evaluatedAt *time.Time
	)
	for rows.Next() {
		var (
			check     domain.ReadinessCheck
			checkID   string
			status    string
			timestamp string
		)
		if err := rows.Scan(&checkID, &status, &check.Detail, &timestamp); err != nil {
			return nil, nil, fmt.Errorf("scan readiness check: %w", err)
		}
		check.ID = domain.ReadinessCheckID(checkID)
		check.Status = domain.ReadinessStatus(status)
		checks = append(checks, check)

		if evaluatedAt == nil {
			parsed, parseErr := parseTime(timestamp)
			if parseErr == nil {
				evaluatedAt = &parsed
			}
		}
	}
	return checks, evaluatedAt, rows.Err()
}

// DistinctDigestKeyID returns any digest key ID recorded on a snapshot.
//
// Startup uses it to tell "no key has ever been used" from "a key was used and
// its file is now missing", which is the difference between generating a key
// and refusing to start.
func (r *SnapshotRepository) DistinctDigestKeyID(ctx context.Context) (string, error) {
	var keyID sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT digest_key_id FROM snapshots
		WHERE digest_key_id <> ''
		ORDER BY id DESC LIMIT 1`).Scan(&keyID)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read digest key id: %w", err)
	}
	return keyID.String, nil
}

// idPlaceholders renders "?, ?, ?" for an ID list and its bound arguments.
func idPlaceholders(ids []int64) (string, []any) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return strings.Join(placeholders, ", "), args
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSnapshotRow(row *sql.Row) (domain.Snapshot, error) {
	snapshot, err := scanSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Snapshot{}, ErrNotFound
	}
	return snapshot, err
}

func scanSnapshot(row rowScanner) (domain.Snapshot, error) {
	var (
		snapshot     domain.Snapshot
		trigger      string
		readiness    string
		specJSON     []byte
		warningsJSON []byte
		evaluatedAt  sql.NullString
		createdAt    string
	)

	if err := row.Scan(
		&snapshot.ID, &snapshot.HostID, &snapshot.ContainerID, &snapshot.ContainerName,
		&snapshot.ImageReference, &snapshot.ImageDigest, &snapshot.ImageID,
		&snapshot.SpecVersion, &specJSON, &snapshot.Checksum,
		&snapshot.HarborMasterVersion, &snapshot.DockerAPIVersion, &snapshot.DockerEngineVersion,
		&trigger, &snapshot.Reason,
		&snapshot.InventoryGeneration, &snapshot.EventSequence,
		&snapshot.WarningCount, &warningsJSON,
		&readiness, &evaluatedAt,
		&snapshot.DigestKeyID, &createdAt,
	); err != nil {
		return domain.Snapshot{}, err
	}

	snapshot.SpecJSON = specJSON
	snapshot.Trigger = domain.SnapshotTrigger(trigger)
	snapshot.ReadinessStatus = domain.ReadinessStatus(readiness)

	if len(warningsJSON) > 0 {
		if err := json.Unmarshal(warningsJSON, &snapshot.Warnings); err != nil {
			// Degrade the field rather than the read: the snapshot itself is
			// still valid evidence.
			snapshot.Warnings = nil
		}
	}
	if snapshot.Warnings == nil {
		snapshot.Warnings = []domain.InventoryWarning{}
	}

	if evaluatedAt.Valid && evaluatedAt.String != "" {
		if parsed, err := parseTime(evaluatedAt.String); err == nil {
			snapshot.ReadinessEvaluatedAt = &parsed
		}
	}

	parsed, err := parseTime(createdAt)
	if err != nil {
		return domain.Snapshot{}, fmt.Errorf("snapshot %d: %w", snapshot.ID, err)
	}
	snapshot.CreatedAt = parsed
	return snapshot, nil
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
