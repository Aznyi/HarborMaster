package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Automation readiness: "if I saved this policy, what could it do right now".
//
// # Why this is a POST that changes nothing
//
// The question is about a policy CONFIGURATION, and a configuration does not
// fit in a query string: it carries a selector, a window, limits and a failure
// plan. So the request needs a body, and a body needs POST.
//
// That makes the method a lie about the semantics, and the compensation is that
// every guard a POST gets is applied anyway: CSRF, the request-size bound, the
// strict decoder, and the write rate limiter. The permission is deliberately
// NOT a write one -- see the route table.
//
// # What the caller may say, and what it may not
//
// The body is the SAME shape the create endpoint takes, deliberately: an
// operator previewing a policy and then saving it must be previewing the thing
// they save. It carries policy CONFIGURATION only.
//
// There is nowhere in it to put a recommendation, a verdict, an update type, an
// eligibility, a dependency state, a snapshot status, or a risk score. Those are
// facts about the estate, HarborMaster establishes every one of them itself, and
// a caller that could supply one could manufacture an answer.

// automationReadinessRequest is a candidate policy to measure.
//
// Embeds the create shape rather than restating it, so a field added there
// cannot be silently unpreviewable here.
type automationReadinessRequest struct {
	updatePolicyRequest

	// PolicyID names the stored policy this configuration is an edit OF, when
	// there is one. It makes the preview replace that policy in the set rather
	// than compete with it, so an operator editing a rule sees the edit instead
	// of seeing their own policy outrank itself.
	//
	// Absent means an unsaved policy. It is an IDENTIFIER of a record
	// HarborMaster re-reads, never a fact about the estate.
	PolicyID *string `json:"policyId,omitempty"`
}

// automationReadinessResponse is the answer.
//
// The report plus the reasons, and nothing from the planner's internals: a
// caller gets counts, closed-vocabulary reasons, and HarborMaster's own
// sentences for them.
type automationReadinessResponse struct {
	Readiness domain.AutomationReadinessReport `json:"readiness"`
	// Engine reports whether automation would actually run. A policy can be
	// perfectly ready on an estate whose engine is switched off, and an
	// operator reading a count needs to know which of those they are looking at.
	Engine bool `json:"engineEnabled"`
}

// handleAutomationReadiness previews one policy against the current estate.
//
// Writes NOTHING. No run row, no decision row, no policy row, no acquisition,
// no execution, no Docker call. See service.AutomationService.Readiness and the
// architecture guards that hold it.
func (s *Server) handleAutomationReadiness(w http.ResponseWriter, r *http.Request) {
	if s.automation == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeUnavailable,
			"automation is not available in this deployment")
		return
	}

	var body automationReadinessRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	// The same requirement the create endpoint applies, for the same reason:
	// defaulting a mode or a ceiling would mean HarborMaster deciding how far
	// an unattended change may go on the caller's behalf -- and then reporting
	// a count for a policy the operator never described.
	if body.Strategy == nil || body.Mode == nil {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"strategy and mode are required")
		return
	}

	candidate, ok := s.readinessCandidate(w, r, body)
	if !ok {
		return
	}

	report, _, err := s.automation.Readiness(r.Context(), &candidate)
	switch {
	case errors.Is(err, service.ErrAutomationDisabled):
		// Readable() is false: the engine is off AND nothing may be read.
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeUnavailable,
			"automation is not available in this deployment")
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "automation readiness failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal,
			"internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, automationReadinessResponse{
		Readiness: report,
		Engine:    s.automation.Enabled(),
	})
}

// readinessCandidate assembles and validates the policy to measure.
//
// Runs the SAME Normalise and Validate the persistence path runs. A
// configuration the API would refuse to store must not be previewable either:
// otherwise an operator reads a count for a policy they cannot save.
func (s *Server) readinessCandidate(
	w http.ResponseWriter,
	r *http.Request,
	body automationReadinessRequest,
) (domain.UpdatePolicy, bool) {
	scope := domain.ScopeSelector
	if body.Scope != nil {
		scope = domain.UpdateScope(strings.TrimSpace(*body.Scope))
	}
	if scope != domain.ScopeAllEligible && body.Selector == nil {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"selector is required unless scope is allEligible")
		return domain.UpdatePolicy{}, false
	}

	candidate := domain.UpdatePolicy{
		// A name only so validation has one; it is never stored and never
		// rendered. The preview is about behaviour, not about naming.
		Name:                  "readiness preview",
		Scope:                 scope,
		Strategy:              domain.UpdateStrategy(*body.Strategy),
		Mode:                  domain.AutomationMode(*body.Mode),
		Enabled:               true,
		MinimumRecommendation: domain.RecommendProceed,
		Window:                domain.MaintenanceWindow{AlwaysOpen: true},
		// The sentinel. Not a valid generated identifier, so it can never be
		// persisted or matched against a stored row, and it loses every
		// identifier tie-break -- so a candidate never takes a container an
		// existing policy already governs at the same priority and breadth.
		PolicyID: domain.AutomationReadinessCandidatePolicyID,
	}

	if body.PolicyID != nil {
		id := strings.TrimSpace(*body.PolicyID)
		if !domain.ValidUpdatePolicyID(id) {
			writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
				"policyId is not a valid update policy identifier")
			return domain.UpdatePolicy{}, false
		}
		candidate.PolicyID = id
	}

	if body.Name != nil {
		candidate.Name = *body.Name
	}
	if body.Selector != nil {
		candidate.Selector = *body.Selector
	}
	if body.Description != nil {
		candidate.Description = *body.Description
	}
	if body.Enabled != nil {
		candidate.Enabled = *body.Enabled
	}
	if body.Priority != nil {
		candidate.Priority = *body.Priority
	}
	if body.MinimumRecommendation != nil {
		candidate.MinimumRecommendation = domain.Recommendation(*body.MinimumRecommendation)
	}
	if body.Window != nil {
		candidate.Window = *body.Window
	}
	if body.Limits != nil {
		candidate.Limits = *body.Limits
	}
	if body.Failure != nil {
		candidate.Failure = *body.Failure
	}

	candidate.Normalise()
	if err := candidate.Validate(domain.DefaultUpdatePolicyLimits()); err != nil {
		if s.writeUpdatePolicyValidationError(w, r, err) {
			return domain.UpdatePolicy{}, false
		}
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the policy configuration is not valid")
		return domain.UpdatePolicy{}, false
	}

	return candidate, true
}
