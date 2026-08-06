package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The drift endpoints.
//
// # What they can and cannot do
//
// Four reads and one write. The write moves a drift record's STATUS and
// nothing else: it cannot change a container, cannot change a snapshot, cannot
// re-run an evaluation, and cannot mark a difference resolved. There is no
// route here that reaches Docker, and adding one would have to get past the
// architecture test that keeps the SDK inside internal/docker.
//
// # Why PATCH is guarded like POST
//
// PATCH is state-changing, so it gets exactly the protections the two POST
// routes get: JSON media type required (which forces a CORS preflight that
// fails, because no CORS headers are served), strict decoding with unknown
// fields rejected, a body size cap, UTF-8 validation on the raw bytes, the
// per-process rate limit, and the Sec-Fetch/Origin checks. Treating PATCH as
// "read-ish" because it only moves an enum would be the mistake.

// DriftReader is the drift capability the API depends on.
//
// A narrow interface rather than *store.DriftRepository, so the handlers stay
// testable without a database and so the surface the API can reach is visible
// in one place. Note what is ABSENT: there is no Evaluate, no Delete, and no
// method that writes anything but a status. The API cannot trigger work or
// destroy history.
type DriftReader interface {
	List(ctx context.Context, filter store.DriftFilter) ([]domain.DriftRecord, int, error)
	Get(ctx context.Context, id int64) (domain.DriftRecord, error)
	Summary(ctx context.Context) (domain.DriftSummary, error)
	Evaluation(ctx context.Context, containerID string) (domain.DriftEvaluation, error)
	UpdateStatus(ctx context.Context, id int64, status domain.DriftStatus,
		note string, now time.Time) (domain.DriftRecord, error)
}

// handleDrift lists drift records.
func (s *Server) handleDrift(w http.ResponseWriter, r *http.Request) {
	if s.drift == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"drift detection is not configured")
		return
	}

	query, err := parseDriftQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	records, total, err := s.drift.List(r.Context(), query.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "drift list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, listResponse[domain.DriftRecord]{
		Items:      records,
		Pagination: newPagination(query.Page, query.PageSize, total),
	})
}

// handleDriftDetail returns one drift record.
func (s *Server) handleDriftDetail(w http.ResponseWriter, r *http.Request) {
	if s.drift == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"drift detection is not configured")
		return
	}

	id, ok := s.driftID(w, r)
	if !ok {
		return
	}

	record, err := s.drift.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "drift record not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "drift load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, record)
}

// handleDriftSummary returns the dashboard aggregate.
func (s *Server) handleDriftSummary(w http.ResponseWriter, r *http.Request) {
	if s.drift == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"drift detection is not configured")
		return
	}

	summary, err := s.drift.Summary(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "drift summary failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, summary)
}

// driftContainerResponse is one container's drift, with the evaluation that
// produced it.
//
// The evaluation is included so a client can tell "no drift" from "never
// checked" without a second request. Serving the records alone would let a UI
// render an empty list as a clean bill of health for a container that has no
// baseline.
type driftContainerResponse struct {
	ContainerID string               `json:"containerId"`
	Records     []domain.DriftRecord `json:"records"`
	Pagination  Pagination           `json:"pagination"`
	// Evaluation is absent when the container has never been evaluated.
	Evaluation *domain.DriftEvaluation `json:"evaluation,omitempty"`
}

// handleDriftByContainer lists one container's drift.
func (s *Server) handleDriftByContainer(w http.ResponseWriter, r *http.Request) {
	if s.drift == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"drift detection is not configured")
		return
	}

	// Resolved through the container repository so a short id prefix works
	// here exactly as it does on every other container route, and so an
	// ambiguous prefix is a 409 rather than an arbitrary pick.
	containerID, ok := s.resolveContainer(w, r)
	if !ok {
		return
	}

	query, err := parseDriftQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}
	// The path segment is authoritative; a containerId query parameter cannot
	// widen the request to another container.
	query.ContainerID = containerID

	records, total, err := s.drift.List(r.Context(), query.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "container drift list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	response := driftContainerResponse{
		ContainerID: containerID,
		Records:     records,
		Pagination:  newPagination(query.Page, query.PageSize, total),
	}

	evaluation, err := s.drift.Evaluation(r.Context(), containerID)
	switch {
	case err == nil:
		response.Evaluation = &evaluation
	case errors.Is(err, store.ErrNotFound):
		// Never evaluated. Left absent rather than invented, which is the
		// distinction this response exists to preserve.
	default:
		s.logger.ErrorContext(r.Context(), "drift evaluation load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, response)
}

// driftStatusRequest is the PATCH body.
//
// Exactly two fields, both optional-by-shape but validated below. Unknown
// fields are rejected by the decoder, so a caller that thinks it can set a
// severity or a value finds out rather than being silently ignored.
type driftStatusRequest struct {
	Status string `json:"status"`
	Note   string `json:"note"`
}

// handleDriftPatch applies an operator's status transition.
//
// The ONLY mutation this phase adds, and it moves an enum on HarborMaster's
// own record. It cannot touch Docker, the container, or the snapshot.
func (s *Server) handleDriftPatch(w http.ResponseWriter, r *http.Request) {
	if s.drift == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"drift detection is not configured")
		return
	}

	// Same guard the POST routes use: Fetch Metadata, Origin, and the
	// per-process rate limit.

	id, ok := s.driftID(w, r)
	if !ok {
		return
	}

	var request driftStatusRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &request); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	status := strings.TrimSpace(request.Status)
	// Validated against the OPERATOR vocabulary, not the full one. active and
	// resolved are engine-owned: a caller asserting "resolved" would be
	// asserting a fact about the world rather than an intent, and the drift
	// list would start lying about what is still true.
	if !domain.ValidOperatorDriftStatus(status) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"status must be one of acknowledged, ignored, expected")
		return
	}

	note, err := s.validateDriftNote(request.Note)
	if err != nil {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}

	record, err := s.drift.UpdateStatus(r.Context(), id, domain.DriftStatus(status), note, s.now())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "drift record not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "drift status update failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	s.auditWrite(r, domain.AuditDriftAnnotated, domain.AuditTargetDrift,
		strconv.FormatInt(record.ID, 10), record.ContainerName,
		"operator status set to "+string(record.Status))

	writeJSON(w, r, s.logger, http.StatusOK, record)
}

// validateDriftNote bounds and validates the operator's annotation.
//
// Length is measured in BYTES and validity in runes: a limit expressed in
// characters would let a caller send a megabyte of astral-plane text under a
// 500-"character" cap, and invalid UTF-8 would corrupt the column.
func (s *Server) validateDriftNote(note string) (string, error) {
	trimmed := strings.TrimSpace(note)
	if trimmed == "" {
		return "", nil
	}

	limit := s.driftCfg.MaxNoteBytes
	if limit <= 0 {
		limit = 500
	}
	if len(trimmed) > limit {
		return "", queryError{message: "note must be at most " +
			strconv.Itoa(limit) + " bytes"}
	}
	if !utf8.ValidString(trimmed) {
		return "", queryError{message: "note must be valid UTF-8"}
	}
	// Control characters are refused rather than stripped. A note is prose an
	// operator typed; anything carrying a control character is either a
	// mistake or an attempt to make the stored value render as something else.
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return "", queryError{message: "note must not contain control characters"}
		}
	}
	return trimmed, nil
}

// driftID reads and validates the {id} path segment.
func (s *Server) driftID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the drift id must be a positive integer")
		return 0, false
	}
	return id, true
}
