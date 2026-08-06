package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The change plan endpoints.
//
// # What they can and cannot do
//
// Four routes: three reads and one write. The write GENERATES HarborMaster's
// own analysis of HarborMaster's own database. It pulls nothing, changes no
// container, restores nothing, and schedules no change -- there is no route here
// that could, and adding one would have to get past the architecture test that
// keeps the Docker SDK inside internal/docker.
//
// A plan is analysis. Nothing in this API applies one, and the plan model has
// no state to apply: no status column, no queue, no target.
//
// # Immutability at the boundary
//
// There is no PATCH and no DELETE. A plan records what was known at one moment;
// a changed world produces a new plan and the old one remains as the record of
// what was believed when a decision was made. An endpoint that edited one would
// destroy exactly the property that makes plans worth keeping.

// PlanReader is the plan capability the API depends on.
//
// A narrow interface rather than *store.PlanRepository, so the handlers stay
// testable without a database and so the surface the API can reach is visible
// in one place. Note what is ABSENT: nothing that writes, updates, or deletes a
// plan.
type PlanReader interface {
	List(ctx context.Context, filter store.PlanFilter) ([]domain.ChangePlan, int, error)
	Get(ctx context.Context, planID string) (domain.ChangePlan, error)
	Current(ctx context.Context, containerID string) (domain.ChangePlan, error)
	History(ctx context.Context, containerID string, page store.Page) ([]domain.ChangePlan, int, error)
	Summary(ctx context.Context) (domain.ChangePlanSummary, error)
}

// PlanGenerator is the planner capability the generate endpoint needs.
//
// Deliberately narrow: the API can ASK for a pass and read the planner's
// status. It cannot generate synchronously, which is what keeps an
// unauthenticated caller from holding a request open across a whole-estate
// pass.
type PlanGenerator interface {
	RequestGeneration()
	Status() domain.PlannerStatus
}

// planUnavailable writes the disabled response, and reports whether it did.
func (s *Server) planUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.plans != nil {
		return false
	}
	writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
		"change planning is not configured")
	return true
}

// planListResponse is the plan listing, with the estate aggregate.
//
// The summary travels WITH the list so a dashboard renders in one request, and
// so the recommendation counts are always beside the plans they describe.
type planListResponse struct {
	Items      []domain.ChangePlan      `json:"items"`
	Pagination Pagination               `json:"pagination"`
	Summary    domain.ChangePlanSummary `json:"summary"`
}

// handlePlans lists change plans.
func (s *Server) handlePlans(w http.ResponseWriter, r *http.Request) {
	if s.planUnavailable(w, r) {
		return
	}

	query, err := parsePlanQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	plans, total, err := s.plans.List(r.Context(), query.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "plan list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	summary, err := s.plans.Summary(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "plan summary failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, planListResponse{
		Items:      plans,
		Pagination: newPagination(query.Page, query.PageSize, total),
		Summary:    summary,
	})
}

// handlePlanDetail returns one plan.
func (s *Server) handlePlanDetail(w http.ResponseWriter, r *http.Request) {
	if s.planUnavailable(w, r) {
		return
	}
	planID, ok := s.planID(w, r)
	if !ok {
		return
	}

	plan, err := s.plans.Get(r.Context(), planID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "plan not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "plan load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, plan)
}

// planContainerResponse is one container's planning view.
//
// The current plan and the history that led to it. History is the "reasoning
// timeline": how HarborMaster's assessment of this container has changed, which
// is what makes a verdict reviewable rather than merely stated.
type planContainerResponse struct {
	ContainerID string `json:"containerId"`
	// Current is absent when the container has no plan -- which means no change
	// is proposed for it, NOT that a change was judged safe. The two are
	// different and the client must be able to tell them apart.
	Current    *domain.ChangePlan  `json:"current,omitempty"`
	History    []domain.ChangePlan `json:"history"`
	Pagination Pagination          `json:"pagination"`
}

// handlePlansByContainer returns one container's planning view.
func (s *Server) handlePlansByContainer(w http.ResponseWriter, r *http.Request) {
	if s.planUnavailable(w, r) {
		return
	}

	// Resolved through the container repository so a short id prefix works here
	// exactly as it does on every other container route, and so an ambiguous
	// prefix is a 409 rather than an arbitrary pick.
	containerID, ok := s.resolveContainer(w, r)
	if !ok {
		return
	}

	page, pageSize, err := parsePage(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	history, total, err := s.plans.History(r.Context(), containerID,
		store.Page{Limit: pageSize, Offset: (page - 1) * pageSize})
	if err != nil {
		s.logger.ErrorContext(r.Context(), "plan history failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	response := planContainerResponse{
		ContainerID: containerID,
		History:     history,
		Pagination:  newPagination(page, pageSize, total),
	}

	current, err := s.plans.Current(r.Context(), containerID)
	switch {
	case err == nil:
		response.Current = &current
	case errors.Is(err, store.ErrNotFound):
		// No plan. Left absent rather than invented: a container with nothing
		// proposed is not a container whose change was assessed as safe.
	default:
		s.logger.ErrorContext(r.Context(), "current plan load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, response)
}

// planGenerateResponse acknowledges a requested pass.
type planGenerateResponse struct {
	// Requested is always true on a 202: the endpoint schedules work and does
	// not wait for it.
	Requested bool                 `json:"requested"`
	Planner   domain.PlannerStatus `json:"planner"`
}

// handlePlanGenerate requests a plan generation pass.
//
// # What it does not do
//
// It does not pull an image, recreate a container, or apply anything. It asks
// HarborMaster to re-read its own tables and re-run its own risk model. The
// word "generate" refers to generating ANALYSIS.
//
// ASYNCHRONOUS deliberately. A synchronous pass over a ten-thousand-container
// estate would hold an unauthenticated request open for minutes. The request is
// coalesced -- calling it in a loop produces one pass, not a backlog -- and it
// is rate limited on top of that. It is also close to free on a settled estate:
// every assessment is fingerprinted, so a pass with nothing new to say writes
// nothing at all.
func (s *Server) handlePlanGenerate(w http.ResponseWriter, r *http.Request) {
	if s.planner == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"change planning is not configured")
		return
	}
	if err := s.guardWrite(r); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	s.planner.RequestGeneration()
	writeJSON(w, r, s.logger, http.StatusAccepted, planGenerateResponse{
		Requested: true,
		Planner:   s.planner.Status(),
	})
}

// ---------------------------------------------------------------- queries --

// planIDPrefix and planIDHexLength describe a generated plan identifier.
const (
	planIDPrefix    = "plan_"
	planIDHexLength = 20
	planIDBytes     = len(planIDPrefix) + planIDHexLength
)

// validPlanID reports whether id has the shape of a generated plan id.
//
// Validated by SHAPE before it reaches a query. Plan ids are server-generated
// and have exactly one form, so anything else is a miss -- and refusing early
// keeps arbitrary caller text out of the database layer entirely.
func validPlanID(id string) bool {
	if len(id) != planIDBytes || !strings.HasPrefix(id, planIDPrefix) {
		return false
	}
	for _, r := range id[len(planIDPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// planID reads and validates the {id} path segment.
func (s *Server) planID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	if !validPlanID(raw) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the plan id is not well formed")
		return "", false
	}
	return raw, true
}

// planQuery is a parsed and validated plan listing request.
type planQuery struct {
	Bands           []domain.RiskBand
	Recommendations []domain.Recommendation
	Updates         []domain.UpdateType
	CurrentOnly     bool
	MinRisk         int

	Sort      string
	Ascending bool
	Page      int
	PageSize  int
}

// parsePlanQuery reads and validates the listing parameters.
//
// Every parameter is validated against a CLOSED VOCABULARY defined in the
// domain package, or is a bounded integer. Nothing a caller sends becomes SQL
// text: a band, recommendation, or update type is matched against the domain's
// allowlist and then travels as a bound parameter, and the sort field selects a
// compile-time column constant from a map.
func parsePlanQuery(query url.Values) (planQuery, error) {
	// Sorted by risk, descending: the dashboard's question is "what most needs
	// attention", and chronological order answers a different one.
	parsed := planQuery{Sort: "band", CurrentOnly: true}

	page, pageSize, err := parsePage(query)
	if err != nil {
		return parsed, err
	}
	parsed.Page, parsed.PageSize = page, pageSize

	if raw := strings.TrimSpace(query.Get("sort")); raw != "" {
		if !store.ValidPlanSortField(raw) {
			return parsed, invalidParam("sort",
				"one of band, risk, recommendation, generatedAt, container, update, id")
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

	// currentOnly defaults to TRUE. A superseded plan describes a world that
	// has moved on, and listing it beside the current one would double-count
	// the estate.
	if raw := strings.TrimSpace(query.Get("currentOnly")); raw != "" {
		switch raw {
		case "true":
			parsed.CurrentOnly = true
		case "false":
			parsed.CurrentOnly = false
		default:
			return parsed, invalidParam("currentOnly", "true or false")
		}
	}

	if raw := strings.TrimSpace(query.Get("minRisk")); raw != "" {
		value, convErr := strconv.Atoi(raw)
		if convErr != nil || value < 0 || value > domain.MaxRiskScore {
			return parsed, invalidParam("minRisk", "an integer between 0 and 100")
		}
		parsed.MinRisk = value
	}

	bands, err := parseVocabulary(query, "band", domain.ValidRiskBand,
		"one of veryLow, low, medium, high, critical")
	if err != nil {
		return parsed, err
	}
	for _, value := range bands {
		parsed.Bands = append(parsed.Bands, domain.RiskBand(value))
	}

	recommendations, err := parseVocabulary(query, "recommendation", domain.ValidRecommendation,
		"one of proceed, proceedWithCaution, manualReview, notRecommended, unknown")
	if err != nil {
		return parsed, err
	}
	for _, value := range recommendations {
		parsed.Recommendations = append(parsed.Recommendations, domain.Recommendation(value))
	}

	updates, err := parseVocabulary(query, "update", domain.ValidUpdateType,
		"one of none, digest, patch, minor, major, prerelease, unknown")
	if err != nil {
		return parsed, err
	}
	for _, value := range updates {
		parsed.Updates = append(parsed.Updates, domain.UpdateType(value))
	}

	return parsed, nil
}

// filter converts a validated query into a repository filter.
func (q planQuery) filter() store.PlanFilter {
	return store.PlanFilter{
		Bands:           q.Bands,
		Recommendations: q.Recommendations,
		Updates:         q.Updates,
		CurrentOnly:     q.CurrentOnly,
		MinRisk:         q.MinRisk,
		Sort:            q.Sort,
		Ascending:       q.Ascending,
		Page: store.Page{
			Limit:  q.PageSize,
			Offset: (q.Page - 1) * q.PageSize,
		},
	}
}
