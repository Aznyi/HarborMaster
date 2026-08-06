package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// Server-Sent Events for live Docker event delivery.
//
// SSE rather than WebSockets, deliberately. The traffic is one-way
// server-to-browser, SSE is plain HTTP so it inherits this server's timeouts,
// security headers, and access log unchanged, and the browser reconnects on its
// own with Last-Event-ID. A WebSocket would add a second protocol and a second
// set of framing bugs to solve a problem that does not exist here.
//
// SECURITY. This endpoint requires `event:read`, like every other event route.
// Everything it emits has already been through redaction before being stored,
// so an attribute whose key matched a sensitive pattern carries the mask rather
// than the secret. No raw Docker payload is ever written to the wire: the
// frames carry HarborMaster's own event model and nothing else.
//
// # A stream is re-authorized, not authorized once
//
// Every other route in HarborMaster re-reads the account on each request, which
// is what makes a disablement, a demotion, or a password change take effect
// immediately. A stream makes ONE request and then runs for as long as the
// client holds the connection -- up to the session's absolute lifetime.
//
// Authorizing it only at connect would therefore make it the one place where
// revoking a session does not stop the flow of estate data. So the session is
// re-checked on every heartbeat, and the stream ends the moment it stops being
// valid. See revalidate.

// SSE event names. A named event lets a client attach separate listeners
// instead of parsing a discriminator out of every payload.
const (
	// sseEventDocker carries one Docker event.
	sseEventDocker = "docker-event"
	// sseEventReady is sent once when the stream opens.
	sseEventReady = "ready"
	// sseEventTruncated warns that a Last-Event-ID replay was capped, so the
	// client knows to reload the paginated history rather than assume it has
	// everything.
	sseEventTruncated = "replay-truncated"
	// sseEventClosed says the server is ending the stream deliberately, so a
	// client can tell "you are signed out" from a dropped connection and stop
	// reconnecting into a 401 loop.
	sseEventClosed = "closed"
)

// closedPayload explains a deliberate close.
type closedPayload struct {
	Reason string `json:"reason"`
}

// readyPayload is the opening frame.
type readyPayload struct {
	// LastEventId is the newest sequence the client now holds, so a client that
	// sent no Last-Event-ID knows where the live stream begins.
	LastEventID int64 `json:"lastEventId"`
	Replayed    int   `json:"replayed"`
	// Redacted is always true. Stated so a client is not left to assume it.
	Redacted bool   `json:"redacted"`
	Notice   string `json:"notice"`
}

// truncatedPayload reports a capped replay.
type truncatedPayload struct {
	// Skipped is how many stored events the replay could not include.
	Skipped int    `json:"skipped"`
	Limit   int    `json:"limit"`
	Notice  string `json:"notice"`
}

// handleEventStream serves the live event stream.
//
// Lifecycle: the subscription is registered before any replay is written, so
// an event arriving mid-replay is buffered rather than lost in the gap between
// reading history and going live. The deferred Close releases the subscriber
// slot on every exit path, including a client that simply disappears.
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	if s.eventEngine == nil || !s.eventEngine.Enabled() {
		// A disabled engine produces no events, so an open stream would be a
		// connection held for nothing. The client reads GET /event-engine to
		// render why.
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"the event engine is disabled by configuration")
		return
	}

	subscription, err := s.eventEngine.Subscribe()
	if err != nil {
		if errors.Is(err, service.ErrTooManySubscribers) {
			// 503 with Retry-After rather than a queue: a client told to come
			// back can back off, while one parked in a queue occupies the
			// connection it was refused.
			w.Header().Set("Retry-After", "5")
			writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeUnavailable,
				"too many event stream subscribers; try again shortly")
			return
		}
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"the event stream is unavailable")
		return
	}
	defer subscription.Close()

	writer, ok := s.prepareStream(w, r)
	if !ok {
		return
	}

	// Replay before going live. Buffered live events wait in the subscription's
	// channel meanwhile, so ordering is preserved and nothing falls through the
	// seam between history and live.
	lastSequence := s.replay(r, writer)

	writer.event(sseEventReady, readyPayload{
		LastEventID: lastSequence,
		Replayed:    writer.replayed,
		Redacted:    true,
		Notice: "Attribute values matching sensitive-name patterns are masked. " +
			"No raw Docker payload is sent.",
	})
	writer.flush()

	heartbeat := time.NewTicker(s.eventEngine.HeartbeatInterval())
	defer heartbeat.Stop()

	ctx := r.Context()
	// The session this stream was opened under. Re-checked on every heartbeat.
	identity, _ := IdentityFrom(ctx)
	for {
		select {
		case <-ctx.Done():
			// The client disconnected, or the server is shutting down. The
			// deferred Close releases the slot.
			return

		case event, open := <-subscription.Events:
			if !open {
				// The engine shut down. Ending the response cleanly lets the
				// HTTP server's drain finish instead of waiting on this handler.
				return
			}
			// An event older than what the client already has can arrive during
			// the seam between replay and live. Skipping it keeps the client's
			// Last-Event-ID monotonic, which is what makes a reconnect correct.
			if event.Sequence <= lastSequence {
				continue
			}
			lastSequence = event.Sequence

			if !writer.event(sseEventDocker, event) {
				return
			}
			if !writer.flush() {
				return
			}

		case <-heartbeat.C:
			// Re-authorize BEFORE writing the heartbeat. A stream whose session
			// has been revoked must stop delivering estate data, and the
			// heartbeat is the natural cadence: it is the only thing that
			// happens on a quiet connection, so the check costs one indexed
			// lookup per interval rather than one per event.
			if !s.streamStillAuthorized(r, identity) {
				writer.event(sseEventClosed, closedPayload{
					Reason: "your session is no longer valid; sign in again",
				})
				writer.flush()
				return
			}

			// A comment frame. It carries no data and no event name, so a
			// client ignores it, but it keeps proxies and load balancers from
			// closing an idle connection.
			if !writer.comment("heartbeat") {
				return
			}
			if !writer.flush() {
				return
			}
		}
	}
}

// streamStillAuthorized reports whether an open stream may keep delivering.
//
// Re-resolves the session token and re-checks the permission, so every way a
// session can end -- sign-out, expiry, a password change, a role change, a
// disablement, an administrator revoking it, the per-account cap superseding it
// -- closes the stream at the next heartbeat.
//
// The permission is re-checked as well as the session, because a demotion from
// operator to viewer leaves a VALID session that no longer holds `event:read`.
//
// Fails CLOSED. A lookup that errors ends the stream: a stream is a standing
// grant, and a grant that cannot be reconfirmed is one that should stop.
func (s *Server) streamStillAuthorized(r *http.Request, opened service.Authenticated) bool {
	if s.auth == nil {
		return false
	}

	current, err := s.auth.Authenticate(r.Context(), opened.Token)
	if err != nil {
		return false
	}
	// The same ACCOUNT and the same SESSION. A token that now resolves to a
	// different session would mean the row was replaced under us, which is not
	// a thing to keep streaming through.
	if current.User.UserID != opened.User.UserID ||
		current.Session.SessionID != opened.Session.SessionID {
		return false
	}
	return current.User.Can(domain.PermEventRead)
}

// prepareStream writes the SSE response headers and clears the write deadline.
//
// Two things here are essential and easy to miss:
//
//   - The server sets a WriteTimeout for ordinary requests, and it applies to
//     the whole response. An SSE response never ends, so without clearing the
//     deadline every stream would be severed after WriteTimeout and present as
//     a flapping connection. http.ResponseController is the supported way to
//     opt one handler out.
//   - X-Accel-Buffering disables nginx's response buffering. Without it a proxy
//     accumulates frames and delivers them in chunks, which turns a live stream
//     into a periodic one.
func (s *Server) prepareStream(w http.ResponseWriter, r *http.Request) (*sseWriter, bool) {
	controller := http.NewResponseController(w)

	// A zero deadline means "no deadline". A ResponseWriter that does not
	// support this returns ErrNotSupported, which is not fatal: the stream then
	// lives until WriteTimeout and the client reconnects, degraded but working.
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		s.logger.DebugContext(r.Context(), "could not clear the sse write deadline",
			slog.String("error", err.Error()))
	}
	if err := controller.SetReadDeadline(time.Time{}); err != nil {
		s.logger.DebugContext(r.Context(), "could not clear the sse read deadline",
			slog.String("error", err.Error()))
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	// no-store as well as no-cache: an intermediary must not retain event data,
	// which describes a privileged host.
	header.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	writer := &sseWriter{w: w, controller: controller, logger: s.logger}

	// The browser's own reconnect delay. Sending it means a client that drops
	// during a daemon restart backs off the way the server wants rather than
	// using its three-second default.
	if hint := s.eventEngine.ReconnectHint(); hint > 0 {
		writer.retry(hint)
	}
	if !writer.flush() {
		return nil, false
	}
	return writer, true
}

// replay re-sends stored events after the client's Last-Event-ID.
//
// Bounded by the configured replay cap. A client that fell a long way behind is
// told its replay was truncated rather than being handed a silent hole: the
// paginated history endpoint is the right tool for a large gap, and streaming
// ten thousand events into a browser to catch up is not.
//
// Returns the highest sequence the client now holds.
func (s *Server) replay(r *http.Request, writer *sseWriter) int64 {
	after, ok := lastEventID(r)
	if !ok || s.dockerEvents == nil {
		return 0
	}

	limit := s.eventEngine.ReplayLimit()
	if limit <= 0 {
		return after
	}

	events, total, err := s.dockerEvents.Since(r.Context(), after, limit)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "event replay failed", slog.String("error", err.Error()))
		return after
	}

	last := after
	for _, event := range events {
		if !writer.event(sseEventDocker, event) {
			return last
		}
		last = event.Sequence
		writer.replayed++
	}

	if total > len(events) {
		writer.event(sseEventTruncated, truncatedPayload{
			Skipped: total - len(events),
			Limit:   limit,
			Notice: "The replay was capped. Reload the event history from " +
				APIPrefix + "/events to see what was skipped.",
		})
	}

	writer.flush()
	return last
}

// lastEventID reads the client's resume point.
//
// The Last-Event-ID HEADER is what the browser's EventSource sends
// automatically on reconnect. The query parameter is accepted too, because
// EventSource offers no way to set a header on the initial connection, so a
// client resuming a session it persisted has no other route.
func lastEventID(r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("lastEventId"))
	}
	if raw == "" {
		return 0, false
	}

	// A malformed or negative ID is ignored rather than rejected: the client is
	// mid-reconnect, and failing the request would leave it with no stream at
	// all. Starting live is the safe degradation.
	sequence, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || sequence < 0 {
		return 0, false
	}
	return sequence, true
}

// sseWriter frames Server-Sent Events.
//
// Every method reports success, and a false return means the client has gone.
// The caller returns immediately on false rather than continuing to write into
// a dead connection.
type sseWriter struct {
	w          http.ResponseWriter
	controller *http.ResponseController
	logger     *slog.Logger
	replayed   int
	// failed latches the first write error, so one dead connection produces one
	// log line rather than one per frame.
	failed bool
}

// event writes a named event frame carrying JSON.
//
// Events with a sequence get an `id:` field, which is what the browser echoes
// back as Last-Event-ID on reconnect. A payload without one (the ready frame)
// gets no id, so it cannot become a resume point.
func (e *sseWriter) event(name string, payload any) bool {
	if e.failed {
		return false
	}

	body, err := json.Marshal(payload)
	if err != nil {
		// A payload that cannot be encoded is dropped rather than written
		// half-formed, which would desynchronise the client's parser.
		e.logger.Error("could not encode an sse payload",
			slog.String("event", name), slog.String("error", err.Error()))
		return true
	}

	var frame strings.Builder
	if event, ok := payload.(domain.DockerEvent); ok && event.Sequence > 0 {
		frame.WriteString("id: ")
		frame.WriteString(strconv.FormatInt(event.Sequence, 10))
		frame.WriteByte('\n')
	}
	frame.WriteString("event: ")
	frame.WriteString(name)
	frame.WriteByte('\n')

	// One data: line per line of payload, per the SSE grammar. The JSON encoder
	// escapes newlines inside strings, so a marshalled payload is single-line in
	// practice -- splitting anyway means a future multi-line payload cannot
	// silently truncate the frame.
	for _, line := range strings.Split(string(body), "\n") {
		frame.WriteString("data: ")
		frame.WriteString(line)
		frame.WriteByte('\n')
	}
	frame.WriteByte('\n')

	return e.write(frame.String())
}

// comment writes a comment frame, used for heartbeats.
func (e *sseWriter) comment(text string) bool {
	return e.write(": " + text + "\n\n")
}

// retry tells the client how long to wait before reconnecting.
func (e *sseWriter) retry(delay time.Duration) bool {
	return e.write(fmt.Sprintf("retry: %d\n\n", delay.Milliseconds()))
}

func (e *sseWriter) write(frame string) bool {
	if e.failed {
		return false
	}
	if _, err := io.WriteString(e.w, frame); err != nil {
		e.failed = true
		return false
	}
	return true
}

// flush pushes buffered bytes to the client.
//
// Without this every frame would sit in the response buffer until it filled,
// which is the difference between a live stream and a periodic one. Flush
// reaches the underlying writer through the access-log wrapper because that
// wrapper implements Unwrap.
func (e *sseWriter) flush() bool {
	if e.failed {
		return false
	}
	if err := e.controller.Flush(); err != nil {
		e.failed = true
		return false
	}
	return true
}
