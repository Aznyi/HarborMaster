package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/Aznyi/HarborMaster/internal/service"
)

// The automatic-updates switch, as three ordinary policy operations.
//
// GET reports the state, POST turns it on, DELETE turns it off. They are
// deliberately thin: every decision lives in UpdatePolicyService, which writes
// the same rows through the same validation an operator's own policy goes
// through, so there is nothing here that can authorise more than the policy
// routes beside it already do.
//
// Authorisation is the SAME permission the policy routes use --
// `automation:read` to look, `automation:manage` to change -- declared in the
// route table like every other route. There is no switch-specific permission,
// because the switch is not a switch-specific capability: it writes an update
// policy, and whoever may write update policies may write this one.

// simpleUpdatesResponse is the switch's state.
//
// `engineEnabled` is reported alongside, because "the switch is off" and "this
// deployment cannot run automation at all" are different problems with
// different remedies, and a UI that cannot tell them apart offers a toggle that
// silently does nothing.
type simpleUpdatesResponse struct {
	service.SimpleUpdatesState
	// EngineEnabled reports HARBORMASTER_AUTOMATION_ENABLED. When false the
	// switch cannot take effect however it is set, and the UI must say so
	// rather than render a control that lies.
	EngineEnabled bool `json:"engineEnabled"`
	// EngineVariable names the environment variable that turns the engine on,
	// so the message can be specific without the frontend hardcoding it.
	EngineVariable string `json:"engineVariable"`
}

// handleSimpleUpdates reports whether automatic updates are on.
func (s *Server) handleSimpleUpdates(w http.ResponseWriter, r *http.Request) {
	if s.updatePolicyUnavailable(w, r) {
		return
	}

	state, err := s.updatePolicies.SimpleUpdates(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "simple updates read failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	writeJSON(w, r, s.logger, http.StatusOK, simpleUpdatesResponse{
		SimpleUpdatesState: state,
		EngineEnabled:      s.automation != nil,
		EngineVariable:     simpleUpdatesEngineVariable,
	})
}

// simpleUpdatesEngineVariable is the setting that has to be on for the engine
// to run at all. Named here so one string reaches the operator.
const simpleUpdatesEngineVariable = "HARBORMASTER_AUTOMATION_ENABLED"

// handleSimpleUpdatesEnable turns automatic updates on.
//
// Refuses when the engine is not configured. A switch that reported success
// while the scheduler was absent would be the exact lie this endpoint exists to
// avoid: the policy would be written, nothing would ever read it, and the
// operator would be told automatic updates were on.
func (s *Server) handleSimpleUpdatesEnable(w http.ResponseWriter, r *http.Request) {
	if s.updatePolicyUnavailable(w, r) {
		return
	}
	if s.automation == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"automatic updates cannot be turned on because the automation engine is "+
				"not enabled for this deployment; set "+simpleUpdatesEngineVariable+
				"=true and restart HarborMaster")
		return
	}

	result, err := s.updatePolicies.EnableSimpleUpdates(r.Context(), s.actorFrom(r))
	if err != nil {
		if errors.Is(err, service.ErrSimpleUpdatesArchived) {
			writeError(w, r, s.logger, http.StatusConflict, CodeConflict, err.Error())
			return
		}
		s.logger.ErrorContext(r.Context(), "simple updates enable failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	writeJSON(w, r, s.logger, http.StatusOK, result)
}

// handleSimpleUpdatesDisable turns automatic updates off.
//
// Disables the managed policy and NOTHING else. Policies an operator wrote are
// not read, edited or withdrawn; no container is touched; no history is
// removed. Turning off a switch that was never on succeeds: the caller asked
// for off and off is what they have.
func (s *Server) handleSimpleUpdatesDisable(w http.ResponseWriter, r *http.Request) {
	if s.updatePolicyUnavailable(w, r) {
		return
	}

	result, err := s.updatePolicies.DisableSimpleUpdates(r.Context(), s.actorFrom(r))
	if err != nil {
		s.logger.ErrorContext(r.Context(), "simple updates disable failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	writeJSON(w, r, s.logger, http.StatusOK, result)
}
