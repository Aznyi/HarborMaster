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

// PlanRepository owns persistence for change plans, and gathers the inputs the
// planner assesses.
//
// # Why the gathering lives here
//
// A plan combines six data sources. Reading them one container at a time would
// be the N+1 pattern six times over: a ten-thousand-container estate would issue
// sixty thousand round trips against a single SQLite connection.
//
// So the planner asks for a BATCH and this file answers with a handful of
// grouped queries -- five per batch, regardless of batch size. That is the whole
// reason the gathering is a repository concern rather than a service one: only
// the repository can turn "these five hundred containers" into five statements.
//
// # Bounded reads
//
// Every list is paginated, every sort field comes from an allowlist, and the
// summary is a handful of aggregates rather than a list the caller counts.
type PlanRepository struct {
	db *sql.DB
}

// planSortFields is the allowlist of sortable columns.
//
// An allowlist rather than validation-by-rejection: the value becomes part of
// the SQL text, so the only safe design is one where caller input SELECTS from
// a fixed set of literals rather than contributing to it.
var planSortFields = map[string]string{
	"generatedAt":    "p.generated_at",
	"risk":           "p.risk_score",
	"band":           planBandRankSQL,
	"recommendation": planRecommendationRankSQL,
	"container":      "p.container_name",
	"update":         planUpdateRankSQL,
	"id":             "p.id",
}

// The rank expressions order by how much a value matters rather than by its
// spelling. Built from literals only.
const (
	planBandRankSQL = `CASE p.risk_band ` +
		`WHEN 'critical' THEN 5 ` +
		`WHEN 'high' THEN 4 ` +
		`WHEN 'medium' THEN 3 ` +
		`WHEN 'low' THEN 2 ` +
		`WHEN 'veryLow' THEN 1 ` +
		`ELSE 0 END`

	planRecommendationRankSQL = `CASE p.recommendation ` +
		`WHEN 'notRecommended' THEN 5 ` +
		`WHEN 'manualReview' THEN 4 ` +
		`WHEN 'unknown' THEN 3 ` +
		`WHEN 'proceedWithCaution' THEN 2 ` +
		`WHEN 'proceed' THEN 1 ` +
		`ELSE 0 END`

	planUpdateRankSQL = `CASE p.update_type ` +
		`WHEN 'major' THEN 6 ` +
		`WHEN 'minor' THEN 5 ` +
		`WHEN 'patch' THEN 4 ` +
		`WHEN 'prerelease' THEN 3 ` +
		`WHEN 'digest' THEN 2 ` +
		`WHEN 'unknown' THEN 1 ` +
		`ELSE 0 END`
)

// ValidPlanSortField reports whether field names a sortable column.
func ValidPlanSortField(field string) bool {
	_, ok := planSortFields[field]
	return ok
}

// PlanFilter narrows a plan listing.
//
// Every field is a closed vocabulary or an exact identifier validated by the
// API layer before it arrives. None of them becomes SQL text.
type PlanFilter struct {
	ContainerID     string
	Bands           []domain.RiskBand
	Recommendations []domain.Recommendation
	Updates         []domain.UpdateType
	// CurrentOnly restricts to the newest plan per container, which is what a
	// dashboard means by "the plans". Superseded plans remain readable, but
	// showing them beside current ones would double-count the estate.
	CurrentOnly bool
	// MinRisk filters on the score. Zero disables it.
	MinRisk int

	Sort      string
	Ascending bool
	Page      Page
}

// superseded is derived rather than stored: a plan is superseded when a newer
// one exists for the same container. Storing it would mean writing to an
// immutable row.
const selectPlanColumns = `
	SELECT p.id, p.plan_id, p.container_id, p.container_name,
	       p.current_image, p.proposed_image, p.current_digest, p.proposed_digest,
	       p.update_type, p.snapshot_id, p.snapshot_available, p.restore_readiness,
	       p.drift_open, p.drift_max_severity, p.policy_open, p.policy_max_severity,
	       p.registry_status, p.registry_detail, p.proposed_published_at,
	       p.risk_score, p.risk_band, p.recommendation, p.summary, p.factors,
	       p.plan_version, p.planner_version, p.input_digest, p.generated_at,
	       EXISTS (SELECT 1 FROM change_plans newer
	                WHERE newer.container_id = p.container_id AND newer.id > p.id) AS superseded
	FROM change_plans p`

// ------------------------------------------------------------- gathering --

// PlanCandidate is one container the planner may produce a plan for.
type PlanCandidate struct {
	ContainerID   string
	ContainerName string
	ImageRef      string
	ImageID       string
}

// Candidates returns a page of present containers, oldest id first.
//
// Deterministically ordered, so a batched pass covers the estate exactly once
// rather than re-reading rows a concurrent insert shifted.
func (r *PlanRepository) Candidates(ctx context.Context, offset, limit int) ([]PlanCandidate, error) {
	if limit < 1 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, image_ref, image_id
		FROM containers
		WHERE present = 1
		ORDER BY id
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query plan candidates: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	candidates := make([]PlanCandidate, 0, limit)
	for rows.Next() {
		var candidate PlanCandidate
		if err := rows.Scan(&candidate.ContainerID, &candidate.ContainerName,
			&candidate.ImageRef, &candidate.ImageID); err != nil {
			return nil, fmt.Errorf("scan plan candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// CountCandidates reports how many containers a full pass would consider.
func (r *PlanRepository) CountCandidates(ctx context.Context) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM containers WHERE present = 1`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count plan candidates: %w", AsError(err))
	}
	return count, nil
}

// SeverityRollup is a per-container count with its worst severity.
type SeverityRollup struct {
	Open        int
	MaxSeverity string
}

// BaselineRollup is a container's newest snapshot and what readiness
// established about it.
type BaselineRollup struct {
	SnapshotID int64
	Readiness  domain.ReadinessStatus
}

// PlanBatchInputs is everything the planner needs for one batch of containers.
//
// Maps keyed by container id, or by image reference where the data is
// per-image. A container absent from a map simply has none of that data, which
// is a meaningful state the planner reads directly.
type PlanBatchInputs struct {
	Drift     map[string]SeverityRollup
	Policy    map[string]SeverityRollup
	Baselines map[string]BaselineRollup
	// Intel is keyed by CANONICAL image reference, because image intelligence
	// is per reference rather than per container -- a hundred containers on one
	// image share one entry.
	Intel map[string]domain.ImageIntel
	// Fingerprints holds the newest plan's input digest per container, which is
	// what duplicate suppression compares against.
	Fingerprints map[string]string
}

// GatherInputs reads every planner input for one batch of containers.
//
// FIVE QUERIES, whatever the batch size. Each is a grouped or filtered read
// over an indexed column with a bounded placeholder run, so a pass over ten
// thousand containers in batches of five hundred costs a hundred queries rather
// than sixty thousand.
func (r *PlanRepository) GatherInputs(
	ctx context.Context,
	containerIDs []string,
	imageRefs []string,
) (PlanBatchInputs, error) {
	inputs := PlanBatchInputs{
		Drift:        make(map[string]SeverityRollup, len(containerIDs)),
		Policy:       make(map[string]SeverityRollup, len(containerIDs)),
		Baselines:    make(map[string]BaselineRollup, len(containerIDs)),
		Intel:        make(map[string]domain.ImageIntel, len(imageRefs)),
		Fingerprints: make(map[string]string, len(containerIDs)),
	}
	if len(containerIDs) == 0 {
		return inputs, nil
	}

	args := make([]any, 0, len(containerIDs))
	for _, id := range containerIDs {
		args = append(args, id)
	}
	// A placeholder run generated from a slice LENGTH, never from content.
	in := placeholders(len(containerIDs))

	// Drift, rolled up per container. The MAX over a CASE rank turns "worst
	// severity" into one grouped pass rather than a query per container.
	if err := r.scanRollup(ctx, `
		SELECT container_id, COUNT(*), MAX(CASE severity
			WHEN 'critical' THEN 4 WHEN 'high' THEN 3
			WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END)
		FROM drift_records
		WHERE status <> 'resolved' AND container_id IN (`+in+`)
		GROUP BY container_id`, args, driftSeverityForRank, inputs.Drift); err != nil {
		return inputs, err
	}

	if err := r.scanRollup(ctx, `
		SELECT container_id, COUNT(*), MAX(CASE severity
			WHEN 'critical' THEN 4 WHEN 'high' THEN 3
			WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END)
		FROM policy_violations
		WHERE status <> 'resolved' AND container_id IN (`+in+`)
		GROUP BY container_id`, args, policySeverityForRank, inputs.Policy); err != nil {
		return inputs, err
	}

	// The newest snapshot per container, with its readiness. MAX(id) inside the
	// grouped subquery matches SnapshotRepository.Baseline's ordering.
	baselineRows, err := r.db.QueryContext(ctx, `
		SELECT s.container_id, s.id, s.readiness_status
		FROM snapshots s
		WHERE s.id IN (
			SELECT MAX(id) FROM snapshots
			WHERE container_id IN (`+in+`)
			GROUP BY container_id
		)`, args...)
	if err != nil {
		return inputs, fmt.Errorf("query plan baselines: %w", AsError(err))
	}
	if err := scanBaselines(baselineRows, inputs.Baselines); err != nil {
		return inputs, err
	}

	// The newest plan's fingerprint per container -- what duplicate suppression
	// compares against.
	fingerprintRows, err := r.db.QueryContext(ctx, `
		SELECT p.container_id, p.input_digest
		FROM change_plans p
		WHERE p.id IN (
			SELECT MAX(id) FROM change_plans
			WHERE container_id IN (`+in+`)
			GROUP BY container_id
		)`, args...)
	if err != nil {
		return inputs, fmt.Errorf("query plan fingerprints: %w", AsError(err))
	}
	if err := scanFingerprints(fingerprintRows, inputs.Fingerprints); err != nil {
		return inputs, err
	}

	if len(imageRefs) > 0 {
		if err := r.gatherIntel(ctx, imageRefs, inputs.Intel); err != nil {
			return inputs, err
		}
	}
	return inputs, nil
}

// gatherIntel reads image intelligence for a batch of references.
func (r *PlanRepository) gatherIntel(
	ctx context.Context,
	imageRefs []string,
	into map[string]domain.ImageIntel,
) error {
	args := make([]any, 0, len(imageRefs))
	for _, reference := range imageRefs {
		args = append(args, reference)
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT i.id, i.reference, i.familiar, i.registry_kind, i.registry,
		       i.namespace, i.repository, i.tag,
		       i.local_digest, i.remote_digest, i.pinned,
		       i.platform_os, i.platform_arch, i.platform_variant, i.platform_missing,
		       i.image_id,
		       i.update_type, i.latest_tag, i.update_reason,
		       i.check_status, i.status_detail,
		       i.first_seen_at, i.last_checked_at, i.last_success_at,
		       i.next_check_at, i.failure_count,
		       i.published_at, i.vendor, i.source, i.labels, i.etag,
		       (SELECT COUNT(*) FROM containers c
		         WHERE c.present = 1 AND c.image_ref = i.reference) AS container_count
		FROM image_intel i
		WHERE i.reference IN (`+placeholders(len(imageRefs))+`)`, args...)
	if err != nil {
		return fmt.Errorf("query plan image intel: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	records, err := scanImageIntel(rows)
	if err != nil {
		return err
	}
	for _, record := range records {
		into[record.Reference] = record
	}
	return nil
}

// scanRollup reads a three-column grouped result into a rollup map.
func (r *PlanRepository) scanRollup(
	ctx context.Context,
	query string,
	args []any,
	severityFor func(int) string,
	into map[string]SeverityRollup,
) error {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("query plan rollup: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			containerID string
			count       int
			rank        int
		)
		if err := rows.Scan(&containerID, &count, &rank); err != nil {
			return fmt.Errorf("scan plan rollup: %w", err)
		}
		into[containerID] = SeverityRollup{Open: count, MaxSeverity: severityFor(rank)}
	}
	return rows.Err()
}

func scanBaselines(rows *sql.Rows, into map[string]BaselineRollup) error {
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			containerID string
			snapshotID  int64
			readiness   string
		)
		if err := rows.Scan(&containerID, &snapshotID, &readiness); err != nil {
			return fmt.Errorf("scan plan baseline: %w", err)
		}
		into[containerID] = BaselineRollup{
			SnapshotID: snapshotID,
			Readiness:  domain.ReadinessStatus(readiness),
		}
	}
	return rows.Err()
}

func scanFingerprints(rows *sql.Rows, into map[string]string) error {
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var containerID, digest string
		if err := rows.Scan(&containerID, &digest); err != nil {
			return fmt.Errorf("scan plan fingerprint: %w", err)
		}
		into[containerID] = digest
	}
	return rows.Err()
}

// driftSeverityForRank and policySeverityForRank invert the CASE expressions
// above. Kept beside them so a change to one is visibly a change to both.
func driftSeverityForRank(rank int) string {
	switch rank {
	case 4:
		return string(domain.DriftSeverityCritical)
	case 3:
		return string(domain.DriftSeverityHigh)
	case 2:
		return string(domain.DriftSeverityMedium)
	case 1:
		return string(domain.DriftSeverityLow)
	default:
		return ""
	}
}

func policySeverityForRank(rank int) string {
	switch rank {
	case 4:
		return string(domain.PolicySeverityCritical)
	case 3:
		return string(domain.PolicySeverityHigh)
	case 2:
		return string(domain.PolicySeverityMedium)
	case 1:
		return string(domain.PolicySeverityLow)
	default:
		return ""
	}
}

// --------------------------------------------------------------- writing --

// InsertResult reports what a generation pass wrote.
type InsertResult struct {
	Inserted int
	// Unchanged counts plans not written because the fingerprint matched --
	// how much work duplicate suppression saved.
	Unchanged int
}

// InsertPlans writes a batch of plans in one transaction.
//
// A plan whose fingerprint already exists for its container is SKIPPED rather
// than written. The application check is the fast path; the unique index is
// what makes the guarantee hold when two passes race, and a conflict there is
// treated as "unchanged" rather than as an error -- another writer having
// already recorded the identical assessment is exactly the outcome wanted.
func (r *PlanRepository) InsertPlans(
	ctx context.Context,
	plans []domain.ChangePlan,
	now time.Time,
) (InsertResult, error) {
	var result InsertResult
	if len(plans) == 0 {
		return result, nil
	}
	stamp := formatTime(now.UTC())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("begin plan insert: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	for _, plan := range plans {
		inserted, err := insertPlan(ctx, tx, plan, stamp)
		if err != nil {
			return result, err
		}
		if inserted {
			result.Inserted++
		} else {
			result.Unchanged++
		}
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit plan insert: %w", AsError(err))
	}
	return result, nil
}

// insertPlan writes one plan, reporting whether it was new.
func insertPlan(ctx context.Context, tx *sql.Tx, plan domain.ChangePlan, stamp string) (bool, error) {
	// A backstop against the model and the storage diverging. A score outside
	// its band would show up as a dashboard that sorts wrongly rather than as
	// an error, so it is caught here rather than discovered later.
	if err := plan.Risk.Validate(); err != nil {
		return false, fmt.Errorf("plan %s: %w", plan.ContainerID, err)
	}

	factors, err := json.Marshal(plan.Risk.Factors)
	if err != nil {
		return false, fmt.Errorf("encode plan factors: %w", err)
	}

	var snapshotID any
	if plan.SnapshotID > 0 {
		snapshotID = plan.SnapshotID
	}
	available := 0
	if plan.SnapshotAvailable {
		available = 1
	}
	var published any
	if plan.ProposedPublishedAt != nil {
		published = formatTime(plan.ProposedPublishedAt.UTC())
	}

	generated := stamp
	if !plan.GeneratedAt.IsZero() {
		generated = formatTime(plan.GeneratedAt.UTC())
	}

	// ON CONFLICT DO NOTHING over the (container, fingerprint) index. The
	// RETURNING clause yields no row when the conflict fired, which is how a
	// racing duplicate is distinguished from a genuine insert without a second
	// query.
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO change_plans
			(plan_id, container_id, container_name,
			 current_image, proposed_image, current_digest, proposed_digest,
			 update_type, snapshot_id, snapshot_available, restore_readiness,
			 drift_open, drift_max_severity, policy_open, policy_max_severity,
			 registry_status, registry_detail, proposed_published_at,
			 risk_score, risk_band, recommendation, summary, factors,
			 plan_version, planner_version, input_digest, generated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (container_id, input_digest) DO NOTHING
		RETURNING id`,
		plan.PlanID, plan.ContainerID, plan.ContainerName,
		plan.CurrentImage, plan.ProposedImage, plan.CurrentDigest, plan.ProposedDigest,
		string(plan.UpdateType), snapshotID, available, string(plan.RestoreReadiness),
		plan.DriftOpen, string(plan.DriftMaxSeverity),
		plan.PolicyOpen, string(plan.PolicyMaxSeverity),
		string(plan.RegistryStatus), plan.RegistryDetail, published,
		plan.Risk.Score, string(plan.Risk.Band), string(plan.Risk.Recommendation),
		plan.Risk.Summary, string(factors),
		plan.PlanVersion, plan.PlannerVersion, plan.InputDigest, generated,
	).Scan(&id)

	if errors.Is(err, sql.ErrNoRows) {
		// The conflict fired: an identical assessment already exists.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("insert plan: %w", AsError(err))
	}
	return true, nil
}

// --------------------------------------------------------------- reading --

// List returns a page of plans and the total matching the filter.
func (r *PlanRepository) List(ctx context.Context, filter PlanFilter) ([]domain.ChangePlan, int, error) {
	where, args := planWhere(filter)

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM change_plans p`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count plans: %w", AsError(err))
	}

	column, ok := planSortFields[filter.Sort]
	if !ok {
		column = "p.generated_at"
	}
	direction := "DESC"
	if filter.Ascending {
		direction = "ASC"
	}

	page := filter.Page.normalise()
	// column and direction come from the allowlist above and a two-valued
	// switch; neither is caller text. Every VALUE is bound.
	query := selectPlanColumns + where +
		` ORDER BY ` + column + ` ` + direction + `, p.id ` + direction +
		` LIMIT ? OFFSET ?`

	rows, err := r.db.QueryContext(ctx, query, append(args, page.Limit, page.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query plans: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	plans, err := scanPlans(rows)
	if err != nil {
		return nil, 0, err
	}
	return plans, total, nil
}

// Get returns one plan by its immutable id.
func (r *PlanRepository) Get(ctx context.Context, planID string) (domain.ChangePlan, error) {
	rows, err := r.db.QueryContext(ctx, selectPlanColumns+` WHERE p.plan_id = ?`, planID)
	if err != nil {
		return domain.ChangePlan{}, fmt.Errorf("query plan: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	plans, err := scanPlans(rows)
	if err != nil {
		return domain.ChangePlan{}, err
	}
	if len(plans) == 0 {
		return domain.ChangePlan{}, ErrNotFound
	}
	return plans[0], nil
}

// Current returns the newest plan for one container.
func (r *PlanRepository) Current(ctx context.Context, containerID string) (domain.ChangePlan, error) {
	rows, err := r.db.QueryContext(ctx,
		selectPlanColumns+` WHERE p.container_id = ? ORDER BY p.id DESC LIMIT 1`, containerID)
	if err != nil {
		return domain.ChangePlan{}, fmt.Errorf("query current plan: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	plans, err := scanPlans(rows)
	if err != nil {
		return domain.ChangePlan{}, err
	}
	if len(plans) == 0 {
		return domain.ChangePlan{}, ErrNotFound
	}
	return plans[0], nil
}

// History returns a page of a container's plans, newest first.
//
// The reasoning timeline: how HarborMaster's assessment of one container has
// changed over time.
func (r *PlanRepository) History(
	ctx context.Context,
	containerID string,
	page Page,
) ([]domain.ChangePlan, int, error) {
	page = page.normalise()

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM change_plans WHERE container_id = ?`,
		containerID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count plan history: %w", AsError(err))
	}

	rows, err := r.db.QueryContext(ctx,
		selectPlanColumns+` WHERE p.container_id = ? ORDER BY p.id DESC LIMIT ? OFFSET ?`,
		containerID, page.Limit, page.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query plan history: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	plans, err := scanPlans(rows)
	if err != nil {
		return nil, 0, err
	}
	return plans, total, nil
}

// Summary computes the dashboard aggregate over CURRENT plans only.
//
// Current rather than all: a superseded plan describes a world that has moved
// on, and counting it beside the current one would double-count the estate.
func (r *PlanRepository) Summary(ctx context.Context) (domain.ChangePlanSummary, error) {
	summary := domain.ChangePlanSummary{
		ByBand:           make(map[domain.RiskBand]int, len(domain.RiskBands)),
		ByRecommendation: make(map[domain.Recommendation]int, len(domain.Recommendations)),
		ByUpdateType:     make(map[domain.UpdateType]int, len(domain.UpdateTypes)),
	}

	// The current-plan set, defined once and reused by every aggregate below.
	const currentPlans = `
		SELECT * FROM change_plans
		WHERE id IN (SELECT MAX(id) FROM change_plans GROUP BY container_id)`

	for _, group := range []struct {
		query string
		apply func(key string, count int)
	}{
		{
			`SELECT risk_band, COUNT(*) FROM (` + currentPlans + `) GROUP BY risk_band`,
			func(key string, count int) { summary.ByBand[domain.RiskBand(key)] = count },
		},
		{
			`SELECT recommendation, COUNT(*) FROM (` + currentPlans + `) GROUP BY recommendation`,
			func(key string, count int) {
				summary.ByRecommendation[domain.Recommendation(key)] = count
			},
		},
		{
			`SELECT update_type, COUNT(*) FROM (` + currentPlans + `) GROUP BY update_type`,
			func(key string, count int) { summary.ByUpdateType[domain.UpdateType(key)] = count },
		},
	} {
		if err := r.scanPlanCounts(ctx, group.query, group.apply); err != nil {
			return summary, err
		}
	}

	var (
		lastGenerated sql.NullString
		version       sql.NullString
	)
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT container_id), MAX(generated_at)
		FROM (`+currentPlans+`)`,
	).Scan(&summary.Plans, &summary.Containers, &lastGenerated); err != nil {
		return summary, fmt.Errorf("summarise plans: %w", AsError(err))
	}

	// The planner version the newest plan was built with, so a UI can say the
	// rule set changed rather than leaving an operator to wonder why every
	// score moved.
	if err := r.db.QueryRowContext(ctx,
		`SELECT planner_version FROM change_plans ORDER BY id DESC LIMIT 1`,
	).Scan(&version); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return summary, fmt.Errorf("read planner version: %w", AsError(err))
	}
	if version.Valid {
		summary.PlannerVersion = version.String
	}

	summary.Actionable = summary.ByRecommendation[domain.RecommendProceed] +
		summary.ByRecommendation[domain.RecommendCaution]
	summary.NeedsReview = summary.ByRecommendation[domain.RecommendManualReview]
	summary.Blocked = summary.ByRecommendation[domain.RecommendAgainst]
	summary.Undetermined = summary.ByRecommendation[domain.RecommendUnknown]

	if lastGenerated.Valid && lastGenerated.String != "" {
		parsed, err := parseTime(lastGenerated.String)
		if err != nil {
			return summary, err
		}
		summary.LastGeneratedAt = &parsed
	}
	return summary, nil
}

// PruneSuperseded deletes superseded plans older than the cutoff.
//
// CURRENT plans are never pruned, whatever their age: the newest plan for a
// container is the standing assessment, and deleting it would leave the
// container looking unplanned rather than unchanged.
func (r *PlanRepository) PruneSuperseded(ctx context.Context, cutoff time.Time, batch int) (int64, error) {
	if batch < 1 {
		batch = 500
	}
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM change_plans
		WHERE id IN (
			SELECT id FROM change_plans
			WHERE generated_at < ?
			  AND id NOT IN (SELECT MAX(id) FROM change_plans GROUP BY container_id)
			LIMIT ?
		)`, formatTime(cutoff.UTC()), batch)
	if err != nil {
		return 0, fmt.Errorf("prune superseded plans: %w", AsError(err))
	}

	// Reported for logging only; the deletion has already happened.
	removed, _ := result.RowsAffected()
	return removed, nil
}

// PruneOrphans removes plans for containers no longer present.
func (r *PlanRepository) PruneOrphans(ctx context.Context, batch int) (int64, error) {
	if batch < 1 {
		batch = 500
	}
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM change_plans
		WHERE id IN (
			SELECT p.id FROM change_plans p
			WHERE NOT EXISTS (
				SELECT 1 FROM containers c
				WHERE c.present = 1 AND c.id = p.container_id
			)
			LIMIT ?
		)`, batch)
	if err != nil {
		return 0, fmt.Errorf("prune orphan plans: %w", AsError(err))
	}

	removed, _ := result.RowsAffected()
	return removed, nil
}

// ---------------------------------------------------------------- helpers --

func (r *PlanRepository) scanPlanCounts(ctx context.Context, query string, apply func(string, int)) error {
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("aggregate plans: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			key   string
			count int
		)
		if err := rows.Scan(&key, &count); err != nil {
			return fmt.Errorf("scan plan aggregate: %w", err)
		}
		apply(key, count)
	}
	return rows.Err()
}

// planWhere builds the filter clause. Every value is bound; only the
// placeholder RUN length varies with the input.
func planWhere(filter PlanFilter) (string, []any) {
	clauses := make([]string, 0, 6)
	args := make([]any, 0, 8)

	if filter.ContainerID != "" {
		clauses = append(clauses, "p.container_id = ?")
		args = append(args, filter.ContainerID)
	}
	if filter.CurrentOnly {
		clauses = append(clauses,
			"p.id IN (SELECT MAX(id) FROM change_plans GROUP BY container_id)")
	}
	if filter.MinRisk > 0 {
		clauses = append(clauses, "p.risk_score >= ?")
		args = append(args, filter.MinRisk)
	}

	if len(filter.Bands) > 0 {
		clauses = append(clauses, "p.risk_band IN ("+placeholders(len(filter.Bands))+")")
		for _, band := range filter.Bands {
			args = append(args, string(band))
		}
	}
	if len(filter.Recommendations) > 0 {
		clauses = append(clauses,
			"p.recommendation IN ("+placeholders(len(filter.Recommendations))+")")
		for _, recommendation := range filter.Recommendations {
			args = append(args, string(recommendation))
		}
	}
	if len(filter.Updates) > 0 {
		clauses = append(clauses, "p.update_type IN ("+placeholders(len(filter.Updates))+")")
		for _, update := range filter.Updates {
			args = append(args, string(update))
		}
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func scanPlans(rows *sql.Rows) ([]domain.ChangePlan, error) {
	plans := make([]domain.ChangePlan, 0, 16)

	for rows.Next() {
		var (
			plan           domain.ChangePlan
			snapshotID     sql.NullInt64
			available      int
			updateType     string
			readiness      string
			driftSeverity  string
			policySeverity string
			registryStatus string
			published      sql.NullString
			band           string
			recommendation string
			factors        string
			generated      string
			superseded     int
		)
		if err := rows.Scan(
			&plan.ID, &plan.PlanID, &plan.ContainerID, &plan.ContainerName,
			&plan.CurrentImage, &plan.ProposedImage, &plan.CurrentDigest, &plan.ProposedDigest,
			&updateType, &snapshotID, &available, &readiness,
			&plan.DriftOpen, &driftSeverity, &plan.PolicyOpen, &policySeverity,
			&registryStatus, &plan.RegistryDetail, &published,
			&plan.Risk.Score, &band, &recommendation, &plan.Risk.Summary, &factors,
			&plan.PlanVersion, &plan.PlannerVersion, &plan.InputDigest, &generated,
			&superseded,
		); err != nil {
			return nil, fmt.Errorf("scan plan: %w", err)
		}

		plan.UpdateType = domain.UpdateType(updateType)
		plan.SnapshotAvailable = available == 1
		plan.RestoreReadiness = domain.ReadinessStatus(readiness)
		plan.DriftMaxSeverity = domain.DriftSeverity(driftSeverity)
		plan.PolicyMaxSeverity = domain.PolicySeverity(policySeverity)
		plan.RegistryStatus = domain.CheckStatus(registryStatus)
		plan.Risk.Band = domain.RiskBand(band)
		plan.Risk.Recommendation = domain.Recommendation(recommendation)
		plan.Superseded = superseded == 1
		if snapshotID.Valid {
			plan.SnapshotID = snapshotID.Int64
		}

		// A factors column that cannot be decoded degrades to an empty
		// explanation rather than failing the whole listing: one malformed row
		// should cost its own reasoning, not the endpoint.
		if err := json.Unmarshal([]byte(factors), &plan.Risk.Factors); err != nil {
			plan.Risk.Factors = nil
		}

		var err error
		if plan.GeneratedAt, err = parseTime(generated); err != nil {
			return nil, err
		}
		plan.ProposedPublishedAt = scanOptionalTime(published)

		plans = append(plans, plan)
	}
	return plans, rows.Err()
}
