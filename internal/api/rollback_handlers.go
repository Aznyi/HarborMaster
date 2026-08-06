package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The rollback endpoints.
//
// # These change something RUNNING, and they change it back
//
// `POST /rollbacks` stops the container a recreation put in place and starts
// the one it replaced. Alongside `POST /executions`, one of two endpoints in
// this API that interrupt a service.
//
// The surface is as small as the recreation's, and narrower in one respect:
//
//   - **One field.** The request body carries an EXECUTION ID and an optional
//     idempotency key. No container, no name, no image, no snapshot, no Docker
//     option. An operator approves undoing a specific recreation; they do not
//     compose a rollback.
//   - **Every identity comes from HarborMaster's own record.** The two
//     container ids, the production name, and the image identity are read from
//     the execution record and re-verified against the live host before
//     anything moves. There is no field on any type in this file that could
//     carry a container id from a caller.
//   - **The handler cannot mutate.** It asks the service, and the service
//     revalidates everything before touching Docker. An architecture test fails
//     the build if this package so much as names the rollback interface.
//   - **Nothing is removed.** A rollback stops and parks the replacement and
//     leaves it there. There is no delete endpoint and no capability behind
//     one.
//
// # Cancel is honest about its limits
//
// `POST /rollbacks/{id}/cancel` works only BEFORE the first mutation. Once the
// replacement has been stopped, the rollback must reach a recorded conclusion,
// and the endpoint answers 409 rather than pretending otherwise.

// RollbackService is the rollback capability the API depends on.
//
// A narrow interface rather than *service.RollbackService, so the handlers stay
// testable and the surface the API can reach is visible in one place. Note what
// is ABSENT: nothing that stops, starts, renames, or removes a container, and
// nothing that edits a stored record.
type RollbackService interface {
	Request(ctx context.Context, request service.RollbackRequest) (domain.Rollback, error)
	Cancel(ctx context.Context, rollbackID string) (domain.Rollback, error)
	Get(ctx context.Context, rollbackID string) (domain.Rollback, []domain.RollbackEvent, error)
	List(ctx context.Context, filter store.RollbackFilter) ([]domain.Rollback, int, error)
	Summary(ctx context.Context) (domain.RollbackSummary, error)
	Eligible(ctx context.Context, executionID string) (domain.RollbackEligibility, error)
	Enabled() bool
}

// rollbackUnavailable writes the disabled response, and reports whether it did.
func (s *Server) rollbackUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.rollbacks != nil {
		return false
	}
	writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
		"manual rollback is not configured")
	return true
}

// rollbackListResponse is the rollback listing with the estate aggregate.
type rollbackListResponse struct {
	Items      []domain.Rollback      `json:"items"`
	Pagination Pagination             `json:"pagination"`
	Summary    domain.RollbackSummary `json:"summary"`
}

// handleRollbacks lists rollbacks.
func (s *Server) handleRollbacks(w http.ResponseWriter, r *http.Request) {
	if s.rollbackUnavailable(w, r) {
		return
	}

	query, err := parseRollbackQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	items, total, err := s.rollbacks.List(r.Context(), query.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "rollback list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	summary, err := s.rollbacks.Summary(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "rollback summary failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, rollbackListResponse{
		Items:      items,
		Pagination: newPagination(query.Page, query.PageSize, total),
		Summary:    summary,
	})
}

// rollbackDetailResponse is one rollback with its audit trail.
type rollbackDetailResponse struct {
	Rollback domain.Rollback        `json:"rollback"`
	Events   []domain.RollbackEvent `json:"events"`
}

// handleRollbackDetail returns one rollback.
func (s *Server) handleRollbackDetail(w http.ResponseWriter, r *http.Request) {
	if s.rollbackUnavailable(w, r) {
		return
	}
	rollbackID, ok := s.rollbackID(w, r)
	if !ok {
		return
	}

	rollback, events, err := s.rollbacks.Get(r.Context(), rollbackID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "rollback not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "rollback load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, rollbackDetailResponse{
		Rollback: rollback,
		Events:   events,
	})
}

// rollbackRequestBody is the create request.
//
// Two fields, and neither names a container. Everything about what will happen
// is derived from the execution record, which HarborMaster wrote itself.
type rollbackRequestBody struct {
	// ExecutionID is the recreation to undo.
	ExecutionID string `json:"executionId"`
	// RequestKey is an optional idempotency key, so a retried request -- or a
	// double-clicked button -- does not start a second rollback.
	RequestKey string `json:"requestKey,omitempty"`
}

// handleRollbackCreate requests a rollback.
//
// # What this endpoint can cause
//
// One container, which a recreation replaced, stopped and swapped back for the
// original that recreation preserved. Nothing else. It cannot create a
// container, cannot remove one, and cannot act on anything the named execution
// did not touch.
//
// Asynchronous: it answers 202 with the recorded request. A synchronous
// response would hold a request open for the length of a stop, two renames, a
// start, and a health wait.
//
// The FULL preflight runs synchronously before the 202, so an operator gets a
// real refusal rather than a queued job that fails a moment later -- and it
// runs AGAIN immediately before the first mutation.
func (s *Server) handleRollbackCreate(w http.ResponseWriter, r *http.Request) {
	if s.rollbackUnavailable(w, r) {
		return
	}

	var body rollbackRequestBody
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	executionID := strings.TrimSpace(body.ExecutionID)
	if !domain.ValidExecutionID(executionID) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"a well-formed execution id is required")
		return
	}
	requestKey := strings.TrimSpace(body.RequestKey)
	if !validRequestKey(requestKey) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the request key must be at most 100 printable ASCII characters")
		return
	}

	rollback, err := s.rollbacks.Request(r.Context(), service.RollbackRequest{
		ExecutionID: executionID,
		RequestKey:  requestKey,
		// Carried onto the record so the OUTCOME can be attributed by a worker
		// that runs minutes from now with no request and no session.
		RequestedBy: s.requesterFrom(r),
	})
	if err != nil {
		// A refusal is recorded too. See Server.auditRefusal.
		var refused service.RollbackRefusedError
		if errors.As(err, &refused) {
			s.auditRefusal(r, domain.AuditRollbackRequested,
				domain.AuditTargetExecution, executionID, string(refused.Refusal))
		}
		s.writeRollbackError(w, r, err)
		return
	}

	s.auditWrite(r, domain.AuditRollbackRequested, domain.AuditTargetRollback,
		rollback.RollbackID, rollback.ContainerName,
		"rollback requested for recreation "+rollback.ExecutionID)

	writeJSON(w, r, s.logger, http.StatusAccepted, rollback)
}

// handleRollbackCancel stops a rollback that has not yet changed anything.
func (s *Server) handleRollbackCancel(w http.ResponseWriter, r *http.Request) {
	if s.rollbackUnavailable(w, r) {
		return
	}
	rollbackID, ok := s.rollbackID(w, r)
	if !ok {
		return
	}

	rollback, err := s.rollbacks.Cancel(r.Context(), rollbackID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "rollback not found")
		return
	}
	if err != nil {
		if errors.Is(err, service.ErrRollbackNotCancellable) {
			// 409 rather than 400. The request was well formed and would have
			// been honoured a moment earlier; what changed is the world.
			writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
				"this rollback has already begun changing the host and must run to a recorded conclusion")
			return
		}
		s.logger.ErrorContext(r.Context(), "rollback cancel failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	s.auditWrite(r, domain.AuditRollbackCancelled, domain.AuditTargetRollback,
		rollback.RollbackID, rollback.ContainerName, "cancelled before the host was changed")

	writeJSON(w, r, s.logger, http.StatusOK, rollback)
}

// rollbackID reads and validates the path parameter.
func (s *Server) rollbackID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	if !domain.ValidRollbackID(raw) {
		// The VALUE is never echoed: it is caller input, and an error message
		// is not a place to reflect it.
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"a well-formed rollback id is required")
		return "", false
	}
	return raw, true
}

// writeRollbackError maps a service failure onto a response.
//
// A REFUSAL is reported with the specific check that said no, because "the
// original is gone" and "somebody else is already rolling this back" call for
// completely different responses from an operator.
func (s *Server) writeRollbackError(w http.ResponseWriter, r *http.Request, err error) {
	var refused service.RollbackRefusedError
	if errors.As(err, &refused) {
		status := http.StatusConflict
		code := CodeConflict
		switch refused.Refusal {
		case domain.RollbackRefusalDisabled:
			status = http.StatusServiceUnavailable
			code = CodeDisabled
		case domain.RollbackRefusalExecutionMissing:
			status = http.StatusNotFound
			code = CodeNotFound
		case domain.RollbackRefusalDockerUnavailable:
			status = http.StatusServiceUnavailable
			code = CodeUnavailable
		case domain.RollbackRefusalLimit:
			// Retryable: the same request succeeds once a slot frees. A
			// conflict code would tell a client to stop trying.
			status = http.StatusTooManyRequests
			code = CodeRateLimited
		}

		writeJSON(w, r, s.logger, status, rollbackRefusalResponse{
			Error: ErrorBody{
				Code:    code,
				Message: refused.Refusal.Explain(),
			},
			Refusal:   refused.Refusal,
			RequestID: RequestIDFrom(r.Context()),
		})
		return
	}

	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "execution not found")
		return
	}

	s.logger.ErrorContext(r.Context(), "rollback request failed", slog.String("error", err.Error()))
	writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
}

// rollbackRefusalResponse carries the refusal alongside the standard error.
//
// The extra field is what lets a UI render the specific remedy rather than a
// generic conflict. It is a closed vocabulary, so it is safe to branch on.
type rollbackRefusalResponse struct {
	Error     ErrorBody              `json:"error"`
	Refusal   domain.RollbackRefusal `json:"refusal"`
	RequestID string                 `json:"requestId,omitempty"`
}

// --------------------------------------------------------------- querying --

// rollbackQuery is the validated listing query.
type rollbackQuery struct {
	ExecutionID    string
	States         []domain.RollbackState
	Failures       []domain.RollbackFailure
	ActiveOnly     bool
	NeedsAttention bool

	Page     int
	PageSize int
}

// filter projects the query onto the store filter.
func (q rollbackQuery) filter() store.RollbackFilter {
	return store.RollbackFilter{
		ExecutionID:    q.ExecutionID,
		States:         q.States,
		Failures:       q.Failures,
		ActiveOnly:     q.ActiveOnly,
		NeedsAttention: q.NeedsAttention,
		Page: store.Page{
			Limit:  q.PageSize,
			Offset: (q.Page - 1) * q.PageSize,
		},
	}
}

// parseRollbackQuery reads and validates the listing parameters.
//
// Every value is checked against a closed vocabulary defined in the domain
// package, or is a bounded integer, or is an identifier validated by shape.
// Nothing here becomes SQL text.
func parseRollbackQuery(query url.Values) (rollbackQuery, error) {
	var parsed rollbackQuery

	page, pageSize, err := parsePage(query)
	if err != nil {
		return parsed, err
	}
	parsed.Page, parsed.PageSize = page, pageSize

	if raw := strings.TrimSpace(query.Get("executionId")); raw != "" {
		if !domain.ValidExecutionID(raw) {
			return parsed, invalidParam("executionId", "a well-formed execution id")
		}
		parsed.ExecutionID = raw
	}

	states, err := parseVocabulary(query, "state", domain.ValidRollbackState,
		"a known rollback state")
	if err != nil {
		return parsed, err
	}
	for _, value := range states {
		parsed.States = append(parsed.States, domain.RollbackState(value))
	}

	failures, err := parseVocabulary(query, "failure", domain.ValidRollbackFailure,
		"a known rollback failure")
	if err != nil {
		return parsed, err
	}
	for _, value := range failures {
		parsed.Failures = append(parsed.Failures, domain.RollbackFailure(value))
	}

	for _, flag := range []struct {
		name string
		into *bool
	}{
		{"activeOnly", &parsed.ActiveOnly},
		{"needsAttention", &parsed.NeedsAttention},
	} {
		raw := strings.TrimSpace(query.Get(flag.name))
		if raw == "" {
			continue
		}
		switch raw {
		case "true":
			*flag.into = true
		case "false":
			*flag.into = false
		default:
			return parsed, invalidParam(flag.name, "true or false")
		}
	}

	return parsed, nil
}
