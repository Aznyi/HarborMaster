package service

import (
	"context"
	"errors"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Notification administration.
//
// # Why this is separate from the engine
//
// The engine ROUTES and DELIVERS. This CONFIGURES. They share a repository and
// nothing else, and keeping them apart means the code that handles an
// administrator's credential is not the code that runs in four delivery workers
// against a queue.
//
// # The credential rule, restated because this is where it is easiest to break
//
// A credential enters through exactly two methods here — Create and Update —
// and leaves through none. There is no read path on this type that returns a
// domain.NotificationSecret, and an architecture test holds it. What a caller
// gets back is always the destination's PUBLIC record, whose Endpoint is a
// scheme and host rather than a URL.
//
// # Validation is here, not in the handler
//
// So a second caller — a future CLI, a future import — cannot reach the
// repository without it. The handler decodes; this validates.

// NotificationAdminStore is the persistence notification administration needs.
type NotificationAdminStore interface {
	CreateDestination(ctx context.Context, destination domain.NotificationDestination,
		secret domain.NotificationSecret, now time.Time) (domain.NotificationDestination, error)
	UpdateDestination(ctx context.Context, destinationID string,
		change store.DestinationChange, now time.Time) (domain.NotificationDestination, error)
	ArchiveDestination(ctx context.Context, destinationID string, now time.Time) error
	DestinationByID(ctx context.Context, destinationID string) (domain.NotificationDestination, error)
	ListDestinations(ctx context.Context, includeArchived bool,
		page store.Page) ([]domain.NotificationDestination, int, error)
	// CountNotificationConfiguration is what both bounds are checked against.
	// One method rather than two because it is one query, and the bound check
	// runs on every create.
	CountNotificationConfiguration(ctx context.Context) (destinations, rules, failing int, err error)

	CreateRule(ctx context.Context, rule domain.NotificationRule,
		now time.Time) (domain.NotificationRule, error)
	UpdateRule(ctx context.Context, ruleID string, change store.NotificationRuleChange,
		now time.Time) (domain.NotificationRule, error)
	ArchiveRule(ctx context.Context, ruleID string, now time.Time) error
	RuleByID(ctx context.Context, ruleID string) (domain.NotificationRule, error)
	ListRules(ctx context.Context, includeArchived bool,
		page store.Page) ([]domain.NotificationRule, int, error)
}

// NotificationAdminService configures destinations and rules.
type NotificationAdminService struct {
	store  NotificationAdminStore
	engine *NotificationService
	audit  *AuditRecorder
	smtp   domain.SMTPSettings
	limits domain.NotificationLimits
	now    func() time.Time
}

// NotificationAdminOptions configures a NotificationAdminService.
type NotificationAdminOptions struct {
	Store  NotificationAdminStore
	Engine *NotificationService
	Audit  *AuditRecorder
	// SMTP is the configured relay. Validation consults it so a deployment with
	// no relay refuses an email destination at CREATE rather than silently
	// failing every delivery afterwards.
	SMTP   domain.SMTPSettings
	Limits domain.NotificationLimits
	Now    func() time.Time
}

// NewNotificationAdminService builds a NotificationAdminService.
func NewNotificationAdminService(opts NotificationAdminOptions) *NotificationAdminService {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	limits := opts.Limits.Normalised()

	return &NotificationAdminService{
		store:  opts.Store,
		engine: opts.Engine,
		audit:  opts.Audit,
		smtp:   opts.SMTP,
		limits: limits,
		now:    now,
	}
}

// Available reports whether notification configuration can be served.
func (s *NotificationAdminService) Available() bool { return s != nil && s.store != nil }

// ------------------------------------------------------------ destinations --

// DestinationResult is a destination and any advice about it.
//
// Warnings are things worth saying that are not reasons to refuse: a
// destination pointing at a private address, an email destination on a relay
// with no credentials. An administrator who meant it proceeds; one who did not
// finds out now rather than at the first alert.
type DestinationResult struct {
	Destination domain.NotificationDestination `json:"destination"`
	Warnings    []string                       `json:"warnings,omitempty"`
}

// CreateDestination stores a destination and its credential.
func (s *NotificationAdminService) CreateDestination(
	ctx context.Context,
	destination domain.NotificationDestination,
	secret domain.NotificationSecret,
	actor Actor,
) (DestinationResult, error) {
	if !s.Available() {
		return DestinationResult{}, ErrNotificationsDisabled
	}

	// The count check before the write. A bound that is only enforced by the
	// database would be a bound expressed as an opaque constraint failure.
	count, _, _, err := s.store.CountNotificationConfiguration(ctx)
	if err != nil {
		return DestinationResult{}, err
	}
	if count >= s.limits.MaxDestinations {
		return DestinationResult{}, ErrTooManyDestinations
	}

	destination.DestinationID = domain.NewNotificationDestinationID()
	// Not something a create may set. A destination that arrived pre-archived
	// would be a row nobody can edit and nothing delivers to.
	destination.Archived = false
	// Nor is health. These are observations, and a caller cannot supply one.
	destination.LastResult = ""
	destination.LastAttemptAt = nil
	destination.LastError = ""
	destination.ConsecutiveFailures = 0

	// The safe rendering, DERIVED from the credential rather than supplied.
	//
	// Two fields for one fact would be two things that can disagree, and the
	// one an administrator reads would be the one that is not used. So the
	// request type has no endpoint field at all and this is the only place it
	// is set.
	destination.Endpoint = safeEndpointFor(destination, secret, s.smtp)

	destination.Normalise()
	if err := destination.Validate(secret, s.smtp, s.limits); err != nil {
		return DestinationResult{}, err
	}

	created, err := s.store.CreateDestination(ctx, destination, secret, s.now())
	if err != nil {
		return DestinationResult{}, err
	}

	s.recordDestinationAudit(ctx, domain.AuditNotificationDestinationCreated, created, actor,
		"created a "+string(created.Channel)+" notification destination")

	return DestinationResult{
		Destination: created,
		Warnings:    created.Warnings(),
	}, nil
}

// UpdateDestination applies a partial change.
func (s *NotificationAdminService) UpdateDestination(
	ctx context.Context,
	destinationID string,
	change store.DestinationChange,
	actor Actor,
) (DestinationResult, error) {
	if !s.Available() {
		return DestinationResult{}, ErrNotificationsDisabled
	}
	if !domain.ValidNotificationDestinationID(destinationID) {
		return DestinationResult{}, store.ErrNotFound
	}

	existing, err := s.store.DestinationByID(ctx, destinationID)
	if err != nil {
		return DestinationResult{}, err
	}

	// Validated as it WOULD BE after the change, not as it arrived. A PATCH
	// that adds a recipient to an email destination has to be checked against
	// the whole destination, not against one field.
	preview := applyDestinationChange(existing, change)
	preview.Normalise()

	// The credential the preview is validated against. An edit that did not
	// mention the URL keeps the stored one -- which this service cannot read --
	// so the preview is told that one EXISTS rather than what it is.
	secret := domain.NotificationSecret{}
	switch {
	case change.Secret != nil:
		secret = *change.Secret
		// A replaced credential means a new safe rendering.
		preview.Endpoint = safeEndpointFor(preview, secret, s.smtp)
	case existing.Channel != domain.ChannelEmail:
		// A stand-in that satisfies "a webhook destination has a URL" without
		// this service ever holding the real one. It is never written: the
		// repository leaves the stored credential alone when Secret is nil.
		//
		// Keyed on the CHANNEL rather than on the stored endpoint. A webhook
		// destination that exists has a credential, because it could not have
		// been created without one -- and reading the endpoint to decide would
		// make an empty one, from any cause, refuse every subsequent edit.
		secret = domain.NotificationSecret{URL: placeholderStoredURL}
	}
	if err := preview.Validate(secret, s.smtp, s.limits); err != nil {
		return DestinationResult{}, err
	}

	change = normalisedDestinationChange(change, preview)

	updated, err := s.store.UpdateDestination(ctx, destinationID, change, s.now())
	if err != nil {
		return DestinationResult{}, err
	}

	reason := "edited a notification destination"
	if change.Secret != nil {
		// Said explicitly, because rotating a credential is the change an
		// administrator reading the audit log most wants to see.
		reason = "edited a notification destination and replaced its credential"
	}
	s.recordDestinationAudit(ctx, domain.AuditNotificationDestinationUpdated, updated, actor, reason)

	return DestinationResult{
		Destination: updated,
		Warnings:    updated.Warnings(),
	}, nil
}

// ArchiveDestination withdraws a destination and destroys its credential.
func (s *NotificationAdminService) ArchiveDestination(
	ctx context.Context,
	destinationID string,
	actor Actor,
) error {
	if !s.Available() {
		return ErrNotificationsDisabled
	}
	if !domain.ValidNotificationDestinationID(destinationID) {
		return store.ErrNotFound
	}

	// Read first, so the audit record can name what was archived. After the
	// archive the row is still there, but reading it before means the record is
	// written from the state the administrator acted on.
	existing, err := s.store.DestinationByID(ctx, destinationID)
	if err != nil {
		return err
	}

	if err := s.store.ArchiveDestination(ctx, destinationID, s.now()); err != nil {
		return err
	}

	s.recordDestinationAudit(ctx, domain.AuditNotificationDestinationArchived, existing, actor,
		"withdrew a notification destination; its stored credential was destroyed")
	return nil
}

// Destination returns one destination's public record.
func (s *NotificationAdminService) Destination(
	ctx context.Context,
	destinationID string,
) (domain.NotificationDestination, error) {
	if !s.Available() {
		return domain.NotificationDestination{}, ErrNotificationsDisabled
	}
	if !domain.ValidNotificationDestinationID(destinationID) {
		return domain.NotificationDestination{}, store.ErrNotFound
	}
	return s.store.DestinationByID(ctx, destinationID)
}

// Destinations returns a bounded page of destinations.
func (s *NotificationAdminService) Destinations(
	ctx context.Context,
	includeArchived bool,
	page store.Page,
) ([]domain.NotificationDestination, int, error) {
	if !s.Available() {
		return nil, 0, ErrNotificationsDisabled
	}
	return s.store.ListDestinations(ctx, includeArchived, page)
}

// TestDestination sends a test notification.
//
// # Why this is audited
//
// It is a real outbound HTTPS request that somebody caused, and "who made this
// host talk to that server" is the question an audit log exists to answer. It
// is also the one write in this service that produces network traffic without
// changing a row, which makes it the easiest to forget.
func (s *NotificationAdminService) TestDestination(
	ctx context.Context,
	destinationID string,
	actor Actor,
) error {
	if !s.Available() {
		return ErrNotificationsDisabled
	}
	if !domain.ValidNotificationDestinationID(destinationID) {
		return store.ErrNotFound
	}

	// The destination must exist, be enabled, and not be archived. Checked here
	// rather than left to the router so the caller gets a reason.
	destination, err := s.store.DestinationByID(ctx, destinationID)
	if err != nil {
		return err
	}
	if destination.Archived || !destination.Enabled {
		return ErrDestinationNotTestable
	}

	if s.engine == nil || !s.engine.Enabled() {
		return ErrNotificationsDisabled
	}
	if err := s.engine.RaiseTest(destinationID); err != nil {
		return err
	}

	s.recordDestinationAudit(ctx, domain.AuditNotificationDestinationTested, destination, actor,
		"sent a test notification to a "+string(destination.Channel)+" destination")
	return nil
}

// -------------------------------------------------------------------- rules --

// RuleResult is a rule and any advice about it.
type RuleResult struct {
	Rule     domain.NotificationRule `json:"rule"`
	Warnings []string                `json:"warnings,omitempty"`
}

// CreateRule stores a routing rule.
func (s *NotificationAdminService) CreateRule(
	ctx context.Context,
	rule domain.NotificationRule,
	actor Actor,
) (RuleResult, error) {
	if !s.Available() {
		return RuleResult{}, ErrNotificationsDisabled
	}

	_, count, _, err := s.store.CountNotificationConfiguration(ctx)
	if err != nil {
		return RuleResult{}, err
	}
	if count >= s.limits.MaxRules {
		return RuleResult{}, ErrTooManyRules
	}

	rule.RuleID = domain.NewNotificationRuleID()
	rule.Archived = false
	rule.Normalise()
	if err := rule.Validate(s.limits); err != nil {
		return RuleResult{}, err
	}
	if err := s.destinationsExist(ctx, rule.Destinations); err != nil {
		return RuleResult{}, err
	}

	created, err := s.store.CreateRule(ctx, rule, s.now())
	if err != nil {
		return RuleResult{}, err
	}

	s.recordRuleAudit(ctx, domain.AuditNotificationRuleCreated, created, actor,
		"created a notification rule")

	return RuleResult{Rule: created, Warnings: created.Warnings()}, nil
}

// UpdateRule applies a partial change.
func (s *NotificationAdminService) UpdateRule(
	ctx context.Context,
	ruleID string,
	change store.NotificationRuleChange,
	actor Actor,
) (RuleResult, error) {
	if !s.Available() {
		return RuleResult{}, ErrNotificationsDisabled
	}
	if !domain.ValidNotificationRuleID(ruleID) {
		return RuleResult{}, store.ErrNotFound
	}

	existing, err := s.store.RuleByID(ctx, ruleID)
	if err != nil {
		return RuleResult{}, err
	}

	preview := applyRuleChange(existing, change)
	preview.Normalise()
	if err := preview.Validate(s.limits); err != nil {
		return RuleResult{}, err
	}
	if change.Destinations != nil {
		if err := s.destinationsExist(ctx, preview.Destinations); err != nil {
			return RuleResult{}, err
		}
	}

	change = normalisedRuleChange(change, preview)

	updated, err := s.store.UpdateRule(ctx, ruleID, change, s.now())
	if err != nil {
		return RuleResult{}, err
	}

	s.recordRuleAudit(ctx, domain.AuditNotificationRuleUpdated, updated, actor,
		"edited a notification rule")

	return RuleResult{Rule: updated, Warnings: updated.Warnings()}, nil
}

// ArchiveRule withdraws a rule.
func (s *NotificationAdminService) ArchiveRule(
	ctx context.Context,
	ruleID string,
	actor Actor,
) error {
	if !s.Available() {
		return ErrNotificationsDisabled
	}
	if !domain.ValidNotificationRuleID(ruleID) {
		return store.ErrNotFound
	}

	existing, err := s.store.RuleByID(ctx, ruleID)
	if err != nil {
		return err
	}
	if err := s.store.ArchiveRule(ctx, ruleID, s.now()); err != nil {
		return err
	}

	s.recordRuleAudit(ctx, domain.AuditNotificationRuleArchived, existing, actor,
		"withdrew a notification rule")
	return nil
}

// Rule returns one rule.
func (s *NotificationAdminService) Rule(
	ctx context.Context,
	ruleID string,
) (domain.NotificationRule, error) {
	if !s.Available() {
		return domain.NotificationRule{}, ErrNotificationsDisabled
	}
	if !domain.ValidNotificationRuleID(ruleID) {
		return domain.NotificationRule{}, store.ErrNotFound
	}
	return s.store.RuleByID(ctx, ruleID)
}

// Rules returns a bounded page of rules.
func (s *NotificationAdminService) Rules(
	ctx context.Context,
	includeArchived bool,
	page store.Page,
) ([]domain.NotificationRule, int, error) {
	if !s.Available() {
		return nil, 0, ErrNotificationsDisabled
	}
	return s.store.ListRules(ctx, includeArchived, page)
}

// ------------------------------------------------------------------ helpers --

// Notification administration errors.
var (
	// ErrTooManyDestinations reports the destination bound.
	ErrTooManyDestinations = errors.New("this deployment already has as many notification destinations as it allows")
	// ErrTooManyRules reports the rule bound.
	ErrTooManyRules = errors.New("this deployment already has as many notification rules as it allows")
	// ErrUnknownDestination reports a rule naming a destination that is not
	// there.
	ErrUnknownDestination = errors.New("this rule names a destination that does not exist")
)

// safeEndpointFor renders where a destination sends, safely.
//
// A webhook's scheme and host, never its path: the path IS the credential for
// Slack, Discord, and Teams. An email destination renders the RELAY, which is
// deployment configuration rather than a secret, and not the recipients --
// those are on the record already.
func safeEndpointFor(
	destination domain.NotificationDestination,
	secret domain.NotificationSecret,
	smtp domain.SMTPSettings,
) string {
	if destination.Channel == domain.ChannelEmail {
		return domain.SafeSMTPEndpoint(smtp.Host, smtp.Port)
	}
	return domain.SafeEndpoint(secret.URL)
}

// placeholderStoredURL stands in for a credential this service may not read.
//
// It exists so that validating an edit which did not mention the URL can still
// answer "does this webhook destination have one" without a read path that
// returns a real credential. It is never written and never sent: the repository
// leaves the stored secret untouched when the change carries none.
const placeholderStoredURL = "https://stored.invalid/credential-already-set"

// destinationsExist refuses a rule that routes nowhere real.
//
// A rule naming a destination that does not exist would silently deliver
// nothing, and an operator would have no way to tell it apart from a rule that
// never matched.
func (s *NotificationAdminService) destinationsExist(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if !domain.ValidNotificationDestinationID(id) {
			return ErrUnknownDestination
		}
		destination, err := s.store.DestinationByID(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return ErrUnknownDestination
		}
		if err != nil {
			return err
		}
		if destination.Archived {
			return ErrUnknownDestination
		}
	}
	return nil
}

// recordDestinationAudit writes the destination audit row.
//
// The NAME and the identifier, never the endpoint. A Slack webhook URL is a
// bearer token in the shape of a path, and an audit page is somewhere an
// administrator reads.
func (s *NotificationAdminService) recordDestinationAudit(
	ctx context.Context,
	action domain.AuditAction,
	destination domain.NotificationDestination,
	actor Actor,
	reason string,
) {
	s.audit.RecordAction(ctx, actor, action, domain.AuditSucceeded,
		domain.AuditTargetNotificationDestination,
		destination.DestinationID, destination.Name, reason)
}

// recordRuleAudit writes the rule audit row.
func (s *NotificationAdminService) recordRuleAudit(
	ctx context.Context,
	action domain.AuditAction,
	rule domain.NotificationRule,
	actor Actor,
	reason string,
) {
	s.audit.RecordAction(ctx, actor, action, domain.AuditSucceeded,
		domain.AuditTargetNotificationRule, rule.RuleID, rule.Name, reason)
}

// applyDestinationChange renders what a destination would become.
func applyDestinationChange(
	existing domain.NotificationDestination,
	change store.DestinationChange,
) domain.NotificationDestination {
	if change.Name != nil {
		existing.Name = *change.Name
	}
	if change.Description != nil {
		existing.Description = *change.Description
	}
	if change.Enabled != nil {
		existing.Enabled = *change.Enabled
	}
	if change.TitlePrefix != nil {
		existing.TitlePrefix = *change.TitlePrefix
	}
	if change.EmailTo != nil {
		existing.EmailTo = *change.EmailTo
	}
	if change.EmailFrom != nil {
		existing.EmailFrom = *change.EmailFrom
	}
	if change.Endpoint != nil {
		existing.Endpoint = *change.Endpoint
	}
	return existing
}

// normalisedDestinationChange rewrites a change with the normalised values, so
// the repository stores the destination that was validated rather than the raw
// one.
func normalisedDestinationChange(
	change store.DestinationChange,
	preview domain.NotificationDestination,
) store.DestinationChange {
	if change.Name != nil {
		change.Name = &preview.Name
	}
	if change.Description != nil {
		change.Description = &preview.Description
	}
	if change.TitlePrefix != nil {
		change.TitlePrefix = &preview.TitlePrefix
	}
	if change.EmailTo != nil {
		recipients := preview.EmailTo
		change.EmailTo = &recipients
	}
	if change.EmailFrom != nil {
		change.EmailFrom = &preview.EmailFrom
	}
	// Endpoint is DERIVED from the credential rather than supplied, so a change
	// that replaced the URL carries the new safe rendering.
	if change.Secret != nil {
		endpoint := preview.Endpoint
		change.Endpoint = &endpoint
	}
	return change
}

// applyRuleChange renders what a rule would become.
func applyRuleChange(
	existing domain.NotificationRule,
	change store.NotificationRuleChange,
) domain.NotificationRule {
	if change.Name != nil {
		existing.Name = *change.Name
	}
	if change.Enabled != nil {
		existing.Enabled = *change.Enabled
	}
	if change.Events != nil {
		existing.Events = *change.Events
	}
	if change.MinimumSeverity != nil {
		existing.MinimumSeverity = *change.MinimumSeverity
	}
	if change.Destinations != nil {
		existing.Destinations = *change.Destinations
	}
	if change.CooldownSeconds != nil {
		existing.CooldownSeconds = *change.CooldownSeconds
	}
	return existing
}

// normalisedRuleChange rewrites a change with the normalised values.
func normalisedRuleChange(
	change store.NotificationRuleChange,
	preview domain.NotificationRule,
) store.NotificationRuleChange {
	if change.Name != nil {
		change.Name = &preview.Name
	}
	if change.Events != nil {
		events := preview.Events
		change.Events = &events
	}
	if change.Destinations != nil {
		destinations := preview.Destinations
		change.Destinations = &destinations
	}
	return change
}

// Compile-time proof that the repository satisfies both notification
// interfaces. A change to either that breaks the match fails the build here,
// where the reason is obvious, rather than in the composition root.
var (
	_ NotificationStore      = (*store.NotificationRepository)(nil)
	_ NotificationAdminStore = (*store.NotificationRepository)(nil)
)
