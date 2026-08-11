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

// AutomationRepository stores what the scheduler decided and why.
//
// # Every write here is bookkeeping
//
// Nothing in this file can change a container. The engine records its reasoning
// through these methods and submits its actual work to the acquisition,
// execution, and rollback services, which own the only capability interfaces
// that reach Docker. A bug in this file can lose an explanation; it cannot move
// a host.
//
// # Bounded growth
//
// A pass writes one run row and one decision row per container considered. On
// an estate of 200 containers polled every 15 minutes that is 19,200 rows a
// day, so retention is not optional: PruneRuns deletes runs older than a
// configured horizon and the decisions cascade with them.
type AutomationRepository struct {
	db *sql.DB
}

// Automation repository errors.
var (
	// ErrAutomationRunActive reports that a pass is already in flight. Raised
	// by the partial unique index rather than by a pre-check, so it holds
	// across processes and across a restart.
	ErrAutomationRunActive = errors.New("an automation pass is already running")
	// ErrPauseActive reports that the container is already paused. Raised by
	// the partial unique index.
	ErrPauseActive = errors.New("automation is already paused for this container")
)

// maxAutomationDecisionsPerRun bounds one pass's decision writes.
//
// A pass considers every container on the host. This is a backstop against a
// pathological inventory, not an expected limit: reaching it means the pass
// stopped recording, which is logged and reported on the run rather than
// silently truncated.
const maxAutomationDecisionsPerRun = 5000

// ------------------------------------------------------------------- runs --

// StartRun records the beginning of a pass.
//
// Written BEFORE anything is examined. A pass that crashed halfway must leave
// evidence that it ran, and a row written at the end would leave none.
//
// Only one pass may be running at a time. The check is a conditional insert
// rather than a read followed by a write, so two schedulers -- or a scheduled
// pass and a manual one racing -- cannot both believe they are the only one.
func (r *AutomationRepository) StartRun(
	ctx context.Context,
	run domain.AutomationRun,
) (domain.AutomationRun, error) {
	stamp := formatTime(run.StartedAt.UTC())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.AutomationRun{}, fmt.Errorf("begin automation run: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	var running int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM automation_runs WHERE state = 'running'`).Scan(&running); err != nil {
		return domain.AutomationRun{}, fmt.Errorf("check running automation pass: %w", AsError(err))
	}
	if running > 0 {
		return domain.AutomationRun{}, ErrAutomationRunActive
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO automation_runs
			(run_id, trigger_source, state, dry_run,
			 requested_by_user_id, requested_by_username,
			 started_at)
		VALUES (?, ?, 'running', ?, ?, ?, ?)`,
		run.RunID, string(run.Trigger), boolToInt(run.DryRun),
		run.RequestedBy.UserID, run.RequestedBy.Username,
		stamp)
	if err != nil {
		return domain.AutomationRun{}, fmt.Errorf("insert automation run: %w", AsError(err))
	}
	if err := tx.Commit(); err != nil {
		return domain.AutomationRun{}, fmt.Errorf("commit automation run: %w", AsError(err))
	}

	id, _ := result.LastInsertId()
	run.ID = id
	run.State = domain.RunRunning
	return run, nil
}

// FinishRun records a pass's outcome and counters.
//
// A CONDITIONAL update restricted to the running state, so a recovery sweep
// that already marked the run interrupted cannot be overwritten by a goroutine
// that survived long enough to finish.
func (r *AutomationRepository) FinishRun(
	ctx context.Context,
	runID string,
	state domain.AutomationRunState,
	counts domain.AutomationRun,
	completedAt time.Time,
) error {
	duration := counts.DurationMs
	if duration < 0 {
		duration = 0
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE automation_runs
		   SET state = ?, considered = ?, eligible = ?, submitted = ?,
		       skipped = ?, failed = ?, message = ?,
		       completed_at = ?, duration_ms = ?
		 WHERE run_id = ? AND state = 'running'`,
		string(state), counts.Considered, counts.Eligible, counts.Submitted,
		counts.Skipped, counts.Failed, counts.Message,
		formatTime(completedAt.UTC()), duration, runID)
	if err != nil {
		return fmt.Errorf("finish automation run: %w", AsError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish automation run: %w", AsError(err))
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// InterruptRuns marks every still-running pass as interrupted.
//
// Called once at startup. A pass changes nothing itself -- it submits work to
// services that checkpoint their own state -- so an interrupted run is a
// bookkeeping gap and never a host in an unknown condition. Recording it is
// still necessary: a run stuck at 'running' forever would block every future
// pass through the single-run rule above.
func (r *AutomationRepository) InterruptRuns(ctx context.Context, at time.Time) (int, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE automation_runs
		   SET state = 'interrupted', completed_at = ?,
		       message = 'HarborMaster restarted while this pass was running'
		 WHERE state = 'running'`,
		formatTime(at.UTC()))
	if err != nil {
		return 0, fmt.Errorf("interrupt automation runs: %w", AsError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("interrupt automation runs: %w", AsError(err))
	}
	return int(affected), nil
}

const selectAutomationRunColumns = `
	SELECT id, run_id, trigger_source, state, dry_run,
	       considered, eligible, submitted, skipped, failed,
	       requested_by_user_id, requested_by_username,
	       message, started_at, completed_at, duration_ms
	FROM automation_runs`

// RunByID reads one pass.
func (r *AutomationRepository) RunByID(ctx context.Context, runID string) (domain.AutomationRun, error) {
	run, err := scanAutomationRun(
		r.db.QueryRowContext(ctx, selectAutomationRunColumns+` WHERE run_id = ?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AutomationRun{}, ErrNotFound
	}
	return run, err
}

// LatestRun reads the most recent pass, or ErrNotFound when none has run.
func (r *AutomationRepository) LatestRun(ctx context.Context) (domain.AutomationRun, error) {
	run, err := scanAutomationRun(
		r.db.QueryRowContext(ctx, selectAutomationRunColumns+` ORDER BY id DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AutomationRun{}, ErrNotFound
	}
	return run, err
}

// AutomationRunFilter narrows a run listing. Every field is a closed vocabulary
// or a bounded page; none becomes SQL text.
type AutomationRunFilter struct {
	States   []domain.AutomationRunState
	Triggers []domain.AutomationTrigger
	// ActedOnly restricts to passes that submitted work, which is the filter an
	// operator asking "when did automation last change something" needs.
	ActedOnly bool
	Page      Page
}

// ListRuns returns a bounded page of passes, newest first.
func (r *AutomationRepository) ListRuns(
	ctx context.Context,
	filter AutomationRunFilter,
) ([]domain.AutomationRun, int, error) {
	page := filter.Page.normalise()

	where := []string{"1 = 1"}
	args := make([]any, 0, 8)

	if len(filter.States) > 0 {
		placeholders := make([]string, 0, len(filter.States))
		for _, state := range filter.States {
			placeholders = append(placeholders, "?")
			args = append(args, string(state))
		}
		where = append(where, "state IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(filter.Triggers) > 0 {
		placeholders := make([]string, 0, len(filter.Triggers))
		for _, trigger := range filter.Triggers {
			placeholders = append(placeholders, "?")
			args = append(args, string(trigger))
		}
		where = append(where, "trigger_source IN ("+strings.Join(placeholders, ", ")+")")
	}
	if filter.ActedOnly {
		where = append(where, "submitted > 0")
	}
	clause := " WHERE " + strings.Join(where, " AND ")

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM automation_runs`+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count automation runs: %w", AsError(err))
	}

	args = append(args, page.Limit, page.Offset)
	rows, err := r.db.QueryContext(ctx,
		selectAutomationRunColumns+clause+` ORDER BY id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list automation runs: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	runs := make([]domain.AutomationRun, 0, page.Limit)
	for rows.Next() {
		run, err := scanAutomationRun(rows)
		if err != nil {
			return nil, 0, err
		}
		runs = append(runs, run)
	}
	return runs, total, rows.Err()
}

// RunSummary aggregates the history for the dashboard.
func (r *AutomationRepository) RunSummary(ctx context.Context) (domain.AutomationRunSummary, error) {
	var summary domain.AutomationRunSummary
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN state = 'completed' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN state = 'failed'    THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(submitted), 0)
		  FROM automation_runs`).
		Scan(&summary.Total, &summary.Completed, &summary.Failed, &summary.Submitted)
	if err != nil {
		return domain.AutomationRunSummary{}, fmt.Errorf("summarise automation runs: %w", AsError(err))
	}
	return summary, nil
}

// PruneRuns deletes passes older than the cutoff.
//
// The decisions cascade. Bounded per call so a first prune on a long-running
// installation cannot hold a write lock for the length of a delete over months
// of history.
func (r *AutomationRepository) PruneRuns(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM automation_runs
		 WHERE id IN (
		       SELECT id FROM automation_runs
		        WHERE state <> 'running' AND started_at < ?
		        ORDER BY id ASC
		        LIMIT ?)`,
		formatTime(before.UTC()), limit)
	if err != nil {
		return 0, fmt.Errorf("prune automation runs: %w", AsError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune automation runs: %w", AsError(err))
	}
	return int(affected), nil
}

func scanAutomationRun(scanner rowScanner) (domain.AutomationRun, error) {
	var (
		run         domain.AutomationRun
		dryRun      int
		completedAt sql.NullString
		startedAt   string
		requester   domain.Requester
	)
	err := scanner.Scan(
		&run.ID, &run.RunID, &run.Trigger, &run.State, &dryRun,
		&run.Considered, &run.Eligible, &run.Submitted, &run.Skipped, &run.Failed,
		&requester.UserID, &requester.Username,
		&run.Message, &startedAt, &completedAt, &run.DurationMs)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AutomationRun{}, err
		}
		return domain.AutomationRun{}, fmt.Errorf("scan automation run: %w", AsError(err))
	}

	run.DryRun = dryRun == 1
	run.RequestedBy = requester

	parsed, err := parseTime(startedAt)
	if err != nil {
		return domain.AutomationRun{}, fmt.Errorf("automation run %s: %w", run.RunID, err)
	}
	run.StartedAt = parsed
	if completedAt.Valid {
		finished, err := parseTime(completedAt.String)
		if err != nil {
			return domain.AutomationRun{}, fmt.Errorf("automation run %s: %w", run.RunID, err)
		}
		run.CompletedAt = &finished
	}
	return run, nil
}

// -------------------------------------------------------------- decisions --

// RecordDecisions writes one pass's decisions in a single transaction.
//
// Batched because a pass on a large estate writes hundreds of rows and one
// transaction per row would make the scheduler the busiest writer on the
// database. Written after the pass has finished deciding, so a decision row
// always reflects a completed decision rather than a partial one.
func (r *AutomationRepository) RecordDecisions(
	ctx context.Context,
	decisions []domain.AutomationDecision,
) (int, error) {
	if len(decisions) == 0 {
		return 0, nil
	}
	if len(decisions) > maxAutomationDecisionsPerRun {
		decisions = decisions[:maxAutomationDecisionsPerRun]
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin automation decisions: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	statement, err := tx.PrepareContext(ctx, `
		INSERT INTO automation_decisions
			(run_id, container_id, container_name, policy_id, policy_name,
			 verdict, reason, detail,
			 plan_id, current_image, proposed_image, proposed_digest,
			 update_type, recommendation,
			 acquisition_id, execution_id, rollback_id, position, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare automation decision: %w", AsError(err))
	}
	defer func() { _ = statement.Close() }()

	written := 0
	for _, decision := range decisions {
		// A policy id must be NULL rather than empty when absent: the column
		// carries a foreign key, and '' is not a policy.
		var policyID any
		if decision.PolicyID != "" {
			policyID = decision.PolicyID
		}
		if _, err := statement.ExecContext(ctx,
			decision.RunID, decision.ContainerID, decision.ContainerName,
			policyID, decision.PolicyName,
			string(decision.Verdict), string(decision.Reason), decision.Detail,
			decision.PlanID, decision.CurrentImage, decision.ProposedImage,
			decision.ProposedDigest, string(decision.UpdateType), string(decision.Recommendation),
			decision.AcquisitionID, decision.ExecutionID, decision.RollbackID,
			decision.Position, formatTime(decision.DecidedAt.UTC())); err != nil {
			return written, fmt.Errorf("insert automation decision: %w", AsError(err))
		}
		written++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit automation decisions: %w", AsError(err))
	}
	return written, nil
}

// AttachExecution records the execution a decision's acquisition led to.
//
// Written after the fact because a decision is recorded when the pass makes it
// and the execution id exists only once the recreation has been submitted. The
// update is restricted to the decision's own acquisition id, so a decision can
// only ever be linked to work it started.
func (r *AutomationRepository) AttachExecution(
	ctx context.Context,
	runID, acquisitionID, executionID string,
) error {
	if acquisitionID == "" || executionID == "" {
		return errors.New("attaching an execution needs both identifiers")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE automation_decisions
		   SET execution_id = ?
		 WHERE run_id = ? AND acquisition_id = ? AND execution_id = ''`,
		executionID, runID, acquisitionID)
	if err != nil {
		return fmt.Errorf("attach execution to decision: %w", AsError(err))
	}
	return nil
}

// PromoteApproved records that a person released a held decision.
//
// Restricted to a decision that is STILL awaiting approval, so two operators
// approving the same decision cannot both submit: the second update matches no
// row and the caller's second acquisition is refused by its own idempotency
// key. Approving is the one place a decision's verdict changes after the pass
// that made it, and it is a conditional update for exactly that reason.
func (r *AutomationRepository) PromoteApproved(
	ctx context.Context,
	runID, containerName, acquisitionID string,
) error {
	if runID == "" || containerName == "" || acquisitionID == "" {
		return errors.New("promoting an approved decision needs the run, the container, and the acquisition")
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE automation_decisions
		   SET verdict = 'update', reason = 'eligible',
		       detail = 'released by an operator',
		       acquisition_id = ?
		 WHERE run_id = ? AND container_name = ?
		   AND verdict = 'awaitingApproval' AND acquisition_id = ''`,
		acquisitionID, runID, containerName)
	if err != nil {
		return fmt.Errorf("promote approved decision: %w", AsError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("promote approved decision: %w", AsError(err))
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SettleDecision marks a decision as having reached a terminal outcome.
//
// The follower's last act on a decision, whatever the ending: the update
// succeeded, the pull failed, a preflight refused, or a rollback was submitted.
// Written once and never cleared -- a settled decision is history.
func (r *AutomationRepository) SettleDecision(
	ctx context.Context,
	runID, containerName string,
	at time.Time,
) error {
	if runID == "" || containerName == "" {
		return errors.New("settling a decision needs the run and the container")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE automation_decisions
		   SET settled_at = ?
		 WHERE run_id = ? AND container_name = ? AND settled_at IS NULL`,
		formatTime(at.UTC()), runID, containerName)
	if err != nil {
		return fmt.Errorf("settle automation decision: %w", AsError(err))
	}
	return nil
}

// AttachRollback records the rollback a decision's execution led to.
func (r *AutomationRepository) AttachRollback(
	ctx context.Context,
	runID, executionID, rollbackID string,
) error {
	if executionID == "" || rollbackID == "" {
		return errors.New("attaching a rollback needs both identifiers")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE automation_decisions
		   SET rollback_id = ?
		 WHERE run_id = ? AND execution_id = ? AND rollback_id = ''`,
		rollbackID, runID, executionID)
	if err != nil {
		return fmt.Errorf("attach rollback to decision: %w", AsError(err))
	}
	return nil
}

const selectAutomationDecisionColumns = `
	SELECT id, run_id, container_id, container_name,
	       COALESCE(policy_id, ''), policy_name,
	       verdict, reason, detail,
	       plan_id, current_image, proposed_image, proposed_digest,
	       update_type, recommendation,
	       acquisition_id, execution_id, rollback_id, position, decided_at
	FROM automation_decisions`

// AutomationDecisionFilter narrows a decision listing.
type AutomationDecisionFilter struct {
	RunID         string
	ContainerName string
	PolicyID      string
	Verdicts      []domain.AutomationVerdict
	// LatestRunOnly restricts the result to the most recent pass.
	//
	// The same restriction CountAwaitingApproval applies, and for the same
	// reason: an approval answers "should this change happen now", and a pass
	// that ran an hour ago asked a question about an hour ago. Every pass since
	// re-asked it and recorded its own held decision, so without this an
	// operator with two outstanding approvals sees one row per pass -- fourteen
	// identical rows for two decisions, thirteen of them answering a question
	// nobody is still asking.
	//
	// It also keeps the queue and the dashboard counter telling the same story.
	// They disagreed while this did not exist.
	LatestRunOnly bool
	Page          Page
}

// ListDecisions returns a bounded page.
//
// Ordered by run then position, so a pass reads back in the sequence it decided
// -- which is what makes a dry run's output "what would happen, in what order"
// rather than an unordered set.
func (r *AutomationRepository) ListDecisions(
	ctx context.Context,
	filter AutomationDecisionFilter,
) ([]domain.AutomationDecision, int, error) {
	page := filter.Page.normalise()

	where := []string{"1 = 1"}
	args := make([]any, 0, 8)

	if filter.RunID != "" {
		where = append(where, "run_id = ?")
		args = append(args, filter.RunID)
	}
	if filter.ContainerName != "" {
		where = append(where, "container_name = ?")
		args = append(args, filter.ContainerName)
	}
	if filter.PolicyID != "" {
		where = append(where, "policy_id = ?")
		args = append(args, filter.PolicyID)
	}
	if filter.LatestRunOnly {
		where = append(where,
			"run_id = (SELECT run_id FROM automation_runs ORDER BY id DESC LIMIT 1)")
	}
	if len(filter.Verdicts) > 0 {
		placeholders := make([]string, 0, len(filter.Verdicts))
		for _, verdict := range filter.Verdicts {
			placeholders = append(placeholders, "?")
			args = append(args, string(verdict))
		}
		where = append(where, "verdict IN ("+strings.Join(placeholders, ", ")+")")
	}
	clause := " WHERE " + strings.Join(where, " AND ")

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM automation_decisions`+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count automation decisions: %w", AsError(err))
	}

	args = append(args, page.Limit, page.Offset)
	rows, err := r.db.QueryContext(ctx, selectAutomationDecisionColumns+clause+`
		 ORDER BY run_id DESC, position ASC, id ASC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list automation decisions: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	decisions := make([]domain.AutomationDecision, 0, page.Limit)
	for rows.Next() {
		decision, err := scanAutomationDecision(rows)
		if err != nil {
			return nil, 0, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, total, rows.Err()
}

// PendingDecisions returns the work the follower still owes a next step.
//
// # Why this is a query and not a walk over recent runs
//
// The follower's question is "what did I start that has not finished", and that
// is a property of the DECISIONS, not of which passes happen to be recent. An
// earlier shape walked the last few runs, which had two defects: an approval is
// promoted after its pass has already finished, and a decision that takes longer
// to resolve than a few scheduler ticks fell out of the window and was abandoned
// with an acquired image and no recreation.
//
// A decision is outstanding when it named an acquisition and has not been
// SETTLED. Settling is its own fact, written when the decision reaches any
// terminal outcome, because "finished" and "was rolled back" are not the same
// thing -- an earlier shape used the absence of a rollback id and consequently
// treated every successful update as permanently outstanding.
//
// Newest first, so a backlog is worked from the most recent, and bounded, so one
// tick's cost is fixed.
func (r *AutomationRepository) PendingDecisions(
	ctx context.Context,
	limit int,
) ([]domain.AutomationDecision, error) {
	if limit < 1 || limit > maxFollowerBatch {
		limit = maxFollowerBatch
	}

	rows, err := r.db.QueryContext(ctx, selectAutomationDecisionColumns+`
		 WHERE verdict = 'update'
		   AND acquisition_id <> ''
		   AND settled_at IS NULL
		 ORDER BY id DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("load pending automation decisions: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	decisions := make([]domain.AutomationDecision, 0, 16)
	for rows.Next() {
		decision, err := scanAutomationDecision(rows)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, rows.Err()
}

// maxFollowerBatch bounds one follower tick.
const maxFollowerBatch = 200

// CountAwaitingApproval returns how many decisions wait for a person.
//
// Restricted to the most recent pass: an approval is an answer to "should this
// change happen now", and last week's proposal is not that question. Older ones
// remain in the history and are simply not counted as pending.
func (r *AutomationRepository) CountAwaitingApproval(ctx context.Context) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM automation_decisions
		 WHERE verdict = 'awaitingApproval'
		   AND run_id = (SELECT run_id FROM automation_runs ORDER BY id DESC LIMIT 1)`).
		Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count decisions awaiting approval: %w", AsError(err))
	}
	return count, nil
}

func scanAutomationDecision(scanner rowScanner) (domain.AutomationDecision, error) {
	var (
		decision  domain.AutomationDecision
		decidedAt string
	)
	err := scanner.Scan(
		&decision.ID, &decision.RunID, &decision.ContainerID, &decision.ContainerName,
		&decision.PolicyID, &decision.PolicyName,
		&decision.Verdict, &decision.Reason, &decision.Detail,
		&decision.PlanID, &decision.CurrentImage, &decision.ProposedImage,
		&decision.ProposedDigest, &decision.UpdateType, &decision.Recommendation,
		&decision.AcquisitionID, &decision.ExecutionID, &decision.RollbackID,
		&decision.Position, &decidedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AutomationDecision{}, err
		}
		return domain.AutomationDecision{}, fmt.Errorf("scan automation decision: %w", AsError(err))
	}
	parsed, err := parseTime(decidedAt)
	if err != nil {
		return domain.AutomationDecision{}, fmt.Errorf("automation decision %d: %w", decision.ID, err)
	}
	decision.DecidedAt = parsed
	return decision, nil
}

// ---------------------------------------------------------------- pauses --

// Pause stops automation for one container.
//
// Keyed on the NAME. A container's id changes every time it is recreated, and
// recreation is exactly what automation does, so a pause keyed on the id would
// be cleared by the very action that went wrong.
//
// The partial unique index refuses a second active pause, so a re-pause of an
// already-paused container reports ErrPauseActive rather than silently creating
// a duplicate an operator would then have to clear twice.
func (r *AutomationRepository) Pause(
	ctx context.Context,
	pause domain.PausedContainer,
) (domain.PausedContainer, error) {
	if strings.TrimSpace(pause.ContainerName) == "" {
		return domain.PausedContainer{}, errors.New("a pause must name a container")
	}

	var resumeAfter any
	if pause.ResumeAfter != nil {
		resumeAfter = formatTime(pause.ResumeAfter.UTC())
	}
	var policyID any
	if pause.PolicyID != "" {
		policyID = pause.PolicyID
	}

	result, err := r.db.ExecContext(ctx, `
		INSERT INTO automation_pauses
			(container_name, container_id, reason, detail, failures,
			 policy_id, rollback_id, execution_id, paused_at, resume_after)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pause.ContainerName, pause.ContainerID, string(pause.Reason), pause.Detail,
		pause.Failures, policyID, pause.RollbackID, pause.ExecutionID,
		formatTime(pause.PausedAt.UTC()), resumeAfter)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.PausedContainer{}, ErrPauseActive
		}
		return domain.PausedContainer{}, fmt.Errorf("pause automation: %w", AsError(err))
	}
	id, _ := result.LastInsertId()
	pause.ID = id
	return pause, nil
}

// Resume clears a pause, recording who cleared it.
//
// An acknowledgement, not a delete: the fact that automation stopped touching a
// container is history an operator may need next month.
func (r *AutomationRepository) Resume(
	ctx context.Context,
	containerName string,
	by domain.Requester,
	at time.Time,
) error {
	if strings.TrimSpace(by.Username) == "" {
		// The schema refuses it too. Checked here so the error names the
		// missing attribution rather than surfacing a constraint violation.
		return errors.New("resuming automation must record who did it")
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE automation_pauses
		   SET acknowledged_at = ?, acknowledged_user_id = ?,
		       acknowledged_username = ?
		 WHERE container_name = ? AND acknowledged_at IS NULL`,
		formatTime(at.UTC()), by.UserID, by.Username, containerName)
	if err != nil {
		return fmt.Errorf("resume automation: %w", AsError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("resume automation: %w", AsError(err))
	}
	if affected == 0 {
		return ErrNotFound
	}
	// A resumed container starts from zero. Leaving the count would pause it
	// again on the next failure, which is not what an operator who investigated
	// and fixed the problem asked for.
	if _, err := r.db.ExecContext(ctx, `
		UPDATE automation_failures
		   SET consecutive = 0, windowed = 0, window_started_at = NULL
		 WHERE container_name = ?`, containerName); err != nil {
		return fmt.Errorf("reset automation failures: %w", AsError(err))
	}
	return nil
}

const selectPauseColumns = `
	SELECT id, container_name, container_id, reason, detail, failures,
	       COALESCE(policy_id, ''), rollback_id, execution_id,
	       paused_at, resume_after,
	       acknowledged_at, acknowledged_user_id, acknowledged_username
	FROM automation_pauses`

// ActivePauses returns every container automation will not touch.
//
// Includes pauses whose cooldown has elapsed: whether an expired cooldown still
// blocks is domain.PausedContainer.Active's decision, made against a single
// clock reading in the scheduler, rather than a SQL comparison against a
// different one.
func (r *AutomationRepository) ActivePauses(ctx context.Context) ([]domain.PausedContainer, error) {
	rows, err := r.db.QueryContext(ctx, selectPauseColumns+`
		 WHERE acknowledged_at IS NULL
		 ORDER BY paused_at DESC, id DESC
		 LIMIT ?`, maxActivePauses)
	if err != nil {
		return nil, fmt.Errorf("load automation pauses: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	pauses := make([]domain.PausedContainer, 0, 8)
	for rows.Next() {
		pause, err := scanPause(rows)
		if err != nil {
			return nil, err
		}
		pauses = append(pauses, pause)
	}
	return pauses, rows.Err()
}

// maxActivePauses bounds the pause load one pass performs.
const maxActivePauses = 1000

// PauseFor reads the active pause for one container, or ErrNotFound.
func (r *AutomationRepository) PauseFor(
	ctx context.Context,
	containerName string,
) (domain.PausedContainer, error) {
	pause, err := scanPause(r.db.QueryRowContext(ctx, selectPauseColumns+`
		 WHERE container_name = ? AND acknowledged_at IS NULL`, containerName))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.PausedContainer{}, ErrNotFound
	}
	return pause, err
}

// ListPauses returns a bounded page including cleared ones, for the history.
func (r *AutomationRepository) ListPauses(
	ctx context.Context,
	activeOnly bool,
	page Page,
) ([]domain.PausedContainer, int, error) {
	page = page.normalise()

	clause := ""
	if activeOnly {
		clause = " WHERE acknowledged_at IS NULL"
	}

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM automation_pauses`+clause).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count automation pauses: %w", AsError(err))
	}

	rows, err := r.db.QueryContext(ctx, selectPauseColumns+clause+`
		 ORDER BY paused_at DESC, id DESC LIMIT ? OFFSET ?`, page.Limit, page.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list automation pauses: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	pauses := make([]domain.PausedContainer, 0, page.Limit)
	for rows.Next() {
		pause, err := scanPause(rows)
		if err != nil {
			return nil, 0, err
		}
		pauses = append(pauses, pause)
	}
	return pauses, total, rows.Err()
}

// CountActivePauses returns how many containers automation will not touch.
func (r *AutomationRepository) CountActivePauses(ctx context.Context) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM automation_pauses WHERE acknowledged_at IS NULL`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count automation pauses: %w", AsError(err))
	}
	return count, nil
}

func scanPause(scanner rowScanner) (domain.PausedContainer, error) {
	var (
		pause          domain.PausedContainer
		pausedAt       string
		resumeAfter    sql.NullString
		acknowledgedAt sql.NullString
		ackUserID      string
		ackUsername    string
	)
	err := scanner.Scan(
		&pause.ID, &pause.ContainerName, &pause.ContainerID,
		&pause.Reason, &pause.Detail, &pause.Failures,
		&pause.PolicyID, &pause.RollbackID, &pause.ExecutionID,
		&pausedAt, &resumeAfter,
		&acknowledgedAt, &ackUserID, &ackUsername)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.PausedContainer{}, err
		}
		return domain.PausedContainer{}, fmt.Errorf("scan automation pause: %w", AsError(err))
	}

	parsed, err := parseTime(pausedAt)
	if err != nil {
		return domain.PausedContainer{}, fmt.Errorf("automation pause %d: %w", pause.ID, err)
	}
	pause.PausedAt = parsed

	if resumeAfter.Valid {
		resume, err := parseTime(resumeAfter.String)
		if err != nil {
			return domain.PausedContainer{}, fmt.Errorf("automation pause %d: %w", pause.ID, err)
		}
		pause.ResumeAfter = &resume
	}
	if acknowledgedAt.Valid {
		acknowledged, err := parseTime(acknowledgedAt.String)
		if err != nil {
			return domain.PausedContainer{}, fmt.Errorf("automation pause %d: %w", pause.ID, err)
		}
		pause.AcknowledgedAt = &acknowledged
		pause.AcknowledgedBy = domain.Requester{UserID: ackUserID, Username: ackUsername}
	}
	return pause, nil
}

// -------------------------------------------------------------- failures --

// AutomationFailureCount is one container's rolling failure record.
type AutomationFailureCount struct {
	ContainerName   string
	Consecutive     int
	Windowed        int
	WindowStartedAt *time.Time
	LastFailureAt   *time.Time
	LastSuccessAt   *time.Time
	LastDetail      string
}

// RecordFailure increments a container's counters and returns the new state.
//
// The windowed count resets when the policy's pause window has elapsed since
// the window started, which is what makes "two failures in 24 hours" mean that
// rather than "two failures ever". Computed here, in one transaction with the
// increment, so two concurrent failures cannot both read the pre-increment
// value and each decide the threshold has not been reached.
func (r *AutomationRepository) RecordFailure(
	ctx context.Context,
	containerName string,
	detail string,
	windowHours int,
	at time.Time,
) (AutomationFailureCount, error) {
	if strings.TrimSpace(containerName) == "" {
		return AutomationFailureCount{}, errors.New("recording a failure needs a container name")
	}
	if windowHours < 1 {
		windowHours = 1
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return AutomationFailureCount{}, fmt.Errorf("begin failure record: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	current, err := readFailureCount(ctx, tx, containerName)
	if err != nil {
		return AutomationFailureCount{}, err
	}

	windowStart := current.WindowStartedAt
	windowed := current.Windowed
	cutoff := at.Add(-time.Duration(windowHours) * time.Hour)
	if windowStart == nil || windowStart.Before(cutoff) {
		// The window has elapsed. A new one starts now, counting this failure.
		started := at.UTC()
		windowStart = &started
		windowed = 0
	}
	windowed++

	stamp := formatTime(at.UTC())
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO automation_failures
			(container_name, consecutive, windowed, window_started_at,
			 last_failure_at, last_detail)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (container_name) DO UPDATE SET
			consecutive       = consecutive + 1,
			windowed          = excluded.windowed,
			window_started_at = excluded.window_started_at,
			last_failure_at   = excluded.last_failure_at,
			last_detail       = excluded.last_detail`,
		containerName, 1, windowed, formatTime(windowStart.UTC()),
		stamp, truncateDetail(detail)); err != nil {
		return AutomationFailureCount{}, fmt.Errorf("record automation failure: %w", AsError(err))
	}

	updated, err := readFailureCount(ctx, tx, containerName)
	if err != nil {
		return AutomationFailureCount{}, err
	}
	if err := tx.Commit(); err != nil {
		return AutomationFailureCount{}, fmt.Errorf("commit failure record: %w", AsError(err))
	}
	return updated, nil
}

// RecordSuccess clears a container's consecutive count.
//
// The WINDOWED count is deliberately left alone. A container that fails, then
// succeeds, then fails again inside the same window has failed twice in that
// window, and a policy that said "pause after two failures in 24 hours" meant
// exactly that. Clearing the window on any success would make the setting
// unreachable for the flapping container it exists to catch.
func (r *AutomationRepository) RecordSuccess(
	ctx context.Context,
	containerName string,
	at time.Time,
) error {
	if strings.TrimSpace(containerName) == "" {
		return errors.New("recording a success needs a container name")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO automation_failures (container_name, consecutive, last_success_at)
		VALUES (?, 0, ?)
		ON CONFLICT (container_name) DO UPDATE SET
			consecutive     = 0,
			last_success_at = excluded.last_success_at`,
		containerName, formatTime(at.UTC()))
	if err != nil {
		return fmt.Errorf("record automation success: %w", AsError(err))
	}
	return nil
}

// FailureCount reads one container's counters.
func (r *AutomationRepository) FailureCount(
	ctx context.Context,
	containerName string,
) (AutomationFailureCount, error) {
	return readFailureCount(ctx, r.db, containerName)
}

// queryContext is satisfied by *sql.DB and *sql.Tx.
type queryContext interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readFailureCount(
	ctx context.Context,
	db queryContext,
	containerName string,
) (AutomationFailureCount, error) {
	var (
		count       = AutomationFailureCount{ContainerName: containerName}
		windowStart sql.NullString
		lastFailure sql.NullString
		lastSuccess sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT consecutive, windowed, window_started_at,
		       last_failure_at, last_success_at, last_detail
		  FROM automation_failures
		 WHERE container_name = ?`, containerName).
		Scan(&count.Consecutive, &count.Windowed, &windowStart,
			&lastFailure, &lastSuccess, &count.LastDetail)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No record is not an error. A container that has never failed has a
		// count of zero, and returning ErrNotFound would make every caller
		// write the same branch.
		return count, nil
	case err != nil:
		return AutomationFailureCount{}, fmt.Errorf("read automation failures: %w", AsError(err))
	}

	for _, field := range []struct {
		source sql.NullString
		target **time.Time
	}{
		{windowStart, &count.WindowStartedAt},
		{lastFailure, &count.LastFailureAt},
		{lastSuccess, &count.LastSuccessAt},
	} {
		if !field.source.Valid {
			continue
		}
		parsed, err := parseTime(field.source.String)
		if err != nil {
			return AutomationFailureCount{}, fmt.Errorf("automation failures for %s: %w", containerName, err)
		}
		value := parsed
		*field.target = &value
	}
	return count, nil
}

// truncateDetail bounds a stored sentence to what the column accepts.
//
// HarborMaster writes these, so the bound is never expected to bite. It is here
// because a CHECK that fails is a lost record, and a lost record about a
// failure is the one you needed.
func truncateDetail(detail string) string {
	const limit = 1024
	if len(detail) <= limit {
		return detail
	}
	return detail[:limit]
}
