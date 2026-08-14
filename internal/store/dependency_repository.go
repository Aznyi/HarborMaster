package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Persistence for workload dependencies.
//
// # Two sources, one table
//
// OPERATOR relationships are stored. DISCOVERED ones are not: they are derived
// from the namespace projection on `containers` on every read, which is why
// this file has a query that returns namespace rows and no query that writes a
// discovered edge.
//
// The reasoning is in migration 0024 and worth restating in one line: a stored
// copy of a fact that changes on every recreation is a second thing that can be
// wrong about the host, and it would need its own invalidation, staleness gate,
// and post-execution reconciliation. Deriving costs one indexed query.
//
// # Every read is bounded
//
// The namespace sweep is capped at MaxAutomationTargets and the operator table
// at domain.MaxOperatorDependencies. Both report truncation rather than
// silently covering a prefix -- a partial graph can only ever make a container
// look more independent, and therefore safer, than it is.

// DependencyRepository owns workload dependency persistence.
type DependencyRepository struct {
	db *sql.DB
}

// NewDependencyRepository builds a DependencyRepository.
func NewDependencyRepository(db *sql.DB) *DependencyRepository {
	return &DependencyRepository{db: db}
}

// ErrDependencyExists reports that the ordered pair is already recorded.
var ErrDependencyExists = errors.New("that dependency relationship already exists")

// ErrDependencyTruncated reports that the estate exceeded a read bound.
//
// Returned rather than logged, because the caller's only safe response is to
// refuse to build a graph. A truncated dependency read is not a smaller answer;
// it is a WRONG one, in the direction that clears containers which should have
// been blocked.
var ErrDependencyTruncated = errors.New("this host has more containers than one dependency evaluation may consider")

// ------------------------------------------------------ derived discovery --

// NamespaceRows returns every present container's namespace facts.
//
// ONE query for the whole estate. The graph is built from this slice in memory,
// so evaluating a two-thousand container host costs one round trip rather than
// one per container -- and the index added by migration 0024 covers every
// column read here, so the table itself is never touched.
//
// The three mode strings are returned RAW. Parsing them is the domain's job:
// a column holding a half-interpreted value is a column two readers can
// disagree about.
func (r *DependencyRepository) NamespaceRows(ctx context.Context) ([]domain.ContainerNamespaceRow, error) {
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM containers WHERE present = 1`).Scan(&total); err != nil {
		return nil, fmt.Errorf("count dependency namespace rows: %w", AsError(err))
	}
	if total > MaxAutomationTargets {
		// Refused, not truncated. See ErrDependencyTruncated.
		return nil, ErrDependencyTruncated
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, network_mode, ipc_mode, pid_mode, namespaces_observed, last_seen_at
		  FROM containers
		 WHERE present = 1
		 ORDER BY id
		 LIMIT ?`, MaxAutomationTargets)
	if err != nil {
		return nil, fmt.Errorf("query dependency namespace rows: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	out := make([]domain.ContainerNamespaceRow, 0, 64)
	for rows.Next() {
		var (
			row      domain.ContainerNamespaceRow
			observed int
			lastSeen string
		)
		if err := rows.Scan(&row.ContainerID, &row.Name,
			&row.Modes.Network, &row.Modes.IPC, &row.Modes.PID,
			&observed, &lastSeen); err != nil {
			return nil, fmt.Errorf("scan dependency namespace row: %w", AsError(err))
		}
		row.Modes.Observed = observed == 1
		if parsed, err := parseTime(lastSeen); err == nil {
			row.ObservedAt = parsed
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, AsError(err)
	}
	return out, nil
}

// Endpoints returns what validation needs to know about every present
// container.
//
// One query, joined against labels in a second, exactly as AutomationTargets
// does. The labels are needed for the self-identity match: HarborMaster
// recognises its own container partly by label, and an endpoint validated
// without them could accept a relationship naming HarborMaster itself.
func (r *DependencyRepository) Endpoints(ctx context.Context) ([]domain.DependencyEndpoint, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, image_ref
		  FROM containers
		 WHERE present = 1
		 ORDER BY id
		 LIMIT ?`, MaxAutomationTargets)
	if err != nil {
		return nil, fmt.Errorf("query dependency endpoints: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	byID := make(map[string]int, 64)
	endpoints := make([]domain.DependencyEndpoint, 0, 64)
	for rows.Next() {
		var endpoint domain.DependencyEndpoint
		if err := rows.Scan(&endpoint.ContainerID, &endpoint.Name, &endpoint.ImageRef); err != nil {
			return nil, fmt.Errorf("scan dependency endpoint: %w", AsError(err))
		}
		endpoint.Present = true
		endpoint.Derived = domain.IsHarborMasterDerivedName(
			domain.NormaliseContainerName(endpoint.Name))
		byID[endpoint.ContainerID] = len(endpoints)
		endpoints = append(endpoints, endpoint)
	}
	if err := rows.Err(); err != nil {
		return nil, AsError(err)
	}
	if len(endpoints) == 0 {
		return endpoints, nil
	}

	labelRows, err := r.db.QueryContext(ctx, `
		SELECT l.container_id, l.key, l.value
		  FROM container_labels l
		  JOIN containers c ON c.id = l.container_id
		 WHERE c.present = 1
		 ORDER BY l.container_id, l.key
		 LIMIT ?`, MaxAutomationTargets*10)
	if err != nil {
		return nil, fmt.Errorf("query dependency endpoint labels: %w", AsError(err))
	}
	defer func() { _ = labelRows.Close() }()

	for labelRows.Next() {
		var containerID, key, value string
		if err := labelRows.Scan(&containerID, &key, &value); err != nil {
			return nil, fmt.Errorf("scan dependency endpoint label: %w", AsError(err))
		}
		index, ok := byID[containerID]
		if !ok {
			continue
		}
		if endpoints[index].Labels == nil {
			endpoints[index].Labels = make(map[string]string, 4)
		}
		endpoints[index].Labels[key] = value
	}
	if err := labelRows.Err(); err != nil {
		return nil, AsError(err)
	}
	return endpoints, nil
}

// ------------------------------------------------------------- operator --

// OperatorDependencies returns every stored relationship, sorted.
//
// Sorted in SQL AND again in the domain: the query's ORDER BY is what makes the
// read deterministic, and domain.SortDependencies is what makes the merged
// discovered-plus-operator list deterministic. Neither is redundant.
func (r *DependencyRepository) OperatorDependencies(ctx context.Context) ([]domain.WorkloadDependency, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT dependency_id, dependent_name, dependency_name,
		       created_at, created_by_user, created_by_name
		  FROM workload_dependencies
		 ORDER BY dependent_name, dependency_name
		 LIMIT ?`, domain.MaxOperatorDependencies)
	if err != nil {
		return nil, fmt.Errorf("query operator dependencies: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	out := make([]domain.WorkloadDependency, 0, 16)
	for rows.Next() {
		var (
			edge      domain.WorkloadDependency
			createdAt string
		)
		if err := rows.Scan(&edge.DependencyID, &edge.Dependent, &edge.Dependency,
			&createdAt, &edge.CreatedBy.UserID, &edge.CreatedBy.Username); err != nil {
			return nil, fmt.Errorf("scan operator dependency: %w", AsError(err))
		}
		edge.Source = domain.DependencyOperator
		if parsed, err := parseTime(createdAt); err == nil {
			edge.CreatedAt = parsed
		}
		out = append(out, edge)
	}
	if err := rows.Err(); err != nil {
		return nil, AsError(err)
	}
	return out, nil
}

// OperatorDependencyCount returns how many relationships are stored.
//
// Read before a create so the bound is enforced against the table rather than
// against whatever the caller happens to have loaded.
func (r *DependencyRepository) OperatorDependencyCount(ctx context.Context) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workload_dependencies`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count operator dependencies: %w", AsError(err))
	}
	return count, nil
}

// Get returns one operator relationship by its id.
func (r *DependencyRepository) Get(ctx context.Context, dependencyID string) (domain.WorkloadDependency, error) {
	// Shape-validated before it reaches the query. The query is parameterised
	// either way; this stops a caller probing the table with arbitrary strings.
	if !domain.ValidDependencyID(dependencyID) {
		return domain.WorkloadDependency{}, ErrNotFound
	}

	var (
		edge      domain.WorkloadDependency
		createdAt string
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT dependency_id, dependent_name, dependency_name,
		       created_at, created_by_user, created_by_name
		  FROM workload_dependencies
		 WHERE dependency_id = ?`, dependencyID).
		Scan(&edge.DependencyID, &edge.Dependent, &edge.Dependency,
			&createdAt, &edge.CreatedBy.UserID, &edge.CreatedBy.Username)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkloadDependency{}, ErrNotFound
	}
	if err != nil {
		return domain.WorkloadDependency{}, fmt.Errorf("read operator dependency: %w", AsError(err))
	}
	edge.Source = domain.DependencyOperator
	if parsed, err := parseTime(createdAt); err == nil {
		edge.CreatedAt = parsed
	}
	return edge, nil
}

// Create records an operator relationship.
//
// The relationship must ALREADY have passed domain.ValidateOperatorDependency.
// This method re-checks only what the database is the authority on -- the
// source and the uniqueness of the pair -- and refuses anything else by letting
// the schema's own constraints reject it.
func (r *DependencyRepository) Create(
	ctx context.Context,
	edge domain.WorkloadDependency,
	now time.Time,
) (domain.WorkloadDependency, error) {
	// A discovered source cannot be stored. Refused here as well as by the
	// CHECK constraint and by the domain validator: a row asserting a runtime
	// requirement the daemon does not enforce would make HarborMaster wait on,
	// or refuse, an update for a reason that is not true.
	if edge.Source != domain.DependencyOperator {
		return domain.WorkloadDependency{},
			fmt.Errorf("refusing to store a %s dependency: only operator relationships are stored", edge.Source)
	}
	if edge.DependencyID == "" {
		edge.DependencyID = domain.NewDependencyID()
	}
	edge.CreatedAt = now.UTC()

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO workload_dependencies
			(dependency_id, dependent_name, dependency_name, source,
			 created_at, created_by_user, created_by_name)
		VALUES (?, ?, ?, 'operator', ?, ?, ?)`,
		edge.DependencyID, edge.Dependent, edge.Dependency,
		formatTime(edge.CreatedAt), edge.CreatedBy.UserID, edge.CreatedBy.Username)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.WorkloadDependency{}, ErrDependencyExists
		}
		return domain.WorkloadDependency{}, fmt.Errorf("insert operator dependency: %w", AsError(err))
	}
	return edge, nil
}

// Delete removes an operator relationship.
//
// Returns ErrNotFound when nothing matched, so a caller cannot learn whether an
// id it guessed exists by observing a different outcome.
func (r *DependencyRepository) Delete(ctx context.Context, dependencyID string) error {
	if !domain.ValidDependencyID(dependencyID) {
		return ErrNotFound
	}

	result, err := r.db.ExecContext(ctx,
		`DELETE FROM workload_dependencies WHERE dependency_id = ?`, dependencyID)
	if err != nil {
		return fmt.Errorf("delete operator dependency: %w", AsError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete operator dependency: %w", AsError(err))
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}
