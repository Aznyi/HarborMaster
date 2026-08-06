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

// The container recreation endpoints.
//
// # These are the only endpoints that change something RUNNING
//
// The acquisition endpoints write to the image store, which affects nothing
// that is serving. `POST /executions` stops a container and replaces it.
//
// The surface is deliberately as small as it is possible to make it:
//
//   - **One field.** The request body carries an ACQUISITION ID and an optional
//     idempotency key. No container, no image, no digest, no tag, no command,
//     no mount, no capability, no privilege flag, no timeout, no force. An
//     operator approves an assessment; they do not compose a container.
//   - **Single use.** An acquisition that has already been executed is refused,
//     and there is no override parameter. A second recreation needs a fresh
//     plan and a fresh acquisition, so what is applied has been assessed
//     against the world as it is now.
//   - **The handler cannot mutate.** It asks the service, and the service
//     revalidates every prerequisite before touching Docker. An architecture
//     test fails the build if this package so much as names the mutation
//     interface, because a handler that could reach it would bypass the
//     preflight -- which is the entire safety model.
//   - **No rollback endpoint, and no capability behind one.** A failed
//     recreation is settled by an operator, using the recovery plan the record
//     carries.
//
// # Cancel is honest about its limits
//
// `POST /executions/{id}/cancel` works only BEFORE the first mutation. Once the
// original has been stopped, the recreation must reach a recorded conclusion,
// and the endpoint answers 409 rather than pretending otherwise.

// ExecutionService is the recreation capability the API depends on.
//
// A narrow interface rather than *service.ExecutionService, so the handlers
// stay testable and so the surface the API can reach is visible in one place.
// Note what is ABSENT: nothing that stops, creates, starts, renames, or removes
// a container, and nothing that edits a stored record.
type ExecutionService interface {
	Request(ctx context.Context, request service.ExecutionRequest) (domain.Execution, error)
	Cancel(ctx context.Context, executionID string) (domain.Execution, error)
	Get(ctx context.Context, executionID string) (domain.Execution, []domain.ExecutionEvent, error)
	List(ctx context.Context, filter store.ExecutionFilter) ([]domain.Execution, int, error)
	Summary(ctx context.Context) (domain.ExecutionSummary, error)
	Eligible(ctx context.Context, acquisitionID string) (domain.ExecutionTarget, domain.ExecutionRefusal, error)
	Enabled() bool
}

// executionUnavailable writes the disabled response, and reports whether it
// did.
func (s *Server) executionUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.executions != nil {
		return false
	}
	writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
		"container recreation is not configured")
	return true
}

// executionListResponse is the execution listing, with the estate aggregate.
type executionListResponse struct {
	Items      []domain.Execution      `json:"items"`
	Pagination Pagination              `json:"pagination"`
	Summary    domain.ExecutionSummary `json:"summary"`
}

// handleExecutions lists recreations.
func (s *Server) handleExecutions(w http.ResponseWriter, r *http.Request) {
	if s.executionUnavailable(w, r) {
		return
	}

	query, err := parseExecutionQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	items, total, err := s.executions.List(r.Context(), query.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "execution list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	summary, err := s.executions.Summary(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "execution summary failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, executionListResponse{
		Items:      items,
		Pagination: newPagination(query.Page, query.PageSize, total),
		Summary:    summary,
	})
}

// executionDetailResponse is one execution with its audit trail.
type executionDetailResponse struct {
	Execution domain.Execution        `json:"execution"`
	Events    []domain.ExecutionEvent `json:"events"`
}

// handleExecutionDetail returns one execution.
func (s *Server) handleExecutionDetail(w http.ResponseWriter, r *http.Request) {
	if s.executionUnavailable(w, r) {
		return
	}
	executionID, ok := s.executionID(w, r)
	if !ok {
		return
	}

	execution, events, err := s.executions.Get(r.Context(), executionID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "execution not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "execution load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, executionDetailResponse{
		Execution: execution,
		Events:    events,
	})
}

// executionRequestBody is the create request.
//
// Two fields, and neither names a container or an image. Everything about what
// will happen is derived from the acquisition, which was itself derived from a
// plan -- which is what makes this endpoint unable to recreate anything an
// operator did not approve through the planning and acquisition path.
type executionRequestBody struct {
	// AcquisitionID is the succeeded download to apply.
	AcquisitionID string `json:"acquisitionId"`
	// RequestKey is an optional idempotency key, so a retried request -- or a
	// double-clicked button -- does not start a second recreation.
	RequestKey string `json:"requestKey,omitempty"`
}

// handleExecutionCreate requests a container recreation.
//
// # What this endpoint can cause
//
// One container, which a current plan recommends changing, stopped and replaced
// with a container built from its own configuration and an image that is
// already on this host and was already verified. Nothing else.
//
// Asynchronous: it answers 202 with the recorded request. A synchronous
// response would hold an unauthenticated request open for the length of a stop,
// a create, a start, and a health wait.
//
// The FULL preflight runs synchronously before the 202, so an operator gets a
// real refusal rather than a queued job that fails a moment later -- and it
// runs AGAIN immediately before the first mutation.
func (s *Server) handleExecutionCreate(w http.ResponseWriter, r *http.Request) {
	if s.executionUnavailable(w, r) {
		return
	}
	// The same write guard every other state-changing endpoint uses: fetch
	// metadata, origin, JSON media type, size limit, rate limit.
	if err := s.guardWrite(r); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	var body executionRequestBody
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	acquisitionID := strings.TrimSpace(body.AcquisitionID)
	if !validAcquisitionID(acquisitionID) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"a well-formed acquisition id is required")
		return
	}
	requestKey := strings.TrimSpace(body.RequestKey)
	if !validRequestKey(requestKey) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the request key must be at most 100 printable ASCII characters")
		return
	}

	execution, err := s.executions.Request(r.Context(), service.ExecutionRequest{
		AcquisitionID: acquisitionID,
		RequestKey:    requestKey,
	})
	if err != nil {
		s.writeExecutionError(w, r, err)
		return
	}

	writeJSON(w, r, s.logger, http.StatusAccepted, execution)
}

// handleExecutionCancel stops a recreation that has not yet changed anything.
func (s *Server) handleExecutionCancel(w http.ResponseWriter, r *http.Request) {
	if s.executionUnavailable(w, r) {
		return
	}
	// The same write guard, minus a body: cancel carries none.
	if err := s.guardWrite(r); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}
	executionID, ok := s.executionID(w, r)
	if !ok {
		return
	}

	execution, err := s.executions.Cancel(r.Context(), executionID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "execution not found")
		return
	}
	if err != nil {
		var refused service.ErrExecutionRefused
		if errors.As(err, &refused) {
			// Past the mutation point, or already finished. Conflict rather
			// than a bad request: the request was well formed and the state was
			// not.
			//
			// The message states WHY rather than just refusing, because "you
			// cannot cancel this" invites an operator to try harder, while "the
			// container has already been stopped" tells them what to do next.
			writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
				"this recreation can no longer be cancelled; it has passed the point "+
					"where stopping would leave the host in an unrecorded state")
			return
		}
		s.logger.ErrorContext(r.Context(), "execution cancel failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, execution)
}

// writeExecutionError maps a service failure onto a status code.
//
// A REFUSAL is reported as 409 with the specific check that said no, because it
// is a statement about the world rather than about the request: the plan is
// superseded, the image is gone, the daemon is down. The operator's request was
// well formed; the situation is not what they thought.
//
// The message is HarborMaster's own fixed phrase for that refusal. No daemon
// text, no registry text, and no caller input reaches it.
func (s *Server) writeExecutionError(w http.ResponseWriter, r *http.Request, err error) {
	var refused service.ErrExecutionRefused
	if errors.As(err, &refused) {
		status := http.StatusConflict
		code := CodeConflict

		switch refused.Refusal {
		case domain.ExecutionRefusalDisabled:
			status, code = http.StatusServiceUnavailable, CodeDisabled
		case domain.ExecutionRefusalAcquisitionMissing,
			domain.ExecutionRefusalPlanMissing,
			domain.ExecutionRefusalContainerMissing:
			status, code = http.StatusNotFound, CodeNotFound
		case domain.ExecutionRefusalLimit:
			// Retry later, and say so with the status that means exactly that.
			status, code = http.StatusTooManyRequests, CodeConflict
		}

		s.writeExecutionRefusal(w, r, status, code, refused.Refusal)
		return
	}

	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "the acquisition was not found")
		return
	}

	s.logger.ErrorContext(r.Context(), "execution request failed", slog.String("error", err.Error()))
	writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
}

// executionRefusalResponse is the error envelope plus the specific check that
// refused.
//
// The extra field exists so a client can branch on WHICH check said no without
// parsing prose: "a newer plan exists" and "the daemon is down" call for
// different things from an operator, and both arrive as a 409.
type executionRefusalResponse struct {
	Error     ErrorBody               `json:"error"`
	Refusal   domain.ExecutionRefusal `json:"refusal"`
	RequestID string                  `json:"requestId,omitempty"`
}

// writeExecutionRefusal renders a preflight refusal.
func (s *Server) writeExecutionRefusal(
	w http.ResponseWriter,
	r *http.Request,
	status int,
	code ErrorCode,
	refusal domain.ExecutionRefusal,
) {
	writeJSON(w, r, s.logger, status, executionRefusalResponse{
		Error:     ErrorBody{Code: code, Message: refusal.Explain()},
		Refusal:   refusal,
		RequestID: RequestIDFrom(r.Context()),
	})
}

// ---------------------------------------------------------------- queries --

// executionID reads and validates the {id} path segment.
//
// Validated by SHAPE before it reaches a query. Execution ids are
// server-generated and have exactly one form, so anything else is a miss -- and
// refusing early keeps arbitrary caller text out of the database layer.
func (s *Server) executionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	if !domain.ValidExecutionID(raw) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the execution id is not well formed")
		return "", false
	}
	return raw, true
}

// executionQuery is a parsed and validated listing request.
type executionQuery struct {
	States         []domain.ExecutionState
	Failures       []domain.ExecutionFailure
	ActiveOnly     bool
	NeedsAttention bool

	Sort      string
	Ascending bool
	Page      int
	PageSize  int
}

// parseExecutionQuery reads and validates the listing parameters.
//
// Every parameter is validated against a CLOSED VOCABULARY defined in the
// domain package, or is a bounded integer. Nothing a caller sends becomes SQL
// text: a state or failure is matched against the domain's allowlist and then
// travels as a bound parameter, and the sort field selects a compile-time
// column constant from a map.
func parseExecutionQuery(query url.Values) (executionQuery, error) {
	// Newest first, with the state ranking putting work that needs attention
	// above everything else: an operator opening this page is asking "is
	// anything wrong".
	parsed := executionQuery{Sort: "requestedAt"}

	page, pageSize, err := parsePage(query)
	if err != nil {
		return parsed, err
	}
	parsed.Page, parsed.PageSize = page, pageSize

	if raw := strings.TrimSpace(query.Get("sort")); raw != "" {
		if !store.ValidExecutionSortField(raw) {
			return parsed, invalidParam("sort",
				"one of requestedAt, completedAt, state, container, id")
		}
		parsed.Sort = raw
	}

	if raw := strings.TrimSpace(query.Get("order")); raw != "" {
		switch raw {
		case "asc":
			parsed.Ascending = true
		case "desc":
			parsed.Ascending = false
		default:
			return parsed, invalidParam("order", "asc or desc")
		}
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

	states, err := parseVocabulary(query, "state", domain.ValidExecutionState,
		"one of queued, validating, capturing, creating, starting, verifying, "+
			"succeeded, failed, cancelled, expired")
	if err != nil {
		return parsed, err
	}
	for _, value := range states {
		parsed.States = append(parsed.States, domain.ExecutionState(value))
	}

	failures, err := parseVocabulary(query, "failure", validNonEmptyExecutionFailure,
		"one of preflight, capture, stop, rename, create, start, healthTimeout, "+
			"unhealthy, notStable, imageMismatch, preservation, network, "+
			"secretUnavailable, dockerUnavailable, timeout, interrupted, "+
			"persistence, internal")
	if err != nil {
		return parsed, err
	}
	for _, value := range failures {
		parsed.Failures = append(parsed.Failures, domain.ExecutionFailure(value))
	}

	return parsed, nil
}

// validNonEmptyExecutionFailure accepts only a real classification.
//
// domain.ValidExecutionFailure accepts the empty string, because "no failure"
// is a legal STORED value -- every successful execution has one. As a filter it
// would mean something quite different, so it is excluded here rather than
// relying on the query parser to skip it.
func validNonEmptyExecutionFailure(name string) bool {
	return name != "" && domain.ValidExecutionFailure(name)
}

// filter converts a validated query into a repository filter.
func (q executionQuery) filter() store.ExecutionFilter {
	return store.ExecutionFilter{
		States:         q.States,
		Failures:       q.Failures,
		ActiveOnly:     q.ActiveOnly,
		NeedsAttention: q.NeedsAttention,
		Sort:           q.Sort,
		Ascending:      q.Ascending,
		Page: store.Page{
			Limit:  q.PageSize,
			Offset: (q.Page - 1) * q.PageSize,
		},
	}
}
