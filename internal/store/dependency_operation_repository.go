package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Persistence for coordinated, dependency-safe provider updates.
//
// # The one ordering rule this file exists to enforce
//
// An operation and its complete member set are written in ONE transaction,
// BEFORE the provider is stopped. If that write fails, nothing is mutated.
//
// The reverse ordering -- stop first, record afterwards -- would leave a
// stopped provider and no record of what depended on it, which is the exact
// situation a restart cannot recover from: the dependents are already broken and
// nothing says which they are.
//
// # Reads are bounded and never N+1
//
// An operation is loaded with its members in TWO queries regardless of how many
// members it has, and the open-operation sweep loads every outstanding
// operation's members in two queries regardless of how many operations there
// are. Both are covered by indexes added in migration 0025.

// DependencyOperationRepository owns operation and member persistence.
type DependencyOperationRepository struct {
	db *sql.DB
}

// NewDependencyOperationRepository builds a DependencyOperationRepository.
func NewDependencyOperationRepository(db *sql.DB) *DependencyOperationRepository {
	return &DependencyOperationRepository{db: db}
}

// ErrDependencyOperationActive reports that the provider already has one.
//
// A refusal, not a fault. Two coordinated updates of the same container would be
// two components deciding what its dependents are attached to.
var ErrDependencyOperationActive = errors.New(
	"a dependency operation for that container is already in progress")

// maxDependencyMembers bounds one operation's member set.
//
// Matches domain.MaxDependencyFanIn: the members ARE the hard dependents, and
// the graph refuses to build past that bound anyway. Stated here as well so the
// write path has its own ceiling rather than inheriting one by coincidence.
const maxDependencyMembers = domain.MaxDependencyFanIn

// Create writes an operation and its complete member set in one transaction.
//
// # Atomic on purpose
//
// A partial member set is worse than none. An operation recorded with two of its
// three dependents would let the provider be stopped while the third is unknown
// to the coordinator -- which is precisely the silent breakage the whole phase
// exists to prevent. So either every member lands or nothing does.
//
// Refuses an empty member set: an operation with no mandatory rebinds is not a
// dependency operation, and the caller should not have created one.
func (r *DependencyOperationRepository) Create(
	ctx context.Context,
	operation domain.DependencyOperation,
	now time.Time,
) (domain.DependencyOperation, error) {
	if len(operation.Members) == 0 {
		return domain.DependencyOperation{},
			errors.New("refusing to record a dependency operation with no members")
	}
	if len(operation.Members) > maxDependencyMembers {
		return domain.DependencyOperation{},
			fmt.Errorf("refusing to record a dependency operation with %d members; the limit is %d",
				len(operation.Members), maxDependencyMembers)
	}
	if operation.OperationID == "" {
		operation.OperationID = domain.NewDependencyOperationID()
	}
	if operation.State == "" {
		operation.State = domain.OperationQueued
	}
	operation.CreatedAt = now.UTC()
	operation.UpdatedAt = now.UTC()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.DependencyOperation{}, fmt.Errorf("begin dependency operation: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO dependency_operations
			(operation_id, provider_name, provider_plan_id, provider_execution_id,
			 state, failure, requested_by_user, requested_by_name,
			 created_at, updated_at, completed_at)
		VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, ?, NULL)`,
		operation.OperationID, operation.Provider,
		operation.ProviderPlanID, operation.ProviderExecutionID,
		string(operation.State),
		operation.RequestedBy.UserID, operation.RequestedBy.Username,
		formatTime(operation.CreatedAt), formatTime(operation.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			// The partial unique index on non-terminal states fired.
			return domain.DependencyOperation{}, ErrDependencyOperationActive
		}
		return domain.DependencyOperation{}, fmt.Errorf("insert dependency operation: %w", AsError(err))
	}

	for index := range operation.Members {
		member := &operation.Members[index]
		member.OperationID = operation.OperationID
		if member.State == "" {
			member.State = domain.MemberPending
		}
		member.CreatedAt = now.UTC()
		member.UpdatedAt = now.UTC()

		_, err = tx.ExecContext(ctx, `
			INSERT INTO dependency_operation_members
				(operation_id, dependent_name, provider_name, source,
				 expected_provider_id, target_provider_id,
				 plan_id, acquisition_id, execution_id,
				 state, refusal, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			member.OperationID, member.Dependent, member.Provider, string(member.Source),
			member.ExpectedProviderID, member.TargetProviderID,
			member.PlanID, member.AcquisitionID, member.ExecutionID,
			string(member.State), string(member.Refusal),
			formatTime(member.CreatedAt), formatTime(member.UpdatedAt))
		if err != nil {
			return domain.DependencyOperation{},
				fmt.Errorf("insert dependency operation member: %w", AsError(err))
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.DependencyOperation{}, fmt.Errorf("commit dependency operation: %w", AsError(err))
	}

	domain.SortDependencyMembers(operation.Members)
	return operation, nil
}

// Get returns one operation with its members.
func (r *DependencyOperationRepository) Get(
	ctx context.Context,
	operationID string,
) (domain.DependencyOperation, error) {
	if !domain.ValidDependencyOperationID(operationID) {
		return domain.DependencyOperation{}, ErrNotFound
	}

	operation, err := r.scanOperation(r.db.QueryRowContext(ctx, `
		SELECT operation_id, provider_name, provider_plan_id, provider_execution_id,
		       state, failure, requested_by_user, requested_by_name,
		       created_at, updated_at, completed_at
		  FROM dependency_operations
		 WHERE operation_id = ?`, operationID))
	if err != nil {
		return domain.DependencyOperation{}, err
	}

	members, err := r.membersFor(ctx, operationID)
	if err != nil {
		return domain.DependencyOperation{}, err
	}
	operation.Members = members
	return operation, nil
}

// Open returns every operation that has not reached a conclusion, with members.
//
// THE RECOVERY QUERY. Two round trips regardless of how many operations or
// members there are: one for the operations, one for every member of all of
// them, joined in memory.
func (r *DependencyOperationRepository) Open(
	ctx context.Context,
	limit int,
) ([]domain.DependencyOperation, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT operation_id, provider_name, provider_plan_id, provider_execution_id,
		       state, failure, requested_by_user, requested_by_name,
		       created_at, updated_at, completed_at
		  FROM dependency_operations
		 WHERE state NOT IN ('succeeded', 'failed', 'blocked')
		 ORDER BY created_at, operation_id
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query open dependency operations: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	operations := make([]domain.DependencyOperation, 0, 8)
	index := make(map[string]int, 8)
	for rows.Next() {
		operation, err := r.scanOperationRows(rows)
		if err != nil {
			return nil, err
		}
		index[operation.OperationID] = len(operations)
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, AsError(err)
	}
	if len(operations) == 0 {
		return operations, nil
	}

	// One query for every member of every open operation. The predicate mirrors
	// the one above rather than being an IN list of ids, so the query text is
	// fixed and carries no caller-length-dependent shape.
	memberRows, err := r.db.QueryContext(ctx, `
		SELECT m.operation_id, m.dependent_name, m.provider_name, m.source,
		       m.expected_provider_id, m.target_provider_id,
		       m.plan_id, m.acquisition_id, m.execution_id,
		       m.state, m.refusal, m.created_at, m.updated_at
		  FROM dependency_operation_members m
		  JOIN dependency_operations o ON o.operation_id = m.operation_id
		 WHERE o.state NOT IN ('succeeded', 'failed', 'blocked')
		 ORDER BY m.operation_id, m.dependent_name
		 LIMIT ?`, limit*maxDependencyMembers)
	if err != nil {
		return nil, fmt.Errorf("query open dependency members: %w", AsError(err))
	}
	defer func() { _ = memberRows.Close() }()

	for memberRows.Next() {
		member, err := scanMember(memberRows)
		if err != nil {
			return nil, err
		}
		position, ok := index[member.OperationID]
		if !ok {
			continue
		}
		operations[position].Members = append(operations[position].Members, member)
	}
	if err := memberRows.Err(); err != nil {
		return nil, AsError(err)
	}

	for position := range operations {
		domain.SortDependencyMembers(operations[position].Members)
	}
	return operations, nil
}

// ActiveForProvider reports whether a coordinated update is already running.
func (r *DependencyOperationRepository) ActiveForProvider(
	ctx context.Context,
	provider string,
) (domain.DependencyOperation, bool, error) {
	operation, err := r.scanOperation(r.db.QueryRowContext(ctx, `
		SELECT operation_id, provider_name, provider_plan_id, provider_execution_id,
		       state, failure, requested_by_user, requested_by_name,
		       created_at, updated_at, completed_at
		  FROM dependency_operations
		 WHERE provider_name = ?
		   AND state NOT IN ('succeeded', 'failed', 'blocked')`,
		domain.NormaliseContainerName(provider)))
	if errors.Is(err, ErrNotFound) {
		return domain.DependencyOperation{}, false, nil
	}
	if err != nil {
		return domain.DependencyOperation{}, false, err
	}
	members, err := r.membersFor(ctx, operation.OperationID)
	if err != nil {
		return domain.DependencyOperation{}, false, err
	}
	operation.Members = members
	return operation, true, nil
}

// AdvanceOperation moves an operation to a new state.
//
// completedAt is written only for a terminal state, matching the CHECK
// constraint: a row cannot claim to be finished without an ending, nor claim to
// be running with one.
func (r *DependencyOperationRepository) AdvanceOperation(
	ctx context.Context,
	operationID string,
	state domain.DependencyOperationState,
	failure domain.DependencyOperationFailure,
	now time.Time,
) error {
	if !domain.ValidDependencyOperationID(operationID) {
		return ErrNotFound
	}
	if !domain.ValidDependencyOperationState(string(state)) {
		return fmt.Errorf("refusing to write dependency operation state %q", state)
	}
	if !domain.ValidDependencyOperationFailure(string(failure)) {
		return fmt.Errorf("refusing to write dependency operation failure %q", failure)
	}

	var completedAt any
	if state.Terminal() {
		completedAt = formatTime(now.UTC())
	}

	result, err := r.db.ExecContext(ctx, `
		UPDATE dependency_operations
		   SET state = ?, failure = ?, updated_at = ?, completed_at = ?
		 WHERE operation_id = ?`,
		string(state), string(failure), formatTime(now.UTC()), completedAt, operationID)
	if err != nil {
		return fmt.Errorf("advance dependency operation: %w", AsError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("advance dependency operation: %w", AsError(err))
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// AttachProviderExecution records which execution is performing the provider's
// own update.
func (r *DependencyOperationRepository) AttachProviderExecution(
	ctx context.Context,
	operationID, executionID string,
	now time.Time,
) error {
	if !domain.ValidDependencyOperationID(operationID) || !domain.ValidExecutionID(executionID) {
		return ErrNotFound
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE dependency_operations
		   SET provider_execution_id = ?, updated_at = ?
		 WHERE operation_id = ?`,
		executionID, formatTime(now.UTC()), operationID)
	if err != nil {
		return fmt.Errorf("attach provider execution: %w", AsError(err))
	}
	return nil
}

// MemberUpdate is one member's new position.
//
// Every field is a HarborMaster-generated identifier or a closed-vocabulary
// value. There is nothing here a caller could put an image, a digest, or a
// container id of their choosing into.
type MemberUpdate struct {
	OperationID string
	Dependent   string
	State       domain.DependencyMemberState
	Refusal     domain.RebindRefusal

	// The three record ids, written only when non-empty so a partial update
	// cannot blank one that is already set.
	PlanID           string
	AcquisitionID    string
	ExecutionID      string
	TargetProviderID string
}

// AdvanceMember moves one member to a new state.
func (r *DependencyOperationRepository) AdvanceMember(
	ctx context.Context,
	update MemberUpdate,
	now time.Time,
) error {
	if !domain.ValidDependencyOperationID(update.OperationID) {
		return ErrNotFound
	}
	if !domain.ValidDependencyMemberState(string(update.State)) {
		return fmt.Errorf("refusing to write dependency member state %q", update.State)
	}
	if update.Refusal != domain.RebindRefusalNone &&
		!domain.ValidRebindRefusal(string(update.Refusal)) {
		return fmt.Errorf("refusing to write dependency member refusal %q", update.Refusal)
	}

	// COALESCE on the empty string, so an update that carries only a state does
	// not blank the ids an earlier step recorded.
	result, err := r.db.ExecContext(ctx, `
		UPDATE dependency_operation_members
		   SET state              = ?,
		       refusal            = ?,
		       plan_id            = CASE WHEN ? <> '' THEN ? ELSE plan_id END,
		       acquisition_id     = CASE WHEN ? <> '' THEN ? ELSE acquisition_id END,
		       execution_id       = CASE WHEN ? <> '' THEN ? ELSE execution_id END,
		       target_provider_id = CASE WHEN ? <> '' THEN ? ELSE target_provider_id END,
		       updated_at         = ?
		 WHERE operation_id = ? AND dependent_name = ?`,
		string(update.State), string(update.Refusal),
		update.PlanID, update.PlanID,
		update.AcquisitionID, update.AcquisitionID,
		update.ExecutionID, update.ExecutionID,
		update.TargetProviderID, update.TargetProviderID,
		formatTime(now.UTC()),
		update.OperationID, domain.NormaliseContainerName(update.Dependent))
	if err != nil {
		return fmt.Errorf("advance dependency member: %w", AsError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("advance dependency member: %w", AsError(err))
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// MembersForDependent returns the outstanding rebinds naming one container.
//
// The per-container read the detail view needs. Indexed, so it is a point
// lookup rather than a scan.
func (r *DependencyOperationRepository) MembersForDependent(
	ctx context.Context,
	dependent string,
) ([]domain.DependencyMember, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT operation_id, dependent_name, provider_name, source,
		       expected_provider_id, target_provider_id,
		       plan_id, acquisition_id, execution_id,
		       state, refusal, created_at, updated_at
		  FROM dependency_operation_members
		 WHERE dependent_name = ?
		 ORDER BY operation_id
		 LIMIT ?`, domain.NormaliseContainerName(dependent), maxDependencyMembers)
	if err != nil {
		return nil, fmt.Errorf("query dependency members for container: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	members := make([]domain.DependencyMember, 0, 4)
	for rows.Next() {
		member, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, AsError(err)
	}
	return members, nil
}

// Recent returns the most recently touched operations, concluded or not.
//
// # Why Open() is not enough for a summary view
//
// An operation whose provider succeeded and one of whose rebinds failed is
// CONCLUDED, as failed -- and it is precisely the state an operator most needs
// to see, because the provider and the successful rebinds are still in place and
// HarborMaster does not roll a dependency group backward. `Open()` excludes it
// by design, so a view built on that alone would report a tidy estate the moment
// the thing worth reporting finished.
//
// Two queries whatever the result size, like Open: the member read joins against
// the same bounded id selection rather than an IN list built from the first
// result.
func (r *DependencyOperationRepository) Recent(
	ctx context.Context,
	limit int,
) ([]domain.DependencyOperation, error) {
	if limit <= 0 || limit > maxRecentOperations {
		limit = maxRecentOperations
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT operation_id, provider_name, provider_plan_id, provider_execution_id,
		       state, failure, requested_by_user, requested_by_name,
		       created_at, updated_at, completed_at
		  FROM dependency_operations
		 ORDER BY updated_at DESC, operation_id
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query recent dependency operations: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	operations := make([]domain.DependencyOperation, 0, 8)
	index := make(map[string]int, 8)
	for rows.Next() {
		operation, err := r.scanOperationRows(rows)
		if err != nil {
			return nil, err
		}
		index[operation.OperationID] = len(operations)
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, AsError(err)
	}
	if len(operations) == 0 {
		return operations, nil
	}

	memberRows, err := r.db.QueryContext(ctx, `
		SELECT m.operation_id, m.dependent_name, m.provider_name, m.source,
		       m.expected_provider_id, m.target_provider_id,
		       m.plan_id, m.acquisition_id, m.execution_id,
		       m.state, m.refusal, m.created_at, m.updated_at
		  FROM dependency_operation_members m
		  JOIN (SELECT operation_id
		          FROM dependency_operations
		         ORDER BY updated_at DESC, operation_id
		         LIMIT ?) recent ON recent.operation_id = m.operation_id
		 ORDER BY m.operation_id, m.dependent_name`, limit)
	if err != nil {
		return nil, fmt.Errorf("query recent dependency members: %w", AsError(err))
	}
	defer func() { _ = memberRows.Close() }()

	for memberRows.Next() {
		member, err := scanMember(memberRows)
		if err != nil {
			return nil, err
		}
		at, known := index[member.OperationID]
		if !known {
			continue
		}
		operations[at].Members = append(operations[at].Members, member)
	}
	if err := memberRows.Err(); err != nil {
		return nil, AsError(err)
	}
	return operations, nil
}

// maxRecentOperations bounds the summary listing.
const maxRecentOperations = 50

// UnsettledMembers returns every reattachment that has not reached VERIFIED.
//
// # Why one query rather than one per container
//
// The container list needs to know, for a page of up to two hundred rows,
// which of them owe a reattachment. Asking per container is an N+1 that grows
// with the estate; asking for the whole outstanding set is ONE indexed query
// whose result is bounded by how much work is actually in flight, which on any
// healthy host is nothing.
//
// The predicate is `state <> verified` rather than a list of the failure
// states, deliberately: a member state this build does not recognise is
// reported as outstanding rather than skipped. Reporting a rebind that turns
// out to be finished is a false alarm; skipping one that is not is a container
// silently detached from its provider.
//
// A member that FAILED stays here, because HarborMaster never retries a
// reattachment on its own. It is a standing statement about the host until a
// person acts, which is exactly what the attention model should say.
func (r *DependencyOperationRepository) UnsettledMembers(
	ctx context.Context,
	limit int,
) ([]domain.DependencyMember, error) {
	if limit <= 0 || limit > maxUnsettledMembers {
		limit = maxUnsettledMembers
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT operation_id, dependent_name, provider_name, source,
		       expected_provider_id, target_provider_id,
		       plan_id, acquisition_id, execution_id,
		       state, refusal, created_at, updated_at
		  FROM dependency_operation_members
		 WHERE state <> ?
		 ORDER BY updated_at DESC, dependent_name
		 LIMIT ?`, string(domain.MemberVerified), limit)
	if err != nil {
		return nil, fmt.Errorf("query outstanding dependency members: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	members := make([]domain.DependencyMember, 0, 8)
	for rows.Next() {
		member, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, AsError(err)
	}
	return members, nil
}

// maxUnsettledMembers bounds the outstanding-reattachment sweep.
//
// Generous relative to reality -- a host with more than this many unfinished
// reattachments has a much larger problem than a truncated list -- and small
// enough that rendering a container page can never become an unbounded read.
const maxUnsettledMembers = 500

// ------------------------------------------------------------- scanning --

func (r *DependencyOperationRepository) membersFor(
	ctx context.Context,
	operationID string,
) ([]domain.DependencyMember, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT operation_id, dependent_name, provider_name, source,
		       expected_provider_id, target_provider_id,
		       plan_id, acquisition_id, execution_id,
		       state, refusal, created_at, updated_at
		  FROM dependency_operation_members
		 WHERE operation_id = ?
		 ORDER BY dependent_name
		 LIMIT ?`, operationID, maxDependencyMembers)
	if err != nil {
		return nil, fmt.Errorf("query dependency members: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	members := make([]domain.DependencyMember, 0, 4)
	for rows.Next() {
		member, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, AsError(err)
	}
	return members, nil
}

func (r *DependencyOperationRepository) scanOperation(row rowScanner) (domain.DependencyOperation, error) {
	operation, err := scanOperationInto(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DependencyOperation{}, ErrNotFound
	}
	if err != nil {
		return domain.DependencyOperation{}, fmt.Errorf("read dependency operation: %w", AsError(err))
	}
	return operation, nil
}

func (r *DependencyOperationRepository) scanOperationRows(rows *sql.Rows) (domain.DependencyOperation, error) {
	operation, err := scanOperationInto(rows)
	if err != nil {
		return domain.DependencyOperation{}, fmt.Errorf("scan dependency operation: %w", AsError(err))
	}
	return operation, nil
}

func scanOperationInto(row rowScanner) (domain.DependencyOperation, error) {
	var (
		operation   domain.DependencyOperation
		state       string
		failure     string
		createdAt   string
		updatedAt   string
		completedAt sql.NullString
	)
	if err := row.Scan(&operation.OperationID, &operation.Provider,
		&operation.ProviderPlanID, &operation.ProviderExecutionID,
		&state, &failure,
		&operation.RequestedBy.UserID, &operation.RequestedBy.Username,
		&createdAt, &updatedAt, &completedAt); err != nil {
		return domain.DependencyOperation{}, err
	}

	operation.State = domain.DependencyOperationState(state)
	operation.Failure = domain.DependencyOperationFailure(failure)
	if parsed, err := parseTime(createdAt); err == nil {
		operation.CreatedAt = parsed
	}
	if parsed, err := parseTime(updatedAt); err == nil {
		operation.UpdatedAt = parsed
	}
	if completedAt.Valid {
		if parsed, err := parseTime(completedAt.String); err == nil {
			operation.CompletedAt = &parsed
		}
	}
	return operation, nil
}

func scanMember(rows *sql.Rows) (domain.DependencyMember, error) {
	var (
		member    domain.DependencyMember
		source    string
		state     string
		refusal   string
		createdAt string
		updatedAt string
	)
	if err := rows.Scan(&member.OperationID, &member.Dependent, &member.Provider, &source,
		&member.ExpectedProviderID, &member.TargetProviderID,
		&member.PlanID, &member.AcquisitionID, &member.ExecutionID,
		&state, &refusal, &createdAt, &updatedAt); err != nil {
		return domain.DependencyMember{}, fmt.Errorf("scan dependency member: %w", AsError(err))
	}

	member.Source = domain.DependencySource(source)
	member.State = domain.DependencyMemberState(state)
	member.Refusal = domain.RebindRefusal(refusal)
	if parsed, err := parseTime(createdAt); err == nil {
		member.CreatedAt = parsed
	}
	if parsed, err := parseTime(updatedAt); err == nil {
		member.UpdatedAt = parsed
	}
	return member, nil
}
