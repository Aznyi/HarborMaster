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

// AuditRepository owns the immutable security audit log.
//
// # Immutable means there is no UPDATE
//
// This file contains exactly one INSERT, several SELECTs, and one bounded
// DELETE for retention. There is no method that modifies a row, and an
// architecture test asserts the package contains no UPDATE against
// audit_events. An audit trail whose rows can be edited proves nothing.
//
// Retention DELETE is the one exception, and it is a different operation from
// editing: removing an old row wholesale is visible as a gap, while changing
// one is not. The cutoff is configured, bounded, and never removes the most
// recent security events.
//
// # Nothing attacker-influenced reaches a column
//
// Every string field is either a closed vocabulary, an identifier HarborMaster
// generated, or a bounded value passed through SanitiseDisplayText. A test puts
// a known secret and a log-forgery payload through every field and asserts
// neither survives.
type AuditRepository struct {
	db *sql.DB
}

const selectAuditColumns = `
	SELECT a.id, a.event_id, a.action, a.outcome,
	       a.actor_user_id, a.actor_username, a.actor_role, a.actor_session_id,
	       a.target_type, a.target_id, a.target_name,
	       a.request_id, a.client_addr, a.reason, a.occurred_at
	FROM audit_events a`

// Record appends one audit event.
//
// # Why this never returns a fatal error to its caller
//
// It does return an error, and every caller logs it -- but no caller ABORTS on
// it. That is deliberate and worth stating: if writing the audit row failed the
// action, then filling the disk would become a way to disable HarborMaster, and
// a failed audit write on a logout would leave the operator logged in.
//
// The trade is that a lost audit row is possible. It is mitigated by the row
// being written in the same transaction as the action wherever the action has
// one, and by the write being tiny and unconditional everywhere else.
func (r *AuditRepository) Record(ctx context.Context, event domain.AuditEvent, now time.Time) error {
	prepared := prepareAuditEvent(event, now)

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO audit_events
			(event_id, action, outcome,
			 actor_user_id, actor_username, actor_role, actor_session_id,
			 target_type, target_id, target_name,
			 request_id, client_addr, reason, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		prepared.EventID, string(prepared.Action), string(prepared.Outcome),
		prepared.ActorUserID, prepared.ActorUsername, string(prepared.ActorRole),
		prepared.ActorSessionID,
		string(prepared.TargetType), prepared.TargetID, prepared.TargetName,
		prepared.RequestID, prepared.ClientAddr, prepared.Reason,
		formatTime(prepared.OccurredAt.UTC()))
	if err != nil {
		return fmt.Errorf("record audit event: %w", AsError(err))
	}
	return nil
}

// RecordAuditTx appends an audit event inside a caller's transaction.
//
// Used where the action itself is transactional -- creating a user, changing a
// role -- so the record and the change commit or roll back together. An audit
// row describing a change that was rolled back would be worse than no row.
func RecordAuditTx(ctx context.Context, tx *sql.Tx, event domain.AuditEvent, now time.Time) error {
	prepared := prepareAuditEvent(event, now)

	_, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events
			(event_id, action, outcome,
			 actor_user_id, actor_username, actor_role, actor_session_id,
			 target_type, target_id, target_name,
			 request_id, client_addr, reason, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		prepared.EventID, string(prepared.Action), string(prepared.Outcome),
		prepared.ActorUserID, prepared.ActorUsername, string(prepared.ActorRole),
		prepared.ActorSessionID,
		string(prepared.TargetType), prepared.TargetID, prepared.TargetName,
		prepared.RequestID, prepared.ClientAddr, prepared.Reason,
		formatTime(prepared.OccurredAt.UTC()))
	if err != nil {
		return fmt.Errorf("record audit event: %w", AsError(err))
	}
	return nil
}

// prepareAuditEvent fills defaults and bounds every string field.
//
// The single choke point through which every audit row passes. Bounding here
// rather than at each of the ~30 call sites is what makes "no unbounded
// attacker text reaches this table" a property of one function rather than a
// convention thirty places have to remember.
func prepareAuditEvent(event domain.AuditEvent, now time.Time) domain.AuditEvent {
	if event.EventID == "" {
		event.EventID = domain.NewAuditEventID()
	}
	if event.Outcome == "" {
		// An unset outcome is a programming error. Recording it as FAILED
		// rather than succeeded is the fail-closed reading: an audit page that
		// under-reports success is merely unhelpful, one that under-reports
		// failure is misleading.
		event.Outcome = domain.AuditFailed
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = now
	}

	event.ActorUsername = domain.SanitiseDisplayText(event.ActorUsername, domain.MaxAuditActorBytes)
	event.TargetID = domain.SanitiseDisplayText(event.TargetID, domain.MaxAuditTargetIDBytes)
	event.TargetName = domain.SanitiseDisplayText(event.TargetName, domain.MaxAuditTargetIDBytes)
	event.ClientAddr = domain.SanitiseDisplayText(event.ClientAddr, domain.MaxAuditAddrBytes)
	event.Reason = domain.SanitiseDisplayText(event.Reason, domain.MaxAuditReasonBytes)
	// The request id is generated from crypto/rand and hex-encoded, so it needs
	// no sanitising -- but it is bounded anyway, because a value that reaches a
	// column should not depend on a property established in another file.
	event.RequestID = domain.SanitiseDisplayText(event.RequestID, 64)

	return event
}

// --------------------------------------------------------------- reading --

// AuditFilter narrows an audit listing.
//
// Every field is a closed vocabulary or an identifier the API layer validated.
// None of them becomes SQL text.
type AuditFilter struct {
	Actions     []domain.AuditAction
	Outcomes    []domain.AuditOutcome
	ActorUserID string
	TargetType  domain.AuditTargetType
	TargetID    string
	// SecurityOnly restricts to authentication, authorization, user
	// administration, and bootstrap events.
	SecurityOnly bool
	// Since bounds the window.
	Since time.Time

	Page Page
}

// List returns a page of audit events, newest first.
//
// Newest first, unlike most histories here: an administrator opening this page
// is asking "what just happened", and the answer is at the top.
func (r *AuditRepository) List(ctx context.Context, filter AuditFilter) ([]domain.AuditEvent, int, error) {
	where, args := auditWhere(filter)

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_events a`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit events: %w", AsError(err))
	}

	page := filter.Page.normalise()
	rows, err := r.db.QueryContext(ctx,
		selectAuditColumns+where+` ORDER BY a.id DESC LIMIT ? OFFSET ?`,
		append(args, page.Limit, page.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query audit events: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	found, err := scanAuditEvents(rows)
	if err != nil {
		return nil, 0, err
	}
	return found, total, nil
}

// Summary computes the aggregate the audit page renders.
//
// Bounded to a window rather than the whole table: "how many failed logins,
// ever" is not a question anyone asks, and computing it would scan an
// arbitrarily large history on every page load.
func (r *AuditRepository) Summary(
	ctx context.Context,
	window time.Duration,
	now time.Time,
) (domain.AuditSummary, error) {
	summary := domain.AuditSummary{
		ByAction:    make(map[domain.AuditAction]int),
		ByOutcome:   make(map[domain.AuditOutcome]int),
		WindowHours: int(window.Hours()),
	}
	since := formatTime(now.Add(-window).UTC())

	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_events`).Scan(&summary.Total); err != nil {
		return summary, fmt.Errorf("count audit events: %w", AsError(err))
	}

	actionRows, err := r.db.QueryContext(ctx,
		`SELECT action, COUNT(*) FROM audit_events WHERE occurred_at >= ? GROUP BY action`,
		since)
	if err != nil {
		return summary, fmt.Errorf("summarise audit actions: %w", AsError(err))
	}
	if err := scanCountsInto(actionRows, func(key string, count int) {
		action := domain.AuditAction(key)
		summary.ByAction[action] = count
		if action.Privileged() {
			summary.PrivilegedActions += count
		}
	}); err != nil {
		return summary, err
	}

	outcomeRows, err := r.db.QueryContext(ctx,
		`SELECT outcome, COUNT(*) FROM audit_events WHERE occurred_at >= ? GROUP BY outcome`,
		since)
	if err != nil {
		return summary, fmt.Errorf("summarise audit outcomes: %w", AsError(err))
	}
	if err := scanCountsInto(outcomeRows, func(key string, count int) {
		summary.ByOutcome[domain.AuditOutcome(key)] = count
		if domain.AuditOutcome(key) == domain.AuditDenied {
			summary.DeniedActions += count
		}
	}); err != nil {
		return summary, err
	}

	summary.FailedLogins = summary.ByAction[domain.AuditLoginFailed] +
		summary.ByAction[domain.AuditLoginRateLimited]

	var last sql.NullString
	if err := r.db.QueryRowContext(ctx,
		`SELECT MAX(occurred_at) FROM audit_events`).Scan(&last); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return summary, fmt.Errorf("read last audit event: %w", AsError(err))
	}
	if last.Valid && last.String != "" {
		parsed, parseErr := parseTime(last.String)
		if parseErr != nil {
			return summary, parseErr
		}
		summary.LastEventAt = &parsed
	}
	return summary, nil
}

// RecentFailuresFor counts recent failed authentications for a client address.
//
// Feeds the per-address throttle. Counted from the audit log rather than from a
// separate table because the log already has the data and a second table would
// be a second thing to keep consistent -- and because a throttle that reads the
// same record an administrator reads cannot silently disagree with it.
func (r *AuditRepository) RecentFailuresFor(
	ctx context.Context,
	clientAddr string,
	since time.Time,
) (int, error) {
	if clientAddr == "" || clientAddr == "unknown" {
		return 0, nil
	}

	var count int
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM audit_events
		WHERE client_addr = ?
		  AND action IN ('auth.login.failed', 'auth.login.rateLimited')
		  AND occurred_at >= ?`,
		clientAddr, formatTime(since.UTC())).Scan(&count); err != nil {
		return 0, fmt.Errorf("count recent failures: %w", AsError(err))
	}
	return count, nil
}

// Prune removes audit events older than the cutoff.
//
// # Why security events are kept longer
//
// Two retentions, not one. An inventory refresh from six months ago is noise;
// a failed login from six months ago is the first entry in a story. The caller
// passes both cutoffs and the statement applies the right one per row.
//
// Bounded batches, so a long history cannot hold the single SQLite writer.
func (r *AuditRepository) Prune(
	ctx context.Context,
	operationalCutoff time.Time,
	securityCutoff time.Time,
	batch int,
) (int64, error) {
	if batch < 1 {
		batch = 500
	}

	result, err := r.db.ExecContext(ctx, `
		DELETE FROM audit_events
		WHERE id IN (
			SELECT id FROM audit_events
			WHERE (
			        action LIKE 'auth.%' OR action LIKE 'user.%' OR action LIKE 'bootstrap.%'
			      ) AND occurred_at < ?
			   OR NOT (
			        action LIKE 'auth.%' OR action LIKE 'user.%' OR action LIKE 'bootstrap.%'
			      ) AND occurred_at < ?
			LIMIT ?
		)`, formatTime(securityCutoff.UTC()), formatTime(operationalCutoff.UTC()), batch)
	if err != nil {
		return 0, fmt.Errorf("prune audit events: %w", AsError(err))
	}

	pruned, _ := result.RowsAffected()
	return pruned, nil
}

// ---------------------------------------------------------------- helpers --

// auditWhere builds the filter clause. Every value is bound; only the
// placeholder RUN length varies with the input.
func auditWhere(filter AuditFilter) (string, []any) {
	clauses := make([]string, 0, 6)
	args := make([]any, 0, 8)

	if len(filter.Actions) > 0 {
		clauses = append(clauses, "a.action IN ("+placeholders(len(filter.Actions))+")")
		for _, action := range filter.Actions {
			args = append(args, string(action))
		}
	}
	if len(filter.Outcomes) > 0 {
		clauses = append(clauses, "a.outcome IN ("+placeholders(len(filter.Outcomes))+")")
		for _, outcome := range filter.Outcomes {
			args = append(args, string(outcome))
		}
	}
	if filter.ActorUserID != "" {
		clauses = append(clauses, "a.actor_user_id = ?")
		args = append(args, filter.ActorUserID)
	}
	if filter.TargetType != "" {
		clauses = append(clauses, "a.target_type = ?")
		args = append(args, string(filter.TargetType))
	}
	if filter.TargetID != "" {
		clauses = append(clauses, "a.target_id = ?")
		args = append(args, filter.TargetID)
	}
	if filter.SecurityOnly {
		// Prefix literals, not caller text.
		clauses = append(clauses,
			"(a.action LIKE 'auth.%' OR a.action LIKE 'user.%' OR a.action LIKE 'bootstrap.%')")
	}
	if !filter.Since.IsZero() {
		clauses = append(clauses, "a.occurred_at >= ?")
		args = append(args, formatTime(filter.Since.UTC()))
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func scanAuditEvents(rows *sql.Rows) ([]domain.AuditEvent, error) {
	out := make([]domain.AuditEvent, 0, 16)

	for rows.Next() {
		var (
			event      domain.AuditEvent
			action     string
			outcome    string
			role       string
			targetType string
			occurred   string
		)
		if err := rows.Scan(&event.ID, &event.EventID, &action, &outcome,
			&event.ActorUserID, &event.ActorUsername, &role, &event.ActorSessionID,
			&targetType, &event.TargetID, &event.TargetName,
			&event.RequestID, &event.ClientAddr, &event.Reason, &occurred); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}

		event.Action = domain.AuditAction(action)
		event.Outcome = domain.AuditOutcome(outcome)
		event.ActorRole = domain.Role(role)
		event.TargetType = domain.AuditTargetType(targetType)

		parsed, err := parseTime(occurred)
		if err != nil {
			return nil, err
		}
		event.OccurredAt = parsed

		out = append(out, event)
	}
	return out, rows.Err()
}
