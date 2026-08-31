package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// One container's update behaviour (C2).
//
// GET reports what was asked for and what the engine will actually do, PUT
// records a choice, DELETE withdraws it.
//
// # These routes change no container
//
// They write one row in one table. The service behind them holds no Docker
// capability, so there is no path from this endpoint to a stop, a start or a
// recreation -- which is the property that makes a dropdown on a container page
// safe to offer at all.
//
// # Authorisation is the automation permission, unchanged
//
// Reading is `inventory:read`, because this is a fact about a container and it
// sits on the container's own page. Changing it is `automation:manage`, the
// same permission that governs update policies -- a preference composes onto a
// policy, and whoever may write the policy may write this.

// ContainerPreferenceService is the per-container behaviour capability the API
// depends on.
type ContainerPreferenceService interface {
	Behavior(ctx context.Context, containerID string) (service.ContainerUpdateBehavior, error)
	SetBehavior(ctx context.Context, containerID string, behavior domain.UpdateBehavior,
		actor service.Actor) (service.ContainerUpdateBehavior, error)
	ClearBehavior(ctx context.Context, containerID string,
		actor service.Actor) (service.ContainerUpdateBehavior, error)
}

// containerBehaviorRequest is a PUT body.
type containerBehaviorRequest struct {
	Behavior *string `json:"behavior"`
}

// containerPreferenceUnavailable writes the disabled response.
func (s *Server) containerPreferenceUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.containerPreferences != nil {
		return false
	}
	writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
		"per-container update behaviour is not configured")
	return true
}

// handleContainerBehavior reports one container's requested and effective
// update behaviour.
func (s *Server) handleContainerBehavior(w http.ResponseWriter, r *http.Request) {
	if s.containerPreferenceUnavailable(w, r) {
		return
	}
	result, err := s.containerPreferences.Behavior(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeContainerBehaviorError(w, r, err, "read")
		return
	}
	writeJSON(w, r, s.logger, http.StatusOK, result)
}

// handleContainerBehaviorSet records an operator's choice.
func (s *Server) handleContainerBehaviorSet(w http.ResponseWriter, r *http.Request) {
	if s.containerPreferenceUnavailable(w, r) {
		return
	}

	var body containerBehaviorRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}
	if body.Behavior == nil {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"behavior is required")
		return
	}

	// Normalised to the closed vocabulary before it reaches the service. An
	// unrecognised value is refused by name rather than stored and later
	// interpreted as something nobody chose.
	behavior := domain.NormaliseUpdateBehavior(*body.Behavior)
	result, err := s.containerPreferences.SetBehavior(
		r.Context(), r.PathValue("id"), behavior, s.actorFrom(r))
	if err != nil {
		s.writeContainerBehaviorError(w, r, err, "set")
		return
	}
	writeJSON(w, r, s.logger, http.StatusOK, result)
}

// handleContainerBehaviorClear withdraws the choice, returning the container to
// whatever policy governs it.
func (s *Server) handleContainerBehaviorClear(w http.ResponseWriter, r *http.Request) {
	if s.containerPreferenceUnavailable(w, r) {
		return
	}
	result, err := s.containerPreferences.ClearBehavior(
		r.Context(), r.PathValue("id"), s.actorFrom(r))
	if err != nil {
		s.writeContainerBehaviorError(w, r, err, "clear")
		return
	}
	writeJSON(w, r, s.logger, http.StatusOK, result)
}

// writeContainerBehaviorError maps a service failure to a response.
//
// An unknown container is 404 and an unknown behaviour is 400. Neither echoes
// the caller's value back, so a crafted id or behaviour cannot become response
// content.
func (s *Server) writeContainerBehaviorError(
	w http.ResponseWriter, r *http.Request, err error, verb string,
) {
	switch {
	case errors.Is(err, service.ErrContainerUnknown):
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "container not found")
	case errors.Is(err, service.ErrUnknownBehavior):
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest, err.Error())
	default:
		s.logger.ErrorContext(r.Context(), "container update behaviour "+verb+" failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
	}
}
