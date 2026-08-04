package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// DockerEventReader is the event-history capability the API depends on.
//
// A narrow interface rather than the concrete repository, matching the rest of
// this package, so handlers can be tested with simple doubles and no database.
type DockerEventReader interface {
	List(ctx context.Context, filter store.DockerEventFilter) ([]domain.DockerEvent, int, error)
	Get(ctx context.Context, sequence int64) (*domain.DockerEvent, error)
	Since(ctx context.Context, after int64, limit int) ([]domain.DockerEvent, int, error)
	DistinctEventProjects(ctx context.Context) ([]string, error)
	DistinctEventActions(ctx context.Context) ([]string, error)
}

// EventEngineReader is the event-engine status capability the API depends on.
type EventEngineReader interface {
	Enabled() bool
	Status(ctx context.Context) domain.EventEngineStatus
	// Subscribe registers an SSE client, or reports the limit is reached.
	Subscribe() (*service.StreamSubscription, error)
	ReplayLimit() int
	HeartbeatInterval() time.Duration
	ReconnectHint() time.Duration
}

// eventDetail is the event-detail response.
//
// It wraps the event with resolved links rather than embedding them in the
// domain type: a link is a presentation concern, and the domain model is also
// what goes to SQLite.
type eventDetail struct {
	domain.DockerEvent
	// Links point at the resources this event concerns, when HarborMaster can
	// resolve them. Absent rather than guessed when it cannot.
	Links eventLinks `json:"links"`
	// Redacted is always true: attribute values whose key matched a sensitive
	// pattern were replaced before the event was ever stored. Stated in the
	// payload so a client is not left to assume it.
	Redacted bool `json:"redacted"`
}

type eventLinks struct {
	Container string `json:"container,omitempty"`
	Image     string `json:"image,omitempty"`
}

// handleEvents lists Docker event history.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.dockerEvents == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"the event engine is not configured")
		return
	}

	filter, err := parseEventFilter(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	events, total, err := s.dockerEvents.List(r.Context(), filter)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "event list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	page := 1
	if filter.Page.Limit > 0 {
		page = filter.Page.Offset/filter.Page.Limit + 1
	}

	writeJSON(w, r, s.logger, http.StatusOK, listResponse[domain.DockerEvent]{
		Items:      events,
		Pagination: newPagination(page, filter.Page.Limit, total),
	})
}

// handleEventDetail returns one event by its local sequence number.
func (s *Server) handleEventDetail(w http.ResponseWriter, r *http.Request) {
	if s.dockerEvents == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"the event engine is not configured")
		return
	}

	raw := r.PathValue("id")
	sequence, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || sequence < 1 {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"an event id must be a positive integer sequence number")
		return
	}

	event, err := s.dockerEvents.Get(r.Context(), sequence)
	if err != nil {
		s.writeLookupError(w, r, err, "event")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, eventDetail{
		DockerEvent: *event,
		Links:       linksFor(*event),
		Redacted:    true,
	})
}

// linksFor resolves the API paths for the resources an event concerns.
//
// Only emitted when the actor ID is one the linked endpoint can actually
// resolve. A link to a container that was destroyed still works: the row is
// retained and marked absent, which is the useful answer.
func linksFor(event domain.DockerEvent) eventLinks {
	var links eventLinks
	if event.ActorID == "" {
		return links
	}

	switch event.Type {
	case domain.EventTypeContainer:
		links.Container = APIPrefix + "/containers/" + event.ActorID
	case domain.EventTypeImage:
		links.Image = APIPrefix + "/images/" + event.ActorID
	}
	return links
}

// handleEventEngine reports event-engine status.
func (s *Server) handleEventEngine(w http.ResponseWriter, r *http.Request) {
	if s.eventEngine == nil {
		// Not configured is different from disabled: report a disabled engine
		// rather than an error, so a client renders the same "off" state either
		// way instead of an unexplained 503.
		writeJSON(w, r, s.logger, http.StatusOK, domain.EventEngineStatus{
			Enabled: false,
			State:   domain.ConnStateDisabled,
		})
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, s.eventEngine.Status(r.Context()))
}

// handleEventFilters reports the filter vocabularies event history contains, so
// the UI populates its selectors from real data rather than a hard-coded list.
func (s *Server) handleEventFilters(w http.ResponseWriter, r *http.Request) {
	if s.dockerEvents == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"the event engine is not configured")
		return
	}

	projects, err := s.dockerEvents.DistinctEventProjects(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "event project list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	actions, err := s.dockerEvents.DistinctEventActions(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "event action list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	types := make([]string, 0, len(domain.DockerEventTypes))
	for _, eventType := range domain.DockerEventTypes {
		types = append(types, string(eventType))
	}
	results := make([]string, 0, len(domain.EventProcessingResults))
	for _, result := range domain.EventProcessingResults {
		results = append(results, string(result))
	}

	writeJSON(w, r, s.logger, http.StatusOK, map[string]any{
		"types":      types,
		"actions":    actions,
		"results":    results,
		"projects":   projects,
		"sortFields": store.EventSortFields(),
	})
}
