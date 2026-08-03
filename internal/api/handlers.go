package api

import (
	"context"
	"net/http"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/version"
)

// HealthChecker is the application capability behind GET /api/v1/health.
// The API depends on this interface rather than *service.HealthService so the
// handlers can be tested without a database or a Docker daemon.
type HealthChecker interface {
	Check(ctx context.Context) domain.HealthReport
}

// handleHealth reports dependency reachability.
//
// It always returns 200. The endpoint answering at all is the liveness signal;
// the body carries the readiness detail. Returning 503 here would make the
// frontend unable to distinguish "HarborMaster is down" from "Docker is down",
// which are very different things for an operator.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	report := s.health.Check(r.Context())
	writeJSON(w, r, s.logger, http.StatusOK, report)
}

// handleVersion reports build metadata.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, s.logger, http.StatusOK, version.Get())
}

// handleAPINotFound answers unknown /api/ routes with JSON.
//
// Without it the SPA fallback would return index.html for a mistyped API path,
// and a client would parse HTML as JSON.
func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "endpoint not found")
}

// handleMethodNotAllowed answers a known path reached with the wrong method.
//
// It is registered per endpoint because the catch-all "/api/" pattern would
// otherwise claim those requests and report 404, hiding the fact that the path
// exists but is read-only.
func (s *Server) handleMethodNotAllowed(allowed string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allowed)
		writeError(w, r, s.logger, http.StatusMethodNotAllowed, CodeMethodNotAllwd, "method not allowed")
	}
}
