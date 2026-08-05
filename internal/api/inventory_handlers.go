package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// InventoryReader is the inventory capability the API depends on.
//
// Narrow interfaces, not the concrete service, so handlers can be tested with
// simple doubles and no database.
type InventoryReader interface {
	Enabled() bool
	Status(ctx context.Context) (domain.InventoryStatus, error)
	// TriggerAsync starts a background refresh, reporting false when one is
	// already running along with when it started.
	TriggerAsync(trigger domain.RefreshTrigger) (bool, time.Time)
	// CheckRuntime reports whether the container runtime is reachable.
	CheckRuntime(ctx context.Context) error
}

// ContainerReader is the container query capability the API depends on.
type ContainerReader interface {
	List(ctx context.Context, filter store.ContainerFilter) ([]domain.ContainerSummary, int, error)
	Get(ctx context.Context, id string) (*domain.ContainerDetail, error)
	ResolveID(ctx context.Context, reference string) (string, error)
	RawInspection(ctx context.Context, id string) ([]byte, error)
	DistinctComposeProjects(ctx context.Context) ([]string, error)
	DistinctImages(ctx context.Context) ([]string, error)
}

// WarningReader supplies a container's inventory warnings.
type WarningReader interface {
	WarningsForContainer(ctx context.Context, containerID string) ([]domain.InventoryWarning, error)
}

// ImageReader is the image query capability the API depends on.
type ImageReader interface {
	List(ctx context.Context, page store.Page) ([]store.ImageUsage, int, error)
	Get(ctx context.Context, id string) (*store.ImageUsage, error)
}

// NetworkReader lists networks.
type NetworkReader interface {
	List(ctx context.Context, page store.Page) ([]domain.Network, int, error)
}

// VolumeReader lists volumes.
type VolumeReader interface {
	List(ctx context.Context, page store.Page) ([]domain.Volume, int, error)
}

// refreshAccepted is the body of a successful manual refresh trigger.
type refreshAccepted struct {
	// Accepted is always true here; a rejected refresh is a 409 instead.
	Accepted  bool      `json:"accepted"`
	Trigger   string    `json:"trigger"`
	StartedAt time.Time `json:"startedAt"`
	Message   string    `json:"message"`
}

// refreshConflict is the body of a refresh rejected because one is running.
type refreshConflict struct {
	Error     ErrorBody `json:"error"`
	RequestID string    `json:"requestId,omitempty"`
	// Active describes the refresh that is already in flight, so a client gets
	// something actionable rather than a bare conflict.
	Active struct {
		InProgress bool      `json:"inProgress"`
		StartedAt  time.Time `json:"startedAt"`
	} `json:"active"`
}

// handleInventory reports inventory and refresh status.
func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) {
	if s.inventory == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"inventory is not configured")
		return
	}

	status, err := s.inventory.Status(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "inventory status failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	writeJSON(w, r, s.logger, http.StatusOK, status)
}

// handleInventoryRefresh triggers a manual refresh.
//
// The refresh is asynchronous and the response is 202: a sweep of a thousand
// containers cannot be something an HTTP client waits on, and the server's own
// write timeout would kill the connection long before it finished. Poll
// GET /api/v1/inventory for completion.
func (s *Server) handleInventoryRefresh(w http.ResponseWriter, r *http.Request) {
	if s.inventory == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"inventory is not configured")
		return
	}
	if !s.inventory.Enabled() {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"inventory is disabled by configuration")
		return
	}

	// The same write-endpoint protections as snapshot capture. This endpoint
	// shipped without them; leaving a weaker sibling next to a hardened one
	// would just move the abuse here. A refresh drives a full sweep of a
	// privileged socket, so an unbounded request rate is a real amplifier.
	if err := s.guardWrite(r); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	// Checked up front so an unreachable daemon is an immediate, honest 503
	// rather than an accepted refresh that fails seconds later out of sight.
	if err := s.inventory.CheckRuntime(r.Context()); err != nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeUnavailable,
			"container runtime is unreachable")
		return
	}

	accepted, startedAt := s.inventory.TriggerAsync(domain.TriggerManual)
	if !accepted {
		s.writeRefreshConflict(w, r, startedAt)
		return
	}

	writeJSON(w, r, s.logger, http.StatusAccepted, refreshAccepted{
		Accepted:  true,
		Trigger:   string(domain.TriggerManual),
		StartedAt: startedAt,
		Message:   "refresh started; poll GET /api/v1/inventory for completion",
	})
}

// writeRefreshConflict reports an already-running refresh as 409, including
// the active refresh's start time.
func (s *Server) writeRefreshConflict(w http.ResponseWriter, r *http.Request, startedAt time.Time) {
	body := refreshConflict{
		Error: ErrorBody{
			Code:    CodeConflict,
			Message: "an inventory refresh is already in progress",
		},
		RequestID: RequestIDFrom(r.Context()),
	}
	body.Active.InProgress = true
	body.Active.StartedAt = startedAt

	writeJSON(w, r, s.logger, http.StatusConflict, body)
}

// handleContainers lists containers with filtering, sorting, and pagination.
func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	if s.containers == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"inventory is not configured")
		return
	}

	filter, err := parseContainerFilter(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	summaries, total, err := s.containers.List(r.Context(), filter)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "container list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, listResponse[domain.ContainerSummary]{
		Items:      summaries,
		Pagination: newPagination(pageFromFilter(filter), filter.Page.Limit, total),
	})
}

// handleContainerFilters reports the distinct filter values present, so the UI
// can populate its selectors from real data rather than guessing.
func (s *Server) handleContainerFilters(w http.ResponseWriter, r *http.Request) {
	if s.containers == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"inventory is not configured")
		return
	}

	projects, err := s.containers.DistinctComposeProjects(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "compose project list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	images, err := s.containers.DistinctImages(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "image list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	states := make([]string, 0, len(domain.ContainerStates))
	for _, state := range domain.ContainerStates {
		states = append(states, string(state))
	}
	health := make([]string, 0, len(domain.HealthStates))
	for _, state := range domain.HealthStates {
		health = append(health, string(state))
	}

	writeJSON(w, r, s.logger, http.StatusOK, map[string]any{
		"states":     states,
		"health":     health,
		"projects":   projects,
		"images":     images,
		"sortFields": store.SortFields(),
	})
}

// handleContainerDetail returns one container's normalized detail.
func (s *Server) handleContainerDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveContainer(w, r)
	if !ok {
		return
	}

	detail, err := s.containers.Get(r.Context(), id)
	if err != nil {
		s.writeLookupError(w, r, err, "container")
		return
	}

	// Warnings live in their own table so they survive independently of the
	// container row; attached here so the detail view is self-contained.
	if s.warnings != nil {
		if warnings, warnErr := s.warnings.WarningsForContainer(r.Context(), id); warnErr == nil {
			detail.Warnings = warnings
		}
	}

	writeJSON(w, r, s.logger, http.StatusOK, detail)
}

// handleContainerRaw returns the redacted raw inspection payload.
//
// Deliberately its own endpoint. The default container response must not carry
// it: it is large, it is only useful for troubleshooting, and keeping it
// separate means a client cannot receive it by accident.
func (s *Server) handleContainerRaw(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveContainer(w, r)
	if !ok {
		return
	}

	raw, err := s.containers.RawInspection(r.Context(), id)
	if err != nil {
		s.writeLookupError(w, r, err, "raw inspection data")
		return
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, map[string]any{
		"containerId": id,
		"redacted":    true,
		"notice": "Sensitive values have been removed. This payload is for " +
			"troubleshooting only and cannot be used to recreate the container exactly.",
		"inspection": decoded,
	})
}

// resolveContainer maps the {id} path value onto a full container ID.
func (s *Server) resolveContainer(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.containers == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"inventory is not configured")
		return "", false
	}

	reference := r.PathValue("id")
	if reference == "" {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"a container id is required")
		return "", false
	}

	id, err := s.containers.ResolveID(r.Context(), reference)
	if err != nil {
		s.writeLookupError(w, r, err, "container")
		return "", false
	}
	return id, true
}

// writeLookupError maps repository errors onto status codes.
//
// An ambiguous prefix is 409 rather than 404: the resource exists, more than
// once, and picking one arbitrarily is exactly the guess an operations tool
// must not make.
func (s *Server) writeLookupError(w http.ResponseWriter, r *http.Request, err error, subject string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, subject+" not found")
	case errors.Is(err, store.ErrAmbiguousID):
		writeError(w, r, s.logger, http.StatusConflict, CodeAmbiguousID,
			"the id prefix matches more than one "+subject+"; supply more characters")
	default:
		s.logger.ErrorContext(r.Context(), "lookup failed",
			slog.String("subject", subject), slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
	}
}

// handleImages lists images with reference counts.
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"inventory is not configured")
		return
	}

	page, pageSize, err := parsePage(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	usages, total, err := s.images.List(r.Context(),
		store.Page{Limit: pageSize, Offset: (page - 1) * pageSize})
	if err != nil {
		s.logger.ErrorContext(r.Context(), "image list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, listResponse[store.ImageUsage]{
		Items:      usages,
		Pagination: newPagination(page, pageSize, total),
	})
}

// handleImageDetail returns one image.
// handleNetworks lists networks.
func (s *Server) handleNetworks(w http.ResponseWriter, r *http.Request) {
	if s.networks == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"inventory is not configured")
		return
	}

	page, pageSize, err := parsePage(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	networks, total, err := s.networks.List(r.Context(),
		store.Page{Limit: pageSize, Offset: (page - 1) * pageSize})
	if err != nil {
		s.logger.ErrorContext(r.Context(), "network list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, listResponse[domain.Network]{
		Items:      networks,
		Pagination: newPagination(page, pageSize, total),
	})
}

// handleVolumes lists volumes.
func (s *Server) handleVolumes(w http.ResponseWriter, r *http.Request) {
	if s.volumes == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"inventory is not configured")
		return
	}

	page, pageSize, err := parsePage(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	volumes, total, err := s.volumes.List(r.Context(),
		store.Page{Limit: pageSize, Offset: (page - 1) * pageSize})
	if err != nil {
		s.logger.ErrorContext(r.Context(), "volume list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, listResponse[domain.Volume]{
		Items:      volumes,
		Pagination: newPagination(page, pageSize, total),
	})
}

// Compile-time check that the concrete service satisfies the API's interface.
var _ InventoryReader = (*service.InventoryService)(nil)
