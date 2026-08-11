package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The update policy endpoints.
//
// # These are the most consequential writes in the API
//
// A compliance policy REPORTS. An update policy ACTS: it is a standing
// instruction that lets HarborMaster stop and replace containers a selector
// reaches, without anybody watching. So every one of these routes needs
// `automation:manage`, which only an administrator holds -- an operator who
// could write one would be granting themselves a permanent, unattended version
// of `execution:create`.
//
// # What the body can and cannot say
//
// A policy names a SELECTOR -- labels, image glob patterns, container names --
// and a ceiling. It does not name a digest, and it cannot: what image a matched
// container is moved to is decided by the planner from registry evidence, and
// the policy only says how far that move may go.
//
// So there is no field on updatePolicyRequest that reaches Docker. The worst a
// hostile policy can do is select more containers than intended, and every one
// of those still has to have a current change plan the planner produced, a
// recommendation the policy permits, an open maintenance window, and a
// successful digest-verified acquisition before anything is recreated.
//
// # Why validation happens in the service and not here
//
// The handler decodes and the service validates. Putting the rules in the
// service means a second caller -- a future CLI, a future import -- cannot
// reach the repository without them.

// updatePolicyRequest is the create and update body.
//
// Every field is a POINTER so "not supplied" and "supplied as the zero value"
// stay distinguishable. Without that, a PATCH omitting `enabled` would silently
// disable a policy, which for an automation rule is a change of behaviour in
// both directions.
type updatePolicyRequest struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
	Priority    *int    `json:"priority,omitempty"`

	// Scope is what the policy is pointed at. Absent means `selector`, which is
	// what every client written before this field existed meant, and what every
	// stored policy already was.
	Scope                 *string                `json:"scope,omitempty"`
	Selector              *domain.UpdateSelector `json:"selector,omitempty"`
	Strategy              *string                `json:"strategy,omitempty"`
	MinimumRecommendation *string                `json:"minimumRecommendation,omitempty"`
	Mode                  *string                `json:"mode,omitempty"`

	Window  *domain.MaintenanceWindow     `json:"window,omitempty"`
	Limits  *domain.UpdateLimits          `json:"limits,omitempty"`
	Failure *domain.UpdateFailureHandling `json:"failure,omitempty"`
}

// updatePolicyListResponse is the policy listing.
type updatePolicyListResponse struct {
	Items      []domain.UpdatePolicy `json:"items"`
	Pagination Pagination            `json:"pagination"`
}

// handleUpdatePolicies lists automation rules.
func (s *Server) handleUpdatePolicies(w http.ResponseWriter, r *http.Request) {
	if s.updatePolicyUnavailable(w, r) {
		return
	}

	query, err := parseUpdatePolicyQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	items, total, err := s.updatePolicies.List(r.Context(), query.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "update policy list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, updatePolicyListResponse{
		Items:      items,
		Pagination: newPagination(query.Page, query.PageSize, total),
	})
}

// handleUpdatePolicyDetail returns one rule.
func (s *Server) handleUpdatePolicyDetail(w http.ResponseWriter, r *http.Request) {
	if s.updatePolicyUnavailable(w, r) {
		return
	}
	policyID, ok := s.updatePolicyID(w, r)
	if !ok {
		return
	}

	result, err := s.updatePolicies.Get(r.Context(), policyID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "update policy not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "update policy load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, result)
}

// handleUpdatePolicyCreate stores a new rule.
func (s *Server) handleUpdatePolicyCreate(w http.ResponseWriter, r *http.Request) {
	if s.updatePolicyUnavailable(w, r) {
		return
	}

	var body updatePolicyRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	// A create must supply the fields that decide what the policy DOES. An
	// omitted mode or strategy on a create is a caller mistake, and defaulting
	// either would mean HarborMaster choosing how far an unattended change may
	// go on somebody's behalf.
	if body.Name == nil || body.Strategy == nil || body.Mode == nil {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"name, strategy and mode are required")
		return
	}
	// The selector is required for every scope that is DECIDED by one. A policy
	// scoped to all eligible containers has nothing to put in it -- the scope
	// already said what it reaches -- and validation refuses one that does.
	//
	// Read from the body rather than from the assembled policy, so "the caller
	// did not send a scope" and "the caller sent selector" stay distinguishable
	// here. Absent scope means `selector`, which keeps every existing client
	// working unchanged.
	scope := domain.ScopeSelector
	if body.Scope != nil {
		scope = domain.UpdateScope(strings.TrimSpace(*body.Scope))
	}
	if scope != domain.ScopeAllEligible && body.Selector == nil {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"selector is required unless scope is allEligible")
		return
	}

	policy := domain.UpdatePolicy{
		Name:     *body.Name,
		Scope:    scope,
		Strategy: domain.UpdateStrategy(*body.Strategy),
		Mode:     domain.AutomationMode(*body.Mode),
		// A rule an administrator just wrote is one they want in force.
		// Explicitly disabling it stays possible.
		Enabled: true,
		// The stricter of the two automatable verdicts. A policy that automates
		// changes the planner flagged for caution must say so.
		MinimumRecommendation: domain.RecommendProceed,
		// No window means no window. Stated explicitly rather than implied by
		// empty fields: "deliberately unrestricted" is an intention an operator
		// should have to express.
		Window: domain.MaintenanceWindow{AlwaysOpen: true},
	}

	if body.Selector != nil {
		policy.Selector = *body.Selector
	}
	if body.Description != nil {
		policy.Description = *body.Description
	}
	if body.Enabled != nil {
		policy.Enabled = *body.Enabled
	}
	if body.Priority != nil {
		policy.Priority = *body.Priority
	}
	if body.MinimumRecommendation != nil {
		policy.MinimumRecommendation = domain.Recommendation(*body.MinimumRecommendation)
	}
	if body.Window != nil {
		policy.Window = *body.Window
	}
	if body.Limits != nil {
		policy.Limits = *body.Limits
	}
	if body.Failure != nil {
		policy.Failure = *body.Failure
	}

	result, err := s.updatePolicies.Create(r.Context(), policy, s.actorFrom(r))
	switch {
	case errors.Is(err, store.ErrUpdatePolicyNameTaken):
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"an update policy with that name already exists")
		return
	case err != nil:
		if s.writeUpdatePolicyValidationError(w, r, err) {
			return
		}
		s.logger.ErrorContext(r.Context(), "update policy create failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	w.Header().Set("Location", APIPrefix+"/update-policies/"+result.Policy.PolicyID)
	writeJSON(w, r, s.logger, http.StatusCreated, result)
}

// handleUpdatePolicyUpdate applies a partial change.
func (s *Server) handleUpdatePolicyUpdate(w http.ResponseWriter, r *http.Request) {
	if s.updatePolicyUnavailable(w, r) {
		return
	}
	policyID, ok := s.updatePolicyID(w, r)
	if !ok {
		return
	}

	var body updatePolicyRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	change := store.UpdatePolicyChange{
		Name:        body.Name,
		Description: body.Description,
		Enabled:     body.Enabled,
		Priority:    body.Priority,
		Selector:    body.Selector,
		// Scope is set below, with the other enums.
		Window:  body.Window,
		Limits:  body.Limits,
		Failure: body.Failure,
	}
	// The four enums arrive as strings so an unknown value is a validation
	// error rather than an unmarshalling one, and are converted here.
	if body.Scope != nil {
		scope := domain.UpdateScope(strings.TrimSpace(*body.Scope))
		change.Scope = &scope
	}
	if body.Strategy != nil {
		strategy := domain.UpdateStrategy(*body.Strategy)
		change.Strategy = &strategy
	}
	if body.MinimumRecommendation != nil {
		recommendation := domain.Recommendation(*body.MinimumRecommendation)
		change.MinimumRecommendation = &recommendation
	}
	if body.Mode != nil {
		mode := domain.AutomationMode(*body.Mode)
		change.Mode = &mode
	}

	result, err := s.updatePolicies.Update(r.Context(), policyID, change, s.actorFrom(r))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "update policy not found")
		return
	case errors.Is(err, store.ErrUpdatePolicyNameTaken):
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"an update policy with that name already exists")
		return
	case err != nil:
		if s.writeUpdatePolicyValidationError(w, r, err) {
			return
		}
		s.logger.ErrorContext(r.Context(), "update policy edit failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, result)
}

// handleUpdatePolicyDelete withdraws a rule.
//
// Archives rather than deletes. Automation decisions and pauses reference this
// row, and the record of what automation did must survive the rule being
// withdrawn.
func (s *Server) handleUpdatePolicyDelete(w http.ResponseWriter, r *http.Request) {
	if s.updatePolicyUnavailable(w, r) {
		return
	}
	policyID, ok := s.updatePolicyID(w, r)
	if !ok {
		return
	}

	err := s.updatePolicies.Archive(r.Context(), policyID, s.actorFrom(r))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "update policy not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "update policy archive failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// updatePolicyID reads and validates the path parameter.
func (s *Server) updatePolicyID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	if !domain.ValidUpdatePolicyID(raw) {
		// The same 404 an unknown id gets. A malformed id must not be
		// distinguishable from an absent one.
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "update policy not found")
		return "", false
	}
	return raw, true
}

// writeUpdatePolicyValidationError renders a field-level rejection, and reports
// whether it handled the error.
//
// The message names the FIELD and the CONSTRAINT, never the offending value:
// a response that reflected its input would be a way to make the API return
// attacker-chosen text.
func (s *Server) writeUpdatePolicyValidationError(
	w http.ResponseWriter,
	r *http.Request,
	err error,
) bool {
	var validation domain.PolicyValidationError
	if !errors.As(err, &validation) {
		return false
	}
	writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
		validation.Field+" "+validation.Message)
	return true
}

// Compile-time proof that the service satisfies the interface the handlers
// depend on. A change to either that breaks the match fails the build here,
// where the reason is obvious, rather than in the composition root.
var _ UpdatePolicyService = (*service.UpdatePolicyService)(nil)
