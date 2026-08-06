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

// AcquisitionRepository owns persistence for image acquisitions.
//
// # The duplicate-work guarantee lives here, not in the service
//
// A partial unique index over the ACTIVE states means at most one acquisition
// per (container, digest) can be in flight. The service checks first because a
// clear refusal is better than a constraint violation, but the index is what
// makes the guarantee hold when two requests race -- and a race is the normal
// case for a button an operator can double-click.
//
// # Records are append-only in the ways that matter
//
// Identity, target, and approval are written once. The lifecycle fields advance
// forwards and stop at a terminal state, enforced by every update below being
// conditional on the current state. A completed acquisition is never rewritten
// by a later attempt: a second request is a second row.
type AcquisitionRepository struct {
	db *sql.DB
}

// ErrAcquisitionActive reports that an acquisition for the same target is
// already in flight.
//
// Raised when the partial unique index refuses an insert, which is the race the
// service's own check cannot close.
var ErrAcquisitionActive = errors.New("an acquisition for this image is already in progress")

// ErrAcquisitionTarget reports that a record does not name one immutable image.
//
// A last-line refusal. Reaching it means a layer above let a target through
// that has no digest, no repository, or a registry host HarborMaster would
// never contact -- and storing it would record an approval for something that
// cannot be safely pulled.
var ErrAcquisitionTarget = errors.New("acquisition target is not a pinned image")

// acquisitionSortFields is the allowlist of sortable columns.
//
// An allowlist rather than validation-by-rejection: the value becomes part of
// the SQL text, so the only safe design is one where caller input SELECTS from
// a fixed set of literals rather than contributing to it.
var acquisitionSortFields = map[string]string{
	"requestedAt": "a.requested_at",
	"completedAt": "a.completed_at",
	"state":       acquisitionStateRankSQL,
	"container":   "a.container_name",
	"id":          "a.id",
}

// acquisitionStateRankSQL orders by lifecycle position rather than by spelling,
// so active work sorts above finished work. Built from literals only.
const acquisitionStateRankSQL = `CASE a.state ` +
	`WHEN 'pulling' THEN 7 ` +
	`WHEN 'verifying' THEN 6 ` +
	`WHEN 'validating' THEN 5 ` +
	`WHEN 'queued' THEN 4 ` +
	`WHEN 'failed' THEN 3 ` +
	`WHEN 'cancelled' THEN 2 ` +
	`WHEN 'expired' THEN 1 ` +
	`ELSE 0 END`

// ValidAcquisitionSortField reports whether field names a sortable column.
func ValidAcquisitionSortField(field string) bool {
	_, ok := acquisitionSortFields[field]
	return ok
}

// AcquisitionFilter narrows an acquisition listing.
//
// Every field is a closed vocabulary or an exact identifier validated by the
// API layer before it arrives. None of them becomes SQL text.
type AcquisitionFilter struct {
	ContainerID string
	PlanID      string
	States      []domain.AcquisitionState
	Failures    []domain.AcquisitionFailure
	// ActiveOnly restricts to work still in progress.
	ActiveOnly bool

	Sort      string
	Ascending bool
	Page      Page
}

const selectAcquisitionColumns = `
	SELECT a.id, a.acquisition_id, a.plan_id, a.container_id, a.container_name,
	       a.target_registry, a.target_repository, a.target_digest, a.target_reference,
	       a.target_os, a.target_arch, a.target_variant,
	       a.state, a.failure, a.refusal, a.message,
	       a.acquired_image_id, a.acquired_digest,
	       a.acquired_os, a.acquired_arch, a.acquired_variant, a.size_bytes,
	       a.layers, a.bytes_transferred, a.progress,
	       a.requested_at, a.started_at, a.completed_at, a.expires_at,
	       a.request_key, a.plan_digest
	FROM acquisitions a`

// ------------------------------------------------------------- creating --

// Create records a new acquisition request.
//
// Returns ErrAcquisitionActive when another acquisition for the same target is
// already in flight, and the EXISTING record when the idempotency key matches
// one already stored. Both are answers rather than errors from the caller's
// point of view: neither starts a second pull.
func (r *AcquisitionRepository) Create(
	ctx context.Context,
	acquisition domain.Acquisition,
	now time.Time,
) (domain.Acquisition, error) {
	if !acquisition.Target.Valid() {
		// A last-line refusal. Reaching it means a layer above let a target
		// through that cannot name one immutable image, and storing it would
		// record an approval for something unpullable.
		return domain.Acquisition{}, ErrAcquisitionTarget
	}

	stamp := formatTime(now.UTC())
	requested := stamp
	if !acquisition.RequestedAt.IsZero() {
		requested = formatTime(acquisition.RequestedAt.UTC())
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Acquisition{}, fmt.Errorf("begin acquisition insert: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	// The idempotency check runs inside the transaction, so a retried request
	// cannot slip between the lookup and the insert.
	if acquisition.RequestKey != "" {
		existing, found, lookupErr := acquisitionByKey(ctx, tx, acquisition.RequestKey)
		if lookupErr != nil {
			return domain.Acquisition{}, lookupErr
		}
		if found {
			return existing, nil
		}
	}

	state := acquisition.State
	if state == "" {
		state = domain.AcquisitionQueued
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO acquisitions
			(acquisition_id, plan_id, container_id, container_name,
			 target_registry, target_repository, target_digest, target_reference,
			 target_os, target_arch, target_variant,
			 state, requested_at, expires_at, request_key, plan_digest,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		acquisition.AcquisitionID, acquisition.PlanID,
		acquisition.ContainerID, acquisition.ContainerName,
		acquisition.Target.Registry, acquisition.Target.Repository,
		acquisition.Target.Digest, acquisition.Target.Reference,
		acquisition.Target.Platform.OS, acquisition.Target.Platform.Architecture,
		acquisition.Target.Platform.Variant,
		string(state), requested, formatTime(acquisition.ExpiresAt.UTC()),
		acquisition.RequestKey, acquisition.PlanDigest,
		stamp, stamp)
	if err != nil {
		// The partial unique index refused: another acquisition for this
		// (container, digest) is active. This is the race the service's own
		// check cannot close, and it is a refusal rather than a fault.
		if isUniqueViolation(err) {
			return domain.Acquisition{}, ErrAcquisitionActive
		}
		return domain.Acquisition{}, fmt.Errorf("insert acquisition: %w", AsError(err))
	}

	if err := insertAcquisitionEvent(ctx, tx, domain.AcquisitionEvent{
		AcquisitionID: acquisition.AcquisitionID,
		State:         state,
		Detail:        "requested by an operator",
	}, stamp); err != nil {
		return domain.Acquisition{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Acquisition{}, fmt.Errorf("commit acquisition insert: %w", AsError(err))
	}

	acquisition.State = state
	return acquisition, nil
}

// acquisitionByKey looks up an acquisition by its idempotency key.
func acquisitionByKey(ctx context.Context, tx *sql.Tx, key string) (domain.Acquisition, bool, error) {
	rows, err := tx.QueryContext(ctx, selectAcquisitionColumns+` WHERE a.request_key = ?`, key)
	if err != nil {
		return domain.Acquisition{}, false, fmt.Errorf("query acquisition by key: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanAcquisitions(rows)
	if err != nil {
		return domain.Acquisition{}, false, err
	}
	if len(found) == 0 {
		return domain.Acquisition{}, false, nil
	}
	return found[0], true, nil
}

// ------------------------------------------------------------ advancing --

// StateChange describes one lifecycle transition.
//
// A single struct rather than a method per transition, so every write goes
// through one conditional update and the "terminal states are terminal" rule is
// enforced in exactly one place.
type StateChange struct {
	AcquisitionID string
	// From restricts the update to acquisitions currently in these states. An
	// empty slice means any ACTIVE state, which is the common case: nothing may
	// move a finished acquisition.
	From []domain.AcquisitionState
	To   domain.AcquisitionState

	Failure domain.AcquisitionFailure
	Refusal domain.AcquisitionRefusal
	Message string

	// Detail is the audit-trail note for this transition.
	Detail string

	// Acquired is set by verification, and records what was actually found on
	// the host rather than what was expected.
	AcquiredImageID  string
	AcquiredDigest   string
	AcquiredPlatform domain.Platform
	SizeBytes        int64

	Layers           int
	BytesTransferred int64
	Progress         string

	// MarkStarted stamps started_at, and is set on the first transition out of
	// queued.
	MarkStarted bool
}

// Advance moves an acquisition to a new state, if it is still where the caller
// believes it is.
//
// Returns false when nothing was updated, which means the acquisition has
// already moved -- cancelled by an operator, or finished by another path. The
// caller treats that as information rather than as an error: losing a race to
// a cancellation is the correct outcome, not a fault.
func (r *AcquisitionRepository) Advance(
	ctx context.Context,
	change StateChange,
	now time.Time,
) (bool, error) {
	stamp := formatTime(now.UTC())

	from := change.From
	if len(from) == 0 {
		// Any active state. A terminal acquisition is never moved.
		from = []domain.AcquisitionState{
			domain.AcquisitionQueued, domain.AcquisitionValidating,
			domain.AcquisitionPulling, domain.AcquisitionVerifying,
		}
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin acquisition update: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	// Every "keep unless supplied" column is a CASE that reads its value TWICE
	// -- once to test it and once to store it -- so each of those values is
	// bound twice, in order. Sanitised values are computed once and reused, so
	// the test and the stored value cannot diverge.
	progress := domain.SanitiseDisplayText(change.Progress, domain.MaxAcquisitionProgressBytes)

	args := []any{
		string(change.To),
		string(change.Failure),
		string(change.Refusal),
		domain.SanitiseDisplayText(change.Message, domain.MaxAcquisitionMessageBytes),
		change.AcquiredImageID, change.AcquiredImageID,
		change.AcquiredDigest, change.AcquiredDigest,
		change.AcquiredPlatform.OS, change.AcquiredPlatform.OS,
		change.AcquiredPlatform.Architecture, change.AcquiredPlatform.Architecture,
		change.AcquiredPlatform.Variant, change.AcquiredPlatform.Variant,
		change.SizeBytes, change.SizeBytes,
		change.Layers, change.Layers,
		change.BytesTransferred, change.BytesTransferred,
		progress, progress,
	}

	// started_at is stamped once and never moved: it is when work actually
	// began, not when the most recent transition happened.
	started := "started_at"
	if change.MarkStarted {
		started = "COALESCE(started_at, ?)"
		args = append(args, stamp)
	}

	// completed_at is set by the transition INTO a terminal state, computed
	// from the target state rather than passed in, so the two cannot disagree.
	completed := "completed_at"
	if change.To.Terminal() {
		completed = "COALESCE(completed_at, ?)"
		args = append(args, stamp)
	}

	args = append(args, stamp, change.AcquisitionID)
	for _, state := range from {
		args = append(args, string(state))
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE acquisitions SET
			state   = ?,
			failure = ?,
			refusal = ?,
			message = ?,
			acquired_image_id = CASE WHEN ? <> '' THEN ? ELSE acquired_image_id END,
			acquired_digest   = CASE WHEN ? <> '' THEN ? ELSE acquired_digest END,
			acquired_os       = CASE WHEN ? <> '' THEN ? ELSE acquired_os END,
			acquired_arch     = CASE WHEN ? <> '' THEN ? ELSE acquired_arch END,
			acquired_variant  = CASE WHEN ? <> '' THEN ? ELSE acquired_variant END,
			size_bytes        = CASE WHEN ? > 0 THEN ? ELSE size_bytes END,
			layers            = CASE WHEN ? > 0 THEN ? ELSE layers END,
			bytes_transferred = CASE WHEN ? > 0 THEN ? ELSE bytes_transferred END,
			progress          = CASE WHEN ? <> '' THEN ? ELSE progress END,
			started_at   = `+started+`,
			completed_at = `+completed+`,
			updated_at   = ?
		WHERE acquisition_id = ?
		  AND state IN (`+placeholders(len(from))+`)`,
		args...)
	if err != nil {
		return false, fmt.Errorf("update acquisition: %w", AsError(err))
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update acquisition: %w", AsError(err))
	}
	if affected == 0 {
		return false, nil
	}

	if err := insertAcquisitionEvent(ctx, tx, domain.AcquisitionEvent{
		AcquisitionID:    change.AcquisitionID,
		State:            change.To,
		Detail:           change.Detail,
		BytesTransferred: change.BytesTransferred,
		Layers:           change.Layers,
	}, stamp); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit acquisition update: %w", AsError(err))
	}
	return true, nil
}

// RecordProgress stores one bounded progress observation.
//
// Only while the acquisition is PULLING. A progress row arriving after
// cancellation would extend the audit trail of an acquisition that has already
// finished, and the trail must describe what happened rather than what was
// still being reported.
func (r *AcquisitionRepository) RecordProgress(
	ctx context.Context,
	acquisitionID string,
	progress string,
	bytesTransferred int64,
	layers int,
	now time.Time,
	maxEvents int,
) error {
	stamp := formatTime(now.UTC())
	detail := domain.SanitiseDisplayText(progress, domain.MaxAcquisitionProgressBytes)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin acquisition progress: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE acquisitions SET
			progress          = ?,
			bytes_transferred = CASE WHEN ? > bytes_transferred THEN ? ELSE bytes_transferred END,
			layers            = CASE WHEN ? > layers THEN ? ELSE layers END,
			updated_at        = ?
		WHERE acquisition_id = ? AND state = 'pulling'`,
		detail, bytesTransferred, bytesTransferred, layers, layers, stamp, acquisitionID)
	if err != nil {
		return fmt.Errorf("update acquisition progress: %w", AsError(err))
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		// Not pulling any more. Nothing to record, and nothing wrong.
		return nil
	}

	// The audit trail is capped per acquisition. The adapter already
	// rate-limits emission and the service caps its own writes; this is the
	// last of the three independent bounds, and the one that holds even if a
	// caller ignores the others.
	if maxEvents > 0 {
		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM acquisition_events WHERE acquisition_id = ?`,
			acquisitionID).Scan(&count); err != nil {
			return fmt.Errorf("count acquisition events: %w", AsError(err))
		}
		if count >= maxEvents {
			// Bounded rather than rotated: the transitions written early are
			// the ones worth keeping, and dropping them to make room for
			// progress noise would lose the audit trail to the chatter.
			return tx.Commit()
		}
	}

	if err := insertAcquisitionEvent(ctx, tx, domain.AcquisitionEvent{
		AcquisitionID:    acquisitionID,
		State:            domain.AcquisitionPulling,
		Detail:           detail,
		BytesTransferred: bytesTransferred,
		Layers:           layers,
	}, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

func insertAcquisitionEvent(
	ctx context.Context,
	tx *sql.Tx,
	event domain.AcquisitionEvent,
	stamp string,
) error {
	at := stamp
	if !event.At.IsZero() {
		at = formatTime(event.At.UTC())
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO acquisition_events
			(acquisition_id, state, detail, bytes_transferred, layers, at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		event.AcquisitionID, string(event.State),
		domain.SanitiseDisplayText(event.Detail, domain.MaxAcquisitionMessageBytes),
		event.BytesTransferred, event.Layers, at)
	if err != nil {
		return fmt.Errorf("insert acquisition event: %w", AsError(err))
	}
	return nil
}

// --------------------------------------------------------------- reading --

// Get returns one acquisition by its immutable id.
func (r *AcquisitionRepository) Get(ctx context.Context, acquisitionID string) (domain.Acquisition, error) {
	rows, err := r.db.QueryContext(ctx,
		selectAcquisitionColumns+` WHERE a.acquisition_id = ?`, acquisitionID)
	if err != nil {
		return domain.Acquisition{}, fmt.Errorf("query acquisition: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanAcquisitions(rows)
	if err != nil {
		return domain.Acquisition{}, err
	}
	if len(found) == 0 {
		return domain.Acquisition{}, ErrNotFound
	}
	return found[0], nil
}

// List returns a page of acquisitions and the total matching the filter.
func (r *AcquisitionRepository) List(
	ctx context.Context,
	filter AcquisitionFilter,
) ([]domain.Acquisition, int, error) {
	where, args := acquisitionWhere(filter)

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM acquisitions a`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count acquisitions: %w", AsError(err))
	}

	column, ok := acquisitionSortFields[filter.Sort]
	if !ok {
		column = "a.requested_at"
	}
	direction := "DESC"
	if filter.Ascending {
		direction = "ASC"
	}

	page := filter.Page.normalise()
	// column and direction come from the allowlist above and a two-valued
	// switch; neither is caller text. Every VALUE is bound.
	query := selectAcquisitionColumns + where +
		` ORDER BY ` + column + ` ` + direction + `, a.id ` + direction +
		` LIMIT ? OFFSET ?`

	rows, err := r.db.QueryContext(ctx, query, append(args, page.Limit, page.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query acquisitions: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanAcquisitions(rows)
	if err != nil {
		return nil, 0, err
	}
	return found, total, nil
}

// Events returns an acquisition's audit trail, oldest first.
//
// Oldest first, unlike every other history in HarborMaster: this one is read as
// a narrative of a single operation rather than scanned for the most recent
// entry.
func (r *AcquisitionRepository) Events(
	ctx context.Context,
	acquisitionID string,
	limit int,
) ([]domain.AcquisitionEvent, error) {
	if limit < 1 || limit > 500 {
		limit = 200
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, acquisition_id, state, detail, bytes_transferred, layers, at
		FROM acquisition_events
		WHERE acquisition_id = ?
		ORDER BY id
		LIMIT ?`, acquisitionID, limit)
	if err != nil {
		return nil, fmt.Errorf("query acquisition events: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	events := make([]domain.AcquisitionEvent, 0, 16)
	for rows.Next() {
		var (
			event domain.AcquisitionEvent
			state string
			at    string
		)
		if err := rows.Scan(&event.ID, &event.AcquisitionID, &state, &event.Detail,
			&event.BytesTransferred, &event.Layers, &at); err != nil {
			return nil, fmt.Errorf("scan acquisition event: %w", err)
		}
		event.State = domain.AcquisitionState(state)

		parsed, err := parseTime(at)
		if err != nil {
			return nil, err
		}
		event.At = parsed
		events = append(events, event)
	}
	return events, rows.Err()
}

// ActiveCount reports how many acquisitions are in flight, in total and for one
// registry.
//
// Both counts in one query, because the service needs both to decide whether a
// request fits within its limits and two queries would give it two moments.
func (r *AcquisitionRepository) ActiveCount(ctx context.Context, registry string) (total, perRegistry int, err error) {
	const activeStates = `('queued', 'validating', 'pulling', 'verifying')`

	err = r.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN target_registry = ? THEN 1 ELSE 0 END), 0)
		FROM acquisitions
		WHERE state IN `+activeStates, registry).Scan(&total, &perRegistry)
	if err != nil {
		return 0, 0, fmt.Errorf("count active acquisitions: %w", AsError(err))
	}
	return total, perRegistry, nil
}

// ActiveForTarget reports whether an acquisition for this image is in flight.
//
// The service's own duplicate check, so it can answer "already downloading"
// rather than letting the unique index refuse the insert with a less specific
// error. The index remains the guarantee; this is the courtesy.
func (r *AcquisitionRepository) ActiveForTarget(
	ctx context.Context,
	containerID, digest string,
) (bool, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM acquisitions
		WHERE container_id = ? AND target_digest = ?
		  AND state IN ('queued', 'validating', 'pulling', 'verifying')`,
		containerID, digest).Scan(&count); err != nil {
		return false, fmt.Errorf("check active acquisition: %w", AsError(err))
	}
	return count > 0, nil
}

// ByRequestKey returns the acquisition a caller's idempotency key created.
//
// Answered before any check that could refuse: a retried request is asking what
// happened to the work it already created, not whether it may create more.
func (r *AcquisitionRepository) ByRequestKey(
	ctx context.Context,
	key string,
) (domain.Acquisition, bool, error) {
	if key == "" {
		return domain.Acquisition{}, false, nil
	}

	rows, err := r.db.QueryContext(ctx,
		selectAcquisitionColumns+` WHERE a.request_key = ?`, key)
	if err != nil {
		return domain.Acquisition{}, false, fmt.Errorf("query acquisition by key: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanAcquisitions(rows)
	if err != nil {
		return domain.Acquisition{}, false, err
	}
	if len(found) == 0 {
		return domain.Acquisition{}, false, nil
	}
	return found[0], true, nil
}

// Claimable returns queued acquisitions, oldest first.
//
// The worker's work list. Ordered by insertion so a queue is a queue.
func (r *AcquisitionRepository) Claimable(ctx context.Context, limit int) ([]domain.Acquisition, error) {
	if limit < 1 {
		limit = 16
	}
	rows, err := r.db.QueryContext(ctx,
		selectAcquisitionColumns+` WHERE a.state = 'queued' ORDER BY a.id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query claimable acquisitions: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	return scanAcquisitions(rows)
}

// Summary computes the dashboard aggregate.
func (r *AcquisitionRepository) Summary(ctx context.Context) (domain.AcquisitionSummary, error) {
	summary := domain.AcquisitionSummary{
		ByState:   make(map[domain.AcquisitionState]int, len(domain.AcquisitionStates)),
		ByFailure: make(map[domain.AcquisitionFailure]int, len(domain.AcquisitionFailures)),
	}

	rows, err := r.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM acquisitions GROUP BY state`)
	if err != nil {
		return summary, fmt.Errorf("summarise acquisitions: %w", AsError(err))
	}
	if err := scanCountsInto(rows, func(key string, count int) {
		state := domain.AcquisitionState(key)
		summary.ByState[state] = count
		summary.Total += count
		if state.Active() {
			summary.Active += count
		}
		if state == domain.AcquisitionSucceeded {
			summary.Succeeded += count
		}
		if state == domain.AcquisitionFailed {
			summary.Failed += count
		}
	}); err != nil {
		return summary, err
	}

	failureRows, err := r.db.QueryContext(ctx,
		`SELECT failure, COUNT(*) FROM acquisitions WHERE failure <> '' GROUP BY failure`)
	if err != nil {
		return summary, fmt.Errorf("summarise acquisition failures: %w", AsError(err))
	}
	if err := scanCountsInto(failureRows, func(key string, count int) {
		summary.ByFailure[domain.AcquisitionFailure(key)] = count
	}); err != nil {
		return summary, err
	}

	var completed sql.NullString
	if err := r.db.QueryRowContext(ctx,
		`SELECT MAX(completed_at) FROM acquisitions`).Scan(&completed); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return summary, fmt.Errorf("read last acquisition: %w", AsError(err))
	}
	if completed.Valid && completed.String != "" {
		parsed, err := parseTime(completed.String)
		if err != nil {
			return summary, err
		}
		summary.LastCompletedAt = &parsed
	}
	return summary, nil
}

// scanCountsInto reads a two-column grouped result.
func scanCountsInto(rows *sql.Rows, apply func(string, int)) error {
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			key   string
			count int
		)
		if err := rows.Scan(&key, &count); err != nil {
			return fmt.Errorf("scan acquisition aggregate: %w", err)
		}
		apply(key, count)
	}
	return rows.Err()
}

// ------------------------------------------------------------- recovery --

// RecoverInterrupted marks acquisitions left mid-flight by a crash or restart.
//
// # Why this exists
//
// An acquisition in `pulling` is a claim about a process that no longer exists.
// The daemon may well have finished the transfer, but HarborMaster did not
// verify it, and an unverified image must never be recorded as acquired.
//
// So the interrupted rows are moved to `failed` with an honest classification
// rather than being resumed. Resuming would mean trusting a transfer nobody
// watched, and re-verifying would mean asserting that the image on the host now
// is the one that particular pull produced -- which is exactly the assumption
// the verification step exists to avoid making.
//
// Called once at startup, before the worker begins.
func (r *AcquisitionRepository) RecoverInterrupted(ctx context.Context, now time.Time) (int64, error) {
	stamp := formatTime(now.UTC())

	result, err := r.db.ExecContext(ctx, `
		UPDATE acquisitions SET
			state        = 'failed',
			failure      = 'internal',
			message      = ?,
			completed_at = COALESCE(completed_at, ?),
			updated_at   = ?
		WHERE state IN ('validating', 'pulling', 'verifying')`,
		"HarborMaster restarted while this acquisition was in progress, so its outcome was never confirmed",
		stamp, stamp)
	if err != nil {
		return 0, fmt.Errorf("recover interrupted acquisitions: %w", AsError(err))
	}

	recovered, _ := result.RowsAffected()
	return recovered, nil
}

// ExpireStale abandons requests that sat queued past their deadline.
//
// An expired request was never validated and never pulled. Running it later
// would be acting on an approval whose evidence has aged, which is precisely
// what the deadline exists to prevent.
func (r *AcquisitionRepository) ExpireStale(ctx context.Context, now time.Time, batch int) (int64, error) {
	if batch < 1 {
		batch = 100
	}
	stamp := formatTime(now.UTC())

	result, err := r.db.ExecContext(ctx, `
		UPDATE acquisitions SET
			state        = 'expired',
			message      = ?,
			completed_at = COALESCE(completed_at, ?),
			updated_at   = ?
		WHERE id IN (
			SELECT id FROM acquisitions
			WHERE state = 'queued' AND expires_at < ?
			LIMIT ?
		)`,
		"this request waited longer than its deadline and was abandoned without being started",
		stamp, stamp, stamp, batch)
	if err != nil {
		return 0, fmt.Errorf("expire acquisitions: %w", AsError(err))
	}

	expired, _ := result.RowsAffected()
	return expired, nil
}

// Prune removes completed acquisitions older than the cutoff.
//
// Terminal rows only, and never the most recent one per container: a completed
// audit record is the evidence that an image was downloaded, and silently
// removing the last one would leave a host with an image nothing accounts for.
func (r *AcquisitionRepository) Prune(ctx context.Context, cutoff time.Time, batch int) (int64, error) {
	if batch < 1 {
		batch = 200
	}

	result, err := r.db.ExecContext(ctx, `
		DELETE FROM acquisitions
		WHERE id IN (
			SELECT id FROM acquisitions
			WHERE completed_at IS NOT NULL
			  AND completed_at < ?
			  AND id NOT IN (SELECT MAX(id) FROM acquisitions GROUP BY container_id)
			LIMIT ?
		)`, formatTime(cutoff.UTC()), batch)
	if err != nil {
		return 0, fmt.Errorf("prune acquisitions: %w", AsError(err))
	}

	pruned, _ := result.RowsAffected()
	return pruned, nil
}

// ---------------------------------------------------------------- helpers --

// acquisitionWhere builds the filter clause. Every value is bound; only the
// placeholder RUN length varies with the input.
func acquisitionWhere(filter AcquisitionFilter) (string, []any) {
	clauses := make([]string, 0, 5)
	args := make([]any, 0, 8)

	if filter.ContainerID != "" {
		clauses = append(clauses, "a.container_id = ?")
		args = append(args, filter.ContainerID)
	}
	if filter.PlanID != "" {
		clauses = append(clauses, "a.plan_id = ?")
		args = append(args, filter.PlanID)
	}
	if filter.ActiveOnly {
		clauses = append(clauses,
			"a.state IN ('queued', 'validating', 'pulling', 'verifying')")
	}
	if len(filter.States) > 0 {
		clauses = append(clauses, "a.state IN ("+placeholders(len(filter.States))+")")
		for _, state := range filter.States {
			args = append(args, string(state))
		}
	}
	if len(filter.Failures) > 0 {
		clauses = append(clauses, "a.failure IN ("+placeholders(len(filter.Failures))+")")
		for _, failure := range filter.Failures {
			args = append(args, string(failure))
		}
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func scanAcquisitions(rows *sql.Rows) ([]domain.Acquisition, error) {
	out := make([]domain.Acquisition, 0, 16)

	for rows.Next() {
		var (
			acquisition domain.Acquisition
			state       string
			failure     string
			refusal     string
			requested   string
			started     sql.NullString
			completed   sql.NullString
			expires     string
		)
		if err := rows.Scan(
			&acquisition.ID, &acquisition.AcquisitionID, &acquisition.PlanID,
			&acquisition.ContainerID, &acquisition.ContainerName,
			&acquisition.Target.Registry, &acquisition.Target.Repository,
			&acquisition.Target.Digest, &acquisition.Target.Reference,
			&acquisition.Target.Platform.OS, &acquisition.Target.Platform.Architecture,
			&acquisition.Target.Platform.Variant,
			&state, &failure, &refusal, &acquisition.Message,
			&acquisition.AcquiredImageID, &acquisition.AcquiredDigest,
			&acquisition.AcquiredPlatform.OS, &acquisition.AcquiredPlatform.Architecture,
			&acquisition.AcquiredPlatform.Variant, &acquisition.SizeBytes,
			&acquisition.Layers, &acquisition.BytesTransferred, &acquisition.Progress,
			&requested, &started, &completed, &expires,
			&acquisition.RequestKey, &acquisition.PlanDigest,
		); err != nil {
			return nil, fmt.Errorf("scan acquisition: %w", err)
		}

		acquisition.State = domain.AcquisitionState(state)
		acquisition.Failure = domain.AcquisitionFailure(failure)
		acquisition.Refusal = domain.AcquisitionRefusal(refusal)

		var err error
		if acquisition.RequestedAt, err = parseTime(requested); err != nil {
			return nil, err
		}
		if acquisition.ExpiresAt, err = parseTime(expires); err != nil {
			return nil, err
		}
		acquisition.StartedAt = scanOptionalTime(started)
		acquisition.CompletedAt = scanOptionalTime(completed)

		out = append(out, acquisition)
	}
	return out, rows.Err()
}
