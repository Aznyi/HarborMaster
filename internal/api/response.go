// Package api exposes HarborMaster's REST interface.
//
// Error handling here follows one rule: internal detail never crosses the
// boundary. Handlers log the real error and return a stable machine-readable
// code plus a short human message. Stack traces, driver errors, and Docker
// socket paths stay on the server side.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/Aznyi/HarborMaster/internal/logging"
)

// ErrorCode is a stable identifier clients can branch on. The prose in
// ErrorBody.Message may change; these values may not.
type ErrorCode string

// Error codes returned by the API.
const (
	CodeNotFound        ErrorCode = "not_found"
	CodeMethodNotAllwd  ErrorCode = "method_not_allowed"
	CodePayloadTooLarge ErrorCode = "payload_too_large"
	CodeInternal        ErrorCode = "internal_error"
	// CodeInvalidRequest marks a malformed query parameter.
	CodeInvalidRequest ErrorCode = "invalid_request"
	// CodeAmbiguousID marks a short ID prefix matching several resources.
	CodeAmbiguousID ErrorCode = "ambiguous_id"
	// CodeConflict marks a request that clashes with in-flight work, such as a
	// refresh requested while one is already running.
	CodeConflict ErrorCode = "conflict"
	// CodeUnavailable marks a dependency being unreachable, such as the Docker
	// Engine during a manual refresh.
	CodeUnavailable ErrorCode = "service_unavailable"
	// CodeDisabled marks a feature switched off by configuration.
	CodeDisabled ErrorCode = "feature_disabled"
)

// Pagination describes the page a list response returned.
type Pagination struct {
	Page       int  `json:"page"`
	PageSize   int  `json:"pageSize"`
	TotalItems int  `json:"totalItems"`
	TotalPages int  `json:"totalPages"`
	HasNext    bool `json:"hasNext"`
	HasPrev    bool `json:"hasPrevious"`
}

// newPagination computes the metadata for a page of results.
func newPagination(page, pageSize, totalItems int) Pagination {
	totalPages := 0
	if pageSize > 0 {
		totalPages = (totalItems + pageSize - 1) / pageSize
	}
	return Pagination{
		Page:       page,
		PageSize:   pageSize,
		TotalItems: totalItems,
		TotalPages: totalPages,
		HasNext:    page < totalPages,
		HasPrev:    page > 1,
	}
}

// listResponse is the envelope for every paginated collection.
//
// A named "items" field rather than a bare array: a top-level array leaves no
// room to add pagination or metadata later without breaking clients.
type listResponse[T any] struct {
	Items      []T        `json:"items"`
	Pagination Pagination `json:"pagination"`
}

// ErrorBody describes a failure.
type ErrorBody struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// ErrorResponse is the envelope for every non-2xx JSON response.
type ErrorResponse struct {
	Error     ErrorBody `json:"error"`
	RequestID string    `json:"requestId,omitempty"`
}

// writeJSON encodes v as the response body.
//
// The payload is marshalled before the status is written so an encoding
// failure can still produce a clean 500 instead of a truncated 200.
func writeJSON(w http.ResponseWriter, r *http.Request, logger *slog.Logger, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// The path is request-derived and goes through SafeAttr. The error is
		// not: it comes from marshalling a value this package constructed, so
		// it names Go types and struct fields rather than anything a caller
		// sent.
		logger.ErrorContext(r.Context(), "encode response failed",
			logging.SafeAttr("path", r.URL.Path),
			slog.String("error", err.Error()))
		writeError(w, r, logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		// The client went away mid-write; nothing to recover, just record it.
		logger.DebugContext(r.Context(), "write response failed", slog.String("error", err.Error()))
	}
}

// writeError renders an error envelope. message must be safe to expose: no
// stack traces, no wrapped internal errors, no filesystem paths.
func writeError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, status int, code ErrorCode, message string) {
	body, err := json.Marshal(ErrorResponse{
		Error:     ErrorBody{Code: code, Message: message},
		RequestID: RequestIDFrom(r.Context()),
	})
	if err != nil {
		http.Error(w, `{"error":{"code":"internal_error","message":"internal error"}}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		logger.DebugContext(r.Context(), "write error response failed", slog.String("error", err.Error()))
	}
}
