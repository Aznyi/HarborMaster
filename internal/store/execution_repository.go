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

// ExecutionRepository owns persistence for container recreations.
//
// # Two guarantees live here rather than in the service
//
// A partial unique index means at most one ACTIVE execution per container, and
// a full unique index means one execution per acquisition, ever. The service
// checks both first because a clear refusal is better than a constraint
// violation, but the indexes are what make the guarantees hold when two
// requests race -- and a race is the normal case for a button someone can
// double-click.
//
// # Checkpoints are written separately from states, and must not fail quietly
//
// Checkpoint is the only write in HarborMaster whose FAILURE is itself a safety
// event. A state transition that does not land leaves a row saying something
// slightly out of date; a checkpoint that does not land leaves HarborMaster
// unable to say what it has already done to the host. So Checkpoint returns an
// error on every failure path including "no row matched", and the caller stops
// rather than attempting the next mutation.
type ExecutionRepository struct {
	db *sql.DB
}

// Errors this repository can produce.
var (
	// ErrExecutionActive reports that a recreation for the same container is
	// already in flight.
	ErrExecutionActive = errors.New("a recreation for this container is already in progress")
	// ErrAcquisitionConsumed reports that the acquisition has already been used
	// for a recreation. Single use, by design: see migration 0010.
	ErrAcquisitionConsumed = errors.New("this acquisition has already been used for a recreation")
	// ErrExecutionTarget reports that a record does not name one immutable
	// image. A last-line refusal; reaching it means a layer above let something
	// through that cannot be safely created from.
	ErrExecutionTarget = errors.New("execution target is not a pinned image")
	// ErrCheckpointNotWritten reports that a checkpoint could not be recorded.
	//
	// Never ignorable. The caller has just changed the host and cannot prove it
	// recorded the fact, so the only safe action is to stop.
	ErrCheckpointNotWritten = errors.New("execution checkpoint was not recorded")
)

// executionSortFields is the allowlist of sortable columns.
//
// An allowlist rather than validation-by-rejection: the value becomes part of
// the SQL text, so the only safe design is one where caller input SELECTS from
// a fixed set of literals rather than contributing to it.
var executionSortFields = map[string]string{
	"requestedAt": "e.requested_at",
	"completedAt": "e.completed_at",
	"state":       executionStateRankSQL,
	"container":   "e.container_name",
	"id":          "e.id",
}

// executionStateRankSQL orders by lifecycle position rather than by spelling,
// so active work sorts above finished work and a failure that left containers
// behind sorts above one that did not. Built from literals only.
const executionStateRankSQL = `CASE ` +
	`WHEN e.state = 'failed' AND e.checkpoint <> '' THEN 10 ` +
	`WHEN e.state = 'verifying' THEN 9 ` +
	`WHEN e.state = 'starting' THEN 8 ` +
	`WHEN e.state = 'creating' THEN 7 ` +
	`WHEN e.state = 'capturing' THEN 6 ` +
	`WHEN e.state = 'validating' THEN 5 ` +
	`WHEN e.state = 'queued' THEN 4 ` +
	`WHEN e.state = 'failed' THEN 3 ` +
	`WHEN e.state = 'cancelled' THEN 2 ` +
	`WHEN e.state = 'expired' THEN 1 ` +
	`ELSE 0 END`

// ValidExecutionSortField reports whether field names a sortable column.
func ValidExecutionSortField(field string) bool {
	_, ok := executionSortFields[field]
	return ok
}

// activeExecutionStatesSQL is the active set as a SQL literal list.
//
// Written once. The service, the indexes, and this file must agree about what
// "active" means, and three copies of a list is how they stop agreeing.
const activeExecutionStatesSQL = `('queued', 'validating', 'capturing', ` +
	`'creating', 'starting', 'verifying')`

// ExecutionFilter narrows an execution listing.
//
// Every field is a closed vocabulary or an exact identifier validated by the
// API layer before it arrives. None of them becomes SQL text.
type ExecutionFilter struct {
	ContainerID string
	PlanID      string
	States      []domain.ExecutionState
	Failures    []domain.ExecutionFailure
	// ActiveOnly restricts to work still in progress.
	ActiveOnly bool
	// NeedsAttention restricts to failures that left containers on the host.
	NeedsAttention bool

	Sort      string
	Ascending bool
	Page      Page
}

const selectExecutionColumns = `
	SELECT e.id, e.execution_id, e.acquisition_id, e.plan_id, e.snapshot_id,
	       e.container_id, e.container_name,
	       e.old_image, e.old_image_id, e.old_image_digest,
	       e.target_registry, e.target_repository, e.target_digest,
	       e.target_reference, e.target_image_id,
	       e.target_os, e.target_arch, e.target_variant,
	       e.state, e.checkpoint, e.failure, e.refusal, e.message,
	       e.replacement_id, e.parked_name, e.quarantine_name, e.original_removed,
	       e.verify_health, e.verify_image, e.verify_preservation, e.verify_network,
	       e.health_state, e.health_checked, e.stability_seconds,
	       e.preservation_report, e.recovery_plan,
	       e.requested_at, e.started_at, e.mutated_at, e.completed_at, e.expires_at,
	       e.request_key, e.plan_digest,
	       e.requested_by_user_id, e.requested_by_username
	FROM executions e`

// -------------------------------------------------------------- creating --

// Create records a new recreation request.
//
// Returns ErrExecutionActive when a recreation for the container is already in
// flight, ErrAcquisitionConsumed when the acquisition has been used before, and
// the EXISTING record when the idempotency key matches one already stored.
func (r *ExecutionRepository) Create(
	ctx context.Context,
	execution domain.Execution,
	now time.Time,
) (domain.Execution, error) {
	if !execution.Target.Valid() {
		return domain.Execution{}, ErrExecutionTarget
	}
	if !domain.ValidExecutionID(execution.ExecutionID) {
		return domain.Execution{}, ErrExecutionTarget
	}

	stamp := formatTime(now.UTC())
	requested := stamp
	if !execution.RequestedAt.IsZero() {
		requested = formatTime(execution.RequestedAt.UTC())
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Execution{}, fmt.Errorf("begin execution insert: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	// The idempotency check runs inside the transaction, so a retried request
	// cannot slip between the lookup and the insert.
	if execution.RequestKey != "" {
		existing, found, lookupErr := executionByKey(ctx, tx, execution.RequestKey)
		if lookupErr != nil {
			return domain.Execution{}, lookupErr
		}
		if found {
			return existing, nil
		}
	}

	state := execution.State
	if state == "" {
		state = domain.ExecutionQueued
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO executions
			(execution_id, acquisition_id, plan_id, snapshot_id,
			 container_id, container_name,
			 old_image, old_image_id, old_image_digest,
			 target_registry, target_repository, target_digest,
			 target_reference, target_image_id,
			 target_os, target_arch, target_variant,
			 state, requested_at, expires_at, request_key, plan_digest,
			 requested_by_user_id, requested_by_username,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		execution.ExecutionID, execution.AcquisitionID, execution.PlanID, execution.SnapshotID,
		execution.ContainerID, execution.ContainerName,
		execution.OldImage, execution.OldImageID, execution.OldImageDigest,
		execution.Target.Registry, execution.Target.Repository, execution.Target.Digest,
		execution.Target.Reference, execution.Target.ImageID,
		execution.Target.Platform.OS, execution.Target.Platform.Architecture,
		execution.Target.Platform.Variant,
		string(state), requested, formatTime(execution.ExpiresAt.UTC()),
		execution.RequestKey, execution.PlanDigest,
		execution.RequestedBy.UserID, execution.RequestedBy.Username,
		stamp, stamp)
	if err != nil {
		if isUniqueViolation(err) {
			// Two indexes can refuse this insert and they mean quite different
			// things, so the row is looked up to say which. Doing it inside the
			// transaction means the answer is consistent with the refusal.
			return domain.Execution{}, classifyExecutionConflict(ctx, tx, execution)
		}
		return domain.Execution{}, fmt.Errorf("insert execution: %w", AsError(err))
	}

	if err := insertExecutionEvent(ctx, tx, domain.ExecutionEvent{
		ExecutionID: execution.ExecutionID,
		State:       state,
		Detail:      "requested by an operator",
	}, stamp); err != nil {
		return domain.Execution{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Execution{}, fmt.Errorf("commit execution insert: %w", AsError(err))
	}

	execution.State = state
	return execution, nil
}

// classifyExecutionConflict decides which unique index refused an insert.
//
// A caller told "already in progress" when the truth is "that acquisition was
// used last week" would retry forever. The two are distinguished by looking,
// rather than by parsing the driver's error text -- which is not part of any
// contract and changes between versions.
func classifyExecutionConflict(ctx context.Context, tx *sql.Tx, execution domain.Execution) error {
	var consumed int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM executions WHERE acquisition_id = ?`,
		execution.AcquisitionID).Scan(&consumed); err == nil && consumed > 0 {
		return ErrAcquisitionConsumed
	}
	return ErrExecutionActive
}

// executionByKey looks up an execution by its idempotency key.
func executionByKey(ctx context.Context, tx *sql.Tx, key string) (domain.Execution, bool, error) {
	rows, err := tx.QueryContext(ctx, selectExecutionColumns+` WHERE e.request_key = ?`, key)
	if err != nil {
		return domain.Execution{}, false, fmt.Errorf("query execution by key: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanExecutions(rows)
	if err != nil {
		return domain.Execution{}, false, err
	}
	if len(found) == 0 {
		return domain.Execution{}, false, nil
	}
	return found[0], true, nil
}

// ------------------------------------------------------------ advancing --

// ExecutionChange describes one lifecycle transition.
//
// A single struct rather than a method per transition, so every write goes
// through one conditional update and the "terminal states are terminal" rule is
// enforced in exactly one place.
type ExecutionChange struct {
	ExecutionID string
	// From restricts the update to executions currently in these states. An
	// empty slice means any ACTIVE state: nothing may move a finished
	// execution.
	From []domain.ExecutionState
	To   domain.ExecutionState

	// Checkpoint, when set, is recorded alongside the transition. Most
	// checkpoints are written by Checkpoint instead; this exists for the
	// transitions that carry one atomically, such as the final success.
	Checkpoint domain.ExecutionCheckpoint

	Failure domain.ExecutionFailure
	Refusal domain.ExecutionRefusal
	Message string
	Detail  string

	// Host state. Each is written the moment it becomes true rather than at the
	// end, because a crash between "true on the host" and "recorded" is exactly
	// what the checkpoint design exists to survive.
	ReplacementID   string
	ParkedName      string
	QuarantineName  string
	OriginalRemoved bool

	// Verification, when the transition carries a verdict.
	Verification *domain.ExecutionVerification
	// Recovery is the manual recovery plan for a failure that left containers.
	Recovery *domain.RecoveryPlan

	// MarkStarted stamps started_at, and MarkMutated stamps mutated_at. Both
	// are set once and never moved.
	MarkStarted bool
	MarkMutated bool
}

// Advance moves an execution to a new state, if it is still where the caller
// believes it is.
//
// Returns false when nothing was updated, which means the execution has already
// moved -- cancelled by an operator, or finished by another path.
func (r *ExecutionRepository) Advance(
	ctx context.Context,
	change ExecutionChange,
	now time.Time,
) (bool, error) {
	stamp := formatTime(now.UTC())

	from := change.From
	if len(from) == 0 {
		from = activeExecutionStates()
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin execution update: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	assignments := []string{
		"state = ?",
		"failure = ?",
		"refusal = ?",
		"message = ?",
	}
	args := []any{
		string(change.To),
		string(change.Failure),
		string(change.Refusal),
		domain.SanitiseDisplayText(change.Message, domain.MaxExecutionMessageBytes),
	}

	// Every "keep unless supplied" column is written only when the caller
	// supplied it, so a transition that says nothing about the replacement id
	// cannot blank one that was already recorded.
	if change.Checkpoint != domain.CheckpointNone {
		assignments = append(assignments, "checkpoint = ?")
		args = append(args, string(change.Checkpoint))
	}
	if change.ReplacementID != "" {
		assignments = append(assignments, "replacement_id = ?")
		args = append(args, change.ReplacementID)
	}
	if change.ParkedName != "" {
		assignments = append(assignments, "parked_name = ?")
		args = append(args, change.ParkedName)
	}
	if change.QuarantineName != "" {
		assignments = append(assignments, "quarantine_name = ?")
		args = append(args, change.QuarantineName)
	}
	if change.OriginalRemoved {
		assignments = append(assignments, "original_removed = 1")
	}

	if change.Verification != nil {
		verification := *change.Verification
		assignments = append(assignments,
			"verify_health = ?", "verify_image = ?",
			"verify_preservation = ?", "verify_network = ?",
			"health_state = ?", "health_checked = ?", "stability_seconds = ?",
			"preservation_report = ?")
		args = append(args,
			resultOrUnknown(verification.Health),
			resultOrUnknown(verification.Image),
			resultOrUnknown(verification.Preservation),
			resultOrUnknown(verification.Network),
			string(verification.HealthState),
			boolToInt(verification.HealthChecked),
			verification.StabilitySeconds,
			encodeJSONColumn(verification.Report))
	}
	if change.Recovery != nil {
		assignments = append(assignments, "recovery_plan = ?")
		args = append(args, encodeJSONColumn(change.Recovery))
	}

	if change.MarkStarted {
		assignments = append(assignments, "started_at = COALESCE(started_at, ?)")
		args = append(args, stamp)
	}
	if change.MarkMutated {
		assignments = append(assignments, "mutated_at = COALESCE(mutated_at, ?)")
		args = append(args, stamp)
	}
	// completed_at is computed from the TARGET state rather than passed in, so
	// the two cannot disagree.
	if change.To.Terminal() {
		assignments = append(assignments, "completed_at = COALESCE(completed_at, ?)")
		args = append(args, stamp)
	}

	assignments = append(assignments, "updated_at = ?")
	args = append(args, stamp, change.ExecutionID)
	for _, state := range from {
		args = append(args, string(state))
	}

	// Every element of `assignments` is a compile-time literal from this
	// function. Only the placeholder RUN length varies with the input, and
	// every VALUE is bound.
	result, err := tx.ExecContext(ctx, `
		UPDATE executions SET `+strings.Join(assignments, ", ")+`
		WHERE execution_id = ?
		  AND state IN (`+placeholders(len(from))+`)`, args...)
	if err != nil {
		return false, fmt.Errorf("update execution: %w", AsError(err))
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update execution: %w", AsError(err))
	}
	if affected == 0 {
		return false, nil
	}

	if err := insertExecutionEvent(ctx, tx, domain.ExecutionEvent{
		ExecutionID: change.ExecutionID,
		State:       change.To,
		Checkpoint:  change.Checkpoint,
		Detail:      change.Detail,
	}, stamp); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit execution update: %w", AsError(err))
	}
	return true, nil
}

// ExecutionCheckpointWrite records that one mutation completed.
type ExecutionCheckpointWrite struct {
	ExecutionID string
	Checkpoint  domain.ExecutionCheckpoint
	// Detail is the audit-trail note. HarborMaster's own words.
	Detail string

	// The host facts this checkpoint establishes, written in the SAME
	// transaction as the checkpoint itself. Recording "the replacement was
	// created" without recording WHICH container was created would leave a
	// recovery pass unable to find it.
	ReplacementID   string
	ParkedName      string
	QuarantineName  string
	OriginalRemoved bool

	// MarkMutated stamps mutated_at, set on the first checkpoint.
	MarkMutated bool
}

// Checkpoint records that one Docker mutation completed.
//
// # Why this is not just another Advance
//
// A checkpoint is written AFTER a mutation succeeds and BEFORE the next one is
// attempted, and its failure is a safety event rather than a nuisance. So:
//
//   - It does not change `state`. The pipeline may be in the middle of a state,
//     and moving the state would lose the distinction between "doing this" and
//     "has done this".
//   - It returns an error when NO ROW MATCHED. Everywhere else in HarborMaster
//     that is treated as information -- something else moved the row first --
//     but here it means the host was changed and the fact was not recorded,
//     which the caller must not proceed past.
//   - It refuses to move a checkpoint BACKWARDS, so a late write from a
//     goroutine that lost a race cannot un-record progress.
func (r *ExecutionRepository) Checkpoint(
	ctx context.Context,
	write ExecutionCheckpointWrite,
	now time.Time,
) error {
	if write.Checkpoint == domain.CheckpointNone {
		return fmt.Errorf("%w: no checkpoint supplied", ErrCheckpointNotWritten)
	}
	rank := checkpointRank(write.Checkpoint)
	if rank == 0 {
		return fmt.Errorf("%w: unknown checkpoint", ErrCheckpointNotWritten)
	}

	stamp := formatTime(now.UTC())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCheckpointNotWritten, AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	assignments := []string{"checkpoint = ?"}
	args := []any{string(write.Checkpoint)}

	if write.ReplacementID != "" {
		assignments = append(assignments, "replacement_id = ?")
		args = append(args, write.ReplacementID)
	}
	if write.ParkedName != "" {
		assignments = append(assignments, "parked_name = ?")
		args = append(args, write.ParkedName)
	}
	if write.QuarantineName != "" {
		assignments = append(assignments, "quarantine_name = ?")
		args = append(args, write.QuarantineName)
	}
	if write.OriginalRemoved {
		assignments = append(assignments, "original_removed = 1")
	}
	if write.MarkMutated {
		assignments = append(assignments, "mutated_at = COALESCE(mutated_at, ?)")
		args = append(args, stamp)
	}
	assignments = append(assignments, "updated_at = ?")
	args = append(args, stamp, write.ExecutionID)

	// The rank comparison is what makes a checkpoint monotonic. Rendered from a
	// literal CASE over the closed vocabulary; the incoming rank is bound.
	args = append(args, rank)

	result, err := tx.ExecContext(ctx, `
		UPDATE executions SET `+strings.Join(assignments, ", ")+`
		WHERE execution_id = ?
		  AND `+checkpointRankSQL+` < ?`, args...)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCheckpointNotWritten, AsError(err))
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCheckpointNotWritten, AsError(err))
	}
	if affected == 0 {
		// Either the row is gone, or its checkpoint is already at or past this
		// one. The second is benign on a retry and the first is not, and the
		// caller cannot tell them apart -- so it must treat both as uncertain
		// and stop.
		return fmt.Errorf("%w: no row advanced to %s", ErrCheckpointNotWritten, write.Checkpoint)
	}

	if err := insertExecutionEvent(ctx, tx, domain.ExecutionEvent{
		ExecutionID: write.ExecutionID,
		State:       domain.ExecutionCreating,
		Checkpoint:  write.Checkpoint,
		Detail:      write.Detail,
	}, stamp); err != nil {
		return fmt.Errorf("%w: %w", ErrCheckpointNotWritten, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: %w", ErrCheckpointNotWritten, AsError(err))
	}
	return nil
}

// checkpointRankSQL orders checkpoints by pipeline position.
//
// Built from literals only, and the ordering matches domain.ExecutionCheckpoint
// exactly. Quarantine ranks alongside verification because it is a terminal
// side branch rather than progress toward removal.
const checkpointRankSQL = `CASE checkpoint ` +
	`WHEN 'originalStopped' THEN 1 ` +
	`WHEN 'originalParked' THEN 2 ` +
	`WHEN 'replacementCreated' THEN 3 ` +
	`WHEN 'replacementStarted' THEN 4 ` +
	`WHEN 'replacementVerified' THEN 5 ` +
	`WHEN 'replacementQuarantined' THEN 6 ` +
	`WHEN 'originalRemoved' THEN 7 ` +
	`ELSE 0 END`

// checkpointRank mirrors checkpointRankSQL in Go. Zero means unknown.
func checkpointRank(checkpoint domain.ExecutionCheckpoint) int {
	switch checkpoint {
	case domain.CheckpointOriginalStopped:
		return 1
	case domain.CheckpointOriginalParked:
		return 2
	case domain.CheckpointReplacementCreated:
		return 3
	case domain.CheckpointReplacementStarted:
		return 4
	case domain.CheckpointReplacementVerified:
		return 5
	case domain.CheckpointReplacementQuarantined:
		return 6
	case domain.CheckpointOriginalRemoved:
		return 7
	default:
		return 0
	}
}

func insertExecutionEvent(
	ctx context.Context,
	tx *sql.Tx,
	event domain.ExecutionEvent,
	stamp string,
) error {
	at := stamp
	if !event.At.IsZero() {
		at = formatTime(event.At.UTC())
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO execution_events (execution_id, state, checkpoint, detail, at)
		VALUES (?, ?, ?, ?, ?)`,
		event.ExecutionID, string(event.State), string(event.Checkpoint),
		domain.SanitiseDisplayText(event.Detail, domain.MaxExecutionMessageBytes), at)
	if err != nil {
		return fmt.Errorf("insert execution event: %w", AsError(err))
	}
	return nil
}

// --------------------------------------------------------------- reading --

// Get returns one execution by its immutable id.
func (r *ExecutionRepository) Get(ctx context.Context, executionID string) (domain.Execution, error) {
	rows, err := r.db.QueryContext(ctx,
		selectExecutionColumns+` WHERE e.execution_id = ?`, executionID)
	if err != nil {
		return domain.Execution{}, fmt.Errorf("query execution: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanExecutions(rows)
	if err != nil {
		return domain.Execution{}, err
	}
	if len(found) == 0 {
		return domain.Execution{}, ErrNotFound
	}
	return found[0], nil
}

// List returns a page of executions and the total matching the filter.
func (r *ExecutionRepository) List(
	ctx context.Context,
	filter ExecutionFilter,
) ([]domain.Execution, int, error) {
	where, args := executionWhere(filter)

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM executions e`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count executions: %w", AsError(err))
	}

	column, ok := executionSortFields[filter.Sort]
	if !ok {
		column = "e.requested_at"
	}
	direction := "DESC"
	if filter.Ascending {
		direction = "ASC"
	}

	page := filter.Page.normalise()
	// column and direction come from the allowlist above and a two-valued
	// switch; neither is caller text. Every VALUE is bound.
	query := selectExecutionColumns + where +
		` ORDER BY ` + column + ` ` + direction + `, e.id ` + direction +
		` LIMIT ? OFFSET ?`

	rows, err := r.db.QueryContext(ctx, query, append(args, page.Limit, page.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query executions: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanExecutions(rows)
	if err != nil {
		return nil, 0, err
	}
	return found, total, nil
}

// Events returns an execution's audit trail, oldest first.
//
// Oldest first: this history is read as the narrative of one operation rather
// than scanned for the most recent entry.
func (r *ExecutionRepository) Events(
	ctx context.Context,
	executionID string,
	limit int,
) ([]domain.ExecutionEvent, error) {
	if limit < 1 || limit > 500 {
		limit = 200
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, execution_id, state, checkpoint, detail, at
		FROM execution_events
		WHERE execution_id = ?
		ORDER BY id
		LIMIT ?`, executionID, limit)
	if err != nil {
		return nil, fmt.Errorf("query execution events: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	events := make([]domain.ExecutionEvent, 0, 16)
	for rows.Next() {
		var (
			event      domain.ExecutionEvent
			state      string
			checkpoint string
			at         string
		)
		if err := rows.Scan(&event.ID, &event.ExecutionID, &state, &checkpoint,
			&event.Detail, &at); err != nil {
			return nil, fmt.Errorf("scan execution event: %w", err)
		}
		event.State = domain.ExecutionState(state)
		event.Checkpoint = domain.ExecutionCheckpoint(checkpoint)

		parsed, err := parseTime(at)
		if err != nil {
			return nil, err
		}
		event.At = parsed
		events = append(events, event)
	}
	return events, rows.Err()
}

// ActiveCount reports how many recreations are in flight.
func (r *ExecutionRepository) ActiveCount(ctx context.Context) (int, error) {
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM executions WHERE state IN `+activeExecutionStatesSQL).
		Scan(&total); err != nil {
		return 0, fmt.Errorf("count active executions: %w", AsError(err))
	}
	return total, nil
}

// ActiveForContainer reports whether a recreation for this container is in
// flight.
func (r *ExecutionRepository) ActiveForContainer(ctx context.Context, containerID string) (bool, error) {
	var count int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM executions WHERE container_id = ? AND state IN `+
			activeExecutionStatesSQL, containerID).Scan(&count); err != nil {
		return false, fmt.Errorf("check active execution: %w", AsError(err))
	}
	return count > 0, nil
}

// ReplacementFor returns the container id that SUCCEEDED this one.
//
// # What this answers, and why it is a record rather than an inference
//
// When HarborMaster recreates a container it writes both ends of the swap: the
// original's id in `container_id` and the new one's in `replacement_id`. This
// returns the second given the first, for a recreation that reached verified
// success.
//
// Its caller is ResolveNamespaceProvider, which has to map the provider id
// frozen into a dependent's captured config onto whatever holds that namespace
// now. That question used to be answered by looking up the NAME the old id
// held in the inventory and finding the container present under it. Two things
// were wrong with that, both found in Stage 5a against a real daemon:
//
//  1. The read it used filtered `present = 1`, so it could never answer for a
//     container that had been replaced -- which is the only case it exists for.
//  2. Even reading absent rows would not have fixed it. A recreation renames the
//     original to its parked name BEFORE removing it, and an inventory refresh
//     that lands in that window records the PARKED name against the old id. The
//     retained row said `hm16-provider.hm-old-exec_4197648fe5c78423e38d`, so no
//     lookup by stable name could have matched.
//
// Recovering a stable name by stripping that suffix was the other option and is
// deliberately not taken: IsHarborMasterDerivedName documents that the shape is
// a display aid and never a security decision, because an operator can name a
// container that way themselves. This is the fact itself, written by
// HarborMaster about a mutation HarborMaster performed.
//
// Only `succeeded` counts. A failed recreation may have left a replacement
// behind in quarantine, and attaching a dependent's namespace to a quarantined
// container is precisely the wrong answer.
//
// Returns ErrNotFound when no such row exists, which every caller turns into a
// refusal. The newest is taken, so a container replaced more than once resolves
// one hop at a time.
func (r *ExecutionRepository) ReplacementFor(
	ctx context.Context,
	containerID string,
) (string, error) {
	if !domain.ValidFullContainerID(containerID) {
		return "", ErrNotFound
	}
	var replacement string
	err := r.db.QueryRowContext(ctx, `
		SELECT replacement_id
		  FROM executions
		 WHERE container_id = ?
		   AND state = 'succeeded'
		   AND replacement_id <> ''
		 ORDER BY id DESC
		 LIMIT 1`, containerID).Scan(&replacement)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", fmt.Errorf("query container replacement: %w", AsError(err))
	}
	if !domain.ValidFullContainerID(replacement) {
		// A stored id that is not a container id is a record this build cannot
		// act on. Refused rather than returned.
		return "", ErrNotFound
	}
	return replacement, nil
}

// ByAcquisition returns the execution that consumed an acquisition.
//
// The single-use check. Any row at all counts, whatever its outcome: see
// migration 0010 for why a cancelled execution still consumes its acquisition.
func (r *ExecutionRepository) ByAcquisition(
	ctx context.Context,
	acquisitionID string,
) (domain.Execution, bool, error) {
	if acquisitionID == "" {
		return domain.Execution{}, false, nil
	}

	rows, err := r.db.QueryContext(ctx,
		selectExecutionColumns+` WHERE e.acquisition_id = ?`, acquisitionID)
	if err != nil {
		return domain.Execution{}, false, fmt.Errorf("query execution by acquisition: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanExecutions(rows)
	if err != nil {
		return domain.Execution{}, false, err
	}
	if len(found) == 0 {
		return domain.Execution{}, false, nil
	}
	return found[0], true, nil
}

// ByRequestKey returns the execution a caller's idempotency key created.
func (r *ExecutionRepository) ByRequestKey(
	ctx context.Context,
	key string,
) (domain.Execution, bool, error) {
	if key == "" {
		return domain.Execution{}, false, nil
	}

	rows, err := r.db.QueryContext(ctx,
		selectExecutionColumns+` WHERE e.request_key = ?`, key)
	if err != nil {
		return domain.Execution{}, false, fmt.Errorf("query execution by key: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanExecutions(rows)
	if err != nil {
		return domain.Execution{}, false, err
	}
	if len(found) == 0 {
		return domain.Execution{}, false, nil
	}
	return found[0], true, nil
}

// Claimable returns queued executions, oldest first.
func (r *ExecutionRepository) Claimable(ctx context.Context, limit int) ([]domain.Execution, error) {
	if limit < 1 {
		limit = 8
	}
	rows, err := r.db.QueryContext(ctx,
		selectExecutionColumns+` WHERE e.state = 'queued' ORDER BY e.id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query claimable executions: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	return scanExecutions(rows)
}

// Summary computes the dashboard aggregate.
func (r *ExecutionRepository) Summary(ctx context.Context) (domain.ExecutionSummary, error) {
	summary := domain.ExecutionSummary{
		ByState:   make(map[domain.ExecutionState]int, len(domain.ExecutionStates)),
		ByFailure: make(map[domain.ExecutionFailure]int, len(domain.ExecutionFailures)),
	}

	rows, err := r.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM executions GROUP BY state`)
	if err != nil {
		return summary, fmt.Errorf("summarise executions: %w", AsError(err))
	}
	if err := scanCountsInto(rows, func(key string, count int) {
		state := domain.ExecutionState(key)
		summary.ByState[state] = count
		summary.Total += count
		if state.Active() {
			summary.Active += count
		}
		switch state {
		case domain.ExecutionSucceeded:
			summary.Succeeded += count
		case domain.ExecutionFailed:
			summary.Failed += count
		}
	}); err != nil {
		return summary, err
	}

	failureRows, err := r.db.QueryContext(ctx,
		`SELECT failure, COUNT(*) FROM executions WHERE failure <> '' GROUP BY failure`)
	if err != nil {
		return summary, fmt.Errorf("summarise execution failures: %w", AsError(err))
	}
	if err := scanCountsInto(failureRows, func(key string, count int) {
		summary.ByFailure[domain.ExecutionFailure(key)] = count
	}); err != nil {
		return summary, err
	}

	// Failures that left containers on the host. Counted from the CHECKPOINT
	// rather than from the failure classification, because the checkpoint is
	// what says whether anything was changed.
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM executions
		WHERE state = 'failed' AND checkpoint <> '' AND checkpoint <> 'originalRemoved'`).
		Scan(&summary.NeedsAttention); err != nil {
		return summary, fmt.Errorf("count executions needing attention: %w", AsError(err))
	}

	var completed sql.NullString
	if err := r.db.QueryRowContext(ctx,
		`SELECT MAX(completed_at) FROM executions`).Scan(&completed); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return summary, fmt.Errorf("read last execution: %w", AsError(err))
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

// ------------------------------------------------------------- recovery --

// Interrupted returns executions left mid-flight by a crash or restart.
//
// # Why this RETURNS rows rather than updating them
//
// The acquisition equivalent is a single blanket UPDATE, and that is correct
// there: an interrupted pull leaves an image in the store and nothing else, so
// every interrupted row gets the same treatment.
//
// A recreation is different. An interrupted one may have changed nothing, or
// may have stopped a container, parked it, created a replacement, and started
// it -- and the right thing to record, the right recovery plan to attach, and
// the right urgency to report all differ. So the rows come back and the service
// decides per row, from the CHECKPOINT.
//
// Bounded, because a database left by a pathological crash must not produce an
// unbounded recovery pass at startup.
func (r *ExecutionRepository) Interrupted(ctx context.Context, limit int) ([]domain.Execution, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx,
		selectExecutionColumns+` WHERE e.state IN ('validating', 'capturing', `+
			`'creating', 'starting', 'verifying') ORDER BY e.id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query interrupted executions: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	return scanExecutions(rows)
}

// ExpireStale abandons requests that sat queued past their deadline.
//
// Queued ONLY, and that restriction is load-bearing: a queued execution has
// changed nothing, so abandoning it is free. Anything past queued must be
// settled by the recovery pass, which reads its checkpoint.
func (r *ExecutionRepository) ExpireStale(ctx context.Context, now time.Time, batch int) (int64, error) {
	if batch < 1 {
		batch = 100
	}
	stamp := formatTime(now.UTC())

	result, err := r.db.ExecContext(ctx, `
		UPDATE executions SET
			state        = 'expired',
			message      = ?,
			completed_at = COALESCE(completed_at, ?),
			updated_at   = ?
		WHERE id IN (
			SELECT id FROM executions
			WHERE state = 'queued' AND expires_at < ?
			LIMIT ?
		)`,
		"this request waited longer than its deadline and was abandoned without being started; nothing on this host was changed",
		stamp, stamp, stamp, batch)
	if err != nil {
		return 0, fmt.Errorf("expire executions: %w", AsError(err))
	}

	expired, _ := result.RowsAffected()
	return expired, nil
}

// Prune removes completed executions older than the cutoff.
//
// # What is never pruned
//
// A failure that left containers on the host. Removing that record would leave
// an operator with two mystery containers and nothing explaining them, which is
// the exact situation the record exists to prevent -- so those rows are kept
// regardless of age, and only settle when the operator resolves them by hand.
//
// The most recent record per container is also kept, so a host always has an
// account of how its containers came to be running what they are running.
func (r *ExecutionRepository) Prune(ctx context.Context, cutoff time.Time, batch int) (int64, error) {
	if batch < 1 {
		batch = 200
	}

	result, err := r.db.ExecContext(ctx, `
		DELETE FROM executions
		WHERE id IN (
			SELECT id FROM executions
			WHERE completed_at IS NOT NULL
			  AND completed_at < ?
			  AND NOT (state = 'failed' AND checkpoint <> '' AND checkpoint <> 'originalRemoved')
			  AND id NOT IN (SELECT MAX(id) FROM executions GROUP BY container_id)
			LIMIT ?
		)`, formatTime(cutoff.UTC()), batch)
	if err != nil {
		return 0, fmt.Errorf("prune executions: %w", AsError(err))
	}

	pruned, _ := result.RowsAffected()
	return pruned, nil
}

// ---------------------------------------------------------------- helpers --

// activeExecutionStates returns the active set. One definition, shared.
func activeExecutionStates() []domain.ExecutionState {
	return []domain.ExecutionState{
		domain.ExecutionQueued, domain.ExecutionValidating, domain.ExecutionCapturing,
		domain.ExecutionCreating, domain.ExecutionStarting, domain.ExecutionVerifying,
	}
}

// executionWhere builds the filter clause. Every value is bound; only the
// placeholder RUN length varies with the input.
func executionWhere(filter ExecutionFilter) (string, []any) {
	clauses := make([]string, 0, 6)
	args := make([]any, 0, 8)

	if filter.ContainerID != "" {
		clauses = append(clauses, "e.container_id = ?")
		args = append(args, filter.ContainerID)
	}
	if filter.PlanID != "" {
		clauses = append(clauses, "e.plan_id = ?")
		args = append(args, filter.PlanID)
	}
	if filter.ActiveOnly {
		clauses = append(clauses, "e.state IN "+activeExecutionStatesSQL)
	}
	if filter.NeedsAttention {
		clauses = append(clauses,
			"e.state = 'failed' AND e.checkpoint <> '' AND e.checkpoint <> 'originalRemoved'")
	}
	if len(filter.States) > 0 {
		clauses = append(clauses, "e.state IN ("+placeholders(len(filter.States))+")")
		for _, state := range filter.States {
			args = append(args, string(state))
		}
	}
	if len(filter.Failures) > 0 {
		clauses = append(clauses, "e.failure IN ("+placeholders(len(filter.Failures))+")")
		for _, failure := range filter.Failures {
			args = append(args, string(failure))
		}
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// resultOrUnknown renders a verification result for storage.
//
// An empty value becomes "unknown" rather than being rejected by the CHECK
// constraint. Unknown is the fail-closed reading: a proof that was never
// recorded is a proof that was never performed.
func resultOrUnknown(result domain.VerificationResult) string {
	if result == "" {
		return string(domain.VerificationUnknown)
	}
	return string(result)
}

// boolToInt lives in image_intel_repository.go: the same "SQLite has no boolean"
// need, already solved once.

// encodeJSONColumn renders a value for a JSON-valued column.
//
// Fails SOFT to the empty string. These columns carry supporting detail -- a
// preservation report, a recovery plan -- and a marshalling failure must not
// prevent the terminal record from being written. Losing the detail of a
// failure is bad; losing the record that it failed is worse.
func encodeJSONColumn(value any) string {
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func scanExecutions(rows *sql.Rows) ([]domain.Execution, error) {
	out := make([]domain.Execution, 0, 16)

	for rows.Next() {
		var (
			execution  domain.Execution
			state      string
			checkpoint string
			failure    string
			refusal    string

			originalRemoved int
			healthResult    string
			imageResult     string
			preserveResult  string
			networkResult   string
			healthState     string
			healthChecked   int

			preservationReport string
			recoveryPlan       string

			requested string
			started   sql.NullString
			mutated   sql.NullString
			completed sql.NullString
			expires   string
		)

		if err := rows.Scan(
			&execution.ID, &execution.ExecutionID, &execution.AcquisitionID,
			&execution.PlanID, &execution.SnapshotID,
			&execution.ContainerID, &execution.ContainerName,
			&execution.OldImage, &execution.OldImageID, &execution.OldImageDigest,
			&execution.Target.Registry, &execution.Target.Repository,
			&execution.Target.Digest, &execution.Target.Reference,
			&execution.Target.ImageID,
			&execution.Target.Platform.OS, &execution.Target.Platform.Architecture,
			&execution.Target.Platform.Variant,
			&state, &checkpoint, &failure, &refusal, &execution.Message,
			&execution.ReplacementID, &execution.ParkedName,
			&execution.QuarantineName, &originalRemoved,
			&healthResult, &imageResult, &preserveResult, &networkResult,
			&healthState, &healthChecked, &execution.Verification.StabilitySeconds,
			&preservationReport, &recoveryPlan,
			&requested, &started, &mutated, &completed, &expires,
			&execution.RequestKey, &execution.PlanDigest,
			&execution.RequestedBy.UserID, &execution.RequestedBy.Username,
		); err != nil {
			return nil, fmt.Errorf("scan execution: %w", err)
		}

		execution.State = domain.ExecutionState(state)
		execution.Checkpoint = domain.ExecutionCheckpoint(checkpoint)
		execution.Failure = domain.ExecutionFailure(failure)
		execution.Refusal = domain.ExecutionRefusal(refusal)
		execution.OriginalRemoved = originalRemoved == 1

		execution.Verification.Health = domain.VerificationResult(healthResult)
		execution.Verification.Image = domain.VerificationResult(imageResult)
		execution.Verification.Preservation = domain.VerificationResult(preserveResult)
		execution.Verification.Network = domain.VerificationResult(networkResult)
		execution.Verification.HealthState = domain.HealthState(healthState)
		execution.Verification.HealthChecked = healthChecked == 1

		// Both JSON columns fail SOFT on read, matching the rest of the store: a
		// report that cannot be decoded should degrade one record's detail, not
		// make the endpoint fail.
		if preservationReport != "" {
			var report domain.PreservationReport
			if err := json.Unmarshal([]byte(preservationReport), &report); err == nil {
				execution.Verification.Report = &report
			}
		}
		if recoveryPlan != "" {
			var plan domain.RecoveryPlan
			if err := json.Unmarshal([]byte(recoveryPlan), &plan); err == nil {
				execution.Recovery = &plan
			}
		}

		var err error
		if execution.RequestedAt, err = parseTime(requested); err != nil {
			return nil, err
		}
		if execution.ExpiresAt, err = parseTime(expires); err != nil {
			return nil, err
		}
		execution.StartedAt = scanOptionalTime(started)
		execution.MutatedAt = scanOptionalTime(mutated)
		execution.CompletedAt = scanOptionalTime(completed)

		out = append(out, execution)
	}
	return out, rows.Err()
}
