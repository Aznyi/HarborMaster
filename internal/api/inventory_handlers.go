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
	// Attention gathers, for one PAGE of containers, what HarborMaster knows
	// about each of them. A fixed number of batched queries whatever the page
	// size -- see internal/store/attention_repository.go for why that bound
	// is the point of the method existing at all.
	Attention(ctx context.Context, keys []store.ContainerKey) (map[string]domain.ContainerEvidence, error)
}

// LineageReader reports what a container FOLLOWS, as distinct from the
// immutable digest it runs.
//
// One method: the detail view asks about one container. There is deliberately
// nothing here that WRITES -- lineage is established by reconciliation and
// advanced by a verified recreation, and no HTTP request may set it.
type LineageReader interface {
	Get(ctx context.Context, containerName string) (domain.ImageLineage, error)
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

	s.auditWrite(r, domain.AuditInventoryRefreshed, domain.AuditTargetInventory,
		"", "", "manual refresh requested")

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

	items, err := s.withAttention(r.Context(), summaries)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "container attention lookup failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, listResponse[containerListItem]{
		Items:      items,
		Pagination: newPagination(pageFromFilter(filter), filter.Page.Limit, total),
	})
}

// containerListItem is a container row plus what HarborMaster knows about it.
//
// The summary is EMBEDDED, so the wire shape gains a field and changes none:
// an existing client reading `name` or `state` sees exactly what it saw
// before. The addition is one object under `attention`.
type containerListItem struct {
	domain.ContainerSummary
	Attention domain.ContainerAttention `json:"attention"`
}

// withAttention decorates a page of containers.
//
// ONE call to the store for the whole page, then a pure assessment per row.
// The alternative -- asking about each container in turn -- is the shape the
// Phase 10 rollback-eligibility defect had, and a container list is the most
// frequently rendered page in the product.
//
// A container with no evidence gets the zero value, which the assessment reads
// as "not checked". That is deliberate and it is the reason this cannot fall
// back to a default: an evidence lookup that quietly returned nothing would
// paint a whole page as unexamined.
func (s *Server) withAttention(
	ctx context.Context,
	summaries []domain.ContainerSummary,
) ([]containerListItem, error) {
	items := make([]containerListItem, 0, len(summaries))
	if len(summaries) == 0 {
		return items, nil
	}

	keys := make([]store.ContainerKey, 0, len(summaries))
	for _, summary := range summaries {
		keys = append(keys, store.ContainerKey{ID: summary.ID, Name: summary.Name})
	}

	evidence, err := s.containers.Attention(ctx, keys)
	if err != nil {
		return nil, err
	}

	// One dependency read for the whole page, never one per row.
	dependencies := s.dependencyFacts(ctx)

	for _, summary := range summaries {
		row := evidence[summary.ID]
		// The inventory row is the authority on its own state; the store fills
		// only what the other subsystems know.
		row.Health = summary.Health
		row.State = summary.State
		row.Present = summary.Present
		if facts, known := dependencies[domain.NormaliseContainerName(summary.Name)]; known {
			facts.Evidence(&row)
		}

		items = append(items, containerListItem{
			ContainerSummary: summary,
			Attention:        domain.AssessContainer(row),
		})
	}
	return items, nil
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

	// What this container FOLLOWS, when HarborMaster follows anything for it.
	// Absent rather than empty when it does not: "nothing is tracked" is a real
	// answer and must not read as "tracking something unnamed".
	if s.lineage != nil {
		if lineage, lineageErr := s.lineage.Get(r.Context(), detail.Overview.Name); lineageErr == nil {
			detail.ImageLineage = &lineage
		}
	}

	// The same projection the list row carries, so the container's own page and
	// the row that led to it can never disagree about whether an update exists.
	// One batched lookup for one container: the identical code path the list
	// uses, which is what keeps the two definitions from drifting apart.
	response := containerDetailResponse{ContainerDetail: *detail}
	evidence, attentionErr := s.containers.Attention(r.Context(),
		[]store.ContainerKey{{ID: detail.Overview.ID, Name: detail.Overview.Name}})
	if attentionErr != nil {
		s.logger.ErrorContext(r.Context(), "container attention lookup failed",
			slog.String("error", attentionErr.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	row := evidence[detail.Overview.ID]
	row.Health = detail.Overview.Health
	row.State = detail.Overview.State
	row.Present = detail.Overview.Present
	// The same dependency facts the list row carries, through the same call, so
	// a container's own page and the row that led to it cannot disagree.
	if facts, known := s.dependencyFacts(r.Context())[domain.NormaliseContainerName(
		detail.Overview.Name)]; known {
		facts.Evidence(&row)
	}
	response.Attention = domain.AssessContainer(row)

	writeJSON(w, r, s.logger, http.StatusOK, response)
}

// containerDetailResponse is the container detail plus its attention block.
//
// The detail is EMBEDDED, so every field the page read before is in the same
// place and `attention` is the addition.
type containerDetailResponse struct {
	domain.ContainerDetail
	Attention domain.ContainerAttention `json:"attention"`
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
