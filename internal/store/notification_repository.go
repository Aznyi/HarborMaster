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

// NotificationRepository owns destinations, rules, and the delivery record.
//
// # The one rule this file exists to enforce
//
// A destination's credential -- the webhook URL, the SMTP password -- is
// readable through exactly ONE method, Secret, and that method is called by the
// sender immediately before it sends. Every other read selects the public
// columns by name.
//
// There is deliberately no `SELECT *` anywhere in this file, and the column
// list is a constant rather than a string built at each call site: a query that
// accidentally selected the secrets table would have to be written on purpose.
type NotificationRepository struct {
	db *sql.DB
}

// Notification repository errors.
var (
	// ErrDestinationNameTaken reports a duplicate destination name.
	ErrDestinationNameTaken = errors.New("a notification destination with that name already exists")
	// ErrNotificationRuleNameTaken reports a duplicate rule name.
	ErrNotificationRuleNameTaken = errors.New("a notification rule with that name already exists")
	// ErrDestinationInUse reports an archive blocked by a rule that routes to it.
	ErrDestinationInUse = errors.New("a notification rule still routes to this destination")
)

// Bounds on what one query may load.
const (
	// maxActiveDestinations bounds the destination load one notification
	// performs. Past it, an estate has more destinations than anybody is
	// reading.
	maxActiveDestinations = 200
	// maxActiveNotificationRules bounds the rule load.
	maxActiveNotificationRules = 200
	// maxDeliveryPruneBatch bounds one retention pass.
	maxDeliveryPruneBatch = 1000
)

// ------------------------------------------------------------ destinations --

// The PUBLIC columns. Named explicitly, and deliberately excluding every column
// of notification_secrets: a read path that wanted a credential would have to
// name a different table.
const selectDestinationColumns = `
	SELECT id, destination_id, name, description, channel, enabled,
	       endpoint, title_prefix, email_to_json, email_from,
	       last_result, last_attempt_at, last_error, consecutive_failures,
	       archived, created_at, updated_at
	FROM notification_destinations`

// CreateDestination stores a destination and its credential in one transaction.
//
// Both or neither. A destination whose secret failed to write would be a row
// that looks configured and cannot send, and an operator would have no way to
// tell it apart from one whose endpoint is simply down.
func (r *NotificationRepository) CreateDestination(
	ctx context.Context,
	destination domain.NotificationDestination,
	secret domain.NotificationSecret,
	now time.Time,
) (domain.NotificationDestination, error) {
	recipients, err := json.Marshal(destination.EmailTo)
	if err != nil {
		return domain.NotificationDestination{}, fmt.Errorf("encode recipients: %w", err)
	}
	stamp := formatTime(now.UTC())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.NotificationDestination{}, fmt.Errorf("begin destination: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	var taken int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM notification_destinations
		 WHERE archived = 0 AND name = ?`, destination.Name).Scan(&taken); err != nil {
		return domain.NotificationDestination{}, fmt.Errorf("check destination name: %w", AsError(err))
	}
	if taken > 0 {
		return domain.NotificationDestination{}, ErrDestinationNameTaken
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO notification_destinations
			(destination_id, name, description, channel, enabled, endpoint,
			 title_prefix, email_to_json, email_from, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		destination.DestinationID, destination.Name, destination.Description,
		string(destination.Channel), boolToInt(destination.Enabled),
		destination.Endpoint, destination.TitlePrefix,
		string(recipients), destination.EmailFrom, stamp, stamp)
	if err != nil {
		return domain.NotificationDestination{}, fmt.Errorf("insert destination: %w", AsError(err))
	}

	if err := upsertSecret(ctx, tx, destination.DestinationID, secret, stamp); err != nil {
		return domain.NotificationDestination{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.NotificationDestination{}, fmt.Errorf("commit destination: %w", AsError(err))
	}

	id, _ := result.LastInsertId()
	destination.ID = id
	destination.CreatedAt = stamp
	destination.UpdatedAt = stamp
	return destination, nil
}

// DestinationChange carries the fields an edit may change.
//
// Each is a pointer so "not supplied" and "supplied as the zero value" stay
// distinguishable. Secret is separate and its own pointer: an edit that did not
// mention the URL must LEAVE IT ALONE, which is what lets an operator rename a
// destination without re-typing a credential they may not have kept.
type DestinationChange struct {
	Name        *string
	Description *string
	Enabled     *bool
	TitlePrefix *string
	EmailTo     *[]string
	EmailFrom   *string
	Endpoint    *string

	Secret *domain.NotificationSecret
}

// UpdateDestination applies a change.
func (r *NotificationRepository) UpdateDestination(
	ctx context.Context,
	destinationID string,
	change DestinationChange,
	now time.Time,
) (domain.NotificationDestination, error) {
	stamp := formatTime(now.UTC())

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.NotificationDestination{}, fmt.Errorf("begin destination edit: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanDestinationRow(tx.QueryRowContext(ctx,
		selectDestinationColumns+` WHERE destination_id = ?`, destinationID))
	if err != nil {
		return domain.NotificationDestination{}, err
	}
	if existing.Archived {
		// An archived destination is history. Editing one would change what a
		// delivery record appears to have been sent to.
		return domain.NotificationDestination{}, ErrNotFound
	}

	merged := existing
	if change.Name != nil {
		merged.Name = *change.Name
	}
	if change.Description != nil {
		merged.Description = *change.Description
	}
	if change.Enabled != nil {
		merged.Enabled = *change.Enabled
	}
	if change.TitlePrefix != nil {
		merged.TitlePrefix = *change.TitlePrefix
	}
	if change.EmailTo != nil {
		merged.EmailTo = *change.EmailTo
	}
	if change.EmailFrom != nil {
		merged.EmailFrom = *change.EmailFrom
	}
	if change.Endpoint != nil {
		merged.Endpoint = *change.Endpoint
	}

	if merged.Name != existing.Name {
		var taken int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM notification_destinations
			 WHERE archived = 0 AND name = ? AND destination_id <> ?`,
			merged.Name, destinationID).Scan(&taken); err != nil {
			return domain.NotificationDestination{}, fmt.Errorf("check destination name: %w", AsError(err))
		}
		if taken > 0 {
			return domain.NotificationDestination{}, ErrDestinationNameTaken
		}
	}

	recipients, err := json.Marshal(merged.EmailTo)
	if err != nil {
		return domain.NotificationDestination{}, fmt.Errorf("encode recipients: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE notification_destinations
		   SET name = ?, description = ?, enabled = ?, endpoint = ?,
		       title_prefix = ?, email_to_json = ?, email_from = ?, updated_at = ?
		 WHERE destination_id = ? AND archived = 0`,
		merged.Name, merged.Description, boolToInt(merged.Enabled), merged.Endpoint,
		merged.TitlePrefix, string(recipients), merged.EmailFrom, stamp,
		destinationID); err != nil {
		return domain.NotificationDestination{}, fmt.Errorf("update destination: %w", AsError(err))
	}

	// The credential is rewritten only when the edit supplied one.
	if change.Secret != nil {
		if err := upsertSecret(ctx, tx, destinationID, *change.Secret, stamp); err != nil {
			return domain.NotificationDestination{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.NotificationDestination{}, fmt.Errorf("commit destination edit: %w", AsError(err))
	}
	merged.UpdatedAt = stamp
	return merged, nil
}

// ArchiveDestination withdraws a destination.
//
// Refused while a rule still routes to it. The alternative -- archiving anyway
// and letting the rule fail at send time -- would turn a configuration mistake
// into a silent loss of notifications, which is the one failure mode this
// subsystem must not have.
func (r *NotificationRepository) ArchiveDestination(
	ctx context.Context,
	destinationID string,
	now time.Time,
) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin destination archive: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	// A rule's destinations are a JSON array, so the check is a LIKE against
	// the quoted id. The id is a generated, shape-validated token with no LIKE
	// metacharacters in its alphabet, so the pattern is exact in practice --
	// and it is bound rather than concatenated regardless.
	var routing int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM notification_rules
		 WHERE archived = 0
		   AND destinations_json LIKE '%' || ? || '%'`,
		`"`+destinationID+`"`).Scan(&routing); err != nil {
		return fmt.Errorf("check destination use: %w", AsError(err))
	}
	if routing > 0 {
		return ErrDestinationInUse
	}

	stamp := formatTime(now.UTC())
	result, err := tx.ExecContext(ctx, `
		UPDATE notification_destinations
		   SET archived = 1, archived_at = ?, enabled = 0, updated_at = ?
		 WHERE destination_id = ? AND archived = 0`,
		stamp, stamp, destinationID)
	if err != nil {
		return fmt.Errorf("archive destination: %w", AsError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("archive destination: %w", AsError(err))
	}
	if affected == 0 {
		return ErrNotFound
	}

	// The credential goes with it. An archived destination cannot send, so
	// keeping its URL would be keeping a credential for no reason -- and if it
	// is ever un-archived, re-entering it is the correct amount of friction.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM notification_secrets WHERE destination_id = ?`,
		destinationID); err != nil {
		return fmt.Errorf("clear destination secret: %w", AsError(err))
	}

	return tx.Commit()
}

// DestinationByID reads one destination's public record.
func (r *NotificationRepository) DestinationByID(
	ctx context.Context,
	destinationID string,
) (domain.NotificationDestination, error) {
	return scanDestinationRow(r.db.QueryRowContext(ctx,
		selectDestinationColumns+` WHERE destination_id = ?`, destinationID))
}

// ListDestinations returns a bounded page of public records.
func (r *NotificationRepository) ListDestinations(
	ctx context.Context,
	includeArchived bool,
	page Page,
) ([]domain.NotificationDestination, int, error) {
	page = page.normalise()

	clause := " WHERE archived = 0"
	if includeArchived {
		clause = ""
	}

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notification_destinations`+clause).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count destinations: %w", AsError(err))
	}

	rows, err := r.db.QueryContext(ctx, selectDestinationColumns+clause+`
		 ORDER BY name ASC, id ASC LIMIT ? OFFSET ?`, page.Limit, page.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list destinations: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	destinations := make([]domain.NotificationDestination, 0, page.Limit)
	for rows.Next() {
		destination, err := scanDestination(rows)
		if err != nil {
			return nil, 0, err
		}
		destinations = append(destinations, destination)
	}
	return destinations, total, rows.Err()
}

// ActiveDestinations returns every destination a notification may reach.
func (r *NotificationRepository) ActiveDestinations(
	ctx context.Context,
) ([]domain.NotificationDestination, error) {
	rows, err := r.db.QueryContext(ctx, selectDestinationColumns+`
		 WHERE archived = 0 AND enabled = 1
		 ORDER BY id ASC LIMIT ?`, maxActiveDestinations)
	if err != nil {
		return nil, fmt.Errorf("load active destinations: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	destinations := make([]domain.NotificationDestination, 0, 8)
	for rows.Next() {
		destination, err := scanDestination(rows)
		if err != nil {
			return nil, err
		}
		destinations = append(destinations, destination)
	}
	return destinations, rows.Err()
}

// Secret reads a destination's credential.
//
// # The only method in this package that returns one
//
// Called by the sender, immediately before it sends, and by nothing else. It is
// deliberately awkward to reach: the destination's public record does not carry
// it, no list returns it, and this method's name says exactly what it is for.
//
// A caller that has one must not log it, must not put it in an error, and must
// not carry it past the send. internal/notify's channels take it as a parameter
// that goes out of scope when Send returns.
func (r *NotificationRepository) Secret(
	ctx context.Context,
	destinationID string,
) (domain.NotificationSecret, error) {
	var secret domain.NotificationSecret
	err := r.db.QueryRowContext(ctx, `
		SELECT webhook_url, smtp_username, smtp_password
		  FROM notification_secrets
		 WHERE destination_id = ?`, destinationID).
		Scan(&secret.URL, &secret.SMTPUsername, &secret.SMTPPassword)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No credential is a valid state for an unauthenticated relay, and an
		// invalid one for a webhook. The caller decides which; the repository
		// reports what it found.
		return domain.NotificationSecret{}, nil
	case err != nil:
		// The error is wrapped WITHOUT the destination id. Everything else in
		// this file names the id in its error; this one does not, because a
		// failure here appears in a log line beside the word "secret" and there
		// is no reason to correlate the two.
		return domain.NotificationSecret{}, fmt.Errorf("read destination credential: %w", AsError(err))
	}
	return secret, nil
}

// RecordDestinationResult updates a destination's health.
//
// Denormalised onto the destination so a list can show "this is not working"
// without joining the delivery history, which is the number an operator most
// needs and the one they would otherwise never look for.
func (r *NotificationRepository) RecordDestinationResult(
	ctx context.Context,
	destinationID string,
	result domain.DeliveryResult,
	detail string,
	at time.Time,
) error {
	// A success resets the consecutive count; a failure increments it. Written
	// as one statement so two concurrent deliveries cannot both read the
	// pre-increment value.
	if result == domain.DeliverySucceeded {
		_, err := r.db.ExecContext(ctx, `
			UPDATE notification_destinations
			   SET last_result = ?, last_attempt_at = ?, last_error = '',
			       consecutive_failures = 0
			 WHERE destination_id = ?`,
			string(result), formatTime(at.UTC()), destinationID)
		if err != nil {
			return fmt.Errorf("record destination result: %w", AsError(err))
		}
		return nil
	}

	_, err := r.db.ExecContext(ctx, `
		UPDATE notification_destinations
		   SET last_result = ?, last_attempt_at = ?, last_error = ?,
		       consecutive_failures = consecutive_failures + 1
		 WHERE destination_id = ?`,
		string(result), formatTime(at.UTC()),
		domain.SanitiseDisplayText(detail, 500), destinationID)
	if err != nil {
		return fmt.Errorf("record destination result: %w", AsError(err))
	}
	return nil
}

// upsertSecret writes a destination's credential inside an open transaction.
func upsertSecret(
	ctx context.Context,
	tx *sql.Tx,
	destinationID string,
	secret domain.NotificationSecret,
	stamp string,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO notification_secrets
			(destination_id, webhook_url, smtp_username, smtp_password, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (destination_id) DO UPDATE SET
			webhook_url   = excluded.webhook_url,
			smtp_username = excluded.smtp_username,
			smtp_password = excluded.smtp_password,
			updated_at    = excluded.updated_at`,
		destinationID, secret.URL, secret.SMTPUsername, secret.SMTPPassword, stamp)
	if err != nil {
		// The error names neither the URL nor the password. AsError already
		// strips driver detail; this wrapping adds no value of its own.
		return fmt.Errorf("store destination credential: %w", AsError(err))
	}
	return nil
}

func scanDestinationRow(row *sql.Row) (domain.NotificationDestination, error) {
	destination, err := scanDestination(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.NotificationDestination{}, ErrNotFound
	}
	return destination, err
}

func scanDestination(scanner rowScanner) (domain.NotificationDestination, error) {
	var (
		destination   domain.NotificationDestination
		enabled       int
		archived      int
		recipients    string
		lastAttemptAt sql.NullString
	)
	err := scanner.Scan(
		&destination.ID, &destination.DestinationID, &destination.Name,
		&destination.Description, &destination.Channel, &enabled,
		&destination.Endpoint, &destination.TitlePrefix, &recipients,
		&destination.EmailFrom, &destination.LastResult, &lastAttemptAt,
		&destination.LastError, &destination.ConsecutiveFailures,
		&archived, &destination.CreatedAt, &destination.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NotificationDestination{}, err
		}
		return domain.NotificationDestination{}, fmt.Errorf("scan destination: %w", AsError(err))
	}
	destination.Enabled = enabled == 1
	destination.Archived = archived == 1

	if err := json.Unmarshal([]byte(recipients), &destination.EmailTo); err != nil {
		return domain.NotificationDestination{}, fmt.Errorf(
			"decode destination %s recipients: %w", destination.DestinationID, err)
	}
	if lastAttemptAt.Valid {
		parsed, err := parseTime(lastAttemptAt.String)
		if err != nil {
			return domain.NotificationDestination{}, err
		}
		destination.LastAttemptAt = &parsed
	}
	return destination, nil
}

// ------------------------------------------------------------------ rules --

const selectNotificationRuleColumns = `
	SELECT id, rule_id, name, enabled, events_json, minimum_severity,
	       destinations_json, cooldown_seconds, archived, created_at, updated_at
	FROM notification_rules`

// CreateRule stores a routing rule.
func (r *NotificationRepository) CreateRule(
	ctx context.Context,
	rule domain.NotificationRule,
	now time.Time,
) (domain.NotificationRule, error) {
	events, destinations, err := encodeRuleLists(rule)
	if err != nil {
		return domain.NotificationRule{}, err
	}
	stamp := formatTime(now.UTC())

	var taken int
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM notification_rules
		 WHERE archived = 0 AND name = ?`, rule.Name).Scan(&taken); err != nil {
		return domain.NotificationRule{}, fmt.Errorf("check rule name: %w", AsError(err))
	}
	if taken > 0 {
		return domain.NotificationRule{}, ErrNotificationRuleNameTaken
	}

	result, err := r.db.ExecContext(ctx, `
		INSERT INTO notification_rules
			(rule_id, name, enabled, events_json, minimum_severity,
			 destinations_json, cooldown_seconds, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rule.RuleID, rule.Name, boolToInt(rule.Enabled), events,
		string(rule.MinimumSeverity), destinations, rule.CooldownSeconds,
		stamp, stamp)
	if err != nil {
		return domain.NotificationRule{}, fmt.Errorf("insert rule: %w", AsError(err))
	}

	id, _ := result.LastInsertId()
	rule.ID = id
	rule.CreatedAt = stamp
	rule.UpdatedAt = stamp
	return rule, nil
}

// NotificationRuleChange carries the fields an edit may change.
type NotificationRuleChange struct {
	Name            *string
	Enabled         *bool
	Events          *[]domain.NotificationEvent
	MinimumSeverity *domain.NotificationSeverity
	Destinations    *[]string
	CooldownSeconds *int
}

// UpdateRule applies a change.
func (r *NotificationRepository) UpdateRule(
	ctx context.Context,
	ruleID string,
	change NotificationRuleChange,
	now time.Time,
) (domain.NotificationRule, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.NotificationRule{}, fmt.Errorf("begin rule edit: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanNotificationRuleRow(tx.QueryRowContext(ctx,
		selectNotificationRuleColumns+` WHERE rule_id = ?`, ruleID))
	if err != nil {
		return domain.NotificationRule{}, err
	}
	if existing.Archived {
		return domain.NotificationRule{}, ErrNotFound
	}

	merged := existing
	if change.Name != nil {
		merged.Name = *change.Name
	}
	if change.Enabled != nil {
		merged.Enabled = *change.Enabled
	}
	if change.Events != nil {
		merged.Events = *change.Events
	}
	if change.MinimumSeverity != nil {
		merged.MinimumSeverity = *change.MinimumSeverity
	}
	if change.Destinations != nil {
		merged.Destinations = *change.Destinations
	}
	if change.CooldownSeconds != nil {
		merged.CooldownSeconds = *change.CooldownSeconds
	}

	if merged.Name != existing.Name {
		var taken int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM notification_rules
			 WHERE archived = 0 AND name = ? AND rule_id <> ?`,
			merged.Name, ruleID).Scan(&taken); err != nil {
			return domain.NotificationRule{}, fmt.Errorf("check rule name: %w", AsError(err))
		}
		if taken > 0 {
			return domain.NotificationRule{}, ErrNotificationRuleNameTaken
		}
	}

	events, destinations, err := encodeRuleLists(merged)
	if err != nil {
		return domain.NotificationRule{}, err
	}
	stamp := formatTime(now.UTC())

	if _, err := tx.ExecContext(ctx, `
		UPDATE notification_rules
		   SET name = ?, enabled = ?, events_json = ?, minimum_severity = ?,
		       destinations_json = ?, cooldown_seconds = ?, updated_at = ?
		 WHERE rule_id = ? AND archived = 0`,
		merged.Name, boolToInt(merged.Enabled), events,
		string(merged.MinimumSeverity), destinations, merged.CooldownSeconds,
		stamp, ruleID); err != nil {
		return domain.NotificationRule{}, fmt.Errorf("update rule: %w", AsError(err))
	}

	if err := tx.Commit(); err != nil {
		return domain.NotificationRule{}, fmt.Errorf("commit rule edit: %w", AsError(err))
	}
	merged.UpdatedAt = stamp
	return merged, nil
}

// ArchiveRule withdraws a routing rule.
func (r *NotificationRepository) ArchiveRule(
	ctx context.Context,
	ruleID string,
	now time.Time,
) error {
	stamp := formatTime(now.UTC())
	result, err := r.db.ExecContext(ctx, `
		UPDATE notification_rules
		   SET archived = 1, archived_at = ?, enabled = 0, updated_at = ?
		 WHERE rule_id = ? AND archived = 0`, stamp, stamp, ruleID)
	if err != nil {
		return fmt.Errorf("archive rule: %w", AsError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("archive rule: %w", AsError(err))
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// RuleByID reads one rule.
func (r *NotificationRepository) RuleByID(
	ctx context.Context,
	ruleID string,
) (domain.NotificationRule, error) {
	return scanNotificationRuleRow(r.db.QueryRowContext(ctx,
		selectNotificationRuleColumns+` WHERE rule_id = ?`, ruleID))
}

// ListRules returns a bounded page.
func (r *NotificationRepository) ListRules(
	ctx context.Context,
	includeArchived bool,
	page Page,
) ([]domain.NotificationRule, int, error) {
	page = page.normalise()

	clause := " WHERE archived = 0"
	if includeArchived {
		clause = ""
	}

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notification_rules`+clause).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count rules: %w", AsError(err))
	}

	rows, err := r.db.QueryContext(ctx, selectNotificationRuleColumns+clause+`
		 ORDER BY name ASC, id ASC LIMIT ? OFFSET ?`, page.Limit, page.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list rules: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	rules := make([]domain.NotificationRule, 0, page.Limit)
	for rows.Next() {
		rule, err := scanNotificationRule(rows)
		if err != nil {
			return nil, 0, err
		}
		rules = append(rules, rule)
	}
	return rules, total, rows.Err()
}

// ActiveRules returns every rule that may route a notification.
func (r *NotificationRepository) ActiveRules(
	ctx context.Context,
) ([]domain.NotificationRule, error) {
	rows, err := r.db.QueryContext(ctx, selectNotificationRuleColumns+`
		 WHERE archived = 0 AND enabled = 1
		 ORDER BY id ASC LIMIT ?`, maxActiveNotificationRules)
	if err != nil {
		return nil, fmt.Errorf("load active rules: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	rules := make([]domain.NotificationRule, 0, 8)
	for rows.Next() {
		rule, err := scanNotificationRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

func encodeRuleLists(rule domain.NotificationRule) (string, string, error) {
	events, err := json.Marshal(rule.Events)
	if err != nil {
		return "", "", fmt.Errorf("encode rule events: %w", err)
	}
	destinations, err := json.Marshal(rule.Destinations)
	if err != nil {
		return "", "", fmt.Errorf("encode rule destinations: %w", err)
	}
	return string(events), string(destinations), nil
}

func scanNotificationRuleRow(row *sql.Row) (domain.NotificationRule, error) {
	rule, err := scanNotificationRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.NotificationRule{}, ErrNotFound
	}
	return rule, err
}

func scanNotificationRule(scanner rowScanner) (domain.NotificationRule, error) {
	var (
		rule         domain.NotificationRule
		enabled      int
		archived     int
		events       string
		destinations string
	)
	err := scanner.Scan(
		&rule.ID, &rule.RuleID, &rule.Name, &enabled, &events,
		&rule.MinimumSeverity, &destinations, &rule.CooldownSeconds,
		&archived, &rule.CreatedAt, &rule.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NotificationRule{}, err
		}
		return domain.NotificationRule{}, fmt.Errorf("scan rule: %w", AsError(err))
	}
	rule.Enabled = enabled == 1
	rule.Archived = archived == 1

	// A row this build cannot decode is refused rather than served with an
	// empty event list, which would be a rule that silently matched everything.
	if err := json.Unmarshal([]byte(events), &rule.Events); err != nil {
		return domain.NotificationRule{}, fmt.Errorf("decode rule %s events: %w", rule.RuleID, err)
	}
	if err := json.Unmarshal([]byte(destinations), &rule.Destinations); err != nil {
		return domain.NotificationRule{}, fmt.Errorf("decode rule %s destinations: %w", rule.RuleID, err)
	}
	return rule, nil
}

// CountNotificationConfiguration returns the destination and rule totals.
func (r *NotificationRepository) CountNotificationConfiguration(
	ctx context.Context,
) (destinations, rules, failing int, err error) {
	err = r.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM notification_destinations WHERE archived = 0),
			(SELECT COUNT(*) FROM notification_rules        WHERE archived = 0),
			(SELECT COUNT(*) FROM notification_destinations
			  WHERE archived = 0 AND last_result = 'failed')`).
		Scan(&destinations, &rules, &failing)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("count notification configuration: %w", AsError(err))
	}
	return destinations, rules, failing, nil
}

// ------------------------------------------------------------ deliveries --

const selectDeliveryColumns = `
	SELECT id, delivery_id, destination_id, destination_name, channel,
	       COALESCE(rule_id, ''), rule_name,
	       event, severity, title, body, container_name,
	       result, attempts, status_code, error, dedup_key,
	       queued_at, completed_at, next_attempt_at, duration_ms
	FROM notification_deliveries`

// RecordDelivery writes a delivery record.
func (r *NotificationRepository) RecordDelivery(
	ctx context.Context,
	delivery domain.NotificationDelivery,
) error {
	var ruleID any
	if delivery.RuleID != "" {
		ruleID = delivery.RuleID
	}
	var completedAt, nextAttemptAt any
	if delivery.CompletedAt != nil {
		completedAt = formatTime(delivery.CompletedAt.UTC())
	}
	if delivery.NextAttemptAt != nil {
		nextAttemptAt = formatTime(delivery.NextAttemptAt.UTC())
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO notification_deliveries
			(delivery_id, destination_id, destination_name, channel,
			 rule_id, rule_name, event, severity, title, body, container_name,
			 result, attempts, status_code, error, dedup_key,
			 queued_at, completed_at, next_attempt_at, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		delivery.DeliveryID, delivery.DestinationID, delivery.DestinationName,
		string(delivery.Channel), ruleID, delivery.RuleName,
		string(delivery.Event), string(delivery.Severity), delivery.Title,
		delivery.Body, delivery.ContainerName,
		string(delivery.Result), delivery.Attempts, delivery.StatusCode,
		delivery.Error, delivery.DedupKey,
		formatTime(delivery.QueuedAt.UTC()), completedAt, nextAttemptAt,
		delivery.DurationMs)
	if err != nil {
		return fmt.Errorf("record delivery: %w", AsError(err))
	}
	return nil
}

// CompleteDelivery records an attempt's outcome.
func (r *NotificationRepository) CompleteDelivery(
	ctx context.Context,
	deliveryID string,
	result domain.DeliveryResult,
	attempts, statusCode int,
	detail string,
	nextAttemptAt *time.Time,
	completedAt time.Time,
	durationMs int64,
) error {
	var (
		completed any
		next      any
	)
	if result.Terminal() {
		completed = formatTime(completedAt.UTC())
	}
	if nextAttemptAt != nil {
		next = formatTime(nextAttemptAt.UTC())
	}
	if durationMs < 0 {
		durationMs = 0
	}

	_, err := r.db.ExecContext(ctx, `
		UPDATE notification_deliveries
		   SET result = ?, attempts = ?, status_code = ?, error = ?,
		       completed_at = ?, next_attempt_at = ?, duration_ms = ?
		 WHERE delivery_id = ?`,
		string(result), attempts, statusCode,
		domain.SanitiseDisplayText(detail, 500),
		completed, next, durationMs, deliveryID)
	if err != nil {
		return fmt.Errorf("complete delivery: %w", AsError(err))
	}
	return nil
}

// DeliveryFilter narrows a delivery listing.
//
// Every field is a closed vocabulary or an identifier the API validated by
// shape. None becomes SQL text.
type DeliveryFilter struct {
	DestinationID string
	ContainerName string
	Results       []domain.DeliveryResult
	Events        []domain.NotificationEvent
	// FailedOnly restricts to the dead letter, which is the filter an operator
	// asking "what did I not get told about" needs.
	FailedOnly bool
	Page       Page
}

// ListDeliveries returns a bounded page, newest first.
func (r *NotificationRepository) ListDeliveries(
	ctx context.Context,
	filter DeliveryFilter,
) ([]domain.NotificationDelivery, int, error) {
	page := filter.Page.normalise()

	where := []string{"1 = 1"}
	args := make([]any, 0, 8)

	if filter.DestinationID != "" {
		where = append(where, "destination_id = ?")
		args = append(args, filter.DestinationID)
	}
	if filter.ContainerName != "" {
		where = append(where, "container_name = ?")
		args = append(args, filter.ContainerName)
	}
	if filter.FailedOnly {
		where = append(where, "result IN ('failed', 'dropped')")
	}
	if len(filter.Results) > 0 {
		placeholders := make([]string, 0, len(filter.Results))
		for _, result := range filter.Results {
			placeholders = append(placeholders, "?")
			args = append(args, string(result))
		}
		where = append(where, "result IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(filter.Events) > 0 {
		placeholders := make([]string, 0, len(filter.Events))
		for _, event := range filter.Events {
			placeholders = append(placeholders, "?")
			args = append(args, string(event))
		}
		where = append(where, "event IN ("+strings.Join(placeholders, ", ")+")")
	}
	clause := " WHERE " + strings.Join(where, " AND ")

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notification_deliveries`+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count deliveries: %w", AsError(err))
	}

	args = append(args, page.Limit, page.Offset)
	rows, err := r.db.QueryContext(ctx, selectDeliveryColumns+clause+`
		 ORDER BY id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list deliveries: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	deliveries := make([]domain.NotificationDelivery, 0, page.Limit)
	for rows.Next() {
		delivery, err := scanDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, total, rows.Err()
}

// DeliveryByID reads one delivery.
func (r *NotificationRepository) DeliveryByID(
	ctx context.Context,
	deliveryID string,
) (domain.NotificationDelivery, error) {
	delivery, err := scanDelivery(r.db.QueryRowContext(ctx,
		selectDeliveryColumns+` WHERE delivery_id = ?`, deliveryID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.NotificationDelivery{}, ErrNotFound
	}
	return delivery, err
}

// DueRetries returns deliveries whose next attempt has come.
//
// Bounded per call. A backlog is worked oldest first, so a destination that was
// down for an hour catches up in order rather than delivering its most recent
// message and abandoning the rest.
func (r *NotificationRepository) DueRetries(
	ctx context.Context,
	now time.Time,
	limit int,
) ([]domain.NotificationDelivery, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, selectDeliveryColumns+`
		 WHERE result = 'retrying' AND next_attempt_at IS NOT NULL
		   AND next_attempt_at <= ?
		 ORDER BY id ASC LIMIT ?`, formatTime(now.UTC()), limit)
	if err != nil {
		return nil, fmt.Errorf("load due retries: %w", AsError(err))
	}
	defer func() { _ = rows.Close() }()

	deliveries := make([]domain.NotificationDelivery, 0, limit)
	for rows.Next() {
		delivery, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, rows.Err()
}

// DeliverySummary aggregates the history for the dashboard.
func (r *NotificationRepository) DeliverySummary(
	ctx context.Context,
) (domain.NotificationSummary, error) {
	var summary domain.NotificationSummary
	err := r.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN result = 'succeeded'  THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN result IN ('failed','dropped') THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN result = 'suppressed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN result = 'dropped'    THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN result IN ('pending','retrying') THEN 1 ELSE 0 END), 0)
		  FROM notification_deliveries`).
		Scan(&summary.Delivered, &summary.Failed, &summary.Suppressed,
			&summary.Dropped, &summary.Pending)
	if err != nil {
		return domain.NotificationSummary{}, fmt.Errorf("summarise deliveries: %w", AsError(err))
	}
	return summary, nil
}

// PruneDeliveries deletes history older than the cutoff.
//
// Bounded per call so a first prune on a long-running installation cannot hold
// the write lock for the length of a delete over months of history.
func (r *NotificationRepository) PruneDeliveries(
	ctx context.Context,
	before time.Time,
	limit int,
) (int, error) {
	if limit <= 0 || limit > maxDeliveryPruneBatch {
		limit = maxDeliveryPruneBatch
	}
	cutoff := formatTime(before.UTC())

	result, err := r.db.ExecContext(ctx, `
		DELETE FROM notification_deliveries
		 WHERE id IN (
		       SELECT id FROM notification_deliveries
		        WHERE result NOT IN ('pending', 'retrying') AND queued_at < ?
		        ORDER BY id ASC LIMIT ?)`, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("prune deliveries: %w", AsError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune deliveries: %w", AsError(err))
	}

	// The dedup table is pruned on the same cutoff. A key whose delivery is
	// gone is a key nothing will ever compare against again, and leaving it
	// would make this the one table that grows forever.
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM notification_dedup WHERE last_sent_at < ?`, cutoff); err != nil {
		return int(affected), fmt.Errorf("prune deduplication keys: %w", AsError(err))
	}
	return int(affected), nil
}

func scanDelivery(scanner rowScanner) (domain.NotificationDelivery, error) {
	var (
		delivery      domain.NotificationDelivery
		queuedAt      string
		completedAt   sql.NullString
		nextAttemptAt sql.NullString
	)
	err := scanner.Scan(
		&delivery.ID, &delivery.DeliveryID, &delivery.DestinationID,
		&delivery.DestinationName, &delivery.Channel,
		&delivery.RuleID, &delivery.RuleName,
		&delivery.Event, &delivery.Severity, &delivery.Title, &delivery.Body,
		&delivery.ContainerName, &delivery.Result, &delivery.Attempts,
		&delivery.StatusCode, &delivery.Error, &delivery.DedupKey,
		&queuedAt, &completedAt, &nextAttemptAt, &delivery.DurationMs)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NotificationDelivery{}, err
		}
		return domain.NotificationDelivery{}, fmt.Errorf("scan delivery: %w", AsError(err))
	}

	parsed, err := parseTime(queuedAt)
	if err != nil {
		return domain.NotificationDelivery{}, err
	}
	delivery.QueuedAt = parsed

	for _, field := range []struct {
		source sql.NullString
		target **time.Time
	}{
		{completedAt, &delivery.CompletedAt},
		{nextAttemptAt, &delivery.NextAttemptAt},
	} {
		if !field.source.Valid {
			continue
		}
		moment, err := parseTime(field.source.String)
		if err != nil {
			return domain.NotificationDelivery{}, err
		}
		value := moment
		*field.target = &value
	}
	return delivery, nil
}

// ---------------------------------------------------------- deduplication --

// ShouldSuppress reports whether a rule's cooldown swallows a notification, and
// records the send when it does not.
//
// # Why the check and the record are one transaction
//
// Two notifications with the same key arriving together would otherwise both
// read "not sent recently" and both send. The read and the write happen under
// one transaction so the second sees the first.
//
// A zero cooldown suppresses nothing and records nothing: an operator who asked
// for every occurrence gets every one, and the dedup table does not grow for
// rules that do not use it.
func (r *NotificationRepository) ShouldSuppress(
	ctx context.Context,
	ruleID, dedupKey string,
	cooldown time.Duration,
	now time.Time,
) (bool, error) {
	if cooldown <= 0 || dedupKey == "" || ruleID == "" {
		return false, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin suppression check: %w", AsError(err))
	}
	defer func() { _ = tx.Rollback() }()

	var lastSent sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT last_sent_at FROM notification_dedup
		 WHERE rule_id = ? AND dedup_key = ?`, ruleID, dedupKey).Scan(&lastSent)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Never sent. Falls through to the record below.
	case err != nil:
		return false, fmt.Errorf("read suppression window: %w", AsError(err))
	default:
		if lastSent.Valid {
			previous, parseErr := parseTime(lastSent.String)
			if parseErr != nil {
				return false, parseErr
			}
			if now.UTC().Sub(previous) < cooldown {
				return true, nil
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO notification_dedup (rule_id, dedup_key, last_sent_at)
		VALUES (?, ?, ?)
		ON CONFLICT (rule_id, dedup_key) DO UPDATE SET
			last_sent_at = excluded.last_sent_at`,
		ruleID, dedupKey, formatTime(now.UTC())); err != nil {
		return false, fmt.Errorf("record suppression window: %w", AsError(err))
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit suppression check: %w", AsError(err))
	}
	return false, nil
}
