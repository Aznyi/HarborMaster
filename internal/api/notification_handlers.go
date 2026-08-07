package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The notification endpoints.
//
// # What makes these different from every other write in the API
//
// A destination carries a CREDENTIAL. A Slack, Discord, or Teams webhook URL is
// a bearer token in the shape of a path: anyone holding it can post into that
// channel forever. So:
//
//   - The URL is accepted on the way IN and never returned on the way out.
//     There is no response type in this file with a field for one, and the
//     service has no read path that produces one.
//   - What a read returns is the destination's public record, whose Endpoint is
//     a scheme and a host. `https://hooks.slack.com` tells an administrator
//     which destination they are looking at without telling a shoulder-surfer
//     how to post to it.
//   - Every write needs `notification:manage`, an ADMINISTRATOR permission. An
//     operator who could create a destination could point HarborMaster's second
//     outbound egress at a server they control and receive every container name
//     and update event this host produces.
//
// Reading the delivery history is `notification:read`, which every role holds:
// "was anybody told about this" is a question an operator needs to answer, and
// a delivery record carries no credential by construction.
//
// # Where the SSRF checks are
//
// Not here. A URL is validated by the domain when it is stored, and the
// resolved ADDRESS is checked again at dial time by the transport's guard --
// which is what makes DNS rebinding ineffective. A handler-level check would be
// a third copy that can disagree with the other two.

// NotificationAdmin is the configuration capability the handlers depend on.
type NotificationAdmin interface {
	Available() bool

	CreateDestination(ctx context.Context, destination domain.NotificationDestination,
		secret domain.NotificationSecret, actor service.Actor) (service.DestinationResult, error)
	UpdateDestination(ctx context.Context, destinationID string,
		change store.DestinationChange, actor service.Actor) (service.DestinationResult, error)
	ArchiveDestination(ctx context.Context, destinationID string, actor service.Actor) error
	Destination(ctx context.Context, destinationID string) (domain.NotificationDestination, error)
	Destinations(ctx context.Context, includeArchived bool,
		page store.Page) ([]domain.NotificationDestination, int, error)
	TestDestination(ctx context.Context, destinationID string, actor service.Actor) error

	CreateRule(ctx context.Context, rule domain.NotificationRule,
		actor service.Actor) (service.RuleResult, error)
	UpdateRule(ctx context.Context, ruleID string, change store.NotificationRuleChange,
		actor service.Actor) (service.RuleResult, error)
	ArchiveRule(ctx context.Context, ruleID string, actor service.Actor) error
	Rule(ctx context.Context, ruleID string) (domain.NotificationRule, error)
	Rules(ctx context.Context, includeArchived bool,
		page store.Page) ([]domain.NotificationRule, int, error)
}

// NotificationReader is the history capability the handlers depend on.
type NotificationReader interface {
	Enabled() bool
	Readable() bool
	Deliveries(ctx context.Context, filter store.DeliveryFilter) ([]domain.NotificationDelivery, int, error)
	Delivery(ctx context.Context, deliveryID string) (domain.NotificationDelivery, error)
	Summary(ctx context.Context) (domain.NotificationSummary, error)
}

// ------------------------------------------------------------- availability --

// notificationsUnavailable writes the disabled response, and reports whether it
// did.
//
// Availability here means CONFIGURABLE, not sending. A deployment with
// notifications switched off still serves these endpoints so an administrator
// can set destinations up and review past deliveries before turning delivery
// on -- which is the order those should happen in.
func (s *Server) notificationsUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.notificationAdmin != nil && s.notificationAdmin.Available() {
		return false
	}
	writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
		"notifications are not configured in this deployment")
	return true
}

// notificationHistoryUnavailable writes the disabled response for the read
// routes.
func (s *Server) notificationHistoryUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.notifications != nil && s.notifications.Readable() {
		return false
	}
	writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
		"notifications are not configured in this deployment")
	return true
}

// ------------------------------------------------------------- destinations --

// destinationRequest is the create and update body.
//
// Every field is a POINTER so "not supplied" and "supplied as the zero value"
// stay distinguishable. Without that, a PATCH omitting `enabled` would silently
// disable a destination.
//
// # There is no `endpoint` field, deliberately
//
// The endpoint is DERIVED from the URL rather than supplied alongside it. Two
// fields would be two things that can disagree, and the one an administrator
// reads would be the one that is not used.
type destinationRequest struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Channel     *string `json:"channel,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
	TitlePrefix *string `json:"titlePrefix,omitempty"`

	// URL is the webhook endpoint, including whatever token its path carries.
	// Accepted here and NEVER returned. Omitting it on an update keeps the
	// stored one, which is what lets an administrator rename a destination
	// without re-typing a credential they may not have kept.
	URL *string `json:"url,omitempty"`

	EmailTo   *[]string `json:"emailTo,omitempty"`
	EmailFrom *string   `json:"emailFrom,omitempty"`
}

// destinationListResponse is the destination listing.
type destinationListResponse struct {
	Items      []domain.NotificationDestination `json:"items"`
	Pagination Pagination                       `json:"pagination"`
}

// handleNotificationDestinations lists destinations.
func (s *Server) handleNotificationDestinations(w http.ResponseWriter, r *http.Request) {
	if s.notificationsUnavailable(w, r) {
		return
	}

	query, err := parseNotificationConfigQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	items, total, err := s.notificationAdmin.Destinations(r.Context(),
		query.IncludeArchived, store.Page{Limit: query.PageSize, Offset: query.Offset()})
	if err != nil {
		s.logger.ErrorContext(r.Context(), "notification destination list failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, destinationListResponse{
		Items:      items,
		Pagination: newPagination(query.Page, query.PageSize, total),
	})
}

// handleNotificationDestinationDetail returns one destination's public record.
func (s *Server) handleNotificationDestinationDetail(w http.ResponseWriter, r *http.Request) {
	if s.notificationsUnavailable(w, r) {
		return
	}
	destinationID, ok := s.notificationDestinationID(w, r)
	if !ok {
		return
	}

	destination, err := s.notificationAdmin.Destination(r.Context(), destinationID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound,
			"notification destination not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "notification destination load failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, destination)
}

// handleNotificationDestinationCreate stores a new destination.
func (s *Server) handleNotificationDestinationCreate(w http.ResponseWriter, r *http.Request) {
	if s.notificationsUnavailable(w, r) {
		return
	}

	var body destinationRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}
	if body.Name == nil || body.Channel == nil {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"name and channel are required")
		return
	}

	destination := domain.NotificationDestination{
		Name:    *body.Name,
		Channel: domain.NotificationChannel(*body.Channel),
		// A destination an administrator just created is one they want working.
		// Explicitly disabling it stays possible.
		Enabled: true,
	}
	if body.Description != nil {
		destination.Description = *body.Description
	}
	if body.Enabled != nil {
		destination.Enabled = *body.Enabled
	}
	if body.TitlePrefix != nil {
		destination.TitlePrefix = *body.TitlePrefix
	}
	if body.EmailTo != nil {
		destination.EmailTo = *body.EmailTo
	}
	if body.EmailFrom != nil {
		destination.EmailFrom = *body.EmailFrom
	}

	secret := domain.NotificationSecret{}
	if body.URL != nil {
		secret.URL = *body.URL
	}

	result, err := s.notificationAdmin.CreateDestination(r.Context(), destination,
		secret, s.actorFrom(r))
	if err != nil {
		s.writeNotificationError(w, r, err, "notification destination create failed")
		return
	}

	w.Header().Set("Location",
		APIPrefix+"/notifications/destinations/"+result.Destination.DestinationID)
	writeJSON(w, r, s.logger, http.StatusCreated, result)
}

// handleNotificationDestinationUpdate applies a partial change.
func (s *Server) handleNotificationDestinationUpdate(w http.ResponseWriter, r *http.Request) {
	if s.notificationsUnavailable(w, r) {
		return
	}
	destinationID, ok := s.notificationDestinationID(w, r)
	if !ok {
		return
	}

	var body destinationRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}
	// The channel is not editable. Changing it would mean a destination whose
	// stored credential is the wrong SHAPE for what it now is -- a webhook URL
	// on an email destination -- and every validation that would have caught
	// that ran when the credential was written.
	if body.Channel != nil {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"a destination's channel cannot be changed; create a new destination instead")
		return
	}

	change := store.DestinationChange{
		Name:        body.Name,
		Description: body.Description,
		Enabled:     body.Enabled,
		TitlePrefix: body.TitlePrefix,
		EmailTo:     body.EmailTo,
		EmailFrom:   body.EmailFrom,
	}
	if body.URL != nil {
		change.Secret = &domain.NotificationSecret{URL: *body.URL}
	}

	result, err := s.notificationAdmin.UpdateDestination(r.Context(), destinationID,
		change, s.actorFrom(r))
	if err != nil {
		s.writeNotificationError(w, r, err, "notification destination edit failed")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, result)
}

// handleNotificationDestinationDelete withdraws a destination.
//
// Archives rather than deletes. Delivery records reference the row, and the
// history of what was sent must survive the destination being withdrawn. The
// stored credential does NOT survive: an archived destination cannot send, so
// keeping its URL would be keeping a credential for no reason.
func (s *Server) handleNotificationDestinationDelete(w http.ResponseWriter, r *http.Request) {
	if s.notificationsUnavailable(w, r) {
		return
	}
	destinationID, ok := s.notificationDestinationID(w, r)
	if !ok {
		return
	}

	err := s.notificationAdmin.ArchiveDestination(r.Context(), destinationID, s.actorFrom(r))
	if err != nil {
		s.writeNotificationError(w, r, err, "notification destination archive failed")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleNotificationDestinationTest sends a test notification.
//
// Accepted rather than OK: the send is asynchronous, travelling the same queue
// and the same transport as every real notification. A synchronous test would
// prove a different path works.
func (s *Server) handleNotificationDestinationTest(w http.ResponseWriter, r *http.Request) {
	if s.notificationsUnavailable(w, r) {
		return
	}
	destinationID, ok := s.notificationDestinationID(w, r)
	if !ok {
		return
	}

	err := s.notificationAdmin.TestDestination(r.Context(), destinationID, s.actorFrom(r))
	if err != nil {
		s.writeNotificationError(w, r, err, "notification test failed")
		return
	}

	writeJSON(w, r, s.logger, http.StatusAccepted, map[string]string{
		"status": "queued",
		"detail": "the test notification was queued; its outcome appears in the delivery history",
	})
}

// -------------------------------------------------------------------- rules --

// ruleRequest is the create and update body.
type ruleRequest struct {
	Name            *string   `json:"name,omitempty"`
	Enabled         *bool     `json:"enabled,omitempty"`
	Events          *[]string `json:"events,omitempty"`
	MinimumSeverity *string   `json:"minimumSeverity,omitempty"`
	Destinations    *[]string `json:"destinations,omitempty"`
	CooldownSeconds *int      `json:"cooldownSeconds,omitempty"`
}

// ruleListResponse is the rule listing.
type ruleListResponse struct {
	Items      []domain.NotificationRule `json:"items"`
	Pagination Pagination                `json:"pagination"`
}

// handleNotificationRules lists routing rules.
func (s *Server) handleNotificationRules(w http.ResponseWriter, r *http.Request) {
	if s.notificationsUnavailable(w, r) {
		return
	}

	query, err := parseNotificationConfigQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	items, total, err := s.notificationAdmin.Rules(r.Context(),
		query.IncludeArchived, store.Page{Limit: query.PageSize, Offset: query.Offset()})
	if err != nil {
		s.logger.ErrorContext(r.Context(), "notification rule list failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, ruleListResponse{
		Items:      items,
		Pagination: newPagination(query.Page, query.PageSize, total),
	})
}

// handleNotificationRuleDetail returns one rule.
func (s *Server) handleNotificationRuleDetail(w http.ResponseWriter, r *http.Request) {
	if s.notificationsUnavailable(w, r) {
		return
	}
	ruleID, ok := s.notificationRuleID(w, r)
	if !ok {
		return
	}

	rule, err := s.notificationAdmin.Rule(r.Context(), ruleID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "notification rule not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "notification rule load failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, rule)
}

// handleNotificationRuleCreate stores a new rule.
func (s *Server) handleNotificationRuleCreate(w http.ResponseWriter, r *http.Request) {
	if s.notificationsUnavailable(w, r) {
		return
	}

	var body ruleRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}
	if body.Name == nil || body.Destinations == nil {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"name and destinations are required")
		return
	}

	rule := domain.NotificationRule{
		Name:         *body.Name,
		Destinations: *body.Destinations,
		Enabled:      true,
		// The severity floor when none is stated. Warning rather than info: a
		// rule an administrator wrote without saying is more likely to be about
		// things going wrong than about routine progress.
		MinimumSeverity: domain.NotifyWarning,
	}
	if body.Enabled != nil {
		rule.Enabled = *body.Enabled
	}
	if body.MinimumSeverity != nil {
		rule.MinimumSeverity = domain.NotificationSeverity(*body.MinimumSeverity)
	}
	if body.Events != nil {
		rule.Events = notificationEvents(*body.Events)
	}
	if body.CooldownSeconds != nil {
		rule.CooldownSeconds = *body.CooldownSeconds
	}

	result, err := s.notificationAdmin.CreateRule(r.Context(), rule, s.actorFrom(r))
	if err != nil {
		s.writeNotificationError(w, r, err, "notification rule create failed")
		return
	}

	w.Header().Set("Location", APIPrefix+"/notifications/rules/"+result.Rule.RuleID)
	writeJSON(w, r, s.logger, http.StatusCreated, result)
}

// handleNotificationRuleUpdate applies a partial change.
func (s *Server) handleNotificationRuleUpdate(w http.ResponseWriter, r *http.Request) {
	if s.notificationsUnavailable(w, r) {
		return
	}
	ruleID, ok := s.notificationRuleID(w, r)
	if !ok {
		return
	}

	var body ruleRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	change := store.NotificationRuleChange{
		Name:            body.Name,
		Enabled:         body.Enabled,
		Destinations:    body.Destinations,
		CooldownSeconds: body.CooldownSeconds,
	}
	// The two vocabularies arrive as strings so an unknown value is a
	// validation error rather than an unmarshalling one.
	if body.MinimumSeverity != nil {
		severity := domain.NotificationSeverity(*body.MinimumSeverity)
		change.MinimumSeverity = &severity
	}
	if body.Events != nil {
		events := notificationEvents(*body.Events)
		change.Events = &events
	}

	result, err := s.notificationAdmin.UpdateRule(r.Context(), ruleID, change, s.actorFrom(r))
	if err != nil {
		s.writeNotificationError(w, r, err, "notification rule edit failed")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, result)
}

// handleNotificationRuleDelete withdraws a rule.
func (s *Server) handleNotificationRuleDelete(w http.ResponseWriter, r *http.Request) {
	if s.notificationsUnavailable(w, r) {
		return
	}
	ruleID, ok := s.notificationRuleID(w, r)
	if !ok {
		return
	}

	err := s.notificationAdmin.ArchiveRule(r.Context(), ruleID, s.actorFrom(r))
	if err != nil {
		s.writeNotificationError(w, r, err, "notification rule archive failed")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- history --

// deliveryListResponse is the delivery history listing.
type deliveryListResponse struct {
	Items      []domain.NotificationDelivery `json:"items"`
	Pagination Pagination                    `json:"pagination"`
}

// handleNotificationDeliveries lists the delivery history.
func (s *Server) handleNotificationDeliveries(w http.ResponseWriter, r *http.Request) {
	if s.notificationHistoryUnavailable(w, r) {
		return
	}

	query, err := parseDeliveryQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	items, total, err := s.notifications.Deliveries(r.Context(), query.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "notification delivery list failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, deliveryListResponse{
		Items:      items,
		Pagination: newPagination(query.Page, query.PageSize, total),
	})
}

// handleNotificationDeliveryDetail returns one delivery record.
func (s *Server) handleNotificationDeliveryDetail(w http.ResponseWriter, r *http.Request) {
	if s.notificationHistoryUnavailable(w, r) {
		return
	}

	// Validated by SHAPE here as well as in the service, matching the
	// destination and rule routes. The service checks too -- a second caller
	// must not be able to reach the repository without it -- but a malformed id
	// should never travel further than the handler that received it.
	deliveryID := strings.TrimSpace(r.PathValue("id"))
	if !domain.ValidNotificationDeliveryID(deliveryID) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound,
			"notification delivery not found")
		return
	}

	delivery, err := s.notifications.Delivery(r.Context(), deliveryID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound,
			"notification delivery not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "notification delivery load failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, delivery)
}

// notificationStatusResponse is what the notifications page header shows.
type notificationStatusResponse struct {
	domain.NotificationSummary
	// Channels is the closed set of channels this build can deliver to, so the
	// UI's picker is built from the same source of truth the sender uses rather
	// than from a list somebody kept in sync by hand.
	Channels []domain.NotificationChannel `json:"channels"`
	// Events and Severities are the vocabularies a rule may select from, for
	// the same reason.
	Events     []domain.NotificationEvent    `json:"events"`
	Severities []domain.NotificationSeverity `json:"severities"`
}

// handleNotificationStatus returns the subsystem's state and vocabularies.
func (s *Server) handleNotificationStatus(w http.ResponseWriter, r *http.Request) {
	if s.notificationHistoryUnavailable(w, r) {
		return
	}

	summary, err := s.notifications.Summary(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "notification summary failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, notificationStatusResponse{
		NotificationSummary: summary,
		Channels:            domain.NotificationChannels,
		Events:              domain.NotificationEvents,
		Severities:          domain.NotificationSeverities,
	})
}

// ------------------------------------------------------------------ helpers --

// notificationDestinationID reads and validates the path parameter.
func (s *Server) notificationDestinationID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	if !domain.ValidNotificationDestinationID(raw) {
		// The same 404 an unknown id gets. A malformed id must not be
		// distinguishable from an absent one.
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound,
			"notification destination not found")
		return "", false
	}
	return raw, true
}

// notificationRuleID reads and validates the path parameter.
func (s *Server) notificationRuleID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	if !domain.ValidNotificationRuleID(raw) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "notification rule not found")
		return "", false
	}
	return raw, true
}

// notificationEvents converts the request's strings to events.
//
// No validation here: an unknown event is refused by the domain, whose message
// names the field and the constraint. Converting first means the refusal is a
// validation error rather than a JSON unmarshalling one, which reads very
// differently to whoever is holding the API.
func notificationEvents(raw []string) []domain.NotificationEvent {
	events := make([]domain.NotificationEvent, 0, len(raw))
	for _, value := range raw {
		events = append(events, domain.NotificationEvent(value))
	}
	return events
}

// writeNotificationError renders the notification write failures.
//
// One place for all of them, because the mapping matters: a validation refusal
// is a 400 an administrator fixes, a name clash is a 409, a bound is a 409, and
// everything else is an internal error whose text never reaches the response.
func (s *Server) writeNotificationError(
	w http.ResponseWriter,
	r *http.Request,
	err error,
	logMessage string,
) {
	var validation domain.PolicyValidationError

	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, service.ErrNotificationsDisabled):
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"notifications are not configured in this deployment")
	case errors.Is(err, service.ErrDestinationNotTestable):
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"this destination is disabled or withdrawn, so it cannot be tested")
	case errors.Is(err, store.ErrDestinationNameTaken):
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"a notification destination with that name already exists")
	case errors.Is(err, store.ErrNotificationRuleNameTaken):
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"a notification rule with that name already exists")
	case errors.Is(err, store.ErrDestinationInUse):
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"a notification rule still routes to this destination; withdraw the rule first")
	case errors.Is(err, service.ErrTooManyDestinations):
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"this deployment already has as many notification destinations as it allows")
	case errors.Is(err, service.ErrTooManyRules):
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"this deployment already has as many notification rules as it allows")
	case errors.Is(err, service.ErrUnknownDestination):
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"this rule names a destination that does not exist")
	case errors.As(err, &validation):
		// The FIELD and the CONSTRAINT, never the offending value: a response
		// that reflected its input would be a way to make the API return
		// attacker-chosen text -- and for these endpoints the input can be a
		// credential.
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			validation.Field+" "+validation.Message)
	default:
		// Logged WITHOUT the error's text. Every write in this file has a
		// credential somewhere in its call stack, and an error from the storage
		// layer has been known to quote the statement.
		s.logger.ErrorContext(r.Context(), logMessage)
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
	}
}

// Compile-time proof that the services satisfy the interfaces the handlers
// depend on.
var (
	_ NotificationAdmin  = (*service.NotificationAdminService)(nil)
	_ NotificationReader = (*service.NotificationService)(nil)
)
