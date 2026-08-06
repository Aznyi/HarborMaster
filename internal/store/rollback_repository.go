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

// Rollback persistence.
//
// # The same shape as the execution repository, for the same reasons
//
// Create refuses a second active rollback for one container. Advance is a
// CONDITIONAL update restricted to the states a transition may legally leave.
// Checkpoint writes what is true of the host in its own transaction, after the
// mutation and before the next. Every read is bound; every filter value is a
// closed vocabulary the API validated.
//
// The one structural difference is the single-use rule. An execution consumes
// its acquisition whatever happens, because acting on a stale approval is the
// risk that rule exists to prevent. A rollback consumes its chance only on
// SUCCESS -- a refused rollback taught the operator what to fix, and taking
// away the ability to try again would punish them for the safety check working.

// Rollback repository errors.
var (
	// ErrRollbackActive reports that a rollback for the same container is
	// already in flight. Raised by the partial unique index rather than by a
	// pre-check, so it holds across processes and across a restart.
	ErrRollbackActive = errors.New("a rollback for this container is already in progress")
	// ErrRollbackAlreadySucceeded reports that this execution has already been
	// rolled back. Single use, with no override.
	ErrRollbackAlreadySucceeded = errors.New("this recreation has already been rolled back")
	// ErrRollbackIdentity reports that a record does not name both containers.
	ErrRollbackIdentity = errors.New("rollback record does not name both containers")
)

// RollbackRepository stores rollbacks and their bounded event trails.
type RollbackRepository struct {
	db *sql.DB
}

// RollbackFilter narrows a rollback listing.
//
// Every field is a closed vocabulary or an identifier the API validated by
// shape. None of them becomes SQL text.
type RollbackFilter struct {
	ExecutionID   string
	ContainerName string
	States        []domain.RollbackState
	Failures      []domain.RollbackFailure
	// ActiveOnly restricts to work still in progress.
	ActiveOnly bool
	// NeedsAttention restricts to failures that left containers on the host.
	NeedsAttention bool

	Page Page
}

const selectRollbackColumns = `
	SELECT r.id, r.rollback_id, r.execution_id, r.container_name,
	       r.original_id, r.parked_name, r.replacement_id, r.replacement_parked_name,
	       r.original_image, r.original_image_id, r.replacement_image,
	       r.state, r.checkpoint, r.failure, r.refusal, r.message,
	       r.verify_health, r.verify_image, r.verify_preservation, r.verify_network,
	       r.health_state, r.health_checked, r.stability_seconds,
	       r.preservation_report, r.recovery_plan,
	       r.requested_at, r.started_at, r.mutated_at, r.completed_at, r.expires_at,
	       r.request_key, r.requested_by_user_id, r.requested_by_username
	FROM rollbacks r`

// -------------------------------------------------------------- creating --

// Create records a new rollback request.
//
// Returns ErrRollbackActive when a rollback for the container is already in
// flight, ErrRollbackAlreadySucceeded when this execution has been rolled back
// before, and the EXISTING record when the idempotency key matches one already
// stored.
func (r *RollbackRepository) Create(
	ctx context.Context,
	rollback domain.Rollback,
	now time.Time,
) (domain.Rollback, error) {
	if !domain.ValidRollbackID(rollback.RollbackID) ||
		!domain.ValidExecutionID(rollback.ExecutionID) {
		return domain.Rollback{}, ErrRollbackIdentity
	}
	// Both container identities are required. A record that named only one
	// could not be recovered after a restart: the pass would know something was
	// moved and not which container.
	if rollback.OriginalID == "" || rollback.ReplacementID == "" ||
		rollback.ContainerName == "" || rollback.ParkedName == "" {
		return domain.Rollback{}, ErrRollbackIdentity
	}

	stamp := formatTime(now.UTC())
	requested := formatTime(rollback.RequestedAt.UTC())
	state := rollback.State
	if state == "" {
		state = domain.RollbackQueued
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Rollback{}, fmt.Errorf("begin rollback insert: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	// The idempotency key is checked INSIDE the transaction, so a retried
	// request cannot race its own first attempt and produce two rows.
	if rollback.RequestKey != "" {
		existing, found, lookupErr := rollbackByRequestKeyTx(ctx, tx, rollback.RequestKey)
		if lookupErr != nil {
			return domain.Rollback{}, lookupErr
		}
		if found {
			return existing, nil
		}
	}

	// So is "has this execution already been rolled back".
	//
	// The unique index cannot answer it: it is PARTIAL on the succeeded state,
	// so it stops two successes but not a fresh request for an execution that
	// already has one. Asking here, in the same transaction as the insert,
	// makes the refusal consistent with the row it is refusing against.
	var rolledBack int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rollbacks WHERE execution_id = ? AND state = 'succeeded'`,
		rollback.ExecutionID).Scan(&rolledBack); err != nil {
		return domain.Rollback{}, fmt.Errorf("check rolled back: %w", AsError(err))
	}
	if rolledBack > 0 {
		return domain.Rollback{}, ErrRollbackAlreadySucceeded
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO rollbacks
			(rollback_id, execution_id, container_name,
			 original_id, parked_name, replacement_id,
			 original_image, original_image_id, replacement_image,
			 state, requested_at, expires_at, request_key,
			 requested_by_user_id, requested_by_username,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rollback.RollbackID, rollback.ExecutionID, rollback.ContainerName,
		rollback.OriginalID, rollback.ParkedName, rollback.ReplacementID,
		rollback.OriginalImage, rollback.OriginalImageID, rollback.ReplacementImage,
		string(state), requested, formatTime(rollback.ExpiresAt.UTC()),
		rollback.RequestKey,
		rollback.RequestedBy.UserID, rollback.RequestedBy.Username,
		stamp, stamp)
	if err != nil {
		if isUniqueViolation(err) {
			// Three indexes can refuse this insert and they mean different
			// things, so the tables are consulted to say which. Doing it inside
			// the transaction means the answer is consistent with the refusal.
			return domain.Rollback{}, classifyRollbackConflict(ctx, tx, rollback)
		}
		return domain.Rollback{}, fmt.Errorf("insert rollback: %w", AsError(err))
	}

	if err := insertRollbackEvent(ctx, tx, domain.RollbackEvent{
		RollbackID: rollback.RollbackID,
		State:      state,
		Detail:     "rollback requested",
	}, stamp); err != nil {
		return domain.Rollback{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Rollback{}, fmt.Errorf("commit rollback insert: %w", AsError(err))
	}
	return r.Get(ctx, rollback.RollbackID)
}

// classifyRollbackConflict says which unique index refused an insert.
func classifyRollbackConflict(
	ctx context.Context,
	tx *sql.Tx,
	rollback domain.Rollback,
) error {
	var succeeded int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rollbacks
		 WHERE execution_id = ? AND state = 'succeeded'`,
		rollback.ExecutionID).Scan(&succeeded); err == nil && succeeded > 0 {
		return ErrRollbackAlreadySucceeded
	}
	return ErrRollbackActive
}

// -------------------------------------------------------------- advancing --

// RollbackChange is one lifecycle transition.
type RollbackChange struct {
	RollbackID string
	// From restricts the update to rollbacks currently in these states. An
	// empty slice means any ACTIVE state: nothing may move a finished rollback.
	From []domain.RollbackState
	To   domain.RollbackState

	// Checkpoint, when set, is recorded alongside the transition. Most
	// checkpoints are written by Checkpoint instead; this exists for the
	// transitions that carry one atomically, such as the final success.
	Checkpoint domain.RollbackCheckpoint

	Failure domain.RollbackFailure
	Refusal domain.RollbackRefusal
	Message string
	Detail  string

	// ReplacementParkedName is written the moment it becomes true rather than
	// at the end, because a crash between "true on the host" and "recorded" is
	// exactly what the checkpoint design exists to survive.
	ReplacementParkedName string

	// Verification, when the transition carries a verdict.
	Verification *domain.RollbackVerification
	// Recovery is the manual recovery plan for a failure that left containers.
	Recovery *domain.RecoveryPlan

	// MarkStarted stamps started_at, and MarkMutated stamps mutated_at.
	MarkStarted bool
	MarkMutated bool
}

// Advance moves a rollback forward, conditionally.
//
// Returns false when no row matched, which means another path already moved it
// -- a cancellation, an expiry, or a restart recovery pass. The caller treats
// that as "somebody else owns this now" rather than as an error.
func (r *RollbackRepository) Advance(
	ctx context.Context,
	change RollbackChange,
	now time.Time,
) (bool, error) {
	stamp := formatTime(now.UTC())

	from := change.From
	if len(from) == 0 {
		from = activeRollbackStates()
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin rollback update: %w", AsError(err))
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
	// supplied it, so a transition that says nothing about the parked name
	// cannot blank one that was already recorded.
	if change.Checkpoint != domain.RollbackCheckpointNone {
		assignments = append(assignments, "checkpoint = ?")
		args = append(args, string(change.Checkpoint))
	}
	if change.ReplacementParkedName != "" {
		assignments = append(assignments, "replacement_parked_name = ?")
		args = append(args, change.ReplacementParkedName)
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
	args = append(args, stamp, change.RollbackID)
	for _, state := range from {
		args = append(args, string(state))
	}

	// Every element of `assignments` is a compile-time literal from this
	// function. Only the placeholder RUN length varies with the input, and
	// every VALUE is bound.
	result, err := tx.ExecContext(ctx, `
		UPDATE rollbacks SET `+strings.Join(assignments, ", ")+`
		WHERE rollback_id = ?
		  AND state IN (`+placeholders(len(from))+`)`, args...)
	if err != nil {
		return false, fmt.Errorf("update rollback: %w", AsError(err))
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update rollback: %w", AsError(err))
	}
	if affected == 0 {
		return false, nil
	}

	if err := insertRollbackEvent(ctx, tx, domain.RollbackEvent{
		RollbackID: change.RollbackID,
		State:      change.To,
		Checkpoint: change.Checkpoint,
		Detail:     change.Detail,
	}, stamp); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit rollback update: %w", AsError(err))
	}
	return true, nil
}

// RollbackCheckpointWrite records that one mutation completed.
type RollbackCheckpointWrite struct {
	RollbackID string
	Checkpoint domain.RollbackCheckpoint
	// Detail is the audit-trail note. HarborMaster's own words.
	Detail string

	// ReplacementParkedName is the host fact this checkpoint establishes,
	// written in the SAME transaction as the checkpoint itself. Recording "the
	// replacement was parked" without recording WHERE would leave a recovery
	// pass unable to describe the host.
	ReplacementParkedName string

	// MarkMutated stamps mutated_at, set on the first checkpoint.
	MarkMutated bool
}

// Checkpoint records that one Docker mutation completed.
//
// # Why this is not just another Advance
//
// A checkpoint is written AFTER a mutation succeeds and BEFORE the next is
// attempted, and it does not change the state. Folding it into Advance would
// mean every checkpoint also moved the lifecycle, and the two answer different
// questions: the state is what HarborMaster is doing, the checkpoint is what is
// true of the host.
//
// Returns false when no row matched, which means the rollback is no longer
// active -- so the caller stops rather than continuing to mutate a host on
// behalf of a record somebody else has settled.
func (r *RollbackRepository) Checkpoint(
	ctx context.Context,
	write RollbackCheckpointWrite,
	now time.Time,
) (bool, error) {
	if !domain.ValidRollbackCheckpoint(string(write.Checkpoint)) {
		return false, fmt.Errorf("checkpoint rollback: %w", ErrInvalidInput)
	}

	stamp := formatTime(now.UTC())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin rollback checkpoint: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	assignments := []string{"checkpoint = ?"}
	args := []any{string(write.Checkpoint)}

	if write.ReplacementParkedName != "" {
		assignments = append(assignments, "replacement_parked_name = ?")
		args = append(args, write.ReplacementParkedName)
	}
	if write.MarkMutated {
		assignments = append(assignments, "mutated_at = COALESCE(mutated_at, ?)")
		args = append(args, stamp)
	}

	assignments = append(assignments, "updated_at = ?")
	args = append(args, stamp, write.RollbackID)

	active := activeRollbackStates()
	for _, state := range active {
		args = append(args, string(state))
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE rollbacks SET `+strings.Join(assignments, ", ")+`
		WHERE rollback_id = ?
		  AND state IN (`+placeholders(len(active))+`)`, args...)
	if err != nil {
		return false, fmt.Errorf("checkpoint rollback: %w", AsError(err))
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checkpoint rollback: %w", AsError(err))
	}
	if affected == 0 {
		return false, nil
	}

	// The event carries the CURRENT state, read back inside the transaction, so
	// the trail says which stage the checkpoint belongs to without the caller
	// having to tell it.
	var state string
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM rollbacks WHERE rollback_id = ?`,
		write.RollbackID).Scan(&state); err != nil {
		return false, fmt.Errorf("read rollback state: %w", AsError(err))
	}

	if err := insertRollbackEvent(ctx, tx, domain.RollbackEvent{
		RollbackID: write.RollbackID,
		State:      domain.RollbackState(state),
		Checkpoint: write.Checkpoint,
		Detail:     write.Detail,
	}, stamp); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit rollback checkpoint: %w", AsError(err))
	}
	return true, nil
}

// ---------------------------------------------------------------- reading --

// Get returns one rollback.
func (r *RollbackRepository) Get(ctx context.Context, rollbackID string) (domain.Rollback, error) {
	rows, err := r.db.QueryContext(ctx,
		selectRollbackColumns+` WHERE r.rollback_id = ?`, rollbackID)
	if err != nil {
		return domain.Rollback{}, fmt.Errorf("query rollback: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanRollbacks(rows)
	if err != nil {
		return domain.Rollback{}, err
	}
	if len(found) == 0 {
		return domain.Rollback{}, ErrNotFound
	}
	return found[0], nil
}

// List returns a page of rollbacks, newest first.
func (r *RollbackRepository) List(
	ctx context.Context,
	filter RollbackFilter,
) ([]domain.Rollback, int, error) {
	where, args := rollbackWhere(filter)

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rollbacks r`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count rollbacks: %w", AsError(err))
	}

	page := filter.Page.normalise()
	rows, err := r.db.QueryContext(ctx,
		selectRollbackColumns+where+` ORDER BY r.id DESC LIMIT ? OFFSET ?`,
		append(args, page.Limit, page.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query rollbacks: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanRollbacks(rows)
	if err != nil {
		return nil, 0, err
	}
	return found, total, nil
}

// Events returns one rollback's bounded audit trail, oldest first.
func (r *RollbackRepository) Events(
	ctx context.Context,
	rollbackID string,
	limit int,
) ([]domain.RollbackEvent, error) {
	if limit < 1 || limit > maxRollbackEvents {
		limit = maxRollbackEvents
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, rollback_id, state, checkpoint, detail, at
		FROM rollback_events
		WHERE rollback_id = ?
		ORDER BY id
		LIMIT ?`, rollbackID, limit)
	if err != nil {
		return nil, fmt.Errorf("query rollback events: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	events := make([]domain.RollbackEvent, 0, 16)
	for rows.Next() {
		var (
			event      domain.RollbackEvent
			state      string
			checkpoint string
			at         string
		)
		if err := rows.Scan(&event.ID, &event.RollbackID, &state, &checkpoint,
			&event.Detail, &at); err != nil {
			return nil, fmt.Errorf("scan rollback event: %w", err)
		}
		event.State = domain.RollbackState(state)
		event.Checkpoint = domain.RollbackCheckpoint(checkpoint)

		parsed, parseErr := parseTime(at)
		if parseErr != nil {
			return nil, parseErr
		}
		event.At = parsed
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rollback events: %w", AsError(err))
	}
	return events, nil
}

// maxRollbackEvents bounds one rollback's trail.
//
// Every entry is written by HarborMaster about its own action, so the real
// number is small and fixed. The bound exists so a future edit cannot produce
// an unbounded document in a response.
const maxRollbackEvents = 200

// ActiveCount reports how many rollbacks are in flight.
//
// # Why there is an exclusion
//
// The preflight runs TWICE: once when the request arrives, and again from
// inside the pipeline immediately before the first mutation. On the second run
// the rollback being assessed is itself active, so a count that included it
// would refuse every rollback for conflicting with itself.
//
// The exclusion is an id HarborMaster generated, never caller input, and it is
// parameterised rather than interpolated.
func (r *RollbackRepository) ActiveCount(ctx context.Context, excluding string) (int, error) {
	active := activeRollbackStates()
	args := make([]any, 0, len(active)+1)
	for _, state := range active {
		args = append(args, string(state))
	}

	query := `SELECT COUNT(*) FROM rollbacks WHERE state IN (` + placeholders(len(active)) + `)`
	if excluding != "" {
		query += ` AND rollback_id <> ?`
		args = append(args, excluding)
	}

	var count int
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count active rollbacks: %w", AsError(err))
	}
	return count, nil
}

// ActiveForContainer reports whether a rollback is in flight for a container.
//
// excluding names a rollback to ignore. See ActiveCount for why.
func (r *RollbackRepository) ActiveForContainer(
	ctx context.Context,
	containerName string,
	excluding string,
) (bool, error) {
	active := activeRollbackStates()
	args := make([]any, 0, len(active)+2)
	args = append(args, containerName)
	for _, state := range active {
		args = append(args, string(state))
	}

	query := `SELECT COUNT(*) FROM rollbacks
		 WHERE container_name = ? AND state IN (` + placeholders(len(active)) + `)`
	if excluding != "" {
		query += ` AND rollback_id <> ?`
		args = append(args, excluding)
	}

	var count int
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return false, fmt.Errorf("check active rollback: %w", AsError(err))
	}
	return count > 0, nil
}

// SucceededForExecution reports whether an execution has already been rolled
// back.
func (r *RollbackRepository) SucceededForExecution(
	ctx context.Context,
	executionID string,
) (bool, error) {
	var count int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rollbacks WHERE execution_id = ? AND state = 'succeeded'`,
		executionID).Scan(&count); err != nil {
		return false, fmt.Errorf("check rolled back: %w", AsError(err))
	}
	return count > 0, nil
}

// ByRequestKey returns the rollback an idempotency key names.
func (r *RollbackRepository) ByRequestKey(
	ctx context.Context,
	key string,
) (domain.Rollback, bool, error) {
	if key == "" {
		return domain.Rollback{}, false, nil
	}

	rows, err := r.db.QueryContext(ctx,
		selectRollbackColumns+` WHERE r.request_key = ?`, key)
	if err != nil {
		return domain.Rollback{}, false, fmt.Errorf("query rollback by key: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanRollbacks(rows)
	if err != nil {
		return domain.Rollback{}, false, err
	}
	if len(found) == 0 {
		return domain.Rollback{}, false, nil
	}
	return found[0], true, nil
}

// rollbackByRequestKeyTx is ByRequestKey inside a caller's transaction.
func rollbackByRequestKeyTx(
	ctx context.Context,
	tx *sql.Tx,
	key string,
) (domain.Rollback, bool, error) {
	rows, err := tx.QueryContext(ctx, selectRollbackColumns+` WHERE r.request_key = ?`, key)
	if err != nil {
		return domain.Rollback{}, false, fmt.Errorf("query rollback by key: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanRollbacks(rows)
	if err != nil {
		return domain.Rollback{}, false, err
	}
	if len(found) == 0 {
		return domain.Rollback{}, false, nil
	}
	return found[0], true, nil
}

// Claimable returns queued rollbacks a worker may start, oldest first.
func (r *RollbackRepository) Claimable(ctx context.Context, limit int) ([]domain.Rollback, error) {
	if limit < 1 {
		limit = 1
	}

	rows, err := r.db.QueryContext(ctx,
		selectRollbackColumns+` WHERE r.state = 'queued' ORDER BY r.id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query claimable rollbacks: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	return scanRollbacks(rows)
}

// Interrupted returns rollbacks left mid-flight by a restart.
//
// A row in a STARTED state at startup cannot be running: this process has just
// begun, and nothing else writes these rows. So each one was interrupted, and
// the recovery pass settles it from its checkpoint without issuing a single
// Docker call.
//
// QUEUED rows are excluded. One of those was never started, so there is nothing
// to settle: it is claimable, and settling it as a failure would turn a request
// that survived a restart into one an operator has to make again.
func (r *RollbackRepository) Interrupted(ctx context.Context, limit int) ([]domain.Rollback, error) {
	if limit < 1 {
		limit = 1
	}

	started := startedRollbackStates()
	args := make([]any, 0, len(started)+1)
	for _, state := range started {
		args = append(args, string(state))
	}
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx,
		selectRollbackColumns+` WHERE r.state IN (`+placeholders(len(started))+`)
		 ORDER BY r.id LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("query interrupted rollbacks: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	return scanRollbacks(rows)
}

// Summary returns the dashboard aggregate.
func (r *RollbackRepository) Summary(ctx context.Context) (domain.RollbackSummary, error) {
	var summary domain.RollbackSummary

	rows, err := r.db.QueryContext(ctx, `
		SELECT state, failure, COUNT(*) FROM rollbacks GROUP BY state, failure`)
	if err != nil {
		return summary, fmt.Errorf("summarise rollbacks: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			state   string
			failure string
			count   int
		)
		if err := rows.Scan(&state, &failure, &count); err != nil {
			return summary, fmt.Errorf("scan rollback summary: %w", err)
		}

		summary.Total += count
		switch domain.RollbackState(state) {
		case domain.RollbackSucceeded:
			summary.Succeeded += count
		case domain.RollbackFailed:
			summary.Failed += count
			if domain.RollbackFailure(failure).NeedsOperator() {
				summary.NeedsAttention += count
			}
		default:
			if domain.RollbackState(state).Active() {
				summary.Active += count
			}
		}
	}
	if err := rows.Err(); err != nil {
		return summary, fmt.Errorf("iterate rollback summary: %w", AsError(err))
	}
	return summary, nil
}

// ---------------------------------------------------------- housekeeping --

// ExpireStale abandons queued requests that waited past their deadline.
//
// QUEUED only. An expired request never started, so abandoning it changes
// nothing on the host -- and restricting the statement to that one state is
// what makes that true rather than merely intended.
func (r *RollbackRepository) ExpireStale(
	ctx context.Context,
	now time.Time,
	batch int,
) (int64, error) {
	if batch < 1 {
		batch = 1
	}
	stamp := formatTime(now.UTC())

	result, err := r.db.ExecContext(ctx, `
		UPDATE rollbacks SET
			state        = 'expired',
			message      = 'the request waited past its deadline and was abandoned before it started',
			completed_at = COALESCE(completed_at, ?),
			updated_at   = ?
		WHERE rollback_id IN (
			SELECT rollback_id FROM rollbacks
			WHERE state = 'queued' AND expires_at < ?
			ORDER BY id
			LIMIT ?
		)`, stamp, stamp, stamp, batch)
	if err != nil {
		return 0, fmt.Errorf("expire rollbacks: %w", AsError(err))
	}

	expired, _ := result.RowsAffected()
	return expired, nil
}

// Prune removes finished rollbacks older than a cutoff.
//
// Terminal rows only, and bounded per pass so a large backlog cannot hold the
// single SQLite writer. Events cascade.
//
// # What is never pruned
//
// A failure that left containers on the host. The same rule the recreation
// records follow, and for the same reason: removing that row would leave an
// operator with two containers and nothing explaining them, which is the exact
// situation the record exists to prevent. Those rows are kept regardless of
// age and settle only when an operator resolves them by hand.
func (r *RollbackRepository) Prune(
	ctx context.Context,
	cutoff time.Time,
	batch int,
) (int64, error) {
	if batch < 1 {
		batch = 1
	}

	result, err := r.db.ExecContext(ctx, `
		DELETE FROM rollbacks
		WHERE rollback_id IN (
			SELECT rollback_id FROM rollbacks
			WHERE state IN ('succeeded', 'failed', 'cancelled', 'expired')
			  AND completed_at IS NOT NULL
			  AND completed_at < ?
			  AND NOT (state = 'failed' AND checkpoint <> '')
			ORDER BY id
			LIMIT ?
		)`, formatTime(cutoff.UTC()), batch)
	if err != nil {
		return 0, fmt.Errorf("prune rollbacks: %w", AsError(err))
	}

	pruned, _ := result.RowsAffected()
	return pruned, nil
}

// ---------------------------------------------------------------- helpers --

// activeRollbackStates lists the states a rollback may be moved from.
func activeRollbackStates() []domain.RollbackState {
	return []domain.RollbackState{
		domain.RollbackQueued, domain.RollbackValidating,
		domain.RollbackStoppingReplacement, domain.RollbackRestoringName,
		domain.RollbackStartingOriginal, domain.RollbackVerifyingOriginal,
	}
}

// startedRollbackStates lists the active states a worker has begun.
//
// Everything in activeRollbackStates except queued. The distinction is what
// separates "was interrupted and needs settling" from "is still waiting to be
// picked up".
func startedRollbackStates() []domain.RollbackState {
	return []domain.RollbackState{
		domain.RollbackValidating,
		domain.RollbackStoppingReplacement, domain.RollbackRestoringName,
		domain.RollbackStartingOriginal, domain.RollbackVerifyingOriginal,
	}
}

// rollbackWhere builds the listing predicate.
//
// Every clause is a compile-time literal and every value is bound. Nothing a
// caller supplies becomes SQL text.
func rollbackWhere(filter RollbackFilter) (string, []any) {
	clauses := make([]string, 0, 6)
	args := make([]any, 0, 8)

	if filter.ExecutionID != "" {
		clauses = append(clauses, "r.execution_id = ?")
		args = append(args, filter.ExecutionID)
	}
	if filter.ContainerName != "" {
		clauses = append(clauses, "r.container_name = ?")
		args = append(args, filter.ContainerName)
	}
	if len(filter.States) > 0 {
		clauses = append(clauses, "r.state IN ("+placeholders(len(filter.States))+")")
		for _, state := range filter.States {
			args = append(args, string(state))
		}
	}
	if len(filter.Failures) > 0 {
		clauses = append(clauses, "r.failure IN ("+placeholders(len(filter.Failures))+")")
		for _, failure := range filter.Failures {
			args = append(args, string(failure))
		}
	}
	if filter.ActiveOnly {
		active := activeRollbackStates()
		clauses = append(clauses, "r.state IN ("+placeholders(len(active))+")")
		for _, state := range active {
			args = append(args, string(state))
		}
	}
	if filter.NeedsAttention {
		// The failures that left containers behind. A literal list rather than
		// a computed one, so the SQL cannot drift from NeedsOperator without
		// somebody editing both.
		clauses = append(clauses,
			"(r.state = 'failed' AND r.failure NOT IN ('', 'preflight'))")
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// insertRollbackEvent appends one bounded trail entry.
func insertRollbackEvent(
	ctx context.Context,
	tx *sql.Tx,
	event domain.RollbackEvent,
	stamp string,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO rollback_events (rollback_id, state, checkpoint, detail, at)
		VALUES (?, ?, ?, ?, ?)`,
		event.RollbackID, string(event.State), string(event.Checkpoint),
		domain.SanitiseDisplayText(event.Detail, domain.MaxExecutionMessageBytes),
		stamp)
	if err != nil {
		return fmt.Errorf("insert rollback event: %w", AsError(err))
	}
	return nil
}

// scanRollbacks reads rollback rows.
func scanRollbacks(rows *sql.Rows) ([]domain.Rollback, error) {
	out := make([]domain.Rollback, 0, 16)

	for rows.Next() {
		var (
			rollback domain.Rollback

			state      string
			checkpoint string
			failure    string
			refusal    string

			healthResult   string
			imageResult    string
			preserveResult string
			networkResult  string
			healthState    string
			healthChecked  int

			preservationReport string
			recoveryPlan       string

			requested string
			started   sql.NullString
			mutated   sql.NullString
			completed sql.NullString
			expires   string
		)

		if err := rows.Scan(
			&rollback.ID, &rollback.RollbackID, &rollback.ExecutionID,
			&rollback.ContainerName,
			&rollback.OriginalID, &rollback.ParkedName, &rollback.ReplacementID,
			&rollback.ReplacementParkedName,
			&rollback.OriginalImage, &rollback.OriginalImageID, &rollback.ReplacementImage,
			&state, &checkpoint, &failure, &refusal, &rollback.Message,
			&healthResult, &imageResult, &preserveResult, &networkResult,
			&healthState, &healthChecked, &rollback.Verification.StabilitySeconds,
			&preservationReport, &recoveryPlan,
			&requested, &started, &mutated, &completed, &expires,
			&rollback.RequestKey,
			&rollback.RequestedBy.UserID, &rollback.RequestedBy.Username,
		); err != nil {
			return nil, fmt.Errorf("scan rollback: %w", err)
		}

		rollback.State = domain.RollbackState(state)
		rollback.Checkpoint = domain.RollbackCheckpoint(checkpoint)
		rollback.Failure = domain.RollbackFailure(failure)
		rollback.Refusal = domain.RollbackRefusal(refusal)

		rollback.Verification.Health = domain.VerificationResult(healthResult)
		rollback.Verification.Image = domain.VerificationResult(imageResult)
		rollback.Verification.Preservation = domain.VerificationResult(preserveResult)
		rollback.Verification.Network = domain.VerificationResult(networkResult)
		rollback.Verification.HealthState = domain.HealthState(healthState)
		rollback.Verification.HealthChecked = healthChecked == 1

		// A column that will not decode is dropped rather than failing the
		// read. It was written by this process from its own types, so a bad
		// value means the row was edited outside HarborMaster -- and refusing
		// to render the whole rollback because one JSON blob is malformed
		// would hide the identities an operator needs most.
		if preservationReport != "" {
			var report domain.PreservationReport
			if err := json.Unmarshal([]byte(preservationReport), &report); err == nil {
				rollback.Verification.Report = &report
			}
		}
		if recoveryPlan != "" {
			var plan domain.RecoveryPlan
			if err := json.Unmarshal([]byte(recoveryPlan), &plan); err == nil {
				rollback.Recovery = &plan
			}
		}

		var err error
		if rollback.RequestedAt, err = parseTime(requested); err != nil {
			return nil, err
		}
		if rollback.ExpiresAt, err = parseTime(expires); err != nil {
			return nil, err
		}
		rollback.StartedAt = scanOptionalTime(started)
		rollback.MutatedAt = scanOptionalTime(mutated)
		rollback.CompletedAt = scanOptionalTime(completed)

		out = append(out, rollback)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rollbacks: %w", AsError(err))
	}
	return out, nil
}
