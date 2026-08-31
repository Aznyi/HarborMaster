package store

import (
	"context"
	"fmt"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Container attention evidence, gathered for a page of containers.
//
// # The cost rule this file exists to obey
//
// A container list row now shows what HarborMaster knows about the container:
// whether an update exists, whether an approval is held, whether automation
// gave up, whether findings are open, whether anything is followed at all, and
// whether the row is a workload or a piece of preserved evidence. Those facts
// live in six tables owned by six subsystems.
//
// The obvious implementation asks each subsystem about each row, which turns
// one HTTP request into six times the page size in queries. The Phase 10
// rollback-eligibility defect was exactly that shape and it is the precedent
// here: a read-heavy page must not amplify.
//
// So this file runs a FIXED NUMBER OF QUERIES -- ten -- whatever the page
// size. Each takes the page's identifiers as bound parameters and returns at
// most one row per container. `TestAttentionCostDoesNotGrowWithPageSize` counts
// them.
//
// # Why the placeholders are safe
//
// The IN lists are built from `len(keys)` question marks and nothing else. No
// identifier, no caller string, and no column name is ever concatenated into
// the SQL; every value travels as a bound parameter. The list length is
// bounded by the page size, which the API normalises before this is reached.

// maxAttentionKeys bounds an IN list.
//
// Well above any page the API will serve (200) and well below SQLite's
// variable limit. A caller asking for more is refused rather than silently
// truncated: a truncated evidence lookup would render some rows as "not
// checked" when they had in fact been checked, which is the one direction this
// model must never fail in.
const maxAttentionKeys = 500

// ErrTooManyAttentionKeys is returned rather than a partial answer.
var ErrTooManyAttentionKeys = fmt.Errorf(
	"attention lookup accepts at most %d containers", maxAttentionKeys)

// ContainerKey identifies one row of the page.
//
// Both fields, because the subsystems disagree about what identifies a
// container: plans, violations and drift key on the container ID, while the
// lineage record, the pause table and the automation decisions key on the
// NAME -- deliberately, because a recreation gives the workload a new ID and
// those three have to survive it.
type ContainerKey struct {
	ID   string
	Name string
	// ImageRef is the reference the container DECLARED, exactly as the daemon
	// reports it -- "nginx:1.27.0-alpine", not the canonical form.
	//
	// Carried because image intelligence is keyed by the CANONICAL reference
	// and the two cannot be joined in SQL. Migration 0015 records what happens
	// when somebody tries: `container_count` matched containers.image_ref
	// against image_intel.reference, those never match, and every image
	// reported zero containers affected. The mapping is a domain function, so
	// it is applied here in Go rather than guessed at in a query.
	//
	// Empty is fine and means "no intelligence for this row", which asserts
	// nothing.
	ImageRef string
}

// Attention returns evidence for the given containers, keyed by container ID.
//
// A container with no evidence gets a zero value, which the domain assessment
// reads as "not checked" rather than "nothing to do".
func (r *ContainerRepository) Attention(
	ctx context.Context,
	keys []ContainerKey,
) (map[string]domain.ContainerEvidence, error) {
	evidence := make(map[string]domain.ContainerEvidence, len(keys))
	if len(keys) == 0 {
		return evidence, nil
	}
	if len(keys) > maxAttentionKeys {
		return nil, ErrTooManyAttentionKeys
	}

	ids := make([]any, 0, len(keys))
	names := make([]any, 0, len(keys))
	byName := make(map[string][]string, len(keys))
	// Canonical image reference -> the rows running it. Many containers share
	// one image, so this is a set rather than a list: the IN clause must not
	// repeat a reference fifty times because fifty containers run it.
	references := make([]any, 0, len(keys))
	byReference := make(map[string][]string, len(keys))
	for _, key := range keys {
		evidence[key.ID] = domain.ContainerEvidence{}
		ids = append(ids, key.ID)

		// The one place raw becomes canonical. A reference NormalizeImageRef
		// refuses has no intelligence record to find, which is the same state
		// as having no record at all and is left as the zero value.
		if key.ImageRef != "" {
			if reference, err := domain.NormalizeImageRef(key.ImageRef); err == nil {
				if _, seen := byReference[reference.Canonical]; !seen {
					references = append(references, reference.Canonical)
				}
				byReference[reference.Canonical] = append(
					byReference[reference.Canonical], key.ID)
			}
		}

		if key.Name == "" {
			continue
		}
		names = append(names, key.Name)
		byName[key.Name] = append(byName[key.Name], key.ID)
	}

	// Applied to every container sharing a name. Two live containers cannot,
	// but a preserved one and its workload can differ only by suffix, and a
	// name-keyed fact belongs to whichever rows carry that name.
	forName := func(name string, apply func(*domain.ContainerEvidence)) {
		for _, id := range byName[name] {
			row := evidence[id]
			apply(&row)
			evidence[id] = row
		}
	}
	// Applied to every container running the image. One intelligence record
	// answers for all of them, which is the whole reason this is one query.
	forReference := func(reference string, apply func(*domain.ContainerEvidence)) {
		for _, id := range byReference[reference] {
			row := evidence[id]
			apply(&row)
			evidence[id] = row
		}
	}
	forID := func(id string, apply func(*domain.ContainerEvidence)) {
		row, known := evidence[id]
		if !known {
			return
		}
		apply(&row)
		evidence[id] = row
	}

	for _, gather := range []func() error{
		func() error { return r.gatherPlans(ctx, ids, forID) },
		func() error { return r.gatherApprovals(ctx, names, forName) },
		func() error { return r.gatherPauses(ctx, names, forName) },
		func() error { return r.gatherViolations(ctx, ids, forID) },
		func() error { return r.gatherDrift(ctx, ids, forID) },
		func() error { return r.gatherLineage(ctx, names, forName) },
		func() error { return r.gatherPreserved(ctx, names, forName) },
		func() error { return r.gatherLastUpdate(ctx, names, forName) },
		func() error { return r.gatherLastRollback(ctx, names, forName) },
		func() error { return r.gatherImageIntel(ctx, references, forReference) },
	} {
		if err := gather(); err != nil {
			return nil, err
		}
	}

	// The name-only classification is the LAST word and the weakest one: it
	// applies only where no record claimed the container, so a real execution
	// or rollback record always wins.
	for _, key := range keys {
		row := evidence[key.ID]
		if row.Preserved == domain.PreservedNone {
			row.Preserved = domain.ClassifyPreservedName(key.Name)
			evidence[key.ID] = row
		}
	}
	return evidence, nil
}

// gatherPlans reads the CURRENT plan per container.
//
// `MAX(id)` per container is the same definition PlanRepository.Summary uses
// for "current". Doing it in the subquery rather than filtering afterwards is
// what keeps this one query for the whole page.
func (r *ContainerRepository) gatherPlans(
	ctx context.Context, ids []any, forID func(string, func(*domain.ContainerEvidence)),
) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT container_id, update_type, recommendation, proposed_image
		  FROM change_plans
		 WHERE id IN (
		       SELECT MAX(id) FROM change_plans
		        WHERE container_id IN (`+placeholders(len(ids))+`)
		        GROUP BY container_id)`, ids...)
	if err != nil {
		return fmt.Errorf("read current plans: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id, updateType, recommendation, proposed string
		if err := rows.Scan(&id, &updateType, &recommendation, &proposed); err != nil {
			return fmt.Errorf("scan current plan: %w", AsError(err))
		}
		forID(id, func(row *domain.ContainerEvidence) {
			row.PlanKnown = true
			row.UpdateType = domain.UpdateType(updateType)
			row.Recommendation = domain.Recommendation(recommendation)
			row.ProposedImage = proposed
		})
	}
	return rows.Err()
}

// gatherImageIntel reads the registry comparison behind each row.
//
// # Why a list row needs this at all
//
// A ChangePlan means "HarborMaster is proposing a change". The planner writes
// none when it compares an image and finds nothing newer, because a plan
// recording a non-event is not a plan -- so the settled, successful, entirely
// ordinary case left no trace in the one subsystem the row was reading, and
// every current container reported "not checked".
//
// The evidence exists; it lives on the image intelligence record, which is
// where the registry comparison happens. This reads it for the page in ONE
// query keyed by canonical reference, so a hundred containers on one image cost
// one row rather than a hundred lookups.
//
// It does not make anything actionable. UpdateNone and UpdateUnknown are both
// refused by UpdateStrategy.Permits, so nothing here can cause an update; it
// changes only what a row is allowed to SAY.
func (r *ContainerRepository) gatherImageIntel(
	ctx context.Context,
	references []any,
	forReference func(string, func(*domain.ContainerEvidence)),
) error {
	if len(references) == 0 {
		return nil
	}
	// The shared column list and the shared scanner, not a third hand-written
	// SELECT over this table. Two used to exist and they drifted -- see
	// gatherIntel in plan_repository.go.
	rows, err := r.db.QueryContext(ctx,
		selectImageIntelColumns+`
		WHERE i.reference IN (`+placeholders(len(references))+`)`, references...)
	if err != nil {
		return fmt.Errorf("read image intelligence: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	records, err := scanImageIntel(rows)
	if err != nil {
		return err
	}
	for _, record := range records {
		forReference(record.Reference, func(row *domain.ContainerEvidence) {
			// ComparisonSettled is the domain's own predicate, and the SAME one
			// the planner applies. A row that has never been successfully
			// compared sets nothing, so its zero value continues to assert
			// nothing at all.
			if !record.ComparisonSettled() {
				return
			}
			row.CheckSettled = true
			row.CheckedUpdate = record.ObservedUpdate()
			row.CheckStatus = record.Status
			row.LastSuccessAt = record.LastSuccessAt
		})
	}
	return rows.Err()
}

// gatherApprovals reads which containers the LATEST pass is holding.
//
// Restricted to the newest run for the same reason the approvals queue and the
// dashboard counter are: every pass re-asks the same question, and an older
// pass's held decision is an answer to a question nobody is still asking.
func (r *ContainerRepository) gatherApprovals(
	ctx context.Context, names []any, forName func(string, func(*domain.ContainerEvidence)),
) error {
	if len(names) == 0 {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT container_name
		  FROM automation_decisions
		 WHERE verdict = 'awaitingApproval'
		   AND run_id = (SELECT run_id FROM automation_runs ORDER BY id DESC LIMIT 1)
		   AND container_name IN (`+placeholders(len(names))+`)`, names...)
	if err != nil {
		return fmt.Errorf("read held approvals: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan held approval: %w", AsError(err))
		}
		forName(name, func(row *domain.ContainerEvidence) { row.AwaitingApproval = true })
	}
	return rows.Err()
}

// gatherPauses reads which containers automation has stopped trying for.
//
// `acknowledged_at IS NULL` is the same definition ListPauses uses for active:
// an acknowledged pause is one somebody has already looked at.
func (r *ContainerRepository) gatherPauses(
	ctx context.Context, names []any, forName func(string, func(*domain.ContainerEvidence)),
) error {
	if len(names) == 0 {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT container_name
		  FROM automation_pauses
		 WHERE acknowledged_at IS NULL
		   AND container_name IN (`+placeholders(len(names))+`)`, names...)
	if err != nil {
		return fmt.Errorf("read automation pauses: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan automation pause: %w", AsError(err))
		}
		forName(name, func(row *domain.ContainerEvidence) { row.AutomationPaused = true })
	}
	return rows.Err()
}

// gatherViolations counts open policy findings and remembers the worst.
//
// Grouped by severity as well as container so the worst one can be picked in
// Go against the domain's own ranking, rather than by teaching SQL an ordering
// that would then exist in two places.
func (r *ContainerRepository) gatherViolations(
	ctx context.Context, ids []any, forID func(string, func(*domain.ContainerEvidence)),
) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT container_id, severity, COUNT(*)
		  FROM policy_violations
		 WHERE status <> 'resolved'
		   AND container_id IN (`+placeholders(len(ids))+`)
		 GROUP BY container_id, severity`, ids...)
	if err != nil {
		return fmt.Errorf("read open violations: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id       string
			severity string
			count    int
		)
		if err := rows.Scan(&id, &severity, &count); err != nil {
			return fmt.Errorf("scan open violation: %w", AsError(err))
		}
		forID(id, func(row *domain.ContainerEvidence) {
			row.OpenViolations += count
			if worseSeverity(domain.PolicySeverity(severity), row.HighestSeverity) {
				row.HighestSeverity = domain.PolicySeverity(severity)
			}
		})
	}
	return rows.Err()
}

// worseSeverity reports whether candidate outranks current.
//
// An empty current is beaten by anything; an unrecognised candidate beats
// nothing, so a severity this build does not know about cannot be presented as
// the worst finding on a container.
func worseSeverity(candidate, current domain.PolicySeverity) bool {
	rank := func(severity domain.PolicySeverity) int {
		for index, known := range domain.PolicySeverities {
			if known == severity {
				return index
			}
		}
		return len(domain.PolicySeverities)
	}
	return rank(candidate) < rank(current)
}

// gatherDrift counts open configuration drift per container.
func (r *ContainerRepository) gatherDrift(
	ctx context.Context, ids []any, forID func(string, func(*domain.ContainerEvidence)),
) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT container_id, COUNT(*)
		  FROM drift_records
		 WHERE status <> 'resolved'
		   AND container_id IN (`+placeholders(len(ids))+`)
		 GROUP BY container_id`, ids...)
	if err != nil {
		return fmt.Errorf("read open drift: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id    string
			count int
		)
		if err := rows.Scan(&id, &count); err != nil {
			return fmt.Errorf("scan open drift: %w", AsError(err))
		}
		forID(id, func(row *domain.ContainerEvidence) { row.OpenDrift = count })
	}
	return rows.Err()
}

// gatherLineage reads whether a container follows anything.
//
// The presence of a row is itself the answer to "has HarborMaster looked",
// which is why `LineageKnown` is set here and not inferred from the state
// being non-empty.
func (r *ContainerRepository) gatherLineage(
	ctx context.Context, names []any, forName func(string, func(*domain.ContainerEvidence)),
) error {
	if len(names) == 0 {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT container_name, state, tracking_familiar, tracking_reference
		  FROM image_lineage
		 WHERE container_name IN (`+placeholders(len(names))+`)`, names...)
	if err != nil {
		return fmt.Errorf("read image lineage: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name, state, familiar, reference string
		if err := rows.Scan(&name, &state, &familiar, &reference); err != nil {
			return fmt.Errorf("scan image lineage: %w", AsError(err))
		}
		forName(name, func(row *domain.ContainerEvidence) {
			row.LineageKnown = true
			row.Tracked = domain.LineageState(state) == domain.LineageTracked
			// The familiar form is what an operator recognises; the canonical
			// reference is the fallback so a row is never blank when something
			// IS being followed.
			if familiar != "" {
				row.TrackingReference = familiar
			} else {
				row.TrackingReference = reference
			}
		})
	}
	return rows.Err()
}

// gatherPreserved marks containers HarborMaster itself parked.
//
// Read from the names HarborMaster DERIVED AND RECORDED, not from the shape of
// the name in front of us. That distinction is the whole point: a name-shaped
// guess can be wrong about an operator's own container, and this is the signal
// the default listing is allowed to act on.
//
// One query over each of the two record types. Both are small relative to the
// inventory and both are indexed on the columns being matched.
func (r *ContainerRepository) gatherPreserved(
	ctx context.Context, names []any, forName func(string, func(*domain.ContainerEvidence)),
) error {
	if len(names) == 0 {
		return nil
	}

	// Executions park the ORIGINAL under .hm-old- and quarantine a FAILED
	// replacement under .hm-failed-. The two mean different things to an
	// operator, so they are distinguished rather than merged.
	list := placeholders(len(names))
	doubled := make([]any, 0, len(names)*2)
	doubled = append(doubled, names...)
	doubled = append(doubled, names...)

	rows, err := r.db.QueryContext(ctx, `
		SELECT container_name, parked_name, quarantine_name
		  FROM executions
		 WHERE parked_name IN (`+list+`) OR quarantine_name IN (`+list+`)`, doubled...)
	if err != nil {
		return fmt.Errorf("read parked containers: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var workload, parked, quarantined string
		if err := rows.Scan(&workload, &parked, &quarantined); err != nil {
			return fmt.Errorf("scan parked container: %w", AsError(err))
		}
		if parked != "" {
			forName(parked, func(row *domain.ContainerEvidence) {
				row.Preserved = domain.PreservedOriginal
				row.PreservedFor = workload
			})
		}
		if quarantined != "" {
			forName(quarantined, func(row *domain.ContainerEvidence) {
				row.Preserved = domain.PreservedFailed
				row.PreservedFor = workload
			})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	return r.gatherRolledBack(ctx, names, forName)
}

// gatherRolledBack marks replacements a rollback moved aside.
//
// Kept separate from the quarantine kind on purpose: a replacement backed out
// by a rollback may have been perfectly healthy, and telling an operator it
// "failed" would be a different and wrong story.
func (r *ContainerRepository) gatherRolledBack(
	ctx context.Context, names []any, forName func(string, func(*domain.ContainerEvidence)),
) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT container_name, replacement_parked_name
		  FROM rollbacks
		 WHERE replacement_parked_name <> ''
		   AND replacement_parked_name IN (`+placeholders(len(names))+`)`, names...)
	if err != nil {
		return fmt.Errorf("read rolled-back containers: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var workload, parked string
		if err := rows.Scan(&workload, &parked); err != nil {
			return fmt.Errorf("scan rolled-back container: %w", AsError(err))
		}
		forName(parked, func(row *domain.ContainerEvidence) {
			row.Preserved = domain.PreservedRolledBack
			row.PreservedFor = workload
		})
	}
	return rows.Err()
}

// preservedNameSet is the sub-select naming every container HarborMaster
// RECORDED itself parking.
//
// A fixed SQL fragment with no parameters, so the exclusion costs one
// correlated sub-select rather than an unbounded list of names travelling
// through Go and back as bind variables. The tables it reads hold one row per
// update and per rollback, not one per container.
//
// Only RECORDED names appear here. A container whose name merely looks like
// one of HarborMaster's is never excluded, because an operator who named a
// container that way themselves would watch it vanish from their own
// inventory and have no way to know why.
const preservedNameSet = `
	SELECT parked_name FROM executions WHERE parked_name <> ''
	 UNION SELECT quarantine_name FROM executions WHERE quarantine_name <> ''
	 UNION SELECT replacement_parked_name FROM rollbacks
	  WHERE replacement_parked_name <> ''`

// gatherLastUpdate reads the most recent recreation per container.
//
// Keyed on the NAME rather than the container id, deliberately: a recreation
// gives the workload a new id, so an id-keyed lookup would report "HarborMaster
// has never updated this" about the very container it had just updated.
//
// One row per container via MAX(id), the same shape the plan lookup uses, so
// this stays one query for the page.
func (r *ContainerRepository) gatherLastUpdate(
	ctx context.Context, names []any, forName func(string, func(*domain.ContainerEvidence)),
) error {
	if len(names) == 0 {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT container_name, execution_id, state, failure, checkpoint,
		       COALESCE(completed_at, requested_at)
		  FROM executions
		 WHERE id IN (
		       SELECT MAX(id) FROM executions
		        WHERE container_name IN (`+placeholders(len(names))+`)
		        GROUP BY container_name)`, names...)
	if err != nil {
		return fmt.Errorf("read last update: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name, id, state, failure, checkpoint, at string
		if err := rows.Scan(&name, &id, &state, &failure, &checkpoint, &at); err != nil {
			return fmt.Errorf("scan last update: %w", AsError(err))
		}
		outcome := domain.ActionOutcome{
			ID: id, State: state, Failure: failure, At: at,
			// The same definition the list endpoint uses: a failure that
			// stopped after it had begun changing the host, and before the
			// original was removed, is one an operator has to settle.
			NeedsAttention: state == "failed" && checkpoint != "" &&
				checkpoint != "originalRemoved",
		}
		forName(name, func(row *domain.ContainerEvidence) { row.LastUpdate = &outcome })
	}
	return rows.Err()
}

// gatherLastRollback reads the most recent rollback per container.
func (r *ContainerRepository) gatherLastRollback(
	ctx context.Context, names []any, forName func(string, func(*domain.ContainerEvidence)),
) error {
	if len(names) == 0 {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT container_name, rollback_id, state, failure,
		       COALESCE(completed_at, requested_at)
		  FROM rollbacks
		 WHERE id IN (
		       SELECT MAX(id) FROM rollbacks
		        WHERE container_name IN (`+placeholders(len(names))+`)
		        GROUP BY container_name)`, names...)
	if err != nil {
		return fmt.Errorf("read last rollback: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name, id, state, failure, at string
		if err := rows.Scan(&name, &id, &state, &failure, &at); err != nil {
			return fmt.Errorf("scan last rollback: %w", AsError(err))
		}
		outcome := domain.ActionOutcome{
			ID: id, State: state, Failure: failure, At: at,
			// A rollback that failed at preflight changed nothing; one that
			// failed later may have left the host part-way.
			NeedsAttention: state == "failed" && failure != "" && failure != "preflight",
		}
		forName(name, func(row *domain.ContainerEvidence) { row.LastRollback = &outcome })
	}
	return rows.Err()
}
