// Write-endpoint hardening.
//
// HarborMaster has exactly two POST routes -- snapshot capture and inventory
// refresh -- and both get the same protections. Neither changes a container:
// one writes HarborMaster's own database, the other re-reads the host. They are
// still worth defending, because both drive real work, and the API is
// unauthenticated by design (it is meant to sit behind a reverse proxy or on
// loopback).
//
// # Layering
//
// The controls here are ordered by how much they can be trusted:
//
//  1. Request validation and rate limiting. These hold against any client and
//     are what actually bounds abuse.
//  2. Fetch Metadata and Origin checks. Defence in depth ONLY. Sec-Fetch-* is
//     absent in older browsers and trivially omitted by a non-browser client,
//     and Origin says nothing about a direct HTTP call. Tests assert that
//     everything in (1) still holds with every header in (2) stripped.
//
// Classic session-riding CSRF does not apply here -- there are no cookies and
// no session -- but a malicious page can still issue a cross-origin form or
// simple fetch, so the checks are worth having as a second layer.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// writeGuardError is a validation failure with a client-safe message.
//
// It carries a status, a stable code, and prose that never echoes the request
// body: an error message is not the place to reflect attacker-controlled input.
type writeGuardError struct {
	status  int
	code    ErrorCode
	message string
	// retryAfter is set on a rate-limit rejection.
	retryAfter time.Duration
}

func (e writeGuardError) Error() string { return e.message }

// isJSONMediaType reports whether a Content-Type header names JSON.
//
// Parsed rather than compared, so "application/json; charset=utf-8" is accepted
// and "application/json-patch+json" is not.
func isJSONMediaType(header string) bool {
	if header == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

// decodeJSONBody strictly decodes exactly one JSON object into dst.
//
// Three rules, each closing a different gap:
//
//   - The media type must be application/json. This also forces a CORS
//     preflight for a cross-origin caller, which fails because no CORS headers
//     are served -- a simple form POST cannot set this header.
//   - Unknown fields are rejected rather than ignored. Silently dropping input
//     is how a client ends up believing an option took effect.
//   - Exactly one object. Trailing content and concatenated objects are
//     refused, closing the ambiguity where two parsers disagree about where the
//     body ended.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	if !isJSONMediaType(r.Header.Get("Content-Type")) {
		return writeGuardError{
			status:  http.StatusUnsupportedMediaType,
			code:    CodeInvalidRequest,
			message: "Content-Type must be application/json",
		}
	}

	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	// The body is read whole before decoding, so its bytes can be checked for
	// UTF-8 validity.
	//
	// This has to happen on the RAW bytes: encoding/json silently replaces an
	// invalid byte sequence inside a string with U+FFFD rather than failing, so
	// by the time a value reaches a Go string it is always valid UTF-8 and the
	// corruption is undetectable. Reading first is safe because MaxBytesReader
	// already bounds how much there can be.
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return jsonDecodeError(err)
	}
	if !utf8.Valid(raw) {
		return writeGuardError{
			status:  http.StatusBadRequest,
			code:    CodeInvalidRequest,
			message: "request body must be valid UTF-8",
		}
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		return jsonDecodeError(err)
	}

	// A second decode must hit EOF. Anything else means the body carried more
	// than one value.
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return writeGuardError{
			status:  http.StatusBadRequest,
			code:    CodeInvalidRequest,
			message: "body must contain exactly one JSON object",
		}
	}
	return nil
}

// jsonDecodeError converts a decoder failure into a client-safe error.
//
// The offending content is never included. json.SyntaxError and friends carry
// offsets and field names, which are safe, but the raw body is not, and the
// simplest way to guarantee that is to say what was wrong rather than what was
// sent.
func jsonDecodeError(err error) error {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		return writeGuardError{
			status:  http.StatusRequestEntityTooLarge,
			code:    CodePayloadTooLarge,
			message: "request body is too large",
		}
	}

	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return writeGuardError{
			status:  http.StatusBadRequest,
			code:    CodeInvalidRequest,
			message: "request body is not valid JSON",
		}
	}

	var unmarshalType *json.UnmarshalTypeError
	if errors.As(err, &unmarshalType) {
		return writeGuardError{
			status:  http.StatusBadRequest,
			code:    CodeInvalidRequest,
			message: "field " + unmarshalType.Field + " has the wrong type",
		}
	}

	if errors.Is(err, io.EOF) {
		return writeGuardError{
			status:  http.StatusBadRequest,
			code:    CodeInvalidRequest,
			message: "request body is empty",
		}
	}

	// DisallowUnknownFields reports through a plain error whose text is
	// `json: unknown field "..."`. The field name came from the request, so it
	// is not echoed.
	if strings.Contains(err.Error(), "unknown field") {
		return writeGuardError{
			status:  http.StatusBadRequest,
			code:    CodeInvalidRequest,
			message: "request body contains an unrecognised field",
		}
	}

	return writeGuardError{
		status:  http.StatusBadRequest,
		code:    CodeInvalidRequest,
		message: "request body could not be decoded",
	}
}

// validateText enforces UTF-8 validity, a byte bound, and the absence of
// control characters on an operator-supplied string.
//
// Invalid UTF-8 is rejected rather than replaced: it would otherwise reach the
// database, the logs, and a UI, and a replacement character silently changes
// what the operator wrote. Control characters are rejected because they are how
// a log record gets forged -- an embedded newline can fabricate a whole line.
func validateText(field, value string, maxBytes int) error {
	if len(value) > maxBytes {
		return writeGuardError{
			status:  http.StatusBadRequest,
			code:    CodeInvalidRequest,
			message: field + " must be at most " + strconv.Itoa(maxBytes) + " bytes",
		}
	}
	if !utf8.ValidString(value) {
		return writeGuardError{
			status:  http.StatusBadRequest,
			code:    CodeInvalidRequest,
			message: field + " must be valid UTF-8",
		}
	}
	for _, r := range value {
		// Tab is allowed; newlines and the rest are not.
		if r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return writeGuardError{
				status:  http.StatusBadRequest,
				code:    CodeInvalidRequest,
				message: field + " must not contain control characters",
			}
		}
	}
	return nil
}

// checkFetchMetadata applies the browser-supplied navigation hints.
//
// DEFENCE IN DEPTH, never the primary control. Sec-Fetch-Site is absent in
// older browsers and omitted entirely by curl, and Origin is absent on a direct
// HTTP call. A client that wants past this simply does not send the headers.
// The validation and rate limiting elsewhere in this file are what actually
// bound abuse, and they hold with all of these stripped.
func checkFetchMetadata(r *http.Request) error {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		return writeGuardError{
			status:  http.StatusForbidden,
			code:    CodeInvalidRequest,
			message: "cross-origin write requests are not accepted",
		}
	}

	// An Origin that does not match the Host is rejected. Absent is allowed:
	// that is what a direct API client looks like.
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return nil
	}
	if !originMatchesHost(origin, r.Host) {
		return writeGuardError{
			status:  http.StatusForbidden,
			code:    CodeInvalidRequest,
			message: "cross-origin write requests are not accepted",
		}
	}
	return nil
}

// originMatchesHost compares an Origin header against the request's Host.
//
// Compared on the host:port authority only. The scheme is deliberately not
// checked: HarborMaster is routinely fronted by a TLS-terminating proxy, so an
// https Origin against an http listener is the normal deployment rather than an
// attack.
func originMatchesHost(origin, host string) bool {
	authority := origin
	if index := strings.Index(authority, "://"); index >= 0 {
		authority = authority[index+3:]
	}
	if index := strings.IndexAny(authority, "/?#"); index >= 0 {
		authority = authority[:index]
	}
	return strings.EqualFold(authority, host)
}

// rateLimiter is a token bucket over the write endpoints.
//
// PER PROCESS, not per client. HarborMaster is unauthenticated, so there is no
// trustworthy client identity to key on: a per-IP bucket is trivially evaded
// behind a proxy or from a botnet while implying a precision the system does
// not have. A single global bucket is honest about what it protects -- the
// process and its database -- and cannot be exhausted by key cardinality.
//
// The cost is that one noisy client can consume the budget for everyone. On a
// single-operator tool fronting one Docker host, that is the right trade.
type rateLimiter struct {
	mu sync.Mutex

	// tokens is the current allowance; burst is the ceiling.
	tokens float64
	burst  float64
	// refillPerSecond is derived from the configured requests-per-minute.
	refillPerSecond float64
	last            time.Time

	now func() time.Time
}

// newRateLimiter builds a limiter from a per-minute rate and a burst.
func newRateLimiter(perMinute float64, burst int, now func() time.Time) *rateLimiter {
	if perMinute <= 0 {
		perMinute = 6
	}
	if burst < 1 {
		burst = 1
	}
	if now == nil {
		now = time.Now
	}

	return &rateLimiter{
		tokens:          float64(burst),
		burst:           float64(burst),
		refillPerSecond: perMinute / 60,
		last:            now(),
		now:             now,
	}
}

// allow consumes a token, reporting how long to wait when none is available.
func (l *rateLimiter) allow() (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	elapsed := now.Sub(l.last).Seconds()
	if elapsed > 0 {
		l.tokens += elapsed * l.refillPerSecond
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
	}

	if l.tokens >= 1 {
		l.tokens--
		return true, 0
	}

	// Round up, so a client that honours Retry-After actually succeeds.
	needed := (1 - l.tokens) / l.refillPerSecond
	wait := time.Duration(needed * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}

// guardWrite applies every write-endpoint protection in order.
//
// Returns an error the caller renders; a nil return means the request may
// proceed.
func (s *Server) guardWrite(r *http.Request) error {
	return s.guardWriteWith(r, s.writeLimiter)
}

// guardPolicyWrite is guardWrite against the POLICY limiter.
//
// A separate bucket, and deliberately a more permissive one. A rate limit
// should be proportional to what the request costs: a snapshot capture or an
// inventory refresh drives a Docker sweep, while a policy write is one small
// transaction on HarborMaster's own table. Sharing one bucket would mean either
// throttling the cheap operation absurdly -- an operator building a five-rule
// policy set would hit 429 -- or under-protecting the expensive one.
//
// POST /policy/evaluate uses this bucket too. The request is cheap; what bounds
// the work it schedules is the coalescing queue behind it, not this limiter.
func (s *Server) guardPolicyWrite(r *http.Request) error {
	return s.guardWriteWith(r, s.policyLimiter)
}

// guardWriteWith applies the checks against a specific bucket.
func (s *Server) guardWriteWith(r *http.Request, limiter *rateLimiter) error {
	if err := checkFetchMetadata(r); err != nil {
		return err
	}
	if limiter != nil {
		if allowed, retryAfter := limiter.allow(); !allowed {
			return writeGuardError{
				status:     http.StatusTooManyRequests,
				code:       CodeRateLimited,
				message:    "too many write requests; slow down",
				retryAfter: retryAfter,
			}
		}
	}
	return nil
}

// writeGuardFailure renders a guard error.
func (s *Server) writeGuardFailure(w http.ResponseWriter, r *http.Request, err error) {
	var guard writeGuardError
	if !errors.As(err, &guard) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest, "invalid request")
		return
	}

	if guard.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(guard.retryAfter.Seconds()+0.999)))
	}
	writeError(w, r, s.logger, guard.status, guard.code, guard.message)
}
