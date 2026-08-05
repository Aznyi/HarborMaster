package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

type contextKey string

const requestIDKey contextKey = "request-id"

// RequestIDHeader carries the correlation ID on both request and response.
const RequestIDHeader = "X-Request-ID"

// RequestIDFrom returns the request ID stored in ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// middleware is the standard decorator shape used by chain.
type middleware func(http.Handler) http.Handler

// chain applies middlewares so that the first listed runs outermost.
func chain(h http.Handler, middlewares ...middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

// withRequestID assigns each request a correlation ID.
//
// A client-supplied X-Request-ID is deliberately ignored: it is attacker
// controlled and would let a caller forge or poison log correlation.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// withRecovery converts a panic into a generic 500.
//
// The stack trace is logged and never written to the response: a trace
// discloses source paths and internal structure to anyone who can reach the
// API.
func withRecovery(logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					// http.ErrAbortHandler is the documented way to abort a
					// response; re-panicking preserves that contract.
					//
					// Matched through errors.Is rather than ==: a handler that
					// wrapped the sentinel before panicking still means to abort,
					// and a bare == would treat that as a real panic and write a
					// 500 into a response the handler deliberately abandoned.
					if abort, ok := recovered.(error); ok && errors.Is(abort, http.ErrAbortHandler) {
						panic(recovered)
					}
					// The panic value is rendered through %v into a string rather
					// than passed as slog.Any. slog.Any on an arbitrary value
					// invokes whatever MarshalJSON or LogValue the value's type
					// defines, and a panic carrying, say, a struct with a
					// credential field would then serialise that field into the
					// log. Formatting to a string keeps the record to what the
					// value chooses to print.
					logger.ErrorContext(r.Context(), "panic recovered",
						slog.String("method", r.Method),
						slog.String("path", r.URL.Path),
						slog.String("requestId", RequestIDFrom(r.Context())),
						slog.String("panic", fmt.Sprintf("%v", recovered)),
						slog.String("stack", stackTrace()))
					writeError(w, r, logger, http.StatusInternalServerError, CodeInternal, "internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// withAccessLog emits one structured record per request.
//
// Only method, path, status, duration and request ID are recorded. Headers,
// query values and bodies are omitted wholesale rather than filtered, because
// any of them can carry a token.
func withAccessLog(logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			logger.InfoContext(r.Context(), "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.written),
				slog.Int64("durationMs", time.Since(start).Milliseconds()),
				slog.String("requestId", RequestIDFrom(r.Context())))
		})
	}
}

// withMaxBody caps the request body so a large upload cannot exhaust memory.
// http.MaxBytesReader makes the read fail past the limit rather than buffering.
func withMaxBody(limit int64, logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				writeError(w, r, logger, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "request body too large")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// contentSecurityPolicy is the policy served with every response.
//
// The frontend is fully self-hosted and embedded in the binary, so the policy
// forbids every remote origin outright.
//
// # No 'unsafe-inline' anywhere
//
// Not for scripts, and -- since the audit -- not for styles either. The Vite
// build emits a single linked stylesheet and no inline <style> block, and the
// application uses no `style` props, so nothing needed the exemption the policy
// previously granted. 'unsafe-inline' on style-src is not harmless: it is what
// turns a hypothetical HTML injection into CSS exfiltration (attribute
// selectors that leak input values through background-image requests) and into
// UI redressing.
//
// The exemption cost nothing to remove, which is exactly why it should not have
// survived. TestSecurityHeaders pins this string so that reintroducing an
// inline style breaks a test rather than silently widening the policy.
//
// If a future change genuinely needs inline styles, the correct answer is a
// per-response nonce or hash, not a blanket exemption.
const contentSecurityPolicy = "default-src 'self'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'; " +
	"object-src 'none'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"style-src 'self'; " +
	"script-src 'self'; " +
	"connect-src 'self'; " +
	"manifest-src 'self'; " +
	"worker-src 'self'; " +
	"upgrade-insecure-requests"

// withSecurityHeaders applies conservative defaults to every response.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		// Stops another origin embedding this response as a subresource. Cheap,
		// and meaningful for an API describing a privileged host.
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy",
			"camera=(), microphone=(), geolocation=(), payment=(), usb=(), "+
				"serial=(), bluetooth=(), midi=(), interest-cohort=()")
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status and byte count for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if s.wrote {
		return
	}
	s.wrote = true
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// stackTrace returns the current goroutine's stack for the log record only.
func stackTrace() string {
	return string(debug.Stack())
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; degrade to a constant rather
		// than take the process down over a log correlation ID.
		return "unavailable"
	}
	return hex.EncodeToString(b[:])
}
