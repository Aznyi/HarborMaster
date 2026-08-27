package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Plan approval: recording that a person reviewed one immutable change plan.
//
// # The caller names a plan and nothing else
//
// There is no request body. Not "an empty object accepted for future use" -- no
// body at all, because there is nothing a caller could legitimately put in one.
//
// The image, the digests, the input digest, the container, the recommendation
// and the risk score are all facts HarborMaster established and stored; it reads
// every one of them from the plan the URL names. A caller able to supply any of
// them could approve a different change from the one they were shown, which is
// the entire attack this endpoint has to be closed against.

// planApprovalResponse is what a caller reads back.
type planApprovalResponse struct {
	Approval domain.PlanApproval `json:"approval"`
	// Valid says whether the approval currently authorises the plan. An
	// approval can stand and still not authorise anything -- the plan may have
	// been superseded, or already applied -- and an operator needs to see both
	// halves rather than a bare boolean.
	Valid bool `json:"valid"`
	// Refusal explains why not, from the closed vocabulary. Empty when valid.
	Refusal string `json:"refusal,omitempty"`
	// Explanation is HarborMaster's own sentence for the refusal.
	Explanation string `json:"explanation,omitempty"`
}

// handlePlanApprovalCreate records a human review of one change plan.
func (s *Server) handlePlanApprovalCreate(w http.ResponseWriter, r *http.Request) {
	if s.planApprovals == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeUnavailable,
			"plan approval is not available in this deployment")
		return
	}
	planID, ok := s.planID(w, r)
	if !ok {
		return
	}

	approval, err := s.planApprovals.Approve(
		r.Context(), planID, s.requesterFrom(r), s.actorFrom(r))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound, "change plan not found")
		return
	case errors.Is(err, service.ErrPlanNotApprovable):
		// 409 rather than 400: the request was well formed, and the plan's
		// state is what refuses it.
		writeError(w, r, s.logger, http.StatusConflict, CodeConflict, err.Error())
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "plan approval failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusCreated, planApprovalResponse{
		Approval: approval,
		Valid:    true,
	})
}

// handlePlanApprovalDetail reports whether a plan has been reviewed.
func (s *Server) handlePlanApprovalDetail(w http.ResponseWriter, r *http.Request) {
	if s.planApprovals == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeUnavailable,
			"plan approval is not available in this deployment")
		return
	}
	planID, ok := s.planID(w, r)
	if !ok {
		return
	}

	approval, refusal, err := s.planApprovals.Get(r.Context(), planID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Either the plan or the approval is absent. Both are "there is no
		// approval here", and distinguishing them would tell an unauthorised
		// caller which plan identifiers exist.
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound,
			"this change plan has not been approved")
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "plan approval read failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	writeJSON(w, r, s.logger, http.StatusOK, planApprovalResponse{
		Approval:    approval,
		Valid:       refusal == domain.PlanApprovalRefusalNone,
		Refusal:     string(refusal),
		Explanation: refusal.Explain(),
	})
}

// handlePlanApprovalDelete withdraws a standing approval.
func (s *Server) handlePlanApprovalDelete(w http.ResponseWriter, r *http.Request) {
	if s.planApprovals == nil {
		writeError(w, r, s.logger, http.StatusServiceUnavailable, CodeUnavailable,
			"plan approval is not available in this deployment")
		return
	}
	planID, ok := s.planID(w, r)
	if !ok {
		return
	}

	err := s.planApprovals.Revoke(
		r.Context(), planID, s.requesterFrom(r), s.actorFrom(r))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, s.logger, http.StatusNotFound, CodeNotFound,
			"this change plan has no approval to withdraw")
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "plan approval revoke failed",
			slog.String("error", err.Error()))
		writeError(w, r, s.logger, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
