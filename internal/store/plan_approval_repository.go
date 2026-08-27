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

// PlanApprovalRepository stores human approvals of immutable change plans.
//
// # Small on purpose
//
// Four operations: approve, read the active one, revoke, and ask whether an
// execution of the plan has already changed the host. Nothing here decides
// whether an approval is VALID -- that is domain.PlanApprovalValid, which is
// pure and shared with the execution preflight, so the database and the decision
// cannot drift apart.
type PlanApprovalRepository struct {
	db *sql.DB
}

// ErrPlanApprovalActive reports that the plan already has an active approval.
var ErrPlanApprovalActive = errors.New("this plan is already approved")

const selectPlanApprovalColumns = `
	SELECT id, plan_id, state,
	       approved_input_digest, approved_proposed_digest,
	       approved_by_user_id, approved_by_username, approved_at,
	       revoked_by_user_id, revoked_by_username, revoked_at,
	       created_at, updated_at
	FROM plan_approvals`

// Approve records one human authorisation of one plan.
//
// # Idempotency is the index, not a lock
//
// The partial unique index on (plan_id) WHERE state = 'active' is what makes two
// concurrent approvals settle on one. The loser is told the plan is already
// approved rather than being allowed to write a second authority, and callers
// can turn that into whatever their API convention needs.
//
// The digests are copied from the plan HERE, by the caller reading the plan --
// never from a request body. See the migration header on why they exist.
func (r *PlanApprovalRepository) Approve(
	ctx context.Context,
	approval domain.PlanApproval,
	at time.Time,
) (domain.PlanApproval, error) {
	if strings.TrimSpace(approval.ApprovedBy.Username) == "" {
		// The schema refuses it too. Checked here so the error names the missing
		// attribution rather than surfacing a constraint violation: an approval
		// nobody made is the one thing this table must never hold.
		return domain.PlanApproval{}, errors.New(
			"approving a plan must record who did it")
	}

	stamp := formatTime(at.UTC())
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO plan_approvals
			(plan_id, state, approved_input_digest, approved_proposed_digest,
			 approved_by_user_id, approved_by_username, approved_at,
			 created_at, updated_at)
		VALUES (?, 'active', ?, ?, ?, ?, ?, ?, ?)`,
		approval.PlanID,
		approval.ApprovedInputDigest, approval.ApprovedProposedDigest,
		approval.ApprovedBy.UserID, approval.ApprovedBy.Username, stamp,
		stamp, stamp)
	if err != nil {
		// The foreign key is asked about FIRST. isUniqueViolation treats any
		// constraint failure as a unique one, so a missing plan would otherwise
		// be reported as "already approved" -- the opposite of what happened.
		if isForeignKeyViolation(err) {
			return domain.PlanApproval{}, ErrNotFound
		}
		if isUniqueViolation(err) {
			return domain.PlanApproval{}, ErrPlanApprovalActive
		}
		return domain.PlanApproval{}, fmt.Errorf("approve plan: %w", AsError(err))
	}

	id, err := result.LastInsertId()
	if err != nil {
		return domain.PlanApproval{}, fmt.Errorf("approve plan: %w", AsError(err))
	}

	approval.ID = id
	approval.State = domain.PlanApprovalActive
	approval.ApprovedAt = at.UTC()
	approval.CreatedAt = at.UTC()
	approval.UpdatedAt = at.UTC()
	return approval, nil
}

// Active returns the plan's standing approval, if it has one.
//
// An indexed point lookup on the partial unique index.
func (r *PlanApprovalRepository) Active(
	ctx context.Context,
	planID string,
) (domain.PlanApproval, error) {
	rows, err := r.db.QueryContext(ctx,
		selectPlanApprovalColumns+` WHERE plan_id = ? AND state = 'active'`, planID)
	if err != nil {
		return domain.PlanApproval{}, fmt.Errorf("read plan approval: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	approvals, err := scanPlanApprovals(rows)
	if err != nil {
		return domain.PlanApproval{}, err
	}
	if len(approvals) == 0 {
		return domain.PlanApproval{}, ErrNotFound
	}
	return approvals[0], nil
}

// Revoke withdraws the plan's active approval.
//
// Conditional UPDATE, so two concurrent revocations settle the same way two
// approvals do: one changes a row, the rest are told there was nothing to
// withdraw. The row is kept -- a withdrawn decision is still a decision somebody
// made, and deleting it would lose that.
func (r *PlanApprovalRepository) Revoke(
	ctx context.Context,
	planID string,
	by domain.Requester,
	at time.Time,
) error {
	if strings.TrimSpace(by.Username) == "" {
		return errors.New("revoking an approval must record who did it")
	}

	stamp := formatTime(at.UTC())
	result, err := r.db.ExecContext(ctx, `
		UPDATE plan_approvals
		   SET state = 'revoked', revoked_at = ?,
		       revoked_by_user_id = ?, revoked_by_username = ?,
		       updated_at = ?
		 WHERE plan_id = ? AND state = 'active'`,
		stamp, by.UserID, by.Username, stamp, planID)
	if err != nil {
		return fmt.Errorf("revoke plan approval: %w", AsError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke plan approval: %w", AsError(err))
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// PlanHasMutated reports whether an execution of this plan already changed the
// host.
//
// # Why this lives here rather than as a column
//
// It is the derived consumption rule. `executions.mutated_at` is where "the host
// was first changed" is already recorded, and it is written by the code that
// does the changing. Copying it into an approval state would duplicate a fact
// that moves without this table being told -- and a stale copy would either
// re-authorise a spent approval or refuse a live one.
//
// An indexed lookup: idx_execution_plan covers (plan_id, id DESC).
func (r *PlanApprovalRepository) PlanHasMutated(
	ctx context.Context,
	planID string,
) (bool, error) {
	var count int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM executions
		 WHERE plan_id = ? AND mutated_at IS NOT NULL`, planID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("read plan mutation history: %w", AsError(err))
	}
	return count > 0, nil
}

func scanPlanApprovals(rows *sql.Rows) ([]domain.PlanApproval, error) {
	var out []domain.PlanApproval
	for rows.Next() {
		var (
			err        error
			approval   domain.PlanApproval
			state      string
			approvedAt string
			revokedAt  sql.NullString
			createdAt  string
			updatedAt  string
		)
		if err := rows.Scan(
			&approval.ID, &approval.PlanID, &state,
			&approval.ApprovedInputDigest, &approval.ApprovedProposedDigest,
			&approval.ApprovedBy.UserID, &approval.ApprovedBy.Username, &approvedAt,
			&approval.RevokedBy.UserID, &approval.RevokedBy.Username, &revokedAt,
			&createdAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan plan approval: %w", AsError(err))
		}

		approval.State = domain.PlanApprovalState(state)
		if approval.ApprovedAt, err = parseTime(approvedAt); err != nil {
			return nil, fmt.Errorf("scan plan approval: %w", err)
		}
		if approval.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, fmt.Errorf("scan plan approval: %w", err)
		}
		if approval.UpdatedAt, err = parseTime(updatedAt); err != nil {
			return nil, fmt.Errorf("scan plan approval: %w", err)
		}
		if revokedAt.Valid {
			moment, err := parseTime(revokedAt.String)
			if err != nil {
				return nil, fmt.Errorf("scan plan approval: %w", err)
			}
			approval.RevokedAt = &moment
		}
		out = append(out, approval)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan plan approvals: %w", AsError(err))
	}
	return out, nil
}
