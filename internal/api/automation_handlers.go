package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The automation endpoints.
//
// # What this surface can cause
//
// One thing: a scheduler pass, which submits the same acquisition requests an
// operator pressing "update" would submit, for the containers the stored
// policies already select.
//
// It cannot be aimed. Read every request body in this file and note what is
// absent from all of them: there is no container id, no container name to
// update, no image, no tag, no digest, and no registry. The three write
// endpoints take, in total:
//
//   - POST /automation/run     -- a dryRun boolean.
//   - POST /automation/approve -- a run id and a container NAME, both of which
//     must match a decision HarborMaster already recorded as awaiting approval.
//   - POST /automation/pause   -- a container name that must be in the
//     inventory, and POST /automation/resume the same.
//
// The approval endpoint is the one worth reading twice. Its container name does
// not choose a target: it SELECTS one of HarborMaster's own held decisions, and
// the engine then re-derives the plan, re-checks that the plan has not moved
// on, and submits the plan id. A name that matches no held decision approves
// nothing.
//
// # The policy endpoints are separate and administrator-only
//
// An update policy decides what the host may do to itself unattended, so
// writing one needs `automation:manage`, which only an administrator holds.
// Running a pass needs `automation:run`, which an operator holds -- a manual
// pass changes WHEN the scheduled work happens, not WHETHER it may.

// AutomationService is the engine capability the API depends on.
//
// A narrow interface rather than *service.AutomationService, so the handlers
// stay testable and the surface the API can reach is visible in one place. Note
// what is ABSENT: nothing that acquires, recreates, or rolls back. The engine
// holds those relationships; the API asks the engine to run, and asks it what
// happened.
type AutomationService interface {
	Enabled() bool
	Readable() bool

	Status(ctx context.Context) (domain.AutomationStatus, error)
	Runs(ctx context.Context, filter store.AutomationRunFilter) ([]domain.AutomationRun, int, error)
	RunDetail(ctx context.Context, runID string, page store.Page) (domain.AutomationRun, []domain.AutomationDecision, int, error)
	Decisions(ctx context.Context, filter store.AutomationDecisionFilter) ([]domain.AutomationDecision, int, error)
	Summary(ctx context.Context) (domain.AutomationRunSummary, error)
	Pauses(ctx context.Context, activeOnly bool, page store.Page) ([]domain.PausedContainer, int, error)
	Upcoming(ctx context.Context) ([]domain.AutomationDecision, error)

	RunNow(ctx context.Context, dryRun bool, requestedBy domain.Requester) (domain.AutomationRun, []domain.AutomationDecision, error)
	Approve(ctx context.Context, runID, containerName string, by domain.Requester, actor service.Actor) (domain.AutomationDecision, error)
	Resume(ctx context.Context, containerName string, by domain.Requester, actor service.Actor) error
	PauseContainer(ctx context.Context, containerName, detail string, actor service.Actor) (domain.PausedContainer, error)
}

// UpdatePolicyService is the automation-rule capability the API depends on.
type UpdatePolicyService interface {
	Create(ctx context.Context, policy domain.UpdatePolicy, actor service.Actor) (service.UpdatePolicyResult, error)
	Update(ctx context.Context, policyID string, change store.UpdatePolicyChange, actor service.Actor) (service.UpdatePolicyResult, error)
	Archive(ctx context.Context, policyID string, actor service.Actor) error
	Get(ctx context.Context, policyID string) (service.UpdatePolicyResult, error)
	List(ctx context.Context, filter store.UpdatePolicyFilter) ([]domain.UpdatePolicy, int, error)
}

// automationUnavailable writes the disabled response, and reports whether it
// did.
func (s *Server) automationUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.automation != nil && s.automation.Readable() {
		return false
	}
	writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
		"the automation engine is not configured")
	return true
}

// updatePolicyUnavailable writes the disabled response for the policy routes.
func (s *Server) updatePolicyUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.updatePolicies != nil {
		return false
	}
	writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
		"update policies are not configured")
	return true
}

// --------------------------------------------------------------- status --

// automationStatusResponse is the dashboard payload.
type automationStatusResponse struct {
	Status  domain.AutomationStatus     `json:"status"`
	History domain.AutomationRunSummary `json:"history"`
}

// handleAutomationStatus returns the engine's current state.
func (s *Server) handleAutomationStatus(w http.ResponseWriter, r *http.Request) {
	if s.automationUnavailable(w, r) {
		return
	}

	status, err := s.automation.Status(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "automation status failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	history, err := s.automation.Summary(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "automation summary failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, automationStatusResponse{
		Status:  status,
		History: history,
	})
}

// ----------------------------------------------------------------- runs --

// automationRunListResponse is the pass history.
type automationRunListResponse struct {
	Items      []domain.AutomationRun      `json:"items"`
	Pagination Pagination                  `json:"pagination"`
	Summary    domain.AutomationRunSummary `json:"summary"`
}

// handleAutomationRuns lists scheduler passes.
func (s *Server) handleAutomationRuns(w http.ResponseWriter, r *http.Request) {
	if s.automationUnavailable(w, r) {
		return
	}

	query, err := parseAutomationRunQuery(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	items, total, err := s.automation.Runs(r.Context(), query.filter())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "automation run list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	summary, err := s.automation.Summary(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "automation summary failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, automationRunListResponse{
		Items:      items,
		Pagination: newPagination(query.Page, query.PageSize, total),
		Summary:    summary,
	})
}

// automationRunDetailResponse is one pass with its reasoning.
type automationRunDetailResponse struct {
	Run        domain.AutomationRun        `json:"run"`
	Decisions  []domain.AutomationDecision `json:"decisions"`
	Pagination Pagination                  `json:"pagination"`
}

// handleAutomationRunDetail returns one pass and the decisions it made.
func (s *Server) handleAutomationRunDetail(w http.ResponseWriter, r *http.Request) {
	if s.automationUnavailable(w, r) {
		return
	}

	runID := strings.TrimSpace(r.PathValue("id"))
	if !domain.ValidAutomationRunID(runID) {
		// The same 404 an unknown id gets. A malformed id must not be
		// distinguishable from an absent one.
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "automation run not found")
		return
	}

	page, pageSize, err := parsePage(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}

	run, decisions, total, err := s.automation.RunDetail(r.Context(), runID,
		store.Page{Limit: pageSize, Offset: (page - 1) * pageSize})
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "automation run not found")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "automation run load failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, automationRunDetailResponse{
		Run:        run,
		Decisions:  decisions,
		Pagination: newPagination(page, pageSize, total),
	})
}

// automationUpcomingResponse is what the next pass would do.
type automationUpcomingResponse struct {
	Items []domain.AutomationDecision `json:"items"`
	// Eligible counts the containers that would be updated, so a client does
	// not have to filter the list to render the one number that matters.
	Eligible int `json:"eligible"`
}

// handleAutomationUpcoming previews the next pass WITHOUT running it.
//
// A read: it evaluates the same decision function over the same evidence and
// writes nothing -- no run row, no decision rows, no request to any service.
// That is the difference between this and `POST /automation/run` with dryRun,
// which does record what it would have done.
//
// A GET, and safe to call repeatedly. It costs one policy query, one inventory
// query, and a bounded per-container plan lookup, which is the same cost a real
// pass pays for its decision half.
func (s *Server) handleAutomationUpcoming(w http.ResponseWriter, r *http.Request) {
	if s.automationUnavailable(w, r) {
		return
	}

	decisions, err := s.automation.Upcoming(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "automation preview failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	eligible := 0
	for _, decision := range decisions {
		if decision.Verdict != domain.VerdictSkip {
			eligible++
		}
	}

	writeJSON(w, r, s.logger, http.StatusOK, automationUpcomingResponse{
		Items:    decisions,
		Eligible: eligible,
	})
}

// automationRunRequest is the manual-pass body.
//
// ONE FIELD, and it is a boolean. There is nothing here to aim.
type automationRunRequest struct {
	// DryRun decides everything and changes nothing. The pass is recorded, so
	// an operator can read what would have happened afterwards.
	DryRun bool `json:"dryRun,omitempty"`
}

// handleAutomationRun starts a pass now.
//
// Synchronous, because the operator pressing the button is watching and a pass
// is bounded by AUTOMATION_PASS_TIMEOUT. The WORK it submits is asynchronous:
// the response says what was decided and submitted, not what finished.
func (s *Server) handleAutomationRun(w http.ResponseWriter, r *http.Request) {
	if s.automationUnavailable(w, r) {
		return
	}
	if !s.automation.Enabled() {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"the automation engine is disabled in this deployment")
		return
	}

	var body automationRunRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	run, decisions, err := s.automation.RunNow(r.Context(), body.DryRun, s.requesterFrom(r))
	switch {
	case errors.Is(err, service.ErrAutomationBusy):
		// 409 rather than 400. The request was well formed and would have been
		// honoured a moment earlier; what changed is the world.
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"an automation pass is already running")
		return
	case errors.Is(err, service.ErrAutomationDisabled):
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"the automation engine is disabled in this deployment")
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "automation run failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, automationRunDetailResponse{
		Run:        run,
		Decisions:  decisions,
		Pagination: newPagination(1, len(decisions), len(decisions)),
	})
}

// ------------------------------------------------------------- approval --

// automationApproveRequest releases one held decision.
//
// The two fields SELECT a decision HarborMaster already made. They do not
// describe one: a run id and a container name that match no held decision
// approve nothing, and the plan the approved decision names is re-derived and
// re-checked before anything is submitted.
type automationApproveRequest struct {
	RunID         string `json:"runId"`
	ContainerName string `json:"containerName"`
}

// handleAutomationApprove applies a decision a policy held for a person.
func (s *Server) handleAutomationApprove(w http.ResponseWriter, r *http.Request) {
	if s.automationUnavailable(w, r) {
		return
	}
	if !s.automation.Enabled() {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"the automation engine is disabled in this deployment")
		return
	}

	var body automationApproveRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return
	}

	runID := strings.TrimSpace(body.RunID)
	if !domain.ValidAutomationRunID(runID) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"a well-formed automation run id is required")
		return
	}
	containerName := strings.TrimSpace(body.ContainerName)
	if !domain.ValidContainerName(containerName) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"a well-formed container name is required")
		return
	}

	decision, err := s.automation.Approve(r.Context(), runID, containerName,
		s.requesterFrom(r), s.actorFrom(r))
	switch {
	case errors.Is(err, service.ErrDecisionNotApprovable):
		// 409: the decision exists or existed, and the world moved. The message
		// is HarborMaster's own and never echoes the request.
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"this decision is no longer approvable; the plan behind it has changed or "+
				"automation is paused for this container")
		return
	case errors.Is(err, service.ErrAutomationDisabled):
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeDisabled,
			"the automation engine is disabled in this deployment")
		return
	case err != nil:
		var refused service.ErrAcquisitionRefused
		if errors.As(err, &refused) {
			writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
				"the acquisition preflight refused this update: "+refused.Refusal.Explain())
			return
		}
		s.logger.ErrorContext(r.Context(), "automation approval failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusAccepted, decision)
}

// --------------------------------------------------------------- pauses --

// automationPauseListResponse is the paused-container listing.
type automationPauseListResponse struct {
	Items      []domain.PausedContainer `json:"items"`
	Pagination Pagination               `json:"pagination"`
}

// handleAutomationPauses lists containers automation will not touch.
func (s *Server) handleAutomationPauses(w http.ResponseWriter, r *http.Request) {
	if s.automationUnavailable(w, r) {
		return
	}

	page, pageSize, err := parsePage(r.URL.Query())
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}
	// Active by default: an operator opening this page is asking what is
	// blocked now, and a list dominated by cleared history buries it.
	activeOnly := r.URL.Query().Get("all") != "true"

	items, total, err := s.automation.Pauses(r.Context(), activeOnly,
		store.Page{Limit: pageSize, Offset: (page - 1) * pageSize})
	if err != nil {
		s.logger.ErrorContext(r.Context(), "automation pause list failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, automationPauseListResponse{
		Items:      items,
		Pagination: newPagination(page, pageSize, total),
	})
}

// automationPauseRequest names a container to pause or resume.
//
// The name must be one the INVENTORY knows. It selects a container HarborMaster
// has already observed; it does not describe one, and pausing cannot change
// anything about the host either way.
type automationPauseRequest struct {
	ContainerName string `json:"containerName"`
	// Reason is the operator's note. Bounded, UTF-8 validated, and sanitised
	// before it reaches a column anybody reads.
	Reason string `json:"reason,omitempty"`
}

// maxPauseReasonBytes bounds the operator's note.
const maxPauseReasonBytes = 500

// handleAutomationPause stops automation for one container.
func (s *Server) handleAutomationPause(w http.ResponseWriter, r *http.Request) {
	if s.automationUnavailable(w, r) {
		return
	}

	body, containerName, ok := s.decodePauseRequest(w, r)
	if !ok {
		return
	}

	pause, err := s.automation.PauseContainer(r.Context(), containerName,
		domain.SanitiseDisplayText(body.Reason, maxPauseReasonBytes), s.actorFrom(r))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound,
			"no container by that name is present in the inventory")
		return
	case errors.Is(err, store.ErrPauseActive):
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict,
			"automation is already paused for this container")
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "automation pause failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, pause)
}

// handleAutomationResume clears a pause.
func (s *Server) handleAutomationResume(w http.ResponseWriter, r *http.Request) {
	if s.automationUnavailable(w, r) {
		return
	}

	_, containerName, ok := s.decodePauseRequest(w, r)
	if !ok {
		return
	}

	err := s.automation.Resume(r.Context(), containerName, s.requesterFrom(r), s.actorFrom(r))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound,
			"automation is not paused for this container")
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "automation resume failed", slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// decodePauseRequest reads and validates a pause or resume body.
func (s *Server) decodePauseRequest(
	w http.ResponseWriter,
	r *http.Request,
) (automationPauseRequest, string, bool) {
	var body automationPauseRequest
	if err := decodeJSONBody(w, r, s.cfg.MaxRequestBytes, &body); err != nil {
		s.writeGuardFailure(w, r, err)
		return automationPauseRequest{}, "", false
	}

	containerName := strings.TrimSpace(body.ContainerName)
	if !domain.ValidContainerName(containerName) {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"a well-formed container name is required")
		return automationPauseRequest{}, "", false
	}
	if len(body.Reason) > maxPauseReasonBytes {
		writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest,
			"the reason must be at most 500 bytes")
		return automationPauseRequest{}, "", false
	}
	return body, containerName, true
}
