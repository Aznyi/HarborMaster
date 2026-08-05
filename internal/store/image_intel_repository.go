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

// ImageIntelRepository owns persistence for image intelligence.
//
// # No inventory data is duplicated
//
// The local image row already holds size, layers, local labels, and the
// content-addressable id, and the container row already holds the reference. A
// row here holds only what the REGISTRY knows and what the scheduler needs, and
// links to the others by id. Container counts and local digests are read back
// by join at query time rather than copied at write time.
//
// # Bounded reads
//
// Every list is paginated, every sort field comes from an allowlist, and the
// summary is a handful of aggregate queries rather than a list the caller
// counts. The scheduler's "what is due" is an index range scan on one column,
// which is what keeps a ten-thousand-reference estate cheap to schedule.
type ImageIntelRepository struct {
	db *sql.DB
}

// imageIntelSortFields is the allowlist of sortable columns.
//
// An allowlist rather than validation-by-rejection: the value becomes part of
// the SQL text, so the only safe design is one where caller input SELECTS from
// a fixed set of literals rather than contributing to it.
var imageIntelSortFields = map[string]string{
	"reference":   "i.reference",
	"registry":    "i.registry",
	"update":      imageUpdateRankSQL,
	"status":      "i.check_status",
	"lastChecked": "i.last_checked_at",
	"containers":  "container_count",
	"id":          "i.id",
}

// imageUpdateRankSQL orders by how much an update matters rather than by the
// spelling of its type. Built from literals only.
const imageUpdateRankSQL = `CASE i.update_type ` +
	`WHEN 'major' THEN 6 ` +
	`WHEN 'minor' THEN 5 ` +
	`WHEN 'patch' THEN 4 ` +
	`WHEN 'prerelease' THEN 3 ` +
	`WHEN 'digest' THEN 2 ` +
	`WHEN 'unknown' THEN 1 ` +
	`ELSE 0 END`

// ValidImageIntelSortField reports whether field names a sortable column.
func ValidImageIntelSortField(field string) bool {
	_, ok := imageIntelSortFields[field]
	return ok
}

// ImageIntelFilter narrows an image intelligence listing.
//
// Every field is a closed vocabulary or an exact identifier validated by the
// API layer before it arrives. None of them becomes SQL text.
type ImageIntelFilter struct {
	Updates  []domain.UpdateType
	Statuses []domain.CheckStatus
	// Registries filters by host. Matched exactly against a stored value.
	Registries []string
	// UpdatesOnly restricts to references with an actionable update, which is
	// what the updates dashboard asks for.
	UpdatesOnly bool
	// InUseOnly restricts to references a present container actually runs.
	InUseOnly bool
	Search    string

	Sort      string
	Ascending bool
	Page      Page
}

// container_count is a correlated subquery rather than a join with a GROUP BY:
// one query serves the page whatever its size, and the count stays correct when
// no container references the image at all.
const selectImageIntelColumns = `
	SELECT i.id, i.reference, i.familiar, i.registry_kind, i.registry,
	       i.namespace, i.repository, i.tag,
	       i.local_digest, i.remote_digest, i.pinned,
	       i.platform_os, i.platform_arch, i.platform_variant, i.image_id,
	       i.update_type, i.latest_tag, i.update_reason,
	       i.check_status, i.status_detail,
	       i.first_seen_at, i.last_checked_at, i.last_success_at,
	       i.next_check_at, i.failure_count,
	       i.published_at, i.vendor, i.source, i.labels, i.etag,
	       (SELECT COUNT(*) FROM containers c
	         WHERE c.present = 1 AND c.image_ref = i.reference) AS container_count
	FROM image_intel i`

// ImageReferenceSeed is one reference observed in the inventory.
type ImageReferenceSeed struct {
	// Reference is the canonical form, produced by domain.NormalizeImageRef.
	Reference string
	Familiar  string

	Kind       domain.RegistryKind
	Registry   string
	Namespace  string
	Repository string
	Tag        string

	// LocalDigest is what the daemon reports for this reference, and ImageID
	// the local image row it resolved to.
	LocalDigest string
	ImageID     string
	Pinned      bool

	Platform domain.Platform

	// Supported is false for a reference that can never be looked up. It is
	// still tracked, so a dashboard can explain the gap in coverage rather than
	// silently omitting the image.
	Supported bool
	// Detail explains an unsupported reference, from a fixed set of phrases.
	Detail string
}

// SyncResult reports what a reconciliation changed.
type SyncResult struct {
	Inserted int
	Updated  int
}

// SyncReferences reconciles the tracked reference set against the inventory.
//
// # What it does and does not overwrite
//
// The reference's IDENTITY fields and everything the local daemon knows are
// refreshed: a container moved onto a new digest for the same tag must be
// visible immediately. Everything the REGISTRY established -- the remote digest,
// the update verdict, the check status, the schedule, the failure count -- is
// left alone, because an inventory refresh has learned nothing about the
// registry and overwriting it would discard a real answer and re-queue work
// that was already done.
//
// A reference seen for the first time is scheduled immediately, so a newly
// deployed container is not invisible until the next interval.
func (r *ImageIntelRepository) SyncReferences(
	ctx context.Context,
	seeds []ImageReferenceSeed,
	now time.Time,
) (SyncResult, error) {
	var result SyncResult
	stamp := formatTime(now.UTC())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("begin image intel sync: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	for _, seed := range seeds {
		status := string(domain.CheckPending)
		// An unsupported reference is recorded as such rather than queued, so
		// the scheduler never picks up work that cannot succeed.
		nextCheck := any(stamp)
		if !seed.Supported {
			status = string(domain.CheckUnsupported)
			nextCheck = nil
		}

		inserted, err := upsertImageIntel(ctx, tx, seed, status, nextCheck, stamp)
		if err != nil {
			return result, err
		}
		if inserted {
			result.Inserted++
		} else {
			result.Updated++
		}
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit image intel sync: %w", AsError(err))
	}
	return result, nil
}

// upsertImageIntel writes one reference, preserving registry-established state.
func upsertImageIntel(
	ctx context.Context,
	tx *sql.Tx,
	seed ImageReferenceSeed,
	status string,
	nextCheck any,
	stamp string,
) (bool, error) {
	pinned := 0
	if seed.Pinned {
		pinned = 1
	}

	// RETURNING created_at tells insert from update in the SAME statement. A
	// follow-up SELECT would be a second round trip per reference, which on a
	// ten-thousand-reference estate is ten thousand avoidable queries inside
	// the transaction that holds the single SQLite writer.
	//
	// The DO UPDATE arm deliberately touches only what the inventory knows.
	// check_status is refreshed ONLY to move a reference that has become
	// unsupported, or to re-queue one that has become supported again -- an
	// inventory refresh has established nothing about the registry.
	var createdAt string
	err := tx.QueryRowContext(ctx, `
		INSERT INTO image_intel
			(reference, familiar, registry_kind, registry, namespace, repository, tag,
			 local_digest, pinned, platform_os, platform_arch, platform_variant,
			 image_id, check_status, status_detail, first_seen_at, next_check_at,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (reference) DO UPDATE SET
			familiar         = excluded.familiar,
			registry_kind    = excluded.registry_kind,
			registry         = excluded.registry,
			namespace        = excluded.namespace,
			repository       = excluded.repository,
			tag              = excluded.tag,
			local_digest     = excluded.local_digest,
			pinned           = excluded.pinned,
			platform_os      = excluded.platform_os,
			platform_arch    = excluded.platform_arch,
			platform_variant = excluded.platform_variant,
			image_id         = excluded.image_id,
			check_status = CASE
				WHEN excluded.check_status = 'unsupported' THEN 'unsupported'
				WHEN image_intel.check_status = 'unsupported' THEN 'pending'
				ELSE image_intel.check_status
			END,
			status_detail = CASE
				WHEN excluded.check_status = 'unsupported' THEN excluded.status_detail
				WHEN image_intel.check_status = 'unsupported' THEN ''
				ELSE image_intel.status_detail
			END,
			next_check_at = CASE
				WHEN excluded.check_status = 'unsupported' THEN NULL
				WHEN image_intel.check_status = 'unsupported' THEN excluded.next_check_at
				ELSE image_intel.next_check_at
			END,
			updated_at = excluded.updated_at
		RETURNING created_at`,
		seed.Reference, seed.Familiar, string(seed.Kind), seed.Registry,
		seed.Namespace, seed.Repository, seed.Tag,
		seed.LocalDigest, pinned,
		seed.Platform.OS, seed.Platform.Architecture, seed.Platform.Variant,
		seed.ImageID, status, seed.Detail, stamp, nextCheck, stamp, stamp,
	).Scan(&createdAt)
	if err != nil {
		return false, fmt.Errorf("upsert image intel: %w", AsError(err))
	}
	return createdAt == stamp, nil
}

// InventoryReferences reads every distinct image reference a present container
// declares.
//
// ONE query for the whole estate. The alternative -- a query per container --
// is the N+1 pattern at its most expensive, and this is the path that runs on
// every inventory refresh.
func (r *ImageIntelRepository) InventoryReferences(ctx context.Context, limit int) ([]InventoryReference, error) {
	if limit < 1 {
		limit = 10000
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT c.image_ref,
		       MAX(c.image_id),
		       MAX(c.image_digest),
		       MAX(COALESCE(im.architecture, '')),
		       MAX(COALESCE(im.os, '')),
		       MAX(COALESCE(im.variant, ''))
		FROM containers c
		LEFT JOIN images im ON im.id = c.image_id
		WHERE c.present = 1 AND c.image_ref <> ''
		GROUP BY c.image_ref
		ORDER BY c.image_ref
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query inventory references: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	references := make([]InventoryReference, 0, 64)
	for rows.Next() {
		var reference InventoryReference
		if err := rows.Scan(&reference.Reference, &reference.ImageID, &reference.Digest,
			&reference.Architecture, &reference.OS, &reference.Variant); err != nil {
			return nil, fmt.Errorf("scan inventory reference: %w", err)
		}
		references = append(references, reference)
	}
	return references, rows.Err()
}

// InventoryReference is one image reference as the inventory records it.
type InventoryReference struct {
	Reference string
	ImageID   string
	// Digest is the manifest digest the daemon reports, when it has one.
	Digest       string
	Architecture string
	OS           string
	Variant      string
}

// Due returns references whose next check is due, oldest schedule first.
//
// Bounded by limit, and excluding hosts that are backing off. Excluding them in
// SQL rather than in the worker is what stops a rate-limited registry from
// filling every batch with work that will immediately be skipped.
func (r *ImageIntelRepository) Due(
	ctx context.Context,
	now time.Time,
	limit int,
	unavailableHosts []string,
) ([]domain.ImageIntel, error) {
	if limit < 1 {
		limit = 50
	}

	where := ` WHERE i.next_check_at IS NOT NULL AND i.next_check_at <= ?
	           AND i.check_status <> 'unsupported'`
	args := []any{formatTime(now.UTC())}

	if len(unavailableHosts) > 0 {
		// A placeholder run generated from a slice LENGTH, never from content.
		where += ` AND i.registry NOT IN (` + placeholders(len(unavailableHosts)) + `)`
		for _, host := range unavailableHosts {
			args = append(args, host)
		}
	}

	query := selectImageIntelColumns + where + ` ORDER BY i.next_check_at, i.id LIMIT ?`
	rows, err := r.db.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("query due images: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	return scanImageIntel(rows)
}

// CountDue reports how many references are past their next-check time.
func (r *ImageIntelRepository) CountDue(ctx context.Context, now time.Time) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM image_intel
		WHERE next_check_at IS NOT NULL AND next_check_at <= ?
		  AND check_status <> 'unsupported'`, formatTime(now.UTC())).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count due images: %w", AsError(err))
	}
	return count, nil
}

// CheckOutcome is the result of one registry lookup, ready to persist.
type CheckOutcome struct {
	Reference string

	Status domain.CheckStatus
	Detail string

	// RemoteDigest, Update, LatestTag and UpdateReason are applied only when
	// Status is ok. A failed lookup must not overwrite a good answer with a
	// blank one: the previous verdict is still the best knowledge available.
	RemoteDigest string
	Update       domain.UpdateType
	LatestTag    string
	UpdateReason string

	Platform    domain.Platform
	PublishedAt *time.Time
	Vendor      string
	Source      string
	Labels      map[string]string
	ETag        string

	// NextCheckAt is when the scheduler should look again, already including
	// any backoff or registry-requested wait.
	NextCheckAt time.Time
	// FailureCount is the running count after this attempt.
	FailureCount int

	// Events are the history rows this outcome produced. Computed by the
	// service, which is the layer that knows what the previous state was.
	Events []domain.ImageUpdateEvent
}

// RecordCheck persists one lookup's outcome and its history, atomically.
//
// One transaction because the record and its history are one statement about
// the world: a reader must not see a digest change with no event explaining it,
// or an event describing a state the row does not hold.
func (r *ImageIntelRepository) RecordCheck(ctx context.Context, outcome CheckOutcome, now time.Time) error {
	stamp := formatTime(now.UTC())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin image check: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	labels := marshalStringMap(outcome.Labels)

	// The vocabularies are normalised here rather than assumed. The service
	// always sets both, but a zero value would fail a CHECK constraint and abort
	// the whole transaction -- and losing a pass's result to a caller's omission
	// is a worse failure than recording a conservative default.
	update := outcome.Update
	if update == "" {
		update = domain.UpdateNone
	}
	status := outcome.Status
	if status == "" {
		status = domain.CheckPending
	}

	var published any
	if outcome.PublishedAt != nil {
		published = formatTime(outcome.PublishedAt.UTC())
	}

	if status == domain.CheckOK {
		_, err = tx.ExecContext(ctx, `
			UPDATE image_intel SET
				remote_digest    = ?,
				update_type      = ?,
				latest_tag       = ?,
				update_reason    = ?,
				check_status     = ?,
				status_detail    = ?,
				platform_os      = CASE WHEN ? <> '' THEN ? ELSE platform_os END,
				platform_arch    = CASE WHEN ? <> '' THEN ? ELSE platform_arch END,
				platform_variant = CASE WHEN ? <> '' THEN ? ELSE platform_variant END,
				published_at     = COALESCE(?, published_at),
				vendor           = CASE WHEN ? <> '' THEN ? ELSE vendor END,
				source           = CASE WHEN ? <> '' THEN ? ELSE source END,
				labels           = CASE WHEN ? <> '{}' THEN ? ELSE labels END,
				etag             = ?,
				last_checked_at  = ?,
				last_success_at  = ?,
				next_check_at    = ?,
				failure_count    = 0,
				updated_at       = ?
			WHERE reference = ?`,
			outcome.RemoteDigest, string(update), outcome.LatestTag, outcome.UpdateReason,
			string(status), outcome.Detail,
			outcome.Platform.OS, outcome.Platform.OS,
			outcome.Platform.Architecture, outcome.Platform.Architecture,
			outcome.Platform.Variant, outcome.Platform.Variant,
			published,
			outcome.Vendor, outcome.Vendor,
			outcome.Source, outcome.Source,
			labels, labels,
			outcome.ETag,
			stamp, stamp, formatTime(outcome.NextCheckAt.UTC()), stamp,
			outcome.Reference)
	} else {
		// A failure records the status and the schedule and NOTHING else. The
		// previous digest and verdict remain the best knowledge available, and
		// blanking them would turn "we could not reach the registry" into "no
		// update is available", which is a different and false claim.
		_, err = tx.ExecContext(ctx, `
			UPDATE image_intel SET
				check_status    = ?,
				status_detail   = ?,
				last_checked_at = ?,
				next_check_at   = ?,
				failure_count   = ?,
				updated_at      = ?
			WHERE reference = ?`,
			string(status), outcome.Detail, stamp,
			formatTime(outcome.NextCheckAt.UTC()), outcome.FailureCount, stamp,
			outcome.Reference)
	}
	if err != nil {
		return fmt.Errorf("update image intel: %w", AsError(err))
	}

	for _, event := range outcome.Events {
		if err := insertImageEvent(ctx, tx, event, stamp); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit image check: %w", AsError(err))
	}
	return nil
}

// insertImageEvent appends one history row.
func insertImageEvent(ctx context.Context, tx *sql.Tx, event domain.ImageUpdateEvent, stamp string) error {
	observed := stamp
	if !event.ObservedAt.IsZero() {
		observed = formatTime(event.ObservedAt.UTC())
	}

	// Normalised for the same reason as the record above: an omitted status
	// would fail a CHECK and cost the whole transaction.
	status := event.Status
	if status == "" {
		status = domain.CheckOK
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO image_update_history
			(reference, observed_at, kind, previous_digest, current_digest,
			 previous_update, current_update, latest_tag, check_status, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.Reference, observed, string(event.Kind),
		event.PreviousDigest, event.CurrentDigest,
		string(event.PreviousUpdate), string(event.CurrentUpdate), event.LatestTag,
		string(status), event.Detail,
	); err != nil {
		return fmt.Errorf("insert image event: %w", AsError(err))
	}
	return nil
}

// List returns a page of tracked references and the total matching the filter.
func (r *ImageIntelRepository) List(
	ctx context.Context,
	filter ImageIntelFilter,
) ([]domain.ImageIntel, int, error) {
	where, args := imageIntelWhere(filter)

	var total int
	countQuery := `SELECT COUNT(*) FROM image_intel i` + where
	if err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count image intel: %w", AsError(err))
	}

	column, ok := imageIntelSortFields[filter.Sort]
	if !ok {
		column = imageUpdateRankSQL
	}
	direction := "DESC"
	if filter.Ascending {
		direction = "ASC"
	}

	page := filter.Page.normalise()
	// column and direction come from the allowlist above and a two-valued
	// switch; neither is caller text. Every VALUE is bound.
	query := selectImageIntelColumns + where +
		` ORDER BY ` + column + ` ` + direction + `, i.id ` + direction +
		` LIMIT ? OFFSET ?`

	rows, err := r.db.QueryContext(ctx, query, append(args, page.Limit, page.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query image intel: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	records, err := scanImageIntel(rows)
	if err != nil {
		return nil, 0, err
	}
	return records, total, nil
}

// Get returns one reference's intelligence.
func (r *ImageIntelRepository) Get(ctx context.Context, reference string) (domain.ImageIntel, error) {
	rows, err := r.db.QueryContext(ctx, selectImageIntelColumns+` WHERE i.reference = ?`, reference)
	if err != nil {
		return domain.ImageIntel{}, fmt.Errorf("query image intel: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	records, err := scanImageIntel(rows)
	if err != nil {
		return domain.ImageIntel{}, err
	}
	if len(records) == 0 {
		return domain.ImageIntel{}, ErrNotFound
	}
	return records[0], nil
}

// ForImageID returns every tracked reference resolving to one local image.
//
// A local image can carry several references -- the same content tagged twice --
// and the image detail endpoint shows all of them rather than picking one.
func (r *ImageIntelRepository) ForImageID(ctx context.Context, imageID string) ([]domain.ImageIntel, error) {
	rows, err := r.db.QueryContext(ctx,
		selectImageIntelColumns+` WHERE i.image_id = ? ORDER BY i.reference LIMIT 100`, imageID)
	if err != nil {
		return nil, fmt.Errorf("query image intel by image: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	return scanImageIntel(rows)
}

// History returns a page of observed changes, newest first.
func (r *ImageIntelRepository) History(
	ctx context.Context,
	reference string,
	page Page,
) ([]domain.ImageUpdateEvent, int, error) {
	page = page.normalise()

	where := ""
	args := []any{}
	if reference != "" {
		where = " WHERE reference = ?"
		args = append(args, reference)
	}

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM image_update_history`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count image history: %w", AsError(err))
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, reference, observed_at, kind, previous_digest, current_digest,
		       previous_update, current_update, latest_tag, check_status, detail
		FROM image_update_history`+where+`
		ORDER BY observed_at DESC, id DESC
		LIMIT ? OFFSET ?`, append(args, page.Limit, page.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query image history: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	events := make([]domain.ImageUpdateEvent, 0, 16)
	for rows.Next() {
		var (
			event          domain.ImageUpdateEvent
			observed       string
			kind           string
			previousUpdate string
			currentUpdate  string
			status         string
		)
		if err := rows.Scan(&event.ID, &event.Reference, &observed, &kind,
			&event.PreviousDigest, &event.CurrentDigest,
			&previousUpdate, &currentUpdate, &event.LatestTag,
			&status, &event.Detail); err != nil {
			return nil, 0, fmt.Errorf("scan image event: %w", err)
		}

		event.Kind = domain.ImageEventKind(kind)
		event.PreviousUpdate = domain.UpdateType(previousUpdate)
		event.CurrentUpdate = domain.UpdateType(currentUpdate)
		event.Status = domain.CheckStatus(status)
		if event.ObservedAt, err = parseTime(observed); err != nil {
			return nil, 0, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return events, total, nil
}

// Summary computes the dashboard aggregate.
//
// Grouped queries plus a few scalars. Each is served by an index and each
// returns at most a handful of rows, so the cost is proportional to the number
// of DISTINCT update types, statuses and registries -- small constants -- not to
// the number of references.
func (r *ImageIntelRepository) Summary(ctx context.Context) (domain.ImageIntelSummary, error) {
	summary := domain.ImageIntelSummary{
		ByUpdate:   make(map[domain.UpdateType]int, len(domain.UpdateTypes)),
		ByStatus:   make(map[domain.CheckStatus]int, len(domain.CheckStatuses)),
		ByRegistry: make(map[string]int, 8),
	}

	for _, group := range []struct {
		query string
		apply func(key string, count int)
	}{
		{
			`SELECT update_type, COUNT(*) FROM image_intel GROUP BY update_type`,
			func(key string, count int) { summary.ByUpdate[domain.UpdateType(key)] = count },
		},
		{
			`SELECT check_status, COUNT(*) FROM image_intel GROUP BY check_status`,
			func(key string, count int) { summary.ByStatus[domain.CheckStatus(key)] = count },
		},
		{
			// Bounded: a pathological estate could reference hundreds of
			// registries, and a dashboard needs the ones that matter.
			`SELECT registry, COUNT(*) AS n FROM image_intel
			 WHERE registry <> '' GROUP BY registry ORDER BY n DESC LIMIT 20`,
			func(key string, count int) { summary.ByRegistry[key] = count },
		},
	} {
		if err := r.scanImageCounts(ctx, group.query, group.apply); err != nil {
			return summary, err
		}
	}

	var lastChecked sql.NullString
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN last_success_at IS NOT NULL THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN check_status = 'pending' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN check_status = 'unsupported' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN update_type NOT IN ('none', 'unknown') THEN 1 ELSE 0 END), 0),
		       MAX(last_checked_at)
		FROM image_intel`,
	).Scan(&summary.Images, &summary.Checked, &summary.Pending,
		&summary.Unsupported, &summary.UpdatesAvailable, &lastChecked); err != nil {
		return summary, fmt.Errorf("summarise image intel: %w", AsError(err))
	}

	// Containers are counted through the reference, so a hundred containers on
	// one outdated image report as a hundred affected containers -- which is the
	// number an operator actually plans around.
	if err := r.db.QueryRowContext(ctx, `
		SELECT
			COALESCE((SELECT COUNT(*) FROM containers WHERE present = 1 AND image_ref <> ''), 0),
			COALESCE((SELECT COUNT(*) FROM containers c
			           JOIN image_intel i ON i.reference = c.image_ref
			          WHERE c.present = 1
			            AND i.update_type NOT IN ('none', 'unknown')), 0)`,
	).Scan(&summary.Containers, &summary.ContainersAffected); err != nil {
		return summary, fmt.Errorf("count affected containers: %w", AsError(err))
	}

	if lastChecked.Valid && lastChecked.String != "" {
		parsed, err := parseTime(lastChecked.String)
		if err != nil {
			return summary, err
		}
		summary.LastCheckedAt = &parsed
	}

	health, err := r.RegistryHealth(ctx)
	if err != nil {
		return summary, err
	}
	summary.Registries = health
	return summary, nil
}

// ------------------------------------------------------------- registries --

// RegistryHealth returns per-host health, with the reference count each serves.
func (r *ImageIntelRepository) RegistryHealth(ctx context.Context) ([]domain.RegistryHealth, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT h.host, h.registry_kind, h.last_success_at, h.last_failure_at,
		       h.consecutive_failures, h.available_at, h.last_detail, h.rate_limited,
		       (SELECT COUNT(*) FROM image_intel i WHERE i.registry = h.host)
		FROM registry_hosts h
		ORDER BY h.host
		LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("query registry health: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	health := make([]domain.RegistryHealth, 0, 8)
	for rows.Next() {
		var (
			entry       domain.RegistryHealth
			kind        string
			lastSuccess sql.NullString
			lastFailure sql.NullString
			available   sql.NullString
			rateLimited int
		)
		if err := rows.Scan(&entry.Host, &kind, &lastSuccess, &lastFailure,
			&entry.ConsecutiveFailures, &available, &entry.LastDetail,
			&rateLimited, &entry.Images); err != nil {
			return nil, fmt.Errorf("scan registry health: %w", err)
		}

		entry.Kind = domain.RegistryKind(kind)
		entry.RateLimited = rateLimited == 1
		entry.LastSuccessAt = scanOptionalTime(lastSuccess)
		entry.LastFailureAt = scanOptionalTime(lastFailure)
		entry.AvailableAt = scanOptionalTime(available)
		health = append(health, entry)
	}
	return health, rows.Err()
}

// HostOutcome records one host's most recent behaviour.
type HostOutcome struct {
	Host string
	Kind domain.RegistryKind

	Success bool
	// Detail is HarborMaster's own description of a failure.
	Detail      string
	RateLimited bool
	// AvailableAt is when the host may be contacted again. Zero clears the
	// hold.
	AvailableAt time.Time
	// ConsecutiveFailures is the running count after this attempt.
	ConsecutiveFailures int
}

// RecordHostOutcome updates a registry host's health.
func (r *ImageIntelRepository) RecordHostOutcome(ctx context.Context, outcome HostOutcome, now time.Time) error {
	stamp := formatTime(now.UTC())

	var (
		lastSuccess any
		lastFailure any
		available   any
	)
	if outcome.Success {
		lastSuccess = stamp
	} else {
		lastFailure = stamp
	}
	if !outcome.AvailableAt.IsZero() {
		available = formatTime(outcome.AvailableAt.UTC())
	}

	rateLimited := 0
	if outcome.RateLimited {
		rateLimited = 1
	}

	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO registry_hosts
			(host, registry_kind, last_success_at, last_failure_at,
			 consecutive_failures, available_at, last_detail, rate_limited,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (host) DO UPDATE SET
			registry_kind        = excluded.registry_kind,
			last_success_at      = COALESCE(excluded.last_success_at, registry_hosts.last_success_at),
			last_failure_at      = COALESCE(excluded.last_failure_at, registry_hosts.last_failure_at),
			consecutive_failures = excluded.consecutive_failures,
			available_at         = excluded.available_at,
			last_detail          = excluded.last_detail,
			rate_limited         = excluded.rate_limited,
			updated_at           = excluded.updated_at`,
		outcome.Host, string(outcome.Kind), lastSuccess, lastFailure,
		outcome.ConsecutiveFailures, available, outcome.Detail, rateLimited,
		stamp, stamp,
	); err != nil {
		return fmt.Errorf("record registry host: %w", AsError(err))
	}
	return nil
}

// UnavailableHosts lists hosts currently backing off.
func (r *ImageIntelRepository) UnavailableHosts(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT host FROM registry_hosts
		WHERE available_at IS NOT NULL AND available_at > ?
		LIMIT 100`, formatTime(now.UTC()))
	if err != nil {
		return nil, fmt.Errorf("query unavailable hosts: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	hosts := make([]string, 0, 4)
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return nil, fmt.Errorf("scan unavailable host: %w", err)
		}
		hosts = append(hosts, host)
	}
	return hosts, rows.Err()
}

// HostFailureCount reads a host's current consecutive-failure count.
func (r *ImageIntelRepository) HostFailureCount(ctx context.Context, host string) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT consecutive_failures FROM registry_hosts WHERE host = ?`, host).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read registry host: %w", AsError(err))
	}
	return count, nil
}

// -------------------------------------------------------------- retention --

// PruneHistory deletes observed changes older than the cutoff.
//
// History is the dimension that grows without bound on an estate whose
// publishers ship often. The current state on image_intel is never pruned: it
// is one row per reference and is bounded by the inventory.
func (r *ImageIntelRepository) PruneHistory(ctx context.Context, cutoff time.Time, batch int) (int64, error) {
	if batch < 1 {
		batch = 500
	}
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM image_update_history
		WHERE id IN (
			SELECT id FROM image_update_history WHERE observed_at < ? LIMIT ?
		)`, formatTime(cutoff.UTC()), batch)
	if err != nil {
		return 0, fmt.Errorf("prune image history: %w", AsError(err))
	}

	// Reported for logging only; the deletion has already happened.
	removed, _ := result.RowsAffected()
	return removed, nil
}

// PruneOrphans removes references no present container declares any more.
//
// Bounded by batch, and cascading to their history. Without this, every tag an
// estate has ever run would be checked forever.
func (r *ImageIntelRepository) PruneOrphans(ctx context.Context, batch int) (int64, error) {
	if batch < 1 {
		batch = 500
	}
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM image_intel
		WHERE id IN (
			SELECT i.id FROM image_intel i
			WHERE NOT EXISTS (
				SELECT 1 FROM containers c
				WHERE c.present = 1 AND c.image_ref = i.reference
			)
			LIMIT ?
		)`, batch)
	if err != nil {
		return 0, fmt.Errorf("prune orphan image intel: %w", AsError(err))
	}

	removed, _ := result.RowsAffected()
	return removed, nil
}

// ---------------------------------------------------------------- helpers --

func (r *ImageIntelRepository) scanImageCounts(ctx context.Context, query string, apply func(string, int)) error {
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("aggregate image intel: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			key   string
			count int
		)
		if err := rows.Scan(&key, &count); err != nil {
			return fmt.Errorf("scan image aggregate: %w", err)
		}
		apply(key, count)
	}
	return rows.Err()
}

// imageIntelWhere builds the filter clause. Every value is bound; only the
// placeholder RUN length varies with the input.
func imageIntelWhere(filter ImageIntelFilter) (string, []any) {
	clauses := make([]string, 0, 6)
	args := make([]any, 0, 8)

	if filter.UpdatesOnly {
		clauses = append(clauses, "i.update_type NOT IN ('none', 'unknown')")
	}
	if filter.InUseOnly {
		clauses = append(clauses, `EXISTS (SELECT 1 FROM containers c
			WHERE c.present = 1 AND c.image_ref = i.reference)`)
	}
	if filter.Search != "" {
		// LIKE with a bound parameter. The wildcards belong to the BOUND VALUE
		// rather than to the SQL text, so a term containing % or _ widens only
		// its own match and cannot alter the statement.
		clauses = append(clauses, `(i.reference LIKE ? ESCAPE '\' OR i.familiar LIKE ? ESCAPE '\')`)
		pattern := "%" + escapeLike(filter.Search) + "%"
		args = append(args, pattern, pattern)
	}

	if len(filter.Updates) > 0 {
		clauses = append(clauses, "i.update_type IN ("+placeholders(len(filter.Updates))+")")
		for _, update := range filter.Updates {
			args = append(args, string(update))
		}
	}
	if len(filter.Statuses) > 0 {
		clauses = append(clauses, "i.check_status IN ("+placeholders(len(filter.Statuses))+")")
		for _, status := range filter.Statuses {
			args = append(args, string(status))
		}
	}
	if len(filter.Registries) > 0 {
		clauses = append(clauses, "i.registry IN ("+placeholders(len(filter.Registries))+")")
		for _, registry := range filter.Registries {
			args = append(args, registry)
		}
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func scanImageIntel(rows *sql.Rows) ([]domain.ImageIntel, error) {
	records := make([]domain.ImageIntel, 0, 16)

	for rows.Next() {
		var (
			record      domain.ImageIntel
			kind        string
			pinned      int
			updateType  string
			status      string
			firstSeen   string
			lastChecked sql.NullString
			lastSuccess sql.NullString
			nextCheck   sql.NullString
			published   sql.NullString
			labels      string
		)
		if err := rows.Scan(
			&record.ID, &record.Reference, &record.Familiar, &kind, &record.Registry,
			&record.Namespace, &record.Repository, &record.Tag,
			&record.LocalDigest, &record.RemoteDigest, &pinned,
			&record.Platform.OS, &record.Platform.Architecture, &record.Platform.Variant,
			&record.ImageID,
			&updateType, &record.LatestTag, &record.UpdateReason,
			&status, &record.StatusDetail,
			&firstSeen, &lastChecked, &lastSuccess, &nextCheck, &record.FailureCount,
			&published, &record.Vendor, &record.Source, &labels, &record.ETag,
			&record.ContainerCount,
		); err != nil {
			return nil, fmt.Errorf("scan image intel: %w", err)
		}

		record.Kind = domain.RegistryKind(kind)
		record.Pinned = pinned == 1
		record.Update = domain.UpdateType(updateType)
		record.Status = domain.CheckStatus(status)

		var err error
		if record.FirstSeenAt, err = parseTime(firstSeen); err != nil {
			return nil, err
		}
		record.LastCheckedAt = scanOptionalTime(lastChecked)
		record.LastSuccessAt = scanOptionalTime(lastSuccess)
		record.NextCheckAt = scanOptionalTime(nextCheck)
		record.PublishedAt = scanOptionalTime(published)
		record.Labels = unmarshalStringMap(labels)

		records = append(records, record)
	}
	return records, rows.Err()
}
