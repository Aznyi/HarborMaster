package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The image intelligence endpoints.
//
// # What they can and cannot do
//
// Three reads and one write. The write SCHEDULES a metadata collection pass; it
// pulls nothing, changes no container, and reaches no Docker capability. There
// is no endpoint here that applies an update, and adding one would have to get
// past the architecture test that keeps the Docker SDK inside internal/docker.
//
// # The parameter that does not exist
//
// None of these endpoints accepts a registry, a host, a URL, or a scheme. The
// refresh endpoint takes NO BODY AND NO TARGET at all. That is deliberate and
// it is the API-layer half of the SSRF defence: registry destinations come only
// from image references the inventory already holds, so there is no request a
// caller can compose that steers HarborMaster at an address of their choosing.
//
// The filters below narrow what is READ FROM THE DATABASE. A registry filter
// matches a stored host exactly; it cannot introduce one.

// ImageIntelReader is the image intelligence capability the API depends on.
//
// A narrow interface rather than *store.ImageIntelRepository, so the handlers
// stay testable without a database and so the surface the API can reach is
// visible in one place. Note what is ABSENT: nothing that writes a record,
// nothing that contacts a registry, and nothing that takes a host.
type ImageIntelReader interface {
	List(ctx context.Context, filter store.ImageIntelFilter) ([]domain.ImageIntel, int, error)
	Get(ctx context.Context, reference string) (domain.ImageIntel, error)
	ForImageID(ctx context.Context, imageID string) ([]domain.ImageIntel, error)
	History(ctx context.Context, reference string, page store.Page) ([]domain.ImageUpdateEvent, int, error)
	Summary(ctx context.Context) (domain.ImageIntelSummary, error)
}

// ImageIntelCollector is the engine capability the refresh endpoint needs.
//
// Deliberately narrow: the API can ASK for a pass and read the engine's status.
// It cannot collect synchronously, which is what keeps an unauthenticated
// caller from holding a request open across a whole-estate sweep -- or from
// driving registry traffic on demand.
type ImageIntelCollector interface {
	RequestCollection()
	Status(ctx context.Context) domain.ImageIntelEngineStatus
}

// imageIntelUnavailable writes the disabled response, and reports whether it
// did.
func (s *Server) imageIntelUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.imageIntel != nil {
		return false
	}
	writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
		"image intelligence is not configured")
	return true
}

// imageUpdatesResponse is the updates listing, with the estate aggregate.
//
// The summary travels WITH the list so a dashboard renders in one request, and
// so the coverage numbers are always beside the update numbers -- an update
// count without "how many were actually checked" invites reading a stale estate
// as a healthy one.
type imageUpdatesResponse struct {
	Items      []domain.ImageIntel      `json:"items"`
	Pagination Pagination               `json:"pagination"`
	Summary    domain.ImageIntelSummary `json:"summary"`
}

// handleImageUpdates lists tracked image references and the estate aggregate.
func (s *Server) handleImageUpdates(w http.ResponseWriter, r *http.Request) {
	if s.imageIntelUnavailable(w, r) {
		return
	}

	query, err := parseImageIntelQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	records, total, err := s.imageIntel.List(r.Context(), query.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "image updates list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	summary, err := s.imageIntel.Summary(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "image summary failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, imageUpdatesResponse{
		Items:      records,
		Pagination: newPagination(query.Page, query.PageSize, total),
		Summary:    summary,
	})
}

// imageDetailResponse is one local image with its registry intelligence.
//
// store.ImageUsage is EMBEDDED rather than nested, so this response is a strict
// superset of the Phase 2 one: existing clients see the same `image` and
// `containerCount` fields in the same places, and `intel` is simply new. An
// endpoint that already had consumers is extended, not replaced.
//
// The intelligence is a LIST because one local image can carry several
// references -- the same content tagged twice -- and picking one arbitrarily
// would hide the others.
type imageDetailResponse struct {
	store.ImageUsage
	// Intel is empty when nothing about this image has been tracked yet, which
	// is different from "no updates available" and is left for the client to
	// distinguish.
	Intel []domain.ImageIntel `json:"intel"`
}

// handleImageDetail returns one image with its registry intelligence.
//
// Extends the Phase 2 endpoint rather than adding a second one: an operator
// looking at an image wants both what the daemon knows and what the registry
// says, and two endpoints would mean two requests to answer one question.
func (s *Server) handleImageDetail(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeUnavailable,
			"the image inventory is not configured")
		return
	}

	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 256 {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the image id must be between 1 and 256 characters")
		return
	}

	usage, err := s.images.Get(r.Context(), id)
	if err != nil {
		// The shared renderer, so an ambiguous prefix answers 409 here exactly
		// as it does on every other id-resolving route.
		s.writeLookupError(w, r, err, "image")
		return
	}

	response := imageDetailResponse{ImageUsage: *usage, Intel: []domain.ImageIntel{}}

	// Intelligence is optional. A deployment with the engine disabled still
	// serves the image, without an empty section implying the registry said
	// nothing.
	if s.imageIntel != nil {
		intel, intelErr := s.imageIntel.ForImageID(r.Context(), usage.Image.ID)
		if intelErr != nil {
			s.logger.ErrorContext(r.Context(), "image intel load failed",
				slog.String("error", intelErr.Error()))
			writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
			return
		}
		response.Intel = intel
	}

	writeJSON(w, r, s.logger, http.StatusOK, response)
}

// handleImageHistory returns the observed changes for one image.
//
// Scoped through the LOCAL IMAGE id, so the path is the same identifier the
// rest of the image API uses. Every reference resolving to that image
// contributes, because an operator asking "what changed about this image" means
// the content, not one of its names.
func (s *Server) handleImageHistory(w http.ResponseWriter, r *http.Request) {
	if s.imageIntelUnavailable(w, r) {
		return
	}
	if s.images == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeUnavailable,
			"the image inventory is not configured")
		return
	}

	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 256 {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the image id must be between 1 and 256 characters")
		return
	}

	page, pageSize, err := parsePage(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	usage, err := s.images.Get(r.Context(), id)
	if err != nil {
		s.writeLookupError(w, r, err, "image")
		return
	}

	references, err := s.imageIntel.ForImageID(r.Context(), usage.Image.ID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "image intel load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	// One reference is the common case by a wide margin, and it is the one the
	// history index serves directly. Several references are merged in memory
	// from bounded pages, which is correct and cheap because ForImageID is
	// itself bounded.
	events := make([]domain.ImageUpdateEvent, 0, pageSize)
	total := 0
	for _, reference := range references {
		found, count, historyErr := s.imageIntel.History(r.Context(), reference.Reference,
			store.Page{Limit: pageSize, Offset: (page - 1) * pageSize})
		if historyErr != nil {
			s.logger.ErrorContext(r.Context(), "image history load failed",
				slog.String("error", historyErr.Error()))
			writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
			return
		}
		events = append(events, found...)
		total += count
	}

	writeJSON(w, r, s.logger, http.StatusOK, imageHistoryResponse{
		ImageID:    usage.Image.ID,
		References: referenceNames(references),
		Items:      events,
		Pagination: newPagination(page, pageSize, total),
	})
}

// imageHistoryResponse is one image's observed changes.
type imageHistoryResponse struct {
	ImageID string `json:"imageId"`
	// References names every tracked reference that resolved to this image, so
	// a merged history is attributable.
	References []string                  `json:"references"`
	Items      []domain.ImageUpdateEvent `json:"items"`
	Pagination Pagination                `json:"pagination"`
}

// referenceNames extracts the canonical references from a record set.
func referenceNames(records []domain.ImageIntel) []string {
	names := make([]string, 0, len(records))
	for _, record := range records {
		names = append(names, record.Reference)
	}
	return names
}

// imageRefreshResponse acknowledges a requested collection pass.
type imageRefreshResponse struct {
	// Requested is always true on a 202: the endpoint schedules work and does
	// not wait for it.
	Requested bool                          `json:"requested"`
	Engine    domain.ImageIntelEngineStatus `json:"engine"`
}

// handleImageRefresh requests a metadata collection pass.
//
// # What it does not do
//
// It does not pull an image, does not touch Docker, and takes no target of any
// kind. It sets a flag that the background worker reads; the worker then
// re-projects the inventory's references and looks up whatever is due, bounded
// by the same concurrency and batch limits every scheduled pass uses.
//
// ASYNCHRONOUS deliberately. A synchronous collection would let an
// unauthenticated caller hold a request open across hundreds of registry
// requests, and would hand them a way to generate outbound traffic on demand.
// The request is coalesced -- calling it in a loop produces one pass, not a
// backlog -- and it is rate limited on top of that.
func (s *Server) handleImageRefresh(w http.ResponseWriter, r *http.Request) {
	if s.imageCollector == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"image intelligence is not configured")
		return
	}

	s.imageCollector.RequestCollection()
	s.auditWrite(r, domain.AuditImageRefreshed, domain.AuditTargetSystem, "", "",
		"registry metadata collection requested")

	writeJSON(w, r, s.logger, http.StatusAccepted, imageRefreshResponse{
		Requested: true,
		Engine:    s.imageCollector.Status(r.Context()),
	})
}

// ------------------------------------------------------------- query part --

// maxImageFilterValues bounds how many values one repeatable filter may carry.
const maxImageFilterValues = 32

// maxRegistryFilterBytes bounds one registry filter value.
//
// A registry filter is matched EXACTLY against a stored host and travels as a
// bound parameter. It cannot introduce a destination -- but it is still caller
// input reaching a query, so it is bounded like everything else.
const maxRegistryFilterBytes = 253

// imageIntelQuery is a parsed and validated image listing request.
type imageIntelQuery struct {
	Updates     []domain.UpdateType
	Statuses    []domain.CheckStatus
	Registries  []string
	UpdatesOnly bool
	InUseOnly   bool
	Search      string

	Sort      string
	Ascending bool
	Page      int
	PageSize  int
}

// parseImageIntelQuery reads and validates the listing parameters.
func parseImageIntelQuery(query url.Values) (imageIntelQuery, error) {
	// Sorted by update significance, descending: the dashboard's question is
	// "what most needs attention", and alphabetical order answers a different
	// one.
	parsed := imageIntelQuery{Sort: "update"}

	page, pageSize, err := parsePage(query)
	if err != nil {
		return parsed, err
	}
	parsed.Page, parsed.PageSize = page, pageSize

	if raw := strings.TrimSpace(query.Get("sort")); raw != "" {
		if !store.ValidImageIntelSortField(raw) {
			return parsed, invalidParam("sort",
				"one of update, reference, registry, status, lastChecked, containers, id")
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
		{"updatesOnly", &parsed.UpdatesOnly},
		{"inUseOnly", &parsed.InUseOnly},
	} {
		if raw := strings.TrimSpace(query.Get(flag.name)); raw != "" {
			switch raw {
			case "true":
				*flag.into = true
			case "false":
				*flag.into = false
			default:
				return parsed, invalidParam(flag.name, "true or false")
			}
		}
	}

	if raw := strings.TrimSpace(query.Get("search")); raw != "" {
		if len(raw) > domain.MaxReferenceBytes {
			return parsed, invalidParam("search", "at most 512 characters")
		}
		parsed.Search = raw
	}

	updates, err := parseVocabulary(query, "update", domain.ValidUpdateType,
		"one of none, digest, patch, minor, major, prerelease, unknown")
	if err != nil {
		return parsed, err
	}
	for _, value := range updates {
		parsed.Updates = append(parsed.Updates, domain.UpdateType(value))
	}

	statuses, err := parseVocabulary(query, "status", domain.ValidCheckStatus,
		"one of pending, ok, failed, rateLimited, unauthorized, notFound, unsupported")
	if err != nil {
		return parsed, err
	}
	for _, value := range statuses {
		parsed.Statuses = append(parsed.Statuses, domain.CheckStatus(value))
	}

	// A registry filter is validated against the SHAPE of a host rather than
	// against a vocabulary, because the set of registries is whatever the estate
	// happens to reference. It is matched exactly against a stored value and
	// travels as a bound parameter, so it narrows a query and can never widen
	// it into a destination.
	for _, raw := range query["registry"] {
		for _, part := range strings.Split(raw, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			if len(parsed.Registries) >= maxImageFilterValues {
				return parsed, invalidParam("registry", "at most 32 values")
			}
			if len(trimmed) > maxRegistryFilterBytes || !validRegistryFilter(trimmed) {
				return parsed, invalidParam("registry", "a registry host name")
			}
			parsed.Registries = append(parsed.Registries, trimmed)
		}
	}

	return parsed, nil
}

// validRegistryFilter reports whether a filter value has the shape of a host.
//
// An allowlist of the characters a host name can contain. Deliberately NOT
// domain.ContactableRegistryHost: this value is only ever compared against a
// stored column, so refusing a host that is stored but not contactable -- an
// unsupported reference the dashboard still lists -- would make part of the data
// unfilterable.
func validRegistryFilter(value string) bool {
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == ':':
		default:
			return false
		}
	}
	return true
}

// filter converts a validated query into a repository filter.
func (q imageIntelQuery) filter() store.ImageIntelFilter {
	return store.ImageIntelFilter{
		Updates:     q.Updates,
		Statuses:    q.Statuses,
		Registries:  q.Registries,
		UpdatesOnly: q.UpdatesOnly,
		InUseOnly:   q.InUseOnly,
		Search:      q.Search,
		Sort:        q.Sort,
		Ascending:   q.Ascending,
		Page: store.Page{
			Limit:  q.PageSize,
			Offset: (q.Page - 1) * q.PageSize,
		},
	}
}
