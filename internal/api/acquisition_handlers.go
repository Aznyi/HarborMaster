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

// The image acquisition endpoints.
//
// # These are the only endpoints that can change the Docker host
//
// Everything else in this API reads. `POST /acquisitions` asks HarborMaster to
// download an approved image into the daemon's local store, which is a write to
// the host.
//
// It is still narrow, and the narrowness is worth stating precisely:
//
//   - **No container is changed.** A container keeps running the image it was
//     created from. There is no endpoint here that stops, starts, recreates, or
//     reconfigures anything, and no capability behind one.
//   - **There is no target parameter.** The request body carries a PLAN ID and
//     an optional idempotency key. No registry, no repository, no digest, no
//     tag, no platform, no pull option. An operator approves an assessment;
//     they do not aim a downloader.
//   - **The handler cannot pull.** It asks the service, and the service
//     revalidates the plan and all its evidence before touching Docker. An
//     architecture test fails the build if this package ever references the
//     pull capability directly, because a handler that could reach it would
//     bypass the preflight -- which is the entire safety model.
//
// # Cancel is a write to HarborMaster's own state
//
// `POST /acquisitions/{id}/cancel` stops work HarborMaster is doing. It changes
// nothing on the host beyond ending a transfer partway, which leaves the
// daemon's store as it was.

// AcquisitionService is the acquisition capability the API depends on.
//
// A narrow interface rather than *service.AcquisitionService, so the handlers
// stay testable and so the surface the API can reach is visible in one place.
// Note what is ABSENT: nothing that pulls, and nothing that edits a stored
// record.
type AcquisitionService interface {
	Request(ctx context.Context, request service.AcquisitionRequest) (domain.Acquisition, error)
	Cancel(ctx context.Context, acquisitionID string) (domain.Acquisition, error)
	Get(ctx context.Context, acquisitionID string) (domain.Acquisition, []domain.AcquisitionEvent, error)
	List(ctx context.Context, filter store.AcquisitionFilter) ([]domain.Acquisition, int, error)
	Summary(ctx context.Context) (domain.AcquisitionSummary, error)
	Eligible(ctx context.Context, planID string) (domain.AcquisitionTarget, domain.AcquisitionRefusal, error)
	Enabled() bool
}

// acquisitionUnavailable writes the disabled response, and reports whether it
// did.
func (s *Server) acquisitionUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.acquisitions != nil {
		return false
	}
	writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
		"image acquisition is not configured")
	return true
}

// acquisitionListResponse is the acquisition listing, with the estate
// aggregate.
type acquisitionListResponse struct {
	Items      []domain.Acquisition      `json:"items"`
	Pagination Pagination                `json:"pagination"`
	Summary    domain.AcquisitionSummary `json:"summary"`
}

// handleAcquisitions lists acquisitions.
func (s *Server) handleAcquisitions(w http.ResponseWriter, r *http.Request) {
	if s.acquisitionUnavailable(w, r) {
		return
	}

	query, err := parseAcquisitionQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	items, total, err := s.acquisitions.List(r.Context(), query.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "acquisition list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	summary, err := s.acquisitions.Summary(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "acquisition summary failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, acquisitionListResponse{
		Items:      items,
		Pagination: newPagination(query.Page, query.PageSize, total),
		Summary:    summary,
	})
}

// acquisitionDetailResponse is one acquisition with its audit trail.
type acquisitionDetailResponse struct {
	Acquisition domain.Acquisition        `json:"acquisition"`
	Events      []domain.AcquisitionEvent `json:"events"`
}

// handleAcquisitionDetail returns one acquisition.
func (s *Server) handleAcquisitionDetail(w http.ResponseWriter, r *http.Request) {
	if s.acquisitionUnavailable(w, r) {
		return
	}
	acquisitionID, ok := s.acquisitionID(w, r)
	if !ok {
		return
	}

	acquisition, events, err := s.acquisitions.Get(r.Context(), acquisitionID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "acquisition not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "acquisition load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, acquisitionDetailResponse{
		Acquisition: acquisition,
		Events:      events,
	})
}

// acquisitionRequestBody is the create request.
//
// Two fields, and neither of them names an image. The target is derived
// entirely from the plan, which is what makes this endpoint unable to fetch
// anything an operator did not approve through the planning path.
type acquisitionRequestBody struct {
	// PlanID is the approved change plan.
	PlanID string `json:"planId"`
	// RequestKey is an optional idempotency key, so a retried request -- or a
	// double-clicked button -- does not start a second download.
	RequestKey string `json:"requestKey,omitempty"`
}

// handleAcquisitionCreate requests an image acquisition.
//
// # What this endpoint can cause
//
// One digest-pinned image download, for a plan that currently recommends the
// change, for a container that currently exists, from a registry the inventory
// already references. Nothing else.
//
// Asynchronous: it answers 202 with the recorded request. A synchronous
// response would hold an unauthenticated request open for the length of a
// multi-gigabyte transfer.
//
// The FULL preflight runs synchronously before the 202, so an operator gets a
// real refusal rather than a queued job that fails a moment later -- and it
// runs AGAIN immediately before the pull, because this call can be well ahead
// of a worker slot.
func (s *Server) handleAcquisitionCreate(w http.ResponseWriter, r *http.Request) {
	if s.acquisitionUnavailable(w, r) {
		return
	}
	// The same write guard every other state-changing endpoint uses: fetch
	// metadata, origin, JSON media type, size limit, rate limit.

	var body acquisitionRequestBody
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	planID := strings.TrimSpace(body.PlanID)
	if !validPlanID(planID) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"a well-formed plan id is required")
		return
	}
	requestKey := strings.TrimSpace(body.RequestKey)
	if !validRequestKey(requestKey) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the request key must be at most 100 printable ASCII characters")
		return
	}

	acquisition, err := s.acquisitions.Request(r.Context(), service.AcquisitionRequest{
		PlanID:     planID,
		RequestKey: requestKey,
		// Carried onto the record so the OUTCOME can be attributed by a worker
		// that runs later with no request and no session.
		RequestedBy: s.requesterFrom(r),
	})
	if err != nil {
		// A refusal is recorded too. Without this a caller could probe the
		// endpoint indefinitely and leave the security log looking idle.
		var refused service.ErrAcquisitionRefused
		if errors.As(err, &refused) {
			s.auditRefusal(r, domain.AuditAcquisitionRequested,
				domain.AuditTargetPlan, planID, string(refused.Refusal))
		}
		s.writeAcquisitionError(w, r, err)
		return
	}

	s.auditWrite(r, domain.AuditAcquisitionRequested, domain.AuditTargetAcquisition,
		acquisition.AcquisitionID, acquisition.ContainerName,
		"download requested for "+acquisition.Target.Digest)

	writeJSON(w, r, s.logger, http.StatusAccepted, acquisition)
}

// handleAcquisitionCancel stops an acquisition.
func (s *Server) handleAcquisitionCancel(w http.ResponseWriter, r *http.Request) {
	if s.acquisitionUnavailable(w, r) {
		return
	}
	// The same write guard, minus a body: cancel carries none. guardWrite does
	// not require a media type -- decodeJSONBody does, and this endpoint has
	// nothing to decode.
	acquisitionID, ok := s.acquisitionID(w, r)
	if !ok {
		return
	}

	acquisition, err := s.acquisitions.Cancel(r.Context(), acquisitionID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "acquisition not found")
		return
	}
	if err != nil {
		var refused service.ErrAcquisitionRefused
		if errors.As(err, &refused) {
			// Already finished, or past the point where stopping is safe.
			// Conflict rather than a bad request: the request was well formed
			// and the state was not.
			writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
				"this acquisition can no longer be cancelled")
			return
		}
		s.logger.ErrorContext(r.Context(), "acquisition cancel failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	s.auditWrite(r, domain.AuditAcquisitionCancelled, domain.AuditTargetAcquisition,
		acquisition.AcquisitionID, acquisition.ContainerName, "download cancelled")

	writeJSON(w, r, s.logger, http.StatusOK, acquisition)
}

// writeAcquisitionError maps a service failure onto a status code.
//
// A REFUSAL is reported as 409 with the specific check that said no, because it
// is a statement about the world rather than about the request: the plan is
// stale, the digest moved, the daemon is down. The operator's request was well
// formed; the situation is not what they thought.
//
// The message is HarborMaster's own fixed phrase for that refusal. No daemon
// text, no registry text, and no caller input reaches it.
func (s *Server) writeAcquisitionError(w http.ResponseWriter, r *http.Request, err error) {
	var refused service.ErrAcquisitionRefused
	if errors.As(err, &refused) {
		status := http.StatusConflict
		code := CodeConflict

		switch refused.Refusal {
		case domain.AcquisitionRefusalDisabled:
			status, code = http.StatusServiceUnavailable, CodeDisabled
		case domain.AcquisitionRefusalPlanMissing, domain.AcquisitionRefusalContainerMissing:
			status, code = http.StatusNotFound, CodeNotFound
		case domain.AcquisitionRefusalLimit:
			// Retry later, and say so with the status that means exactly
			// that. CodeConflict is the code the shared write limiter already
			// uses for a 429, so a client branching on it sees one vocabulary.
			status, code = http.StatusTooManyRequests, CodeRateLimited
		}

		s.writeRefusal(w, r, status, code, refused.Refusal)
		return
	}

	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "the change plan was not found")
		return
	}

	s.logger.ErrorContext(r.Context(), "acquisition request failed", slog.String("error", err.Error()))
	writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
}

// acquisitionRefusalResponse is the error envelope plus the specific check that
// refused.
//
// The extra field exists so a client can branch on WHICH check said no without
// parsing prose: "the digest changed" and "the daemon is down" call for
// different things from an operator, and both arrive as a 409.
//
// The refusal is a closed vocabulary, and the message is HarborMaster's own
// fixed phrase for it. Neither carries daemon text, registry text, or caller
// input.
type acquisitionRefusalResponse struct {
	Error     ErrorBody                 `json:"error"`
	Refusal   domain.AcquisitionRefusal `json:"refusal"`
	RequestID string                    `json:"requestId,omitempty"`
}

// writeRefusal renders a preflight refusal.
func (s *Server) writeRefusal(
	w http.ResponseWriter,
	r *http.Request,
	status int,
	code ErrorCode,
	refusal domain.AcquisitionRefusal,
) {
	writeJSON(w, r, s.logger, status, acquisitionRefusalResponse{
		Error:     ErrorBody{Code: code, Message: refusal.Explain()},
		Refusal:   refusal,
		RequestID: RequestIDFrom(r.Context()),
	})
}

// ---------------------------------------------------------------- queries --

// acquisitionIDPrefix and acquisitionIDHexLength describe a generated id.
const (
	acquisitionIDPrefix    = "acq_"
	acquisitionIDHexLength = 20
	acquisitionIDBytes     = len(acquisitionIDPrefix) + acquisitionIDHexLength
)

// validAcquisitionID reports whether id has the shape of a generated id.
//
// Validated by SHAPE before it reaches a query. Acquisition ids are
// server-generated and have exactly one form, so anything else is a miss -- and
// refusing early keeps arbitrary caller text out of the database layer.
func validAcquisitionID(id string) bool {
	if len(id) != acquisitionIDBytes || !strings.HasPrefix(id, acquisitionIDPrefix) {
		return false
	}
	for _, r := range id[len(acquisitionIDPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// acquisitionID reads and validates the {id} path segment.
func (s *Server) acquisitionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	if !validAcquisitionID(raw) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the acquisition id is not well formed")
		return "", false
	}
	return raw, true
}

// maxRequestKeyBytes bounds an idempotency key.
const maxRequestKeyBytes = 100

// validRequestKey reports whether an idempotency key is acceptable.
//
// Printable ASCII only, and bounded. The value reaches a unique index and an
// API response; an allowlist keeps control characters and multi-byte
// lookalikes out of both. Empty is valid and means "no key".
func validRequestKey(key string) bool {
	if key == "" {
		return true
	}
	if len(key) > maxRequestKeyBytes {
		return false
	}
	for _, r := range key {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

// acquisitionQuery is a parsed and validated listing request.
type acquisitionQuery struct {
	States     []domain.AcquisitionState
	Failures   []domain.AcquisitionFailure
	ActiveOnly bool

	Sort      string
	Ascending bool
	Page      int
	PageSize  int
}

// parseAcquisitionQuery reads and validates the listing parameters.
//
// Every parameter is validated against a CLOSED VOCABULARY defined in the
// domain package, or is a bounded integer. Nothing a caller sends becomes SQL
// text: a state or failure is matched against the domain's allowlist and then
// travels as a bound parameter, and the sort field selects a compile-time
// column constant from a map.
func parseAcquisitionQuery(query url.Values) (acquisitionQuery, error) {
	// Newest first: an operator opening this page is asking "what just
	// happened", and the state ranking puts active work above finished work.
	parsed := acquisitionQuery{Sort: "requestedAt"}

	page, pageSize, err := parsePage(query)
	if err != nil {
		return parsed, err
	}
	parsed.Page, parsed.PageSize = page, pageSize

	if raw := strings.TrimSpace(query.Get("sort")); raw != "" {
		if !store.ValidAcquisitionSortField(raw) {
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

	if raw := strings.TrimSpace(query.Get("activeOnly")); raw != "" {
		switch raw {
		case "true":
			parsed.ActiveOnly = true
		case "false":
			parsed.ActiveOnly = false
		default:
			return parsed, invalidParam("activeOnly", "true or false")
		}
	}

	states, err := parseVocabulary(query, "state", domain.ValidAcquisitionState,
		"one of queued, validating, pulling, verifying, succeeded, failed, cancelled, expired")
	if err != nil {
		return parsed, err
	}
	for _, value := range states {
		parsed.States = append(parsed.States, domain.AcquisitionState(value))
	}

	failures, err := parseVocabulary(query, "failure", validNonEmptyAcquisitionFailure,
		"one of preflight, dockerUnavailable, registry, transfer, timeout, "+
			"digestMismatch, platformMismatch, verification, internal")
	if err != nil {
		return parsed, err
	}
	for _, value := range failures {
		parsed.Failures = append(parsed.Failures, domain.AcquisitionFailure(value))
	}

	return parsed, nil
}

// validNonEmptyAcquisitionFailure accepts only a real classification.
//
// domain.ValidAcquisitionFailure accepts the empty string, because "no failure"
// is a legal STORED value -- every successful acquisition has one. As a filter
// it would mean something quite different, so it is excluded here rather than
// relying on the query parser to skip it.
func validNonEmptyAcquisitionFailure(name string) bool {
	return name != "" && domain.ValidAcquisitionFailure(name)
}

// filter converts a validated query into a repository filter.
func (q acquisitionQuery) filter() store.AcquisitionFilter {
	return store.AcquisitionFilter{
		States:     q.States,
		Failures:   q.Failures,
		ActiveOnly: q.ActiveOnly,
		Sort:       q.Sort,
		Ascending:  q.Ascending,
		Page: store.Page{
			Limit:  q.PageSize,
			Offset: (q.Page - 1) * q.PageSize,
		},
	}
}
